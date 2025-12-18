# Build stage
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o llm-router ./cmd/llm-router/main.go

# Runtime stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata wget

# Create non-root user
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/llm-router .

# Copy config files if they exist
COPY --from=builder /app/configs /app/configs

# Set ownership and permissions
RUN chown -R appuser:appuser /app && \
    chmod +x /app/llm-router

# Switch to non-root user
USER appuser

EXPOSE 8086

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8086/health || exit 1

CMD ["./llm-router"]
