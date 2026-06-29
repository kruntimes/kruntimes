---
title: "架构"
---

kruntimes 将 Kubernetes 层面的容量管理与请求层面的 Run 分配分离。

## 组件

| 组件 | 职责 |
| --- | --- |
| Run CRD | 单次执行的持久记录：runtime、输入、分配、阶段、重试策略、时间戳、输出和 artifact 引用。 |
| Runtime CRD | 定义 Runtime Pod 池、容量、Pod 模板和 artifact store 配置。 |
| Runtime Controller | 将 Runtime CR 协调为 Deployment、RBAC、NetworkPolicy 和 runtime maintainer Deployment。 |
| Scheduler | 监视 Pending Runs 并将其分配给健康且有空余容量的 Runtime Pods。 |
| Runtimed | 每个 Runtime Pod 中的 sidecar。认领已分配的 Runs，调用本地 Runtime Server，更新 Run 状态，上传 artifacts，并输出结构化日志。 |
| Runtime Server | 执行 workload 的本地 gRPC server。内置实现包括 Bash 和 Python。 |
| Runtime Maintainer | Runtime 级别的长期运行 worker，负责生命周期超出单个 Runtime Pod 的维护工作，包括 artifact 清理。 |
| Stale Run Reaper | 检测已分配但 Runtime Pod 消失或心跳停止的 Runs，并应用重试或终止失败策略。 |
| krt | 用于创建、查看、取消、记录和检查 Runs 的 CLI 工具。 |

## 控制流

```text
User / krt
  │
  ▼
Run CRD: Pending
  │
  ▼
Scheduler
  │ 选择健康且有空余容量的 Runtime Pod
  ▼
Run CRD: Scheduled + assignedPod
  │
  ▼
指定 Runtime Pod 中的 runtimed sidecar
  │ 认领 Run 并调用本地 Runtime Server
  ▼
Runtime Server
  │ 执行 workload
  ▼
runtimed 更新 Run 状态、输出、artifacts 和日志
```

## Runtime Pod 模型

```text
Runtime Pod
├── runtimed sidecar
└── runtime 容器
    └── Runtime Server gRPC endpoint
```

`runtimed` 负责 Kubernetes 通信。Runtime Server 仅实现本地执行协议。

## 调度模型

Runtime Pods 暴露：

- Kubernetes `PodReady`，
- `kruntimes.io/RuntimedReady` 心跳，
- runtime 标签，
- 静态容量注解。

调度器从 Run 状态（而非 Pod 注解）中获取快速变化的使用情况。Runtime Pod 仅在就绪、心跳新鲜且未超过容量时才是候选调度目标。

当某个 Run 释放容量时，调度器会唤醒同一 namespace 和 runtime 的 Pending Runs。定期重试作为后备机制。

## 状态模型

`Run.status.phase` 使用以下阶段：

- `Pending`
- `Scheduled`
- `Running`
- `Succeeded`
- `Failed`
- `Timeout`
- `Cancelled`

失败、超时和取消的终止条件已被规范化处理。

## 数据边界

Kubernetes 存储紧凑的控制平面状态：

- 生命周期阶段，
- 分配信息，
- 有界输出，
- artifact 引用，
- 时间戳，
- conditions。

大数据保持在 etcd 之外：

- 完整的 stdout/stderr 存储在结构化日志中，
- artifact 文件存储在配置的 ArtifactStore 中，
- runtime 本地执行状态保留至 `Forget`。

## 设计决策

- **不在请求时创建 Pod**：预热 Runtime Pods 吸收短生命周期 Runs。
- **CRD 作为 source of truth**：Kubernetes 保持为持久控制平面。
- **Runtime Server 是本地的**：不需要全局 runtime service mesh。
- **至少一次执行语义**：runtimed 和 stale reaper 共享重试语义。
- **受信任的内置 runtimes**：内置 runtimes 以隔离性换取低延迟。

隔离限制见 [Security and Threat Model](security.md)。
