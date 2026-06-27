---
title: "FAQ"
---

## kruntimes 是 Kubernetes scheduler 的替代品吗？

不是。Kubernetes 仍然负责调度 Runtime Pods。kruntimes 负责在这些预热的 Runtime Pods
内部调度单个 Runs。

## kruntimes 会为每个 Run 创建一个 Pod 吗？

不会。Run 是一个 CRD object。scheduler 会把它分配到已有且有可用 capacity 的 Runtime Pod。

## 内置 runtimes 可以安全运行不可信代码吗？

不可以。内置 Bash 和 Python runtimes 仅适用于 trusted code。它们不提供 per-Run 进程、
文件系统、网络、CPU、内存或 ServiceAccount 隔离。

对于不可信 workloads，请使用 namespace 隔离，或实现具备更强隔离能力的 custom runtimes。

## Logs 存在哪里？

完整 stdout 和 stderr 会作为带 Run UID 的结构化 runtimed logs 输出。它们不会被完整复制到
`Run.status.message`。

## Artifacts 存在哪里？

Artifacts 写入 `$KRUNTIME_ARTIFACTS_DIR`，并通过配置的 ArtifactStore 持久化到 etcd 之外。
`Run.status.artifactRefs` 存储紧凑 metadata。

## 为什么 Run 一直 Pending？

通常是因为没有匹配的健康 Runtime Pod 具备可用 capacity。见
[Troubleshooting](troubleshooting.md)。

## Timeout 时会发生什么？

Timeout 会以 `Timeout` 终态 phase 结束。它不会被报告为泛化的 `Failed`。

## kruntimes 提供什么执行保证？

执行语义是 at-least-once。Runtime Servers 必须让重复 `Execute` delivery 具备确定性且安全。

## Runtime 可以使用自定义 ServiceAccount 吗？

可以。设置 `Runtime.spec.template.spec.serviceAccountName`。Runtime controller 会为 runtimed
权限创建 namespace-scoped RBAC。

## API 稳定了吗？

还没有。项目处于 `v0.x experimental`，CRDs 是 `v1alpha1`。

## 如何 benchmark scheduler？

见 [Performance Benchmarks](benchmarks.md)。benchmark execution 是贡献者 workflow，
并假设存在本地开发环境。
