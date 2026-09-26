# Agent Sandbox 的 Session Mode

状态：**已接受；正在实现**

## 问题

`Run.mode.function` 是固定 handler 的 RPC 模型，适合 FaaS 和 tool endpoint，但不是
agent sandbox。sandbox 需要可变 workspace、任意命令、文件操作、有序多步工作和独立 session
lifecycle。

Session mode 复用现有 `Run` 作为 lifecycle object，不增加 `Sandbox` CRD。一个 session Run
占用预热 Runtime capacity，并在关闭、过期、失败或删除前提供同一个有状态执行环境。

## Run API

`Run.spec.mode` 必须且只能设置 `task`、`function` 或 `session` 之一：

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

Run termination 是独立且单调的 control-plane 请求：

```yaml
spec:
  termination:
    mode: Drain # 或 Immediate
```

`Immediate` 取消工作。`Drain` 只对 Session Run 合法，表示已接受的 operation 完成后成功 finalization。
termination request 一旦设置，不能删除或降级；draining Session 可以升级为 `Immediate`，以在不成功导出结果时停止。

Session 使用已有的 `Pending -> Scheduled -> Running -> Ready` lifecycle。`Ready` 表示 owning
runtimed 已在本地 Runtime Server 注册 session 并接受 operation；它是 active phase 并持续占用
Runtime capacity。

## Registration Reconciliation

session registration 有意拆分为 local work 与 Kubernetes status reconciliation 两部分。runtimed
观察到被分配给自己的 `Scheduled` Run 后，先 claim Run 并通过 status 更新为 `Running`。之后对该缓存
`Running` Run 的 reconcile 会调用本地 Runtime Server 的 `GetSessionStatus`。

- `READY` 会将 Run transition 到 `Ready`，发布 endpoint，并启动 idle tracking。
- `NOT_FOUND` 且没有 registration in flight 时，异步开始 source preparation 与
  `RegisterSession`；如果已有 registration in flight，runtimed 只 requeue Run。
- `REGISTERING` 保持 `Running` 并 requeue。
- `FAILED`、`CLOSED`、`CLOSING` 或非法状态会在进入 `Ready` 前走普通 Run retry path。

异步 worker 不读取或写入 Run API object。它仅记录 local registration failure、发出 Event 与 log，
然后 enqueue Run。下一次 reconcile 消费该结果并应用普通 retry policy。这样所有 `Run.status` 更新都在
reconcile path 内完成，也不需要依赖 direct API read 抢在 informer cache update 之前读取最新对象。

v0 中用于 session 的 Runtime 应配置为有效 `runs: 1` capacity。scheduler 仍只执行通用资源
capacity 计算；runtimed 是本地独占的权威门控：它原子地拒绝在其他 Run active 时 claim
Session，并在 Session active 时拒绝 claim 其他任何 Run。这在本地竞争或 Runtime 配置错误时也能
防止混合执行；被该门控拒绝的 Run 保持 `Scheduled`，直至 Runtime Pod 可用。

`source` 与 `artifactInputs` 在 session Ready 前初始化 workspace，不是 command 定义。session
Run 不得设置 `spec.workspace`：v0 为其在 assigned Runtime Pod 创建按 Run UID 隔离的 ephemeral
workspace。同一 Pod 上的操作之间保留文件、已安装依赖和 process-visible state；Pod 丢失会使
session 失败，v0 不承诺 checkpoint、resume 或透明迁移。

`Run.spec.env` 会在 registration 时被固定，并提供给每个 session command。command 可以提供自己的
environment map；其中的值只对该 command 覆盖已注册的值。

`Run.spec.timeout` 限制整个 reservation。`idleTimeoutSeconds` 在没有 accepted mutation 或 command
activity 后过期。Session 继续使用普通 Run 的 cancellation、deletion、TTL、authorization、endpoint
和 assignment-UID fencing。注册在 `Ready` 前可以 retry，因为此时尚不存在可用的 session state；
进入 Ready 后 assigned-Pod loss 是 terminal，client 必须创建新的 Session Run，而不能在空 workspace
中静默继续。idle expiry 同样是 terminal：它会关闭本地 session、清理 ephemeral workspace，并记录为
`RunTimeout`。重新打开或再次提交同一个 Run 不能恢复它；需要新 sandbox 的 client 必须创建新的
Session Run。显式 suspend/resume 是独立的 v1 design item，不能从 timeout recovery 隐式推导。

## 删除清理

在注册 Session 或 Function 前，runtimed 会增加共享的
`kruntimes.io/registration-cleanup` finalizer。删除不同于正常 terminal phase：Kubernetes
可以在 Run 处于 `Running`、`Ready` 或 `Finalizing` 时删除它，status update 不是 deletion
protocol。finalizer 会保留 object，直到其 owning runtimed 已停止新的 data-plane work、幂等调用
`CloseSession` 或 `UnregisterFunction`、释放 local capacity，并只删除 Run-local state。只有清理
成功后才移除 finalizer；transient close error 会保留 finalizer 并 requeue reconciliation。

正常 Runtime Pod shutdown 时，runtimed 会在退出前 drain 当前的 `Running`、`Ready` 和
`Finalizing` registration，且仅在 Pod-local close 或 unregister 成功后移除该 finalizer。fenced
`PodTerminating` status transition 仍由同时监测 `Finalizing` 的 stale Run reaper 负责；hard Pod loss
会跳过 best-effort drain，因此仍依赖该 reaper。

finalizer 不会删除被引用的 PersistentWorkspace 或 ArtifactStore data；这些 resources 保留各自的
lifecycle owner。runtimed restart 后，owner 会先恢复幂等的 local registration，再关闭它；不能仅因
in-memory handle 丢失就静默移除 finalizer。

## Completion 与 Artifact Export

cancellation 与成功完成 sandbox 使用同一个单调的 `spec.termination` request，但 mode 不同。
`Immediate` 表示 terminal cancellation；`Drain` 表示正常结束已完成的 Session 工作。SDK 成功的 `Close`
helper 设置 `termination.mode: Drain`；`Cancel` helper 设置 `termination.mode: Immediate`。alpha API
直接替换较早的 `cancelRequested` boolean，而不是同时保留两个 control。

收到 completion 请求后，controller 将 Run 从 `Ready` transition 到 active 的 `Finalizing` phase。
gateway 在该 phase 拒绝新的 operation。owner runtimed 会在每个 operation 自身 deadline 内 drain 已被
接受的 operation，然后关闭本地 Runtime Server session。这样会在最终收集前冻结 ephemeral workspace。

若 Runtime 配置了 ArtifactStore，runtimed 会在准备 Session workspace 时创建
`$KRUNTIME_ARTIFACTS_DIR`。client 将显式需要导出的文件写入该目录。关闭本地 session 后，runtimed
使用普通 Run ArtifactStore contract 校验并上传这些文件，将 compact ref 写入
`Run.status.artifactRefs`，之后才将 Run transition 到 `Succeeded`。command history、任意文件内容和
unbounded output 不会写入 Run status。

ArtifactStore transport failure 会使 Run 保持 `Finalizing`，保留 local workspace 并重试。invalid artifact
则以 artifact-specific reason 进入 terminal `Failed`。若 finalization 期间请求 `Immediate` termination，
则它优先：停止 drain 和 artifact export、关闭 session，并记录 `Cancelled`。timeout 与 assigned-Pod loss
同样保持其已有 terminal 语义；它们不会将不完整 workspace 标记为成功导出。

## 服务与请求路径

以下概念必须区分：

- **Runtime gateway**：当 Helm chart 的 `gateway.enabled` 为 true 时安装的共享
  `runtime-gateway` Deployment。其 Pod 为集群中的所有 Runtime 运行 gateway server；它是 stateless
  的，不拥有 session、不维护 operation queue，也不直接调用 Runtime Server。
- **Runtime gateway Service**：指向 Runtime gateway Deployment 的共享 Kubernetes `ClusterIP`
  Service。它是 `Run.status.endpoint` 中的稳定 HTTP 地址。
- **Runtime gateway server**：每个 Runtime gateway Pod 内的 HTTP server。Kubernetes Service 只能
  转发 traffic，不能把 HTTP request 转换为 gRPC；gateway server 解析当前 Session Run assignment，
  再经该 Runtime 的 Kubernetes Service 通过 `SessionRuntime` gRPC 发送每个 request。它不拥有
  session、不选择 Runtime Pod，也不维护 operation queue。
- **Runtime Service**：由 Runtime controller 为每个 Runtime 创建的 ClusterIP Service。它选择该
  Runtime 的 ready Pod 并暴露 runtimed 的 `session-runtime` port；gateway 只能通过此 Service
  到达 Runtime Pod。
- **runtimed**：运行在每个 Runtime Pod 中，并为 gateway traffic 实现 `SessionRuntime`。runtimed 可能收到
  实际 assigned 给另一个 Runtime Pod 的 session request。
- **owner runtimed**：目标 Session Run 所 assigned Runtime Pod 中的 runtimed。只有它维护该 session
  的 FIFO queue、operation state、idle timer 和 local lifecycle。
- **Runtime Server**：与 owner runtimed colocated 的 execution backend。它实现独立且仅本地使用的
  `SessionRuntime` gRPC service，但只接受本地 runtimed 的调用；它不接收 client traffic、不 authorize
  Kubernetes user、不跨 Pod 路由，也不拥有 session queue。

chart 而非 controller 拥有 gateway Deployment、Service、RBAC 和 replicas。values 控制是否安装该组件。
TLS termination 与 external exposure 在该 cluster-local Service 之外配置。Runtime controller 创建每个 Runtime
Service；Runtime 的创建、更新或删除不改变 gateway 自身的 Kubernetes resource。

请求路径如下：

```text
External client / SDK
  -> Runtime gateway Service (HTTP)
     -> shared Runtime gateway Deployment 中的 Runtime gateway server
        -> session 所属 Runtime 的 Kubernetes Service
           -> 一个 ready Runtime Pod 的 runtimed (SessionRuntime gRPC)
              -> owner runtimed（仅当第一个 runtimed 不是 owner 时）
              -> local Runtime Server (SessionRuntime gRPC)
```

Runtime gateway server 暴露 versioned HTTP API，并接收 client 的 Kubernetes bearer token。
每个 endpoint path 标识 namespace、Runtime 和 immutable Run UID。gateway server 验证目标 Run，从当前
assignment 推导 `SessionIdentity`，再调用该 Runtime 的 Service。Kubernetes 将 call 路由到一个 ready
Runtime Pod。收到 request 的 runtimed 只列出按自己 Runtime 名称索引的 Run，然后检查目标仍是具有相同
assignment 的 Session Run；若其 Pod UID 不是 assigned Pod UID，则将同一个 call 单跳转发给同一 Runtime 中的
owner runtimed。forwarding marker 防止 loop。owner runtimed 完成 queue admission，再通过 local
`SessionRuntime` gRPC 调用 colocated Runtime Server。

在转发 HTTP request 前，gateway 使用 Kubernetes `TokenReview` authenticate bearer token，并通过
`SubjectAccessReview` authorize 对精确目标 Run 的访问。为限制这些 Kubernetes control-plane 请求，gateway
默认将成功 decision cache 30 秒。这个 in-memory cache 最多保存 1024 条 entry；每个 key 由 bearer token 的
SHA-256 digest、Run namespace、name 以及 immutable UID 组成。它不会保存 bearer token、denied decision 或
authorization error。将 `--authorization-cache-ttl=0` 或
`--authorization-cache-capacity=0` 任一设为零即可禁用 cache。已缓存成功 decision 最多会在配置的 TTL 内继续
生效，因此 authorization change 在这段时间后才会影响该 decision。

每个 Runtime gateway Pod 默认最多接受 128 个并发 HTTP request。admission 不会等待：超过单个 Pod limit 的
request 立即返回 `429 Too Many Requests`，不会在 gateway queue 中无限等待。health check 不消耗 request slot。
Helm value `gateway.maxConcurrentRequests` 为每个 gateway Pod 配置该 limit。

gateway server 将下列 HTTP API operation 映射到 `SessionRuntime` gRPC method：

| HTTP API | `SessionRuntime` method | 行为 |
| --- | --- |
| `GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}` | `GetSessionStatus` | 返回 readiness 与 bounded session metadata |
| `POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:execute` | `ExecuteSessionOperation` | 执行一个 command 或 file mutation |
| `POST /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:stream` | `StreamSessionOperation` | 执行一个 operation，并流式返回有序 NDJSON progress 与 terminal event |
| `GET /v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/files` | `ReadSessionFile`、`ListSessionFiles` | 有界的 workspace-relative file access |

Exec request 必须且只能提供 `argv` 或 `shell`。`argv` 直接执行程序；`shell` 显式选择 Runtime
shell。二者都支持 bounded stdin、relative working directory、bounded environment overrides 与
operation timeout。

owner runtimed 将所有 command 和 file mutation 按每个 session 一个 FIFO queue 排序：

```text
Queued -> Running -> Succeeded | Failed | Cancelled | TimedOut
```

同一时刻只运行一个 mutation；read/list/status 不进入队列。effective queue size 是
`mode.session.queueSize` 和 runtimed global maximum 的最小值；effective operation timeout 也同时
受 `mode.session.operationTimeout` 和 global maximum 限制。v0 默认允许 32 个 queued mutation，
operation limit 为五分钟；Run 只能降低 queue size 或 operation timeout。管理员通过 Helm
`runtimed.session.maxQueueSize`、`runtimed.session.maxOperationTimeout` 和
`runtimed.session.closeTimeout` 配置这些平台级上限，以及 runtimed 等待 Runtime Server 完成
session close 的最长时间。

session 关闭时，owner runtimed 拒绝新 operation、取消 queued work、向 running process group 发送
termination，等待 grace period 后仍未退出则 force-kill。

graceful process-termination period 是 Runtime Server 的实现配置，而不是 Run 或 Runtime API 字段。内置
Runtime chart 分别通过 `bash.sessionTerminationGraceSeconds` 和
`python.sessionTerminationGraceSeconds` 暴露该配置，默认均为两秒。直接创建 Runtime CR 的用户应在
`spec.template.spec.containers[0].args` 配置 image 文档化的 flag，例如：

```yaml
spec:
  template:
    spec:
      containers:
        - name: runtime
          args:
            - --session-termination-grace=2s
```

所有支持 Session Mode 的 Runtime Server 都必须停止 active operation 及其 child process tree，并在配置的
grace period 后 forcefully 终止 backend-specific execution unit。Bash 和 Python 使用 process group；sandbox
或 microVM backend 可以采用自己的等价机制。Runtime Server 作者必须文档化自己的配置和行为。operator
必须让配置的 grace 小于 `runtimed.session.closeTimeout` 并留出足够余量，以便 operation 退出后
`CloseSession` 能够返回。

file path 必须相对 session workspace；absolute path、traversal 和 symlink escape 都被拒绝。v0 中，gateway
JSON request body 上限为 1 MiB。内置 Runtime Server 将 direct file read/write，以及每个 command 的 stdout 与
stderr stream 均限制为 1 MiB。更大的 durable result 使用 ArtifactStore。

`ListSessionFiles` 只列出 direct child，并且必须分页。HTTP endpoint 接受可选的 `path`、`limit` 与
`pageToken` query parameter。`limit` 默认是 100，合法范围为 `1..1000`；非法值返回 `400 Bad Request`。
每个 response 都包含 `entries` array 与 `nextPageToken` string。空的 `nextPageToken` 表示未观察到后续
entry。例如：

```json
{
  "entries": [
    {"path": "build.log", "directory": false, "sizeBytes": 1024}
  ],
  "nextPageToken": "eyJ2IjoxLCJwYXRoIjoiIiwiYWZ0ZXIiOiJidWlsZC5sb2cifQ"
}
```

token 对 client 是 opaque 的，但可在同一个 Runtime 的 ready Pod 之间传递。它是 versioned base64url
cursor，包含请求的 relative directory 与最后一个返回 entry 的名称。Runtime Server 拒绝 malformed token，
以及被用于不同 directory 的 token。entry 按其 UTF-8 encoded name 的 byte-wise lexicographic order
排序；token 从严格晚于该名称的 entry 继续。此排序是 cross-runtime contract 的一部分，因此 Bash 和 Python
Runtime Server 产生可互换的 page boundary。

listing 不是 filesystem snapshot。翻页期间的 mutation 可能让后续页面遗漏或重复 entry；需要 fresh view 的
caller 必须从空 token 重新开始。Runtime Server 而非仅 HTTP gateway 强制 page limit，因此 direct
`SessionRuntime` caller 也不能造成无界 listing response。Go 与 Python SDK 暴露显式分页的 `ListFiles`
operation，接收 page-options value 并返回一页结果及 next token；不会使用 helper 隐藏无界 iteration。Go SDK
使用 `ListFiles(ctx, ListFilesOptions{Directory, Limit, PageToken})`，返回带有 `Entries` 与
`NextPageToken` 的 `FilePage`；Python SDK 使用
`list_files(ListFilesOptions(directory, limit, page_token))`，返回带有 `entries` 与
`next_page_token` 的 `FilePage`。limit 为零时不发送该 query parameter，并由 Runtime Server 选择默认 page
size。

## Runtime Server 协议

owner runtimed 负责 queue admission、operation lifecycle、Run status updates、capacity release 和
structured audit logs。Runtime Server 负责本地 workspace confinement、process groups 和 local session
state。两个 hop 使用同一套 gRPC `SessionRuntime` message：runtimed 为 gateway traffic 实现该 service，并将
owner 已接受的 request proxy 到本地 Runtime Server。session queue 与 operation state 只由 runtimed 维护，
因此 Runtime Server 不会独立重排工作。

`SessionRuntime` 是可选的 Runtime Server extension。未实现它的 Runtime 仍然可以支持 Task 或 Function
Run。将 Session Run 分配给该 Runtime 时，会在 registration 阶段因 Runtime capability mismatch 失败；
Session support 不在 `Runtime.spec` 中声明。

`SessionRuntime` 的 method 集合为：

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

`ListSessionFilesRequest` 除 `path` 外携带可选的正整数 `limit` 与 opaque `page_token`。
`ListSessionFilesResponse` 在 direct child entries 外携带 `next_page_token`。Runtime Server 验证与 HTTP
endpoint 相同的 `1..1000` limit 与 cursor semantics。

每个 request 携带 immutable Run UID 和 assignment identity。gateway server 从当前 Run assignment 推导该
identity；它不是 client 控制的 HTTP input。收到 request 的 runtimed 要么将其转发给 owner，要么在自己是
owner 时完成 queue admission 后调用本地 Runtime Server。`RegisterSession` 接收已准备的 workspace path 和
immutable source inputs；同一 identity 下调用是幂等的。`ExecuteSessionOperation` 包含恰好一个 `oneof`
payload：command、file write、directory creation、delete 或 rename。其 request context 携带 command timeout；
cancellation 终止对应 process group。read/list RPC 是 synchronous、有界的，不进入 mutation queue。本地
Runtime Server 不路由 request，也不分配 operation state。`CloseSession` 是幂等操作：owner runtimed 拒绝新的
gateway operation 后，它清理 local state。

external client 通过共享的 Runtime gateway Service 调用 HTTP，绝不直接访问 Runtime Server。gateway server
调用目标 Runtime Service 的 `SessionRuntime` endpoint；owner runtimed 将已接受的 request proxy 到本地
Runtime Server。

## SDK Contract

面向 agent 的 Go 和 Python SDK 将 Session Run 称为 **Sandbox**。这只是用户层语义：一个 Sandbox
由一个 `spec.mode.session` 的 `Run` 支撑，Kubernetes Run lifecycle 仍是权威来源。SDK 不创建另一种
Sandbox resource，也不会绕过 gateway。

两个 SDK 提供相同的 lifecycle 与操作：

| Helper | 行为 |
| --- | --- |
| `Create` | 使用请求的 Runtime、source、artifact inputs、environment 与 timeout settings 创建 Session Run |
| `Open` | 读取一个已有的、具名的 Session Run；绝不创建或重新注册它 |
| `Wait` | watch 或 poll 至 `Ready` 或 terminal Run phase；返回 typed terminal 或 readiness error |
| `Execute` | 通过 Run endpoint 发送恰好一个 command 或 file mutation；绝不隐式重试 mutation |
| `ReadFile`、`ListFiles`、`WriteFile`、`CreateDirectory`、`DeleteFile`、`RenameFile` | 使用有界且 workspace-relative 的 gateway operations |
| `Logs` | 读取 assigned runtimed container log，并按不可变 Run UID 过滤结构化日志行；不引入 gateway log store |
| `Close` | 设置 `spec.termination.mode: Drain`，等待 finalization、artifact export 和 `Succeeded`；任何其他 terminal phase 都返回 typed state error |
| `Cancel` | 设置 `spec.termination.mode: Immediate`，等待 `Cancelled`、Runtime Server close、workspace cleanup 与 capacity release；任何其他 terminal phase 都返回 typed state error |

`Open` 和每次 data-plane call 都从当前 Run status 推导 endpoint。SDK 会拒绝非 Session Run、未处于
`Ready` 的 Run，或 endpoint Run UID 与已打开 Run 不匹配的情况。HTTP failures 以保留 status code 和
有界 server message 的 typed errors 暴露。SDK 绝不会把 transport failure 当作 mutation 未执行的证明。

in-cluster caller 使用调用者的 Kubernetes REST credentials 创建/watch Runs、读取 runtimed logs，并向
Runtime gateway Service 认证。local caller 则对 shared Runtime gateway Pod 建立 scoped port-forward，保持
endpoint path 不变，并使用同一套 Kubernetes credentials。Runtime Servers 和 runtimed gRPC ports 在两种
模式下都仍是私有实现细节。

public Session API 不暴露 Runtime backend。v0 backend 是每个 Runtime Pod 一个 trusted container
session。未来 Runtime implementation 可以用 gVisor 或 microVM actor 在 worker Pod 中 multiplex 多个
session，而无需改变 Run 或 `SessionRuntime` API。该模型借鉴 Agent Substrate 的 actor/worker 分层，但不把其
snapshot/multiplexing 实现引入 v0。

## 安全与可观测性

v0 session 是 **trusted-workload preview**，不是 untrusted LLM-generated code 的安全隔离。
Runtime Pod template 仍负责 image pinning、ServiceAccount、resource limits、security context 与
network policy。v1.0 必须至少提供一种 secure session backend，初始目标为 gVisor。

owner runtimed 会为每个 session mutation 输出 structured JSONL container logs，包含 Run UID、assignment identity、
type、timestamps、result 和 exit code。bounded command output 使用已有的 `stdout` 与
`stderr` stream；独立的 `audit` line 不包含 command text、stdin 或 file contents。kruntimes 不持久化
这些日志，Kubernetes log collector（例如 Fluent Bit）负责持久化和导出。command history、file contents、
stdout、stderr 与 high-frequency event 不写入 `Run.status`。ArtifactStore 是大输出与 export 的 durable path。

## 非目标

v0 不提供 session checkpoint/resume、branching、persistent REPL processes、durable audit storage、
concurrent mutations、shared Runtime Pod sessions 或 untrusted code 的 secure execution。
