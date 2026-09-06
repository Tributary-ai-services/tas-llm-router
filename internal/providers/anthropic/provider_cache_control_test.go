package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// #100: the gateway advertises cache_control passthrough, but before this the
// Anthropic provider threaded ONLY the system-block breakpoint — a
// cache_control on a tool, a content part, or a non-system message reached the
// type and was then silently dropped, so passthrough failed for exactly the
// agentic turns where prompt caching pays most. These tests marshal the
// converted request the way the SDK sends it and assert the breakpoint survives
// on each surface.

func ephemeral() *types.CacheControl { return &types.CacheControl{Type: "ephemeral"} }

func TestAnthropicProvider_ToolCacheControlThreaded(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []types.Message{{Role: "user", Content: "hi"}},
		Tools: []types.Tool{{
			Type: "function",
			Function: types.Function{
				Name:        "get_weather",
				Description: "Get weather",
				Parameters:  map[string]interface{}{"type": "object"},
			},
			CacheControl: ephemeral(),
		}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Tools[0])
	js := string(b)
	if !strings.Contains(js, `"cache_control"`) || !strings.Contains(js, "ephemeral") {
		t.Fatalf("tool cache_control not threaded:\n%s", js)
	}
}

func TestAnthropicProvider_MessageCacheControlThreaded(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []types.Message{
			{Role: "user", Content: "cache through here", CacheControl: ephemeral()},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages[0])
	if !strings.Contains(string(b), `"cache_control"`) {
		t.Fatalf("message-level cache_control not threaded:\n%s", b)
	}
}

func TestAnthropicProvider_ContentPartCacheControlThreaded(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []types.Message{{
			Role: "user",
			Content: []types.ContentPart{
				{Type: "text", Text: "large stable context", CacheControl: ephemeral()},
				{Type: "text", Text: "the volatile question"},
			},
		}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.Messages[0])
	if !strings.Contains(string(b), `"cache_control"`) {
		t.Fatalf("content-part cache_control not threaded:\n%s", b)
	}
}

// The system-block breakpoint that already worked must keep working.
func TestAnthropicProvider_SystemCacheControlStillThreaded(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []types.Message{
			{Role: "system", Content: "big stable system prompt", CacheControl: ephemeral()},
			{Role: "user", Content: "hi"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got.System)
	if !strings.Contains(string(b), `"cache_control"`) {
		t.Fatalf("system cache_control not threaded:\n%s", b)
	}
}

// Regression: nothing requested → no breakpoint anywhere. A stray cache_control
// would 400 on models below the minimum and quietly change caching behaviour.
func TestAnthropicProvider_NoCacheControlByDefault(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-3-5-sonnet-20241022",
		Messages: []types.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
		},
		Tools: []types.Tool{{
			Type:     "function",
			Function: types.Function{Name: "t", Parameters: map[string]interface{}{"type": "object"}},
		}},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "cache_control") {
		t.Fatalf("unexpected cache_control when none was requested:\n%s", b)
	}
}
