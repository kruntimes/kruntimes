# Function Mode 生命周期与 Invoke Dataplane

状态：**提案，等待 review**

本文细化 [Function Mode 和 Agent Sandboxes](../function-mode/) 中的 function-mode 目标，
定义实现前必须 review 的 Run lifecycle、status API、gateway boundary、recovery behavior 和
invocation semantics。

## 问题

现有设计确定了 function-mode Run 和 Runtime gateway，但仍有若干 correctness 与 security
问题没有明确：

- `Ready` 是否为 terminal phase，以及通用 Run controllers 如何对它分类；
- registration 失败或 assigned Runtime Pod 丢失后如何处理；
- cancellation、deletion、total timeout 和 idle timeout 如何释放 capacity；
- invoke request 如何在热路径不查询 Kubernetes API 的情况下到达 owning runtimed；
- caller 如何完成 authentication 和 authorization；
- transport retry 是否可能导致 invocation 被执行两次；
- invocation concurrency、payload、outputs、artifacts 和 logs 如何保持有界；
- 哪些状态属于 Run status，哪些应留在 dataplane。

只增加 `Ready` phase 和 endpoint 字段会让这些行为继续保持未定义，并增加后续兼容成本。

## Control Plane 与 Dataplane

Kubernetes 仍是持久化 lifecycle control plane：

- function-mode `Run` 声明 source、Runtime、handler、timeouts 和 retry policy；
- scheduler 与 task Run 一样分配 Runtime capacity；
- runtimed 准备并注册 function、更新有界 Run status，并释放 reservation；
- Runtime Pod 丢失后由 controllers 恢复或终止 Run。

Invocation 是 dataplane operation：

- client 通过 Runtime gateway Service invoke；
- runtimed 使用内存中的 ownership/readiness cache 路由；
- owning runtimed 调用本地 Runtime Server；
- 单次 invocation 不创建 Kubernetes object，也不向 Run status 追加 history。

Scheduler 和 Runtime Server 继续不感知 agent、sandbox、Workflow 或 SDK session。

## Run Status API

Run phase enum 增加 `Ready`：

```go
const RunReady RunPhase = "Ready"
```

`Ready` 是 active、non-terminal phase。通用 phase helpers、capacity accounting、
stale-pod recovery、cancellation、metrics、CLI waits、TTL cleanup 和 completed Run GC 都必须
显式识别它。只有 `Succeeded`、`Failed`、`Timeout` 和 `Cancelled` 保持 terminal。

Function-mode status 增加一个稳定 endpoint reference：

```go
type RunEndpointProtocol string

const RunEndpointProtocolHTTPS RunEndpointProtocol = "HTTPS"

type RunEndpoint struct {
    Protocol RunEndpointProtocol `json:"protocol"`
    URL      string              `json:"url"`
    CABundle []byte              `json:"caBundle,omitempty"`
}

type RunStatus struct {
    // 省略已有字段。
    AssignedPodUID string       `json:"assignedPodUID,omitempty"`
    Endpoint       *RunEndpoint `json:"endpoint,omitempty"`
}
```

`assignedPodUID` 用于区分 Pod name reuse，并与 `assignedPod` 一起设置和清除。endpoint URL
限制为 2048 bytes，PEM CA bundle 限制为 16 KiB；这些 bounds 属于 CRD validation。

第一版 public invoke protocol 是 HTTPS。Runtime Server communication 继续使用内部 gRPC，
不会通过该字段暴露。当 Runtime gateway 使用 controller-managed CA 时，`caBundle` 包含有界
PEM trust bundle；certificate 链到 client 已配置的 trust root 时可省略。支持其他 public
protocol 需要后续 API review。

endpoint path 包含 namespace、Run name 和 immutable Run UID，避免被删除的 Run name
意外指向后续新建 Run：

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

`Ready=True` 表示 assigned runtimed 已使用 Run UID 在本地 Runtime Server 完成注册，并可
接受 invokes。在 scheduling、registration、retry、cancellation 和 terminal phases 中，它为
false，并带有明确 reason。Run recovery 期间 endpoint 可以保持稳定；client 必须使用 phase
或 Ready condition 判断 readiness，不能只检查 endpoint 是否存在。

Opaque Runtime Server registration IDs、ownership-cache entries、invocation counters 和
invocation history 不属于 Run status。Run UID 是 runtimed 与 Runtime Server 之间的稳定
registration identity。

## Spec Transition Rules

Execution inputs 创建后不能变化。CRD transition validation 应使 `runtime`、`source`、`mode`、
`env`、`timeout` 和 `retryPolicy` 对 task 和 function Runs 都不可变。否则 reconciler 可能在
assignment 后观察到不同 program 或 execution mode。

`cancelRequested` 只能从 false 变为 true。已有 `ttlSecondsAfterFinished` 可以保持 mutable，
因为它控制 execution 结束后的 retention，而不是 execution behavior。

这些是 experimental API 的 breaking validation changes，必须在重新生成 CRD 前完成 review。

## Lifecycle State Machine

正常 function lifecycle 为：

```text
Pending -> Scheduled -> Running (preparing/registering) -> Ready
```

runtimed claim Run 时设置 `Run.status.startTime`。在 function mode 中，`Run.status.attempt`
使用已有 shared retry engine 统计 registration lifecycle attempts，并包含首次 attempt。对同一
attempt 的不确定 Pod-local registration RPC 重试是幂等的，不会递增 status。

Terminal transitions：

- assignment 前 cancellation 由 scheduler 结束为 `Cancelled`；
- assignment 后 cancellation 由 owning runtimed unregister、清理本地状态、释放 capacity，
  然后设置为 `Cancelled`；
- registration error 按 `retryPolicy` 重试，耗尽后进入 `Failed`；
- assigned Runtime Pod 丢失会让 Run unavailable，并进入带 fencing 的 stale recovery；旧
  assignment 被 fence 后，新 attempt 可以在另一个 ready Pod 上注册，耗尽后进入 `Failed`；
- `spec.timeout` 在设置时表示从 `startTime` 开始计算的最大 reservation lifetime；过期后
  unregister 并进入 `Timeout`；
- `mode.function.idleTimeoutSeconds` 在一个 registration epoch 内没有 completed 或 in-flight
  invocation activity 后过期，并以 reason `IdleTimeout` 进入 `Timeout`。

Invocation-level handler error、request timeout 和 concurrency rejection 不改变 Run phase，
也不消耗 Run retry attempt。它们是单次 invocation 的结果，不是 registered function lifecycle
失败。会使 registration 失效的 fatal Runtime Server error 会让 Run not ready 并进入
registration recovery。

Runtime Server 负责当前 registration generation 的精确 last-activity clock，并通过
`FunctionStatus` 暴露；不会每次 invoke 都 checkpoint 到 etcd。如果 Runtime Server process
恢复 registration，也会恢复该 clock。如果 Runtime Pod 被替换，新 registration 会创建新
epoch 并重置 idle timer。这样既避免提前过期，也不会形成高频 Run status write path。

## Ownership Epoch 与 Fencing

Run UID 是稳定的 external function identity。新 registration 使用 `status.attempt`、
`assignedPod` 和 `assignedPodUID` 进行 fencing。`RegisterFunction` 携带 Run UID 和 registration
attempt，然后返回 opaque local registration ID。`FunctionStatus`、`InvokeFunction` 和
`UnregisterFunction` 使用该 registration ID，而不重复传递 attempt。local gateway 仅在 active
assignment 与最新 cache entry 匹配时 invoke。

Pod readiness 或 stale heartbeat 本身不能证明旧 function 已停止。Recovery 会先使旧
assignment 无法再通过 gateway Service 到达，并确认以下任一 fence：

- 精确 assigned Pod UID 已不存在；
- Pod 正在 deleting，且已从 Service endpoints 移除；
- old owning runtimed 已确认 unregister 对应 epoch。

只有满足 fence 后，shared retry 才能清除 assignment 并调度新 attempt。Peer routing request 携带
expected Run UID、registration attempt 和 assigned Pod UID，mismatch 时被拒绝。这可以防止正常
recovery 同时向两个 assignment routing。网络在 dispatch 后失败时，kruntimes 仍不承诺 exactly-once invocation；这一独立
ambiguity 由后面的 invoke contract 定义。

## Cleanup 与 Finalization

注册 function 前，runtimed 增加 `kruntimes.io/function-cleanup` finalizer。Function Run 进入
deleting 后，即使正常 execution creation 已停止，仍需 reconcile：

1. 停止接受新 invokes；
2. 等待或取消有界的 in-flight invokes；
3. 幂等调用 `UnregisterFunction`；
4. 删除 function-local workspace 和 retained invocation state；
5. 释放 active capacity entry；
6. 移除 finalizer。

如果 assigned Pod 已不存在，stale-recovery controller 可在确认 Pod-local registration 不可能
继续 serving 后移除 finalizer。PersistentWorkspace 和 ArtifactStore cleanup 仍由已有
controllers 与 policies 负责；function finalizer 不得隐式删除 shared persistent data。

Cancellation 保留 Run object 并记录 terminal status。Deletion 是独立的 user request，使用
finalizer path。两种操作在 controller 和 runtimed restart 后都必须保持幂等。

## Runtime Gateway Service

Runtime controller 为每个 Runtime 创建一个 ClusterIP gateway Service。Service 由 Runtime
owner，并通过专用 runtimed gateway port 选择该 pool 中所有 ready Runtime Pods。Controller
同时管理 Runtime-scoped CA 和只挂载到 runtimed 的 serving certificate Secret。Certificate
覆盖 gateway Service DNS name。Rotation 会先发布同时包含 old/new CA 的 bundle，再替换
serving certificates，并在所有 Runtime Pods 完成 rollout 后删除 retired CA。

```text
Runtime gateway Service
  -> Runtime pool 中任意 runtimed
     -> owning runtimed（直接到达或经过一次 peer proxy）
        -> local Runtime Server
```

每个 runtimed 为其 Runtime 和 namespace 中所有 assigned function Runs 维护 watch-backed
cache：

```text
Run namespace/name/UID -> assigned Pod address -> lifecycle readiness
```

cache 由 informer events 异步填充。invoke handler 不得同步从 Kubernetes API 读取 Run 或
Pod。请求处理规则：

- UID 或 current-object 不匹配：`404 Not Found`；
- Run 已知但 not ready、recovering 或 terminating：`503 Service Unavailable`；
- local owner：调用本地 Runtime Server；
- remote owner：最多 proxy 一次到 owning Pod gateway port；
- owner stale 或 unreachable：返回 `503 Service Unavailable` 并 enqueue recovery；
- 达到 local concurrency limit：`429 Too Many Requests`。

peer request 保留原 caller credential，并由 owner 再次 authorization。Peer connection 使用
TLS 并验证相同 Runtime-scoped CA；连接 owning Pod address 时，client 使用 shared serving
certificate 中的 gateway Service DNS name 完成校验。hop marker 防止 proxy loop。Service
routing 和 cache recovery 必须容忍请求落到尚未通过 watch cache 观察到最新 assignment 的
Pod。

## Authentication 与 Authorization

第一版 gateway 使用 HTTPS 且仅为 ClusterIP，不声称自己是 Internet ingress。由于每个
client request 都携带 Kubernetes bearer token，因此不支持 plaintext HTTP。接收请求的
runtimed 使用该 token 提交 `SelfSubjectAccessReview`，并要求 caller 有权 `get` 被 invoke 的
精确 Run。Token 必须对 Kubernetes API audience 有效。

Authorization decision 最多缓存 30 秒，并且不能超过 token 已知 expiry。cache key 使用 token
digest 加 Run namespace、name 和 UID；raw token 不记录到日志或 Run status。Proxy request
转发原 token，owner 重复执行或复用同样有界的 authorization decision。

这会在 authorization cache miss 时产生 Kubernetes API traffic，但 ownership/readiness
routing 不查询 API。它能保留 namespace RBAC，并避免为 custom Runtime service account
授予广泛 Secret access 或 TokenReview 权限。

External exposure 需要单独 review 的 authenticated ingress 或 gateway。即使开启 Kubernetes
authorization，也应通过 NetworkPolicy 将 direct gateway access 限制到预期 agent 和 platform
namespaces。

## Invoke Contract

第一版 HTTPS request 明确保持有界：

```http
POST /v1/namespaces/{namespace}/runs/{name}/{uid}/invoke
Authorization: Bearer <kubernetes-token>
Content-Type: application/json
X-Kruntime-Invocation-ID: <caller-generated-id>
```

初始限制：

- request body：1 MiB；
- invocation ID：128 bytes；
- outputs：使用 bounded Run outputs 相同的 key/count/value limits；
- artifact references：使用 Run artifact references 相同的 count 和 metadata limits；
- v0.x 中每个 function Run 只允许一个 in-flight invocation；
- 不提供无界 server-side queue。

Runtime gateway 在 dispatch 到 owner 或 Runtime Server 后绝不自动 retry invocation。连接失败
可能代表 unknown execution outcome。SDK 也默认不 retry execution。未来 deduplication 或
idempotency contract 可以使用 caller-generated invocation ID，但 v0.x 不承诺 exactly-once。

成功和失败 response 都包含 invocation ID。Structured runtimed logs 包含 Runtime、Run UID 和
invocation ID。Invocation stdout/stderr 通过 log collection 或 Runtime Server contract 定义的
bounded response field 暴露，绝不追加到 `Run.status.message`。

## Runtime Server Boundary

内部 gRPC API 增加以 Run UID 为 key 的幂等 lifecycle operations：

- `RegisterFunction`：注册 working directory、handler 和 environment；
- `FunctionStatus`：报告 registered/readiness state、fatal errors、in-flight count，以及当前
  registration generation 的 last activity；
- `InvokeFunction`：使用 invocation ID 和 timeout 执行一次 bounded request；
- `UnregisterFunction`：停止新请求并释放 registration。

使用同一 immutable configuration 重复注册同一 Run UID 和 registration attempt 应幂等成功，并返回
同一个 opaque registration ID；使用不同 configuration 注册同一 attempt 必须失败。更高的
registration attempt 创建新的 generation 并使旧 ID 失效。Unregister 不存在的 registration 应成功。
具体 protobuf fields 和 Bash/Python adapter behavior 属于独立 implementation PR，但必须保持这些语义。

## Capacity 与 Concurrency

Runtime `runs` capacity 控制一个 Runtime Pod 可以承载多少个 task executions 或 registered
function Runs。Ready function Run 在 terminal cleanup 或 deletion 释放前持续占用一个 unit。

Invocation concurrency 不是 scheduler capacity。v0.x 中，runtimed 与 Runtime Server 对每个
function Run 只允许一个 in-flight invocation，并对 overlap request 返回 `429`。后续设计可以
增加 per-function 和 Runtime-wide invocation concurrency 或 queue controls，而不让 scheduler
感知单次 invoke。

需要 exclusive Pod 和 workspace ownership 的 agent sandbox deployment 应继续使用 Runtime
`runs: "1"`。SDK 可以 validation 或 warning 这一部署选择，但不能改变 scheduler semantics。

## Component Boundaries

| Component | Responsibility |
| --- | --- |
| Runtime controller | Reconcile Runtime-owned gateway Service 和 ports。 |
| Scheduler | 分配通用 Run capacity；把 Ready 视为 active、non-terminal。 |
| runtimed | Register/unregister、维护 lifecycle status 和 routing cache、authorize/proxy invokes、执行 local limits，并清理本地状态。 |
| Runtime Server | 保存 registration/activity state、执行 invokes，并暴露幂等 local lifecycle APIs。 |
| stale recovery | 检测 owning Pod 丢失、释放已经不可能执行的 local cleanup，并进入 shared Run retry/exhaustion semantics。 |
| SDK 和 `krt` | Watch readiness、发现 endpoint、提供 credential、在不隐式 retry execution 的情况下 invoke，并暴露 typed errors。 |

Workflow controllers 不感知 function registration 和 invocation。后续它们可以创建 function
Runs，但只使用 public Run API。

## Implementation Plan

1. 增加 API prerequisites：`Ready`、assigned Pod UID、endpoint status、transition validation、
   finalizer constant、generated CRDs 和通用 phase-classification tests。
2. 增加 Runtime gateway Service reconciliation 以及 Helm/RBAC coverage。
3. 增加 Runtime-scoped gateway TLS certificate reconciliation、CA publication、rotation 和
   pod rollout behavior。
4. 增加 Runtime Server register/status/invoke/unregister protobuf APIs 和内置 Bash/Python
   adapters。
5. 增加 runtimed registration lifecycle、finalization、shared retry integration、timeout handling
   和 restart recovery。
6. 增加 HTTPS gateway、watch-backed routing cache、local/peer dispatch、
   SelfSubjectAccessReview authorization、limits 和 metrics。
7. 增加 `krt invoke`，然后增加 Go/Python sandbox SDK connection layer。
8. 增加覆盖 registration、TLS rotation、authorization、local/proxied invoke、concurrency rejection、
   cancellation、deletion、idle/lifetime timeout、Runtime Pod loss 和 retry exhaustion 的 E2E。
9. 只有 supported path 通过 E2E 后，才完成 agent sandbox demo。
