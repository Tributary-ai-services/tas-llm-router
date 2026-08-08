// Package credentials resolves a tenant's stored BYOK vendor key (Plan #14)
// from aiqg-dashboard-be's internal endpoint, with a short-TTL in-memory cache
// to avoid a round-trip per request. The plaintext key is held only transiently
// (in the cache for the TTL, and per-request to inject upstream) — never
// persisted, never logged. See aether-shared/data-models/aiqg/provider-credentials.md §5.
package credentials

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Resolution is the effective per-(tenant,provider) BYOK state.
type Resolution struct {
	Found               bool
	APIKey              string // decrypted; empty when Found is false
	CredentialID        string
	AllowSharedFallback bool // whether the tenant permits falling back to the TAS shared key
}

// Resolver is an HTTP client of /internal/credentials/resolve with a TTL cache.
type Resolver struct {
	HTTP              *http.Client
	BaseURL           string
	InternalAuthToken string
	TTL               time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	res Resolution
	exp time.Time
}

// NewResolver builds a resolver. ttl<=0 defaults to 60s. A short TTL keeps
// plaintext keys in memory only briefly and lets a newly-added/rotated key take
// effect quickly.
func NewResolver(baseURL, internalAuthToken string, ttl time.Duration) *Resolver {
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return &Resolver{
		HTTP:              &http.Client{Timeout: 2 * time.Second},
		BaseURL:           strings.TrimRight(baseURL, "/"),
		InternalAuthToken: internalAuthToken,
		TTL:               ttl,
		cache:             make(map[string]cacheEntry),
	}
}

type resolveResponse struct {
	Found               bool   `json:"found"`
	APIKey              string `json:"api_key"`
	CredentialID        string `json:"credential_id"`
	AllowSharedFallback bool   `json:"allow_shared_fallback"`
}

// Resolve returns the effective BYOK state for (tenant, provider). Cached
// results (both found and not-found) are served within the TTL. On any
// transport/decoding error the caller decides degradation (typically: fall back
// to the per-request header or the TAS shared key) — Resolve returns the error
// so the caller can log once.
func (r *Resolver) Resolve(ctx context.Context, tenantID, provider string) (Resolution, error) {
	key := tenantID + "|" + provider
	now := time.Now()

	r.mu.Lock()
	if e, ok := r.cache[key]; ok && now.Before(e.exp) {
		r.mu.Unlock()
		return e.res, nil
	}
	r.mu.Unlock()

	u := fmt.Sprintf("%s/internal/credentials/resolve?tenant=%s&provider=%s",
		r.BaseURL, url.QueryEscape(tenantID), url.QueryEscape(provider))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Resolution{}, err
	}
	req.Header.Set("Internal-Auth", r.InternalAuthToken)
	resp, err := r.HTTP.Do(req)
	if err != nil {
		return Resolution{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return Resolution{}, fmt.Errorf("credentials: dashboard returned status %d", resp.StatusCode)
	}
	var out resolveResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Resolution{}, fmt.Errorf("credentials: decode: %w", err)
	}
	res := Resolution{
		Found:               out.Found,
		APIKey:              out.APIKey,
		CredentialID:        out.CredentialID,
		AllowSharedFallback: out.AllowSharedFallback,
	}
	r.mu.Lock()
	r.cache[key] = cacheEntry{res: res, exp: now.Add(r.TTL)}
	r.mu.Unlock()
	return res, nil
}
