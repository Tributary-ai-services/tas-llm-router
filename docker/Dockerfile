# Build stage
#
# Gatekeeper's pkg/scan default match engine is Intel Hyperscan
# (github.com/flier/gohs, //go:build !nohs). This image builds that engine —
# CGO on, libhyperscan-dev in the builder, debian runtime — matching Gatekeeper's
# own service. Hyperscan is x86_64-only, so the target is pinned to linux/amd64.
FROM golang:1.24-bookworm AS builder

# libhyperscan-dev + pkg-config compile the Hyperscan bindings; git for the
# version stamp.
RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates tzdata \
        libhyperscan-dev pkg-config \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /build

# Copy local-replace dependencies.
# Gatekeeper transitively pulls aether-shared/go-events; both are
# replaced to local paths in tas-llm-router's go.mod, so both must
# exist under the build context.
COPY Gatekeeper/ /build/Gatekeeper/
COPY aether-shared/go-events/ /build/aether-shared/go-events/
# Shared traffic matcher (route rules + experiment cohorts). Same sibling-replace
# pattern; must land before `go mod download` so the replace target resolves.
COPY aether-shared/go-aiqg-matcher/ /build/aether-shared/go-aiqg-matcher/
# Shared routing-decision contract: health, budgets, fallback, constraints,
# affinity, selection and synthetic identification (routing steps 2-5). Same
# sibling-replace pattern.
#
# Adding a sibling `replace` to go.mod REQUIRES a COPY here. That coupling is
# invisible from go.mod, and it is how this Dockerfile went stale: the gateway
# has depended on this module since step 2 while the image build had no line
# for it, so the next image build would have failed on a missing replace target
# rather than on anything in the change that triggered it.
COPY aether-shared/go-aiqg-resilience/ /build/aether-shared/go-aiqg-resilience/

WORKDIR /build/app

# Copy go mod files and download dependencies
COPY tas-llm-router/go.mod tas-llm-router/go.sum ./
RUN go mod download

# Copy source code
COPY tas-llm-router/ .

# Build-time version stamp for AIQG events. The events package reads
# this via -ldflags "-X .../events.GatewayVersion=<value>" so emitted
# events carry a stable identifier (git short-SHA) instead of the
# baked-in "dev" default. Default kept as "dev" so unset builds still
# produce a meaningful value.
ARG GATEWAY_VERSION=dev

# Build with the default (Hyperscan) engine. CGO must be on for the gohs
# bindings; no `nohs` tag. -ldflags overrides pkg/aiqg/events.GatewayVersion at
# link time. The binary links libhs.so.5 + glibc dynamically, so the runtime is
# debian with libhyperscan5, not alpine.
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build \
    -ldflags "-w -s -X github.com/tributary-ai/llm-router-waf/pkg/aiqg/events.GatewayVersion=${GATEWAY_VERSION}" \
    -o llm-router ./cmd/llm-router/main.go

# Runtime stage
FROM debian:bookworm-slim

# libhyperscan5 is REQUIRED, not optional: the CGO binary links libhs.so.5
# dynamically, so without it the server fails at startup with
# "libhs.so.5: cannot open shared object file".
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata libhyperscan5 wget \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -g 1000 appuser && \
    useradd -u 1000 -g appuser -s /bin/sh -M appuser

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

CMD ["./llm-router", "--config", "configs/config.yaml"]
