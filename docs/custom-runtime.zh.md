---
title: "自定义 Runtime 开发指南"
---

自定义 Runtime 允许你将 workload-specific execution environment 接入 kruntimes，同时将
Kubernetes watches、Run 认领、重试、artifact 上传、日志和状态更新交由 `runtimed`
sidecar 处理。

大多数 custom Runtime 应从扩展已有 Runtime image 开始。只有当内置执行语义不够用时，才
需要实现 Runtime Server API。

## Runtime Image 定制

对于 Runtime image 的定制有两个选项：

1. **扩展内置 Runtime image。** 从 Bash 或 Python Runtime image 开始，添加 packages、
   内部 binaries、certificates、SDKs 或 config files，然后将该 image 注册为新的
   `Runtime`。这是大多数团队推荐的第一条路径。
2. **实现 Runtime Server。** 当你需要不同的执行语义、专用 worker、sandboxing layer
   或非进程执行模型时，实现 Runtime gRPC API。

两种 image 定制选项都使用同一个 `Runtime` CRD，也使用同一个
`Runtime.spec.template` Pod template 模型。

### Option 1：扩展内置 Runtime Image

当 Bash 或 Python 执行语义已经够用，但运行环境需要额外工具时，使用这条路径。

下面是一个安装 `jq` 的 Bash Runtime image 示例：

```dockerfile
ARG KRUNTIMES_VERSION=0.0.3
FROM ghcr.io/kruntimes/bash-runtime:${KRUNTIMES_VERSION}

USER 0
RUN apt-get update \
  && apt-get install -y --no-install-recommends jq \
  && rm -rf /var/lib/apt/lists/*
USER 65532
```

构建并推送到集群可拉取的 registry：

```bash
CUSTOM_BASH_IMAGE=ghcr.io/example/my-bash-runtime:0.1.0
docker build \
  --build-arg KRUNTIMES_VERSION=0.0.3 \
  -t "${CUSTOM_BASH_IMAGE}" \
  ./my-bash-runtime
docker push "${CUSTOM_BASH_IMAGE}"
```

可以用同样方式添加内部 CLI、model tools、CA certificates 或语言包。最终 image 的用户
应与你计划使用的 security context 兼容。

### Option 2：实现 Runtime Server Image

当内置 Runtime execution model 不够用时，使用这条路径。Runtime Server 是运行在
Runtime Pod 内部的进程，实现 `api/runtime/v1/runtime.proto`。

Runtime Server API 是 Runtime Pod 本地的。默认不作为集群服务暴露。

server 必须实现：

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

打包 image 时，Runtime Server 应监听 `Runtime.spec.port` 引用的端口。Runtime Pod
template 中的第一个容器必须命名为 `runtime`。

## 注册 Runtime

两种 custom image 都用 `Runtime` 对象注册：

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
          env:
            - name: EXAMPLE_MODE
              value: production
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: "2"
              memory: 2Gi
```

`spec.capacity.resources.runs` 控制每个 Runtime Pod 的并发 Runs 数。Controller 将静态
capacity 复制到 Pod 注解中。scheduler 使用 Run cache 获取快速变化的 active usage，并
只分配到 Kubernetes Ready、`kruntimes.io/RuntimedReady` 且低于 capacity 的 Pods。
runtimed 在认领 Scheduled Run 前也强制执行相同的本地 capacity。

## 定制 Pod Template

`Runtime.spec.template` 是 Pod template。Runtime owner 可以定制：

- runtime container image 和 args，
- CPU 和 memory requests / limits，
- environment variables，
- security context，
- ServiceAccount，
- image pull secrets，
- node selectors、tolerations、affinity 和 topology spread constraints，
- init containers，
- extra volumes 和 mounts，
- additional sidecars。

Controller 会保留不冲突的 Pod template 字段，并注入 kruntimes 管理的字段：

- `runtime` 和 `app` selector labels，
- `runtime` 容器上的 `grpc` port，
- `runtime` 和 `runtimed` 容器上的 `/workspace` mount，
- `runtimed` sidecar，
- `workspace` 和 `artifact-store` volumes，
- Runtime Pod NetworkPolicy，
- 所选 ServiceAccount 的 namespace-scoped RBAC。

使用保留名称 `runtimed`、`workspace` 或 `artifact-store` 的用户提供条目将被忽略或替换。
artifact store volume 不会挂载到用户容器中。

## Runtime Server 语义

本节只适用于自己实现 Runtime Server 的路径。

`Execute` 以提供的 Run ID 启动一次执行。请求包含：

- `id`：用作执行 ID 的稳定 Run UID，
- `args`：runtimed 规范化 Run input semantics 后得到的命令或负载参数。Inline source
  会作为独立的 `script` 发送，并带空 args。当 workspace 中存在 entrypoint 文件时，
  内置 Runtimes 会将这些值作为 entrypoint 参数传入。当没有准备 source 或 entrypoint
  文件时，每个 Runtime 需要文档化自己如何解释 args。
- `env`：Run spec 中的环境变量，
- `timeout_seconds`：Run spec 中请求的超时时间，
- `working_dir`：准备好的 workspace 目录，
- `entrypoint`：`working_dir` 内的相对入口路径；对于 inline source，runtimed 会发送
  `script`，
- `handler`：可选的 `module.function` handler，用于支持函数式调用的 runtimes。

Runtime Server 应接受执行并快速返回，或返回 gRPC 错误。长时间运行的工作应异步进行，
同时通过 `Status` 报告进度。

如果以已存在的 ID 调用 `Execute`，Runtime Server 必须选择确定性行为。内置 Bash Runtime
会取消并替换之前的执行；内置 Python Runtime 拒绝重复。custom Runtime 作者应记录该行为，
并使其在至少一次 `Execute` 投递下是安全的。

`Status` 返回保留的状态：pending、running、succeeded 或 failed。超时和取消由 runtimed
在 Run status 层表示。Runtime Server 仍应在自身超时到期或收到 `Cancel` 时终止工作。

`List` 返回保留的执行记录，使 runtimed 能在重启后恢复 active Runs。Runtime Server 应
保留 running 和 terminal executions，直到 runtimed 调用 `Forget`。`Forget` 在 runtimed
已持久化 Run status 并上传 artifacts 后释放 terminal execution state。

`Cancel` 应尽力停止执行及其所有子进程。多次调用应是安全的。

`Health` 由 runtimed readiness checks 和 Kubernetes probes 使用。当 Runtime Server 无法
接受新 work 时，返回 `healthy=false` 并附带简短消息。

## Workspace 与数据路径

Runtimed 在 `/workspace/<runUID>` 下准备源代码，并将该路径作为 `working_dir` 发送。
除非其文档化的执行模型另有要求，Runtime Server 应仅在该目录内执行。

保留文件：

- `$KRUNTIME_OUTPUTS`：以换行分隔的 `key=value` 文件，用于有界 Run 输出，
- `$KRUNTIME_ARTIFACTS_DIR`：用户代码可将 artifact 文件写入以进行上传的目录。

Runtime Server 不应将大型日志、artifacts 或进度流写入 Run status。runtimed 拥有
artifact 上传和状态更新的所有权。

## 安全边界

除非 Runtime 实现提供了更强的隔离，否则内置和 custom Runtime Servers 在 warm Runtime
Pods 内运行受信任的代码。运行不受信任代码的 custom Runtime 应创建自己的 per-Run
sandbox、container、microVM、process isolation、filesystem policy 和 network policy。

不要在 `Run.spec.env` 中放置凭据。优先使用受信任 Runtime Pods 挂载的 Kubernetes
Secrets 或在 Run 对象外部管理的后端特定凭据。

## 兼容性与测试

custom Runtime 作者应将 Runtime CRD template contract、proto service 和 Run lifecycle
semantics 视为 `v0.x` 的兼容性接口。由于 CRD 是 `v1alpha1`，minor release 可能仍会更改
字段或行为。发布说明必须明确指出 breaking changes 和迁移步骤。

在发布 Runtime image 前，请测试：

- 正常成功和失败，
- 超时和取消，
- 重复或多次 `Execute`，
- 通过 `List` 进行 runtimed restart recovery，
- `Forget` cleanup，
- 有界 stdout/stderr，
- workspace cleanup 和 artifact upload。
