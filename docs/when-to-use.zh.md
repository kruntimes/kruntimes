# 什么时候使用 kruntimes

kruntimes 是 Kubernetes 上的 warm execution substrate。它适合平台团队在保留
Kubernetes 作为持久控制面的同时，获得低延迟、高吞吐的执行能力。

它不是 Kubernetes 周边所有执行平台的替代品。适合使用 kruntimes 的问题通常是
请求时 Pod 启动、突发执行调度，或自定义 warm Runtime pool。不适合把它当成完整
serverless 平台、workflow 引擎、集群 batch scheduler 替代品，或者不可信代码沙箱。

## 适合使用 kruntimes 的场景

### 需要快速调度短任务

kruntimes 适合短生命周期 Run，尤其是每次执行都创建新 Pod 会太慢或太贵的场景。
常见例子包括自动化脚本、CI micro-step、serverless 风格函数，以及内部代码执行工具。

核心价值是避开请求时 Pod 创建。Kubernetes 负责保持 Runtime Pod 预热，
kruntimes 负责把单个 Run 分配到这些 warm Pod 上。

### 团队能控制 Runtime 环境

kruntimes 最适合由平台团队控制 Runtime image、runtime protocol、安全边界和资源模型
的场景。这通常出现在内部开发者平台、CI infrastructure、AI agent infrastructure
和可信自动化系统中。

默认模型不是 “接收任意不可信代码，并为每个 Run 提供强隔离”。内置 Runtime 是可信代码
示例。

### 需要分层调度

当 Kubernetes 仍然负责粗粒度放置，但单个执行需要更快的应用层调度时，
kruntimes 很有价值。

Kubernetes 把 Runtime Pod 调度到节点上。kruntimes 根据 Runtime 健康状态和容量，在
这些 pool 内调度 Run。对于高并发短任务和 batch workload，包括 AI batch workload，
应用层通常比默认 Kubernetes Pod scheduler 拥有更多执行上下文。

### 需要自定义 warm pool

当不同 workload family 需要不同的 warm 执行环境时，可以使用 kruntimes，例如 Bash、
Python、模型工具链、构建工具、内部 SDK，或领域专用 Runtime Server。

Runtime CRD 让这些 pool 保持 Kubernetes-native；Runtime Server protocol 则允许实现
专用执行逻辑。

## 不适合使用 kruntimes 的场景

### 需要完整 Serverless 平台

如果你需要 event source、scale-to-zero、流量路由、revision、rollout、请求服务和
生产级 FaaS 生命周期管理，应使用 Knative 这类 serverless 平台。

kruntimes 聚焦于把执行调度到 warm Runtime pool 中。它是更底层的 building block。

### 需要成熟通用 Workflow 平台

如果核心问题是 DAG 编排、多步骤 retry、artifact 驱动的 workflow composition、
template、审批，或带 UI 的运行历史，应使用 Argo Workflows 或 Tekton 这类 workflow
系统。

kruntimes 可以执行单个 step，也有基础 Workflow CRD。未来的 workflow 能力预计会聚焦于
基于 warm Runtime pool 提供更快的 CI/CD 任务执行。这和替代成熟 workflow 平台不同；
后者提供更完整的生态集成、丰富 UI、审批和完整 workflow 生命周期管理。

### 需要替代集群 Batch Scheduler

如果核心需求是 gang scheduling、队列公平性、quota 管理、拓扑感知调度、抢占或多租户
batch policy，应使用 Volcano 或 Kubernetes 原生 scheduling 生态。

kruntimes 通过复用 warm pool 补充 Kubernetes scheduling。它不替代集群级调度策略。

### 需要运行强隔离的不可信代码

不要使用内置 Bash 或 Python Runtime 运行不可信代码。它们不提供 per-Run 的进程、
网络、文件系统、CPU 或内存强隔离。

如果需要运行不可信代码，应把 kruntimes 和合适的沙箱 Runtime 实现结合起来，例如
microVM、带严格策略的容器、语言级沙箱，或符合你威胁模型的其他隔离技术。

### 普通 Deployment 加 Worker Queue 已经足够

如果一个简单的 queue consumer Deployment 已经满足延迟、可运维性和隔离要求，
kruntimes 可能不是必要的。只有当 Kubernetes-native Run object、状态、调度、
Runtime pool、artifact 和 CLI workflow 都重要时，它才会带来更明显价值。

## 决策 Checklist

如果多数条件成立，可以考虑使用 kruntimes：

- Pod 启动延迟处在用户可感知路径或 pipeline 关键路径上。
- workload 是可信的，或由你控制的自定义 Runtime 提供隔离。
- 团队希望使用 Kubernetes CRD、RBAC、metrics 和 Helm 进行运维。
- Runtime Pod 可以保持 warm，且资源浪费可接受。
- 应用层调度能比 “每次执行创建一个 Pod” 做出更好的分发决策。

如果存在以下情况，需要谨慎：

- workload 是长时间运行，Pod 启动延迟并不重要。
- 需要强不可信代码隔离，但还没有 sandboxed Runtime。
- 成熟 workflow 平台能力或 batch queue policy 才是核心产品需求。
- 团队不想运维 Runtime pool。
- 更简单的 worker queue 已经满足延迟和可观测性目标。

## 典型起步场景

最强的早期切入点是可信内部代码执行，尤其是 cold start 明显影响开发者体验或平台吞吐
的场景。例如 AI agent tools、内部代码 sandbox、CI micro-step 和自动化任务。

这些 workload 受益于 warm environment、bounded status、artifact reference、
结构化日志和 Kubernetes-native ownership，同时不要求 kruntimes 一开始就成为完整的
serverless、workflow、batch 或 sandboxing 平台。
