// Package responsecache implements the AIQG exact-match response cache (C1,
// docs/AIQG-CACHING.md). A hit serves a previously-computed response for an
// equivalent request without calling the vendor — the ultimate token reduction
// (100%) and, more importantly, a 1–5s call collapsed to ~gateway latency.
//
// Three invariants make this an AIQG cache rather than a generic one:
//
//   - Tenant isolation is the cardinal rule. tenant_id is always in the key AND
//     the entry is Redis-namespaced aiqg:cache:{tenant}:{hash}, so a purge or a
//     scan is tenant-scoped and a cross-tenant read is structurally impossible.
//   - The key is built on the POST-inbound-scan (redacted) messages, so the
//     cache never keys on or stores raw PII (docs/AIQG-CACHING.md §2/§5; the G1
//     redaction it depends on is a precondition, wired in server.go).
//   - Default-safe: only deterministic requests (temperature==0 or seed set) are
//     cached, and streaming / tool-calling / experiment-claimed requests never
//     are (§1 scope, §4). Everything is opt-in behind Config.Enabled.
//
// This package is the storage + key + eligibility core. The request-path wiring
// (lookup before routing, store after the outbound scan, cache_state stamping)
// lives in internal/server.
package responsecache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// keyPrefix namespaces every entry. The tenant segment follows so a KEYS/SCAN
// over aiqg:cache:{tenant}:* is a tenant-scoped purge (right-to-be-forgotten).
const keyPrefix = "aiqg:cache"

// Config is the C1 cache profile. v1 is a single global profile; per-route
// profiles (docs/AIQG-CACHING.md §4) layer on top of this later.
type Config struct {
	// Enabled gates the whole feature. Default off.
	Enabled bool
	// TTL bounds how long an entry lives. Also the ceiling on any per-route TTL.
	TTL time.Duration
	// RequireDeterministic caches only requests that pin their sampling
	// (temperature==0 or seed set). Default true — serving one sampled answer
	// to repeated temperature>0 prompts is wrong for most workflows.
	RequireDeterministic bool
	// MaxBodyBytes skips storing responses larger than this (0 = no limit).
	MaxBodyBytes int
}

// Entry is a stored response. Response is the marshaled, post-outbound-scan
// *types.ChatResponse — already safe to return on a hit.
type Entry struct {
	Response         json.RawMessage `json:"response"`
	Vendor           string          `json:"vendor,omitempty"`
	Model            string          `json:"model,omitempty"`
	PromptTokens     int             `json:"prompt_tokens,omitempty"`
	CompletionTokens int             `json:"completion_tokens,omitempty"`
	ScoringVersion   string          `json:"scoring_version,omitempty"`
	StoredAtUnix     int64           `json:"stored_at"`
}

// Cache is the storage port. Get/Set take tenant and hash separately so the
// implementation owns the namespacing and a cross-tenant read cannot be
// expressed by a caller.
type Cache interface {
	Get(ctx context.Context, tenantID, hash string) (*Entry, bool, error)
	Set(ctx context.Context, tenantID, hash string, e *Entry, ttl time.Duration) error
	// PurgeTenant flushes a tenant's whole namespace (tenant delete /
	// right-to-be-forgotten). Returns the number of entries removed.
	PurgeTenant(ctx context.Context, tenantID string) (int, error)
}

func redisKey(tenantID, hash string) string {
	return fmt.Sprintf("%s:%s:%s", keyPrefix, tenantID, hash)
}

// RedisCache is the shared-across-replicas implementation. It reuses the AIQG
// Redis client (same lifecycle as linkage / the prompt-cache probe).
type RedisCache struct {
	r redis.UniversalClient
}

// NewRedisCache wraps a Redis client. Nil client is a programming error.
func NewRedisCache(r redis.UniversalClient) *RedisCache { return &RedisCache{r: r} }

func (c *RedisCache) Get(ctx context.Context, tenantID, hash string) (*Entry, bool, error) {
	b, err := c.r.Get(ctx, redisKey(tenantID, hash)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		// A corrupt entry is a miss, not an error — never fail a request on it.
		return nil, false, nil
	}
	return &e, true, nil
}

func (c *RedisCache) Set(ctx context.Context, tenantID, hash string, e *Entry, ttl time.Duration) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.r.Set(ctx, redisKey(tenantID, hash), b, ttl).Err()
}

func (c *RedisCache) PurgeTenant(ctx context.Context, tenantID string) (int, error) {
	pattern := fmt.Sprintf("%s:%s:*", keyPrefix, tenantID)
	var (
		cursor uint64
		total  int
	)
	for {
		keys, next, err := c.r.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			return total, err
		}
		if len(keys) > 0 {
			if err := c.r.Del(ctx, keys...).Err(); err != nil {
				return total, err
			}
			total += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return total, nil
}

// MemoryCache is a bounded, TTL-honoring in-process cache for tests and
// single-replica deployments. Across replicas it under-shares (each pod has its
// own map) but never serves a stale-past-TTL or cross-tenant entry.
type MemoryCache struct {
	mu      sync.Mutex
	m       map[string]memEntry
	maxKeys int
}

type memEntry struct {
	e       Entry
	expires time.Time
}

// NewMemoryCache bounds the map at maxKeys (<=0 uses 4096). Eviction on insert
// drops an arbitrary entry — deterministic recompute on the next miss, never a
// correctness cost.
func NewMemoryCache(maxKeys int) *MemoryCache {
	if maxKeys <= 0 {
		maxKeys = 4096
	}
	return &MemoryCache{m: make(map[string]memEntry), maxKeys: maxKeys}
}

func (c *MemoryCache) Get(_ context.Context, tenantID, hash string) (*Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := redisKey(tenantID, hash)
	me, ok := c.m[k]
	if !ok {
		return nil, false, nil
	}
	if !me.expires.IsZero() && timeNow().After(me.expires) {
		delete(c.m, k)
		return nil, false, nil
	}
	cp := me.e
	return &cp, true, nil
}

func (c *MemoryCache) Set(_ context.Context, tenantID, hash string, e *Entry, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := redisKey(tenantID, hash)
	if _, exists := c.m[k]; !exists && len(c.m) >= c.maxKeys {
		for ek := range c.m { // evict one arbitrary entry
			delete(c.m, ek)
			break
		}
	}
	var exp time.Time
	if ttl > 0 {
		exp = timeNow().Add(ttl)
	}
	c.m[k] = memEntry{e: *e, expires: exp}
	return nil
}

func (c *MemoryCache) PurgeTenant(_ context.Context, tenantID string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := fmt.Sprintf("%s:%s:", keyPrefix, tenantID)
	n := 0
	for k := range c.m {
		if strings.HasPrefix(k, prefix) {
			delete(c.m, k)
			n++
		}
	}
	return n, nil
}

// timeNow is overridable in tests. Kept package-local (not time.Now inline) so
// TTL expiry is testable without sleeping.
var timeNow = time.Now

// KeyHash canonicalizes the output-affecting shape of a request into a stable
// sha256 hex digest — the {hash} segment of the Redis key.
//
// tenantID is folded into the hash (belt-and-suspenders with the key
// namespace), and messages MUST be the post-redaction array so a hit never
// depends on raw PII. Only fields that change the vendor's answer are included;
// request id, user id, timestamp, streaming flag and routing hints are excluded
// so genuinely-equivalent requests collapse to one entry.
//
// Determinism note: encoding/json is deterministic — struct fields in
// declaration order, map keys sorted — so identical inputs always yield an
// identical digest (docs/AIQG-PROMPT-CACHE-CONTROL.md §12.1). No manual sorting
// is needed.
func KeyHash(tenantID, vendor string, req *types.ChatRequest, scoringVersion string) string {
	type sig struct {
		Tenant           string           `json:"t"`
		Vendor           string           `json:"v"`
		Model            string           `json:"m"`
		Messages         []types.Message  `json:"msgs"`
		Temperature      *float32         `json:"temp,omitempty"`
		TopP             *float32         `json:"top_p,omitempty"`
		MaxTokens        *int             `json:"max_tokens,omitempty"`
		FrequencyPenalty *float32         `json:"fp,omitempty"`
		PresencePenalty  *float32         `json:"pp,omitempty"`
		Stop             []string         `json:"stop,omitempty"`
		Seed             *int             `json:"seed,omitempty"`
		ResponseFormat   any              `json:"rf,omitempty"`
		Tools            []types.Tool     `json:"tools,omitempty"`
		Functions        []types.Function `json:"funcs,omitempty"`
		Scoring          string           `json:"sc"`
	}
	s := sig{
		Tenant: tenantID, Vendor: vendor, Model: req.Model, Messages: req.Messages,
		Temperature: req.Temperature, TopP: req.TopP, MaxTokens: req.MaxTokens,
		FrequencyPenalty: req.FrequencyPenalty, PresencePenalty: req.PresencePenalty,
		Stop: req.Stop, Seed: req.Seed, ResponseFormat: req.ResponseFormat,
		Tools: req.Tools, Functions: req.Functions, Scoring: scoringVersion,
	}
	b, _ := json.Marshal(s)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Decision is the eligibility verdict for one request.
type Decision struct {
	// Cacheable: the request may be looked up and, on a miss, stored.
	Cacheable bool
	// Bypass: the caller explicitly opted out (TAS-Cache: off|bypass). Distinct
	// from "not eligible" so the event can stamp cache_state=bypass.
	Bypass bool
	// Reason is a short human tag for the not-cacheable case (logging/debug).
	Reason string
}

// Decide applies the C1 eligibility rules (docs/AIQG-CACHING.md §1 scope, §4).
// It does NOT consider experiment claims — that's a request-path concern the
// caller layers on (experiment-claimed ⇒ bypass, §7).
func Decide(req *types.ChatRequest, cfg Config, header string) Decision {
	switch strings.ToLower(strings.TrimSpace(header)) {
	case "off", "bypass", "no-cache", "no-store":
		return Decision{Bypass: true, Reason: "header"}
	}
	if !cfg.Enabled {
		return Decision{Reason: "disabled"}
	}
	if req.Stream {
		return Decision{Reason: "streaming"} // never cache streaming (v1)
	}
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		return Decision{Reason: "tools"} // tool/function calls are side-effecting
	}
	if cfg.RequireDeterministic && !isDeterministic(req) {
		return Decision{Reason: "nondeterministic"}
	}
	return Decision{Cacheable: true}
}

// isDeterministic is true when the request pins its sampling: temperature
// explicitly 0, or a seed set. A nil temperature means the vendor default
// (typically 1.0), so it is treated as non-deterministic.
func isDeterministic(req *types.ChatRequest) bool {
	if req.Seed != nil {
		return true
	}
	return req.Temperature != nil && *req.Temperature == 0
}
