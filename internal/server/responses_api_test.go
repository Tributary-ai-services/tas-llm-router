package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestParseResponses_StringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-mini","instructions":"Be terse.","input":"Hello","max_output_tokens":64,"temperature":0.3}`)
	req, err := parseResponsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o-mini" {
		t.Errorf("model %q", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 64 {
		t.Errorf("max_output_tokens not mapped to MaxTokens: %+v", req.MaxTokens)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "Be terse." {
		t.Fatalf("instructions not a leading system message: %+v", req.Messages)
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "Hello" {
		t.Errorf("string input not a user message: %+v", req.Messages[1])
	}
}

func TestParseResponses_ItemsWithToolRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"gpt-4o-mini","max_output_tokens":128,
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"weather in Paris?"}]},
			{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"Paris\"}"},
			{"type":"function_call_output","call_id":"call_1","output":"18C sunny"}
		],
		"tools":[{"type":"function","name":"get_weather","description":"get","parameters":{"type":"object"}}],
		"tool_choice":"auto"
	}`)
	req, err := parseResponsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content != "weather in Paris?" {
		t.Errorf("user item wrong: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "assistant" || len(req.Messages[1].ToolCalls) != 1 || req.Messages[1].ToolCalls[0].ID != "call_1" {
		t.Errorf("function_call not mapped to assistant tool_call: %+v", req.Messages[1])
	}
	if req.Messages[2].Role != "tool" || req.Messages[2].ToolCallID != "call_1" || req.Messages[2].Content != "18C sunny" {
		t.Errorf("function_call_output not mapped to tool message: %+v", req.Messages[2])
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "get_weather" {
		t.Errorf("tool not translated: %+v", req.Tools)
	}
	if req.ToolChoice != "auto" {
		t.Errorf("tool_choice = %v", req.ToolChoice)
	}
}

func TestParseResponses_TASExtensions(t *testing.T) {
	body := []byte(`{"model":"gpt-4o-mini","input":"hi","optimize_for":"cost","max_cost":0.01,
		"required_features":["vision"],
		"retry_config":{"max_attempts":3,"backoff_type":"exponential"},
		"fallback_config":{"enabled":true,"preferred_chain":["anthropic"]}}`)
	req, err := parseResponsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if req.OptimizeFor != "cost" {
		t.Errorf("optimize_for not carried: %q", req.OptimizeFor)
	}
	if req.MaxCost == nil || *req.MaxCost != 0.01 {
		t.Errorf("max_cost not carried: %+v", req.MaxCost)
	}
	if len(req.RequiredFeatures) != 1 || req.RequiredFeatures[0] != "vision" {
		t.Errorf("required_features not carried: %+v", req.RequiredFeatures)
	}
	if req.RetryConfig == nil || req.RetryConfig.MaxAttempts != 3 {
		t.Errorf("retry_config not carried: %+v", req.RetryConfig)
	}
	if req.FallbackConfig == nil || !req.FallbackConfig.Enabled {
		t.Errorf("fallback_config not carried: %+v", req.FallbackConfig)
	}
}

func TestParseAnthropicMessages_TASExtensions(t *testing.T) {
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"optimize_for":"quality","max_cost":0.5}`)
	req, err := parseAnthropicToChatRequest(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if req.OptimizeFor != "quality" {
		t.Errorf("optimize_for not carried on messages: %q", req.OptimizeFor)
	}
	if req.MaxCost == nil || *req.MaxCost != 0.5 {
		t.Errorf("max_cost not carried on messages: %+v", req.MaxCost)
	}
}

func TestChatResponseToResponses_TextAndTool(t *testing.T) {
	resp := &types.ChatResponse{
		ID:    "chatcmpl-1",
		Model: "gpt-4o-mini",
		Choices: []types.Choice{{
			Message: types.Message{
				Role:    "assistant",
				Content: "Hi there",
				ToolCalls: []types.ToolCall{
					{ID: "call_9", Type: "function", Function: types.Function{Name: "f", Arguments: `{"a":1}`}},
				},
			},
			FinishReason: "tool_calls",
		}},
		Usage: &types.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10},
	}
	obj := chatResponseToResponses(resp)
	if obj.Object != "response" || obj.Status != "completed" {
		t.Errorf("envelope wrong: %+v", obj)
	}
	if !strings.HasPrefix(obj.ID, "resp_") {
		t.Errorf("id not resp_ prefixed: %s", obj.ID)
	}
	if len(obj.Output) != 2 {
		t.Fatalf("expected message + function_call output items, got %d: %+v", len(obj.Output), obj.Output)
	}
	if obj.Output[0].Type != "message" || len(obj.Output[0].Content) != 1 || obj.Output[0].Content[0].Type != "output_text" || obj.Output[0].Content[0].Text != "Hi there" {
		t.Errorf("message item wrong: %+v", obj.Output[0])
	}
	if obj.Output[1].Type != "function_call" || obj.Output[1].CallID != "call_9" || obj.Output[1].Name != "f" {
		t.Errorf("function_call item wrong: %+v", obj.Output[1])
	}
	if obj.Usage == nil || obj.Usage.InputTokens != 7 || obj.Usage.OutputTokens != 3 {
		t.Errorf("usage wrong: %+v", obj.Usage)
	}
}

func TestResponsesStreamEncoder_TextEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	enc := newResponsesStreamEncoder(rec, nil, &types.ChatRequest{Model: "gpt-4o-mini"}, nil)
	enc.writeChunk(&types.ChatChunk{ID: "chatcmpl-1", Model: "gpt-4o-mini", Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "Hel"}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{Content: "lo"}}}})
	enc.writeChunk(&types.ChatChunk{Choices: []types.ChoiceChunk{{Delta: &types.Message{}, FinishReason: "stop"}}, Usage: &types.Usage{PromptTokens: 5, CompletionTokens: 2}})
	enc.done()

	out := rec.Body.String()
	wantOrder := []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	}
	pos := 0
	for _, wnt := range wantOrder {
		idx := strings.Index(out[pos:], wnt)
		if idx < 0 {
			t.Fatalf("missing/out-of-order event %q in:\n%s", wnt, out)
		}
		pos += idx + len(wnt)
	}
	if !strings.Contains(out, `"delta":"Hel"`) || !strings.Contains(out, `"text":"Hello"`) {
		t.Errorf("text deltas/aggregate missing:\n%s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Error("responses stream must not emit [DONE]")
	}

	// The response.completed event must carry the FULL output so the SDK can
	// reconstruct output_text from it (regression: empty output → empty
	// get_final_response().output_text).
	var completedText string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
				Output []struct {
					Type    string `json:"type"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
				Usage responsesUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue
		}
		if ev.Type == "response.completed" {
			if ev.Response.Status != "completed" {
				t.Errorf("completed status = %q", ev.Response.Status)
			}
			if ev.Response.Usage.OutputTokens != 2 {
				t.Errorf("completed usage output_tokens = %d, want 2", ev.Response.Usage.OutputTokens)
			}
			for _, it := range ev.Response.Output {
				if it.Type == "message" {
					for _, c := range it.Content {
						completedText += c.Text
					}
				}
			}
		}
	}
	if completedText != "Hello" {
		t.Errorf("response.completed output text = %q, want Hello", completedText)
	}
	// every data line is valid JSON and carries a sequence_number
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
			t.Errorf("invalid JSON: %q", line)
		} else if _, ok := v["sequence_number"]; !ok {
			t.Errorf("missing sequence_number: %q", line)
		}
	}
}
