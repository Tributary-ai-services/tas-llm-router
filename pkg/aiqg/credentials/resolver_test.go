package credentials

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolver_FoundAndCached(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("Internal-Auth") != "tok" {
			t.Errorf("missing Internal-Auth header")
		}
		if r.URL.Query().Get("tenant") != "t-1" || r.URL.Query().Get("provider") != "openai" {
			t.Errorf("bad query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"found":true,"api_key":"sk-x","credential_id":"c1","allow_shared_fallback":false}`))
	}))
	defer srv.Close()

	r := NewResolver(srv.URL, "tok", time.Minute)
	res, err := r.Resolve(context.Background(), "t-1", "openai")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !res.Found || res.APIKey != "sk-x" || res.CredentialID != "c1" || res.AllowSharedFallback {
		t.Errorf("unexpected: %+v", res)
	}
	// Second call within TTL → served from cache, no extra HTTP call.
	if _, err := r.Resolve(context.Background(), "t-1", "openai"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected 1 HTTP call (cached), got %d", got)
	}
}

func TestResolver_NotFoundCarriesFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"found":false,"allow_shared_fallback":true}`))
	}))
	defer srv.Close()
	res, err := NewResolver(srv.URL, "tok", time.Minute).Resolve(context.Background(), "t-1", "anthropic")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Found || res.APIKey != "" || !res.AllowSharedFallback {
		t.Errorf("want not-found + fallback, got %+v", res)
	}
}

func TestResolver_ErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := NewResolver(srv.URL, "tok", time.Minute).Resolve(context.Background(), "t-1", "openai"); err == nil {
		t.Fatal("want error on 500")
	}
}

func TestResolver_TTLExpiry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"found":true,"api_key":"sk","allow_shared_fallback":false}`))
	}))
	defer srv.Close()
	r := NewResolver(srv.URL, "tok", 20*time.Millisecond)
	_, _ = r.Resolve(context.Background(), "t", "openai")
	time.Sleep(40 * time.Millisecond)
	_, _ = r.Resolve(context.Background(), "t", "openai")
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected re-fetch after TTL, got %d calls", got)
	}
}
