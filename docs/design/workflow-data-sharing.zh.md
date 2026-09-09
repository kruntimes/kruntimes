# Workflow Data Sharing

本文描述 v0.x 设计。API prerequisite、RuntimePodLocal binding、scheduler admission、
runtimed workspace preparation、Workflow job-local workspace 和 job-to-job artifact transfer
均已实现。

RuntimePodLocal binding、scheduler fencing 和 runtimed preparation：**已实现**

目标是定义 Workflow jobs 和 Runs 如何共享数据，同时不让 scheduler 或 runtimed 理解
Workflow-specific 语义。这个设计由 v0.x workflow demo 目标驱动：job-to-job 数据应通过
artifacts 传递，而同一个 job 内的 Runs 应在 Workflow controller 请求 co-location 时可以
共享 job-local workspace。

## 当前状态

当前 experimental Workflow API 支持：

- 带 `needs` 的 jobs；
- job 内顺序执行 steps；
- 每个 step 创建 child Run；
- 来自 `KRUNTIME_OUTPUTS` 的有界 step outputs；
- 用于小字符串 outputs 的 cross-step 和 cross-job expression references。
- 使用 inline Kubernetes `VolumeSource` 和 `emptyDir` 默认值的 `Runtime.spec.workspace`；
- `PersistentWorkspace` API types、CRD validation、status，以及带 UID fencing 的 binding lifecycle；
- 通用 Run workspace references 和 Kubernetes-style Run affinity fields。
- workspace-aware scheduler filtering，以及 workspace 变更触发的 Pending Run wakeup；
- controller-managed workspace lifecycle 和 cleanup，包括显式删除和 unused-TTL 的 E2E 覆盖；
- 通过 admission-time `use` review 和针对已验证 Workflow child Run 的 fenced controller
  ServiceAccount path 实现 identity-based workspace authorization。

## 目标

- Jobs 通过 ArtifactStore-backed artifacts 交换 durable data。
- 同一个 Workflow job 内的 Runs 可以共享 job-local `PersistentWorkspace`。
- Workflow controller 拥有 job/workflow 语义。
- Scheduler 和 runtimed 保持 workflow-agnostic。它们只暴露通用 placement 和 workspace
  primitives，供其他功能复用。
- API 保持 cross-job data durable、auditable，并且不依赖 Runtime Pod placement。
- 在实现前明确 cleanup、failure recovery 和 permission boundaries。

## 非目标

- 这不是 Argo Workflows 或 Tekton 的完整替代品。
- 这不会增加通用分布式文件系统。
- 这不会让 Runtime Pods 对任意 hostile code 安全。
- 这不要求 scheduler 或 runtimed 理解 Workflows、jobs 或 steps。
- 这不会默认让 job-local workspaces 跨 node 或跨 Pod。

## 数据共享模型

有两条数据共享路径：

| 边界 | 机制 | 原因 |
| --- | --- | --- |
| Job to job | ArtifactStore-backed artifacts | Durable、auditable，跨 Runtime Pods 和 nodes 可用。 |
| 同一个 job 内的 Run to Run | `PersistentWorkspace` 加 Run affinity | 为同一个 job 内顺序 steps 提供快速本地共享。 |

小的 scalar values 继续使用 bounded outputs：

```text
step -> KRUNTIME_OUTPUTS -> Run.status.outputs -> Workflow status
```

较大的文件不应嵌入 Workflow 或 Run status。它们应通过 artifact references 或被引用的
workspace 传递。

### Artifact Input Contract

artifact transfer 分为两层，这样 Workflow controller 不复制数据，而 runtimed
也不需要了解 Workflow：

1. 通用 Run artifact input 包含 immutable `ArtifactRef` 和 relative destination
   path。执行前，runtimed 通过配置的 `ArtifactStore` 打开该 reference，并将内容安全地
   stage 到 Run working directory 下。file artifact 会复制到 destination path；directory
   artifact 会被解压到该位置，且不允许 symlink 或 path traversal。解压后的 regular file
   使用规范化的 `0640` mode；如果 producer 文件具有 owner-execute bit，则保留该 bit，
   得到 `0740`。不会继承 artifact metadata 中的 group、other、write 或 special mode bits。
2. Workflow step 使用 `jobs.<job-id>.artifacts.<artifact-name>` 表达 source。当 job
   ready 后，Workflow controller 从 producing job 的 compact artifact status 解析该名称，
   再 materialize 通用 Run input。Run API 和 runtimed 都不包含 Workflow、job 或 step
   identifier。

producing job 必须是 consuming job 的 `needs` dependency。当所有 dependencies 都成功后
artifact 仍不存在时，Workflow job 应 deterministically failed，而不是无限等待。

`ArtifactRef` 只标识 storage coordinates，不携带 authorization。v0.x 中，交换 artifacts
的 jobs 必须使用 compatible 的 `Runtime.spec.artifactStore` configuration：consuming
runtimed 必须能用自己的 credentials 和 store scope 打开 producer reference。该约束支持 shared
PVC filesystem store 和 shared S3 bucket/prefix access。具有 independent credentials 的
project-wide artifact relay 是后续 feature；Workflow controller 不代理 artifact bytes。

artifact name 在同一个 job 内形成一个 namespace。Job status 会为每个名称投影最近成功的 child
Run reference，因此后续 sequential step 可以有意替换前一个 artifact。consumer 会在 producer job
succeed 后读取其最终 reference。

## PersistentWorkspace CRD

`PersistentWorkspace` 表示 workspace 边界和生命周期。它不是 Workflow-specific 对象；
Workflow 只是其中一个使用方。

它不选择底层 Kubernetes volume。`PersistentWorkspace` 会绑定到目标
`Runtime.spec.workspace` 声明的 workspace volume。对于初始的 `RuntimePodLocal` mode，
workspace 实现为该 Runtime Pod 挂载的 `/workspace` volume 下的子目录。

目标形态：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: PersistentWorkspace
metadata:
  name: ci-build-workspace
spec:
  runtime: bash
  mode: RuntimePodLocal
  ttlSecondsAfterUnused: 3600
  cleanupPolicy: DeleteAfterTTL
status:
  phase: Bound
  runtime: bash
  boundPod: runtime-bash-7f587b4668-njcks
  boundPodUID: 2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6
  path: /workspace/persistent/ci-build-workspace
  lastUsedTime: "2026-07-06T12:00:00Z"
```

第一版支持的 mode 应该是 `RuntimePodLocal`：workspace 位于某个特定 Runtime Pod 上，只有
调度到该 Pod 的 Runs 才能复用。

durability 和 sharing 特征来自 Runtime workspace volume。如果 Runtime workspace 是
in-memory `emptyDir`，那么 `PersistentWorkspace` 也是 Runtime-Pod-local，并随 Pod 丢失。
如果未来 Runtime workspace 由 PVC 或其他 Kubernetes volume source 支撑，workspace 可以继承
该 backing store 的 durability 和 attachment rules。

## Runtime Workspace Volume

当前 `Runtime.spec.workspace` inline Kubernetes `VolumeSource` 字段；如果没有显式设置
workspace volume source，controller 会把保留的 `workspace` volume 创建成 `emptyDir`。

目标方向：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: bash
spec:
  workspace:
    persistentVolumeClaim:
      claimName: bash-workspace
```

更推荐的 API 是把 Kubernetes `corev1.VolumeSource` 字段 inline 到 `spec.workspace` 下，而不是发明一套单独的
workspace volume model，或再包一层 `volumeSource` object。当没有显式 workspace volume
source 时，`emptyDir` 仍然是默认行为。`emptyDir` 选项，例如 `sizeLimit`，应使用原生的
`workspace.emptyDir.sizeLimit` 形态，而不是 kruntimes-specific shorthand。

Runtime workspace volume 的扩展是 durable 或 PVC-backed `PersistentWorkspace` 的前置工作。
第一版仍可以基于现有 `emptyDir` 行为实现 `RuntimePodLocal`，但设计上不应把 emptyDir 固化为
唯一 backing store。

### 建议的 RuntimePodLocal Binding Lifecycle

binding controller 在 v0.x 中应遵循以下规则：

1. 未绑定 workspace 在其引用的 Runtime 没有 ready Runtime Pods 时保持等待。无论等待或已经绑定，
   它都不会消耗或预留 Run capacity。
2. 有候选 Pod 时，controller 先按 `metadata.name` 对 ready Runtime Pod 排序，再根据
   PersistentWorkspace UID 的稳定哈希选择一个 Pod。这样可将首次绑定分散到多个 ready Pod，且在
   候选集合不变时保持 deterministic；后续调度工作使用 `status.boundPod` 和
   `status.boundPodUID`，而不是试图重复这个选择。
3. controller 记录 `status.phase: Bound`、`status.runtime`、`status.boundPod`，以及计划使用的本地
   immutable 的 `status.boundPodUID`，以及计划使用的本地
   `status.path: /workspace/persistent/<workspace-name>`。controller 不会自行创建目录；runtimed 在引用
   它的 Run 启动时创建。
4. Bound workspace 在 Pod 仍存在时保持绑定，即使该 Pod 暂时不 ready。status conditions 会让
   availability 问题可见；引用它的 Runs 将保持 Pending，直到后续 scheduler 和 runtimed 工作能够
   安全地使用这个 binding。
5. bound Pod 被删除、不再存在，或同名 Pod 的 UID 不同时，workspace 变为 `Lost`。controller 不得静默地
   把它绑定到另一个 Pod：对于 `RuntimePodLocal`，那会让调用者把新的空目录误认为原有数据。恢复需要显式
   创建新的 workspace，或等待未来经过 review 的 recovery API。

这个 binding slice 仅写入 metadata。TTL cleanup、filesystem deletion、`lastUsedTime` 和 Run
admission/preparation 都是独立的后续工作。

### Cleanup Protocol

`PersistentWorkspace` cleanup 是两部分协议。controller 负责 logical lifecycle；runtimed
负责删除 Runtime Pod-local mount 中的 bytes。controller 不得通过 Pod exec 删除路径，也不得因为
删除 workspace 而假定某个 custom Runtime 实现了 shell command。

1. PersistentWorkspace controller 按 `spec.workspace.name` 为 Run 建立索引。当任一引用 Run
   非 terminal 时 workspace 为 active。当最后一个 active Run 变为 terminal 时，controller
   只设置一次 `status.lastUsedTime`。新 Bound 且没有 Run 的 workspace 从 Bound 时开始 unused
   interval。
2. `cleanupPolicy: DeleteAfterTTL` 仅在设置且到达 `ttlSecondsAfterUnused` 后开始 cleanup。nil TTL
   有意表示不自动删除。`Retain` 永远不自动开始 cleanup。
3. controller 请求 cleanup 时，将 workspace 置为 `Released`，然后请求 Kubernetes deletion，
   同时保留专用 workspace finalizer。`Released` 对调度是 terminal：
   不允许新的 Run claim 它。
4. 已记录 `status.boundPod` 上的 runtimed 仅 watch 绑定到自己 Pod 的 workspace object。在独立确认
   没有 local non-terminal Run 引用该 workspace 后，它只删除
   `/workspace/persistent/<workspace-name>`，然后移除该 finalizer。它不写 Workspace status。该操作
   不需要 Workflow 语义，也不需要 runtime-server extension。
5. controller 是唯一的 status writer。如果其 bound Pod 已消失，workspace 已为 `Lost`；没有剩余
   Pod-local data 需要删除，controller 不等待 runtimed 即可移除 finalizer。

所有已 Bound workspace 不论 cleanup policy 或 deletion 原因都遵循同一删除协议：finalizer 保留 object，直到 bound
runtimed 在物理删除后移除 finalizer。`Retain` 仅禁止自动 TTL 删除；显式删除仍会移除 Pod-local directory。
如果 live bound Pod 暂时 unavailable，cleanup 保持 pending，不能冒险删除另一个 Pod 上的目录；该 Pod 被删除后
workspace 转为 `Lost`，并解除 finalizer 阻塞。

### 权限边界设计

仅对 `PersistentWorkspace` resource 使用 Kubernetes RBAC 并不足够：它控制谁可以读取或修改 object，
但不能控制一个允许创建 Run 的主体是否可以使用某个已有 workspace。scheduler 和 runtimed 不能承担该决策，
因为它们拿不到原始 Kubernetes request identity。

已实现的 v1.0 模型在 `persistentworkspaces/use` subresource 上使用 Kubernetes-native 的 `use` permission：

1. validating admission webhook 处理带 `spec.workspace` 的直接 Run create。它要求被引用的 workspace
   已存在，检查其 Runtime 与 Run 匹配，然后为请求主体同步创建并等待 `SubjectAccessReview`：verb 为 `use`，resource
   为 `persistentworkspaces`，subresource 为 `use`，resource name 为 workspace name。webhook 必须在有界 timeout
   内返回 admission decision：建议为 review 分配约两秒，并为完整 webhook request 配置五秒。review 被拒绝、超时或
   API error 时，Run 会被拒绝，而不是让未授权 reference 保持 Pending。webhook 配置使用 `failurePolicy: Fail`，因此
   webhook 不可达或超时时也会 fail closed。
2. namespace administrator 通过普通 Role 或 RoleBinding 授予该 permission。`resourceNames` 可以将主体限制到
   指定 workspace，而无需在 CRD 中增加 kruntimes-specific ACL。
3. chart 配置的 controller ServiceAccount 对 Workflow child Run 有一条受限的 internal path。仅当 webhook
   能证明 Run 和被引用 workspace 的 controller owner reference 指向同一个 live `WorkflowRun` UID、两者的
   `WorkflowRunUIDLabel` 都匹配该 UID、且 workflow job label 相同，才允许该身份绕过第二次
   `SubjectAccessReview`。malformed、stale 或 cross-job reference 会被拒绝。用户即使复制 labels 或
   owner references 也无法获得该路径，因为只有配置的 ServiceAccount identity 会进入它；其它所有调用方
   都走普通 `use` review。
4. scheduler 和 runtimed 保持 workflow-agnostic 和 authorization-agnostic。它们继续只负责 workspace existence、
   Runtime compatibility、binding、lifecycle 和 Pod placement。

这会改变 direct Run 对不存在 workspace reference 的行为：不再接受后等待未来 object，而是在 admission 时拒绝。
Workflow controller 仅在其 owned workspace 已存在后创建 child Run，因此仍保留正常的 asynchronous binding 行为。

Helm chart 部署 webhook Service、由 chart 管理的 TLS Secret，以及配置了 `failurePolicy: Fail` 的
`ValidatingWebhookConfiguration`，并将 controller ServiceAccount identity 显式配置给 webhook process。
该 ServiceAccount 可以创建 `SubjectAccessReview`，但没有泛化的 `persistentworkspaces/use` permission；仅能通过
上面已验证的 Workflow child-Run path 使用 workspace reference。面向 impersonation 的 integration 和 E2E coverage
仍属于后续工作。

## Run Workspace Reference

Runs 应能通过一个小的 typed object reference 引用 workspace。`PersistentWorkspace` 是这个
API 的默认 kind，但这个 reference shape 为未来 workspace providers 留出空间：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: ci-build-package
spec:
  runtime: bash
  workspace:
    name: ci-build-workspace
    kind: PersistentWorkspace
    apiGroup: kruntimes.io/v1alpha1
  source:
    inline: |
      tar -czf "$KRUNTIME_ARTIFACTS_DIR/dist.tgz" src
```

`kind` 和 `apiGroup` 是 optional。省略时默认是 `PersistentWorkspace` 和
`kruntimes.io/v1alpha1`。

runtimed 在执行前准备被引用的 workspace path。workspace lifecycle 由
`PersistentWorkspace` controller 拥有。对于 task-mode Run，
runtimed 将 inline 和 Git source stage 到 workspace 保留的
`.kruntimes/runs/<run-uid>` 目录，以 workspace 作为 current directory 执行。outputs 和
artifact staging 也使用这个 Run-local directory。runtimed 不会删除 referenced workspace
中的任何路径；PersistentWorkspace controller 根据其 lifecycle 与 cleanup policy 回收。
这样所有 runtimed-managed 文件都不会覆盖其他 Run 有意共享的文件。Function-mode source 保持在
workspace root，以便 handler module resolution 仍然相对于 working directory。

## Run Affinity

Run affinity 应使用贴近 Kubernetes 的概念，因为用户已经通过 Pods 理解 affinity 和
anti-affinity。

目标形态：

```yaml
spec:
  affinity:
    runAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              workflows.kruntimes.io/workflow: ci-data-sharing-demo
              workflows.kruntimes.io/job: build
          topologyKey: kruntimes.io/runtime-pod
```

具体 type names 可以在 API 设计阶段调整，但概念应贴近 Kubernetes：

- required vs preferred rules；
- label selectors；
- topology keys；
- affinity 和 anti-affinity。

对于 job-local workspace sharing，Workflow controller 可以创建 job 中第一个 Run，绑定或发现
workspace，并为同一 job 后续 Runs 添加 required affinity。scheduler 只评估通用 Run
placement rules。

## Workflow API

目标 workflow 形态：

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: ci-data-sharing-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: checkout
          run: |
            mkdir -p src
            echo 'print("hello")' > src/app.py
        - name: test
          run: |
            test -f src/app.py
            echo "tests=passed" >> "$KRUNTIME_OUTPUTS"
        - name: package
          run: |
            mkdir -p "$KRUNTIME_ARTIFACTS_DIR"
            tar -czf "$KRUNTIME_ARTIFACTS_DIR/dist.tgz" src
    deploy:
      runs-on: bash
      needs:
        - build
      steps:
        - name: verify-artifact
          artifacts:
            - from: jobs.build.artifacts.dist.tgz
              path: dist.tgz
          run: |
            tar -tzf dist.tgz
            echo "artifact verified"
```

Workflow spec 不为默认 job-local sharing model 暴露 workspace controls。当一个 Workflow job
运行多个 steps 时，Workflow controller 创建并拥有 job-local `PersistentWorkspace`，具体
spec 由 controller 配置控制。常见场景下，用户不需要选择 workspace name、storage mode、
TTL 或 cleanup policy。

这个形态区分了：

- `checkout`、`test` 和 `package` 的 job-local workspace sharing；
- 从 `$KRUNTIME_ARTIFACTS_DIR` 自动上传的 job-scope artifacts；
- 从 `jobs.build.artifacts.dist.tgz` 到 `deploy` 的显式 artifact transfer；
- expressions 使用的 bounded scalar outputs。

在一个 job 内，steps 共享同一个 `KRUNTIME_ARTIFACTS_DIR` namespace。因此 artifact reference
不包含产生它的 step name。下游 job 使用 `jobs.<job-id>.artifacts.<filename>` 导入 artifact。

## Status Model

Workflow status 应暴露紧凑 artifact references，而不是 artifact contents：

```yaml
status:
  jobs:
    build:
      artifacts:
        dist.tgz:
          name: dist.tgz
          driver: Filesystem
          type: File
          location:
            filesystem:
              path: runs/<run-uid>/artifacts/dist.tgz
      steps:
        - name: package
          runName: ci-data-sharing-demo-build-package
          outputs:
            tests: passed
```

Workflow status 不应暴露 workspace binding 细节。这些细节应存在于 `PersistentWorkspace`
对象上，供 operators 查看。Workflow 只应暴露用户相关的 conditions 和 messages，例如某个
job 正在等待本地 workspace capacity，或者因为 controller-owned workspace 丢失而失败。

## 组件边界

| 组件 | 责任 |
| --- | --- |
| Workflow controller | 解释 job/step 语义，基于 controller defaults 创建 job-local workspaces，创建 child Runs，连接 artifact inputs，并把 outputs/artifact refs 提升到 Workflow status。 |
| PersistentWorkspace controller | 拥有 workspace lifecycle、绑定到 Runtime workspace volumes、status、TTL 和 cleanup。 |
| Scheduler | Snapshot 通用 Run、Runtime Pod 和 referenced workspace state；应用 workspace fencing、Runtime capacity 和 Run affinity/anti-affinity。不理解 Workflows。 |
| runtimed | 准备被引用的 workspace paths，stage artifact inputs，collect artifact outputs。不理解 Workflows，也不删除 referenced workspace 中的路径。 |
| ArtifactStore | 将 durable artifacts 存储在 etcd 之外。 |

## 失败和恢复

- 如果 Runtime Pod 消失，由该 Pod workspace volume 支撑的 `RuntimePodLocal` workspaces 变为
  `Lost`；它们不会自动 rebind 到另一个 Pod。
- 引用 missing、Pending、Lost、Runtime-incompatible 或 UID-mismatched workspace Pod 的 Runs 必须保持
  Pending 并显示明确 message。workspace changes 会重新入队匹配的 Pending Runs；上述状态都不是 scheduler
  terminal failure。
- Workflow controller 应通过 Workflow conditions 或 messages 暴露 workspace-related
  failures，但不在 Workflow spec 中暴露 workspace controls。
- Workspace cleanup 不应依赖 Runtime Pod 仍然存在。
- job 之间的 artifact transfer 在 Runtime Pod loss 之后仍应有效，因为 artifacts 存储在 Pod
  之外。

## 安全和隔离

`PersistentWorkspace` 会扩大其边界内的 blast radius。初始模型应把共享 workspace 的用户视为
相互 trusted。

必需 safeguards：

- namespace-scoped workspace references；
- auto-created workspaces 到 Workflow 或 WorkflowRun 的 owner references；
- workflow、job 和 controller ownership labels；
- validation 拒绝 artifact inputs 中的绝对路径和 path traversal；
- 显式 cleanup policy 和 TTL；
- 所有已 Bound workspace 的 finalizer-based deletion、active-Run usage tracking 和仅由
  runtimed 执行的 physical cleanup；
- 文档警告 shared workspace 不是 hostile-code isolation。

## 实现顺序

1. 增加本文档并 review API shape。
2. 扩展 `Runtime.spec.workspace` 以 inline Kubernetes `VolumeSource` 字段，同时保留当前
   emptyDir 默认行为。
3. 增加 `PersistentWorkspace` API types、CRD validation、status 和 controller skeleton。
   绑定到 Runtime Pods、Run workspace references 和 cleanup 是后续独立实现步骤。
4. 增加 Run `workspace` reference fields。
5. 增加 Kubernetes-style Run affinity/anti-affinity fields。
6. 更新 scheduler placement，使其支持 required/preferred Run affinity，同时保持无 capacity
   Runs Pending。
7. review bound-Pod UID fencing 修订后，增加 `status.boundPodUID`，再将 `RuntimePodLocal`
   PersistentWorkspaces 绑定到 ready Runtime Pods，并记录 lifecycle status，不修改 runtime filesystems。
8. 增加通用 `Workspace` scheduler Filter plugin。**已实现：** scheduling snapshot 只解析一次
   referenced workspace；filter 拒绝 Runtime 不匹配的 candidates；对于 Bound RuntimePodLocal workspace，
   只允许 fenced `status.boundPod` 和 `status.boundPodUID`。Pending 或 Lost workspace 没有 eligible
   candidates，因此对应 Run 保持 Pending 并显示清晰的 scheduling message；workspace changes 会重新入队
   对应的 Pending Run。
9. 更新 runtimed workspace preparation，使其支持 referenced workspaces。**已实现：**
   referenced Run 在 persistent workspace 中执行；outputs 和 artifact staging 仍然是
   Run-local；task source 被 stage 到保留的 per-Run directory。PersistentWorkspace lifecycle
   负责 workspace 内的 cleanup。
10. 在 Workflow controller 中组合 job-local workspaces。**已实现：**
    初始化时会为每个 inline job 创建一个由 WorkflowRun 拥有的
    PersistentWorkspace，并让该 job 的每个 child Run 引用它。reusable
    Workflow-call job 不在父级创建 workspace；其 materialized child
    WorkflowRun 拥有自己的 job workspace。
11. 增加 Workflow step artifact input fields 和 job-scoped artifact status。
    **已实现：**step 可从 direct `needs` dependency 引用
    `jobs.<job-id>.artifacts.<artifact-name>`；controller 将其解析为通用的
    `Run.spec.artifactInputs`。
12. 将 child Run artifact refs 提升到 Workflow status。**已实现：**成功的 Job 在
    `status.jobs.<job-id>.artifacts` 中暴露每个 artifact name 最后一次成功的 ref。
13. 增加 E2E 覆盖 Runtime workspace volume sources、job-local workspace sharing、
   job-to-job artifact passing、Runtime Pod loss、cleanup 和 permission boundaries。
   **已实现：**E2E 覆盖 Runtime workspace sources、job-local sharing、Run artifact staging、
   job-to-job transfer、Runtime Pod loss、cleanup 和 permission boundaries。admission authorization
   与 controller ownership 另有 focused unit 和 integration coverage。
14. 实现 cleanup protocol：active-Run tracking、Released admission fencing、workspace finalizer、
    runtimed local-path cleanup，以及 TTL、explicit deletion、retained workspace 和 cleanup 中
    bound-Pod loss 的 E2E 覆盖。
