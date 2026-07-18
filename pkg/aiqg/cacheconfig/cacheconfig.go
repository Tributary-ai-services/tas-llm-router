// Package cacheconfig resolves per-tenant cache overrides from aiqg-dashboard-be
// and applies them over the gateway's global cache defaults. It is the safe,
// per-tenant enablement path for C4 semantic SERVING (a global flip would turn
// probabilistic hits on for everyone at once). Mirrors pkg/aiqg/experiments: a
// TTL cache over an HTTP loader against the dashboard's /internal endpoint.
//
// The governing rule everywhere: a nil field means "use the gateway's global
// default", so a tenant can override just one knob and inherit the rest.
package cacheconfig

import (
	"context"
	"sync"
	"time"
)

// Config is a tenant's resolved cache overrides (mirrors the dashboard-be
// aiqg.cache_config spec). Every field is a pointer so absent = global default.
type Config struct {
	Exact    *ExactConfig    `json:"exact,omitempty"`
	Semantic *SemanticConfig `json:"semantic,omitempty"`
	Judge    *JudgeConfig    `json:"judge,omitempty"`
}

// ExactConfig overrides the C1 exact-match response cache.
type ExactConfig struct {
	Enabled    *bool `json:"enabled,omitempty"`
	TTLSeconds *int  `json:"ttl_seconds,omitempty"`
}

// SemanticConfig overrides the C4 semantic cache. Shadow=false means SERVE
// (probabilistic hits) — the correctness-sensitive opt-in.
type SemanticConfig struct {
	Enabled       *bool    `json:"enabled,omitempty"`
	Shadow        *bool    `json:"shadow,omitempty"`
	MinSimilarity *float64 `json:"min_similarity,omitempty"`
}

// JudgeConfig overrides the C4 L3 async-judge sampling for this tenant.
type JudgeConfig struct {
	Enabled    *bool    `json:"enabled,omitempty"`
	SampleRate *float64 `json:"sample_rate,omitempty"`
	DailyUSD   *float64 `json:"daily_usd,omitempty"`
}

// --- effective-value helpers: apply a tenant override over a global default ---

// ExactEnabled returns the effective C1 on/off for this tenant.
func (c *Config) ExactEnabled(global bool) bool {
	if c != nil && c.Exact != nil && c.Exact.Enabled != nil {
		return *c.Exact.Enabled
	}
	return global
}

// ExactTTL returns the effective C1 TTL, or the global default when unset/invalid.
func (c *Config) ExactTTL(global time.Duration) time.Duration {
	if c != nil && c.Exact != nil && c.Exact.TTLSeconds != nil && *c.Exact.TTLSeconds > 0 {
		return time.Duration(*c.Exact.TTLSeconds) * time.Second
	}
	return global
}

// SemanticEnabled returns the effective C4 on/off (whether the cascade runs at all).
func (c *Config) SemanticEnabled(global bool) bool {
	if c != nil && c.Semantic != nil && c.Semantic.Enabled != nil {
		return *c.Semantic.Enabled
	}
	return global
}

// SemanticShadow returns the effective shadow mode. True = log-only (serve
// nothing); false = SERVE semantic hits. The global default is shadow (safe).
func (c *Config) SemanticShadow(global bool) bool {
	if c != nil && c.Semantic != nil && c.Semantic.Shadow != nil {
		return *c.Semantic.Shadow
	}
	return global
}

// SemanticMinSimilarity returns the effective L1 threshold. A per-request
// override may only TIGHTEN (raise) the floor — a tenant must not be able to
// widen its own false-hit rate (§10.2) — so the max of override and global wins.
func (c *Config) SemanticMinSimilarity(global float64) float64 {
	if c != nil && c.Semantic != nil && c.Semantic.MinSimilarity != nil {
		if v := *c.Semantic.MinSimilarity; v > global {
			return v
		}
	}
	return global
}

// JudgeEnabled returns the effective L3-judge on/off for this tenant.
func (c *Config) JudgeEnabled(global bool) bool {
	if c != nil && c.Judge != nil && c.Judge.Enabled != nil {
		return *c.Judge.Enabled
	}
	return global
}

// JudgeSampleRate returns the effective judge sample rate.
func (c *Config) JudgeSampleRate(global float64) float64 {
	if c != nil && c.Judge != nil && c.Judge.SampleRate != nil {
		return *c.Judge.SampleRate
	}
	return global
}

// Loader fetches a tenant's cache config. Implementations wrap the dashboard's
// internal endpoint; the interface keeps the resolver testable.
type Loader interface {
	ForTenant(ctx context.Context, tenantID string) (*Config, error)
}

// Resolver caches each tenant's config (TTL). Bounds dashboard load and the
// worst-case staleness after an edit. Nil-safe: a nil *Resolver returns a nil
// Config (all global defaults), so callers need no branch.
type Resolver struct {
	loader Loader
	ttl    time.Duration
	mu     sync.Mutex
	cache  map[string]cacheEntry
	now    func() time.Time // injectable for tests
}

type cacheEntry struct {
	cfg       *Config
	expiresAt time.Time
}

// NewResolver wraps a Loader with a TTL cache. ttl<=0 defaults to 30s (matches
// the experiments + policy kill-switch propagation target).
func NewResolver(loader Loader, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &Resolver{loader: loader, ttl: ttl, cache: map[string]cacheEntry{}, now: time.Now}
}

// ForTenant returns the tenant's cache config, or nil when none is set / the
// tenant is empty / the dashboard is unreachable and nothing is cached. It NEVER
// fails — a broken config path degrades to the gateway's global defaults, never
// to a wrong cache decision. A nil return is a valid "all defaults" answer.
func (r *Resolver) ForTenant(ctx context.Context, tenantID string) *Config {
	if r == nil || tenantID == "" {
		return nil
	}
	now := r.now()
	r.mu.Lock()
	if e, ok := r.cache[tenantID]; ok && now.Before(e.expiresAt) {
		cfg := e.cfg
		r.mu.Unlock()
		return cfg
	}
	r.mu.Unlock()

	cfg, err := r.loader.ForTenant(ctx, tenantID)
	if err != nil {
		// Degrade: serve any stale cache, else nil (global defaults). Never fail.
		r.mu.Lock()
		stale := r.cache[tenantID].cfg
		r.mu.Unlock()
		return stale
	}
	r.mu.Lock()
	r.cache[tenantID] = cacheEntry{cfg: cfg, expiresAt: now.Add(r.ttl)}
	r.mu.Unlock()
	return cfg
}
