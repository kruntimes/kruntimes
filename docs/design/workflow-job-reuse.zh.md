# Job-Level Reusable Workflow Execution

状态：**提案，等待 review**

本文细化 [Workflow Reuse](../workflow-reuse/) 中的 job-level `uses` 模型，定义一个在
controller restart 和 reusable `Workflow` 更新后仍保持确定性的 execution boundary。

## 问题

现有设计提出将 job-level reusable Workflow call 展开为 nested job group，但没有定义：

- caller `needs` 如何连接 callee roots 和 leaves；
- nested jobs 如何出现在 `WorkflowRun.status.jobs`；
- nested execution path 如何满足 Kubernetes label 长度限制；
- cancellation、failure、outputs 和 restart recovery 如何跨越 call boundary；
- 已 accepted 的 WorkflowRun 如何避免观察到 referenced `Workflow` 的后续修改。

最后一点已经造成 top-level `WorkflowRun.spec.uses` 的实现缺口：controller 会根据 referenced
definition 初始化 status，但执行仍读取 `WorkflowRun.spec.jobs`；对于该 shape，此字段为空。

## 决策

job-level reusable Workflow call 使用 child `WorkflowRun` 表示。caller job 仍是一个逻辑
dependency 和 output 节点。called Workflow 的 jobs 保留在 child WorkflowRun 的本地状态中，
而不是扁平化到 parent status map。

该方案会为每次 call 增加一个 Kubernetes object 和 controller transition。这些开销不在 Run
调度热路径上，换来明确的 ownership、有界命名、本地 status、递归 cancellation，以及独立的
workspace/artifact boundary。

第一版保持 namespace-local，不增加新的 public invocation CRD。

## 用户模型

API 保持直接：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: make package
    deploy:
      needs: [build]
      uses: deploy-workflow
      with:
        environment: staging
    notify:
      needs: [deploy]
      runs-on: bash
      steps:
        - name: send
          run: send-notification
```

`deploy` 表现为一个 caller job：

- 只有 `build` 成功后才启动；
- child WorkflowRun 内可以并行执行多个 jobs；
- 只有 child WorkflowRun succeeded 时它才 succeeded；
- `notify` 等待完整 child WorkflowRun，而不是某个内部 leaf；
- called Workflow outputs 成为 `deploy` job outputs；
- called jobs 不与 caller jobs 共享 workspace。

该语义保留了
[GitHub Actions reusable-workflow model](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows)
中有价值的部分：reusable Workflow 在 job level 调用，并通过 caller job 使用其 outputs。
kruntimes 的实现仍保持 Kubernetes-native 和 namespace-local。

## Parent 和 Child Status

`JobStatus` 增加可选 `workflowRunName`，与 `StepStatus.runName` 对称：

```yaml
status:
  jobs:
    deploy:
      phase: Running
      pre: [build]
      workflowRunName: release-deploy-7f6d98c04a
```

call job 有 `workflowRunName`，没有 step statuses。child WorkflowRun 包含自己的本地
`status.jobs` map。因此 nested names 不需要编码到 parent map key 或 child Run label 中。

child WorkflowRun 使用 controller owner reference 指向 parent。其名称由 parent UID 和 caller
job name 确定性生成。保留的 labels 和 annotations 会记录 root WorkflowRun UID、snapshot 和
call path，用于 recovery 和诊断。

parent controller watch owned child WorkflowRuns，并按如下规则投影状态：

| Child WorkflowRun | Caller job |
| --- | --- |
| `Pending` 或 `Running` | `Running` |
| `Succeeded` | `Succeeded` |
| `Failed` | `Failed` |
| 非 parent cancellation 导致的 `Cancelled` | `Failed` |

parent cancellation 期间，controller 会请求取消 active child WorkflowRuns 和 direct child
Runs。两类 children 都 settled 后，parent 才进入 `Cancelled`。从未启动的 jobs 保持原有
`Pending` 或 `Waiting` phase。

## Immutable Execution Snapshot

accepted execution 不能再次读取 mutable `Workflow` definitions。在初始化 `status.jobs` 或
创建任何 child 前，controller 会递归解析完整的 namespace-local Workflow call tree，并写入
immutable execution snapshot。

### Snapshot Record 格式

snapshot 在 `ControllerRevision.data.raw` 中使用两种 JSON record。它们是
controller-private record，而不是 CRD；record 显式包含 schema version，因此未来不兼容的格式
变更必须经过 review 的 migration，不能静默地重新解释已经 accepted 的 execution。

```go
type WorkflowExecutionSnapshotIndex struct {
    SchemaVersion string                 `json:"schemaVersion"` // v1alpha1
    Nodes         []WorkflowSnapshotNode `json:"nodes"`
}

type WorkflowSnapshotNode struct {
    CallPath           string            `json:"callPath"`
    DefinitionRevision string            `json:"definitionRevision"`
    With               map[string]string `json:"with,omitempty"`
}

type WorkflowDefinitionSnapshot struct {
    SchemaVersion string                 `json:"schemaVersion"` // v1alpha1
    Source        WorkflowSnapshotSource `json:"source"`
    Inputs        map[string]WorkflowInputSpec
    Outputs       map[string]WorkflowOutputSpec
    Jobs          map[string]JobSpec
}

type WorkflowSnapshotSource struct {
    Kind            string `json:"kind"` // WorkflowRun 或 Workflow
    Name            string `json:"name"`
    UID             string `json:"uid"`
    Generation      int64  `json:"generation"`
    ResourceVersion string `json:"resourceVersion"`
}
```

每个 index 都有一个 `root` node。inline root 使用 `WorkflowRun` definition snapshot；root
`spec.uses` node 和每个 nested call 使用 `Workflow` definition snapshot。job-level call path
在 caller path 后增加 `/jobs/<job-name>`。job name 不允许包含 `/`，因此它在 reconciliation
间没有歧义且保持稳定。`With` 保留在 call node，以便之后在 caller context 中求值；它绝不会被
合并到 callee definition。

definition revision 和 index name 都从 canonical JSON 与 root WorkflowRun UID 进行
content-address。controller 先创建 definition revisions，再创建 index，最后才持久化
`status.snapshotName`。因此 restart 要么找到完整且匹配的 index，要么安全重建缺失的 immutable
records。在接受 WorkflowRun 前，它只删除未被完整 index 引用的 root-owned definition revisions。
resolver 会在创建 index 或 execution child 前拒绝超过 Kubernetes object size limit 的 record。

snapshot 不放在 status 中，而是存储在由 WorkflowRun owner 的 `ControllerRevision`
objects 中。Kubernetes API validation 会使已成功创建 revision 的 `data` 不可变：

- 一个较小的 index ControllerRevision 将稳定 call paths 映射到 definition revisions；
- definition ControllerRevisions 在 JSON `data` fields 中包含 normalized Workflow inputs、
  outputs 和 jobs；
- 每个 entry 记录 source name、UID、generation 和 resource version；
- call nodes 保留尚未求值的 `with` expressions，后续在 caller context 中求值；
- revision data 不包含 runtime results 或 secret material；
- owner references 使其随 root WorkflowRun 被 garbage collection。

Snapshot names 使用确定性的 content-addressed 方式生成。创建过程保持幂等：发生部分失败后，
reconcile 会先校验并复用匹配的 immutable ControllerRevision data，再创建缺失 entries。
Controller 不依赖 mutable revision labels 或 annotations 保证 correctness。WorkflowRun accepted
前会删除未被引用的 partial revisions。

`WorkflowRun.status.snapshotName` 记录 index ControllerRevision 名称。Status 不复制完整 job
specs、scripts 或 environment values。如果 snapshot 无法满足 ControllerRevision object limits，
resolution 会在执行前失败。

一旦 `snapshotName` 持久化，后续 reconcile 只读取 snapshot，不再读取当前 `Workflow`
objects。child WorkflowRun 继承 root snapshot 和 call-path annotation，因此 nested calls 使用
同一份 immutable tree。

root WorkflowRun spec 也是 execution input。创建后，`jobs`、`uses` 和 `with` 不可修改；
`cancelRequested` 只允许从 false 变成 true，不能恢复为 false。这些规则需要 CRD transition
validation。

## Resolution 和 Cycle Detection

snapshot resolution 在创建任何 execution child 前完成：

1. 选择 inline root jobs，或者解析 top-level `spec.uses`。
2. 校验并绑定 literal top-level inputs。
3. 递归解析同 namespace 中的所有 job-level `uses`。
4. 将每个 definition version 和 call path 记录到 snapshot。
5. 拒绝 missing definitions、invalid inputs、unsupported shapes，以及直接或间接 Workflow
   call cycles。
6. 持久化 immutable snapshot ControllerRevisions。
7. 根据 snapshot 初始化轻量 status，并设置 `Accepted=True`。

初始安全限制为最大 call depth 8，以及每个 root execution 最多 64 个 reusable Workflow call
nodes。超过任一限制都会在创建 child 前拒绝 WorkflowRun。即使 graph 无环，这些限制也能防止
意外递归扩张。

## Reconciliation

controller 保持现有 load/calculate/apply/patch 结构。

加载的资源增加：

- root snapshot index 和 definition ControllerRevisions；
- reconciled WorkflowRun owner 的 direct child WorkflowRuns；
- inline jobs 对应的 direct child Runs。

plan 可以产生两类 runnable target：

- inline step target 创建或复用 child Run；
- reusable call target 创建或复用 child WorkflowRun。

一次 reconcile 可以并行启动所有当前 ready targets，但每个 caller job 最多一个 target。创建的
child identities 会在单次 status patch 前记录到 desired status。确定性名称和 owner watches
能够在 restart 后修复 create-before-status-patch failure。

top-level `spec.uses` 不创建 wrapper child WorkflowRun。root WorkflowRun 会根据 immutable
snapshot 初始化并执行 root jobs。只有 job-level calls 创建 child WorkflowRuns。

## Job Shape 和 Outputs

call job 支持 `needs`、`uses` 和 `with`。v0.x 不支持 `runs-on`、`steps` 或 caller-defined
`outputs`。called Workflow 为其内部 jobs 选择 runtimes，并通过 `Workflow.spec.outputs` 暴露
outputs。

input expressions 只在 call job ready 时求值，并使用 caller 已完成 dependency outputs。求值后
的具体值放入 child WorkflowRun 的 immutable `with` map。Secret inputs 继续保持 out of scope，
直到单独的 secret-handling design 完成 review。

output evaluation 仍属于现有 expression/output propagation story。本文只确定 boundary：child
Workflow outputs 会提升为 caller job outputs，downstream jobs 通过普通 jobs context 访问。

## Component Boundaries

- WorkflowRun controller 负责 resolution、snapshots、child WorkflowRuns、child Runs、status
  projection 和 cancellation propagation。
- Workflow 和 Action controllers 继续只校验 definitions。
- Scheduler 和 runtimed 继续只看到独立 Runs，不感知 Workflow calls 和 snapshots。
- PersistentWorkspace 和 ArtifactStore 行为属于 called jobs，不属于 synthetic caller job。

## API 和 RBAC Changes

实现前需要 review 以下 API 和运维变更：

- 增加 `WorkflowRun.status.snapshotName`；
- 增加 `JobStatus.workflowRunName`；
- 增加 WorkflowRun spec transition validation，保证 execution inputs immutable 且 cancellation
  单向变化；
- 拒绝在 `uses` job 中设置 `runs-on`、`steps` 和 caller-defined `outputs`；
- 保留 snapshot/call-path labels 和 annotations；
- 授予 WorkflowRun controller namespace-scoped `apps` ControllerRevision
  get/list/watch/create/delete permissions；
- 除 child Runs 外，watch owned child WorkflowRuns。

## Rejected Alternatives

### 将 callee jobs 扁平化到 parent status

扁平化可以避免 child WorkflowRun objects，但需要 synthetic caller nodes、将 external dependency
edges 重写到 callee roots 和 leaves、把 nested paths 泄漏到 labels，并在一个 status map 中混合
独立的 workspace/artifact boundaries。Nested cancellation 和 output aggregation 也更难解释。

### 每次 reconcile 都读取当前 Workflow

该方案简单，但 reusable definition 被修改时会改变 in-progress execution，无法提供确定性的
retry 或 restart recovery。

### 将 resolved definitions 复制到 WorkflowRun status

这会扩大 status，并可能复制 scripts 和 environment values。Status 应保持轻量 execution state，
而不是 execution-spec store。

## Implementation Plan

1. API prerequisites：status references、transition validation、reserved metadata、generated
   CRDs，以及 ControllerRevision/child-WorkflowRun RBAC。
2. Snapshot storage 和 recursive resolver：version capture、limits、input validation，以及
   cross-Workflow cycle detection。
3. 从 immutable snapshot 执行 top-level `spec.uses`，修复当前只初始化 status 的缺口。
4. 为 ready job-level calls 创建并观察 child WorkflowRuns，包括 dependency 和 terminal
   propagation。
5. 增加 restart、mutation、nested-call、cancellation 和 invalid-graph tests。
6. 在现有 output propagation work 中集成 expression inputs 和 child Workflow outputs。
7. 增加 E2E coverage，然后更新最终 workflow demo。
