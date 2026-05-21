# kruntime

Two-layer scheduling system on Kubernetes that eliminates cold-start latency by keeping warm runtime pods ready to execute code in milliseconds.

## Architecture

```
                 ┌──────────────┐
                 │   run-cli    │
                 └──────┬───────┘
                        │ create
                        ▼
┌─────────────────────────────────────────────┐
│  Run CRD (Pending)                          │
│    spec.runtime: bash                       │
│    spec.commands: ["echo hello"]            │
└────────────────────┬────────────────────────┘
                     │ watch
                     ▼
┌─────────────────────────────────────────────┐
│  Scheduler                                  │
│    finds matching Runtime pods by label     │
│    sets assignedPod + phase=Scheduled       │
└────────────────────┬────────────────────────┘
                     │ watch (by assigned pod)
                     ▼
┌─────────────────────────────────────────────┐
│  Runtimed (sidecar)  ──gRPC──▶  Runtime Server │
│    claims Run            │    (bash / custom)│
│    delegates Execute()   │    runs commands  │
│    polls Status()        │                   │
│    updates Run CRD       │                   │
└──────────────────────────┴───────────────────┘
```

Scheduler and runtimed are fully decoupled — they communicate exclusively through Run CRD status updates.

## Components

| Component | Description |
|-----------|-------------|
| **Run CRD** | Central state machine: Pending → Scheduled → Running → Succeeded/Failed |
| **Runtime CRD** | Defines a runtime type (image, port, replicas). Controller auto-creates Deployment with runtimed sidecar. |
| **Scheduler** | K8s controller that watches Pending Runs and assigns them to Runtime Pods |
| **Runtime Controller** | Watches Runtime CRs, creates Deployments with runtimed sidecar injected |
| **Runtimed** | Sidecar in each Runtime Pod. Watches Runs assigned to its pod, delegates execution to the Runtime Server via gRPC |
| **Runtime Server** | Pluggable gRPC server (`Execute` / `Status` / `List` / `Cancel`). Default: bash runtime. |
| **run-cli** | CLI for creating and monitoring Runs |

## Quick Start

### Prerequisites

- Go 1.26+
- Kubernetes cluster (or [kind](https://kind.sigs.k8s.io/) for E2E)
- Helm 3
- protoc (for proto generation; `make proto` auto-installs if missing)

### Build

```bash
make build
```

Produces five binaries in `bin/`: `scheduler`, `controller`, `runtimed`, `bash-runtime`, `run-cli`.

### Deploy

```bash
# Deploy the kruntime platform (CRDs, scheduler, controller, RBAC)
make deploy

# Deploy built-in runtimes (bash runtime)
make deploy-runtimes
```

### Create a Run

```bash
# Quick test
run-cli run --runtime bash --wait -- echo "Hello from kruntime"

# List runs
run-cli list --all-namespaces

# Get run details
run-cli get run-xxxxxxxx
```

### E2E Testing

```bash
make e2e-setup    # creates kind cluster, builds images, deploys everything
make e2e-test     # runs full lifecycle + scheduler responsiveness tests
make e2e-cleanup  # tears down kind cluster
```

## Development

```bash
make proto                 # generate gRPC code from proto
make generate manifests    # generate deepcopy + CRDs
make test                  # unit tests
make test-integration      # integration tests (envtest)
make docker-build          # build all Docker images
```

## Run Lifecycle

```
Pending → Scheduled → Running → Succeeded
                            → Failed
```

## Custom Runtimes

Implement the `Runtime` gRPC service (`api/runtime/v1/runtime.proto`) to create custom runtimes:

```protobuf
service Runtime {
    rpc Execute(ExecuteRequest) returns (ExecuteResponse);
    rpc Status(StatusRequest) returns (StatusResponse);
    rpc List(ListRequest) returns (ListResponse);
    rpc Cancel(CancelRequest) returns (CancelResponse);
}
```

Then define a Runtime CR referencing your image:

```yaml
apiVersion: kruntime.airconduct.com/v1alpha1
kind: Runtime
metadata:
  name: my-python
spec:
  image: my-python-runtime:latest
  port: 9091
  replicas: 3
```

## Metrics

| Component | Port | Metric | Description |
|-----------|------|--------|-------------|
| Scheduler | :8080 | `kruntime_scheduler_sync_total` | Runs processed |
| | | `kruntime_scheduler_sync_duration_seconds` | Scheduling latency |
| | | `kruntime_scheduler_no_pods_total` | Runs with no available runtime |
| Runtimed | :9090 | `kruntime_runtimed_runs_total` | Runs completed |
| | | `kruntime_runtimed_run_duration_seconds` | Execution duration |
| | | `kruntime_runtimed_claim_conflicts_total` | Claim conflicts |
| Controller | :8082 | (controller-runtime defaults) | |

## Project Structure

```
api/
├── v1alpha1/          Run + Runtime CRD types
└── runtime/v1/        gRPC proto + generated code
cmd/
├── scheduler/         Scheduler entry point
├── controller/        Runtime controller entry point
├── runtimed/             Agent sidecar entry point
├── bash-runtime/      Default bash runtime server
└── run-cli/           CLI tool
internal/
├── runtimed/             runtimed controller (claim + gRPC delegation)
├── controller/        Runtime controller (Deployment creation)
├── scheduler/         Run reconciler + scheduling strategies
├── runtime/bash/      Bash runtime gRPC server implementation
└── runcli/            CLI subcommands (run, get, list)
charts/
├── kruntime/          Platform Helm chart (CRDs, scheduler, controller)
└── kruntime-runtimes/ Built-in runtimes Helm chart
test/
├── e2e/               End-to-end tests (kind cluster)
└── integration/       Integration tests (envtest)
```

## License

Apache 2.0
