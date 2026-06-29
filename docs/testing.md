# Testing Guide

This guide lists the test suites and when to run them.

## Unit Tests

```bash
make test
```

Covers Go packages outside integration and E2E tests. Also runs generation,
formatting, vet, and protobuf generation prerequisites.

## Integration Tests

```bash
make test-integration
```

Uses envtest for controller and CRD behavior.

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
