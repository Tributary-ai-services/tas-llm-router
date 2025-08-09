# User Guide

This guide explains how to use the LLM Router WAF as an end user or application developer.

## Table of Contents

- [Getting Started](#getting-started)
- [API Usage](#api-usage)
- [Authentication](#authentication)
- [Supported Models](#supported-models)
- [Examples](#examples)
- [Error Handling](#error-handling)
- [Rate Limits](#rate-limits)

## Getting Started

The LLM Router WAF provides OpenAI and Anthropic compatible APIs, allowing you to use your existing SDKs and code with minimal changes.

### Base URL
```
http://localhost:8080  # Default local deployment
```

### API Versions
- `/v1/` - OpenAI compatible endpoints
- `/v1/messages` - Anthropic compatible endpoint

## Authentication

### API Keys
Use your API key in the request headers:

```bash
# Method 1: X-API-Key header
curl -H "X-API-Key: your-api-key" ...

# Method 2: Authorization Bearer token
curl -H "Authorization: Bearer your-api-key" ...
```

### JWT Tokens
For JWT authentication:

```bash
curl -H "Authorization: Bearer your-jwt-token" ...
```

## API Usage

### OpenAI Compatible Endpoints

#### Chat Completions
```bash
POST /v1/chat/completions
```

**Example Request:**
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {"role": "user", "content": "What is the capital of France?"}
    ],
    "temperature": 0.7,
    "max_tokens": 100
  }'
```

**Response:**
```json
{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1677652288,
  "choices": [{
    "index": 0,
    "message": {
      "role": "assistant",
      "content": "The capital of France is Paris."
    },
    "finish_reason": "stop"
  }],
  "usage": {
    "prompt_tokens": 13,
    "completion_tokens": 7,
    "total_tokens": 20
  }
}
```

#### Text Completions (Legacy)
```bash
POST /v1/completions
```

**Example Request:**
```bash
curl -X POST http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo-instruct",
    "prompt": "The capital of France is",
    "max_tokens": 10
  }'
```

#### Function Calling
```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {"role": "user", "content": "What is the weather like in Boston?"}
    ],
    "functions": [
      {
        "name": "get_current_weather",
        "description": "Get the current weather in a given location",
        "parameters": {
          "type": "object",
          "properties": {
            "location": {
              "type": "string",
              "description": "The city and state, e.g. San Francisco, CA"
            },
            "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]}
          },
          "required": ["location"]
        }
      }
    ],
    "function_call": "auto"
  }'
```

### Anthropic Compatible Endpoints

#### Messages API
```bash
POST /v1/messages
```

**Example Request:**
```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "claude-3-sonnet-20240229",
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello, Claude"}
    ]
  }'
```

**Response:**
```json
{
  "id": "msg_013Zva2CMHLNnXjNJJKqJ2EF",
  "type": "message",
  "role": "assistant",
  "content": [
    {
      "type": "text",
      "text": "Hello! It's nice to meet you. How can I help you today?"
    }
  ],
  "model": "claude-3-sonnet-20240229",
  "stop_reason": "end_turn",
  "stop_sequence": null,
  "usage": {
    "input_tokens": 10,
    "output_tokens": 25
  }
}
```

## Supported Models

### OpenAI Models
- `gpt-4` - GPT-4 latest
- `gpt-4-turbo` - GPT-4 Turbo
- `gpt-4-turbo-preview` - GPT-4 Turbo Preview
- `gpt-3.5-turbo` - GPT-3.5 Turbo latest
- `gpt-3.5-turbo-instruct` - GPT-3.5 Instruct
- `gpt-4-vision-preview` - GPT-4 with Vision

### Anthropic Models
- `claude-3-opus-20240229` - Claude 3 Opus
- `claude-3-sonnet-20240229` - Claude 3 Sonnet
- `claude-3-haiku-20240307` - Claude 3 Haiku
- `claude-instant-1.2` - Claude Instant

## Examples

### Using with OpenAI SDK (Python)

```python
import openai

# Point to your LLM Router WAF instance
openai.api_base = "http://localhost:8080/v1"
openai.api_key = "your-api-key"

# Use normally - the router will handle provider selection
response = openai.ChatCompletion.create(
    model="gpt-3.5-turbo",
    messages=[
        {"role": "user", "content": "Hello, world!"}
    ]
)

print(response.choices[0].message.content)
```

### Using with Anthropic SDK (Python)

```python
import anthropic

# Create client pointing to your router
client = anthropic.Anthropic(
    api_key="your-api-key",
    base_url="http://localhost:8080/v1"
)

# Use normally
message = client.messages.create(
    model="claude-3-sonnet-20240229",
    max_tokens=1024,
    messages=[
        {"role": "user", "content": "Hello, Claude"}
    ]
)

print(message.content[0].text)
```

### Using with JavaScript/Node.js

```javascript
// Using fetch API
const response = await fetch('http://localhost:8080/v1/chat/completions', {
  method: 'POST',
  headers: {
    'Content-Type': 'application/json',
    'X-API-Key': 'your-api-key'
  },
  body: JSON.stringify({
    model: 'gpt-3.5-turbo',
    messages: [
      { role: 'user', content: 'Hello from JavaScript!' }
    ]
  })
});

const data = await response.json();
console.log(data.choices[0].message.content);
```

### Streaming Responses

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {"role": "user", "content": "Tell me a story"}
    ],
    "stream": true
  }'
```

## Error Handling

### HTTP Status Codes
- `200` - Success
- `400` - Bad Request (invalid parameters)
- `401` - Unauthorized (invalid API key)
- `403` - Forbidden (insufficient permissions)
- `429` - Too Many Requests (rate limit exceeded)
- `500` - Internal Server Error
- `502` - Bad Gateway (provider error)
- `503` - Service Unavailable

### Error Response Format

```json
{
  "error": {
    "message": "Invalid API key provided",
    "type": "authentication_error",
    "code": 401
  },
  "timestamp": 1677652288
}
```

### Common Error Types
- `authentication_error` - Invalid credentials
- `rate_limit_error` - Too many requests
- `validation_error` - Invalid request format
- `provider_error` - Upstream provider issue

## Rate Limits

### Default Limits
- **Per API Key**: 60 requests/minute
- **Burst**: 10 requests
- **IP-based**: Fallback for unauthenticated requests

### Rate Limit Headers
```
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 59
X-RateLimit-Reset: 1677652348
```

### Handling Rate Limits

When rate limited (HTTP 429), the response includes:
```json
{
  "error": {
    "message": "Rate limit exceeded",
    "type": "rate_limit_error",
    "code": 429,
    "retry_after": 60
  },
  "timestamp": 1677652288
}
```

**Best Practices:**
- Implement exponential backoff
- Respect the `Retry-After` header
- Use request queuing for high-volume applications

## Management Endpoints

### Health Check
```bash
GET /health
```

Response:
```json
{
  "status": "healthy",
  "timestamp": 1677652288,
  "version": "1.0.0"
}
```

### Provider Health
```bash
GET /v1/health/openai
GET /v1/health/anthropic
```

### List Providers
```bash
GET /v1/providers
```

Response:
```json
{
  "providers": [
    {
      "name": "openai",
      "status": "healthy",
      "models": ["gpt-3.5-turbo", "gpt-4"]
    },
    {
      "name": "anthropic", 
      "status": "healthy",
      "models": ["claude-3-sonnet-20240229"]
    }
  ]
}
```

### Routing Decision
```bash
POST /v1/routing/decision
```

Request:
```json
{
  "model": "gpt-3.5-turbo",
  "strategy": "cost_optimized"
}
```

Response:
```json
{
  "provider": "openai",
  "model": "gpt-3.5-turbo",
  "estimated_cost": 0.002,
  "reasoning": "Lowest cost option available"
}
```

## Best Practices

### Performance
- Use streaming for long responses
- Implement client-side caching when appropriate
- Choose appropriate models for your use case

### Cost Optimization  
- Use the cost-optimized routing strategy
- Monitor usage through the management API
- Set appropriate max_tokens limits

### Security
- Keep API keys secure and rotate regularly
- Use HTTPS in production
- Implement proper error handling
- Don't log sensitive data

### Reliability
- Implement retry logic with exponential backoff
- Handle rate limits gracefully
- Monitor health endpoints
- Use appropriate timeout values