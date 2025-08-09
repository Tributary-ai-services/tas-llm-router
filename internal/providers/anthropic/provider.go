package anthropic

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sirupsen/logrus"
	
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// AnthropicProvider implements the LLMProvider interface for Anthropic Claude
type AnthropicProvider struct {
	client *anthropic.Client
	config *AnthropicConfig
	logger *logrus.Logger
}

// AnthropicConfig holds Anthropic-specific configuration
type AnthropicConfig struct {
	APIKey  string            `yaml:"api_key"`
	BaseURL string            `yaml:"base_url"`
	Models  []types.ModelInfo `yaml:"models"`
	Timeout time.Duration     `yaml:"timeout"`
}

// NewAnthropicProvider creates a new Anthropic provider instance
func NewAnthropicProvider(config *AnthropicConfig, logger *logrus.Logger) *AnthropicProvider {
	opts := []option.RequestOption{
		option.WithAPIKey(config.APIKey),
	}
	
	if config.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(config.BaseURL))
	}
	
	client := anthropic.NewClient(opts...)
	
	return &AnthropicProvider{
		client: &client,
		config: config,
		logger: logger,
	}
}

// GetProviderName returns the provider name
func (p *AnthropicProvider) GetProviderName() string {
	return "anthropic"
}

// GetCapabilities returns the capabilities of the Anthropic provider
func (p *AnthropicProvider) GetCapabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{
		ProviderName:              "anthropic",
		SupportedModels:           p.config.Models,
		SupportsFunctions:         true, // Tool use
		SupportsParallelFunctions: false, // Claude doesn't support parallel tool calls
		SupportsVision:            true,
		SupportsStructuredOutput:  false, // No strict JSON schema mode
		SupportsStreaming:         true,
		SupportsAssistants:        false, // No assistants API
		SupportsBatch:             false, // No batch API yet
		MaxContextWindow:          200000, // Claude-3.5 Sonnet context window
		SupportedImageFormats:     []string{"png", "jpeg", "webp", "gif"},
		CostPer1KTokens: types.CostStructure{
			InputCostPer1K:  0.003, // Default Claude-3.5 Sonnet pricing
			OutputCostPer1K: 0.015,
			Currency:        "USD",
		},
		AnthropicSpecific: &types.AnthropicCapabilities{
			SupportsSystemMessages:    true,
			MaxSystemMessageLength:    100000,
			SupportsStopSequences:     true,
			SupportsToolUse:           true,
			MaxToolCalls:              5,
			SupportedStopSequences:    []string{"\n\nHuman:", "\n\nAssistant:"},
		},
	}
}

// ChatCompletion performs a chat completion request
func (p *AnthropicProvider) ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error) {
	// Convert our request to Anthropic format
	anthropicReq, err := p.convertToAnthropicRequest(req)
	if err != nil {
		p.logger.WithError(err).Error("Failed to convert request to Anthropic format")
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

	// Make the API call
	resp, err := p.client.Messages.New(ctx, *anthropicReq)
	if err != nil {
		p.logger.WithError(err).Error("Anthropic API call failed")
		return nil, fmt.Errorf("anthropic api call failed: %w", err)
	}

	// Convert response back to our format
	return p.convertFromAnthropicResponse(resp, req), nil
}

// StreamCompletion performs a streaming chat completion request
func (p *AnthropicProvider) StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error) {
	// For now, return an error as streaming implementation needs to be updated for the current SDK
	return nil, fmt.Errorf("streaming not yet implemented for current Anthropic SDK version")
}

// EstimateCost estimates the cost for a chat completion request
func (p *AnthropicProvider) EstimateCost(req *types.ChatRequest) (*types.CostEstimate, error) {
	// Find model info
	var modelInfo *types.ModelInfo
	for _, model := range p.config.Models {
		if model.Name == req.Model || model.ProviderModelID == req.Model {
			modelInfo = &model
			break
		}
	}

	if modelInfo == nil {
		return nil, fmt.Errorf("model %s not found in configuration", req.Model)
	}

	// Estimate input tokens (rough approximation)
	inputTokens := p.estimateTokens(req)

	// Estimate output tokens (use max_tokens or default)
	outputTokens := 100 // default
	if req.MaxTokens != nil {
		outputTokens = *req.MaxTokens
	}

	totalTokens := inputTokens + outputTokens
	inputCost := float64(inputTokens) * modelInfo.InputCostPer1K / 1000
	outputCost := float64(outputTokens) * modelInfo.OutputCostPer1K / 1000
	totalCost := inputCost + outputCost

	return &types.CostEstimate{
		InputTokens:     inputTokens,
		OutputTokens:    outputTokens,
		TotalTokens:     totalTokens,
		InputCost:       inputCost,
		OutputCost:      outputCost,
		TotalCost:       totalCost,
		CostPer1KTokens: (modelInfo.InputCostPer1K + modelInfo.OutputCostPer1K) / 2,
	}, nil
}

// HealthCheck performs a health check on the Anthropic API
func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	// Simple health check using a minimal message
	testReq := anthropic.MessageNewParams{
		Model: anthropic.Model("claude-3-haiku-20240307"), // Use cheapest model for health check
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock("test")),
		},
		MaxTokens: 1,
	}
	
	_, err := p.client.Messages.New(ctx, testReq)
	if err != nil {
		p.logger.WithError(err).Error("Anthropic health check failed")
		return fmt.Errorf("anthropic health check failed: %w", err)
	}
	
	p.logger.Debug("Anthropic health check passed")
	return nil
}

// Interface implementations for advanced features

// SupportsFunctionCalling implements FunctionCallingProvider
func (p *AnthropicProvider) SupportsFunctionCalling() bool {
	return true // Claude supports tool use
}

// SupportsParallelFunctions implements FunctionCallingProvider  
func (p *AnthropicProvider) SupportsParallelFunctions() bool {
	return false // Claude doesn't support parallel tool calls
}

// SupportsVision implements VisionProvider
func (p *AnthropicProvider) SupportsVision() bool {
	return true
}

// GetSupportedImageFormats implements VisionProvider
func (p *AnthropicProvider) GetSupportedImageFormats() []string {
	return []string{"png", "jpeg", "webp", "gif"}
}

// SupportsStructuredOutput implements StructuredOutputProvider
func (p *AnthropicProvider) SupportsStructuredOutput() bool {
	return false // No strict JSON schema mode
}

// SupportsStrictMode implements StructuredOutputProvider
func (p *AnthropicProvider) SupportsStrictMode() bool {
	return false
}

// SupportsBatch implements BatchProvider
func (p *AnthropicProvider) SupportsBatch() bool {
	return false // No batch API yet
}

// CreateBatch implements BatchProvider (returns not supported error)
func (p *AnthropicProvider) CreateBatch(ctx context.Context, req *types.BatchRequest) (*types.BatchResponse, error) {
	return nil, fmt.Errorf("batch processing not supported by Anthropic provider")
}

// SupportsAssistants implements AssistantProvider
func (p *AnthropicProvider) SupportsAssistants() bool {
	return false // No assistants API
}

// CreateAssistant implements AssistantProvider (returns not supported error)
func (p *AnthropicProvider) CreateAssistant(ctx context.Context, req *types.AssistantRequest) (*types.AssistantResponse, error) {
	return nil, fmt.Errorf("assistants not supported by Anthropic provider")
}

// Helper functions

// convertToAnthropicRequest converts our unified request to Anthropic's format
func (p *AnthropicProvider) convertToAnthropicRequest(req *types.ChatRequest) (*anthropic.MessageNewParams, error) {
	// Extract system message if present
	var systemMessage string
	var messages []anthropic.MessageParam
	
	for _, msg := range req.Messages {
		if msg.Role == "system" {
			// Claude handles system messages separately
			switch content := msg.Content.(type) {
			case string:
				systemMessage = content
			default:
				return nil, fmt.Errorf("system messages must be text only for Anthropic")
			}
			continue
		}
		
		// Convert regular messages
		anthropicMsg, err := p.convertMessage(msg)
		if err != nil {
			return nil, err
		}
		messages = append(messages, anthropicMsg)
	}

	// Build the request
	anthropicReq := &anthropic.MessageNewParams{
		Model:    anthropic.Model(req.Model),
		Messages: messages,
	}

	// Set system message if present
	if systemMessage != "" {
		anthropicReq.System = []anthropic.TextBlockParam{
			{Text: systemMessage, Type: "text"},
		}
	}

	// Set optional parameters
	if req.MaxTokens != nil {
		anthropicReq.MaxTokens = int64(*req.MaxTokens)
	} else {
		anthropicReq.MaxTokens = 1024 // Anthropic requires max_tokens
	}
	
	if req.Temperature != nil {
		anthropicReq.Temperature = anthropic.Float(float64(*req.Temperature))
	}
	
	if req.TopP != nil {
		anthropicReq.TopP = anthropic.Float(float64(*req.TopP))
	}
	
	if len(req.Stop) > 0 {
		stopSeqs := make([]string, len(req.Stop))
		copy(stopSeqs, req.Stop)
		anthropicReq.StopSequences = stopSeqs
	}

	// Handle tools (Anthropic's function calling) - simplified for now
	if len(req.Tools) > 0 {
		var tools []anthropic.ToolUnionParam
		for _, tool := range req.Tools {
			if tool.Type == "function" {
				// Convert parameters schema if available
				var inputSchema anthropic.ToolInputSchemaParam
				if tool.Function.Parameters != nil {
					// For now, use an empty schema as direct conversion is complex
					inputSchema = anthropic.ToolInputSchemaParam{}
				}
				
				// Create tool using the union constructor
				anthropicTool := anthropic.ToolUnionParamOfTool(
					inputSchema,
					tool.Function.Name,
				)
				
				tools = append(tools, anthropicTool)
			}
		}
		anthropicReq.Tools = tools
	}

	return anthropicReq, nil
}

// convertMessage converts a unified message to Anthropic format
func (p *AnthropicProvider) convertMessage(msg types.Message) (anthropic.MessageParam, error) {
	// Handle content based on type and create appropriate message
	switch content := msg.Content.(type) {
	case string:
		// Simple text message
		if msg.Role == "user" {
			return anthropic.NewUserMessage(anthropic.NewTextBlock(content)), nil
		} else {
			return anthropic.NewAssistantMessage(anthropic.NewTextBlock(content)), nil
		}
		
	case []types.ContentPart:
		// Multimodal message - only handle text parts for now
		var blocks []anthropic.ContentBlockParamUnion
		for _, part := range content {
			if part.Type == "text" {
				blocks = append(blocks, anthropic.NewTextBlock(part.Text))
			}
			// Skip image parts for now - would need base64 conversion
		}
		
		if msg.Role == "user" {
			return anthropic.NewUserMessage(blocks...), nil
		} else {
			return anthropic.NewAssistantMessage(blocks...), nil
		}
		
	default:
		// Convert any other type to string
		contentStr := fmt.Sprintf("%v", content)
		if msg.Role == "user" {
			return anthropic.NewUserMessage(anthropic.NewTextBlock(contentStr)), nil
		} else {
			return anthropic.NewAssistantMessage(anthropic.NewTextBlock(contentStr)), nil
		}
	}
}


// convertFromAnthropicResponse converts Anthropic's response to our format
func (p *AnthropicProvider) convertFromAnthropicResponse(resp *anthropic.Message, req *types.ChatRequest) *types.ChatResponse {
	// Build choices from content blocks
	var choices []types.Choice
	
	choice := types.Choice{
		Index:        0,
		FinishReason: string(resp.StopReason),
		Message: types.Message{
			Role:    "assistant",
			Content: "", // Will be built from blocks
		},
	}
	
	// Process content blocks - simple text extraction for now
	var textContent strings.Builder
	
	for _, block := range resp.Content {
		if block.Type == "text" {
			textContent.WriteString(block.Text)
		}
	}
	
	choice.Message.Content = textContent.String()
	choices = append(choices, choice)
	
	// Build usage information
	var usage *types.Usage
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		usage = &types.Usage{
			PromptTokens:     int(resp.Usage.InputTokens),
			CompletionTokens: int(resp.Usage.OutputTokens),
			TotalTokens:      int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
		}
	}
	
	return &types.ChatResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   string(resp.Model),
		Choices: choices,
		Usage:   usage,
	}
}


// estimateTokens provides a rough estimate of tokens in the request
func (p *AnthropicProvider) estimateTokens(req *types.ChatRequest) int {
	totalChars := 0
	
	for _, msg := range req.Messages {
		switch content := msg.Content.(type) {
		case string:
			totalChars += len(content)
		case []types.ContentPart:
			for _, part := range content {
				if part.Type == "text" {
					totalChars += len(part.Text)
				}
				// Images add significant token cost for Claude
				if part.Type == "image_url" {
					totalChars += 1500 // Rough image token equivalent for Claude
				}
			}
		}
		
		// Add role tokens
		totalChars += len(msg.Role)
	}
	
	// Add tool tokens
	for _, tool := range req.Tools {
		totalChars += len(tool.Function.Name) + len(tool.Function.Description)
	}
	
	// Claude token estimation: approximately 3.5 chars per token
	return totalChars * 10 / 35
}

// Ensure AnthropicProvider implements all the interfaces
var _ providers.LLMProvider = (*AnthropicProvider)(nil)
var _ providers.FunctionCallingProvider = (*AnthropicProvider)(nil)
var _ providers.VisionProvider = (*AnthropicProvider)(nil)
var _ providers.StructuredOutputProvider = (*AnthropicProvider)(nil)
var _ providers.BatchProvider = (*AnthropicProvider)(nil)
var _ providers.AssistantProvider = (*AnthropicProvider)(nil)