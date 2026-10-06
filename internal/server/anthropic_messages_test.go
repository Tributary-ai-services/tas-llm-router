package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestParseAnthropicToChatRequest_Basic(t *testing.T) {
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"max_tokens": 256,
		"temperature": 0.5,
		"system": "You are terse.",
		"stop_sequences": ["STOP"],
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "claude-3-5-sonnet" {
		t.Errorf("model = %q", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 256 {
		t.Errorf("max_tokens not carried: %+v", req.MaxTokens)
	}
	if req.Temperature == nil || *req.Temperature != 0.5 {
		t.Errorf("temperature not carried")
	}
	if len(req.Stop) != 1 || req.Stop[0] != "STOP" {
		t.Errorf("stop_sequences not carried: %v", req.Stop)
	}
	// system becomes a leading system message
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Fatalf("expected leading system message, got %+v", req.Messages)
	}
	if req.Messages[0].Content != "You are terse." {
		t.Errorf("system content = %v", req.Messages[0].Content)
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "Hello" {
		t.Errorf("user message = %+v", req.Messages[1])
	}
}

func TestParseAnthropicToChatRequest_RequiresMaxTokens(t *testing.T) {
	body := []byte(`{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if _, err := parseAnthropicToChatRequest(body, true); err == nil {
		t.Fatal("expected error for missing max_tokens")
	}
}

func TestParseAnthropicToChatRequest_RequiresModel(t *testing.T) {
	body := []byte(`{"max_tokens":10,"messages":[]}`)
	if _, err := parseAnthropicToChatRequest(body, true); err == nil {
		t.Fatal("expected error for missing model")
	}
}

func TestParseAnthropicToChatRequest_ToolsAndChoice(t *testing.T) {
	body := []byte(`{
		"model":"claude","max_tokens":50,
		"messages":[{"role":"user","content":"weather?"}],
		"tools":[{"name":"get_weather","description":"get it","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"any"}
	}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" || req.Tools[0].Type != "function" {
		t.Fatalf("tool not translated: %+v", req.Tools)
	}
	if req.ToolChoice != "required" { // "any" -> "required"
		t.Errorf("tool_choice = %v, want required", req.ToolChoice)
	}
}

func TestAnthropicMessage_ToolResultFansOut(t *testing.T) {
	// A user turn carrying a tool_result + text should produce a role=tool
	// message (first) then a user message.
	m := anthropicWireMessage{
		Role: "user",
		Content: json.RawMessage(`[
			{"type":"tool_result","tool_use_id":"toolu_1","content":"42"},
			{"type":"text","text":"thanks"}
		]`),
	}
	out, err := anthropicMessageToInternal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(out), out)
	}
	if out[0].Role != "tool" || out[0].ToolCallID != "toolu_1" || out[0].Content != "42" {
		t.Errorf("tool message wrong: %+v", out[0])
	}
	if out[1].Role != "user" || out[1].Content != "thanks" {
		t.Errorf("user message wrong: %+v", out[1])
	}
}

func TestAnthropicMessage_AssistantToolUse(t *testing.T) {
	m := anthropicWireMessage{
		Role: "assistant",
		Content: json.RawMessage(`[
			{"type":"text","text":"let me check"},
			{"type":"tool_use","id":"toolu_9","name":"lookup","input":{"q":"x"}}
		]`),
	}
	out, err := anthropicMessageToInternal(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 message, got %d", len(out))
	}
	if out[0].Role != "assistant" || out[0].Content != "let me check" {
		t.Errorf("assistant content wrong: %+v", out[0])
	}
	if len(out[0].ToolCalls) != 1 || out[0].ToolCalls[0].ID != "toolu_9" || out[0].ToolCalls[0].Function.Name != "lookup" {
		t.Errorf("tool_call wrong: %+v", out[0].ToolCalls)
	}
}

func TestChatResponseToAnthropic_Text(t *testing.T) {
	resp := &types.ChatResponse{
		ID:    "chatcmpl-123",
		Model: "claude-3-5-sonnet",
		Choices: []types.Choice{
			{Message: types.Message{Role: "assistant", Content: "Hi there"}, FinishReason: "stop"},
		},
		Usage: &types.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	msg := chatResponseToAnthropic(resp)
	if msg.Type != "message" || msg.Role != "assistant" {
		t.Errorf("envelope wrong: %+v", msg)
	}
	if !strings.HasPrefix(msg.ID, "msg_") {
		t.Errorf("id not msg_ prefixed: %s", msg.ID)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" || msg.Content[0].Text != "Hi there" {
		t.Errorf("content wrong: %+v", msg.Content)
	}
	if msg.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if msg.Usage.InputTokens != 10 || msg.Usage.OutputTokens != 5 {
		t.Errorf("usage wrong: %+v", msg.Usage)
	}
}

func TestChatResponseToAnthropic_ToolUse(t *testing.T) {
	resp := &types.ChatResponse{
		ID:    "chatcmpl-xyz",
		Model: "claude",
		Choices: []types.Choice{
			{
				Message: types.Message{
					Role: "assistant",
					ToolCalls: []types.ToolCall{
						{ID: "toolu_1", Type: "function", Function: types.Function{Name: "get_weather", Arguments: `{"city":"SF"}`}},
					},
				},
				FinishReason: "tool_calls",
			},
		},
	}
	msg := chatResponseToAnthropic(resp)
	if msg.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" {
		t.Fatalf("content wrong: %+v", msg.Content)
	}
	if msg.Content[0].Name != "get_weather" || string(msg.Content[0].Input) != `{"city":"SF"}` {
		t.Errorf("tool_use block wrong: %+v", msg.Content[0])
	}
}

func TestFinishReasonToAnthropic(t *testing.T) {
	cases := map[string]string{
		"stop":           "end_turn",
		"length":         "max_tokens",
		"tool_calls":     "tool_use",
		"content_filter": "end_turn",
		"":               "end_turn",
	}
	for in, want := range cases {
		if got := finishReasonToAnthropic(in, false); got != want {
			t.Errorf("finishReasonToAnthropic(%q) = %q, want %q", in, got, want)
		}
	}
	if got := finishReasonToAnthropic("stop", true); got != "tool_use" {
		t.Errorf("ended-on-tool-use should be tool_use, got %q", got)
	}
}

func TestAnthropicStreamEncoder_TextStream(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude"}, nil)

	// two text deltas then a finishing chunk
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Model: "claude", Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "Hel"}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "lo"}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "stop"}}, Usage: &types.Usage{CompletionTokens: 2}})
	enc.done()

	out := rec.Body.String()
	// Expected event ordering for a native Anthropic text stream.
	wantOrder := []string{
		"event: message_start",
		"event: content_block_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	}
	pos := 0
	for _, w := range wantOrder {
		idx := strings.Index(out[pos:], w)
		if idx < 0 {
			t.Fatalf("missing/out-of-order event %q in:\n%s", w, out)
		}
		pos += idx + len(w)
	}
	// no OpenAI framing
	if strings.Contains(out, "[DONE]") {
		t.Error("anthropic stream must not emit [DONE]")
	}
	// text_delta content present
	if !strings.Contains(out, `"text":"Hel"`) || !strings.Contains(out, `"text":"lo"`) {
		t.Errorf("text deltas missing:\n%s", out)
	}
	// stop_reason end_turn on message_delta
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("stop_reason missing:\n%s", out)
	}
	// every data line must be valid JSON
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			var v interface{}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Errorf("invalid JSON data line %q: %v", line, err)
			}
		}
	}
}

func TestAnthropicStreamEncoder_ToolUseBuffered(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-2", Model: "claude", Choices: []types.ChoiceChunk{
		{Delta: &types.Message{ToolCalls: []types.ToolCall{{ID: "toolu_1", Function: types.Function{Name: "f", Arguments: `{"a":`}}}}},
	}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{
		{Delta: &types.Message{ToolCalls: []types.ToolCall{{ID: "toolu_1", Function: types.Function{Arguments: `1}`}}}}, FinishReason: "tool_calls"},
	}})
	enc.done()
	out := rec.Body.String()
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, `"name":"f"`) {
		t.Errorf("tool_use block missing:\n%s", out)
	}
	if !strings.Contains(out, `"partial_json":"{\"a\":1}"`) {
		t.Errorf("accumulated tool args missing:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) {
		t.Errorf("stop_reason tool_use missing:\n%s", out)
	}
}

func TestOpenAIStreamEncoder_UnchangedFraming(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := &openAIStreamEncoder{w: rec}
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-3", Object: "chat.completion.chunk", Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "hi"}}}})
	// Terminal finish chunk WITHOUT a delta — providers emit these; the OpenAI
	// SDK's .stream() helper crashes on a choice with no delta.
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-3", Object: "chat.completion.chunk", Choices: []types.ChoiceChunk{{FinishReason: "stop"}}})
	enc.done()
	out := rec.Body.String()
	if !strings.Contains(out, "data: ") || !strings.Contains(out, "[DONE]") {
		t.Errorf("openai framing wrong:\n%s", out)
	}
	if strings.Contains(out, "event: ") {
		t.Errorf("openai stream must not use named events:\n%s", out)
	}
	// Every emitted choice must carry a delta object (helper calls delta.to_dict()).
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		var chunk struct {
			Choices []map[string]interface{} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
			t.Fatalf("bad chunk json: %v", err)
		}
		for _, ch := range chunk.Choices {
			if _, ok := ch["delta"]; !ok {
				t.Errorf("choice missing delta (breaks OpenAI .stream()): %s", line)
			}
		}
	}
}

// #172: a mid-stream failure must be representable on the wire in each dialect.
func TestStreamEncoders_WriteError(t *testing.T) {
	se := &types.StreamError{Message: "vendor exploded", Type: "upstream_stream_error"}

	// OpenAI dialect: an error frame, then the [DONE] sentinel, no named events.
	rec := httptest.NewRecorder()
	(&openAIStreamEncoder{w: rec}).writeError(se)
	out := rec.Body.String()
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, "vendor exploded") {
		t.Errorf("openai error frame missing:\n%s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Errorf("openai error must still terminate with [DONE]:\n%s", out)
	}
	if strings.Contains(out, "event: ") {
		t.Errorf("openai must not use named events:\n%s", out)
	}

	// Anthropic dialect: a native `error` event, never [DONE].
	rec2 := httptest.NewRecorder()
	newAnthropicStreamEncoder(rec2, nil, &types.ChatRequest{Model: "claude"}, nil).writeError(se)
	out2 := rec2.Body.String()
	if !strings.Contains(out2, "event: error") || !strings.Contains(out2, "vendor exploded") {
		t.Errorf("anthropic error event missing:\n%s", out2)
	}
	if strings.Contains(out2, "[DONE]") {
		t.Errorf("anthropic must not emit [DONE]:\n%s", out2)
	}
	for _, line := range strings.Split(out2, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var v interface{}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
				t.Errorf("invalid JSON data line %q: %v", line, err)
			}
		}
	}
}

// TestParseAnthropic_SystemBlocksPreserved is the regression test for the
// measured 2026-10-03 failure: Claude Code's system prompt vanished because the
// three blocks it sends were concatenated into one, putting the vendor's own
// `x-anthropic-billing-header:` marker at the head of the merged block and
// handing the real instructions to that marker's consumption. 6,318 tokens in,
// 14 billed, HTTP 200, plausible answer, nothing logged.
func TestParseAnthropic_SystemBlocksPreserved(t *testing.T) {
	const marker = "x-anthropic-billing-header: cc_version=2.1.288.7d4; cc_entrypoint=sdk-cli;"
	body := []byte(`{
		"model": "claude-haiku-4-5-20251001",
		"max_tokens": 64,
		"system": [
			{"type": "text", "text": "` + marker + `"},
			{"type": "text", "text": "You are a Claude agent.", "cache_control": {"type": "ephemeral", "ttl": "1h"}},
			{"type": "text", "text": "Long operating instructions.", "cache_control": {"type": "ephemeral", "ttl": "1h"}}
		],
		"messages": [{"role": "user", "content": "hi"}]
	}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Messages[0].Role != "system" {
		t.Fatalf("first message should be the system prompt, got %q", req.Messages[0].Role)
	}
	parts, ok := req.Messages[0].Content.([]types.ContentPart)
	if !ok {
		t.Fatalf("system content must stay a block array, got %T (flattening is the bug)", req.Messages[0].Content)
	}
	if len(parts) != 3 {
		t.Fatalf("want 3 system blocks, got %d", len(parts))
	}
	// The marker must remain ALONE in its own block — that is the whole fix.
	if parts[0].Text != marker {
		t.Errorf("block 0 must be exactly the marker, got %q", parts[0].Text)
	}
	if parts[0].CacheControl != nil {
		t.Errorf("block 0 carried no cache_control; one was invented")
	}
	for _, i := range []int{1, 2} {
		if parts[i].CacheControl == nil {
			t.Errorf("block %d lost its cache_control (full price on every turn)", i)
		} else if parts[i].CacheControl.TTL != "1h" {
			t.Errorf("block %d TTL = %q, want 1h carried through", i, parts[i].CacheControl.TTL)
		}
	}
}

func TestParseAnthropic_SystemSingleBlockStaysString(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":8,"system":[{"type":"text","text":"be brief"}],
		"messages":[{"role":"user","content":"hi"}]}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, ok := req.Messages[0].Content.(string); !ok || got != "be brief" {
		t.Errorf("a single plain block should stay a string, got %#v", req.Messages[0].Content)
	}
}

func TestParseAnthropic_ToolAndMessageCacheControl(t *testing.T) {
	body := []byte(`{
		"model": "m", "max_tokens": 8,
		"tools": [{"name":"t","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}],
		"messages": [
			{"role": "user", "content": [
				{"type":"text","text":"first"},
				{"type":"text","text":"last","cache_control":{"type":"ephemeral"}}
			]},
			{"role": "user", "content": [{"type":"text","text":"plain only"}]}
		]
	}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Tools[0].CacheControl == nil {
		t.Error("tool breakpoint dropped — the tool block is the biggest repeated prefix there is")
	}
	parts, ok := req.Messages[0].Content.([]types.ContentPart)
	if !ok {
		t.Fatalf("a turn with a breakpoint must keep its blocks, got %T", req.Messages[0].Content)
	}
	if parts[0].CacheControl != nil || parts[1].CacheControl == nil {
		t.Error("breakpoint moved: position is the meaning")
	}
	// No breakpoint anywhere → the cheap string form is still used.
	if _, ok := req.Messages[1].Content.(string); !ok {
		t.Errorf("breakpoint-free turn should stay a string, got %T", req.Messages[1].Content)
	}
}

// The vendor was caching and the client could not see it: events recorded
// cache_creation 37,341 then cache_read 25,894 per turn on 2026-10-05 while
// Claude Code reported cache_read_input_tokens: 0, because message_start
// rendered only input and output tokens. A client that cannot see its cache
// reads cannot reason about its own cost.
func TestAnthropicStream_CacheTokensReachTheClient(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)

	// Usage on the FIRST chunk, as the provider now sends it from message_start.
	enc.writeChunk(&types.ChatChunk{
		ID: "msg_1", Model: "claude-sonnet-5-5",
		Usage:   &types.Usage{PromptTokens: 474, CacheCreationTokens: 21, CacheReadTokens: 25894},
		Choices: []types.ChoiceChunk{{Delta: &types.Message{Role: "assistant"}}},
	})
	enc.writeChunk(&types.ChatChunk{
		Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "pong"}}},
	})
	enc.done()

	out := rec.Body.String()
	var start map[string]interface{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Fatalf("invalid JSON in stream: %v", err)
		}
		if v["type"] == "message_start" {
			start = v["message"].(map[string]interface{})
		}
	}
	if start == nil {
		t.Fatal("no message_start emitted")
	}
	u, ok := start["usage"].(map[string]interface{})
	if !ok {
		t.Fatal("message_start carried no usage")
	}
	for field, want := range map[string]float64{
		"input_tokens":                474,
		"cache_creation_input_tokens": 21,
		"cache_read_input_tokens":     25894,
	} {
		got, present := u[field]
		if !present {
			t.Errorf("message_start usage is missing %s — the client cannot distinguish absent from zero", field)
			continue
		}
		if got.(float64) != want {
			t.Errorf("message_start usage %s = %v, want %v", field, got, want)
		}
	}
}

// Cache fields must be PRESENT even at zero, so a client never has to guess
// whether the field is missing or the value is genuinely nothing.
func TestAnthropicStream_CacheFieldsPresentWhenZero(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "msg_2", Model: "claude-sonnet-5-5",
		Usage:   &types.Usage{PromptTokens: 10},
		Choices: []types.ChoiceChunk{{Delta: &types.Message{Role: "assistant"}}}})
	enc.done()
	out := rec.Body.String()
	if !strings.Contains(out, `"cache_creation_input_tokens":0`) ||
		!strings.Contains(out, `"cache_read_input_tokens":0`) {
		t.Errorf("zero cache fields must still be emitted in message_start:\n%s", out)
	}
}

// Extended thinking (RT-6). Claude Code asks for it on every request with a
// 31,999-token budget; before this the field was discarded by JSON decoding, so
// the gateway served a measurably weaker model than the caller configured and
// said nothing. These assert the boundary: the ask survives, and a replayed
// thinking block keeps the signature that makes it replayable.
func TestParseAnthropic_ThinkingRequestSurvives(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-5-5","max_tokens":64000,
		"thinking":{"type":"enabled","budget_tokens":31999,"display":"omitted"},
		"messages":[{"role":"user","content":"hi"}]}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.Thinking == nil {
		t.Fatal("thinking was dropped at the boundary — the caller's ask is invisible from here on")
	}
	if req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens != 31999 {
		t.Errorf("thinking not carried faithfully: %+v", req.Thinking)
	}
	if req.Thinking.Display != "omitted" {
		t.Errorf("display lost: %+v", req.Thinking)
	}
}

func TestParseAnthropic_ReplayedThinkingKeepsSignature(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"step one","signature":"sig-abc"},
			{"type":"redacted_thinking","data":"opaque-xyz"},
			{"type":"text","text":"answer"}
		]},
		{"role":"user","content":"and then?"}
	]}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	parts, ok := req.Messages[1].Content.([]types.ContentPart)
	if !ok {
		t.Fatalf("a turn carrying reasoning must keep its blocks, got %T", req.Messages[1].Content)
	}
	var think, redacted *types.ContentPart
	for i := range parts {
		switch parts[i].Type {
		case "thinking":
			think = &parts[i]
		case "redacted_thinking":
			redacted = &parts[i]
		}
	}
	if think == nil {
		t.Fatal("thinking block dropped on replay")
	}
	// The signature is the whole point: the vendor rejects an unsigned block, so
	// forwarding one without it would be worse than dropping it.
	if think.Signature != "sig-abc" || think.Thinking != "step one" {
		t.Errorf("thinking block not preserved verbatim: %+v", think)
	}
	if redacted == nil || redacted.Data != "opaque-xyz" {
		t.Errorf("redacted_thinking payload not round-tripped: %+v", redacted)
	}
}

func TestChatResponseToAnthropic_EmitsReasoningBeforeText(t *testing.T) {
	resp := &types.ChatResponse{
		ID: "chatcmpl-1", Model: "claude-sonnet-5-5",
		Choices: []types.Choice{{
			FinishReason: "stop",
			Message: types.Message{
				Role:    "assistant",
				Content: "the answer",
				Reasoning: []types.ContentPart{
					{Type: "thinking", Thinking: "", Signature: "sig-1"},
				},
			},
		}},
	}
	msg := chatResponseToAnthropic(resp)
	if len(msg.Content) != 2 {
		t.Fatalf("want reasoning + text, got %d blocks: %+v", len(msg.Content), msg.Content)
	}
	if msg.Content[0].Type != "thinking" {
		t.Errorf("reasoning must come first, got %q", msg.Content[0].Type)
	}
	// Empty thinking text with a signature is the NORMAL case on 4.7+ models
	// (display defaults to "omitted"), so the block must survive on the strength
	// of its signature alone.
	if msg.Content[0].Signature != "sig-1" {
		t.Errorf("signature lost in the response: %+v", msg.Content[0])
	}
	if msg.Content[1].Type != "text" || msg.Content[1].Text != "the answer" {
		t.Errorf("answer block wrong: %+v", msg.Content[1])
	}
}

// RT-7: the encoder renders streamed reasoning as real Anthropic block events.
//
// eventSeq extracts the (event, block-type-or-delta-type, index) sequence, so
// the assertions are about ORDER and BLOCK STRUCTURE — which is where this can
// go wrong in ways that still look like valid SSE.
func eventSeq(body string) []string {
	var seq []string
	for _, frame := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		var p struct {
			Index        *int `json:"index"`
			ContentBlock *struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta *struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		_ = json.Unmarshal([]byte(data), &p)
		label := name
		switch {
		case p.ContentBlock != nil:
			label += ":" + p.ContentBlock.Type
		case p.Delta != nil && p.Delta.Type != "":
			label += ":" + p.Delta.Type
		}
		if p.Index != nil {
			label += fmt.Sprintf("#%d", *p.Index)
		}
		seq = append(seq, label)
	}
	return seq
}

func TestAnthropicStreamEncoder_ThinkingPrecedesTextAndCloses(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)

	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Thinking: "Check the file "}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Thinking: "before editing."}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Signature: "sig-abc"}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "Done."}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "stop"}}})
	enc.done()

	got := eventSeq(rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start:thinking#0",
		"content_block_delta:thinking_delta#0",
		"content_block_delta:thinking_delta#0",
		"content_block_delta:signature_delta#0",
		// The reasoning block MUST close before the answer opens: two open
		// blocks at once leave the client unable to attribute a delta.
		"content_block_stop#0",
		"content_block_start:text#1",
		"content_block_delta:text_delta#1",
		"content_block_stop#1",
		"message_delta",
		"message_stop",
	}
	if len(got) != len(want) {
		t.Fatalf("event count %d, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q\nfull: %v", i, got[i], want[i], got)
		}
	}
	if !strings.Contains(rec.Body.String(), "sig-abc") {
		t.Error("signature never reached the wire — the turn cannot be replayed")
	}
}

// The normal case on the 4.7+ generation: display defaults to "omitted", so
// there is no thinking TEXT at all and the block is nothing but a signature.
// It must still produce a complete, replayable block.
func TestAnthropicStreamEncoder_SignatureOnlyStillOpensABlock(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)

	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Signature: "sig-only"}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "Answer."}}}})
	enc.done()

	got := eventSeq(rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start:thinking#0",
		"content_block_delta:signature_delta#0",
		"content_block_stop#0",
		"content_block_start:text#1",
		"content_block_delta:text_delta#1",
		"content_block_stop#1",
		"message_delta",
		"message_stop",
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("event %d wrong\n got: %v\nwant: %v", i, got, want)
		}
	}
}

// A reasoning-only turn never opens a text block, so done() is the only place
// its block can be closed. Leaving it open ends the stream mid-block.
func TestAnthropicStreamEncoder_ReasoningOnlyTurnClosesAtDone(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Thinking: "Thinking...", Signature: ""}}}}}})
	enc.done()

	got := eventSeq(rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start:thinking#0",
		"content_block_delta:thinking_delta#0",
		"content_block_stop#0",
		"message_delta",
		"message_stop",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Redacted reasoning is complete in one event, so it is emitted as a whole
// block rather than opened and streamed — and it must not swallow the answer.
func TestAnthropicStreamEncoder_RedactedThinkingIsAWholeBlock(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "redacted_thinking", Data: "opaque-xyz"}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "Answer."}}}})
	enc.done()

	body := rec.Body.String()
	got := eventSeq(body)
	want := []string{
		"message_start",
		"content_block_start:redacted_thinking#0",
		"content_block_stop#0",
		"content_block_start:text#1",
		"content_block_delta:text_delta#1",
		"content_block_stop#1",
		"message_delta",
		"message_stop",
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Fatalf("event %d wrong\n got: %v\nwant: %v", i, got, want)
		}
	}
	if !strings.Contains(body, "opaque-xyz") {
		t.Error("redacted payload never reached the wire; it must round-trip verbatim")
	}
}

// Tool calls are buffered to done() and must land AFTER the reasoning block,
// with no index collision — the two features share nextIndex.
func TestAnthropicStreamEncoder_ThinkingAndToolCallsDoNotCollide(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newAnthropicStreamEncoder(rec, nil, &types.ChatRequest{Model: "claude-sonnet-5-5"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Choices: []types.ChoiceChunk{{Delta: &types.Message{
		Reasoning: []types.ContentPart{{Type: "thinking", Signature: "sig-1"}}}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{
		ToolCalls: []types.ToolCall{{ID: "toolu_1", Type: "function",
			Function: types.Function{Name: "read_file", Arguments: `{"path":"a.py"}`}}}}}}})
	enc.done()

	got := eventSeq(rec.Body.String())
	want := []string{
		"message_start",
		"content_block_start:thinking#0",
		"content_block_delta:signature_delta#0",
		"content_block_stop#0",
		"content_block_start:tool_use#1",
		"content_block_delta:input_json_delta#1",
		"content_block_stop#1",
		"message_delta",
		"message_stop",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// RT-7: a thinking block must never be stored in the response cache. Its
// signature is bound to the conversation that produced it, and a cache hit
// serves it to a different one by definition — the key is the request, so the
// same prompt in a new conversation IS the hit case. A client replaying a
// foreign signature gets a 400 on its next turn and it looks like its own bug.
//
// Exercises the real stripReasoningForCache that maybeStoreInCache calls,
// rather than a copy of its logic — a test that reimplements the thing it is
// checking passes whatever the code does.
func TestResponseCache_ReasoningIsNotStored(t *testing.T) {
	resp := &types.ChatResponse{
		ID: "chatcmpl-1", Model: "claude-sonnet-5-5",
		Choices: []types.Choice{{
			Message: types.Message{
				Role: "assistant", Content: "the answer",
				Reasoning: []types.ContentPart{{Type: "thinking", Thinking: "step one", Signature: "sig-abc"}},
			},
		}},
	}
	body, err := json.Marshal(stripReasoningForCache(resp))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leaked := range []string{"sig-abc", "step one", "reasoning"} {
		if strings.Contains(string(body), leaked) {
			t.Errorf("cached body carries %q; a replayed signature 400s in another conversation:\n%s", leaked, body)
		}
	}
	if !strings.Contains(string(body), "the answer") {
		t.Errorf("stripping reasoning took the answer with it:\n%s", body)
	}
	// The caller's own copy must be untouched — it is still being written to
	// the client, reasoning included.
	if len(resp.Choices[0].Message.Reasoning) != 1 {
		t.Error("stripping mutated the live response; the client would lose its thinking block")
	}
}

// The `anthropic-beta` header may be repeated AND comma-separated in the same
// request; Claude Code sends nine values at once.
func TestParseBetaHeader(t *testing.T) {
	got := parseBetaHeader([]string{
		"claude-code-20250219,oauth-2025-04-20",
		" context-1m-2025-08-07 , interleaved-thinking-2025-05-14 ",
		"context-1m-2025-08-07", // duplicate across headers
		"",
	})
	want := []string{
		"claude-code-20250219",
		"oauth-2025-04-20",
		"context-1m-2025-08-07",
		"interleaved-thinking-2025-05-14",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("beta %d = %q, want %q (order is preserved so the vendor sees them as sent)", i, got[i], want[i])
		}
	}
	if parseBetaHeader(nil) != nil {
		t.Error("no header must produce no betas, not an empty slice that serialises")
	}
}

// A tool_result carrying a screenshot must keep its blocks; the text-only
// flattening would drop the image between the client and the model.
func TestParseAnthropic_ToolResultImageSurvives(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"screenshot it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"shot","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[
			{"type":"text","text":"captured"},
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}}
		]}]}
	]}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var tool *types.Message
	for i := range req.Messages {
		if req.Messages[i].Role == "tool" {
			tool = &req.Messages[i]
		}
	}
	if tool == nil {
		t.Fatal("tool result message missing")
	}
	parts, ok := tool.Content.([]types.ContentPart)
	if !ok {
		t.Fatalf("a tool result with an image must keep its blocks, got %T", tool.Content)
	}
	var sawImage, sawText bool
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != nil && strings.Contains(p.ImageURL.URL, "iVBORw0KGgo=") {
			sawImage = true
		}
		if p.Type == "text" && p.Text == "captured" {
			sawText = true
		}
	}
	if !sawImage || !sawText {
		t.Errorf("tool_result blocks lost content: %+v", parts)
	}
}

// An ordinary text-only tool result must still be a plain string — the block
// form is reserved for the case that needs it.
func TestParseAnthropic_PlainToolResultStaysAString(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"read it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"read","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[
			{"type":"text","text":"file contents"}
		]}]}
	]}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role != "tool" {
			continue
		}
		s, ok := m.Content.(string)
		if !ok {
			t.Fatalf("text-only tool result changed shape to %T", m.Content)
		}
		if s != "file contents" {
			t.Errorf("tool result text = %q", s)
		}
	}
}
