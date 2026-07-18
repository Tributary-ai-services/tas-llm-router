package cacheconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPLoader loads per-tenant cache config from aiqg-dashboard-be's internal
// endpoint (GET /internal/cache-config?tenant=). Reuses the same base URL +
// Internal-Auth token as the policy/experiments resolvers.
type HTTPLoader struct {
	HTTP              *http.Client
	BaseURL           string
	InternalAuthToken string
}

// NewHTTPLoader builds a loader against the dashboard.
func NewHTTPLoader(baseURL, internalAuthToken string) *HTTPLoader {
	return &HTTPLoader{
		HTTP:              &http.Client{Timeout: 2 * time.Second},
		BaseURL:           strings.TrimRight(baseURL, "/"),
		InternalAuthToken: internalAuthToken,
	}
}

// cacheConfigResponse is the endpoint envelope: {tenant_id, cache_config: {...}}.
type cacheConfigResponse struct {
	TenantID    string  `json:"tenant_id"`
	CacheConfig *Config `json:"cache_config"`
}

// ForTenant fetches the tenant's config. A 200 with an empty spec decodes to a
// non-nil, all-nil Config (every field falls back to the global default).
func (l *HTTPLoader) ForTenant(ctx context.Context, tenantID string) (*Config, error) {
	url := l.BaseURL + "/internal/cache-config?tenant=" + tenantID
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
		return nil, fmt.Errorf("cacheconfig: dashboard returned status %d", resp.StatusCode)
	}
	var out cacheConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("cacheconfig: decode response: %w", err)
	}
	return out.CacheConfig, nil
}
