// Package policy resolves which AIQG policy bundle applies to a
// request by calling aiqg-dashboard-be's /internal/policy/resolve
// endpoint. Phase 4.0 — observation-only: the result rides on the
// emitted AIQG response event but takes no enforcement action yet.
//
// The Resolver interface lets tests swap in deterministic results
// without spinning up an HTTP server; production wires NewDashboardResolver.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	resilience "github.com/Tributary-ai-services/aether-shared/go-aiqg-resilience"
)

// Resolution names the bundle that applies to a request along with
// the precedence rung that picked it. Source values mirror the
// dashboard-be constants:
//
//	"explicit"      — TAS-Policy-Bundle header named a real bundle
//	"tenant_active" — tenant has a bundle with active=true
//	"default"       — no configured bundle matched; observe-all sentinel
type Resolution struct {
	BundleID   string
	BundleName string
	Source     string
	// Reduction is the resolved bundle's payload-reduction config (raw
	// JSON; the `reduction` block of PolicyBundle.Spec). Empty when the
	// bundle has no extraction policy. Parse with ParseReduction. Plan #7
	// Phase 2: the gateway reads this to shadow/apply the Gatekeeper
	// extract pipeline. Today it's only carried + parsed, not executed.
	Reduction json.RawMessage

	// Target is the provider/model a route rule steers to, when one does.
	// Nil means no override and the router selects exactly as it does today,
	// which is what makes this additive: an older resolver that omits the
	// field yields nil and nothing changes.
	Target *Target

	// Health and Budgets are the matched rule's resilience overrides. Kept as
	// the shared go-aiqg-resilience types rather than raw JSON because, unlike
	// Reduction, the gateway itself acts on these values — and the shape is
	// already owned by a module both services import, so there is nothing to
	// keep opaque.
	//
	// Nil means "inherit the gateway defaults", which is deliberately distinct
	// from a zero-valued struct: a rule that overrides only eject_for must not
	// silently reset every other threshold to zero.
	Health  *resilience.Health
	Budgets *resilience.Budgets

	// Fallback is the matched rule's ordered failover chain; Constraints are
	// the tenant's declared compliance limits.
	//
	// Constraints arrive on EVERY resolution, including the default rung where
	// no rule matched, because they bound routing as a whole. A constraint
	// that only travelled with a matched rule would be unenforced for exactly
	// the traffic nobody wrote a rule for.
	Fallback    *resilience.Fallback
	Constraints *resilience.Constraints
	// Affinity is the matched rule's provider-affinity block, overriding the
	// gateway's own configuration for this request.
	Affinity *resilience.Affinity
	// Selection / Switching / Verbosity drive step 5's expected_cost and
	// weighted strategies. Verbosity arrives only when a rule asks for
	// expected_cost — see the resolver.
	Selection *resilience.Selection
	Switching *resilience.Switching
	Verbosity []resilience.Verbosity
}

// Target is a resolved routing destination. Model empty means "keep the
// caller's model" — a rule may pin a provider without pinning a model.
//
// Source records WHICH RUNG decided, so "why did this request go to
// Anthropic" is answerable from the event rather than reconstructed.
type Target struct {
	Provider string
	Model    string
	Source   string
}

// Default returns the sentinel resolution used when the resolver is
// unavailable or returns an error. Callers can treat the result as
// "observe-all" — the eventual enforcement engine (Stage 4.1+) maps
// this to "every rule is action=log."
func Default() Resolution {
	return Resolution{BundleName: "(default)", Source: "default"}
}

// Resolver is the dependency surface the AIQG middleware needs. Errors
// from Resolve never propagate to the client request — the middleware
// degrades to Default() instead. Returning an error here just signals
// "log this so an operator can investigate."
type Resolver interface {
	Resolve(ctx context.Context, req ResolveRequest) (Resolution, error)
}

// ResolveRequest captures the inputs the resolver needs. Only TenantID
// is required; the other fields slot into Stage 4.1+ precedence rules
// (route matching) and are plumbed now to keep the contract stable.
type ResolveRequest struct {
	TenantID           string
	AIQGAccountID      string
	PolicyBundleHeader string // raw TAS-Policy-Bundle header value
	SourceApp          string
	Path               string
	// Routing attributes — only known at routing time, so the gateway
	// re-resolves then (ResolveBundleForRouting). Empty at receipt.
	Model        string
	WorkflowType string
	Vendor       string
}

// DashboardResolver calls aiqg-dashboard-be's POST /internal/policy/resolve.
// Mirrors the same shape + auth pattern as tokens.DashboardResolver
// so operators only configure one base URL + internal auth token.
type DashboardResolver struct {
	HTTP              *http.Client
	BaseURL           string
	InternalAuthToken string
}

// NewDashboardResolver returns a Resolver pointing at the configured
// dashboard-be base URL. Same baseURL as tokens.NewDashboardResolver
// — "http://aiqg-dashboard-be.aiqg.svc.cluster.local:8095" in cluster.
//
// The 2s timeout matches tokens.DashboardResolver; the resolver is
// called once per AIQG request so the per-request cost is bounded.
// Future: an in-process TTL cache shaves the per-request HTTP if
// resolution becomes hot (most resolutions for a single tenant will
// return the same bundle within seconds).
func NewDashboardResolver(baseURL, internalAuthToken string) (*DashboardResolver, error) {
	if baseURL == "" {
		return nil, errors.New("policy.NewDashboardResolver: baseURL required")
	}
	if internalAuthToken == "" {
		return nil, errors.New("policy.NewDashboardResolver: internalAuthToken required")
	}
	return &DashboardResolver{
		HTTP:              &http.Client{Timeout: 2 * time.Second},
		BaseURL:           baseURL,
		InternalAuthToken: internalAuthToken,
	}, nil
}

// resolveRequest mirrors the JSON shape of aiqg-dashboard-be's
// internal/handlers/internal_policy.go ResolveRequest. Keeping the
// types in sync is enforced by a smoke test, not by an import (would
// drag Gin into this package's dep tree otherwise).
type resolveRequest struct {
	TenantID           string `json:"tenant_id"`
	AIQGAccountID      string `json:"aiqg_account_id,omitempty"`
	PolicyBundleHeader string `json:"policy_bundle_header,omitempty"`
	SourceApp          string `json:"source_app,omitempty"`
	Path               string `json:"path,omitempty"`
	Model              string `json:"model,omitempty"`
	WorkflowType       string `json:"workflow_type,omitempty"`
	Vendor             string `json:"vendor,omitempty"`
}

type resolveResponse struct {
	BundleID   string          `json:"bundle_id"`
	BundleName string          `json:"bundle_name"`
	Source     string          `json:"source"`
	Reduction  json.RawMessage `json:"reduction,omitempty"`
	Target     *struct {
		Provider string `json:"provider,omitempty"`
		Model    string `json:"model,omitempty"`
		Source   string `json:"source,omitempty"`
	} `json:"target,omitempty"`
	Health      *resilience.Health      `json:"health,omitempty"`
	Budgets     *resilience.Budgets     `json:"budgets,omitempty"`
	Fallback    *resilience.Fallback    `json:"fallback,omitempty"`
	Constraints *resilience.Constraints `json:"constraints,omitempty"`
	Affinity    *resilience.Affinity    `json:"affinity,omitempty"`
	Selection   *resilience.Selection   `json:"selection,omitempty"`
	Switching   *resilience.Switching   `json:"switching,omitempty"`
	Verbosity   []resilience.Verbosity  `json:"verbosity,omitempty"`
}

// ErrResolverBadRequest is returned when the dashboard rejects the
// request body (400). Callers should treat this as a permanent
// failure for that request shape — retrying won't help — but other
// requests can still succeed, so degrade to Default() rather than
// crashing.
var ErrResolverBadRequest = errors.New("policy.DashboardResolver: dashboard returned 400")

// ErrBundleNotFound maps the 404 response (explicit header named an
// unknown bundle). Distinguished from generic transport errors so
// callers can emit a clearer "operator typo on TAS-Policy-Bundle"
// signal rather than the generic resolver-unavailable warning.
var ErrBundleNotFound = errors.New("policy.DashboardResolver: bundle not found")

// Resolve returns the bundle resolution for the given request.
// Errors are returned for transport failures, bad-request, and
// not-found cases; success returns the resolution + nil. The
// middleware swallows the error and falls back to Default() so
// the request never fails because of policy resolution.
func (r *DashboardResolver) Resolve(ctx context.Context, req ResolveRequest) (Resolution, error) {
	if req.TenantID == "" {
		return Default(), errors.New("policy.DashboardResolver: tenant_id required")
	}

	body, err := json.Marshal(resolveRequest{
		TenantID:           req.TenantID,
		AIQGAccountID:      req.AIQGAccountID,
		PolicyBundleHeader: req.PolicyBundleHeader,
		SourceApp:          req.SourceApp,
		Path:               req.Path,
		Model:              req.Model,
		WorkflowType:       req.WorkflowType,
		Vendor:             req.Vendor,
	})
	if err != nil {
		return Default(), fmt.Errorf("policy.DashboardResolver: marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.BaseURL+"/internal/policy/resolve", bytes.NewReader(body))
	if err != nil {
		return Default(), fmt.Errorf("policy.DashboardResolver: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Internal-Auth", r.InternalAuthToken)

	resp, err := r.HTTP.Do(httpReq)
	if err != nil {
		return Default(), fmt.Errorf("policy.DashboardResolver: do: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var v resolveResponse
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return Default(), fmt.Errorf("policy.DashboardResolver: decode 200: %w", err)
		}
		res := Resolution{BundleID: v.BundleID, BundleName: v.BundleName, Source: v.Source, Reduction: v.Reduction}
		if v.Target != nil && strings.TrimSpace(v.Target.Provider) != "" {
			res.Target = &Target{
				Provider: strings.ToLower(strings.TrimSpace(v.Target.Provider)),
				Model:    strings.TrimSpace(v.Target.Model),
				Source:   v.Target.Source,
			}
		}
		// Carried through only when the control plane sent them; an absent
		// block stays nil so the gateway's own defaults apply untouched.
		res.Health, res.Budgets = v.Health, v.Budgets
		res.Fallback, res.Constraints, res.Affinity = v.Fallback, v.Constraints, v.Affinity
		res.Selection, res.Switching, res.Verbosity = v.Selection, v.Switching, v.Verbosity
		return res, nil
	case http.StatusNotFound:
		// Explicit header named an unknown bundle. Caller can log
		// distinctly so operators see "your TAS-Policy-Bundle header
		// is wrong" rather than the generic resolver-down warning.
		return Default(), ErrBundleNotFound
	case http.StatusBadRequest:
		return Default(), ErrResolverBadRequest
	default:
		return Default(), fmt.Errorf("policy.DashboardResolver: unexpected status %d", resp.StatusCode)
	}
}

// StaticResolver is a test helper that always returns the same
// resolution. Used in unit tests that need to exercise the gateway
// middleware's resolution-handling logic without a fake HTTP server.
type StaticResolver struct {
	Result Resolution
	Err    error
}

func (r StaticResolver) Resolve(_ context.Context, _ ResolveRequest) (Resolution, error) {
	return r.Result, r.Err
}
