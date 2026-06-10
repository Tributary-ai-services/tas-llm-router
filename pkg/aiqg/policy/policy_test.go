package policy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubServer returns an httptest.Server that responds with status +
// body for every /internal/policy/resolve call. The test asserts on
// request shape and on the Resolution/error the client returns.
func stubServer(t *testing.T, status int, body string, captureBearer *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/policy/resolve" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if captureBearer != nil {
			*captureBearer = r.Header.Get("Internal-Auth")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestDashboardResolver_OK(t *testing.T) {
	srv := stubServer(t, http.StatusOK,
		`{"bundle_id":"b1","bundle_name":"prod-strict","source":"explicit"}`, nil)
	defer srv.Close()

	r, err := NewDashboardResolver(srv.URL, "secret")
	if err != nil {
		t.Fatalf("NewDashboardResolver: %v", err)
	}
	res, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t1"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.BundleID != "b1" || res.BundleName != "prod-strict" || res.Source != "explicit" {
		t.Errorf("got %+v", res)
	}
}

func TestDashboardResolver_404IsBundleNotFound(t *testing.T) {
	srv := stubServer(t, http.StatusNotFound, `{}`, nil)
	defer srv.Close()
	r, _ := NewDashboardResolver(srv.URL, "secret")
	_, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t1"})
	if !errors.Is(err, ErrBundleNotFound) {
		t.Errorf("err = %v want ErrBundleNotFound", err)
	}
}

func TestDashboardResolver_400IsBadRequest(t *testing.T) {
	srv := stubServer(t, http.StatusBadRequest, `{}`, nil)
	defer srv.Close()
	r, _ := NewDashboardResolver(srv.URL, "secret")
	_, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t1"})
	if !errors.Is(err, ErrResolverBadRequest) {
		t.Errorf("err = %v want ErrResolverBadRequest", err)
	}
}

func TestDashboardResolver_InternalAuthHeader(t *testing.T) {
	var seen string
	srv := stubServer(t, http.StatusOK,
		`{"bundle_id":"","bundle_name":"(default)","source":"default"}`, &seen)
	defer srv.Close()
	r, _ := NewDashboardResolver(srv.URL, "expected-secret")
	if _, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t1"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if seen != "expected-secret" {
		t.Errorf("Internal-Auth header = %q want expected-secret", seen)
	}
}

func TestDashboardResolver_DegradesToDefaultOnError(t *testing.T) {
	// 5xx — neither ErrBundleNotFound nor ErrResolverBadRequest;
	// caller still gets a usable Default() resolution.
	srv := stubServer(t, http.StatusServiceUnavailable, `{}`, nil)
	defer srv.Close()
	r, _ := NewDashboardResolver(srv.URL, "secret")
	res, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t1"})
	if err == nil {
		t.Errorf("expected error for 503")
	}
	// The caller can still proceed with the returned Default()
	if res.Source != "default" || res.BundleName != "(default)" {
		t.Errorf("degraded resolution = %+v, want Default()", res)
	}
}

func TestDashboardResolver_RequiresTenantID(t *testing.T) {
	// Don't stand up a server — the resolver should fail fast on
	// missing tenant_id without making an HTTP call.
	r, _ := NewDashboardResolver("http://invalid", "secret")
	_, err := r.Resolve(context.Background(), ResolveRequest{TenantID: ""})
	if err == nil || !strings.Contains(err.Error(), "tenant_id required") {
		t.Errorf("err = %v want tenant_id required", err)
	}
}

func TestStaticResolver(t *testing.T) {
	want := Resolution{BundleID: "x", BundleName: "y", Source: "tenant_active"}
	r := StaticResolver{Result: want}
	got, err := r.Resolve(context.Background(), ResolveRequest{TenantID: "t"})
	if err != nil || got != want {
		t.Errorf("got %+v err=%v want %+v err=nil", got, err, want)
	}
}
