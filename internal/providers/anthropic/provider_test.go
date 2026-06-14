package anthropic

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestAnthropicProvider_GetProviderName(t *testing.T) {
	provider := createTestProvider(t)

	name := provider.GetProviderName()
	if name != "anthropic" {
		t.Errorf("Expected provider name 'anthropic', got %s", name)
	}
}

func TestAnthropicProvider_GetCapabilities(t *testing.T) {
	provider := createTestProvider(t)

	caps := provider.GetCapabilities()

	// Test basic capabilities
	if caps.ProviderName != "anthropic" {
		t.Errorf("Expected provider name 'anthropic', got %s", caps.ProviderName)
	}

	if !caps.SupportsFunctions {
		t.Error("Anthropic should support functions (tool use)")
	}

	if caps.SupportsParallelFunctions {
		t.Error("Anthropic should not support parallel functions")
	}

	if !caps.SupportsVision {
		t.Error("Anthropic should support vision")
	}

	if !caps.SupportsStreaming {
		t.Error("Anthropic should support streaming")
	}

	if caps.SupportsStructuredOutput {
		t.Error("Anthropic should not support structured output (no JSON schema mode)")
	}

	if caps.AnthropicSpecific == nil {
		t.Error("Anthropic specific capabilities should not be nil")
	}

	// Test Anthropic-specific capabilities
	if !caps.AnthropicSpecific.SupportsSystemMessages {
		t.Error("Anthropic should support system messages")
	}

	if !caps.AnthropicSpecific.SupportsToolUse {
		t.Error("Anthropic should support tool use")
	}
}

func TestAnthropicProvider_EstimateCost(t *testing.T) {
	provider := createTestProvider(t)

	tests := []struct {
		name            string
		request         *types.ChatRequest
		expectedMinCost float64
	}{
		{
			name: "Simple request",
			request: &types.ChatRequest{
				Model: "claude-3-haiku-20240307",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
				MaxTokens: intPtr(100),
			},
			expectedMinCost: 0.0,
		},
		{
			name: "Request with system message",
			request: &types.ChatRequest{
				Model: "claude-3-5-sonnet-20241022",
				Messages: []types.Message{
					{Role: "system", Content: "You are a helpful assistant."},
					{Role: "user", Content: "Please explain how anthropic models work."},
				},
				MaxTokens: intPtr(500),
			},
			expectedMinCost: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimate, err := provider.EstimateCost(tt.request)
			if err != nil {
				t.Fatalf("EstimateCost failed: %v", err)
			}

			if estimate.TotalCost <= tt.expectedMinCost {
				t.Errorf("Expected cost > %f, got %f", tt.expectedMinCost, estimate.TotalCost)
			}

			if estimate.InputTokens <= 0 {
				t.Error("Input tokens should be > 0")
			}

			if estimate.OutputTokens != *tt.request.MaxTokens {
				t.Errorf("Expected output tokens %d, got %d", *tt.request.MaxTokens, estimate.OutputTokens)
			}
		})
	}
}

func TestAnthropicProvider_ConvertRequest(t *testing.T) {
	provider := createTestProvider(t)

	tests := []struct {
		name    string
		request *types.ChatRequest
		wantErr bool
	}{
		{
			name: "Basic chat request",
			request: &types.ChatRequest{
				Model: "claude-3-haiku-20240307",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "Request with system message",
			request: &types.ChatRequest{
				Model: "claude-3-5-sonnet-20241022",
				Messages: []types.Message{
					{Role: "system", Content: "You are helpful"},
					{Role: "user", Content: "Hi"},
				},
			},
			wantErr: false,
		},
		{
			name: "Request with tools",
			request: &types.ChatRequest{
				Model: "claude-3-5-sonnet-20241022",
				Messages: []types.Message{
					{Role: "user", Content: "What's the weather?"},
				},
				Tools: []types.Tool{
					{
						Type: "function",
						Function: types.Function{
							Name:        "get_weather",
							Description: "Get weather information",
							Parameters:  map[string]interface{}{"type": "object"},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "Invalid system message format",
			request: &types.ChatRequest{
				Model: "claude-3-haiku-20240307",
				Messages: []types.Message{
					{
						Role: "system",
						Content: []types.ContentPart{
							{Type: "text", Text: "System"},
						},
					},
				},
			},
			wantErr: true, // System messages must be text only
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := provider.convertToAnthropicRequest(tt.request)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertToAnthropicRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && req == nil {
				t.Error("Expected non-nil request")
			}
		})
	}
}

// Regression: the OpenAI→Anthropic tool conversion used to send an empty
// input_schema, which Anthropic rejects with
// "tools.0.custom.input_schema: Field required". The schema's properties +
// required must survive the translation.
func TestAnthropicProvider_ToolInputSchemaCarriesThrough(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model:    "claude-3-5-sonnet-20241022",
		Messages: []types.Message{{Role: "user", Content: "What's the weather in SF?"}},
		Tools: []types.Tool{{
			Type: "function",
			Function: types.Function{
				Name:        "get_weather",
				Description: "Get current weather",
				Parameters: map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"location": map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"location"},
				},
			},
		}},
	}

	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convertToAnthropicRequest: %v", err)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got.Tools))
	}

	// Marshal the tool the way the SDK sends it on the wire and assert the
	// schema is present and populated (not the old empty {}).
	b, err := json.Marshal(got.Tools[0])
	if err != nil {
		t.Fatalf("marshal tool: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"input_schema"`, `"type":"object"`, `"properties"`, `"location"`, `"required"`} {
		if !strings.Contains(js, want) {
			t.Errorf("tool JSON missing %s:\n%s", want, js)
		}
	}
}

// A tool with nil/empty parameters must still get a valid object schema so
// input_schema is never omitted.
func TestAnthropicProvider_ToolEmptyParamsStillValid(t *testing.T) {
	schema := toAnthropicInputSchema(nil)
	b, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"type":"object"`) || !strings.Contains(js, `"properties"`) {
		t.Errorf("empty-params schema must still be a valid object: %s", js)
	}
}

// The response converter must surface Anthropic tool_use blocks as OpenAI
// tool_calls (with the input as the arguments JSON) and normalize
// stop_reason=tool_use → finish_reason=tool_calls.
func TestAnthropicProvider_ResponseToolUseExtraction(t *testing.T) {
	provider := createTestProvider(t)
	resp := &anthropic.Message{
		ID:         "msg_1",
		Model:      "claude-haiku-4-5-20251001",
		StopReason: anthropic.StopReasonToolUse,
		Content: []anthropic.ContentBlockUnion{
			{Type: "text", Text: "Let me check."},
			{Type: "tool_use", ID: "toolu_42", Name: "get_weather", Input: json.RawMessage(`{"location":"SF"}`)},
		},
	}
	got := provider.convertFromAnthropicResponse(resp, &types.ChatRequest{})
	if len(got.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(got.Choices))
	}
	ch := got.Choices[0]
	if ch.FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", ch.FinishReason)
	}
	if len(ch.Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool_call, got %d", len(ch.Message.ToolCalls))
	}
	tc := ch.Message.ToolCalls[0]
	if tc.ID != "toolu_42" || tc.Type != "function" || tc.Function.Name != "get_weather" {
		t.Errorf("tool_call not mapped: %+v", tc)
	}
	if !strings.Contains(tc.Function.Arguments, `"location":"SF"`) {
		t.Errorf("arguments not carried: %q", tc.Function.Arguments)
	}
	if ch.Message.Content != "Let me check." {
		t.Errorf("text content lost: %q", ch.Message.Content)
	}
}

func TestAnthropicProvider_FinishReasonNormalization(t *testing.T) {
	provider := createTestProvider(t)
	cases := map[anthropic.StopReason]string{
		anthropic.StopReasonEndTurn:   "stop",
		anthropic.StopReasonMaxTokens: "length",
		anthropic.StopReasonToolUse:   "tool_calls",
	}
	for sr, want := range cases {
		got := provider.convertFromAnthropicResponse(&anthropic.Message{StopReason: sr}, &types.ChatRequest{})
		if got.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %q → %q, want %q", sr, got.Choices[0].FinishReason, want)
		}
	}
}

// A multi-turn tool loop (assistant tool_calls + role=tool result) must convert
// without error — previously these collapsed to empty text and broke the loop.
func TestAnthropicProvider_MultiTurnToolMessagesConvert(t *testing.T) {
	provider := createTestProvider(t)
	req := &types.ChatRequest{
		Model: "claude-haiku-4-5-20251001",
		Messages: []types.Message{
			{Role: "user", Content: "Weather in SF?"},
			{Role: "assistant", ToolCalls: []types.ToolCall{
				{ID: "toolu_1", Type: "function", Function: types.Function{Name: "get_weather", Arguments: `{"location":"SF"}`}},
			}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "62F and foggy"},
			{Role: "user", Content: "Thanks."},
		},
	}
	got, err := provider.convertToAnthropicRequest(req)
	if err != nil {
		t.Fatalf("convertToAnthropicRequest: %v", err)
	}
	// 4 messages in → 4 Anthropic messages out (none dropped).
	if len(got.Messages) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(got.Messages))
	}
	// The whole thing must marshal (tool_use + tool_result blocks well-formed).
	if _, err := json.Marshal(got.Messages); err != nil {
		t.Errorf("messages do not marshal: %v", err)
	}
}

func TestAnthropicProvider_Interfaces(t *testing.T) {
	provider := createTestProvider(t)

	// Test FunctionCallingProvider interface
	if !provider.SupportsFunctionCalling() {
		t.Error("Anthropic should support function calling (tool use)")
	}

	if provider.SupportsParallelFunctions() {
		t.Error("Anthropic should not support parallel functions")
	}

	// Test VisionProvider interface
	if !provider.SupportsVision() {
		t.Error("Anthropic should support vision")
	}

	formats := provider.GetSupportedImageFormats()
	expectedFormats := []string{"png", "jpeg", "webp", "gif"}
	if len(formats) != len(expectedFormats) {
		t.Errorf("Expected %d image formats, got %d", len(expectedFormats), len(formats))
	}

	// Test StructuredOutputProvider interface
	if provider.SupportsStructuredOutput() {
		t.Error("Anthropic should not support structured output")
	}

	if provider.SupportsStrictMode() {
		t.Error("Anthropic should not support strict mode")
	}

	// Test BatchProvider interface
	if provider.SupportsBatch() {
		t.Error("Anthropic should not support batch processing yet")
	}

	// Test AssistantProvider interface
	if provider.SupportsAssistants() {
		t.Error("Anthropic should not support assistants API")
	}
}

func TestAnthropicProvider_TokenEstimation(t *testing.T) {
	provider := createTestProvider(t)

	tests := []struct {
		name              string
		request           *types.ChatRequest
		minExpectedTokens int
	}{
		{
			name: "Simple text",
			request: &types.ChatRequest{
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
			},
			minExpectedTokens: 1,
		},
		{
			name: "Longer text",
			request: &types.ChatRequest{
				Messages: []types.Message{
					{Role: "user", Content: "This is a longer message that should result in more tokens being estimated"},
				},
			},
			minExpectedTokens: 10,
		},
		{
			name: "With image",
			request: &types.ChatRequest{
				Messages: []types.Message{
					{
						Role: "user",
						Content: []types.ContentPart{
							{Type: "text", Text: "What's this?"},
							{Type: "image_url", ImageURL: &types.ImageURL{URL: "test.jpg"}},
						},
					},
				},
			},
			minExpectedTokens: 400, // Images add ~1500 chars = ~400+ tokens
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tokens := provider.estimateTokens(tt.request)
			if tokens < tt.minExpectedTokens {
				t.Errorf("Expected at least %d tokens, got %d", tt.minExpectedTokens, tokens)
			}
		})
	}
}

// Helper functions
func createTestProvider(t *testing.T) *AnthropicProvider {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	config := &AnthropicConfig{
		APIKey: "test-api-key",
		Models: []types.ModelInfo{
			{
				Name:             "claude-3-haiku-20240307",
				ProviderModelID:  "claude-3-haiku-20240307",
				InputCostPer1K:   0.00025,
				OutputCostPer1K:  0.00125,
				MaxContextWindow: 200000,
				MaxOutputTokens:  4096,
			},
			{
				Name:             "claude-3-5-sonnet-20241022",
				ProviderModelID:  "claude-3-5-sonnet-20241022",
				InputCostPer1K:   0.003,
				OutputCostPer1K:  0.015,
				MaxContextWindow: 200000,
				MaxOutputTokens:  8192,
			},
		},
		Timeout: 30 * time.Second,
	}

	return NewAnthropicProvider(config, logger)
}

func intPtr(i int) *int {
	return &i
}

// Benchmark tests
func BenchmarkAnthropicProvider_EstimateCost(b *testing.B) {
	provider := createTestProvider(&testing.T{})
	req := &types.ChatRequest{
		Model: "claude-3-haiku-20240307",
		Messages: []types.Message{
			{Role: "user", Content: "Hello, this is a benchmark test"},
		},
		MaxTokens: intPtr(100),
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = provider.EstimateCost(req)
	}
}

func BenchmarkAnthropicProvider_ConvertRequest(b *testing.B) {
	provider := createTestProvider(&testing.T{})
	req := &types.ChatRequest{
		Model: "claude-3-haiku-20240307",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = provider.convertToAnthropicRequest(req)
	}
}
