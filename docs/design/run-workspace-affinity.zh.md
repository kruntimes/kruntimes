# Run Workspace 引用与 Affinity

状态：**已评审**

本文固定 Workflow data sharing 下一项前置能力的 API shape：通用的 Run workspace
引用，以及 Run-to-Run affinity。本文不实现 workspace binding、scheduler placement 或
runtimed 文件准备；它们将分别通过可 review 的实现 PR 完成。

## 范围

`PersistentWorkspace` 是带有 Runtime binding 与生命周期的 namespace-scoped 资源。Run 需要
一个小型 typed reference 才能使用它。后续 Run 还需要一种熟悉的方式，要求或偏好与另一 Run
位于同一个 Runtime Pod，而不是暴露不透明、scheduler-specific 的 sticky key。

此 API 也必须能在 Workflow 外使用。用户可以直接创建 `PersistentWorkspace` 和一个或多个
Run；Workflow controller 只是未来的一个 consumer。

## Run Workspace Reference

`Run.spec.workspace` 是可选的 typed local reference：

```go
type RunWorkspaceReference struct {
    Name     string `json:"name"`
    Kind     string `json:"kind,omitempty"`
    APIGroup string `json:"apiGroup,omitempty"`
}

type RunSpec struct {
    // Existing fields omitted.
    Workspace *RunWorkspaceReference `json:"workspace,omitempty"`
}
```

第一版只接受以下窄范围值：

| 字段 | 必填 | 默认值 | 第一版接受的值 |
| --- | --- | --- | --- |
| `name` | 是 | 无 | Run 所在 namespace 内的 DNS-1123 subdomain name。 |
| `kind` | 否 | `PersistentWorkspace` | `PersistentWorkspace`。 |
| `apiGroup` | 否 | `kruntimes.io/v1alpha1` | `kruntimes.io/v1alpha1`。 |

`apiGroup` 保留实验性 API 中提出的紧凑 reference 形式。它标识当前 served 的 group/version，
而不是任意 remote resource。未来若需要通用 workspace-provider API，应通过经过 review 的 API
change 引入 versioned reference semantics，而不是悄悄扩大这些字段的含义。

该引用始终只作用于本 namespace。它没有 `namespace` 字段，API 也会拒绝不支持的
kind/group 组合。即便创建者拥有读取权限，Run 也不能引用其他 namespace 的 workspace。

第一版中，下列两种形式等价：

```yaml
spec:
  workspace:
    name: ci-build-workspace
```

```yaml
spec:
  workspace:
    name: ci-build-workspace
    kind: PersistentWorkspace
    apiGroup: kruntimes.io/v1alpha1
```

Run reference 只表达 intent。它不会复制 workspace status、bind workspace 或隐式选择 Runtime
Pod。PersistentWorkspace controller 仍拥有 binding 与 lifecycle。后续 scheduler 和 runtimed
PR 必须拒绝不兼容的 Runtime binding，并遵守已 bound workspace 的 placement，同时不能理解任何
Workflow 概念。

## Run Affinity API

Run affinity 使用 Kubernetes 中熟悉的 required/preferred 模型与 label selector 词汇，但不会
直接复用 `corev1.Affinity`。Kubernetes 用该类型为新 Pod 选择 Node；kruntimes 是为 Run 从已有
的 ready Runtime Pod 中选择一个。直接复用会把当前并未实现的 Node/Pod 语义变成表面 API。

```go
type RunAffinity struct {
    RunAffinity     *RunAffinityRules `json:"runAffinity,omitempty"`
    RunAntiAffinity *RunAffinityRules `json:"runAntiAffinity,omitempty"`
}

type RunAffinityRules struct {
    RequiredDuringSchedulingIgnoredDuringExecution []RunAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedRunAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type WeightedRunAffinityTerm struct {
    Weight          int32           `json:"weight"`
    RunAffinityTerm RunAffinityTerm `json:"runAffinityTerm"`
}

type RunAffinityTerm struct {
    LabelSelector *metav1.LabelSelector `json:"labelSelector"`
    TopologyKey   string                `json:"topologyKey"`
}

type RunSpec struct {
    // Existing fields omitted.
    Affinity *RunAffinity `json:"affinity,omitempty"`
}
```

字段名有意贴近 Kubernetes：

- `requiredDuringSchedulingIgnoredDuringExecution` 是硬调度约束。不满足任一 term 的 candidate
  不可用；若没有 eligible candidate，Run 继续保持 `Pending`。
- `preferredDuringSchedulingIgnoredDuringExecution` 是软 scoring hint。它不能绕过 required
  rules 或 Runtime capacity。
- `runAffinity` 要求匹配的 active Run 位于请求的 topology。
- `runAntiAffinity` 要求匹配的 active Run 不位于该 topology。

初版只支持一个 topology key：

```text
kruntimes.io/runtime-pod
```

对该 key，topology equality 表示 assigned Runtime Pod name 相等。这是
`RuntimePodLocal` PersistentWorkspace 所需要、直接且易理解的约束；scheduler 不需要解释 Node、
zone 或 Kubernetes Pod affinity。

所有 term 都匹配同 namespace `Run` objects 上的 labels。API 本身不定义 scheduler 的 bootstrap 或
reservation behavior。当前 single-Run implementation 使用已 assignment 的 `Scheduled`、`Running` 和
`Ready` Runs 作为 active targets，但这不足以处理第一条 Run 需要与其它 Run 产生 required affinity 的 cohort。
建议的 [Scheduler Framework](scheduler-framework/) design 定义 actual targets、同一 cycle 的
planned targets 和 Run 间亲和性 bootstrap。在替换当前 placement implementation 前，必须 review 这些
execution semantics。

示例：要求后续 build step 运行在前一步选定的 Runtime Pod：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: ci-build-test
  labels:
    workflows.kruntimes.io/workflow: ci-data-sharing-demo
    workflows.kruntimes.io/job: build
spec:
  runtime: bash
  workspace:
    name: ci-build-workspace
  affinity:
    runAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              workflows.kruntimes.io/workflow: ci-data-sharing-demo
              workflows.kruntimes.io/job: build
          topologyKey: kruntimes.io/runtime-pod
  mode:
    task: {}
```

Workflow controller 在生成的 Run 中会使用更有选择性的 label set，确保一个 job 不会因用户复制了
不相关的 labels 而匹配无关 Run。通用 API 则有意允许任意 caller-owned labels。

## Validation 与 Transition Rules

第一版 CRD validation 保持机械且有边界：

- `workspace.name` 使用 Kubernetes object name 的长度和格式 validation。
- 若设置 `workspace.kind`，必须为 `PersistentWorkspace`。
- 若设置 `workspace.apiGroup`，必须为 `kruntimes.io/v1alpha1`。
- 每个 affinity term 都需要非空 `labelSelector`，且只能使用支持的 `topologyKey`。
- required 与 preferred term lists 都有小型上限；preferred weight 是 1 到 100 的整数。

与其他 execution inputs 相同，`workspace` 和 `affinity` 在 Run 创建后不可变。assignment 后修改
其中任意一个，可能让 Running Run 观察到不同的 shared-data boundary 或 placement requirement。
这是实验性 API validation change，API skeleton PR 必须重新生成 CRDs 并增加 integration tests。

初版刻意不支持 cross-namespace selectors、namespace selectors、Node affinity、任意 topology、
topology spread，或直接的 `podName`/sticky-key field。每项都会扩大 scheduler 的 trust 与 RBAC
surface，需要独立 design review。

## Scheduler Contract

API skeleton 只声明和 validation fields。调度执行语义由建议的
[Scheduler Framework](scheduler-framework/) 文档定义。特别是，implementation 必须使用有界
planner，并将 PreFilter、Filter、Score、Reserve 和 Bind 作为独立阶段，而不是让每个 Run reconcile
独立决定 placement。

scheduler 仍保持 Workflow-agnostic。它不创建 workspace，也不理解 job 或 step labels；它只评估
通用 Run labels 与声明的 affinity terms。

## 与 PersistentWorkspace 的交互

Run 使用 workspace 前，`PersistentWorkspace.spec.runtime` 必须等于 `Run.spec.runtime`。对于
`RuntimePodLocal`，workspace bound 后 scheduler 必须只允许 `status.boundPod`；它可以直接表达此
内部约束，而不要求 controller 伪造一条 affinity target Run。public affinity API 对直接用户以及
需要跟随另一条 active Run 的后续 steps 仍有价值。

missing、pending、lost 或 incompatible workspace 在调度时不是 terminal Run failure。后续
workspace-aware reconciliation design 必须提供明确 condition 和 requeue behavior；精确的
failure/retry policy 不属于本 API PR。

## 实现顺序

1. 只增加 Go API types、deepcopy generation、CRD schemas 与 validation integration tests。
2. 增加 scheduler candidate filtering/scoring 及 focused unit/integration coverage。
3. 在 controller 和 runtimed 中增加 PersistentWorkspace binding 与 Run workspace
   admission/preparation。
4. 增加 Workflow controller composition 和端到端 data-sharing coverage。

步骤 2 到 4 必须保持为独立 PR，确保 scheduler 和 runtimed 的组件边界清晰。
