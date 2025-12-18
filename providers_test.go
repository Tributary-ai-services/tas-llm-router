package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tributary-ai/llm-router-waf/internal/providers/anthropic"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func TestOpenAIProvider(t *testing.T) {
	// Skip if no API key
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		t.Skip("OPENAI_API_KEY environment variable not set")
	}

	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // Reduce noise in tests

	// Create OpenAI config
	config := &openai.OpenAIConfig{
		APIKey: apiKey,
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

	// Create provider
	provider := openai.NewOpenAIProvider(config, logger)

	t.Run("ChatCompletion", func(t *testing.T) {
		request := &types.ChatRequest{
			Model: "gpt-3.5-turbo",
			Messages: []types.Message{
				{
					Role:    "user",
					Content: "Say 'Hello from OpenAI!' and nothing else.",
				},
			},
			MaxTokens: intPtr(20),
		}

		ctx := context.Background()
		response, err := provider.ChatCompletion(ctx, request)

		require.NoError(t, err)
		assert.NotNil(t, response)
		assert.Contains(t, response.Model, "gpt-3.5-turbo")
		assert.Len(t, response.Choices, 1)
		assert.Equal(t, "assistant", response.Choices[0].Message.Role)
		assert.NotEmpty(t, response.Choices[0].Message.Content)
		assert.NotNil(t, response.Usage)
		assert.Greater(t, response.Usage.TotalTokens, 0)

		t.Logf("OpenAI Response: %s", response.Choices[0].Message.Content)
		t.Logf("Usage: %d prompt + %d completion = %d total tokens", 
			response.Usage.PromptTokens, 
			response.Usage.CompletionTokens, 
			response.Usage.TotalTokens)
	})

	t.Run("HealthCheck", func(t *testing.T) {
		ctx := context.Background()
		err := provider.HealthCheck(ctx)
		assert.NoError(t, err)
	})

	t.Run("ProviderCapabilities", func(t *testing.T) {
		capabilities := provider.GetCapabilities()
		assert.Equal(t, "openai", capabilities.ProviderName)
		assert.True(t, capabilities.SupportsFunctions)
		assert.True(t, capabilities.SupportsVision)
		assert.True(t, capabilities.SupportsStreaming)
	})
}

func TestAnthropicProvider(t *testing.T) {
	// Skip if no API key
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY environment variable not set")
	}

	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel) // Reduce noise in tests

	// Create Anthropic config
	config := &anthropic.AnthropicConfig{
		APIKey: apiKey,
		Models: []types.ModelInfo{
			{
				Name:              "claude-3-haiku-20240307",
				ProviderModelID:   "claude-3-haiku-20240307",
				InputCostPer1K:    0.00025,
				OutputCostPer1K:   0.00125,
				MaxContextWindow:  200000,
				MaxOutputTokens:   4096,
			},
		},
		Timeout: 30 * time.Second,
	}

	// Create provider
	provider := anthropic.NewAnthropicProvider(config, logger)

	t.Run("ChatCompletion", func(t *testing.T) {
		request := &types.ChatRequest{
			Model: "claude-3-haiku-20240307",
			Messages: []types.Message{
				{
					Role:    "user",
					Content: "Say 'Hello from Anthropic!' and nothing else.",
				},
			},
			MaxTokens: intPtr(20),
		}

		ctx := context.Background()
		response, err := provider.ChatCompletion(ctx, request)

		require.NoError(t, err)
		assert.NotNil(t, response)
		assert.Equal(t, "claude-3-haiku-20240307", response.Model)
		assert.Len(t, response.Choices, 1)
		assert.Equal(t, "assistant", response.Choices[0].Message.Role)
		assert.NotEmpty(t, response.Choices[0].Message.Content)
		assert.NotNil(t, response.Usage)
		assert.Greater(t, response.Usage.TotalTokens, 0)

		t.Logf("Anthropic Response: %s", response.Choices[0].Message.Content)
		t.Logf("Usage: %d prompt + %d completion = %d total tokens", 
			response.Usage.PromptTokens, 
			response.Usage.CompletionTokens, 
			response.Usage.TotalTokens)
	})

	t.Run("HealthCheck", func(t *testing.T) {
		ctx := context.Background()
		err := provider.HealthCheck(ctx)
		assert.NoError(t, err)
	})

	t.Run("ProviderCapabilities", func(t *testing.T) {
		capabilities := provider.GetCapabilities()
		assert.Equal(t, "anthropic", capabilities.ProviderName)
		assert.True(t, capabilities.SupportsFunctions)
		assert.True(t, capabilities.SupportsVision)
		assert.True(t, capabilities.SupportsStreaming)
	})
}

func TestBothProvidersComparison(t *testing.T) {
	openaiKey := os.Getenv("OPENAI_API_KEY")
	anthropicKey := os.Getenv("ANTHROPIC_API_KEY")
	
	if openaiKey == "" || anthropicKey == "" {
		t.Skip("Both OPENAI_API_KEY and ANTHROPIC_API_KEY environment variables required")
	}

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	// Setup OpenAI
	openaiConfig := &openai.OpenAIConfig{
		APIKey: openaiKey,
		Models: []types.ModelInfo{
			{
				Name:            "gpt-3.5-turbo",
				ProviderModelID: "gpt-3.5-turbo",
			},
		},
		Timeout: 30 * time.Second,
	}
	openaiProvider := openai.NewOpenAIProvider(openaiConfig, logger)

	// Setup Anthropic
	anthropicConfig := &anthropic.AnthropicConfig{
		APIKey: anthropicKey,
		Models: []types.ModelInfo{
			{
				Name:            "claude-3-haiku-20240307",
				ProviderModelID: "claude-3-haiku-20240307",
			},
		},
		Timeout: 30 * time.Second,
	}
	anthropicProvider := anthropic.NewAnthropicProvider(anthropicConfig, logger)

	t.Run("SamePromptComparison", func(t *testing.T) {
		prompt := "Explain what 2+2 equals in exactly one short sentence."
		ctx := context.Background()

		// Test OpenAI
		openaiRequest := &types.ChatRequest{
			Model: "gpt-3.5-turbo",
			Messages: []types.Message{
				{Role: "user", Content: prompt},
			},
			MaxTokens: intPtr(50),
		}

		openaiResponse, err := openaiProvider.ChatCompletion(ctx, openaiRequest)
		require.NoError(t, err)

		// Test Anthropic
		anthropicRequest := &types.ChatRequest{
			Model: "claude-3-haiku-20240307",
			Messages: []types.Message{
				{Role: "user", Content: prompt},
			},
			MaxTokens: intPtr(50),
		}

		anthropicResponse, err := anthropicProvider.ChatCompletion(ctx, anthropicRequest)
		require.NoError(t, err)

		// Compare responses
		t.Logf("OpenAI Response: %s (tokens: %d)", 
			openaiResponse.Choices[0].Message.Content, 
			openaiResponse.Usage.TotalTokens)
		t.Logf("Anthropic Response: %s (tokens: %d)", 
			anthropicResponse.Choices[0].Message.Content, 
			anthropicResponse.Usage.TotalTokens)

		// Both should provide responses
		assert.NotEmpty(t, openaiResponse.Choices[0].Message.Content)
		assert.NotEmpty(t, anthropicResponse.Choices[0].Message.Content)
	})

	t.Run("HealthChecksComparison", func(t *testing.T) {
		ctx := context.Background()

		// Test both health checks
		openaiErr := openaiProvider.HealthCheck(ctx)
		anthropicErr := anthropicProvider.HealthCheck(ctx)

		assert.NoError(t, openaiErr, "OpenAI health check should pass")
		assert.NoError(t, anthropicErr, "Anthropic health check should pass")

		t.Log("✓ Both providers passed health checks")
	})
}

// Helper function
func intPtr(i int) *int {
	return &i
}