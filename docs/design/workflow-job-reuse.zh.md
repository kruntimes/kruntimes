# Job 级可复用 Workflow 执行

状态：**待评审**

本文定义 v0.x 中 job 级可复用 Workflow 的执行边界。

## 决策

`WorkflowRun` 只表示带 inline jobs 的一次执行，不在 root 引用可复用 `Workflow`。`Workflow` 是模板：

- `krt workflow trigger <name>` 读取模板、校验并渲染 inputs，然后创建带 inline jobs 的 `WorkflowRun`；
- job 的 `uses` 表示一次可复用 Workflow 调用；
- 该 job ready 后，parent 创建一个带 inline jobs 的 child `WorkflowRun`；
- 每个 WorkflowRun 都拥有自己的 immutable execution snapshot，只协调直接 jobs 和直接 child WorkflowRuns。

这使嵌套复用天然递归，而不要求一个 controller 携带 root 范围的执行树。parent 将每次调用视为一个 job；child 拥有该调用展开的全部 jobs。

## 执行拓扑

本模型的 ownership 很直接：

```text
WorkflowRun release
  直接 job: build
  直接调用 job: deploy
    WorkflowRun release-call-deploy
      直接 job: apply
      直接调用 job: verify
        WorkflowRun release-call-deploy-call-verify
          直接 job: smoke
```

每个 controller 只创建和观察自己所 reconcile 的 WorkflowRun 直接拥有的对象。parent/child 状态传播、取消、artifacts 以及未来 PersistentWorkspace 的边界因此都是局部的。

## API 形式

`WorkflowRun.spec.jobs` 必填。删除 `WorkflowRun.spec.uses` 和 `WorkflowRun.spec.with`。直接使用 `kubectl create` 时必须提供 inline jobs。

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
        environment: ${{ jobs.build.outputs.environment }}
```

复用模板的标准触发方式为：

```text
krt workflow trigger deploy-workflow --input environment=staging
  -> 校验模板 inputs
  -> 将 inputs 渲染到 inline jobs
  -> 创建 WorkflowRun
```

最终 WorkflowRun 保存的是渲染后的 jobs，而不是模板引用，因此模板后续修改不会改变已创建的 root execution。

## 调用可复用 Workflow

`deploy` ready 后，parent controller：

1. 从同一 namespace 读取 `deploy-workflow`。
2. 使用 caller context 渲染 `with`，并校验 callee inputs。
3. 将 `inputs.*` 渲染到 callee jobs。
4. 用这些 inline jobs 创建直接 child WorkflowRun。
5. 设置 owner reference，并将 child 名称写入 `parent.status.jobs.deploy.workflowRunName`。

child 正常执行，也可以为其 `uses` jobs 创建自己的直接 child WorkflowRuns。parent 不拥有也不检查 grandchildren。

调用是**延迟绑定**：被引用的 Workflow 在 caller job ready 时读取。模板修改会影响尚未创建的 child；child 创建后，其渲染 jobs 和 snapshot 都是 immutable。未来若需要更早绑定，应引入显式 template versioning。

## 局部 Snapshot 与 Output Contract

每个 WorkflowRun 自己拥有一个 `ControllerRevision`，名称由自身 UID 确定，并记录在 `status.snapshotName`。它只包含：

- `spec`：接受的 inline `WorkflowRun.spec`，包括本地 jobs topology；
- `outputContract`：仅当该 WorkflowRun 从可复用 Workflow materialize 出来时，保存一个以 source Workflow 名称为 key 的单项 map；其 value 是该 Workflow 声明的 `spec.outputs`。

```yaml
apiVersion: apps/v1
kind: ControllerRevision
metadata:
  name: release-call-deploy-snapshot-8d91c3f4
  ownerReferences:
    - apiVersion: kruntimes.io/v1alpha1
      kind: WorkflowRun
      name: release-call-deploy
data:
  spec:
    jobs:
      apply:
        runs-on: bash
        steps: [{ name: deploy, run: deploy --environment=staging }]
  outputContract:
    deploy-workflow:
      outputs:
        endpoint:
          value: ${{ jobs.apply.outputs.endpoint }}
```

output contract 是 child 创建后保留的唯一 source-template 数据。parent 必须使用与实际执行 jobs 配套的 output 定义；如果 child 完成后读取可变的当前 Workflow，模板变更会让同一执行产生不同的 parent output。

一个 snapshot 只由自己的 WorkflowRun 拥有和使用。

## 调用 Provenance 与 Cycle Detection

Workflow reuse 不能创建 `A -> B -> A` 这样的无界调用链。controller 必须在创建 child
WorkflowRun **之前**检测 cycle，不能依赖之后的 cleanup 来停止递归创建。

`krt wf trigger` 和 WorkflowRun controller 仅在创建 materialized WorkflowRun 时使用保留的
`kruntimes.io/workflow-source` annotation。该 WorkflowRun 第一次 reconcile 时，controller
校验 namespace-local Workflow name，并把它冻结到 local snapshot：

```yaml
data:
  spec: { ... }
  source:
    workflow: build-and-test
```

该 annotation 不是 execution input，也不是面向用户的 `WorkflowRun.spec` field。直接创建的
inline WorkflowRun 没有 source。snapshot 创建后，planning 只读取 snapshot，不读取可变的
annotation 或当前 Workflow definition。

创建 `uses: <target>` child 前，controller 沿当前 WorkflowRun 的 owner chain 向上读取每个
ancestor snapshot 中冻结的 `source.workflow`，并将 target 加到有序 ancestry：

- target 已出现时，call job 变为 `Failed`，并使用确定性 message，例如
  `workflow call cycle: A -> B -> A`；不创建 child；
- 加入 target 后的 template depth 超过 8 时，call job 变为 `Failed` 并记录 depth-limit
  message；不创建 child；
- parent provenance 缺失或无效是确定性的 call-resolution failure，不是可 retry 的
  controller error。

之后正常的 failed-dependency propagation 会把 dependent jobs 标为 `Skipped`，普通的
WorkflowRun terminal aggregation 决定 parent phase。整个机制局限在每个 WorkflowRun：不修改
scheduler 或 runtimed，也不引入 root-wide execution-tree object。

## Inputs 与 Outputs

`JobStatus` 增加有界的 `outputs` map。inline job 和可复用 Workflow 调用的输出都在同一位置暴露：

```yaml
status:
  jobs:
    deploy:
      phase: Succeeded
      workflowRunName: release-call-deploy
      outputs:
        endpoint: https://staging.example.com
```

inline job 的所有 steps 成功后，controller 使用 `JobSpec.outputs` 和 Run status 中的 step outputs 计算 job output。可复用调用的 child WorkflowRun 成功后，parent 读取 child 的局部 snapshot，用冻结的 `outputContract` 对 `child.status.jobs` 求值，并将结果写到 caller job 的 `JobStatus.outputs`。

下游渲染统一使用：

```yaml
${{ jobs.deploy.outputs.endpoint }}
```

只有显式声明、有界、结构化的键值 outputs 可以进入 status。日志和大文件不进入 status，继续使用 logging 和 artifact 机制。缺失引用或 output 求值失败会在启动下一个 dependent target 前使受影响 job 失败。

## 状态、失败与取消

可复用调用 job 有 `workflowRunName`，没有 step statuses。parent 如下投影其直接 child 的状态：

| Child WorkflowRun | Caller job |
| --- | --- |
| `Pending` 或 `Running` | `Running` |
| `Succeeded` 且 output 求值成功 | `Succeeded` |
| `Succeeded` 但 output 求值失败 | `Failed` |
| `Failed` | `Failed` |
| parent 未取消时的 `Cancelled` | `Failed` |

取消时，每个 WorkflowRun 只请求取消直接 child Runs 和直接 child WorkflowRuns。parent 仅在这些直接 children 都结束后变为 terminal；递归取消通过 owner watches 和每个 child 的自身 reconcile 自然完成。

## Controller 职责

每次 reconcile 中，WorkflowRun controller：

1. 加载 WorkflowRun、其局部 snapshot、直接 child Runs 和直接 child WorkflowRuns。
2. 从这些资源推导本地 job 和 WorkflowRun status。
3. 根据 snapshot spec 与已完成 dependency outputs 计算可运行的本地 targets。
4. 创建所有相互独立的 ready targets：inline step 创建 Run，`uses` job 创建 child WorkflowRun。
5. 仅当推导状态变化时 patch status。

Scheduler 和 runtimed 仍然只处理独立 `Run`，不了解 Workflow reuse、snapshot 或 output contract。

## 校验与限制

- `WorkflowRun.spec.jobs` 非空，创建后 immutable。
- WorkflowRun 自身不能包含 `uses` 或 `with`。
- 调用 job 包含 `needs`、`uses` 和可选 `with`，不能包含 `runs-on` 或 `steps`。
- 创建 child 前校验 inputs 和 expression references。
- 在创建 child 前，根据沿 parent/child owner chain 的冻结 call provenance 检测 Workflow cycle；
  初始最大嵌套深度为 8。
- job 与 step outputs 受 CRD 大小限制；artifacts 不是 outputs。

## 可复用 Actions

本文不定义 Action 的执行机制，但确立一条原则：复用在直接 execution boundary 展开。未来 Action 将在 caller step/Run 中解析，而不加入 root 范围的 Workflow snapshot 或 controller traversal tree。

## 实现计划

1. 增加 local WorkflowRun snapshot envelope 与 `JobStatus.outputs`。
2. 实现 `krt workflow trigger` 的模板 input 校验、渲染和 inline WorkflowRun 创建。
3. 实现直接 child WorkflowRun 创建、input rendering 和冻结 output contracts。
4. 实现局部 job-output 求值、child-output projection、restart recovery 和模板变更语义测试。
5. 添加 nested calls E2E coverage，包括 self-reference 和 `A -> B -> A` cycle rejection、
   output propagation、cancellation，以及 child 创建前后模板更新。
6. 单独设计 Action expansion，并沿用相同的直接边界原则。
