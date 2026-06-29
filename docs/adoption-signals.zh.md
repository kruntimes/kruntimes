# Adoption Signals

本文定义 kruntimes 公开发布后应跟踪的 adoption signals。目标是判断项目价值是否容易
理解，目标用户是否愿意用真实 workload 试用，以及非 maintainer 是否能独立完成上手。

这些 signal 是产品验证输入，不是 vanity metrics。Star、download 和 page view 有参考
价值，但不能证明 kruntimes 解决了紧迫的执行问题。

## Primary Signals

| Signal | 目标 | 为什么重要 | 证据 |
| --- | --- | --- | --- |
| 价值理解 | 新用户能在两分钟内解释项目价值。 | 如果用户不能快速描述价值，说明定位不清晰。 | Discovery notes、demo feedback、issue/Discussion 表述或用户引用。 |
| 真实 workload 试用 | 至少两个 design partner 用真实 workload 试用。 | 真实 workload 会暴露 example 无法发现的 scheduling、security、artifact 和运维问题。 | 具名 design partner 记录、workload 描述、blocker list 和 follow-up 结果。 |
| 独立 quick start | 至少一个非 maintainer 在没有私下帮助的情况下完成 quick start。 | 项目必须能被没有参与构建的人使用。 | Issue/Discussion 确认、screen share 记录或书面 onboarding report。 |

## Secondary Signals

下面这些指标用于理解 momentum，但不能单独作为产品价值被验证的证据：

- GitHub stars、forks 和 watchers。
- 文档 page views 和 search queries。
- Helm chart pulls 和 image pulls。
- CLI release downloads。
- 非 maintainer 提交的 issues、Discussions 和 PRs。
- 博客、社交媒体、演讲或内部平台评估中的提及。

## 目标用户分群

优先收集最可能感受到问题的用户反馈：

- 运营内部开发者平台的 platform teams。
- 运行大量短 step 的 CI infrastructure teams。
- 运行可信工具或代码 sandbox 的 AI agent infrastructure teams。
- 已经使用 Kubernetes，但受 Pod startup latency 或 queue visibility 困扰的 automation teams。

只需要长运行服务、不可信代码隔离或成熟 workflow UI 的用户反馈仍然有价值，但不应该主导
第一个 wedge 判断。

## Tracking Template

每次 conversation、trial 或 onboarding attempt 记录一行。

| 日期 | 用户 / 团队 | Segment | Workload | Signal | Outcome | Follow-up |
| --- | --- | --- | --- | --- | --- | --- |
| YYYY-MM-DD | TBD | Platform / CI / AI infra / Automation | 简短描述 | Comprehension / Trial / Quick start | Pass / Partial / Fail | 指向 issue、PR、notes 或 next action |

除非用户明确同意被引用，否则不要把私人姓名和公司细节写进公开文档。公开跟踪可以使用
`design-partner-a` 这类匿名标签。

## 验证节奏

项目早期每周 review adoption signals：

1. 统计新增 conversation、真实 workload trial、独立 quick start 和非 maintainer
   contribution。
2. 从 notes 和 issues 中提取重复 blocker。
3. 如果同一 blocker 在多个目标用户 conversation 中出现，更新 roadmap。
4. 如果目标用户能理解项目但不愿意尝试真实 workload，重新审视 primary wedge。

## 判断规则

用下面的规则解释 signal：

- 如果用户无法在两分钟内解释价值，优先改进 positioning、overview docs 和 examples。
- 如果用户理解价值但不尝试真实 workload，优先改进 demo path、installation path 和目标
  segment 聚焦。
- 如果用户尝试真实 workload 但反复遇到同一个运维 blocker，优先解决该 blocker，而不是
  扩大 roadmap。
- 如果非 maintainer 无法完成 quick start，把 onboarding 视为下一个公开里程碑的 release
  blocker。
- 如果多个 design partner 在相似 workload 上成功，再用这个模式收敛 primary wedge。

## 公开报告

在证据足够之前，用定性状态报告 adoption：

- `Exploring`：正在访谈目标用户。
- `Trialing`：至少一个 design partner 正在尝试真实 workload。
- `Validated`：至少两个 design partner 完成真实 workload trial，并且至少一个非
  maintainer 完成 quick start。

不要仅因为 repository metrics 增长就把 wedge 标记为 validated。
