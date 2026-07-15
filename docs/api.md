# API Reference

kruntimes exposes Kubernetes CRDs and a local Runtime Server gRPC API.

## Kubernetes APIs

All CRDs are currently `apiVersion: kruntimes.io/v1alpha1`.

### Run

`Run` represents one execution.

Common spec fields:

| Field | Description |
| --- | --- |
| `spec.runtime` | Runtime name to execute on. Scheduler only considers Runtime Pods in the same namespace. |
| `spec.env` | Environment variables for the execution. Do not store secrets directly here. |
| `spec.source` | Optional source files or Git source prepared into the workspace. |
| `spec.mode.task.entrypoint` | Relative path inside the workspace for one-shot task execution. Absolute paths and `..` are rejected. |
| `spec.mode.task.args` | Arguments or command payload passed to the Runtime Server for one-shot task execution. |
| `spec.mode.function.handler` | Callable `module.function` entrypoint for function-mode Runs. |
| `spec.timeoutSeconds` | Execution timeout. Timeout terminal phase is `Timeout`. |
| `spec.retryPolicy` | Retry attempts and backoff. Execution is at-least-once. |
| `spec.cancelRequested` | User cancellation request. |

Execution input semantics:

- `spec.source.inline` is a standalone script. When it is present, runtimed
  writes it to the default `script` file and does not pass task `entrypoint` or
  `args` to the Runtime Server.
- `spec.mode.task.entrypoint` selects the relative file path inside the
  workspace to execute for Git source or files already present in the
  workspace.
- When `spec.mode.task.entrypoint` is used, `spec.mode.task.args` are passed as
  arguments to that entrypoint.
- When no source or entrypoint file is prepared, task args are interpreted by
  the selected Runtime. Built-in Bash treats a single arg as `bash -c <arg>`,
  preserves explicit `sh -c ...` and `bash -c ...`, and keeps the legacy
  multi-arg behavior of joining args as newline-separated Bash script lines.
  Built-in Python runs `python <args...>`.
- `spec.mode` is required. Exactly one of `spec.mode.task` or
  `spec.mode.function` must be set.
- The `krt run -- <command> [args...]` CLI stores command words directly in
  `spec.mode.task.args`. It does not add shell quoting. Use
  `krt run -- sh -c '...'` for shell evaluation, or use `--file` for inline
  source mode.

Common status fields:

| Field | Description |
| --- | --- |
| `status.phase` | `Pending`, `Scheduled`, `Running`, `Succeeded`, `Failed`, `Timeout`, or `Cancelled`. |
| `status.assignedPod` | Runtime Pod selected by the scheduler. |
| `status.attempt` | Current deterministic attempt count. |
| `status.outputs` | Bounded structured outputs from `$KRUNTIME_OUTPUTS`. |
| `status.artifactRefs` | Compact artifact references for files stored outside etcd. |
| `status.conditions` | Kubernetes list-map conditions for lifecycle states. |

Minimal example:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  source:
    inline: |
      echo hello
```

Task mode example:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello-task
spec:
  runtime: bash
  mode:
    task:
      args:
        - echo hello
```

Function mode is experimental. The API shape is present so handler
configuration lives under function mode, but repeated low-latency invocation
requires the runtime gateway and function runtime contract work tracked in the
roadmap.

### Runtime

`Runtime` defines a warm execution pool.

Common spec fields:

| Field | Description |
| --- | --- |
| `spec.replicas` | Desired Runtime Pod count. |
| `spec.capacity.resources` | Per-pod logical capacity, including built-in `runs`. |
| `spec.template` | `PodTemplateSpec` for Runtime Pods. |
| `spec.daemonImage` | Optional override for the injected `runtimed` sidecar image. |
| `spec.artifactStore` | Artifact backend configuration snapshot used by runtimed and maintainers. |
| `spec.workspace` | Shared workspace volume. It defaults to an `emptyDir`; it can inline Kubernetes `VolumeSource` fields such as `persistentVolumeClaim`. |

The controller owns reserved Runtime Pod fields needed by kruntimes, including
the injected `runtimed` container and control-plane labels/annotations.

Workspace examples:

```yaml
spec:
  workspace:
    emptyDir:
      sizeLimit: 10Gi
```

```yaml
spec:
  workspace:
    persistentVolumeClaim:
      claimName: bash-workspace
```

### PersistentWorkspace

`PersistentWorkspace` represents a named workspace boundary that can later be
referenced by Runs and Workflow-managed jobs. It is not Workflow-specific.

Current spec fields:

| Field | Description |
| --- | --- |
| `spec.runtime` | Runtime whose workspace volume backs this workspace. |
| `spec.mode` | Binding mode. The first supported value is `RuntimePodLocal`. |
| `spec.ttlSecondsAfterUnused` | Optional retention window after the workspace becomes unused. |
| `spec.cleanupPolicy` | Cleanup behavior. Supported values are `DeleteAfterTTL` and `Retain`. |

Current status fields:

| Field | Description |
| --- | --- |
| `status.phase` | Lifecycle phase: `Pending`, `Bound`, `Lost`, or `Released`. |
| `status.runtime` | Observed Runtime name. |
| `status.boundPod` | Runtime Pod backing the workspace once binding is implemented. |
| `status.path` | Runtime-local workspace path once binding is implemented. |
| `status.lastUsedTime` | Last observed use time. |
| `status.conditions` | Lifecycle and validation conditions. |

The initial controller validates and records lifecycle status only. Runtime Pod
binding, Run workspace references, and cleanup are tracked in the roadmap.

### Workflow

`Workflow` defines a reusable workflow. It is a definition object, not an
execution instance. Create `WorkflowRun` objects to execute inline jobs or to
call reusable Workflows.

Current spec fields:

| Field | Description |
| --- | --- |
| `spec.inputs` | Optional typed string inputs accepted by this Workflow. |
| `spec.outputs` | Optional expression-based outputs exposed by this Workflow. |
| `spec.jobs` | Reusable jobs. Each job currently supports either inline `steps` or namespace-local `uses`. |

Current status fields:

| Field | Description |
| --- | --- |
| `status.conditions` | Definition-level readiness and validation conditions. The skeleton controller records `Ready=True`. |

Workflow execution moved to the `WorkflowRun` API. Namespace-local `uses`
resolution, input binding, output propagation, and WorkflowRun execution are
tracked in the roadmap.

### WorkflowRun

`WorkflowRun` is the execution-instance API for the reusable workflow model.
The controller currently resolves top-level reusable Workflow references and
inputs, executes inline jobs as sequential step Runs, derives step and job
status, aggregates settled jobs into a terminal WorkflowRun phase, and
propagates cancellation to active child Runs. Reusable job calls, Action
expansion, and output propagation remain in the roadmap.

Before initializing the status graph or creating child Runs, the controller
rejects inline and resolved reusable Workflow job graphs with unknown
dependencies or dependency cycles. The rejection message includes a stable
cycle path for diagnosis.

Current spec shape:

| Field | Description |
| --- | --- |
| `spec.jobs` | Inline jobs to execute. Exactly one of `spec.jobs` or `spec.uses` must be set. |
| `spec.uses` | Namespace-local reusable Workflow name to execute. Exactly one of `spec.jobs` or `spec.uses` must be set. |
| `spec.with` | String inputs passed to the reusable Workflow named by `spec.uses`. |
| `spec.cancelRequested` | Requests cancellation. The controller stops creating child Runs, sets `cancelRequested` on every active child Run, and waits for them to settle. |

Current status fields:

| Field | Description |
| --- | --- |
| `status.phase` | `Pending`, `Running`, `Succeeded`, `Failed`, or `Cancelled`. After all jobs settle, the controller sets `Failed` if any job failed and `Succeeded` otherwise. A cancellation request results in `Cancelled` after active child Runs settle. |
| `status.jobs` | Lightweight resolved job status keyed by job name. Each job records `pre` and ordered step statuses. |
| `status.conditions` | Lifecycle conditions. The skeleton controller records `Accepted=True`. |

Job phases are `Pending`, `Waiting`, `Running`, `Succeeded`, `Failed`, and
`Skipped`. The controller transitively marks a job `Skipped` when it is blocked
by a failed or skipped dependency, never creates child Runs for it, and
continues independent jobs.
During WorkflowRun cancellation, jobs that never started retain their existing
`Pending` or `Waiting` phase rather than becoming `Skipped`.

### Action

`Action` defines a reusable step group for the target WorkflowRun model. It is a
definition object, not an execution instance.

Current spec fields:

| Field | Description |
| --- | --- |
| `spec.inputs` | Optional typed string inputs accepted by the Action. |
| `spec.outputs` | Optional expression-based outputs exposed by the Action. |
| `spec.steps` | Ordered reusable steps. The first version supports `run` steps only. |

Current status fields:

| Field | Description |
| --- | --- |
| `status.conditions` | Definition-level readiness and validation conditions. |

The initial controller records definition readiness only. Namespace-local
`uses` resolution, input binding, output propagation, and WorkflowRun execution
are tracked in the roadmap.

## Runtime Server gRPC API

Runtime Servers implement `api/runtime/v1/runtime.proto`:

```protobuf
service Runtime {
  rpc Execute(ExecuteRequest) returns (ExecuteResponse);
  rpc Status(StatusRequest) returns (StatusResponse);
  rpc List(ListRequest) returns (ListResponse);
  rpc Cancel(CancelRequest) returns (CancelResponse);
  rpc Forget(ForgetRequest) returns (ForgetResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
}
```

See [Custom Runtime Development Guide](custom-runtime.md) for behavior
requirements, retries, cancellation, workspace paths, and compatibility rules.

## Authentication and Authorization

Kubernetes RBAC controls access to CRDs and pod port-forwarding. Runtime Server
gRPC endpoints are local to Runtime Pods and are not exposed as Services by
default. NetworkPolicy restricts direct access to runtimed endpoints.

See [Security and Threat Model](security.md) for recommended role separation.

## Validation

CRDs include schema and CEL validation for supported fields, sizes, names,
entrypoints, and workflow shapes. Contributors should regenerate CRDs when API
types change; see the [Development Guide](development.md).
