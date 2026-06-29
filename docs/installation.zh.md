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

对于本地集群，请确保 Helm values 中引用的 kruntimes images 在集群内可用。例如，
使用集群可访问的 registry，或通过本地集群工具加载本地 build 的 images。

## Platform Chart

每个集群安装一次 platform chart：

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

platform chart 会安装：

- CRDs，
- controller，
- scheduler，
- platform RBAC，
- metrics Services，
- 可选的 ServiceMonitor。

## Built-In Runtime Chart

将内置 Runtime CRs 安装到需要执行 Runs 的 namespaces：

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace \
  --set bash.image=<bash-runtime-image> \
  --set python.image=<python-runtime-image>
```

## Image 配置

共享环境应使用不可变 image tags 或 digests。除本地开发外，避免使用 mutable tags。

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
