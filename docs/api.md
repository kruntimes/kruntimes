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

The controller owns reserved Runtime Pod fields needed by kruntimes, including
the injected `runtimed` container and control-plane labels/annotations.

### Workflow

`Workflow` orchestrates child Runs. Workflow docs are still intentionally
minimal while the API remains experimental.

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
