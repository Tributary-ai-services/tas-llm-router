# Security Guide

This guide covers the security features, configuration, and best practices for the LLM Router WAF.

## Table of Contents

- [Security Overview](#security-overview)
- [Authentication & Authorization](#authentication--authorization)
- [Rate Limiting](#rate-limiting)
- [Request Validation](#request-validation)
- [Audit Logging](#audit-logging)
- [Network Security](#network-security)
- [Security Best Practices](#security-best-practices)
- [Compliance](#compliance)

## Security Overview

The LLM Router WAF provides comprehensive security features designed to protect your LLM infrastructure:

### Security Layers

```
┌─────────────────┐
│   TLS/HTTPS     │ ← Transport Security
├─────────────────┤
│  Authentication │ ← API Keys, JWT
├─────────────────┤
│ Authorization   │ ← Permissions, RBAC
├─────────────────┤
│  Rate Limiting  │ ← Traffic Control
├─────────────────┤
│  Validation     │ ← Input Sanitization
├─────────────────┤
│ Audit Logging   │ ← Security Monitoring
└─────────────────┘
```

### Key Security Features

- **Multi-factor Authentication**: API keys, JWT tokens
- **Advanced Rate Limiting**: Per-user, IP-based, model-specific
- **Input Validation**: Request sanitization, XSS prevention
- **Comprehensive Auditing**: All requests logged and monitored
- **Network Security**: CORS, security headers, IP filtering
- **Zero-trust Architecture**: All requests validated by default

## Authentication & Authorization

### API Key Authentication

API keys provide simple, secure access to the router.

#### Configuration

```yaml
security:
  api_keys:
    - "prod-api-key-abc123def456"
    - "staging-api-key-789ghi012"
    - "dev-api-key-345jkl678"
```

#### Environment Variables

```bash
export API_KEYS="key1,key2,key3"
```

#### Usage

```bash
# X-API-Key header (recommended)
curl -H "X-API-Key: your-api-key" ...

# API-Key header
curl -H "API-Key: your-api-key" ...

# Authorization Bearer
curl -H "Authorization: Bearer your-api-key" ...
```

#### API Key Security

- **Length**: Minimum 32 characters
- **Format**: Use cryptographically secure random strings
- **Rotation**: Rotate keys regularly (monthly/quarterly)
- **Storage**: Store securely (env vars, secrets manager)

```bash
# Generate secure API key
openssl rand -hex 32

# Or using UUID
uuidgen | tr -d '-'
```

### JWT Authentication

JWT tokens provide stateless authentication with claims and expiration.

#### Configuration

```yaml
security:
  jwt:
    secret: "your-256-bit-secret-key-here"
    expiry: "24h"
    issuer: "llm-router-waf"
    algorithm: "HS256"
```

#### JWT Claims Structure

```json
{
  "iss": "llm-router-waf",
  "sub": "user123",
  "aud": "llm-router",
  "exp": 1677652288,
  "iat": 1677565888,
  "nbf": 1677565888,
  "user_id": "user123",
  "permissions": ["api:access", "admin:read"],
  "metadata": {
    "organization": "acme-corp",
    "tier": "premium"
  }
}
```

#### JWT Token Generation

```bash
# Example JWT generation (pseudo-code)
payload='{
  "user_id": "user123",
  "permissions": ["api:access"],
  "metadata": {"tier": "premium"}
}'

jwt_token=$(generate_jwt "$payload" "$JWT_SECRET")
```

### Permission System

#### Built-in Permissions

- `api:access` - Basic API access
- `api:stream` - Streaming endpoint access
- `admin:read` - Read admin endpoints
- `admin:write` - Write admin operations
- `providers:list` - List providers
- `routing:decision` - Access routing decision API

#### Permission Validation

```go
// Example permission check (internal)
func (h *Handler) requirePermission(permission string) middleware.Func {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            authInfo, ok := security.GetAuthInfo(r.Context())
            if !ok {
                http.Error(w, "Authentication required", 401)
                return
            }
            
            if !hasPermission(authInfo.Permissions, permission) {
                http.Error(w, "Insufficient permissions", 403)
                return
            }
            
            next.ServeHTTP(w, r)
        })
    }
}
```

## Rate Limiting

### Rate Limiting Strategies

#### Per-User Rate Limiting

```yaml
security:
  rate_limiting:
    enabled: true
    requests_per_minute: 60
    burst_size: 10
    window_duration: "1m"
```

#### IP-based Rate Limiting

```yaml
security:
  rate_limiting:
    ip_based:
      enabled: true
      requests_per_minute: 100
      burst_size: 20
      whitelist:
        - "192.168.1.0/24"
        - "10.0.0.0/8"
```

#### Model-specific Rate Limits

```yaml
security:
  rate_limiting:
    model_limits:
      "gpt-4":
        requests_per_minute: 20
        burst_size: 5
      "claude-3-opus":
        requests_per_minute: 10
        burst_size: 2
```

### Rate Limit Algorithms

#### Token Bucket Algorithm

The router uses a token bucket algorithm for smooth rate limiting:

- **Tokens**: Available requests
- **Bucket Size**: Maximum burst capacity
- **Refill Rate**: Tokens added per time period
- **Consumption**: One token per request

#### Rate Limit Headers

```http
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 45
X-RateLimit-Reset: 1677652348
Retry-After: 60
```

### Handling Rate Limits

#### Best Practices

1. **Monitor Headers**: Check rate limit headers
2. **Exponential Backoff**: Implement backoff on 429 errors
3. **Request Queuing**: Queue requests to stay under limits
4. **Distribute Load**: Use multiple API keys if needed

#### Example Implementation

```python
import time
import random

class RateLimitHandler:
    def __init__(self, base_delay=1, max_delay=60):
        self.base_delay = base_delay
        self.max_delay = max_delay
    
    def handle_rate_limit(self, response, attempt=0):
        if response.status_code == 429:
            retry_after = int(response.headers.get('Retry-After', self.base_delay))
            delay = min(retry_after + random.uniform(0, 1), self.max_delay)
            time.sleep(delay)
            return True
        return False
```

## Request Validation

### Input Validation

The router validates all incoming requests to prevent malicious input.

#### Validation Configuration

```yaml
security:
  request_validation:
    max_request_size: 10485760  # 10MB
    max_json_depth: 20
    max_field_length: 1024
    allowed_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
    allowed_content_types: 
      - "application/json"
      - "text/plain"
      - "multipart/form-data"
    blocked_patterns:
      - "(?i)<script[^>]*>.*?</script>"
      - "(?i)javascript:"
      - "(?i)data:text/html"
      - "(?i)vbscript:"
```

### Content Validation

#### JSON Validation

```go
// JSON structure validation
type ValidationRules struct {
    MaxDepth       int
    MaxFieldLength int
    RequiredFields []string
    AllowedFields  []string
}

func validateJSON(data []byte, rules ValidationRules) error {
    var obj interface{}
    if err := json.Unmarshal(data, &obj); err != nil {
        return fmt.Errorf("invalid JSON: %w", err)
    }
    
    if depth := calculateDepth(obj); depth > rules.MaxDepth {
        return fmt.Errorf("JSON depth %d exceeds maximum %d", depth, rules.MaxDepth)
    }
    
    return validateFields(obj, rules)
}
```

#### XSS Prevention

```go
func sanitizeInput(input string) string {
    // Remove dangerous HTML tags
    re := regexp.MustCompile(`<script[^>]*>.*?</script>`)
    input = re.ReplaceAllString(input, "")
    
    // Remove JavaScript URLs
    re = regexp.MustCompile(`(?i)javascript:`)
    input = re.ReplaceAllString(input, "")
    
    // Remove null bytes
    input = strings.ReplaceAll(input, "\x00", "")
    
    return input
}
```

### IP Filtering

#### Whitelist Configuration

```yaml
security:
  request_validation:
    ip_whitelist:
      - "192.168.1.0/24"    # Private network
      - "10.0.0.0/8"        # Corporate network
      - "203.0.113.0/24"    # Specific public range
```

#### Blacklist Configuration

```yaml
security:
  request_validation:
    ip_blacklist:
      - "192.0.2.0/24"      # Known bad actors
      - "198.51.100.50"     # Specific malicious IP
```

## Audit Logging

### Comprehensive Security Logging

All security events are logged with structured data for analysis.

#### Audit Configuration

```yaml
security:
  audit:
    enabled: true
    log_file: "logs/security-audit.log"
    max_file_size: 104857600  # 100MB
    max_files: 10
    buffer_size: 1000
    flush_interval: "10s"
    include_request_headers: false
    include_response_headers: false
    sensitive_fields:
      - "password"
      - "token"
      - "secret"
      - "key"
      - "authorization"
```

### Security Event Types

#### Authentication Events

```json
{
  "timestamp": "2024-01-15T10:30:00Z",
  "event_type": "authentication_success",
  "user_id": "user123",
  "ip_address": "192.168.1.100",
  "user_agent": "MyApp/1.0",
  "auth_method": "api_key",
  "session_id": "sess_abc123"
}
```

#### Authorization Events

```json
{
  "timestamp": "2024-01-15T10:30:05Z",
  "event_type": "authorization_failure",
  "user_id": "user123",
  "ip_address": "192.168.1.100",
  "resource": "/v1/admin/users",
  "required_permission": "admin:read",
  "user_permissions": ["api:access"]
}
```

#### Rate Limit Events

```json
{
  "timestamp": "2024-01-15T10:30:10Z",
  "event_type": "rate_limit_exceeded",
  "user_id": "user123",
  "ip_address": "192.168.1.100",
  "rate_limit": "60/minute",
  "current_usage": 61,
  "retry_after": 45
}
```

#### Security Violations

```json
{
  "timestamp": "2024-01-15T10:30:15Z",
  "event_type": "security_violation",
  "ip_address": "203.0.113.50",
  "violation_type": "blocked_pattern_detected",
  "details": {
    "pattern": "(?i)<script",
    "field": "content",
    "blocked_content": "<script>alert('xss')</script>"
  }
}
```

### Log Analysis

#### Security Monitoring Queries

```bash
# Failed authentication attempts
jq 'select(.event_type == "authentication_failure")' security-audit.log

# Rate limit violations by IP
jq 'select(.event_type == "rate_limit_exceeded") | .ip_address' security-audit.log | sort | uniq -c

# Security violations in last hour
jq --arg since "$(date -d '1 hour ago' -u +%Y-%m-%dT%H:%M:%SZ)" 'select(.timestamp > $since and .event_type == "security_violation")' security-audit.log

# Top attacking IPs
jq 'select(.event_type | test("violation|failure"))' security-audit.log | jq -r '.ip_address' | sort | uniq -c | sort -nr | head -10
```

### SIEM Integration

#### Syslog Export

```yaml
security:
  audit:
    syslog:
      enabled: true
      server: "siem.company.com:514"
      protocol: "tcp"
      facility: "local0"
      tag: "llm-router"
```

#### JSON Export

```yaml
security:
  audit:
    remote_endpoint: "https://siem.company.com/api/events"
    remote_token: "your-siem-token"
    batch_size: 100
    flush_interval: "30s"
```

## Network Security

### TLS Configuration

Always use HTTPS in production:

```nginx
server {
    listen 443 ssl http2;
    server_name llm-router.company.com;
    
    ssl_certificate /path/to/cert.pem;
    ssl_certificate_key /path/to/key.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers ECDHE-RSA-AES128-GCM-SHA256:ECDHE-RSA-AES256-GCM-SHA384;
    ssl_prefer_server_ciphers off;
    
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

### CORS Configuration

```yaml
security:
  cors:
    allowed_origins:
      - "https://app.company.com"
      - "https://*.company.com"
    allowed_methods: ["GET", "POST", "PUT", "DELETE", "OPTIONS"]
    allowed_headers: 
      - "Content-Type"
      - "Authorization"
      - "X-API-Key"
      - "X-Requested-With"
    max_age: 86400
    allow_credentials: false
```

### Security Headers

The router automatically adds security headers:

```http
X-Content-Type-Options: nosniff
X-Frame-Options: DENY
X-XSS-Protection: 1; mode=block
Referrer-Policy: strict-origin-when-cross-origin
Content-Security-Policy: default-src 'self'
Strict-Transport-Security: max-age=31536000; includeSubDomains
```

## Security Best Practices

### API Key Management

#### Generation

```bash
# Generate cryptographically secure API keys
openssl rand -base64 32

# Or using Python
python3 -c "import secrets; print(secrets.token_urlsafe(32))"

# Or using Node.js
node -e "console.log(require('crypto').randomBytes(32).toString('base64'))"
```

#### Storage

✅ **Good Practices:**
- Use environment variables
- Use secrets management systems (AWS Secrets Manager, HashiCorp Vault)
- Encrypt at rest
- Restrict access permissions

❌ **Avoid:**
- Hardcoding in source code
- Storing in plain text files
- Logging API keys
- Sharing via insecure channels

#### Rotation

```bash
# Automated key rotation script
#!/bin/bash
NEW_KEY=$(openssl rand -base64 32)

# Update secrets manager
aws secretsmanager put-secret-value \
  --secret-id llm-router-api-keys \
  --secret-string "$NEW_KEY"

# Restart service to pick up new key
kubectl rollout restart deployment/llm-router
```

### Network Security

#### Firewall Rules

```bash
# Allow only necessary ports
ufw allow 22/tcp      # SSH
ufw allow 443/tcp     # HTTPS
ufw deny 8080/tcp     # Block direct access to router
```

#### VPC Configuration

```yaml
# Example AWS Security Group
SecurityGroup:
  Type: AWS::EC2::SecurityGroup
  Properties:
    GroupDescription: LLM Router WAF Security Group
    SecurityGroupIngress:
      - IpProtocol: tcp
        FromPort: 443
        ToPort: 443
        CidrIp: 0.0.0.0/0
        Description: HTTPS traffic
      - IpProtocol: tcp
        FromPort: 8080
        ToPort: 8080
        SourceSecurityGroupId: !Ref LoadBalancerSecurityGroup
        Description: Internal traffic from load balancer
```

### Monitoring & Alerting

#### Security Alerts

```yaml
# Example alerting rules
alerts:
  - name: "High Failed Authentication Rate"
    condition: "authentication_failures > 10 per minute"
    severity: "warning"
    notification: "slack://security-channel"
  
  - name: "Rate Limit Violations"
    condition: "rate_limit_exceeded > 50 per minute"
    severity: "warning"
    
  - name: "Security Violation Detected"
    condition: "security_violation > 0"
    severity: "critical"
    notification: "pagerduty://security-team"
```

#### Health Monitoring

```bash
# Security health check script
#!/bin/bash

# Check authentication endpoint
if ! curl -f -H "X-API-Key: test-key" https://llm-router.company.com/health; then
    echo "CRITICAL: Authentication check failed"
    exit 2
fi

# Check rate limiting
for i in {1..5}; do
    curl -H "X-API-Key: test-key" https://llm-router.company.com/v1/providers
done

if curl -H "X-API-Key: test-key" https://llm-router.company.com/v1/providers | grep -q "429"; then
    echo "OK: Rate limiting is working"
else
    echo "WARNING: Rate limiting may not be working"
    exit 1
fi
```

### Incident Response

#### Security Incident Playbook

1. **Detection**: Monitor security logs and alerts
2. **Assessment**: Determine severity and scope
3. **Containment**: Block malicious IPs, disable compromised keys
4. **Eradication**: Remove malicious content, patch vulnerabilities
5. **Recovery**: Restore services, implement additional monitoring
6. **Lessons Learned**: Update security measures and documentation

#### Emergency Procedures

```bash
# Emergency IP blocking
iptables -A INPUT -s 203.0.113.50 -j DROP

# Emergency API key revocation
# Update configuration to remove compromised key
# Restart service
systemctl restart llm-router

# Emergency rate limit adjustment
# Temporarily reduce rate limits during attack
export RATE_LIMIT_REQUESTS_PER_MINUTE=10
systemctl restart llm-router
```

## Compliance

### Data Protection

#### GDPR Compliance
- **Data Minimization**: Only collect necessary data
- **Purpose Limitation**: Use data only for stated purposes  
- **Data Retention**: Automatic log rotation and deletion
- **Right to Erasure**: Ability to delete user data from logs

#### CCPA Compliance
- **Transparency**: Clear privacy policy
- **User Rights**: Access to personal data in logs
- **Data Security**: Encryption and access controls

### Industry Standards

#### SOC 2 Compliance
- **Security**: Access controls, encryption, monitoring
- **Availability**: Uptime monitoring, incident response
- **Processing Integrity**: Input validation, audit logging
- **Confidentiality**: Data encryption, access controls
- **Privacy**: Data handling policies, user consent

#### ISO 27001
- **Information Security Management**: Policies and procedures
- **Risk Management**: Regular security assessments
- **Incident Management**: Incident response procedures
- **Business Continuity**: Backup and recovery plans

### Security Assessments

#### Regular Security Reviews

```bash
# Security assessment checklist
□ API key strength and rotation
□ Rate limiting effectiveness
□ Input validation coverage
□ Audit log completeness
□ Network security configuration
□ TLS certificate validity
□ Dependency vulnerabilities
□ Access control verification
```

#### Penetration Testing

```bash
# Example security testing
# Test authentication bypass
curl -X POST https://llm-router.company.com/v1/chat/completions

# Test injection attacks
curl -X POST https://llm-router.company.com/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"<script>alert(1)</script>","messages":[]}'

# Test rate limiting
for i in {1..100}; do
  curl -H "X-API-Key: test-key" https://llm-router.company.com/v1/providers &
done
```