// Package experiments implements the gateway side of the AIQG Experiments
// runner (docs/AIQG-EXPERIMENTS-RUNNER.md): it loads a tenant's active
// experiments from aiqg-dashboard-be, matches a request against each cohort,
// and deterministically assigns it to a variant. Assignment is sticky (same
// identity → same variant) and respects max_traffic_pct.
//
// dry_run experiments only get ASSIGNED + stamped on the event (no routing
// change); running experiments additionally carry an Override the router
// applies (Phase 2b). Resolution never fails a request — any error degrades to
// "no assignment" (route normally), mirroring the policy resolver.
package experiments

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Lifecycle states the gateway acts on. Others are never returned by the
// cache-load endpoint.
const (
	StatusDryRun  = "dry_run"
	StatusRunning = "running"
)

// Variant is one arm. key="control" is the baseline (empty Override).
type Variant struct {
	Key      string          `json:"key"`
	Weight   int             `json:"weight"`
	Override json.RawMessage `json:"override,omitempty"`
}

// Cohort is the eligibility matcher. Empty fields match anything; non-empty
// lists are OR-within / AND-across (a request must satisfy every set field).
type Cohort struct {
	URLPath      string   `json:"url_path,omitempty"`
	SourceApp    []string `json:"source_app,omitempty"`
	Model        []string `json:"model,omitempty"`
	WorkflowType []string `json:"workflow_type,omitempty"`
}

// Assignment controls the sticky key + decorrelation salt.
type Assignment struct {
	KeySource string `json:"key_source,omitempty"` // conversation|user|flow|principal|ip|request
	Salt      string `json:"salt,omitempty"`
}

// Guardrails — only the fields the gateway enforces inline. max_traffic_pct
// caps the eligible share; the rest of a matched cohort is left untouched.
type Guardrails struct {
	MaxTrafficPct *int `json:"max_traffic_pct,omitempty"`
}

// Experiment mirrors the aiqg-dashboard-be /internal/experiments payload. The
// JSONB config blocks deserialize straight into the typed sub-structs.
type Experiment struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Status     string     `json:"status"`
	Cohort     Cohort     `json:"cohort"`
	Assignment Assignment `json:"assignment"`
	Guardrails Guardrails `json:"guardrails"`
	Variants   []Variant  `json:"variants"`
	StartsAt   *time.Time `json:"starts_at,omitempty"`
	EndsAt     *time.Time `json:"ends_at,omitempty"`
}

// Identity is the sticky-key ladder for a request. RequestID is the always-
// present random fallback (so an un-instrumented caller still gets a clean,
// if non-sticky, split).
type Identity struct {
	UserID         string
	ConversationID string
	FlowID         string
	PrincipalID    string
	ClientIP       string
	RequestID      string
}

// ReqAttrs are the request attributes a cohort matches on.
type ReqAttrs struct {
	SourceApp    string
	Model        string
	WorkflowType string
	Path         string
}

// Decision is the assignment outcome. Apply is true only for running
// experiments (dry_run assigns + stamps but the router ignores Override).
type Decision struct {
	ExperimentID string
	Variant      string
	Override     json.RawMessage
	Apply        bool
}

// key picks the sticky key per Assignment.KeySource, falling down the ladder to
// RequestID when the chosen source is empty.
func (a Assignment) key(id Identity) string {
	ladder := []string{id.ConversationID, id.UserID, id.FlowID, id.PrincipalID, id.ClientIP, id.RequestID}
	switch a.KeySource {
	case "user":
		ladder = []string{id.UserID, id.ConversationID, id.FlowID, id.PrincipalID, id.ClientIP, id.RequestID}
	case "flow":
		ladder = []string{id.FlowID, id.ConversationID, id.UserID, id.PrincipalID, id.ClientIP, id.RequestID}
	case "principal":
		ladder = []string{id.PrincipalID, id.ClientIP, id.RequestID}
	case "ip":
		ladder = []string{id.ClientIP, id.RequestID}
	case "request":
		ladder = []string{id.RequestID}
	}
	for _, k := range ladder {
		if k != "" {
			return k
		}
	}
	return id.RequestID
}

// bucket maps (experiment, salt, key) to a stable 0..9999 bucket.
func bucket(expID, salt, key string) int {
	h := crc32.ChecksumIEEE([]byte(expID + ":" + salt + ":" + key))
	return int(h % 10000)
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// matches reports cohort eligibility. URLPath uses substring containment (a
// conservative stand-in for the route-rule regex; over-matches rather than
// under-matches, consistent with the collision-detection bias).
func (c Cohort) matches(a ReqAttrs) bool {
	if len(c.SourceApp) > 0 && !containsFold(c.SourceApp, a.SourceApp) {
		return false
	}
	if len(c.Model) > 0 && !containsFold(c.Model, a.Model) {
		return false
	}
	if len(c.WorkflowType) > 0 && !containsFold(c.WorkflowType, a.WorkflowType) {
		return false
	}
	if c.URLPath != "" && !strings.Contains(a.Path, c.URLPath) {
		return false
	}
	return true
}

// withinSchedule reports whether now is inside [StartsAt, EndsAt] (open ends
// when unset).
func (e Experiment) withinSchedule(now time.Time) bool {
	if e.StartsAt != nil && now.Before(*e.StartsAt) {
		return false
	}
	if e.EndsAt != nil && now.After(*e.EndsAt) {
		return false
	}
	return true
}

// assign deterministically maps an identity to a variant within the eligible
// band, or returns nil when the bucket falls outside max_traffic_pct
// ("untouched" — route normally, no stamping). Weights split the eligible band
// (so a 50/50 capped at 10% = 5% control-arm / 5% variant / 90% untouched).
func (e Experiment) assign(id Identity) *Decision {
	if len(e.Variants) == 0 {
		return nil
	}
	maxPct := 100
	if e.Guardrails.MaxTrafficPct != nil {
		maxPct = *e.Guardrails.MaxTrafficPct
	}
	if maxPct <= 0 {
		return nil
	}
	if maxPct > 100 {
		maxPct = 100
	}
	b := bucket(e.ID, e.Assignment.Salt, e.Assignment.key(id))
	band := maxPct * 100 // eligible bucket count out of 10000
	if b >= band {
		return nil // untouched
	}
	pos := b * 10000 / band // rescale into 0..9999 across the eligible band
	cum := 0
	for _, v := range e.Variants {
		cum += v.Weight * 100
		if pos < cum {
			return &Decision{ExperimentID: e.ID, Variant: v.Key, Override: v.Override}
		}
	}
	last := e.Variants[len(e.Variants)-1]
	return &Decision{ExperimentID: e.ID, Variant: last.Key, Override: last.Override}
}

// Loader fetches a tenant's active (dry_run+running) experiments. The HTTP
// implementation hits aiqg-dashboard-be; tests inject a fake.
type Loader interface {
	ActiveForTenant(ctx context.Context, tenantID string) ([]Experiment, error)
}

// Resolver caches each tenant's active experiments (TTL) and assigns requests.
// Cache bounds dashboard load + worst-case staleness on edits.
type Resolver struct {
	loader Loader
	ttl    time.Duration
	mu     sync.Mutex
	cache  map[string]cacheEntry
	now    func() time.Time // injectable for tests
}

type cacheEntry struct {
	experiments []Experiment
	expiresAt   time.Time
}

// NewResolver wraps a Loader with a TTL cache. ttl<=0 defaults to 30s (the
// kill-switch propagation target in §7).
func NewResolver(loader Loader, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Resolver{loader: loader, ttl: ttl, cache: map[string]cacheEntry{}, now: time.Now}
}

func (r *Resolver) active(ctx context.Context, tenantID string) []Experiment {
	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[tenantID]; ok && now.Before(e.expiresAt) {
		exps := e.experiments
		r.mu.Unlock()
		return exps
	}
	r.mu.Unlock()

	exps, err := r.loader.ActiveForTenant(ctx, tenantID)
	if err != nil {
		// Degrade: serve any stale cache, else no experiments. Never fail.
		r.mu.Lock()
		stale := r.cache[tenantID].experiments
		r.mu.Unlock()
		return stale
	}
	r.mu.Lock()
	r.cache[tenantID] = cacheEntry{experiments: exps, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return exps
}

// Resolve returns the variant decision for a request, or nil when no
// experiment claims it. v1 collision rule: the first matching cohort (by
// dashboard order = created_at) claims the request and it participates in that
// one only — claim-on-match, so a matched-but-untouched bucket still leaves
// other experiments' pools (returns nil = route normally).
func (r *Resolver) Resolve(ctx context.Context, tenantID string, id Identity, attrs ReqAttrs) *Decision {
	if r == nil {
		return nil
	}
	now := r.now()
	for _, e := range r.active(ctx, tenantID) {
		if e.Status != StatusDryRun && e.Status != StatusRunning {
			continue
		}
		if !e.withinSchedule(now) {
			continue
		}
		if !e.Cohort.matches(attrs) {
			continue
		}
		// Cohort matched — this experiment claims the request.
		d := e.assign(id)
		if d == nil {
			return nil // claimed but in the untouched band
		}
		d.Apply = e.Status == StatusRunning
		return d
	}
	return nil
}

// NonControlOverrides returns the non-control variants of an active experiment
// for a tenant — the arms a control-arm request is shadow-evaluated against.
// Empty when the experiment isn't active/known.
func (r *Resolver) NonControlOverrides(ctx context.Context, tenantID, experimentID string) []Variant {
	if r == nil {
		return nil
	}
	for _, e := range r.active(ctx, tenantID) {
		if e.ID != experimentID {
			continue
		}
		out := make([]Variant, 0, len(e.Variants))
		for _, v := range e.Variants {
			if v.Key != "control" {
				out = append(out, v)
			}
		}
		return out
	}
	return nil
}

// HTTPLoader loads experiments from aiqg-dashboard-be's internal cache endpoint.
type HTTPLoader struct {
	HTTP              *http.Client
	BaseURL           string
	InternalAuthToken string
}

// NewHTTPLoader builds a loader against the dashboard. Reuses the same base URL
// + Internal-Auth token as the policy resolver.
func NewHTTPLoader(baseURL, internalAuthToken string) *HTTPLoader {
	return &HTTPLoader{
		HTTP:              &http.Client{Timeout: 2 * time.Second},
		BaseURL:           strings.TrimRight(baseURL, "/"),
		InternalAuthToken: internalAuthToken,
	}
}

func (l *HTTPLoader) ActiveForTenant(ctx context.Context, tenantID string) ([]Experiment, error) {
	url := l.BaseURL + "/internal/experiments?tenant=" + tenantID
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Internal-Auth", l.InternalAuthToken)
	resp, err := l.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("experiments load: status %d", resp.StatusCode)
	}
	var body struct {
		Experiments []Experiment `json:"experiments"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	return body.Experiments, nil
}
