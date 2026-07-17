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

### 嵌套 Reuse

reusable Workflow 可以继续调用其他 reusable Workflow。例如，`deploy-workflow` 在完成部署后可以
调用 `smoke-test`：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: deploy-workflow
spec:
  inputs:
    environment: { type: string, required: true }
  jobs:
    apply:
      runs-on: bash
      steps:
        - name: deploy
          run: deploy
    verify:
      needs: [apply]
      uses: smoke-test
      with:
        endpoint: https://staging.example.com
---
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: smoke-test
spec:
  inputs:
    endpoint: { type: string, required: true }
  jobs:
    smoke:
      runs-on: bash
      steps:
        - name: check
          run: check-service
```

accepted 的 `release` execution 会在启动工作前解析整个调用树：

```text
root                         WorkflowRun release
root/jobs/deploy             Workflow deploy-workflow
root/jobs/deploy/jobs/verify Workflow smoke-test
```

root controller 只创建并观察 `deploy` 对应的 child WorkflowRun。该 child 执行 `apply` 后，会创建并
观察 `verify` 对应的自己的 child WorkflowRun。每个 WorkflowRun 都有本地 jobs status map；parent
call job 只有在其 direct child WorkflowRun succeeded 后才会 succeeded。因此，嵌套的
`smoke-test` WorkflowRun settled 前，`release.status.jobs.deploy` 不会 succeeded。

cancellation 和 failure 也遵循相同的 direct-parent boundary。取消 `release` 会请求取消它的
`deploy` child；该 controller 随后请求取消 `verify`。反过来，`smoke-test` failed 会先让 `verify`
failed，再让 `release` 中的 `deploy` call failed。controller 在正常 reconciliation 中不需要遍历任意
depth 的 descendants。

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

### Snapshot Storage

每个 root WorkflowRun 恰好 owner 一个 snapshot `ControllerRevision`。
`ControllerRevision.data` 是对直接序列化的 public specs 的一个很小的 envelope，不引入第二套
Workflow model：

- `root.spec` 始终是 accepted 时完整的 `WorkflowRun.spec`；
- root 使用 reusable Workflow 时，`root.workflow` 保存该引用解析到的完整 `Workflow.spec`；
  inline root 则省略它；
- `workflows[call-path]` 是每个 job-level call 解析到的完整 `Workflow.spec`。

root revision name 保存在 `WorkflowRun.status.snapshotName`。nested child 通过保留 metadata 接收
该名称及自身的 call path，并从同一个 snapshot 读取 definition。`with` 保留在存储的 caller
`JobSpec` 中，和用户提交的内容完全一致；不会复制到 controller-specific record。

以上面的 `release` 为例，下面展示 snapshot ControllerRevision 的完整结构（内容经过必要简化）。
其名称仅用于示例：controller 会从 root WorkflowRun UID 确定性生成名称。

```yaml
# root WorkflowRun owns 唯一的 snapshot revision。
apiVersion: apps/v1
kind: ControllerRevision
metadata:
  name: release-snapshot-root-8d91c3f4
  namespace: default
  labels:
    kruntimes.io/root-workflowrun-uid: 7e4d41cb-69c8-4fa1-8e31-f9135512c22b
  ownerReferences:
    - apiVersion: kruntimes.io/v1alpha1
      kind: WorkflowRun
      name: release
      uid: 7e4d41cb-69c8-4fa1-8e31-f9135512c22b
      controller: true
      blockOwnerDeletion: true
revision: 1
data:
  root:
    spec: # accepted 时完整的 WorkflowRun.spec
      jobs:
        build: { runs-on: bash, steps: [{ name: package, run: make package }] }
        deploy: { needs: [build], uses: deploy-workflow, with: { environment: staging } }
        notify: { needs: [deploy], runs-on: bash, steps: [{ name: send, run: send-notification }] }
  workflows:
    root/jobs/deploy: # 此 call 解析到的完整 Workflow.spec
      inputs:
        environment: { type: string, required: true }
      jobs:
        apply: { runs-on: bash, steps: [{ name: deploy, run: deploy }] }
        verify: { needs: [apply], uses: smoke-test, with: { endpoint: https://staging.example.com } }
    root/jobs/deploy/jobs/verify: # nested call 解析到的完整 Workflow.spec
      inputs:
        endpoint: { type: string, required: true }
      jobs:
        smoke: { runs-on: bash, steps: [{ name: check, run: check-service }] }
```

root snapshot 始终保存 `WorkflowRun.spec`。root 使用 `spec.uses` 时，还会在 `root.workflow`
保存解析后的 `Workflow.spec`。它还保存 `root/jobs/deploy` 对应的解析后 `Workflow.spec`，以及
`root/jobs/deploy/jobs/verify` 对应的 nested `Workflow.spec`。当 `deploy` runnable 时，controller
在 caller context 中求值已存储的 `with.environment`，并使用相同的 snapshot name 与
`root/jobs/deploy` call path 创建 child WorkflowRun。该 child 后续会从同一份 snapshot 解析
`verify`。两个 controller 都不会再次读取当前的 `deploy-workflow` 或 `smoke-test` object。

job-level call path 在 caller path 后增加 `/jobs/<job-name>`。job name 不允许包含 `/`，因此它在
reconciliation 间没有歧义且保持稳定。该 path 是 `ControllerRevision.data` 中的 YAML/JSON map key，
`/` 在这里合法；它不是 Kubernetes label、annotation 或 object name。JSON Pointer 会将 `/` 转义为
`~1`，但 snapshot data 不可变，controller 不会按 path patch 它。resolver 会在创建 execution child 前
拒绝超过 Kubernetes object size limit 的存储 spec。

snapshot 不放在 status 中，而是存储在由 WorkflowRun owner 的一个 `ControllerRevision` 中。
Kubernetes API validation 会使已成功创建 revision 的 `data` 不可变。revision metadata 记录 root
WorkflowRun UID，用于 lookup 和 garbage collection；child WorkflowRun metadata 记录自身的 call
path。revision data 不包含 runtime results 或 secret material。

Snapshot name 由 root UID 确定性生成。创建过程保持幂等：发生部分失败后，reconcile 会先校验并
复用匹配的 immutable revision，再接受 WorkflowRun。

`WorkflowRun.status.snapshotName` 记录 root ControllerRevision 名称。Status 不复制完整 job specs、
scripts 或 environment values。如果 snapshot 序列化后超过 1 MiB，resolution 会在执行前失败。
这个显式限制为 etcd 常见的 1.5 MiB request limit 留出余量；cluster operator 可以配置不同的 API
server 或 etcd limits。

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
4. 将完整的解析树存储到一个 immutable snapshot revision。
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

- root snapshot ControllerRevision；
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
