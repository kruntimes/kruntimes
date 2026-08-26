---
title: "项目状态与路线图"
---

kruntimes 作为 `v0.x experimental` 项目活跃开发中。API 是 `v1alpha1`，可能在稳定发布
之前发生变更。

## 当前状态

已完成的基础功能包括：

- Run 和 Runtime CRDs。
- 预热 Runtime Pod 调度。
- Bash 和 Python 内置 runtimes。
- 有界输出和外部 artifact 引用。
- 通过长期运行的 maintainers 进行 Runtime artifact 清理。
- 重试、超时、取消、stale-pod 恢复和终止条件。
- Helm charts、发布工作流、SBOM、签名、CLI releases 和 benchmark harness。
- 安全、运维、发布、兼容性和自定义 Runtime 文档。

## 近期路线图

### 公开后的产品验证

已完成的验证支撑材料：

- 已发布对比指南，覆盖 kruntimes vs Knative、Argo Workflows、Tekton、Volcano，
  以及基于 Deployment 的 worker queue。
- 已发布清晰的 “when to use / when not to use” 指南，让用户理解 kruntimes 是
  warm execution substrate，不是完整 serverless platform、workflow engine、batch
  scheduler replacement 或 hostile-code sandbox。
- 已发布三个端到端 demo：低延迟 Bash/Python Run、burst short-task execution，
  以及 custom Bash Runtime image。
- 已定义 go/no-go signals：用户能在两分钟内解释项目价值，至少两个 design partners
  用真实 workload 试用，至少一个非 maintainer 跑通 quick start。
- 已增加用于 target-user interviews 和 design-partner trials 的公开 issue
  templates。

仍在验证：

- 招募来自 platform、CI 和 AI agent infrastructure 团队的 design partners，
  覆盖 short-lived、high-concurrency 或 agent-driven workloads。
- 与 5-8 个目标用户验证核心问题，确认他们是否在过去六个月真实遇到 Pod cold start、
  burst throughput 或 infrastructure-ownership 约束。
- 选择并验证第一个 primary wedge。当前假设是 AI agent tools 和 trusted internal
  code-execution sandboxes，CI micro-steps 和 automation tasks 作为次级场景。

### v0.x 实验期

下一阶段的重点是把公开的 `v0.x` release 推进成一个连贯的实验性产品。当前执行顺序：

实现顺序说明：新增 CRD、generated deepcopy、controller manager wiring、Helm RBAC
或 integration validation 的 API skeleton PR 应逐个 merge。一个 PR merge 后，后续 API
skeleton PR 需要 rebase 到 `main`，重新生成 manifests，并重新运行 `make test`、
`make test-integration` 和 `make test-helm`。这样可以避免 generated files 和手写
controller wiring 累积不必要的冲突。

- [x] Release/package hygiene：去掉已发布 image package 名字里冗余的
  `kruntimes-` 前缀，发布新 release，清理旧 package，并修正文档、安装和 demo 中
  的不一致。
- [x] Run input semantics：统一并稳定 `inline`、`entrypoint`、`args` 在 API、
  runtimes、CLI 示例、文档和测试中的行为。目标语义是：`inline` 是独立脚本，存在时
  `entrypoint` 和 `args` 不生效；`entrypoint` 指向脚本文件，`args` 作为参数传给
  `entrypoint`；当 `entrypoint` 不存在时，`args` 在 shell-style runtimes 中作为
  shell commands 执行。
- [x] Docs usability：为用户需要执行的命令增加 copy buttons，去掉示例中不必要的
  Helm overrides，并在 demo 使用 `krt` 命令前明确说明如何安装 `krt`。
- [x] Docs theme support：文档站点支持 Light theme、Dark theme，以及 Sync with
  system preference。
- [x] CLI baseline：增加 `krt version`，方便用户和维护者确认当前 CLI version、
  commit 和 build timestamp。
- [x] Benchmark correctness：诊断为什么 `latency.complete` 明显高于手动创建单个
  Run 的体感耗时，并明确 benchmark 测的是端到端 latency、调度 latency、
  watch/update latency，还是 runtime execution time。
- [ ] Runtime readiness visibility：可靠地将 Deployment readiness reconcile 到
  `Runtime.status.readyReplicas`，通过 `krt runtime list/get` 展示，并为 Pod 变为 ready 或 unavailable 时的
  status update 增加 integration 和 E2E coverage。
- [x] Scheduler framework：将独立的 per-Run placement 替换为 scheduler queue 和 Kubernetes-style 的
  单 Run scheduling cycles。在改变 scheduler behavior 前，review
  [Scheduler Framework](design/scheduler-framework.md) architecture。
  初始实现 TODO：
  - [x] review Run queue ownership、snapshot、PreFilter、Filter、Score、Reserve/Assume、Bind、status 和
    retry semantics；
  - [x] 在将 scheduler capacity check 扩展到内建 `runs` resource 以外前，review
    [Run resource accounting](design/run-resource-accounting.md) API；
  - [x] 在 queue/planner interfaces 后重构 scheduler internals，同时保留当前 observable behavior 和
    metrics；
  - [x] 增加 deterministic selection、assumed-capacity、bind-conflict 和 restart-recovery coverage；
  - [x] 实现 assumed affinity targets 和 Run 间亲和性 bootstrap：
    - [x] review Filter-plugin 修订，然后在 scheduler planner 中实现独立的
      RuntimePodAvailability 和 RunAffinity filters；
    - [x] 将 namespace-local actual assignment 和尚未确认的 assumed assignment 投影为不可变的
      affinity-target snapshot；
    - [x] 增加 required Run affinity 和 anti-affinity filter，以及有界的 Pending waiting reason；
    - [x] 在 deterministic capacity placement 前，对 preferred affinity 和 anti-affinity 评分；
    - [x] 允许 label-matching 的 eligible Run seed 一个空的 Run 间亲和性 cohort，同时让不能满足的
      dependency 保持 Pending；
    - [x] 增加 actual target、assumed target、bootstrap、anti-affinity、capacity 和 recovery 的
      unit、integration 与 E2E coverage；
  - [x] 为 Pending Run wakeups 增加 Runtime field index，避免每次 Runtime Pod 或 capacity event 都扫描
    namespace；
  - [x] 引入 Kubernetes-style 的 weighted Score plugins：
    - [x] 每个 plugin 为每个通过 Filter 的 Pod 打分，而不在 plugin 内缩小 candidate set；
    - [x] 将 plugin score normalization 至 `0..100`，应用 fixed internal weights，聚合 totals 并按总分降序排名；
    - [x] 对 equal totals 保留 framework-owned 的 deterministic Pod-name tie breaking；
    - [x] 增加 normalization、weights、ties 和 errors 的 unit 与 integration coverage；
  - [x] 增加有界 scheduler metrics：
    - [x] 按有界 plugin 和 reason 统计 Filter-plugin 对 Pod 的 rejection；
    - [x] 按有界 stage 统计 stale `Reserve` 和 conflicting `Bind` 操作；
    - [x] 按有界 event source 统计 requested Pending Run wakeups；
- [ ] Function-mode Runs：定义 mutually exclusive 的
  `Run.spec.mode.task` 和 `Run.spec.mode.function` 语义，让 function Run 可以 reserve
  预热 Runtime Pod，向 runtimed/runtime-server 注册 callable function，保持 ready 状态
  以支持多次低延迟 invocation，并在删除或 idle timeout 时释放 reservation。
  Function-mode Runs 仍然遵守普通 Runtime capacity，因此当 capacity 允许时，多个
  function Runs 可以共享同一个 Runtime Pod。这个能力应该走 dataplane invoke path，
  而不是为每次 invocation 创建 Kubernetes object。
  初始实现 TODO：
  - [x] 增加 `Run.spec.mode.task` 和 `Run.spec.mode.function` API 字段、CRD validation
    和 runtime helpers；
  - [x] 在 API 稳定前删除 top-level 的 `entrypoint`、`args` 和 `handler`；
  - [x] 将 CLI 创建和高层用户文档迁移为使用 `spec.mode.task`；
  - [x] review 并确认
    [function lifecycle 和 invoke dataplane 设计](design/function-mode-lifecycle.md)；
  - [x] 增加 `Ready`、assigned Pod UID、有界 endpoint status、generated CRDs 和
    active/non-terminal phase-classification tests；
  - [x] 增加 immutable execution-input transitions 和 function cleanup finalizer
    constant；
  - [x] 在注册 inline function Run 前 review 并批准
    [Function Inline Source 物化](design/function-inline-source.md) API；
  - [x] 删除 top-level `Run.spec.handler`、`Run.spec.entrypoint` 和 `Run.spec.args`；
    handler 放在 `Run.spec.mode.function.handler` 下，task input 放在
    `Run.spec.mode.task` 下；
  - [x] review 并确认
    [Function Runtime Server 协议](design/function-runtime-contract.md)；
  - [x] 增加以 Run UID 为 key 的幂等 register/status/invoke/unregister protobuf operations；
  - [ ] 实现内置 function adapters：
    - [x] Bash FunctionRuntime adapter：handler validation、registration fencing、单个
      in-flight invocation、有界输出和 unregister drain；
    - [x] Python FunctionRuntime adapter：handler validation、registration fencing、单个
      in-flight invocation、有界输出和 unregister drain；
  - [ ] 增加有界 invocation outputs/artifact references，以及以 Run UID 和 invocation ID
    为 key 的 structured logs；
  - [ ] 以可独立 review 的分片实现 function control-plane lifecycle：
    - [x] 增加 deterministic FunctionRuntime registration request builder，
      包含 immutable-input digest coverage；
    - [x] 在 Run working directory 下经过验证的 `source.inlinePath` 物化 inline function source；
    - [ ] 让已 assigned 的 function Run 完成 source preparation、安装 cleanup finalizer，
      通过 runtimed FunctionRuntime client 进行 local registration，并完成
      `Running -> Ready` transition；
    - [ ] 观察 local `FunctionStatus`，处理 fatal registration loss、total Run timeout 和
      Runtime Server-owned idle timeout；
    - [ ] 将 registration failure 接入 shared retry engine，同时不 retry 单次 invocation
      failure；
    - [ ] 实现 cancellation 和 deletion finalization：drain 或 cancel local registration，
      只清理 function-local state，并释放 capacity；
    - [ ] 在 runtimed restart 后恢复 active function registration，并使用 assignment-UID
      fencing reconcile stale Runtime Pod assignment；
    - [ ] 增加 registration、retry、timeout、cancellation、deletion、restart recovery 和
      stale-pod fencing 的 unit、integration 和 E2E coverage；
  - [ ] 覆盖 function registration、ready status、local/proxied invoke、多次 invocation、
    idle timeout、explicit release、Runtime Pod restart recovery 和 cleanup。
- [x] Runtime gateway invoke path：在 Helm chart 中增加可选的共享 `runtime-gateway`
  Deployment 和 ClusterIP Service。gateway 暴露稳定的 HTTP Run endpoint，解析目标 Run，并调用
  其 Runtime 的 Kubernetes Service。Kubernetes 选择 ready Runtime Pod；runtimed 只在该 Runtime
  内解析 owner。
  初始实现 TODO：
  - [x] 在变更 chart 或 endpoint transport 前，定义 [gateway TLS 和传输边界约定](design/runtime-gateway-transport.zh.md)；
  - [x] 为共享 gateway Deployment、ClusterIP Service、专用 runtimed gRPC port 和
    HTTP-to-gRPC adapter 增加 Helm templates、`gateway.enabled`、values、RBAC 以及 unit/render coverage；
  - [x] 实现明确的 `http`/`https` gateway protocol set；默认使用 chart-managed TLS Secret，
    允许 existing Secret，并在两种 protocol 同时启用时发布 HTTPS 及其 CA bundle；
  - [x] 为配置的 TLS Secret 增加可选的 cert-manager `Certificate` rendering；
  - [x] 实现 Runtime-scoped Run lookup，以及有界 local 或 single-hop peer routing；
  - [x] 使用 immutable Run UID 和 assigned Pod UID fence routing，在转发前拒绝 stale assignment；
  - [x] 通过 Kubernetes TokenReview authenticate caller token，并通过 SubjectAccessReview
    authorize target Run；
  - [x] 增加 bounded authorization decision cache；
  - [x] 执行每个 gateway 的有界 HTTP request concurrency limit；
  - [x] 在需要时使显式 request 和 response limits 可配置；
  - [x] 增加同时启用 HTTP 和 HTTPS 的 E2E coverage，包括对 chart-managed certificate 的严格验证；
  - [x] 实现经过 review 的有界、支持分页的 `ListSessionFiles` contract：
    - [x] 在 Session Mode design 中定义 HTTP、gRPC、SDK、排序、cursor、mutation-consistency 和
      response-bound semantics；
    - [x] 向 gRPC contract 增加 `limit`、`page_token` 与 `next_page_token`，并重新生成 client；
    - [x] 在内置 Runtime Server、runtimed proxy、HTTP gateway 以及 Go/Python SDK 中实现一致的有界
      cursor paging；
      - [x] 在 Bash 和 Python Runtime Server 中执行 direct gRPC paging，使用共享 cursor encoding
        和 UTF-8 byte-wise ordering；
      - [x] 通过 gateway 映射 HTTP `limit` 与 `pageToken`，并通过 runtimed proxy routing
        保留 page fields；
    - [x] 增加 limits、ordering、invalid/mismatched token 与 traversal-safe multi-page listing 的
      unit、integration 和 E2E coverage。
- [x] Agent sandbox 的 Session-mode Runs：不引入独立 `Sandbox` CRD，而是通过预热 Runtime
  Pod 上的 stateful、mutable workspace 提供 sandbox。已接受的
  [Session Mode 设计](design/session-mode.zh.md) 使用 `Run.spec.mode.session`、由共享 gateway
  转换为 runtimed 暴露的 `SessionRuntime` gRPC service 的 HTTP API 与 v0 trusted container backend。Session Run 通过专用 `runs: 1` Runtime 与
  runtimed 本地 claim 门控实现独占，ephemeral；assigned Pod 丢失时终止，而不是在新 Pod 上静默恢复。
  初始实现 TODO：
  - [x] review 并确认 API、lifecycle、queue、data-plane、security 和 future-backend 设计；
  - [x] 增加 `Run.spec.mode.session`、validation、generated CRDs 和 status conditions；拒绝
    声明 `spec.workspace` 的 Session Run；
  - [x] 将 source 与 artifact inputs 初始化到 Run-UID-scoped ephemeral workspace，在本地注册
    session，并将 Run transition 到 `Ready`；
  - [x] 实现内部 `SessionRuntime` contract，覆盖 lifecycle、workspace-constrained commands、
    file operations、process groups 和 operation status；
  - [x] 在共享 gateway 增加 authenticated versioned Session HTTP API、HTTP-to-`SessionRuntime`
    translation，以及有界 request/response handling；
  - [x] 由 owner runtimed 为现有 Kubernetes log collector 和 `krt logs` 输出 structured
    Session output 与 audit logs；不创建单独的 gateway log store；
  - [x] 实现每个 session 的 FIFO mutation queue：每次一个 active command 或 file mutation、
    global 与 per-Run bounds、默认/最大 operation timeout、cancellation 和 graceful termination；
    - [x] 在 owner runtimed 通过每个 Session 的 FIFO queue 串行 mutation，支持 Run 级 queue/timeout
      限制、队列满拒绝与 Session close 时取消；
    - [x] 暴露 global queue、operation timeout 上限，以及 runtimed 到 Runtime Server 的
      session close deadline 的管理员配置接口；
    - [x] 定义 backend-specific 的 graceful process-termination 配置；不会将通用 setting 注入
      任意 custom Runtime image；
  - [x] 将 operation history 与 audit events 写到 structured external logs；Run status 只保留
    有界 readiness、endpoint 与 artifact reference 数据，大输出通过 ArtifactStore 导出；
    - [x] 用单调的 `termination.mode` API 替换 `cancelRequested`：`Immediate` 用于 cancellation，
      仅 Session 合法的 `Drain` 用于正常 completion；实现 `Ready -> Finalizing -> Succeeded` lifecycle，
      以及彼此独立的 SDK `Close` 和 `Cancel` helper；
    - [x] fence 新 gateway operation、drain 已接受的 operation，并在收集最终 artifact 前关闭本地
      Runtime Server；
    - [x] 仅在 Session Run 的 Runtime 配置 ArtifactStore 时提供 `$KRUNTIME_ARTIFACTS_DIR`；校验并上传
      最终文件、在 status 保留 compact ref、在 Finalizing 重试 transient store failure，并确定性地使
      invalid artifact 失败；
    - [x] 增加 successful completion/export、finalization 期间 cancellation、transient store retry、
      invalid artifact 与 Runtime Pod loss 的测试覆盖；
  - [x] 增加 Python 和 Go SDK：create/open/wait/execute/files/logs/close helpers、typed errors、
    direct in-cluster access 与 local port-forward support；
  - [x] 增加 Kubernetes diagnosis agent 示例，使用 Session Run 实现 multi-step scripts、files、
    results 和 cleanup；在 feature 被视为 supported 前用该示例发现剩余 product gaps；
  - [x] 增加 registration、ordering、timeout、cancellation、idle expiry、cleanup、authorization、
    file-boundary enforcement、Runtime Pod loss 和 gateway routing 的 unit、integration 与 E2E
    coverage；
    - [x] 通过 authenticated HTTP gateway 的 E2E 验证每个 Session 的 FIFO mutation ordering；
    - [x] 通过 E2E 验证取消 Session Run 会终止活跃的 gateway command，并拒绝后续 gateway access；
    - [x] 通过 focused unit、integration 与 E2E tests 验证 gateway routing、bearer-token
      authentication、Run authorization、workspace file-boundary enforcement、registration
      environment、structured command logs、idle/total timeout、Drain completion、SDK access，
      以及 assigned Runtime Pod loss。
- [ ] v0.x examples：增加 LLM agent 示例和 workflow 示例，并用这些示例反推缺失的
  产品和 API 能力。
- [x] Workflow data sharing：设计并实现由 workflow demo 反推出的 first-class cross-Run
  storage 语义。目标模型：
  - job 之间通过 ArtifactStore-backed step outputs 和 inputs 传递数据；
  - 同一个 Workflow job 内的 Run-to-Run 数据可以共享 `PersistentWorkspace`；
  - `PersistentWorkspace` 是 namespace-scoped CRD，用来表示 workspace 边界、生命周期、
    status、cleanup policy，以及可选的 Runtime binding；
  - Run affinity/anti-affinity 应贴近 Kubernetes 风格的 affinity 概念，让用户不用理解内部
    sticky keys 也能表达 co-location；
  - scheduler 和 runtimed 必须保持 workflow-agnostic。它们只提供通用 placement 和 workspace
    primitives；Workflow controller 组合这些 primitives 实现 job-local workspace sharing；
  - demo 应驱动实现，并在 API 稳定前持续暴露 gap。
  初始实现 TODO：
  - [x] 增加设计文档，覆盖 API shape、lifecycle、failure modes、cleanup、security 和
    compatibility；
  - [x] 扩展 `Runtime.spec.workspace` 以 inline Kubernetes `VolumeSource` 字段，同时保留
    当前 emptyDir 默认行为；
  - [x] 增加 `PersistentWorkspace` API types、CRD validation、status 和 controller
    skeleton；
  - [x] review Run workspace reference 与 affinity 的专用 API shape，再增加 API skeleton；
  - [x] 为 Run 增加 workspace reference 和 Kubernetes-style Run affinity 字段；
  - [x] 通过经过 review 的 [scheduler framework](design/scheduler-framework.md) 实现
    required/preferred Run affinity，同时在无 capacity 时继续保持 Run Pending；
  - [x] review 并定义 `RuntimePodLocal` binding semantics：不预留 capacity 的 deterministic
    ready-Pod selection、planned path ownership，以及 bound-Pod deletion 后 sticky `Lost` status：
    - [x] review `status.boundPodUID` fencing 修订，避免同名 Pod 重建时静默替换
      RuntimePodLocal workspace；
    - [x] 增加该 status field 并重新生成 CRD；
    - [x] 实现 metadata-only binding：通过稳定 UID 哈希将绑定分散到 ready Runtime Pods，并增加
      Runtime 和 Pod watches；
    - [x] Pod 仅暂时 unavailable 时保留原 binding；当 Pod 名称消失或 UID 改变时，永久转为
      `Lost`；
    - [x] 增加 focused controller 和 API validation coverage。
  - [x] 在不引入 Workflow 概念的前提下增加通用 `Workspace` scheduler Filter plugin：要求
    `Run.spec.workspace` 匹配其 Runtime 和 Bound RuntimePodLocal workspace，并仅保留其
    fenced bound Pod 作为 candidate；unresolved 或 Lost workspace 保持 Pending 并给出清晰信息，
    并在 referenced workspace 变更时唤醒匹配的 Pending Runs。
  - [x] 更新 runtimed workspace preparation 和 cleanup，使其支持被引用的 persistent
    workspace 但不感知 Workflow 语义：只创建 bound workspace directory、保留其内容，并只
    清理 Run-local temporary state。
  - [x] 在 Workflow controller 中组合这些 generic primitives：创建并 owner job-local
    PersistentWorkspace、为每个 child Run 添加 workspace reference 和 bound-Pod placement，并在
    不向 Workflow API 暴露 workspace controls 的情况下呈现 workspace loss。
  - [x] 增加显式 step artifact inputs 和 job-scoped artifact references：将
    `jobs.<job>.artifacts.<name>` stage 到 downstream child Runs，并把 compact child Run
    artifact refs 提升到 Workflow status。
  - [x] 完成 Runtime workspace volume sources、job-local workspace sharing、
    job-to-job artifact passing、Runtime Pod loss、cleanup 和权限边界的 E2E 覆盖：
    - [x] Runtime workspace sources、job-local sharing、job-to-job artifact passing 和
      Runtime Pod loss；
    - [x] 显式删除 cleanup；
    - [x] 自动 TTL cleanup；
    - [x] 权限边界：
      - [x] review `persistentworkspaces/use` authorization contract，以及 direct Run 对不存在
        reference 的行为；
      - [x] 增加带 SubjectAccessReview、已 review failure policy 以及 Helm/TLS installation support 的
        validating admission webhook；
      - [x] 证明 controller 创建的 Workflow child Run 不能绕过 workspace authorization boundary；
      - [x] 增加面向 impersonation 的 integration 和 E2E coverage，覆盖 allow、deny、named-resource
        和 controller-owned case。
  - [x] 将 PersistentWorkspace cleanup 作为单独 review 的 lifecycle slice 实现：active Run
    tracking、`Released` scheduling fence、finalizer-based deletion、仅 runtimed 执行的
    Pod-local directory removal、删除/TTL E2E 覆盖，以及 focused loss controller 覆盖。
- [x] Workflow reuse model：在 Workflow API 稳定前拆分执行实例和可复用定义。目标模型：
  - 将当前表示 execution instance 的 `Workflow` API 替换为 `WorkflowRun`；
  - `WorkflowRun.spec` 只包含 inline `jobs`；`krt workflow trigger` 将 reusable
    Workflow 渲染为 inline execution instance；
  - 新增可复用 `Workflow` CRD，`WorkflowRun` 的 job 可以通过 `uses: <workflow-name>`
    和可选 `with` 调用同 namespace 下的 Workflow；
  - 新增可复用 `Action` CRD，`WorkflowRun` 或 `Workflow` 的 step 可以通过
    `uses: <action-name>` 和可选 `with` 调用同 namespace 下的 Action；
  - 第一版保持 namespace-local 名称引用；在需要 cross-namespace 或 remote references 之前，
    不引入冗长的 `workflowRef` 和 `actionRef` 字段；
  - validation 必须保证清晰的 local shapes：WorkflowRun inline jobs、job `uses` vs
    `steps`、step `uses` vs `run`；
  - Action 在 caller job context 内运行，默认共享 caller job 的 runtime、workspace、
    artifacts 和 environment，除非未来 API 显式 override；
  - reusable Workflow job 拥有自己的 job/workspace/artifact boundary，并通过 inputs、
    outputs 和 artifacts 与 caller 通信；
  - 围绕新的 `WorkflowRun`、`Workflow` 和 `Action` 拆分更新 CRDs、controller
    reconciliation、CLI verbs、docs 和 E2E。
  初始实现 TODO：
  - [x] 增加设计文档，覆盖 API shape、validation、status、component boundaries 和
    breaking-change scope；
  - [x] 增加 `WorkflowRun` API types、CRD validation、status 和 controller skeleton；
  - [x] 将 `Workflow` API types 改为 reusable definitions；
  - [x] 增加 `Action` API types、CRD validation、status 和 controller skeleton；
  - [x] 为 reusable Workflow definitions 和 WorkflowRun skeleton 增加面向 workflow
    语义的 `krt wf` verbs；
  - [x] 更新 CLI verbs 和 docs，使 execution 使用 `WorkflowRun`；
  - [x] 为 inline WorkflowRuns 初始化轻量 `status.jobs[*].pre` 和有序 `steps`；
  - [x] 在 inline execution changes 开始前审计现有 E2E tests，并更新受影响的 cases，
    保证整个实现过程中 `make e2e` 始终可以通过；
  - [x] 实现 ready jobs 的 inline WorkflowRun first-step Run creation；
  - [x] 将 WorkflowRun controller reconciliation 重构为
    load/calculate/apply/patch 结构：每次 reconciliation 默认推导 status，只有 external
    side effects 才建模为 actions；
  - [x] 实现 child Run status observation 和 step status updates；
  - [x] 定义并 review child failure、cancellation、dependency propagation 和
    WorkflowRun terminal-status semantics：failure 后 independent jobs 继续，
    dependency-blocked jobs 为 `Skipped`，WorkflowRun 在 executable jobs settled 后聚合；
  - [x] 实现 observed step success 后的 next-step creation；
  - [x] 根据 observed step states 实现 job terminal-state aggregation；
  - [x] 增加 terminal-status 和 cancellation API prerequisites、重新生成 CRDs，以及 child Run
    patch RBAC；
  - [x] 在创建 child Runs 前校验 inline WorkflowRun job DAG 中的 unknown dependencies
    和 multi-job cycles；
  - [x] 实现 deterministic failed-dependency propagation 到 `JobSkipped`；
  - [x] 实现 WorkflowRun terminal aggregation；
  - [x] 实现 WorkflowRun cancellation propagation；
  - [x] 验证 in-progress inline WorkflowRuns 的 controller restart recovery，包括
    child Run 已创建但 status 尚未持久化的故障窗口；
  - [x] 按照完成 review 的
    [execution-boundary design](design/workflow-job-reuse.md) 实现 job-level reusable
    Workflow calls：
    - [x] review 并批准 direct child WorkflowRun 和 local snapshot model；
    - [x] 删除 root `WorkflowRun.spec.uses`/`with`，并实现 template trigger 到 rendered inline
      WorkflowRun 的创建；
    - [x] 为每个 WorkflowRun 增加包含 local execution spec 和有界 `JobStatus.outputs` 的
      immutable snapshot；
    - [x] 在每个 materialized child WorkflowRun annotation 中保存 frozen source output
      contract；
    - [x] 为 ready job-level calls 创建并观察 child WorkflowRuns，包括 input rendering 和
      output-contract capture；
    - [x] 将 inline 和 child Workflow outputs 投影到有界的
      `WorkflowRun.status.jobs.<job>.outputs`；
    - [x] 验证 child 创建前的 late-binding、child 创建后的 deterministic behavior、restart
      recovery、nested calls、cancellation 和 invalid graphs，包括创建 child 前拒绝
      `A -> B -> A` cycle；
  - [x] 按照已 review 的 [Action Execution](design/workflow-action-execution.md)
    模型实现 step-level Action expansion：
    - [x] 增加 Action call 的 status、immutable snapshot 和 CRD validation shape；
    - [x] 在定义的 execution boundaries 计算 `inputs`、`steps` 和 `jobs` expressions；
    - [x] 将 Action calls materialize 为普通 child Runs，聚合其 terminal states 和
      declared outputs，并在 controller restart 后恢复；
    - [x] 在为受影响目标创建任何 child Run 前，拒绝 nested Action calls、missing Actions、
      invalid input bindings 和 invalid Action output expressions；
  - [x] 增加 E2E 覆盖 inline `WorkflowRun`、reusable Workflow calls、Action calls、
    validation failures、output propagation 和 controller restart recovery。
- [ ] Dashboard：设计并实现只读 web dashboard，类似 Tekton Dashboard，可以按
  namespace 查看 Runs，并检查状态和日志。
  初始实现 TODO：
  - [x] 增加只读 [Dashboard 设计文档](design/dashboard/)，覆盖 scope、architecture、
    RBAC、log access 和 implementation sequence；
  - [ ] review 并定义 v0.x Kubernetes bearer-token login 模型、request-scoped Kubernetes
    clients，以及 local-only kubeconfig proxy 边界；
  - 增加 dashboard backend，提供只读 Kubernetes API access；
  - 实现 Run list/detail APIs，并遵守 namespace-aware RBAC；
  - 通过 backend-controlled 路径代理 Run log tail/follow；
  - 增加只读 frontend views，覆盖 namespace selection、Run lists、Run details、
    conditions、outputs、artifact references 和 logs；
  - 增加可选 Helm installation support 和 E2E smoke coverage。
- [ ] 随着安装面逐步稳定，继续推进供应链、安全、兼容性和运维加固。

### 迈向 v1.0

- 稳定 CRD API。
- [ ] 至少增加一个 secure Session backend，初始目标为 gVisor，用于执行不受信任的
  LLM-generated code。在提供该能力前，定义 Runtime Server contract、isolation boundary、
  resource accounting、networking policy，以及与 Session workspace 和 gateway API 的兼容性。
- [ ] 仅在 Python 3.15 正式镜像可用、且包括 `grpcio` 在内的所有锁定 native dependency
  都发布兼容的 `cp315` wheels 后，才将 Python Runtime base image 恢复升级至受支持的
  Python 3.15 release。以镜像构建和 runtime test 作为升级 gate；不要依赖 slim
  production image 中隐式发生的 source build。
- [ ] 将 `Run.spec.priority` 作为 scheduler API 加入。先 review priority、fairness、aging/starvation、
  namespace isolation、authorization、retry/backoff 和 non-preemption semantics，再用 scheduler-owned
  queue ordering 替换 controller-runtime event ordering，并增加 unit、integration 和 E2E coverage。
- [ ] 支持为 function-mode Run 显式配置 concurrent invocations。保留默认的单个 in-flight invocation，
  定义 per-function concurrency limits、invocation/workspace isolation semantics，并继续执行 Runtime Pod
  capacity enforcement。
- [ ] 设计 persistent per-registration Function worker processes，以降低 Python invocation 的启动开销。
  在替换当前每次 invocation 都启动 subprocess 的模型前，review worker lifecycle、module state、
  cancellation、concurrency、output limits 和 isolation。
- [ ] 在不削弱覆盖的前提下缩短本地和 CI E2E suite 执行时间：识别不必要的串行等待，安全地并行
  隔离的 case，并拆分快速反馈和较慢的 lifecycle coverage，同时保留完整 release gate。
- [ ] Runtime 删除时清理其 runtime maintainer。定义 ownership 和 finalization，避免 orphaned
  maintainer 累积，同时保留仍需要 artifact cleanup 的 Run 的正确性。
- [ ] 增加明确的 `Run` suspend 和 resume 语义。定义 Pending、正在执行的 task、function 和
  Session Run 的 API 与 lifecycle 行为，包括 process、workspace、request 和 timeout accounting
  中哪些保留、哪些丢弃；不能将其建模为 best-effort 的 boolean 开关。
- 定义兼容性和迁移保证。
- 记录弃用策略。
- 明确生产环境的多租户隔离策略。
- 发布稳定的安装和升级指南。

## 开源就绪

详细的开源就绪清单见 [Open Source Readiness Plan](open-source-readiness.md)。

## 发布历史

见 [CHANGELOG.md](https://github.com/kruntimes/kruntimes/blob/main/CHANGELOG.md)
和 [Release Process](release.md)。
