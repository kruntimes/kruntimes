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
- [ ] Runtime readiness visibility: reliably reconcile Deployment readiness
  into `Runtime.status.readyReplicas`, show it through `krt runtime list/get`,
  and add integration and E2E coverage for status updates as Pods become ready
  or unavailable.
- [x] Scheduler framework: replace independent per-Run placement with a
  scheduler queue and Kubernetes-style single-Run scheduling cycles. Review the
  [Scheduler Framework](design/scheduler-framework.md) architecture before
  changing scheduler behavior.
  Initial implementation TODO:
  - [x] review Run queue ownership, snapshot, PreFilter, Filter, Score,
    Reserve/Assume, Bind, status, and retry semantics;
  - [x] review the [Run resource accounting](design/run-resource-accounting.md)
    API before extending scheduler capacity checks beyond the built-in `runs`
    resource;
  - [x] refactor scheduler internals behind queue/planner interfaces while
    preserving current observable behavior and metrics;
  - [x] add deterministic selection, assumed-capacity, bind-conflict,
    and restart-recovery coverage;
  - [x] implement assumed affinity targets and Inter-Run Affinity bootstrap:
    - [x] review the Filter-plugin amendment, then implement independent
      RuntimePodAvailability and RunAffinity filters in the scheduler planner;
    - [x] project namespace-local actual assignments and unconfirmed assumed
      assignments into an immutable affinity-target snapshot;
    - [x] add required Run affinity and anti-affinity filtering with bounded
      Pending waiting reasons;
    - [x] score preferred affinity and anti-affinity ahead of deterministic
      capacity placement;
    - [x] allow an eligible label-matching Run to seed an empty Inter-Run
      Affinity cohort, while keeping unsatisfiable dependencies Pending;
    - [x] add unit, integration, and E2E coverage for actual targets, assumed
      targets, bootstrap, anti-affinity, capacity, and recovery;
  - [x] add a Runtime field index for Pending Run wakeups instead of scanning a
    namespace for every Runtime Pod or capacity event;
  - [x] introduce Kubernetes-style weighted Score plugins:
    - [x] score every Filter-accepted Pod in every plugin instead of reducing
      candidates within a plugin;
    - [x] normalize plugin scores to `0..100`, apply fixed internal weights,
      aggregate totals, and rank candidates by descending total;
    - [x] retain framework-owned deterministic Pod-name tie breaking for equal
      totals;
    - [x] add unit and integration coverage for normalization, weights, ties,
      and errors;
  - [x] add bounded scheduler metrics:
    - [x] count Filter-plugin Pod rejections by bounded plugin and reason;
    - [x] count stale `Reserve` and conflicting `Bind` operations by bounded
      stage;
    - [x] count requested Pending Run wakeups by bounded event source;
- [ ] Function-mode Runs: define mutually exclusive
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
  - [x] review and approve the
    [Function Inline Source Materialization](design/function-inline-source.md)
    API before registering inline function Runs;
  - [x] remove top-level `Run.spec.handler`, `Run.spec.entrypoint`, and
    `Run.spec.args`; keep handler under `Run.spec.mode.function.handler` and
    task input under `Run.spec.mode.task`;
  - [x] review and approve the
    [Function Runtime Server Contract](design/function-runtime-contract.md);
  - [x] add idempotent register/status/invoke/unregister protobuf operations
    keyed by Run UID;
  - [ ] implement built-in function adapters:
    - [x] Bash FunctionRuntime adapter with handler validation, registration
      fencing, one in-flight invocation, bounded output, and unregister drain;
    - [x] Python FunctionRuntime adapter with handler validation, registration
      fencing, one in-flight invocation, bounded output, and unregister drain;
  - [ ] add bounded invocation outputs/artifact references and structured logs
    keyed by Run UID and invocation ID;
  - [ ] implement the function control-plane lifecycle in independently
    reviewable slices:
    - [x] add a deterministic FunctionRuntime registration request builder,
      including immutable-input digest coverage;
    - [x] materialize inline function source at the validated
      `source.inlinePath` below the Run working directory;
    - [ ] transition assigned function Runs through source preparation,
      cleanup-finalizer installation, local registration through a runtimed
      FunctionRuntime client, and `Running -> Ready`;
    - [ ] observe local `FunctionStatus` for fatal registration loss, total Run
      timeout, and Runtime Server-owned idle timeout;
    - [ ] integrate registration failures with the shared retry engine without
      retrying individual invocation failures;
    - [ ] implement cancellation and deletion finalization: drain or cancel the
      local registration, clean only function-local state, and release capacity;
    - [ ] recover active function registrations after runtimed restart and
      reconcile stale Runtime Pod assignments with assignment-UID fencing;
    - [ ] add unit, integration, and E2E coverage for registration, retry,
      timeout, cancellation, deletion, restart recovery, and stale-pod fencing;
  - [ ] cover function registration, ready status, local and proxied invoke,
    repeated invocation, idle timeout, explicit release, Runtime Pod restart
    recovery, and cleanup.
- [x] Runtime gateway invoke path: add an optional shared `runtime-gateway`
  Deployment and ClusterIP Service to the Helm chart. The gateway exposes the
  stable HTTP Run endpoint, resolves the requested Run, and calls the
  Kubernetes Service for its Runtime. Kubernetes selects a ready Runtime Pod;
  runtimed resolves ownership only within that Runtime.
  Initial implementation TODO:
  - [x] define the [gateway TLS and transfer-bound contract](design/runtime-gateway-transport.md)
    before changing the chart or endpoint transport;
  - [x] add Helm templates, `gateway.enabled`, values, RBAC, a dedicated
    runtimed gRPC port, HTTP-to-gRPC adapter, and unit/render coverage for the
    shared gateway Deployment and ClusterIP Service;
  - [x] implement an explicit `http`/`https` gateway protocol set;
    use a chart-managed TLS Secret by default, permit an existing Secret, and
    publish HTTPS plus its CA bundle when both protocols are enabled;
  - [x] add optional cert-manager `Certificate` rendering for the configured
    TLS Secret;
  - [x] implement Runtime-scoped Run lookup and bounded local or single-hop
    peer routing;
  - [x] fence routing with immutable Run UID and assigned Pod UID, rejecting
    stale assignments before forwarding;
  - [x] authenticate caller tokens through Kubernetes TokenReview and authorize
    the target Run through SubjectAccessReview;
  - [x] add a bounded authorization decision cache;
  - [x] enforce a bounded per-gateway HTTP request concurrency limit;
  - [x] make explicit request and response limits configurable where needed;
  - [x] add E2E coverage with HTTP and HTTPS enabled simultaneously, including
    strict verification of the chart-managed certificate;
  - [x] implement the reviewed bounded, paginated `ListSessionFiles` contract:
    - [x] define HTTP, gRPC, SDK, ordering, cursor, mutation-consistency, and
      response-bound semantics in the Session Mode design;
    - [x] add `limit`, `page_token`, and `next_page_token` to the gRPC contract
      and regenerate clients;
    - [x] implement identical bounded cursor paging in built-in Runtime Servers,
      runtimed proxying, the HTTP gateway, and Go/Python SDKs;
      - [x] enforce direct gRPC paging in Bash and Python Runtime Servers with
        shared cursor encoding and UTF-8 byte-wise ordering;
      - [x] map HTTP `limit` and `pageToken` through the gateway and preserve
        page fields through runtimed proxy routing;
    - [x] add unit, integration, and E2E coverage for limits, ordering, invalid
      or mismatched tokens, and traversal-safe multi-page listings.
- [x] Session-mode Runs for agent sandboxes: provide a stateful, mutable
  workspace on a warm Runtime Pod without introducing a separate `Sandbox` CRD.
  The accepted [Session Mode design](design/session-mode.md) uses
  `Run.spec.mode.session`, an HTTP API translated by the shared gateway to the
  `SessionRuntime` gRPC service exposed by runtimed, and a v0 trusted container
  backend.
  A Session Run is exclusive through its dedicated `runs: 1` Runtime and the
  runtimed local claim gate, ephemeral, and
  terminates on assigned Pod loss rather than silently resuming on a new Pod.
  Initial implementation TODO:
  - [x] review and approve the API, lifecycle, queue, data-plane, security, and
    future-backend design;
  - [x] add `Run.spec.mode.session`, validation, generated CRDs, and status
    conditions; reject a Session Run that declares `spec.workspace`;
  - [x] initialize source and artifact inputs into a Run-UID-scoped ephemeral
    workspace, register the session locally, and transition the Run to `Ready`;
  - [x] implement the internal `SessionRuntime` contract for lifecycle,
    workspace-constrained commands, file operations, process groups, and
    operation status;
  - [x] add the authenticated versioned Session HTTP API to the shared gateway,
    HTTP-to-`SessionRuntime` translation, and bounded request/response handling;
  - [x] emit structured Session output and audit logs from owner runtimed for
    existing Kubernetes log collectors and `krt logs`; do not create a separate
    gateway log store;
  - [x] implement the per-session FIFO mutation queue: one active command or
    file mutation, global and per-Run bounds, default/max operation timeout,
    cancellation, and graceful termination;
    - [x] serialize owner-runtimed mutations with per-Session FIFO queues,
      Run-scoped queue/timeout limits, queue-full rejection, and cancellation
      on Session close;
    - [x] expose administrator configuration for global queue and operation
      timeout limits, plus the runtimed-to-Runtime-Server close deadline;
    - [x] define backend-specific graceful process-termination configuration.
      A common setting is not injected into arbitrary custom Runtime images;
  - [x] keep operation history and audit events in structured external logs,
    retain only bounded readiness, endpoint, and artifact-reference data in Run
    status, and export large outputs through ArtifactStore;
    - [x] replace `cancelRequested` with a monotonic `termination.mode` API:
      `Immediate` for cancellation and Session-only `Drain` for normal
      completion; implement `Ready -> Finalizing -> Succeeded` and distinct SDK
      `Close` and `Cancel` helpers;
    - [x] fence new gateway operations, drain already accepted operations, and
      close the local Runtime Server before collecting final artifacts;
    - [x] provide `$KRUNTIME_ARTIFACTS_DIR` only for Session Runs whose Runtime
      has an ArtifactStore; validate and upload final files, retain compact refs
      in status, retry transient store failures while Finalizing, and fail
      invalid artifacts deterministically;
    - [x] cover successful completion/export, cancellation during finalization,
      transient store retry, invalid artifacts, and Runtime Pod loss in tests;
  - [x] add Python and Go SDKs with create/open/wait/execute/files/logs/close
    helpers, typed errors, direct in-cluster access, and local port-forward
    support;
  - [x] add a Kubernetes diagnosis agent example that uses a Session Run for
    multi-step scripts, files, results, and cleanup; use it to find remaining
    product gaps before calling the feature supported;
  - [x] add unit, integration, and E2E coverage for registration, ordering,
    timeout, cancellation, idle expiry, cleanup, authorization, file-boundary
    enforcement, Runtime Pod loss, and gateway routing;
    - [x] verify per-session FIFO mutation ordering through the authenticated
      HTTP gateway in E2E;
    - [x] verify that cancelling a Session Run terminates an active gateway
      command and rejects subsequent gateway access in E2E;
    - [x] verify gateway routing, bearer-token authentication, Run authorization,
      workspace file-boundary enforcement, registration environment, structured
      command logs, idle and total timeout, Drain completion, SDK access, and
      assigned Runtime Pod loss across focused unit, integration, and E2E tests.
- [ ] v0.x examples: add LLM agent and workflow examples, then use those
  examples to identify missing product and API capabilities.
- [x] Workflow data sharing: design and implement first-class cross-Run storage
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
  - [x] implement required/preferred Run affinity through the reviewed
    [scheduler framework](design/scheduler-framework.md), while keeping
    no-capacity Runs Pending;
  - [x] review and define `RuntimePodLocal` binding semantics: deterministic
    ready-Pod selection without capacity reservation, planned path ownership,
    and sticky `Lost` status after bound-Pod deletion:
    - [x] review the `status.boundPodUID` fencing amendment so same-name Pod
      recreation cannot silently replace a RuntimePodLocal workspace;
    - [x] add the status field and regenerate CRDs;
    - [x] implement metadata-only binding with stable UID-hash distribution
      across ready Runtime Pods, with Runtime and Pod watches;
    - [x] retain the original binding while the Pod is merely unavailable, and
      transition permanently to `Lost` when the name disappears or its UID
      changes;
    - [x] add focused controller and API validation coverage.
  - [x] add a generic `Workspace` scheduler Filter plugin without introducing
    Workflow concepts: require `Run.spec.workspace` to match its Runtime and a
    Bound RuntimePodLocal workspace, and filter candidates to its fenced bound
    Pod while keeping unresolved or Lost workspaces Pending with a clear
    message; wake matching Pending Runs when the referenced workspace changes.
  - [x] update runtimed workspace preparation and cleanup to support referenced
    persistent workspaces without knowing Workflow semantics; create only the
    bound workspace directory, preserve its contents, and clean only Run-local
    temporary state.
  - [x] compose the generic primitives in the Workflow controller: create and
    own a job-local PersistentWorkspace, add the workspace reference and
    bound-Pod placement to each child Run, and surface workspace loss without
    exposing workspace controls in Workflow APIs.
  - [x] add explicit step artifact inputs and job-scoped artifact references;
    stage `jobs.<job>.artifacts.<name>` into downstream child Runs and promote
    compact child Run artifact refs into Workflow status.
  - [x] complete E2E coverage for Runtime workspace volume sources, job-local
    workspace sharing, job-to-job artifact passing, Runtime Pod loss, cleanup,
    and permission boundaries:
    - [x] Runtime workspace sources, job-local sharing, job-to-job artifact
      passing, and Runtime Pod loss;
    - [x] explicit-deletion cleanup;
    - [x] automatic-TTL cleanup;
    - [x] permission boundaries:
      - [x] review the `persistentworkspaces/use` authorization contract and
        direct-Run missing-reference behavior;
      - [x] add a validating admission webhook with SubjectAccessReview,
        reviewed failure policy, and Helm/TLS installation support;
      - [x] prove controller-created Workflow child Runs cannot bypass the
        workspace authorization boundary;
      - [x] add impersonation-focused integration and E2E coverage for allow,
        deny, named-resource, and controller-owned cases.
  - [x] implement PersistentWorkspace cleanup as a separately reviewed lifecycle
    slice: active Run tracking, `Released` scheduling fence, finalizer-based
    deletion, runtimed-only Pod-local directory removal, deletion/TTL E2E
    coverage, and focused loss controller coverage.
- [x] Workflow reuse model: split execution instances from reusable
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
  - [x] implement job-level reusable Workflow calls through the reviewed
    [execution-boundary design](design/workflow-job-reuse.md):
    - [x] review and approve the direct child WorkflowRun and local snapshot model;
    - [x] remove root `WorkflowRun.spec.uses`/`with` and implement template
      triggering as rendered inline WorkflowRun creation;
    - [x] add a per-WorkflowRun immutable snapshot with the local execution
      spec and bounded `JobStatus.outputs`;
    - [x] capture the frozen source output contract in each materialized child
      WorkflowRun annotation;
    - [x] create and observe child WorkflowRuns for ready job-level calls,
      including input rendering and output-contract capture;
    - [x] project inline and child Workflow outputs into bounded
      `WorkflowRun.status.jobs.<job>.outputs` values;
    - [x] verify late-binding behavior before child creation, deterministic
      behavior after child creation, restart recovery, nested calls,
      cancellation, and invalid graphs, including `A -> B -> A` cycle
      rejection before child creation;
  - [x] implement step-level Action expansion through the reviewed
    [Action Execution](design/workflow-action-execution.md) model:
    - [x] add the Action-call status, immutable snapshot, and CRD validation
      shape;
    - [x] evaluate `inputs`, `steps`, and `jobs` expressions at the defined
      execution boundaries;
    - [x] materialize Action calls into ordinary child Runs, aggregate their
      terminal states and declared outputs, and recover after a controller
      restart;
    - [x] reject nested Action calls, missing Actions, invalid input bindings,
      and invalid Action output expressions before creating an affected child
      Run;
  - [x] add E2E coverage for inline `WorkflowRun`, reusable Workflow calls, Action
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
- [ ] Add at least one secure Session backend, initially gVisor, for untrusted
  LLM-generated code. Define the Runtime Server contract, isolation boundary,
  resource accounting, networking policy, and compatibility with the Session
  workspace and gateway APIs before making it available.
- [ ] Restore the Python runtime base image to a supported Python 3.15 release
  only after its final image is available and every locked native dependency,
  including `grpcio`, publishes compatible `cp315` wheels. Keep the image build
  and runtime test coverage as the upgrade gate; do not rely on an implicit
  source build in the slim production image.
- [ ] Add `Run.spec.priority` as a scheduler API. First review the priority,
  fairness, aging/starvation, namespace isolation, authorization, retry/backoff,
  and non-preemption semantics, then replace controller-runtime event ordering
  with scheduler-owned queue ordering and add unit, integration, and E2E
  coverage.
- [ ] Support explicitly configured concurrent invocations for a function-mode
  Run. Preserve the default single in-flight invocation, define per-function
  concurrency limits and invocation/workspace isolation semantics, and retain
  Runtime Pod capacity enforcement.
- [ ] Design persistent per-registration Function worker processes to reduce
  Python invocation startup overhead. Review worker lifecycle, module state,
  cancellation, concurrency, output limits, and isolation before replacing the
  current per-invocation subprocess model.
- [ ] Reduce local and CI E2E suite duration without weakening coverage:
  identify avoidable serial waits, safely parallelize isolated cases, and split
  fast feedback from slower lifecycle coverage while preserving a complete
  release gate.
- [ ] Clean up a Runtime's runtime maintainer when the Runtime is deleted.
  Define ownership and finalization so orphaned maintainers do not accumulate,
  while preserving artifact cleanup for Runs that still require it.
- [ ] Add explicit `Run` suspend and resume semantics. Define API and lifecycle
  behavior for Pending, executing task, function, and Session Runs, including
  what is retained or discarded for processes, workspaces, requests, and
  timeout accounting; do not model this as a best-effort boolean switch.
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
