# Function Mode and Agent Sandboxes

This document describes a target v0.x design. It is not implemented yet.

The goal is to let kruntimes serve as a low-latency sandbox execution substrate
for agent platforms. An agent should be able to reserve a warm Runtime Pod,
prepare a callable function or actor, invoke it many times through a dataplane
API, and release it without putting every invocation through Kubernetes
reconciliation.

## Motivation

One-shot Runs are useful for short tasks, CI steps, and automation commands.
Agent workloads often need a different execution shape:

- an LLM planner decides what task to run;
- generated scripts or sub-agent tasks need an isolated execution environment;
- multiple invocations should reuse the same prepared code, environment, and
  workspace;
- the invoke path must be fast enough for interactive agent loops;
- high-frequency invocations should not write unbounded history to etcd.

Kubernetes remains the lifecycle control plane. The invoke path should be a
runtime dataplane path.

## Goals

- Use `Run` as the lifecycle object for both one-shot tasks and function-mode
  sandboxes.
- Add `Run.spec.mode.function` so a Run can reserve a Runtime Pod and stay
  callable until deletion or idle timeout.
- Expose a stable runtime gateway endpoint from Run status.
- Route invoke requests through runtimed to the Runtime Pod that owns the Run.
- Keep scheduler and runtimed generic. They should not understand agent,
  workflow, or MCP semantics.
- Provide SDKs so agent developers do not need to hand-roll Kubernetes watches,
  gateway discovery, port-forwarding, cleanup, and error handling.

## Non-Goals

- kruntimes does not become an agent framework.
- kruntimes does not own prompt management, model routing, memory, tool
  catalogs, or multi-agent planning.
- Function mode is not a replacement for Workflow APIs.
- Function mode does not make built-in runtimes hostile-code sandboxes by
  default. Stronger isolation remains a separate runtime and deployment choice.

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
  name: kube-diagnose-agent
spec:
  runtime: python
  source:
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
    url: https://python-gateway.kruntimes-demo.svc.cluster.local/v1/namespaces/kruntimes-demo/runs/kube-diagnose-agent/2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6/invoke
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

This is important for keeping the scheduler generic. Function mode should not
imply Pod exclusivity. The scheduler only decides whether a Runtime Pod has
capacity for another Run; it does not know whether that Run represents an agent
sandbox, an internal tool, or another product-level concept.

Agent sandbox use cases are different. They often expect strong workspace
ownership, predictable cleanup, and fewer surprising cross-run interactions. For
that case, the recommended deployment shape is one function-mode Run per
Runtime Pod:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: python-agent-sandbox
spec:
  capacity:
    resources:
      runs: "1"
```

SDKs can provide guardrails for this recommendation. For example, an agent
sandbox SDK may warn when the target Runtime has capacity greater than one, or
offer a helper that creates or selects a dedicated Runtime configured with
`runs: "1"`. The guardrail should live in the SDK or higher-level integration;
the scheduler should continue to enforce only the generic capacity contract.

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

Each Runtime should get a gateway Service:

```text
python-runtime-gateway Service
  -> Runtime Pods for Runtime=python
     -> runtimed sidecar
        -> local Runtime Server
```

The Service address is stable. Kubernetes Service load balancing can send an
invoke request to any runtimed in that Runtime pool, so each runtimed needs an
ownership cache:

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

## Agent SDKs

Agent developers should not need to assemble this flow manually. The first SDKs
should target Python and Go.

The SDK should expose sandbox semantics, even though the kruntimes control-plane
object underneath is a function-mode Run. This matches the developer model used
by projects such as Kubernetes SIG Apps agent-sandbox: callers create or attach
to a sandbox, run commands or invoke tools, transfer files, disconnect when they
want to preserve the session, and terminate when they want cleanup.

An SDK should provide a sandbox-facing API:

- create, open, reattach to, disconnect from, and terminate a sandbox session;
- hide the function-mode Run object unless the caller asks for low-level
  Kubernetes metadata;
- wait for the underlying Run to become `Ready`;
- discover the runtime gateway endpoint;
- optionally verify that the selected agent-sandbox Runtime is configured for
  one Run per Runtime Pod;
- use direct in-cluster URLs when running inside Kubernetes;
- fall back to port-forwarding for local development;
- invoke tools or run commands with typed request and response objects;
- expose file operations as sandbox operations, for example write, read, list,
  and exists;
- read outputs, artifacts, and logs through sandbox methods;
- apply timeouts and retries for idempotent operations;
- reconnect after local network interruption;
- clean up the underlying function-mode Run by default, with explicit options to
  preserve, disconnect, or reattach.

Example shape:

```python
from kruntimes import SandboxClient

client = SandboxClient(namespace="kruntimes-demo")

with client.create_sandbox(
    name="kube-diagnose-agent",
    runtime="python",
    source_file="agent_tool.py",
    idle_timeout_seconds=600,
) as sandbox:
    sandbox.files.write("request.json", b'{"namespace":"default"}')

    result = sandbox.commands.run({
        "task": "diagnose-kubernetes",
        "clusterSnapshot": {
            "namespace": "default",
            "pods": []
        },
    })

    report = sandbox.files.read("report.md")
    print(result.outputs["summary"])
```

The SDK API can use `Sandbox` as a developer-facing concept without introducing
a Kubernetes `Sandbox` CRD. A sandbox handle maps to a function-mode Run plus
gateway connection state, file/log/artifact helpers, and lifecycle cleanup.

Recommended SDK objects:

- `SandboxClient`: owns Kubernetes client configuration, gateway discovery, and
  tracked sandbox sessions.
- `Sandbox`: represents one opened sandbox handle and exposes lifecycle,
  command/tool, file, log, artifact, and identity helpers.
- `Commands` or `Tools`: executes a command or structured tool request inside
  the sandbox.
- `Files`: uploads, downloads, lists, and checks workspace files.
- `Info`: read-only identity metadata such as Run name, Run UID, namespace,
  Runtime, assigned Pod, gateway URL, and readiness.

The SDK should support at least three connection modes:

- gateway mode for production traffic through the runtime gateway Service or an
  external Gateway;
- local port-forward mode for development and CI, without requiring users to
  call `kubectl port-forward` manually;
- direct URL mode for in-cluster agents or custom domains.

Retry behavior should distinguish idempotent operations from execution. File
read/write/list/exists and readiness checks can retry on transient transport
errors. Tool or command execution should default to one attempt unless the caller
opts in, because arbitrary execution may not be idempotent.

Typed errors should make recovery explicit, for example not ready, timeout,
gateway unavailable, port-forward died, sandbox deleted, orphaned Run,
retries exhausted, and non-OK invoke response.

## Workspace, Files, Logs, and Artifacts

Agent tasks commonly need to upload generated scripts, inspect files, and fetch
reports. Those operations should not become Kubernetes reconciliation loops.

Required APIs:

- upload file or directory into the function workspace;
- list and read workspace files;
- stream logs for the function Run and individual invokes;
- publish artifacts through the configured ArtifactStore;
- return artifact references from invoke responses;
- clean workspace state on deletion or idle timeout.

For v0.x, workspace operations can be limited to trusted agent integrations.
Multi-tenant production use needs clear RBAC, network policy, and runtime
isolation guidance.

## Integration Boundary

Agent frameworks and MCP-style tool servers can integrate with kruntimes by
mapping a tool call to a function-mode Run:

```text
Agent / MCP tool server
  -> kruntimes SDK
     -> Run lifecycle through Kubernetes API
     -> invoke through runtime gateway
     -> structured result back to agent
```

kruntimes should not require an agent framework to adopt kruntimes-specific
planning concepts. The integration boundary is a sandbox handle plus invoke
API.

## Reliability and Security Requirements

Function mode needs E2E coverage for:

- function registration and ready status;
- local invoke and proxied invoke;
- repeated invocation;
- artifact reuse;
- idle timeout;
- explicit release;
- runtime pod restart recovery;
- cleanup;
- service account selection;
- runtime pod security context;
- resource limits;
- network policy guidance.

Future stronger runtime backends such as gVisor, Kata, or Firecracker should
fit behind the same Runtime abstraction.

## Implementation Sequence

1. Add the API design and validation for mutually exclusive `spec.mode.task`
   and `spec.mode.function`.
2. Remove top-level `Run.spec.handler`, `Run.spec.entrypoint`, and
   `Run.spec.args`; use `Run.spec.mode.function.handler` and
   `Run.spec.mode.task` instead.
3. Add runtime gateway Service reconciliation for each Runtime.
4. Add runtimed ownership cache and invoke routing.
5. Add Runtime Server register, invoke, unregister, and status APIs.
6. Implement built-in Bash/Python function-mode adapters.
7. Add `krt invoke` and the first SDK shape.
8. Add E2E tests covering ready, invoke, proxy, cleanup, and restart recovery.
9. Update the agent demo from a target design to a supported path.
