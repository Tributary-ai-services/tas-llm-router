package tokens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DashboardResolver resolves tas_qg_live_* bearers by calling
// aiqg-dashboard-be's POST /internal/auth/validate endpoint. Replaces
// the K8s-Secret-mounted MapResolver for any deployment where the
// dashboard backend is reachable.
//
// HTTP semantic mapping (must match aiqg-dashboard-be PR #3):
//
//	200 → populated Token
//	401 → ErrTokenNotFound  ("token_unknown")
//	403 → ErrTokenSuspended (still returns the Token so the upstream
//	      middleware can log the resolved tenant for triage)
//	5xx / network errors → generic error → middleware emits 503
//	      "token_resolver_unavailable" to the customer
//
// 2s HTTP timeout — this call is on the hot path of every AIQG
// request, but the original 500ms budget was too tight: a cold
// dashboard-be replica (BestEffort QoS, fresh PG connection pool)
// could need ~500-1000ms just to round-trip the validate call,
// returning 503 token_resolver_unavailable to the customer even
// though dashboard-be was healthy. Bumped on 2026-06-06 after
// observed 503s on production smoke traffic. Keep-alive on the
// shared http.Transport keeps subsequent calls fast (~5-15ms in
// cluster), so the 2s ceiling only applies to first-after-rollout
// requests.
type DashboardResolver struct {
	HTTP              *http.Client
	BaseURL           string
	InternalAuthToken string
}

// NewDashboardResolver builds a Resolver pointed at the configured
// dashboard-be base URL. baseURL is everything before /internal/auth/validate
// — typically "http://aiqg-dashboard-be.aiqg.svc.cluster.local:8095" in
// cluster.
//
// The internalAuthToken is the shared secret required by the
// /internal/* route group; the dashboard-be middleware does a
// constant-time compare against this value.
func NewDashboardResolver(baseURL, internalAuthToken string) (*DashboardResolver, error) {
	if baseURL == "" {
		return nil, errors.New("tokens.NewDashboardResolver: baseURL required")
	}
	if internalAuthToken == "" {
		return nil, errors.New("tokens.NewDashboardResolver: internalAuthToken required (else dashboard-be rejects)")
	}
	return &DashboardResolver{
		HTTP: &http.Client{
			// 2s — see the DashboardResolver doc comment for the
			// "why bumped from 500ms" rationale. Steady-state cluster
			// roundtrip is ~5-15ms; this only really applies to the
			// first call after a dashboard-be replica wakes up.
			Timeout: 2 * time.Second,
		},
		BaseURL:           baseURL,
		InternalAuthToken: internalAuthToken,
	}, nil
}

// validateRequest and validateResponse mirror the shapes in
// aiqg-dashboard-be/internal/handlers/internal_auth.go. Kept in this
// repo verbatim rather than imported to avoid a circular dep (the
// dashboard repo doesn't import this one, but importing it here would
// pull in Gin etc. just for two struct definitions).
type validateRequest struct {
	Bearer string `json:"bearer"`
}

type validateResponse struct {
	TokenID       string `json:"token_id"`
	TenantID      string `json:"tenant_id"`
	AIQGAccountID string `json:"aiqg_account_id"`
	SourceApp     string `json:"source_app"`
	Suspended     bool   `json:"suspended"`
}

// Resolve calls the dashboard-be validate endpoint and converts its
// response to the Resolver contract. Always sends a JSON body even
// for empty bearer (the upstream middleware never calls with empty,
// so the early ErrTokenNotFound short-circuit here is defense in depth).
func (r *DashboardResolver) Resolve(ctx context.Context, bearer string) (*Token, error) {
	if bearer == "" {
		return nil, ErrTokenNotFound
	}

	body, err := json.Marshal(validateRequest{Bearer: bearer})
	if err != nil {
		return nil, fmt.Errorf("tokens.DashboardResolver: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.BaseURL+"/internal/auth/validate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tokens.DashboardResolver: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Internal-Auth", r.InternalAuthToken)

	resp, err := r.HTTP.Do(req)
	if err != nil {
		// Network failure, DNS, timeout — all surface as transport
		// errors that the upstream middleware logs + maps to 503.
		return nil, fmt.Errorf("tokens.DashboardResolver: do: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var v validateResponse
		if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
			return nil, fmt.Errorf("tokens.DashboardResolver: decode 200: %w", err)
		}
		return &Token{
			TokenID:       v.TokenID,
			TenantID:      v.TenantID,
			AIQGAccountID: v.AIQGAccountID,
			SourceApp:     v.SourceApp,
			Suspended:     v.Suspended,
		}, nil

	case http.StatusUnauthorized:
		// dashboard-be returns 401 for both "token_unknown" and
		// "internal_auth_invalid" / "internal_auth_missing". The
		// latter two are misconfig on OUR side — surface them with
		// a distinct log so the operator can fix without thinking
		// it's a customer-bearer issue.
		body, _ := io.ReadAll(resp.Body)
		if bodyMentions(body, "internal_auth") {
			return nil, fmt.Errorf("tokens.DashboardResolver: dashboard-be rejected Internal-Auth (check INTERNAL_AUTH_TOKEN config): %s", string(body))
		}
		return nil, ErrTokenNotFound

	case http.StatusForbidden:
		// Suspended account — surface the resolved Token alongside
		// the error so the middleware can log the resolved tenant.
		var partial struct {
			TokenID       string `json:"token_id"`
			TenantID      string `json:"tenant_id"`
			AIQGAccountID string `json:"aiqg_account_id"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&partial)
		return &Token{
			TokenID:       partial.TokenID,
			TenantID:      partial.TenantID,
			AIQGAccountID: partial.AIQGAccountID,
			Suspended:     true,
		}, ErrTokenSuspended

	default:
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("tokens.DashboardResolver: unexpected status %d: %s",
			resp.StatusCode, string(body))
	}
}

// bodyMentions is a substring check used to disambiguate the two
// reasons dashboard-be returns 401. Keep tight to literal field
// names from the dashboard-be JSON shape.
func bodyMentions(body []byte, needle string) bool {
	for i := 0; i+len(needle) <= len(body); i++ {
		if string(body[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}
