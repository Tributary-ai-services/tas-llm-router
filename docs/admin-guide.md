# Administrator Guide

This guide covers installation, configuration, deployment, and operational aspects of the LLM Router WAF.

## Table of Contents

- [Installation](#installation)
- [Configuration](#configuration)
- [Security Configuration](#security-configuration)
- [Deployment](#deployment)
- [Monitoring](#monitoring)
- [Maintenance](#maintenance)
- [Troubleshooting](#troubleshooting)

## Installation

### Prerequisites

- **Go 1.21+** (for building from source)
- **Docker** (for containerized deployment)
- **Linux/macOS/Windows** (cross-platform support)

### From Source

```bash
# Clone repository
git clone https://github.com/tributary-ai/llm-router-waf.git
cd llm-router-waf

# Build application
go build -o llm-router cmd/llm-router/main.go

# Verify installation
./llm-router --version
```

### Using Docker

```bash
# Pull image
docker pull tributary-ai/llm-router-waf:latest

# Run container
docker run -p 8080:8080 \
  -v $(pwd)/configs:/app/configs \
  -e OPENAI_API_KEY=your-key \
  tributary-ai/llm-router-waf:latest
```

### Binary Releases

```bash
# Download latest release
curl -L https://github.com/tributary-ai/llm-router-waf/releases/latest/download/llm-router-linux-amd64.tar.gz | tar xz

# Install to system path
sudo mv llm-router /usr/local/bin/
```

## Configuration

### Configuration Files

The primary configuration file is `configs/config.yaml`:

```yaml
# Server Configuration
server:
  port: "8080"
  read_timeout: 30s
  write_timeout: 30s
  max_header_bytes: 1048576  # 1MB

# Router Configuration
router:
  default_strategy: "cost_optimized"  # cost_optimized, performance, round_robin, specific
  health_check_interval: 30s
  max_cost_threshold: 1.0
  enable_fallback_chaining: true
  request_timeout: 120s

# Provider Configuration
providers:
  openai:
    enabled: true
    api_key_env: "OPENAI_API_KEY"
    organization_env: "OPENAI_ORG_ID"
    timeout: 60s
    max_retries: 3
    models:
      - name: "gpt-3.5-turbo"
        enabled: true
        cost_per_token: 0.000002
        max_context_window: 16385
      - name: "gpt-4"
        enabled: true
        cost_per_token: 0.00003
        max_context_window: 8192

  anthropic:
    enabled: true
    api_key_env: "ANTHROPIC_API_KEY"
    timeout: 60s
    max_retries: 3
    models:
      - name: "claude-3-sonnet-20240229"
        enabled: true
        cost_per_token: 0.000015
        max_context_window: 200000

# Security Configuration
security:
  api_keys:
    - "your-api-key-1"
    - "your-api-key-2"
  
  rate_limiting:
    enabled: true
    requests_per_minute: 60
    burst_size: 10
    window_duration: "1m"
  
  cors:
    allowed_origins: ["*"]
    allowed_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
    allowed_headers: ["Content-Type", "Authorization", "X-API-Key"]

# Logging Configuration
logging:
  level: "info"  # debug, info, warn, error
  format: "json"  # json, text
  output: "stdout"  # stdout, file
  file: "logs/llm-router.log"
```

### Environment Variables

Override configuration with environment variables:

```bash
# Server
export LLM_ROUTER_PORT=8080
export LLM_ROUTER_LOG_LEVEL=info

# Providers
export OPENAI_API_KEY=sk-...
export ANTHROPIC_API_KEY=sk-ant-...

# Security
export API_KEYS=key1,key2,key3
export RATE_LIMIT_ENABLED=true
export RATE_LIMIT_REQUESTS_PER_MINUTE=100

# Router
export LLM_ROUTER_DEFAULT_STRATEGY=cost_optimized
```

### Configuration Validation

```bash
# Validate configuration
./llm-router --config configs/config.yaml --validate

# Check configuration
./llm-router --config configs/config.yaml --check-config
```

## Security Configuration

### Authentication

#### API Key Authentication

```yaml
security:
  api_keys:
    - "prod-key-abc123"
    - "staging-key-def456"
    - "dev-key-ghi789"
```

Environment variable method:
```bash
export API_KEYS="key1,key2,key3"
```

#### JWT Authentication

```yaml
security:
  jwt:
    secret: "your-jwt-secret-key"
    expiry: "24h"
    issuer: "llm-router-waf"
```

Generate JWT tokens:
```bash
# Using the router CLI (if implemented)
./llm-router generate-jwt --user-id=user123 --permissions=api:access

# Or use external JWT tools
```

### Rate Limiting

#### Per-User Rate Limits

```yaml
security:
  rate_limiting:
    enabled: true
    requests_per_minute: 60
    burst_size: 10
    window_duration: "1m"
    cleanup_interval: "5m"
```

#### IP-based Rate Limits

```yaml
security:
  rate_limiting:
    ip_based:
      enabled: true
      requests_per_minute: 100
      burst_size: 20
```

### Request Validation

```yaml
security:
  request_validation:
    max_request_size: 10485760  # 10MB
    allowed_methods: ["GET", "POST", "PUT", "DELETE"]
    allowed_content_types: ["application/json", "text/plain"]
    max_json_depth: 20
    max_field_length: 1024
    blocked_patterns:
      - "(?i)script"
      - "(?i)javascript:"
    ip_whitelist: []
    ip_blacklist: []
```

### CORS Configuration

```yaml
security:
  cors:
    allowed_origins: 
      - "https://yourdomain.com"
      - "https://*.yourdomain.com"
    allowed_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
    allowed_headers: ["Content-Type", "Authorization", "X-API-Key"]
    max_age: "86400"
```

### Audit Logging

```yaml
security:
  audit:
    enabled: true
    log_file: "logs/audit.log"
    max_file_size: 104857600  # 100MB
    max_files: 10
    buffer_size: 1000
    flush_interval: "10s"
    include_request: false
    include_response: false
    sensitive_fields: ["password", "token", "secret"]
```

## Deployment

### Systemd Service

Create `/etc/systemd/system/llm-router.service`:

```ini
[Unit]
Description=LLM Router WAF
After=network.target

[Service]
Type=simple
User=llm-router
Group=llm-router
WorkingDirectory=/opt/llm-router
ExecStart=/opt/llm-router/llm-router --config /opt/llm-router/configs/config.yaml
Restart=always
RestartSec=5

# Environment
Environment=OPENAI_API_KEY=your-key
Environment=ANTHROPIC_API_KEY=your-key
EnvironmentFile=-/etc/llm-router/environment

# Security
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/opt/llm-router/logs
ReadWritePaths=/opt/llm-router/data

[Install]
WantedBy=multi-user.target
```

Enable and start:
```bash
sudo systemctl enable llm-router
sudo systemctl start llm-router
sudo systemctl status llm-router
```

### Docker Deployment

#### Docker Compose

Create `docker-compose.yml`:

```yaml
version: '3.8'

services:
  llm-router:
    image: tributary-ai/llm-router-waf:latest
    ports:
      - "8080:8080"
    environment:
      - OPENAI_API_KEY=${OPENAI_API_KEY}
      - ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY}
      - LLM_ROUTER_LOG_LEVEL=info
    volumes:
      - ./configs:/app/configs:ro
      - ./logs:/app/logs
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:8080/health"]
      interval: 30s
      timeout: 10s
      retries: 3

  # Optional: Redis for advanced features
  redis:
    image: redis:alpine
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data
    restart: unless-stopped

volumes:
  redis_data:
```

Deploy:
```bash
docker-compose up -d
```

### Kubernetes Deployment

#### Deployment Manifest

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llm-router
  labels:
    app: llm-router
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
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: llm-router-secrets
              key: anthropic-api-key
        volumeMounts:
        - name: config
          mountPath: /app/configs
        livenessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          httpGet:
            path: /health
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 5
        resources:
          requests:
            memory: "256Mi"
            cpu: "250m"
          limits:
            memory: "512Mi"
            cpu: "500m"
      volumes:
      - name: config
        configMap:
          name: llm-router-config
---
apiVersion: v1
kind: Service
metadata:
  name: llm-router-service
spec:
  selector:
    app: llm-router
  ports:
    - protocol: TCP
      port: 80
      targetPort: 8080
  type: LoadBalancer
```

#### ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: llm-router-config
data:
  config.yaml: |
    server:
      port: "8080"
    router:
      default_strategy: "cost_optimized"
    # ... rest of config
```

#### Secrets

```bash
kubectl create secret generic llm-router-secrets \
  --from-literal=openai-api-key=your-openai-key \
  --from-literal=anthropic-api-key=your-anthropic-key
```

### Load Balancer Configuration

#### Nginx

```nginx
upstream llm_router {
    server 127.0.0.1:8080;
    server 127.0.0.1:8081;
    server 127.0.0.1:8082;
}

server {
    listen 80;
    server_name llm-router.yourdomain.com;

    location / {
        proxy_pass http://llm_router;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        
        # Streaming support
        proxy_buffering off;
        proxy_cache off;
        
        # Timeouts
        proxy_connect_timeout 60s;
        proxy_send_timeout 60s;
        proxy_read_timeout 60s;
    }
}
```

## Monitoring

### Health Checks

```bash
# Basic health
curl http://localhost:8080/health

# Provider health
curl http://localhost:8080/v1/health/openai
curl http://localhost:8080/v1/health/anthropic

# Detailed health with metrics
curl http://localhost:8080/health?details=true
```

### Metrics Endpoints

```bash
# Prometheus metrics (if enabled)
curl http://localhost:8080/metrics

# Internal stats
curl http://localhost:8080/v1/stats
```

### Log Analysis

#### Structured Logging
```bash
# Follow logs
tail -f logs/llm-router.log | jq .

# Filter by level
tail -f logs/llm-router.log | jq 'select(.level == "error")'

# Filter by component
tail -f logs/llm-router.log | jq 'select(.component == "router")'
```

#### Audit Logs
```bash
# View audit events
tail -f logs/audit.log | jq 'select(.audit_event == true)'

# Authentication events
tail -f logs/audit.log | jq 'select(.event_type == "authentication_failure")'
```

### Performance Monitoring

#### Key Metrics
- Request rate (requests/second)
- Response time (percentiles)
- Error rate (%)
- Provider health status
- Rate limit violations
- Authentication failures

#### Alerting Thresholds
```yaml
alerts:
  - name: "High Error Rate"
    condition: "error_rate > 5%"
    duration: "5m"
  
  - name: "High Response Time"
    condition: "response_time_p95 > 2s"
    duration: "2m"
  
  - name: "Provider Down"
    condition: "provider_health == false"
    duration: "30s"
```

## Maintenance

### Log Rotation

Configure logrotate for log files:

```bash
# /etc/logrotate.d/llm-router
/opt/llm-router/logs/*.log {
    daily
    rotate 30
    compress
    delaycompress
    missingok
    notifempty
    create 644 llm-router llm-router
    postrotate
        systemctl reload llm-router
    endscript
}
```

### Database Cleanup (if using persistent storage)

```bash
# Clean old audit logs
./llm-router cleanup --older-than 90d --dry-run
./llm-router cleanup --older-than 90d

# Clean rate limit data
./llm-router cleanup-rate-limits
```

### Configuration Updates

```bash
# Validate new config
./llm-router --config configs/new-config.yaml --validate

# Test configuration
./llm-router --config configs/new-config.yaml --test

# Reload configuration (if supported)
sudo systemctl reload llm-router

# Or restart service
sudo systemctl restart llm-router
```

### API Key Rotation

```bash
# Generate new API key
new_key=$(openssl rand -hex 32)

# Add new key to configuration
# Update config.yaml or environment variables

# Reload configuration
sudo systemctl reload llm-router

# Verify new key works
curl -H "X-API-Key: $new_key" http://localhost:8080/health

# Remove old key from configuration
# Reload again
```

## Troubleshooting

### Common Issues

#### High CPU Usage
```bash
# Check request patterns
curl http://localhost:8080/v1/stats | jq '.requests_per_second'

# Check provider response times
curl http://localhost:8080/v1/providers | jq '.providers[] | {name, avg_response_time}'

# Enable debug logging temporarily
export LLM_ROUTER_LOG_LEVEL=debug
sudo systemctl restart llm-router
```

#### Memory Leaks
```bash
# Monitor memory usage
watch -n 5 'ps aux | grep llm-router'

# Check Go memory stats (if metrics enabled)
curl http://localhost:8080/metrics | grep go_memstats

# Restart service if needed
sudo systemctl restart llm-router
```

#### Provider Connection Issues
```bash
# Test provider connectivity
curl -v https://api.openai.com/v1/models
curl -v https://api.anthropic.com/v1/messages

# Check DNS resolution
nslookup api.openai.com
nslookup api.anthropic.com

# Verify API keys
./llm-router test-providers --config configs/config.yaml
```

### Debug Mode

Enable debug logging:
```yaml
logging:
  level: "debug"
```

Or via environment:
```bash
export LLM_ROUTER_LOG_LEVEL=debug
```

### Collecting Diagnostics

```bash
# Generate diagnostics report
./llm-router diagnostics --output diagnostics.json

# Include recent logs
./llm-router diagnostics --include-logs --last 1h

# Full system info
./llm-router diagnostics --system-info --config-check
```

### Performance Profiling

```bash
# Enable profiling endpoint (development only)
./llm-router --enable-pprof

# Collect CPU profile
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# Collect memory profile
go tool pprof http://localhost:6060/debug/pprof/heap
```

### Log Analysis Examples

```bash
# Find authentication failures
grep "authentication_failed" logs/llm-router.log | tail -20

# Analyze response times
grep "request_completed" logs/llm-router.log | jq '.duration_ms' | sort -n

# Check rate limit violations
grep "rate_limit_exceeded" logs/llm-router.log | jq '.ip_address' | sort | uniq -c

# Provider error analysis
grep "provider_error" logs/llm-router.log | jq '.provider' | sort | uniq -c
```