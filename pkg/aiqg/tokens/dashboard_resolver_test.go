package tokens

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDashboardResolver_Validation(t *testing.T) {
	if _, err := NewDashboardResolver("", "x"); err == nil {
		t.Errorf("expected error for empty baseURL")
	}
	if _, err := NewDashboardResolver("http://x", ""); err == nil {
		t.Errorf("expected error for empty internalAuthToken")
	}
}

func TestDashboardResolver_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/auth/validate" {
			t.Errorf("path=%q", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method=%q", r.Method)
		}
		if r.Header.Get("Internal-Auth") != "secret-x" {
			t.Errorf("Internal-Auth header missing or wrong: %q", r.Header.Get("Internal-Auth"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type=%q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"bearer":"tas_qg_live_abc"`) {
			t.Errorf("body doesn't contain bearer: %s", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_id":        "tok-1",
			"tenant_id":       "tenant-a",
			"aiqg_account_id": "account-a",
			"source_app":      "billing",
			"suspended":       false,
		})
	}))
	defer srv.Close()

	r, err := NewDashboardResolver(srv.URL, "secret-x")
	if err != nil {
		t.Fatalf("NewDashboardResolver: %v", err)
	}
	tok, err := r.Resolve(context.Background(), "tas_qg_live_abc")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if tok.TenantID != "tenant-a" || tok.AIQGAccountID != "account-a" || tok.TokenID != "tok-1" || tok.SourceApp != "billing" {
		t.Errorf("token mismatch: %#v", tok)
	}
}

func TestDashboardResolver_Unauthorized_TokenUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "token_unknown", "message": "bearer not recognized"},
		})
	}))
	defer srv.Close()

	r, _ := NewDashboardResolver(srv.URL, "x")
	_, err := r.Resolve(context.Background(), "tas_qg_live_unknown")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected ErrTokenNotFound, got %v", err)
	}
}

// dashboard-be also returns 401 when our Internal-Auth header is
// wrong — distinct from a customer "token_unknown". Disambiguate
// via the body so the operator sees the right error in logs.
func TestDashboardResolver_Unauthorized_InternalAuthInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"code": "internal_auth_invalid", "message": "Internal-Auth header did not match"},
		})
	}))
	defer srv.Close()

	r, _ := NewDashboardResolver(srv.URL, "x")
	_, err := r.Resolve(context.Background(), "tas_qg_live_x")
	if err == nil || errors.Is(err, ErrTokenNotFound) {
		t.Errorf("expected misconfig error, got %v", err)
	}
	if !strings.Contains(err.Error(), "Internal-Auth") {
		t.Errorf("error should mention Internal-Auth: %v", err)
	}
}

func TestDashboardResolver_Forbidden_Suspended(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":           map[string]string{"code": "account_suspended"},
			"token_id":        "tok-suspended",
			"tenant_id":       "tenant-b",
			"aiqg_account_id": "account-b",
		})
	}))
	defer srv.Close()

	r, _ := NewDashboardResolver(srv.URL, "x")
	tok, err := r.Resolve(context.Background(), "tas_qg_live_suspended")
	if !errors.Is(err, ErrTokenSuspended) {
		t.Fatalf("expected ErrTokenSuspended, got %v", err)
	}
	if tok == nil {
		t.Fatalf("Token must be populated even on suspended (for triage logging)")
	}
	if tok.TenantID != "tenant-b" || !tok.Suspended {
		t.Errorf("partial token mismatch: %#v", tok)
	}
}

func TestDashboardResolver_500_ReturnsGenericError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r, _ := NewDashboardResolver(srv.URL, "x")
	_, err := r.Resolve(context.Background(), "tas_qg_live_x")
	if err == nil {
		t.Fatalf("expected error on 500")
	}
	// Must NOT be ErrTokenNotFound / ErrTokenSuspended — upstream
	// middleware needs the distinction to choose 503 vs 401 vs 403.
	if errors.Is(err, ErrTokenNotFound) || errors.Is(err, ErrTokenSuspended) {
		t.Errorf("500 should not map to known errors, got %v", err)
	}
}

func TestDashboardResolver_NetworkError_ReturnsError(t *testing.T) {
	// Resolver pointed at an unreachable URL.
	r, _ := NewDashboardResolver("http://127.0.0.1:1/no-such-port", "x")
	r.HTTP.Timeout = 50 * time.Millisecond
	_, err := r.Resolve(context.Background(), "tas_qg_live_x")
	if err == nil {
		t.Errorf("expected network error")
	}
}

func TestDashboardResolver_EmptyBearer_ShortCircuits(t *testing.T) {
	r, _ := NewDashboardResolver("http://stub", "x")
	_, err := r.Resolve(context.Background(), "")
	if !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("empty bearer should short-circuit to ErrTokenNotFound, got %v", err)
	}
}

func TestDashboardResolver_TimeoutBoundsHotPath(t *testing.T) {
	r, _ := NewDashboardResolver("http://x", "y")
	if r.HTTP.Timeout > time.Second {
		t.Errorf("DashboardResolver timeout=%v should be sub-second (this is on the hot path)", r.HTTP.Timeout)
	}
}

// Compile-time assertion that DashboardResolver satisfies Resolver.
var _ Resolver = (*DashboardResolver)(nil)
