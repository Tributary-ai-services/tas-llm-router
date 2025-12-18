#!/bin/bash
set -e

# Start TAS LLM Router with Aether Shared Infrastructure Integration
# This script manages the LLM Router when using shared infrastructure services

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" &> /dev/null && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"
SHARED_DIR="../../aether-shared"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

# Check if aether-shared directory exists
if [ ! -d "$SHARED_DIR" ]; then
    log_error "Aether-shared directory not found at $SHARED_DIR"
    log_info "Make sure aether-shared is cloned and accessible"
    exit 1
fi

# Check if shared infrastructure is running
check_shared_infrastructure() {
    log_info "Checking if shared infrastructure is running..."
    
    if ! docker network ls | grep -q "tas-shared-network"; then
        log_error "Shared network 'tas-shared-network' not found"
        log_info "Please start shared infrastructure first:"
        log_info "  cd $SHARED_DIR && docker-compose -f docker-compose.shared-infrastructure.yml up -d"
        exit 1
    fi

    # Check if key shared services are running
    local services=("tas-redis-shared" "tas-postgres-shared" "tas-prometheus-shared")
    for service in "${services[@]}"; do
        if ! docker ps --format "table {{.Names}}" | grep -q "$service"; then
            log_warning "Shared service '$service' not running"
            log_info "Starting shared infrastructure..."
            (cd "$SHARED_DIR" && docker-compose -f docker-compose.shared-infrastructure.yml up -d)
            break
        fi
    done

    log_success "Shared infrastructure is available"
}

# Initialize database schema if needed
init_database() {
    log_info "Initializing LLM Router database schema..."
    
    # Run database initialization
    docker-compose -f "$SCRIPT_DIR/docker-compose.aether-shared.yml" --profile init up llm-router-db-init
    local exit_code=$?
    
    if [ $exit_code -eq 0 ]; then
        log_success "Database schema initialized"
    else
        log_warning "Database initialization may have failed (exit code: $exit_code)"
        log_info "This might be normal if schema already exists"
    fi
    
    # Clean up the init container
    docker-compose -f "$SCRIPT_DIR/docker-compose.aether-shared.yml" --profile init rm -f llm-router-db-init
}

# Start LLM Router services
start_services() {
    log_info "Starting LLM Router with shared infrastructure integration..."
    
    cd "$SCRIPT_DIR"
    docker-compose -f docker-compose.aether-shared.yml up -d llm-router-aether
    
    if [ $? -eq 0 ]; then
        log_success "LLM Router started successfully"
        log_info "Services available at:"
        log_info "  - LLM Router API: http://localhost:8086"
        log_info "  - Shared Grafana: http://localhost:3000"
        log_info "  - Shared Prometheus: http://localhost:9090"
        log_info "  - Metrics Adapter (debug): http://localhost:9092"
    else
        log_error "Failed to start LLM Router services"
        exit 1
    fi
}

# Show service status
show_status() {
    log_info "Service Status:"
    docker-compose -f "$SCRIPT_DIR/docker-compose.aether-shared.yml" ps
    
    echo ""
    log_info "Shared Infrastructure Status:"
    docker ps --format "table {{.Names}}\t{{.Status}}\t{{.Ports}}" --filter "name=tas-"
}

# Check required environment variables
check_environment() {
    log_info "Checking required environment variables..."
    
    local missing_vars=()
    
    if [[ -z "${OPENAI_API_KEY:-}" ]]; then
        missing_vars+=("OPENAI_API_KEY")
    fi
    
    if [[ -z "${ANTHROPIC_API_KEY:-}" ]]; then
        missing_vars+=("ANTHROPIC_API_KEY")
    fi
    
    if [[ ${#missing_vars[@]} -gt 0 ]]; then
        log_error "Missing required environment variables:"
        for var in "${missing_vars[@]}"; do
            log_error "  - $var"
        done
        log_info "Please set these variables before starting:"
        log_info "  export OPENAI_API_KEY=\"sk-your-openai-key\""
        log_info "  export ANTHROPIC_API_KEY=\"sk-ant-your-anthropic-key\""
        exit 1
    fi
    
    log_success "All required environment variables are set"
}

# Main execution
main() {
    case "${1:-start}" in
        "start")
            check_environment
            check_shared_infrastructure
            init_database
            start_services
            show_status
            ;;
        "stop")
            log_info "Stopping LLM Router services..."
            cd "$SCRIPT_DIR"
            docker-compose -f docker-compose.aether-shared.yml down
            log_success "LLM Router services stopped"
            ;;
        "restart")
            log_info "Restarting LLM Router services..."
            cd "$SCRIPT_DIR"
            docker-compose -f docker-compose.aether-shared.yml down
            check_environment
            check_shared_infrastructure
            start_services
            show_status
            ;;
        "status")
            show_status
            ;;
        "logs")
            cd "$SCRIPT_DIR"
            docker-compose -f docker-compose.aether-shared.yml logs -f "${2:-llm-router-aether}"
            ;;
        "init-db")
            check_shared_infrastructure
            init_database
            ;;
        "help"|"-h"|"--help")
            echo "Usage: $0 [COMMAND]"
            echo ""
            echo "Commands:"
            echo "  start     Start LLM Router with shared infrastructure (default)"
            echo "  stop      Stop LLM Router services"
            echo "  restart   Restart LLM Router services"  
            echo "  status    Show service status"
            echo "  logs      Show logs (optional: specify service name)"
            echo "  init-db   Initialize database schema only"
            echo "  help      Show this help message"
            echo ""
            echo "Environment Setup:"
            echo "  1. Set required API keys:"
            echo "     export OPENAI_API_KEY=\"sk-your-openai-key\""
            echo "     export ANTHROPIC_API_KEY=\"sk-ant-your-anthropic-key\""
            echo ""
            echo "  2. Or copy and customize environment file:"
            echo "     cp .env.aether-shared.example .env.aether-shared"
            echo "     source .env.aether-shared"
            echo ""
            echo "Examples:"
            echo "  $0 start"
            echo "  $0 logs llm-router-aether"
            echo "  $0 status"
            ;;
        *)
            log_error "Unknown command: $1"
            log_info "Use '$0 help' for usage information"
            exit 1
            ;;
    esac
}

main "$@"