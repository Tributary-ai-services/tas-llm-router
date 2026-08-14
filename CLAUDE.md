# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**TAS LLM Router** is an enterprise-grade Web Application Firewall and intelligent router for Large Language Model APIs. It provides seamless access to multiple LLM providers (OpenAI, Anthropic, etc.) while maintaining enterprise security, compliance scanning, cost optimization, and comprehensive monitoring. Built with Go, it serves as the centralized LLM gateway for all TAS services.

## Data Models & Schema Reference

### Service-Specific Data Models
This service's data models are comprehensively documented in the centralized data models repository:

**Location**: `../aether-shared/data-models/tas-llm-router/`

#### Key Request/Response Models:
- **Request Format** (`request-format.md`) - ChatRequest structure with messages, model selection, and advanced parameters
- **Response Format** (`response-format.md`) - ChatResponse structure for streaming and non-streaming responses
- **Model Configurations** (`model-configurations.md`) - Supported LLM providers (GPT-4, Claude 3), routing strategies, and cost optimization

#### Cross-Service Integration:
- **Document Upload Flow** (`../aether-shared/data-models/cross-service/flows/document-upload.md`) - AI-powered document classification and analysis
- **Platform ERD** (`../aether-shared/data-models/cross-service/diagrams/platform-erd.md`) - Complete entity relationship diagram
- **Architecture Overview** (`../aether-shared/data-models/cross-service/diagrams/architecture-overview.md`) - LLM routing in system architecture

#### When to Reference Data Models:
1. Before adding new LLM providers or modifying routing logic
2. When implementing new request/response formats or advanced features
3. When debugging LLM integration issues or response parsing errors
4. When onboarding new developers to understand the routing architecture
5. Before modifying cost optimization or model selection algorithms

**Main Documentation Hub**: `../aether-shared/data-models/README.md` - Complete navigation for all 38 data model files

## Technology Stack

- **Language**: Go 1.21+
- **Framework**: Gin HTTP framework
- **Authentication**: Keycloak integration with JWT validation
- **Monitoring**: Prometheus metrics + Grafana dashboards
- **Configuration**: YAML config with environment variable overrides
- **Logging**: Structured JSON logging with configurable levels

## Key Features

### Core Routing Capabilities
- **Multi-Provider Support**: OpenAI, Anthropic (Claude), with extensible architecture
- **Intelligent Routing**: Cost-optimized, performance-based, round-robin, and specific provider routing
- **Zero Feature Loss**: Full native API compatibility with provider-specific features
- **Health Monitoring**: Automatic provider health checks with failover
- **Cost Estimation**: Real-time cost calculation and optimization

### Advanced LLM Features
- **Function Calling**: Full support for OpenAI function calling and Anthropic tool use
- **Vision Support**: Image analysis capabilities where supported
- **Streaming**: Real-time response streaming with Server-Sent Events (SSE)
- **Structured Output**: JSON schema validation (OpenAI)
- **Batch Processing**: Bulk request handling
- **Assistants API**: OpenAI Assistants integration

### Security & Compliance
- **PII Detection**: Automatic detection and redaction of sensitive information
- **Compliance Scanning**: GDPR, HIPAA compliance checks on requests and responses
- **Rate Limiting**: Per-tenant and per-user request throttling
- **Audit Logging**: Complete request/response tracking with compliance metadata
- **Content Filtering**: Prompt injection detection and output filtering

## Common Commands

```bash
# Build the application
make build

# Run tests
make test

# Run tests with coverage
make test-coverage

# Start development dependencies (Kafka, Redis, PostgreSQL)
make dev-services

# Build Docker image
make docker-build

# Run locally with default configuration
go run cmd/llm-router/main.go

# Run with custom configuration
go run cmd/llm-router/main.go --config configs/config.yaml
```

## API Endpoints

### OpenAI-compatible
- `POST /v1/chat/completions` - Chat completion endpoint (set `"stream": true` for SSE streaming)
- `POST /v1/completions` - Accepted for compatibility; delegates to chat (expects a `messages[]` body, not the legacy `prompt` shape)
- `POST /v1/embeddings` - Text embeddings (`client.embeddings.create`); routes to the embeddings-capable provider (OpenAI). Returns float vectors regardless of the requested `encoding_format`.
- `GET /v1/models` - List available models. **SDK-aware:** returns OpenAI `{object:"list", data:[…]}` by default, or Anthropic's `{data:[{type:"model",…}]}` shape (Anthropic-owned models only) when the caller sends `anthropic-version` (the Anthropic SDK).
- `GET /v1/models/{model}` - Retrieve a single model (also SDK-aware)

### Anthropic-compatible
- `POST /v1/messages` - Native Anthropic Messages API (top-level `system`, required `max_tokens`, `content` block arrays, native named-event SSE streaming). Translated at the boundary and run through the same pipeline, so a `/v1/messages` request can still be cost-routed to any provider.

> Note: there are **no** `/v1/openai/*` or `/v1/anthropic/*` raw passthrough routes — requests are parsed, scanned, routed, and re-serialized, never reverse-proxied verbatim.

### Pointing a stock SDK at the gateway (AIQG mode)

The customer-facing ingress authenticates on the **gateway token** (`tas_qg_live_*`). Two supported setups:

- **Design 1 (default): token in the SDK slot, vendor key in the vault.** The customer stores their provider (BYOK) key once in the encrypted vault (Plan #14), then points the SDK at the gateway using the gateway token as the `api_key`:
  ```python
  OpenAI(api_key="tas_qg_live_…", base_url="https://<gw>/v1")
  Anthropic(api_key="tas_qg_live_…", base_url="https://<gw>")
  ```
  The gateway lifts the token from `Authorization: Bearer` (OpenAI SDK) or `x-api-key` (Anthropic SDK) — recognized by the `tas_qg_live_` prefix — resolves the tenant, then injects the stored vendor key upstream (falling back to the TAS shared key only when the tenant allows it).
- **Design 2 (power users): vendor key in the SDK slot, identity in a header.** Send the real vendor key as `api_key` and set the gateway token via a custom header:
  ```python
  OpenAI(api_key="sk-…", base_url="https://<gw>/v1", default_headers={"TAS-Auth": "tas_qg_live_…"})
  ```

### Management
- `GET /health` - Health check endpoint
- `GET /metrics` - Prometheus metrics endpoint
- `GET /v1/providers` - Provider status and health information
- `GET /v1/capabilities` - Per-provider capability matrix

## Integration Points

- **Aether Backend**: AI-powered document analysis and classification
- **TAS Agent Builder**: LLM-powered agent creation and execution
- **AudiModal**: Document content analysis and PII detection
- **DeepLake API**: Context retrieval for RAG workflows
- **Keycloak**: Multi-tenant authentication and authorization
- **Kafka**: Async processing events and compliance logging

## Configuration

Configuration is managed via YAML files with environment variable overrides:

```yaml
server:
  port: 8085
  timeout: 300s

providers:
  openai:
    enabled: true
    api_key: ${OPENAI_API_KEY}
    models: [gpt-4, gpt-3.5-turbo]

  anthropic:
    enabled: true
    api_key: ${ANTHROPIC_API_KEY}
    models: [claude-3-opus, claude-3-sonnet]

routing:
  strategy: cost-optimized  # or: round-robin, performance, specific
  fallback_enabled: true
```

## Important Notes

- All LLM requests are scanned for PII and compliance violations before routing
- Cost optimization uses real-time pricing data and model performance metrics
- Provider failover is automatic with health check monitoring
- Streaming responses maintain full feature compatibility with native provider APIs
- Compliance logging is immutable and retained for 7 years
- Integration with shared TAS infrastructure via `tas-shared-network` Docker network
