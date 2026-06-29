---
title: "兼容性矩阵"
---

kruntimes 目前是一个 `v0.x` 实验性项目，使用 `v1alpha1` API。此矩阵记录了经过刻意测试
或用于发布产物的版本。超出这些范围的可能也能工作，但不属于当前公开的兼容性声明。

## 策略

- 兼容性声明通过正常的 PR 更新。
- 在 API 仍处于实验性阶段时，次版本号发布可能会更改支持的版本。
- 除非 CI、发布工作流或有文档记录的手动验证覆盖了某个 Kubernetes、Helm、Go 或 Python
  版本，否则发布不应声明支持该版本。

## Kubernetes

| 范围 | 版本 | 状态 | 证据 |
| --- | --- | --- | --- |
| API/controller 集成测试 | `1.32` | 已测试 | 集成测试工作流中的 `ENVTEST_K8S_VERSION = 1.32`。 |
| E2E 集群 | kind 默认 Kubernetes 版本 | 在公开 release tag 前测试 | E2E 工作流创建或复用 `kruntimes-e2e` kind 集群。 |
| 更新的 Kubernetes 次版本 | 未认证 | 尽力而为 | 项目使用 `k8s.io/* v0.36.x` 的 Kubernetes 客户端库，但更新的 API server 版本在被记录为支持之前需要显式验证。 |

## Helm

| 范围 | 版本 | 状态 | 证据 |
| --- | --- | --- | --- |
| Helm chart 渲染 | Helm 3 | 必需 | Charts 使用 `apiVersion: v2`；chart 验证运行 `helm lint` 和 `helm template`。 |
| 多 release 和多 namespace 安装 | Helm 3 | 已测试 | `hack/verify-helm-multi-release.py` 和 `hack/verify-helm-multi-namespace.py`。 |
| Helm OCI chart 发布 | Helm 3 OCI registry 支持 | 由 `Release Charts` 发布 | Charts 被打包并推送到 `oci://ghcr.io/<owner>/charts`。 |

## Go

| 范围 | 版本 | 状态 | 证据 |
| --- | --- | --- | --- |
| 模块工具链 | `1.26.4` | 必需 | `go.mod` 的 `go` 指令。 |
| Docker 镜像构建 | `1.26.4` | 必需 | 项目 Dockerfiles 中的 Go builder 镜像。 |
| 本地生成工具 | 锁定在 `Makefile` 中 | 必需 | `controller-gen`、`setup-envtest`、`golangci-lint`、`govulncheck`、`protoc` 和 proto 插件在使用前进行版本检查。 |

## Python

| 范围 | 版本 | 状态 | 证据 |
| --- | --- | --- | --- |
| Python Runtime 发布镜像 | `3.14.6-slim-trixie` | 必需 | `Dockerfile.python-runtime`。 |
| Python Runtime 包下限 | `>=3.12` | 必需 | `runtimes/python/pyproject.toml`。 |
| Python Runtime 单元测试 | `3.12` | 已测试 | CI 使用 `astral-sh/setup-uv` 并设置 `python-version: "3.12"`。 |
| 依赖锁文件 | `uv.lock` | 必需 | Docker 构建使用 `uv sync --locked`。 |

## krt CLI 发布产物

| 平台 | 架构 | 状态 |
| --- | --- | --- |
| Linux | `amd64`、`arm64` | 由 `Release CLI` 发布。 |
| macOS | `amd64`、`arm64` | 由 `Release CLI` 发布。 |
| Windows | `amd64` | 由 `Release CLI` 发布。 |

每个 CLI 归档文件附有校验和文件和 GitHub artifact provenance attestation。
验证命令见 `docs/release.md`。
