---
title: "自定义 Runtime 开发指南"
---

自定义 Runtime 允许你将新的执行环境接入 kruntimes，同时将 Kubernetes watches、
Run 认领、重试、artifact 上传、日志和状态更新交由 `runtimed` sidecar 处理。

Runtime Server API 是 Runtime Pod 本地的。默认不作为集群服务暴露。

## 协议约定

Runtime Server 必须实现 `api/runtime/v1/runtime.proto`：

```protobuf
service Runtime {
  rpc Execute(ExecuteRequest) returns (ExecuteResponse);
  rpc Status(StatusRequest) returns (StatusResponse);
  rpc List(ListRequest) returns (ListResponse);
  rpc Cancel(CancelRequest) returns (CancelResponse);
  rpc Forget(ForgetRequest) returns (ForgetResponse);
  rpc Health(HealthRequest) returns (HealthResponse);
}
```

生成的 Go 包是 `github.com/kruntimes/kruntimes/api/runtime/v1`。
其他语言应从同一 proto 生成客户端和服务器。

## 执行语义

`Execute` 以提供的 Run ID 启动一次执行。请求包含：

- `id`：用作执行 ID 的稳定 Run UID，
- `args`：命令或负载参数，
- `env`：Run spec 中的环境变量，
- `timeout_seconds`：Run spec 中请求的超时时间，
- `working_dir`：准备好的 workspace 目录，
- `entrypoint`：`working_dir` 内的相对入口路径，
- `handler`：可选的 `module.function` handler，用于支持函数式调用的 runtimes。

Runtime Server 应接受执行并快速返回，或返回 gRPC 错误。长时间运行的工作应异步进行，
同时通过 `Status` 报告进度。

如果以已存在的 ID 调用 `Execute`，Runtime Server 必须选择确定性行为。内置 Bash Runtime
会取消并替换之前的执行；内置 Python Runtime 拒绝重复。自定义 Runtime 作者应记录该行为，
并使其在至少一次 `Execute` 投递下是安全的。

## Status

`Status` 返回最新保留的状态：

- `EXECUTION_STATE_PENDING` 当 work 已接受但尚未启动时，
- `EXECUTION_STATE_RUNNING` 当 work 处于活跃状态时，
- `EXECUTION_STATE_SUCCEEDED` 成功完成后，
- `EXECUTION_STATE_FAILED` runtime 级别失败后。

超时和取消由 runtimed 在 Run 状态层表示。Runtime Server 仍应在自身超时到期或收到
`Cancel` 时终止工作。

`stdout`、`stderr` 和 `error_message` 必须是有界的。不要在内存中保留无界输出。
大型 artifacts 应写入 `$KRUNTIME_ARTIFACTS_DIR`；紧凑的结构化输出应写入
`$KRUNTIME_OUTPUTS`。

## List 与恢复

`List` 返回所有保留的执行记录。runtimed 在重启后调用它以重建本地活跃 Run 状态。

Runtime Server 应保留 running 和 terminal 执行状态，直到 runtimed 调用 `Forget`。
如果 Runtime Server 在 runtimed 观察到终止状态之前丢失了正在运行的执行，runtimed
将其视为 `ExecutionLost` 并应用正常的重试或终止失败策略。

## Cancel

`Cancel` 应尽力停止执行及其所有子进程。多次调用应是安全的。

推荐行为：

- 当执行 ID 未知时返回 `NotFound`，
- 终止整个进程组或等效执行树，
- 短暂等待优雅终止，
- 优雅终止未完成时强制终止，
- 取消完成后释放活跃执行资源。

## Forget

`Forget` 在 runtimed 已持久化终止 Run 状态并上传 artifacts 后释放保留的终止执行
状态和输出。

Runtime Server 应以 `FailedPrecondition` 拒绝活跃执行的 `Forget`。runtimed 将
`NotFound` 视为已释放。

## Health

`Health` 由 runtimed 就绪检查和 Kubernetes 探针使用。当 Runtime Server 无法接受
新 work 时，返回 `healthy=false` 并附带简短消息。

## Workspace 与数据路径

Runtimed 在 `/workspace/<runUID>` 下准备源代码，并将该路径作为 `working_dir` 发送。
除非其文档化的执行模型另有要求，Runtime Server 应仅在该目录内执行。

保留文件：

- `$KRUNTIME_OUTPUTS`：以换行分隔的 `key=value` 文件，用于有界 Run 输出，
- `$KRUNTIME_ARTIFACTS_DIR`：用户代码可将 artifact 文件写入以进行上传的目录。

Runtime Server 不应将大型日志、artifacts 或进度流写入 Run 状态。runtimed 拥有
artifact 上传和状态更新的所有权。

## Runtime CRD

使用 `Runtime` 对象部署 Runtime Server。模板中的第一个容器必须命名为 `runtime`。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: my-runtime
spec:
  port: 9091
  replicas: 2
  capacity:
    resources:
      runs: "2"
  template:
    spec:
      serviceAccountName: my-runtime-runtimed
      containers:
        - name: runtime
          image: ghcr.io/example/my-runtime:0.1.0
          args:
            - --port=9091
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "2"
              memory: 2Gi
```

Controller 拥有或注入以下内容：

- `runtime` 和 `app` selector 标签，
- `runtime` 容器上的 `grpc` 端口，
- `runtime` 和 `runtimed` 容器上的 `/workspace` 挂载，
- `runtimed` sidecar，
- `workspace` 和 `artifact-store` 卷，
- Runtime Pod NetworkPolicy，
- 所选 ServiceAccount 的 namespace 范围 RBAC。

使用保留名称 `runtimed`、`workspace` 或 `artifact-store` 的用户提供条目将被忽略或替换。
Artifact store 卷不会挂载到用户容器中。

Controller 保留自定义探针、资源、安全上下文、标签、注解、调度约束、镜像拉取密钥、
init 容器和额外的 sidecar，只要它们不与保留字段冲突。

## 容量

设置 `spec.capacity.resources.runs` 来控制每个 Runtime Pod 的并发 Runs 数。
Controller 将静态容量复制到 Pod 注解中。调度器使用其 Run 缓存获取快速变化的活跃使用量，
并仅分配到 Kubernetes Ready、`kruntimes.io/RuntimedReady` 且低于容量的 Pods。
Runtimed 在认领 Scheduled Run 之前也强制执行相同的本地容量。

## 安全边界

除非 Runtime 实现提供了更强的隔离，否则内置和自定义 Runtime Server 在预热 Runtime
Pods 内运行受信任的代码。运行不受信任代码的自定义 Runtime 应创建自己的 per-Run 沙箱、
容器、microVM、进程隔离、文件系统策略和网络策略。

不要在 `Run.spec.env` 中放置凭据。优先使用受信任 Runtime Pods 挂载的 Kubernetes
Secrets 或在 Run 对象外部管理的后端特定凭据。

## 兼容性

自定义 Runtime 作者应将 proto service 和 Run 生命周期语义视为 `v0.x` 的兼容性接口。
由于 CRD 是 `v1alpha1`，次版本号发布可能仍会更改字段或行为。发布说明必须明确指出
breaking changes 和迁移步骤。

在发布 Runtime 镜像之前，请测试：

- 正常成功和失败，
- 超时和取消，
- 重复或多次 `Execute`，
- 通过 `List` 进行 runtimed 重启恢复，
- `Forget` 清理，
- 有界 stdout/stderr，
- workspace 清理和 artifact 上传。
