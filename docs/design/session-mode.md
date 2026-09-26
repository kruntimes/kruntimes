# Session Mode for Agent Sandboxes

Status: **Accepted; implementation in progress**

## Problem

`Run.mode.function` is a fixed-handler RPC model. It is appropriate for FaaS
and tool endpoints, but it is not an agent sandbox: an agent sandbox needs a
mutable workspace, arbitrary commands, file operations, ordered multi-step
work, and a session lifecycle.

Session mode uses the existing `Run` lifecycle object rather than a new
`Sandbox` CRD. A session Run reserves warm Runtime capacity and exposes one
stateful execution environment until it is closed, expires, fails, or is
deleted.

## Run API

`Run.spec.mode` is exactly one of `task`, `function`, or `session`:

```go
type RunMode struct {
    Task     *RunTaskMode     `json:"task,omitempty"`
    Function *RunFunctionMode `json:"function,omitempty"`
    Session  *RunSessionMode  `json:"session,omitempty"`
}

type RunSessionMode struct {
    IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`
    QueueSize          *int32 `json:"queueSize,omitempty"`
    OperationTimeout   *metav1.Duration `json:"operationTimeout,omitempty"`
}
```

Run termination is an independent, monotonic control-plane request:

```yaml
spec:
  termination:
    mode: Drain # or Immediate
```

`Immediate` cancels work. `Drain` is valid only for Session Runs and requests a
successful finalization after already accepted operations complete. A request
cannot be removed or downgraded; a draining Session may be escalated to
`Immediate` when it must stop without exporting a successful result.

Session Runs use the existing `Pending -> Scheduled -> Running -> Ready`
lifecycle. `Ready` means the owning runtimed has registered a session with the
local Runtime Server and accepts session operations. It remains active and
holds Runtime capacity.

## Registration Reconciliation

Session registration is deliberately split between local work and Kubernetes
status reconciliation. When runtimed observes its assigned `Scheduled` Run, it
claims the Run and updates its status to `Running`. A later reconciliation of
that cached `Running` Run queries the local Runtime Server with
`GetSessionStatus`.

- `READY` transitions the Run to `Ready`, publishes its endpoint, and starts
  idle tracking.
- `NOT_FOUND` starts asynchronous source preparation and `RegisterSession` if
  no registration is already in flight. While one is in flight, runtimed only
  requeues the Run.
- `REGISTERING` remains `Running` and is requeued.
- `FAILED`, `CLOSED`, `CLOSING`, or an invalid state enter the ordinary Run
  retry path before `Ready`.

The asynchronous worker never reads or writes the Run API object. It records a
local registration failure, emits an Event and log entry, and enqueues the Run.
The next reconciliation consumes that result and applies the normal retry
policy. This keeps every `Run.status` update in the reconcile path and avoids
depending on a direct API read to outrun an informer cache update.

For v0, configure a Runtime used for sessions with effective `runs: 1`
capacity. The scheduler continues to apply only generic resource capacity
accounting. runtimed is the authoritative local gate: it atomically refuses to
claim a Session while any Run is active, and refuses to claim any Run while a
Session is active. This prevents mixed execution even during local races or a
misconfigured Runtime; a Run rejected by that gate remains `Scheduled` until
the Runtime Pod becomes available.

`source` and `artifactInputs` initialize the session before it becomes Ready.
They are not command definitions. Session Runs must not set `spec.workspace`:
v0 creates one Run-UID-scoped ephemeral workspace on the assigned Runtime Pod.
Files, installed dependencies, and process-visible state persist across session
operations on that Pod. Pod loss fails the session; v0 has no checkpoint,
resume, or transparent migration promise.

`Run.spec.env` is captured during registration and is available to every
session command. A command may supply its own environment map; those values
override the registered values for that command only.

`Run.spec.timeout` bounds the entire reservation. `idleTimeoutSeconds` expires
the session after no accepted mutation or command activity. A Session Run uses
the normal Run cancellation, deletion, TTL, authorization, endpoint, and
assignment-UID fencing rules. Registration can retry before `Ready`, when no
usable session state exists. Once Ready, an assigned-Pod loss is terminal: the
client must create a new Session Run rather than silently continuing in an
empty workspace. Idle expiry is also terminal: it closes the local session,
cleans its ephemeral workspace, and records `RunTimeout`. Reopening or
resubmitting the same Run cannot restore it; a client that needs a new sandbox
must create a new Session Run. Explicit suspend and resume semantics are a
separate v1 design item, not an implicit consequence of timeout recovery.

## Deletion Cleanup

Before Session or Function registration, runtimed adds the shared
`kruntimes.io/registration-cleanup` finalizer. Deletion is different from a
normal terminal phase: Kubernetes may delete a Run while it is `Running`,
`Ready`, or `Finalizing`, and status updates are not the deletion protocol.
The finalizer keeps the object present until its owning runtimed has stopped
new data-plane work, idempotently called `CloseSession` or
`UnregisterFunction`, released local capacity, and removed only Run-local
state. It is removed only after that cleanup succeeds; a transient close error
keeps the finalizer and requeues reconciliation.

The finalizer never deletes a referenced PersistentWorkspace or ArtifactStore
data. Those resources retain their own lifecycle owners. After a runtimed
restart, the owner recovers an idempotent local registration before closing it;
it must not silently remove the finalizer merely because its in-memory handle
was lost.

On a normal Runtime Pod shutdown, runtimed drains current `Running`, `Ready`,
and `Finalizing` registrations before exit and removes this finalizer only
after the Pod-local close or unregister succeeds. It leaves the fenced
`PodTerminating` status transition to the stale Run reaper, which also monitors
`Finalizing`; hard Pod loss still relies on that reaper because it bypasses the
best-effort drain.

## Completion and Artifact Export

Cancellation and successful sandbox completion use the same monotonic
`spec.termination` request with distinct modes. `Immediate` is terminal
cancellation. `Drain` is the normal end of useful Session work. The SDK's
successful `Close` helper sets `termination.mode: Drain`; its `Cancel` helper
sets `termination.mode: Immediate`. The alpha API replaces the older
`cancelRequested` boolean rather than carrying both controls.

When completion is requested, the controller transitions the Run from `Ready`
to the active `Finalizing` phase. The gateway rejects new operations in that
phase. Owner runtimed drains already accepted operations up to their individual
operation deadlines, then closes the local Runtime Server session. This freezes
the ephemeral workspace before final collection.

If the Runtime has an ArtifactStore, runtimed creates `$KRUNTIME_ARTIFACTS_DIR`
when it prepares the Session workspace. A client writes explicitly exported
files there. After the local session is closed, runtimed validates and uploads
those files using the ordinary Run ArtifactStore contract, records the compact
refs in `Run.status.artifactRefs`, and only then transitions the Run to
`Succeeded`. It never writes command history, arbitrary file contents, or
unbounded output into Run status.

An ArtifactStore transport failure leaves the Run in `Finalizing`, retains the
local workspace, and is retried. An invalid artifact is terminal `Failed` with
an artifact-specific reason. `Immediate` termination requested during
finalization wins: it stops draining and artifact export, closes the session,
and records `Cancelled`. Timeout and assigned-Pod loss likewise retain their
existing terminal semantics; they do not claim a successful export of an
incomplete workspace.

## Services and Request Flow

The following terms are distinct:

- **Runtime gateway** is one shared `runtime-gateway` Deployment installed by
  the Helm chart when `gateway.enabled` is true. Its Pods run the gateway server
  for all Runtimes in the cluster. It is stateless: it does not own a session,
  queue operations, or call a Runtime Server directly.
- **Runtime gateway Service** is the shared Kubernetes `ClusterIP` Service for
  the Runtime gateway Deployment. It is the stable HTTP address carried in
  `Run.status.endpoint`.
- **Runtime gateway server** is the HTTP server in each Runtime gateway Pod.
  Kubernetes Services only forward traffic; they cannot translate HTTP requests
  to gRPC. The gateway server resolves the current Session Run assignment and
  sends each request through that Runtime's Kubernetes Service to
  `SessionRuntime` gRPC. It does not own a session, select Runtime Pods, or
  queue operations.
- **Runtime Service** is a ClusterIP Service created for every Runtime by the
  Runtime controller. It selects that Runtime's ready Pods and exposes the
  runtimed `session-runtime` port. It is the only Service the gateway uses to
  reach a Runtime Pod.
- **runtimed** runs in every Runtime Pod and implements `SessionRuntime` for
  gateway traffic. A runtimed may receive a request for a session assigned to
  another Runtime Pod.
- **Owner runtimed** is the runtimed in the Runtime Pod assigned to the target
  Session Run. It is the only component that owns that session's FIFO queue,
  operation state, idle timer, and local lifecycle.
- **Runtime Server** is the execution backend colocated with owner runtimed.
  It also implements `SessionRuntime`, but accepts calls only from its local
  runtimed. It never accepts client traffic, authorizes Kubernetes users,
  routes across Pods, or owns the session queue.

The chart, rather than a controller, owns the gateway Deployment, Service, RBAC,
and replicas. Values control whether the component is installed. TLS termination
and external exposure are configured outside this cluster-local Service. The Runtime controller
creates each Runtime Service; the gateway's Kubernetes resources do not change
when a Runtime is created, updated, or deleted.

The request path is therefore:

```text
External client / SDK
  -> Runtime gateway Service (HTTP)
     -> Runtime gateway server in the shared Runtime gateway Deployment
        -> Kubernetes Service for the Session's Runtime
           -> a ready Runtime Pod's runtimed (SessionRuntime gRPC)
              -> owner runtimed (only when the first runtimed is not the owner)
              -> local Runtime Server (SessionRuntime gRPC)
```

The Runtime gateway server exposes a versioned HTTP API and receives the
client's Kubernetes bearer token. Every endpoint path identifies namespace,
Runtime, and immutable Run UID. The gateway server verifies the requested Run,
derives its `SessionIdentity` from the current assignment, and calls that
Runtime's Service. Kubernetes routes the call to one ready Runtime Pod. The
receiving runtimed only lists Runs indexed by its own Runtime name, then verifies
that the requested Run is still a Session Run with the same assignment. If its
Pod UID is not the assigned Pod UID, it forwards the same call once to the owner
runtimed in the same Runtime. A forwarding marker prevents loops. The owner
runtimed performs queue admission and invokes its colocated Runtime Server over
local `SessionRuntime` gRPC.

Before forwarding an HTTP request, the gateway authenticates its bearer token
with Kubernetes `TokenReview` and authorizes access to the exact Run with
`SubjectAccessReview`. To bound this control-plane work, it caches successful
decisions for 30 seconds by default. The in-memory cache is capped at 1024
entries and keys each decision by a SHA-256 token digest plus the Run namespace,
name, and immutable UID. It never retains bearer tokens, denied decisions, or
authorization errors. Set either `--authorization-cache-ttl=0` or
`--authorization-cache-capacity=0` to disable the cache. Authorization changes
can take up to the configured TTL to affect an already cached successful
decision.

Each Runtime gateway Pod accepts at most 128 concurrent HTTP requests by
default. Admission is non-blocking: a request above the per-Pod limit receives
`429 Too Many Requests` instead of waiting in an unbounded gateway queue.
Health checks do not consume a request slot. The Helm value
`gateway.maxConcurrentRequests` configures the limit for every gateway Pod.

The gateway server maps the following HTTP API operations to `SessionRuntime`
gRPC methods:

| HTTP API | `SessionRuntime` method | Behavior |
| --- | --- |
| `GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}` | `GetSessionStatus` | return readiness and bounded session metadata |
| `POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:execute` | `ExecuteSessionOperation` | execute one command or file mutation |
| `POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:stream` | `StreamSessionOperation` | execute one operation and stream ordered NDJSON progress and terminal events |
| `GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/files` | `ReadSessionFile`, `ListSessionFiles` | bounded workspace-relative file access |

An exec request supplies exactly one of `argv` or `shell`. `argv` directly
executes a program. `shell` deliberately opts into the Runtime's shell. Both
support bounded stdin, a relative working directory, bounded environment
overrides, and an operation timeout.

Owner runtimed serializes commands and file mutations through one FIFO queue
per session:

```text
Queued -> Running -> Succeeded | Failed | Cancelled | TimedOut
```

Only one mutation runs at a time. Read/list/status requests do not enter the
queue. The effective queue size is the minimum of `mode.session.queueSize`
and the runtimed global maximum. The effective operation timeout is likewise
bounded by `mode.session.operationTimeout` and the global maximum. The v0
defaults are 32 queued mutations and a five-minute operation limit. A Run can
only reduce either limit. Administrators configure these platform-wide limits,
and the maximum time runtimed waits for the Runtime Server to finish closing a
session, through Helm `runtimed.session.maxQueueSize`,
`runtimed.session.maxOperationTimeout`, and
`runtimed.session.closeTimeout`.

When a session closes, owner runtimed rejects new operations, cancels queued work,
sends termination to the running process group, waits the grace period, then
force-kills it if necessary.

The graceful process-termination period is a Runtime Server implementation
setting, not a Run or Runtime API field. The built-in Runtime chart exposes
`bash.sessionTerminationGraceSeconds` and
`python.sessionTerminationGraceSeconds`; both default to two seconds. A user
who creates a Runtime CR directly configures the image's documented flag in
`spec.template.spec.containers[0].args`, for example:

```yaml
spec:
  template:
    spec:
      containers:
        - name: runtime
          args:
            - --session-termination-grace=2s
```

Every Runtime Server that supports Session Mode must stop the active operation
and its child process tree, then forcefully terminate its backend-specific
execution unit after its configured grace period. Bash and Python use process
groups; a sandbox or microVM backend can use its own equivalent. Runtime Server
authors must document their configuration and behavior. Operators must leave
sufficient time between the configured grace and
`runtimed.session.closeTimeout` for `CloseSession` to return after the operation
exits.

File paths are always relative to the session workspace. Absolute paths,
traversal, and symlink escapes are rejected. In v0, the gateway JSON request
body is limited to 1 MiB. Built-in Runtime Servers limit direct file reads and
writes, plus each command stdout and stderr stream, to 1 MiB. Larger durable
results use ArtifactStore.

`ListSessionFiles` lists direct children only and is paginated. The HTTP
endpoint accepts optional `path`, `limit`, and `pageToken` query parameters.
`limit` defaults to 100 and must be in `1..1000`; an invalid value returns
`400 Bad Request`. Each response contains an `entries` array and a
`nextPageToken` string. An empty `nextPageToken` means that no later entry was
observed. For example:

```json
{
  "entries": [
    {"path": "build.log", "directory": false, "sizeBytes": 1024}
  ],
  "nextPageToken": "eyJ2IjoxLCJwYXRoIjoiIiwiYWZ0ZXIiOiJidWlsZC5sb2cifQ"
}
```

The token is opaque to clients but portable between ready Pods of the same
Runtime. It is a versioned base64url cursor containing the requested relative
directory and the final returned entry name. A Runtime Server rejects malformed
tokens and a token used with a different directory. Entries are ordered by
their UTF-8 encoded names in byte-wise lexicographic order; the token resumes
strictly after that name. This ordering is part of the cross-runtime contract,
so Bash and Python Runtime Servers produce interchangeable page boundaries.

A listing is not a filesystem snapshot. Mutations between pages can cause a
later page to omit or repeat entries; a caller that needs a fresh view restarts
from an empty token. The Runtime Server, not only the HTTP gateway, enforces
the page limit so direct `SessionRuntime` callers cannot cause an unbounded
listing response. The Go and Python SDKs expose an explicit paged `ListFiles`
operation with a page-options value and return one page plus its next token;
they do not hide unbounded iteration behind a helper. The Go SDK uses
`ListFiles(ctx, ListFilesOptions{Directory, Limit, PageToken})` and returns a
`FilePage` with `Entries` and `NextPageToken`. The Python SDK uses
`list_files(ListFilesOptions(directory, limit, page_token))` and returns a
`FilePage` with `entries` and `next_page_token`. A zero limit omits the query
parameter and lets the Runtime Server select its default page size.

## Runtime Server Contract

Owner runtimed owns queue admission, operation lifecycle, Run status updates,
capacity release, and structured audit logs. Runtime Server owns local
workspace confinement, process groups, and local session state. Both hops use
the same gRPC `SessionRuntime` messages: runtimed implements the service for
gateway traffic and proxies an accepted owner request to its local Runtime
Server. runtimed alone owns the session queue and operation state, so a Runtime
Server never independently reorders work.

`SessionRuntime` is an optional Runtime Server extension. A Runtime that does
not implement it can still support Task or Function Runs. Assigning a Session
Run to such a Runtime fails during registration with a Runtime capability
mismatch; Session support is not declared in `Runtime.spec`.

The `SessionRuntime` method set is:

```proto
service SessionRuntime {
  rpc RegisterSession(RegisterSessionRequest) returns (SessionStatus);
  rpc ExecuteSessionOperation(ExecuteSessionOperationRequest)
      returns (ExecuteSessionOperationResponse);
  rpc ReadSessionFile(ReadSessionFileRequest) returns (ReadSessionFileResponse);
  rpc ListSessionFiles(ListSessionFilesRequest) returns (ListSessionFilesResponse);
  rpc CloseSession(CloseSessionRequest) returns (CloseSessionResponse);
  rpc GetSessionStatus(GetSessionStatusRequest) returns (SessionStatus);
}
```

`ListSessionFilesRequest` carries the optional positive `limit` and opaque
`page_token` in addition to `path`. `ListSessionFilesResponse` carries
`next_page_token` with the direct child entries. Runtime Servers validate the
same `1..1000` limit and cursor semantics as the HTTP endpoint.

Every request includes the immutable Run UID and assignment identity. The
gateway server derives that identity from the current Run assignment; it is not
client-controlled HTTP input. A receiving runtimed either forwards the request
to the owner or, when it is the owner, applies queue admission before calling
its local Runtime Server. `RegisterSession` receives the prepared workspace
path and immutable source inputs; it is idempotent for the same identity.
`ExecuteSessionOperation` contains exactly one `oneof` payload: a command,
file write, directory creation, delete, or rename. Its request context carries
the command timeout; cancellation terminates the matching process group. Read
and list RPCs are synchronous, bounded, and do not enter the mutation queue.
The local Runtime Server does not route requests or allocate operation state.
`CloseSession` is idempotent and removes local state after owner runtimed has
rejected new gateway operations.

External clients invoke HTTP through the shared Runtime gateway Service and
never reach a Runtime Server directly. The gateway server calls the target
Runtime Service's `SessionRuntime` endpoint; owner runtimed proxies accepted
requests to its local Runtime Server.

## SDK Contract

The agent-facing Go and Python SDKs call a Session Run a **Sandbox**. This is a
user-level name only: a Sandbox is backed by one `Run` with
`spec.mode.session`, and the Kubernetes Run lifecycle remains authoritative.
The SDKs do not create another Sandbox resource or bypass the gateway.

Both SDKs expose the same lifecycle and operations:

| Helper | Behavior |
| --- | --- |
| `Create` | create a Session Run from the requested Runtime, source, artifact inputs, environment, and timeout settings |
| `Open` | read an existing named Session Run; it never creates or re-registers one |
| `Wait` | watch or poll until `Ready` or a terminal Run phase; return a typed terminal or readiness error |
| `Execute` | send exactly one command or file mutation through the Run endpoint; never retry a mutation implicitly |
| `ReadFile`, `ListFiles`, `WriteFile`, `CreateDirectory`, `DeleteFile`, `RenameFile` | use the bounded, workspace-relative gateway operations |
| `Logs` | read the assigned runtimed container log and filter the structured lines for the immutable Run UID; it does not introduce a gateway log store |
| `Close` | set `spec.termination.mode: Drain` and wait for finalization, artifact export, and `Succeeded`; return a typed state error for any other terminal phase |
| `Cancel` | set `spec.termination.mode: Immediate` and wait for `Cancelled`, Runtime Server close, workspace cleanup, and capacity release; return a typed state error for any other terminal phase |

`Open` and every data-plane call derive the endpoint from the current Run
status. The SDK rejects a non-Session Run, a Run that is not `Ready`, or an
endpoint whose Run UID does not match the opened Run. HTTP failures are exposed
as typed errors that retain the status code and bounded server message. The SDK
never treats a transport failure as proof that a mutation did not run.

For in-cluster callers, the SDK uses the caller's Kubernetes REST credentials
to create/watch Runs, read runtimed logs, and authenticate to the Runtime
gateway Service. For local callers, it creates a scoped port-forward to the
shared Runtime gateway Pod while preserving the endpoint path and uses the same
Kubernetes credentials. Runtime Servers and runtimed gRPC ports remain private
implementation details in both modes.

The public Session API does not expose a Runtime backend choice. The v0 backend
is one trusted container session per Runtime Pod. A future Runtime implementation
may multiplex multiple sessions in a worker Pod through gVisor or microVM
actors without changing the Run or `SessionRuntime` API. This follows the useful
actor-versus-worker separation demonstrated by Agent Substrate, without making
its snapshotting or multiplexing model a v0 dependency.

## Security and Observability

v0 sessions are a **trusted-workload preview**. They are not safe isolation for
untrusted LLM-generated code. Runtime Pod templates remain responsible for
image pinning, ServiceAccount, resource limits, security context, and network
policy. v1.0 must provide at least one secure session backend, initially gVisor.

Owner runtimed emits structured JSONL container logs for every session mutation.
Lines are keyed by Run UID, assignment identity, type, timestamps, result, and
exit code; bounded command output uses the existing `stdout` and `stderr`
streams, while a separate `audit` line omits command text, stdin, and file
contents. kruntimes does not retain these logs: Kubernetes log collectors such
as Fluent Bit own persistence and export. Command history, file contents,
stdout, stderr, and high-frequency events are not written to `Run.status`.
ArtifactStore remains the durable path for large outputs and exports.

## Non-Goals

v0 does not provide session checkpoint/resume, branching, persistent REPL
processes, durable audit storage, concurrent mutations, shared Runtime Pod
sessions, or secure execution of untrusted code.
