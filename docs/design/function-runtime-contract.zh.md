---
title: "Function Runtime Server 协议"
---

# Function Runtime Server 协议

状态：**待评审**

本文定义 function-mode Run 的内部 gRPC 协议。它细化了
[Function Mode 生命周期与 Invoke Dataplane](../function-mode-lifecycle/)，但不改变公开
Run API，也不会把 Runtime Server 暴露到 Runtime Pod 外。

## 范围

现有 `Runtime` service 支持一次性执行。Function mode 新增四个只由 runtimed 在 Pod 内
调用的操作：

- `RegisterFunction` 创建或恢复一个可调用 function 注册；
- `FunctionStatus` 查询本地就绪、活动和致命状态；
- `InvokeFunction` 执行一次有界 invocation；
- `UnregisterFunction` drain 或取消工作，并删除本地状态。

Runtime Server 不读取 Kubernetes 对象、不认证调用方、不路由 gateway 请求、不上传
artifact，也不调度 capacity。这些职责分别属于 runtimed、Runtime gateway 和 control
plane。

新增 RPC 会改变实验期 custom Runtime protocol。以下精确 message shape 和语义必须在修改
`runtime.proto`、生成 stubs 或内置 Runtime 实现前完成评审。

## 注册 Epoch

每个操作都由 Run UID 和 attempt 约束：

```protobuf
message FunctionIdentity {
  // Kubernetes Run UID，不使用可变的 name。
  string run_uid = 1;
  // 从一开始计数的 Run retry/ownership attempt。
  int32 attempt = 2;
}
```

`run_uid` 加 `attempt` 构成本地 registration epoch。runtimed 只有在拥有该 epoch 时才能调用
本地 Runtime Server。过期操作绝不能修改或删除同一 Run UID 的新注册。

`registration_digest` 是 runtimed 对规范化且不可变的注册输入计算得到的小写 SHA-256：已
解析的 source identity、handler、environment 和 Runtime 可见的注册设置。它不包含短暂的
working-directory 路径；它是幂等校验，不是凭据。

## 建议的 Protobuf API

以下是对 `executor.v1.Runtime` 的增量扩展：

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

gateway 初期接收 JSON，并设置 `content_type=application/json`。本地协议使用 opaque bytes，
使受信 custom Runtime 后续可采用其他编码。输入和 response bytes 不写入 `Run.status`。

## 注册与状态语义

`RegisterFunction` 在接收工作前校验 working directory、handler、environment、timeout 和
digest。

- 重复相同 identity 和 digest 时返回当前状态，不重新初始化 function。
- 相同 identity 使用不同 digest 时返回 `AlreadyExists`，不替换注册。
- 过期 attempt 返回 `FailedPrecondition`，不能影响新注册。
- 永久初始化失败通过 `FunctionStatus` 的 `FAILED` 和有界 `fatal_error` 暴露。

`FunctionStatus` 只读取 Runtime Server 本地状态。`last_activity_unix_nano` 在尚无已完成或
in-flight 工作时为零。`fatal_error` 是有界诊断文本而不是日志。`NotFound` 表示没有精确的
注册；`FailedPrecondition` 表示 epoch 不匹配。runtimed 以有界频率轮询它用于健康检查和 idle
timeout，绝不把每次 activity 更新写回 Kubernetes。

## Invocation 语义

`InvokeFunction` 要求精确 identity 处于 `READY`。v0.x 每个 function Run 只允许一个
in-flight invocation，并且不排队。

- `invocation_id` 由调用方生成，用于关联，最大 128 bytes。
- 它不是去重 key。未知结果后的重试可能再次执行；dispatch 后没有组件自动重试。
- `timeout_millis` 由 runtimed 限制为剩余 Run 生命周期和 gateway deadline 以内。零表示
  有界 gateway 默认值，绝不代表无限运行。
- `ResourceExhausted` 映射为 gateway HTTP 429。`DeadlineExceeded` 仅影响本次 invocation，
  不影响 function Run 生命周期。draining、stale 或未就绪注册的 `FailedPrecondition` 映射为
  HTTP 503。

`outputs` 使用与 `Run.status.outputs` 相同的 key、数量和 value 限制。Runtime Server 返回的
artifact 只是本地写入文件的声明，并不是外部 artifact 引用。每个 `relative_path` 必须非空、
相对且不含 `..` segment。runtimed 校验声明，经 Runtime ArtifactStore 上传文件，再返回公开
的 `ArtifactRef`。这让 storage credentials 和外部坐标不会进入 custom Runtime Server。

stdout/stderr 使用以 Run UID 和 invocation ID 为 key 的结构化 runtime logs；它们既不是 RPC
response field，也不写入 `Run.status.message`。

## 注销

`UnregisterFunction` 先转为 `DRAINING` 并拒绝新 invocation。`cancel_in_flight=false` 时，
最多等待 `drain_timeout_millis` 直到活动 invocation 完成；`cancel_in_flight=true` 时，立即
取消后释放 registration-local state。

注销不存在的精确 epoch 应成功。新 attempt 已存在时注销旧 attempt 必须返回
`FailedPrecondition`，避免延迟 cleanup 删除恢复后的注册。

## 限制与错误映射

| 值 | 初始限制 | 强制方 |
| --- | ---: | --- |
| Request body | 1 MiB | Gateway 与 runtimed |
| Invocation ID | 128 bytes | Gateway 与 runtimed |
| Response body | 1 MiB | runtimed |
| Outputs | 现有 Run output 限制 | runtimed |
| Artifact declarations | 现有 ArtifactRef 数量和 metadata 限制 | runtimed |
| In-flight calls | 每个 function Run 一个 | Runtime Server |
| `fatal_error` | 4 KiB | Runtime Server |

| gRPC code | 含义 | Gateway 结果 |
| --- | --- | --- |
| `InvalidArgument` | 无效 handler、path、payload 或 limit | HTTP 400 |
| `NotFound` | 未知注册 | HTTP 404，或 cache recheck 后 HTTP 503 |
| `AlreadyExists` | 相同 epoch、不同 digest | Registration failure |
| `FailedPrecondition` | Stale、draining 或未就绪 epoch | HTTP 503 |
| `ResourceExhausted` | Invocation 已在执行 | HTTP 429 |
| `DeadlineExceeded` | Invocation deadline 已到 | HTTP 504 |
| `Unavailable` | Runtime Server 无法接收工作 | HTTP 503 与 lifecycle recovery |

## 内置 Runtime 要求

Python 导入 `module.function`，传递解码后的 JSON input，并把返回值编码为 JSON output。Bash 将
handler 视为 runtime-defined executable entrypoint，通过 standard input 或有界文件接收 input，
绝不使用 shell-string interpolation。两个 adapter 都只能在已注册的 working directory 下运行，
遵守 context cancellation，并且每个 registration 最多一个活动 invocation。

现有 task-only Runtime Server 仍然有效。只有未来的兼容性/健康握手确认这些 RPC 受支持后才启用
function mode；不得通过 `Execute` 模拟 function invocation。

## 待确认的评审决策

1. 使用 Run UID 加 attempt 作为 registration epoch 和 stale-operation fence。
2. 本地 invoke payload 使用 opaque bytes 加 content type；JSON 是第一个 gateway 编码。
3. Runtime Server 只声明已校验的相对 artifact 路径；runtimed 负责上传和创建公开 ArtifactRef。
4. 不承诺 invocation-ID 去重，也不承诺自动 execution retry。
5. v0.x 每个已注册 function Run 最多一个 in-flight invocation。

批准后，实现可以拆为 protobuf/stub generation、Bash/Python adapters，以及 runtimed
registration lifecycle/gateway 工作。
