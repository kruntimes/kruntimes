# Action Execution

Status: **Implemented**

This document defines step-level reusable Action execution for v0.x Workflow
reuse. It extends the existing inline WorkflowRun execution and job-level
reusable Workflow call model without introducing a separate execution object.

## Decision

An Action call is one logical caller step:

```yaml
steps:
  - name: setup
    uses: setup-python-tools
    with:
      version: "3.13"
```

The Action's `spec.steps` are ordinary sequential child Runs within the caller
job. They inherit the caller job's runtime, workspace configuration,
environment, artifact configuration, and placement constraints. An Action does
not create a child WorkflowRun and does not introduce a scheduler or runtimed
concept.

The logical `setup` step exposes the Action's declared outputs at
`steps.setup.outputs.<name>`. Its internal steps remain observable as ordered
Action-step status, so failures and restart recovery are durable without
pretending that Action internals were user-defined top-level job steps.

## API and Status Shape

`StepSpec` has exactly one execution form:

- an inline `run` step; or
- an Action call through namespace-local `uses`, with optional string `with`.

`with` is invalid without `uses`. `args` and `env` are valid only on an inline
`run` step; an Action call takes parameters only through `with`. Action
definition steps support only inline `run`; Action nesting is not part of v0.x.

`StepStatus` gains an optional ordered `actionSteps` list. An inline step uses
its existing `runName` and `outputs` fields. An Action-call step has no
`runName`; its `actionSteps` own the actual Run identities and terminal state.
The outer step becomes `Succeeded` only after every Action step succeeds and
the Action's declared outputs evaluate successfully. It becomes `Failed` when
an Action step fails or the output contract cannot be evaluated.

```yaml
status:
  jobs:
    build:
      steps:
        - name: setup
          phase: Succeeded
          outputs:
            python-version: "3.13"
          actionSteps:
            - name: install
              phase: Succeeded
              runName: build-setup-install
            - name: verify
              phase: Succeeded
              runName: build-setup-verify
```

The `actionSteps` list is status only. The Action source and expanded step
specification belong in the WorkflowRun's immutable ControllerRevision, not
in status.

## Resolution and Snapshot

The WorkflowRun controller resolves an Action when it initializes the
WorkflowRun. It loads each namespace-local Action referenced by an inline job
or a rendered reusable Workflow and freezes the Action name and complete
`ActionSpec` in that WorkflowRun's private execution snapshot. This is the
equivalent of the child WorkflowRun snapshot used by a reusable Workflow call:
an Action expands to multiple Runs but does not own a child WorkflowRun of its
own. An Action update after a WorkflowRun has been accepted cannot affect its
execution.

The snapshot retains Action input expressions, rather than prematurely
evaluating them. When the logical call step becomes the next runnable step,
the controller:

1. evaluates the call's `with` values against currently available `inputs`,
   completed preceding `steps`, and completed dependency `jobs` outputs;
2. applies Action input defaults, rejects unknown inputs, and rejects missing
   required inputs;
3. substitutes `inputs.<name>` in the frozen Action step specs and output
   contract;
4. creates the first not-yet-created Action child Run.

This preserves late binding to prior outputs while freezing the Action
definition itself. A restart reloads both the logical workflow spec and the
frozen Action definitions from the same snapshot.

## Run Identity and Ordering

Every Action child Run remains directly owned by the WorkflowRun and uses the
existing WorkflowRun/job labels. It also carries a bounded controller-owned
step identity that includes the logical caller step and Action-local step.
Run names are deterministic from those identities. The controller discovers
existing child Runs by those labels before creating another one.

The controller starts at most one next target per job in one reconciliation:

- an inline step creates its one child Run;
- an Action call creates its first incomplete Action child Run;
- after an Action child Run succeeds, the next reconciliation creates its next
  Action child Run;
- after the final Action child Run succeeds, the controller evaluates the
  Action outputs and allows the next caller step to start.

Independent jobs remain eligible in the same reconciliation, as with current
inline job execution.

## Expressions and Outputs

v0.x supports string interpolation only:

| Context | Availability |
| --- | --- |
| `inputs.<name>` | resolved inputs of the current reusable Workflow or Action |
| `steps.<step>.outputs.<name>` | a successful preceding logical step in the same job |
| `jobs.<job>.outputs.<name>` | a successful dependency job in the same WorkflowRun |

Expressions are evaluated only at the boundary that has the needed values.
Unknown names, unavailable outputs, malformed paths, or references to a later
step are deterministic validation failures for the affected logical step. No
child Run is created for that target. The parent job then follows existing
failure and dependency-propagation semantics.

Action outputs are evaluated from successful Action-step outputs using the
Action's frozen `spec.outputs`. Their values are written only to the logical
call step's bounded `outputs` map. Later caller steps consume them through
`steps.<call-name>.outputs.<name>`; they do not address Action internals.

## Failure, Cancellation, and Limits

- A missing Action, nested Action step, invalid call input, or invalid Action
  output expression fails the logical Action step before creating the affected
  child Run.
- A failed, cancelled, or timed-out Action child Run fails its logical Action
  step and therefore its job.
- WorkflowRun cancellation requests cancellation of all non-terminal Action
  child Runs through the existing direct-child mechanism.
- Actions remain namespace-local. Cross-namespace, remote, and recursive
  Action references are out of scope.
- Existing bounded limits apply: at most 64 jobs, 128 caller steps per job,
  128 Action steps per call, 64 input/output entries, and bounded status and
  ControllerRevision data.

## Component Boundaries

The WorkflowRun controller resolves, snapshots, executes, and aggregates
Actions. The Action controller continues to own definition readiness only.
Scheduler and runtimed receive ordinary Run objects and remain unaware of
Actions.

## Implementation Sequence

1. Add the API validation, status shape, snapshot format, and unit coverage.
2. Add expression-context evaluation and Action input binding.
3. Materialize Action child Runs, status aggregation, output projection, and
   restart recovery.
4. Add integration and E2E coverage for success, failures, outputs, and
   cancellation.
