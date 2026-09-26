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
- Use a browser- and CLI-friendly HTTP response stream that works with bearer
  authentication.
- Bound every Runtime-emitted event without introducing an unbounded event
  buffer.

## Non-goals for the first delivery

- Durable event retention or replay after a Runtime Pod is lost.
- Continuing an operation after its client connection is cancelled.
- Bidirectional user input or approval. Those require a separately admitted
  Session operation and are not encoded as a reply on the stream.
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

The public HTTP endpoint is:

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

The HTTP representation uses lower-case protocol values: output `stream` is
`stdout` or `stderr`; progress `kind` is `status`, `text_delta`,
`tool_call_started`, or `tool_call_finished`. Binary `data` fields are standard
JSON base64 strings.

## Lifecycle, cancellation, and bounds

The gateway authorizes the stream exactly as it authorizes unary operations.
It passes the HTTP request context to runtimed. Client disconnect, gateway
shutdown, immediate Session cancellation, and the effective operation timeout
cancel that context; runtimed cancels the local Runtime Server stream and frees
the active queue entry. `Drain` accepts an already admitted stream and rejects
new ones as it does today.

Runtimed limits each Runtime-emitted event. It forwards events directly rather
than accumulating an unbounded response buffer. An over-limit or malformed
event terminates the operation with a resource-limit failure. Runtime-provided
progress must not contain credentials, command stdin, or unbounded tool output.
The existing response-size limit continues to apply to terminal command
results and each gateway JSON line.

## Compatibility and rollout

`ExecuteSessionOperation` and `operations:execute` are unchanged. Built-in
Runtimes may initially return `Unimplemented` for the streaming gRPC method;
the gateway maps that to a clear capability error. Interactive Runtimes opt in
by implementing the method. The GitHub Issue Labeler Runtime will emit Pi text
and tool lifecycle events, while its existing unary `message` command continues
to return the final answer for non-streaming clients.

The Go and Python Session SDKs will add explicit streaming helpers rather than
silently changing `Execute` return types. Dashboard will use the streaming
endpoint for agent turns and render events incrementally.
