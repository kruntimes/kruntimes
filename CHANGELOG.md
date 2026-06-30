# Changelog

All notable changes to kruntimes are recorded here.

This project follows Semantic Versioning while the public API remains
`v1alpha1`. During `v0.x`, breaking API or behavior changes can still happen,
but each release note must call them out explicitly.

## Unreleased

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
