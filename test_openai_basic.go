package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/tributary-ai/llm-router-waf/internal/providers/openai"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

func main() {
	// Get API key from environment
	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		log.Fatal("OPENAI_API_KEY environment variable is required")
	}

	// Create logger
	logger := logrus.New()
	logger.SetLevel(logrus.InfoLevel)

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

	// Create a simple chat request
	request := &types.ChatRequest{
		Model: "gpt-3.5-turbo",
		Messages: []types.Message{
			{
				Role:    "user",
				Content: "Hello! Can you respond with a simple greeting?",
			},
		},
		MaxTokens: intPtr(50),
	}

	// Make the request
	fmt.Println("Sending request to OpenAI...")
	ctx := context.Background()
	response, err := provider.ChatCompletion(ctx, request)
	if err != nil {
		log.Fatalf("Error making OpenAI request: %v", err)
	}

	// Print the response
	fmt.Printf("OpenAI Response:\n")
	fmt.Printf("Model: %s\n", response.Model)
	fmt.Printf("Message: %s\n", response.Choices[0].Message.Content)
	fmt.Printf("Usage - Prompt Tokens: %d, Completion Tokens: %d, Total: %d\n",
		response.Usage.PromptTokens,
		response.Usage.CompletionTokens,
		response.Usage.TotalTokens)
	fmt.Println("✓ Test completed successfully!")
}

func intPtr(i int) *int {
	return &i
}