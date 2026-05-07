#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PARENT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REGISTRY="registry-api.tas.scharber.com"
IMAGE="${REGISTRY}/tas-llm-router"

echo "=== Building tas-llm-router ==="
echo "Context: ${PARENT_DIR} (so Dockerfile can COPY Gatekeeper/ and aether-shared/go-events/)"

docker build \
    -f "${SCRIPT_DIR}/docker/Dockerfile" \
    -t "${IMAGE}:latest" \
    "${PARENT_DIR}"

echo "Pushing to registry..."
docker push "${IMAGE}:latest"

echo "=== Done: ${IMAGE}:latest ==="
