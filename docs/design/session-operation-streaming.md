# Session Operation Streaming

## Context

`ExecuteSessionOperation` is a unary request. That is appropriate for short
commands and atomic file mutations, but an interactive Runtime can spend a
single operation on several model requests and tool calls before it has a final
answer. A caller then receives no indication that the Session is alive until
the complete operation returns.

This design adds a generic, ordered live event stream for one Session operation.
It is not an agent-specific API: Runtimes may use it for command output,
progress reporting, or interactive agent turns. The existing unary operation
API remains supported.

## Goals

- Let a caller receive accepted, progress, output, terminal-result, and error
  events as an operation runs.
- Preserve the owner runtimed's FIFO queue and existing cancellation,
  authorization, assignment fencing, and operation timeout semantics.
- Keep Runtime Server and runtimed ports private; the Runtime gateway is the
  only public data-plane endpoint.
- Use browser- and CLI-friendly streaming transports that work with bearer
  authentication.
- Bound every Runtime-emitted event without introducing an unbounded event
  buffer.

## Non-goals for the first delivery

- Durable event retention or replay after a Runtime Pod is lost.
- Durable operation replay after a client disconnect. A live operation may
  continue after its connection closes, subject to its ordinary timeout and the
  Session lease, but the disconnected client has no retained event cursor.
- Runtime-defined bidirectional user input or approval. The WebSocket transport
  supports client cancellation, but further messages require a separate Runtime
  protocol extension and operation admission design.
- Persisting high-frequency events in `Run.status`, Kubernetes Events, or
  container logs. Runtimed structured logs remain the audit path.

Durable operation records, idempotent turn submission, and cursor-based replay
remain tracked by [#36](https://github.com/kruntimes/kruntimes/issues/36). The
live stream is deliberately a compatible foundation for those additions rather
than claiming to solve recovery semantics prematurely.

## API

The Runtime gRPC contract adds a server-streaming method:

```proto
rpc StreamSessionOperation(ExecuteSessionOperationRequest)
    returns (stream SessionOperationEvent);
```

`SessionOperationEvent` has a strictly increasing per-operation sequence number
and a `oneof` payload:

- `accepted`: the owner runtimed admitted the operation to its FIFO queue;
- `output`: bounded `stdout` or `stderr` bytes for a command Runtime;
- `progress`: a Runtime-defined, bounded event (`text_delta`,
  `tool_call_started`, `tool_call_finished`, or `status`), with a typed kind,
  optional tool identity, and JSON payload;
- `completed`: the same bounded `SessionCommandResult` returned by the unary
  operation, when applicable;
- `failed`: a terminal gRPC-compatible code and safe message.

The owner runtimed—not the gateway—assigns the sequence numbers. It forwards
the Runtime Server event stream while its queue entry is active. This keeps the
queue's mutation ordering intact even when a gateway request lands on a
non-owner Runtime Pod and is forwarded once to the owner.

The compatible HTTP endpoint is:

```
POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:stream
Content-Type: application/json
Accept: application/x-ndjson
```

Its request body is identical to `operations:execute`. The response is an
`application/x-ndjson; charset=utf-8` stream: one complete JSON event per line,
in sequence order. The gateway writes and flushes each event. Go's `net/http`
automatically chooses HTTP/1.1 chunked transfer encoding when no content length
is supplied; the server must not set `Transfer-Encoding` manually. HTTP/2 has
its native data framing and needs no special case.

Clients use `fetch` and consume `response.body` as a `ReadableStream`, which
allows the same bearer-token headers used by all other gateway operations. A
CLI consumes JSON lines directly. A request that cannot be authorized or
admitted fails before any response event with the existing HTTP error mapping.
Once event bytes have been written, a terminal failure is represented by a
`failed` event because HTTP status cannot safely change mid-stream.

The Session connection's internal transport is the existing operation WebSocket:

```text
GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:ws
Upgrade: websocket
```

The HTTP upgrade is authenticated and authorized before the connection is
accepted. Browser requests with an `Origin` header must be same-origin; native
clients without `Origin` continue to use bearer-token or client-certificate
authentication in the upgrade request. Opening a connection does not submit an
operation. The first version allows one in-flight operation per connection;
this matches one agent turn at a time and does not invent operation
multiplexing semantics.

The SDK's `Send` writes an internal frame like this:

```json
{
  "type": "send",
  "idempotencyKey": "optional-client-key",
  "operation": {
    "command": {"shell": "classify and label issue #123"}
  }
}
```

The gateway forwards `operation` through the existing operation admission path.
It returns an ordered `SessionOperationEvent` in each server text frame. The
first `accepted` event provides the server-generated operation ID, which the
SDK returns from `Send`; `Receive` exposes subsequent events without exposing
frames or WebSocket details:

```json
{
  "sequence": 1,
  "type": "accepted",
  "accepted": {"operationID": "op-7b7b"}
}
```

During the operation, `Cancel(operationID)` writes the internal frame
`{"type":"cancel","operationID":"op-7b7b"}`. The gateway rejects a
cancellation whose ID is not the current operation. A second `send` before a
terminal event is a connection protocol error. WebSocket ping/pong frames carry
the SDK lease heartbeat; no application-level heartbeat message is public.
Arbitrary interactive input or approval messages are not yet part of the
Runtime protocol.

An error before WebSocket upgrade is an ordinary HTTP error. An error after
upgrade is sent as `{"type":"error","error":"..."}` followed by a WebSocket
close frame. This is necessary because an HTTP status cannot change after a
successful upgrade.

The HTTP representation uses lower-case protocol values: output `stream` is
`stdout` or `stderr`; progress `kind` is `status`, `text_delta`,
`tool_call_started`, or `tool_call_finished`. Binary `data` fields are standard
JSON base64 strings.

## SDK session connection

The SDK is an agent-sandbox API, not an HTTP transport wrapper. It exposes one
Session connection model in both Go and Python:

```text
Runtime.AcquireSandbox (create Session Run) -> assigned Runtime Pod
-> Sandbox.OpenSession -> Send / Receive / Cancel -> Session.Close
-> Sandbox.Release (stop/delete Session Run)
```

`Sandbox` is the SDK object and its one Session is represented by one
session-mode Run. `AcquireSandbox` creates that Run and waits for successful
scheduling plus registration before returning an active Sandbox. A Sandbox
supports exactly one Session in this version.

Capacity is acquired atomically when `AcquireSandbox` creates the Run and the
scheduler assigns it to a Runtime Pod. `OpenSession` only opens a streaming
connection to that ready Session; it never creates a Run or changes capacity.

The public data-plane vocabulary is transport-independent:

| Operation | Meaning |
| --- | --- |
| `Send` | submit one Session operation and return its operation ID |
| `Receive` | receive the next ordered event, including its operation ID |
| `Cancel` | request cancellation of one submitted operation |
| `Session.Close` | close only the streaming connection |
| `ReleaseSandbox` | stop the Session, wait for cleanup, then delete its Session Run |

The SDK does not expose `Stream`, `Resume`, `StreamWebSocket`, NDJSON, or
WebSocket names. Internally it maintains a persistent, authenticated,
bidirectional WebSocket connection. Client messages carry an operation request
or cancellation; server messages carry the ordered `SessionOperationEvent` and
its operation ID. A gateway error after upgrade is surfaced as a typed SDK
transport error rather than as an event.

SDK constructors derived from a Kubernetes REST configuration use its TLS CA
bundle and optional client certificate/key for the connection, and apply an
explicit bearer token when configured. A custom SDK transport must implement
this connection boundary itself; an HTTP `RoundTripper` alone cannot upgrade a
WebSocket. Scoped Console port-forward adapters rewrite the connection endpoint
as they do regular HTTP endpoints and preserve caller credentials.

The first version does not automatically retry or replay an operation after a
transport failure. The operation outcome is then unknown; the caller may open a
new connection before the Session lease expires and inspect Session state.
Durable operation replay is separate from this live-connection contract.

## Lifecycle, cancellation, and bounds

The gateway authorizes the connection exactly as it authorizes unary
operations. `Cancel`, gateway shutdown, immediate Session termination, and the
effective operation timeout cancel an active Runtime Server operation and free
the queue entry. Closing the SDK Session connection only detaches the event
consumer; it does not cancel admitted work or release Session capacity. The
owner runtimed continues to enforce the operation timeout and Session lease.
`Drain` accepts an already admitted operation and rejects new ones as it does
today.

Runtimed limits each Runtime-emitted event. It forwards events directly rather
than accumulating an unbounded response buffer. An over-limit or malformed
event terminates the operation with a resource-limit failure. Runtime-provided
progress must not contain credentials, command stdin, or unbounded tool output.
The existing response-size limit continues to apply to terminal command
results and each gateway JSON line.

## Compatibility and rollout

`ExecuteSessionOperation`, `operations:execute`, and the NDJSON
`operations:stream` endpoint remain available for non-SDK callers. Built-in Runtimes may initially
return `Unimplemented` for the streaming gRPC method; the gateway maps that to
a clear capability error. Interactive Runtimes opt in by implementing the
method. The GitHub Issue Labeler Runtime will emit Pi text and tool lifecycle
events, while its existing unary `message` command continues to return the
final answer for non-streaming clients.

The Go and Python Session SDKs expose `AcquireSandbox`, `OpenSession`, `Send`,
`Receive`, `Cancel`, `Session.Close`, and `Sandbox.Release` rather than
transport-specific streaming helpers. Console opens the Session connection for
agent turns and renders events incrementally.
