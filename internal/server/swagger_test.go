package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
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
