# kruntimes
<p align="center">
  <img src="docs/logo.png" alt="kruntimes logo" />
</p>

A two-layer scheduling system built on Kubernetes that eliminates cold-start latency by keeping warm runtime pods ready to execute code in milliseconds. Built for AI agents, CI/CD tasks, serverless functions, and any short-lived, high-concurrency workloads.

## Background

Building a serverless platform on vanilla Kubernetes faces four fundamental challenges:

**Cold start: minutes, not milliseconds.** Pod scheduling, image pulling, and container initialization take 1-5 minutes. Serverless requires sub-second startup. While Firecracker boots in ~100ms, combining it with lazy image loading requires non-standard K8s modifications most platform teams can't afford.

**Scheduler mismatch.** The K8s scheduler is built for long-running services. It struggles with massive volumes of short-lived, high-concurrency tasks — low throughput, slow decisions. Batch schedulers (Volcano, etc.) target big-data jobs but lack awareness of fine-grained function workloads.

**Elasticity gap.** Native HPA reacts too slowly to traffic spikes. KEDA makes fast scaling decisions, but the underlying Pods still start slowly — "decide fast, execute slow." The result is always a compromise: cold-start hacks or expensive pre-warmed pools.

**Conway's Law.** The team building the serverless platform is rarely the team managing the K8s infrastructure. Organizational boundaries make it politically impossible to push through low-level optimizations like custom schedulers, image lazy-loading, or node pre-warming. The only viable path is to build *above* K8s, not inside it.

## Design

kruntimes solves these by treating K8s as an IaaS layer and building all serverless logic in the application layer, under full control of the platform team.

### Reuse over creation

Instead of creating a new Pod for every request, kruntimes maintains pools of pre-warmed **Runtime Pods**. Each pod already has the required toolchain, dependencies, and a running daemon. When a Run arrives, the scheduler assigns it to an existing hot pod. Startup drops from minutes to milliseconds — the slowest parts of Pod creation (scheduling, image pulling) never happen at request time.

### Two-layer scheduling

```
Layer 1 (K8s)  →  maintains Runtime Pod pools (coarse, low-frequency)
Layer 2 (app)  →  assigns individual Runs to pods within a pool (fine, high-throughput)
```

This separation lets us implement application-level queuing, prioritization, and resource allocation without touching K8s internals.

### Runtime abstraction

Different execution environments (Go, Python, Node.js, BuildKit) are modeled as distinct **Runtime** types. Each Runtime is an independent Deployment pool with a specific container image and toolchain. This gives natural environment isolation, guarantees consistency (all runs use the same pre-built image), and makes adding new languages trivial — deploy a new pool, register a label.

### Declarative CRDs, not P2P

The **Run CRD** is the sole source of truth — it holds *what to run*, *which environment*, *who is assigned*, and *the result*. All components communicate exclusively through CRD status updates:

- **Scheduler** watches Pending Runs, sets `assignedPod` + `phase=Scheduled`
- **Runtimed** watches Runs assigned to its pod, delegates to the Runtime Server via gRPC, updates status
- **Failover** is free: if a pod dies, its Runs timeout and are re-scheduled

No connection pools. No IP tracking. No P2P. Just etcd.

## Architecture

```
                 ┌──────────────┐
                 │     krt      │
                 └──────┬───────┘
                        │ create
                        ▼
┌─────────────────────────────────────────────┐
│  Run CRD (Pending)                          │
│    spec.runtime: bash                       │
│    spec.args: ["echo hello"]                │
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
│  Runtimed (sidecar) ──gRPC──▶ Runtime Server│
│    claims Run            │   (bash / custom)│
│    delegates Execute()   │   runs commands  │
│    polls Status()        │                  │
│    updates Run CRD       │                  │
└──────────────────────────┴──────────────────┘
```

## Components

| Component | Description |
|-----------|-------------|
| **Run CRD** | Central state machine: Pending → Scheduled → Running → Succeeded/Failed |
| **Runtime CRD** | Defines a runtime type (image, port, replicas). Controller auto-creates Deployment with runtimed daemon. |
| **Scheduler** | K8s controller that watches Pending Runs and assigns them to Runtime Pods in the same namespace. |
| **Runtime Controller** | Watches Runtime CRs, creates Deployments with runtimed daemon injected. |
| **Runtimed** | Daemon in each Runtime Pod. Watches Runs assigned to its pod, delegates execution to the Runtime Server via gRPC. |
| **Runtime Server** | Pluggable gRPC service (`Execute` / `Status` / `List` / `Cancel`). Built-in: bash, python. |
| **Python Runtime** | Python 3.13 gRPC server. Supports inline scripts, git repo, and FaaS entrypoint (`module.function`). |
| **krt** | CLI for creating and monitoring Runs. |

## Quick Start

### Prerequisites

- Go 1.26+
- Kubernetes cluster (or [kind](https://kind.sigs.k8s.io/) for E2E)
- Helm 3

### Build

```bash
make build
```

Produces five binaries: `scheduler`, `controller`, `runtimed`, `bash-runtime`, `krt`. The Python runtime is Docker-only.

### Deploy

```bash
make deploy           # platform (CRDs, scheduler, controller, RBAC)
make deploy-runtimes  # built-in runtimes (bash, python)
```

### Create a Run

```bash
krt run --runtime bash --wait -- echo "Hello from kruntimes"
krt run --runtime python --inline "print('Hello')" --wait
krt list --all-namespaces
krt get run-xxxxxxxx
```

### E2E Testing

```bash
make e2e-setup    # creates kind cluster, builds images, deploys everything
make e2e-test     # full lifecycle + scheduler responsiveness
make e2e-cleanup  # tears down kind cluster
```

## Roadmap

### v0.1 — Current

- [x] Run CRD with full lifecycle (Pending → Scheduled → Running → Succeeded/Failed)
- [x] Runtime CRD + controller with automatic daemon injection
- [x] Pluggable gRPC Runtime Server interface
- [x] Default bash runtime
- [x] Two-layer scheduling (least-loaded strategy)
- [x] Helm chart deployment (platform + runtimes)
- [x] Prometheus metrics (scheduler + runtimed)
- [x] Leader election for scheduler and controller HA
- [x] E2E test suite with kind

### v0.2 — Runtimes & Workflows

- [x] Built-in runtimes: Python
- [ ] Built-in runtimes: Go, Node.js, WASM

**v0.2.1 — Workflow**
- [ ] Workflow CRD (jobs, steps, needs, outputs)
- [ ] `${{ }}` expression resolution (steps/jobs outputs)
- [ ] `$OUTPUTS` file → `Run.Status.Outputs`
- [ ] CLI: `krt workflow create/list/get`

**v0.2.2 — Action**
- [ ] Action CRD (reusable step templates)
- [ ] `uses: actions/<name>` with `with:` inputs
- [ ] Action input/output passing

**v0.2.3 — WorkflowTemplate**
- [ ] WorkflowTemplate CRD (reusable job templates)
- [ ] `uses: workflows/<name>` with `with:` inputs
- [ ] Template input/output passing

**v0.2.x — Scheduling**
- [ ] Runtime SDK (Python, Go) for programmatic Run creation
- [ ] Custom scheduling strategies (priority, affinity, bin-packing)
- [ ] Per-Run resource limits enforced by Runtime Server (cgroups)
- [ ] GPU and extended resource support in Runtime CRD

### v0.3 — Advanced Runtimes

- [ ] Custom Runtime Server examples: Slurm, Ray
- [ ] Runtime-managed scheduling mode (Runtime Server handles its own queue)
- [ ] Multi-tenant namespace isolation

### v1.0 — Platform Features

- [ ] Stale run reaper (auto-requeue runs stuck in Running with dead daemon)
- [ ] CronRun CRD for scheduled execution
- [ ] Webhook triggers (GitHub, Slack, etc.)

## Development

```bash
make proto                 # generate Go gRPC code
make proto-python          # generate Python gRPC stubs (requires uv)
make generate manifests    # generate deepcopy + CRDs
make test                  # unit tests
make test-integration      # integration tests (envtest)
make docker-build          # build all Docker images
```

## Python Runtime

### Development Setup

```bash
# Install uv (Python package manager)
curl -LsSf https://astral.sh/uv/install.sh | sh

# Generate Python gRPC stubs + install deps
cd runtimes/python
uv sync

# Run Python unit tests
uv run python -m unittest server_test -v
```

### How it works

The Python runtime is a standalone gRPC server (Python 3.13) deployed alongside the runtimed daemon. The runtimed handles code preparation (inline dump, git clone) on the shared `/workspace` volume, then delegates execution to the Python runtime via gRPC.

**Execution flow:**
1. Runtimed prepares source on `/workspace/<uid>/` — inline code dumped to the `entrypoint` file (default `"script"`), or git clone to `repo/`
2. Runtimed calls gRPC `Execute` with `working_dir` set to the prepared directory, `entrypoint` to the script name, and `handler` (if FaaS mode)
3. If `handler` is set (e.g. `"app.handler"`), the Python runtime imports the module and calls `handler(event)` with `args` as the event payload
4. Otherwise, it runs `python <working_dir>/<entrypoint> <args>` as a script

| Mode | Example |
|------|---------|
| Inline | `krt run --runtime python --inline "print(1+1)"` |
| FaaS | `krt run --runtime python --inline $'def handler(e):\n  return {"ok": True}' --handler "script.handler"` |
| Repo | `krt run --runtime python --repo-url https://github.com/user/proj.git` |
| Entrypoint | `krt run --runtime python --repo-url <url> --entrypoint "main.py"` |

## Run Lifecycle

```
Pending → Scheduled → Running → Succeeded
                            → Failed
```

## Custom Runtimes

Implement the `Runtime` gRPC service (`api/runtime/v1/runtime.proto`):

```protobuf
service Runtime {
    rpc Execute(ExecuteRequest) returns (ExecuteResponse);
    rpc Status(StatusRequest) returns (StatusResponse);
    rpc List(ListRequest) returns (ListResponse);
    rpc Cancel(CancelRequest) returns (CancelResponse);
}
```

Deploy with a Runtime CR:

```yaml
apiVersion: kruntimes.kruntimes.com/v1alpha1
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
| Scheduler | :8080 | `kruntimes_scheduler_sync_total` | Runs processed |
| | | `kruntimes_scheduler_sync_duration_seconds` | Scheduling latency |
| | | `kruntimes_scheduler_no_pods_total` | Runs with no available runtime |
| Runtimed | :9090 | `kruntimes_runtimed_runs_total` | Runs completed |
| | | `kruntimes_runtimed_run_duration_seconds` | Execution duration |
| | | `kruntimes_runtimed_claim_conflicts_total` | Claim conflicts |
| Controller | :8082 | (controller-runtime defaults) | |

## Project Structure

```
api/
├── v1alpha1/          Run + Runtime CRD types
└── runtime/v1/        gRPC proto + generated code
cmd/
├── scheduler/         Scheduler entry point
├── controller/        Runtime controller entry point
├── runtimed/          Runtimed daemon entry point
└── krt/               CLI tool
runtimes/
├── bash/              Bash runtime gRPC server (Go)
│   └── cmd/
└── python/            Python runtime gRPC server + tests
    ├── server.py
    ├── server_test.py
    ├── cmd/main.py
    └── pb/             Generated gRPC stubs
internal/
├── runtimed/          Runtimed controller (claim + gRPC delegation)
├── controller/        Runtime controller (Deployment creation)
├── scheduler/         Run reconciler + scheduling strategies
└── krt/               CLI subcommands (run, get, list)
charts/
├── kruntimes/          Platform Helm chart (CRDs, scheduler, controller)
└── kruntimes-runtimes/ Built-in runtimes Helm chart
test/
├── e2e/               End-to-end tests (kind cluster)
└── integration/       Integration tests (envtest)
```

## License

Apache 2.0
