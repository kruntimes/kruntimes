---
title: "项目概览"
---

kruntimes 是一个 Kubernetes-native execution engine，用于在预热的
Runtime Pod 池上运行 serverless functions、CI pipelines、包含 AI workload
的 batch workloads，以及 AI agent tasks/sandboxes。它不会为每次执行创建新
Pod，而是复用热 Runtime Pods，并在应用层完成细粒度调度，从而在不修改
Kubernetes 内部机制的前提下降低启动延迟和运维复杂度。

## 动机

Kubernetes 是运行服务和长生命周期作业的强大基础设施，但原生 Kubernetes
本身并不是一个完整的低延迟 serverless 或 batch execution engine。
Serverless、CI、automation 和 agent workload 往往都会经历同一条请求时
Pod 启动路径：

1. 创建 Pod，
2. 等待 Kubernetes 调度，
3. 拉取或解包镜像，
4. 创建容器，
5. 初始化网络，
6. 等待 readiness，
7. 最后才运行用户 workload。

当 workload 生命周期很短、并发很高、对延迟敏感，或属于更大 batch pipeline
的一部分时，这条路径的成本会非常明显。

基础设施层面的优化可以降低其中一部分成本，但通常要求团队拥有 cluster
scheduler、node runtime、image distribution、CNI 或 sandboxing layer 的控制权，
并且能够长期承担优化和维护这些层的工程成本。kruntimes 面向需要更快执行语义、
同时希望保持在 Kubernetes 内部机制之上的平台团队。

## 方案

kruntimes 使用两层调度：

| 层级 | 职责 | 频率 |
| --- | --- | --- |
| Kubernetes | 调度并维持 Runtime Pod 池。 | 粗粒度 |
| kruntimes | 将单个 Run 分配到预热的 Runtime Pod。 | 细粒度 |

Kubernetes API 仍然通过 CRD 作为持久控制平面。Runtime Pods 持有本地执行
capacity，并运行一个 `runtimed` sidecar，与本地 Runtime Server 协作。

这是一种 hierarchical scheduling：Kubernetes 为预热 Runtime pools 做粗粒度放置，
kruntimes 在这些 pools 内做细粒度 Run 放置。这个模型支持低延迟 serverless-style
执行和高性能 batch workloads，而不需要为每次执行创建一个 Pod。

## 核心价值

- 避免请求时创建 Pod。
- 让执行环境保持预热。
- 通过 CRDs、Helm、RBAC、metrics 和 status conditions 保持 Kubernetes-native 运维体验。
- 让团队在 Kubernetes 之上构建低延迟执行能力，而不需要接管并长期维护 cluster
  scheduler、image distribution、CNI、CRI 或 sandboxing layer 的自定义行为。
- 支持 hierarchical scheduling，适合既需要 Kubernetes-level pool management，
  又需要快速 application-level dispatch 的 workloads。

## 使用场景

- AI agent tools 和 code execution。
- AI agent tasks 和 sandboxes。
- 可信 serverless workloads。
- CI/CD 和 automation tasks。
- 高并发短生命周期脚本。
- 受益于 hierarchical scheduling 的高性能 batch workloads。
- 面向专用执行环境的自定义 Runtime pools。

## 当前限制

- API 是 `v1alpha1`，仍处于 experimental 阶段。
- 内置 Bash 和 Python runtimes 仅适用于 trusted code。
- 内置 runtimes 不提供 per-Run 进程、网络、文件系统、CPU 或内存隔离。
- 大量日志和 artifacts 存储在 etcd 之外；Run status 只保留紧凑 metadata 和 references。

信任边界见 [Security and Threat Model](security.md)。
