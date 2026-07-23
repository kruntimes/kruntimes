# Scheduler Framework

Status: **Proposal; review required before implementation or affinity API
semantics change**

This document defines the target scheduling architecture for kruntimes. It
replaces the current model of independently reconciling each Pending Run with
a scheduler queue and a single-Run scheduling cycle. Each cycle evaluates one
Run against a coherent snapshot of Runtime Pods, active assignments, and
scheduler-local assumed assignments.

It does not add a public API in this design slice. Any `Run.spec.priority`
requires a separate reviewed API design.

## Problem

The current scheduler processes one Pending Run at a time. It lists Runtime
Pods and Runs, filters candidates, selects one Pod, and patches that Run's
status. A second reconcile starts from a new cache snapshot.

That model has two limits:

- filters, scoring rules, retry behavior, and capacity accounting are growing
  inside one reconciler, making future features such as priority difficult to
  add or reason about;
- required Run affinity currently sees only assigned active Runs. A set of
  Pending Runs that need to co-locate cannot use each other's intended
  placement, so an affinity cohort cannot reliably bootstrap.

Scheduling all Pending Runs cluster-wide is not the answer. It creates
unbounded planning work, head-of-line blocking, and poor latency for newly
submitted Runs. The target is a repeatable single-Run scheduling cycle, like
Kubernetes' scheduler.

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

The scheduler queue holds one `(namespace, name)` Run key per Pending Run that
is eligible to schedule. A dequeue performs one scheduling cycle for that Run.
A deterministic base ordering uses creation timestamp and UID; a future
reviewed priority policy can replace that queue ordering while retaining aging
or fairness rules.

Changes are inputs to this queue, not placement decisions. Creating `run-a`
enqueues only `run-a`. Runtime Pod readiness/capacity changes and active Run
releases reactivate the affected Pending Runs through a Runtime index; they do
not choose a Pod themselves. Each reactivated Run is still scheduled in its own
cycle.

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

Each reservation belongs to one Run. As in Kubernetes, the assumed placement
lets later scheduling cycles observe capacity consumption and an affinity target
before the status patch completes. Bind failure releases that reservation and
removes the assumed assignment; a successful patch is later observed as an
actual assignment. Reservations are never persisted as annotations, capacity
counters, or user-visible status fields.
After a restart, the next snapshot reconstructs capacity from assigned active
Runs, so no separate reservation recovery protocol is required.

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

- **Queue ordering**: currently deterministic FIFO-like ordering; a future
  priority design can define priority classes, aging, quotas, and fairness.
- **PreFilter/Filter**: Runtime readiness, capacity, workspace placement, and
  required affinity are separate predicates rather than branches in one
  reconciler.
- **Score**: preferred affinity and least-loaded scoring are independent
  weighted inputs with stable tie breaking.
- **Reserve/Assume/Bind**: an assumed assignment makes a selected Runtime Pod
  and consumed capacity visible to later Run cycles before the Run is bound.

## Observability

The implementation should retain scheduling latency and result metrics, then
add bounded labels/counters for queue activation, scheduling-cycle duration,
filter rejection reason, assumed-assignment conflict, and unschedulable wakeup. Run
names, selectors, and Pod names must not be metric labels.

## Implementation Sequence

1. Review this architecture and update the Run affinity design to make this
   document authoritative for scheduling execution semantics.
2. Refactor scheduler internals behind a queue/planner interface while
   preserving current one-Run observable behavior and existing metrics.
3. Implement snapshot, PreFilter, Filter, Score, Reserve/Assume, and Bind with
   unit tests for deterministic selection, assumed capacity accounting, and bind
   conflicts.
4. Implement assumed-target matching and Inter-Run Affinity bootstrap with
   integration and E2E coverage.
5. Add priority only after a separate API and fairness design review.

The affinity implementation PR must not merge until steps 1 and the intended
bootstrap/status semantics are reviewed.
