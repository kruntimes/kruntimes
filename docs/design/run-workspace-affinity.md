# Run Workspace References and Affinity

Status: **Reviewed**

This document fixes the API shape for the next Workflow data-sharing
prerequisite: a generic Run workspace reference and Run-to-Run affinity. It
does not implement workspace binding, scheduler placement, or runtimed file
preparation. Those are separate, reviewable implementation PRs.

## Scope

`PersistentWorkspace` is a namespace-scoped resource with a Runtime binding and
lifecycle. A Run needs a small, typed reference to use one. Later Runs also need
a familiar way to require or prefer the same Runtime Pod as another Run, without
exposing an opaque scheduler-specific sticky key.

The API must remain useful outside Workflows. A user can create a
`PersistentWorkspace` and one or more Runs directly; a Workflow controller is
only one future consumer.

## Run Workspace Reference

`Run.spec.workspace` is an optional typed local reference:

```go
type RunWorkspaceReference struct {
    Name     string `json:"name"`
    Kind     string `json:"kind,omitempty"`
    APIGroup string `json:"apiGroup,omitempty"`
}

type RunSpec struct {
    // Existing fields omitted.
    Workspace *RunWorkspaceReference `json:"workspace,omitempty"`
}
```

The initial served values are deliberately narrow:

| Field | Required | Default | Initial accepted value |
| --- | --- | --- | --- |
| `name` | Yes | None | A DNS-1123 subdomain name in the Run namespace. |
| `kind` | No | `PersistentWorkspace` | `PersistentWorkspace`. |
| `apiGroup` | No | `kruntimes.io/v1alpha1` | `kruntimes.io/v1alpha1`. |

`apiGroup` preserves the compact reference form proposed for the experimental
API. It identifies the currently served group/version, not an arbitrary remote
resource. A future general workspace-provider API can introduce versioned
reference semantics through a reviewed API change rather than silently making
these fields mean something broader.

The reference is always namespace-local. It has no `namespace` field, and the
API rejects unsupported kind/group combinations. A Run cannot refer to a
workspace in another namespace, even if its creator has permission to read it.

For example, these forms are equivalent in the initial API:

```yaml
spec:
  workspace:
    name: ci-build-workspace
```

```yaml
spec:
  workspace:
    name: ci-build-workspace
    kind: PersistentWorkspace
    apiGroup: kruntimes.io/v1alpha1
```

The Run reference only expresses intent. It does **not** copy workspace status,
bind a workspace, or implicitly select a Runtime Pod. The PersistentWorkspace
controller remains the owner of binding and lifecycle. The scheduler and
runtimed follow-up PRs must reject incompatible Runtime bindings and honor a
bound workspace's placement without learning any Workflow concepts.

## Run Affinity API

Run affinity follows Kubernetes affinity's familiar required/preferred model
and label-selector vocabulary, but it deliberately does not reuse
`corev1.Affinity`. Kubernetes uses that type to choose a Node for a new Pod;
kruntimes chooses an existing ready Runtime Pod for a Run. Reusing it would
present unsupported Node and Pod semantics as functional API surface.

```go
type RunAffinity struct {
    RunAffinity     *RunAffinityRules `json:"runAffinity,omitempty"`
    RunAntiAffinity *RunAffinityRules `json:"runAntiAffinity,omitempty"`
}

type RunAffinityRules struct {
    RequiredDuringSchedulingIgnoredDuringExecution []RunAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`
    PreferredDuringSchedulingIgnoredDuringExecution []WeightedRunAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

type WeightedRunAffinityTerm struct {
    Weight          int32           `json:"weight"`
    RunAffinityTerm RunAffinityTerm `json:"runAffinityTerm"`
}

type RunAffinityTerm struct {
    LabelSelector *metav1.LabelSelector `json:"labelSelector"`
    TopologyKey   string                `json:"topologyKey"`
}

type RunSpec struct {
    // Existing fields omitted.
    Affinity *RunAffinity `json:"affinity,omitempty"`
}
```

The field names intentionally mirror Kubernetes:

- `requiredDuringSchedulingIgnoredDuringExecution` is a hard scheduling
  constraint. A candidate that does not satisfy every term is ineligible; the
  Run remains `Pending` if no candidate is eligible.
- `preferredDuringSchedulingIgnoredDuringExecution` is a soft scoring hint.
  It never lets a candidate violate required rules or Runtime capacity.
- `runAffinity` requires a matching active Run to share the requested topology.
- `runAntiAffinity` requires matching active Runs to be absent from that
  topology.

The initial API supports exactly one topology key:

```text
kruntimes.io/runtime-pod
```

For this key, topology equality means equality of the assigned Runtime Pod
name. It is the direct, understandable constraint required by a
`RuntimePodLocal` PersistentWorkspace. The scheduler need not interpret a
node, zone, or a Kubernetes Pod's affinity to evaluate it.

Every term matches labels on namespace-local `Run` objects. The API itself does
not define the scheduler's bootstrap or reservation behavior. The current
single-Run implementation uses assigned `Scheduled`, `Running`, and `Ready`
Runs as active targets, but that is insufficient for a cohort whose first Run
has required affinity to other Runs. The proposed [Scheduler Framework and
Scheduler Framework](scheduler-framework/) design defines actual targets,
same-cycle planned targets, and Inter-Run Affinity bootstrap. Those execution
semantics require review before replacing the current placement implementation.

Example: require a later build step to run on the Runtime Pod selected for a
previous step.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: ci-build-test
  labels:
    workflows.kruntimes.io/workflow: ci-data-sharing-demo
    workflows.kruntimes.io/job: build
spec:
  runtime: bash
  workspace:
    name: ci-build-workspace
  affinity:
    runAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        - labelSelector:
            matchLabels:
              workflows.kruntimes.io/workflow: ci-data-sharing-demo
              workflows.kruntimes.io/job: build
          topologyKey: kruntimes.io/runtime-pod
  mode:
    task: {}
```

The Workflow controller will use a more selective label set in its generated
Runs so that a job cannot accidentally match an unrelated Run with copied user
labels. The generic API itself intentionally permits any caller-owned labels.

## Validation and Transition Rules

The initial CRD validation is intentionally mechanical and bounded:

- `workspace.name` has Kubernetes object-name length and format validation.
- `workspace.kind`, when present, must be `PersistentWorkspace`.
- `workspace.apiGroup`, when present, must be `kruntimes.io/v1alpha1`.
- every affinity term requires a non-empty `labelSelector` and the only
  supported `topologyKey`.
- required and preferred term lists each have a small bounded maximum; preferred
  weights are integers from 1 through 100.

As with other execution inputs, `workspace` and `affinity` are immutable after
Run creation. Changing either after assignment could make a Running Run observe
a different shared-data boundary or placement requirement. This is an
experimental API validation change and requires regenerated CRDs and integration
tests in the API skeleton PR.

The initial API intentionally does not offer cross-namespace selectors,
namespace selectors, Node affinity, arbitrary topologies, topology spread, or
a direct `podName`/sticky-key field. Each expands the scheduler's trust and
RBAC surface and needs its own design review.

## Scheduler Contract

The API skeleton only declares and validates fields. Scheduling execution
semantics are defined by the proposed [Scheduler Framework](scheduler-framework/)
document. In particular, the implementation
must use a bounded planner with separate PreFilter, Filter, Score, Reserve, and
Bind stages rather than independently deciding placement in each Run
reconcile.

The scheduler remains Workflow-agnostic. It neither creates workspaces nor
interprets job or step labels. It evaluates generic Run labels and the declared
affinity terms only.

## Interaction with PersistentWorkspace

`PersistentWorkspace.spec.runtime` must equal `Run.spec.runtime` before a Run
can use that workspace. For `RuntimePodLocal`, after the workspace is bound the
scheduler must allow only its `status.boundPod`; it may express that internal
constraint directly rather than requiring a controller to manufacture an
affinity target Run. The public affinity API remains useful for direct users and
for later steps that must follow another active Run.

A missing, pending, lost, or incompatible workspace is not a terminal Run
failure at scheduling time. The later workspace-aware reconciliation design must
provide a clear condition and requeue behavior. Its exact failure/retry policy
is intentionally outside this API PR.

## Implementation Sequence

1. Add the Go API types, deepcopy generation, CRD schemas, and validation
   integration tests only.
2. Add scheduler candidate filtering and scoring with focused unit and
   integration coverage.
3. Add PersistentWorkspace binding and Run workspace admission/preparation in
   the controller and runtimed.
4. Add Workflow controller composition and end-to-end data-sharing coverage.

Steps 2 through 4 must remain separate PRs so each preserves the scheduler and
runtimed component boundary.
