# Image URL to use all building/pushing image targets
IMG_SCHEDULER ?= kruntime-scheduler:latest
IMG_CONTROLLER ?= kruntime-controller:latest
IMG_AGENT ?= kruntime-agent:latest
IMG_BASH_RUNTIME ?= kruntime-bash-runtime:latest

# ENVTEST_K8S_VERSION refers to the version of k8s to use for envtest
ENVTEST_K8S_VERSION = 1.32

# NAMESPACE for helm deploy
NAMESPACE ?= default

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used
CONTAINER_TOOL ?= docker

# HELM binary
HELM ?= helm

.PHONY: all
all: proto generate manifests build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests into Helm chart.
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=charts/kruntime/crds

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: generate manifests fmt vet ## Run unit tests.
	go test $$(go list ./... | grep -v /test/integration) -coverprofile cover.out

.PHONY: test-integration
test-integration: generate manifests setup-envtest ## Run integration tests (requires envtest).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	go test ./test/integration/... -v -count=1

##@ E2E

KIND_CLUSTER_NAME ?= kruntime-e2e

.PHONY: e2e-setup
e2e-setup: docker-build manifests ## Create kind cluster, load images, and deploy chart.
	kind get clusters | grep $(KIND_CLUSTER_NAME) || kind create cluster --name $(KIND_CLUSTER_NAME) --wait 120s
	kind load docker-image $(IMG_SCHEDULER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_CONTROLLER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_AGENT) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_BASH_RUNTIME) --name $(KIND_CLUSTER_NAME)
	$(HELM) upgrade --install kruntime ./charts/kruntime \
		--namespace $(NAMESPACE) --create-namespace --wait --timeout 120s

.PHONY: e2e-test
e2e-test: ## Run E2E tests against the kind cluster.
	go test ./test/e2e/... -v -count=1 -tags=e2e

.PHONY: e2e
e2e: e2e-setup e2e-test ## Full E2E: setup cluster, deploy, run tests.

.PHONY: e2e-cleanup
e2e-cleanup: ## Delete the kind cluster.
	kind delete cluster --name $(KIND_CLUSTER_NAME)

##@ Build

.PHONY: build
build: generate ## Build all binaries.
	go build -o bin/scheduler ./cmd/scheduler
	go build -o bin/agent ./cmd/agent
	go build -o bin/controller ./cmd/controller
	go build -o bin/task-cli ./cmd/task-cli
	go build -o bin/bash-runtime ./cmd/bash-runtime

.PHONY: build-scheduler
build-scheduler: generate ## Build scheduler binary.
	go build -o bin/scheduler ./cmd/scheduler

.PHONY: build-agent
build-agent: generate ## Build agent binary.
	go build -o bin/agent ./cmd/agent

.PHONY: build-controller
build-controller: generate ## Build controller binary.
	go build -o bin/controller ./cmd/controller

.PHONY: build-cli
build-cli: generate ## Build task-cli binary.
	go build -o bin/task-cli ./cmd/task-cli

.PHONY: build-bash-runtime
build-bash-runtime: generate ## Build bash-runtime binary.
	go build -o bin/bash-runtime ./cmd/bash-runtime

.PHONY: run-scheduler
run-scheduler: generate manifests ## Run scheduler locally (requires kubeconfig).
	go run ./cmd/scheduler --kubeconfig=$(HOME)/.kube/config

.PHONY: run-agent
run-agent: generate manifests ## Run agent locally (requires kubeconfig).
	go run ./cmd/agent --kubeconfig=$(HOME)/.kube/config

##@ Docker

.PHONY: docker-build
docker-build: docker-build-scheduler docker-build-controller docker-build-agent docker-build-bash-runtime ## Build all Docker images.

.PHONY: docker-build-scheduler
docker-build-scheduler: ## Build scheduler Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_SCHEDULER) -f Dockerfile.scheduler .

.PHONY: docker-build-controller
docker-build-controller: ## Build controller Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_CONTROLLER) -f Dockerfile.controller .

.PHONY: docker-build-agent
docker-build-agent: ## Build agent Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_AGENT) -f Dockerfile.agent .

.PHONY: docker-build-bash-runtime
docker-build-bash-runtime: ## Build bash-runtime Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_BASH_RUNTIME) -f Dockerfile.bash-runtime .

.PHONY: docker-push
docker-push: ## Push Docker images.
	$(CONTAINER_TOOL) push $(IMG_SCHEDULER)
	$(CONTAINER_TOOL) push $(IMG_CONTROLLER)
	$(CONTAINER_TOOL) push $(IMG_AGENT)
	$(CONTAINER_TOOL) push $(IMG_BASH_RUNTIME)

##@ Helm

.PHONY: template
template: manifests ## Render Helm chart to stdout for validation.
	$(HELM) template kruntime ./charts/kruntime --namespace $(NAMESPACE)

##@ Deployment

.PHONY: deploy
deploy: manifests ## Deploy kruntime platform via Helm.
	$(HELM) upgrade --install kruntime ./charts/kruntime --namespace $(NAMESPACE) --create-namespace

.PHONY: deploy-runtimes
deploy-runtimes: ## Deploy built-in runtimes via Helm.
	$(HELM) upgrade --install kruntime-runtimes ./charts/kruntime-runtimes --namespace $(NAMESPACE) --create-namespace

.PHONY: undeploy
undeploy: ## Remove all kruntime resources from K8s cluster.
	-$(HELM) uninstall kruntime-runtimes --namespace $(NAMESPACE) --ignore-not-found
	-$(HELM) uninstall kruntime --namespace $(NAMESPACE) --ignore-not-found

##@ Proto

PROTOC ?= $(GOBIN)/protoc
PROTOC_GEN_GO = $(GOBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC = $(GOBIN)/protoc-gen-go-grpc

.PHONY: proto
proto: protoc protoc-gen-go protoc-gen-go-grpc ## Generate gRPC code from proto definitions.
	PATH="$(GOBIN):$(PATH)" $(PROTOC) \
		--proto_path=. \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		api/runtime/v1/runtime.proto

##@ Tools

CONTROLLER_GEN = $(GOBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if not already installed.
	@test -x $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.17.3

SETUP_ENVTEST = $(GOBIN)/setup-envtest
.PHONY: setup-envtest
setup-envtest: ## Download setup-envtest locally if not already installed.
	@test -x $(SETUP_ENVTEST) || go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: protoc
protoc: ## Install protoc compiler if not present.
	@test -x $(PROTOC) || ( \
		V=29.3; ARCH=linux-x86_64; \
		wget -q -O /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v$$V/protoc-$$V-$$ARCH.zip && \
		python3 -c "import zipfile,sys;zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" /tmp/protoc.zip /tmp/protoc-install && \
		cp /tmp/protoc-install/bin/protoc $(PROTOC) && chmod +x $(PROTOC) && \
		rm -rf /tmp/protoc.zip /tmp/protoc-install \
	)

.PHONY: protoc-gen-go
protoc-gen-go: ## Install protoc-gen-go if not present.
	@test -x $(PROTOC_GEN_GO) || go install google.golang.org/protobuf/cmd/protoc-gen-go@latest

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: ## Install protoc-gen-go-grpc if not present.
	@test -x $(PROTOC_GEN_GO_GRPC) || go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
