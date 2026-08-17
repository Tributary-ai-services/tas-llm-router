package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// CORS preflight must be answered for POST-only routes.
//
// Regression test for a failure that was silent in production for as long as
// the browser-facing endpoints existed. corsMiddleware was attached with
// gorilla/mux's r.Use(), which only fires on MATCHED routes. The completion
// routes are registered .Methods("POST"), so a browser's OPTIONS preflight
// matched nothing, fell through to the 404 handler, and came back with no
// Access-Control-* headers — which makes the browser block the real request
// before it is ever sent. The aiqg gateway ingress carries no CORS
// annotations either, so nothing upstream compensated.
//
// The fix wraps the whole router in Start(); these tests pin the behaviour
// that fix provides, independent of route registration.
func corsHandler() http.Handler {
	s := &Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		// POST-only, like the real route: anything else 404s from the router.
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return s.corsMiddleware(mux)
}

func TestCORSPreflightAnsweredOnPostOnlyRoute(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://aiqg.example.com")
	req.Header.Set("Access-Control-Request-Method", "POST")
	rec := httptest.NewRecorder()

	corsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200 — a non-200 preflight blocks the real request", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Error("preflight carried no Access-Control-Allow-Origin")
	}
}

// Preflight must also be answered on a path the router does not know, since an
// unmatched OPTIONS is exactly what used to 404 without headers.
func TestCORSPreflightAnsweredOnUnknownPath(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/v1/nonexistent", nil)
	req.Header.Set("Origin", "https://aiqg.example.com")
	rec := httptest.NewRecorder()

	corsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("preflight on unknown path = %d, want 200", rec.Code)
	}
}

func TestCORSExposesCacheVerdict(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://aiqg.example.com")
	rec := httptest.NewRecorder()

	corsHandler().ServeHTTP(rec, req)

	expose := rec.Header().Get("Access-Control-Expose-Headers")
	// X-TAS-Cache is the ONLY signal a browser has for hit vs miss — the cache
	// fields are not promoted into Loki labels either.
	if !strings.Contains(expose, "X-TAS-Cache") {
		t.Errorf("Access-Control-Expose-Headers lacks X-TAS-Cache: %q", expose)
	}
}

func TestCORSAllowsAttributionHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Origin", "https://aiqg.example.com")
	rec := httptest.NewRecorder()

	corsHandler().ServeHTTP(rec, req)

	allow := rec.Header().Get("Access-Control-Allow-Headers")
	// Without these a browser client can send traffic but not attribute it, so
	// its own requests show up unattributed in Traffic Explorer.
	for _, h := range []string{"TAS-Auth", "TAS-Agent-Id", "TAS-Flow-Id", "TAS-Conversation-Id", "baggage"} {
		if !strings.Contains(allow, h) {
			t.Errorf("Access-Control-Allow-Headers lacks %s: %q", h, allow)
		}
	}
}
