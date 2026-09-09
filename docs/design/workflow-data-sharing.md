# Workflow Data Sharing

This document describes the v0.x design. Its API prerequisites, RuntimePodLocal
binding, scheduler admission, runtimed workspace preparation, job-local
Workflow workspaces, and job-to-job artifact transfer are implemented.

RuntimePodLocal binding, scheduler fencing, and runtimed preparation:
**Implemented**

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
- `Runtime.spec.workspace` with an inline Kubernetes `VolumeSource` and an
  `emptyDir` default;
- `PersistentWorkspace` API types, CRD validation, status, and controller
  binding lifecycle with UID fencing;
- generic Run workspace references and Kubernetes-style Run affinity fields.
- workspace-aware scheduler filtering and Pending Run wakeups on workspace
  changes;
- controller-managed workspace lifecycle and cleanup, including explicit
  deletion and unused-TTL E2E coverage;
- identity-based workspace authorization through admission-time `use` reviews
  and a fenced controller ServiceAccount path for verified Workflow child Runs.

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

### Artifact Input Contract

Artifact transfer has two layers so that the Workflow controller never copies
data and runtimed never needs Workflow knowledge:

1. A generic Run artifact input contains an immutable `ArtifactRef` and a
   relative destination path. Before execution, runtimed opens the reference
   through its configured `ArtifactStore` and safely stages it below the Run
   working directory. File artifacts are copied to the destination path;
   directory artifacts are extracted into it without permitting symlinks or
   path traversal. Extracted regular files use a normalized `0640` mode;
   the producer's owner-execute bit is retained, yielding `0740` for an
   executable file. Group, other, write, and special mode bits are never
   inherited from artifact metadata.
2. A Workflow step expresses the source as
   `jobs.<job-id>.artifacts.<artifact-name>`. When its job becomes ready, the
   Workflow controller resolves that name from the producing job's compact
   artifact status and materializes the generic Run input. Neither the Run API
   nor runtimed contains a Workflow, job, or step identifier.

The producing job must be a `needs` dependency of the consuming job. A missing
artifact after all dependencies have succeeded is a deterministic Workflow job
failure, rather than a request that waits forever.

An `ArtifactRef` identifies storage coordinates, not authorization. In v0.x,
jobs that exchange artifacts must use compatible `Runtime.spec.artifactStore`
configuration: the consuming runtimed must be able to open the producer's
reference with its own credentials and store scope. This supports shared PVC
filesystem stores and shared S3 bucket/prefix access. A project-wide artifact
relay with independent credentials is a future feature; the Workflow controller
does not proxy artifact bytes.

Artifact names form one job-local namespace. Job status projects the most
recent successful child Run reference for each name, so a later sequential step
can intentionally replace an earlier artifact. Consumers read the final
producer-job reference after that job succeeds.

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
  boundPodUID: 2c24c1f0-9f8f-4f80-82d5-3dd16a12d1e6
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

### Proposed RuntimePodLocal Binding Lifecycle

The binding controller should use the following v0.x rules:

1. An unbound workspace waits while its referenced Runtime has no ready Runtime
   Pods. It does not consume or reserve Run capacity while waiting or after it
   is bound.
2. When candidates exist, the controller sorts ready Runtime Pods by
   `metadata.name` and selects one using a stable hash of the
   PersistentWorkspace UID. This spreads first bindings across ready Pods while
   keeping retries stable for the same candidate set; later scheduling work uses
   `status.boundPod` and `status.boundPodUID` rather than trying to repeat this
   selection.
3. The controller records `status.phase: Bound`, `status.runtime`,
   `status.boundPod`, immutable `status.boundPodUID`, and
   `status.path: /workspace/persistent/<workspace-name>`. It does not create
   the directory itself: runtimed creates it when a referenced Run starts.
4. A Bound workspace remains bound while its Pod exists, even if that Pod is
   temporarily not ready. The status conditions make the availability problem
   visible, and Runs referring to it stay Pending until later scheduler and
   runtimed work can use that binding safely.
5. If the bound Pod is deleted, no longer exists, or a same-name Pod has a
   different UID, the workspace becomes `Lost`. The controller must not
   silently bind it to another Pod: for `RuntimePodLocal`, that would make a
   caller observe a new empty directory as though it contained the original
   data. Recovery requires an explicit new workspace or a future reviewed
   recovery API.

Binding is metadata-only in this slice. TTL cleanup, filesystem deletion,
`lastUsedTime`, and Run admission/preparation remain separate follow-up work.

### Cleanup Protocol

`PersistentWorkspace` cleanup is a two-part protocol. The controller owns the
logical lifecycle; runtimed owns deletion of bytes in the Runtime Pod-local
mount. The controller must never use Pod exec or assume that a custom Runtime
implements a shell command merely to remove a workspace.

1. The PersistentWorkspace controller indexes Runs by `spec.workspace.name`.
   A workspace is active while any referencing Run is non-terminal. When the
   last active Run becomes terminal, the controller sets `status.lastUsedTime`
   once. A newly Bound workspace with no Runs starts its unused interval when
   it becomes Bound.
2. `cleanupPolicy: DeleteAfterTTL` starts cleanup only when
   `ttlSecondsAfterUnused` is set and has elapsed. A nil TTL deliberately means
   no automatic deletion. `Retain` never starts automatic cleanup.
3. On a cleanup request, the controller changes the workspace to `Released`,
   then requests Kubernetes deletion while keeping a dedicated workspace
   finalizer. A Released workspace is terminal for scheduling: no new Run may
   claim it.
4. The runtimed instance on the recorded `status.boundPod` watches only
   workspace objects bound to its own Pod. After independently confirming that
   no local non-terminal Run references the workspace, it removes exactly
   `/workspace/persistent/<workspace-name>` and removes that finalizer. It does
   not write Workspace status. Runtimed does not need Workflow knowledge or any
   runtime-server extension for this operation.
5. The controller is the only status writer. If its bound Pod has disappeared,
   the workspace is already `Lost`; there is no remaining Pod-local data to
   remove, so the controller removes the finalizer without waiting for
   runtimed.

All bound workspaces use the same deletion protocol, regardless of cleanup
policy or deletion reason: the finalizer keeps the object until the bound
runtimed removes it after physical removal. `Retain` disables automatic TTL
deletion only; an explicit deletion still removes its Pod-local directory. If
a live bound Pod is unavailable, cleanup remains pending rather than risking a
directory on another Pod; deletion of that Pod transitions the workspace to
`Lost` and unblocks the finalizer.

### Permission Boundary Design

Kubernetes RBAC on the `PersistentWorkspace` resource alone is insufficient:
it controls who can read or mutate the object, but not whether a principal that
can create a Run may consume a particular existing workspace. Scheduler and
runtimed cannot make that decision because they do not receive the original
Kubernetes request identity.

The implemented v1.0 model uses a Kubernetes-native `use` permission on the
`persistentworkspaces/use` subresource:

1. A validating admission webhook handles direct Run creation with
   `spec.workspace`. It requires that the referenced workspace already exists,
   checks that its Runtime matches the Run, then synchronously creates and
   waits for a `SubjectAccessReview` for the requesting principal. The review
   uses verb `use`, resource `persistentworkspaces`, subresource `use`, and the
   workspace name as the resource name. The webhook must return an admission
   decision within a bounded timeout: it should give the review about two
   seconds, while the webhook configuration allows five seconds for the full
   request. A denied review, timeout, or API error rejects the Run rather than
   leaving an unauthorized reference Pending. The webhook configuration uses
   `failurePolicy: Fail`, so an unreachable or timed-out webhook also fails
   closed.
2. Namespace administrators grant this permission through ordinary Role or
   RoleBinding rules. `resourceNames` can restrict a principal to named
   workspaces without adding a kruntimes-specific ACL to the CRD.
3. The chart-configured controller ServiceAccount has a narrow internal path
   for Workflow child Runs. The webhook permits that identity without a second
   `SubjectAccessReview` only when it proves that the Run and referenced
   workspace have controller owner references to the same live `WorkflowRun`
   UID, their `WorkflowRunUIDLabel` values match that UID, and their workflow
   job labels match. A malformed, stale, or cross-job reference is denied. A
   user cannot obtain this path by copying labels or owner references because
   only the configured ServiceAccount identity enters it; every other caller
   follows the ordinary `use` review.
4. Scheduler and runtimed remain workflow-agnostic and authorization-agnostic.
   They continue to enforce only workspace existence, Runtime compatibility,
   binding, lifecycle, and Pod placement.

This changes direct Run behavior for a missing workspace reference: instead of
being accepted and waiting for a future object, it is rejected at admission.
Workflow-controlled child Runs are created only after their owned workspace
exists, so they retain their normal asynchronous binding behavior.

The Helm chart deploys the webhook Service, a chart-managed TLS Secret, and a
`ValidatingWebhookConfiguration` with `failurePolicy: Fail`. It configures the
controller ServiceAccount identity explicitly on the webhook process. That
ServiceAccount may create `SubjectAccessReview` objects, but receives no
generic `persistentworkspaces/use` permission: workspace references are allowed
only through the verified Workflow child-Run path above.
Impersonation-focused integration and E2E coverage remain follow-up work.

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

runtimed prepares the referenced workspace path before execution. The workspace
lifecycle is owned by the `PersistentWorkspace` controller. For task-mode Runs, runtimed stages
inline and Git sources under the workspace-reserved
`.kruntimes/runs/<run-uid>` directory, executes them with the workspace as the
current directory, and uses that same Run-local directory for outputs and
artifact staging. Runtimed does not delete any path in a referenced workspace;
the PersistentWorkspace controller applies its lifecycle and cleanup policy.
This keeps all runtimed-managed files from overwriting files that other Runs
intentionally share. Function-mode source remains at the workspace root so
handler module resolution stays relative to its working directory.

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
              path: dist.tgz
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
          driver: Filesystem
          type: File
          location:
            filesystem:
              path: runs/<run-uid>/artifacts/dist.tgz
      steps:
        - name: package
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
| Scheduler | Snapshots generic Run, Runtime Pod, and referenced workspace state; applies workspace fencing, Runtime capacity, and Run affinity/anti-affinity. It does not know about Workflows. |
| runtimed | Prepares referenced workspace paths, stages artifact inputs, and collects artifact outputs. It does not know about Workflows or delete referenced workspace paths. |
| ArtifactStore | Stores durable artifacts outside etcd. |

## Failure and Recovery

- If a Runtime Pod disappears, `RuntimePodLocal` workspaces backed by that Pod's
  workspace volume become `Lost`; they are not automatically rebound to another
  Pod.
- Runs that reference a missing, Pending, Lost, Runtime-incompatible, or
  UID-mismatched workspace Pod stay Pending with a clear message. Workspace
  changes requeue matching Pending Runs; none of these states is a scheduler
  terminal failure.
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
- finalizer-based deletion, active-Run usage tracking, and runtimed-only
  physical cleanup for every bound workspace;
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
7. After reviewing the bound-Pod UID fencing amendment, add
   `status.boundPodUID`, then bind `RuntimePodLocal` PersistentWorkspaces to
   ready Runtime Pods and record their lifecycle status without touching runtime
   filesystems.
8. Add a generic `Workspace` scheduler Filter plugin. **Implemented:** the
   scheduling snapshot resolves a referenced workspace once; the filter rejects
   candidates whose Runtime does not match, and for a Bound RuntimePodLocal
   workspace admits only the fenced `status.boundPod` and
   `status.boundPodUID`. A Pending or Lost workspace has no eligible candidates,
   so its Run remains Pending with a clear scheduling message and is requeued
   when the workspace changes.
9. Update runtimed workspace preparation for referenced workspaces.
   **Implemented:** referenced Runs execute in the persistent workspace;
   outputs and artifact staging remain Run-local; task source is staged under
   the reserved per-Run directory. PersistentWorkspace lifecycle owns cleanup
   within the workspace.
10. Compose job-local workspaces in the Workflow controller. **Implemented:**
    initialization creates one WorkflowRun-owned PersistentWorkspace for each
    inline job and every child Run for that job references it. Reusable
    Workflow-call jobs create no parent workspace; their materialized child
    WorkflowRun owns its own job workspaces.
11. Add Workflow step artifact input fields and job-scoped artifact status.
    **Implemented:** a step can reference
    `jobs.<job-id>.artifacts.<artifact-name>` from a direct `needs` dependency;
    the controller resolves it into a generic `Run.spec.artifactInputs` entry.
12. Promote child Run artifact refs into Workflow status. **Implemented:** a
    successful Job exposes the final successful ref for each artifact name in
    `status.jobs.<job-id>.artifacts`.
13. Add E2E coverage for Runtime workspace volume sources, job-local workspace
    sharing, job-to-job artifact passing, Runtime Pod loss, cleanup, and
    permission boundaries. **Implemented:** E2E covers Runtime workspace
    sources, job-local sharing, Run artifact staging, job-to-job transfer,
    Runtime Pod loss, cleanup, and permission boundaries. Admission
    authorization and controller ownership also have focused unit and
    integration coverage.
14. Implement the cleanup protocol: active-Run tracking, Released admission
    fencing, a workspace finalizer, runtimed local-path cleanup, and E2E
    coverage for TTL, explicit deletion, retained workspaces, and bound-Pod
    loss during cleanup.
