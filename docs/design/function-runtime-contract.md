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

## Registration Identity

For a task Run, `Run.status.attempt` counts execution attempts. For a function
Run, it counts registration lifecycle attempts: the initial registration is
`1`, and the shared retry engine increments it only after a registration
failure enters retry or reassignment. Retrying an uncertain Pod-local
`RegisterFunction` RPC for the same registration attempt is idempotent and
does not change Run status.

`registration_attempt` is not an invocation counter. `invocation_id` is
separate caller-generated correlation data for one function call.

`RegisterFunction` uses Run UID plus `registration_attempt` to establish a
new local registration generation. It returns an opaque `registration_id`.
All later Pod-local operations use that ID instead of repeating the attempt.
The Runtime Server binds the ID to its Run UID and registration attempt, and a
stale ID must never change, invoke, or remove a newer registration.

`registration_digest` is a lowercase SHA-256 digest calculated by runtimed
from canonical, immutable registration inputs: resolved source identity,
handler, environment, and runtime-visible registration settings. It excludes
the transient working-directory path. It is an idempotency check, not a
credential.

### Example Calls and Registration Fence

The following illustrates the Pod-local calls made by runtimed. It is an
example of the proposed protocol, not a public gateway command.

1. A function Run with UID `2b5d...` starts its first registration on Runtime
   Pod A. `Run.status.attempt` is `1`, so runtimed A registers the function:

   ```console
   grpcurl -plaintext -d '{
     "runUid": "2b5d...",
     "registrationAttempt": 1,
     "workingDir": "/workspace/runs/2b5d",
     "handler": "handler.handle",
     "idleTimeoutSeconds": 300,
     "registrationDigest": "sha256:..."
   }' 127.0.0.1:9090 executor.v1.Runtime/RegisterFunction
   ```

   A successful response contains a server-generated registration reference:

   ```json
   {
     "registration": {
       "runUid": "2b5d...",
       "registrationId": "reg_01J..."
     },
     "state": "FUNCTION_REGISTRATION_STATE_READY"
   }
   ```

2. A gateway request is routed to runtimed A. It assigns an invocation ID and
   sends the payload without creating another Kubernetes object:

   ```console
   grpcurl -plaintext -d '{
     "registration": {
       "runUid": "2b5d...",
       "registrationId": "reg_01J..."
     },
     "invocationId": "01J...",
     "contentType": "application/json",
     "input": "eyJjb21tYW5kIjoic3RhdHVzIn0="
   }' 127.0.0.1:9090 executor.v1.Runtime/InvokeFunction
   ```

   In protobuf JSON, `bytes` is base64-encoded; the decoded input is
   `{"command":"status"}`. Another call gets another `invocation_id`, but uses
   the same registration ID while this registration remains active.

3. If the registration fails and the retry policy allows recovery, the shared
   retry engine advances `Run.status.attempt` to `2` before the scheduler
   assigns or reassigns a Runtime Pod. The next `RegisterFunction` uses
   `registration_attempt: 2` and receives a new registration ID. An invoke or
   unregister request carrying `reg_01J...` must return `FailedPrecondition`
   after the newer registration supersedes it; it cannot affect the new
   registration.

The gateway and runtimed also fence routing using assigned Pod identity. The
registration ID protects the Runtime Server's local registration state; it does
not make an invocation exactly once.

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

message FunctionRegistration {
  // Kubernetes Run UID, never its mutable name.
  string run_uid = 1;
  // Opaque Runtime Server-generated ID for one local registration generation.
  string registration_id = 2;
}

message RegisterFunctionRequest {
  string run_uid = 1;
  int32 registration_attempt = 2;
  string working_dir = 3;
  string handler = 4;
  map<string, string> env = 5;
  int64 idle_timeout_seconds = 6;
  string registration_digest = 7;
}

message RegisterFunctionResponse {
  FunctionRegistration registration = 1;
  FunctionRegistrationState state = 2;
}

message FunctionStatusRequest { FunctionRegistration registration = 1; }

message FunctionStatusResponse {
  FunctionRegistration registration = 1;
  FunctionRegistrationState state = 2;
  int32 in_flight = 3;
  int64 last_activity_unix_nano = 4;
  string fatal_error = 5;
}

message InvokeFunctionRequest {
  FunctionRegistration registration = 1;
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
  FunctionRegistration registration = 1;
  string invocation_id = 2;
  bytes output = 3;
  string content_type = 4;
  map<string, string> outputs = 5;
  repeated FunctionArtifactOutput artifacts = 6;
}

message UnregisterFunctionRequest {
  FunctionRegistration registration = 1;
  bool cancel_in_flight = 2;
  int64 drain_timeout_millis = 3;
}

message UnregisterFunctionResponse { FunctionRegistration registration = 1; }

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
timeout, digest, and one-based `registration_attempt` before accepting work.

- Repeating the same Run UID, registration attempt, and digest returns the
  current registration reference without reinitializing the function.
- A different digest for the same Run UID and registration attempt returns
  `AlreadyExists` and does not replace the registration.
- A higher registration attempt supersedes an older local registration and
  creates a new opaque registration ID. A lower attempt returns
  `FailedPrecondition`.
- Permanent initialization failure is surfaced as `FAILED` with a bounded
  `fatal_error` through `FunctionStatus`.

`FunctionStatus` only reads local Runtime Server state. `last_activity_unix_nano`
is zero until there is completed or in-flight work to report. `fatal_error` is
bounded diagnostic text, not logs. `NotFound` means no registration has this
Run UID; `FailedPrecondition` means its registration ID is stale, draining, or
unready. runtimed polls it at a bounded cadence for health and idle timeout,
never writing each activity update to Kubernetes.

## Invocation Semantics

`InvokeFunction` requires a `READY` registration for the supplied registration
reference. v0.x
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

Runtime logs are structured by Run UID and invocation ID. Adapter-captured
function output populates the RPC `output` field and is not automatically
logged. Built-in Bash uses handler stdout as function output and stderr as
structured logs; neither is written to `Run.status.message`.

## Unregistration

`UnregisterFunction` first moves the registration to `DRAINING`, rejecting new
invokes. With `cancel_in_flight=false`, it waits no longer than
`drain_timeout_millis` for the active invocation. With `cancel_in_flight=true`,
it cancels immediately, then releases registration-local state.

Unregistering an absent registration succeeds. Unregistering a stale
registration ID returns `FailedPrecondition` and cannot delete a newer
registration for the same Run UID.

## Limits and Error Mapping

| Value | Initial limit | Enforcement |
| --- | ---: | --- |
| Request body | 1 MiB | Gateway and runtimed |
| Registration ID | 128 bytes | Runtime Server and runtimed |
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
| `AlreadyExists` | Same registration attempt, different digest | Registration failure |
| `FailedPrecondition` | Stale registration ID or unready registration | HTTP 503 |
| `ResourceExhausted` | Invocation already active | HTTP 429 |
| `DeadlineExceeded` | Invocation deadline elapsed | HTTP 504 |
| `Unavailable` | Runtime Server cannot accept work | HTTP 503 and lifecycle recovery |

## Built-in Runtime Requirements

Python imports `module.function`, passes decoded JSON input, and encodes its
return value as JSON output. Bash follows the [AWS Lambda custom-runtime
handler model](https://docs.aws.amazon.com/lambda/latest/dg/runtimes-walkthrough.html):
its handler is `file.function`, where `file` names a `.sh` file relative
to the registered working directory. During registration, the Bash Runtime
sources `file.sh` and validates that `function` exists. For an
`application/json` invocation, it calls that function with the payload as one
quoted positional argument and captures its stdout as the response output. It
never evaluates either the handler or request payload as shell source, and it
does not interpolate request data into a command string. Both adapters operate
beneath the registered working directory, honor context cancellation, and
permit only one active invocation per registration.

Existing task-only Runtime Servers remain valid. Function mode is enabled only
after a future compatibility/health handshake confirms support for these RPCs;
there is no fallback that emulates function invocation through `Execute`.

## Review Decisions Requested

1. Use `Run.status.attempt` as the function registration lifecycle attempt;
   use the Runtime Server-generated registration ID for subsequent local calls
   and stale-operation fencing.
2. Use opaque bytes plus content type for local invoke payloads; JSON is the
   first gateway encoding.
3. Let Runtime Servers declare validated relative artifact paths while
   runtimed owns upload and public ArtifactRef creation.
4. Do not promise invocation-ID deduplication or automatic execution retry.
5. Limit v0.x to one in-flight invocation per registered function Run.

After approval, implementation can be split into protobuf/stub generation,
Bash and Python adapters, and runtimed registration lifecycle/gateway work.
