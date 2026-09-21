# Changelog

All notable changes to kruntimes are recorded here.

This project follows Semantic Versioning while the public API remains
`v1alpha1`. During `v0.x`, breaking API or behavior changes can still happen,
but each release note must call them out explicitly.

## Unreleased

## 0.0.5 - 2026-09-21

### Added

- Added experimental `Workflow`, `WorkflowRun`, `Action`, and
  `PersistentWorkspace` APIs with reusable Actions, job workspaces, artifact
  transfer, execution snapshots, cancellation, and terminal aggregation.
- Added experimental Function and Session Run modes, built-in Bash/Python
  Runtime Server support, lifecycle controls, and Runtime Gateway operations.
- Added the Kubernetes aggregated Run log API, `krt logs` support, and a
  Dashboard with Runtime, Run, WorkflowRun, log, theme, and authenticated-user
  views.
- Added Runtime Pod readiness visibility, resource-aware scheduling, affinity,
  reservations, and scheduler framework metrics.
- Added Go and Python sandbox SDKs, the Kubernetes diagnosis-agent demo, and a
  CI-style WorkflowRun demo.
- Added `krt version` to print the CLI version, commit, and build timestamp.
- Added a GitHub Benchmark workflow that runs the default hot-path benchmark in
  the same kind-based environment as E2E.
- Added documentation site theme controls for light, dark, and system
  preference modes.

### Changed

- Improved benchmark correctness by separating execution latency from
  end-to-end backlog latency and preventing capacity probe Runs from
  contaminating measured samples.
- Changed default benchmark parameters to a no-sleep hot-path case with enough
  Runtime capacity for all Runs.
- Stabilized Run input semantics so `source.inline` executes as a standalone
  script, while `entrypoint` and `args` apply only to non-inline execution
  paths.
- Updated the default Helm application images to `0.0.5`.

### Fixed

- Reaped orphaned Bash Runtime children and preserved executable artifact
  inputs.
- Added a release preflight that rejects chart `appVersion` drift before a tag
  is created, and documented GitHub Container Registry package access checks.

## 0.0.3 - 2026-07-01

### Changed

- Renamed published container image packages to remove the redundant
  `kruntimes-` prefix. New images are published as `scheduler`, `controller`,
  `runtimed`, `bash-runtime`, and `python-runtime` under
  `ghcr.io/kruntimes/`.
- Updated Helm chart defaults to use the published `ghcr.io/kruntimes/*`
  image repositories directly.

## 0.0.2 - 2026-06-30

### Added

- Initial release process documentation for SemVer tags, changelog entries, and
  release notes.
- Release preflight, artifact verification, and failed-release handling
  guidance.
- GitHub Actions release workflow for multi-platform `krt` CLI binaries with
  checksums and provenance attestations.
- Kubernetes, Helm, Go, Python, and `krt` release artifact compatibility
  matrix.
- Operations guide covering installation, upgrade, uninstall,
  troubleshooting, backup, and restore procedures.
- Custom Runtime development guide covering Runtime Server protocol semantics,
  Runtime CRD template ownership, capacity, and compatibility expectations.
- GitHub Actions release workflow for publishing Helm OCI charts to GitHub
  Container Registry.
- GitHub Pages custom domain configuration for `https://kruntimes.io/`.

### Changed

- Decoupled Helm chart package versions from kruntimes application versions
  while keeping chart `appVersion` aligned with release tags.
- Improved release image build performance with BuildKit cache reuse and native
  cross-compilation for Go-based multi-architecture images.
