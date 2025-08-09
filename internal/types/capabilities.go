package types

// Provider capabilities and configuration
type ProviderCapabilities struct {
	ProviderName              string                     `json:"provider_name"`
	SupportedModels           []ModelInfo                `json:"supported_models"`
	SupportsFunctions         bool                       `json:"supports_functions"`
	SupportsParallelFunctions bool                       `json:"supports_parallel_functions"`
	SupportsVision            bool                       `json:"supports_vision"`
	SupportsStructuredOutput  bool                       `json:"supports_structured_output"`
	SupportsStreaming         bool                       `json:"supports_streaming"`
	SupportsAssistants        bool                       `json:"supports_assistants"`
	SupportsBatch             bool                       `json:"supports_batch"`
	MaxContextWindow          int                        `json:"max_context_window"`
	SupportedImageFormats     []string                   `json:"supported_image_formats"`
	CostPer1KTokens           CostStructure              `json:"cost_per_1k_tokens"`
	
	// Provider-specific capabilities
	OpenAISpecific            *OpenAICapabilities        `json:"openai_specific,omitempty"`
	AnthropicSpecific         *AnthropicCapabilities     `json:"anthropic_specific,omitempty"`
}

type ModelInfo struct {
	Name                 string   `json:"name"`
	DisplayName          string   `json:"display_name"`
	MaxContextWindow     int      `json:"max_context_window"`
	MaxOutputTokens      int      `json:"max_output_tokens"`
	SupportsFunctions    bool     `json:"supports_functions"`
	SupportsVision       bool     `json:"supports_vision"`
	SupportsStructured   bool     `json:"supports_structured_output"`
	InputCostPer1K       float64  `json:"input_cost_per_1k"`
	OutputCostPer1K      float64  `json:"output_cost_per_1k"`
	
	// Provider-specific model info
	ProviderModelID      string   `json:"provider_model_id,omitempty"`
	Tags                 []string `json:"tags,omitempty"`
}

type CostStructure struct {
	InputCostPer1K  float64 `json:"input_cost_per_1k"`
	OutputCostPer1K float64 `json:"output_cost_per_1k"`
	Currency        string  `json:"currency"`
}

// Provider-specific capabilities
type OpenAICapabilities struct {
	SupportsJSONSchema        bool     `json:"supports_json_schema"`
	SupportsStrictMode        bool     `json:"supports_strict_mode"`
	SupportsLogProbs          bool     `json:"supports_log_probs"`
	SupportsSeed              bool     `json:"supports_seed"`
	SupportsSystemFingerprint bool     `json:"supports_system_fingerprint"`
	SupportsParallelFunctions bool     `json:"supports_parallel_functions"`
	MaxFunctionCalls          int      `json:"max_function_calls"`
	SupportedResponseFormats  []string `json:"supported_response_formats"`
}

type AnthropicCapabilities struct {
	SupportsSystemMessages    bool     `json:"supports_system_messages"`
	MaxSystemMessageLength    int      `json:"max_system_message_length"`
	SupportsStopSequences     bool     `json:"supports_stop_sequences"`
	SupportsToolUse           bool     `json:"supports_tool_use"`
	MaxToolCalls              int      `json:"max_tool_calls"`
	SupportedStopSequences    []string `json:"supported_stop_sequences"`
}

// Health check types
type HealthStatus struct {
	Status        string `json:"status"` // "healthy", "degraded", "unhealthy"
	ResponseTime  int64  `json:"response_time_ms"`
	LastChecked   int64  `json:"last_checked"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

// Routing configuration
type RoutingStrategy struct {
	Type               string             `json:"type"` // "cost_optimized", "performance", "round_robin", "weighted"
	Weights            map[string]float64 `json:"weights,omitempty"`
	CostThreshold      float64            `json:"cost_threshold,omitempty"`
	LatencyThreshold   int64              `json:"latency_threshold_ms,omitempty"`
	FailoverEnabled    bool               `json:"failover_enabled"`
	HealthCheckEnabled bool               `json:"health_check_enabled"`
}