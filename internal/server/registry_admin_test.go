package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/registry"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// regStubAdapter satisfies adapters.ProviderAdapter for the admin-endpoint tests.
type regStubAdapter struct {
	name   string
	models []types.ModelInfo
	valid  map[string]bool
}

func (a *regStubAdapter) GetProviderName() string { return a.name }
func (a *regStubAdapter) DiscoverModels(context.Context) ([]types.ModelInfo, error) {
	return a.models, nil
}
func (a *regStubAdapter) ValidateModel(_ context.Context, m string) (bool, error) {
	return a.valid[m], nil
}
func (a *regStubAdapter) ResolveAlias(_ context.Context, alias string) (string, error) {
	return alias, nil
}

func regLogger() *logrus.Logger {
	l := logrus.New()
	l.SetLevel(logrus.FatalLevel)
	return l
}

// regServer builds a Server with a registry pre-populated from a stub adapter.
func regServer(t *testing.T) *Server {
	t.Helper()
	log := regLogger()
	reg := registry.New(registry.NewMemoryStore(), log)
	reg.RegisterAdapter("anthropic", &regStubAdapter{
		name: "anthropic",
		models: []types.ModelInfo{
			{Name: "claude-sonnet-4-5", Status: types.ModelStatusActive},
			{Name: "claude-gone", Status: types.ModelStatusUnavailable},
		},
		valid: map[string]bool{"claude-sonnet-4-5": true},
	})
	eng := registry.NewSyncEngine(reg, registry.SyncConfig{Enabled: true}, log)
	s := &Server{logger: log}
	s.SetRegistry(reg, eng)
	if err := reg.SyncAll(context.Background()); err != nil {
		t.Fatalf("seed sync: %v", err)
	}
	return s
}

func TestRegistryAdmin_ListModels(t *testing.T) {
	s := regServer(t)
	rec := httptest.NewRecorder()
	s.handleListRegistryModels(rec, httptest.NewRequest(http.MethodGet, "/v1/registry/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Providers map[string][]types.ModelInfo `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Providers["anthropic"]) != 2 {
		t.Errorf("anthropic models = %d, want 2", len(body.Providers["anthropic"]))
	}
}

func TestRegistryAdmin_ListProviderModelsAnd404(t *testing.T) {
	s := regServer(t)

	rec := httptest.NewRecorder()
	req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/v1/registry/models/anthropic", nil), map[string]string{"provider": "anthropic"})
	s.handleListProviderModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("anthropic status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/v1/registry/models/nope", nil), map[string]string{"provider": "nope"})
	s.handleListProviderModels(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider status = %d, want 404", rec.Code)
	}
}

func TestRegistryAdmin_Status(t *testing.T) {
	s := regServer(t)
	rec := httptest.NewRecorder()
	s.handleRegistryStatus(rec, httptest.NewRequest(http.MethodGet, "/v1/registry/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "anthropic") {
		t.Errorf("status body missing provider: %s", rec.Body.String())
	}
}

func TestRegistryAdmin_SyncRateLimited(t *testing.T) {
	s := regServer(t)

	rec := httptest.NewRecorder()
	s.handleRegistrySync(rec, httptest.NewRequest(http.MethodPost, "/v1/registry/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first sync status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Immediate second call is rate-limited.
	rec = httptest.NewRecorder()
	s.handleRegistrySync(rec, httptest.NewRequest(http.MethodPost, "/v1/registry/sync", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second sync status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry Retry-After")
	}
}

func TestRegistryAdmin_Validate(t *testing.T) {
	s := regServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/registry/validate", strings.NewReader(`{"provider":"anthropic","model":"claude-sonnet-4-5"}`))
	s.handleValidateModel(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body struct {
		Available bool `json:"available"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !body.Available {
		t.Errorf("claude-sonnet-4-5 should validate: %s", rec.Body.String())
	}

	// Bad body → 400.
	rec = httptest.NewRecorder()
	s.handleValidateModel(rec, httptest.NewRequest(http.MethodPost, "/v1/registry/validate", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty body status = %d, want 400", rec.Code)
	}
}

// Every endpoint returns 503 when the registry is disabled (not wired).
func TestRegistryAdmin_DisabledReturns503(t *testing.T) {
	s := &Server{logger: regLogger()}
	for _, h := range []http.HandlerFunc{
		s.handleListRegistryModels, s.handleRegistryStatus, s.handleRegistrySync,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/v1/registry/x", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("disabled endpoint status = %d, want 503", rec.Code)
		}
	}
}
