package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
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
		SupportsFunctions:         true,  // Tool use
		SupportsParallelFunctions: false, // Claude doesn't support parallel tool calls
		SupportsVision:            true,
		SupportsStructuredOutput:  false, // No strict JSON schema mode
		SupportsStreaming:         true,
		SupportsAssistants:        false,  // No assistants API
		SupportsBatch:             false,  // No batch API yet
		MaxContextWindow:          200000, // Claude-3.5 Sonnet context window
		SupportedImageFormats:     []string{"png", "jpeg", "webp", "gif"},
		CostPer1KTokens: types.CostStructure{
			InputCostPer1K:  0.003, // Default Claude-3.5 Sonnet pricing
			OutputCostPer1K: 0.015,
			Currency:        "USD",
		},
		AnthropicSpecific: &types.AnthropicCapabilities{
			SupportsSystemMessages: true,
			MaxSystemMessageLength: 100000,
			SupportsStopSequences:  true,
			SupportsToolUse:        true,
			MaxToolCalls:           5,
			SupportedStopSequences: []string{"\n\nHuman:", "\n\nAssistant:"},
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

	// AIQG: mirror StreamCompletion — stamp forwarded + wire httptrace
	// so the response event carries gateway_ingress_ms and
	// vendor_ttfb_ms. Both are no-ops outside AIQG mode.
	instrumentation.StampForwarded(ctx)
	ctx = instrumentation.Attach(ctx)

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
	// Convert our request to Anthropic format
	anthropicReq, err := p.convertToAnthropicRequest(req)
	if err != nil {
		p.logger.WithError(err).Error("Failed to convert request to Anthropic format")
		return nil, fmt.Errorf("failed to convert request: %w", err)
	}

	// AIQG: stamp forwarded + wire httptrace so GotFirstResponseByte
	// → StampTTFB populates vendor_ttfb_ms. Both no-ops outside AIQG mode.
	instrumentation.StampForwarded(ctx)
	ctx = instrumentation.Attach(ctx)

	// Create the streaming request
	stream := p.client.Messages.NewStreaming(ctx, *anthropicReq)

	// Create our response channel
	chunks := make(chan *types.ChatChunk, 100)

	// Start goroutine to process stream
	go func() {
		defer close(chunks)
		defer instrumentation.StampLastChunk(ctx)

		ttftStamped := false
		message := anthropic.Message{}
		for stream.Next() {
			event := stream.Current()
			err := message.Accumulate(event)
			if err != nil {
				p.logger.WithError(err).Error("Failed to accumulate streaming event")
				return
			}

			// AIQG: per the architect-review fix (§4 Risk #5), TTFT must
			// be the first non-empty content delta. Anthropic emits
			// message_start, content_block_start, ping, and message_delta
			// events that carry no generated text — those are NOT TTFT.
			// hasContent is true only for ContentBlockDelta events with
			// non-empty TextDelta (or future tool-use delta types that
			// carry generated arguments).
			hasContent := anthropicEventHasContent(event)
			if hasContent && !ttftStamped {
				instrumentation.StampTTFT(ctx)
				ttftStamped = true
			}
			instrumentation.StampChunk(ctx, hasContent)

			// Convert event to our chunk format
			chunk := p.convertStreamEvent(event, req, &message)
			if chunk != nil {
				select {
				case chunks <- chunk:
				case <-ctx.Done():
					return
				}
			}
		}

		if stream.Err() != nil {
			p.logger.WithError(stream.Err()).Error("Anthropic streaming error")
			return
		}

		// Send final chunk with finish reason and usage
		finalChunk := &types.ChatChunk{
			ID:      message.ID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   string(message.Model),
			Choices: []types.ChoiceChunk{
				{
					Index:        0,
					Delta:        &types.Message{Role: "assistant"},
					FinishReason: string(message.StopReason),
				},
			},
		}
		if message.Usage.InputTokens > 0 || message.Usage.OutputTokens > 0 ||
			message.Usage.CacheCreationInputTokens > 0 || message.Usage.CacheReadInputTokens > 0 {
			finalChunk.Usage = &types.Usage{
				PromptTokens:        int(message.Usage.InputTokens),
				CompletionTokens:    int(message.Usage.OutputTokens),
				TotalTokens:         int(message.Usage.InputTokens + message.Usage.OutputTokens),
				CacheCreationTokens: int(message.Usage.CacheCreationInputTokens),
				CacheReadTokens:     int(message.Usage.CacheReadInputTokens),
			}
		}
		select {
		case chunks <- finalChunk:
		case <-ctx.Done():
		}
	}()

	return chunks, nil
}

// anthropicEventHasContent reports whether a streaming event carries
// actual generated content. Used by AIQG TTFT detection — the first
// event with hasContent=true is what we count as the model's first
// generated token. MessageStart, ContentBlockStart, MessageDelta (usage
// only), and Ping events all return false because they precede or
// surround content without containing it.
func anthropicEventHasContent(event anthropic.MessageStreamEventUnion) bool {
	v, ok := event.AsAny().(anthropic.ContentBlockDeltaEvent)
	if !ok {
		return false
	}
	switch d := v.Delta.AsAny().(type) {
	case anthropic.TextDelta:
		return d.Text != ""
	case anthropic.InputJSONDelta:
		// Tool-use argument fragments — generated by the model, count
		// toward TTFT for agentic workflows.
		return d.PartialJSON != ""
	}
	return false
}

// convertStreamEvent converts an Anthropic streaming event to our ChatChunk format
func (p *AnthropicProvider) convertStreamEvent(event anthropic.MessageStreamEventUnion, req *types.ChatRequest, message *anthropic.Message) *types.ChatChunk {
	switch variant := event.AsAny().(type) {
	case anthropic.ContentBlockDeltaEvent:
		switch delta := variant.Delta.AsAny().(type) {
		case anthropic.TextDelta:
			return &types.ChatChunk{
				ID:      message.ID,
				Object:  "chat.completion.chunk",
				Created: time.Now().Unix(),
				Model:   req.Model,
				Choices: []types.ChoiceChunk{
					{
						Index: 0,
						Delta: &types.Message{
							Role:    "assistant",
							Content: delta.Text,
						},
					},
				},
			}
		}
	case anthropic.MessageStartEvent:
		// Send initial chunk with role
		return &types.ChatChunk{
			ID:      message.ID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []types.ChoiceChunk{
				{
					Index: 0,
					Delta: &types.Message{
						Role: "assistant",
					},
				},
			},
		}
	}
	return nil
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
	model := p.pickHealthCheckModel()
	if model == "" {
		return fmt.Errorf("anthropic health check skipped: no models configured")
	}

	testReq := anthropic.MessageNewParams{
		Model: anthropic.Model(model),
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

// pickHealthCheckModel chooses a currently-configured model for the health
// probe. We can't hardcode a model name — Anthropic deprecates dated snapshots
// and the probe then 404s, marking the whole provider unhealthy. Prefer a
// Haiku-family model (cheapest), fall back to the first configured model.
func (p *AnthropicProvider) pickHealthCheckModel() string {
	if len(p.config.Models) == 0 {
		return ""
	}
	for _, m := range p.config.Models {
		if strings.Contains(strings.ToLower(m.Name), "haiku") {
			return m.Name
		}
	}
	return p.config.Models[0].Name
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

	// Handle tools (OpenAI function-calling → Anthropic tool use). The
	// JSON-Schema in tool.Function.Parameters maps directly onto Anthropic's
	// input_schema — Anthropic REQUIRES a non-empty input_schema (it 400s
	// with "tools.N.custom.input_schema: Field required" otherwise), so we
	// translate properties/required across rather than sending an empty one.
	if len(req.Tools) > 0 {
		var tools []anthropic.ToolUnionParam
		for _, tool := range req.Tools {
			if tool.Type == "function" || tool.Type == "" {
				anthropicTool := anthropic.ToolUnionParamOfTool(
					toAnthropicInputSchema(tool.Function.Parameters),
					tool.Function.Name,
				)
				if tool.Function.Description != "" {
					anthropicTool.OfTool.Description = anthropic.String(tool.Function.Description)
				}
				tools = append(tools, anthropicTool)
			}
		}
		anthropicReq.Tools = tools
	}

	return anthropicReq, nil
}

// toAnthropicInputSchema translates an OpenAI tool's JSON-Schema parameters
// (an interface{} that decodes to a map with "type"/"properties"/"required")
// into Anthropic's ToolInputSchemaParam. Anthropic requires input_schema to be
// present with type=object, so a tool with no/!object parameters still gets a
// valid empty-object schema rather than an omitted field.
func toAnthropicInputSchema(params interface{}) anthropic.ToolInputSchemaParam {
	// Default: a valid `{"type":"object","properties":{}}` so the field is
	// always present (Type marshals its zero value as "object").
	schema := anthropic.ToolInputSchemaParam{Properties: map[string]interface{}{}}

	m, ok := params.(map[string]interface{})
	if !ok {
		return schema
	}
	if props, ok := m["properties"]; ok && props != nil {
		schema.Properties = props
	}
	if reqRaw, ok := m["required"].([]interface{}); ok {
		required := make([]string, 0, len(reqRaw))
		for _, r := range reqRaw {
			if s, ok := r.(string); ok {
				required = append(required, s)
			}
		}
		if len(required) > 0 {
			schema.Required = required
		}
	}
	return schema
}

// convertMessage converts a unified (OpenAI-shaped) message to Anthropic
// format, including the tool round-trip: a role=tool result becomes a
// user-side tool_result block, and an assistant turn carrying tool_calls
// becomes tool_use blocks. Without these a multi-turn tool loop breaks the
// moment the caller sends results back.
func (p *AnthropicProvider) convertMessage(msg types.Message) (anthropic.MessageParam, error) {
	// Tool result (role=tool): Anthropic carries results in a USER message as
	// tool_result blocks keyed by the originating tool_use id.
	if msg.Role == "tool" {
		return anthropic.NewUserMessage(
			anthropic.NewToolResultBlock(msg.ToolCallID, messageContentString(msg.Content), false),
		), nil
	}

	// Assistant turn that issued tool calls → tool_use blocks (+ any text).
	if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
		var blocks []anthropic.ContentBlockParamUnion
		if s := messageContentString(msg.Content); s != "" {
			blocks = append(blocks, anthropic.NewTextBlock(s))
		}
		for _, tc := range msg.ToolCalls {
			var input any
			if tc.Function.Arguments != "" {
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
			}
			if input == nil {
				input = map[string]any{}
			}
			blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, input, tc.Function.Name))
		}
		return anthropic.NewAssistantMessage(blocks...), nil
	}

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

// messageContentString flattens a message's content (string or multimodal
// parts) to plain text — used when emitting Anthropic tool_use / tool_result
// blocks, which carry text only.
func messageContentString(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []types.ContentPart:
		var sb strings.Builder
		for _, p := range c {
			if p.Type == "text" {
				sb.WriteString(p.Text)
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// convertFromAnthropicResponse converts Anthropic's response to our format
func (p *AnthropicProvider) convertFromAnthropicResponse(resp *anthropic.Message, req *types.ChatRequest) *types.ChatResponse {
	// Build choices from content blocks
	var choices []types.Choice

	// Normalize Anthropic's stop_reason to the OpenAI finish_reason vocabulary
	// callers (and the linked-tier served-tool detection) expect:
	// end_turn→stop, max_tokens→length, tool_use→tool_calls.
	finishReason := string(resp.StopReason)
	switch resp.StopReason {
	case "end_turn", "stop_sequence":
		finishReason = "stop"
	case "max_tokens":
		finishReason = "length"
	case "tool_use":
		finishReason = "tool_calls"
	}

	choice := types.Choice{
		Index:        0,
		FinishReason: finishReason,
		Message: types.Message{
			Role:    "assistant",
			Content: "", // Will be built from blocks
		},
	}

	// Process content blocks: concatenate text, and surface tool_use blocks
	// as OpenAI-shaped tool_calls so the caller's agent loop sees them. The
	// block's Input is already a JSON object → it becomes the tool_call's
	// arguments string verbatim.
	var textContent strings.Builder
	var toolCalls []types.ToolCall

	for _, block := range resp.Content {
		switch block.Type {
		case "text":
			textContent.WriteString(block.Text)
		case "tool_use":
			// ContentBlockUnion exposes the tool_use fields (ID/Name/Input)
			// at the top level; Input is the args as a JSON object verbatim.
			args := string(block.Input)
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, types.ToolCall{
				ID:   block.ID,
				Type: "function",
				Function: types.Function{
					Name:      block.Name,
					Arguments: args,
				},
			})
		}
	}

	choice.Message.Content = textContent.String()
	choice.Message.ToolCalls = toolCalls
	choices = append(choices, choice)

	// Build usage information
	var usage *types.Usage
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 ||
		resp.Usage.CacheCreationInputTokens > 0 || resp.Usage.CacheReadInputTokens > 0 {
		usage = &types.Usage{
			PromptTokens:        int(resp.Usage.InputTokens),
			CompletionTokens:    int(resp.Usage.OutputTokens),
			TotalTokens:         int(resp.Usage.InputTokens + resp.Usage.OutputTokens),
			CacheCreationTokens: int(resp.Usage.CacheCreationInputTokens),
			CacheReadTokens:     int(resp.Usage.CacheReadInputTokens),
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
