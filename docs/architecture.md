# Architecture

kruntimes separates Kubernetes-level capacity management from request-level Run
assignment.

## Components

| Component | Responsibility |
| --- | --- |
| Run CRD | Durable record for one execution: runtime, input, assignment, phase, retry policy, timestamps, outputs, and artifact refs. |
| Runtime CRD | Defines a Runtime Pod pool, capacity, pod template, and artifact store configuration. |
| Runtime Controller | Reconciles Runtime CRs into Deployments, RBAC, NetworkPolicy, and runtime maintainer Deployments. |
| Scheduler | Watches Pending Runs and assigns them to healthy Runtime Pods with available capacity. |
| Runtimed | Sidecar in each Runtime Pod. Claims assigned Runs, calls the local Runtime Server, updates Run status, uploads artifacts, and emits structured logs. |
| Runtime Server | Local gRPC server that performs execution. Built-in implementations include Bash and Python. |
| Runtime Maintainer | Long-running Runtime-scope worker for maintenance that must outlive individual Runtime Pods, including artifact cleanup. |
| Stale Run Reaper | Detects assigned Runs whose Runtime Pod disappeared or stopped heartbeating and applies retry or terminal failure policy. |
| krt | CLI for creating, watching, cancelling, logging, and inspecting Runs. |

## Control Flow

```text
User / krt
  │
  ▼
Run CRD: Pending
  │
  ▼
Scheduler
  │ selects healthy Runtime Pod with available capacity
  ▼
Run CRD: Scheduled + assignedPod
  │
  ▼
runtimed sidecar in assigned Runtime Pod
  │ claims Run and calls local Runtime Server
  ▼
Runtime Server
  │ executes workload
  ▼
runtimed updates Run status, outputs, artifacts, and logs
```

## Runtime Pod Model

```text
Runtime Pod
├── runtimed sidecar
└── runtime container
    └── Runtime Server gRPC endpoint
```

`runtimed` owns Kubernetes communication. Runtime Servers only implement the
local execution protocol.

## Scheduling Model

Runtime Pods expose:

- Kubernetes `PodReady`,
- `kruntimes.io/RuntimedReady` heartbeat,
- runtime labels,
- static capacity annotations.

The scheduler derives fast-changing usage from Run state, not from pod
annotations. A Runtime Pod is a candidate only when it is ready, fresh, and
below capacity.

When a Run stops consuming capacity, the scheduler wakes Pending Runs for the
same namespace and runtime. A periodic retry remains as a fallback.

## State Model

`Run.status.phase` uses these phases:

- `Pending`
- `Scheduled`
- `Running`
- `Succeeded`
- `Failed`
- `Timeout`
- `Cancelled`

Terminal conditions are normalized for failed, timeout, and cancelled outcomes.

## Data Boundaries

Kubernetes stores compact control-plane state:

- lifecycle phase,
- assignment,
- bounded outputs,
- artifact references,
- timestamps,
- conditions.

Large data stays outside etcd:

- full stdout/stderr in structured logs,
- artifact files in the configured ArtifactStore,
- runtime-local execution state until `Forget`.

## Design Decisions

- **No request-time Pod creation**: warm Runtime Pods absorb short Runs.
- **CRDs as source of truth**: Kubernetes remains the durable control plane.
- **Runtime Servers are local**: no global runtime service mesh is required.
- **At-least-once execution**: runtimed and stale reaper share retry semantics.
- **Trusted built-ins**: built-in runtimes trade isolation for low latency.

See [Security and Threat Model](security.md) for isolation limits.
