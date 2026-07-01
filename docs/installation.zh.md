---
title: "安装"
---

kruntimes 使用 Helm 安装。当前模型是 cluster-wide platform installation 加上
namespace-local Runtime definitions。

## 要求

- 支持 CRD 的 Kubernetes 集群。
- Helm 3。
- kubectl 已配置到目标集群。
- 集群能够拉取的 kruntimes images。

项目明确测试过的版本见 [Compatibility Matrix](compatibility.md)。

## Kubernetes 集群

kruntimes 运行在 Kubernetes 上。请从一个你能够管理的集群开始：

- 生产或共享 Kubernetes 集群，或
- 本地集群，例如 [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) 或
  [minikube](https://minikube.sigs.k8s.io/docs/start/)。

按照集群提供方的 setup guide 完成准备后，验证访问：

```bash
kubectl cluster-info
```

发布版 charts 默认使用 `ghcr.io/kruntimes/` 下的镜像。对于使用本地 build 镜像的本地
集群，请用集群内可访问的 tags 覆盖 image values。

## Platform Chart

每个集群安装一次 platform chart：

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace
```

platform chart 会安装：

- CRDs，
- controller，
- scheduler，
- platform RBAC，
- metrics Services，
- 可选的 ServiceMonitor。

## krt CLI

基础 Kubernetes 操作不强制依赖 `krt` CLI，但它是查看 Run logs、下载 artifacts、取消
Runs、跟踪 Run status 的最直接方式。端到端 demo 会使用 `krt logs`，同时也提供等价
的 `kubectl` 命令。

在 Linux 或 macOS 上安装发布的 CLI archive：

```bash
KRUNTIMES_VERSION=0.0.3
OS="$(uname | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "${ARCH}" in
  x86_64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

curl -L -o /tmp/krt.tar.gz \
  "https://github.com/kruntimes/kruntimes/releases/download/v${KRUNTIMES_VERSION}/krt_v${KRUNTIMES_VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf /tmp/krt.tar.gz -C /tmp
sudo install /tmp/krt /usr/local/bin/krt
krt version
krt --help
```

Windows 用户可以从 GitHub release 页面下载
`krt_v${KRUNTIMES_VERSION}_windows_amd64.tar.gz`，并将 `krt.exe` 放到 `PATH` 中。

Checksum 和 provenance verification 见 [Release Process](release.md#krt-cli)。

## Built-In Runtime Chart

将内置 Runtime CRs 安装到需要执行 Runs 的 namespaces：

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace
```

## Image 配置

共享环境应使用不可变 image tags 或 digests。除本地开发外，避免使用 mutable tags。

只有在使用自定义或本地 build 镜像时才覆盖 chart image values：

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

## 多 Release

Helm charts 使用 release fullnames 生成资源名。多 release 和多 namespace 渲染由
chart tests 覆盖。

## 卸载

先删除 Runtime releases，再删除 platform：

```bash
helm uninstall kruntimes-runtimes --namespace default
helm uninstall kruntimes --namespace kruntimes-system
```

升级、备份、恢复和故障排查流程见 [Operations Guide](operations.md)。
