# LLM Router WAF

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Build Status](https://img.shields.io/badge/Build-Passing-brightgreen.svg)](.)
[![Security](https://img.shields.io/badge/Security-Enterprise%20Grade-green.svg)](docs/security-guide.md)

> **Enterprise-grade LLM routing and security layer with zero feature loss**

The LLM Router WAF is a production-ready Web Application Firewall and intelligent router for Large Language Model APIs. It provides seamless access to multiple LLM providers (OpenAI, Anthropic, etc.) while maintaining enterprise security, cost optimization, and comprehensive monitoring.

## Features

### Core Routing
- **Multi-Provider Support**: OpenAI, Anthropic (Claude), with extensible architecture
- **Intelligent Routing**: Cost-optimized, performance-based, round-robin, and specific provider routing
- **Zero Feature Loss**: Full native API compatibility with provider-specific features
- **Health Monitoring**: Automatic provider health checks with failover
- **Cost Estimation**: Real-time cost calculation and optimization

### Advanced Features
- **Function Calling**: Full support for OpenAI function calling and Anthropic tool use
- **Vision Support**: Image analysis capabilities where supported
- **Streaming**: Real-time response streaming
- **Structured Output**: JSON schema validation (OpenAI)
- **Batch Processing**: Bulk request handling (OpenAI)
- **Assistants API**: OpenAI Assistants integration

### Production Ready
- **Configuration Management**: YAML config with environment variable overrides
- **Comprehensive Logging**: Structured JSON logging with configurable levels
- **HTTP Server**: Production-grade server with middleware and CORS
- **Graceful Shutdown**: Clean shutdown handling
- **Health Checks**: Built-in health monitoring endpoints

## Quick Start

### 1. Set Environment Variables

```bash
export OPENAI_API_KEY="your-openai-api-key"
export ANTHROPIC_API_KEY="your-anthropic-api-key"
```

### 2. Run with Default Configuration

```bash
go run cmd/llm-router/main.go
```

### 3. Run with Custom Configuration

```bash
go run cmd/llm-router/main.go --config configs/config.yaml
```

### 4. Test the Router

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ],
    "optimize_for": "cost"
  }'
```

## API Endpoints

### Chat Completions
- `POST /v1/chat/completions` - OpenAI compatible chat completions
- `POST /v1/messages` - Anthropic compatible messages

### Management
- `GET /v1/providers` - List registered providers
- `GET /v1/providers/{name}` - Get provider details
- `GET /v1/health` - Overall system health
- `GET /v1/health/{name}` - Provider-specific health
- `GET /v1/capabilities` - Provider capabilities
- `POST /v1/routing/decision` - Get routing decision without execution

### Health Check
- `GET /health` - Simple health check

## Configuration

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `OPENAI_API_KEY` | OpenAI API key | Required for OpenAI |
| `ANTHROPIC_API_KEY` | Anthropic API key | Required for Anthropic |
| `LLM_ROUTER_PORT` | Server port | 8080 |
| `LLM_ROUTER_LOG_LEVEL` | Log level | info |
| `LLM_ROUTER_LOG_FORMAT` | Log format (json/text) | json |
| `LLM_ROUTER_DEFAULT_STRATEGY` | Default routing strategy | cost_optimized |

### Configuration File

Create a `configs/config.yaml` file (see `config.example.yaml` for full example):

```yaml
server:
  port: "8080"
  read_timeout: 30s
  write_timeout: 30s

router:
  default_strategy: "cost_optimized"
  health_check_interval: 30s
  max_cost_threshold: 1.0

providers:
  openai:
    api_key: "${OPENAI_API_KEY}"
    models:
      - name: "gpt-4o"
        provider_model_id: "gpt-4o"
        input_cost_per_1k: 0.005
        output_cost_per_1k: 0.015

logging:
  level: "info"
  format: "json"
  output: "stdout"
```

## Routing Strategies

### Cost Optimized (default)
Routes to the provider with the lowest estimated cost for the request.

```json
{
  "optimize_for": "cost",
  "model": "gpt-3.5-turbo"
}
```

### Performance Optimized
Routes to the provider with the best performance characteristics.

```json
{
  "optimize_for": "performance",
  "model": "gpt-4o"
}
```

### Round Robin
Distributes requests evenly across healthy providers.

```json
{
  "optimize_for": "round_robin"
}
```

### Specific Provider
Routes to a specific provider based on model prefix.

```json
{
  "model": "gpt-4o"  // Routes to OpenAI
}
```

```json
{
  "model": "claude-3-5-sonnet-20241022"  // Routes to Anthropic
}
```

## Advanced Usage

### Function Calling

```json
{
  "model": "gpt-4o",
  "messages": [
    {"role": "user", "content": "What's the weather like?"}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Get current weather",
        "parameters": {
          "type": "object",
          "properties": {
            "location": {"type": "string"}
          }
        }
      }
    }
  ]
}
```

### Vision Support

```json
{
  "model": "gpt-4o",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "What's in this image?"},
        {"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}}
      ]
    }
  ]
}
```

### Streaming

```json
{
  "model": "gpt-4o",
  "messages": [
    {"role": "user", "content": "Tell me a story"}
  ],
  "stream": true
}
```

## Building and Deployment

### Build Binary

```bash
go build -o llm-router cmd/llm-router/main.go
```

### Docker Development

#### Standalone Mode
To run the full development stack with observability:

```bash
cd docker
docker-compose -f docker-compose.dev.yml up -d
```

This starts:
- Redis for caching and rate limiting
- Prometheus for metrics collection  
- Grafana for dashboards (admin/admin)
- Jaeger for distributed tracing
- OpenTelemetry Collector
- PostgreSQL for analytics
- Vault for secrets management
- LLM Router application (optional, with `--profile full-stack`)

Access the services:
- LLM Router: http://localhost:8085
- Grafana: http://localhost:3002
- Prometheus: http://localhost:9091
- Jaeger: http://localhost:16686

#### Aether Shared Infrastructure Integration
To run LLM Router integrated with shared TAS infrastructure:

```bash
cd docker
./start-aether-shared.sh start
```

This mode uses shared infrastructure services from aether-shared:
- Shared Redis, PostgreSQL, Prometheus, Grafana
- Shared Keycloak for authentication
- Shared Kafka for messaging  
- Shared MinIO for object storage

Access the services:
- LLM Router: http://localhost:8086
- Shared Grafana: http://localhost:3000
- Shared Prometheus: http://localhost:9090

**Prerequisites:** Ensure aether-shared infrastructure is running:
```bash
cd ../aether-shared
docker-compose -f docker-compose.shared-infrastructure.yml up -d
```

### Docker Deployment

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -o llm-router cmd/llm-router/main.go

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/llm-router .
COPY --from=builder /app/configs/config.yaml ./configs/config.yaml
CMD ["./llm-router", "--config", "configs/config.yaml"]
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm-router
spec:
  replicas: 3
  selector:
    matchLabels:
      app: llm-router
  template:
    metadata:
      labels:
        app: llm-router
    spec:
      containers:
      - name: llm-router
        image: llm-router:latest
        ports:
        - containerPort: 8080
        env:
        - name: OPENAI_API_KEY
          valueFrom:
            secretKeyRef:
              name: llm-router-secrets
              key: openai-api-key
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: llm-router-secrets
              key: anthropic-api-key
```

## Testing

### Run Tests

```bash
go test ./...
```

### Run Integration Tests

```bash
go test ./internal/integration
```

### Run Benchmarks

```bash
go test -bench=. ./internal/integration
```

## Monitoring

### Health Checks

The router provides comprehensive health monitoring:

```bash
# Overall health
curl http://localhost:8080/health

# Provider-specific health
curl http://localhost:8080/v1/health/openai
```

### Metrics

All requests are logged with structured data including:
- Provider selection reasoning
- Cost estimates
- Response times
- Routing metadata

### Debugging

Enable debug logging for detailed routing decisions:

```bash
LLM_ROUTER_LOG_LEVEL=debug go run cmd/llm-router/main.go
```

## Architecture

### Project Structure

```
├── cmd/llm-router/          # Main application entry point
├── internal/
│   ├── config/              # Configuration management
│   ├── providers/           # Provider implementations
│   │   ├── interfaces.go    # Provider interfaces
│   │   ├── openai/         # OpenAI provider
│   │   └── anthropic/      # Anthropic provider
│   ├── routing/            # Routing engine
│   ├── server/             # HTTP server
│   ├── types/              # Shared types
│   └── integration/        # Integration tests
├── config.example.yaml     # Example configuration
└── README.md              # This file
```

### Key Components

1. **Providers**: Implement the `LLMProvider` interface with full API compatibility
2. **Router**: Intelligent request routing with multiple strategies
3. **Server**: HTTP server with OpenAI/Anthropic compatible endpoints
4. **Config**: Flexible configuration with defaults and validation

## Contributing

1. Fork the repository
2. Create a feature branch
3. Add tests for new functionality
4. Ensure all tests pass
5. Submit a pull request

## License

This project is licensed under the MIT License.

## 📚 Documentation

| Document | Description |
|----------|-------------|
| **[📖 Complete Documentation](docs/)** | Full documentation hub |
| **[👤 User Guide](docs/user-guide.md)** | API usage, examples, SDK integration |
| **[🔧 Admin Guide](docs/admin-guide.md)** | Installation, configuration, deployment |
| **[👩‍💻 Developer Guide](docs/developer-guide.md)** | Development setup, contributing |
| **[🔐 Security Guide](docs/security-guide.md)** | Security features and best practices |
| **[📋 API Reference](docs/api-reference.md)** | Complete API documentation |

## 📊 Current Status

### ✅ **Implemented - Day 1 (Core Infrastructure)**
- [x] HTTP server with middleware architecture
- [x] Provider abstraction layer  
- [x] OpenAI provider (complete API support)
- [x] Anthropic provider (Claude support)
- [x] Intelligent routing engine with multiple strategies
- [x] Configuration management with environment overrides
- [x] Health checks and provider monitoring

### ✅ **Implemented - Day 2 Task 1 (Security & Authentication)**
- [x] JWT and API key authentication with permissions
- [x] Advanced rate limiting with token bucket algorithm
- [x] Comprehensive request validation and sanitization
- [x] Security audit logging with structured events
- [x] Security middleware stack with CORS support
- [x] Server integration with graceful shutdown

### 🚧 **In Progress - Day 2 (Advanced Features)**
- [ ] Advanced routing & load balancing algorithms
- [ ] Observability & monitoring (Prometheus, OpenTelemetry)
- [ ] Caching & performance optimization  
- [ ] Data pipeline & analytics
- [ ] Advanced provider features
- [ ] Configuration management & deployment automation
- [ ] Comprehensive testing & quality assurance

## 🆘 Support & Community

- **📖 Documentation**: [docs/](docs/)
- **🐛 Bug Reports**: [GitHub Issues](https://github.com/tributary-ai/llm-router-waf/issues)
- **💬 Discussions**: [GitHub Discussions](https://github.com/tributary-ai/llm-router-waf/discussions)
- **📧 Security**: security@tributary.ai
- **💼 Enterprise**: enterprise@tributary.ai