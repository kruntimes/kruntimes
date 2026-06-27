# Open Source Readiness Plan

This document tracks the work needed before kruntimes is presented as a mature
open source project. The goal is not to complete every production-grade feature
before the first public release. The goal is to make the legal boundaries,
quality bar, release process, security model, documentation, and current project
status clear enough for users and contributors.

The detailed checklist is currently maintained in the
[Simplified Chinese version](open-source-readiness.zh.md).

## Release Positioning

The first public release should be positioned as `v0.x experimental`:

- APIs are `v1alpha1` and may change before a stable release.
- Built-in Runtimes are for trusted workloads and do not provide strong tenant
  isolation.
- Supported Kubernetes versions, installation scope, upgrade boundaries, and
  security limits must be clearly documented.
- Public release blockers should be complete before the repository is opened.

## Readiness Areas

The readiness plan covers:

- legal, license, security, support, governance, and contributor baseline,
- required CI and reproducible validation,
- Runtime concurrency and resource lifecycle safety,
- workflow state correctness,
- bounded outputs, artifact references, and artifact cleanup,
- scheduling, retry, timeout, and cancellation semantics,
- runtime API contracts and custom Runtime development,
- Helm installation, upgrade, uninstall, and operations guidance,
- release automation, SBOM, provenance, and image signing,
- documentation structure, website, troubleshooting, and community entry points.

## Current Status

Most P0 and P1 implementation and documentation items have been completed. The
remaining work is tracked in the source checklist and roadmap, including
repository settings that must be completed after the repository is public.

See [Project Status and Roadmap](roadmap.md) for the public-facing roadmap.
