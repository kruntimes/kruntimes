# Scheduler Framework and Inter-Run Affinity

Status: **Proposal; review required before implementation or affinity API
semantics change**

This document defines the target scheduling architecture for kruntimes. It
replaces the current model of independently reconciling each Pending Run with a
leader-owned scheduler that plans a bounded set of Runs against one coherent
snapshot of Runtime Pods, capacity, and existing assignments.

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
submitted Runs. The target is a bounded, repeatable planning cycle scoped to
one Runtime queue key.

## Goals

- Keep no-capacity and temporarily unsatisfied Runs in `Pending`.
- Make filters, scoring, reservations, binding, and retry/wakeup behavior
  independently testable.
- Let assignments made earlier in a planning cycle influence later assignments
  without exposing temporary state in the Kubernetes API.
- Preserve the scheduler/runtimed boundary: scheduling decides a Runtime Pod;
  runtimed owns execution and local preparation.
- Provide an extension point for priority and fairness.

## Non-Goals

- A cluster-wide all-Pending-Run optimization pass.
- Workflow-aware scheduling or interpretation of Workflow job labels.
- Changing the public Run affinity type in this PR.
- Replacing Kubernetes scheduling of Runtime Pods themselves.

## Scheduling Scope and Queue

The scheduler owns a logical queue for each `(namespace, runtime)` key. A Run
enters that queue when it is Pending, references that Runtime, and is not
waiting for retry backoff. Runtime Pod readiness/capacity changes and active
Run releases enqueue the affected key again.

The scheduler leader processes a bounded set of eligible Runs from one key in
each planning cycle. The set size and planning time budget are implementation
configuration, not Run API fields.
When a budget is reached, remaining eligible Runs stay queued for the next
cycle. A deterministic base ordering uses creation timestamp and UID; a future
reviewed priority policy can replace that queue ordering while retaining aging
or fairness rules.

Controller watches only add or reactivate queue keys. They do not independently
make placement decisions for each Run. The existing Deployment leader election
ensures only one active planner mutates scheduling state at a time.

## Planning Cycle

For one queue key, the scheduler performs:

1. **Snapshot**: list the eligible Pending Runs, ready Runtime Pods, and active
   assignments for the namespace/runtime key.
2. **PreFilter**: validate scheduler-visible Run inputs and compile selector or
   resource state once per Run. Invalid data is a permanent configuration
   failure; defensive handling records a terminal `Failed` status with an
   actionable reason.
3. **Filter**: remove Pods that are not ready, have stale runtimed readiness,
   lack unreserved capacity, violate a bound workspace placement, or violate a
   required affinity/anti-affinity term.
4. **Score**: score eligible Pods using preferred affinity, available capacity,
   and least loaded placement. Tie breaking is stable by Pod name.
5. **Reserve**: record the chosen Pod and consume capacity in the in-memory
   planning state. Subsequent Runs in the same planning cycle observe this tentative
   assignment.
6. **Bind**: patch each reserved Run to `Scheduled` with its Pod name and UID.
   A resource-version conflict or stale Pod observation discards that Run's
   reservation and requeues the key; it is not a terminal failure.

Each reservation is independent: the scheduler attempts to bind every reserved
Run separately, and a failure for one Run does not roll back other successfully
bound Runs. As in Kubernetes, the reservation is an assumed in-memory
placement: it lets later scheduling decisions observe capacity consumption
before the status patch completes. Reservations are never persisted as
annotations, capacity counters, or user-visible status fields.
After a restart, the next snapshot reconstructs capacity from assigned active
Runs, so no separate reservation recovery protocol is required.

## Affinity Semantics

Required and preferred affinity terms continue to use the existing
namespace-local Run labels and `kruntimes.io/runtime-pod` topology. During one
planning cycle, a term may match either:

- an **actual target**, an active Run already assigned to a Runtime Pod; or
- a **planned target**, an earlier Run reservation in the same planning cycle.

This lets later members of a cohort co-locate with an earlier tentative
assignment while still respecting capacity.

### Inter-Run Affinity Bootstrap

For a required `runAffinity` term with no actual or planned matching target,
the current Run may seed the cohort only when its own labels match that term's
selector. The scheduler then chooses any Pod that satisfies its other hard
constraints and reserves it. Later matching Runs can use that planned target.

This follows Kubernetes' bootstrap exception for a first matching workload.
In kruntimes it is named **Inter-Run Affinity**: the first member may be
scheduled when no matching member exists, provided it matches the term itself.
It prevents a homogeneous affinity cohort from waiting forever while preserving
the meaning of a required constraint.

This rule does **not** make unrelated label dependencies satisfiable. If Run A
requires labels only Run B has and Run B requires labels only Run A has, neither
Run can seed the placement. They remain `Pending` with an affinity waiting
reason until a matching active or planned target exists.

## Status and Retry Semantics

| Situation | Run state | Scheduler action |
| --- | --- | --- |
| No ready Pod, capacity, or currently satisfiable required affinity | `Pending` | Record a bounded waiting reason and reactivate on relevant changes or backoff expiry. |
| Invalid scheduler-visible constraint | `Failed` | Record an actionable terminal reason; do not hot-loop. |
| Preferred affinity cannot be met | `Scheduled` when another feasible Pod exists | Continue with normal scoring; preference is not a hard constraint. |
| Bind conflict or stale snapshot | `Pending` | Discard the reservation and requeue the key. |
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
- **Reserve/Bind**: a reservation makes a selected Runtime Pod and consumed
  capacity visible to later decisions in the same planning cycle before each
  Run is independently bound.

## Observability

The implementation should retain scheduling latency and result metrics, then
add bounded labels/counters for queue activation, planning set size, planning duration,
filter rejection reason, reservation conflict, and unschedulable wakeup. Run
names, selectors, and Pod names must not be metric labels.

## Implementation Sequence

1. Review this architecture and update the Run affinity design to make this
   document authoritative for scheduling execution semantics.
2. Refactor scheduler internals behind a queue/planner interface while
   preserving current one-Run observable behavior and existing metrics.
3. Implement snapshot, PreFilter, Filter, Score, Reserve, and Bind with unit
   tests for deterministic planning, capacity accounting, and bind conflicts.
4. Implement planned-target matching and Inter-Run Affinity bootstrap with
   integration and E2E coverage.
5. Add priority only after a separate API and fairness design review.

The affinity implementation PR must not merge until steps 1 and the intended
bootstrap/status semantics are reviewed.
