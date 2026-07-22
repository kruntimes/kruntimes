---
title: "Scheduler Framework 与批量规划"
---

# Scheduler Framework 与批量规划

状态：**Proposal；实现或改变 affinity API 语义前必须 review**

本文定义 kruntimes 的目标调度架构。它将当前“每个 Pending Run 独立 reconcile”的模型替换为
leader-owned scheduler：针对同一个 Runtime Pod、capacity 和已有 assignment 的一致快照，统一规划一组
有界 Runs。

本文档不增加 public API。任何 `Run.spec.priority` 或显式 group-scheduling API 都需要单独的、经过
review 的 API design。

## 问题

当前 scheduler 每次只处理一条 Pending Run：列出 Runtime Pods 和 Runs，过滤 candidate，选择一个 Pod，
然后 patch 这一条 Run 的 status。下一次 reconcile 从新的 cache snapshot 开始。

该模型有两个限制：

- filter、scoring、retry 和 capacity accounting 不断堆积在同一个 reconciler 中，未来很难加入或推理
  priority 等 feature；
- required Run affinity 目前只看到已 assignment 的 active Runs。需要 co-locate 的一组 Pending Runs
  无法看到彼此的预期 placement，因此 affinity cohort 无法可靠 bootstrap。

每次对整个集群的所有 Pending Runs 做调度不是答案。这会带来无界 planning work、head-of-line blocking，
并拉高新提交 Run 的延迟。目标是针对单个 Runtime queue key 的有界、可重复 planning cycle。

## 目标

- 无 capacity 或暂时无法满足约束的 Run 继续保持 `Pending`。
- 让 filters、scoring、reservations、binding、retry/wakeup behavior 都可以独立测试。
- 让同一个 planning batch 中较早 assignment 的结果影响后续 Runs，而不把临时状态暴露到 Kubernetes API。
- 保持 scheduler/runtimed 边界：scheduler 决定 Runtime Pod；runtimed 负责 execution 和 local preparation。
- 为 priority、fairness 和未来显式 group-scheduling feature 提供扩展点。

## 非目标

- 对全局所有 Pending Runs 进行一次优化 pass。
- Workflow-aware scheduling 或解释 Workflow job labels。
- 为任意 dependency cycles 隐式提供 all-or-nothing placement。
- 在本 PR 中修改 public Run affinity type。
- 取代 Kubernetes 对 Runtime Pods 自身的调度。

## 调度范围与队列

scheduler 为每个 `(namespace, runtime)` key 维护逻辑队列。当 Run 是 Pending、引用该 Runtime 且不在
retry backoff 等待中时，它进入该队列。Runtime Pod readiness/capacity 变化和 active Run 释放 capacity 时，
都会再次 enqueue 对应 key。

scheduler leader 从一个 key 中 drain 有界 batch。batch size 和 planning time budget 是实现配置，而不是
Run API fields。达到预算后，其余 eligible Runs 留给下一个 cycle。确定性的基础排序使用 creation timestamp
和 UID；未来经过 review 的 priority policy 可以替换 queue ordering，但必须保留 aging 或 fairness rules。

controller watches 只负责添加或重新激活 queue keys，不再让每一条 Run 独立做 placement decision。现有
Deployment leader election 保证同一时间只有一个 active planner 修改 scheduling state。

## Planning Cycle

对一个 queue key，scheduler 执行：

1. **Snapshot**：列出该 namespace/runtime key 的 eligible Pending Runs、ready Runtime Pods 和 active
   assignments。
2. **PreFilter**：校验 scheduler 可见的 Run inputs，并对每个 Run 只编译一次 selector 或 resource state。
   invalid data 是永久 configuration failure；防御性处理应记录带 actionable reason 的 terminal `Failed` status。
3. **Filter**：移除不 ready、runtimed readiness stale、没有 unreserved capacity、违反 bound workspace
   placement，或违反 required affinity/anti-affinity term 的 Pods。
4. **Score**：按 preferred affinity、available capacity 和 least loaded 对 eligible Pods 打分。Pod name 用于
   stable tie breaking。
5. **Reserve**：在内存 planning state 中记录选中的 Pod 并消耗 capacity。同一 batch 的后续 Runs 可以看到
   这个 tentative assignment。
6. **Bind**：将每个 reserved Run patch 为带 Pod name 和 UID 的 `Scheduled`。resource-version conflict 或
   stale Pod observation 会丢弃该 Run reservation 并重新 enqueue key；它不是 terminal failure。

scheduler 只提交最终 Run assignments。reservations 不会以 annotation、capacity counter 或 user-visible status
字段持久化。restart 后，下一个 snapshot 根据 assigned active Runs 重建 capacity，因此不需要独立的
reservation recovery protocol。

## Affinity 语义

required 和 preferred affinity terms 继续使用现有 namespace-local Run labels 和
`kruntimes.io/runtime-pod` topology。一个 planning cycle 中，term 可以匹配：

- **actual target**：已经 assignment 到 Runtime Pod 的 active Run；或
- **planned target**：同一 batch 中较早 Run 的 reservation。

这样 cohort 的后续成员可以和较早的 tentative assignment co-locate，同时仍遵守 capacity。

### Self-Affinity Bootstrap

如果 required `runAffinity` term 没有 actual 或 planned matching target，当前 Run 只有在自己的 labels 也
匹配该 term selector 时，才能作为 cohort seed。scheduler 随后选择满足其余 hard constraints 的任意 Pod
并 reserve。后续 matching Runs 可以使用这个 planned target。

该规则有意类似 Kubernetes 的 self-affinity bootstrap：没有 matching member 时，第一个 member 可以被
调度，前提是它匹配该 term 自身。它避免 homogeneous affinity cohort 永远等待，同时保留 required constraint
的含义。

此规则**不能**解决任意 mutual dependency cycle。如果 Run A 只要求 Run B 拥有的 labels，而 Run B 只要求
Run A 拥有的 labels，二者都不是 self-affinity seed。它们会以 affinity reason 保持 unschedulable。未来需要
严格 all-or-nothing placement 的 workload 必须使用显式、经过 review 的 scheduling-group API，而不能依赖
偶然 queue order 或推断出来的 Workflow structure。

## Status 与 Retry 语义

| 情况 | Run 状态 | Scheduler action |
| --- | --- | --- |
| 没有 ready Pod、capacity 或当前可满足的 required affinity | `Pending` | 记录有界 waiting reason，并在相关变化或 backoff 到期时重新激活。 |
| scheduler-visible constraint 非法 | `Failed` | 记录 actionable terminal reason；不能 hot-loop。 |
| 无法满足 preferred affinity | 存在其他 feasible Pod 时为 `Scheduled` | 继续正常 scoring；preference 不是 hard constraint。 |
| Bind conflict 或 stale snapshot | `Pending` | 丢弃 reservation 并重新 enqueue key。 |
| Bind 后 Runtime Pod 不健康 | 现有 retry/reassignment flow | scheduler 不再创造独立 retry engine。 |

任何 terminal transition 都必须使用 shared terminal-status helper，保证 conditions 和 completion time
保持 normalized。

## 可扩展性

framework 有显式的 internal extension points：

- **Queue ordering**：当前是确定性的 FIFO-like ordering；未来 priority design 可以定义 priority classes、
  aging、quotas 和 fairness。
- **PreFilter/Filter**：Runtime readiness、capacity、workspace placement 和 required affinity 是独立
  predicates，而不是一个 reconciler 中的 branches。
- **Score**：preferred affinity 和 least-loaded scoring 是独立 weighted inputs，并具有 stable tie breaking。
- **Reserve/Bind**：未来显式 scheduling group 可以在任何 Run bind 前，为一个已知 group 做 atomic planning。

显式 group feature 必须定义 membership、minimum/required cardinality、timeout、cancellation、partial success
和 observability。它不由 Run affinity 隐式推断，也不属于本文 API。

## 可观测性

实现应保留现有 scheduling latency 与 result metrics，再增加 queue activation、batch size、planning duration、
filter rejection reason、reservation conflict 和 unschedulable wakeup 的有界 labels/counters。Run names、
selectors 和 Pod names 不能作为 metric labels。

## 实现顺序

1. Review 本架构，并更新 Run affinity design，使本文成为 scheduling execution semantics 的权威来源。
2. 在 queue/planner interface 后重构 scheduler internals，同时保留当前 one-Run observable behavior 与
   existing metrics。
3. 实现 Snapshot、PreFilter、Filter、Score、Reserve 和 Bind，并增加 deterministic planning、capacity
   accounting 和 bind conflicts 的 unit tests。
4. 实现 planned-target matching 和 self-affinity bootstrap，并增加 integration 与 E2E coverage。
5. 只有经过独立 API 与 fairness design review 后，才加入 priority。
6. 只有当 Workflow 或 batch demo 证明需要 all-or-nothing placement 时，才设计显式 scheduling-group API。

在第 1 步以及预期 bootstrap/status semantics 被 review 前，affinity implementation PR 不能 merge。
