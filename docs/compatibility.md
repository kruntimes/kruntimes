# Compatibility Matrix

kruntimes is currently a `v0.x` experimental project with `v1alpha1` APIs. This
matrix documents the versions that are intentionally tested or used for release
artifacts. Anything outside these rows may work, but is not part of the current
public compatibility claim.

## Policy

- Compatibility claims are updated through normal PRs.
- Minor releases may change supported versions while the API remains
  experimental.
- A release should not claim support for a Kubernetes, Helm, Go, or Python
  version unless CI, release workflows, or documented manual validation cover
  that version.

## Kubernetes

| Scope | Version | Status | Evidence |
| --- | --- | --- | --- |
| API/controller integration tests | `1.32` | Tested | `ENVTEST_K8S_VERSION = 1.32` in the integration test workflow. |
| E2E cluster | kind default Kubernetes version | Tested before public release tags | The E2E workflow creates or reuses the `kruntimes-e2e` kind cluster. |
| Newer Kubernetes minors | Not certified | Best effort | The project uses Kubernetes client libraries from `k8s.io/* v0.36.x`, but newer API server versions need explicit validation before being documented as supported. |

## Helm

| Scope | Version | Status | Evidence |
| --- | --- | --- | --- |
| Helm chart rendering | Helm 3 | Required | Charts use `apiVersion: v2`; chart validation runs `helm lint` and `helm template`. |
| Multi-release and multi-namespace installs | Helm 3 | Tested | `hack/verify-helm-multi-release.py` and `hack/verify-helm-multi-namespace.py`. |
| Helm OCI chart publication | Helm 3 OCI registry support | Released by `Release Charts` | Charts are packaged and pushed to `oci://ghcr.io/<owner>/charts`. |

## Go

| Scope | Version | Status | Evidence |
| --- | --- | --- | --- |
| Module toolchain | `1.26.4` | Required | `go.mod` `go` directive. |
| Docker image builds | `1.26.4` | Required | Go builder images in project Dockerfiles. |
| Local generated tools | Pinned in `Makefile` | Required | `controller-gen`, `setup-envtest`, `golangci-lint`, `govulncheck`, `protoc`, and proto plugins are version checked before use. |

## Python

| Scope | Version | Status | Evidence |
| --- | --- | --- | --- |
| Python Runtime release image | `3.14.6-slim-trixie` | Required | `Dockerfile.python-runtime`. |
| Python Runtime package lower bound | `>=3.12` | Required | `runtimes/python/pyproject.toml`. |
| Python Runtime unit tests | `3.12` | Tested | CI uses `astral-sh/setup-uv` with `python-version: "3.12"`. |
| Dependency lockfile | `uv.lock` | Required | Docker builds use `uv sync --locked`. |

## krt CLI Release Artifacts

| Platform | Architecture | Status |
| --- | --- | --- |
| Linux | `amd64`, `arm64` | Released by `Release CLI`. |
| macOS | `amd64`, `arm64` | Released by `Release CLI`. |
| Windows | `amd64` | Released by `Release CLI`. |

Each CLI archive is accompanied by a checksum file and GitHub artifact
provenance attestation. See `docs/release.md` for verification commands.
