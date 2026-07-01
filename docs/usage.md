# Usage Guide

This guide covers the common user workflows for Runtime and Run objects.

## Create a Runtime

A Runtime defines a pool of warm Runtime Pods.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: bash
spec:
  replicas: 2
  capacity:
    resources:
      runs: 4
  template:
    metadata:
      labels:
        runtime: bash
    spec:
      containers:
        - name: runtime
          image: kruntimes-bash-runtime:latest
          imagePullPolicy: IfNotPresent
          ports:
            - containerPort: 19091
```

Important fields:

- `spec.replicas`: number of Runtime Pods.
- `spec.capacity.resources.runs`: concurrent Runs per Runtime Pod.
- `spec.template`: Pod template used to create Runtime Pods.
- `spec.template.spec.serviceAccountName`: optional user-defined workload
  ServiceAccount; the controller grants the runtimed permissions it needs in
  the same namespace.

## Create a Run

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
  entrypoint: script
```

The scheduler watches Pending Runs and assigns them to healthy Runtime Pods in
the same namespace.

## Use Environment Variables

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: env-example
spec:
  runtime: bash
  env:
    MESSAGE: hello
  source:
    inline: |
      echo "$MESSAGE"
  entrypoint: script
```

Do not put secrets directly in `Run.spec.env`. Use namespace separation,
Runtime-controlled mounts, or an admission policy appropriate for your cluster.

## Use Source and Entrypoint

Run source is prepared into a per-Run workspace. Entrypoints must be relative
paths and cannot contain `..`.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: script-example
spec:
  runtime: bash
  source:
    inline: |
      echo "hello from source"
  entrypoint: script
```

## Outputs

Structured outputs are written by the workload as `KEY=VALUE` lines to
`$KRUNTIME_OUTPUTS`. runtimed stores bounded outputs in `Run.status.outputs`.

```bash
echo "result=ok" >> "$KRUNTIME_OUTPUTS"
```

## Artifacts

Files below `$KRUNTIME_ARTIFACTS_DIR` are persisted through the configured
ArtifactStore. Run status stores compact `artifactRefs` metadata instead of the
full artifact data.

```bash
mkdir -p "$KRUNTIME_ARTIFACTS_DIR"
echo "artifact body" > "$KRUNTIME_ARTIFACTS_DIR/result.txt"
```

## Cancellation

Set `spec.cancelRequested` to request cancellation:

```bash
kubectl patch run hello --type merge -p '{"spec":{"cancelRequested":true}}'
```

The terminal phase becomes `Cancelled` when cancellation is applied.

## Timeouts and Retries

Runs can define timeouts and retry policy. Timeouts end in the `Timeout`
terminal phase, not generic `Failed`.

Retry behavior is at-least-once. Runtime Servers must make duplicate `Execute`
delivery deterministic and safe.

## Logs

Full stdout and stderr are exposed through structured runtimed logs keyed by
Run UID. They are not copied wholesale into `status.message`.

## CLI

The `krt` CLI supports kubeconfig/context, namespace selection, waiting, output
formats, logs, cancellation, and result inspection. Published release binaries
are documented in [Release Process](release.md).
