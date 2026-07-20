# Performance Benchmarks

kruntimes includes an opt-in benchmark harness for measuring scheduler latency,
completion throughput, Runtime capacity behavior, and control-plane request
latency against a real Kubernetes cluster.

Benchmark numbers depend on cluster size, node pressure, storage, image
locality, API server configuration, and the benchmark parameters. Always record
the command, cluster type, Kubernetes version, kruntimes image tags or digests,
Runtime replica/capacity settings, and the full JSON output when using numbers
in release notes or comparisons.

## How to Run Benchmarks

### Environment

The benchmark runs on Kubernetes. You can run it against:

- a local kind cluster created by the project E2E setup, or
- a GitHub Actions runner through the `Benchmark` workflow.

For local runs, use the same setup path as E2E tests:

```bash
make benchmark
```

`make benchmark` runs `make e2e-setup` first. That builds fresh local images,
loads them into the configured kind cluster, upgrades the platform chart, and
then runs the benchmark with the exact images loaded into kind.

If the environment already exists, run only the harness:

```bash
make benchmark-run
```

The GitHub `Benchmark` workflow runs the same `e2e-setup` environment in kind
and records the default hot-path benchmark in the workflow summary.

### Default Parameters

The default benchmark is the no-sleep hot-path case:

| Parameter | Default |
| --- | ---: |
| `KRUNTIMES_BENCHMARK_RUNS` | `50` |
| `KRUNTIMES_BENCHMARK_CONCURRENCY` | `25` |
| `KRUNTIMES_BENCHMARK_REPLICAS` | `2` |
| `KRUNTIMES_BENCHMARK_CAPACITY` | `64` |
| `KRUNTIMES_BENCHMARK_SLEEP` | `0s` |
| `KRUNTIMES_BENCHMARK_POLL_INTERVAL` | `50ms` |
| `KRUNTIMES_BENCHMARK_CAPACITY_PROBE` | `false` |

Total Runtime capacity is intentionally higher than the number of Runs, so the
default result is not dominated by capacity queueing.

### Parameterized Example

To run a backlog/drain case with workload sleep and constrained capacity, pass
parameters from the outside:

```bash
KRUNTIMES_BENCHMARK_RUNS=50 \
KRUNTIMES_BENCHMARK_CONCURRENCY=10 \
KRUNTIMES_BENCHMARK_REPLICAS=2 \
KRUNTIMES_BENCHMARK_CAPACITY=4 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
KRUNTIMES_BENCHMARK_CAPACITY_PROBE=true \
make benchmark-run
```

This intentionally creates more Runs than Runtime capacity so the benchmark
covers backlog drain after earlier Runs finish.

### Output Fields

- `latency.schedule`: time from local create request start until the scheduler
  writes the `Scheduled=True` condition.
- `latency.dispatch`: time from local create request start until runtimed writes
  `status.startTime`.
- `latency.execution`: time from `status.startTime` to
  `status.completionTime`. This excludes queueing before runtimed starts the
  Run.
- `latency.complete`: time from local create request start until
  `status.completionTime`. This is end-to-end latency and includes time waiting
  for Runtime capacity when the benchmark is capacity constrained.

The harness polls the Kubernetes API to discover state changes, but it derives
lifecycle latency from the timestamps written into `Run.status`; the polling
interval therefore does not inflate a reported transition duration.
- `throughput.runsPerSecond`: successful terminal Runs divided by benchmark wall
  time.
- `capacity.maxObservedRunningRuns`: maximum concurrent Running Runs observed
  during polling.
- `capacity.observedPendingAtCapacity`: whether pending work was observed while
  all configured Runtime slots were occupied.
- `controlPlane.apiCreate`, `controlPlane.apiList`, and `controlPlane.apiGet`:
  client-side Kubernetes API request latency during the benchmark.
- `controlPlane.pods`: scheduler/controller readiness and restart counts in the
  configured control-plane namespace.

## Local Results

### Default Hot-Path Result

Command:

```bash
make benchmark-run
```

Parameters: 50 Runs, 2 Runtime Pods, 64 Run slots per Pod, 25 concurrent create
requests, no workload sleep, 50 ms polling, and capacity probe disabled.

| Metric | p50 | p95 | Notes |
| --- | ---: | ---: | --- |
| `latency.schedule` | 207.361 ms | 302.429 ms | local create start to assigned Pod observed by benchmark |
| `latency.dispatch` | 207.456 ms | 322.593 ms | local create start to Running observed by benchmark |
| `latency.execution` | 372.785 ms | 449.617 ms | Running to terminal observed by benchmark |
| `latency.complete` | 580.422 ms | 627.611 ms | local create start to terminal observed by benchmark |

Additional observations:

- successful Runs: 50
- throughput: 11.30 Runs/s
- configured Runtime capacity: 128 Run slots
- max observed Running Runs: 50
- pending at capacity: false

### Backlog/Drain Result with Parameters

Command:

```bash
KRUNTIMES_BENCHMARK_RUNS=50 \
KRUNTIMES_BENCHMARK_CONCURRENCY=10 \
KRUNTIMES_BENCHMARK_REPLICAS=2 \
KRUNTIMES_BENCHMARK_CAPACITY=4 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
KRUNTIMES_BENCHMARK_CAPACITY_PROBE=true \
make benchmark-run
```

Parameters: 50 Runs, 2 Runtime Pods, 4 Run slots per Pod, 10 concurrent create
requests, 500 ms workload sleep, and capacity probe enabled.

| Metric | p50 | p95 | Notes |
| --- | ---: | ---: | --- |
| `latency.schedule` | 4419.550 ms | 5830.094 ms | includes backlog queueing |
| `latency.dispatch` | 4615.892 ms | 6224.137 ms | includes backlog queueing |
| `latency.execution` | N/A | N/A | not collected by the older result |
| `latency.complete` | 5112.102 ms | 6625.569 ms | end-to-end backlog/drain latency |

Additional observations:

- successful Runs: 50
- throughput: 5.05 Runs/s
- configured Runtime capacity: 8 Run slots
- max observed Running Runs: 8
- pending at capacity: true
