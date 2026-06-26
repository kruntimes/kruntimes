# Custom Runtime Development Guide

Custom Runtimes let you plug a new execution environment into kruntimes while
leaving Kubernetes watches, Run claiming, retries, artifact upload, logs, and
status updates to the `runtimed` sidecar.

The Runtime Server API is local to a Runtime Pod. It is not exposed as a
cluster service by default.

## Contract

A Runtime Server must implement `api/runtime/v1/runtime.proto`:

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

The generated Go package is `github.com/kruntimes/kruntimes/api/runtime/v1`.
Other languages should generate clients and servers from the same proto.

## Execution Semantics

`Execute` starts an execution for the supplied Run ID. The request includes:

- `id`: stable Run UID used as the execution ID,
- `args`: command or payload arguments,
- `env`: environment variables from the Run spec,
- `timeout_seconds`: requested timeout from the Run spec,
- `working_dir`: prepared workspace directory,
- `entrypoint`: relative entrypoint path inside `working_dir`,
- `handler`: optional `module.function` handler for runtimes that support
  function-style invocation.

Runtime Servers should either accept the execution and return quickly, or
return a gRPC error. Long-running work should happen asynchronously while
`Status` reports progress.

If `Execute` is called with an ID that already exists, the Runtime Server must
choose deterministic behavior. The built-in Bash Runtime cancels and replaces
the previous execution; the built-in Python Runtime rejects duplicates. Custom
Runtime authors should document the behavior and make it safe for at-least-once
`Execute` delivery.

## Status

`Status` returns the latest retained state:

- `EXECUTION_STATE_PENDING` when work has been accepted but not started,
- `EXECUTION_STATE_RUNNING` while work is active,
- `EXECUTION_STATE_SUCCEEDED` after successful completion,
- `EXECUTION_STATE_FAILED` after runtime-level failure.

Timeout and cancellation are represented by runtimed at the Run status layer.
The Runtime Server should still terminate work when its own timeout expires or
when `Cancel` is received.

`stdout`, `stderr`, and `error_message` must be bounded. Do not retain
unbounded output in memory. Large artifacts should be written to
`$KRUNTIME_ARTIFACTS_DIR`; compact structured outputs should be written to
`$KRUNTIME_OUTPUTS`.

## List and Recovery

`List` returns all retained executions. Runtimed calls it after restart to
rebuild local active Run state.

Runtime Servers should retain running and terminal executions until runtimed
calls `Forget`. If a Runtime Server loses a running execution before runtimed
observes a terminal state, runtimed treats it as `ExecutionLost` and applies
normal retry or terminal failure policy.

## Cancel

`Cancel` should make a best effort to stop the execution and any child
processes. It should be safe to call multiple times.

Recommended behavior:

- return `NotFound` when the execution ID is unknown,
- terminate the whole process group or equivalent execution tree,
- wait briefly for graceful termination,
- force kill when graceful termination does not complete,
- release active execution resources after cancellation completes.

## Forget

`Forget` releases retained terminal execution state and output after runtimed
has persisted terminal Run status and uploaded artifacts.

Runtime Servers should reject `Forget` for active executions with
`FailedPrecondition`. `NotFound` is treated by runtimed as already released.

## Health

`Health` is used by runtimed readiness checks and Kubernetes probes. Return
`healthy=false` with a short message when the Runtime Server cannot accept new
work.

## Workspace and Data Paths

Runtimed prepares source code under `/workspace/<runUID>` and sends that path
as `working_dir`. Runtime Servers should execute only within that directory
unless their documented execution model requires otherwise.

Reserved files:

- `$KRUNTIME_OUTPUTS`: newline-delimited `key=value` file for bounded Run
  outputs,
- `$KRUNTIME_ARTIFACTS_DIR`: directory where user code may write artifact
  files for upload.

Runtime Servers should not write large logs, artifacts, or progress streams to
Run status. Runtimed owns artifact upload and status updates.

## Runtime CRD

Deploy a Runtime Server with a `Runtime` object. The first container in the
template must be named `runtime`.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: my-runtime
spec:
  port: 9091
  replicas: 2
  capacity:
    resources:
      runs: "2"
  template:
    spec:
      serviceAccountName: my-runtime-runtimed
      containers:
        - name: runtime
          image: ghcr.io/example/my-runtime:0.1.0
          args:
            - --port=9091
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "2"
              memory: 2Gi
```

The controller owns or injects:

- `runtime` and `app` selector labels,
- `grpc` port on the `runtime` container,
- `/workspace` mount on the `runtime` and `runtimed` containers,
- the `runtimed` sidecar,
- `workspace` and `artifact-store` volumes,
- Runtime Pod NetworkPolicy,
- namespace-scoped RBAC for the selected ServiceAccount.

User-provided entries using the reserved names `runtimed`, `workspace`, or
`artifact-store` are ignored or replaced. The artifact store volume is not
mounted into user containers.

The controller preserves custom probes, resources, security contexts, labels,
annotations, scheduling constraints, image pull secrets, init containers, and
additional sidecars when they do not conflict with reserved fields.

## Capacity

Set `spec.capacity.resources.runs` to control concurrent Runs per Runtime Pod.
The controller copies static capacity to Pod annotations. The scheduler uses
its Run cache for fast-changing active usage and only assigns to Pods that are
Kubernetes Ready, `kruntimes.io/RuntimedReady`, and below capacity. Runtimed
enforces the same local capacity before claiming a Scheduled Run.

## Security Boundary

Built-in and custom Runtime Servers run trusted code inside warm Runtime Pods
unless the Runtime implementation provides stronger isolation. A custom Runtime
that runs untrusted code should create its own per-Run sandbox, container,
microVM, process isolation, filesystem policy, and network policy.

Do not put credentials in `Run.spec.env`. Prefer Kubernetes Secrets mounted by
trusted Runtime Pods or backend-specific credentials managed outside Run
objects.

## Compatibility

Custom Runtime authors should treat the proto service and Run lifecycle
semantics as the compatibility surface for `v0.x`. Because the CRDs are
`v1alpha1`, minor releases may still change fields or behavior. Release notes
must call out breaking changes and migration steps.

Before publishing a Runtime image, test:

- normal success and failure,
- timeout and cancellation,
- duplicate or repeated `Execute`,
- runtimed restart recovery through `List`,
- `Forget` cleanup,
- bounded stdout/stderr,
- workspace cleanup and artifact upload.
