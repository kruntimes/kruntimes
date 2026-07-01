---
title: "目标用户验证 Playbook"
---

# 目标用户验证 Playbook

这个 playbook 将 [开源准备计划](open-source-readiness.md) 中公开后的验证事项变成可重复
执行的流程。它本身不是验证证据。只有真实用户完成活动并记录证据后，才能把 readiness
事项标为完成。

## 目标

用这个流程回答三个问题：

1. 目标用户能否快速理解 kruntimes 的价值？
2. 他们是否真的有 Pod startup latency、burst throughput 或 infrastructure ownership
   相关的紧迫执行问题？
3. 第一个 wedge，也就是 AI agent tools 和 trusted internal code-execution sandboxes，
   是否足够强，可以指导近期产品工作？

## 目标用户分组

招募 5-8 个用户，覆盖以下分组：

| 分组 | 关注点 | 强信号 |
| --- | --- | --- |
| AI agent infrastructure | 运行 agent 所需的 trusted tools、code execution 或 sandbox-like tasks 的团队。 | 他们已经维护 warm workers，或被 per-task Pod startup 卡住。 |
| Platform engineering | 为内部团队提供 execution API 的 internal developer platform 团队。 | 他们需要 Kubernetes-native ownership、RBAC、status 和 runtime pool control。 |
| CI infrastructure | 运行大量短可信 CI steps 的团队。 | Cold start 或 queue drain time 已经影响开发者体验。 |
| Automation platforms | 运行短内部脚本或运维任务的团队。 | 他们需要快速 dispatch 和清晰 Run status，但不想自建完整 worker platform。 |

不要让主要诉求是 hostile-code isolation、成熟 workflow UI、event-driven serving 或
cluster-level batch policy 的反馈过度主导第一阶段判断。这类用户的反对意见仍然有价值，
但不应该定义第一个 wedge。

## 外联消息

使用简短、问题导向的外联：

```text
I'm validating kruntimes, a Kubernetes-native execution engine for running
trusted short tasks on pre-warmed Runtime Pods.

I'm looking for teams that run AI agent tools, internal code execution, CI
micro-steps, or automation tasks on Kubernetes and feel Pod startup latency,
burst queue drain, or runtime ownership pain.

Would you be open to a 30-minute conversation or trying a small demo workload?
I'm mainly trying to learn whether this solves a real problem, not to sell a
finished platform.
```

公开的访谈反馈可以通过
[Target user interview issue template](https://github.com/kruntimes/kruntimes/issues/new?template=target_user_interview.yml)
记录。这个模板只用于公开、非敏感的摘要。不要在公开 issue 中包含真实公司机密、凭证、私有源代码、客户数据或安全漏洞报告。

## 访谈脚本

第一次对话聚焦当前行为，而不是让用户描述想要的功能。

1. 你们今天运行哪些 short-lived execution workloads？
2. 当前如何调度：one Pod per task、warm workers、queue consumer，还是其他系统？
3. Startup latency 在哪里影响用户或 pipeline？
4. burst 场景下，什么因素决定 queue drain time？
5. runtime image、security policy、ServiceAccount 和 resource limits 由谁负责？
6. 用户需要哪些 logs、artifacts、status、cancellation 和 retry semantics？
7. 什么情况会让 Kubernetes-native warm execution pool 变得不可接受？
8. 听完介绍后，你会如何用自己的话解释 kruntimes？
9. 你愿意用真实非生产 workload 试一下吗？如果不愿意，缺什么？

当用户解释项目价值或拒绝项目时，尽量记录原话。原话比维护者总结更能作为证据。

## 试用任务

让每个合格用户试一个小 workload：

1. 在本地或开发 Kubernetes 集群安装 kruntimes。
2. 跑通 [快速开始](quickstart.md)。
3. 跑至少一个 [端到端 demo](demos.md)，优先选择最接近他们分组的 workload。
4. 查看 Run status、compact outputs 和 logs。
5. 将 demo command 替换为一个真实内部命令、脚本，或他们 workload 的 toy 版本。
6. 记录 setup time、blockers、缺失权限、文档困惑，以及他们愿意或不愿意继续的节点。

只有用户运行了真实或有代表性的 workload，才把 trial 计为 design-partner success。只看
maintainer 演示不算。

公开 trial feedback 可以通过
[Design partner trial issue template](https://github.com/kruntimes/kruntimes/issues/new?template=design_partner_trial.yml)
记录。私下访谈应在内部使用同样字段，并且不要在未经明确同意的情况下公开公司名称、
credentials、私有源代码或客户数据。

## 证据模板

每次对话或 trial 记录一行。除非用户明确同意公开引用，不要在公开文档中写真实公司或个人
名称。

| Date | User Label | Segment | Workload | Signal | Evidence | Follow-up |
| --- | --- | --- | --- | --- | --- | --- |
| YYYY-MM-DD | target-user-a | AI infra / Platform / CI / Automation | Short description | Comprehension / Trial interest / Trial / Quick start / Rejection | Link to interview issue, trial issue, Discussion, notes, or anonymized quote | Next action |

## Wedge 判断规则

用以下规则评估 AI agent tools 和 trusted internal code-execution wedge：

- **Validate**：至少两个 wedge 内目标用户运行有代表性的 workloads，并确认 warm Runtime
  pool model 解决了当前问题。
- **Keep exploring**：用户理解价值，但还没有试真实 workload。
- **Refocus positioning**：用户无法在两分钟内解释项目价值。
- **Shift wedge**：另一个分组表现出更强 urgency 和 trial 意愿，而 AI/tooling 用户主要
  反馈 hostile-code isolation 或成熟 workflow UI 等非目标诉求。
- **Prioritize blockers**：多个 trial 遇到同一个缺失能力，即使它不在原 roadmap 上，也
  应优先处理。

仓库指标、stars 和 downloads 只是背景信息。没有用户对话和 workload trial，不能验证
wedge。

## 每周复盘

公开验证早期每周复盘一次证据：

1. 统计 conversations、真实 workload trials、independent quick starts，以及非 maintainer
   issues 或 PRs。
2. 找出重复 blockers 和让人困惑的文档。
3. 决定下一周重点是更多访谈、demo 质量、安装修复还是产品变更。
4. 只有当证据改变项目方向时，才更新 [Adoption Signals](adoption-signals.md) 和 roadmap。
