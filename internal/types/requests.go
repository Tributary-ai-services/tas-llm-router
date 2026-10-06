package types

import (
	"time"
)

// Core request/response types
type ChatRequest struct {
	ID               string          `json:"id"`
	Model            string          `json:"model"`
	Messages         []Message       `json:"messages"`
	Temperature      *float32        `json:"temperature,omitempty"`
	MaxTokens        *int            `json:"max_tokens,omitempty"`
	TopP             *float32        `json:"top_p,omitempty"`
	FrequencyPenalty *float32        `json:"frequency_penalty,omitempty"`
	PresencePenalty  *float32        `json:"presence_penalty,omitempty"`
	Stop             []string        `json:"stop,omitempty"`
	Stream           bool            `json:"stream"`
	Functions        []Function      `json:"functions,omitempty"`
	FunctionCall     interface{}     `json:"function_call,omitempty"`
	Tools            []Tool          `json:"tools,omitempty"`
	ToolChoice       interface{}     `json:"tool_choice,omitempty"`
	ResponseFormat   *ResponseFormat `json:"response_format,omitempty"`
	Seed             *int            `json:"seed,omitempty"`

	// Routing hints
	OptimizeFor      OptimizationType `json:"optimize_for,omitempty"`
	RequiredFeatures []string         `json:"required_features,omitempty"`
	MaxCost          *float64         `json:"max_cost,omitempty"`

	// Betas is the client's `anthropic-beta` header, split on commas. Carried
	// because the gateway re-serialises every request rather than proxying it,
	// so a header the caller set reaches the vendor only if something puts it
	// back. Which of these are actually forwarded is the provider's decision,
	// not the caller's -- see the allowlist in the Anthropic provider.
	Betas []string `json:"betas,omitempty"`

	// Thinking is the client's extended-thinking request. Carried as our own
	// type rather than the vendor's because the two generations disagree about
	// how to express it: pre-4.7 models take {type:"enabled", budget_tokens:N},
	// while the 4.7+ generation REJECTS budget_tokens outright and runs
	// adaptive thinking when the parameter is simply absent. The provider
	// decides which shape to send; this records only what the caller asked for.
	Thinking *ThinkingConfig `json:"thinking,omitempty"`

	// Retry and fallback controls
	RetryConfig    *RetryConfig    `json:"retry_config,omitempty"`
	FallbackConfig *FallbackConfig `json:"fallback_config,omitempty"`

	// Metadata
	UserID        string    `json:"user_id"`
	ApplicationID string    `json:"application_id"`
	Timestamp     time.Time `json:"timestamp"`
}

type Message struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"` // string or []ContentPart for multimodal
	Name    string      `json:"name,omitempty"`
	// CacheControl marks a breakpoint at the end of this message. Provided at
	// message level as well as block level because the common case — "cache
	// everything through the end of my system prompt" — is awkward to express
	// when Content is a plain string, which is how most callers send it.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
	// Reasoning holds extended-thinking blocks for this turn, kept OUT of
	// Content on purpose: Content is what the OpenAI-shaped surface renders as
	// a string, and folding reasoning into it would change that wire format for
	// every existing client. The Anthropic renderer emits these; the OpenAI one
	// ignores them, which is the correct behaviour for a format that has no
	// equivalent.
	Reasoning  []ContentPart          `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall             `json:"tool_calls,omitempty"`
	ToolCallID string                 `json:"tool_call_id,omitempty"` // For tool result messages (role=tool)
	Metadata   map[string]interface{} `json:"metadata,omitempty"`     // Per-message metadata (e.g. trust: "pre_scanned")
}

type ContentPart struct {
	Type     string    `json:"type"` // "text" | "image_url" | "thinking" | "redacted_thinking"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	// Thinking and Signature carry an extended-thinking block through the
	// gateway. The signature is what makes the block REPLAYABLE: the vendor
	// verifies it when the block is sent back in a later turn, so a block
	// forwarded without its signature is worse than one dropped — it looks
	// like reasoning the model can trust and is rejected.
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	// Data is a redacted_thinking block's opaque payload. It is not readable
	// and must be round-tripped verbatim.
	Data string `json:"data,omitempty"`
	// CacheControl marks this block as a vendor prompt-cache breakpoint.
	//
	// Until this field existed the gateway DROPPED a client's cache_control
	// silently — Go's encoding/json discards unknown fields — so callers could
	// ask for prompt caching, get no error, and pay full price forever
	// (tas-llm-router#100). We even read cache-token usage back, reporting a
	// saving we could never request.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

// ThinkingConfig is a client's extended-thinking request.
//
// Why this is not a passthrough of the vendor's own type: the pinned SDK can
// express only {type:"enabled", budget_tokens:N} and {type:"disabled"}, and
// `adaptive` — the only form the 4.7+ generation accepts — has no
// representation in it at all. That turns out not to matter, because on those
// models thinking is ON by default and omitting the parameter runs it
// adaptively. So the translation is: pass a budget to the models that take
// one, and send nothing to the models that do not. Carrying the caller's
// intent in our own type is what lets the provider make that decision per
// model instead of at the boundary.
type ThinkingConfig struct {
	// Type is "enabled", "disabled" or "adaptive" (what a modern client sends).
	Type string `json:"type"`
	// BudgetTokens is meaningful only for "enabled", and only on models that
	// still accept it. Must be below max_tokens and at least 1024.
	BudgetTokens int `json:"budget_tokens,omitempty"`
	// Display is the client's visibility preference ("omitted" | "summarized" |
	// "updates"). Carried so it is not silently lost; the pinned SDK cannot
	// send it, which is recorded as a drop rather than ignored.
	Display string `json:"display,omitempty"`
}

// CacheControl is a vendor prompt-cache breakpoint: "cache everything up to and
// including this block".
//
// The type is deliberately Anthropic-shaped because Anthropic is the only
// provider that takes an explicit breakpoint. OpenAI caches automatically with
// no control surface, so for OpenAI this field is simply inert — it is not an
// error to send it, and silently ignoring it there is correct rather than
// lossy.
type CacheControl struct {
	// Type is "ephemeral" — the only value the API accepts today.
	Type string `json:"type"`
	// TTL is "5m" or "1h". Empty means the vendor default (5m). The 1h option
	// requires an SDK upgrade (see docs/AIQG-PROMPT-CACHE-CONTROL.md §7), so it
	// is carried but may not yet be honoured.
	TTL string `json:"ttl,omitempty"`
}

type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto", "low", "high"
}

type Function struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Parameters  interface{} `json:"parameters,omitempty"`
	Arguments   string      `json:"arguments,omitempty"` // Used in tool_call responses (JSON string of args)
}

type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function,omitempty"`
	// CacheControl on the LAST tool caches the whole tool block. Tools render
	// first, so a breakpoint here is the cheapest large win for agent traffic,
	// where tool definitions are big and identical on every turn.
	CacheControl *CacheControl `json:"cache_control,omitempty"`
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
	InputFileID      string                 `json:"input_file_id"`
	Endpoint         string                 `json:"endpoint"`
	CompletionWindow string                 `json:"completion_window"`
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
	Enabled             bool     `json:"enabled"`                     // Enable fallback to healthy providers
	PreferredChain      []string `json:"preferred_chain,omitempty"`   // Custom fallback order
	MaxCostIncrease     *float64 `json:"max_cost_increase,omitempty"` // Max % cost increase allowed (e.g., 0.5 = 50%)
	RequireSameFeatures bool     `json:"require_same_features"`       // Must support same capabilities
}
