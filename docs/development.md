# Development Guide

This guide covers local development for contributors.

## Requirements

- Go version from `go.mod`
- Docker or compatible container tool
- Helm 3
- kubectl
- kind
- Python 3.12+ and uv for Python Runtime work

Pinned tool versions are declared in `Makefile`. The Make targets install or
verify local tools before use. Targets that download prebuilt binaries (protoc,
Helm) select the release asset for the host OS and architecture; set
`PROTOC_ARCH` or `HELM_PLATFORM` to fetch another platform's asset instead.

## Repository Setup

```bash
git clone https://github.com/kruntimes/kruntimes.git
cd kruntimes
make test
```

## Generate Code and CRDs

```bash
make generate manifests
```

Generated files must stay current. CI fails if generation changes tracked files.

## Build Binaries

```bash
make build
```

Individual targets:

```bash
make build-scheduler
make build-controller
make build-runtimed
make build-cli
make build-bash-runtime
```

## Build Images

```bash
make docker-build
```

Override image names:

```bash
IMG_CONTROLLER=ghcr.io/example/controller:dev make docker-build-controller
```

## Local E2E Environment

```bash
make e2e-setup
make e2e-test
```

Or run both:

```bash
make e2e
```

To validate the optional cert-manager certificate flow, install it only for
that E2E run and name the focused test explicitly:

```bash
make e2e-cert-manager-run E2E_TEST=TestSessionGatewayServesCertManagerTLS
```

To validate the opt-in gateway request, response, and header bounds with their
small E2E-specific values:

```bash
make e2e-gateway-bounds-run E2E_TEST=TestSessionGatewayEnforcesTransferBounds
```

Clean up:

```bash
make e2e-cleanup
```

## Python Runtime

```bash
cd runtimes/python
uv sync --frozen
uv run --frozen python -m unittest server_test -v
```

Regenerate Python protobuf stubs from the repository root:

```bash
make proto-python
```

## Contribution Flow

1. Open or reference an issue when the change is non-trivial.
2. Create a branch.
3. Keep changes focused.
4. Run the relevant tests.
5. Open a PR using the template.

Read [CONTRIBUTING.md](https://github.com/kruntimes/kruntimes/blob/main/CONTRIBUTING.md),
[GOVERNANCE.md](https://github.com/kruntimes/kruntimes/blob/main/GOVERNANCE.md),
and [CODE_OF_CONDUCT.md](https://github.com/kruntimes/kruntimes/blob/main/CODE_OF_CONDUCT.md).

## API Compatibility

The project is currently experimental. Even so, public API and CRD changes
should include:

- generated CRD updates,
- tests,
- documentation updates,
- changelog entry when user-visible.
