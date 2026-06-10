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
	"time"
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
}

type resolveResponse struct {
	BundleID   string `json:"bundle_id"`
	BundleName string `json:"bundle_name"`
	Source     string `json:"source"`
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
		return Resolution{BundleID: v.BundleID, BundleName: v.BundleName, Source: v.Source}, nil
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
