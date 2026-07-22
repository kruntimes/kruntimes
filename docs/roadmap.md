# Project Status and Roadmap

kruntimes is actively developed as a `v0.x experimental` project. APIs are
`v1alpha1` and may change before a stable release.

## Current Status

Completed foundations include:

- Run and Runtime CRDs.
- Warm Runtime Pod scheduling.
- Bash and Python built-in runtimes.
- bounded outputs and external artifact references.
- Runtime artifact cleanup through long-running maintainers.
- retry, timeout, cancellation, stale-pod recovery, and terminal conditions.
- Helm charts, release workflows, SBOM, signing, CLI releases, and benchmark
  harness.
- security, operations, release, compatibility, and custom Runtime docs.

## Near-Term Roadmap

### Post-Public Validation

Completed validation support:

- Published a comparison guide covering kruntimes vs Knative, Argo Workflows,
  Tekton, Volcano, and a worker queue on a Deployment.
- Published a clear "when to use / when not to use" guide so users understand
  that kruntimes is a warm execution substrate, not a full serverless platform,
  workflow engine, batch scheduler replacement, or hostile-code sandbox.
- Published three end-to-end demos: low-latency Bash/Python Run, burst
  short-task execution, and custom Bash Runtime image.
- Defined go/no-go signals: users can explain the value in two minutes, at
  least two design partners try it on real workloads, and at least one
  non-maintainer completes the quick start.
- Added public issue templates for target-user interviews and design-partner
  trials.

Still validating:

- Recruit design partners from platform, CI, and AI agent infrastructure teams
  that run short-lived, high-concurrency, or agent-driven workloads.
- Validate the core problem with 5-8 target users and capture whether they have
  experienced Pod cold start, burst throughput, or infrastructure-ownership
  constraints in the last six months.
- Choose and validate the first primary wedge. The current hypothesis is AI
  agent tools and trusted internal code-execution sandboxes, with CI micro-steps
  and automation tasks as secondary use cases.

### v0.x Experimental

The next development phase is focused on turning the public `v0.x` release into
a coherent experimental product. The current execution order is:

Implementation sequencing note: API skeleton PRs that add CRDs, generated
deepcopy code, controller manager wiring, Helm RBAC, or integration validation
should merge one at a time. After one lands, rebase the next API skeleton PR on
`main`, regenerate manifests, and rerun `make test`, `make test-integration`,
and `make test-helm`. This keeps generated files and hand-written controller
wiring from accumulating avoidable conflicts.

- [x] Release/package hygiene: rename published image packages to remove the
  redundant `kruntimes-` prefix, publish a new release, clean up old packages,
  and align installation, demo, and release documentation.
- [x] Run input semantics: audit and stabilize `inline`, `entrypoint`, and
  `args` behavior across API, runtimes, CLI examples, docs, and tests. The
  intended model is: `inline` is a standalone script and takes precedence over
  `entrypoint` and `args`; `entrypoint` points to a script file and receives
  `args` as parameters; when `entrypoint` is absent, `args` execute as shell
  commands for shell-style runtimes.
- [x] Docs usability: add copy buttons for user-executed commands, remove
  unnecessary Helm overrides from examples, and make `krt` installation visible
  before demos use `krt` commands.
- [x] Docs theme support: let readers choose light theme, dark theme, or sync
  with system preference on the documentation site.
- [x] CLI baseline: add `krt version` so users and maintainers can report the
  installed CLI version, commit, and build timestamp.
- [x] Benchmark correctness: diagnose why `latency.complete` is much higher
  than a manually observed single Run, and clarify whether benchmarks measure
  end-to-end latency, scheduling latency, watch/update latency, or runtime
  execution time.
- [ ] Scheduler framework and batch planning: replace independent per-Run
  placement with a leader-owned, bounded planner per `(namespace, runtime)`
  queue key. Review the [Scheduler Framework and Batch Planning](design/scheduler-framework.md)
  architecture before changing scheduler behavior.
  Initial implementation TODO:
  - [ ] review queue ownership, bounded batch, snapshot, PreFilter, Filter,
    Score, Reserve, Bind, status, and retry semantics;
  - [ ] refactor scheduler internals behind queue/planner interfaces while
    preserving current observable behavior and metrics;
  - [ ] add deterministic batch planning, capacity reservation, bind-conflict,
    and restart-recovery coverage;
  - [ ] implement same-cycle planned affinity targets and self-affinity
    bootstrap, with integration and E2E coverage;
  - [ ] define priority, fairness, and starvation policy in a separate API
    design before adding `Run.spec.priority` or equivalent API;
  - [ ] design explicit scheduling groups only when a Workflow or batch demo
    proves an all-or-nothing placement requirement.
- [ ] Function-mode Runs for agent sandboxes: define mutually exclusive
  `Run.spec.mode.task` and `Run.spec.mode.function` semantics so a function Run
  can reserve a pre-warmed Runtime Pod, register a callable function with
  runtimed/runtime-server, stay ready for repeated low-latency invocations, and
  release the reservation on deletion or idle timeout. Function-mode Runs still
  obey normal Runtime capacity, so multiple function Runs can share one Runtime
  Pod when capacity allows it. This should use a dataplane invoke path rather
  than a per-invocation Kubernetes object.
  Initial implementation TODO:
  - [x] add `Run.spec.mode.task` and `Run.spec.mode.function` API fields, CRD
    validation, and runtime helpers;
  - [x] remove top-level `entrypoint`, `args`, and `handler` before API
    stabilization;
  - [x] migrate CLI creation and high-level user docs to use `spec.mode.task`;
  - [x] review and approve the
    [function lifecycle and invoke dataplane design](design/function-mode-lifecycle.md);
  - [x] add `Ready`, assigned Pod UID, bounded endpoint status, generated CRDs,
    and active/non-terminal phase-classification tests;
  - [x] add immutable execution-input transitions and the function cleanup
    finalizer constant;
  - [ ] implement registration lifecycle, shared retry integration,
    reservation/idle timeout, finalization, and restart recovery;
- [ ] Runtime gateway invoke path: create one gateway Service per Runtime, use
  that Service as the stable Run invoke endpoint, route requests to the
  runtimed that owns the assigned Runtime Pod, and rely on runtimed's in-memory
  ownership/readiness cache instead of synchronous Kubernetes API reads on the
  invoke path.
  Initial implementation TODO:
  - [ ] reconcile a Runtime-owned ClusterIP gateway Service and dedicated
    runtimed gateway port;
  - [ ] reconcile Runtime-scoped TLS serving certificates and bounded CA
    publication, rotation, and Runtime Pod rollout;
  - [ ] implement watch-backed ownership/readiness caches and bounded local or
    single-hop peer routing;
  - [ ] fence registration epochs before stale-pod reassignment and reject
    mismatched Run UID, attempt, or assigned Pod UID;
  - [ ] authorize callers through Kubernetes SelfSubjectAccessReview with a
    bounded decision cache;
  - [ ] enforce TLS, request, response, concurrency, and proxy-loop limits;
- [x] Function-mode API cleanup: remove top-level `Run.spec.handler`,
  `Run.spec.entrypoint`, and `Run.spec.args`; keep handler under
  `Run.spec.mode.function.handler` and task input under `Run.spec.mode.task`.
- [ ] Function-mode runtime contract: add runtime-server register, invoke, and
  unregister APIs; define bounded invoke request inputs, response outputs,
  artifact references, and log access without writing high-frequency invocation
  history into Run status.
  Initial implementation TODO:
  - [ ] review and approve the
    [Function Runtime Server Contract](design/function-runtime-contract.md);
  - [ ] add idempotent register/status/invoke/unregister protobuf operations
    keyed by Run UID;
  - [ ] implement built-in Bash and Python function adapters;
  - [ ] add bounded invocation outputs/artifact references and structured logs
    keyed by Run UID and invocation ID;
- [ ] Function-mode reliability and isolation: cover function registration,
  ready status, local and proxied invoke, repeated invocation, artifact reuse,
  idle timeout, explicit release, runtime pod restart recovery, cleanup, service
  account selection, runtime pod security context, resource limits, network
  policy guidance, and future stronger runtimes such as gVisor, Kata, or
  Firecracker.
- [ ] Agent sandbox SDKs: provide first-class SDKs for agent developers,
  starting with Python and Go. The SDK should expose sandbox-facing semantics
  such as create/open/reattach/disconnect/terminate, command or tool execution,
  file operations, logs, artifacts, and identity metadata while hiding the
  underlying function-mode Run unless low-level metadata is requested. It should
  also hide readiness polling, gateway discovery, port-forward fallback for
  local development, direct in-cluster URLs, bounded outputs/artifacts,
  timeouts, retries for idempotent operations, typed errors, and guardrails that
  recommend or verify one Run per Runtime Pod for AgentSandbox-style
  integrations.
- [ ] Agent sandbox workspace and file APIs: define how agents upload generated
  scripts or inputs, read files, list workspace content, fetch artifacts, and
  stream or retrieve logs without treating every operation as a Kubernetes
  reconciliation loop.
- [ ] Agent framework integration: design a thin integration layer for agent
  frameworks and MCP-style tool servers so a tool call can acquire or reuse a
  sandbox handle backed by a function-mode Run, invoke it through the gateway,
  return structured results, and clean up, disconnect, reattach, or preserve the
  sandbox according to the agent session policy.
- [ ] Agent sandbox identity and connectivity: document and implement the model
  for stable Run identity, gateway addressing, in-cluster and external access,
  service account/RBAC boundaries, network policy, and multi-tenant naming so
  agent platforms can safely hand a sandbox handle to sub-agents.
- [ ] v0.x examples: add LLM agent and workflow examples, then use those
  examples to identify missing product and API capabilities.
- [ ] Workflow data sharing: design and implement first-class cross-Run storage
  semantics discovered from the workflow demo. Target model:
  - job-to-job data moves through ArtifactStore-backed step outputs and inputs;
  - Run-to-Run data inside one Workflow job can share a `PersistentWorkspace`;
  - `PersistentWorkspace` is a namespace-scoped CRD that represents a workspace
    boundary, lifecycle, status, cleanup policy, and optional Runtime binding;
  - Run affinity/anti-affinity should follow Kubernetes-style affinity concepts
    so users can understand co-location without learning internal sticky keys;
  - scheduler and runtimed must stay workflow-agnostic. They should expose
    generic placement and workspace primitives; Workflow controller composes
    those primitives for job-local workspace sharing;
  - demos should drive the implementation and keep exposing gaps before the API
    is treated as stable.
  Initial implementation TODO:
  - [x] add a design document covering API shape, lifecycle, failure modes,
    cleanup, security, and compatibility;
  - [x] extend `Runtime.spec.workspace` to inline Kubernetes `VolumeSource`
    fields while keeping the current emptyDir default behavior;
  - [x] add `PersistentWorkspace` API types, CRD validation, status, and
    controller skeleton;
  - [x] review the dedicated Run workspace-reference and affinity API shape
    before adding the API skeleton;
  - [x] add Run fields for workspace reference and Kubernetes-style Run affinity;
  - [ ] implement required/preferred Run affinity through the reviewed
    [scheduler framework](design/scheduler-framework.md), while keeping
    no-capacity Runs Pending;
  - [ ] review and define `RuntimePodLocal` binding semantics: deterministic
    ready-Pod selection without capacity reservation, planned path ownership,
    and sticky `Lost` status after bound-Pod deletion;
  - update runtimed workspace preparation and cleanup to support referenced
    persistent workspaces without knowing Workflow semantics;
  - promote child Run artifact refs into Workflow status and add explicit step
    artifact inputs;
  - add E2E coverage for Runtime workspace volume sources, job-local workspace
    sharing, job-to-job artifact passing, Runtime Pod loss, cleanup, and
    permission boundaries.
- [ ] Workflow reuse model: split execution instances from reusable
  definitions before Workflow APIs stabilize. Target model:
  - replace the current execution-instance `Workflow` API with `WorkflowRun`;
  - `WorkflowRun.spec` contains inline `jobs` only; `krt workflow trigger`
    renders a reusable Workflow into an inline execution instance;
  - add a reusable `Workflow` CRD whose jobs can be called from a
    `WorkflowRun` job with `uses: <workflow-name>` and optional `with`;
  - add a reusable `Action` CRD whose steps can be called from a
    `WorkflowRun` or `Workflow` step with `uses: <action-name>` and optional
    `with`;
  - keep names namespace-local in the first version; avoid verbose
    `workflowRef` and `actionRef` fields until cross-namespace or remote
    references are required;
  - validation must enforce clear local shapes: WorkflowRun inline jobs, job
    `uses` vs `steps`, and step `uses` vs `run`;
  - Actions run inside the caller job context and share that job runtime,
    workspace, artifacts, and environment unless a future API explicitly
    overrides them;
  - reusable Workflow jobs have their own job/workspace/artifact boundary and
    communicate with callers through inputs, outputs, and artifacts;
  - update CRDs, controller reconciliation, CLI verbs, docs, and E2E around the
    new `WorkflowRun`, `Workflow`, and `Action` split.
  Initial implementation TODO:
  - [x] add a design document covering API shape, validation, status,
    component boundaries, and breaking-change scope;
  - [x] add `WorkflowRun` API types, CRD validation, status, and controller
    skeleton;
  - [x] change `Workflow` API types to reusable definitions;
  - [x] add `Action` API types, CRD validation, status, and controller
    skeleton;
  - [x] add workflow-oriented `krt wf` verbs for reusable Workflow definitions
    and WorkflowRun skeletons;
  - [x] update CLI verbs and docs so execution uses `WorkflowRun`;
  - [x] initialize lightweight `status.jobs[*].pre` and ordered `steps` for
    inline WorkflowRuns;
  - [x] audit existing E2E tests before inline execution changes and update
    affected cases so `make e2e` stays passing during implementation;
  - [x] implement inline WorkflowRun first-step Run creation for ready jobs;
  - [x] refactor WorkflowRun controller reconciliation into a
    load/calculate/apply/patch structure where status is derived on every
    reconciliation and only external side effects are actions;
  - [x] implement child Run status observation and step status updates;
  - [x] define and review child failure, cancellation, dependency propagation,
    and WorkflowRun terminal-status semantics: independent jobs continue after
    a failure, dependency-blocked jobs are `Skipped`, and WorkflowRun aggregates
    after executable jobs settle;
  - [x] implement next-step creation after observed step success;
  - [x] implement job terminal-state aggregation from observed step states;
  - [x] add terminal-status and cancellation API prerequisites, regenerated CRDs,
    and child Run patch RBAC;
  - [x] validate inline WorkflowRun job DAGs for unknown dependencies and
    multi-job cycles before creating child Runs;
  - [x] implement deterministic failed-dependency propagation to `JobSkipped`;
  - [x] implement WorkflowRun terminal aggregation;
  - [x] implement WorkflowRun cancellation propagation;
  - [x] verify controller restart recovery for in-progress inline WorkflowRuns,
    including child Run creation before status persistence;
  - [ ] implement job-level reusable Workflow calls through the reviewed
    [execution-boundary design](design/workflow-job-reuse.md):
    - [x] review and approve the direct child WorkflowRun and local snapshot model;
    - [x] remove root `WorkflowRun.spec.uses`/`with` and implement template
      triggering as rendered inline WorkflowRun creation;
    - [x] add a per-WorkflowRun immutable snapshot with the local execution
      spec and bounded `JobStatus.outputs`;
    - [ ] capture the frozen source output contract in each materialized child
      snapshot;
    - [ ] create and observe child WorkflowRuns for ready job-level calls,
      including input rendering and output-contract capture;
    - [ ] project inline and child Workflow outputs into bounded
      `WorkflowRun.status.jobs.<job>.outputs` values;
    - [ ] verify late-binding behavior before child creation, deterministic
      behavior after child creation, restart recovery, nested calls,
      cancellation, and invalid graphs, including `A -> B -> A` cycle
      rejection before child creation;
  - implement step-level Action expansion;
  - implement expression evaluation for `inputs`, `steps`, and `jobs` contexts;
  - promote child Run outputs into WorkflowRun step/job/workflow outputs;
  - add E2E coverage for inline `WorkflowRun`, reusable Workflow calls, Action
    calls, validation failures, output propagation, and controller restart
    recovery.
- [ ] Dashboard: design and build a read-only web dashboard, similar in spirit
  to Tekton Dashboard, that can browse Runs by namespace and inspect status and
  logs.
  Initial implementation TODO:
  - [x] add a read-only [Dashboard design document](design/dashboard/) covering
    scope, architecture, RBAC, log access, and implementation sequence;
  - [ ] review and define the v0.x Kubernetes bearer-token login model,
    request-scoped Kubernetes clients, and the local-only kubeconfig proxy
    boundary;
  - add a dashboard backend with read-only Kubernetes API access;
  - implement Run list/detail APIs with namespace-aware RBAC;
  - proxy Run log tail/follow through a backend-controlled path;
  - add read-only frontend views for namespace selection, Run lists, Run
    details, conditions, outputs, artifact references, and logs;
  - add optional Helm installation support and E2E smoke coverage.
- [ ] Continue supply-chain, security, compatibility, and operational
  hardening as the installation surface stabilizes.

### Toward v1.0

- Stabilize CRD APIs.
- Define compatibility and migration guarantees.
- Document deprecation policy.
- Clarify multi-tenant isolation strategy for production environments.
- Publish stable installation and upgrade guidance.

## Open Source Readiness

The detailed readiness checklist is maintained in
[Open Source Readiness Plan](open-source-readiness.md).

## Release History

See [CHANGELOG.md](https://github.com/kruntimes/kruntimes/blob/main/CHANGELOG.md)
and [Release Process](release.md).
