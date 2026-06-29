# 对比指南

本文说明 kruntimes 与 Kubernetes 周边执行系统的关系。目标是帮助用户选择合适工具，
而不是把 kruntimes 定位成所有系统的通用替代品。

kruntimes 是早期阶段的 Kubernetes-native warm execution 项目。当前实现会保持
Runtime Pod 预热，并在这些 pool 内调度单个 Run。长期方向会和 serverless、CI/CD
和 batch execution platform 的一部分目标空间重叠，但当前 kruntimes 还不是这些系统的
成熟替代品。

## 总览

| 方案 | 核心职责 | 适合场景 | 与 kruntimes 的关系 |
| --- | --- | --- | --- |
| kruntimes | 把 Run 调度到 warm Runtime Pod。 | 探索低延迟可信执行、CI micro-step、自动化、AI agent tools，以及受益于 warm pool 的 batch work 的早期用户。 | 实验阶段。部分未来目标会和 serverless、CI/CD、batch platform 重叠，但当前成熟度低很多。 |
| Knative | 在 Kubernetes 上构建和运行 serverless service。 | 请求服务、event source、revision、流量拆分和 scale-to-zero。 | 成熟 serverless 平台。kruntimes 未来可能覆盖一部分相邻执行需求，但目前不具备 Knative 的平台能力。 |
| Argo Workflows | 编排 Kubernetes-native workflow DAG。 | 多步骤 workflow、template、artifact 驱动 pipeline、retry 和 workflow UI/history。 | 成熟 workflow engine。kruntimes 有早期 Workflow CRD，但不是 Argo workflow 生命周期和生态的替代品。 |
| Tekton | 使用 Kubernetes resource 定义 CI/CD pipeline。 | 软件交付 pipeline、task、workspace、catalog 和 pipeline 集成。 | 成熟 CI/CD 框架。kruntimes 未来可能走向更快 CI/CD task execution，但当前不能替代 Tekton。 |
| Volcano | 在集群级调度 batch workload。 | 队列公平性、gang scheduling、quota、preemption 和计算密集 batch policy。 | 集群级 batch scheduler。kruntimes 当前提供 warm pool dispatch，而不是集群调度策略。 |
| Deployment 上的 worker queue | 用长运行 Pod 消费队列任务。 | 带应用自有队列语义的简单内部任务。 | 除非需要 Kubernetes-native Run object 和 Runtime pool，否则通常是更简单、更成熟的选择。 |

## kruntimes vs Knative

Knative 是成熟的 cloud-native serverless 平台，关注 serving、eventing、revision、
流量路由、autoscaling 和 scale-to-zero。

kruntimes 的一些长期目标靠近 serverless 空间：快速 function-like execution、warm
runtime pool 和 Kubernetes-native execution API。但当前 kruntimes 不具备 Knative 的
生产级平台表面积。只有当你能接受实验阶段 execution substrate，并且眼前问题是 warm
trusted execution 而不是完整 serverless application platform 时，才适合评估 kruntimes。

需要以下能力时选择 Knative：

- HTTP serving 和请求路由。
- Revision、流量拆分和 rollout 行为。
- Event source 和 event delivery。
- Service 的 scale-to-zero 语义。

在探索以下方向时可以考虑 kruntimes：

- 快速执行可信 command、function 或代码片段。
- 为重复短任务保持 Runtime pool 预热。
- Kubernetes-native Run status、artifact、log 和 scheduling。
- 为专用执行环境实现自定义 Runtime Server。

## kruntimes vs Argo Workflows

Argo Workflows 是成熟 workflow engine。它建模 DAG、template、step、parameter、
artifact、retry 和 workflow-level history。

kruntimes 当前更偏底层。它执行 Run 并保持执行路径预热。它的 Workflow CRD 还很早期，
不应该被视为 Argo 的 workflow 生命周期、UI、生态或运维模型的替代品。

需要以下能力时选择 Argo Workflows：

- 丰富的 DAG 编排和 workflow template。
- Workflow UI、history、resubmission 和运维工具。
- Artifact 驱动的 step composition。
- Kubernetes workflow 周边生态集成。

在探索以下方向时可以考虑 kruntimes：

- 单个可信 step 的低延迟执行。
- 为重复短 command 或 script 准备 warm Runtime pool。
- 在预热 Kubernetes Pod 内做细粒度调度。
- 聚焦更快 CI/CD 风格 task execution 的未来 workflow 行为，而不是完整 workflow
  platform parity。

## kruntimes vs Tekton

Tekton 是成熟 Kubernetes-native CI/CD 框架。它提供 Pipeline、Task、Workspace、catalog、
trigger，以及面向软件交付系统的 supply-chain 集成。

kruntimes 未来可能走向更快 CI/CD task execution，尤其是受 Pod startup latency 影响的
短可信 step。但今天它不是 CI/CD 框架，也不提供 Tekton 的 pipeline authoring、catalog、
trigger、workspace model 或交付生态。

需要以下能力时选择 Tekton：

- CI/CD pipeline 语义和可复用 Task。
- Workspace、pipeline resource、trigger 和交付集成。
- 软件交付自动化的标准模型。
- 与现有 Tekton 工具生态兼容。

在探索以下方向时可以考虑 kruntimes：

- 为小型 CI step 或自动化 command 提供快速执行 substrate。
- 为重复工作保持自定义 Runtime image 预热。
- 在 Run object 上记录 status 和 artifact reference，而不是管理完整 pipeline 生命周期。
- warm CI/CD execution 的早期设计空间，而不是成熟 Tekton 替代品。

## kruntimes vs Volcano

Volcano 是 Kubernetes 的 batch scheduling system，关注计算密集 workload 的集群级
队列和调度策略。

kruntimes 不替代 Kubernetes scheduler 或 Volcano。它使用 Kubernetes 放置 Runtime Pod，
然后在这些 warm pool 内 dispatch Run。这可能解决另一层问题，但它不是集群级 batch
scheduling policy 系统。

需要以下能力时选择 Volcano：

- Gang scheduling 或 co-scheduling。
- Queue fairness、quota、priority 和 preemption。
- 计算密集 batch workload 的集群级策略。
- 需要协调多个 Pod 的调度行为。

在探索以下方向时可以考虑 kruntimes：

- 在已经放置好的 Runtime Pod 内快速重复执行。
- 基于 Runtime 健康和容量做应用层 dispatch。
- 分层调度：Kubernetes 处理 pool placement，kruntimes 处理 pool 内 Run placement。
- per-execution Pod startup 是瓶颈的 batch workload。

## kruntimes vs Deployment 上的 Worker Queue

Deployment 加 queue 通常是运行后台任务的最简单方式。如果这个模型已经满足延迟、状态、
安全和运维要求，它可能就是正确选择。

kruntimes 增加了 Kubernetes-native execution object、scheduling、artifact、
bounded status、structured logs 和 Runtime pool management。当执行 API 需要通过
Kubernetes 可见和可运维，而不是隐藏在某个应用队列中时，这些能力可能有价值。它们也会
增加运维表面积，而且项目仍在早期。

需要以下能力时选择 worker queue：

- 尽可能简单的架构。
- 应用自有 queue 语义。
- 不需要 Kubernetes-native Run object 的长运行 worker。
- 最小化 CRD 和平台表面积。

在探索以下方向时可以考虑 kruntimes：

- 带 status、terminal condition、retry、timeout 和 cancellation 的 Run CRD。
- per-Runtime capacity 和 health-aware scheduling。
- Run status 中的 artifact reference 和 bounded output。
- 面向多个团队或工具的可复用执行 substrate，同时接受项目仍在成熟过程中。

## 决策启发

先看核心问题：

- 如果问题是 serving traffic，优先看 Knative。
- 如果问题是 workflow orchestration，优先看 Argo Workflows。
- 如果问题是 CI/CD pipeline authoring，优先看 Tekton。
- 如果问题是 cluster-level batch policy，优先看 Volcano。
- 如果问题是简单后台任务，考虑 worker queue。
- 如果问题是在 Kubernetes 上通过 warm pool 做低延迟可信执行，并且你能接受早期项目，
  可以评估 kruntimes。

不要假设 Argo Workflows 或 Tekton 这类现有系统会适配 kruntimes 作为执行后端。更现实的
早期采用路径是自定义内部平台、CLI 或 automation service 直接创建 Run。
