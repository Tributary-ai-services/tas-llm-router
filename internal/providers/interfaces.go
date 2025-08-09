package providers

import (
	"context"

	"github.com/tributary-ai/llm-router-waf/internal/types"
)

// Core provider interface - all providers must implement
type LLMProvider interface {
	GetCapabilities() types.ProviderCapabilities
	GetProviderName() string
	ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error)
	StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error)
	EstimateCost(req *types.ChatRequest) (*types.CostEstimate, error)
	HealthCheck(ctx context.Context) error
}

// Advanced feature interfaces
type FunctionCallingProvider interface {
	LLMProvider
	SupportsFunctionCalling() bool
	SupportsParallelFunctions() bool
}

type VisionProvider interface {
	LLMProvider
	SupportsVision() bool
	GetSupportedImageFormats() []string
}

type StructuredOutputProvider interface {
	LLMProvider
	SupportsStructuredOutput() bool
	SupportsStrictMode() bool // OpenAI's strict JSON schema mode
}

type BatchProvider interface {
	LLMProvider
	SupportsBatch() bool
	CreateBatch(ctx context.Context, req *types.BatchRequest) (*types.BatchResponse, error)
}

type AssistantProvider interface {
	LLMProvider
	SupportsAssistants() bool
	CreateAssistant(ctx context.Context, req *types.AssistantRequest) (*types.AssistantResponse, error)
}