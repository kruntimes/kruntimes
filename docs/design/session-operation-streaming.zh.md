# Session Operation 流式事件

## 背景

`ExecuteSessionOperation` 是 unary request。这适合短命令和原子文件 mutation，但 interactive
Runtime 的一次 operation 可能要经过多次 model request 和 tool call 才产生最终回答。在完整 operation
返回前，caller 无法得知 Session 是否仍在工作。

本设计为单个 Session operation 增加通用、有序的实时事件流。它不是 agent 专用 API：Runtime 可将它用于
command output、progress reporting 或 interactive agent turn。已有 unary operation API 保持支持。

## 目标

- 在 operation 运行时向 caller 提供 accepted、progress、output、terminal result 和 error event。
- 保留 owner runtimed 的 FIFO queue，以及既有 cancellation、authorization、assignment fencing 与
  operation timeout 语义。
- Runtime Server 与 runtimed port 保持私有；Runtime gateway 仍是唯一公开 data-plane endpoint。
- 使用同时适合 browser 和 CLI、并能使用 bearer authentication 的 HTTP response stream。
- 限制每个 Runtime-emitted event 的大小，且不引入无界 event buffer。

## 首次交付的非目标

- Runtime Pod 丢失后的 durable event retention 或 replay。
- client connection 取消后继续执行 operation。
- 双向 user input 或 approval；它们需要另一个经过 admission 的 Session operation，不能作为 stream 的 reply。
- 将高频 event 写入 `Run.status`、Kubernetes Events 或 container logs。runtimed structured log 仍是
  audit path。

durable operation record、idempotent turn submission 和 cursor-based replay 继续由
[#36](https://github.com/kruntimes/kruntimes/issues/36) 跟踪。实时流刻意作为这些能力的兼容基础，不能过早
宣称已经解决 recovery semantics。

## API

Runtime gRPC contract 增加 server-streaming method：

```proto
rpc StreamSessionOperation(ExecuteSessionOperationRequest)
    returns (stream SessionOperationEvent);
```

`SessionOperationEvent` 具有每个 operation 严格递增的 sequence number，以及 `oneof` payload：

- `accepted`：owner runtimed 已将 operation admission 到 FIFO queue；
- `output`：command Runtime 的有界 `stdout` 或 `stderr` bytes；
- `progress`：Runtime 定义的有界 event（`text_delta`、`tool_call_started`、
  `tool_call_finished` 或 `status`），包括 typed kind、可选 tool identity 和 JSON payload；
- `completed`：适用时，与 unary operation 返回相同的有界 `SessionCommandResult`；
- `failed`：terminal 的 gRPC-compatible code 与安全 message。

由 owner runtimed 而非 gateway 分配 sequence number。它在 queue entry 处于 active 时转发 Runtime
Server event stream。这样即使 gateway request 到达非 owner Runtime Pod、再单跳转发给 owner，也不会破坏
queue mutation ordering。

公开 HTTP endpoint：

```
POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:stream
Content-Type: application/json
Accept: application/x-ndjson
```

request body 与 `operations:execute` 完全一致。response 是 `application/x-ndjson; charset=utf-8`
stream：每行一个完整 JSON event，并按 sequence 顺序排列。gateway 每个 event 都会 write 并 flush。在未设置
content length 时，Go `net/http` 自动选择 HTTP/1.1 chunked transfer encoding；server 不应手动设置
`Transfer-Encoding`。HTTP/2 使用自身 data framing，无需特殊处理。

client 用 `fetch` 消费 `response.body` 的 `ReadableStream`，因此可以使用与其他 gateway operation 相同的
bearer-token header。CLI 直接读取 JSON line。无法 authorize 或 admission 的 request 会在任何 response
event 前使用现有 HTTP error mapping 失败。event byte 一旦写出，HTTP status 就不能安全改变；之后 terminal
failure 用 `failed` event 表示。

HTTP representation 使用小写 protocol value：output 的 `stream` 为 `stdout` 或 `stderr`；progress 的
`kind` 为 `status`、`text_delta`、`tool_call_started` 或 `tool_call_finished`。二进制 `data` field 使用标准
JSON base64 string。

## Lifecycle、cancellation 与边界

gateway 对 stream 的 authorization 与 unary operation 相同，并将 HTTP request context 传给 runtimed。
client disconnect、gateway shutdown、Immediate Session cancellation 和 effective operation timeout 都会取消该
context；runtimed 取消本地 Runtime Server stream 并释放 active queue entry。`Drain` 允许已 admission 的 stream
完成，并像现在一样拒绝新的 stream。

runtimed 限制每个 Runtime-emitted event，并直接转发 event，而不累计无界 response buffer。超限或 malformed
event 会以 resource-limit failure 结束 operation。Runtime 提供的 progress 不能包含 credential、command stdin
或无界 tool output。已有 response-size limit 继续约束 terminal command result 和每条 gateway JSON line。

## 兼容性与 rollout

`ExecuteSessionOperation` 与 `operations:execute` 不变。built-in Runtime 初期可以对 streaming gRPC method
返回 `Unimplemented`；gateway 会映射为明确的 capability error。interactive Runtime 通过实现该 method opt in。
GitHub Issue Labeler Runtime 将发出 Pi text 和 tool lifecycle event；它已有的 unary `message` command 继续为
非 streaming client 返回最终回答。

Go 和 Python Session SDK 将增加显式 streaming helper，而不是静默改变 `Execute` 的返回类型。Dashboard 对
agent turn 使用 streaming endpoint 并逐步渲染 event。
