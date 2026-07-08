---
title: "API 参考"
---

kruntimes 暴露 Kubernetes CRDs 和本地 Runtime Server gRPC API。

## Kubernetes APIs

当前所有 CRDs 都是 `apiVersion: kruntimes.io/v1alpha1`。

### Run

`Run` 表示一次执行。

常见 spec 字段：

| Field | Description |
| --- | --- |
| `spec.runtime` | 要执行的 Runtime 名称。scheduler 只会考虑同一 namespace 内的 Runtime Pods。 |
| `spec.env` | 执行环境变量。不要直接在这里存储 secrets。 |
| `spec.source` | 可选的 source files 或 Git source，会被准备到 workspace。 |
| `spec.mode.task.entrypoint` | one-shot task execution 使用的 workspace 内相对路径。绝对路径和 `..` 会被拒绝。 |
| `spec.mode.task.args` | one-shot task execution 传递给 Runtime Server 的参数或命令 payload。 |
| `spec.mode.function.handler` | function-mode Runs 使用的 callable `module.function` 入口。 |
| `spec.timeoutSeconds` | 执行 timeout。timeout 的终态 phase 是 `Timeout`。 |
| `spec.retryPolicy` | retry 次数和 backoff。执行语义是 at-least-once。 |
| `spec.cancelRequested` | 用户取消请求。 |

执行输入语义：

- `spec.source.inline` 是独立脚本。存在时，runtimed 会把它写入默认的 `script` 文件，
  并且不会把 task `entrypoint` 或 `args` 传给 Runtime Server。
- `spec.mode.task.entrypoint` 为 Git source 或 workspace 中已经存在的文件选择要执行的
  相对路径。
- 使用 `spec.mode.task.entrypoint` 时，`spec.mode.task.args` 会作为参数传给该
  entrypoint。
- 当没有准备 source 或 entrypoint 文件时，task args 由所选 Runtime 解释。内置 Bash
  会将单个 arg 作为 `bash -c <arg>` 执行，保留显式 `sh -c ...` 和 `bash -c ...` 的
  shell invocation，并保持旧的多 arg 行为：把 args 拼成以换行分隔的 Bash script lines。
  内置 Python 会执行 `python <args...>`。
- `spec.mode` 是必填字段。`spec.mode.task` 和 `spec.mode.function` 必须且只能设置一个。
- `krt run -- <command> [args...]` CLI 会把 command words 原样存入
  `spec.mode.task.args`，不会额外添加 shell quoting。需要 shell evaluation 时使用
  `krt run -- sh -c '...'`，或者使用 `--file` 的 inline source mode。

常见 status 字段：

| Field | Description |
| --- | --- |
| `status.phase` | `Pending`、`Scheduled`、`Running`、`Succeeded`、`Failed`、`Timeout` 或 `Cancelled`。 |
| `status.assignedPod` | scheduler 选中的 Runtime Pod。 |
| `status.attempt` | 当前确定性 attempt count。 |
| `status.outputs` | 来自 `$KRUNTIME_OUTPUTS` 的有界结构化 outputs。 |
| `status.artifactRefs` | 存储在 etcd 之外的文件的紧凑 artifact references。 |
| `status.conditions` | 用于生命周期状态的 Kubernetes list-map conditions。 |

最小示例：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  source:
    inline: |
      echo hello
```

Task mode 示例：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello-task
spec:
  runtime: bash
  mode:
    task:
      args:
        - echo hello
```

Function mode 仍然是 experimental。当前 API shape 已经存在，因此 handler configuration
可以放在 function mode 下；但 repeated low-latency invocation 还需要 roadmap 中的 runtime
gateway 和 function runtime contract 工作。

### Runtime

`Runtime` 定义一个预热执行池。

常见 spec 字段：

| Field | Description |
| --- | --- |
| `spec.replicas` | 期望的 Runtime Pod 数量。 |
| `spec.capacity.resources` | 每个 Pod 的逻辑 capacity，包括内置的 `runs`。 |
| `spec.template` | Runtime Pods 使用的 `PodTemplateSpec`。 |
| `spec.daemonImage` | 可选的注入 `runtimed` sidecar image override。 |
| `spec.artifactStore` | runtimed 和 maintainers 使用的 artifact backend configuration snapshot。 |
| `spec.workspace` | 共享 workspace volume。默认是 `emptyDir`；也可以 inline Kubernetes `VolumeSource` 字段，例如 `persistentVolumeClaim`。 |

controller 拥有 kruntimes 所需的保留 Runtime Pod 字段，包括注入的 `runtimed` container
以及 control-plane labels/annotations。

Workspace 示例：

```yaml
spec:
  workspace:
    emptyDir:
      sizeLimit: 10Gi
```

```yaml
spec:
  workspace:
    persistentVolumeClaim:
      claimName: bash-workspace
```

### Workflow

`Workflow` 编排 child Runs。由于 API 仍然 experimental，Workflow 文档目前刻意保持最小化。

## Runtime Server gRPC API

Runtime Servers 实现 `api/runtime/v1/runtime.proto`：

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

行为要求、retries、cancellation、workspace paths 和兼容性规则见
[Custom Runtime Development Guide](custom-runtime.md)。

## Authentication and Authorization

Kubernetes RBAC 控制 CRDs 和 pod port-forwarding 的访问。Runtime Server gRPC endpoints
默认只在 Runtime Pods 本地，不暴露为 Services。NetworkPolicy 会限制对 runtimed endpoints
的直接访问。

推荐的角色隔离见 [Security and Threat Model](security.md)。

## Validation

CRDs 包含 schema 和 CEL validation，用于支持的字段、大小、名称、entrypoints 和 workflow
形状。贡献者在 API types 变化时应重新生成 CRDs；见 [Development Guide](development.md)。
