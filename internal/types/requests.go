package types

import (
	"time"
)

// Core request/response types
type ChatRequest struct {
	ID               string                 `json:"id"`
	Model            string                 `json:"model"`
	Messages         []Message              `json:"messages"`
	Temperature      *float32               `json:"temperature,omitempty"`
	MaxTokens        *int                   `json:"max_tokens,omitempty"`
	TopP             *float32               `json:"top_p,omitempty"`
	FrequencyPenalty *float32               `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float32               `json:"presence_penalty,omitempty"`
	Stop             []string               `json:"stop,omitempty"`
	Stream           bool                   `json:"stream"`
	Functions        []Function             `json:"functions,omitempty"`
	FunctionCall     interface{}            `json:"function_call,omitempty"`
	Tools            []Tool                 `json:"tools,omitempty"`
	ToolChoice       interface{}            `json:"tool_choice,omitempty"`
	ResponseFormat   *ResponseFormat        `json:"response_format,omitempty"`
	Seed             *int                   `json:"seed,omitempty"`
	
	// Routing hints
	OptimizeFor      OptimizationType       `json:"optimize_for,omitempty"`
	RequiredFeatures []string               `json:"required_features,omitempty"`
	MaxCost          *float64               `json:"max_cost,omitempty"`
	
	// Retry and fallback controls
	RetryConfig      *RetryConfig           `json:"retry_config,omitempty"`
	FallbackConfig   *FallbackConfig        `json:"fallback_config,omitempty"`
	
	// Metadata
	UserID           string                 `json:"user_id"`
	ApplicationID    string                 `json:"application_id"`
	Timestamp        time.Time              `json:"timestamp"`
}

type Message struct {
	Role      string      `json:"role"`
	Content   interface{} `json:"content"` // string or []ContentPart for multimodal
	Name      string      `json:"name,omitempty"`
	ToolCalls []ToolCall  `json:"tool_calls,omitempty"`
}

type ContentPart struct {
	Type     string    `json:"type"` // "text" or "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

type Function struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function,omitempty"`
}

type ToolCall struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type ResponseFormat struct {
	Type       string      `json:"type"` // "text", "json_object", "json_schema"
	JSONSchema *JSONSchema `json:"json_schema,omitempty"`
}

type JSONSchema struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description,omitempty"`
	Schema      map[string]interface{} `json:"schema"`
	Strict      bool                   `json:"strict,omitempty"` // OpenAI specific
}

// Enums and supporting types
type OptimizationType string

const (
	OptimizeCost        OptimizationType = "cost"
	OptimizePerformance OptimizationType = "performance"
	OptimizeQuality     OptimizationType = "quality"
)

// Batch processing types
type BatchRequest struct {
	InputFileID      string `json:"input_file_id"`
	Endpoint         string `json:"endpoint"`
	CompletionWindow string `json:"completion_window"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"`
}

type BatchResponse struct {
	ID               string                 `json:"id"`
	Object           string                 `json:"object"`
	Endpoint         string                 `json:"endpoint"`
	Errors           []BatchError           `json:"errors,omitempty"`
	InputFileID      string                 `json:"input_file_id"`
	CompletionWindow string                 `json:"completion_window"`
	Status           string                 `json:"status"`
	OutputFileID     string                 `json:"output_file_id,omitempty"`
	ErrorFileID      string                 `json:"error_file_id,omitempty"`
	CreatedAt        int64                  `json:"created_at"`
	InProgressAt     int64                  `json:"in_progress_at,omitempty"`
	ExpiresAt        int64                  `json:"expires_at,omitempty"`
	CompletedAt      int64                  `json:"completed_at,omitempty"`
	FailedAt         int64                  `json:"failed_at,omitempty"`
	ExpiredAt        int64                  `json:"expired_at,omitempty"`
	CancelledAt      int64                  `json:"cancelled_at,omitempty"`
	RequestCounts    BatchRequestCounts     `json:"request_counts"`
	Metadata         map[string]interface{} `json:"metadata,omitempty"`
}

type BatchError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Param   string `json:"param,omitempty"`
	Line    int    `json:"line,omitempty"`
}

type BatchRequestCounts struct {
	Total     int `json:"total"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
}

// Assistant types
type AssistantRequest struct {
	Model        string                 `json:"model"`
	Name         string                 `json:"name,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Instructions string                 `json:"instructions,omitempty"`
	Tools        []Tool                 `json:"tools,omitempty"`
	FileIDs      []string               `json:"file_ids,omitempty"`
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
}

type AssistantResponse struct {
	ID           string                 `json:"id"`
	Object       string                 `json:"object"`
	CreatedAt    int64                  `json:"created_at"`
	Name         string                 `json:"name,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Model        string                 `json:"model"`
	Instructions string                 `json:"instructions,omitempty"`
	Tools        []Tool                 `json:"tools"`
	FileIDs      []string               `json:"file_ids"`
	Metadata     map[string]interface{} `json:"metadata"`
}

// Retry and fallback control structures
type RetryConfig struct {
	MaxAttempts     int           `json:"max_attempts"`               // 0 = no retry, 1-5 allowed  
	BackoffType     string        `json:"backoff_type"`               // "linear", "exponential"
	BaseDelay       time.Duration `json:"base_delay"`                 // Starting delay (e.g., 1s)
	MaxDelay        time.Duration `json:"max_delay"`                  // Cap on delay (e.g., 30s)
	RetryableErrors []string      `json:"retryable_errors,omitempty"` // Which errors to retry
}

type FallbackConfig struct {
	Enabled             bool     `json:"enabled"`                          // Enable fallback to healthy providers
	PreferredChain      []string `json:"preferred_chain,omitempty"`        // Custom fallback order
	MaxCostIncrease     *float64 `json:"max_cost_increase,omitempty"`      // Max % cost increase allowed (e.g., 0.5 = 50%)
	RequireSameFeatures bool     `json:"require_same_features"`            // Must support same capabilities
}