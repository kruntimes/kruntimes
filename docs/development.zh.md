---
title: "开发指南"
---

本指南涵盖面向贡献者的本地开发。

## 环境要求

- `go.mod` 中指定的 Go 版本
- Docker 或兼容的容器工具
- Helm 3
- kubectl
- kind
- Python 3.12+ 和 uv（用于 Python Runtime 开发）

锁定的工具版本在 `Makefile` 中声明。Make targets 在使用前安装或验证本地工具。

## 仓库设置

```bash
git clone https://github.com/kruntimes/kruntimes.git
cd kruntimes
make test
```

## 生成代码和 CRD

```bash
make generate manifests
```

生成文件必须保持最新。如果生成更改了已跟踪的文件，CI 会失败。

## 构建二进制文件

```bash
make build
```

单独构建目标：

```bash
make build-scheduler
make build-controller
make build-runtimed
make build-cli
make build-bash-runtime
```

## 构建镜像

```bash
make docker-build
```

覆盖镜像名称：

```bash
IMG_CONTROLLER=ghcr.io/example/controller:dev make docker-build-controller
```

## 本地 E2E 环境

```bash
make e2e-setup
make e2e-test
```

或一次性运行两者：

```bash
make e2e
```

清理：

```bash
make e2e-cleanup
```

## Python Runtime

```bash
cd runtimes/python
uv sync --frozen
uv run --frozen python -m unittest server_test -v
```

从仓库根目录重新生成 Python protobuf stubs：

```bash
make proto-python
```

## 贡献流程

1. 对于非平凡的变更，先创建或引用一个 issue。
2. 创建分支。
3. 保持变更聚焦。
4. 运行相关测试。
5. 使用模板创建 PR。

请阅读 [CONTRIBUTING.md](https://github.com/kruntimes/kruntimes/blob/main/CONTRIBUTING.md)、
[GOVERNANCE.md](https://github.com/kruntimes/kruntimes/blob/main/GOVERNANCE.md)
和 [CODE_OF_CONDUCT.md](https://github.com/kruntimes/kruntimes/blob/main/CODE_OF_CONDUCT.md)。

## API 兼容性

项目目前处于实验性阶段。即便如此，公共 API 和 CRD 的变更应包括：

- 生成的 CRD 更新，
- 测试，
- 文档更新，
- 对用户可见的变更需包含 changelog 条目。
