# LLM Router WAF Documentation

Welcome to the LLM Router WAF (Web Application Firewall) - a production-ready routing and security layer for Large Language Model APIs.

## 🆕 New Features: Client-Controlled Retry & Fallback

The LLM Router now supports powerful client-configurable retry and fallback behavior with comprehensive OpenAPI documentation and validation!

### 🌐 Interactive API Documentation
- **[Swagger UI](http://localhost:8086/docs)** - Interactive API explorer with live examples
- **[OpenAPI Spec](http://localhost:8086/docs/openapi.yaml)** - Complete API specification
- **✅ Schema Validation** - Automatic request/response validation

### 🔄 Enhanced Reliability Features
- **Client-Controlled Retry** - Exponential/linear backoff with configurable error patterns
- **Smart Fallback** - Automatic failover with cost and feature constraints  
- **Detailed Metadata** - Full visibility into retry attempts and routing decisions

## Quick Links

- **[User Guide](user-guide.md)** - How to use the LLM Router WAF API
- **[Admin Guide](admin-guide.md)** - Configuration and deployment 
- **[Developer Guide](developer-guide.md)** - Contributing and development
- **[API Reference](api-reference.md)** - Complete API documentation
- **[Security Guide](security-guide.md)** - Security features and configuration

## Overview

The LLM Router WAF is an intelligent routing and security layer that sits between your applications and LLM providers (OpenAI, Anthropic, etc.). It provides:

### 🎯 Core Features
- **Zero Feature Loss** - Full API compatibility with provider SDKs
- **Intelligent Routing** - Cost optimization, performance, and failover
- **Enterprise Security** - Authentication, rate limiting, audit logging
- **Comprehensive Monitoring** - Health checks, metrics, and observability

### 🔐 Security Features
- JWT and API key authentication
- Advanced rate limiting with token bucket algorithm
- Request validation and sanitization
- Comprehensive audit logging
- CORS and security headers
- IP whitelisting/blacklisting

### 🚀 Provider Support
- **OpenAI** - Complete GPT models, function calling, vision
- **Anthropic** - Claude models with full feature support
- **Extensible** - Easy to add new providers

### 📊 Routing Strategies
- Cost-optimized routing
- Performance-based routing
- Round-robin load balancing
- Provider-specific routing
- Intelligent failover

## Architecture

```
┌─────────────────┐    ┌─────────────────┐    ┌─────────────────┐
│   Your App      │───▶│  LLM Router     │───▶│   Providers     │
│                 │    │      WAF        │    │  OpenAI/Claude  │
└─────────────────┘    └─────────────────┘    └─────────────────┘
                              │
                              ▼
                       ┌─────────────────┐
                       │   Security &    │
                       │   Monitoring    │
                       └─────────────────┘
```

## Quick Start

### 1. Installation

```bash
# Clone the repository
git clone https://github.com/tributary-ai/llm-router-waf.git
cd llm-router-waf

# Build the application
go build -o llm-router cmd/llm-router/main.go
```

### 2. Configuration

```bash
# Copy example configuration
cp configs/config.yaml.example configs/config.yaml

# Set your API keys
export OPENAI_API_KEY="your-openai-key"
export ANTHROPIC_API_KEY="your-anthropic-key"
```

### 3. Run

```bash
# Start the router
./llm-router --config configs/config.yaml

# Router will be available at http://localhost:8080
```

### 4. Test

```bash
# Test with OpenAI-compatible request
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

## What's Implemented

### ✅ Day 1 - Core Infrastructure
- [x] Project structure and dependencies
- [x] Provider interfaces and type system
- [x] OpenAI provider with full API support
- [x] Anthropic provider with Claude support
- [x] Intelligent routing engine
- [x] HTTP server with middleware
- [x] Configuration management
- [x] Main application and testing

### ✅ Day 2 Task 1 - Security & Authentication
- [x] JWT and API key authentication
- [x] Advanced rate limiting
- [x] Request validation and sanitization
- [x] Security audit logging
- [x] Security middleware stack
- [x] Server integration

### 🚧 In Progress
- [ ] Advanced routing & load balancing
- [ ] Observability & monitoring
- [ ] Caching & performance optimization
- [ ] Data pipeline & analytics
- [ ] Advanced provider features
- [ ] Configuration management & deployment
- [ ] Testing & quality assurance

## Directory Structure

```
llm-router-waf/
├── cmd/llm-router/          # Application entry point
├── internal/
│   ├── config/              # Configuration management
│   ├── middleware/          # HTTP middleware
│   ├── providers/           # LLM provider implementations
│   │   ├── openai/         # OpenAI integration
│   │   └── anthropic/      # Anthropic integration
│   ├── routing/            # Routing engine
│   ├── security/           # Security components
│   ├── server/             # HTTP server
│   └── types/              # Common types
├── configs/                # Configuration files
├── docs/                   # Documentation
├── docker/                 # Docker configurations
└── tests/                  # Test files
```

## Support

- **Issues**: [GitHub Issues](https://github.com/tributary-ai/llm-router-waf/issues)
- **Documentation**: [Full Documentation](docs/)
- **Examples**: [examples/](examples/)

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.