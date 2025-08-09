# Developer Guide

This guide covers development setup, architecture, and contribution guidelines for the LLM Router WAF project.

## Table of Contents

- [Development Setup](#development-setup)
- [Architecture Overview](#architecture-overview)
- [Code Organization](#code-organization)
- [Adding New Providers](#adding-new-providers)
- [Testing](#testing)
- [Contributing](#contributing)
- [API Design Guidelines](#api-design-guidelines)

## Development Setup

### Prerequisites

- **Go 1.21+**
- **Git**
- **Make** (optional, for build scripts)
- **Docker** (for testing)

### Local Development

```bash
# Clone repository
git clone https://github.com/tributary-ai/llm-router-waf.git
cd llm-router-waf

# Install dependencies
go mod download

# Run tests
go test ./...

# Build application
go build -o llm-router cmd/llm-router/main.go

# Run with development config
./llm-router --config configs/config.yaml --dev-mode
```

### Development Environment

Create `.env.dev`:
```bash
# Development environment
LLM_ROUTER_PORT=8080
LLM_ROUTER_LOG_LEVEL=debug
LLM_ROUTER_LOG_FORMAT=text

# Provider API keys (use test keys)
OPENAI_API_KEY=sk-test-...
ANTHROPIC_API_KEY=sk-ant-test-...

# Development security
API_KEYS=dev-key-123,test-key-456
RATE_LIMIT_ENABLED=false
```

### IDE Setup

#### VS Code Configuration

`.vscode/settings.json`:
```json
{
  "go.useLanguageServer": true,
  "go.formatTool": "goimports",
  "go.lintTool": "golangci-lint",
  "go.testFlags": ["-v"],
  "go.buildFlags": ["-tags=dev"],
  "editor.formatOnSave": true
}
```

`.vscode/launch.json`:
```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Launch LLM Router",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/llm-router/main.go",
      "args": [
        "--config",
        "${workspaceFolder}/configs/config.yaml",
        "--dev-mode"
      ],
      "env": {
        "LLM_ROUTER_LOG_LEVEL": "debug"
      }
    }
  ]
}
```

## Architecture Overview

### High-Level Architecture

```
┌─────────────────┐
│   HTTP Server   │ ← Gorilla Mux, Middleware
└─────────────────┘
         │
┌─────────────────┐
│ Security Layer  │ ← Auth, Rate Limiting, Validation
└─────────────────┘
         │
┌─────────────────┐
│ Routing Engine  │ ← Strategy Selection, Load Balancing
└─────────────────┘
         │
┌─────────────────┐
│   Providers     │ ← OpenAI, Anthropic, etc.
└─────────────────┘
```

### Core Components

#### 1. HTTP Server (`internal/server/`)
- Request/response handling
- Middleware chain management
- Route registration
- Graceful shutdown

#### 2. Security Layer (`internal/security/`, `internal/middleware/`)
- Authentication (JWT, API keys)
- Rate limiting (token bucket)
- Request validation
- Audit logging
- CORS handling

#### 3. Routing Engine (`internal/routing/`)
- Provider selection strategies
- Health checking
- Failover logic
- Cost estimation

#### 4. Provider Abstraction (`internal/providers/`)
- Common interface for all providers
- Provider-specific implementations
- Request/response translation
- Error handling

#### 5. Configuration (`internal/config/`)
- YAML configuration loading
- Environment variable overrides
- Validation
- Type conversion

### Design Principles

#### 1. Zero Feature Loss
Every feature of upstream providers must be accessible through the router without modification.

#### 2. Provider Agnostic
New providers should integrate seamlessly using the common interface.

#### 3. Security First
All requests are authenticated, validated, and audited by default.

#### 4. Observable
Comprehensive logging, metrics, and health checks for operational visibility.

#### 5. Scalable
Stateless design with horizontal scaling capability.

## Code Organization

### Directory Structure

```
internal/
├── config/              # Configuration management
│   ├── config.go       # Main config struct and loading
│   └── validation.go   # Config validation
├── middleware/          # HTTP middleware
│   └── security.go     # Security middleware stack
├── providers/           # Provider implementations
│   ├── interfaces.go   # Common provider interfaces
│   ├── openai/         # OpenAI provider
│   │   ├── provider.go # Main provider implementation
│   │   ├── types.go    # OpenAI-specific types
│   │   └── client.go   # HTTP client wrapper
│   └── anthropic/      # Anthropic provider
│       ├── provider.go
│       ├── types.go
│       └── client.go
├── routing/             # Routing logic
│   ├── router.go       # Main router implementation
│   ├── strategies.go   # Routing strategies
│   └── decision.go     # Decision tracking
├── security/            # Security components
│   ├── auth.go         # Authentication
│   ├── ratelimit.go    # Rate limiting
│   ├── validation.go   # Request validation
│   └── audit.go        # Audit logging
├── server/              # HTTP server
│   ├── server.go       # Server implementation
│   ├── handlers.go     # Request handlers
│   └── middleware.go   # Server middleware
└── types/               # Common types
    ├── requests.go     # Request types
    ├── responses.go    # Response types
    └── common.go       # Shared types
```

### Code Style

#### Go Conventions
- Follow standard Go formatting (`gofmt`, `goimports`)
- Use meaningful variable and function names
- Write self-documenting code
- Include package and public function documentation

#### Error Handling
```go
// Always wrap errors with context
if err != nil {
    return nil, fmt.Errorf("failed to create client: %w", err)
}

// Use custom error types for specific cases
type ProviderError struct {
    Provider string
    Code     int
    Message  string
    Err      error
}

func (e *ProviderError) Error() string {
    return fmt.Sprintf("provider %s error (%d): %s", e.Provider, e.Code, e.Message)
}
```

#### Logging
```go
// Use structured logging with logrus
logger.WithFields(logrus.Fields{
    "provider": "openai",
    "model":    "gpt-3.5-turbo",
    "duration": duration,
}).Info("Request completed")

// Log errors with full context
logger.WithError(err).WithFields(logrus.Fields{
    "provider": providerName,
    "request_id": requestID,
}).Error("Provider request failed")
```

## Adding New Providers

### Step 1: Define Provider Interface

All providers must implement the core interfaces in `internal/providers/interfaces.go`:

```go
type LLMProvider interface {
    GetCapabilities() types.ProviderCapabilities
    GetProviderName() string
    ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error)
    StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error)
    EstimateCost(req *types.ChatRequest) (*types.CostEstimate, error)
    HealthCheck(ctx context.Context) error
}
```

### Step 2: Create Provider Package

Create `internal/providers/newprovider/`:

```go
// provider.go
package newprovider

import (
    "context"
    "github.com/tributary-ai/llm-router-waf/internal/providers"
    "github.com/tributary-ai/llm-router-waf/internal/types"
)

type Provider struct {
    config *Config
    client *Client
    logger *logrus.Logger
}

func NewProvider(config *Config, logger *logrus.Logger) (*Provider, error) {
    client, err := NewClient(config)
    if err != nil {
        return nil, fmt.Errorf("failed to create client: %w", err)
    }

    return &Provider{
        config: config,
        client: client,
        logger: logger,
    }, nil
}

func (p *Provider) GetProviderName() string {
    return "newprovider"
}

func (p *Provider) GetCapabilities() types.ProviderCapabilities {
    return types.ProviderCapabilities{
        ChatCompletion:   true,
        StreamCompletion: true,
        FunctionCalling:  false,
        Vision:          false,
    }
}

func (p *Provider) ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error) {
    // Convert common request to provider-specific format
    providerReq := p.convertRequest(req)
    
    // Make API call
    providerResp, err := p.client.ChatCompletion(ctx, providerReq)
    if err != nil {
        return nil, fmt.Errorf("provider API call failed: %w", err)
    }
    
    // Convert provider response to common format
    return p.convertResponse(providerResp), nil
}

// Implement other required methods...
```

### Step 3: Add Configuration

Update `internal/config/config.go`:

```go
type ProvidersConfig struct {
    OpenAI      openai.Config      `yaml:"openai"`
    Anthropic   anthropic.Config   `yaml:"anthropic"`
    NewProvider newprovider.Config `yaml:"newprovider"`
}
```

### Step 4: Register Provider

Update the provider registration in your main application:

```go
// Register new provider
if cfg.Providers.NewProvider.Enabled {
    provider, err := newprovider.NewProvider(&cfg.Providers.NewProvider, logger)
    if err != nil {
        return fmt.Errorf("failed to create newprovider: %w", err)
    }
    router.RegisterProvider(provider)
}
```

### Step 5: Add Tests

Create comprehensive tests:

```go
// provider_test.go
package newprovider

func TestProvider_ChatCompletion(t *testing.T) {
    provider := setupTestProvider(t)
    
    req := &types.ChatRequest{
        Model: "test-model",
        Messages: []types.Message{
            {Role: "user", Content: "Hello"},
        },
    }
    
    resp, err := provider.ChatCompletion(context.Background(), req)
    assert.NoError(t, err)
    assert.NotNil(t, resp)
    assert.NotEmpty(t, resp.Choices)
}
```

## Testing

### Unit Tests

```bash
# Run all tests
go test ./...

# Run with coverage
go test -cover ./...

# Run specific package tests
go test ./internal/providers/openai

# Run with race detection
go test -race ./...
```

### Integration Tests

```bash
# Run integration tests (requires API keys)
go test -tags=integration ./tests/integration/

# Run with test providers only
go test -tags=integration,testonly ./tests/integration/
```

### Test Structure

```go
func TestProviderIntegration(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test in short mode")
    }
    
    // Setup
    provider := setupProvider(t)
    
    // Test cases
    tests := []struct {
        name     string
        request  *types.ChatRequest
        wantErr  bool
    }{
        {
            name: "basic chat completion",
            request: &types.ChatRequest{
                Model: "gpt-3.5-turbo",
                Messages: []types.Message{
                    {Role: "user", Content: "Hello"},
                },
            },
            wantErr: false,
        },
    }
    
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            resp, err := provider.ChatCompletion(context.Background(), tt.request)
            if tt.wantErr {
                assert.Error(t, err)
                return
            }
            assert.NoError(t, err)
            assert.NotNil(t, resp)
        })
    }
}
```

### Benchmarks

```go
func BenchmarkProvider_ChatCompletion(b *testing.B) {
    provider := setupProvider(b)
    req := &types.ChatRequest{
        Model: "gpt-3.5-turbo",
        Messages: []types.Message{
            {Role: "user", Content: "Hello"},
        },
    }
    
    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        _, err := provider.ChatCompletion(context.Background(), req)
        if err != nil {
            b.Fatal(err)
        }
    }
}
```

## Contributing

### Contribution Workflow

1. **Fork** the repository
2. **Create** a feature branch: `git checkout -b feature/awesome-feature`
3. **Make** your changes
4. **Add** tests for new functionality
5. **Run** tests: `go test ./...`
6. **Run** linting: `golangci-lint run`
7. **Commit** your changes: `git commit -m 'Add awesome feature'`
8. **Push** to the branch: `git push origin feature/awesome-feature`
9. **Open** a Pull Request

### Code Review Guidelines

#### For Contributors
- Write clear commit messages
- Include tests for new features
- Update documentation as needed
- Keep PRs focused and small
- Respond to feedback promptly

#### For Reviewers
- Be constructive and specific
- Check for security implications
- Verify test coverage
- Ensure documentation updates
- Test functionality when possible

### Commit Message Format

```
type(scope): short description

Longer description if needed

Fixes #123
```

Types:
- `feat`: New feature
- `fix`: Bug fix
- `docs`: Documentation changes
- `style`: Code style changes
- `refactor`: Code refactoring
- `test`: Test additions/changes
- `chore`: Build/tool changes

### Pull Request Template

```markdown
## Description
Brief description of changes

## Type of Change
- [ ] Bug fix
- [ ] New feature
- [ ] Breaking change
- [ ] Documentation update

## Testing
- [ ] Unit tests pass
- [ ] Integration tests pass
- [ ] Manual testing completed

## Checklist
- [ ] Code follows style guidelines
- [ ] Self-review completed
- [ ] Documentation updated
- [ ] Tests added/updated
```

## API Design Guidelines

### Request/Response Design

#### Consistency
- Use consistent naming conventions
- Follow HTTP status code standards
- Provide meaningful error messages

#### Backwards Compatibility
- Don't remove fields in responses
- Use optional fields for new features
- Version APIs when breaking changes needed

#### Error Responses

```go
type ErrorResponse struct {
    Error struct {
        Message string `json:"message"`
        Type    string `json:"type"`
        Code    int    `json:"code"`
        Details map[string]interface{} `json:"details,omitempty"`
    } `json:"error"`
    Timestamp int64 `json:"timestamp"`
}
```

### Performance Considerations

#### Connection Pooling
```go
// Use proper HTTP client configuration
client := &http.Client{
    Transport: &http.Transport{
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
        IdleConnTimeout:     90 * time.Second,
    },
    Timeout: 60 * time.Second,
}
```

#### Context Usage
```go
// Always accept and pass context
func (p *Provider) ChatCompletion(ctx context.Context, req *types.ChatRequest) (*types.ChatResponse, error) {
    // Use context for timeouts and cancellation
    ctx, cancel := context.WithTimeout(ctx, p.config.Timeout)
    defer cancel()
    
    // Pass context to HTTP requests
    httpReq = httpReq.WithContext(ctx)
    
    return p.client.Do(httpReq)
}
```

#### Memory Management
```go
// Use streaming for large responses
func (p *Provider) StreamCompletion(ctx context.Context, req *types.ChatRequest) (<-chan *types.ChatChunk, error) {
    ch := make(chan *types.ChatChunk, 100)
    
    go func() {
        defer close(ch)
        // Stream processing logic
    }()
    
    return ch, nil
}
```

### Security Guidelines

#### Input Validation
```go
func validateRequest(req *types.ChatRequest) error {
    if req.Model == "" {
        return errors.New("model is required")
    }
    
    if len(req.Messages) == 0 {
        return errors.New("messages are required")
    }
    
    // Validate each message
    for i, msg := range req.Messages {
        if err := validateMessage(msg); err != nil {
            return fmt.Errorf("invalid message at index %d: %w", i, err)
        }
    }
    
    return nil
}
```

#### Sensitive Data
```go
// Never log sensitive data
logger.WithFields(logrus.Fields{
    "provider": "openai",
    "model":    req.Model,
    "api_key":  maskAPIKey(apiKey), // Always mask
}).Info("Making provider request")

func maskAPIKey(key string) string {
    if len(key) <= 8 {
        return "****"
    }
    return key[:4] + "****"
}
```