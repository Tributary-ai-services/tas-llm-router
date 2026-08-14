package server

import (
	"bufio"
	"encoding/json"
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
