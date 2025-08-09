package types

import (
	"time"
)

// Response types
type ChatResponse struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []Choice           `json:"choices"`
	Usage             *Usage             `json:"usage,omitempty"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
	
	// Routing metadata (added by router)
	RouterMetadata    *RouterMetadata    `json:"router_metadata,omitempty"`
}

type Choice struct {
	Index        int          `json:"index"`
	Message      Message      `json:"message,omitempty"`
	Delta        *Message     `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Logprobs     *Logprobs    `json:"logprobs,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type Logprobs struct {
	Content []TokenLogprob `json:"content,omitempty"`
}

type TokenLogprob struct {
	Token   string             `json:"token"`
	Logprob float64            `json:"logprob"`
	Bytes   []int              `json:"bytes,omitempty"`
	TopLogprobs []TopLogprob    `json:"top_logprobs,omitempty"`
}

type TopLogprob struct {
	Token   string  `json:"token"`
	Logprob float64 `json:"logprob"`
	Bytes   []int   `json:"bytes,omitempty"`
}

// Streaming response
type ChatChunk struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	Created           int64              `json:"created"`
	Model             string             `json:"model"`
	Choices           []ChoiceChunk      `json:"choices"`
	Usage             *Usage             `json:"usage,omitempty"`
	SystemFingerprint string             `json:"system_fingerprint,omitempty"`
	
	// Routing metadata (added by router)
	RouterMetadata    *RouterMetadata    `json:"router_metadata,omitempty"`
}

type ChoiceChunk struct {
	Index        int          `json:"index"`
	Delta        *Message     `json:"delta,omitempty"`
	FinishReason string       `json:"finish_reason,omitempty"`
	Logprobs     *Logprobs    `json:"logprobs,omitempty"`
}

// Router-specific types
type RouterMetadata struct {
	Provider         string        `json:"provider"`
	Model            string        `json:"model"`
	RoutingReason    []string      `json:"routing_reason"`
	EstimatedCost    float64       `json:"estimated_cost"`
	ActualCost       float64       `json:"actual_cost,omitempty"`
	ProcessingTime   time.Duration `json:"processing_time"`
	RequestID        string        `json:"request_id"`
	ProviderLatency  time.Duration `json:"provider_latency"`
}

type CostEstimate struct {
	InputTokens      int     `json:"input_tokens"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	TotalTokens      int     `json:"total_tokens"`
	InputCost        float64 `json:"input_cost"`
	OutputCost       float64 `json:"output_cost"`
	TotalCost        float64 `json:"total_cost"`
	CostPer1KTokens  float64 `json:"cost_per_1k_tokens"`
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