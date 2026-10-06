package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/sirupsen/logrus"

	"github.com/tributary-ai/llm-router-waf/internal/instrumentation"
	"github.com/tributary-ai/llm-router-waf/internal/providers"
	"github.com/tributary-ai/llm-router-waf/internal/types"
	"github.com/tributary-ai/llm-router-waf/internal/upstreamkey"
	"github.com/tributary-ai/llm-router-waf/pkg/aiqg/metrics"
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

// clientFor returns a per-request client keyed by the BYOK override on ctx
// (Plan #14), or the statically-configured client when none is set. The
// override branch is inert until a tenant stores a key, so default traffic is
// unchanged. SDK client construction is a cheap HTTP-wrapper build.
func (p *AnthropicProvider) clientFor(ctx context.Context) *anthropic.Client {
	key := upstreamkey.From(ctx)
	if key == "" {
		return p.client
	}
	opts := []option.RequestOption{option.WithAPIKey(key)}
	if p.config.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(p.config.BaseURL))
	}
	c := anthropic.NewClient(opts...)
	return &c
}

// GetProviderName returns the provider name
func (p *AnthropicProvider) GetProviderName() string {
	return "anthropic"
}

// GetCapabilities returns the capabilities of the Anthropic provider
// minThinkingBudget is the vendor's floor for an explicit thinking budget
// (1024 tokens). Below it the request is rejected, so a smaller ask cannot be
// honoured and is recorded as a drop instead.
const minThinkingBudget = 1024

// hasRestrictedParams reports whether a model is in Anthropic's 4.7+
// generation, which refuses temperature, top_p and top_k. It reads the
// DECLARED catalog flag rather than pattern-matching the name, for the same
// reason the OpenAI side does: a prefix rule mis-handles the next family
// silently. An unknown model falls back to sending the parameters, which is
// the pre-RT-5 behaviour and so cannot regress a model that works today.
func (p *AnthropicProvider) hasRestrictedParams(model string) bool {
	for _, m := range p.config.Models {
		if strings.EqualFold(m.Name, model) || strings.EqualFold(m.ProviderModelID, model) {
			return m.RestrictedParams
		}
	}
	return false
}

func (p *AnthropicProvider) GetCapabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{
		ProviderName:              "anthropic",
		SupportedModels:           p.config.Models,
		SupportsFunctions:         true,  // Tool use
		SupportsParallelFunctions: false, // Claude doesn't support parallel tool calls
		// EFFECTIVE (gateway) capability, not the vendor's: the request
		// translation layer has no arm for multimodal image content, so vision
		// does not work end-to-end through the gateway even though Claude
		// supports it. Reporting the vendor flag here misleads an integrator who
		// checks /v1/capabilities before building (#174). Flip to true only when
		// the translation layer carries image content.
		SupportsVision:           false,
		SupportsStructuredOutput: false, // No strict JSON schema mode
		SupportsStreaming:        true,
		SupportsAssistants:       false,  // No assistants API
		SupportsBatch:            false,  // No batch API yet
		MaxContextWindow:         200000, // Claude-3.5 Sonnet context window
		SupportedImageFormats:    []string{"png", "jpeg", "webp", "gif"},
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
	resp, err := p.clientFor(ctx).Messages.New(ctx, *anthropicReq)
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
	stream := p.clientFor(ctx).Messages.NewStreaming(ctx, *anthropicReq)

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
				select {
				case chunks <- &types.ChatChunk{Error: &types.StreamError{
					Message: err.Error(), Type: "upstream_stream_error"}}:
				case <-ctx.Done():
				}
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
			select {
			case chunks <- &types.ChatChunk{Error: &types.StreamError{
				Message: stream.Err().Error(), Type: "upstream_stream_error"}}:
			case <-ctx.Done():
			}
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

// convertStreamEvent converts an Anthropic streaming event to our ChatChunk format.
//
// # Tool calls must be carried, not dropped
//
// This function used to handle exactly two events — a text delta and
// message_start — so every tool-use event returned nil and a streamed tool call
// was silently discarded. Measured 2026-10-05 on the same request one flag
// apart: non-streaming returned stop_reason=tool_use with the block and its
// parsed input, while streaming returned NO content blocks and
// stop_reason=end_turn, both having spent the same 50 output tokens generating
// the call. The vendor produced it; we threw it away.
//
// The blast radius was every streaming agent against an Anthropic upstream, on
// both wire formats, because this sits upstream of format translation. Claude
// Code found it immediately — it always streams and it lives on tool calls — and
// reported `success` with an empty result, which is how a dropped tool call
// looks from the outside.
//
// anthropicEventHasContent, just above, has always handled InputJSONDelta for
// TTFT and calls it "tool-use argument fragments … for agentic workflows", so
// the gap was an oversight rather than a decision.
//
// # The shape the encoders expect
//
// The downstream encoders accumulate by ToolCall.ID: a call arrives with an id
// and a name, and later argument fragments arrive with an EMPTY id to continue
// the most recent call (anthropicStreamEncoder.bufferTool). That is exactly how
// Anthropic streams a tool call — content_block_start carries id+name,
// input_json_delta carries the argument text — so the mapping is one to one.
func (p *AnthropicProvider) convertStreamEvent(event anthropic.MessageStreamEventUnion, req *types.ChatRequest, message *anthropic.Message) *types.ChatChunk {
	chunk := func(delta *types.Message) *types.ChatChunk {
		return &types.ChatChunk{
			ID:      message.ID,
			Object:  "chat.completion.chunk",
			Created: time.Now().Unix(),
			Model:   req.Model,
			Choices: []types.ChoiceChunk{{Index: 0, Delta: delta}},
		}
	}

	switch variant := event.AsAny().(type) {
	case anthropic.ContentBlockStartEvent:
		// A tool_use block opens here, and this is the ONLY event carrying its
		// id and name. Text blocks need no opening chunk — their deltas carry
		// everything the encoders need.
		switch cb := variant.ContentBlock.AsAny().(type) {
		case anthropic.ToolUseBlock:
			return chunk(&types.Message{
				Role: "assistant",
				ToolCalls: []types.ToolCall{{
					ID:       cb.ID,
					Type:     "function",
					Function: types.Function{Name: cb.Name},
				}},
			})
		case anthropic.RedactedThinkingBlock:
			// Redacted reasoning arrives COMPLETE in the start event with no
			// deltas following, so it is forwarded here or not at all.
			return chunk(&types.Message{
				Role:      "assistant",
				Reasoning: []types.ContentPart{{Type: "redacted_thinking", Data: cb.Data}},
			})
		}
		// A thinking block's start event carries nothing but an empty string;
		// the content arrives as deltas, so the encoder opens the block lazily
		// on the first one, exactly as it does for text.
	case anthropic.ContentBlockDeltaEvent:
		switch delta := variant.Delta.AsAny().(type) {
		case anthropic.TextDelta:
			return chunk(&types.Message{Role: "assistant", Content: delta.Text})
		case anthropic.ThinkingDelta:
			return chunk(&types.Message{
				Role:      "assistant",
				Reasoning: []types.ContentPart{{Type: "thinking", Thinking: delta.Thinking}},
			})
		case anthropic.SignatureDelta:
			// The signature arrives in its own event at the END of the block,
			// and it is the part that makes the reasoning replayable. A client
			// that receives the thinking text without it cannot send the turn
			// back, so this event matters even when the text was empty --
			// which is the normal case, since display defaults to "omitted" on
			// the 4.7+ generation.
			return chunk(&types.Message{
				Role:      "assistant",
				Reasoning: []types.ContentPart{{Type: "thinking", Signature: delta.Signature}},
			})
		case anthropic.InputJSONDelta:
			// Argument fragment for the block opened above. The id is left
			// empty deliberately: that is how the encoders know this continues
			// the current call rather than starting another one.
			return chunk(&types.Message{
				Role: "assistant",
				ToolCalls: []types.ToolCall{{
					Type:     "function",
					Function: types.Function{Arguments: delta.PartialJSON},
				}},
			})
		}
	case anthropic.MessageStartEvent:
		// Send the initial chunk with role — and with the input-token count,
		// which arrives ONLY here. Without it a streamed response reported
		// input_tokens: 0 to the client for the whole stream (the event record
		// was always correct), so a client sizing its context from the response
		// — as Claude Code does, to decide when to compact — read zero.
		c := chunk(&types.Message{Role: "assistant"})
		if in := variant.Message.Usage.InputTokens; in > 0 {
			c.Usage = &types.Usage{
				PromptTokens:        int(in),
				CacheCreationTokens: int(variant.Message.Usage.CacheCreationInputTokens),
				CacheReadTokens:     int(variant.Message.Usage.CacheReadInputTokens),
				TotalTokens:         int(in),
			}
		}
		return c
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
// ProbeModel tests whether a model answers by sending a minimal (1-token)
// request. It satisfies registry/adapters.AnthropicModelProber so the model
// registry can validate Anthropic models, which have no list-models API (#2).
//
// Contract (per AnthropicModelProber):
//   - (true, nil)  the model answered — available.
//   - (false, nil) the vendor definitively rejected it (HTTP 404 not_found) —
//     unavailable.
//   - (false, err) transient/unknown failure — the caller must NOT downgrade.
func (p *AnthropicProvider) ProbeModel(ctx context.Context, model string) (bool, error) {
	if model == "" {
		return false, fmt.Errorf("anthropic probe: empty model")
	}
	req := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("test"))},
		MaxTokens: 1,
	}
	if _, err := p.clientFor(ctx).Messages.New(ctx, req); err != nil {
		if probeErrorMeansUnavailable(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// probeErrorMeansUnavailable reports whether a probe error is a DEFINITIVE
// "this model does not exist" — an HTTP 404, which Anthropic returns as a
// not_found_error for an unknown model id — rather than a transient failure
// (network, rate limit, overload) that must never mark a real model
// unavailable.
func probeErrorMeansUnavailable(err error) bool {
	var apiErr *anthropic.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}

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
	// Effective gateway capability: the translation layer does not carry image
	// content (#174). Not the vendor's raw flag.
	return false
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
	// systemCached records whether the caller asked for a prompt-cache
	// breakpoint at the end of the system block. That single placement caches
	// tools AND system together — tools render first — which for agent traffic
	// is the largest, safest win available (see
	// docs/AIQG-PROMPT-CACHE-CONTROL.md §4.1).
	var systemCached bool
	// systemBlocks is the block-preserving form. The vendor's `system` field is
	// an ARRAY, and a caller who sent several blocks meant several blocks:
	// merging them moves every cache_control and, when the first block is a
	// marker the vendor consumes on its own (Claude Code's
	// `x-anthropic-billing-header:` line), it feeds the whole merged block to
	// that consumption and the real prompt disappears. See
	// server.anthropicSystemToMessage for the measurement.
	var systemBlocks []anthropic.TextBlockParam
	var messages []anthropic.MessageParam

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			// Claude handles system messages separately
			switch content := msg.Content.(type) {
			case string:
				systemMessage = content
			default:
				parts, ok := systemPartsFrom(content)
				if !ok {
					return nil, fmt.Errorf("system messages must be text only for Anthropic")
				}
				for _, part := range parts {
					if part.Type != "text" {
						// A non-text system block has nowhere to go: the vendor's
						// system field holds text blocks only.
						return nil, fmt.Errorf("system messages must be text only for Anthropic")
					}
					block := anthropic.TextBlockParam{Text: part.Text, Type: "text"}
					if part.CacheControl != nil {
						block.CacheControl = ephemeralCacheControl()
					}
					systemBlocks = append(systemBlocks, block)
				}
			}
			if msg.CacheControl != nil {
				systemCached = true
			}
			continue
		}

		// Convert regular messages
		anthropicMsg, err := p.convertMessage(msg)
		if err != nil {
			return nil, err
		}
		// Honour a message-level breakpoint on a non-system message. Before #100
		// only the system block's cache_control was threaded; a cache_control on
		// a user/assistant/tool message reached the type and was then dropped,
		// so passthrough silently failed for exactly the agentic turns where it
		// pays most.
		if msg.CacheControl != nil {
			markLastBlockCached(&anthropicMsg)
		}
		messages = append(messages, anthropicMsg)
	}

	// Build the request
	anthropicReq := &anthropic.MessageNewParams{
		Model:    anthropic.Model(req.Model),
		Messages: messages,
	}

	// Set system message if present. The block-preserving form wins when it is
	// populated; the string form remains the path for the ordinary
	// single-block/other-provider case.
	if len(systemBlocks) > 0 {
		if systemCached {
			// A message-level breakpoint means "cache through the end of the
			// system prompt", which is the last block.
			last := &systemBlocks[len(systemBlocks)-1]
			if last.CacheControl.Type == "" {
				last.CacheControl = ephemeralCacheControl()
			}
		}
		anthropicReq.System = systemBlocks
	} else if systemMessage != "" {
		block := anthropic.TextBlockParam{Text: systemMessage, Type: "text"}
		if systemCached {
			// The pinned SDK's CacheControlEphemeralParam carries Type only —
			// no TTL field — so a caller's 1h request cannot be expressed here
			// and silently gets the 5m default. Honouring the breakpoint at
			// the wrong TTL is still far better than dropping it, which is
			// what happened before this existed. Upgrading the SDK is tracked
			// separately (docs/AIQG-PROMPT-CACHE-CONTROL.md §7).
			block.CacheControl = ephemeralCacheControl()
		}
		anthropicReq.System = []anthropic.TextBlockParam{block}
	}

	// Set optional parameters
	if req.MaxTokens != nil {
		anthropicReq.MaxTokens = int64(*req.MaxTokens)
	} else {
		anthropicReq.MaxTokens = 1024 // Anthropic requires max_tokens
	}

	// The 4.7+ generation (opus-4-7/4-8/5/5-5, sonnet-5/5-5, fable-5/5-1)
	// rejects temperature, top_p and top_k as "deprecated for this model" --
	// a hard 400 that reaches the caller as a 500. Measured 2026-10-06 one
	// parameter at a time; stop_sequences and max_tokens are still fine here,
	// which is where this differs from OpenAI's restricted set. Temperature 1
	// is accepted and so is preserved rather than dropped.
	//
	// Dropping is the only way to serve the request, but a caller that asked
	// for temperature 0 and silently got the default gets a different answer
	// -- and on this platform that also changes cache behaviour and what the
	// judge scores -- so each drop is counted. See RT-5.
	restricted := p.hasRestrictedParams(req.Model)

	if req.Temperature != nil {
		if !restricted || *req.Temperature == 1 {
			anthropicReq.Temperature = anthropic.Float(float64(*req.Temperature))
		} else {
			metrics.ParamDroppedTotal.WithLabelValues("anthropic", "temperature").Inc()
		}
	}

	// Anthropic refuses temperature and top_p TOGETHER on every model -- 400
	// "`temperature` and `top_p` cannot both be specified for this model.
	// Please use only one." Either alone is fine. Measured 2026-10-06 on
	// claude-haiku-4-5, i.e. on an UNrestricted model, so this is independent
	// of the 4.7+ restriction above and breaks the older models too.
	//
	// OpenAI accepts both, so an OpenAI-shaped client that sets both is a
	// perfectly ordinary request here and must not 500. temperature wins
	// because it is the knob nearly every client sets and the one callers
	// reason about; top_p is the specialist alternative. The drop is counted,
	// so "we ignored your top_p" is visible rather than silent.
	if req.TopP != nil {
		switch {
		case restricted:
			metrics.ParamDroppedTotal.WithLabelValues("anthropic", "top_p").Inc()
		case anthropicReq.Temperature.Valid():
			metrics.ParamDroppedTotal.WithLabelValues("anthropic", "top_p_with_temperature").Inc()
		default:
			anthropicReq.TopP = anthropic.Float(float64(*req.TopP))
		}
	}

	// Extended thinking (RT-6, built on RT-5's declared-restriction flag).
	//
	// The two generations disagree about how to ask for it, and the pinned SDK
	// can express only one of the two forms:
	//   pre-4.7  : {type:"enabled", budget_tokens:N} — accepted, so passed on.
	//   4.7+     : budget_tokens is REJECTED (400) and {type:"disabled"} is too;
	//              thinking is on by default and omitting the parameter runs it
	//              ADAPTIVELY, which is what a modern client wants anyway.
	// So the restricted branch sends nothing — and that is the fix, not a
	// concession: the caller asked for extended thinking and gets it, chosen by
	// the model instead of by a budget the model would refuse. `adaptive` has no
	// representation in SDK v1.7.0 at all, so this is also the only form
	// available without an SDK upgrade.
	//
	// Every divergence from what was asked is counted, because thinking changes
	// the answer, the latency and the bill, and a silent substitution here would
	// be indistinguishable from the pre-RT-6 behaviour of dropping it entirely.
	if t := req.Thinking; t != nil {
		switch {
		case restricted:
			// Omit: the vendor runs adaptive thinking. Record what was elided.
			if t.BudgetTokens > 0 {
				metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_budget").Inc()
			}
			if strings.EqualFold(t.Type, "disabled") {
				// Refusing to disable is the vendor's rule, not ours; counted so
				// a caller who asked for no thinking can see they got some.
				metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_disabled").Inc()
			}
		case strings.EqualFold(t.Type, "disabled"):
			anthropicReq.Thinking = anthropic.ThinkingConfigParamUnion{
				OfDisabled: &anthropic.ThinkingConfigDisabledParam{},
			}
		case t.BudgetTokens >= minThinkingBudget:
			budget := int64(t.BudgetTokens)
			// The budget must stay below max_tokens or the vendor 400s. Clamping
			// serves the request; dropping to the floor would silently change
			// the depth asked for, so the clamp is counted either way.
			if anthropicReq.MaxTokens > 0 && budget >= anthropicReq.MaxTokens {
				budget = anthropicReq.MaxTokens - 1
				metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_budget_clamped").Inc()
			}
			if budget >= minThinkingBudget {
				anthropicReq.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
			} else {
				// max_tokens is too small to leave room for the minimum budget.
				metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_budget").Inc()
			}
		default:
			// An "enabled"/"adaptive" request with no usable budget on a model
			// that requires one. Omitting is the only legal shape.
			metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_budget").Inc()
		}
		if t.Display != "" {
			// The pinned SDK has no field for it; say so rather than imply the
			// client's visibility choice was honoured.
			metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_display").Inc()
		}
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
				// A breakpoint on a tool caches the whole tool block (tools
				// render first), the cheapest large win for agent traffic whose
				// tool definitions are big and identical every turn. Threaded
				// per #100; before this it was dropped like the message blocks.
				if tool.CacheControl != nil {
					anthropicTool.OfTool.CacheControl = ephemeralCacheControl()
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
			// Reasoning blocks first: they must keep their position and their
			// signature, which is what the vendor verifies on replay.
			switch part.Type {
			case "thinking":
				if part.Signature != "" {
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfThinking: &anthropic.ThinkingBlockParam{
							Thinking: part.Thinking, Signature: part.Signature,
						},
					})
				} else {
					// Unsigned: the vendor would reject it. Dropping is the only
					// safe option, and it is counted so the loss is visible.
					metrics.ParamDroppedTotal.WithLabelValues("anthropic", "thinking_block_unsigned").Inc()
				}
				continue
			case "redacted_thinking":
				if part.Data != "" {
					blocks = append(blocks, anthropic.ContentBlockParamUnion{
						OfRedactedThinking: &anthropic.RedactedThinkingBlockParam{Data: part.Data},
					})
				}
				continue
			}
			if part.Type == "text" {
				blk := anthropic.NewTextBlock(part.Text)
				// Per-block breakpoint (#100 passthrough): a cache_control on
				// this specific content part caches the prefix through it.
				if part.CacheControl != nil {
					setBlockCacheControl(&blk)
				}
				blocks = append(blocks, blk)
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

// setBlockCacheControl marks one content block as a vendor prompt-cache
// breakpoint, on whichever block variant the union actually holds.
//
// The pinned SDK's CacheControlEphemeralParam carries Type only — no TTL field
// — so a caller's 1h request cannot be expressed and lands at the 5m default
// (SDK upgrade tracked in docs/AIQG-PROMPT-CACHE-CONTROL.md §7). Honouring the
// breakpoint at the wrong TTL is far better than dropping it, which is what the
// gateway did before #100 for every breakpoint that was not on the system
// block.
func setBlockCacheControl(b *anthropic.ContentBlockParamUnion) {
	switch {
	case b.OfText != nil:
		b.OfText.CacheControl = ephemeralCacheControl()
	case b.OfImage != nil:
		b.OfImage.CacheControl = ephemeralCacheControl()
	case b.OfToolUse != nil:
		b.OfToolUse.CacheControl = ephemeralCacheControl()
	case b.OfToolResult != nil:
		b.OfToolResult.CacheControl = ephemeralCacheControl()
	}
}

// systemPartsFrom coerces a system message's content into content parts.
//
// The in-process path hands over []types.ContentPart directly. The round-trip
// path ([]interface{}, after the request has been through JSON) is handled too,
// because a system prompt silently degrading to "must be text only" on whichever
// path happens to re-serialize is the kind of difference nobody finds until it
// is in production.
func systemPartsFrom(content any) ([]types.ContentPart, bool) {
	switch c := content.(type) {
	case []types.ContentPart:
		return c, true
	case []interface{}:
		raw, err := json.Marshal(c)
		if err != nil {
			return nil, false
		}
		var parts []types.ContentPart
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, false
		}
		return parts, true
	default:
		return nil, false
	}
}

// ephemeralCacheControl builds a breakpoint that actually serializes. The SDK
// tags CacheControlEphemeralParam `omitzero`, and its only field is a `constant`
// whose zero value is empty — so a bare CacheControlEphemeralParam{} marshals to
// NOTHING and the breakpoint silently vanishes on the wire. Type must be set
// explicitly for the field to appear. (This is why the system-block breakpoint
// looked wired but produced no cache_control before #100's provider fix.)
func ephemeralCacheControl() anthropic.CacheControlEphemeralParam {
	cc := anthropic.CacheControlEphemeralParam{}
	cc.Type = "ephemeral"
	return cc
}

// markLastBlockCached puts a breakpoint at the end of a message. A message-level
// cache_control means "cache everything through the end of this message" (see
// types.Message.CacheControl), and Anthropic caches the prefix up to and
// including the block the breakpoint sits on — so the last block is the right
// place.
func markLastBlockCached(m *anthropic.MessageParam) {
	if m == nil || len(m.Content) == 0 {
		return
	}
	setBlockCacheControl(&m.Content[len(m.Content)-1])
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
	var reasoning []types.ContentPart

	for _, block := range resp.Content {
		switch block.Type {
		case "thinking":
			// Carried with its signature so the caller can replay the turn. The
			// text may be empty (display:"omitted" is the vendor default on the
			// 4.7+ generation) — the signature is the part that matters.
			tb := block.AsThinking()
			reasoning = append(reasoning, types.ContentPart{
				Type: "thinking", Thinking: tb.Thinking, Signature: tb.Signature,
			})
			continue
		case "redacted_thinking":
			rb := block.AsRedactedThinking()
			reasoning = append(reasoning, types.ContentPart{
				Type: "redacted_thinking", Data: rb.Data,
			})
			continue
		}
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
	choice.Message.Reasoning = reasoning
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
