# LLM Router WAF Documentation

## Overview

The LLM Router WAF (Web Application Firewall) is a production-ready routing and security layer for Large Language Model APIs. It provides intelligent routing between multiple LLM providers while maintaining enterprise-grade security, monitoring, and cost optimization.

## 📚 Documentation

### For Users
- **[User Guide](user-guide.md)** - How to use the API, examples, and best practices
- **[API Reference](api-reference.md)** - Complete API documentation with examples

### For Administrators  
- **[Admin Guide](admin-guide.md)** - Installation, configuration, and deployment
- **[Security Guide](security-guide.md)** - Security features and best practices

### For Developers
- **[Developer Guide](developer-guide.md)** - Development setup and contribution guidelines

## 🚀 Quick Start

### 1. Install
```bash
git clone https://github.com/tributary-ai/llm-router-waf.git
cd llm-router-waf
go build -o llm-router cmd/llm-router/main.go
```

### 2. Configure
```bash
cp configs/config.yaml.example configs/config.yaml
export OPENAI_API_KEY="your-openai-key"
export ANTHROPIC_API_KEY="your-anthropic-key"
```

### 3. Run
```bash
./llm-router --config configs/config.yaml
```

### 4. Test
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "Hello!"}]}'
```

## 🏗️ Architecture

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

## ✨ Key Features

### 🎯 Core Features
- **Zero Feature Loss** - Full compatibility with provider SDKs
- **Intelligent Routing** - Cost optimization and performance-based routing
- **Failover Support** - Automatic provider failover on errors
- **Health Monitoring** - Continuous provider health checks

### 🔐 Security Features
- **Authentication** - API key and JWT token support
- **Rate Limiting** - Advanced token bucket rate limiting
- **Input Validation** - Comprehensive request validation and sanitization
- **Audit Logging** - Full security event logging
- **CORS Support** - Cross-origin request handling
- **IP Filtering** - Whitelist and blacklist support

### 📊 Routing Strategies
- **Cost Optimized** - Route to cheapest provider
- **Performance** - Route based on response times
- **Round Robin** - Distribute load evenly
- **Provider Specific** - Force specific provider
- **Fallback Chains** - Multi-tier failover

### 🌐 Provider Support
- **OpenAI** - Complete API support including GPT-4, function calling, vision
- **Anthropic** - Claude models with streaming support
- **Extensible** - Easy to add new providers

## 📈 Current Status

### ✅ Implemented (Day 1)
- [x] Core infrastructure and HTTP server
- [x] Provider abstraction layer
- [x] OpenAI provider (complete API support)
- [x] Anthropic provider (Claude support)
- [x] Intelligent routing engine
- [x] Configuration management
- [x] Health checks and monitoring

### ✅ Implemented (Day 2 - Task 1)
- [x] JWT and API key authentication
- [x] Advanced rate limiting with token bucket
- [x] Request validation and sanitization
- [x] Comprehensive audit logging
- [x] Security middleware stack
- [x] CORS and security headers

### 🚧 In Progress (Day 2)
- [ ] Advanced routing and load balancing
- [ ] Observability and monitoring (Prometheus, tracing)
- [ ] Caching and performance optimization
- [ ] Data pipeline and analytics
- [ ] Advanced provider features
- [ ] Configuration management and deployment tools
- [ ] Comprehensive testing suite

## 🛠️ Use Cases

### Enterprise API Gateway
- Centralized LLM access control
- Cost monitoring and optimization
- Security policy enforcement
- Multi-tenant isolation

### Development Teams
- Unified API across multiple providers
- Easy provider switching and testing
- Cost tracking per team/project
- Rate limiting and access control

### Production Applications
- High availability with failover
- Performance optimization
- Security compliance (SOC2, GDPR)
- Comprehensive audit trails

### Cost Optimization
- Automatic routing to cheapest provider
- Usage analytics and reporting
- Budget controls and alerts
- Cost allocation by user/team

## 🔧 Configuration

### Basic Configuration
```yaml
server:
  port: "8080"

providers:
  openai:
    enabled: true
    api_key_env: "OPENAI_API_KEY"
  anthropic:
    enabled: true
    api_key_env: "ANTHROPIC_API_KEY"

security:
  api_keys: ["your-api-key"]
  rate_limiting:
    enabled: true
    requests_per_minute: 60
```

### Environment Variables
```bash
# Required
export OPENAI_API_KEY="sk-..."
export ANTHROPIC_API_KEY="sk-ant-..."

# Optional
export LLM_ROUTER_PORT=8080
export LLM_ROUTER_LOG_LEVEL=info
export API_KEYS="key1,key2,key3"
```

## 📊 Monitoring

### Health Endpoints
```bash
# Overall health
curl http://localhost:8080/health

# Provider health
curl http://localhost:8080/v1/health/openai
curl http://localhost:8080/v1/health/anthropic
```

### Management Endpoints
```bash
# List providers
curl http://localhost:8080/v1/providers

# Get routing decision
curl -X POST http://localhost:8080/v1/routing/decision \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo"}'
```

## 🐳 Deployment

### Docker
```bash
docker run -p 8080:8080 \
  -e OPENAI_API_KEY=your-key \
  -e ANTHROPIC_API_KEY=your-key \
  -v $(pwd)/configs:/app/configs \
  tributary-ai/llm-router-waf:latest
```

### Kubernetes
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
        image: tributary-ai/llm-router-waf:latest
        ports:
        - containerPort: 8080
        env:
        - name: OPENAI_API_KEY
          valueFrom:
            secretKeyRef:
              name: llm-router-secrets
              key: openai-api-key
```

## 🤝 Contributing

We welcome contributions! See the [Developer Guide](developer-guide.md) for details on:

- Development setup
- Code organization  
- Adding new providers
- Testing guidelines
- Pull request process

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](../LICENSE) file for details.

## 🆘 Support

- **Documentation**: [docs/](.)
- **Issues**: [GitHub Issues](https://github.com/tributary-ai/llm-router-waf/issues)
- **Examples**: [examples/](../examples/)
- **Discussions**: [GitHub Discussions](https://github.com/tributary-ai/llm-router-waf/discussions)

## 🗺️ Roadmap

### Phase 1 - Core Infrastructure ✅
- Basic routing and provider support
- Security fundamentals
- Configuration management

### Phase 2 - Advanced Features 🚧  
- Advanced routing algorithms
- Comprehensive monitoring
- Performance optimization
- Analytics and reporting

### Phase 3 - Enterprise Features 📋
- Multi-tenancy
- Advanced security features
- Compliance tooling
- Enterprise integrations

### Phase 4 - AI-Powered Features 🔮
- Intelligent request optimization
- Automatic model selection
- Predictive scaling
- Anomaly detection