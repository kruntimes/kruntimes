# Function Mode

> The shared Runtime Gateway and `Run.status.endpoint` were removed. Invoke a
> ready Function Run through the aggregated `functions.kruntimes.io`
> `runs/function` API; see [Run extension API](run-extension-api.md).

This document describes the v0.x Function-mode design and its implemented
control-plane and invoke-dataplane baseline. Deferred work is called out in
the [roadmap](../roadmap/).

The goal is to let kruntimes expose low-latency fixed-handler invocations
without putting every invocation through Kubernetes reconciliation. Mutable
workspaces, arbitrary commands, and file operations belong to the separate
[Session Mode for Agent Sandboxes](../session-mode/) design.

## Motivation

One-shot Runs are useful for short tasks, CI steps, and automation commands.
Some workloads instead expose a stable operation through a fixed handler:

- a caller invokes the same handler repeatedly with different bounded inputs;
- repeated invocations reuse prepared source and the Runtime Server's
  registration state;
- the invoke path must be fast enough for request-response use cases;
- high-frequency invocations should not write unbounded history to etcd.

Kubernetes remains the lifecycle control plane. The invoke path should be a
runtime dataplane path.

## Goals

- Use `Run` as the lifecycle object for both one-shot tasks and fixed-handler
  functions.
- Add `Run.spec.mode.function` so a Run can reserve a Runtime Pod and stay
  callable until deletion or idle timeout.
- Expose a stable runtime gateway endpoint from Run status.
- Route invoke requests through runtimed to the Runtime Pod that owns the Run.
- Keep scheduler and runtimed generic. They should not understand agent,
  workflow, or MCP semantics.

## Non-Goals

- kruntimes does not become an agent framework.
- kruntimes does not own prompt management, model routing, memory, tool
  catalogs, or multi-agent planning.
- Function mode is not a replacement for Workflow APIs.
- Function mode does not provide an arbitrary-command sandbox, mutable
  workspace API, or file API.

## Proposed Run Model

`spec.source` describes where the code or files come from. It is shared by task
and function modes.

`spec.mode` is a mutually exclusive mode-specific configuration object:

```go
type RunMode struct {
    Task     *TaskMode     `json:"task,omitempty"`
    Function *FunctionMode `json:"function,omitempty"`
}

type TaskMode struct {
    Entrypoint string   `json:"entrypoint,omitempty"`
    Args       []string `json:"args,omitempty"`
}

type FunctionMode struct {
    Handler            string `json:"handler,omitempty"`
    IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`
}
```

Exactly one of `mode.task` or `mode.function` must be set.

One-shot task Runs remain the default. `entrypoint` and `args` belong to task
mode because they describe how to start a process once:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: summarize-once
spec:
  runtime: python
  source:
    inline: |
      print("hello")
  mode:
    task:
      entrypoint: main.py
      args:
        - --verbose
```

Function-mode Runs reserve a Runtime Pod and register callable code. `handler`
belongs to function mode because it identifies the callable function entrypoint,
similar to AWS Lambda's `filename.function` convention:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: diagnose-service
spec:
  runtime: python
  source:
    inlinePath: main.py
    inline: |
      def invoke(request):
          return {
              "outputs": {
                  "summary": "diagnosis complete"
              }
          }
  mode:
    function:
      handler: main.invoke
      idleTimeoutSeconds: 600
```

The Run is ready when runtimed has prepared the source, registered it with the
local Runtime Server, and can accept invoke traffic:

```yaml
status:
  phase: Ready
  assignedPod: runtime-python-7f587b4668-njcks
  endpoint:
    protocol: HTTPS
    url: https://runtime-gateway.kruntimes-system.svc.cluster.local/v1/namespaces/kruntimes-demo/runtimes/python/runs/2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6:invoke
    caBundle: <base64-encoded-PEM>
  conditions:
    - type: Ready
      status: "True"
      reason: FunctionRegistered
```

The exact phase, endpoint, retry, timeout, cleanup, routing, authorization, and
invocation semantics are defined in
[Function Mode Lifecycle and Invoke Dataplane](../function-mode-lifecycle/).

`Ready` is not terminal for function-mode Runs. Deletion, cancellation, failed
registration, or idle timeout ends the reservation.

## Scheduling and Capacity

Function-mode Runs still use the normal Runtime capacity model. A Runtime Pod
can own more than one function-mode Run when the Runtime capacity allows it. For
example, a Runtime with `runs: "2"` can register two ready function-mode Runs on
the same Runtime Pod.

This keeps the scheduler generic. Function mode does not imply Pod exclusivity:
the scheduler only decides whether a Runtime Pod has capacity for another Run.
Session Mode, in contrast, requires exclusive v0 capacity because it owns a
mutable workspace.

## Handler Field Placement

Earlier drafts used a top-level handler field:

```yaml
spec:
  handler: module.function
```

The handler concept is still useful. It is common in FaaS systems, including
AWS Lambda, where a handler selects the concrete callable entrypoint. The
problem is its location. A top-level `handler` sits next to task-only concepts
such as `entrypoint` and `args`, which makes the execution model harder to
understand.

The API keeps handler under function mode:

```yaml
spec:
  source:
    git:
      url: https://github.com/example/tools.git
      ref: main
  mode:
    function:
      handler: diagnose.invoke
```

Top-level `handler`, `entrypoint`, and `args` fields are not part of the target
Run API. Task mode keeps `entrypoint` and `args` under `mode.task`, while
function mode keeps `handler` under `mode.function`.

## Runtime Gateway

The detailed gateway routing and authorization contract is defined in
[Function Mode Lifecycle and Invoke Dataplane](../function-mode-lifecycle/).

All Runtimes share one `runtime-gateway` Deployment and its ClusterIP Service.
The Helm chart installs them when `gateway.enabled` is true. The gateway
Deployment runs stateless HTTP servers. A Run endpoint identifies the namespace,
Runtime, and Run UID; the gateway calls the Kubernetes Service for that Runtime,
which selects a ready Runtime Pod before runtimed resolves the owner:

```text
client
  -> shared runtime-gateway Service
     -> runtime-gateway Pod
        -> Kubernetes Service for Runtime=python
           -> ready Runtime Pod's runtimed
              -> owning runtimed when different
                 -> local Runtime Server
```

The gateway Service address is stable. Each Runtime controller-created Service
selects its ready Pods, and each runtimed resolves ownership only for Runs of
its own Runtime:

```text
Run namespace/name/UID -> assigned Runtime Pod UID -> attempt -> readiness
```

Invoke behavior:

- if the request lands on the owning runtimed, invoke the local Runtime Server;
- if the request lands on another runtimed, proxy to the owning Runtime Pod;
- if the Run is not ready, return a typed 409 or 503 error;
- if the Run does not exist or is not owned by the Runtime, return 404;
- do not synchronously read the Kubernetes API on the invoke path.

## Runtime Server Contract

Runtime Servers need a function-mode contract in addition to one-shot execute:

- `RegisterFunction`: prepare code for a Run UID and ownership attempt.
- `InvokeFunction`: run a request against a registered function.
- `UnregisterFunction`: release runtime-local state.
- `FunctionStatus`: report readiness and runtime-local errors.

The idempotency, fencing, timeout, and bounded invoke semantics are defined in
[Function Mode Lifecycle and Invoke Dataplane](../function-mode-lifecycle/).

Invoke responses should contain bounded structured data:

```json
{
  "outputs": {
    "summary": "1 pending pod found",
    "suspected_cause": "insufficient cpu"
  },
  "artifactRefs": [
    {
      "name": "diagnosis.json",
      "uri": "s3://kruntimes-artifacts/runs/kube-diagnose-agent/diagnosis.json"
    }
  ]
}
```

High-frequency invocation history should not be written to `Run.status` by
default. Persisted history can be added later through explicit audit sinks,
metrics, logs, or artifact metadata.

## Reliability and Security Requirements

Function mode needs E2E coverage for:

- function registration and ready status;
- local invoke and proxied invoke;
- repeated invocation;
- idle timeout;
- explicit release;
- runtime pod restart recovery;
- cleanup;

Function invocation remains a bounded request-response API. It does not persist
arbitrary command history or workspace contents in `Run.status`.

## Implementation Sequence

1. Add the API design and validation for mutually exclusive `spec.mode.task`
   and `spec.mode.function`.
2. Remove top-level `Run.spec.handler`, `Run.spec.entrypoint`, and
   `Run.spec.args`; use `Run.spec.mode.function.handler` and
   `Run.spec.mode.task` instead.
3. Add Helm templates and values for the optional shared runtime-gateway
   Deployment and Service.
4. Add runtimed ownership cache and invoke routing.
5. Add Runtime Server register, invoke, unregister, and status APIs.
6. Implement built-in Bash/Python function-mode adapters.
7. Add `krt invoke`.
8. Add E2E tests covering ready, invoke, proxy, cleanup, and restart recovery.
