---
title: "故障排查"
---

本指南涵盖常见故障及应首先运行的命令。

## Run 一直处于 Pending 状态

检查 Run：

```bash
kubectl get run <name> -o yaml
```

常见原因：

- 没有匹配 `spec.runtime` 的 Runtime，
- Run namespace 中没有 Runtime Pods，
- Runtime Pods 不是 `Ready`，
- runtimed 心跳缺失或过时，
- 所有 Runtime Pods 均处于容量上限。

检查 Runtime Pods：

```bash
kubectl get pods -l runtime=<runtime-name>
kubectl describe pod -l runtime=<runtime-name>
```

## Run 处于 Scheduled 状态但未 Running

检查分配的 Pod 和 runtimed 日志：

```bash
kubectl get run <name> -o jsonpath='{.status.assignedPod}'
kubectl logs <assigned-pod> -c runtimed
```

常见原因：

- runtimed 无法认领 Run，
- Runtime Server 无法连接，
- Runtime Server 返回了临时错误，
- workspace 准备失败。

## Runtime Pods 未变为 Ready

```bash
kubectl get runtime <name> -o yaml
kubectl get deploy,pods -l runtime=<name>
kubectl describe pod -l runtime=<name>
```

常见原因：

- runtime 镜像无法拉取，
- 容器端口与 Runtime Server 端口不匹配，
- readiness 或 runtimed 心跳失败，
- 自定义 ServiceAccount 缺少所需权限。

## 本地集群中的 Image Pull Backoff

如果本地 kind 或 minikube 集群显示 `ImagePullBackOff`，确认 Runtime 镜像引用与集群
可用的镜像匹配：

```bash
kubectl describe pod <runtime-pod>
```

对于本地集群，使用集群工具加载本地构建的镜像，或配置 Helm values 使用集群可拉取的
registry 中的镜像。

## Artifact 清理卡住

```bash
kubectl get run <name> -o yaml
kubectl get deploy -l kruntimes.io/runtime-maintainer=true
kubectl logs deploy/<runtime-maintainer-deploy>
```

常见原因：

- artifact store 凭据被删除，
- 外部 object store 不可用，
- 旧 Run 状态缺少持久的 artifact store 快照。

清理设计为幂等的，可在临时故障后恢复。

## krt 无法读取日志或 Artifacts

检查 Runtime namespace 中 `pods/portforward` 的 RBAC 权限。日志和 artifact 访问可能
需要对分配的 Runtime Pod 或 runtime maintainer service 的 port-forward 权限。

## Helm 安装失败

本地渲染 manifests：

```bash
helm template kruntimes ./charts/kruntimes --namespace <namespace>
```

贡献者可从仓库运行 chart 验证；详见 [Testing Guide](testing.md)。

## 测试后生成文件发生变更

生成文件工作流是贡献者任务。代码生成命令和提交预期见
[Development Guide](development.md)。

## 需要更多帮助

支持渠道和预期见 [SUPPORT.md](https://github.com/kruntimes/kruntimes/blob/main/SUPPORT.md)。
