# LLM Invocation Package Design

## Overview

This document outlines the design for a Go package that enables other services to submit and execute LLM invocations. The package will provide a unified interface for interacting with multiple LLM providers, supporting streaming responses, tools/functions, and MCP (Model Context Protocol) capabilities.

## Goals

1. **Provider Abstraction**: Support multiple LLM providers (OpenAI, Anthropic, Google, etc.) through a common interface
2. **Streaming by Default**: All LLM invocations should support streaming responses
3. **Tools & Functions**: Support for function calling, tools, and MCP protocols
4. **Per-Request Configuration**: Allow model-specific options to be set on each request
5. **Type Safety**: Provide strongly-typed interfaces for requests and responses
6. **Easy Integration**: Simple API that can be imported and used by other services

## Package Structure

```
github.com/tributary-ai/llm-invocation/
├── client.go           # Main client interface
├── providers/          # Provider implementations
│   ├── provider.go     # Provider interface definition
│   ├── openai/
│   ├── anthropic/
│   ├── google/
│   └── registry.go     # Provider registry
├── streaming/          # Streaming utilities
│   ├── stream.go       # Stream interface
│   └── aggregator.go   # Stream aggregation utilities
├── tools/              # Tools and function calling
│   ├── executor.go     # Tool execution framework
│   ├── registry.go     # Tool registry
│   └── mcp/           # MCP protocol support
├── types/              # Common types
│   ├── request.go      # Request types
│   ├── response.go     # Response types
│   └── errors.go       # Error types
└── examples/           # Usage examples
```

## Core Interfaces

### Client Interface

```go
package llminvocation

import (
    "context"
    "io"
)

// Client is the main interface for LLM invocations
type Client interface {
    // Invoke sends a request to the LLM and returns a streaming response
    Invoke(ctx context.Context, req *InvocationRequest) (ResponseStream, error)
    
    // InvokeSync sends a request and waits for the complete response
    InvokeSync(ctx context.Context, req *InvocationRequest) (*InvocationResponse, error)
    
    // ListModels returns available models for the configured providers
    ListModels(ctx context.Context) ([]ModelInfo, error)
    
    // RegisterTool registers a tool that can be called by the LLM
    RegisterTool(tool Tool) error
    
    // RegisterMCPServer registers an MCP server
    RegisterMCPServer(server MCPServer) error
}

// ResponseStream represents a streaming response from an LLM
type ResponseStream interface {
    // Next returns the next chunk in the stream
    Next() (*StreamChunk, error)
    
    // Close closes the stream
    Close() error
    
    // Aggregate collects all chunks into a complete response
    Aggregate() (*InvocationResponse, error)
}
```

### Provider Interface

```go
package providers

import (
    "context"
)

// Provider defines the interface that all LLM providers must implement
type Provider interface {
    // Name returns the provider name (e.g., "openai", "anthropic")
    Name() string
    
    // Models returns the list of available models
    Models() []ModelInfo
    
    // Invoke handles a streaming invocation
    Invoke(ctx context.Context, req *ProviderRequest) (<-chan *StreamChunk, error)
    
    // SupportsFeature checks if a feature is supported
    SupportsFeature(feature Feature) bool
    
    // ValidateRequest validates a request for this provider
    ValidateRequest(req *ProviderRequest) error
}

// Feature represents a provider capability
type Feature string

const (
    FeatureFunctionCalling  Feature = "function_calling"
    FeatureParallelTools    Feature = "parallel_tools"
    FeatureVision          Feature = "vision"
    FeatureJSONMode        Feature = "json_mode"
    FeatureStructuredOutput Feature = "structured_output"
    FeatureMCP             Feature = "mcp"
)
```

## Request/Response Types

### Invocation Request

```go
type InvocationRequest struct {
    // Provider selection
    Provider string `json:"provider,omitempty"`  // Optional: auto-select if not specified
    Model    string `json:"model"`              // Required: model identifier
    
    // Messages
    Messages []Message `json:"messages"`
    
    // Model parameters
    Options ModelOptions `json:"options,omitempty"`
    
    // Tools and functions
    Tools      []Tool      `json:"tools,omitempty"`
    ToolChoice interface{} `json:"tool_choice,omitempty"`
    
    // Response format
    ResponseFormat *ResponseFormat `json:"response_format,omitempty"`
    
    // Streaming preference (defaults to true)
    Stream *bool `json:"stream,omitempty"`
    
    // Request metadata
    Metadata map[string]interface{} `json:"metadata,omitempty"`
}

type ModelOptions struct {
    Temperature      *float32 `json:"temperature,omitempty"`
    MaxTokens        *int     `json:"max_tokens,omitempty"`
    TopP             *float32 `json:"top_p,omitempty"`
    FrequencyPenalty *float32 `json:"frequency_penalty,omitempty"`
    PresencePenalty  *float32 `json:"presence_penalty,omitempty"`
    Stop             []string `json:"stop,omitempty"`
    Seed             *int     `json:"seed,omitempty"`
    
    // Provider-specific options
    ProviderOptions map[string]interface{} `json:"provider_options,omitempty"`
}

type Message struct {
    Role       string       `json:"role"`
    Content    MessageContent `json:"content"`
    Name       string       `json:"name,omitempty"`
    ToolCalls  []ToolCall   `json:"tool_calls,omitempty"`
    ToolCallID string       `json:"tool_call_id,omitempty"`
}

// MessageContent can be a string or multimodal content
type MessageContent interface{}
```

### Response Types

```go
type InvocationResponse struct {
    ID        string    `json:"id"`
    Provider  string    `json:"provider"`
    Model     string    `json:"model"`
    Created   time.Time `json:"created"`
    Choices   []Choice  `json:"choices"`
    Usage     *Usage    `json:"usage,omitempty"`
    Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

type StreamChunk struct {
    ID       string       `json:"id"`
    Provider string       `json:"provider"`
    Model    string       `json:"model"`
    Delta    *Delta       `json:"delta,omitempty"`
    Choices  []DeltaChoice `json:"choices,omitempty"`
    Usage    *Usage       `json:"usage,omitempty"`
    Error    *Error       `json:"error,omitempty"`
}

type Choice struct {
    Index        int         `json:"index"`
    Message      Message     `json:"message"`
    FinishReason string      `json:"finish_reason,omitempty"`
    ToolCalls    []ToolCall  `json:"tool_calls,omitempty"`
}
```

## Tools and MCP Support

### Tool Interface

```go
type Tool interface {
    // Name returns the tool name
    Name() string
    
    // Description returns the tool description
    Description() string
    
    // Parameters returns the JSON schema for parameters
    Parameters() map[string]interface{}
    
    // Execute runs the tool with given arguments
    Execute(ctx context.Context, args map[string]interface{}) (interface{}, error)
}

// ToolRegistry manages available tools
type ToolRegistry interface {
    Register(tool Tool) error
    Get(name string) (Tool, error)
    List() []Tool
    Execute(ctx context.Context, name string, args map[string]interface{}) (interface{}, error)
}
```

### MCP Protocol Support

```go
type MCPServer interface {
    // Connect establishes connection to MCP server
    Connect(ctx context.Context) error
    
    // ListTools returns available tools from the server
    ListTools(ctx context.Context) ([]Tool, error)
    
    // ExecuteTool executes a tool on the MCP server
    ExecuteTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error)
    
    // Disconnect closes the connection
    Disconnect() error
}

type MCPClient struct {
    servers []MCPServer
    // ... implementation details
}
```

## Implementation Details

### Provider Registry

```go
// ProviderRegistry manages available providers
type ProviderRegistry struct {
    providers map[string]Provider
    mu        sync.RWMutex
}

func (r *ProviderRegistry) Register(provider Provider) error {
    r.mu.Lock()
    defer r.mu.Unlock()
    
    name := provider.Name()
    if _, exists := r.providers[name]; exists {
        return fmt.Errorf("provider %s already registered", name)
    }
    
    r.providers[name] = provider
    return nil
}

func (r *ProviderRegistry) Get(name string) (Provider, error) {
    r.mu.RLock()
    defer r.mu.RUnlock()
    
    provider, exists := r.providers[name]
    if !exists {
        return nil, fmt.Errorf("provider %s not found", name)
    }
    
    return provider, nil
}
```

### Streaming Implementation

```go
// StreamAggregator collects stream chunks into a complete response
type StreamAggregator struct {
    chunks []StreamChunk
    mu     sync.Mutex
}

func (a *StreamAggregator) Add(chunk StreamChunk) {
    a.mu.Lock()
    defer a.mu.Unlock()
    a.chunks = append(a.chunks, chunk)
}

func (a *StreamAggregator) Aggregate() (*InvocationResponse, error) {
    a.mu.Lock()
    defer a.mu.Unlock()
    
    // Combine chunks into complete response
    // Implementation details...
}
```

### Client Implementation

```go
type client struct {
    registry     *ProviderRegistry
    toolRegistry ToolRegistry
    mcpClient    *MCPClient
    config       *Config
}

func NewClient(config *Config) (Client, error) {
    c := &client{
        registry:     NewProviderRegistry(),
        toolRegistry: NewToolRegistry(),
        config:       config,
    }
    
    // Initialize providers based on config
    if err := c.initializeProviders(); err != nil {
        return nil, err
    }
    
    return c, nil
}

func (c *client) Invoke(ctx context.Context, req *InvocationRequest) (ResponseStream, error) {
    // Select provider
    provider, err := c.selectProvider(req)
    if err != nil {
        return nil, err
    }
    
    // Validate request
    if err := provider.ValidateRequest(req); err != nil {
        return nil, err
    }
    
    // Set default streaming to true
    if req.Stream == nil {
        stream := true
        req.Stream = &stream
    }
    
    // Convert to provider-specific request
    providerReq := c.convertRequest(req, provider)
    
    // Invoke provider
    streamChan, err := provider.Invoke(ctx, providerReq)
    if err != nil {
        return nil, err
    }
    
    // Wrap in ResponseStream
    return NewResponseStream(streamChan), nil
}
```

## Configuration

```go
type Config struct {
    Providers map[string]ProviderConfig `json:"providers"`
    Tools     ToolsConfig               `json:"tools"`
    MCP       MCPConfig                 `json:"mcp"`
    Defaults  DefaultsConfig            `json:"defaults"`
}

type ProviderConfig struct {
    Enabled bool                   `json:"enabled"`
    APIKey  string                 `json:"api_key"`
    BaseURL string                 `json:"base_url,omitempty"`
    Models  []string               `json:"models,omitempty"`
    Options map[string]interface{} `json:"options,omitempty"`
}

type DefaultsConfig struct {
    Provider string       `json:"provider,omitempty"`
    Model    string       `json:"model,omitempty"`
    Options  ModelOptions `json:"options,omitempty"`
}
```

## Usage Examples

### Basic Usage

```go
import (
    "context"
    "fmt"
    llm "github.com/tributary-ai/llm-invocation"
)

func main() {
    // Create client with configuration
    config := &llm.Config{
        Providers: map[string]llm.ProviderConfig{
            "openai": {
                Enabled: true,
                APIKey:  os.Getenv("OPENAI_API_KEY"),
            },
            "anthropic": {
                Enabled: true,
                APIKey:  os.Getenv("ANTHROPIC_API_KEY"),
            },
        },
    }
    
    client, err := llm.NewClient(config)
    if err != nil {
        log.Fatal(err)
    }
    
    // Create request
    req := &llm.InvocationRequest{
        Model: "gpt-4-turbo-preview",
        Messages: []llm.Message{
            {
                Role:    "user",
                Content: "Explain quantum computing in simple terms",
            },
        },
        Options: llm.ModelOptions{
            Temperature: ptr(0.7),
            MaxTokens:   ptr(500),
        },
    }
    
    // Stream response
    stream, err := client.Invoke(context.Background(), req)
    if err != nil {
        log.Fatal(err)
    }
    defer stream.Close()
    
    // Process stream
    for {
        chunk, err := stream.Next()
        if err != nil {
            break
        }
        fmt.Print(chunk.Delta.Content)
    }
}
```

### Tool Usage

```go
// Define a custom tool
type WeatherTool struct{}

func (w *WeatherTool) Name() string {
    return "get_weather"
}

func (w *WeatherTool) Description() string {
    return "Get current weather for a location"
}

func (w *WeatherTool) Parameters() map[string]interface{} {
    return map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "location": map[string]interface{}{
                "type":        "string",
                "description": "City and state",
            },
        },
        "required": []string{"location"},
    }
}

func (w *WeatherTool) Execute(ctx context.Context, args map[string]interface{}) (interface{}, error) {
    location := args["location"].(string)
    // Implementation...
    return map[string]interface{}{
        "temperature": 72,
        "condition":   "sunny",
    }, nil
}

// Register and use tool
client.RegisterTool(&WeatherTool{})

req := &llm.InvocationRequest{
    Model: "gpt-4-turbo-preview",
    Messages: []llm.Message{
        {
            Role:    "user",
            Content: "What's the weather in San Francisco?",
        },
    },
    Tools: []llm.Tool{
        {
            Type:     "function",
            Function: client.GetTool("get_weather"),
        },
    },
}
```

### MCP Integration

```go
// Connect to MCP server
mcpServer := llm.NewMCPServer("localhost:8080")
client.RegisterMCPServer(mcpServer)

// Tools from MCP server are automatically available
req := &llm.InvocationRequest{
    Model: "claude-3-opus",
    Messages: []llm.Message{
        {
            Role:    "user",
            Content: "Search for recent papers on quantum computing",
        },
    },
    // MCP tools are automatically included
}
```

## Error Handling

```go
type Error struct {
    Code     string                 `json:"code"`
    Message  string                 `json:"message"`
    Provider string                 `json:"provider,omitempty"`
    Details  map[string]interface{} `json:"details,omitempty"`
}

// Common error codes
const (
    ErrProviderNotFound     = "provider_not_found"
    ErrModelNotSupported    = "model_not_supported"
    ErrRateLimitExceeded    = "rate_limit_exceeded"
    ErrInvalidRequest       = "invalid_request"
    ErrToolExecutionFailed  = "tool_execution_failed"
    ErrStreamingFailed      = "streaming_failed"
)
```

## Testing Strategy

1. **Unit Tests**: Test individual components (providers, tools, streaming)
2. **Integration Tests**: Test provider integrations with mock servers
3. **Contract Tests**: Ensure provider implementations meet interface contracts
4. **Example Tests**: Validate usage examples work correctly

## Security Considerations

1. **API Key Management**: Secure storage and rotation of API keys
2. **Input Validation**: Validate all inputs before sending to providers
3. **Tool Execution**: Sandbox tool execution to prevent malicious code
4. **Rate Limiting**: Implement rate limiting to prevent abuse
5. **Audit Logging**: Log all invocations for security monitoring

## Performance Considerations

1. **Connection Pooling**: Reuse HTTP connections to providers
2. **Streaming Efficiency**: Minimize memory usage during streaming
3. **Concurrent Requests**: Support concurrent invocations
4. **Caching**: Cache model lists and provider capabilities
5. **Timeout Management**: Configurable timeouts for all operations

## Future Enhancements

1. **Provider Fallback**: Automatic fallback to alternative providers
2. **Response Caching**: Cache responses for identical requests
3. **Metrics Collection**: Built-in metrics for monitoring
4. **Middleware Support**: Plugin architecture for extending functionality
5. **WebSocket Support**: Real-time bidirectional communication