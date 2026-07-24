---
title: "Run 资源记账"
---

# Run 资源记账

状态：**Proposal；实现前需要 API review**

Runtime capacity 不应只限制并发 Run 数量。`Runtime.spec.capacity.resources`
已经声明每个 Runtime Pod 的命名 capacity，Runtime controller 会将其投影为 Pod annotation；但 scheduler
目前只对内建 `runs` resource 记账。本文定义完整执行所有 capacity 所需的 Run-side request model。

## API

增加 immutable 的 `Run.spec.resources`，类型为 `corev1.ResourceList`。它表示 Run 在整个 active lifetime
内消耗的 logical Runtime resources：从 `Scheduled` 到 terminal completion 或 function release。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
spec:
  runtime: python
  resources:
    runs: "1"
    example.com/gpu: "1"
  mode:
    task:
      args: ["python", "train.py"]
```

`runs` 是内建 logical resource。省略时默认 request 为 `1`，因此现有 Run 保持现在的 capacity 行为。其他
resource 可选且必须显式 request。这些是 scheduler resources，并非 container CPU/memory requests；用户仍然
通过 Runtime Pod template 独立配置 Kubernetes container resources。

resource quantity 必须是非负整数。零 request 被忽略。初始 API 不定义 limits、overcommit、sharing ratio 或
runtime-specific resource classes。

## Capacity Contract

`Runtime.spec.capacity.resources` 仍然声明每个 Pod 的 logical capacity。Runtime controller 把每个值复制到
对应 Runtime Pod annotation：

```yaml
kruntimes.io/capacity.runs: "4"
kruntimes.io/capacity.example.com/gpu: "1"
```

对每个 candidate Pod，scheduler 读取完整 annotation 集；只要任一 request 超过 capacity，就过滤该 Pod：

```text
usage[pod][resource] + request[run][resource] <= capacity[pod][resource]
```

active assigned Runs 与 scheduler-local assumed assignments 都以完整 resource request 计入 `usage`。因此
Reserve/Assume 与 Bind 共享同一记账模型。least-loaded strategy 初期可以继续只按 `runs` score；multi-resource
scoring 属于单独的 policy design。

如果 Run request 的 resource 没有被 candidate Pod 声明，该 Pod 不可行。没有 ready Pod 满足全部 request 时，Run
保持 `Pending`，并记录有界 insufficient-capacity reason；Runtime Pod capacity 变化或 active/assumed usage
释放时会重新激活。malformed request 属于 invalid Run configuration，应在调度前 validation 失败。

## 兼容性与边界

- 现有 Run 省略 `resources`；scheduler 将其视为 `{runs: 1}`。
- 现有 Runtime Pod 没有 `runs` annotation 时保留当前默认 `runs` capacity；其他任何 resource 都没有隐式 capacity。
- scheduler 记账是 namespace-local 的：Run assignment 与 Runtime Pod selection 均在 Run namespace 内。
- runtimed 负责 execution，不做 placement decision；此阶段无需理解 logical resource name。
- 本文不改变 Kubernetes 对 Runtime Pods 的调度、Runtime Pod template 的 container requests/limits，或 function
  invocation concurrency。

## 实现顺序

1. Review 本 API 及其 Pending/validation semantics。
2. 增加 `Run.spec.resources`、immutability validation、generated CRDs 与 user-facing API docs。
3. 增加解析完整 capacity annotations 的 Runtime Pod helpers。
4. scheduler 累积完整 active/assumed Run resource usage，并按照上述 contract filter candidates。
5. 增加 defaults、multi-resource placement、release/reactivation 和 missing capacity 的 unit、integration 与 E2E
   coverage。
