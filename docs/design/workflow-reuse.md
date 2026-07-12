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
- failed, cancelled, or timed-out child Runs fail the owning WorkflowRun unless
  a future retry/continue-on-error API explicitly changes that behavior.

The first version should support one execution strategy:

1. Accept the WorkflowRun and set `status.phase=Pending`.
2. Resolve references and bind inputs.
3. Persist resolved predecessor job edges in `status.jobs[*].pre`.
4. Start ready jobs by creating the first step Run for each job.
5. When a step Run succeeds, collect outputs and create the next step Run.
6. When all steps in a job succeed, evaluate job outputs and mark the job
   succeeded.
7. When all jobs succeed, evaluate WorkflowRun outputs and mark the WorkflowRun
   succeeded.

This deliberately avoids adding a separate WorkflowRunInvocation API. Child
Runs remain the durable execution records, and scheduler/runtimed continue to
operate only on Runs.

The WorkflowRun controller should keep reconciliation structured as
load/plan/apply: load the WorkflowRun and all child Runs, derive the current
state, compare it with the desired state, and plan exactly one operation. It
then applies that operation and patches WorkflowRun status. A reconciliation
must not loop through multiple operations or create multiple child Runs before
the status update. This makes each transition durable and restart-safe, and
keeps new execution states explicit as child Run observation, next-step
creation, restart recovery, and reusable call expansion land.

Inline WorkflowRun execution should land in small, reviewable steps:

1. Before changing execution behavior, audit the existing E2E tests. Remove or
   update stale cases that still exercise the old Workflow execution model so
   `make e2e` can stay passing throughout the migration.
2. Create only the first child Run for each ready inline job, record the child
   Run name on the matching ordered step status, and make creation idempotent
   by discovering existing child Runs through labels.
3. Before adding more execution states, refactor the WorkflowRun controller
   into a load/plan/apply state-machine shape: load the WorkflowRun and related
   resources, derive the current state, switch on that state to produce the
   intended actions, then apply Kubernetes writes.
4. Watch or reconcile child Runs owned by a WorkflowRun and copy terminal child
   Run phase into the matching step status.
5. When a step succeeds, create the next step Run in the same job; when a step
   fails, is cancelled, or times out, fail the job and WorkflowRun.
6. When all steps in a job succeed, mark the job succeeded and unblock jobs
   whose `pre` dependencies have all succeeded.
7. When all jobs succeed, mark the WorkflowRun succeeded; if any dependency
   job fails, fail dependent jobs without creating child Runs.
8. Add restart recovery tests that prove the controller can continue from
   `status.jobs[*].steps[*].runName` and child Run labels without duplicating
   Runs.
9. Add E2E coverage only after the controller can execute an inline
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
8. Refactor WorkflowRun controller reconciliation into a load/plan/apply
   state-machine structure.
9. Implement child Run status observation and step status updates.
10. Implement next-step creation, job terminal handling, and WorkflowRun
   terminal handling.
11. Implement controller restart recovery for in-progress inline WorkflowRuns.
12. Implement job-level reusable Workflow calls.
13. Implement step-level Action expansion.
14. Implement expression evaluation and output propagation.
15. Update CLI verbs and docs to use `WorkflowRun` for execution.
16. Add E2E coverage for inline `WorkflowRun`, reusable Workflow calls, Action
    calls, validation failures, output propagation, and controller restart
    recovery from the status DAG edges.
17. Update the final v0.x demos after the reusable model is implemented.

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
- Stale E2E stubs for the old Workflow execution model have been removed so
  E2E stays focused on behavior that should still pass during the migration.
- Inline WorkflowRuns create first-step child Runs for ready inline jobs and
  record the child Run name in ordered step status. Child Run result
  observation and next-step creation are still follow-up work.
- Old Workflow execution E2E coverage is skipped until WorkflowRun execution
  lands.
