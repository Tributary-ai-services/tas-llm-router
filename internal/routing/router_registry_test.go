package routing

import (
	"errors"
	"testing"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// stubReg implements routing.ModelRegistry for the resolution tests.
type stubReg struct {
	aliasModel map[string]string          // alias -> target model
	aliasProv  map[string]string          // alias -> provider
	modelProv  map[string]string          // model -> provider
	info       map[string]types.ModelInfo // "provider/model" -> entry
	fallback   map[string]types.ModelInfo // "provider/model" -> fallback
}

func (s *stubReg) ResolveAliasAny(alias string) (string, string, bool) {
	if m, ok := s.aliasModel[alias]; ok {
		return m, s.aliasProv[alias], true
	}
	return "", "", false
}
func (s *stubReg) FindProvider(model string) (string, bool) {
	p, ok := s.modelProv[model]
	return p, ok
}
func (s *stubReg) GetModel(provider, model string) (*types.ModelInfo, error) {
	if m, ok := s.info[provider+"/"+model]; ok {
		return &m, nil
	}
	return nil, errors.New("not found")
}
func (s *stubReg) GetFallback(provider, model string) (*types.ModelInfo, error) {
	if m, ok := s.fallback[provider+"/"+model]; ok {
		return &m, nil
	}
	return nil, errors.New("no fallback")
}

func regRouter(t *testing.T, reg ModelRegistry) *Router {
	t.Helper()
	log := logrus.New()
	log.SetLevel(logrus.FatalLevel)
	r := NewRouter(log)
	if reg != nil {
		r.SetRegistry(reg)
	}
	return r
}

func TestRegistry_ResolvesAlias(t *testing.T) {
	r := regRouter(t, &stubReg{
		aliasModel: map[string]string{"smart": "claude-sonnet-4-5"},
		aliasProv:  map[string]string{"smart": "anthropic"},
		info:       map[string]types.ModelInfo{"anthropic/claude-sonnet-4-5": {Name: "claude-sonnet-4-5", Status: types.ModelStatusActive}},
	})
	req := &types.ChatRequest{Model: "smart"}
	res := r.resolveViaRegistry(req)

	if req.Model != "claude-sonnet-4-5" {
		t.Errorf("req.Model = %q, want claude-sonnet-4-5", req.Model)
	}
	if res.original != "smart" || res.resolved != "claude-sonnet-4-5" {
		t.Errorf("resolution = %+v", res)
	}
	if res.fellBack {
		t.Error("alias resolution should not be a fallback")
	}
}

func TestRegistry_FallsBackOnUnavailable(t *testing.T) {
	r := regRouter(t, &stubReg{
		modelProv: map[string]string{"claude-gone": "anthropic"},
		info:      map[string]types.ModelInfo{"anthropic/claude-gone": {Name: "claude-gone", Status: types.ModelStatusUnavailable}},
		fallback:  map[string]types.ModelInfo{"anthropic/claude-gone": {Name: "claude-sonnet-4-5", Status: types.ModelStatusActive}},
	})
	req := &types.ChatRequest{Model: "claude-gone"}
	res := r.resolveViaRegistry(req)

	if req.Model != "claude-sonnet-4-5" {
		t.Errorf("req.Model = %q, want the fallback claude-sonnet-4-5", req.Model)
	}
	if !res.fellBack || res.fallbackReason != "model_unavailable" {
		t.Errorf("resolution = %+v, want fellBack + model_unavailable", res)
	}
	if res.resolved != "claude-sonnet-4-5" {
		t.Errorf("resolved = %q", res.resolved)
	}
}

// A deprecated model still answers, so it is served unchanged (no surprise
// substitution) — only a warning.
func TestRegistry_DeprecatedServesUnchanged(t *testing.T) {
	r := regRouter(t, &stubReg{
		modelProv: map[string]string{"claude-3-5-sonnet": "anthropic"},
		info:      map[string]types.ModelInfo{"anthropic/claude-3-5-sonnet": {Name: "claude-3-5-sonnet", Status: types.ModelStatusDeprecated, ReplacementModel: "claude-sonnet-4-5"}},
		fallback:  map[string]types.ModelInfo{"anthropic/claude-3-5-sonnet": {Name: "claude-sonnet-4-5"}},
	})
	req := &types.ChatRequest{Model: "claude-3-5-sonnet"}
	res := r.resolveViaRegistry(req)

	if req.Model != "claude-3-5-sonnet" {
		t.Errorf("deprecated model was substituted: req.Model = %q", req.Model)
	}
	if res.fellBack || res.resolved != "" {
		t.Errorf("deprecated must not fall back: %+v", res)
	}
}

// A model the registry does not know is left exactly as-is — the safety
// guarantee that registry traffic never affects unknown models.
func TestRegistry_UnknownModelUnchanged(t *testing.T) {
	r := regRouter(t, &stubReg{})
	req := &types.ChatRequest{Model: "some-model-nobody-knows"}
	res := r.resolveViaRegistry(req)

	if req.Model != "some-model-nobody-knows" {
		t.Errorf("unknown model changed: %q", req.Model)
	}
	if res.resolved != "" || res.fellBack {
		t.Errorf("unknown model produced a resolution: %+v", res)
	}
}

// No registry configured (the default) → complete no-op.
func TestRegistry_NilIsNoOp(t *testing.T) {
	r := regRouter(t, nil)
	req := &types.ChatRequest{Model: "anything"}
	res := r.resolveViaRegistry(req)
	if req.Model != "anything" || res.resolved != "" || res.fellBack {
		t.Errorf("nil registry was not a no-op: model=%q res=%+v", req.Model, res)
	}
}

// A known, active model routes unchanged.
func TestRegistry_ActiveModelUnchanged(t *testing.T) {
	r := regRouter(t, &stubReg{
		modelProv: map[string]string{"gpt-4o": "openai"},
		info:      map[string]types.ModelInfo{"openai/gpt-4o": {Name: "gpt-4o", Status: types.ModelStatusActive}},
	})
	req := &types.ChatRequest{Model: "gpt-4o"}
	res := r.resolveViaRegistry(req)
	if req.Model != "gpt-4o" || res.resolved != "" || res.fellBack {
		t.Errorf("active model changed: model=%q res=%+v", req.Model, res)
	}
}
