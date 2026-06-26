# Open Source Readiness Plan

本文档记录 kruntimes 在公开仓库前需要完成的改进项。目标不是在首次公开前完成
所有生产级能力，而是确保项目具备清晰的法律边界、可信的基本质量、可复现的发布
流程，以及与当前实现一致的功能和安全声明。

## Release Positioning

首次公开建议定位为 `v0.x experimental`：

- API 为 `v1alpha1`，暂不承诺向后兼容。
- 内置 Runtime 仅适用于可信 workload，不提供强安全隔离。
- 默认支持的 Kubernetes 版本、安装作用域和升级边界必须明确记录。
- P0 项全部完成后再将仓库设为 public。

## P0: Public Release Blockers

### 1. Legal and Community Baseline

- [x] 添加开源许可证和必要的版权声明。
- [x] 添加 `SECURITY.md`，说明漏洞报告渠道、响应范围和支持版本。
- [x] 添加 `CONTRIBUTING.md`、`CODE_OF_CONDUCT.md`、`SUPPORT.md`。
- [x] 添加维护者列表、`CODEOWNERS`、Issue 和 PR 模板。
- [ ] 公开后启用 main 分支保护，要求 CI 通过和至少一次 review。

验收标准：

- GitHub 正确识别项目许可证。
- 新贡献者无需私下沟通即可完成开发、测试和提交 PR。
- 安全问题存在非公开报告渠道。

### 2. Required CI

- [x] CI 执行 `make test`、`make test-integration` 和 Helm lint/template。
- [x] CI 执行 Go race detector，至少覆盖 runtime、scheduler、controller 和 runtimed。
- [x] CI 执行 Python Runtime 单元测试。
- [x] 增加 `govulncheck`、依赖更新机器人和基础 secret scanning。
- [x] 增加生成文件一致性检查，确保生成的 Go API 和 CRD 文件保持最新。
- [x] 定期或按发布执行 `make e2e`。

验收标准：

- 所有 required checks 在干净环境中可复现。
- 生成文件漂移、数据竞争和可达漏洞会阻止合并。

### 3. Runtime Concurrency and Resource Lifecycle

当前 Bash Runtime 在执行 goroutine 与 `Status`、`List`、`Cancel`、重复
`Execute` 之间存在真实数据竞争，已由 `go test -race` 复现。

- [x] 为 Bash Runtime 每个 execution 建立完整的并发保护和不可变状态快照。
- [x] 为 Python Runtime 每个 execution 建立完整的并发保护和不可变状态快照。
- [x] 为 Bash Runtime stdout/stderr 设置有界缓冲，禁止无限内存增长。
- [x] 为 Python Runtime stdout/stderr 设置有界缓冲，禁止无限内存增长。
- [x] 为 Runtime API 增加 execution `Forget` 生命周期。
- [x] Run 完成后清理 `/workspace/<runUID>`，同时保留制品上传所需顺序。
- [x] 对 Bash Runtime 的取消和超时终止整个进程组，并等待退出。
- [x] 对 Python Runtime 的取消和超时终止整个进程组，并等待退出。
- [x] 修复 Python Runtime 中共享 task 状态的并发访问。
- [x] 明确 Python handler 模式的隔离方案；在未隔离前标记为 trusted-code only。
- [x] 将 `workspace.sizeLimit` 实际应用到 Runtime Pod 的 `emptyDir`。

验收标准：

- `go test -race` 全部通过。
- 持续执行大量 Run 后，Runtime task 数、workspace 和内存不会单调增长。
- 取消或超时后不存在遗留子进程。

### 4. Workflow State Correctness

- [x] 将 child Run 的 `Timeout` 和 `Cancelled` 转换为终态 Step/Job/Workflow。
- [x] 未知 `needs` 必须在 admission 或 reconcile 阶段明确失败。
- [x] 校验 step 必须包含一个当前支持的执行方式；不能静默接受未实现的 `uses`。
- [x] 为 job/step 名称增加 Kubernetes 名称和 label 约束。
- [x] 生成 child Run 名称时使用截断加稳定 hash，避免超长名称。
- [x] 为以上场景增加 controller 和 E2E 测试。

验收标准：

- 合法 Workflow 最终收敛到 `Succeeded` 或 `Failed`。
- 无效 DAG 在创建或首次 reconcile 时给出明确错误，不会永久 Pending/Running。

### 5. Security and Trust Boundary

- [x] 拒绝绝对路径和包含 `..` 的 entrypoint，确保源码写入不逃逸 Run workspace。
- [x] 限制 Git source 协议，增加 clone/checkout 超时、大小和输出限制。
- [x] 明确 Runtime、Run 创建权限及其安全含义。
- [x] 为 runtimed status/artifact gRPC 增加网络访问控制；至少提供默认 NetworkPolicy。
- [x] 为所有平台和内置 Runtime 容器提供安全的默认 security context。
- [x] 禁止默认 privilege escalation，启用 seccomp，按容器能力设置只读根文件系统。
- [x] 编写 threat model，明确同一 Runtime Pod 内多个 Run 共享进程、网络和 workspace
  所带来的限制。
- [x] 修正 README 中“per-Run resource limits”和“clean workspace”等超出当前实现的声明。

验收标准：

- 文档不会暗示内置 Runtime 能安全运行不可信代码。
- 默认 chart 在启用 Pod Security Standards 的集群中可安装。
- Run 输入不能通过路径或 Git source 访问未授权的本地资源。

### 6. Installation Scope and Helm Correctness

- [x] 明确选择 cluster-wide 或 single-namespace 安装模型。
- [x] Runtime Deployment 引用的 runtimed ServiceAccount 必须存在于 Runtime namespace。
- [x] 为自定义 `Runtime.spec.template.spec.serviceAccountName` 增加 namespace-scoped
  RBAC controller，确保对应 ServiceAccount 拥有 runtimed 所需最小权限。
- [x] 使用 `Runtime.spec.template` (`PodTemplateSpec`) 统一 Runtime Pod 自定义，
  取代 Runtime spec 中重复的 PodSpec-like 字段，并明确 controller 保留字段。
- [x] 所有 Helm 资源名称使用 release fullname，支持多个 release 共存。
- [x] 移除未使用 values，并让 replicas、leader election、ports、imagePullSecrets、
  security context、scheduling constraints 等可配置。
- [x] 避免默认使用 `latest`，镜像 tag 应跟随 chart appVersion。
- [x] 增加多 namespace 模板/安装测试。
- [x] 增加第二个 Helm release 的模板测试。

验收标准：

- 文档声明的 namespace 模型可真实工作。
- 使用自定义 Runtime ServiceAccount 时，权限授予是 namespace-scoped、最小权限且
  可审计的。
- Runtime Pod 自定义模型在公开前有明确边界，不需要在未来兼容两套重叠 API。
- 同一集群可以安装两个无资源命名冲突的 release。
- chart 默认值能够引用公开发布的、不可变版本镜像。

### 7. Artifact Cleanup Ownership

Artifact finalizer 最初由 Runtime Pod 内的 runtimed 执行，导致 Runtime 删除或缩容到零
时无法清理。现在 controller 从 Run status 中读取持久化的 store 配置快照，并按
artifact store hash 确保长期运行的 runtime maintainer Deployment 存在。maintainer 不依赖
当前 Runtime spec 或 Runtime 是否仍存在。

- [x] 将 artifact finalizer 清理迁移到独立、长期存在的 controller，或提供等价的
  central GC。
- [x] 定义 store 配置删除、凭据丢失和外部对象不存在时的恢复语义。
- [x] 对部分上传失败进行回滚，避免孤立对象。
- [x] S3 上传前执行最终存储大小限制，避免先上传再拒绝。
- [x] 增加 finalizer 故障恢复和 Runtime 删除后的 E2E 测试。

验收标准：

- Runtime 不存在时仍能删除带 artifact finalizer 的 Run。
- 临时存储故障恢复后，清理会自动继续且保持幂等。

### 8. Supply Chain and Known Vulnerabilities

审查时 `govulncheck` 检测到 5 个可达漏洞：

- Go `1.26.3` 标准库问题，修复版本为 `1.26.4`。
- `golang.org/x/net v0.49.0` 问题，需升级到至少 `v0.55.0`。

改进项：

- [x] 升级 Go toolchain 和受影响依赖，并让 `govulncheck` 通过。
- [x] Docker 基础镜像固定到明确 patch version 和 digest。
- [x] Python 镜像通过 `uv.lock` 安装依赖，不直接安装未固定版本。
- [x] 固定 Makefile 中 controller-gen、setup-envtest、golangci-lint、
  protoc plugins 和 uv 的版本。
- [x] 发布镜像生成 SBOM，并使用 cosign 或等价机制签名。

验收标准：

- 发布分支不存在已知可达的 high/critical 漏洞。
- 构建工具、基础镜像和语言依赖均可复现。

## P1: Correctness and Operability

- [x] Stale reaper 同时检查 Kubernetes `PodReady` 和
  `kruntimes.io/RuntimedReady` heartbeat。
- [x] Stale reaper 返回 status update 错误，并统一 terminal condition 更新。
- [x] 修复 scheduler 在检查 `NewManager` 错误前使用 `mgr` 的初始化顺序。
- [x] runtimed 主动区分 transient Status 错误和 execution `NotFound`。
- [x] `krt run --wait` 正确处理 `Timeout` 和 `Cancelled`。
- [x] `krt logs` 同时处理 stdout/stderr，避免 stderr 重复和 offset 越界。
- [x] CLI 使用 Cobra command context，支持 kubeconfig/context、当前 namespace 和
  `json`/`yaml` 输出。
- [x] 增加真实 readiness check，而不是固定返回成功。
- [x] 为 CRD 增加 CEL 校验和字段大小限制。
- [x] 为 Run/Workflow conditions 使用 Kubernetes list-map markers。
- [x] 补齐 queue time、dispatch latency、retry、failure 和 active Run metrics。
- [x] Chart 创建 metrics Service，并提供可选 ServiceMonitor。

## P2: Release and Contributor Experience

- [x] 建立 SemVer、CHANGELOG 和 release notes 流程。
- [x] 发布 scheduler/controller/runtimed/runtime 镜像。
- [ ] 发布 Helm OCI chart。
- [x] 发布 `krt` 多平台二进制、checksum 和 provenance。
- [x] 增加 Kubernetes/Helm/Go/Python 兼容性矩阵。
- [x] 增加安装、升级、卸载、故障排查和备份恢复文档。
- [x] 增加自定义 Runtime 开发指南和协议兼容性约定。
- [ ] 增加性能基准：调度延迟、吞吐量、Runtime 容量和控制面负载。
- [ ] 校准 README roadmap，只将有实现、测试和可用安装路径的能力标为完成。

## Suggested PR Sequence

1. **Repository baseline**: license、community files、GitHub templates、基础 CI。
2. **Runtime safety**: race、进程组取消、有界输出、execution/workspace cleanup。
3. **Workflow correctness**: terminal propagation、DAG validation、名称约束。
4. **Input security**: entrypoint、Git source、trusted workload 文档。
5. **Helm scope**: namespace 模型、资源命名、ServiceAccount、security defaults。
6. **Artifact lifecycle**: central finalizer controller、rollback、恢复测试。
7. **Supply chain**: toolchain/依赖升级、镜像 pin、SBOM 和签名。
8. **CLI and observability**: terminal handling、logs、metrics、readiness。
9. **Release automation and docs**: images、chart、CLI、compatibility、operations。

其中第 2、3、4 项可以在明确 API 依赖后并行；第 5 和第 6 项需要先确定安装作用域及
artifact store 的配置所有权。

## Audit Evidence

本次审查执行结果：

- `make test`: 通过。
- `make test-integration`: 通过。
- Helm lint: 通过。
- Python Runtime unit tests: 通过。
- `go test -race`: 失败，复现 Bash Runtime 并发读写。
- `make govulncheck`: 通过。
- 完整 `make e2e`: 本次审查未执行。
