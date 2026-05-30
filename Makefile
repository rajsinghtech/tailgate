# tailgate — Kubernetes operator that gives pods native Tailscale egress via a
# shared tailscaled gateway per EgressGroup, with a node agent that veth-stitches
# member pods into the gateway netns.
#
# NOTE: go.mod carries a LOCAL `replace tailscale.com => <local path>`. Building
# from source in CI requires that replace to be repointed to a published version
# (the human resolves this separately); the targets below force GOWORK=off so the
# personal go.work in the parent tree never leaks into a build.

# Image registry/repo prefix. The three images are <REGISTRY>-{operator,agent,gateway}.
REGISTRY ?= ghcr.io/rajsinghtech/tailgate
IMG_TAG ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")

OPERATOR_IMG ?= $(REGISTRY)-operator:$(IMG_TAG)
AGENT_IMG    ?= $(REGISTRY)-agent:$(IMG_TAG)
GATEWAY_IMG  ?= $(REGISTRY)-gateway:$(IMG_TAG)

# Container tool. Tested with docker; override for podman etc.
CONTAINER_TOOL ?= docker

# Build platform. The agent is linux-only (//go:build linux), so all binaries are
# cross-compiled for linux; GOARCH defaults to the host arch.
GOOS ?= linux
GOARCH ?= $(shell go env GOARCH)

# kind cluster used by kind-load / e2e.
KIND_CLUSTER ?= tailgate-e2e

# Helm chart location (conventional path; chart is added separately).
HELM_CHART_DIR ?= charts/tailgate

# Setting SHELL to bash allows bash commands to be executed by recipes.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests from API types.
	GOWORK=off "$(CONTROLLER_GEN)" crd paths="./api/..." output:crd:artifacts:config=config/crd

.PHONY: generate
generate: controller-gen ## Generate DeepCopy method implementations.
	GOWORK=off "$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	GOWORK=off go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	GOWORK=off go vet ./...

.PHONY: test
test: generate fmt vet ## Run unit tests (excludes e2e).
	GOWORK=off go test $$(GOWORK=off go list ./... | grep -v /test/e2e) -coverprofile cover.out

.PHONY: test-e2e
test-e2e: ## Run e2e tests against an ephemeral tailnet on a kind cluster (needs TS_ORG_OAUTH_CLIENT_ID/SECRET).
	GOWORK=off go test -tags=e2e ./test/e2e/ -v -timeout 30m

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter.
	GOWORK=off "$(GOLANGCI_LINT)" run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes.
	GOWORK=off "$(GOLANGCI_LINT)" run --fix

##@ Build

# The agent is linux-only, so everything is cross-compiled for linux. GOWORK=off
# keeps the parent go.work out of the build.
.PHONY: build
build: generate fmt vet ## Build all four binaries into bin/ (cross-compiled for linux).
	GOWORK=off CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/tailgate-operator ./cmd/tailgate-operator
	GOWORK=off CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/tailgate-agent    ./cmd/tailgate-agent
	GOWORK=off CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/tailgate-cni      ./cmd/tailgate-cni
	GOWORK=off CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/tailgate-gateway  ./cmd/tailgate-gateway

##@ Images

# Three images, each built from its own Dockerfile in deploy/docker. The operator
# and agent Dockerfiles are multi-stage source builds onto distroless; the gateway
# Dockerfile builds the gateway cmd then lands it on the tailscale/tailscale base.
DOCKER_DIR ?= deploy/docker

.PHONY: docker-build
docker-build: ## Build the three images (operator, agent, gateway) from source.
	$(CONTAINER_TOOL) build -t $(OPERATOR_IMG) -f $(DOCKER_DIR)/Dockerfile.operator .
	$(CONTAINER_TOOL) build -t $(AGENT_IMG)    -f $(DOCKER_DIR)/Dockerfile.agent    .
	$(CONTAINER_TOOL) build -t $(GATEWAY_IMG)  -f $(DOCKER_DIR)/Dockerfile.gateway  .

.PHONY: docker-push
docker-push: ## Push the three images to ghcr.
	$(CONTAINER_TOOL) push $(OPERATOR_IMG)
	$(CONTAINER_TOOL) push $(AGENT_IMG)
	$(CONTAINER_TOOL) push $(GATEWAY_IMG)

.PHONY: kind-load
kind-load: docker-build ## Build and load the three images into the kind cluster.
	kind load docker-image $(OPERATOR_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(AGENT_IMG)    --name $(KIND_CLUSTER)
	kind load docker-image $(GATEWAY_IMG)  --name $(KIND_CLUSTER)

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart.
	helm lint $(HELM_CHART_DIR)

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p "$(LOCALBIN)"

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT  = $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.20.1
GOLANGCI_LINT_VERSION ?= v2.12.2

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] && [ "$$(readlink -- "$(1)" 2>/dev/null)" = "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f "$(1)" ;\
GOWORK=off GOBIN="$(LOCALBIN)" go install $${package} ;\
mv "$(LOCALBIN)/$$(basename "$(1)")" "$(1)-$(3)" ;\
} ;\
ln -sf "$$(realpath "$(1)-$(3)")" "$(1)"
endef
