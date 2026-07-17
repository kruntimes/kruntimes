# Job-Level Reusable Workflow Execution

Status: **Proposed for review**

This document refines the job-level `uses` model introduced in
[Workflow Reuse](../workflow-reuse/). It defines an execution boundary that is
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

### Nested Reuse

Reusable Workflows may call other reusable Workflows. For example,
`deploy-workflow` can use `smoke-test` after it applies the deployment:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: deploy-workflow
spec:
  inputs:
    environment: { type: string, required: true }
  jobs:
    apply:
      runs-on: bash
      steps:
        - name: deploy
          run: deploy
    verify:
      needs: [apply]
      uses: smoke-test
      with:
        endpoint: https://staging.example.com
---
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: smoke-test
spec:
  inputs:
    endpoint: { type: string, required: true }
  jobs:
    smoke:
      runs-on: bash
      steps:
        - name: check
          run: check-service
```

The accepted `release` execution resolves this tree before it starts work:

```text
root                         WorkflowRun release
root/jobs/deploy             Workflow deploy-workflow
root/jobs/deploy/jobs/verify Workflow smoke-test
```

The root controller creates and observes only the child WorkflowRun for
`deploy`. That child executes `apply`, then creates and observes its own child
WorkflowRun for `verify`. Each WorkflowRun has a local jobs status map; a
parent call job succeeds only when its direct child WorkflowRun succeeds.
Consequently, `release.status.jobs.deploy` cannot succeed until the nested
`smoke-test` WorkflowRun has settled.

Cancellation and failure follow the same direct-parent boundary. Cancelling
`release` requests cancellation of its `deploy` child; that controller then
requests cancellation of `verify`. Conversely, a failed `smoke-test` makes
`verify` fail, then makes the `deploy` call fail in `release`. Controllers do
not need to traverse arbitrary descendants during normal reconciliation.

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

### Snapshot Storage

Each root WorkflowRun owns exactly one snapshot `ControllerRevision`.
`ControllerRevision.data` is a small envelope around direct serialized public
specs, not a second Workflow model:

- `root.spec` is always the exact accepted `WorkflowRun.spec`;
- `root.workflow`, when the root uses a reusable Workflow, is the exact
  resolved `Workflow.spec` for that reference; it is omitted for an inline
  root;
- `workflows[call-path]` is the exact `Workflow.spec` resolved for each
  job-level call.

The root revision name is recorded in `WorkflowRun.status.snapshotName`. A
nested child receives that name and its call path through reserved metadata,
then reads its definition from the same snapshot. `with` remains in the stored
caller `JobSpec`, exactly where the user wrote it; it is not copied into a
controller-specific record.

For the `release` example above, the following is an abbreviated but complete
shape of the snapshot ControllerRevision. Its name is illustrative: the
controller derives it deterministically from the root WorkflowRun UID.

```yaml
# The root WorkflowRun owns the single snapshot revision.
apiVersion: apps/v1
kind: ControllerRevision
metadata:
  name: release-snapshot-root-8d91c3f4
  namespace: default
  labels:
    kruntimes.io/root-workflowrun-uid: 7e4d41cb-69c8-4fa1-8e31-f9135512c22b
  ownerReferences:
    - apiVersion: kruntimes.io/v1alpha1
      kind: WorkflowRun
      name: release
      uid: 7e4d41cb-69c8-4fa1-8e31-f9135512c22b
      controller: true
      blockOwnerDeletion: true
revision: 1
data:
  root:
    spec: # exact accepted WorkflowRun.spec
      jobs:
        build: { runs-on: bash, steps: [{ name: package, run: make package }] }
        deploy: { needs: [build], uses: deploy-workflow, with: { environment: staging } }
        notify: { needs: [deploy], runs-on: bash, steps: [{ name: send, run: send-notification }] }
  workflows:
    root/jobs/deploy: # exact Workflow.spec resolved for this call
      inputs:
        environment: { type: string, required: true }
      jobs:
        apply: { runs-on: bash, steps: [{ name: deploy, run: deploy }] }
        verify: { needs: [apply], uses: smoke-test, with: { endpoint: https://staging.example.com } }
    root/jobs/deploy/jobs/verify: # exact Workflow.spec resolved for the nested call
      inputs:
        endpoint: { type: string, required: true }
      jobs:
        smoke: { runs-on: bash, steps: [{ name: check, run: check-service }] }
```

The root snapshot always stores the direct `WorkflowRun.spec`. For a root
`spec.uses`, it also stores that resolved `Workflow.spec` at `root.workflow`.
It stores the resolved `Workflow.spec` for `root/jobs/deploy`, and the nested
`Workflow.spec` for `root/jobs/deploy/jobs/verify`. When `deploy` becomes runnable, the controller
evaluates its stored `with.environment` in the caller context and creates a
child WorkflowRun with the same snapshot name and the `root/jobs/deploy` call
path. That child later resolves `verify` from the same snapshot. Neither
controller reads the current `deploy-workflow` or `smoke-test` object again.

A job-level call path appends `/jobs/<job-name>` to its caller path. Job names
cannot contain `/`, so this is unambiguous and stable across reconciliation.
The path is a YAML/JSON map key in `ControllerRevision.data`, where `/` is
valid; it is not a Kubernetes label, annotation, or object name. JSON Pointer
would escape `/` as `~1`, but snapshot data is immutable and is never patched
by path. The resolver rejects a stored spec that exceeds Kubernetes object size
limits before it creates an execution child.

The snapshot is stored outside status in a WorkflowRun-owned
`ControllerRevision`. Kubernetes API validation makes a successfully created revision's
`data` immutable. Revision metadata records the root WorkflowRun UID for
lookup and garbage collection; child WorkflowRun metadata records its call path.
Revision data contains no runtime results or secret material.

Snapshot names are deterministic from the root UID. Creation is idempotent:
after a partial failure, reconciliation verifies and reuses a matching
immutable revision before accepting the WorkflowRun.

`WorkflowRun.status.snapshotName` records the root ControllerRevision name.
Status does not copy full job specs, scripts, or environment values. If a
snapshot exceeds 1 MiB after serialization, resolution fails before execution.
The explicit limit leaves headroom below etcd's commonly configured 1.5 MiB
request limit; cluster operators may configure different API server or etcd
limits.

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
4. Store the complete resolved tree in one immutable snapshot revision.
5. Reject missing definitions, invalid inputs, unsupported shapes, and direct
   or indirect Workflow call cycles.
6. Persist immutable snapshot ControllerRevisions.
7. Initialize lightweight status from the snapshot and set `Accepted=True`.

Initial safety limits are a maximum call depth of 8 and at most 64 reusable
Workflow call nodes per root execution. Exceeding either limit rejects the
WorkflowRun before child creation. These bounds prevent accidental recursive
expansion even when the graph is acyclic.

## Reconciliation

The controller keeps the existing load/calculate/apply/patch structure.

Loaded resources add:

- the root snapshot ControllerRevision;
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
- grant the WorkflowRun controller namespace-scoped `apps` ControllerRevision
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
   metadata, generated CRDs, and ControllerRevision/child-WorkflowRun RBAC.
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
