# Workflow Reuse

This document describes a target v0.x design. It is not implemented yet.

The goal is to split workflow execution instances from reusable workflow and
step definitions before the Workflow API stabilizes. The current experimental
`Workflow` CRD represents an execution instance. That shape is too limiting for
CI/CD and automation use cases where teams need reusable workflows and reusable
step groups.

## Current State

The current experimental Workflow API provides:

- one `Workflow` object as the execution instance;
- inline `jobs`;
- inline step `run` scripts;
- `needs` dependencies;
- bounded step/job outputs;
- a future-looking `uses` field that is currently rejected by validation.

It does not yet provide:

- reusable workflow definitions;
- reusable action definitions;
- workflow calls from jobs;
- action calls from steps;
- a clean separation between definition status and run status.

## Goals

- Rename the execution instance API to `WorkflowRun`.
- Reuse the `Workflow` kind for reusable workflow definitions.
- Add an `Action` kind for reusable step groups.
- Keep first-version references namespace-local and short: `uses: <name>`.
- Use `with` for inputs.
- Keep reusable Actions inside the caller job context.
- Give reusable Workflow calls their own job/workspace/artifact boundary.
- Keep validation strict so each object has one clear shape.

## Non-Goals

- No cross-namespace, remote, Git, OCI, or marketplace references in the first
  version.
- No GitHub Actions compatibility promise.
- No matrix strategy in this design.
- No UI or run history design beyond the CRD status shapes required for v0.x.
- No backwards-compatible migration requirement for the current experimental
  `Workflow` execution instance API.

## API Overview

The target split is:

| Kind | Role |
| --- | --- |
| `WorkflowRun` | Execution instance. Defines jobs inline or calls one reusable `Workflow`. |
| `Workflow` | Reusable workflow definition. Can be called from a `WorkflowRun` or from a job. |
| `Action` | Reusable step group. Can be called from a step inside a `WorkflowRun` or `Workflow`. |

## WorkflowRun

`WorkflowRun` is the object users create to start work. It supports either
inline jobs or a top-level workflow call.

Inline form:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: |
            echo "building"
```

Reusable Workflow call form:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  uses: build-and-test
  with:
    image: agent:v0.1.0
```

Validation must enforce that top-level `uses` and inline `jobs` are mutually
exclusive.

## Reusable Workflow

`Workflow` becomes a reusable definition. It is not itself an execution
instance.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: build-and-test
spec:
  inputs:
    image:
      type: string
      required: true
  outputs:
    image:
      value: ${{ jobs.build.outputs.image }}
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: |
            echo "image=${{ inputs.image }}" >> "$KRUNTIME_OUTPUTS"
```

A job can also call a reusable Workflow:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release-demo
spec:
  jobs:
    release:
      uses: build-and-test
      with:
        image: agent:v0.1.0
```

Validation must enforce that job `uses` and job `steps` are mutually exclusive.

Reusable Workflow jobs have their own job/workspace/artifact boundary. They
communicate with callers through inputs, outputs, and artifacts.

## Action

`Action` is a reusable step group.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Action
metadata:
  name: setup-python-tools
spec:
  inputs:
    version:
      type: string
      default: "3.12"
  outputs:
    python-version:
      value: ${{ steps.setup.outputs.python-version }}
  steps:
    - name: setup
      run: |
        echo "python-version=${{ inputs.version }}" >> "$KRUNTIME_OUTPUTS"
        echo "installing toolchain"
```

Steps call an Action with `uses`:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: build-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: setup
          uses: setup-python-tools
          with:
            version: "3.13"
        - name: package
          run: |
            echo "using ${{ steps.setup.outputs.python-version }}"
```

Validation must enforce that step `uses` and step `run` are mutually exclusive.

Actions run inside the caller job context. By default they share the caller job
runtime, workspace, artifacts, environment, and scheduling placement. This makes
Actions lightweight step composition, not a nested workflow execution.

## Inputs and Outputs

The first version should support simple typed string inputs:

```yaml
inputs:
  image:
    type: string
    required: true
  version:
    type: string
    default: "3.12"
```

Outputs should be expression-based:

```yaml
outputs:
  image:
    value: ${{ jobs.build.outputs.image }}
```

For v0.x, validation should prefer a narrow model:

- input `type` starts with `string`;
- `required` and `default` are mutually constrained;
- `with` values are strings;
- missing required inputs fail validation or reconcile early;
- unknown input names fail validation or reconcile early.

The WorkflowRun controller should bind inputs before expanding child jobs or
steps:

1. Start from the callee's declared `inputs`.
2. Apply each input `default`.
3. Overlay caller-provided `with` values.
4. Reject missing required inputs.
5. Reject unknown `with` keys.
6. Store the lightweight resolved DAG edges in `WorkflowRun.status.jobs`
   before creating Runs.

Do not let an already-started WorkflowRun observe mutable changes to a
referenced `Workflow` or `Action`. Reusable definitions may change over time,
but each WorkflowRun execution must be deterministic once accepted. Later work
can add explicit revisioning; the first version should capture enough resolved
data to make retries and controller restarts stable.

Step outputs come from child Run results. A step writes small key-value outputs
to `KRUNTIME_OUTPUTS`; runtimed persists them to the child Run status. The
WorkflowRun controller reads those child Run outputs and promotes them into the
matching ordered `WorkflowRun.status.jobs.<job>.steps[]` entry.

Job outputs are evaluated after all steps in the job succeed. Workflow outputs
are evaluated after all jobs in the reusable Workflow succeed. Output
evaluation must fail the WorkflowRun when an expression references a missing
job, step, or output key.

## Reference Resolution

The first version should keep references namespace-local:

```yaml
uses: build-and-test
uses: setup-python-tools
```

Do not introduce `workflowRef`, `actionRef`, cross-namespace references, remote
URLs, Git refs, or OCI refs until there is a concrete need.

This keeps the API small and avoids creating a reference format that must be
supported long-term before the execution model is stable.

Reference resolution should happen in this order:

1. Resolve a top-level `WorkflowRun.spec.uses` to a `Workflow` in the same
   namespace.
2. Expand the referenced Workflow jobs into the WorkflowRun execution graph.
3. Resolve each job-level `uses` to a same-namespace reusable `Workflow`.
4. Expand reusable Workflow calls as nested job groups with their own
   job/workspace/artifact boundary.
5. Resolve each step-level `uses` to a same-namespace `Action`.
6. Expand Action steps inline inside the caller job context.
7. Detect cycles before creating any child Runs.

Cycles must be rejected across Workflow calls. An Action must not call another
Action in the first version because nested Action expansion is not needed yet
and makes cycle detection harder. A reusable Workflow may call another
Workflow only when the controller can prove the call graph is acyclic.

Resolution failures should set the WorkflowRun to `Failed` before creating any
child Runs. Examples include missing references, wrong namespace assumptions,
unsupported nested Action calls, input binding failures, and cycles.

## Execution Graph

The WorkflowRun controller owns graph expansion and execution state. It should
not rely on scheduler or runtimed to understand Workflow concepts.

The first implementation should use a simple deterministic graph model:

- every job has a stable execution path, such as `jobs.build` or
  `jobs.release.jobs.build` for a job expanded from a reusable Workflow call;
- every step has a stable execution path, such as `jobs.build.steps.package`;
- each child Run is labeled with the WorkflowRun name, job path, and step path;
- child Run names are generated deterministically enough for idempotent
  reconciliation, or are discovered through labels before creating new Runs;
- the controller creates Runs only when all dependency jobs have succeeded;
- terminal child Run phases are preserved on their owning step, then aggregated
  into job and WorkflowRun state according to the terminal semantics below.

The first version should support one execution strategy:

1. Accept the WorkflowRun and set `status.phase=Pending`.
2. Resolve references and bind inputs.
3. Persist resolved predecessor job edges in `status.jobs[*].pre`.
4. Start every runnable step: the first step of each dependency-ready job and
   the next step after a successful predecessor in a running job.
5. When a step Run succeeds, collect outputs; the following reconciliation
   includes its next step with any other runnable steps.
6. Aggregate observed terminal step states into the job state: all succeeded
   steps succeed the job, while any failed step fails it. Job output evaluation
   is deferred until output propagation is implemented.
7. After all executable jobs have reached a terminal state, evaluate
   WorkflowRun outputs and determine its terminal state.

This deliberately avoids adding a separate WorkflowRunInvocation API. Child
Runs remain the durable execution records, and scheduler/runtimed continue to
operate only on Runs.

The WorkflowRun controller should keep reconciliation structured as
load/calculate/apply/patch: load the WorkflowRun and all child Runs, derive the
desired status and current state from those resources, calculate one action,
apply the action, incorporate its result into the desired status, and patch
status only when it differs from the persisted status. Status projection is
part of every reconciliation; observing child Runs and aggregating terminal
steps into job phases are not separate actions. Current state and action remain
intentionally separate. The initial `Empty` state has an `Initialize` action,
which validates controller-level semantics, resolves references and inputs,
persists the execution graph, and sets `Accepted=True`. A failed initialization
sets `Accepted=False` and does not create child Runs. Later execution actions
must not modify `Accepted`: an accepted WorkflowRun can still fail while
executing.

A reconciliation must not loop through multiple external actions before the
status update. One `StartRunnableSteps` action may materialize every currently
runnable step, including steps made runnable by status derived at the start of
that reconciliation. Each job contributes at most one target, so the action
does not advance a job through multiple execution operations in the same
reconciliation. If the action fails, the controller returns the error without
patching status. If it succeeds, created Run identities and running phases are
added to the desired status before the single conditional status patch. This
keeps external operations explicit, idempotent, and restart-safe without using
extra reconciliations for internal status projection.

## Failure, Cancellation, and Terminal Semantics

The v0.x default follows the familiar GitHub Actions job-dependency model:
independent jobs run in parallel, while a failed or skipped prerequisite skips
its dependents. Conditional execution, `continue-on-error`, and matrix
fail-fast behavior are deliberate future API additions; they are not implicit
controller behavior in the first version.

- A terminal child Run is copied to the matching step without rewriting its
  phase. In particular, `RunTimeout` remains `RunTimeout`, and `Cancelled`
  remains `Cancelled`.
- A job succeeds only when all of its steps succeed. A failed, cancelled, or
  timed-out step makes its owning job `Failed`.
- Independent jobs continue to be created and allowed to finish after another
  job fails. A job that depends, directly or transitively, on a failed or
  skipped job is marked `Skipped` and never creates a child Run. Its `pre`
  edges and predecessor job phases identify the blocker, so it is not itself
  `Failed`.
- The controller waits until every executable job has reached a terminal state
  or been skipped. The WorkflowRun is `Failed` if any job failed; otherwise it
  is `Succeeded`, including the case where jobs were skipped only because of a
  dependency. WorkflowRun status must preserve the job-level reasons so the
  aggregate phase is explainable.
- Cancelling a WorkflowRun prevents new child Runs from being created and
  requests cancellation for every non-terminal child Run. Once those children
  have settled, the WorkflowRun is `Cancelled`; it is not converted to
  `Failed` because a child reports cancellation or timeout during this process.

### API Prerequisites

The existing WorkflowRun API cannot represent all of these semantics. Before
the controller implements dependency propagation or cancellation, the API must
add the following fields and phases:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
spec:
  cancelRequested: true
status:
  phase: Cancelled
  jobs:
    test:
      phase: Skipped
      pre: [build]
```

- `WorkflowRun.spec.cancelRequested` is a user intent, mirroring
  `Run.spec.cancelRequested`. Once observed, the controller must not create
  more child Runs for that WorkflowRun.
- `WorkflowPhase` must add terminal `Cancelled`.
- `JobPhase` must add terminal `Skipped`. It means the job was not executed
  because a predecessor failed or was skipped. The existing `pre` edges plus
  predecessor job phases identify the blocking job, so v0.x does not add a
  redundant `blockedBy` status field.
- `JobPhase` does not add `Cancelled` in v0.x. A step cancelled as ordinary
  execution failure makes its running job `Failed`; cancellation of the whole
  WorkflowRun is represented by the parent `Cancelled` phase.

Cancellation is a separate controller action. It takes priority over normal
execution actions, patches `spec.cancelRequested=true` on every non-terminal
child Run, and waits for their terminal phases to be observed. It then sets the
parent WorkflowRun to `Cancelled`. Jobs that were never started retain their
current Pending or Waiting state because they were not skipped by a DAG
dependency; the parent terminal phase explains why they will not run.

Outside cancellation, dependency propagation and WorkflowRun finalization are
default status derivations performed before planning external actions:

1. mark a Pending or Waiting job `Skipped` when any predecessor is `Failed` or
   `Skipped`; independent jobs remain eligible to start;
2. after all executable jobs have settled, set WorkflowRun `Failed` when any
   job is `Failed`, otherwise set it `Succeeded`.

The API change requires regenerated CRDs and controller RBAC allowing the
WorkflowRun controller to patch child Runs for cancellation.

Inline WorkflowRun execution should land in small, reviewable steps:

1. Before changing execution behavior, audit the existing E2E tests. Remove or
   update stale cases that still exercise the old Workflow execution model so
   `make e2e` can stay passing throughout the migration.
2. Create only the first child Run for each ready inline job, record the child
   Run name on the matching ordered step status, and make creation idempotent
   by discovering existing child Runs through labels.
3. Before adding more execution states, refactor the WorkflowRun controller
   into a load/calculate/apply/patch shape: load the WorkflowRun and related
   resources, derive desired status and current state, calculate one external
   action, apply it, incorporate its result, and conditionally patch status.
4. Watch or reconcile child Runs owned by a WorkflowRun and copy terminal child
   Run phase into the matching step status.
5. Define and review failure, cancellation, and terminal-status semantics:
   independent jobs continue after a failure, dependency-blocked jobs become
   `Skipped`, and the WorkflowRun aggregates only after all executable jobs
   settle.
6. When a step succeeds and a later step is pending, create the next step Run
   in the same job.
7. Aggregate terminal step states into terminal job states: all succeeded
   steps succeed the job; any failed, cancelled, or timed-out step fails it.
8. Add the reviewed terminal-status and cancellation API prerequisites,
   regenerate CRDs, and grant child Run patch RBAC.
9. Mark jobs `Skipped` when a failed or skipped predecessor blocks them; when a
   job succeeds, unblock jobs whose `pre` dependencies have all succeeded.
10. Finalize a non-cancelled WorkflowRun as `Succeeded` or `Failed` after all
    executable jobs settle.
11. Handle `spec.cancelRequested` by cancelling active child Runs and
    finalizing the WorkflowRun as `Cancelled`.
12. Add restart recovery tests that prove the controller can continue from
   `status.jobs[*].steps[*].runName` and child Run labels without duplicating
   Runs.
13. Add E2E coverage only after the controller can execute an inline
   WorkflowRun end to end.

## Expression Context

For v0.x, expressions should stay intentionally small. They should support only
string interpolation from known contexts:

| Context | Available from |
| --- | --- |
| `inputs.<name>` | resolved inputs for the current Workflow, Action, or WorkflowRun |
| `steps.<step>.outputs.<name>` | previous steps in the same job |
| `jobs.<job>.outputs.<name>` | completed dependency jobs in the same graph boundary |

Expressions should not access Kubernetes objects, environment variables,
secrets, files, arbitrary functions, or network resources. Secret handling
needs a separate design before it is exposed to Workflow expressions.

Evaluation must be deterministic and side-effect free. Unsupported syntax or
missing values should fail the WorkflowRun with a clear condition and message.

## Status Model

`WorkflowRun.status` owns execution state:

```yaml
status:
  phase: Running
  jobs:
    build:
      phase: Running
      pre: []
      steps:
        - name: package
          phase: Succeeded
          outputs:
            image: agent:v0.1.0
    test:
      phase: Waiting
      pre:
        - build
      steps:
        - name: unit
          phase: Pending
```

`Workflow.status` and `Action.status` should contain definition-level
conditions only, such as validation or readiness. They should not contain
per-execution job or step state.

The first implementation stores only lightweight DAG edges and ordered step
status for inline `WorkflowRun.spec.jobs`. It does not store full job specs,
step commands, environment, or source data in status.

## Component Boundaries

| Component | Responsibility |
| --- | --- |
| WorkflowRun controller | Expands inline jobs, resolves reusable Workflow and Action references, creates child Runs, and updates execution status. |
| Workflow controller | Validates reusable Workflow definitions and exposes definition conditions. |
| Action controller | Validates reusable Action definitions and exposes definition conditions. |
| Scheduler | Schedules child Runs only. It does not know about Workflow reuse. |
| runtimed | Executes child Runs only. It does not know about Workflow reuse. |

## Breaking Change

This is a breaking API change from the current experimental Workflow model:

- current `Workflow` execution instances become `WorkflowRun`;
- `Workflow` becomes reusable definition only;
- no compatibility shim is required because Workflow is still experimental and
  not part of a stable API promise.

Docs, examples, CLI verbs, CRDs, and E2E tests must be updated together when
the implementation lands.

## Implementation Sequence

1. Add this design document and review the API shape.
2. Add `WorkflowRun` API types, CRD validation, status, and controller skeleton.
3. Change `Workflow` API types to reusable definitions.
4. Add `Action` API types, CRD validation, status, and controller skeleton.
   Namespace-local resolution, input binding, output propagation, and
   WorkflowRun execution are separate follow-up implementation steps.
5. Implement lightweight DAG edge snapshotting and namespace-local top-level
   `WorkflowRun.spec.uses` resolution.
6. Implement input binding for top-level reusable Workflow calls.
7. Implement inline WorkflowRun first-step Run creation for ready jobs.
8. Refactor WorkflowRun controller reconciliation into a
   load/calculate/apply/patch structure with default status projection and
   external side effects represented as actions.
9. Implement child Run status observation and step status updates.
10. Define and review child failure, cancellation, dependency propagation, and
    WorkflowRun terminal-status semantics: independent jobs continue, blocked
    dependents are `Skipped`, and terminal status is aggregated after all
    executable jobs settle.
11. Implement next-step creation after observed step success.
12. Implement job terminal-state aggregation from observed step states.
13. Add terminal-status and cancellation API prerequisites, regenerated CRDs,
    and child Run patch RBAC.
14. Implement failed-dependency propagation to `JobSkipped`.
15. Implement WorkflowRun terminal aggregation.
16. Implement WorkflowRun cancellation propagation.
17. Verify controller restart recovery for in-progress inline WorkflowRuns,
    including child Run creation before status persistence.
18. Implement job-level reusable Workflow calls.
19. Implement step-level Action expansion.
20. Implement expression evaluation and output propagation.
21. Update CLI verbs and docs to use `WorkflowRun` for execution.
22. Add E2E coverage for inline `WorkflowRun`, reusable Workflow calls, Action
   calls, validation failures, output propagation, and controller restart
   recovery from the status DAG edges.
23. Update the final v0.x demos after the reusable model is implemented.

Current implementation status:

- `WorkflowRun`, `Workflow`, and `Action` API skeletons exist.
- `Workflow` is now a reusable definition skeleton and no longer executes
  child Runs.
- Inline WorkflowRuns initialize `status.jobs[*].pre` and ordered
  `status.jobs[*].steps`.
- Top-level `WorkflowRun.spec.uses` resolves a same-namespace reusable
  Workflow and initializes `status.jobs` from the referenced Workflow jobs.
  Missing references fail the WorkflowRun before child Runs are created.
- Top-level reusable Workflow calls bind string inputs early: defaults are
  applied, missing required inputs fail, and unknown `with` keys fail. Bound
  values are not evaluated into child Runs until WorkflowRun execution lands.
- Inline and resolved reusable Workflow job DAGs reject unknown dependencies
  and multi-job cycles before status graph initialization or child Run creation.
- Stale E2E stubs for the old Workflow execution model have been removed so
  E2E stays focused on behavior that should still pass during the migration.
- Inline WorkflowRuns create first-step and next-step child Runs for runnable
  jobs and record child Run names in ordered step status.
- WorkflowRuns observe terminal child Run phases, copy them into matching step
  status, aggregate terminal job phases, and finalize after all jobs settle.
  Any failed job fails the WorkflowRun; otherwise it succeeds, including when
  remaining jobs are skipped.
- WorkflowRun cancellation stops new child Run creation, idempotently requests
  cancellation for active child Runs, and finalizes as `Cancelled` after they
  settle. Jobs that never started retain their `Pending` or `Waiting` phase.
- Restart recovery is verified across the create-before-status-patch failure
  window: a replacement controller discovers child Runs through durable labels,
  repairs step status, and continues terminal observation without duplicates.
- Old Workflow execution E2E coverage is skipped until WorkflowRun execution
  lands.
