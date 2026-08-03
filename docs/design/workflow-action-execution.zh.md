---
title: "Action Execution"
---

# Action Execution

状态：**Proposed**

本文定义 v0.x Workflow reuse 的 step-level reusable Action execution。它扩展现有的
inline WorkflowRun execution 和 job-level reusable Workflow call model，但不会引入新的
execution object。

## 决策

Action call 是调用方的一个逻辑 step：

```yaml
steps:
  - name: setup
    uses: setup-python-tools
    with:
      version: "3.13"
```

Action 的 `spec.steps` 是 caller job 内按顺序执行的普通 child Runs。它们继承 caller job 的
runtime、workspace configuration、environment、artifact configuration 和 placement constraints。
Action 不创建 child WorkflowRun，也不会向 scheduler 或 runtimed 引入新的概念。

逻辑 `setup` step 通过 `steps.setup.outputs.<name>` 暴露 Action 声明的 outputs。其内部 steps
以有序的 Action-step status 形式保持可见，因此 failure 和 restart recovery 都是 durable 的，
同时不会把 Action internals 伪装成用户定义的 top-level job steps。

## API 和 Status Shape

`StepSpec` 只能有一种 execution form：

- inline `run` step；或
- 通过 namespace-local `uses` 调用 Action，并可选传入 string `with`。

没有 `uses` 时 `with` 非法。`args` 和 `env` 只允许用于 inline `run` step；Action call
只能通过 `with` 传递参数。Action definition steps 只支持 inline `run`；v0.x 不支持
Action nesting。

`StepStatus` 增加 optional ordered `actionSteps` list。inline step 继续使用既有 `runName` 和
`outputs` 字段。Action-call step 没有 `runName`；其 `actionSteps` 保存实际 Run identities 和
terminal state。所有 Action steps 成功且 Action 声明 outputs 计算成功后，outer step 才会变为
`Succeeded`。任何 Action step failure 或 output-contract evaluation failure 都会使其变为
`Failed`。

```yaml
status:
  jobs:
    build:
      steps:
        - name: setup
          phase: Succeeded
          outputs:
            python-version: "3.13"
          actionSteps:
            - name: install
              phase: Succeeded
              runName: build-setup-install
            - name: verify
              phase: Succeeded
              runName: build-setup-verify
```

`actionSteps` 只属于 status。Action source 和展开后的 step specification 必须保存在
WorkflowRun 的 immutable ControllerRevision 中，不能写入 status。

## Resolution 和 Snapshot

WorkflowRun controller 在初始化 WorkflowRun 时解析 Action。它加载 inline job 或 rendered
reusable Workflow 引用的每个 namespace-local Action，并将 Action name 和完整 `ActionSpec`
冻结到该 WorkflowRun 的 private execution snapshot。这等同于 reusable Workflow call 使用的 child
WorkflowRun snapshot：Action 会展开为多个 Runs，但没有自己的 child WorkflowRun。WorkflowRun 被接受后
对 Action 的更新不影响该次 execution。

snapshot 保留 Action input expressions，而不是过早计算。逻辑 call step 变成 next runnable step 时，
controller：

1. 使用当前可用的 `inputs`、已完成的前序 `steps` 和已完成 dependency `jobs` outputs 计算 call 的
   `with` values；
2. 应用 Action input defaults，拒绝 unknown inputs 和 missing required inputs；
3. 在冻结的 Action step specs 和 output contract 中替换 `inputs.<name>`；
4. 创建第一个尚未创建的 Action child Run。

这保留了对 prior outputs 的 late binding，同时冻结 Action definition 本身。restart 会从同一个
snapshot 重新加载逻辑 workflow spec 和冻结的 Action definitions。

## Run Identity 和 Ordering

每个 Action child Run 仍由 WorkflowRun 直接 owner，并使用既有 WorkflowRun/job labels。它还带有一个
有界 controller-owned step identity，包含 logical caller step 和 Action-local step。Run names 从这些
identities 确定性生成。controller 在创建前通过 labels 发现 existing child Runs。

每次 reconciliation 中，每个 job 最多启动一个 next target：

- inline step 创建其一个 child Run；
- Action call 创建第一个 incomplete Action child Run；
- Action child Run 成功后，下一次 reconciliation 创建下一个 Action child Run；
- 最后一个 Action child Run 成功后，controller 计算 Action outputs，并允许后续 caller step 启动。

与当前 inline job execution 一样，independent jobs 在同一次 reconciliation 仍然可以运行。

## Expressions 和 Outputs

v0.x 只支持 string interpolation：

| Context | 可用时机 |
| --- | --- |
| `inputs.<name>` | 当前 reusable Workflow 或 Action 的 resolved inputs |
| `steps.<step>.outputs.<name>` | 同一 job 内已经成功的前序 logical step |
| `jobs.<job>.outputs.<name>` | 同一 WorkflowRun 内已经成功的 dependency job |

只有在具备所需值的 boundary 才计算 expression。unknown name、unavailable output、malformed path，
或对 later step 的引用都会成为受影响 logical step 的确定性 validation failure。该 target 不会创建
child Run，随后 parent job 按现有 failure 和 dependency-propagation semantics 处理。

Action outputs 根据 Action frozen `spec.outputs`，从成功的 Action-step outputs 计算。结果只写入逻辑
call step 的 bounded `outputs` map。后续 caller steps 通过 `steps.<call-name>.outputs.<name>` 消费它们，
不能直接访问 Action internals。

## Failure、Cancellation 和 Limits

- missing Action、nested Action step、invalid call input 或 invalid Action output expression 会在创建受影响
  child Run 前使逻辑 Action step failed。
- failed、cancelled 或 timed-out 的 Action child Run 会使逻辑 Action step failed，从而使其 job failed。
- WorkflowRun cancellation 通过现有 direct-child mechanism 请求取消所有 non-terminal Action child Runs。
- Actions 保持 namespace-local。cross-namespace、remote 和 recursive Action references 不在范围内。
- 继续使用既有有界限制：最多 64 jobs、每个 job 128 caller steps、每个 call 128 Action steps、64
  input/output entries，以及有界 status 和 ControllerRevision data。

## Component Boundaries

WorkflowRun controller 负责 resolve、snapshot、execute 和 aggregate Actions。Action controller 继续只
负责 definition readiness。Scheduler 和 runtimed 只收到普通 Run objects，不理解 Actions。

## Implementation Sequence

1. 增加 API validation、status shape、snapshot format 和 unit coverage。
2. 增加 expression-context evaluation 和 Action input binding。
3. materialize Action child Runs、status aggregation、output projection 和 restart recovery。
4. 增加 success、failures、outputs 和 cancellation 的 integration/E2E coverage。
