# Function Runtime Server Contract

Status: **Proposed for review**

This document defines the internal gRPC contract for function-mode Runs. It
refines [Function Mode Lifecycle and Invoke Dataplane](../function-mode-lifecycle/)
without changing the public Run API or exposing the Runtime Server outside its
Runtime Pod.

## Scope

The existing `Runtime` service supports one-shot execution. Function mode adds
four Pod-local operations called only by runtimed:

- `RegisterFunction` creates or resumes one callable function registration.
- `FunctionStatus` reads local readiness, activity, and fatal state.
- `InvokeFunction` executes one bounded invocation.
- `UnregisterFunction` drains or cancels work and removes local state.

The Runtime Server does not read Kubernetes objects, authenticate callers,
route gateway requests, upload artifacts, or schedule capacity. Those concerns
remain with runtimed, the Runtime gateway, and the control plane.

Adding these RPCs changes the experimental custom Runtime protocol. The exact
shape and semantics below require review before `runtime.proto`, generated
stubs, or built-in Runtime implementations change.

## Registration Epoch

Every operation is scoped to a Run UID and attempt:

```protobuf
message FunctionIdentity {
  // Kubernetes Run UID, never its mutable name.
  string run_uid = 1;
  // One-based Run retry and ownership attempt.
  int32 attempt = 2;
}
```

`run_uid` plus `attempt` is the local registration epoch. runtimed invokes a
local Runtime Server only while it owns that exact epoch. A stale operation
must never change or remove a newer registration for the same Run UID.

`registration_digest` is a lowercase SHA-256 digest calculated by runtimed
from canonical, immutable registration inputs: resolved source identity,
handler, environment, and runtime-visible registration settings. It excludes
the transient working-directory path. It is an idempotency check, not a
credential.

## Proposed Protobuf API

The following is the proposed additive extension to `executor.v1.Runtime`:

```protobuf
service Runtime {
  // Existing task RPCs omitted.
  rpc RegisterFunction(RegisterFunctionRequest) returns (RegisterFunctionResponse);
  rpc FunctionStatus(FunctionStatusRequest) returns (FunctionStatusResponse);
  rpc InvokeFunction(InvokeFunctionRequest) returns (InvokeFunctionResponse);
  rpc UnregisterFunction(UnregisterFunctionRequest) returns (UnregisterFunctionResponse);
}

message FunctionIdentity {
  string run_uid = 1;
  int32 attempt = 2;
}

message RegisterFunctionRequest {
  FunctionIdentity identity = 1;
  string working_dir = 2;
  string handler = 3;
  map<string, string> env = 4;
  int64 idle_timeout_seconds = 5;
  string registration_digest = 6;
}

message RegisterFunctionResponse {
  FunctionIdentity identity = 1;
  FunctionRegistrationState state = 2;
}

message FunctionStatusRequest { FunctionIdentity identity = 1; }

message FunctionStatusResponse {
  FunctionIdentity identity = 1;
  FunctionRegistrationState state = 2;
  int32 in_flight = 3;
  int64 last_activity_unix_nano = 4;
  string fatal_error = 5;
}

message InvokeFunctionRequest {
  FunctionIdentity identity = 1;
  string invocation_id = 2;
  bytes input = 3;
  string content_type = 4;
  int64 timeout_millis = 5;
}

message FunctionArtifactOutput {
  string name = 1;
  string relative_path = 2;
  string content_type = 3;
}

message InvokeFunctionResponse {
  FunctionIdentity identity = 1;
  string invocation_id = 2;
  bytes output = 3;
  string content_type = 4;
  map<string, string> outputs = 5;
  repeated FunctionArtifactOutput artifacts = 6;
}

message UnregisterFunctionRequest {
  FunctionIdentity identity = 1;
  bool cancel_in_flight = 2;
  int64 drain_timeout_millis = 3;
}

message UnregisterFunctionResponse { FunctionIdentity identity = 1; }

enum FunctionRegistrationState {
  FUNCTION_REGISTRATION_STATE_UNSPECIFIED = 0;
  FUNCTION_REGISTRATION_STATE_REGISTERING = 1;
  FUNCTION_REGISTRATION_STATE_READY = 2;
  FUNCTION_REGISTRATION_STATE_DRAINING = 3;
  FUNCTION_REGISTRATION_STATE_FAILED = 4;
}
```

The gateway initially accepts JSON and sets `content_type` to
`application/json`. The local protocol uses opaque bytes so trusted custom
Runtimes can use another representation later. Inputs and response bytes are
never written to `Run.status`.

## Registration and Status Semantics

`RegisterFunction` validates its working directory, handler, environment,
timeout, and digest before accepting work.

- Repeating the same identity and digest returns the current state without
  reinitializing the function.
- A different digest for the same identity returns `AlreadyExists` and does
  not replace the registration.
- A stale attempt returns `FailedPrecondition`; it cannot affect a newer
  registration.
- Permanent initialization failure is surfaced as `FAILED` with a bounded
  `fatal_error` through `FunctionStatus`.

`FunctionStatus` only reads local Runtime Server state. `last_activity_unix_nano`
is zero until there is completed or in-flight work to report. `fatal_error` is
bounded diagnostic text, not logs. `NotFound` means no exact registration;
`FailedPrecondition` means a mismatched epoch. runtimed polls it at a bounded
cadence for health and idle timeout, never writing each activity update to
Kubernetes.

## Invocation Semantics

`InvokeFunction` requires a `READY` registration for the exact identity. v0.x
allows one in-flight invocation per function Run and does not queue requests.

- `invocation_id` is caller-generated correlation data, limited to 128 bytes.
- It is not a deduplication key. Retrying after an unknown result can execute
  work again; no component automatically retries after dispatch.
- `timeout_millis` is bounded by runtimed to the remaining Run lifetime and
  gateway deadline. Zero means the bounded gateway default, never unlimited
  runtime work.
- `ResourceExhausted` maps to gateway HTTP 429. `DeadlineExceeded` affects
  only this invocation, not the function Run lifecycle. `FailedPrecondition`
  maps to HTTP 503 for draining, stale, or unready registration.

`outputs` follow the key, count, and value bounds used by `Run.status.outputs`.
Runtime Server artifact results are declarations of locally written files, not
external artifact references. Each `relative_path` is non-empty, relative, and
cannot contain a `..` segment. runtimed validates declarations, uploads files
through the Runtime ArtifactStore, and returns public `ArtifactRef` values.
This keeps storage credentials and external coordinates out of custom Runtime
Servers.

stdout and stderr use structured runtime logs keyed by Run UID and invocation
ID. They are neither RPC response fields nor `Run.status.message` content.

## Unregistration

`UnregisterFunction` first moves the registration to `DRAINING`, rejecting new
invokes. With `cancel_in_flight=false`, it waits no longer than
`drain_timeout_millis` for the active invocation. With `cancel_in_flight=true`,
it cancels immediately, then releases registration-local state.

Unregistering an absent exact epoch succeeds. Unregistering an old attempt
while a newer attempt exists returns `FailedPrecondition`, preventing late
cleanup from deleting a recovered registration.

## Limits and Error Mapping

| Value | Initial limit | Enforcement |
| --- | ---: | --- |
| Request body | 1 MiB | Gateway and runtimed |
| Invocation ID | 128 bytes | Gateway and runtimed |
| Response body | 1 MiB | runtimed |
| Outputs | Existing Run output limits | runtimed |
| Artifact declarations | Existing ArtifactRef count and metadata limits | runtimed |
| In-flight calls | One per function Run | Runtime Server |
| `fatal_error` | 4 KiB | Runtime Server |

| gRPC code | Meaning | Gateway result |
| --- | --- | --- |
| `InvalidArgument` | Invalid handler, path, payload, or limit | HTTP 400 |
| `NotFound` | Unknown registration | HTTP 404 or 503 after cache recheck |
| `AlreadyExists` | Same epoch, different digest | Registration failure |
| `FailedPrecondition` | Stale, draining, or unready epoch | HTTP 503 |
| `ResourceExhausted` | Invocation already active | HTTP 429 |
| `DeadlineExceeded` | Invocation deadline elapsed | HTTP 504 |
| `Unavailable` | Runtime Server cannot accept work | HTTP 503 and lifecycle recovery |

## Built-in Runtime Requirements

Python imports `module.function`, passes decoded JSON input, and encodes its
return value as JSON output. Bash treats its handler as a runtime-defined
executable entrypoint and receives input through standard input or a bounded
file, never shell-string interpolation. Both adapters operate beneath the
registered working directory, honor context cancellation, and permit only one
active invocation per registration.

Existing task-only Runtime Servers remain valid. Function mode is enabled only
after a future compatibility/health handshake confirms support for these RPCs;
there is no fallback that emulates function invocation through `Execute`.

## Review Decisions Requested

1. Use Run UID plus attempt as the registration epoch and stale-operation
   fence.
2. Use opaque bytes plus content type for local invoke payloads; JSON is the
   first gateway encoding.
3. Let Runtime Servers declare validated relative artifact paths while
   runtimed owns upload and public ArtifactRef creation.
4. Do not promise invocation-ID deduplication or automatic execution retry.
5. Limit v0.x to one in-flight invocation per registered function Run.

After approval, implementation can be split into protobuf/stub generation,
Bash and Python adapters, and runtimed registration lifecycle/gateway work.
