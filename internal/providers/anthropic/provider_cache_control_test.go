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

// The other half of the 2026-10-03 finding: a multi-block system prompt has to
// reach the vendor as multiple blocks. Merged into one, Claude Code's leading
// `x-anthropic-billing-header:` marker consumes the whole block and the real
// prompt never arrives — measured at 6,318 tokens counted against 14 billed.
func TestAnthropicProvider_SystemBlocksStayBlocks(t *testing.T) {
	const marker = "x-anthropic-billing-header: cc_version=2.1.288.7d4; cc_entrypoint=sdk-cli;"
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "system", Content: []types.ContentPart{
				{Type: "text", Text: marker},
				{Type: "text", Text: "You are a Claude agent.", CacheControl: ephemeral()},
				{Type: "text", Text: "Long operating instructions.", CacheControl: ephemeral()},
			}},
			{Role: "user", Content: "hi"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got.System) != 3 {
		t.Fatalf("want 3 system blocks on the wire, got %d — merging is the bug", len(got.System))
	}
	if got.System[0].Text != marker {
		t.Errorf("block 0 must carry the marker alone, got %q", got.System[0].Text)
	}
	if strings.Contains(got.System[0].Text, "operating instructions") {
		t.Error("instructions merged into the marker block: the prompt would be consumed with it")
	}
	b, _ := json.Marshal(got.System)
	if n := strings.Count(string(b), `"cache_control"`); n != 2 {
		t.Errorf("want 2 breakpoints across the system blocks, got %d:\n%s", n, b)
	}
}

// A system message that arrives as []interface{} (the JSON round-trip shape)
// must not degrade to "must be text only".
func TestAnthropicProvider_SystemBlocksAfterJSONRoundTrip(t *testing.T) {
	provider := createTestProvider(t)
	raw := []interface{}{
		map[string]interface{}{"type": "text", "text": "first"},
		map[string]interface{}{"type": "text", "text": "second",
			"cache_control": map[string]interface{}{"type": "ephemeral"}},
	}
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "system", Content: raw},
			{Role: "user", Content: "hi"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(got.System) != 2 {
		t.Fatalf("want 2 system blocks, got %d", len(got.System))
	}
	b, _ := json.Marshal(got.System)
	if !strings.Contains(string(b), `"cache_control"`) {
		t.Errorf("breakpoint lost on the round-trip shape:\n%s", b)
	}
}

// A message-level breakpoint on a block-form system prompt lands on the LAST
// block, which is what "cache through the end of my system prompt" means.
func TestAnthropicProvider_SystemBlocksMessageLevelBreakpointOnLast(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "system", CacheControl: ephemeral(), Content: []types.ContentPart{
				{Type: "text", Text: "one"},
				{Type: "text", Text: "two"},
			}},
			{Role: "user", Content: "hi"},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	b0, _ := json.Marshal(got.System[0])
	b1, _ := json.Marshal(got.System[1])
	if strings.Contains(string(b0), "cache_control") {
		t.Errorf("breakpoint on the wrong block:\n%s", b0)
	}
	if !strings.Contains(string(b1), "cache_control") {
		t.Errorf("message-level breakpoint did not reach the last block:\n%s", b1)
	}
}
