# Workflow Reuse

本文描述 v0.x 的目标设计，当前尚未实现。

目标是在 Workflow API 稳定前拆分 workflow execution instances 和 reusable workflow/step
definitions。当前 experimental `Workflow` CRD 表示 execution instance。这个形态对 CI/CD
和 automation 场景不够用，因为团队需要 reusable workflows 和 reusable step groups。

## 当前状态

当前 experimental Workflow API 提供：

- 一个作为 execution instance 的 `Workflow` 对象；
- inline `jobs`；
- inline step `run` scripts；
- `needs` dependencies；
- 有界 step/job outputs；
- 一个 future-looking `uses` 字段，但目前 validation 会拒绝它。

当前尚不支持：

- reusable workflow definitions；
- reusable action definitions；
- job 调用 workflow；
- step 调用 action；
- definition status 和 run status 的清晰分离。

## 目标

- 将 execution instance API 改名为 `WorkflowRun`。
- 使用 `Workflow` kind 表示 reusable workflow definition。
- 增加 `Action` kind 表示 reusable step group。
- 第一版 references 保持 namespace-local 且简短：`uses: <name>`。
- 使用 `with` 传递 inputs。
- reusable Actions 保持在 caller job context 内。
- reusable Workflow calls 拥有自己的 job/workspace/artifact boundary。
- validation 保持严格，确保每个 object 只有一种清晰 shape。

## 非目标

- 第一版不支持 cross-namespace、remote、Git、OCI 或 marketplace references。
- 不承诺 GitHub Actions 兼容。
- 本设计不包含 matrix strategy。
- 除 v0.x 所需 CRD status shape 外，不设计 UI 或 run history。
- 当前 experimental `Workflow` execution instance API 不要求 backwards-compatible
  migration。

## API Overview

目标拆分如下：

| Kind | 角色 |
| --- | --- |
| `WorkflowRun` | Execution instance。inline 定义 jobs，或调用一个 reusable `Workflow`。 |
| `Workflow` | Reusable workflow definition。可被 `WorkflowRun` 或 job 调用。 |
| `Action` | Reusable step group。可被 `WorkflowRun` 或 `Workflow` 中的 step 调用。 |

## WorkflowRun

`WorkflowRun` 是用户用来启动执行的对象。它支持 inline jobs，或 top-level workflow call。

Inline 形态：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: |
            echo "building"
```

Reusable Workflow call 形态：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  uses: build-and-test
  with:
    image: agent:v0.1.0
```

Validation 必须保证 top-level `uses` 和 inline `jobs` 互斥。

## Reusable Workflow

`Workflow` 变成 reusable definition。它本身不是 execution instance。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: build-and-test
spec:
  inputs:
    image:
      type: string
      required: true
  outputs:
    image:
      value: ${{ jobs.build.outputs.image }}
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: |
            echo "image=${{ inputs.image }}" >> "$KRUNTIME_OUTPUTS"
```

Job 也可以调用 reusable Workflow：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    release:
      uses: build-and-test
      with:
        image: agent:v0.1.0
```

Validation 必须保证 job `uses` 和 job `steps` 互斥。

Reusable Workflow jobs 拥有自己的 job/workspace/artifact boundary。它们通过 inputs、
outputs 和 artifacts 与 caller 通信。

## Action

`Action` 是 reusable step group。

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Action
metadata:
  name: setup-python-tools
spec:
  inputs:
    version:
      type: string
      default: "3.12"
  outputs:
    python-version:
      value: ${{ steps.setup.outputs.python-version }}
  steps:
    - name: setup
      run: |
        echo "python-version=${{ inputs.version }}" >> "$KRUNTIME_OUTPUTS"
        echo "installing toolchain"
```

Step 通过 `uses` 调用 Action：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: build-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: setup
          uses: setup-python-tools
          with:
            version: "3.13"
        - name: package
          run: |
            echo "using ${{ steps.setup.outputs.python-version }}"
```

Validation 必须保证 step `uses` 和 step `run` 互斥。

Actions 在 caller job context 内运行。默认共享 caller job 的 runtime、workspace、artifacts、
environment 和 scheduling placement。这意味着 Actions 是轻量 step composition，而不是
nested workflow execution。

## Inputs and Outputs

第一版应支持简单 typed string inputs：

```yaml
inputs:
  image:
    type: string
    required: true
  version:
    type: string
    default: "3.12"
```

Outputs 应基于 expressions：

```yaml
outputs:
  image:
    value: ${{ jobs.build.outputs.image }}
```

对于 v0.x，validation 应保持窄模型：

- input `type` 从 `string` 开始；
- `required` 和 `default` 有互斥/约束关系；
- `with` values 是 strings；
- 缺少 required inputs 时 validation 或 reconcile 早期失败；
- 未知 input names 时 validation 或 reconcile 早期失败。

WorkflowRun controller 应在展开 child jobs 或 steps 之前完成 input binding：

1. 从 callee 声明的 `inputs` 开始。
2. 应用每个 input 的 `default`。
3. 覆盖 caller 通过 `with` 传入的 values。
4. 拒绝缺少 required inputs 的调用。
5. 拒绝未知的 `with` keys。
6. 在创建 Runs 之前，将轻量 resolved DAG edges 存入 `WorkflowRun.status.jobs`。

已经启动的 WorkflowRun 不应观察到被引用 `Workflow` 或 `Action` 的后续变更。Reusable
definitions 可以随时间变化，但每个 WorkflowRun 一旦 accepted，它的执行必须是确定的。后续
可以增加显式 revisioning；第一版应捕获足够的 resolved data，让 retries 和 controller
restart 后的恢复保持稳定。

Step outputs 来自 child Run 结果。step 将小型 key-value outputs 写入
`KRUNTIME_OUTPUTS`；runtimed 将其持久化到 child Run status。WorkflowRun controller
读取这些 child Run outputs，并提升到匹配的有序
`WorkflowRun.status.jobs.<job>.steps[]` entry。

Job outputs 在 job 内所有 steps 成功后计算。Workflow outputs 在 reusable Workflow 内所有
jobs 成功后计算。当 expression 引用了不存在的 job、step 或 output key 时，output
evaluation 必须让 WorkflowRun 失败。

## Reference Resolution

第一版 references 保持 namespace-local：

```yaml
uses: build-and-test
uses: setup-python-tools
```

在出现具体需求前，不引入 `workflowRef`、`actionRef`、cross-namespace references、remote
URLs、Git refs 或 OCI refs。

这样可以保持 API 小，并避免在 execution model 稳定前过早创建必须长期支持的 reference
format。

Reference resolution 应按以下顺序发生：

1. 将 top-level `WorkflowRun.spec.uses` 解析到同 namespace 下的 `Workflow`。
2. 将被引用 Workflow 的 jobs 展开到 WorkflowRun execution graph。
3. 将每个 job-level `uses` 解析到同 namespace 下的 reusable `Workflow`。
4. 将 reusable Workflow calls 展开为 nested job groups，并保持它们自己的
   job/workspace/artifact boundary。
5. 将每个 step-level `uses` 解析到同 namespace 下的 `Action`。
6. 将 Action steps inline 展开到 caller job context。
7. 在创建任何 child Runs 之前检测 cycles。

Workflow calls 之间的 cycles 必须被拒绝。第一版中 Action 不应再调用另一个 Action，因为
nested Action expansion 目前不需要，而且会增加 cycle detection 的复杂度。Reusable
Workflow 只有在 controller 能证明 call graph 无环时，才可以调用另一个 Workflow。

Resolution failures 应在创建任何 child Runs 前将 WorkflowRun 置为 `Failed`。典型情况包括
missing references、namespace 假设不匹配、unsupported nested Action calls、input binding
失败以及 cycles。

## Execution Graph

WorkflowRun controller 拥有 graph expansion 和 execution state。它不应依赖 scheduler 或
runtimed 理解 Workflow 概念。

第一版应使用简单、确定性的 graph 模型：

- 每个 job 都有稳定的 execution path，例如 `jobs.build`，或从 reusable Workflow call
  展开的 `jobs.release.jobs.build`；
- 每个 step 都有稳定的 execution path，例如 `jobs.build.steps.package`；
- 每个 child Run 都带有 WorkflowRun name、job path 和 step path labels；
- child Run names 要么足够确定以支持 idempotent reconciliation，要么在创建新 Runs 前
  通过 labels 发现已有 Runs；
- controller 只在所有 dependency jobs 成功后创建 Runs；
- terminal child Run phases 会保留在其 owning step 上，再按照下述 terminal semantics
  聚合为 job 和 WorkflowRun 状态。

第一版应支持一种执行策略：

1. Accept WorkflowRun，并设置 `status.phase=Pending`。
2. Resolve references 并 bind inputs。
3. 将 resolved predecessor job edges 持久化到 `status.jobs[*].pre`。
4. 启动所有 runnable steps：每个 dependency-ready job 的第一个 step，以及 running job
   中 successful predecessor 之后的下一个 step。
5. 当 step Run 成功时收集 outputs；下一次 reconciliation 会将其 next step 与其他
   runnable steps 一起处理。
6. 将 observed terminal step states 聚合为 job state：所有 steps succeeded 时 job
   succeeded，任一 step failed 时 job failed。job output evaluation 延后到实现 output
   propagation 时处理。
7. 当所有可执行 jobs 到达 terminal state 后，计算 WorkflowRun outputs，并确定
   WorkflowRun 的 terminal state。

这有意避免增加单独的 WorkflowRunInvocation API。Child Runs 仍然是持久 execution records，
scheduler/runtimed 仍然只操作 Runs。

WorkflowRun controller 的 reconciliation 应保持 load/calculate/apply/patch 结构：加载
WorkflowRun 和所有 child Runs，根据这些 resources 推导 desired status 和 current state，
计算一个 action，执行 action，将执行结果合并进 desired status，并仅在 desired status 与
已持久化 status 存在差异时 patch。status projection 是每次 reconciliation 的默认步骤；
观察 child Runs 以及将 terminal steps 聚合为 job phase 不应成为独立 action。current state
与 action 仍然有意分离。初始的 `Empty` state 对应 `Initialize` action：它负责 validation
controller-level semantics、resolve references 和 inputs、persist execution graph，并设置
`Accepted=True`。初始化失败时设置 `Accepted=False`，且不能创建 child Runs。后续 execution
actions 不得修改 `Accepted`：已经 accepted 的 WorkflowRun 仍然可能在执行时失败。

一次 reconciliation 不能在 status 更新前循环执行多个 external actions。一个
`StartRunnableSteps` action 可以 materialize 所有当前 runnable steps，包括本轮开始时根据
resources 推导状态后刚刚变为 runnable 的 steps。每个 job 最多贡献一个 target，因此 action
不会在同一次 reconciliation 中让同一个 job 执行多个 execution operations。action 失败时，
controller 直接返回 error，不 patch status；action 成功时，将新建 Run 的 identity 和 running
phase 合并进 desired status，再执行一次有条件的 status patch。这样 external operations 保持
显式、幂等且 restart-safe，同时不会为内部 status projection 额外消耗 reconciliation。

## Failure、Cancellation 和 Terminal Semantics

v0.x 的默认行为对齐 GitHub Actions 的 job dependency model：independent jobs 并行运行，
failed 或 skipped prerequisite 会让其 dependents 被跳过。conditional execution、
`continue-on-error` 和 matrix fail-fast 是后续明确的 API 扩展，而不是第一版 controller
隐式行为。

- terminal child Run 会被复制到对应 step，且不得改写其 phase。特别是 `RunTimeout` 保持为
  `RunTimeout`，`Cancelled` 保持为 `Cancelled`。
- 只有 job 的所有 steps 都 succeeded，job 才 succeeded。任一 step failed、cancelled 或
  timed out 都会使 owning job 进入 `Failed`。
- 一个 job 失败后，independent jobs 仍会被创建并允许完成。直接或传递依赖于 failed 或
  skipped job 的 job 会进入 `Skipped`，且永不创建 child Run；其 `pre` edges 和
  predecessor job phases 可以识别 blocker，故它自身不是 `Failed`。
- controller 等待所有 executable jobs terminal 或被 skipped。任一 job `Failed` 时，
  WorkflowRun 为 `Failed`；否则 WorkflowRun 为 `Succeeded`，包括仅因 dependency 而
  skipped 的 jobs。WorkflowRun status 必须保留 job-level reasons，使 aggregate phase
  可解释。
- 取消 WorkflowRun 时，controller 停止创建新的 child Runs，并请求取消所有 non-terminal
  child Runs。待它们 settled 后，WorkflowRun 为 `Cancelled`；不得因为过程中 child 报告
  cancellation 或 timeout 而转成 `Failed`。

### API Prerequisites

现有 WorkflowRun API 还不能完整表达这些语义。在 controller 实现 dependency propagation 或
cancellation 前，API 必须增加以下 fields 和 phases：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
spec:
  cancelRequested: true
status:
  phase: Cancelled
  jobs:
    test:
      phase: Skipped
      pre: [build]
```

- `WorkflowRun.spec.cancelRequested` 是 user intent，对齐 `Run.spec.cancelRequested`。
  controller 一旦观察到它，就不能再为该 WorkflowRun 创建 child Runs。
- `WorkflowPhase` 必须增加 terminal `Cancelled`。
- `JobPhase` 必须增加 terminal `Skipped`。它表示 job 因 predecessor failed 或 skipped
  而未执行。已有的 `pre` edges 加上 predecessor job phases 就可以识别 blocking job，故 v0.x
  不增加冗余的 `blockedBy` status field。
- v0.x 不为 `JobPhase` 增加 `Cancelled`。普通执行中 step cancellation 会使 running job
  进入 `Failed`；整体 WorkflowRun cancellation 由 parent 的 `Cancelled` phase 表达。

Cancellation 是独立的 controller action，优先于正常 execution actions。它会给每个
non-terminal child Run patch `spec.cancelRequested=true`，并等待观察到它们 terminal。之后把
parent WorkflowRun 设为 `Cancelled`。从未启动的 jobs 维持当前 Pending 或 Waiting，因为它们
不是被 DAG dependency skip；parent terminal phase 说明它们不会继续运行。

非 cancellation 场景中，dependency propagation 和 WorkflowRun finalization 是在规划
external actions 前默认执行的 status derivations：

1. 任一 predecessor 为 `Failed` 或 `Skipped` 时，将 Pending 或 Waiting job 标记为
   `Skipped`；independent jobs 仍然可以启动；
2. 所有 executable jobs settled 后，若任一 job 是 `Failed`，WorkflowRun 为 `Failed`；
   否则为 `Succeeded`。

API change 需要重新生成 CRDs，并为 WorkflowRun controller 增加 patch child Runs 的 RBAC。

Inline WorkflowRun execution 应拆成小的、可 review 的步骤落地：

1. 在修改 execution behavior 前，先审计现有 E2E tests。移除或更新仍在测试旧
   Workflow execution model 的失效 case，保证整个迁移过程中 `make e2e` 始终可以通过。
2. 只为 ready inline jobs 创建第一个 child Run，将 child Run name 记录到对应的有序
   step status，并通过 labels 发现已有 child Runs 来保证创建幂等。
3. 在增加更多 execution states 之前，将 WorkflowRun controller 重构为
   load/calculate/apply/patch 形态：加载 WorkflowRun 和相关资源，推导 desired status
   与 current state，计算一个 external action，执行后合并结果，并按需 patch status。
4. Watch 或 reconcile 属于 WorkflowRun 的 child Runs，并将 terminal child Run phase
   复制到对应 step status。
5. 定义并 review failure、cancellation 和 terminal-status semantics：failure 后
   independent jobs 继续；被 dependency 阻塞的 jobs 进入 `Skipped`；WorkflowRun 在
   所有 executable jobs settled 后聚合终态。
6. 当 step 成功且存在 pending 的后续 step 时，在同一 job 内创建 next-step Run。
7. 将 terminal step states 聚合为 terminal job states：所有 steps succeeded 时 job
   succeeded；任何 step failed、cancelled 或 timed out 时 job failed。
8. 增加已 review 的 terminal-status 与 cancellation API prerequisites，重新生成 CRDs，
   并授予 child Run patch RBAC。
9. 当 failed 或 skipped predecessor 阻塞 job 时，将 job 标记为 `Skipped`；当 job
   succeeded 时，解锁所有 `pre` dependencies 已成功的 jobs。
10. 所有 executable jobs settled 后，将非 cancelled WorkflowRun finalize 为 `Succeeded`
    或 `Failed`。
11. 处理 `spec.cancelRequested`：取消 active child Runs，并将 WorkflowRun finalize 为
    `Cancelled`。
12. 增加 restart recovery tests，证明 controller 可以从
   `status.jobs[*].steps[*].runName` 和 child Run labels 继续执行，且不会重复创建 Runs。
13. 只有当 controller 能端到端执行 inline WorkflowRun 后，再增加 E2E coverage。

## Expression Context

对于 v0.x，expressions 应保持足够小，只支持来自已知 context 的 string interpolation：

| Context | 来源 |
| --- | --- |
| `inputs.<name>` | 当前 Workflow、Action 或 WorkflowRun 的 resolved inputs |
| `steps.<step>.outputs.<name>` | 同一 job 内已经完成的前序 steps |
| `jobs.<job>.outputs.<name>` | 同一 graph boundary 内已经完成的 dependency jobs |

Expressions 不应访问 Kubernetes objects、environment variables、secrets、files、
arbitrary functions 或 network resources。Secret handling 在暴露给 Workflow expressions
之前需要单独设计。

Evaluation 必须是确定且无副作用的。Unsupported syntax 或 missing values 应让
WorkflowRun 失败，并提供清晰 condition 和 message。

## Status Model

`WorkflowRun.status` 拥有 execution state：

```yaml
status:
  phase: Running
  jobs:
    build:
      phase: Running
      pre: []
      steps:
        - name: package
          phase: Succeeded
          outputs:
            image: agent:v0.1.0
    test:
      phase: Waiting
      pre:
        - build
      steps:
        - name: unit
          phase: Pending
```

`Workflow.status` 和 `Action.status` 只应包含 definition-level conditions，例如 validation
或 readiness。它们不包含 per-execution job 或 step state。

第一版实现只会为 inline `WorkflowRun.spec.jobs` 存储轻量 DAG edges 和有序 step status。
它不会把完整 job specs、step commands、environment 或 source data 存入 status。

## 组件边界

| 组件 | 责任 |
| --- | --- |
| WorkflowRun controller | 展开 inline jobs，解析 reusable Workflow 和 Action references，创建 child Runs，并更新 execution status。 |
| Workflow controller | 校验 reusable Workflow definitions，并暴露 definition conditions。 |
| Action controller | 校验 reusable Action definitions，并暴露 definition conditions。 |
| Scheduler | 只调度 child Runs。不理解 Workflow reuse。 |
| runtimed | 只执行 child Runs。不理解 Workflow reuse。 |

## Breaking Change

这是相对于当前 experimental Workflow model 的 breaking API change：

- 当前 `Workflow` execution instances 变为 `WorkflowRun`；
- `Workflow` 只表示 reusable definition；
- 因为 Workflow 仍是 experimental，且不属于 stable API promise，不需要 compatibility shim。

实现落地时必须同时更新 docs、examples、CLI verbs、CRDs 和 E2E tests。

## 实现顺序

1. 增加本文档并 review API shape。
2. 增加 `WorkflowRun` API types、CRD validation、status 和 controller skeleton。
3. 将 `Workflow` API types 改为 reusable definitions。
4. 增加 `Action` API types、CRD validation、status 和 controller skeleton。
   Namespace-local resolution、input binding、output propagation 和 WorkflowRun
   execution 是后续独立实现步骤。
5. 实现轻量 DAG edge snapshotting 和 top-level `WorkflowRun.spec.uses` 的
   namespace-local resolution。
6. 实现 top-level reusable Workflow calls 的 input binding。
7. 实现 ready jobs 的 inline WorkflowRun first-step Run creation。
8. 将 WorkflowRun controller reconciliation 重构为 load/calculate/apply/patch 结构：
   默认推导 status，并将 external side effects 建模为 actions。
9. 实现 child Run status observation 和 step status updates。
10. 定义并 review child failure、cancellation、dependency propagation 和 WorkflowRun
    terminal-status semantics：independent jobs 继续，blocked dependents 为 `Skipped`，
    并在所有 executable jobs settled 后聚合终态。
11. 实现 observed step success 后的 next-step creation。
12. 根据 observed step states 实现 job terminal-state aggregation。
13. 增加 terminal-status 和 cancellation API prerequisites、重新生成 CRDs，以及 child Run
    patch RBAC。
14. 实现到 `JobSkipped` 的 failed-dependency propagation。
15. 实现 WorkflowRun terminal aggregation。
16. 实现 WorkflowRun cancellation propagation。
17. 实现 in-progress inline WorkflowRuns 的 controller restart recovery。
18. 实现 job-level reusable Workflow calls。
19. 实现 step-level Action expansion。
20. 实现 expression evaluation 和 output propagation。
21. 更新 CLI verbs 和 docs，使 execution 使用 `WorkflowRun`。
22. 增加 E2E 覆盖 inline `WorkflowRun`、reusable Workflow calls、Action calls、
   validation failures、output propagation，以及从 status DAG edges 进行 controller
   restart recovery。
20. reusable model 实现后，更新最终 v0.x demos。

当前实现状态：

- `WorkflowRun`、`Workflow` 和 `Action` API skeletons 已存在。
- `Workflow` 现在是 reusable definition skeleton，不再执行 child Runs。
- Inline WorkflowRuns 会初始化 `status.jobs[*].pre` 和有序
  `status.jobs[*].steps`。
- Top-level `WorkflowRun.spec.uses` 会解析同 namespace 下的 reusable
  Workflow，并基于被引用 Workflow 的 jobs 初始化 `status.jobs`。Missing
  references 会在创建 child Runs 前让 WorkflowRun 失败。
- Top-level reusable Workflow calls 会提前绑定 string inputs：应用 defaults，
  missing required inputs 会失败，unknown `with` keys 也会失败。Bound values 会等到
  WorkflowRun execution 实现后再用于 child Runs。
- 旧 Workflow execution model 的 stale E2E stubs 已删除，保证迁移期间 E2E 聚焦于
  仍应保持 passing 的行为。
- Inline WorkflowRuns 会为 ready inline jobs 创建 first-step child Runs，并将 child
  Run name 记录到有序 step status 中。Child Run result observation 和 next-step
  creation 仍是后续工作。
- WorkflowRuns 会观察 terminal child Run phases，并复制到匹配的 step status。
  Next-step creation 和 job/WorkflowRun terminal handling 仍是后续工作。
- 旧 Workflow execution E2E coverage 暂时 skip，等待 WorkflowRun execution 实现后恢复。
