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

## 集成测试

```bash
make test-integration
```

使用 envtest 测试 controller 和 CRD 行为。

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
