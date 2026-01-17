# LLM Router WAF - Development Makefile

# Variables
BINARY_NAME=llm-router
DOCKER_IMAGE=llm-router-waf
VERSION?=v1.0.0
BUILD_TIME=$(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
GIT_COMMIT=$(shell git rev-parse --short HEAD)

# Go build flags
LDFLAGS=-ldflags "-X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME} -X main.GitCommit=${GIT_COMMIT}"

# Default target
.DEFAULT_GOAL := help

##@ Day 1 Tasks

.PHONY: build
build: ## Build the main binary
	go build ${LDFLAGS} -o ${BINARY_NAME} cmd/llm-router/main.go

.PHONY: test-day1
test-day1: ## Run Day 1 test suite
	./test_day1.sh

.PHONY: clean
clean: ## Clean build artifacts
	rm -f ${BINARY_NAME}
	go clean ./...

##@ Day 2 Development

.PHONY: dev-setup-day2
dev-setup-day2: ## Setup Day 2 development environment
	@echo "🚀 Setting up Day 2 development environment..."
	@echo "📦 Installing additional dependencies..."
	go get github.com/prometheus/client_golang
	go get go.opentelemetry.io/otel
	go get github.com/redis/go-redis/v9
	go get github.com/hashicorp/vault/api
	go get github.com/stretchr/testify/assert
	go get github.com/stretchr/testify/mock
	go mod tidy
	@echo "🐳 Starting development services with Docker Compose..."
	docker-compose -f docker/docker-compose.dev.yml up -d
	@echo "✅ Day 2 development environment ready!"

.PHONY: dev-services
dev-services: ## Start development services (Redis, Prometheus, etc.)
	docker-compose -f docker/docker-compose.dev.yml up -d

.PHONY: dev-services-stop
dev-services-stop: ## Stop development services
	docker-compose -f docker/docker-compose.dev.yml down

.PHONY: test-day2
test-day2: ## Run Day 2 test suite
	@echo "🧪 Running Day 2 tests..."
	@echo "🔒 Testing security components..."
	go test ./internal/security/... -v
	@echo "📊 Testing observability components..."  
	go test ./internal/observability/... -v
	@echo "⚡ Testing performance components..."
	go test ./internal/cache/... ./internal/performance/... -v
	@echo "📈 Testing analytics components..."
	go test ./internal/analytics/... -v
	@echo "🔄 Testing advanced routing..."
	go test ./internal/routing/... -v
	@echo "✅ All Day 2 tests completed!"

.PHONY: test-integration
test-integration: ## Run integration tests with real APIs
	@echo "🌐 Running integration tests..."
	go test ./test/integration/... -v -timeout=300s

.PHONY: test-load
test-load: ## Run load tests
	@echo "⚡ Running load tests..."
	go test ./test/load/... -v -timeout=600s

.PHONY: test-chaos
test-chaos: ## Run chaos engineering tests
	@echo "💥 Running chaos tests..."
	go test ./test/chaos/... -v -timeout=300s

.PHONY: test-security
test-security: ## Run security tests
	@echo "🔒 Running security tests..."
	go test ./test/security/... -v

##@ Testing & Quality

.PHONY: test
test: ## Run all tests
	go test ./... -v

.PHONY: test-coverage
test-coverage: ## Generate test coverage report
	go test ./... -coverprofile=coverage.out
	go tool cover -html=coverage.out -o coverage.html
	@echo "📊 Coverage report generated: coverage.html"

.PHONY: test-race
test-race: ## Run tests with race detection
	go test ./... -race -short

.PHONY: benchmark
benchmark: ## Run benchmark tests
	go test ./... -bench=. -benchmem -benchtime=5s

.PHONY: lint
lint: ## Run linters
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "📝 golangci-lint not found, running go vet instead"; \
		go vet ./...; \
	fi

.PHONY: fmt
fmt: ## Format code
	go fmt ./...
	goimports -w .

##@ Docker & Containerization

.PHONY: docker-build
docker-build: ## Build Docker image
	docker build -f docker/Dockerfile -t ${DOCKER_IMAGE}:${VERSION} .
	docker tag ${DOCKER_IMAGE}:${VERSION} ${DOCKER_IMAGE}:latest

.PHONY: docker-run
docker-run: docker-build ## Run Docker container
	docker run -p 8080:8080 --env-file .env ${DOCKER_IMAGE}:latest

.PHONY: docker-push
docker-push: docker-build ## Push Docker image to registry
	docker push ${DOCKER_IMAGE}:${VERSION}
	docker push ${DOCKER_IMAGE}:latest

##@ Kubernetes & Deployment

.PHONY: k8s-deploy
k8s-deploy: ## Deploy to Kubernetes
	@echo "🚀 Deploying to Kubernetes..."
	kubectl apply -f k8s/namespace.yaml
	kubectl apply -f k8s/configmap.yaml
	kubectl apply -f k8s/secret.yaml
	kubectl apply -f k8s/deployment.yaml
	kubectl apply -f k8s/service.yaml
	kubectl apply -f k8s/ingress.yaml

.PHONY: k8s-delete
k8s-delete: ## Delete Kubernetes resources
	kubectl delete -f k8s/

.PHONY: helm-install
helm-install: ## Install via Helm
	helm install llm-router helm/llm-router/ --values helm/llm-router/values.yaml

.PHONY: helm-upgrade
helm-upgrade: ## Upgrade Helm deployment
	helm upgrade llm-router helm/llm-router/ --values helm/llm-router/values.yaml

.PHONY: helm-uninstall
helm-uninstall: ## Uninstall Helm deployment
	helm uninstall llm-router

##@ Monitoring & Analytics

.PHONY: metrics
metrics: ## View Prometheus metrics
	@echo "📊 Opening metrics endpoint..."
	@curl -s http://localhost:8080/metrics | head -20

.PHONY: health
health: ## Check system health
	@echo "🏥 Checking system health..."
	@curl -s http://localhost:8080/health | jq '.'

.PHONY: analytics-report
analytics-report: ## Generate analytics report
	@echo "📈 Generating analytics report..."
	go run cmd/analytics/main.go --report --output=analytics-report.json
	@echo "📋 Report saved to: analytics-report.json"

.PHONY: dashboard
dashboard: ## Open Grafana dashboard
	@echo "📊 Opening Grafana dashboard..."
	@echo "URL: http://localhost:3000 (admin/admin)"

##@ Development Utilities

.PHONY: dev-run
dev-run: ## Run in development mode with hot reload
	@echo "🔥 Starting development server with hot reload..."
	air -c .air.toml

.PHONY: dev-config
dev-config: ## Generate development configuration
	@echo "⚙️  Generating development configuration..."
	cp configs/config.example.yaml configs/config.dev.yaml
	@echo "✅ Edit configs/config.dev.yaml for development"

.PHONY: generate-docs
generate-docs: ## Generate API documentation
	@echo "📚 Generating API documentation..."
	@if command -v swag >/dev/null 2>&1; then \
		swag init -g cmd/llm-router/main.go; \
	else \
		echo "⚠️  swag not found, install with: go install github.com/swaggo/swag/cmd/swag@latest"; \
	fi

.PHONY: migrate-data
migrate-data: ## Run data migrations
	@echo "🗄️  Running data migrations..."
	go run cmd/migrate/main.go

.PHONY: seed-data
seed-data: ## Seed development data
	@echo "🌱 Seeding development data..."
	go run cmd/seed/main.go

##@ Environment Management

.PHONY: env-dev
env-dev: ## Set up development environment
	cp .env.example .env.dev
	@echo "✅ Edit .env.dev with your development settings"

.PHONY: env-staging
env-staging: ## Deploy to staging
	@echo "🚀 Deploying to staging environment..."
	kubectl apply -f k8s/staging/ --recursive

.PHONY: env-prod
env-prod: ## Deploy to production
	@echo "⚠️  Deploying to production - are you sure? [y/N]"
	@read confirm && [ "$$confirm" = "y" ] || exit 1
	@echo "🚀 Deploying to production environment..."
	kubectl apply -f k8s/production/ --recursive

##@ Maintenance & Troubleshooting

.PHONY: logs
logs: ## View application logs
	kubectl logs -f deployment/llm-router --tail=100

.PHONY: logs-dev
logs-dev: ## View local development logs
	docker-compose -f docker/docker-compose.dev.yml logs -f llm-router

.PHONY: debug
debug: ## Run with debugger
	dlv debug cmd/llm-router/main.go

.PHONY: profile-cpu
profile-cpu: ## Profile CPU usage
	go tool pprof http://localhost:8080/debug/pprof/profile?seconds=30

.PHONY: profile-memory
profile-memory: ## Profile memory usage
	go tool pprof http://localhost:8080/debug/pprof/heap

.PHONY: backup-config
backup-config: ## Backup configuration
	@mkdir -p backups
	@cp -r configs backups/configs-$(shell date +%Y%m%d-%H%M%S)
	@echo "✅ Configuration backed up to backups/"

##@ Help

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: version
version: ## Show version information
	@echo "LLM Router WAF"
	@echo "Version: ${VERSION}"
	@echo "Build Time: ${BUILD_TIME}"  
	@echo "Git Commit: ${GIT_COMMIT}"

# Include environment-specific makefiles if they exist
-include Makefile.local