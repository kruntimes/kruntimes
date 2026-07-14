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

### PersistentWorkspace

`PersistentWorkspace` 表示一个命名的 workspace 边界，后续可以被 Runs 和 Workflow-managed
jobs 引用。它不是 Workflow-specific 对象。

当前 spec 字段：

| Field | Description |
| --- | --- |
| `spec.runtime` | 支撑该 workspace 的 Runtime workspace volume 所属 Runtime。 |
| `spec.mode` | 绑定模式。第一版支持 `RuntimePodLocal`。 |
| `spec.ttlSecondsAfterUnused` | workspace 变为 unused 后的可选保留窗口。 |
| `spec.cleanupPolicy` | 清理行为。支持 `DeleteAfterTTL` 和 `Retain`。 |

当前 status 字段：

| Field | Description |
| --- | --- |
| `status.phase` | 生命周期阶段：`Pending`、`Bound`、`Lost` 或 `Released`。 |
| `status.runtime` | observed Runtime 名称。 |
| `status.boundPod` | 绑定实现完成后支撑 workspace 的 Runtime Pod。 |
| `status.path` | 绑定实现完成后的 runtime-local workspace path。 |
| `status.lastUsedTime` | 最后一次 observed use time。 |
| `status.conditions` | lifecycle 和 validation conditions。 |

初始 controller 只负责 validation 和 lifecycle status。Runtime Pod binding、Run workspace
references 和 cleanup 仍在 roadmap 中跟踪。

### Workflow

`Workflow` 定义 reusable workflow。它是 definition object，不是 execution instance。
创建 `WorkflowRun` 才会执行 inline jobs 或调用 reusable Workflows。

当前 spec 字段：

| Field | Description |
| --- | --- |
| `spec.inputs` | 该 Workflow 接受的可选 typed string inputs。 |
| `spec.outputs` | 该 Workflow 暴露的可选 expression-based outputs。 |
| `spec.jobs` | Reusable jobs。每个 job 当前支持 inline `steps` 或 namespace-local `uses` 二选一。 |

当前 status 字段：

| Field | Description |
| --- | --- |
| `status.conditions` | Definition-level readiness 和 validation conditions。skeleton controller 会记录 `Ready=True`。 |

Workflow execution 已迁移到 `WorkflowRun` API。Namespace-local `uses` resolution、
input binding、output propagation 和 WorkflowRun execution 仍在 roadmap 中跟踪。

### WorkflowRun

`WorkflowRun` 是 reusable workflow model 的 execution-instance API。controller 当前支持
解析 top-level reusable Workflow references 和 inputs，将 inline jobs 执行为顺序 step Runs，
并推导 step 与 job status。Reusable job calls、Action expansion、output propagation、
WorkflowRun terminal aggregation 和 cancellation propagation 仍在 roadmap 中。

当前 spec 形状：

| Field | Description |
| --- | --- |
| `spec.jobs` | Inline jobs。`spec.jobs` 和 `spec.uses` 必须且只能设置一个。 |
| `spec.uses` | 要执行的 namespace-local reusable Workflow 名称。`spec.jobs` 和 `spec.uses` 必须且只能设置一个。 |
| `spec.with` | 传给 `spec.uses` 引用的 reusable Workflow 的 string inputs。 |
| `spec.cancelRequested` | 请求取消 WorkflowRun 的 user intent。该字段已可用，但尚未实现 child Run cancellation propagation。 |

当前 status 字段：

| Field | Description |
| --- | --- |
| `status.phase` | `Pending`、`Running`、`Succeeded`、`Failed` 或 `Cancelled`。尚未实现 WorkflowRun terminal aggregation 和 cancellation handling。 |
| `status.jobs` | 按 job name 记录轻量 resolved job status。每个 job 记录 `pre` 和有序 step statuses。 |
| `status.conditions` | Lifecycle conditions。skeleton controller 会记录 `Accepted=True`。 |

Job phases 包括 `Pending`、`Waiting`、`Running`、`Succeeded`、`Failed` 和 `Skipped`。
当 job 被 failed 或 skipped dependency 阻塞时，controller 会 transitively 将其标记为
`Skipped`，不会为它创建 child Runs，并继续执行 independent jobs。

### Action

`Action` 为目标 WorkflowRun model 定义可复用 step group。它是 definition object，
不是 execution instance。

当前 spec 字段：

| Field | Description |
| --- | --- |
| `spec.inputs` | Action 接受的可选 typed string inputs。 |
| `spec.outputs` | Action 暴露的可选 expression-based outputs。 |
| `spec.steps` | 有序 reusable steps。第一版只支持 `run` steps。 |

当前 status 字段：

| Field | Description |
| --- | --- |
| `status.conditions` | definition-level readiness 和 validation conditions。 |

初始 controller 只记录 definition readiness。Namespace-local `uses` resolution、input
binding、output propagation 和 WorkflowRun execution 仍在 roadmap 中跟踪。

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
