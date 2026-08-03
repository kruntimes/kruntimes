# Scheduler Framework

Status: **Accepted; Filter, Reserve/Assume/Bind, and Inter-Run Affinity implementation complete**

This document defines the target scheduling architecture for kruntimes. It
replaces the current model of independently reconciling each Pending Run with
a scheduler queue and a single-Run scheduling cycle. Each cycle evaluates one
Run against a coherent snapshot of Runtime Pods, active assignments, and
scheduler-local assumed assignments.

## Problem

The current scheduler processes one Pending Run at a time. It lists Runtime
Pods and Runs, filters candidates, selects one Pod, and patches that Run's
status. A second reconcile starts from a new cache snapshot.

That model has two limits:

- filters, scoring rules, retry behavior, and capacity accounting are growing
  inside one reconciler, making future features such as priority difficult to
  add or reason about;
- between candidate selection and the `Scheduled` status patch, there is no
  scheduler-local representation of the tentative assignment;
- required Run affinity currently sees only assigned active Runs. A set of
  Pending Runs that need to co-locate cannot use each other's intended
  placement, so an affinity cohort cannot reliably bootstrap.

## Goals

- Keep no-capacity and temporarily unsatisfied Runs in `Pending`.
- Make filters, scoring, reservations, binding, and retry/wakeup behavior
  independently testable.
- Make a selected assignment visible to later scheduling cycles before its
  status patch completes, without exposing temporary state in the Kubernetes
  API.
- Preserve the scheduler/runtimed boundary: scheduling decides a Runtime Pod;
  runtimed owns execution and local preparation.
- Provide an extension point for priority and fairness.

## Non-Goals

- A cluster-wide all-Pending-Run optimization pass.
- Workflow-aware scheduling or interpretation of Workflow job labels.
- Changing the public Run affinity type in this PR.
- Replacing Kubernetes scheduling of Runtime Pods themselves.

## Scheduling Scope and Queue

The controller-runtime workqueue holds one `(namespace, name)` Run key per
eligible Pending Run and coalesces repeated activations for the same key. A
dequeue performs one scheduling cycle for that Run. Current ordering follows
controller-runtime event and requeue ordering; kruntimes does not yet impose a
creation-time or UID ordering. A future reviewed priority policy can define
ordering, aging, and fairness.

Queue entries are created or reactivated by these events:

- a Run becomes eligible to schedule;
- a Runtime Pod becomes ready, unavailable, or changes capacity; or
- an assigned Run leaves the active set and releases capacity.

For Runtime Pod and capacity events, the scheduler uses a controller-runtime
local-cache `Run.spec.runtime` field index to find Pending Runs for that
Runtime and adds their Run keys to the queue. This is not a Kubernetes API
Server field selector, so this query must continue to use the manager cache
client rather than an API reader. The event handler does not select a Runtime
Pod or patch a Run; that happens only after the queue worker dequeues an
individual Run key.

## Planning Cycle

For one dequeued Run, the scheduler performs:

1. **Snapshot**: read the Run, ready Runtime Pods, active assignments, and
   assumed assignments for that Run's namespace/runtime key.
2. **PreFilter**: validate scheduler-visible Run inputs and compile selector or
   resource state once per Run. Invalid data is a permanent configuration
   failure; defensive handling records a terminal `Failed` status with an
   actionable reason.
3. **Filter**: remove Pods that are not ready, have stale runtimed readiness,
   lack unreserved capacity, violate a bound workspace placement, or violate a
   required affinity/anti-affinity term.
4. **Score**: score eligible Pods using preferred affinity, available capacity,
   and least loaded placement. Tie breaking is stable by Pod name.
5. **Reserve and Assume**: record the chosen Pod and consume capacity in the
   scheduler-local assumed cache. Subsequent Run cycles observe this tentative
   assignment.
6. **Bind**: patch the Run to `Scheduled` with its Pod name and UID. A
   resource-version conflict or stale Pod observation discards that Run's
reservation and requeues the key; it is not a terminal failure.

### Filter Plugins

The scheduler applies registered Filter plugins to every Runtime Pod in the
snapshot. A plugin receives the immutable scheduling snapshot, the Run's
precomputed state, and one Pod; it returns either feasible or a bounded
rejection reason. Plugins must not patch Kubernetes objects, mutate the
assumed-reservation cache, or make placement decisions.

The initial registry has two independent filters, evaluated in deterministic
registration order:

- **RuntimePodAvailability** verifies Runtime Pod readiness, runtimed
  heartbeat freshness, and the complete logical resource request against
  effective capacity.
- **RunAffinity** evaluates required Run affinity and anti-affinity against
  actual and assumed targets, including the Inter-Run Affinity bootstrap rule.

The planner prepares each plugin's Run-specific state once during PreFilter,
then runs the filters for every Pod and stops evaluating that Pod at its first
rejection. It passes only Pods accepted by every filter to Score. Rejection
reasons are aggregated only into bounded Pending status messages and metrics;
they never expose Pod names, selectors, or Run names.

Preferred affinity remains a scoring concern, not a Filter plugin. The current
implementation first narrows feasible candidates to Pods matching a preferred
term when any exist, then uses `LeastLoaded` capacity placement with stable Pod
name tie breaking. The proposed Score-plugin contract below preserves this
behavior; Reserve, Assume, and Bind remain the only operations allowed to
mutate scheduler-local placement state.

### Score Plugins (Proposed)

The scheduler applies registered Score plugins in deterministic order to the
Pods that passed every Filter plugin. A Score plugin receives the immutable
snapshot, precomputed Run state, and current candidate set. It calculates one
comparable score per candidate, retains only candidates tied for its best
score, and passes that reduced set to the next Score plugin. It must not patch
Kubernetes objects or mutate reservation state.

This is an ordered score-phase pipeline, not a weighted-score sum. It preserves
the current public behavior: a preferred affinity match wins over a lower
capacity score, while capacity resolves ties between equally preferred Pods.
Changing that precedence to an additive or configurable weighted policy would
change Run placement semantics and requires a separate reviewed design.

The initial registry will be:

1. **PreferredRunAffinity** sums the existing preferred affinity and
   anti-affinity weights for each candidate, then retains the highest-scoring
   candidates.
2. **LeastLoaded** compares projected complete logical-resource utilization,
   first by dominant utilization and then by total utilization, retaining the
   least-loaded candidates.
3. **PodNameTieBreak** chooses the lexicographically smallest remaining Pod
   name. It is a framework-owned finalization step, not a configurable plugin.

Score plugins make their own errors explicit. A scoring error aborts the
planning cycle without creating an assumption or writing a Run status; normal
controller retry handles transient errors. Filters remain responsible for
unschedulability and bounded Pending messages.

Each reservation belongs to one Run. As in Kubernetes, the assumed placement
lets later scheduling cycles observe capacity consumption and an affinity target
before the status patch completes. Bind failure releases that reservation and
removes the assumed assignment; a successful patch is later observed as an
actual assignment. Reservations are never persisted as annotations, capacity
counters, or user-visible status fields.

### Reservation Lifecycle

The assumed cache is keyed by immutable `(namespace, Run UID)`. Each entry
stores the Run name, selected Pod name, and complete logical resource request.
The Run object passed to Bind retains its observed resource version, so the
Kubernetes status update itself detects concurrent changes. A mutex protects
reserve, release, and snapshot accounting so concurrent queue workers cannot
reserve the same capacity twice.

Snapshot accounting is:

```text
effectiveUsage[pod] = activeAssignedRunUsage[pod] + unconfirmedAssumedUsage[pod]
```

Before adding assumed usage, the scheduler reconciles the cache against the
Run list in the snapshot. An assumption is removed when its Run is observed as
an active assignment to the same Pod, because the active Run usage now owns the
reservation. It is also removed when the same Run UID is terminal, absent, or
assigned to a different Pod. A Pending observation retains an assumption: an
informer can still be behind a successful Bind, and dropping it during that
window would permit overcommit. This makes the handoff from assumed to actual
usage exact and prevents double-counting.

`Reserve` atomically verifies that the selected Pod still has capacity after
all existing assumptions, then inserts the full request. `Bind` patches the Run
status with the selected Pod name and UID. A successful patch deliberately does
not release the assumption: it remains until the informer observes the actual
Run assignment. Any failed patch, including a resource-version conflict,
immediately releases the assumption. The Run remains `Pending` and is requeued
on a conflict; a non-conflict API error is returned for normal controller retry.

If the scheduler process stops between reserve and bind, leader failover starts
with an empty assumed cache. The new leader reconstructs capacity only from
persisted active Run assignments. Thus an unpersisted assumption disappears,
while a successful status patch becomes actual usage when observed; no separate
reservation recovery object or TTL is needed.

## High Availability

High availability is separate from queue and affinity semantics. The initial
implementation requires one active scheduler planner for the cluster. The Helm
deployment already enables controller-manager leader election, so standby
replicas do not consume Run keys or write Run assignments.

On leader failover, the new active planner starts with an empty assumed cache.
It rebuilds its queue from Pending Runs and reconstructs capacity from assigned
active Runs. An assumed assignment whose status patch did not complete therefore
disappears; a successfully patched Run is observed as an actual assignment.
Future scheduler sharding needs a separate ownership design; it is not implied
by this proposal.

## Affinity Semantics

Required and preferred affinity terms continue to use the existing
namespace-local Run labels and `kruntimes.io/runtime-pod` topology. During each
scheduling cycle, a term may match either:

- an **actual target**, an active Run already assigned to a Runtime Pod; or
- an **assumed target**, a Run with a scheduler-local reservation whose status
  patch has not completed.

This lets a later Run cycle co-locate with an earlier tentative assignment while
still respecting capacity.

### Inter-Run Affinity

For a required `runAffinity` term with no actual or assumed matching target,
the current Run may seed the cohort only when its own labels match that term's
selector. The scheduler then chooses any Pod that satisfies its other hard
constraints and records an assumed assignment. Later matching Runs can use that
assumed target.

This follows Kubernetes' bootstrap exception for a first matching workload.
In kruntimes it is named **Inter-Run Affinity**: the first member may be
scheduled when no matching member exists, provided it matches the term itself.
It prevents a homogeneous affinity cohort from waiting forever while preserving
the meaning of a required constraint.

This rule does **not** make unrelated label dependencies satisfiable. If Run A
requires labels only Run B has and Run B requires labels only Run A has, neither
Run can seed the placement. They remain `Pending` with an affinity waiting
reason until a matching actual or assumed target exists.

## Status and Retry Semantics

| Situation | Run state | Scheduler action |
| --- | --- | --- |
| No ready Pod, capacity, or currently satisfiable required affinity | `Pending` | Record a bounded waiting reason and reactivate on relevant changes or backoff expiry. |
| Invalid scheduler-visible constraint | `Failed` | Record an actionable terminal reason; do not hot-loop. |
| Preferred affinity cannot be met | `Scheduled` when another feasible Pod exists | Continue with normal scoring; preference is not a hard constraint. |
| Bind conflict or stale snapshot | `Pending` | Release the assumed assignment and requeue the Run. |
| Runtime Pod becomes unhealthy after bind | Existing retry/reassignment flow | Scheduler does not invent a separate retry engine. |

The scheduler must use the shared terminal-status helper for any terminal
transition so conditions and completion time remain normalized.

## Extensibility

The framework has explicit internal extension points:

- **Queue ordering**: controller-runtime event/requeue ordering is used today;
  a future priority design can define priority classes, aging, quotas, and
  fairness.
- **PreFilter/Filter**: Runtime readiness/capacity and required affinity are
  independently registered Filter plugins rather than branches in one
  reconciler. Future hard predicates follow the same contract.
- **Score**: ordered Score plugins preserve preferred-affinity precedence,
  then resolve ties by least-loaded capacity and Pod name. A future weighted
  aggregation policy needs its own design.
- **Reserve/Assume/Bind**: an assumed assignment makes a selected Runtime Pod
  and consumed capacity visible to later Run cycles before the Run is bound.

## Observability

The implementation retains scheduling latency, queue duration, and result
metrics. Follow-up work adds bounded labels/counters for queue activation,
scheduling-cycle duration, filter rejection reason, assumed-assignment
conflict, and unschedulable wakeup. Run names, selectors, and Pod names must
not be metric labels.

## Implementation Sequence

1. Review this architecture and update the Run affinity design to make this
   document authoritative for scheduling execution semantics.
2. Refactor scheduler internals behind the controller-runtime queue and a
   snapshot/planning interface while preserving current one-Run observable
   behavior and existing metrics. This core step is complete; it does not
   introduce assumed reservations or affinity semantics.
3. Implement Reserve/Assume and Bind with unit tests for deterministic
   selection, assumed capacity accounting, handoff to actual assignments, and
   bind conflicts. This step is complete.
4. Implement independent RuntimePodAvailability and RunAffinity filters,
   assumed-target matching, and Inter-Run Affinity bootstrap with unit,
   integration, and E2E coverage. This step is complete.
5. Add a Runtime field index for Pending Run wakeups, preserving the existing
   coalesced-key and one-Run-cycle behavior. This step is complete.
6. Review and introduce ordered Score plugins for preferred affinity and
   capacity placement while preserving existing precedence.
7. Add bounded metrics for filter rejection, reservations, and wakeups.
8. Add priority only after a separate API and fairness design review.
