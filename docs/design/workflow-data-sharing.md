# Workflow Data Sharing

This document describes a target v0.x design. It is not implemented yet.

The goal is to define how Workflow jobs and Runs share data without making
scheduler or runtimed understand Workflow-specific semantics. The design is
driven by the v0.x workflow demo target: job-to-job data should move through
artifacts, while Runs inside one job should be able to share a job-local
workspace when the Workflow controller asks for co-location.

## Current State

The current experimental Workflow API supports:

- jobs with `needs`;
- sequential steps inside a job;
- child Runs per step;
- bounded step outputs from `KRUNTIME_OUTPUTS`;
- cross-step and cross-job expression references for small string outputs.

It does not yet provide:

- first-class artifact inputs between jobs;
- a workspace object or lifecycle;
- Run affinity/anti-affinity for co-locating child Runs;
- explicit promotion of child Run artifact references into Workflow status;
- cleanup and permission boundaries for shared job-local workspaces.

## Goals

- Jobs exchange durable data through ArtifactStore-backed artifacts.
- Runs inside one Workflow job can share a job-local `PersistentWorkspace`.
- Workflow controller owns job/workflow semantics.
- Scheduler and runtimed stay workflow-agnostic. They expose generic placement
  and workspace primitives that other features can also use.
- The API keeps cross-job data durable, auditable, and independent of Runtime
  Pod placement.
- The design makes cleanup, failure recovery, and permission boundaries
  explicit before implementation.

## Non-Goals

- This is not a full replacement for Argo Workflows or Tekton.
- This does not add a general distributed filesystem.
- This does not make Runtime Pods safe for arbitrary hostile code.
- This does not require scheduler or runtimed to know about Workflows, jobs, or
  steps.
- This does not make job-local workspaces cross-node or cross-Pod by default.

## Data Sharing Model

There are two data-sharing paths:

| Boundary | Mechanism | Reason |
| --- | --- | --- |
| Job to job | ArtifactStore-backed artifacts | Durable, auditable, works across Runtime Pods and nodes. |
| Run to Run inside one job | `PersistentWorkspace` plus Run affinity | Fast local sharing for sequential steps in the same job. |

Small scalar values continue to use bounded outputs:

```text
step -> KRUNTIME_OUTPUTS -> Run.status.outputs -> Workflow status
```

Larger files should not be embedded in Workflow or Run status. They should move
through artifact references or a referenced workspace.

## PersistentWorkspace CRD

`PersistentWorkspace` represents a workspace boundary and lifecycle. It is not a
Workflow-specific object; Workflow is one consumer.

It does not select the underlying Kubernetes volume. A `PersistentWorkspace` is
bound to the workspace volume declared by the target `Runtime.spec.workspace`.
For the initial `RuntimePodLocal` mode, the workspace is implemented as a
subdirectory under that Runtime Pod's mounted `/workspace` volume.

Target shape:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: PersistentWorkspace
metadata:
  name: ci-build-workspace
spec:
  runtime: bash
  mode: RuntimePodLocal
  ttlSecondsAfterUnused: 3600
  cleanupPolicy: DeleteAfterTTL
status:
  phase: Bound
  runtime: bash
  boundPod: runtime-bash-7f587b4668-njcks
  path: /workspace/persistent/ci-build-workspace
  lastUsedTime: "2026-07-06T12:00:00Z"
```

The first supported mode should be `RuntimePodLocal`: the workspace lives on a
specific Runtime Pod and can be reused only by Runs scheduled to that Pod.

The durability and sharing characteristics come from the Runtime workspace
volume. If the Runtime workspace is an in-memory `emptyDir`, the
`PersistentWorkspace` is also Runtime-Pod-local and lost with the Pod. If the
Runtime workspace is backed by a PVC or another Kubernetes volume source in the
future, the workspace can inherit that backing store's durability and attachment
rules.

## Runtime Workspace Volume

Today `Runtime.spec.workspace` inlines Kubernetes `VolumeSource` fields, and the
controller creates the reserved `workspace` volume as `emptyDir` when no explicit
workspace volume source is set.

Target direction:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Runtime
metadata:
  name: bash
spec:
  workspace:
    persistentVolumeClaim:
      claimName: bash-workspace
```

The preferred API should inline Kubernetes `corev1.VolumeSource` fields under
`spec.workspace` instead of inventing a separate workspace volume model or
nesting another `volumeSource` object. `emptyDir` remains the default when no
explicit workspace volume source is set. EmptyDir options such as `sizeLimit`
should use the native `workspace.emptyDir.sizeLimit` shape instead of a
kruntimes-specific shorthand.

This Runtime workspace volume work is a prerequisite for durable or
PVC-backed `PersistentWorkspace` behavior. The first implementation can still
ship `RuntimePodLocal` against the existing `emptyDir` behavior, but the design
should not bake in emptyDir as the only backing store.

## Run Workspace Reference

Runs should be able to reference a workspace through a small typed object
reference. `PersistentWorkspace` is the default kind for this API, but the
reference shape leaves room for future workspace providers:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: ci-build-package
spec:
  runtime: bash
  workspace:
    name: ci-build-workspace
    kind: PersistentWorkspace
    apiGroup: kruntimes.io/v1alpha1
  source:
    inline: |
      tar -czf "$KRUNTIME_ARTIFACTS_DIR/dist.tgz" src
```

`kind` and `apiGroup` are optional. When omitted, they default to
`PersistentWorkspace` and `kruntimes.io/v1alpha1`.

runtimed prepares the referenced workspace path before execution and cleans only
per-Run temporary state after the Run finishes. The workspace lifecycle is owned
by the `PersistentWorkspace` controller.

## Run Affinity

Run affinity should use Kubernetes-style concepts because users already
understand affinity and anti-affinity from Pods.

Target shape:

```yaml
spec:
  affinity:
    runAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              workflows.kruntimes.io/workflow: ci-data-sharing-demo
              workflows.kruntimes.io/job: build
          topologyKey: kruntimes.io/runtime-pod
```

The exact type names may change during API design, but the concepts should stay
close to Kubernetes:

- required vs preferred rules;
- label selectors;
- topology keys;
- affinity and anti-affinity.

For job-local workspace sharing, the Workflow controller can create the first
Run in a job, bind or discover the workspace, and add required affinity to later
Runs in the same job. The scheduler only evaluates generic Run placement rules.

## Workflow API

Target workflow shape:

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: ci-data-sharing-demo
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: checkout
          run: |
            mkdir -p src
            echo 'print("hello")' > src/app.py
        - name: test
          run: |
            test -f src/app.py
            echo "tests=passed" >> "$KRUNTIME_OUTPUTS"
        - name: package
          run: |
            mkdir -p "$KRUNTIME_ARTIFACTS_DIR"
            tar -czf "$KRUNTIME_ARTIFACTS_DIR/dist.tgz" src
    deploy:
      runs-on: bash
      needs:
        - build
      steps:
        - name: verify-artifact
          artifacts:
            - from: jobs.build.artifacts.dist.tgz
              path: ./dist.tgz
          run: |
            tar -tzf dist.tgz
            echo "artifact verified"
```

Workflow spec does not expose workspace controls for this default job-local
sharing model. When a Workflow job runs multiple steps, the Workflow controller
creates and owns the job-local `PersistentWorkspace`, and its spec is controlled
by controller configuration. Users should not need to choose workspace names,
storage modes, TTLs, or cleanup policies in the common case.

This shape separates:

- job-local workspace sharing for `checkout`, `test`, and `package`;
- automatic artifact upload from `$KRUNTIME_ARTIFACTS_DIR` at job scope;
- explicit artifact transfer from `jobs.build.artifacts.dist.tgz` into
  `deploy`;
- bounded scalar outputs for expressions.

Within a job, steps share the same `KRUNTIME_ARTIFACTS_DIR` namespace. Artifact
references therefore do not include the producing step name. A downstream job
imports an artifact with `jobs.<job-id>.artifacts.<filename>`.

## Status Model

Workflow status should expose compact artifact references, not artifact
contents:

```yaml
status:
  jobs:
    build:
      artifacts:
        dist.tgz:
          name: dist.tgz
          uri: s3://kruntimes-artifacts/workflows/ci-data-sharing-demo/jobs/build/dist.tgz
      steps:
        package:
          runName: ci-data-sharing-demo-build-package
          outputs:
            tests: passed
```

Workflow status should not expose workspace binding details. Those details live
on `PersistentWorkspace` objects for operators. Workflow should surface only
user-relevant conditions and messages, such as a job waiting for local
workspace capacity or failing because its controller-owned workspace was lost.

## Component Boundaries

| Component | Responsibility |
| --- | --- |
| Workflow controller | Interprets job/step semantics, creates job-local workspaces from controller defaults, creates child Runs, wires artifact inputs, promotes outputs/artifact refs into Workflow status. |
| PersistentWorkspace controller | Owns workspace lifecycle, binding to Runtime workspace volumes, status, TTL, and cleanup. |
| Scheduler | Applies generic Runtime capacity and Run affinity/anti-affinity. It does not know about Workflows. |
| runtimed | Prepares referenced workspace paths, stages artifact inputs, collects artifact outputs, and cleans per-Run temporary state. It does not know about Workflows. |
| ArtifactStore | Stores durable artifacts outside etcd. |

## Failure and Recovery

- If a Runtime Pod disappears, `RuntimePodLocal` workspaces backed by that Pod's
  workspace volume become unavailable.
- Runs that require an unavailable workspace should stay Pending or fail with a
  clear workspace condition, depending on retry policy and controller decision.
- The Workflow controller should surface workspace-related failures in Workflow
  conditions or messages without exposing workspace controls in Workflow spec.
- Workspace cleanup must not depend on the Runtime Pod still existing.
- Artifact transfer between jobs should remain valid after Runtime Pod loss
  because artifacts are stored outside the Pod.

## Security and Isolation

`PersistentWorkspace` increases the blast radius within its boundary. The
initial model should treat shared workspace users as mutually trusted.

Required safeguards:

- namespace-scoped workspace references;
- owner references from auto-created workspaces to the Workflow or WorkflowRun;
- labels for workflow, job, and controller ownership;
- validation that rejects absolute paths and path traversal in artifact inputs;
- explicit cleanup policy and TTL;
- documented warning that shared workspace is not hostile-code isolation.

## Implementation Sequence

1. Add this design document and review the API shape.
2. Extend `Runtime.spec.workspace` to inline Kubernetes `VolumeSource` fields,
   while preserving the current emptyDir default behavior.
3. Add `PersistentWorkspace` API types, CRD validation, status, and controller
   skeleton. Binding to Runtime Pods, Run workspace references, and cleanup are
   separate follow-up implementation steps.
4. Add Run `workspace` reference fields.
5. Add Kubernetes-style Run affinity/anti-affinity fields.
6. Update scheduler placement to respect required/preferred Run affinity while
   keeping no-capacity Runs Pending.
7. Update runtimed workspace preparation and cleanup for referenced
   workspaces.
8. Add Workflow step artifact input fields and job-scoped artifact status.
9. Promote child Run artifact refs into Workflow status.
10. Add E2E coverage for Runtime workspace volume sources, job-local workspace
   sharing, job-to-job artifact
   passing, Runtime Pod loss, cleanup, and permission boundaries.
