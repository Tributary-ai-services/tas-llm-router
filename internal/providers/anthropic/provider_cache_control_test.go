package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
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

// The hole the August 2026 smoke tests left: tool_use was verified
// non-streaming, streaming was verified text-only, and the COMBINATION — which
// is all a coding agent ever does — was never exercised. A streamed tool call
// was therefore dropped in silence until Claude Code hit it on 2026-10-05.
//
// The events are built by unmarshalling the JSON Anthropic actually puts on the
// wire, because the SDK's union re-parses each variant from the raw JSON it was
// decoded from — a struct literal yields an empty variant and would make these
// tests pass against any implementation, including the broken one.
func streamEvent(t *testing.T, raw string) anthropic.MessageStreamEventUnion {
	t.Helper()
	var ev anthropic.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	return ev
}

func TestConvertStreamEvent_ToolUseBlockStartCarriesIDAndName(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_start","index":0,
		"content_block":{"type":"tool_use","id":"toolu_abc","name":"read_file","input":{}}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("content_block_start for a tool_use was dropped — the id and name arrive ONLY here")
	}
	tcs := c.Choices[0].Delta.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(tcs))
	}
	if tcs[0].ID != "toolu_abc" || tcs[0].Function.Name != "read_file" {
		t.Errorf("id/name lost: %+v", tcs[0])
	}
}

func TestConvertStreamEvent_InputJSONDeltaContinuesTheCall(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_delta","index":0,
		"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"calc.py\"}"}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("input_json_delta was dropped — the tool call would arrive with no arguments")
	}
	tcs := c.Choices[0].Delta.ToolCalls
	if len(tcs) != 1 || tcs[0].Function.Arguments != `{"path":"calc.py"}` {
		t.Fatalf("argument fragment lost: %+v", tcs)
	}
	// The empty id is load-bearing: it is how the encoders know this continues
	// the current call instead of opening a second one (bufferTool).
	if tcs[0].ID != "" {
		t.Errorf("fragment must carry an empty id to continue the open call, got %q", tcs[0].ID)
	}
}

func TestConvertStreamEvent_MessageStartCarriesInputTokens(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"message_start","message":{"id":"msg_1",
		"type":"message","role":"assistant","model":"claude-sonnet-5-5","content":[],
		"usage":{"input_tokens":397,"output_tokens":0,"cache_read_input_tokens":52829}}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil || c.Usage == nil {
		t.Fatal("message_start carried no usage — input tokens arrive only here, so the client reads 0 for the whole stream")
	}
	if c.Usage.PromptTokens != 397 || c.Usage.CacheReadTokens != 52829 {
		t.Errorf("usage not carried: %+v", c.Usage)
	}
}

// A text delta must still carry its text, and must not be mistaken for a tool call.
func TestConvertStreamEvent_TextDeltaUnaffected(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_delta","index":0,
		"delta":{"type":"text_delta","text":"pong"}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("text delta dropped")
	}
	if got, _ := c.Choices[0].Delta.Content.(string); got != "pong" {
		t.Errorf("text = %q, want pong", got)
	}
	if len(c.Choices[0].Delta.ToolCalls) != 0 {
		t.Error("a text delta must not produce a tool call")
	}
}

// RT-7: streamed reasoning. Claude Code always streams, so without these the
// thinking RT-6 enabled reaches the vendor and never comes back to the client
// — and the signature is what makes a turn replayable, so losing it on the
// streaming path loses interleaved thinking on the NEXT turn, silently.
//
// Built from the wire JSON Anthropic actually sends, not SDK struct literals:
// the SDK re-parses each union variant from unexported raw JSON, so a literal
// passes against a converter that handles nothing (the #242 lesson).
func TestConvertStreamEvent_ThinkingDeltaCarriesText(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_delta","index":0,
		"delta":{"type":"thinking_delta","thinking":"Let me check the file first."}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("thinking_delta was dropped — the client receives no reasoning at all")
	}
	r := c.Choices[0].Delta.Reasoning
	if len(r) != 1 || r[0].Type != "thinking" || r[0].Thinking != "Let me check the file first." {
		t.Fatalf("thinking text lost: %+v", r)
	}
}

func TestConvertStreamEvent_SignatureDeltaCarriesSignature(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_delta","index":0,
		"delta":{"type":"signature_delta","signature":"ErUBCkYIBRgCIkDx3a"}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("signature_delta was dropped — the block cannot be replayed without it")
	}
	r := c.Choices[0].Delta.Reasoning
	if len(r) != 1 || r[0].Signature != "ErUBCkYIBRgCIkDx3a" {
		t.Fatalf("signature lost: %+v", r)
	}
}

// Redacted reasoning arrives complete in the start event with no deltas after,
// so it is forwarded there or not at all.
func TestConvertStreamEvent_RedactedThinkingBlockStart(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_start","index":0,
		"content_block":{"type":"redacted_thinking","data":"EroBCkYIBRgCKkBc"}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c == nil {
		t.Fatal("redacted_thinking start was dropped — no delta follows it, so it is lost for good")
	}
	r := c.Choices[0].Delta.Reasoning
	if len(r) != 1 || r[0].Type != "redacted_thinking" || r[0].Data != "EroBCkYIBRgCKkBc" {
		t.Fatalf("redacted payload lost: %+v", r)
	}
}

// A thinking block's own start event carries nothing but an empty string, so
// it must NOT open anything — the encoder opens lazily on the first delta, the
// same way it does for text. Emitting a chunk here would open an empty block.
func TestConvertStreamEvent_ThinkingBlockStartIsNotAChunk(t *testing.T) {
	p := createTestProvider(t)
	c := p.convertStreamEvent(streamEvent(t, `{"type":"content_block_start","index":0,
		"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		&types.ChatRequest{Model: "claude-sonnet-5-5"}, &anthropic.Message{ID: "msg_1"})
	if c != nil {
		t.Errorf("thinking block_start produced a chunk; it carries no content: %+v", c.Choices[0].Delta)
	}
}
