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

## Status Model

`WorkflowRun.status` 拥有 execution state：

```yaml
status:
  phase: Running
  jobs:
    build:
      phase: Running
      steps:
        setup:
          phase: Succeeded
          outputs:
            python-version: "3.13"
```

`Workflow.status` 和 `Action.status` 只应包含 definition-level conditions，例如 validation
或 readiness。它们不包含 per-execution job 或 step state。

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
5. 实现 top-level、job 和 step `uses` 的 namespace-local reference resolution。
6. 实现 input binding、expression context 和 output propagation。
7. 更新 CLI verbs 和 docs，使 execution 使用 `WorkflowRun`。
8. 增加 E2E 覆盖 inline `WorkflowRun`、reusable Workflow calls、Action calls、
   validation failures 和 output propagation。
9. reusable model 实现后，更新最终 v0.x demos。

当前实现状态：

- `WorkflowRun`、`Workflow` 和 `Action` API skeletons 已存在。
- `Workflow` 现在是 reusable definition skeleton，不再执行 child Runs。
- 旧 Workflow execution E2E coverage 暂时 skip，等待 WorkflowRun execution 实现后恢复。
