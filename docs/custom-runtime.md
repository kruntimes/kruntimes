# Custom Runtime Development Guide

Custom Runtimes let you bring a workload-specific execution environment to
kruntimes while leaving Kubernetes watches, Run claiming, retries, artifact
upload, logs, and status updates to the `runtimed` sidecar.

Most custom Runtimes should start by extending an existing Runtime image. You
only need to implement the Runtime Server API when the built-in execution
semantics are not enough.

## Runtime Image Customization

There are two options for customizing a Runtime image:

1. **Extend a built-in Runtime image.** Start from the Bash or Python Runtime
   image, add packages, internal binaries, certificates, SDKs, or config files,
   then register that image as a new `Runtime`. This is the recommended first
   path for most teams.
2. **Implement a Runtime Server.** Build a server that implements the Runtime
   gRPC API when you need different execution semantics, a specialized worker,
   a sandboxing layer, or a non-process execution model.

Both image customization options use the same `Runtime` CRD and the same
`Runtime.spec.template` Pod template model.

### Option 1: Extend a Built-In Runtime Image

Use this path when Bash or Python execution is enough, but the environment
needs extra tools.

Example Bash Runtime image with `jq` installed:

```dockerfile
ARG KRUNTIMES_VERSION=0.0.3
FROM ghcr.io/kruntimes/bash-runtime:${KRUNTIMES_VERSION}

USER 0
RUN apt-get update \
  && apt-get install -y --no-install-recommends jq \
  && rm -rf /var/lib/apt/lists/*
USER 65532
```

Build and push the image to a registry the cluster can pull:

```bash
CUSTOM_BASH_IMAGE=ghcr.io/example/my-bash-runtime:0.1.0
docker build \
  --build-arg KRUNTIMES_VERSION=0.0.3 \
  -t "${CUSTOM_BASH_IMAGE}" \
  ./my-bash-runtime
docker push "${CUSTOM_BASH_IMAGE}"
```

Use the same pattern to add internal CLIs, model tools, CA certificates, or
language packages. Keep the final image user compatible with the security
context you plan to run in.

### Option 2: Implement a Runtime Server Image

Use this path when the built-in Runtime execution model is not enough. A
Runtime Server is a process that listens inside the Runtime Pod and implements
`api/runtime/v1/runtime.proto`.

The Runtime Server API is local to a Runtime Pod. It is not exposed as a
cluster service by default.

The server must implement:

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

When packaging the image, expose the Runtime Server on the port referenced by
`Runtime.spec.port`. The first container in the Runtime Pod template must be
named `runtime`.

## Register the Runtime

Register either kind of custom image with a `Runtime` object:

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
          env:
            - name: EXAMPLE_MODE
              value: production
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "2"
              memory: 2Gi
```

`spec.capacity.resources.runs` controls concurrent Runs per Runtime Pod. The
controller copies static capacity to Pod annotations. The scheduler uses its
Run cache for fast-changing active usage and only assigns to Pods that are
Kubernetes Ready, `kruntimes.io/RuntimedReady`, and below capacity. Runtimed
enforces the same local capacity before claiming a Scheduled Run.

## Customize the Pod Template

`Runtime.spec.template` is a Pod template. Runtime owners can customize:

- runtime container image and args,
- CPU and memory requests and limits,
- environment variables,
- security context,
- ServiceAccount,
- image pull secrets,
- node selectors, tolerations, affinity, and topology spread constraints,
- init containers,
- extra volumes and mounts,
- additional sidecars.

The controller preserves non-conflicting Pod template fields and injects
kruntimes-managed fields:

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

## Runtime Server Semantics

This section applies only when you implement a Runtime Server.

`Execute` starts an execution for the supplied Run ID. The request includes:

- `id`: stable Run UID used as the execution ID,
- `args`: command or payload arguments after runtimed normalizes Run input
  semantics. Inline source is sent as a standalone `script` with empty args.
  If an entrypoint file exists in the workspace, built-in Runtimes pass these
  values to the entrypoint. If no source or entrypoint file is prepared, each
  Runtime documents how it interprets args.
- `env`: environment variables from the Run spec,
- `timeout_seconds`: requested timeout from the Run spec,
- `working_dir`: prepared workspace directory,
- `entrypoint`: relative entrypoint path inside `working_dir`; runtimed sends
  `script` for inline source,
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

`Status` returns retained state: pending, running, succeeded, or failed.
Timeout and cancellation are represented by runtimed at the Run status layer.
The Runtime Server should still terminate work when its own timeout expires or
when `Cancel` is received.

`List` returns retained executions so runtimed can recover active Runs after a
restart. Runtime Servers should retain running and terminal executions until
runtimed calls `Forget`. `Forget` releases terminal execution state after
runtimed has persisted Run status and uploaded artifacts.

`Cancel` should make a best effort to stop the execution and any child
processes. It should be safe to call multiple times.

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

## Security Boundary

Built-in and custom Runtime Servers run trusted code inside warm Runtime Pods
unless the Runtime implementation provides stronger isolation. A custom Runtime
that runs untrusted code should create its own per-Run sandbox, container,
microVM, process isolation, filesystem policy, and network policy.

Do not put credentials in `Run.spec.env`. Prefer Kubernetes Secrets mounted by
trusted Runtime Pods or backend-specific credentials managed outside Run
objects.

## Compatibility and Testing

Custom Runtime authors should treat the Runtime CRD template contract, proto
service, and Run lifecycle semantics as the compatibility surface for `v0.x`.
Because the CRDs are `v1alpha1`, minor releases may still change fields or
behavior. Release notes must call out breaking changes and migration steps.

Before publishing a Runtime image, test:

- normal success and failure,
- timeout and cancellation,
- duplicate or repeated `Execute`,
- runtimed restart recovery through `List`,
- `Forget` cleanup,
- bounded stdout/stderr,
- workspace cleanup and artifact upload.
