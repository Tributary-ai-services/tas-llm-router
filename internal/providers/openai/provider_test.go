package openai

import (
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestOpenAIProvider_GetProviderName(t *testing.T) {
	provider := createTestProvider(t)
	
	name := provider.GetProviderName()
	if name != "openai" {
		t.Errorf("Expected provider name 'openai', got %s", name)
	}
}

func TestOpenAIProvider_GetCapabilities(t *testing.T) {
	provider := createTestProvider(t)
	
	caps := provider.GetCapabilities()
	
	// Test basic capabilities
	if caps.ProviderName != "openai" {
		t.Errorf("Expected provider name 'openai', got %s", caps.ProviderName)
	}
	
	if !caps.SupportsFunctions {
		t.Error("OpenAI should support functions")
	}
	
	if !caps.SupportsVision {
		t.Error("OpenAI should support vision")
	}
	
	if !caps.SupportsStreaming {
		t.Error("OpenAI should support streaming")
	}
	
	if !caps.SupportsStructuredOutput {
		t.Error("OpenAI should support structured output")
	}
	
	if caps.OpenAISpecific == nil {
		t.Error("OpenAI specific capabilities should not be nil")
	}
}

func TestOpenAIProvider_EstimateCost(t *testing.T) {
	provider := createTestProvider(t)
	
	tests := []struct {
		name           string
		request        *types.ChatRequest
		expectedMinCost float64
	}{
		{
			name: "Simple request",
			request: &types.ChatRequest{
				Model: "gpt-3.5-turbo",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
				MaxTokens: intPtr(100),
			},
			expectedMinCost: 0.0, // Should be > 0
		},
		{
			name: "Long request",
			request: &types.ChatRequest{
				Model: "gpt-3.5-turbo",
				Messages: []types.Message{
					{Role: "system", Content: "You are a helpful assistant."},
					{Role: "user", Content: "Please help me understand how cost estimation works in LLM routing systems."},
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

func TestOpenAIProvider_ConvertRequest(t *testing.T) {
	provider := createTestProvider(t)
	
	// Test various request conversions
	tests := []struct {
		name    string
		request *types.ChatRequest
		wantErr bool
	}{
		{
			name: "Basic chat request",
			request: &types.ChatRequest{
				Model: "gpt-3.5-turbo",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
			},
			wantErr: false,
		},
		{
			name: "Request with tools",
			request: &types.ChatRequest{
				Model: "gpt-4o",
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
			name: "Request with vision",
			request: &types.ChatRequest{
				Model: "gpt-4o",
				Messages: []types.Message{
					{
						Role: "user",
						Content: []types.ContentPart{
							{Type: "text", Text: "What's in this image?"},
							{Type: "image_url", ImageURL: &types.ImageURL{URL: "https://example.com/image.jpg"}},
						},
					},
				},
			},
			wantErr: false,
		},
	}
	
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := provider.convertToOpenAIRequest(tt.request)
			if (err != nil) != tt.wantErr {
				t.Errorf("convertToOpenAIRequest() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			
			if !tt.wantErr && req == nil {
				t.Error("Expected non-nil request")
			}
		})
	}
}

func TestOpenAIProvider_Interfaces(t *testing.T) {
	provider := createTestProvider(t)
	
	// Test FunctionCallingProvider interface
	if !provider.SupportsFunctionCalling() {
		t.Error("OpenAI should support function calling")
	}
	
	if !provider.SupportsParallelFunctions() {
		t.Error("OpenAI should support parallel functions")
	}
	
	// Test VisionProvider interface
	if !provider.SupportsVision() {
		t.Error("OpenAI should support vision")
	}
	
	formats := provider.GetSupportedImageFormats()
	if len(formats) == 0 {
		t.Error("OpenAI should support image formats")
	}
	
	// Test StructuredOutputProvider interface
	if !provider.SupportsStructuredOutput() {
		t.Error("OpenAI should support structured output")
	}
	
	if !provider.SupportsStrictMode() {
		t.Error("OpenAI should support strict mode")
	}
	
	// Test BatchProvider interface
	if !provider.SupportsBatch() {
		t.Error("OpenAI should support batch processing")
	}
	
	// Test AssistantProvider interface
	if !provider.SupportsAssistants() {
		t.Error("OpenAI should support assistants")
	}
}

// Helper functions
func createTestProvider(t *testing.T) *OpenAIProvider {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	
	config := &OpenAIConfig{
		APIKey: "test-api-key",
		Models: []types.ModelInfo{
			{
				Name:              "gpt-3.5-turbo",
				ProviderModelID:   "gpt-3.5-turbo",
				InputCostPer1K:    0.0015,
				OutputCostPer1K:   0.002,
				MaxContextWindow:  16385,
				MaxOutputTokens:   4096,
			},
			{
				Name:              "gpt-4o",
				ProviderModelID:   "gpt-4o",
				InputCostPer1K:    0.005,
				OutputCostPer1K:   0.015,
				MaxContextWindow:  128000,
				MaxOutputTokens:   4096,
				SupportsVision:    true,
				SupportsFunctions: true,
			},
		},
		Timeout: 30 * time.Second,
	}
	
	return NewOpenAIProvider(config, logger)
}

func intPtr(i int) *int {
	return &i
}

func float32Ptr(f float32) *float32 {
	return &f
}

// Benchmark tests
func BenchmarkOpenAIProvider_EstimateCost(b *testing.B) {
	provider := createTestProvider(&testing.T{})
	req := &types.ChatRequest{
		Model: "gpt-3.5-turbo",
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

func BenchmarkOpenAIProvider_ConvertRequest(b *testing.B) {
	provider := createTestProvider(&testing.T{})
	req := &types.ChatRequest{
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
	}
	
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = provider.convertToOpenAIRequest(req)
	}
}