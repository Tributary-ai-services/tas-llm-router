package types

import (
	"time"
)

// Response types
type ChatResponse struct {
	ID                string   `json:"id"`
	Object            string   `json:"object"`
	Created           int64    `json:"created"`
	Model             string   `json:"model"`
	Choices           []Choice `json:"choices"`
	Usage             *Usage   `json:"usage,omitempty"`
	SystemFingerprint string   `json:"system_fingerprint,omitempty"`

	// Routing metadata (added by router)
	RouterMetadata *RouterMetadata `json:"router_metadata,omitempty"`
}

type Choice struct {
	Index        int       `json:"index"`
	Message      Message   `json:"message,omitempty"`
	Delta        *Message  `json:"delta,omitempty"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// Cache-token breakdown for cache-aware cost accounting. Anthropic
	// reports these separately from input_tokens (which is uncached-only);
	// OpenAI reports cached tokens under prompt_tokens_details.cached_tokens.
	// Additive + omitempty — zero/absent for providers or responses without
	// prompt caching, so existing consumers are unaffected.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_tokens,omitempty"`
}

type Logprobs struct {
	Content []TokenLogprob `json:"content,omitempty"`
}

type TokenLogprob struct {
	Token       string       `json:"token"`
	Logprob     float64      `json:"logprob"`
	Bytes       []int        `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob `json:"top_logprobs,omitempty"`
}

type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// StreamError is a terminal error frame carried on the internal chunk stream
// when a vendor dies mid-stream (or the upstream connection breaks). A provider
// emits one final ChatChunk with Error set instead of silently closing the
// channel; the streaming handler renders it as the wire dialect's error event so
// a truncated stream is no longer byte-indistinguishable from a complete one.
type StreamError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Streaming response
type ChatChunk struct {
	ID                string        `json:"id"`
	Object            string        `json:"object"`
	Created           int64         `json:"created"`
	Model             string        `json:"model"`
	Choices           []ChoiceChunk `json:"choices"`
	Usage             *Usage        `json:"usage,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`

	// Error, when non-nil, marks this as a terminal error frame rather than a
	// content chunk. It is consumed by the streaming handler (which turns it
	// into a wire-level error event) and is never itself marshaled as a chunk.
	Error *StreamError `json:"-"`

	// Routing metadata (added by router)
	RouterMetadata *RouterMetadata `json:"router_metadata,omitempty"`
}

type ChoiceChunk struct {
	Index        int       `json:"index"`
	Delta        *Message  `json:"delta,omitempty"`
	FinishReason string    `json:"finish_reason,omitempty"`
	Logprobs     *Logprobs `json:"logprobs,omitempty"`
}

// Router-specific types
type RouterMetadata struct {
	Provider        string        `json:"provider"`
	Model           string        `json:"model"`
	RoutingReason   []string      `json:"routing_reason"`
	EstimatedCost   float64       `json:"estimated_cost"`
	ActualCost      float64       `json:"actual_cost,omitempty"`
	ProcessingTime  time.Duration `json:"processing_time"`
	RequestID       string        `json:"request_id"`
	ProviderLatency time.Duration `json:"provider_latency"`

	// Retry and fallback metadata
	AttemptCount    int      `json:"attempt_count"`              // How many attempts made (1 = no retries)
	FailedProviders []string `json:"failed_providers,omitempty"` // Providers that failed before success
	FallbackUsed    bool     `json:"fallback_used"`              // Whether fallback was triggered
	RetryDelays     []int64  `json:"retry_delays,omitempty"`     // Delay between attempts (ms)
	TotalRetryTime  int64    `json:"total_retry_time,omitempty"` // Total time spent on retries (ms)
}

type CostEstimate struct {
	InputTokens     int     `json:"input_tokens"`
	OutputTokens    int     `json:"output_tokens,omitempty"`
	TotalTokens     int     `json:"total_tokens"`
	InputCost       float64 `json:"input_cost"`
	OutputCost      float64 `json:"output_cost"`
	TotalCost       float64 `json:"total_cost"`
	CostPer1KTokens float64 `json:"cost_per_1k_tokens"`
}

// Error response
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Param   string `json:"param,omitempty"`
	Code    string `json:"code,omitempty"`
}

// Models endpoint response
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelInfo `json:"data"`
}

// OpenAIModel is the OpenAI-native model object returned by GET /v1/models so a
// stock OpenAI SDK's client.models.list()/retrieve() deserializes correctly.
// It is deliberately distinct from ModelInfo (the rich internal capability
// record) — the OpenAI wire shape is just id/object/created/owned_by.
type OpenAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`   // always "model"
	Created int64  `json:"created"`  // creation epoch; 0 when unknown
	OwnedBy string `json:"owned_by"` // provider name (e.g. "openai", "anthropic")
}

// OpenAIModelList is the {object:"list", data:[…]} envelope OpenAI's
// GET /v1/models returns.
type OpenAIModelList struct {
	Object string        `json:"object"` // always "list"
	Data   []OpenAIModel `json:"data"`
}

// AnthropicModel is the Anthropic-native model object returned by GET /v1/models
// when the caller is the Anthropic SDK (detected via the anthropic-version
// header). Distinct wire shape from OpenAIModel: type/id/display_name/created_at.
type AnthropicModel struct {
	Type        string `json:"type"` // always "model"
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at,omitempty"` // RFC3339; omitted when unknown
}

// AnthropicModelList is Anthropic's list envelope for GET /v1/models.
type AnthropicModelList struct {
	Data    []AnthropicModel `json:"data"`
	HasMore bool             `json:"has_more"`
	FirstID string           `json:"first_id,omitempty"`
	LastID  string           `json:"last_id,omitempty"`
}

// EmbeddingRequest is the OpenAI-compatible POST /v1/embeddings request.
type EmbeddingRequest struct {
	Model          string      `json:"model"`
	Input          interface{} `json:"input"` // string | []string | []int | [][]int
	EncodingFormat string      `json:"encoding_format,omitempty"`
	Dimensions     *int        `json:"dimensions,omitempty"`
	User           string      `json:"user,omitempty"`
}

// EmbeddingResponse is the OpenAI-compatible embeddings response.
type EmbeddingResponse struct {
	Object string          `json:"object"` // "list"
	Data   []EmbeddingData `json:"data"`
	Model  string          `json:"model"`
	Usage  *EmbeddingUsage `json:"usage,omitempty"`
}

type EmbeddingData struct {
	Object    string    `json:"object"` // "embedding"
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type EmbeddingUsage struct {
	PromptTokens int `json:"prompt_tokens"`
	TotalTokens  int `json:"total_tokens"`
}
