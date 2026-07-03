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
  - add a design document covering API shape, lifecycle, failure modes, cleanup,
    security, and compatibility;
  - add `PersistentWorkspace` API types, CRD validation, status, and controller;
  - add Run fields for workspace reference and Kubernetes-style Run affinity;
  - update scheduler placement to respect required/preferred Run affinity while
    keeping no-capacity Runs Pending;
  - update runtimed workspace preparation and cleanup to support referenced
    persistent workspaces without knowing Workflow semantics;
  - promote child Run artifact refs into Workflow status and add explicit step
    artifact inputs;
  - add E2E coverage for job-local workspace sharing, job-to-job artifact
    passing, Runtime Pod loss, cleanup, and permission boundaries.
- [ ] Workflow reuse model: split execution instances from reusable
  definitions before Workflow APIs stabilize. Target model:
  - replace the current execution-instance `Workflow` API with `WorkflowRun`;
  - `WorkflowRun.spec` supports either inline `jobs` or top-level `uses` plus
    `with` inputs;
  - add a reusable `Workflow` CRD whose jobs can be called from a
    `WorkflowRun` job with `uses: <workflow-name>` and optional `with`;
  - add a reusable `Action` CRD whose steps can be called from a
    `WorkflowRun` or `Workflow` step with `uses: <action-name>` and optional
    `with`;
  - keep names namespace-local in the first version; avoid verbose
    `workflowRef` and `actionRef` fields until cross-namespace or remote
    references are required;
  - validation must enforce mutually exclusive shapes: top-level `uses` vs
    inline `jobs`, job `uses` vs `steps`, and step `uses` vs `run`;
  - Actions run inside the caller job context and share that job runtime,
    workspace, artifacts, and environment unless a future API explicitly
    overrides them;
  - reusable Workflow jobs have their own job/workspace/artifact boundary and
    communicate with callers through inputs, outputs, and artifacts;
  - update CRDs, controller reconciliation, CLI verbs, docs, and E2E around the
    new `WorkflowRun`, `Workflow`, and `Action` split.
- [ ] Dashboard: design and build a read-only web dashboard, similar in spirit
  to Tekton Dashboard, that can browse Runs by namespace and inspect status and
  logs.
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
