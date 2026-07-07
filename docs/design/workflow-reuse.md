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

## Status Model

`WorkflowRun.status` owns execution state:

```yaml
status:
  phase: Running
  jobs:
    build:
      phase: Running
      steps:
        setup:
          phase: Succeeded
          outputs:
            python-version: "3.13"
```

`Workflow.status` and `Action.status` should contain definition-level
conditions only, such as validation or readiness. They should not contain
per-execution job or step state.

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
5. Implement namespace-local reference resolution for top-level, job, and step
   `uses`.
6. Implement input binding, expression context, and output propagation.
7. Update CLI verbs and docs to use `WorkflowRun` for execution.
8. Add E2E coverage for inline `WorkflowRun`, reusable Workflow calls, Action
   calls, validation failures, and output propagation.
9. Update the final v0.x demos after the reusable model is implemented.
