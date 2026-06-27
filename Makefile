# Image URL to use all building/pushing image targets
IMG_SCHEDULER ?= kruntimes-scheduler:latest
IMG_CONTROLLER ?= kruntimes-controller:latest
IMG_RUNTIMED ?= kruntimes-runtimed:latest
IMG_BASH_RUNTIME ?= kruntimes-bash-runtime:latest
IMG_PYTHON_RUNTIME ?= kruntimes-python-runtime:latest

# ENVTEST_K8S_VERSION refers to the version of k8s to use for envtest
ENVTEST_K8S_VERSION = 1.32

# Pinned local tool versions. Keep these explicit so clean developer and CI
# environments install reproducible toolchains instead of resolving @latest.
CONTROLLER_GEN_VERSION ?= v0.17.3
SETUP_ENVTEST_VERSION ?= v0.24.1
GOLANGCI_LINT_VERSION ?= v2.12.2
GOVULNCHECK_VERSION ?= v1.4.0
PROTOC_VERSION ?= 29.3
PROTOC_ARCH ?= linux-x86_64
PROTOC_GEN_GO_VERSION ?= v1.36.11
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
UV_VERSION ?= 0.11.16

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
vet: generate proto ## Run go vet against code.
	go vet ./...

.PHONY: lint
lint: fmt vet golangci-lint ## Run go fmt, go vet, and golangci-lint.
	$(GOLANGCI_LINT) run ./...

.PHONY: test
test: generate manifests proto fmt vet ## Run unit tests.
	go test $$(go list ./... | grep -v /test/integration | grep -v /test/e2e) -coverprofile cover.out

.PHONY: test-integration
test-integration: generate manifests setup-envtest ## Run integration tests (requires envtest).
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" \
	go test ./test/integration/... -v -count=1 -failfast

.PHONY: test-race
test-race: generate manifests proto ## Run focused Go race-detector coverage for runtime and control-plane packages.
	go test -race ./internal/controller ./internal/scheduler ./internal/runtimed ./runtimes/bash -count=1

.PHONY: govulncheck
govulncheck: govulncheck-tool ## Run govulncheck against all Go packages.
	$(GOVULNCHECK) ./...

.PHONY: test-s3-integration
test-s3-integration: ## Run S3 ArtifactStore integration tests against MinIO.
	CONTAINER_TOOL=$(CONTAINER_TOOL) ./hack/test-s3-integration.sh

##@ E2E

KIND_CLUSTER_NAME ?= kruntimes-e2e
E2E_IMAGE_TAG ?= latest
E2E_RUN_IMAGE_TAG ?= e2e-$(shell date +%Y%m%d%H%M%S)
E2E_IMG_SCHEDULER ?= kruntimes-scheduler:$(E2E_IMAGE_TAG)
E2E_IMG_CONTROLLER ?= kruntimes-controller:$(E2E_IMAGE_TAG)
E2E_IMG_RUNTIMED ?= kruntimes-runtimed:$(E2E_IMAGE_TAG)
E2E_IMG_BASH_RUNTIME ?= kruntimes-bash-runtime:$(E2E_IMAGE_TAG)
E2E_IMG_PYTHON_RUNTIME ?= kruntimes-python-runtime:$(E2E_IMAGE_TAG)
.PHONY: e2e-setup
e2e-setup: IMG_SCHEDULER = $(E2E_IMG_SCHEDULER)
e2e-setup: IMG_CONTROLLER = $(E2E_IMG_CONTROLLER)
e2e-setup: IMG_RUNTIMED = $(E2E_IMG_RUNTIMED)
e2e-setup: IMG_BASH_RUNTIME = $(E2E_IMG_BASH_RUNTIME)
e2e-setup: IMG_PYTHON_RUNTIME = $(E2E_IMG_PYTHON_RUNTIME)
e2e-setup: manifests docker-build ## Create kind cluster, load images, and deploy chart.
	kind get clusters | grep $(KIND_CLUSTER_NAME) || kind create cluster --name $(KIND_CLUSTER_NAME) --wait 120s
	kind load docker-image $(E2E_IMG_SCHEDULER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(E2E_IMG_CONTROLLER) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(E2E_IMG_RUNTIMED) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(E2E_IMG_BASH_RUNTIME) --name $(KIND_CLUSTER_NAME)
	kind load docker-image $(E2E_IMG_PYTHON_RUNTIME) --name $(KIND_CLUSTER_NAME)
	# Server-side apply avoids copying large CRD schemas into the 256 KiB last-applied annotation.
	kubectl apply --server-side --force-conflicts -f charts/kruntimes/crds
	$(HELM) upgrade --install kruntimes ./charts/kruntimes \
		--set scheduler.image=$(E2E_IMG_SCHEDULER) \
		--set controller.image=$(E2E_IMG_CONTROLLER) \
		--set runtimed.image=$(E2E_IMG_RUNTIMED) \
		--namespace $(NAMESPACE) --create-namespace --wait --timeout 120s

.PHONY: e2e-test
e2e-test: generate ## Run E2E tests against the kind cluster.
	KRUNTIMES_BASH_RUNTIME_IMAGE=$(E2E_IMG_BASH_RUNTIME) \
	KRUNTIMES_PYTHON_RUNTIME_IMAGE=$(E2E_IMG_PYTHON_RUNTIME) \
	KRUNTIMES_RUNTIMED_IMAGE=$(E2E_IMG_RUNTIMED) \
	go test ./test/e2e/... -v -count=1 -failfast

.PHONY: e2e
e2e: E2E_IMAGE_TAG := $(E2E_RUN_IMAGE_TAG)
e2e: e2e-setup e2e-test ## Full E2E: setup cluster, deploy, run tests.

# Preserve setup-before-test ordering even when make is invoked with -j.
.NOTPARALLEL: e2e-setup e2e

.PHONY: e2e-cleanup
e2e-cleanup: ## Delete the kind cluster.
	kind delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: benchmark
benchmark: e2e-setup ## Run the performance benchmark against the current Kubernetes context.
	KRUNTIMES_BASH_RUNTIME_IMAGE=$(E2E_IMG_BASH_RUNTIME) \
	KRUNTIMES_RUNTIMED_IMAGE=$(E2E_IMG_RUNTIMED) \
	go run ./hack/benchmark

##@ Build

.PHONY: build
build: generate proto ## Build all binaries.
	go build -o bin/scheduler ./cmd/scheduler
	go build -o bin/runtimed ./cmd/runtimed
	go build -o bin/controller ./cmd/controller
	go build -o bin/krt ./cmd/krt
	go build -o bin/bash-runtime ./runtimes/bash/cmd

.PHONY: build-scheduler
build-scheduler: generate ## Build scheduler binary.
	go build -o bin/scheduler ./cmd/scheduler

.PHONY: build-runtimed
build-runtimed: generate proto ## Build runtimed binary.
	go build -o bin/runtimed ./cmd/runtimed

.PHONY: build-controller
build-controller: generate ## Build controller binary.
	go build -o bin/controller ./cmd/controller

.PHONY: build-cli
build-cli: generate ## Build krt binary.
	go build -o bin/krt ./cmd/krt

.PHONY: build-bash-runtime
build-bash-runtime: proto ## Build bash-runtime binary.
	go build -o bin/bash-runtime ./runtimes/bash/cmd

.PHONY: run-scheduler
run-scheduler: generate manifests ## Run scheduler locally (requires kubeconfig).
	go run ./cmd/scheduler --kubeconfig=$(HOME)/.kube/config

.PHONY: run-runtimed
run-runtimed: generate manifests proto ## Run runtimed locally (requires kubeconfig).
	go run ./cmd/runtimed --kubeconfig=$(HOME)/.kube/config

##@ Docker

.PHONY: docker-build
docker-build: docker-build-scheduler docker-build-controller docker-build-runtimed docker-build-bash-runtime docker-build-python-runtime ## Build all Docker images.

.PHONY: docker-build-scheduler
docker-build-scheduler: generate ## Build scheduler Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_SCHEDULER) -f Dockerfile.scheduler .

.PHONY: docker-build-controller
docker-build-controller: generate ## Build controller Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_CONTROLLER) -f Dockerfile.controller .

.PHONY: docker-build-runtimed
docker-build-runtimed: generate proto ## Build runtimed Docker image.
	$(CONTAINER_TOOL) build -t $(IMG_RUNTIMED) -f Dockerfile.runtimed .

.PHONY: docker-build-bash-runtime
docker-build-bash-runtime: proto ## Build bash-runtime Docker image.
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

.PHONY: test-helm
test-helm: manifests ## Validate Helm charts and multi-release rendering.
	$(HELM) lint ./charts/kruntimes ./charts/kruntimes-runtimes
	$(HELM) template kruntimes ./charts/kruntimes --namespace kruntimes-system
	$(HELM) template kruntimes-runtimes ./charts/kruntimes-runtimes --namespace default
	./hack/verify-helm-multi-release.py
	./hack/verify-helm-images.py
	./hack/verify-helm-multi-namespace.py
	./hack/verify-helm-metrics.py
	./hack/verify-helm-config-values.py

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
		api/runtime/v1/runtime.proto \
		api/artifact/v1/artifact.proto

##@ Tools

CONTROLLER_GEN = $(GOBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen locally if not already installed.
	@if ! test -x $(CONTROLLER_GEN) || ! $(CONTROLLER_GEN) --version | grep -q "$(CONTROLLER_GEN_VERSION)"; then \
		go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION); \
	fi

SETUP_ENVTEST = $(GOBIN)/setup-envtest
.PHONY: setup-envtest
setup-envtest: ## Download setup-envtest locally if not already installed.
	@if ! test -x $(SETUP_ENVTEST) || ! $(SETUP_ENVTEST) version | grep -q "$(SETUP_ENVTEST_VERSION)"; then \
		go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(SETUP_ENVTEST_VERSION); \
	fi

GOLANGCI_LINT = $(GOBIN)/golangci-lint
.PHONY: golangci-lint
golangci-lint: ## Install golangci-lint if not present.
	@if ! test -x $(GOLANGCI_LINT) || ! $(GOLANGCI_LINT) --version | grep -q "$(GOLANGCI_LINT_VERSION)"; then \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION); \
	fi

GOVULNCHECK = $(GOBIN)/govulncheck
.PHONY: govulncheck-tool
govulncheck-tool: ## Install govulncheck if not present.
	@if ! test -x $(GOVULNCHECK) || ! $(GOVULNCHECK) -version | grep -q "$(GOVULNCHECK_VERSION)"; then \
		go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION); \
	fi

.PHONY: protoc
protoc: ## Install protoc compiler if not present.
	@if ! test -x $(PROTOC) || ! $(PROTOC) --version | grep -q "libprotoc $(PROTOC_VERSION)"; then \
		wget -q -O /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v$(PROTOC_VERSION)/protoc-$(PROTOC_VERSION)-$(PROTOC_ARCH).zip && \
		python3 -c "import zipfile,sys;zipfile.ZipFile(sys.argv[1]).extractall(sys.argv[2])" /tmp/protoc.zip /tmp/protoc-install && \
		cp /tmp/protoc-install/bin/protoc $(PROTOC) && chmod +x $(PROTOC) && \
		rm -rf /tmp/protoc.zip /tmp/protoc-install; \
	fi

.PHONY: protoc-gen-go
protoc-gen-go: ## Install protoc-gen-go if not present.
	@if ! test -x $(PROTOC_GEN_GO) || ! $(PROTOC_GEN_GO) --version | grep -q "$(PROTOC_GEN_GO_VERSION)"; then \
		go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION); \
	fi

.PHONY: protoc-gen-go-grpc
protoc-gen-go-grpc: ## Install protoc-gen-go-grpc if not present.
	@if ! test -x $(PROTOC_GEN_GO_GRPC) || ! $(PROTOC_GEN_GO_GRPC) --version | grep -q "$(PROTOC_GEN_GO_GRPC_VERSION)"; then \
		go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION); \
	fi

UV = $(GOBIN)/uv
.PHONY: uv
uv: ## Install uv locally if not present.
	@if ! test -x "$(UV)" || ! "$(UV)" --version | grep -q "uv $(UV_VERSION)"; then \
		mkdir -p "$(GOBIN)"; \
		installer=$$(mktemp); \
		trap 'rm -f "$$installer"' 0; \
		curl -LsSf https://astral.sh/uv/$(UV_VERSION)/install.sh -o "$$installer"; \
		env UV_INSTALL_DIR="$(GOBIN)" UV_NO_MODIFY_PATH=1 sh "$$installer"; \
	fi

.PHONY: proto-python
proto-python: uv ## Generate Python gRPC stubs from proto.
	@mkdir -p runtimes/python/pb
	@touch runtimes/python/pb/__init__.py
	@cd runtimes/python && $(UV) run python -m grpc_tools.protoc \
		--proto_path=../../api/runtime/v1 \
		--python_out=pb \
		--grpc_python_out=pb \
		../../api/runtime/v1/runtime.proto
	@cd runtimes/python && sed -i 's/^import runtime_pb2 as/from . import runtime_pb2 as/' pb/runtime_pb2_grpc.py
