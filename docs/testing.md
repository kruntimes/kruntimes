# Testing Guide

This guide lists the test suites and when to run them.

## Unit Tests

```bash
make test
```

Covers Go packages outside integration and E2E tests. Also runs generation,
formatting, vet, and protobuf generation prerequisites.

### Caller-managed tool and cache paths

Build tools and downloads do not require a writable container root filesystem.
Set standard environment variables to direct them to a writable workspace; the
Makefile passes `GOBIN` and `TMPDIR` to every tool bootstrap, while Go and uv
honor their own cache variables.

```bash
export GOBIN="$PWD/.tools/bin"
export TMPDIR="$PWD/.tmp"
export GOPATH="$PWD/.go"
export GOCACHE="$PWD/.cache/go-build"
export GOMODCACHE="$PWD/.cache/go-mod"
export UV_CACHE_DIR="$PWD/.cache/uv"
mkdir -p "$GOBIN" "$TMPDIR"
make test
```

This also applies to `make proto`, `make test-integration`, `make lint`, and
the security targets.

## Integration Tests

```bash
make test-integration
```

Uses envtest for controller and CRD behavior.

To run one integration test with the same envtest setup:

```bash
make test-integration-run INTEGRATION_TEST=TestSessionFilePaginationContract
```

## Race Detector

```bash
make test-race
```

Focused race coverage for controller, scheduler, runtimed, and Bash Runtime.

## Helm Tests

```bash
make test-helm
```

Validates chart linting, template rendering, multi-release rendering, and
multi-namespace rendering.

## Python Runtime Tests

```bash
cd runtimes/python
uv sync --frozen
uv run --frozen python -m unittest server_test -v
```

## Python Sandbox SDK and Agent Demo

Run the SDK lifecycle and local gateway adapter tests, plus the Kubernetes
diagnosis agent's allowlist tests, without adding them to the Runtime's locked
Python environment:

```bash
make test-sdk-python
UV_CACHE_DIR=/tmp/kruntimes-uv-cache uv build --directory sdk/python
```

## E2E Tests

```bash
make e2e
```

`make e2e` builds images, creates or reuses a kind cluster, loads images,
deploys Helm charts, and runs E2E tests.

Use this when changes affect:

- CRD behavior,
- scheduling,
- runtimed execution,
- Helm install paths,
- artifact storage,
- CLI behavior against a real cluster.

## Benchmarks

```bash
make benchmark
```

The benchmark uses the E2E setup path and measures scheduling latency,
throughput, Runtime capacity behavior, and control-plane request latency.

See [Performance Benchmarks](benchmarks.md).

## Security and Dependency Checks

```bash
make govulncheck
```

Security workflow also runs scheduled scans in GitHub Actions.

## Adding Tests

- Add unit tests near the package being changed.
- Add integration tests for controller-runtime, CRD validation, and admission
  behavior.
- Add E2E tests for behavior that only appears in a real cluster.
- Update docs when tests cover user-visible behavior.
