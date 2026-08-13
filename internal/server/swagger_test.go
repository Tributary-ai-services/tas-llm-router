package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gorilla/mux"
)

// TestHandleOpenAPISpecFromAnyWorkingDirectory guards the regression that made
// /docs/openapi.yaml return 404 in the container: the spec used to be read from
// a path relative to the process working directory.
func TestHandleOpenAPISpecFromAnyWorkingDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	s := &Server{}

	t.Run("yaml", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleOpenAPISpec(rec, httptest.NewRequest(http.MethodGet, "/docs/openapi.yaml", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Content-Type"); got != "text/yaml" {
			t.Errorf("Content-Type = %q, want %q", got, "text/yaml")
		}
		if body := rec.Body.String(); !strings.HasPrefix(body, "openapi: 3.0.3") {
			t.Errorf("body does not start with the OpenAPI version, got %.40q", body)
		}
	})

	t.Run("json", func(t *testing.T) {
		rec := httptest.NewRecorder()
		s.handleOpenAPISpec(rec, httptest.NewRequest(http.MethodGet, "/docs/openapi.json", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want %q", got, "application/json")
		}

		// yaml.v2 decoded mappings as map[interface{}]interface{}, which
		// json.Marshal rejects — the JSON route never actually worked.
		var spec struct {
			OpenAPI string                     `json:"openapi"`
			Paths   map[string]json.RawMessage `json:"paths"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("response is not valid JSON: %v", err)
		}
		if spec.OpenAPI == "" {
			t.Error("openapi version missing from JSON spec")
		}
		for _, path := range []string{"/v1/chat/completions", "/metrics", "/aiqg/metrics"} {
			if _, ok := spec.Paths[path]; !ok {
				t.Errorf("%s missing from JSON spec", path)
			}
		}
	})
}

// TestSwaggerIndexHasNoBlockedResources guards the regression that left the
// page rendering its header and nothing else: the service sets
// `default-src 'self'`, so the CDN <link>/<script> tags and the inline
// bootstrap script the page used to carry were all blocked by the browser.
func TestSwaggerIndexHasNoBlockedResources(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).serveSwaggerIndex(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	body := rec.Body.String()
	if strings.Contains(body, "unpkg.com") || strings.Contains(body, "//cdn.") {
		t.Error("page references a third-party origin, which default-src 'self' blocks")
	}
	for _, tag := range []string{"<script>", "<style>"} {
		if strings.Contains(body, tag) {
			t.Errorf("page carries an inline %s, which the CSP blocks", tag)
		}
	}
	for _, asset := range []string{"/docs/ui/swagger-ui.css", "/docs/ui/custom.css", "/docs/ui/swagger-ui-bundle.js", "/docs/ui/init.js"} {
		if !strings.Contains(body, asset) {
			t.Errorf("page does not reference %s", asset)
		}
	}

	// The page must override the service-wide policy, or the style
	// attributes Swagger UI renders are dropped.
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self' 'unsafe-inline'") {
		t.Errorf("unexpected CSP: %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Error("CSP allows inline scripts; the bundle is self-hosted, so it should not need to")
	}
}

// TestSwaggerUIAssetsAreServed checks the vendored bundle is reachable at the
// paths the page asks for, and that the catch-all route does not shadow it.
func TestSwaggerUIAssetsAreServed(t *testing.T) {
	t.Chdir(t.TempDir())

	router := mux.NewRouter()
	(&Server{}).setupSwaggerRoutes(router)

	for path, wantType := range map[string]string{
		"/docs/ui/swagger-ui-bundle.js": "text/javascript",
		"/docs/ui/init.js":              "text/javascript",
		"/docs/ui/swagger-ui.css":       "text/css",
		"/docs/ui/custom.css":           "text/css",
	} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, wantType) {
				t.Errorf("Content-Type = %q, want prefix %q", got, wantType)
			}
			if rec.Body.Len() == 0 {
				t.Error("empty body")
			}
			if strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
				t.Error("served the index page instead of the asset — catch-all route shadows /docs/ui/")
			}
		})
	}
}

// TestEmbeddedSpecIsValid keeps the embedded spec loadable by the validation
// middleware, which parses the same bytes at startup.
func TestEmbeddedSpecIsValid(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handleOpenAPISpec(rec, httptest.NewRequest(http.MethodGet, "/docs/openapi.yaml", nil))

	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("embedded spec failed to load: %v", err)
	}
	if err := doc.Validate(loader.Context); err != nil {
		t.Fatalf("embedded spec is not a valid OpenAPI document: %v", err)
	}
}
