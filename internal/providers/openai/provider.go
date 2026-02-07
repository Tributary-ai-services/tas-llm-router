package openai

import (
	"context"
	"fmt"
	"time"

	"github.com/sashabaranov/go-openai"
	"github.com/sirupsen/logrus"
	
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// OpenAIProvider implements the LLMProvider interface for OpenAI
type OpenAIProvider struct {
	client *openai.Client
	config *OpenAIConfig
	logger *logrus.Logger
}

// OpenAIConfig holds OpenAI-specific configuration
type OpenAIConfig struct {
	APIKey      string            `yaml:"api_key"`
	BaseURL     string            `yaml:"base_url"`
	OrgID       string            `yaml:"org_id"`
	Models      []types.ModelInfo `yaml:"models"`
	Timeout     time.Duration     `yaml:"timeout"`
}

// NewOpenAIProvider creates a new OpenAI provider instance
func NewOpenAIProvider(config *OpenAIConfig, logger *logrus.Logger) *OpenAIProvider {
	clientConfig := openai.DefaultConfig(config.APIKey)
	
	if config.BaseURL != "" {
		clientConfig.BaseURL = config.BaseURL
	}
	if config.OrgID != "" {
		clientConfig.OrgID = config.OrgID
	}
	
	client := openai.NewClientWithConfig(clientConfig)
	
	return &OpenAIProvider{
		client: client,
		config: config,
		logger: logger,
	}
}

// GetProviderName returns the provider name
func (p *OpenAIProvider) GetProviderName() string {
	return "openai"
}

// GetCapabilities returns the capabilities of the OpenAI provider
func (p *OpenAIProvider) GetCapabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{
		ProviderName:              "openai",
		SupportedModels:           p.config.Models,
		SupportsFunctions:         true,
		SupportsParallelFunctions: true,
		SupportsVision:            true,
		SupportsStructuredOutput:  true,
		SupportsStreaming:         true,
		SupportsAssistants:        true,
		SupportsBatch:             true,
		MaxContextWindow:          128000, // GPT-4 context window
		SupportedImageFormats:     []string{"png", "jpeg", "webp", "gif"},
		CostPer1KTokens: types.CostStructure{
			InputCostPer1K:  0.005, // Default GPT-4 pricing
			OutputCostPer1K: 0.015,
			Currency:        "USD",
		},
		OpenAISpecific: &types.OpenAICapabilities{
			SupportsJSONSchema:        true,
			SupportsStrictMode:        true,
			SupportsLogProbs:          true,
			SupportsSeed:              true,
			SupportsSystemFingerprint: true,
			SupportsParallelFunctions: true,
			MaxFunctionCalls:          10,
			SupportedResponseFormats:  []string{"text", "json_object", "json_schema"},
		},
	}
}

// ChatCompletion performs a chat completion request
func (p *OpenAIProvider) ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error) {
	// Convert our request to OpenAI format
	openaiReq, err := p.convertToOpenAIRequest(req)
	if err != nil {
		p.logger.WithError(err).Error("Failed to convert request to OpenAI format")
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

	// Make the API call
	resp, err := p.client.CreateChatCompletion(ctx, *openaiReq)
	if err != nil {
		p.logger.WithError(err).Error("OpenAI API call failed")
		return nil, fmt.Errorf("openai api call failed: %w", err)
	}

	// Convert response back to our format
	return p.convertFromOpenAIResponse(&resp, req), nil
}

// StreamCompletion performs a streaming chat completion request
func (p *OpenAIProvider) StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error) {
	// Convert our request to OpenAI format
	openaiReq, err := p.convertToOpenAIRequest(req)
	if err != nil {
		p.logger.WithError(err).Error("Failed to convert request to OpenAI format")
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

	// Enable streaming
	openaiReq.Stream = true

	// Make the streaming API call
	stream, err := p.client.CreateChatCompletionStream(ctx, *openaiReq)
	if err != nil {
		p.logger.WithError(err).Error("OpenAI streaming API call failed")
		return nil, fmt.Errorf("openai streaming api call failed: %w", err)
	}

	// Create our response channel
	chunks := make(chan *types.ChatChunk, 100)

	// Start goroutine to process stream
	go func() {
		defer close(chunks)
		defer stream.Close()

		for {
			response, err := stream.Recv()
			if err != nil {
				if err.Error() != "EOF" {
					p.logger.WithError(err).Error("Error receiving stream chunk")
				}
				return
			}

			// Convert chunk to our format
			chunk := p.convertFromOpenAIChunk(&response, req)
			select {
			case chunks <- chunk:
			case <-ctx.Done():
				return
			}
		}
	}()

	return chunks, nil
}

// EstimateCost estimates the cost for a chat completion request
func (p *OpenAIProvider) EstimateCost(req *types.ChatRequest) (*types.CostEstimate, error) {
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

// HealthCheck performs a health check on the OpenAI API
func (p *OpenAIProvider) HealthCheck(ctx context.Context) error {
	// Simple health check using models endpoint
	_, err := p.client.ListModels(ctx)
	if err != nil {
		p.logger.WithError(err).Error("OpenAI health check failed")
		return fmt.Errorf("openai health check failed: %w", err)
	}
	
	p.logger.Debug("OpenAI health check passed")
	return nil
}

// Interface implementations for advanced features

// SupportsFunctionCalling implements FunctionCallingProvider
func (p *OpenAIProvider) SupportsFunctionCalling() bool {
	return true
}

// SupportsParallelFunctions implements FunctionCallingProvider
func (p *OpenAIProvider) SupportsParallelFunctions() bool {
	return true
}

// SupportsVision implements VisionProvider
func (p *OpenAIProvider) SupportsVision() bool {
	return true
}

// GetSupportedImageFormats implements VisionProvider
func (p *OpenAIProvider) GetSupportedImageFormats() []string {
	return []string{"png", "jpeg", "webp", "gif"}
}

// SupportsStructuredOutput implements StructuredOutputProvider
func (p *OpenAIProvider) SupportsStructuredOutput() bool {
	return true
}

// SupportsStrictMode implements StructuredOutputProvider
func (p *OpenAIProvider) SupportsStrictMode() bool {
	return true
}

// SupportsBatch implements BatchProvider
func (p *OpenAIProvider) SupportsBatch() bool {
	return true
}

// CreateBatch implements BatchProvider
func (p *OpenAIProvider) CreateBatch(ctx context.Context, req *types.BatchRequest) (*types.BatchResponse, error) {
	// Convert to OpenAI batch request format
	openaiReq := openai.CreateBatchRequest{
		InputFileID:      req.InputFileID,
		Endpoint:         openai.BatchEndpoint(req.Endpoint),
		CompletionWindow: req.CompletionWindow,
		Metadata:         req.Metadata,
	}

	resp, err := p.client.CreateBatch(ctx, openaiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create batch: %w", err)
	}

	// Helper function to safely get string from pointer
	getStringPtr := func(s *string) string {
		if s == nil {
			return ""
		}
		return *s
	}

	// Helper function to safely get int64 from pointer int
	getInt64Ptr := func(i *int) int64 {
		if i == nil {
			return 0
		}
		return int64(*i)
	}

	// Convert response
	return &types.BatchResponse{
		ID:               resp.ID,
		Object:           resp.Object,
		Endpoint:         string(resp.Endpoint),
		InputFileID:      resp.InputFileID,
		CompletionWindow: resp.CompletionWindow,
		Status:           resp.Status,
		OutputFileID:     getStringPtr(resp.OutputFileID),
		ErrorFileID:      getStringPtr(resp.ErrorFileID),
		CreatedAt:        int64(resp.CreatedAt),
		InProgressAt:     getInt64Ptr(resp.InProgressAt),
		ExpiresAt:        getInt64Ptr(resp.ExpiresAt),
		CompletedAt:      getInt64Ptr(resp.CompletedAt),
		FailedAt:         getInt64Ptr(resp.FailedAt),
		ExpiredAt:        getInt64Ptr(resp.ExpiredAt),
		CancelledAt:      getInt64Ptr(resp.CancelledAt),
		RequestCounts: types.BatchRequestCounts{
			Total:     resp.RequestCounts.Total,
			Completed: resp.RequestCounts.Completed,
			Failed:    resp.RequestCounts.Failed,
		},
		Metadata: resp.Metadata,
	}, nil
}

// SupportsAssistants implements AssistantProvider
func (p *OpenAIProvider) SupportsAssistants() bool {
	return true
}

// CreateAssistant implements AssistantProvider
func (p *OpenAIProvider) CreateAssistant(ctx context.Context, req *types.AssistantRequest) (*types.AssistantResponse, error) {
	// Convert tools
	var tools []openai.AssistantTool
	for _, tool := range req.Tools {
		if tool.Type == "function" {
			tools = append(tools, openai.AssistantTool{
				Type: openai.AssistantToolTypeFunction,
				Function: &openai.FunctionDefinition{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				},
			})
		}
	}

	// Convert to OpenAI assistant request
	openaiReq := openai.AssistantRequest{
		Model:        req.Model,
		Name:         &req.Name,
		Description:  &req.Description,
		Instructions: &req.Instructions,
		Tools:        tools,
		FileIDs:      req.FileIDs,
		Metadata:     req.Metadata,
	}

	resp, err := p.client.CreateAssistant(ctx, openaiReq)
	if err != nil {
		return nil, fmt.Errorf("failed to create assistant: %w", err)
	}

	// Convert response tools back
	var responseTools []types.Tool
	for _, tool := range resp.Tools {
		if tool.Type == openai.AssistantToolTypeFunction && tool.Function != nil {
			responseTools = append(responseTools, types.Tool{
				Type: "function",
				Function: types.Function{
					Name:        tool.Function.Name,
					Description: tool.Function.Description,
					Parameters:  tool.Function.Parameters,
				},
			})
		}
	}

	return &types.AssistantResponse{
		ID:           resp.ID,
		Object:       resp.Object,
		CreatedAt:    resp.CreatedAt,
		Name:         getString(resp.Name),
		Description:  getString(resp.Description),
		Model:        resp.Model,
		Instructions: getString(resp.Instructions),
		Tools:        responseTools,
		FileIDs:      resp.FileIDs,
		Metadata:     resp.Metadata,
	}, nil
}

// Helper functions

// convertToOpenAIRequest converts our unified request to OpenAI's format
func (p *OpenAIProvider) convertToOpenAIRequest(req *types.ChatRequest) (*openai.ChatCompletionRequest, error) {
	// Convert messages
	var messages []openai.ChatCompletionMessage
	for _, msg := range req.Messages {
		openaiMsg := openai.ChatCompletionMessage{
			Role:       msg.Role,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}

		// Handle content (string or multipart)
		switch content := msg.Content.(type) {
		case string:
			openaiMsg.Content = content
		case []types.ContentPart:
			var multiContent []openai.ChatMessagePart
			for _, part := range content {
				switch part.Type {
				case "text":
					multiContent = append(multiContent, openai.ChatMessagePart{
						Type: openai.ChatMessagePartTypeText,
						Text: part.Text,
					})
				case "image_url":
					if part.ImageURL != nil {
						multiContent = append(multiContent, openai.ChatMessagePart{
							Type: openai.ChatMessagePartTypeImageURL,
							ImageURL: &openai.ChatMessageImageURL{
								URL:    part.ImageURL.URL,
								Detail: openai.ImageURLDetail(part.ImageURL.Detail),
							},
						})
					}
				}
			}
			openaiMsg.MultiContent = multiContent
		}

		// Handle tool calls on assistant messages
		if len(msg.ToolCalls) > 0 {
			var toolCalls []openai.ToolCall
			for _, tc := range msg.ToolCalls {
				toolCalls = append(toolCalls, openai.ToolCall{
					ID:   tc.ID,
					Type: openai.ToolType(tc.Type),
					Function: openai.FunctionCall{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
			openaiMsg.ToolCalls = toolCalls
		}

		messages = append(messages, openaiMsg)
	}

	openaiReq := &openai.ChatCompletionRequest{
		Model:    req.Model,
		Messages: messages,
		Stop:     req.Stop,
		Stream:   req.Stream,
	}

	// Set optional fields
	if req.Temperature != nil {
		openaiReq.Temperature = *req.Temperature
	}
	if req.MaxTokens != nil {
		openaiReq.MaxTokens = *req.MaxTokens
	}
	if req.TopP != nil {
		openaiReq.TopP = *req.TopP
	}
	if req.FrequencyPenalty != nil {
		openaiReq.FrequencyPenalty = *req.FrequencyPenalty
	}
	if req.PresencePenalty != nil {
		openaiReq.PresencePenalty = *req.PresencePenalty
	}
	if req.Seed != nil {
		openaiReq.Seed = req.Seed
	}

	// Handle functions (legacy)
	if len(req.Functions) > 0 {
		var functions []openai.FunctionDefinition
		for _, f := range req.Functions {
			functions = append(functions, openai.FunctionDefinition{
				Name:        f.Name,
				Description: f.Description,
				Parameters:  f.Parameters,
			})
		}
		openaiReq.Functions = functions
		openaiReq.FunctionCall = req.FunctionCall
	}

	// Handle tools
	if len(req.Tools) > 0 {
		var tools []openai.Tool
		for _, tool := range req.Tools {
			if tool.Type == "function" {
				tools = append(tools, openai.Tool{
					Type: openai.ToolTypeFunction,
					Function: &openai.FunctionDefinition{
						Name:        tool.Function.Name,
						Description: tool.Function.Description,
						Parameters:  tool.Function.Parameters,
					},
				})
			}
		}
		openaiReq.Tools = tools
		openaiReq.ToolChoice = req.ToolChoice
	}

	// Handle response format
	if req.ResponseFormat != nil {
		openaiReq.ResponseFormat = &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatType(req.ResponseFormat.Type),
		}

		// Handle JSON schema (if supported by OpenAI SDK version)
		if req.ResponseFormat.JSONSchema != nil {
			// Note: Some versions of the OpenAI SDK may not support JSONSchema
			// This is a placeholder for when it becomes available
			p.logger.Debug("JSON Schema response format requested but may not be fully supported in current SDK version")
		}
	}

	return openaiReq, nil
}

// convertFromOpenAIResponse converts OpenAI's response to our format
func (p *OpenAIProvider) convertFromOpenAIResponse(resp *openai.ChatCompletionResponse, req *types.ChatRequest) *types.ChatResponse {
	// Convert choices
	var choices []types.Choice
	for _, choice := range resp.Choices {
		ourChoice := types.Choice{
			Index:        choice.Index,
			FinishReason: string(choice.FinishReason),
		}

		// Convert message
		ourChoice.Message = types.Message{
			Role:    choice.Message.Role,
			Content: choice.Message.Content,
		}

		// Convert tool calls if present
		if len(choice.Message.ToolCalls) > 0 {
			var toolCalls []types.ToolCall
			for _, tc := range choice.Message.ToolCalls {
				toolCalls = append(toolCalls, types.ToolCall{
					ID:   tc.ID,
					Type: string(tc.Type),
					Function: types.Function{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
			ourChoice.Message.ToolCalls = toolCalls
		}

		choices = append(choices, ourChoice)
	}

	// Convert usage
	var usage *types.Usage
	if resp.Usage.TotalTokens > 0 {
		usage = &types.Usage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		}
	}

	return &types.ChatResponse{
		ID:                resp.ID,
		Object:            resp.Object,
		Created:           resp.Created,
		Model:             resp.Model,
		Choices:           choices,
		Usage:             usage,
		SystemFingerprint: resp.SystemFingerprint,
	}
}

// convertFromOpenAIChunk converts OpenAI's streaming chunk to our format
func (p *OpenAIProvider) convertFromOpenAIChunk(chunk *openai.ChatCompletionStreamResponse, req *types.ChatRequest) *types.ChatChunk {
	// Convert choices
	var choices []types.ChoiceChunk
	for _, choice := range chunk.Choices {
		ourChoice := types.ChoiceChunk{
			Index:        choice.Index,
			FinishReason: string(choice.FinishReason),
		}

		// Convert delta
		if choice.Delta.Content != "" || choice.Delta.Role != "" {
			ourChoice.Delta = &types.Message{
				Role:    choice.Delta.Role,
				Content: choice.Delta.Content,
			}

			// Handle tool calls in delta
			if len(choice.Delta.ToolCalls) > 0 {
				var toolCalls []types.ToolCall
				for _, tc := range choice.Delta.ToolCalls {
					toolCall := types.ToolCall{
						ID:   tc.ID,
						Type: string(tc.Type),
						Function: types.Function{
							Name:        tc.Function.Name,
							Parameters:  map[string]interface{}{"arguments": tc.Function.Arguments},
						},
					}
					toolCalls = append(toolCalls, toolCall)
				}
				ourChoice.Delta.ToolCalls = toolCalls
			}
		}

		choices = append(choices, ourChoice)
	}

	// Convert usage
	var usage *types.Usage
	if chunk.Usage != nil && chunk.Usage.TotalTokens > 0 {
		usage = &types.Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}

	return &types.ChatChunk{
		ID:                chunk.ID,
		Object:            chunk.Object,
		Created:           chunk.Created,
		Model:             chunk.Model,
		Choices:           choices,
		Usage:             usage,
		SystemFingerprint: chunk.SystemFingerprint,
	}
}

// estimateTokens provides a rough estimate of tokens in the request
func (p *OpenAIProvider) estimateTokens(req *types.ChatRequest) int {
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
				// Images add significant token cost, rough estimate
				if part.Type == "image_url" {
					totalChars += 1000 // Rough image token equivalent
				}
			}
		}
		
		// Add role and name tokens
		totalChars += len(msg.Role) + len(msg.Name)
	}
	
	// Add function/tool tokens
	for _, fn := range req.Functions {
		totalChars += len(fn.Name) + len(fn.Description)
	}
	for _, tool := range req.Tools {
		totalChars += len(tool.Function.Name) + len(tool.Function.Description)
	}
	
	// Rough approximation: 4 chars per token
	return totalChars / 4
}

// getString safely gets string value from pointer
func getString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Ensure OpenAIProvider implements all the interfaces
var _ providers.LLMProvider = (*OpenAIProvider)(nil)
var _ providers.FunctionCallingProvider = (*OpenAIProvider)(nil)
var _ providers.VisionProvider = (*OpenAIProvider)(nil)
var _ providers.StructuredOutputProvider = (*OpenAIProvider)(nil)
var _ providers.BatchProvider = (*OpenAIProvider)(nil)
var _ providers.AssistantProvider = (*OpenAIProvider)(nil)