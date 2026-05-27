# Image URL to use all building/pushing image targets
IMG_SCHEDULER ?= kruntimes-scheduler:latest
IMG_CONTROLLER ?= kruntimes-controller:latest
IMG_RUNTIMED ?= kruntimes-runtimed:latest
IMG_BASH_RUNTIME ?= kruntimes-bash-runtime:latest
IMG_PYTHON_RUNTIME ?= kruntimes-python-runtime:latest

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
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=charts/kruntimes/crds

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: fmt vet golangci-lint ## Run go fmt, go vet, and golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: generate manifests fmt vet ## Run unit tests.
	go test $$(go list ./... | grep -v /test/integration) -coverprofile cover.out

.PHONY: test-integration
test-integration: generate manifests setup-envtest ## Run integration tests (requires envtest).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	go test ./test/integration/... -v -count=1

##@ E2E

KIND_CLUSTER_NAME ?= kruntimes-e2e

.PHONY: e2e-setup
e2e-setup: docker-build manifests ## Create kind cluster, load images, and deploy chart.
	kind get clusters | grep $(KIND_CLUSTER_NAME) || kind create cluster --name $(KIND_CLUSTER_NAME) --wait 120s
	kind load docker-image $(IMG_SCHEDULER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_CONTROLLER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_RUNTIMED) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_BASH_RUNTIME) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(IMG_PYTHON_RUNTIME) --name $(KIND_CLUSTER_NAME)
	$(HELM) upgrade --install kruntimes ./charts/kruntimes \
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
	go build -o bin/runtimed ./cmd/runtimed
	go build -o bin/controller ./cmd/controller
	go build -o bin/krt ./cmd/krt
	go build -o bin/bash-runtime ./runtimes/bash/cmd

.PHONY: build-scheduler
build-scheduler: generate ## Build scheduler binary.
	go build -o bin/scheduler ./cmd/scheduler

.PHONY: build-runtimed
build-runtimed: generate ## Build runtimed binary.
	go build -o bin/runtimed ./cmd/runtimed

.PHONY: build-controller
build-controller: generate ## Build controller binary.
	go build -o bin/controller ./cmd/controller

.PHONY: build-cli
build-cli: generate ## Build krt binary.
	go build -o bin/krt ./cmd/krt

.PHONY: build-bash-runtime
build-bash-runtime: generate ## Build bash-runtime binary.
	go build -o bin/bash-runtime ./runtimes/bash/cmd

.PHONY: run-scheduler
run-scheduler: generate manifests ## Run scheduler locally (requires kubeconfig).
	go run ./cmd/scheduler --kubeconfig=$(HOME)/.kube/config

.PHONY: run-runtimed
run-runtimed: generate manifests ## Run runtimed locally (requires kubeconfig).
	go run ./cmd/runtimed --kubeconfig=$(HOME)/.kube/config

##@ Docker

.PHONY: docker-build
docker-build: docker-build-scheduler docker-build-controller docker-build-runtimed docker-build-bash-runtime docker-build-python-runtime ## Build all Docker images.

.PHONY: docker-build-scheduler
docker-build-scheduler: ## Build scheduler Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_SCHEDULER) -f Dockerfile.scheduler .

.PHONY: docker-build-controller
docker-build-controller: ## Build controller Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_CONTROLLER) -f Dockerfile.controller .

.PHONY: docker-build-runtimed
docker-build-runtimed: ## Build runtimed Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_RUNTIMED) -f Dockerfile.runtimed .

.PHONY: docker-build-bash-runtime
docker-build-bash-runtime: ## Build bash-runtime Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_BASH_RUNTIME) -f Dockerfile.bash-runtime .

.PHONY: docker-build-python-runtime
docker-build-python-runtime: proto-python ## Build python-runtime Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_PYTHON_RUNTIME) -f Dockerfile.python-runtime .

.PHONY: docker-push
docker-push: ## Push Docker images.
	$(CONTAINER_TOOL) push $(IMG_SCHEDULER)
	$(CONTAINER_TOOL) push $(IMG_CONTROLLER)
	$(CONTAINER_TOOL) push $(IMG_RUNTIMED)
	$(CONTAINER_TOOL) push $(IMG_BASH_RUNTIME)
	$(CONTAINER_TOOL) push $(IMG_PYTHON_RUNTIME)

##@ Helm

.PHONY: template
template: manifests ## Render Helm chart to stdout for validation.
	$(HELM) template kruntimes ./charts/kruntimes --namespace $(NAMESPACE)

##@ Deployment

.PHONY: deploy
deploy: manifests ## Deploy kruntimes platform via Helm.
	$(HELM) upgrade --install kruntimes ./charts/kruntimes --namespace $(NAMESPACE) --create-namespace

.PHONY: deploy-runtimes
deploy-runtimes: ## Deploy built-in runtimes via Helm.
	$(HELM) upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes --namespace $(NAMESPACE) --create-namespace

.PHONY: undeploy
undeploy: ## Remove all kruntimes resources from K8s cluster.
	-$(HELM) uninstall kruntimes-runtimes --namespace $(NAMESPACE) --ignore-not-found
	-$(HELM) uninstall kruntimes --namespace $(NAMESPACE) --ignore-not-found

##@ Proto

PROTOC ?= $(GOBIN)/protoc
PROTOC_GEN_GO = $(GOBIN)/protoc-gen-go
PROTOC_GEN_GO_GRPC = $(GOBIN)/protoc-gen-go-grpc

.PHONY: proto
proto: protoc protoc-gen-go protoc-gen-go-grpc ## Generate gRPC code from proto definitions.
	@PATH="$(GOBIN):$(PATH)" $(PROTOC) \
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

GOLANGCI_LINT = $(GOBIN)/golangci-lint
.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint if not present.
	@test -x $(GOLANGCI_LINT) || go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

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

.PHONY: proto-python
proto-python: ## Generate Python gRPC stubs from proto.
	@mkdir -p runtimes/python/pb
	@touch runtimes/python/pb/__init__.py
	@cd runtimes/python && uv run python -m grpc_tools.protoc \
		--proto_path=../../api/runtime/v1 \
		--python_out=pb \
		--grpc_python_out=pb \
		../../api/runtime/v1/runtime.proto
	@cd runtimes/python && sed -i 's/^import runtime_pb2 as/from . import runtime_pb2 as/' pb/runtime_pb2_grpc.py
