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
          image: ghcr.io/kruntimes/bash-runtime:0.0.5
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
```

The scheduler watches Pending Runs and assigns them to healthy Runtime Pods in
the same namespace.

## Operate a Session Run with krt

Create a Session-mode Run and wait for its phase to become `Ready`:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: shell-session
spec:
  runtime: bash
  mode:
    session: {}
```

`krt list` and `kubectl get run` show the Run's `MODE` (`task`, `session`, or
`function`). The Kubernetes column is filled by the scheduler after it observes
the Run.

Run one command through the authenticated Session gateway:

```bash
krt session exec -n default shell-session -- sh -c 'echo hello; echo warning >&2'
```

Command words are passed as `argv`; use `sh -c` for shell expansion. `exec`
returns a non-zero CLI status when the operation fails or the command exits
non-zero. It does not allocate a TTY or stream live stdin.

Use `attach` to submit one shell command per line on the same Session
connection. Each line is a separate operation. The prompt includes the Run
name, for example `shell-session> `:

```bash
krt session attach -n default shell-session
```

For scripts, `attach` accepts one JSON operation per input line and can emit
one JSON event per output line:

```bash
printf '%s\n' '{"command":{"shell":"echo hello"}}' |
  krt session attach -n default shell-session --input ndjson --output ndjson
```

Output events contain base64-encoded `data` fields, as in the gateway wire
format. The default text view writes command output as it arrives and reports
command failures. Use `--show-events` with `exec` or `attach` to see operation
IDs, progress, and completion messages. NDJSON output always includes every
event. Ctrl-C requests cancellation of the active operation before closing the
connection. Session commands connect to the Kubernetes API server using the
current kubeconfig, then stream through the `sessions.kruntimes.io` aggregated
API. The caller needs `get` on the named `kruntimes.io/runs` resource and
`get` on `sessions.kruntimes.io/runs/session`. Client certificates are verified
by the Kubernetes API server; no Console client CA or port-forward is needed.
Closing `attach` does not terminate the Session Run.

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
```

Do not put secrets directly in `Run.spec.env`. Use namespace separation,
Runtime-controlled mounts, or an admission policy appropriate for your cluster.

## Use Inline Source

Inline source is a standalone script. When `spec.source.inline` is present,
runtimed writes it to the default `script` file and ignores task `entrypoint`
and `args`.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: inline-example
spec:
  runtime: bash
  source:
    inline: |
      echo "hello from inline source"
```

## Use Entrypoint and Args

Entrypoints select a relative file path inside the prepared workspace. They are
used for Git source or files already present in the workspace. Entrypoints must
be relative paths and cannot contain `..`.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: script-example
spec:
  runtime: bash
  source:
    repoURL: https://github.com/example/scripts.git
    commitSHA: main
  mode:
    task:
      entrypoint: run.sh
```

When `entrypoint` is used, `args` are passed to that file. For the built-in Bash
Runtime this means:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: script-args-example
spec:
  runtime: bash
  source:
    repoURL: https://github.com/example/scripts.git
    commitSHA: main
  mode:
    task:
      entrypoint: run.sh
      args:
        - hello
```

## Use Args Without Source

When no source or entrypoint file is prepared, `mode.task.args` are interpreted
by the selected Runtime:

- Built-in Bash treats one arg as `bash -c <arg>`.
- Built-in Bash preserves explicit `sh -c ...` and `bash -c ...` invocations.
- Built-in Bash keeps legacy multi-arg behavior by joining args as
  newline-separated Bash script lines.
- Built-in Python runs `python <args...>`.

For shell behavior through the CLI, pass the shell explicitly:

```bash
krt run --runtime bash -- sh -c 'echo "hello from $SHELL"'
```

The CLI stores command words in `spec.mode.task.args`.

For repeatable scripts, prefer source mode:

```bash
krt run --runtime bash --file ./script.sh
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

## WorkflowRun Skeleton

`WorkflowRun` is the execution-instance API for the reusable workflow model.
It executes inline jobs, observes their child Runs, projects bounded step and
job outputs into status, and honors dependency ordering. A job can also call a
namespace-local reusable Workflow.

Create an inline WorkflowRun manifest:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: ci-demo
spec:
  jobs:
    test:
      runs-on: bash
      steps:
        - name: unit
          run: make test
```

Submit and inspect it with `krt`:

```bash
krt wf run -f workflowrun.yaml -n default
krt wf run ls -n default
krt wf run get ci-demo -n default
krt wf run delete ci-demo -n default
```

Create an inline WorkflowRun directly:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: make package
```

`krt wf run -f` also accepts a small GitHub Actions style workflow file and
converts it to an inline `WorkflowRun`.

For reusable Workflow definitions:

```bash
krt wf create -f workflow.yaml -n default
krt wf ls -n default
krt wf trigger build-and-test --set ref=main -n default
krt wf delete build-and-test -n default
```

`krt wf trigger` reads the reusable Workflow, validates the supplied inputs,
renders `inputs.*` expressions into inline jobs, and creates the resulting
`WorkflowRun`.

`krt wf run cancel` is reserved for the future cancellation API and currently
returns a clear unsupported error.

## CLI

The `krt` CLI supports kubeconfig/context, namespace selection, waiting, output
formats, logs, cancellation, and result inspection. Published release binaries
are documented in [Release Process](release.md).
