#!/usr/bin/env bash
# Build tailgate binaries + images and load them into the kind cluster.
# Usage: hack/build.sh [TAG] [KIND_CLUSTER]
#   TAG          image tag (default: dev). Use a fresh tag to dodge kind's image cache.
#   KIND_CLUSTER kind cluster name (default: tailgate-e2e).
set -euo pipefail
cd "$(dirname "$0")/.."

TAG="${1:-dev}"
CLUSTER="${2:-tailgate-e2e}"
ARCH="${ARCH:-arm64}" # kind node arch (Apple Silicon docker VM = arm64)

echo ">> building linux/${ARCH} binaries"
export GOWORK=off GOOS=linux GOARCH="${ARCH}" CGO_ENABLED=0
go build -o bin/tailgate-operator ./cmd/tailgate-operator
go build -o bin/tailgate-agent    ./cmd/tailgate-agent
go build -o bin/tailgate-cni      ./cmd/tailgate-cni
go build -o bin/tailgate-gateway  ./cmd/tailgate-gateway

for c in operator agent gateway; do
  img="tailgate-${c}:${TAG}"
  echo ">> docker build ${img}"
  docker build -q -t "${img}" -f "deploy/docker/Dockerfile.${c}" . >/dev/null
  echo ">> kind load ${img} -> ${CLUSTER}"
  kind load docker-image "${img}" --name "${CLUSTER}"
done

echo ">> done (tag ${TAG})"
