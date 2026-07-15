# Job-Level Reusable Workflow Execution

Status: **Proposed for review**

This document refines the job-level `uses` model introduced in
[Workflow Reuse](workflow-reuse.md). It defines an execution boundary that is
deterministic across controller restarts and reusable `Workflow` updates.

## Problem

The existing design says that a job-level reusable Workflow call expands into
a nested job group, but it does not define:

- how caller `needs` edges connect to callee roots and leaves;
- how nested jobs appear in `WorkflowRun.status.jobs`;
- how nested execution paths fit Kubernetes label limits;
- how cancellation, failure, outputs, and restart recovery cross the call
  boundary;
- how an accepted WorkflowRun avoids observing later changes to a referenced
  `Workflow`.

The last point is already an implementation gap for top-level
`WorkflowRun.spec.uses`: the controller initializes status from the referenced
definition, but execution still reads `WorkflowRun.spec.jobs`, which is empty
for that shape.

## Decision

A job-level reusable Workflow call is represented by a child `WorkflowRun`.
The caller job remains one logical dependency and output node. The called
Workflow's jobs remain local to the child WorkflowRun instead of being flattened
into the parent status map.

This choice adds one Kubernetes object and controller transition per call. That
cost is outside the hot Run scheduling path and buys explicit ownership,
bounded names, local status, recursive cancellation, and independent
workspace/artifact boundaries.

The first implementation remains namespace-local and does not add a new public
invocation CRD.

## User Model

The API shape remains direct:

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
        environment: staging
    notify:
      needs: [deploy]
      runs-on: bash
      steps:
        - name: send
          run: send-notification
```

`deploy` behaves as one caller job:

- it starts only after `build` succeeds;
- its child WorkflowRun may execute multiple jobs in parallel;
- it succeeds only when the child WorkflowRun succeeds;
- `notify` waits for the complete child WorkflowRun, not one internal leaf;
- called Workflow outputs become outputs of the `deploy` job;
- called jobs do not share a workspace with caller jobs.

This follows the useful part of the
[GitHub Actions reusable-workflow model](https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows):
reusable Workflows are called at job level, while their declared outputs are
consumed through the caller job. The kruntimes implementation remains
Kubernetes-native and namespace-local.

## Parent and Child Status

`JobStatus` gains an optional `workflowRunName`, symmetric with
`StepStatus.runName`:

```yaml
status:
  jobs:
    deploy:
      phase: Running
      pre: [build]
      workflowRunName: release-deploy-7f6d98c04a
```

A call job has `workflowRunName` and no step statuses. The child WorkflowRun
contains its own local `status.jobs` map. Nested names therefore never need to
be encoded into a parent map key or a child Run label.

The child WorkflowRun has a controller owner reference to its parent. Its name
is deterministic from the parent UID and caller job name. Reserved labels and
annotations identify the root WorkflowRun UID, snapshot, and call path for
recovery and diagnostics.

The parent controller watches owned child WorkflowRuns and projects state as
follows:

| Child WorkflowRun | Caller job |
| --- | --- |
| `Pending` or `Running` | `Running` |
| `Succeeded` | `Succeeded` |
| `Failed` | `Failed` |
| `Cancelled` outside parent cancellation | `Failed` |

During parent cancellation, the controller requests cancellation on active
child WorkflowRuns and direct child Runs. The parent becomes `Cancelled` only
after both kinds of children settle. Jobs that never started retain their
existing `Pending` or `Waiting` phase.

## Immutable Execution Snapshot

Accepted executions must not read mutable `Workflow` definitions again. Before
initializing `status.jobs` or creating any child, the controller recursively
resolves the complete namespace-local Workflow call tree and writes an
immutable execution snapshot.

The snapshot is stored outside status in WorkflowRun-owned, immutable
ConfigMaps:

- a small index ConfigMap maps stable call paths to definition snapshots;
- definition ConfigMaps contain normalized Workflow inputs, outputs, and jobs;
- every entry records source name, UID, generation, and resource version;
- call nodes retain unevaluated `with` expressions for later evaluation in the
  caller context;
- ConfigMaps contain no runtime results or secret material;
- owner references provide garbage collection with the root WorkflowRun.

Snapshot names are deterministic and content-addressed. Creation is idempotent:
after a partial failure, reconciliation verifies and reuses matching immutable
ConfigMaps before creating missing entries. Unreferenced partial snapshots are
deleted before the WorkflowRun is accepted.

`WorkflowRun.status.snapshotName` records the index ConfigMap name. Status does
not copy full job specs, scripts, or environment values. If a snapshot cannot
fit Kubernetes object limits, resolution fails before execution.

Once `snapshotName` is persisted, all reconciliation reads the snapshot rather
than current `Workflow` objects. A child WorkflowRun inherits the root snapshot
and a call-path annotation, so nested calls use the same immutable tree.

The root WorkflowRun spec is also execution input. After creation, `jobs`,
`uses`, and `with` are immutable. `cancelRequested` may transition from false to
true but not back to false. These rules require CRD transition validation.

## Resolution and Cycle Detection

Snapshot resolution happens before any execution child is created:

1. Select inline root jobs or resolve top-level `spec.uses`.
2. Validate and bind literal top-level inputs.
3. Recursively resolve every job-level `uses` in the same namespace.
4. Record each definition version and call path in the snapshot.
5. Reject missing definitions, invalid inputs, unsupported shapes, and direct
   or indirect Workflow call cycles.
6. Persist immutable snapshot ConfigMaps.
7. Initialize lightweight status from the snapshot and set `Accepted=True`.

Initial safety limits are a maximum call depth of 8 and at most 64 reusable
Workflow call nodes per root execution. Exceeding either limit rejects the
WorkflowRun before child creation. These bounds prevent accidental recursive
expansion even when the graph is acyclic.

## Reconciliation

The controller keeps the existing load/calculate/apply/patch structure.

Loaded resources add:

- the root snapshot index and definition ConfigMaps;
- direct child WorkflowRuns owned by the reconciled WorkflowRun;
- direct child Runs for inline jobs.

Planning can produce one of two runnable target kinds:

- an inline step target creates or reuses a child Run;
- a reusable call target creates or reuses a child WorkflowRun.

One reconcile may start all currently ready targets in parallel, but at most
one target per caller job. Created child identities are recorded in desired
status before the single status patch. Deterministic names and owner watches
repair create-before-status-patch failures after restart.

Top-level `spec.uses` does not create a wrapper child WorkflowRun. The root
WorkflowRun initializes and executes the root jobs from its immutable snapshot.
Only job-level calls create child WorkflowRuns.

## Job Shape and Outputs

A call job supports `needs`, `uses`, and `with`. It does not support `runs-on`,
`steps`, or caller-defined `outputs` in v0.x. The called Workflow selects
runtimes for its own jobs and exposes outputs through `Workflow.spec.outputs`.

Input expressions are evaluated only when the call job becomes runnable, using
the caller's completed dependency outputs. The resulting concrete values are
placed in the child WorkflowRun's immutable `with` map. Secret inputs remain
out of scope until a separate secret-handling design is reviewed.

Output evaluation remains part of the existing expression/output propagation
story. This design only fixes the boundary: child Workflow outputs are promoted
to caller job outputs, and downstream jobs access them through the normal jobs
context.

## Component Boundaries

- The WorkflowRun controller owns resolution, snapshots, child WorkflowRuns,
  child Runs, status projection, and cancellation propagation.
- The Workflow and Action controllers continue to validate definitions only.
- Scheduler and runtimed continue to see only independent Runs and remain
  unaware of Workflow calls and snapshots.
- PersistentWorkspace and ArtifactStore behavior remains attached to the
  called jobs, not to the synthetic caller job.

## API and RBAC Changes

Implementation requires review of these API and operational changes:

- add `WorkflowRun.status.snapshotName`;
- add `JobStatus.workflowRunName`;
- add WorkflowRun spec transition validation for immutable execution inputs and
  one-way cancellation;
- reject `runs-on`, `steps`, and caller-defined `outputs` on a `uses` job;
- reserve snapshot/call-path labels and annotations;
- grant the WorkflowRun controller namespace-scoped ConfigMap
  get/list/watch/create/delete permissions;
- watch owned child WorkflowRuns as well as child Runs.

## Rejected Alternatives

### Flatten callee jobs into parent status

Flattening avoids child WorkflowRun objects, but it requires synthetic caller
nodes, rewrites external dependency edges to callee roots and leaves, leaks
nested paths into labels, and mixes independent workspace/artifact boundaries
in one status map. It also makes nested cancellation and output aggregation
harder to explain.

### Read the current Workflow on every reconcile

This is simple but changes an in-progress execution when a reusable definition
is edited. It cannot provide deterministic retries or restart recovery.

### Copy resolved definitions into WorkflowRun status

This makes status large and may duplicate scripts and environment values.
Status should remain lightweight execution state, not an execution-spec store.

## Implementation Plan

1. API prerequisites: status references, transition validation, reserved
   metadata, generated CRDs, and ConfigMap/child-WorkflowRun RBAC.
2. Snapshot storage and recursive resolver with version capture, limits, input
   validation, and cross-Workflow cycle detection.
3. Execute top-level `spec.uses` from the immutable snapshot, fixing the current
   status-only resolution gap.
4. Create and observe child WorkflowRuns for ready job-level calls, including
   dependency and terminal propagation.
5. Add restart, mutation, nested-call, cancellation, and invalid-graph tests.
6. Integrate expression inputs and child Workflow outputs in the existing
   output propagation work.
7. Add E2E coverage, then update the final workflow demo.
