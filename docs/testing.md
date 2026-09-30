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

### Suite layout

The suite lives in `test/e2e/` and is organized by responsibility instead of a
single multi-thousand-line file:

| File | Responsibility |
| --- | --- |
| `main_test.go` | Kubernetes clients, image references, feature switches |
| `runtime_fixture_test.go` | Runtime pools, pod readiness, diagnostics, restarts |
| `wait_test.go` | condition-driven waits |
| `run_assert_test.go` | Run assertions and termination requests |
| `gateway_fixture_test.go` | Console gateway and log API fixtures |
| `artifact_fixture_test.go` | artifact store assertions |
| `runtime_test.go` | Runtime pool and one-shot Run execution |
| `session_test.go` | Session mode and Console gateway |
| `function_test.go` | Function mode |
| `workflow_test.go` | Workflows, actions, and reusable workflows |
| `artifact_test.go` | artifact export and staging |
| `workspace_test.go` | PersistentWorkspace |
| `scheduler_test.go` | placement, capacity, affinity, cancellation |
| `diagnosis_test.go` | demo diagnosis runtime |

Scenarios create resources with `GenerateName` or a timestamp suffix and clean
them up with `t.Cleanup`, so no scenario depends on the order in which another
scenario ran. The only long-lived shared state is the warm `bash` and `python`
Runtime pools: the first scenario that needs one creates it, and later
scenarios reuse it instead of re-issuing the same Runtime update and pod wait.

### Parallel execution

Scenarios that do not mutate cluster-wide state call `t.Parallel()`. The suite
runs at most `E2E_PARALLEL` (default `4`) of them at a time:

```bash
make e2e                        # default fan-out
E2E_PARALLEL=1 make e2e         # serial, historical order
make e2e-test E2E_PARALLEL=8    # reuse an existing cluster, wider fan-out
```

The following scenarios stay serial and intentionally omit `t.Parallel()`:

- `TestWorkflowRunRecoversActionAfterControllerRestart` restarts the controller
  Deployment, which pauses reconciliation for every other scenario.
- `TestRuntimeReadyReplicasTracksRuntimedAvailability`,
  `TestRuntimedRecoversRunningRunAfterRestart`, and
  `TestFunctionRunRecoversInvocationAfterRuntimedRestart` kill `runtimed` in a
  Runtime pod to exercise restart recovery.
- The `scheduler_test.go` scenarios assert scheduling latency, capacity, and
  wake-up ordering, which parallel load would distort.
- `TestSessionGatewayServesCertManagerTLS` runs in the dedicated
  `e2e-cert-manager-run` and `e2e-console-bounds-run` jobs.

Parallelism is bounded by cluster capacity, not by the tests: each parallel
scenario may create its own Runtime pod, and the shared warm pools are created
with 8 Run slots so scenarios do not queue behind each other on one pod.
Scenario-owned Runtime CRs keep the product default capacity.

### Timings

`make e2e-test` runs the suite through `hack/e2e-timings`, which preserves the
normal test log and additionally writes:

- `e2e-timings.json` — per-test durations, uploaded as the `e2e-timings` CI
  artifact, and
- `e2e-timings.md` — slowest-test table appended to the workflow run summary.

Durations are compared against `test/e2e/timings-baseline.json`. Tests that are
both at least 25% and at least 2s slower than the baseline are listed as
regressions; the comparison is advisory because E2E durations depend on the
runner, and it never fails the build. Use `-regression-percent`,
`-regression-seconds`, and `-top` to change the reporting thresholds:

```bash
go test ./test/e2e/... -json | go run ./hack/e2e-timings -top 30 -report /tmp/e2e.json
```

Snapshot and commit a new baseline after an intentional suite change by copying
the `e2e-timings.json` artifact over `test/e2e/timings-baseline.json`.

### Measured improvement

The numbers below come from `make e2e` runs on a GitHub-hosted runner with the
same 65 tests (63 run, 2 environment-gated skips):

| Suite | Fan-out | Test phase | `make e2e` job |
| --- | --- | --- | --- |
| pre-split baseline | serial | 435.1s | 13m17s |
| condition waits + shared pools | serial (`E2E_PARALLEL=1`) | 418.0s | 13m21s |
| condition waits + shared pools + parallel | 4 | 180.0s | 10m40s |

Condition-driven waits and the shared warm pools remove about 4% of the test
phase on their own; safe parallelism removes a further 57% relative to serial,
for a 58.6% reduction in total test-phase wall clock. The remaining slowest
cases are bounded by product timers rather than by the harness: the pod-loss
and stale-Run scenarios wait for the controller's 30-second stale requeue, so
they stay at 32-37s each regardless of scheduling. Image builds, kind cluster
creation, and Helm deployment are unchanged and dominate the rest of the job.

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
