package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/config"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/routing"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestRouterIntegration(t *testing.T) {
	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // Reduce noise during tests

	// Create router
	router := routing.NewRouter(logger)

	// Create mock OpenAI provider configuration
	openaiConfig := &openai.OpenAIConfig{
		APIKey: "test-api-key", // This won't actually be used in this test
		Models: []types.ModelInfo{
			{
				Name:              "gpt-3.5-turbo",
				ProviderModelID:   "gpt-3.5-turbo",
				InputCostPer1K:    0.0015,
				OutputCostPer1K:   0.002,
				MaxContextWindow:  16385,
				MaxOutputTokens:   4096,
			},
		},
		Timeout: 30 * time.Second,
	}

	// Register provider
	openaiProvider := openai.NewOpenAIProvider(openaiConfig, logger)
	router.RegisterProvider("openai", openaiProvider)

	// Test that providers are registered
	providers := router.ListProviders()
	if len(providers) != 1 {
		t.Fatalf("Expected 1 provider, got %d", len(providers))
	}

	if providers[0] != "openai" {
		t.Fatalf("Expected provider 'openai', got %s", providers[0])
	}

	// Test provider retrieval
	provider, exists := router.GetProvider("openai")
	if !exists {
		t.Fatal("OpenAI provider should exist")
	}

	if provider.GetProviderName() != "openai" {
		t.Fatalf("Expected provider name 'openai', got %s", provider.GetProviderName())
	}

	// Test capabilities
	capabilities := router.GetCapabilities()
	if len(capabilities) != 1 {
		t.Fatalf("Expected 1 provider capabilities, got %d", len(capabilities))
	}

	openaiCaps, exists := capabilities["openai"]
	if !exists {
		t.Fatal("OpenAI capabilities should exist")
	}

	if openaiCaps.ProviderName != "openai" {
		t.Fatalf("Expected provider name 'openai', got %s", openaiCaps.ProviderName)
	}

	// Test routing decision (without actual API call)
	req := &types.ChatRequest{
		ID:    "test-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "Hello, world!",
			},
		},
		OptimizeFor: types.OptimizeCost,
		Timestamp:   time.Now(),
	}

	ctx := context.Background()
	metadata, routedProvider, err := router.Route(ctx, req)
	if err != nil {
		t.Fatalf("Routing failed: %v", err)
	}

	if metadata.Provider != "openai" {
		t.Fatalf("Expected routing to 'openai', got %s", metadata.Provider)
	}

	if routedProvider.GetProviderName() != "openai" {
		t.Fatalf("Expected routed provider 'openai', got %s", routedProvider.GetProviderName())
	}
}

func TestConfigurationLoading(t *testing.T) {
	// Test loading configuration with mock API keys set
	t.Setenv("OPENAI_API_KEY", "test-openai-key")
	t.Setenv("ANTHROPIC_API_KEY", "test-anthropic-key")
	
	// Test loading configuration with defaults (no file)
	cfg, err := config.LoadConfig("")
	if err != nil {
		t.Fatalf("Failed to load default config: %v", err)
	}

	// Verify defaults
	if cfg.Server.Port != "8080" {
		t.Fatalf("Expected default port '8080', got %s", cfg.Server.Port)
	}

	if cfg.Router.DefaultStrategy != "cost_optimized" {
		t.Fatalf("Expected default strategy 'cost_optimized', got %s", cfg.Router.DefaultStrategy)
	}

	if cfg.Logging.Level != "info" {
		t.Fatalf("Expected default log level 'info', got %s", cfg.Logging.Level)
	}

	// Test server config conversion
	serverConfig := cfg.ToServerConfig()
	if serverConfig.Port != cfg.Server.Port {
		t.Fatalf("Server config conversion failed")
	}

	// Test enabled providers (should have both with API keys)
	enabledProviders := cfg.GetEnabledProviders()
	if len(enabledProviders) != 2 {
		t.Fatalf("Expected 2 enabled providers with API keys, got %d", len(enabledProviders))
	}
}

func TestCostEstimation(t *testing.T) {
	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	// Create OpenAI provider with test config
	config := &openai.OpenAIConfig{
		APIKey: "test-key",
		Models: []types.ModelInfo{
			{
				Name:              "gpt-3.5-turbo",
				ProviderModelID:   "gpt-3.5-turbo",
				InputCostPer1K:    0.0015,
				OutputCostPer1K:   0.002,
				MaxContextWindow:  16385,
				MaxOutputTokens:   4096,
			},
		},
	}

	provider := openai.NewOpenAIProvider(config, logger)

	// Test cost estimation
	req := &types.ChatRequest{
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "Hello, this is a test message for cost estimation",
			},
		},
		MaxTokens: func() *int { i := 100; return &i }(),
	}

	estimate, err := provider.EstimateCost(req)
	if err != nil {
		t.Fatalf("Cost estimation failed: %v", err)
	}

	if estimate.TotalCost <= 0 {
		t.Fatalf("Expected positive total cost, got %f", estimate.TotalCost)
	}

	if estimate.InputTokens <= 0 {
		t.Fatalf("Expected positive input tokens, got %d", estimate.InputTokens)
	}

	if estimate.OutputTokens != 100 {
		t.Fatalf("Expected 100 output tokens, got %d", estimate.OutputTokens)
	}
}

func BenchmarkRouting(b *testing.B) {
	// Setup
	logger := logrus.New()
	logger.SetLevel(logrus.ErrorLevel) // Minimal logging for benchmark

	router := routing.NewRouter(logger)

	openaiConfig := &openai.OpenAIConfig{
		APIKey: "test-key",
		Models: []types.ModelInfo{
			{
				Name:              "gpt-3.5-turbo",
				ProviderModelID:   "gpt-3.5-turbo",
				InputCostPer1K:    0.0015,
				OutputCostPer1K:   0.002,
				MaxContextWindow:  16385,
				MaxOutputTokens:   4096,
			},
		},
	}

	openaiProvider := openai.NewOpenAIProvider(openaiConfig, logger)
	router.RegisterProvider("openai", openaiProvider)

	req := &types.ChatRequest{
		ID:    "benchmark-request",
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "Hello, world!",
			},
		},
		OptimizeFor: types.OptimizeCost,
		Timestamp:   time.Now(),
	}

	ctx := context.Background()

	// Benchmark routing
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, err := router.Route(ctx, req)
		if err != nil {
			b.Fatalf("Routing failed: %v", err)
		}
	}
}