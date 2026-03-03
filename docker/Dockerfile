# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /build

# Copy Gatekeeper dependency (local replace directive)
COPY Gatekeeper/ /build/Gatekeeper/

WORKDIR /build/app

# Copy go mod files and download dependencies
COPY tas-llm-router/go.mod tas-llm-router/go.sum ./
RUN go mod download

# Copy source code
COPY tas-llm-router/ .

# Build with nohs tag (regexp engine, no CGO/Hyperscan needed)
RUN CGO_ENABLED=0 GOOS=linux go build -tags nohs -a -installsuffix cgo -o llm-router ./cmd/llm-router/main.go

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata wget

# Create non-root user
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /build/app/llm-router .

# Copy config files if they exist
COPY --from=builder /build/app/configs /app/configs

# Set ownership and permissions
RUN chown -R appuser:appuser /app && \
    chmod +x /app/llm-router

# Switch to non-root user
USER appuser

EXPOSE 8086

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8086/health || exit 1

CMD ["./llm-router"]
