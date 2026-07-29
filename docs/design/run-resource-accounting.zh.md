---
title: "Run 资源记账"
---

# Run 资源记账

状态：**Accepted；Scheduler 实现中**

Runtime capacity 不应只限制并发 Run 数量。`Runtime.spec.capacity.resources`
已经声明每个 Runtime Pod 的命名 capacity，Runtime controller 会将其投影为 Pod annotation；但 scheduler
目前只对内建 `runs` resource 记账。本文定义完整执行所有 capacity 所需的 Run-side request model。

## API

增加 immutable 的 `Run.spec.resources.requests`，类型为 `corev1.ResourceList`。它表示 Run 在整个
active lifetime 内消耗的 logical Runtime resources：从 `Scheduled` 到 terminal completion 或 function
release。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
spec:
  runtime: python
  resources:
    requests:
      runs: "1"
      example.com/gpu: "1"
  mode:
    task:
      args: ["python", "train.py"]
```

`runs` 是内建 logical resource。省略 `resources` 或 `resources.requests.runs` 时默认 request 为 `1`，因此
现有 Run 保持现在的 capacity 行为。其他 resource 可选且必须显式 request。这些是 logical Runtime resources，
并非 container CPU/memory requests；用户仍然通过 Runtime Pod template 独立配置 Kubernetes container
resources。

resource quantity 必须是非负整数。零 request 被忽略。初始 API 有意不定义 `limits`：scheduler 和 runtimed
都会按 request amount 保留 logical capacity。加入 limits 需要先单独 review overcommit、runtime-side
enforcement、sharing ratio 和 runtime-specific resource classes 的模型。只有这些语义确定后，才可以在同一个
resource object 中加入未来的 `limits` field。

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

active assigned Runs 与 scheduler-local assumed assignments 都以完整 resource request 计入 scheduler `usage`。
因此 Reserve/Assume 与 Bind 共享同一记账模型。least-loaded strategy 会对每个已声明 resource 的 projected
allocation 评分：首先最小化最高 utilization ratio，然后最小化所有 utilization ratio 的总和，最后使用 Pod name
作为稳定的 tie break。

如果 Run request 的 resource 没有被 candidate Pod 声明，该 Pod 不可行。没有 ready Pod 满足全部 request 时，Run
保持 `Pending`，并记录有界 insufficient-capacity reason；Runtime Pod capacity 变化或 active/assumed usage
释放时会重新激活。malformed request 属于 invalid Run configuration，应在 Scheduler
prefilter validation 阶段、placement 之前失败。

## Runtime Admission

scheduler 记账不是 execution authorization。runtimed 必须为它所在的 Runtime Pod 维护 watch-backed local
resource usage cache。它在 claim `Scheduled` Run 前，原子地检查 Run 的完整 request 是否适合该 Pod 的 annotation
capacity；只有所有 resource 都适合时才保留 request 并执行 claim。

request 不适合时，runtimed 保持 Run 为 `Scheduled`；它不开始 Runtime Server call、不把 Run 标记为 failed、也不
释放 assignment。local active Run 变为 terminal 或 capacity 变化时，它会重新检查 queued Run。这使 runtimed 能够
在 scheduler cache stale、并发 transition 或旧 scheduler version 创建 assignment 时作为 execution-time guard。
如果某个 Run request 大于所有 eligible Pod，它通常应保持 `Pending`，因为 scheduler 不会分配它。

## 兼容性与边界

- 现有 Run 省略 `resources`；scheduler 将其视为 `{runs: 1}`。
- 现有 Runtime Pod 没有 `runs` annotation 时保留当前默认 `runs` capacity；其他任何 resource 都没有隐式 capacity。
- scheduler 记账是 namespace-local 的：Run assignment 与 Runtime Pod selection 均在 Run namespace 内。
- runtimed 从 Pod-local resource cache 强制执行 local execution admission；它不在 Pods 之间选择，也不做
  placement decision。
- 本文不改变 Kubernetes 对 Runtime Pods 的调度、Runtime Pod template 的 container requests/limits，或 function
  invocation concurrency。

## 实现顺序

1. Review 本 API 及其 Pending/validation semantics。
2. 增加 `Run.spec.resources`、immutability validation、generated CRDs 与 user-facing API docs。
3. 增加解析完整 capacity annotations 的 Runtime Pod helpers。
4. scheduler 累积完整 active/assumed Run resource usage，并按照上述 contract filter candidates。
5. 增加 runtimed 的 atomic local admission/reservation cache。capacity-blocked 的 assigned Run 保持
   `Scheduled`，直到 local reserved resources 被释放。
6. 增加 defaults、multi-resource placement、release/reactivation、missing capacity，以及 stale 或 competing
   assignments 后 runtimed admission 的 unit、integration 与 E2E coverage。
