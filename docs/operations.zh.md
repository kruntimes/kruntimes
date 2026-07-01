---
title: "运维指南"
---

本指南涵盖当前 cluster-wide kruntimes 安装模式的 day-two 运维操作。

kruntimes 目前仍是 `v0.x experimental`，使用 `v1alpha1` API。升级前请备份 manifests
和 values，并阅读发布说明中的 breaking changes。

## 安装

每个集群安装一次 platform chart。它安装 CRDs、scheduler、controller、集群级别的 RBAC、
metrics Services 和可选的监控资源。

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace
```

将内置 Runtime CRs 安装到每个应托管 Runtime Pods 和 Runs 的 namespace：

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace
```

共享集群请使用显式 image tags 或 digests。除开发外，不要依赖可变的本地镜像名。只有
在使用自定义或本地 build 镜像时才覆盖 chart image values。

## 升级

升级前：

1. 阅读发布说明和 `CHANGELOG.md`。
2. 检查 `docs/compatibility.md` 了解 Kubernetes、Helm、Go、Python 和 CLI 的兼容性变更。
3. 备份 Helm values 和 kruntimes custom resources。
4. 如果从源码构建，运行发布预检。

先升级 platform 以使 CRDs 和 controllers 准备好应对 Runtime schema 或行为变更：

```bash
helm upgrade kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --reuse-values
```

然后升级各 workload namespace 中的 Runtime definitions：

```bash
helm upgrade kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --reuse-values
```

升级后，验证控制平面和 Runtime Pods：

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,runs -A
kubectl get pods -A -l runtime
```

## 卸载

先移除 workload namespace 中的内置 Runtime releases，再移除 platform：

```bash
helm uninstall kruntimes-runtimes --namespace default --ignore-not-found
helm uninstall kruntimes --namespace kruntimes-system --ignore-not-found
```

Platform 卸载默认不删除 CRDs。这是有意为之：删除 CRDs 会删除集群中所有的 `Runtime`、
`Run` 和 `Workflow` 对象。仅在备份或有意丢弃这些对象后才移除 CRDs：

```bash
kubectl delete crd runs.kruntimes.io runtimes.kruntimes.io workflows.kruntimes.io
```

外部 artifact stores、object buckets、PVC 内容和外部日志系统不会被 Helm 卸载删除。

## 备份

至少备份以下内容：

- Platform 和 Runtime charts 的 Helm release values。
- 你打算恢复的 namespace 中的 `Runtime`、`Run` 和 `Workflow` custom resources。
- Runtime artifact stores 或自定义 Runtime Pod 模板引用的 Secrets。
- Artifact stores 使用的 PVCs 或 object storage buckets。

Kubernetes 对象备份示例：

```bash
kubectl get runtime,runs,workflows -A -o yaml > kruntimes-objects.yaml
helm get values kruntimes -n kruntimes-system --all > kruntimes-values.yaml
helm get values kruntimes-runtimes -n default --all > kruntimes-runtimes-values.yaml
```

Artifact 和日志备份依赖于具体后端。对于文件系统 artifact stores，备份引用的 PVCs。
对于 S3-compatible stores，按供应商流程备份 bucket 或 prefix。kruntimes 不管理
日志采集存储。

## 恢复

按以下顺序恢复：

1. 恢复外部依赖，如 Secrets、PVCs、object buckets 和日志后端。
2. 安装或升级 platform chart。
3. 恢复 `Runtime` 对象和 Runtime chart releases。
4. 仅在引用的 Runtime 和 artifact store 配置可用时才恢复 `Run` 和 `Workflow` 对象。

Artifact 清理使用持久化的 `Run.status.artifactStore` 快照。对于没有此快照的旧 Runs，
controller 可能需要原始 Runtime artifact store 配置才能继续清理。

## 故障排查

### Run 一直处于 Pending 状态

检查同一 namespace 中是否存在具有所请求名称的 Runtime，并且其 Pods 是否就绪：

```bash
kubectl get runtime,pods -n <namespace>
kubectl describe run <run> -n <namespace>
```

调度器仅分配到 Kubernetes Ready、`kruntimes.io/RuntimedReady` 且低于配置容量的
Runtime Pods。

### Run 处于 Scheduled 状态但未 Running

检查分配的 Pod 和 runtimed 日志：

```bash
kubectl get run <run> -n <namespace> -o yaml
kubectl logs <runtime-pod> -n <namespace> -c runtimed
```

如果 Runtime Pod 已消失，重试行为取决于 `Run.spec.retry`。

### Runtime Pods 未变为 Ready

检查 Runtime、生成的 Deployment 和 Pod events：

```bash
kubectl describe runtime <runtime> -n <namespace>
kubectl describe deploy runtime-<runtime> -n <namespace>
kubectl describe pod <runtime-pod> -n <namespace>
```

常见原因包括镜像拉取失败、资源不足、Secret 缺失、无效的 Pod 模板字段或 Runtime Server
不响应健康检查。

### Artifact 清理卡住

检查 Run finalizers、存储的 artifact 配置和 runtime maintainer Deployment：

```bash
kubectl get run <run> -n <namespace> -o yaml
kubectl get deploy,pods -n <namespace> -l app.kubernetes.io/component=runtime-maintainer
kubectl logs deploy/<runtime-maintainer-deployment> -n <namespace>
```

缺失 artifact 凭据、删除的 PVCs、删除的 buckets 或不可用的 S3 endpoints 可能会阻止
finalizer 清理，直到依赖项恢复。

### krt 无法读取日志或 artifacts

`krt logs` 和 artifact 下载使用 Kubernetes port-forwarding 到 Runtime Pod。
用户需要权限来获取 Runs 和 Pods，并在 namespace 中创建 `pods/portforward`。

```bash
kubectl auth can-i get runs -n <namespace>
kubectl auth can-i get pods -n <namespace>
kubectl auth can-i create pods/portforward -n <namespace>
```
