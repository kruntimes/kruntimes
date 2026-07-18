# Function Mode Lifecycle and Invoke Dataplane

Status: **Proposed for review**

This document refines the function-mode target in
[Function Mode and Agent Sandboxes](../function-mode/). It defines the Run
lifecycle, status API, gateway boundary, recovery behavior, and invocation
semantics that must be reviewed before implementation.

## Problem

The existing design establishes a function-mode Run and a Runtime gateway, but
it leaves several correctness and security questions unresolved:

- whether `Ready` is a terminal phase and how generic Run controllers classify
  it;
- what happens when registration fails or the assigned Runtime Pod disappears;
- how cancellation, deletion, total timeout, and idle timeout release capacity;
- how an invoke request reaches the owning runtimed without a Kubernetes API
  lookup on the hot path;
- how callers are authenticated and authorized;
- whether a transport retry may execute an invocation twice;
- how invocation concurrency, payloads, outputs, artifacts, and logs remain
  bounded;
- which state belongs in Run status and which state remains in the dataplane.

Adding only a `Ready` phase and an endpoint field would leave these behaviors
undefined and make later compatibility harder.

## Control Plane and Dataplane

Kubernetes remains the durable lifecycle control plane:

- a function-mode `Run` declares source, Runtime, handler, timeouts, and retry
  policy;
- the scheduler assigns Runtime capacity exactly as it does for task Runs;
- runtimed prepares and registers the function, updates bounded Run status, and
  releases the reservation;
- controllers recover or terminate the Run when its Runtime Pod disappears.

Invocation is a dataplane operation:

- clients invoke through the Runtime gateway Service;
- runtimed routes from an in-memory ownership/readiness cache;
- the owning runtimed calls its local Runtime Server;
- individual invocations do not create Kubernetes objects and do not append
  history to Run status.

Scheduler and Runtime Server remain unaware of agents, sandboxes, Workflows,
and SDK sessions.

## Run Status API

The Run phase enum gains `Ready`:

```go
const RunReady RunPhase = "Ready"
```

`Ready` is active and non-terminal. Generic phase helpers, capacity accounting,
stale-pod recovery, cancellation, metrics, CLI waits, TTL cleanup, and completed
Run GC must all classify it explicitly. Only `Succeeded`, `Failed`, `Timeout`,
and `Cancelled` remain terminal.

Function-mode status gains one stable endpoint reference:

```go
type RunEndpointProtocol string

const RunEndpointProtocolHTTPS RunEndpointProtocol = "HTTPS"

type RunEndpoint struct {
    Protocol RunEndpointProtocol `json:"protocol"`
    URL      string              `json:"url"`
    CABundle []byte              `json:"caBundle,omitempty"`
}

type RunStatus struct {
    // Existing fields omitted.
    AssignedPodUID string       `json:"assignedPodUID,omitempty"`
    Endpoint       *RunEndpoint `json:"endpoint,omitempty"`
}
```

`assignedPodUID` disambiguates Pod-name reuse and is set and cleared with
`assignedPod`. The endpoint URL is limited to 2048 bytes and the PEM CA bundle
to 16 KiB. These bounds are part of CRD validation.

The first public invoke protocol is HTTPS. Runtime Server communication remains
internal gRPC and is not exposed through this field. `caBundle` contains a
bounded PEM trust bundle when the Runtime gateway uses the controller-managed
CA; it is omitted when the certificate chains to a trust root configured by
the client. Supporting another public protocol requires a later API review.

The endpoint path includes namespace, Run name, and immutable Run UID so a
deleted Run name cannot address a newly created Run accidentally:

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

`Ready=True` means the assigned runtimed has registered the Run UID with the
local Runtime Server and can accept invokes. During scheduling, registration,
retry, cancellation, and terminal phases it is false with a specific reason.
The endpoint may remain stable while a Run is temporarily unavailable during
recovery; clients must use phase or the Ready condition, not endpoint presence,
as the readiness signal.

Opaque Runtime Server registration IDs, ownership-cache entries, invocation
counters, and invocation history do not belong in Run status. The Run UID is
the stable registration identity across runtimed and Runtime Server.

## Spec Transition Rules

Execution inputs must not change after creation. CRD transition validation
should make `runtime`, `source`, `mode`, `env`, `timeout`, and `retryPolicy`
immutable for both task and function Runs. Otherwise a reconciler can observe a
different program or execution mode after assignment.

`cancelRequested` may transition only from false to true. The existing
`ttlSecondsAfterFinished` field may remain mutable because it controls retention
after execution rather than execution behavior.

These are breaking validation changes to the experimental API and require
review before the CRD is regenerated.

## Lifecycle State Machine

The normal function lifecycle is:

```text
Pending -> Scheduled -> Running (preparing/registering) -> Ready
```

`Run.status.startTime` is set when runtimed claims the Run. In function mode,
`Run.status.attempt` counts registration lifecycle attempts, including the
initial attempt, using the existing shared retry engine. Retrying an uncertain
Pod-local registration RPC for the same attempt is idempotent and does not
increment status.

Terminal transitions are:

- cancellation before assignment ends as `Cancelled` in the scheduler;
- cancellation after assignment makes the owning runtimed unregister, clean
  local state, release capacity, and then set `Cancelled`;
- a registration error retries according to `retryPolicy`; exhaustion ends as
  `Failed`;
- assigned Runtime Pod loss makes the Run unavailable and enters fenced stale
  recovery; after the old assignment is fenced, a new attempt may register on
  another ready Pod, while exhaustion ends as `Failed`;
- `spec.timeout`, when set, is the maximum reservation lifetime from
  `startTime`; expiry unregisters and ends as `Timeout`;
- `mode.function.idleTimeoutSeconds`, when set, expires one registration epoch
  after no completed or in-flight invocation activity and ends as `Timeout`
  with reason `IdleTimeout`.

Invocation-level handler errors, request timeouts, and rejected concurrency do
not change Run phase and do not consume Run retry attempts. They are results of
one invocation, not failures of the registered function lifecycle. A fatal
Runtime Server error that invalidates registration makes the Run not ready and
enters registration recovery.

The Runtime Server owns the precise last-activity clock for a registration
generation and exposes it through `FunctionStatus`; it is not checkpointed on every
invoke to etcd. If the Runtime Server process recovers its registration, it
also recovers that clock. If the Runtime Pod is replaced, registration creates
a new epoch and resets the idle timer. This avoids early expiry and avoids a
high-frequency Run status write path.

## Ownership Epoch and Fencing

Run UID is the stable external function identity. A new registration is fenced
by `status.attempt`, `assignedPod`, and `assignedPodUID`. `RegisterFunction`
carries the Run UID and registration attempt, then returns an opaque local
registration ID. `FunctionStatus`, `InvokeFunction`, and `UnregisterFunction`
carry that registration ID instead of repeating the attempt. A local gateway
invokes only when its active assignment matches the newest cache entry.

Pod readiness or a stale heartbeat alone is not proof that an old function has
stopped. Recovery first makes the old assignment unreachable through the
gateway Service and confirms one of these fences:

- the exact assigned Pod UID no longer exists;
- the Pod is deleting and has been removed from Service endpoints; or
- the old owning runtimed acknowledged unregister for that epoch.

Only then may shared retry clear assignment and schedule a new attempt. Peer
routing requests carry the expected Run UID, registration attempt, and assigned
Pod UID and are rejected on mismatch. This prevents normal recovery from
routing to two assignments at once. kruntimes
still does not promise exactly-once invocation when a network fails after
dispatch; that separate ambiguity is defined by the invoke contract below.

## Cleanup and Finalization

Before registering a function, runtimed adds the
`kruntimes.io/function-cleanup` finalizer. A deleting function Run is reconciled
even though normal execution creation has stopped:

1. stop accepting new invokes;
2. wait for or cancel bounded in-flight invokes;
3. call `UnregisterFunction` idempotently;
4. remove function-local workspace and retained invocation state;
5. release the active capacity entry;
6. remove the finalizer.

If the assigned Pod no longer exists, the stale-recovery controller may remove
the finalizer after confirming that the Pod-local registration cannot still be
serving. PersistentWorkspace and ArtifactStore cleanup remain owned by their
existing controllers and policies; the function finalizer must not delete
shared persistent data implicitly.

Cancellation keeps the Run object and records a terminal status. Deletion is a
separate user request and uses the finalizer path. Both operations are
idempotent across controller and runtimed restarts.

## Runtime Gateway Service

The Runtime controller creates one ClusterIP gateway Service for each Runtime.
The Service is owned by the Runtime and selects all ready Runtime Pods in that
pool on a dedicated runtimed gateway port. The controller also manages a
Runtime-scoped CA and serving certificate Secret mounted only into runtimed.
Certificates cover the gateway Service DNS name. Rotation publishes an
overlapping old/new CA bundle before replacing serving certificates, then
removes the retired CA after all Runtime Pods have rolled.

```text
Runtime gateway Service
  -> any runtimed in the Runtime pool
     -> owning runtimed, directly or through one peer proxy hop
        -> local Runtime Server
```

Every runtimed maintains a watch-backed cache for all function Runs assigned to
its Runtime and namespace:

```text
Run namespace/name/UID -> assigned Pod address -> lifecycle readiness
```

The cache is populated asynchronously from informer events. The invoke handler
must not synchronously read Runs or Pods from the Kubernetes API. Requests are
handled as follows:

- UID or current-object mismatch: `404 Not Found`;
- known Run but not ready, recovering, or terminating: `503 Service Unavailable`;
- local owner: invoke the local Runtime Server;
- remote owner: proxy once to the owning Pod's gateway port;
- stale or unreachable owner: `503 Service Unavailable` and enqueue recovery;
- local concurrency limit reached: `429 Too Many Requests`.

The peer request retains the original caller credential and is authorized by
the owner too. Peer connections use TLS and verify the same Runtime-scoped CA;
when connecting to an owning Pod address, the client verifies against the
gateway Service DNS name carried by the shared serving certificate. A hop
marker prevents proxy loops. Service routing and cache recovery must tolerate a
request reaching a Pod whose watch cache has not yet observed the newest
assignment.

## Authentication and Authorization

The initial gateway is HTTPS and ClusterIP-only; it does not claim to be an
Internet ingress. Plaintext HTTP is not supported because every client request
carries a Kubernetes bearer token. The receiving runtimed uses that token to
submit a `SelfSubjectAccessReview` and requires the caller to be allowed to
`get` the exact Run being invoked. The token must be valid for the Kubernetes
API audience.

Authorization decisions may be cached for at most 30 seconds and never beyond
the token's known expiry. Cache keys use a token digest plus Run namespace,
name, and UID; raw tokens are never logged or stored in Run status. A proxied
request forwards the original token and the owner repeats or reuses the same
bounded authorization decision.

This introduces Kubernetes API traffic on authorization cache misses, but not
for ownership or readiness routing. It preserves namespace RBAC without
granting a custom Runtime service account broad Secret access or TokenReview
privileges.

External exposure requires a separately reviewed authenticated ingress or
gateway. NetworkPolicy should restrict direct gateway access to expected agent
and platform namespaces even when Kubernetes authorization is enabled.

## Invoke Contract

The first HTTPS request is deliberately bounded:

```http
POST /v1/namespaces/{namespace}/runs/{name}/{uid}/invoke
Authorization: Bearer <kubernetes-token>
Content-Type: application/json
X-Kruntime-Invocation-ID: <caller-generated-id>
```

Initial limits are:

- request body: 1 MiB;
- invocation ID: 128 bytes;
- outputs: the same key/count/value limits as bounded Run outputs;
- artifact references: the same count and metadata limits as Run artifact
  references;
- one in-flight invocation per function Run in v0.x;
- no unbounded server-side queue.

The Runtime gateway never automatically retries an invocation after dispatch
to an owner or Runtime Server. A connection failure may have an unknown
execution outcome. The SDK also defaults to no execution retry. A future
deduplication or idempotency contract may use the caller-generated invocation
ID, but v0.x does not promise exactly-once execution.

Successful and failed responses include the invocation ID. Structured runtimed
logs include Runtime, Run UID, and invocation ID. Invocation stdout/stderr is
exposed through log collection or a bounded response field defined by the
Runtime Server contract; it is never appended to `Run.status.message`.

## Runtime Server Boundary

The internal gRPC API gains idempotent lifecycle operations keyed by Run UID:

- `RegisterFunction`: register working directory, handler, and environment;
- `FunctionStatus`: report registered/readiness state, fatal errors, in-flight
  count, and last activity for the current registration generation;
- `InvokeFunction`: execute one bounded request with an invocation ID and
  timeout;
- `UnregisterFunction`: stop new work and release the registration.

Registering the same Run UID and registration attempt with the same immutable
configuration succeeds idempotently and returns the same opaque registration
ID. Registering that attempt with different configuration fails. A higher
registration attempt creates a new generation and invalidates the old ID.
Unregistering an absent registration succeeds. Exact protobuf fields and
Bash/Python adapter behavior are a separate implementation PR, but they must
preserve these semantics.

## Capacity and Concurrency

Runtime `runs` capacity controls how many task executions or registered
function Runs may occupy one Runtime Pod. A Ready function Run continues to
consume one unit until terminal cleanup or deletion releases it.

Invocation concurrency is not scheduler capacity. In v0.x, runtimed and the
Runtime Server allow one in-flight invocation per function Run and reject an
overlapping request with `429`. A later design may add per-function and
Runtime-wide invocation concurrency or queue controls without teaching the
scheduler about individual invokes.

Agent sandbox deployments should continue to use Runtime `runs: "1"` when they
need exclusive Pod and workspace ownership. The SDK may validate or warn about
that deployment choice, but it must not change scheduler semantics.

## Component Boundaries

| Component | Responsibility |
| --- | --- |
| Runtime controller | Reconcile the Runtime-owned gateway Service and ports. |
| Scheduler | Assign generic Run capacity; treat Ready as active and non-terminal. |
| runtimed | Register/unregister, own lifecycle status, maintain routing caches, authorize and proxy invokes, enforce local limits, and clean local state. |
| Runtime Server | Hold registration and activity state, execute invokes, and expose idempotent local lifecycle APIs. |
| stale recovery | Detect lost owning Pods, release impossible local cleanup, and enter shared Run retry/exhaustion semantics. |
| SDK and `krt` | Watch readiness, discover endpoint, supply credentials, invoke without implicit execution retries, and expose typed errors. |

Workflow controllers remain unaware of function registration and invocation.
They may create function Runs later, but they use only the public Run API.

## Implementation Plan

1. Add API prerequisites: `Ready`, assigned Pod UID, endpoint status,
   transition validation, finalizer constant, generated CRDs, and generic
   phase-classification tests.
2. Add Runtime gateway Service reconciliation and Helm/RBAC coverage.
3. Add Runtime-scoped gateway TLS certificate reconciliation, CA publication,
   rotation, and pod rollout behavior.
4. Add Runtime Server register/status/invoke/unregister protobuf APIs and
   built-in Bash/Python adapters.
5. Add runtimed registration lifecycle, finalization, shared retry integration,
   timeout handling, and restart recovery.
6. Add the HTTPS gateway, watch-backed routing cache, local/peer dispatch,
   SelfSubjectAccessReview authorization, limits, and metrics.
7. Add `krt invoke`, then the Go and Python sandbox SDK connection layer.
8. Add E2E coverage for registration, TLS rotation, authorization, local and
   proxied invoke,
   concurrency rejection, cancellation, deletion, idle/lifetime timeout,
   Runtime Pod loss, and retry exhaustion.
9. Complete the agent sandbox demo only after the supported path passes E2E.
