# Job-Level Reusable Workflow Execution

Status: **Accepted**

This document defines the v0.x execution boundary for job-level reusable
Workflows.

## Decision

`WorkflowRun` is an execution instance with inline jobs only. It never refers
to a reusable `Workflow` at its root. A reusable `Workflow` is a template:

- `krt workflow trigger <name>` reads the template, validates and renders its
  inputs, then creates an inline `WorkflowRun`;
- a job with `uses` is a reusable Workflow call;
- when that call becomes runnable, its parent creates an inline child
  `WorkflowRun` from the referenced template;
- every WorkflowRun owns its own immutable execution snapshot and reconciles
  only its direct jobs and direct child WorkflowRuns.

This makes nested reuse recursive without requiring one controller to carry a
root-wide execution tree. A parent sees each reusable call as one job. The
child owns all jobs that were expanded from that call.

## Execution Topology

The model has simple ownership:

```text
WorkflowRun release
  direct job: build
  direct call job: deploy
    WorkflowRun release-call-deploy
      direct job: apply
      direct call job: verify
        WorkflowRun release-call-deploy-call-verify
          direct job: smoke
```

Each controller only creates and observes objects directly owned by the
WorkflowRun it reconciles. Parent/child state propagation, cancellation,
artifacts, and future PersistentWorkspace boundaries therefore remain local.

## API Shape

`WorkflowRun.spec.jobs` is required. `WorkflowRun.spec.uses` and
`WorkflowRun.spec.with` are removed. A direct `kubectl create` must provide
inline jobs.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: release
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: package
          run: make package
    deploy:
      needs: [build]
      uses: deploy-workflow
      with:
        environment: ${{ jobs.build.outputs.environment }}
```

The supported trigger path for a reusable template is:

```text
krt workflow trigger deploy-workflow --input environment=staging
  -> validates template inputs
  -> renders inputs into inline jobs
  -> creates WorkflowRun
```

The resulting WorkflowRun contains the rendered jobs, not a template reference.
This guarantees that later template edits cannot alter an already-created root
execution.

## Calling a Reusable Workflow

When `deploy` is runnable, the parent controller:

1. Loads `deploy-workflow` from the same namespace.
2. Renders its `with` values from the caller context and validates the callee
   inputs.
3. Renders `inputs.*` into the callee jobs.
4. Creates a direct child WorkflowRun with those inline jobs and one
   `kruntimes.io/workflow-output.<name>` annotation for each frozen source
   Workflow output expression.
5. Sets an owner reference and records the child name in
   `parent.status.jobs.deploy.workflowRunName`.

The child begins normally and may itself create direct child WorkflowRuns for
its own `uses` jobs. Its parent does not inspect or own those grandchildren.

Calls are deliberately **late-bound**: a referenced Workflow is read when its
caller job becomes runnable. A template update before that point affects a
child that has not yet been created. Once the child exists, its rendered jobs
and snapshot are immutable. Explicit template versioning can provide earlier
binding in a future API.

## Local Snapshot and Output Contract

Every WorkflowRun owns one `ControllerRevision`, named deterministically from
its own UID and recorded in `status.snapshotName`. Its data contains exactly:

- `spec`: the accepted inline `WorkflowRun.spec`, including its local job
  topology;

```yaml
apiVersion: apps/v1
kind: ControllerRevision
metadata:
  name: release-call-deploy-snapshot-8d91c3f4
  ownerReferences:
    - apiVersion: kruntimes.io/v1alpha1
      kind: WorkflowRun
      name: release-call-deploy
data:
  spec:
    jobs:
      apply:
        runs-on: bash
        steps: [{ name: deploy, run: deploy --environment=staging }]
```

For a child materialized from a reusable Workflow, the controller writes the
frozen source output contract to the child at creation time:

```yaml
metadata:
  annotations:
    kruntimes.io/workflow-output.endpoint: ${{ jobs.apply.outputs.endpoint }}
```

The child initializes its own snapshot during its own reconciliation. The
output annotations remain the contract accompanying its inline jobs, so the
parent can evaluate them against `child.status.jobs` without loading or owning
the child's ControllerRevision. Reading a mutable current Workflow after a
child completes would make the same execution produce different parent output
values after a template edit.

A snapshot is owned and used only by its own WorkflowRun.

## Workflow Call Graph Validation

Workflow reuse must not create an unbounded chain such as `A -> B -> A`. Cycle
validation happens while resolving reusable Workflow definitions, before any
WorkflowRun is created for the selected template or call job.

`krt wf trigger A` recursively reads every namespace-local Workflow reachable
from `A` through job-level `uses`. It validates missing references, cycles, and
the initial maximum nesting depth of 8 before rendering inputs and creating the
inline root WorkflowRun. For `A -> B -> A`, triggering fails with a
deterministic `workflow call cycle: A -> B -> A` error and creates no
WorkflowRun.

The WorkflowRun controller applies the same graph validation when a ready
inline job references `uses: A`. It validates the graph rooted at `A` before
rendering inputs or creating the direct child WorkflowRun. A deterministic
validation failure marks only that call job `Failed`; it creates no child and
is not retried. Normal failed-dependency propagation then marks dependent jobs
`Skipped`, and ordinary WorkflowRun terminal aggregation determines the parent
phase.

The CLI and controller must share one graph-validation implementation and
error format so both paths make the same decision for the same Workflow graph.
The shared logic loads namespace-local Workflow definitions, performs a
depth-first traversal with the current name stack, and does not persist
provenance annotations, owner-chain metadata, or a root-wide execution tree.
Scheduler and runtimed behavior is unchanged.

## Inputs and Outputs

`JobStatus` gains a bounded `outputs` map. All job outputs, whether produced by
an inline job or by a reusable Workflow call, are exposed at the same path:

```yaml
status:
  jobs:
    deploy:
      phase: Succeeded
      workflowRunName: release-call-deploy
      outputs:
        endpoint: https://staging.example.com
```

For an inline job, the controller evaluates `JobSpec.outputs` after its steps
succeed, using the step outputs in Run status. For a reusable call, after the
child WorkflowRun succeeds, the parent reads the child's frozen
`kruntimes.io/workflow-output.<name>` annotations, evaluates them against
`child.status.jobs`, and writes the resulting values to the caller job's
`JobStatus.outputs`. It never reads the child's private snapshot.

Downstream rendering is uniform:

```yaml
jobs:
  notify:
    needs: [deploy]
    runs-on: bash
    steps:
      - name: send
        run: notify --endpoint '${{ jobs.deploy.outputs.endpoint }}'
```

Only declared, bounded, structured key-value outputs belong in status. Logs
and large files remain outside status and use the existing logging and artifact
mechanisms. Missing references or failed output evaluation fail the affected
job before its next dependent target starts.

## Status, Failure, and Cancellation

A reusable call job has `workflowRunName` and no step statuses. The parent
projects its direct child state as follows:

| Child WorkflowRun | Caller job |
| --- | --- |
| `Pending` or `Running` | `Running` |
| `Succeeded` and output evaluation succeeds | `Succeeded` |
| `Succeeded` and output evaluation fails | `Failed` |
| `Failed` | `Failed` |
| `Cancelled` outside parent cancellation | `Failed` |

On cancellation, each WorkflowRun requests cancellation only for its direct
child Runs and direct child WorkflowRuns. A parent becomes terminal only after
those direct children settle; recursive cancellation follows naturally through
owner watches and each child's own reconciliation.

## Controller Responsibilities

For every reconciliation, the WorkflowRun controller:

1. Loads the WorkflowRun, its local snapshot, direct child Runs, and direct
   child WorkflowRuns.
2. Derives local job and WorkflowRun status from those resources.
3. Calculates runnable local targets from the snapshot spec and completed
   dependency outputs.
4. Creates all independent ready targets: a Run for an inline step or a child
   WorkflowRun for a `uses` job.
5. Patches status only when the derived state changed.

The scheduler and runtimed continue to see only independent `Run` resources.
They have no Workflow reuse, snapshot, or output-contract behavior.

## Validation and Limits

- `WorkflowRun.spec.jobs` must be non-empty and is immutable after creation.
- `WorkflowRun` itself cannot contain `uses` or `with`.
- A call job has `needs`, `uses`, and optional `with`; it cannot contain
  `runs-on` or `steps`.
- Inputs and expression references are validated before creating a child.
- Reusable Workflow output names must be valid annotation suffixes, and their
  frozen output contract must fit within the Kubernetes annotation budget.
- Workflow cycles are detected while resolving the referenced Workflow graph
  before a root or child WorkflowRun is created. The initial maximum nesting
  depth is 8.
- Job and step outputs are bounded by CRD limits; artifacts are not outputs.

## Reusable Actions

This decision deliberately does not define Action execution. It establishes a
useful rule for that future work: reuse expands at the direct execution
boundary. An Action will be resolved into its caller step/Run, rather than
being added to a root-wide Workflow snapshot or controller traversal tree.

## Implementation Status

1. [x] Add the local WorkflowRun snapshot envelope and `JobStatus.outputs`.
2. [x] Implement `krt workflow
   trigger` as template input validation, rendering, and inline WorkflowRun
   creation.
3. [x] Implement direct child WorkflowRun creation with input rendering and frozen
   output contracts.
4. [x] Implement local job-output evaluation, child-output projection, restart
   recovery, and template-mutation semantics tests.
5. [x] Add E2E coverage for nested calls, `A -> B -> A` cycle rejection,
   output propagation, cancellation, and template updates before versus after
   child creation; add unit coverage for self-reference rejection.
6. [ ] Design Action expansion separately, using the same direct-boundary rule.
