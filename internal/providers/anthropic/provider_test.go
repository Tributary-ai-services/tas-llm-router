package anthropic

import (
	"testing"
	"time"

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
		name           string
		request        *types.ChatRequest
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
				Name:              "claude-3-haiku-20240307",
				ProviderModelID:   "claude-3-haiku-20240307",
				InputCostPer1K:    0.00025,
				OutputCostPer1K:   0.00125,
				MaxContextWindow:  200000,
				MaxOutputTokens:   4096,
			},
			{
				Name:              "claude-3-5-sonnet-20241022",
				ProviderModelID:   "claude-3-5-sonnet-20241022",
				InputCostPer1K:    0.003,
				OutputCostPer1K:   0.015,
				MaxContextWindow:  200000,
				MaxOutputTokens:   8192,
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