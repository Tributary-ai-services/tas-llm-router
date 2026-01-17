#!/bin/bash
# Day 1 Go Test Suite Runner for LLM Router WAF

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test counters
TESTS_PASSED=0
TESTS_FAILED=0

# Helper functions
log_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

log_success() {
    echo -e "${GREEN}✅ $1${NC}"
    ((TESTS_PASSED++))
}

log_error() {
    echo -e "${RED}❌ $1${NC}"
    ((TESTS_FAILED++))
}

log_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

run_go_test() {
    local test_name="$1"
    local package="$2"
    local args="$3"
    
    echo -e "\n${BLUE}🧪 Running Go tests: $test_name${NC}"
    if go test $package $args -v; then
        log_success "$test_name passed"
        return 0
    else
        log_error "$test_name failed"
        return 1
    fi
}

run_build_test() {
    local test_name="$1"
    local package="$2"
    
    echo -e "\n${BLUE}🏗️  Building: $test_name${NC}"
    if go build $package > /dev/null 2>&1; then
        log_success "$test_name build passed"
        return 0
    else
        log_error "$test_name build failed"
        return 1
    fi
}

run_benchmark() {
    local test_name="$1"
    local package="$2"
    
    echo -e "\n${BLUE}⚡ Running benchmark: $test_name${NC}"
    if go test $package -bench=. -benchmem -benchtime=1s; then
        log_success "$test_name benchmark completed"
        return 0
    else
        log_error "$test_name benchmark failed"
        return 1
    fi
}

# Main test execution
main() {
    echo -e "${BLUE}"
    echo "╔══════════════════════════════════════╗"
    echo "║    Day 1 Go Test Suite - LLM Router  ║"
    echo "║           WAF Implementation         ║"
    echo "╚══════════════════════════════════════╝"
    echo -e "${NC}\n"

    # Set test environment
    export OPENAI_API_KEY="test-openai-key"
    export ANTHROPIC_API_KEY="test-anthropic-key"
    export LLM_ROUTER_LOG_LEVEL="error"  # Reduce noise during tests

    # Task 1: Project Structure & Dependencies
    echo -e "${YELLOW}📦 Task 1: Testing Project Structure & Dependencies${NC}"
    
    if go mod verify > /dev/null 2>&1; then
        log_success "Go module verification passed"
    else
        log_error "Go module verification failed"
    fi
    
    if go mod tidy > /dev/null 2>&1; then
        log_success "Go module tidy passed"
    else
        log_error "Go module tidy failed"
    fi
    
    if go build -o llm-router cmd/llm-router/main.go > /dev/null 2>&1; then
        log_success "Main binary build passed"
        rm -f llm-router
    else
        log_error "Main binary build failed"
    fi

    # Task 2: Core Interfaces & Types
    echo -e "\n${YELLOW}🏗️  Task 2: Testing Core Interfaces & Types${NC}"
    
    run_build_test "Provider interfaces" "./internal/providers"
    run_build_test "Types system" "./internal/types"
    run_build_test "Routing system" "./internal/routing"
    run_build_test "Server system" "./internal/server"
    run_build_test "Config system" "./internal/config"

    # Task 3 & 4: Provider Implementations
    echo -e "\n${YELLOW}🤖 Task 3 & 4: Testing Provider Implementations${NC}"
    
    run_build_test "OpenAI provider" "./internal/providers/openai"
    run_build_test "Anthropic provider" "./internal/providers/anthropic"
    
    # Run provider-specific tests
    run_go_test "OpenAI provider tests" "./internal/providers/openai" ""
    run_go_test "Anthropic provider tests" "./internal/providers/anthropic" ""

    # Task 5: Routing Engine
    echo -e "\n${YELLOW}🔀 Task 5: Testing Routing Engine${NC}"
    
    run_go_test "Router tests" "./internal/routing" ""

    # Task 6: HTTP Server (build tests only)
    echo -e "\n${YELLOW}🌐 Task 6: Testing HTTP Server${NC}"
    
    run_build_test "HTTP server" "./internal/server"

    # Task 7: Configuration Management
    echo -e "\n${YELLOW}⚙️  Task 7: Testing Configuration Management${NC}"
    
    run_go_test "Configuration tests" "./internal/config" ""

    # Task 8: Integration Testing
    echo -e "\n${YELLOW}🚀 Task 8: Testing Integration${NC}"
    
    run_go_test "Integration tests" "./internal/integration" ""

    # Additional comprehensive tests
    echo -e "\n${YELLOW}🔍 Comprehensive Testing${NC}"
    
    # Test all packages
    echo -e "\n${BLUE}🧪 Running all Go tests${NC}"
    if go test ./... -v; then
        log_success "All Go tests passed"
    else
        log_error "Some Go tests failed"
    fi
    
    # Run race detection tests
    echo -e "\n${BLUE}🏃 Running race detection tests${NC}"
    if go test ./... -race -short; then
        log_success "Race detection tests passed"
    else
        log_error "Race detection tests failed"
    fi
    
    # Run vet
    echo -e "\n${BLUE}🔬 Running go vet${NC}"
    if go vet ./...; then
        log_success "Go vet passed"
    else
        log_error "Go vet failed"
    fi

    # Performance Benchmarks
    echo -e "\n${YELLOW}⚡ Performance Benchmarks${NC}"
    
    run_benchmark "OpenAI provider benchmarks" "./internal/providers/openai"
    run_benchmark "Anthropic provider benchmarks" "./internal/providers/anthropic"
    run_benchmark "Router benchmarks" "./internal/routing"
    run_benchmark "Configuration benchmarks" "./internal/config"
    run_benchmark "Integration benchmarks" "./internal/integration"

    # Coverage report
    echo -e "\n${YELLOW}📊 Test Coverage${NC}"
    
    echo -e "\n${BLUE}🧪 Generating coverage report${NC}"
    if go test ./... -coverprofile=coverage.out > /dev/null 2>&1; then
        coverage=$(go tool cover -func=coverage.out | tail -1 | awk '{print $3}')
        echo -e "${GREEN}Total coverage: $coverage${NC}"
        log_success "Coverage report generated"
        rm -f coverage.out
    else
        log_error "Coverage report generation failed"
    fi

    # Final summary
    echo -e "\n${BLUE}╔══════════════════════════════════════╗"
    echo -e "║             Test Summary             ║"
    echo -e "╚══════════════════════════════════════╝${NC}\n"

    echo -e "Tests Passed: ${GREEN}$TESTS_PASSED${NC}"
    echo -e "Tests Failed: ${RED}$TESTS_FAILED${NC}"
    
    if [ $TESTS_FAILED -eq 0 ]; then
        echo -e "\n${GREEN}🎉 All Day 1 Go tests passed! LLM Router WAF implementation is ready.${NC}"
        exit 0
    else
        echo -e "\n${RED}💥 Some tests failed. Please review the errors above.${NC}"
        exit 1
    fi
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."
    
    # Check Go installation
    if ! command -v go &> /dev/null; then
        log_error "Go is not installed or not in PATH"
        exit 1
    fi
    
    # Check Go version
    GO_VERSION=$(go version | cut -d' ' -f3 | sed 's/go//')
    log_info "Go version: $GO_VERSION"
    
    # Check if we're in the right directory
    if [ ! -f "go.mod" ] || [ ! -d "internal" ] || [ ! -d "cmd" ]; then
        log_error "Please run this script from the project root directory"
        exit 1
    fi
    
    log_success "Prerequisites check passed"
}

# Show help
show_help() {
    echo "Usage: $0 [options]"
    echo ""
    echo "Options:"
    echo "  -h, --help     Show this help message"
    echo "  -v, --verbose  Enable verbose output"
    echo "  -c, --coverage Show detailed coverage report"
    echo ""
    echo "This script runs the complete Day 1 test suite for the LLM Router WAF."
    echo "It includes unit tests, integration tests, benchmarks, and code quality checks."
}

# Parse command line arguments
VERBOSE=false
COVERAGE=false

while [[ $# -gt 0 ]]; do
    case $1 in
        -h|--help)
            show_help
            exit 0
            ;;
        -v|--verbose)
            VERBOSE=true
            shift
            ;;
        -c|--coverage)
            COVERAGE=true
            shift
            ;;
        *)
            echo "Unknown option: $1"
            show_help
            exit 1
            ;;
    esac
done

# Run the tests
echo -e "${BLUE}Starting prerequisite checks...${NC}"
check_prerequisites

echo -e "\n${BLUE}Starting main test suite...${NC}"
main