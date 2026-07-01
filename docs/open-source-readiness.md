---
title: "Open Source Readiness Plan"
---

This document tracks the improvements kruntimes needs to complete before the
repository is made public. The goal is not to complete every production-grade
capability before the first public release, but to ensure that the project has
clear legal boundaries, credible baseline quality, reproducible release
processes, and functional and security claims that match the current
implementation.

## Release Positioning

The first public release should be positioned as `v0.x experimental`:

- APIs are `v1alpha1` and do not yet promise backward compatibility.
- Built-in Runtimes are for trusted workloads only and do not provide strong
  security isolation.
- Supported Kubernetes versions, installation scope, and upgrade boundaries
  must be clearly documented.
- All P0 items must be completed before the repository is made public.

## P0: Public Release Blockers

### 1. Legal and Community Baseline

- [x] Add an open source license and required copyright notices.
- [x] Add `SECURITY.md` describing the vulnerability reporting channel,
  response scope, and supported versions.
- [x] Add `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SUPPORT.md`.
- [x] Add maintainer list, `CODEOWNERS`, issue and PR templates.
- [x] Enable main branch protection after going public, requiring CI pass
  and at least one review.

Acceptance criteria:

- GitHub correctly identifies the project license.
- New contributors can develop, test, and submit a PR without private
  communication.
- A private reporting channel exists for security issues.

### 2. Required CI

- [x] CI runs `make test`, `make test-integration`, and Helm lint/template.
- [x] CI runs the Go race detector, covering at least runtime, scheduler,
  controller, and runtimed.
- [x] CI runs Python Runtime unit tests.
- [x] Add `govulncheck`, dependency update bot, and basic secret scanning.
- [x] Add generated-file consistency checks to ensure generated Go API and
  CRD files stay current.
- [x] Run `make e2e` on a schedule or per release.

Acceptance criteria:

- All required checks are reproducible in a clean environment.
- Generated file drift, data races, and reachable vulnerabilities block
  merge.

### 3. Runtime Concurrency and Resource Lifecycle

The Bash Runtime had real data races between execution goroutines and
`Status`, `List`, `Cancel`, and duplicate `Execute`, reproduced by
`go test -race`.

- [x] Establish complete concurrency protection and immutable state
  snapshots for every Bash Runtime execution.
- [x] Establish complete concurrency protection and immutable state
  snapshots for every Python Runtime execution.
- [x] Set bounded buffers for Bash Runtime stdout/stderr; prevent unbounded
  memory growth.
- [x] Set bounded buffers for Python Runtime stdout/stderr; prevent
  unbounded memory growth.
- [x] Add execution `Forget` lifecycle to the Runtime API.
- [x] Clean up `/workspace/<runUID>` after Run completion while preserving
  the ordering required for artifact upload.
- [x] Bash Runtime cancellation and timeout terminate the entire process
  group and wait for exit.
- [x] Python Runtime cancellation and timeout terminate the entire process
  group and wait for exit.
- [x] Fix concurrent access to shared task state in the Python Runtime.
- [x] Clarify the isolation approach for Python handler mode; mark as
  trusted-code only until isolation is in place.
- [x] Apply `workspace.sizeLimit` to the Runtime Pod `emptyDir`.

Acceptance criteria:

- `go test -race` passes completely.
- Runtime task count, workspace usage, and memory do not grow
  monotonically under sustained Run load.
- No orphaned child processes remain after cancel or timeout.

### 4. Workflow State Correctness

- [x] Map child Run `Timeout` and `Cancelled` to terminal
  Step/Job/Workflow states.
- [x] Unknown `needs` must fail explicitly at admission or reconciliation.
- [x] Validate that a step includes a currently supported execution method;
  do not silently accept unimplemented `uses`.
- [x] Apply Kubernetes name and label constraints to job/step names.
- [x] Use truncation with a stable hash when generating child Run names to
  avoid overly long names.
- [x] Add controller and E2E tests for the above scenarios.

Acceptance criteria:

- Valid Workflows eventually converge to `Succeeded` or `Failed`.
- Invalid DAGs produce a clear error at creation or first reconciliation
  and do not remain Pending/Running indefinitely.

### 5. Security and Trust Boundary

- [x] Reject absolute paths and entrypoints containing `..`; ensure source
  writes do not escape the Run workspace.
- [x] Restrict Git source protocols; add clone/checkout timeout, size, and
  output limits.
- [x] Document Runtime and Run creation permissions and their security
  implications.
- [x] Add network access control for runtimed status/artifact gRPC; at
  minimum provide a default NetworkPolicy.
- [x] Provide secure default security contexts for all platform and
  built-in Runtime containers.
- [x] Disable default privilege escalation, enable seccomp, and set
  read-only root filesystem per container capabilities.
- [x] Write a threat model that clearly states the limitations of multiple
  Runs sharing process, network, and workspace within the same Runtime Pod.
- [x] Fix README claims about "per-Run resource limits" and "clean
  workspace" that exceed the current implementation.

Acceptance criteria:

- Documentation does not imply that built-in Runtimes can safely run
  untrusted code.
- Default charts install on clusters with Pod Security Standards enabled.
- Run inputs cannot access unauthorized local resources through paths or
  Git sources.

### 6. Installation Scope and Helm Correctness

- [x] Explicitly choose a cluster-wide or single-namespace installation
  model.
- [x] The runtimed ServiceAccount referenced by Runtime Deployments must
  exist in the Runtime namespace.
- [x] Add a namespace-scoped RBAC controller for custom
  `Runtime.spec.template.spec.serviceAccountName` to ensure the
  corresponding ServiceAccount has the minimum permissions required by
  runtimed.
- [x] Use `Runtime.spec.template` (`PodTemplateSpec`) to unify Runtime Pod
  customization, replacing duplicated PodSpec-like fields in the Runtime
  spec, and clearly document controller-reserved fields.
- [x] Use release fullname for all Helm resource names to support multiple
  co-existing releases.
- [x] Remove unused values; make replicas, leader election, ports,
  imagePullSecrets, security context, and scheduling constraints
  configurable.
- [x] Avoid default `latest` tags; image tags should follow chart
  appVersion.
- [x] Add multi-namespace template/install tests.
- [x] Add template tests for a second Helm release.

Acceptance criteria:

- The documented namespace model actually works.
- When a custom Runtime ServiceAccount is used, permission grants are
  namespace-scoped, minimal, and auditable.
- The Runtime Pod customization model has clear boundaries before going
  public, without needing to support two overlapping APIs in the future.
- Two releases can be installed in the same cluster without resource naming
  conflicts.
- Chart defaults reference publicly released, immutable image versions.

### 7. Artifact Cleanup Ownership

The artifact finalizer was originally executed by runtimed inside the
Runtime Pod, which prevented cleanup when the Runtime was deleted or scaled
to zero. The controller now reads a persisted store configuration snapshot
from Run status and ensures a long-running runtime maintainer Deployment
exists per artifact store hash. The maintainer does not depend on the
current Runtime spec or on whether the Runtime still exists.

- [x] Migrate artifact finalizer cleanup to an independent, long-lived
  controller or provide equivalent central GC.
- [x] Define recovery semantics when store configuration is deleted,
  credentials are lost, or external objects do not exist.
- [x] Roll back partial upload failures to avoid orphaned objects.
- [x] Enforce a final storage size limit before S3 upload, rather than
  uploading first and rejecting later.
- [x] Add E2E tests for finalizer failure recovery and post-Runtime-deletion
  cleanup.

Acceptance criteria:

- Runs with artifact finalizers can be deleted even when the Runtime no
  longer exists.
- After transient storage failures recover, cleanup resumes automatically
  and remains idempotent.

### 8. Supply Chain and Known Vulnerabilities

At review time, `govulncheck` detected 5 reachable vulnerabilities:

- Go `1.26.3` standard library issues, fixed in `1.26.4`.
- `golang.org/x/net v0.49.0` issues, requiring an upgrade to at least
  `v0.55.0`.

Improvements:

- [x] Upgrade Go toolchain and affected dependencies so that
  `govulncheck` passes.
- [x] Pin Docker base images to explicit patch versions and digests.
- [x] Install Python dependencies via `uv.lock` rather than directly
  installing unpinned versions.
- [x] Pin versions of controller-gen, setup-envtest, golangci-lint, protoc
  plugins, and uv in the Makefile.
- [x] Generate SBOMs for release images and sign them with cosign or an
  equivalent mechanism.

Acceptance criteria:

- No known reachable high/critical vulnerabilities exist on release
  branches.
- Build tools, base images, and language dependencies are all reproducible.

## P1: Correctness and Operability

- [x] Stale reaper checks both Kubernetes `PodReady` and
  `kruntimes.io/RuntimedReady` heartbeat.
- [x] Stale reaper returns status update errors and unifies terminal
  condition updates.
- [x] Fix scheduler initialization order where `mgr` was used before
  checking `NewManager` error.
- [x] Runtimed actively distinguishes transient Status errors from
  execution `NotFound`.
- [x] `krt run --wait` correctly handles `Timeout` and `Cancelled`.
- [x] `krt logs` handles both stdout/stderr, avoiding stderr duplication
  and offset out-of-bounds.
- [x] CLI uses Cobra command context, supports kubeconfig/context, current
  namespace, and `json`/`yaml` output.
- [x] Add a real readiness check instead of a hardcoded success response.
- [x] Add CEL validation and field size limits for CRDs.
- [x] Use Kubernetes list-map markers for Run/Workflow conditions.
- [x] Add queue time, dispatch latency, retry, failure, and active Run
  metrics.
- [x] Chart creates metrics Services and provides optional ServiceMonitor.
- [ ] Clarify and normalize `args`, `source.inline`, and `entrypoint`
  semantics across built-in Runtimes, the CLI, API docs, and examples.

## P2: Release and Contributor Experience

- [x] Establish SemVer, CHANGELOG, and release notes processes.
- [x] Publish scheduler/controller/runtimed/runtime images.
- [x] Publish Helm OCI charts.
- [x] Organize project documentation site with Hugo and publish via
  GitHub Pages.
- [x] Publish `krt` multi-platform binaries, checksums, and provenance.
- [x] Add Kubernetes/Helm/Go/Python compatibility matrix.
- [x] Add installation, upgrade, uninstall, troubleshooting, backup, and
  restore documentation.
- [x] Add custom Runtime development guide and protocol compatibility
  contract.
- [x] Add performance benchmarks: scheduling latency, throughput, Runtime
  capacity, and control-plane load.
- [x] Align README roadmap so only capabilities with implementation, tests,
  and a working install path are marked complete.

## P3: Post-Public Product Validation

- [x] Publish a comparison guide covering kruntimes vs Knative, Argo Workflows,
  Tekton, Volcano, and a worker queue on a Deployment.
- [x] Add a "when to use / when not to use" guide that clearly positions
  kruntimes as a warm execution substrate rather than a full serverless
  platform, workflow engine, batch scheduler replacement, or hostile-code
  sandbox.
- [ ] Recruit 5-8 target users from platform, CI, and AI agent infrastructure
  teams and validate whether they experience Pod cold start, burst throughput,
  or infrastructure-ownership constraints.
- [x] Publish three end-to-end demos: low-latency Bash/Python Run, burst
  short-task execution, and custom Runtime skeleton.
- [x] Define and track public adoption signals: users can explain the value in
  two minutes, at least two design partners try real workloads, and at least one
  non-maintainer completes the quick start.
- [ ] Validate the first primary wedge. The current hypothesis is AI agent
  tools and trusted internal code-execution sandboxes, with CI micro-steps and
  automation tasks as secondary use cases.

## Suggested PR Sequence

1. **Repository baseline**: license, community files, GitHub templates, basic CI.
2. **Runtime safety**: race, process group cancellation, bounded output,
   execution/workspace cleanup.
3. **Workflow correctness**: terminal propagation, DAG validation, name
   constraints.
4. **Input security**: entrypoint, Git source, trusted workload documentation.
5. **Helm scope**: namespace model, resource naming, ServiceAccount, security
   defaults.
6. **Artifact lifecycle**: central finalizer controller, rollback, recovery tests.
7. **Supply chain**: toolchain/dependency upgrades, image pinning, SBOM and
   signing.
8. **CLI and observability**: terminal handling, logs, metrics, readiness.
9. **Release automation and docs**: images, chart, CLI, compatibility,
   operations.

Items 2, 3, and 4 can proceed in parallel once API dependencies are clear.
Items 5 and 6 require agreement on installation scope and artifact store
configuration ownership first.

## Audit Evidence

Results from this review:

- `make test`: passed.
- `make test-integration`: passed.
- Helm lint: passed.
- Python Runtime unit tests: passed.
- `go test -race`: failed, reproduced Bash Runtime concurrent reads/writes.
- `make govulncheck`: passed.
- Full `make e2e`: not executed during this review.
