# Workflow Data Sharing

本文描述 v0.x 的目标设计，当前尚未实现。

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

当前尚不支持：

- job 之间的 first-class artifact inputs；
- workspace object 或 lifecycle；
- 用于 co-locate child Runs 的 Run affinity/anti-affinity；
- 将 child Run artifact references 显式提升到 Workflow status；
- shared job-local workspace 的 cleanup 和权限边界。

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
2. 有候选 Pod 时，controller 选择按名称字典序最小的 ready Runtime Pod。在稳定 Pod 集合下该选择是
   deterministic 的；后续调度工作使用 `status.boundPod`，而不是试图重复这个选择。
3. controller 记录 `status.phase: Bound`、`status.runtime`、`status.boundPod`，以及计划使用的本地
   路径 `/workspace/persistent/<workspace-name>`。controller 不会自行创建目录；runtimed 在引用它的
   Run 启动时创建。
4. Bound workspace 在 Pod 仍存在时保持绑定，即使该 Pod 暂时不 ready。status conditions 会让
   availability 问题可见；引用它的 Runs 将保持 Pending，直到后续 scheduler 和 runtimed 工作能够
   安全地使用这个 binding。
5. bound Pod 被删除或不再存在时，workspace 变为 `Lost`。controller 不得静默地把它绑定到另一个
   Pod：对于 `RuntimePodLocal`，那会让调用者把新的空目录误认为原有数据。恢复需要显式创建新的
   workspace，或等待未来经过 review 的 recovery API。

这个 binding slice 仅写入 metadata。TTL cleanup、filesystem deletion、`lastUsedTime` 和 Run
admission/preparation 都是独立的后续工作。

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

runtimed 在执行前准备被引用的 workspace path，并在 Run 完成后只清理 per-Run temporary
state。workspace lifecycle 由 `PersistentWorkspace` controller 拥有。

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
              path: ./dist.tgz
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
          uri: s3://kruntimes-artifacts/workflows/ci-data-sharing-demo/jobs/build/dist.tgz
      steps:
        package:
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
| Scheduler | 应用通用 Runtime capacity 和 Run affinity/anti-affinity。不理解 Workflows。 |
| runtimed | 准备被引用的 workspace paths，stage artifact inputs，collect artifact outputs，并清理 per-Run temporary state。不理解 Workflows。 |
| ArtifactStore | 将 durable artifacts 存储在 etcd 之外。 |

## 失败和恢复

- 如果 Runtime Pod 消失，由该 Pod workspace volume 支撑的 `RuntimePodLocal` workspaces 变为
  `Lost`；它们不会自动 rebind 到另一个 Pod。
- 需要不可用 workspace 的 Runs 应保持 Pending，或根据 retry policy/controller decision 以清晰
  workspace condition 失败。
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
7. 将 `RuntimePodLocal` PersistentWorkspaces 绑定到 ready Runtime Pods，并记录 lifecycle
   status，不修改 runtime filesystems。
8. 更新 runtimed workspace preparation 和 cleanup，使其支持 referenced workspaces。
9. 增加 Workflow step artifact input fields 和 job-scoped artifact status。
10. 将 child Run artifact refs 提升到 Workflow status。
11. 增加 E2E 覆盖 Runtime workspace volume sources、job-local workspace sharing、
   job-to-job artifact passing、Runtime Pod loss、cleanup 和 permission boundaries。
