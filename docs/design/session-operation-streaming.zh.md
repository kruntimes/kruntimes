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
- 使用同时适合 browser 和 CLI、并能使用 bearer authentication 的流式 transport。
- 限制每个 Runtime-emitted event 的大小，且不引入无界 event buffer。

## 首次交付的非目标

- Runtime Pod 丢失后的 durable event retention 或 replay。
- client disconnect 后的 durable operation replay。live operation 可以在 connection 关闭后继续，
但仍受其普通 timeout 与 Session lease 约束；disconnected client 没有 retained event cursor。
- Runtime 定义的双向 user input 或 approval。WebSocket transport 支持 client cancellation；更多消息需要单独的
  Runtime protocol extension 与 operation admission design。
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

兼容的 HTTP endpoint：

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

Session connection 的内部 transport 是 WebSocket：

```text
GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}:ws
Upgrade: websocket
```

HTTP upgrade 会在 connection 被接受前完成 authentication 和 authorization。带有 `Origin` header 的 browser
request 必须为 same-origin；不带 `Origin` 的 native client 继续在 upgrade request 中使用 bearer token 或 client
certificate。打开 connection 不会提交 operation。首个版本每条 connection 只允许一个 in-flight
operation；这符合每次一个 agent turn 的模型，并且不会虚构 operation multiplexing semantics。

SDK 的 `Send` 会写入类似下面的内部 frame：

```json
{
  "type": "send",
  "idempotencyKey": "optional-client-key",
  "operation": {
    "command": {"shell": "classify and label issue #123"}
  }
}
```

gateway 通过已有 operation admission path 转发 `operation`。它为每个有序
`SessionOperationEvent` 返回一个 server text frame。第一个 `accepted` event 提供 server-generated
operation ID，SDK 将其作为 `Send` 的返回值；`Receive` 暴露后续 event，但不暴露 frame 或 WebSocket 细节：

```json
{
  "sequence": 1,
  "type": "accepted",
  "accepted": {"operationID": "op-7b7b"}
}
```

operation 期间，`Cancel(operationID)` 写入内部 frame
`{"type":"cancel","operationID":"op-7b7b"}`。gateway 拒绝 ID 不是当前 operation 的 cancellation。
terminal event 前第二个 `send` 是 connection protocol error。WebSocket ping/pong frame 承载 SDK lease
heartbeat；没有公开的 application-level heartbeat message。任意 interactive input 或 approval message 尚未成为
Runtime protocol 的一部分。

WebSocket upgrade 前的 error 仍是普通 HTTP error。upgrade 后的 error 将以
`{"type":"error","error":"..."}` 发送，随后发送 WebSocket close frame；成功 upgrade 后不能再更改 HTTP status。

HTTP representation 使用小写 protocol value：output 的 `stream` 为 `stdout` 或 `stderr`；progress 的
`kind` 为 `status`、`text_delta`、`tool_call_started` 或 `tool_call_finished`。二进制 `data` field 使用标准
JSON base64 string。

## SDK Session connection

SDK 是 agent sandbox API，而不是 HTTP transport wrapper。Go 与 Python 都只暴露一套 Session
connection model：

```text
Runtime.AcquireSandbox (create Session Run) -> assigned Runtime Pod
-> Sandbox.OpenSession -> Send / Receive / Cancel -> Session.Close
-> Sandbox.Release (stop/delete Session Run)
```

`Sandbox` 是 SDK object，它的唯一 Session 由一个 session-mode Run 表示。`AcquireSandbox` 创建
该 Run，并在成功完成 scheduling 与 registration 后才返回 active Sandbox。此版本每个 Sandbox 只支持
一个 Session。

capacity 在 `AcquireSandbox` 创建 Run 并由 scheduler 分配给 Runtime Pod 时原子地获得。`OpenSession`
只会打开到该 Ready Session 的 streaming connection，不会创建 Run 或改变 capacity。

公开的 data-plane vocabulary 与 transport 无关：

| 操作 | 含义 |
| --- | --- |
| `Send` | 提交一个 Session operation 并返回其 operation ID |
| `Receive` | 接收下一个有序 event，其中包含 operation ID |
| `Cancel` | 请求取消一个已提交 operation |
| `Session.Close` | 只关闭 streaming connection |
| `ReleaseSandbox` | 停止 Session、等待 cleanup，然后删除其 Session Run |

SDK 不暴露 `Stream`、`Resume`、`StreamWebSocket`、NDJSON 或 WebSocket 名称。其内部维护一个持久、
已认证的双向 WebSocket connection。client message 携带 operation request 或 cancellation；server
message 携带有序的 `SessionOperationEvent` 及 operation ID。upgrade 后的 gateway error 会转换为 typed
SDK transport error，而不是普通 event。

由 Kubernetes REST configuration 构造的 SDK 会为该 connection 使用其中的 TLS CA bundle 和可选的
client certificate/key，并在配置时使用显式 bearer token。custom SDK transport 必须自行实现这一
connection boundary；仅有 HTTP `RoundTripper` 无法完成 WebSocket upgrade。受限的 Console
port-forward adapter 会像改写普通 HTTP endpoint 一样改写 connection endpoint，并保留 caller credential。

首个版本不会在 transport failure 后自动 retry 或 replay operation；此时 operation outcome 未知，caller
可以在 Session lease 到期前打开新 connection 并读取 Session state。durable operation replay 不属于该
live-connection contract。

## Lifecycle、cancellation 与边界

gateway 对 connection 的 authorization 与 unary operation 相同。`Cancel`、gateway shutdown、Immediate
Session termination 和 effective operation timeout 会取消 active Runtime Server operation 并释放 queue entry。
关闭 SDK Session connection 只会 detach event consumer，不会取消已 admission 的 work，也不会 release
Session capacity。owner runtimed 继续强制 operation timeout 与 Session lease。`Drain` 允许已 admission 的
operation 完成，并像现在一样拒绝新的 operation。

runtimed 限制每个 Runtime-emitted event，并直接转发 event，而不累计无界 response buffer。超限或 malformed
event 会以 resource-limit failure 结束 operation。Runtime 提供的 progress 不能包含 credential、command stdin
或无界 tool output。已有 response-size limit 继续约束 terminal command result 和每条 gateway JSON line。

## 兼容性与 rollout

`ExecuteSessionOperation`、`operations:execute` 与 NDJSON `operations:stream` endpoint 仍可用于 non-SDK caller。built-in Runtime
初期可以对 streaming gRPC method 返回 `Unimplemented`；gateway 会映射为明确的 capability error。interactive Runtime
通过实现该 method opt in。GitHub Issue Labeler Runtime 将发出 Pi text 和 tool lifecycle event；它已有的 unary
`message` command 继续为非 streaming client 返回最终回答。

Go 与 Python Session SDK 暴露 `AcquireSandbox`、`OpenSession`、`Send`、`Receive`、`Cancel`、
`Session.Close` 与 `Sandbox.Release`，而不是 transport-specific streaming helper。Console 为 agent turn 打开
Session connection 并逐步渲染 event。
