---
title: "测试指南"
---

本指南列出测试套件以及何时运行它们。

## 单元测试

```bash
make test
```

覆盖集成测试和 E2E 测试之外的 Go 包。同时运行生成、格式化、vet 和 protobuf 生成先行
检查。

### 调用方管理的工具与缓存路径

构建工具和下载内容不要求容器根文件系统可写。可通过标准环境变量将它们定向到可写的
workspace：Makefile 会将 `GOBIN` 和 `TMPDIR` 传递给每个工具安装步骤，Go 和 uv 则遵循
各自的缓存环境变量。

```bash
export GOBIN="$PWD/.tools/bin"
export TMPDIR="$PWD/.tmp"
export GOPATH="$PWD/.go"
export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
export UV_CACHE_DIR="$PWD/.cache/uv"
mkdir -p "$GOBIN" "$TMPDIR"
make test
```

这同样适用于 `make proto`、`make test-integration`、`make lint` 和安全检查目标。

## 集成测试

```bash
make test-integration
```

使用 envtest 测试 controller 和 CRD 行为。

使用同一套 envtest 环境运行单个集成测试：

```bash
make test-integration-run INTEGRATION_TEST=TestSessionFilePaginationContract
```

## 竞态检测

```bash
make test-race
```

针对 controller、scheduler、runtimed 和 Bash Runtime 的竞态覆盖。

## Helm 测试

```bash
make test-helm
```

验证 chart lint 检查、模板渲染、多 release 渲染和多 namespace 渲染。

## Python Runtime 测试

```bash
cd runtimes/python
uv sync --frozen
uv run --frozen python -m unittest server_test -v
```

## Python Sandbox SDK 和 Agent Demo

下面的命令运行 SDK lifecycle、local gateway adapter tests 以及 Kubernetes diagnosis agent 的
allowlist tests，不会把它们加入 Runtime 的 locked Python environment：

```bash
make test-sdk-python
UV_CACHE_DIR=/tmp/kruntimes-uv-cache uv build --directory sdk/python
```

## E2E 测试

```bash
make e2e
```

`make e2e` 构建镜像，创建或复用 kind 集群，加载镜像，部署 Helm charts，并运行 E2E 测试。

当变更影响以下内容时使用：

- CRD 行为，
- 调度，
- runtimed 执行，
- Helm 安装路径，
- artifact 存储，
- 基于真实集群的 CLI 行为。

### 用例组织

E2E 用例位于 `test/e2e/`，按职责拆分，而不是集中在一个数千行的文件里：

| 文件 | 职责 |
| --- | --- |
| `main_test.go` | Kubernetes 客户端、镜像引用、功能开关 |
| `runtime_fixture_test.go` | Runtime 池、Pod 就绪、诊断、重启 |
| `wait_test.go` | 条件驱动的等待 |
| `run_assert_test.go` | Run 断言与终止请求 |
| `gateway_fixture_test.go` | Console gateway 与 log API 夹具 |
| `artifact_fixture_test.go` | artifact 存储断言 |
| `runtime_test.go` | Runtime 池与单次 Run 执行 |
| `session_test.go` | Session 模式与 Console gateway |
| `function_test.go` | Function 模式 |
| `workflow_test.go` | Workflow、Action 与可复用 Workflow |
| `artifact_test.go` | artifact 导出与暂存 |
| `workspace_test.go` | PersistentWorkspace |
| `scheduler_test.go` | 调度、容量、亲和性与取消 |
| `diagnosis_test.go` | demo 诊断 Runtime |

用例使用 `GenerateName` 或时间戳后缀创建资源，并通过 `t.Cleanup` 回收，因此不依赖其他用例的执行顺序。唯一长期存在的共享状态是两个预热 Runtime 池（`bash` 与 `python`）：第一个需要它的用例负责创建，后续用例复用，不再重复下发相同的 Runtime 更新并等待 Pod。

### 并行执行

不修改集群级状态的用例会调用 `t.Parallel()`，同时运行的用例数上限为 `E2E_PARALLEL`（默认 `4`）：

```bash
make e2e                        # 默认并发度
E2E_PARALLEL=1 make e2e         # 串行，保持原有顺序
make e2e-test E2E_PARALLEL=8    # 复用已有集群，提高并发
```

以下用例必须串行，因此有意不调用 `t.Parallel()`：

- `TestWorkflowRunRecoversActionAfterControllerRestart` 会重启 controller Deployment，导致其他用例的调谐暂停。
- `TestRuntimeReadyReplicasTracksRuntimedAvailability`、`TestRuntimedRecoversRunningRunAfterRestart` 与 `TestFunctionRunRecoversInvocationAfterRuntimedRestart` 会杀掉 Runtime Pod 中的 `runtimed` 来验证重启恢复。
- `scheduler_test.go` 中的用例断言调度延迟、容量与唤醒顺序，并行负载会干扰测量。
- `TestSessionGatewayServesCertManagerTLS` 在独立的 `e2e-cert-manager-run` / `e2e-console-bounds-run` 任务中运行。

并行度受集群容量限制而非测试本身：每个并行用例都可能创建自己的 Runtime Pod，共享预热池以 8 个并发 Run 槽位创建，避免用例在单个 Pod 上排队；用例自有的 Runtime CR 仍使用产品默认容量。

### 耗时数据

`make e2e-test` 通过 `hack/e2e-timings` 运行用例，在保留原有测试日志的同时额外产出：

- `e2e-timings.json`：每个用例的耗时以及本次运行的并发度，作为 `e2e-timings` CI artifact 上传；
- `e2e-timings.md`：最慢用例表格与基线对比，追加到 workflow run summary。

每次运行会与 `test/e2e/timings-baseline.json` 对比：摘要给出测试阶段 wall clock、用例耗时总和，以及超过 `-regression-percent`（默认 50）与 `-regression-seconds`（默认 5）的用例级变慢。用例级列表仅供参考（并行调度会让单个用例耗时波动），且对比永远不会导致构建失败。只有两次报告记录的并发度相同时才会比较 wall clock。

```bash
go test ./test/e2e/... -json | go run ./hack/e2e-timings -parallel 4 -top 30 -report /tmp/e2e.json
```

在有意调整用例后，可将 CI 产出的 `e2e-timings.json` 复制覆盖 `test/e2e/timings-baseline.json`，并把其中的 `parallelism` 字段更新为本次测量使用的并发度。

### 实测提升

下表数据来自 GitHub 托管运行器上的 `make e2e`，用例集相同（65 个用例，63 个执行，2 个因环境开关跳过）：

| 用例集 | 并发度 | 测试阶段 | `make e2e` 作业 |
| --- | --- | --- | --- |
| 拆分前基线 | 串行 | 435.1s | 13m17s |
| 条件等待 + 共享预热池 | 串行（`E2E_PARALLEL=1`） | 418.0s | 13m21s |
| 条件等待 + 共享预热池 + 并行 | 4 | 180.0s | 10m40s |

条件驱动的等待与共享预热池本身约减少 4% 的测试阶段耗时；安全并行在此基础上再减少 57%，整体测试阶段 wall clock 下降 58.6%。剩余最慢的用例受产品定时器而非测试框架限制：Pod 丢失与 stale Run 用例需要等待 controller 30 秒的 stale 重入间隔，因此无论调度方式都保持在 32-37 秒。镜像构建、kind 集群创建与 Helm 部署耗时不变，占作业其余部分的主要开销。

## 基准测试

```bash
make benchmark
```

基准测试使用 E2E 设置路径，测量调度延迟、吞吐量、Runtime 容量行为和控制平面请求延迟。

详见 [Performance Benchmarks](benchmarks.md)。

## 安全与依赖检查

```bash
make govulncheck
```

安全 workflow 也在 GitHub Actions 中运行定期扫描。

## 添加测试

- 在被变更的包附近添加单元测试。
- 为 controller-runtime、CRD 验证和 admission 行为添加集成测试。
- 为仅在真实集群中出现的行为添加 E2E 测试。
- 当测试覆盖用户可见行为时更新文档。
