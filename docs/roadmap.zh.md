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
- [ ] Agent sandbox 所需的 Function-mode Runs：定义 mutually exclusive 的
  `Run.spec.mode.task` 和 `Run.spec.mode.function` 语义，让 function Run 可以 reserve
  预热 Runtime Pod，向 runtimed/runtime-server 注册 callable function，保持 ready 状态
  以支持多次低延迟 invocation，并在删除或 idle timeout 时释放 reservation。
  Function-mode Runs 仍然遵守普通 Runtime capacity，因此当 capacity 允许时，多个
  function Runs 可以共享同一个 Runtime Pod。这个能力应该走 dataplane invoke path，
  而不是为每次 invocation 创建 Kubernetes object。
- [ ] Runtime gateway invoke path：为每个 Runtime 创建一个 gateway Service，把这个
  Service 作为稳定的 Run invoke endpoint，将请求路由到拥有 assigned Runtime Pod 的
  runtimed，并在 invoke path 上依赖 runtimed 的内存 ownership/readiness cache，而不是
  同步读取 Kubernetes API。
- [ ] Function-mode API cleanup：在 API 稳定前将现有 top-level `Run.spec.handler`
  字段迁移到 `Run.spec.mode.function.handler`。Handler 仍然是有用的 FaaS 概念，但它
  不应该和 task-only 的 `entrypoint`、`args` 字段一起放在顶层 Run spec。
- [ ] Function-mode runtime contract：增加 runtime-server register、invoke 和
  unregister APIs；定义有界 invoke request inputs、response outputs、artifact
  references 和 log access，同时避免把高频 invocation history 写入 Run status。
- [ ] Function-mode reliability and isolation：覆盖 function registration、ready
  status、local/proxied invoke、多次 invocation、artifact reuse、idle timeout、
  explicit release、runtime pod restart recovery、cleanup、service account 选择、
  runtime pod security context、resource limits、network policy guidance，以及未来
  gVisor、Kata、Firecracker 等更强 runtime。
- [ ] Agent sandbox SDKs：为 agent 开发者提供 first-class SDK，优先支持 Python 和
  Go。SDK 应暴露 sandbox-facing 语义，例如 create/open/reattach/disconnect/terminate、
  command 或 tool execution、file operations、logs、artifacts 和 identity metadata；
  默认隐藏底层 function-mode Run，除非调用方请求低层 metadata。SDK 还应隐藏 readiness
  polling、gateway discovery、本地开发的 port-forward fallback、in-cluster direct URLs、
  有界 outputs/artifacts、timeouts、幂等操作 retries、typed errors，并提供 guardrails，
  建议或验证 AgentSandbox-style integration 使用每个 Runtime Pod 只承载一个 Run 的配置。
- [ ] Agent sandbox workspace and file APIs：定义 agent 如何上传生成的 scripts 或
  inputs、读取文件、列出 workspace 内容、获取 artifacts、stream 或 retrieve logs，并且
  不把每个操作都变成 Kubernetes reconciliation loop。
- [ ] Agent framework integration：为 agent frameworks 和 MCP-style tool servers 设计
  一层轻量集成，使 tool call 可以 acquire 或 reuse 由 function-mode Run 支撑的 sandbox
  handle，通过 gateway invoke，返回结构化结果，并按照 agent session policy cleanup、
  disconnect、reattach 或 preserve sandbox。
- [ ] Agent sandbox identity and connectivity：文档化并实现 stable Run identity、
  gateway addressing、in-cluster/external access、service account/RBAC 边界、network
  policy 和 multi-tenant naming 模型，使 agent platform 可以安全地把 sandbox handle
  交给 sub-agents。
- [ ] v0.x examples：增加 LLM agent 示例和 workflow 示例，并用这些示例反推缺失的
  产品和 API 能力。
- [ ] Workflow data sharing：设计并实现由 workflow demo 反推出的 first-class cross-Run
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
  - 扩展 `Runtime.spec.workspace` 以支持 Kubernetes `VolumeSource`，同时保留当前 emptyDir
    默认行为和 sizeLimit 语义；
  - 增加 `PersistentWorkspace` API types、CRD validation、status 和 controller；
  - 为 Run 增加 workspace reference 和 Kubernetes-style Run affinity 字段；
  - 更新 scheduler placement，使其支持 required/preferred Run affinity，同时在无 capacity
    时继续保持 Run Pending；
  - 更新 runtimed workspace preparation 和 cleanup，使其支持被引用的 persistent workspace，
    但不感知 Workflow 语义；
  - 将 child Run artifact refs 提升到 Workflow status，并增加显式 step artifact inputs；
  - 增加 E2E 覆盖 Runtime workspace volume sources、job-local workspace sharing、
    job-to-job artifact passing、Runtime Pod loss、cleanup 和权限边界。
- [ ] Workflow reuse model：在 Workflow API 稳定前拆分执行实例和可复用定义。目标模型：
  - 将当前表示 execution instance 的 `Workflow` API 替换为 `WorkflowRun`；
  - `WorkflowRun.spec` 支持 inline `jobs`，也支持 top-level `uses` 加 `with` inputs；
  - 新增可复用 `Workflow` CRD，`WorkflowRun` 的 job 可以通过 `uses: <workflow-name>`
    和可选 `with` 调用同 namespace 下的 Workflow；
  - 新增可复用 `Action` CRD，`WorkflowRun` 或 `Workflow` 的 step 可以通过
    `uses: <action-name>` 和可选 `with` 调用同 namespace 下的 Action；
  - 第一版保持 namespace-local 名称引用；在需要 cross-namespace 或 remote references 之前，
    不引入冗长的 `workflowRef` 和 `actionRef` 字段；
  - validation 必须保证互斥 shape：top-level `uses` vs inline `jobs`、job `uses`
    vs `steps`、step `uses` vs `run`；
  - Action 在 caller job context 内运行，默认共享 caller job 的 runtime、workspace、
    artifacts 和 environment，除非未来 API 显式 override；
  - reusable Workflow job 拥有自己的 job/workspace/artifact boundary，并通过 inputs、
    outputs 和 artifacts 与 caller 通信；
  - 围绕新的 `WorkflowRun`、`Workflow` 和 `Action` 拆分更新 CRDs、controller
    reconciliation、CLI verbs、docs 和 E2E。
- [ ] Dashboard：设计并实现只读 web dashboard，类似 Tekton Dashboard，可以按
  namespace 查看 Runs，并检查状态和日志。
- [ ] 随着安装面逐步稳定，继续推进供应链、安全、兼容性和运维加固。

### 迈向 v1.0

- 稳定 CRD API。
- 定义兼容性和迁移保证。
- 记录弃用策略。
- 明确生产环境的多租户隔离策略。
- 发布稳定的安装和升级指南。

## 开源就绪

详细的开源就绪清单见 [Open Source Readiness Plan](open-source-readiness.md)。

## 发布历史

见 [CHANGELOG.md](https://github.com/kruntimes/kruntimes/blob/main/CHANGELOG.md)
和 [Release Process](release.md)。
