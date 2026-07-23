---
title: "Scheduler Framework"
---

# Scheduler Framework

状态：**Proposal；实现或改变 affinity API 语义前必须 review**

本文定义 kruntimes 的目标调度架构。它将当前“每个 Pending Run 独立 reconcile”的模型替换为 scheduler
queue 与单 Run scheduling cycle。每个 cycle 针对一个 Run，读取 Runtime Pods、active assignments 和
scheduler-local assumed assignments 的一致快照。

## 问题

当前 scheduler 每次只处理一条 Pending Run：列出 Runtime Pods 和 Runs，过滤 candidate，选择一个 Pod，
然后 patch 这一条 Run 的 status。下一次 reconcile 从新的 cache snapshot 开始。

该模型有两个限制：

- filter、scoring、retry 和 capacity accounting 不断堆积在同一个 reconciler 中，未来很难加入或推理
  priority 等 feature；
- candidate selection 与 `Scheduled` status patch 之间，没有 scheduler-local 的 tentative assignment
  representation；
- required Run affinity 目前只看到已 assignment 的 active Runs。需要 co-locate 的一组 Pending Runs
  无法看到彼此的预期 placement，因此 affinity cohort 无法可靠 bootstrap。

## 目标

- 无 capacity 或暂时无法满足约束的 Run 继续保持 `Pending`。
- 让 filters、scoring、reservations、binding、retry/wakeup behavior 都可以独立测试。
- 让选中的 assignment 在 status patch 完成前对后续 scheduling cycles 可见，而不把临时状态暴露到
  Kubernetes API。
- 保持 scheduler/runtimed 边界：scheduler 决定 Runtime Pod；runtimed 负责 execution 和 local preparation。
- 为 priority 和 fairness 提供扩展点。

## 非目标

- 对全局所有 Pending Runs 进行一次优化 pass。
- Workflow-aware scheduling 或解释 Workflow job labels。
- 在本 PR 中修改 public Run affinity type。
- 取代 Kubernetes 对 Runtime Pods 自身的调度。

## 调度范围与队列

scheduler queue 为每条 eligible Pending Run 保存一个 `(namespace, name)` Run key。dequeue 一个 key 时，只为
该 Run 执行一次 scheduling cycle。确定性的基础排序使用 creation timestamp 和 UID；未来经过 review 的
priority policy 可以替换 queue ordering，但必须保留 aging 或 fairness rules。

以下 events 会创建或重新激活 queue entries：

- 一条 Run 变为 eligible to schedule；
- 一条 Runtime Pod 变为 ready、unavailable，或其 capacity 发生变化；或
- 一条 assigned Run 离开 active set 并释放 capacity。

对于 Runtime Pod 和 capacity events，scheduler 通过 index 找到引用该 Runtime 的 Pending Runs，并将它们的
Run keys 加入 queue。event handler 不会选择 Runtime Pod 或 patch Run；只有 queue worker dequeue 单条 Run key
后才会执行这些操作。

## Planning Cycle

对一个 dequeue 的 Run，scheduler 执行：

1. **Snapshot**：读取该 Run、其 namespace/runtime key 的 ready Runtime Pods、active assignments 和
   assumed assignments。
2. **PreFilter**：校验 scheduler 可见的 Run inputs，并对每个 Run 只编译一次 selector 或 resource state。
   invalid data 是永久 configuration failure；防御性处理应记录带 actionable reason 的 terminal `Failed` status。
3. **Filter**：移除不 ready、runtimed readiness stale、没有 unreserved capacity、违反 bound workspace
   placement，或违反 required affinity/anti-affinity term 的 Pods。
4. **Score**：按 preferred affinity、available capacity 和 least loaded 对 eligible Pods 打分。Pod name 用于
   stable tie breaking。
5. **Reserve and Assume**：在 scheduler-local assumed cache 中记录选中的 Pod 并消耗 capacity。后续 Run
   cycles 可以看到这个 tentative assignment。
6. **Bind**：将该 Run patch 为带 Pod name 和 UID 的 `Scheduled`。resource-version conflict 或 stale Pod
   observation 会释放该 Run 的 reservation 并重新 enqueue 它；这不是 terminal failure。

每个 reservation 属于一条 Run。与 Kubernetes 一样，assumed placement 让后续 scheduling cycles 在 status
patch 完成前看到 capacity consumption 和 affinity target。bind 失败会释放 reservation 并移除 assumed
assignment；成功 patch 后，该 Run 最终会作为 actual assignment 被观察到。reservations 不会以 annotation、
capacity counter 或 user-visible status 字段持久化。restart 后，下一个 snapshot 根据 assigned active Runs
重建 capacity，因此不需要独立的 reservation recovery protocol。

## 高可用

高可用与 queue 和 affinity 语义分离。初版 implementation 要求整个集群只有一个 active scheduler
planner。Helm deployment 已启用 controller-manager leader election，因此 standby replicas 不会消费 Run
keys，也不会写入 Run assignments。

leader failover 后，新 active planner 从空的 assumed cache 开始：它从 Pending Runs 重建 queue，并从
assigned active Runs 重建 capacity。尚未完成 status patch 的 assumed assignment 会因此消失；已成功 patch 的
Run 则会作为 actual assignment 被观察到。未来若要 scheduler sharding，需要独立的 ownership design；本文不
隐含这一能力。

## Affinity 语义

required 和 preferred affinity terms 继续使用现有 namespace-local Run labels 和
`kruntimes.io/runtime-pod` topology。每个 scheduling cycle 中，term 可以匹配：

- **actual target**：已经 assignment 到 Runtime Pod 的 active Run；或
- **assumed target**：一条具有 scheduler-local reservation、但 status patch 尚未完成的 Run。

这样后续 Run cycle 可以和较早的 tentative assignment co-locate，同时仍遵守 capacity。

### Run 间亲和性

如果 required `runAffinity` term 没有 actual 或 assumed matching target，当前 Run 只有在自己的 labels 也
匹配该 term selector 时，才能作为 cohort seed。scheduler 随后选择满足其余 hard constraints 的任意 Pod
并记录 assumed assignment。后续 matching Runs 可以使用这个 assumed target。

该规则遵循 Kubernetes 对第一个 matching workload 的 bootstrap exception。在 kruntimes 中称为**Run 间亲和性**：
没有 matching member 时，第一个 member 可以被调度，前提是它匹配该 term 自身。它避免 homogeneous affinity
cohort 永远等待，同时保留 required constraint 的含义。

此规则**不能**让无关的 label dependency 自动满足。如果 Run A 只要求 Run B 拥有的 labels，而 Run B 只要求
Run A 拥有的 labels，二者都不能成为 placement seed。它们会带着 affinity waiting reason 保持 `Pending`，直到
出现 matching 的 actual 或 assumed target。

## Status 与 Retry 语义

| 情况 | Run 状态 | Scheduler action |
| --- | --- | --- |
| 没有 ready Pod、capacity 或当前可满足的 required affinity | `Pending` | 记录有界 waiting reason，并在相关变化或 backoff 到期时重新激活。 |
| scheduler-visible constraint 非法 | `Failed` | 记录 actionable terminal reason；不能 hot-loop。 |
| 无法满足 preferred affinity | 存在其他 feasible Pod 时为 `Scheduled` | 继续正常 scoring；preference 不是 hard constraint。 |
| Bind conflict 或 stale snapshot | `Pending` | 释放 assumed assignment 并重新 enqueue Run。 |
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
- **Reserve/Assume/Bind**：assumed assignment 会让选中的 Runtime Pod 和已消耗的 capacity 在 Run bind 前
  对后续 Run cycles 可见。

## 可观测性

实现应保留现有 scheduling latency 与 result metrics，再增加 queue activation、scheduling-cycle duration、
filter rejection reason、assumed-assignment conflict 和 unschedulable wakeup 的有界 labels/counters。Run names、
selectors 和 Pod names 不能作为 metric labels。

## 实现顺序

1. Review 本架构，并更新 Run affinity design，使本文成为 scheduling execution semantics 的权威来源。
2. 在 queue/planner interface 后重构 scheduler internals，同时保留当前 one-Run observable behavior 与
   existing metrics。
3. 实现 Snapshot、PreFilter、Filter、Score、Reserve/Assume 和 Bind，并增加 deterministic selection、assumed
   capacity accounting 和 bind conflicts 的 unit tests。
4. 实现 assumed-target matching 和 Run 间亲和性 bootstrap，并增加 integration 与 E2E coverage。
5. 只有经过独立 API 与 fairness design review 后，才加入 priority。

在第 1 步以及预期 bootstrap/status semantics 被 review 前，affinity implementation PR 不能 merge。
