# Day 1 Test Plan - LLM Router WAF

This test plan validates all Day 1 implementation tasks to ensure the LLM Router WAF system is working correctly.

## Test Environment Setup

### Prerequisites
- Go 1.23+ installed
- Internet connection for dependency downloads
- Test API keys (optional for most tests)

### Environment Variables for Full Testing
```bash
export OPENAI_API_KEY="sk-test-key"  # Use real key for API tests
export ANTHROPIC_API_KEY="sk-ant-test-key"  # Use real key for API tests
export LLM_ROUTER_LOG_LEVEL="debug"
```

## Task 1: Project Structure & Dependencies

### Test 1.1: Go Module Structure
```bash
# Verify go.mod exists and is valid
go mod verify
go mod tidy
```

**Expected Result**: ✅ No errors, dependencies resolved

### Test 1.2: Build System
```bash
# Build the application
go build -o llm-router cmd/llm-router/main.go
```

**Expected Result**: ✅ Binary created successfully without errors

### Test 1.3: Dependency Check
```bash
# Check all dependencies are available
go list -m all
```

**Expected Result**: ✅ All required dependencies listed:
- github.com/anthropics/anthropic-sdk-go
- github.com/sashabaranov/go-openai
- github.com/gorilla/mux
- github.com/sirupsen/logrus
- gopkg.in/yaml.v3

### Test 1.4: Package Structure
```bash
# Verify directory structure exists
ls -la internal/
ls -la cmd/
ls -la configs/
```

**Expected Result**: ✅ All required directories present:
- `internal/config/`
- `internal/providers/`
- `internal/routing/`
- `internal/server/`
- `internal/types/`
- `cmd/llm-router/`
- `configs/`

## Task 2: Core Interfaces & Types

### Test 2.1: Interface Compilation
```bash
# Test that all interfaces compile
go build ./internal/providers
go build ./internal/types
```

**Expected Result**: ✅ No compilation errors

### Test 2.2: Type System Validation
```bash
# Run type-focused tests
go test ./internal/integration -run TestConfigurationLoading -v
```

**Expected Result**: ✅ Configuration loads with proper type validation

### Test 2.3: Interface Compatibility
```bash
# Check provider interface implementations compile
go build ./internal/providers/openai
go build ./internal/providers/anthropic
```

**Expected Result**: ✅ Provider implementations satisfy interfaces

## Task 3: OpenAI Provider Implementation

### Test 3.1: Provider Registration
```bash
# Test OpenAI provider can be instantiated
go test ./internal/integration -run TestRouterIntegration -v
```

**Expected Result**: ✅ OpenAI provider registered successfully

### Test 3.2: Cost Estimation
```bash
# Test cost calculation
go test ./internal/integration -run TestCostEstimation -v
```

**Expected Result**: ✅ Cost estimation returns positive values

### Test 3.3: Capabilities Check
```bash
# Verify provider capabilities
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
curl -s http://localhost:8080/v1/capabilities | jq '.capabilities.openai'
kill $PID
```

**Expected Result**: ✅ OpenAI capabilities returned with:
- `supports_functions: true`
- `supports_vision: true`  
- `supports_streaming: true`
- Model list with cost information

## Task 4: Anthropic Provider Implementation

### Test 4.1: Provider Registration
```bash
# Check Anthropic provider in capabilities
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
curl -s http://localhost:8080/v1/capabilities | jq '.capabilities.anthropic'
kill $PID
```

**Expected Result**: ✅ Anthropic capabilities returned with:
- `supports_functions: true` (tool use)
- `supports_vision: true`
- Claude models listed

### Test 4.2: Provider Health
```bash
# Test provider health endpoint
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
curl -s http://localhost:8080/v1/health/anthropic
kill $PID
```

**Expected Result**: ✅ Health status returned (may be "unknown" without real API key)

## Task 5: Routing Engine Implementation

### Test 5.1: Basic Routing
```bash
# Test routing decision endpoint
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
curl -X POST http://localhost:8080/v1/routing/decision \
  -H "Content-Type: application/json" \
  -d '{"model": "gpt-3.5-turbo", "messages": [{"role": "user", "content": "test"}], "optimize_for": "cost"}'
kill $PID
```

**Expected Result**: ✅ Routing decision returned with:
- Selected provider
- Reasoning array
- Cost estimate
- Feature compatibility

### Test 5.2: Strategy Testing
```bash
# Test different routing strategies
for strategy in cost performance round_robin; do
  echo "Testing $strategy strategy..."
  # Test logic here
done
```

**Expected Result**: ✅ Different providers selected based on strategy

### Test 5.3: Provider Health Monitoring
```bash
# Test health monitoring
go test ./internal/integration -run BenchmarkRouting
```

**Expected Result**: ✅ Routing performance acceptable (< 10ms per route)

## Task 6: HTTP Server & Handlers

### Test 6.1: Server Startup
```bash
# Test server starts successfully
timeout 5s ./llm-router --config configs/config.yaml
```

**Expected Result**: ✅ Server starts, logs show:
- Providers registered
- HTTP server starting on port 8080

### Test 6.2: Health Endpoints
```bash
./llm-router --config configs/config.yaml &
PID=$!
sleep 2

# Test health endpoints
curl -f http://localhost:8080/health
curl -f http://localhost:8080/v1/health
curl -f http://localhost:8080/v1/providers

kill $PID
```

**Expected Result**: ✅ All health endpoints return 200 OK

### Test 6.3: CORS Headers
```bash
./llm-router --config configs/config.yaml &
PID=$!
sleep 2

curl -I -X OPTIONS http://localhost:8080/v1/chat/completions \
  -H "Origin: https://example.com" \
  -H "Access-Control-Request-Method: POST"

kill $PID
```

**Expected Result**: ✅ CORS headers present in response

### Test 6.4: Error Handling
```bash
./llm-router --config configs/config.yaml &
PID=$!
sleep 2

# Test malformed JSON
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"invalid": json}'

# Test missing content-type
curl -X POST http://localhost:8080/v1/chat/completions \
  -d '{"model": "test"}'

kill $PID
```

**Expected Result**: ✅ Proper error responses with 400 status codes

## Task 7: Configuration Management

### Test 7.1: Configuration Loading
```bash
# Test default configuration
OPENAI_API_KEY=test-key ANTHROPIC_API_KEY=test-key2 ./llm-router &
PID=$!
sleep 2
kill $PID
```

**Expected Result**: ✅ Loads with default configuration

### Test 7.2: File Configuration
```bash
# Test file-based configuration
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
kill $PID
```

**Expected Result**: ✅ Loads configuration from file

### Test 7.3: Environment Override
```bash
# Test environment variable override
LLM_ROUTER_PORT=9090 ./llm-router --config configs/config.yaml &
PID=$!
sleep 2
netstat -an | grep 9090 || ss -tulpn | grep 9090
kill $PID
```

**Expected Result**: ✅ Server starts on port 9090 (overridden)

### Test 7.4: Configuration Validation
```bash
# Test invalid configuration
echo "invalid: yaml: content" > /tmp/invalid.yaml
./llm-router --config /tmp/invalid.yaml
```

**Expected Result**: ✅ Error message about invalid configuration

## Task 8: Main Application & Testing

### Test 8.1: Command Line Interface
```bash
# Test help output
./llm-router --help

# Test version output  
./llm-router --version

# Test invalid flag
./llm-router --invalid-flag
```

**Expected Result**: ✅ 
- Help shows all options and examples
- Version shows version info
- Invalid flag shows usage help

### Test 8.2: Graceful Shutdown
```bash
# Test graceful shutdown
./llm-router --config configs/config.yaml &
PID=$!
sleep 2
kill -TERM $PID
wait $PID
```

**Expected Result**: ✅ Server shuts down gracefully with cleanup message

### Test 8.3: Integration Tests
```bash
# Run all integration tests
go test ./internal/integration -v
```

**Expected Result**: ✅ All integration tests pass:
- TestRouterIntegration
- TestConfigurationLoading  
- TestCostEstimation

### Test 8.4: Performance Benchmarks
```bash
# Run benchmark tests
go test ./internal/integration -bench=. -benchmem
```

**Expected Result**: ✅ Routing performance benchmarks complete

## End-to-End Workflow Tests

### E2E Test 1: Complete Chat Completion Flow
```bash
./llm-router --config configs/config.yaml &
PID=$!
sleep 2

# Test chat completion endpoint (will fail without real API key)
curl -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [{"role": "user", "content": "Hello"}],
    "optimize_for": "cost",
    "max_tokens": 50
  }'

kill $PID
```

**Expected Result**: ✅ Request routed successfully (routing metadata returned)

### E2E Test 2: Provider Fallback
```bash
# Test with provider that should fail health check
# Implementation depends on configuration
```

**Expected Result**: ✅ Routes to healthy provider when primary fails

### E2E Test 3: Cost Optimization
```bash
./llm-router --config configs/config.yaml &
PID=$!
sleep 2

# Test cost-optimized routing decision
curl -X POST http://localhost:8080/v1/routing/decision \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-3.5-turbo",
    "messages": [{"role": "user", "content": "test message"}],
    "optimize_for": "cost"
  }' | jq '.estimated_cost'

kill $PID
```

**Expected Result**: ✅ Lowest cost provider selected with cost reasoning

## Available Go Test Files

The following comprehensive Go test files have been created:

### Provider Tests
- **`internal/providers/openai/provider_test.go`**: Tests for OpenAI provider
  - Provider name and capabilities
  - Cost estimation
  - Request conversion
  - Interface implementations
  - Benchmarks

- **`internal/providers/anthropic/provider_test.go`**: Tests for Anthropic provider  
  - Provider name and capabilities
  - Cost estimation with Claude-specific features
  - Request conversion (including system messages)
  - Token estimation
  - Interface implementations
  - Benchmarks

### Routing Tests
- **`internal/routing/router_test.go`**: Tests for routing engine
  - Provider registration
  - Multiple routing strategies (cost, performance, round-robin)
  - Feature filtering and compatibility
  - Health monitoring
  - Routing context building
  - Benchmarks

### Configuration Tests
- **`internal/config/config_test.go`**: Tests for configuration system
  - Default configuration loading
  - Environment variable overrides
  - File-based configuration
  - Validation testing
  - Provider enablement logic
  - Configuration file I/O
  - Benchmarks

### Integration Tests
- **`internal/integration/integration_test.go`**: End-to-end integration tests
  - Router integration with providers
  - Configuration loading with API keys
  - Cost estimation workflows
  - Performance benchmarks

## Test Execution Scripts

### Quick Test Runner
```bash
# Run all tests
go test ./... -v

# Run tests with coverage
go test ./... -cover

# Run benchmarks
go test ./... -bench=. -benchmem
```

### Comprehensive Test Script
```bash
# Use the included test runner
./test_day1.sh

# Or with options
./test_day1.sh --help
./test_day1.sh --verbose --coverage
```

## Success Criteria

All tests should pass with the following outcomes:

✅ **Task 1**: Project builds without errors, all dependencies resolved  
✅ **Task 2**: Types and interfaces compile correctly  
✅ **Task 3**: OpenAI provider implements all interfaces  
✅ **Task 4**: Anthropic provider implements all interfaces  
✅ **Task 5**: Routing engine makes intelligent decisions  
✅ **Task 6**: HTTP server handles requests properly  
✅ **Task 7**: Configuration system works with files and env vars  
✅ **Task 8**: Main application and tests function correctly  

## Failure Investigation

If any test fails:

1. **Check Dependencies**: `go mod download && go mod verify`
2. **Check Syntax**: `go vet ./...`
3. **Check Imports**: `go mod tidy`
4. **Check Types**: Look for interface implementation errors
5. **Check Config**: Verify YAML syntax in `configs/config.yaml`
6. **Check Ports**: Ensure test ports are available
7. **Check Logs**: Use `LLM_ROUTER_LOG_LEVEL=debug` for detailed logging

## Performance Baselines

Expected performance targets:
- **Build Time**: < 30 seconds
- **Startup Time**: < 3 seconds  
- **Routing Decision**: < 10ms
- **Memory Usage**: < 50MB at startup
- **HTTP Response Time**: < 100ms for health checks