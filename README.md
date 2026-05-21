# kruntime

Two-layer scheduling system for Kubernetes that reduces CI/CD run startup time from minutes to seconds.

## Architecture

```
CI System → run-cli → Run CRD (Pending)
                          │
                    Scheduler → Run (Scheduled, assignedPod)
                          │
                    Agent (runtime pod) → executes → Run (Succeeded/Failed)
```

Key insight: Scheduler and Agent are fully decoupled — they communicate exclusively through Run CRD status updates.

## Components

- **Task CRD** (`kruntime.airconduct.com/v1alpha1`) — the central state machine
- **Scheduler** — K8s controller that watches Pending Tasks and assigns them to Runtime Pods
- **Agent** — process inside each Runtime Pod that claims and executes assigned Tasks
- **run-cli** — lightweight CLI for creating and monitoring Tasks

## Quick Start

### Prerequisites

- Go 1.26+
- Kubernetes cluster (or envtest for integration tests)
- controller-gen (`make controller-gen` installs it automatically)

### Build

```bash
make build
```

This produces three binaries in `bin/`:
- `scheduler` — the run scheduling controller
- `agent` — the per-pod run executor
- `run-cli` — the command-line interface

### Deploy to Kubernetes

```bash
# Deploy CRDs
kubectl apply -k config/crd

# Deploy RBAC
kubectl apply -k config/rbac

# Deploy scheduler
kubectl apply -f config/manager/scheduler_deployment.yaml

# Deploy runtime pods (example: golang-1.26)
kubectl apply -f config/manager/runtime_deployment.yaml
```

### Create a Task

```bash
# Run a quick test
run-cli run --runtime golang-1.26 --wait -- echo "Hello from kruntime"

# Run tests with a repo
run-cli run --runtime golang-1.26 --repo-url https://github.com/example/repo.git --wait -- go test ./...

# List tasks
run-cli list --all-namespaces

# Get run details
run-cli get run-xxxxxxxx
```

## Development

```bash
# Run all code generation
make generate manifests

# Run unit tests
make test

# Run integration tests
make test-integration

# Build Docker images
make docker-build
```

## Run Lifecycle

```
Pending → Scheduled → Running → Succeeded
                              → Failed
```

## Runtime Pods

Runtime pods are standard K8s Deployments with a `runtime` label. The scheduler matches Tasks to pods by matching `spec.runtime` to the pod's `runtime` label.

Example runtime label: `runtime: golang-1.26`

## Metrics

### Scheduler (exposed at :8080)
- `kruntime_scheduler_sync_total` — tasks processed
- `kruntime_scheduler_sync_duration_seconds` — scheduling latency
- `kruntime_scheduler_no_pods_total` — tasks with no available runtime

### Agent (exposed at :9090)
- `kruntime_agent_tasks_total` — tasks completed
- `kruntime_agent_task_duration_seconds` — execution duration
- `kruntime_agent_claim_conflicts_total` — claim conflicts

## License

Apache 2.0
