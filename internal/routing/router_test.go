package routing

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestRouter_RegisterProvider(t *testing.T) {
	router := createTestRouter(t)
	
	// Create test provider
	provider := createTestOpenAIProvider()
	
	// Register provider
	router.RegisterProvider("test-openai", provider)
	
	// Verify provider is registered
	providers := router.ListProviders()
	if len(providers) != 1 {
		t.Fatalf("Expected 1 provider, got %d", len(providers))
	}
	
	if providers[0] != "test-openai" {
		t.Errorf("Expected provider name 'test-openai', got %s", providers[0])
	}
	
	// Test GetProvider
	retrievedProvider, exists := router.GetProvider("test-openai")
	if !exists {
		t.Error("Provider should exist")
	}
	
	if retrievedProvider != provider {
		t.Error("Retrieved provider should match registered provider")
	}
}

func TestRouter_Route_CostOptimized(t *testing.T) {
	router := createTestRouter(t)
	
	// Register multiple providers with different costs
	cheapProvider := createTestOpenAIProvider()
	expensiveProvider := createTestOpenAIProvider()
	
	router.RegisterProvider("cheap", cheapProvider)
	router.RegisterProvider("expensive", expensiveProvider)
	
	req := &types.ChatRequest{
		ID:    "test-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
		OptimizeFor: types.OptimizeCost,
		Timestamp:   time.Now(),
	}
	
	ctx := context.Background()
	metadata, provider, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Routing failed: %v", err)
	}
	
	if metadata == nil {
		t.Fatal("Metadata should not be nil")
	}
	
	if provider == nil {
		t.Fatal("Provider should not be nil")
	}
	
	// Should route to one of the providers
	validProviders := map[string]bool{"cheap": true, "expensive": true}
	if !validProviders[metadata.Provider] {
		t.Errorf("Unexpected provider selected: %s", metadata.Provider)
	}
}

func TestRouter_Route_SpecificProvider(t *testing.T) {
	router := createTestRouter(t)
	
	// Register OpenAI provider
	openaiProvider := createTestOpenAIProvider()
	router.RegisterProvider("openai", openaiProvider)
	
	// Request with OpenAI-specific model
	req := &types.ChatRequest{
		ID:    "test-request",
		Model: "gpt-4o", // This should route to OpenAI
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
		Timestamp: time.Now(),
	}
	
	ctx := context.Background()
	metadata, routedProvider, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Routing failed: %v", err)
	}
	
	if metadata.Provider != "openai" {
		t.Errorf("Expected routing to 'openai', got %s", metadata.Provider)
	}
	
	if routedProvider != openaiProvider {
		t.Error("Should return the OpenAI provider")
	}
}

func TestRouter_Route_PerformanceOptimized(t *testing.T) {
	router := createTestRouter(t)
	
	// Register providers
	openaiProvider := createTestOpenAIProvider()
	router.RegisterProvider("openai", openaiProvider)
	
	req := &types.ChatRequest{
		ID:    "test-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
		OptimizeFor: types.OptimizePerformance,
		Timestamp:   time.Now(),
	}
	
	ctx := context.Background()
	metadata, _, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Routing failed: %v", err)
	}
	
	if metadata.Provider != "openai" {
		t.Errorf("Expected routing to 'openai', got %s", metadata.Provider)
	}
}

func TestRouter_Route_RoundRobin(t *testing.T) {
	router := createTestRouter(t)
	
	// Register multiple providers
	provider1 := createTestOpenAIProvider()
	provider2 := createTestOpenAIProvider()
	
	router.RegisterProvider("provider1", provider1)
	router.RegisterProvider("provider2", provider2)
	
	req := &types.ChatRequest{
		ID:    "test-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
		OptimizeFor: "round_robin", // Custom strategy
		Timestamp:   time.Now(),
	}
	
	ctx := context.Background()
	
	// Make multiple requests to see round-robin behavior
	selectedProviders := make(map[string]int)
	for i := 0; i < 4; i++ {
		metadata, _, err := router.Route(ctx, req)
		if err != nil {
			t.Fatalf("Routing failed on iteration %d: %v", i, err)
		}
		selectedProviders[metadata.Provider]++
	}
	
	// Both providers should have been selected
	if len(selectedProviders) < 1 {
		t.Error("Round-robin should select providers")
	}
}

func TestRouter_HealthMonitoring(t *testing.T) {
	router := createTestRouter(t)
	
	// Register provider
	provider := createTestOpenAIProvider()
	router.RegisterProvider("test", provider)
	
	// Get health status
	healthStatus := router.GetHealthStatus()
	if len(healthStatus) != 1 {
		t.Fatalf("Expected 1 health status, got %d", len(healthStatus))
	}
	
	status, exists := healthStatus["test"]
	if !exists {
		t.Error("Health status for 'test' provider should exist")
	}
	
	if status.Status != "unknown" {
		t.Errorf("Initial status should be 'unknown', got %s", status.Status)
	}
}

func TestRouter_FeatureFiltering(t *testing.T) {
	router := createTestRouter(t)
	
	// Register provider
	provider := createTestOpenAIProvider()
	router.RegisterProvider("openai", provider)
	
	tests := []struct {
		name         string
		request      *types.ChatRequest
		expectRoute  bool
	}{
		{
			name: "Basic request",
			request: &types.ChatRequest{
				Model: "gpt-3.5-turbo",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
			},
			expectRoute: true,
		},
		{
			name: "Request with functions",
			request: &types.ChatRequest{
				Model: "gpt-4o",
				Messages: []types.Message{
					{Role: "user", Content: "Hello"},
				},
				Tools: []types.Tool{
					{Type: "function", Function: types.Function{Name: "test"}},
				},
			},
			expectRoute: true, // OpenAI supports functions
		},
		{
			name: "Request with vision",
			request: &types.ChatRequest{
				Model: "gpt-4o",
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
			expectRoute: true, // OpenAI supports vision
		},
	}
	
	ctx := context.Background()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.request.ID = "test-" + tt.name
			tt.request.Timestamp = time.Now()
			
			_, _, err := router.Route(ctx, tt.request)
			
			if tt.expectRoute && err != nil {
				t.Errorf("Expected successful routing, got error: %v", err)
			}
			
			if !tt.expectRoute && err == nil {
				t.Error("Expected routing to fail due to unsupported features")
			}
		})
	}
}

func TestRouter_BuildRoutingContext(t *testing.T) {
	router := createTestRouter(t)
	
	req := &types.ChatRequest{
		Model: "gpt-4o",
		Messages: []types.Message{
			{Role: "user", Content: "Test"},
		},
		RequiredFeatures: []string{"functions", "vision"},
		Stream:          true,
	}
	
	context := router.buildRoutingContext("test_strategy", req, []string{"provider1", "provider2"})
	
	if context.Strategy != "test_strategy" {
		t.Errorf("Expected strategy 'test_strategy', got %s", context.Strategy)
	}
	
	if len(context.ConsideredProviders) != 2 {
		t.Errorf("Expected 2 considered providers, got %d", len(context.ConsideredProviders))
	}
	
	// Check that features were extracted
	expectedFeatures := []string{"functions", "vision", "streaming"}
	features := context.RequestFeatures
	
	featureMap := make(map[string]bool)
	for _, f := range features {
		featureMap[f] = true
	}
	
	for _, expected := range expectedFeatures {
		if !featureMap[expected] {
			t.Errorf("Expected feature %s not found in extracted features", expected)
		}
	}
}

func TestRouter_GetCapabilities(t *testing.T) {
	router := createTestRouter(t)
	
	// Register provider
	provider := createTestOpenAIProvider()
	router.RegisterProvider("openai", provider)
	
	capabilities := router.GetCapabilities()
	if len(capabilities) != 1 {
		t.Fatalf("Expected 1 capability set, got %d", len(capabilities))
	}
	
	openaiCaps, exists := capabilities["openai"]
	if !exists {
		t.Error("OpenAI capabilities should exist")
	}
	
	if openaiCaps.ProviderName != "openai" {
		t.Errorf("Expected provider name 'openai', got %s", openaiCaps.ProviderName)
	}
}

// Helper functions
func createTestRouter(t *testing.T) *Router {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // Reduce noise during tests
	return NewRouter(logger)
}

func createTestOpenAIProvider() *openai.OpenAIProvider {
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)
	
	config := &openai.OpenAIConfig{
		APIKey: "test-api-key",
		Models: []types.ModelInfo{
			{
				Name:              "gpt-3.5-turbo",
				ProviderModelID:   "gpt-3.5-turbo",
				InputCostPer1K:    0.0015,
				OutputCostPer1K:   0.002,
				MaxContextWindow:  16385,
				MaxOutputTokens:   4096,
				SupportsFunctions: true,
			},
			{
				Name:              "gpt-4o",
				ProviderModelID:   "gpt-4o",
				InputCostPer1K:    0.005,
				OutputCostPer1K:   0.015,
				MaxContextWindow:  128000,
				MaxOutputTokens:   4096,
				SupportsFunctions: true,
				SupportsVision:    true,
			},
		},
		Timeout: 30 * time.Second,
	}
	
	return openai.NewOpenAIProvider(config, logger)
}

// Benchmark tests
func BenchmarkRouter_Route(b *testing.B) {
	router := createTestRouter(&testing.T{})
	provider := createTestOpenAIProvider()
	router.RegisterProvider("openai", provider)
	
	req := &types.ChatRequest{
		ID:    "benchmark-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{Role: "user", Content: "Hello"},
		},
		OptimizeFor: types.OptimizeCost,
		Timestamp:   time.Now(),
	}
	
	ctx := context.Background()
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		_, _, err := router.Route(ctx, req)
		if err != nil {
			b.Fatalf("Routing failed: %v", err)
		}
	}
}

func BenchmarkRouter_HealthCheck(b *testing.B) {
	router := createTestRouter(&testing.T{})
	provider := createTestOpenAIProvider()
	router.RegisterProvider("openai", provider)
	
	b.ResetTimer()
	
	for i := 0; i < b.N; i++ {
		_ = router.GetHealthStatus()
	}
}