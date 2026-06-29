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

- Publish a comparison guide: kruntimes vs Knative, Argo Workflows, Tekton,
  Volcano, and a worker queue on a Deployment.
- Add a clear "when to use / when not to use" guide so users understand that
  kruntimes is a warm execution substrate, not a full serverless platform,
  workflow engine, batch scheduler replacement, or hostile-code sandbox.
- Recruit design partners from platform, CI, and AI agent infrastructure teams
  that run short-lived, high-concurrency, or agent-driven workloads.
- Validate the core problem with 5-8 target users and capture whether they have
  experienced Pod cold start, burst throughput, or infrastructure-ownership
  constraints in the last six months.
- Publish three end-to-end demos: low-latency Bash/Python Run, burst short-task
  execution, and custom Runtime skeleton.
- Track go/no-go signals: users can explain the value in two minutes, at least
  two design partners try it on real workloads, and at least one non-maintainer
  completes the quick start.

### v0.x Experimental

- Keep public documentation aligned with implementation.
- Harden E2E coverage for scheduling, artifact cleanup, and workflow behavior.
- Improve CLI ergonomics and examples.
- Expand custom Runtime examples.
- Continue supply-chain and security hardening.
- Choose and validate the first primary wedge. The current hypothesis is AI
  agent tools and trusted internal code-execution sandboxes, with CI micro-steps
  and automation tasks as secondary use cases.

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
