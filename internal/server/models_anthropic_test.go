package server

import (
	"net/http/httptest"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestAnthropicSDKRequest(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/v1/models", nil)
	if anthropicSDKRequest(r1) {
		t.Error("no anthropic-version header should not be detected as Anthropic SDK")
	}
	r2 := httptest.NewRequest("GET", "/v1/models", nil)
	r2.Header.Set("anthropic-version", "2023-06-01")
	if !anthropicSDKRequest(r2) {
		t.Error("anthropic-version header should be detected as Anthropic SDK")
	}
}

func TestAnthropicModels_FilterAndShape(t *testing.T) {
	caps := map[string]types.ProviderCapabilities{
		"openai": {ProviderName: "openai", SupportedModels: []types.ModelInfo{
			{Name: "gpt-4o"}, {Name: "gpt-4o-mini"},
		}},
		"anthropic": {ProviderName: "anthropic", SupportedModels: []types.ModelInfo{
			{Name: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
			{Name: "claude-haiku-4-5-20251001"},
		}},
	}
	s := &Server{}
	models := s.anthropicModels(caps)

	// Only Anthropic-owned models, sorted by id.
	if len(models) != 2 {
		t.Fatalf("expected 2 anthropic models, got %d: %+v", len(models), models)
	}
	for _, m := range models {
		if m.Type != "model" {
			t.Errorf("type = %q, want model", m.Type)
		}
		if m.DisplayName == "" {
			t.Errorf("display_name should fall back to id, got empty for %s", m.ID)
		}
	}
	if models[0].ID != "claude-haiku-4-5-20251001" {
		t.Errorf("not sorted by id: %+v", models)
	}
	// DisplayName carried through, and fallback to id where absent.
	if models[1].ID != "claude-sonnet-4-6" || models[1].DisplayName != "Claude Sonnet 4.6" {
		t.Errorf("display name not carried: %+v", models[1])
	}
	if models[0].DisplayName != "claude-haiku-4-5-20251001" {
		t.Errorf("display name fallback wrong: %+v", models[0])
	}
}
