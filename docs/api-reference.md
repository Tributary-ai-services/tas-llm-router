# API Reference

Complete API reference for the LLM Router WAF.

## Table of Contents

- [Authentication](#authentication)
- [OpenAI Compatible Endpoints](#openai-compatible-endpoints)
- [Anthropic Compatible Endpoints](#anthropic-compatible-endpoints)
- [Management Endpoints](#management-endpoints)
- [Error Responses](#error-responses)
- [Rate Limiting](#rate-limiting)

## Authentication

All API requests must be authenticated using one of the following methods:

### API Key Authentication

Include your API key in the request headers:

```http
X-API-Key: your-api-key
```

or

```http
API-Key: your-api-key
```

### Bearer Token Authentication

Use Bearer token format for API keys or JWT tokens:

```http
Authorization: Bearer your-api-key-or-jwt-token
```

## OpenAI Compatible Endpoints

### Chat Completions

Creates a chat completion response for the provided messages.

```http
POST /v1/chat/completions
```

#### Request Body

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | Yes | ID of the model to use |
| `messages` | array | Yes | Array of message objects |
| `temperature` | number | No | Sampling temperature (0-2) |
| `max_tokens` | integer | No | Maximum tokens to generate |
| `top_p` | number | No | Nucleus sampling parameter |
| `n` | integer | No | Number of completions to generate |
| `stream` | boolean | No | Whether to stream responses |
| `stop` | string/array | No | Stop sequences |
| `presence_penalty` | number | No | Presence penalty (-2 to 2) |
| `frequency_penalty` | number | No | Frequency penalty (-2 to 2) |
| `logit_bias` | object | No | Token logit biases |
| `user` | string | No | User identifier |
| `functions` | array | No | Available functions for calling |
| `function_call` | string/object | No | Control function calling |

#### Message Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `role` | string | Yes | Role: `system`, `user`, `assistant`, or `function` |
| `content` | string | Yes | Message content |
| `name` | string | No | Name of function (for function role) |
| `function_call` | object | No | Function call details (for assistant role) |

#### Example Request

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {
        "role": "system",
        "content": "You are a helpful assistant."
      },
      {
        "role": "user",
        "content": "What is the capital of France?"
      }
    ],
    "temperature": 0.7,
    "max_tokens": 100
  }'
```

#### Response

```json
{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1677652288,
  "model": "gpt-3.5-turbo",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "The capital of France is Paris."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 20,
    "completion_tokens": 7,
    "total_tokens": 27
  }
}
```

#### Streaming Response

When `stream: true`, responses are sent as Server-Sent Events:

```
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":"The"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"gpt-3.5-turbo","choices":[{"index":0,"delta":{"content":" capital"},"finish_reason":null}]}

data: [DONE]
```

### Text Completions

Creates a completion for the provided prompt.

```http
POST /v1/completions
```

#### Request Body

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | Yes | ID of the model to use |
| `prompt` | string/array | Yes | Prompt(s) to generate completions for |
| `max_tokens` | integer | No | Maximum tokens to generate |
| `temperature` | number | No | Sampling temperature |
| `top_p` | number | No | Nucleus sampling parameter |
| `n` | integer | No | Number of completions |
| `stream` | boolean | No | Whether to stream responses |
| `logprobs` | integer | No | Include log probabilities |
| `echo` | boolean | No | Echo back the prompt |
| `stop` | string/array | No | Stop sequences |
| `presence_penalty` | number | No | Presence penalty |
| `frequency_penalty` | number | No | Frequency penalty |
| `best_of` | integer | No | Generate multiple and return best |
| `logit_bias` | object | No | Token logit biases |
| `user` | string | No | User identifier |

#### Example Request

```bash
curl -X POST http://localhost:8080/v1/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo-instruct",
    "prompt": "The quick brown fox",
    "max_tokens": 20,
    "temperature": 0.5
  }'
```

### Function Calling

Functions allow the model to call external tools.

#### Function Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | Yes | Function name |
| `description` | string | No | Function description |
| `parameters` | object | Yes | JSON Schema for parameters |

#### Example with Functions

```bash
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [
      {
        "role": "user",
        "content": "What is the weather like in Boston?"
      }
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
            "unit": {
              "type": "string",
              "enum": ["celsius", "fahrenheit"]
            }
          },
          "required": ["location"]
        }
      }
    ],
    "function_call": "auto"
  }'
```

## Anthropic Compatible Endpoints

### Messages

Create a message with Claude models.

```http
POST /v1/messages
```

#### Request Body

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | Yes | Claude model to use |
| `messages` | array | Yes | Array of message objects |
| `max_tokens` | integer | Yes | Maximum tokens to generate |
| `system` | string | No | System message |
| `temperature` | number | No | Sampling temperature (0-1) |
| `top_p` | number | No | Nucleus sampling parameter |
| `top_k` | integer | No | Top-k sampling parameter |
| `stop_sequences` | array | No | Stop sequences |
| `stream` | boolean | No | Whether to stream responses |

#### Message Object

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `role` | string | Yes | Either `user` or `assistant` |
| `content` | string/array | Yes | Message content |

#### Example Request

```bash
curl -X POST http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "claude-3-sonnet-20240229",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "Hello, Claude! How are you today?"
      }
    ]
  }'
```

#### Response

```json
{
  "id": "msg_013Zva2CMHLNnXjNJJKqJ2EF",
  "type": "message",
  "role": "assistant",
  "content": [
    {
      "type": "text",
      "text": "Hello! I'm doing well, thank you for asking. How can I help you today?"
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

## Management Endpoints

### Health Check

Check the health of the LLM Router.

```http
GET /health
```

#### Response

```json
{
  "status": "healthy",
  "timestamp": 1677652288,
  "version": "1.0.0",
  "uptime": "2h15m30s"
}
```

### Provider Health

Check the health of a specific provider.

```http
GET /v1/health/{provider}
```

#### Parameters

- `provider`: Provider name (`openai`, `anthropic`)

#### Example

```bash
curl http://localhost:8080/v1/health/openai
```

#### Response

```json
{
  "provider": "openai",
  "status": "healthy",
  "last_check": 1677652288,
  "response_time_ms": 250,
  "error_rate": 0.02
}
```

### List Providers

Get information about all configured providers.

```http
GET /v1/providers
```

#### Response

```json
{
  "providers": [
    {
      "name": "openai",
      "status": "healthy",
      "enabled": true,
      "models": [
        {
          "name": "gpt-3.5-turbo",
          "enabled": true,
          "cost_per_token": 0.000002,
          "context_window": 16385
        },
        {
          "name": "gpt-4",
          "enabled": true,
          "cost_per_token": 0.00003,
          "context_window": 8192
        }
      ]
    },
    {
      "name": "anthropic",
      "status": "healthy",
      "enabled": true,
      "models": [
        {
          "name": "claude-3-sonnet-20240229",
          "enabled": true,
          "cost_per_token": 0.000015,
          "context_window": 200000
        }
      ]
    }
  ]
}
```

### Get Provider Details

Get detailed information about a specific provider.

```http
GET /v1/providers/{name}
```

#### Parameters

- `name`: Provider name

#### Example

```bash
curl http://localhost:8080/v1/providers/openai
```

### Provider Capabilities

Get the capabilities of all providers.

```http
GET /v1/capabilities
```

#### Response

```json
{
  "capabilities": {
    "openai": {
      "chat_completion": true,
      "text_completion": true,
      "function_calling": true,
      "vision": true,
      "streaming": true,
      "batch": true
    },
    "anthropic": {
      "chat_completion": true,
      "text_completion": false,
      "function_calling": false,
      "vision": false,
      "streaming": true,
      "batch": false
    }
  }
}
```

### Routing Decision

Get routing decision for a given request without executing it.

```http
POST /v1/routing/decision
```

#### Request Body

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `model` | string | Yes | Model to route |
| `strategy` | string | No | Routing strategy |
| `messages` | array | No | Messages for cost estimation |

#### Example Request

```bash
curl -X POST http://localhost:8080/v1/routing/decision \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-api-key" \
  -d '{
    "model": "gpt-3.5-turbo",
    "strategy": "cost_optimized",
    "messages": [
      {
        "role": "user",
        "content": "Hello, world!"
      }
    ]
  }'
```

#### Response

```json
{
  "provider": "openai",
  "model": "gpt-3.5-turbo",
  "strategy_used": "cost_optimized",
  "estimated_cost": 0.000024,
  "estimated_tokens": {
    "prompt": 10,
    "completion": 50,
    "total": 60
  },
  "reasoning": "Selected based on lowest cost per token",
  "alternatives": [
    {
      "provider": "openai",
      "model": "gpt-4",
      "estimated_cost": 0.0018,
      "reason": "Higher cost alternative"
    }
  ]
}
```

## Error Responses

All errors follow a consistent format:

```json
{
  "error": {
    "message": "Error description",
    "type": "error_type",
    "code": 400,
    "details": {
      "field": "Additional error details"
    }
  },
  "timestamp": 1677652288
}
```

### Error Types

| Type | Description |
|------|-------------|
| `authentication_error` | Invalid or missing credentials |
| `authorization_error` | Insufficient permissions |
| `validation_error` | Invalid request format or parameters |
| `rate_limit_error` | Rate limit exceeded |
| `provider_error` | Upstream provider error |
| `internal_error` | Internal server error |
| `not_found_error` | Resource not found |

### HTTP Status Codes

| Code | Description |
|------|-------------|
| `200` | Success |
| `400` | Bad Request |
| `401` | Unauthorized |
| `403` | Forbidden |
| `404` | Not Found |
| `429` | Too Many Requests |
| `500` | Internal Server Error |
| `502` | Bad Gateway |
| `503` | Service Unavailable |

### Example Error Responses

#### Authentication Error

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

#### Validation Error

```json
{
  "error": {
    "message": "Request validation failed",
    "type": "validation_error",
    "code": 400,
    "details": {
      "model": "Model is required",
      "messages": "Messages cannot be empty"
    }
  },
  "timestamp": 1677652288
}
```

#### Rate Limit Error

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

## Rate Limiting

### Rate Limit Headers

All responses include rate limit information:

```http
X-RateLimit-Limit: 60
X-RateLimit-Remaining: 59
X-RateLimit-Reset: 1677652348
```

### Rate Limit Information

| Header | Description |
|--------|-------------|
| `X-RateLimit-Limit` | Total requests allowed per window |
| `X-RateLimit-Remaining` | Requests remaining in current window |
| `X-RateLimit-Reset` | Unix timestamp when window resets |
| `Retry-After` | Seconds to wait when rate limited |

### Handling Rate Limits

When you receive a 429 status code:

1. Check the `Retry-After` header
2. Wait the specified number of seconds
3. Implement exponential backoff for retries
4. Consider reducing request rate

#### Example Rate Limit Handling (Python)

```python
import time
import requests

def make_request_with_backoff(url, headers, data, max_retries=3):
    for attempt in range(max_retries):
        response = requests.post(url, headers=headers, json=data)
        
        if response.status_code == 429:
            retry_after = int(response.headers.get('Retry-After', 60))
            time.sleep(retry_after)
            continue
        
        return response
    
    raise Exception("Max retries exceeded")
```