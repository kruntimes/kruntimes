# Performance Benchmarks

kruntimes includes an opt-in benchmark harness for measuring scheduler latency,
completion throughput, Runtime capacity behavior, and control-plane request
latency against a real Kubernetes cluster.

The benchmark is not part of default CI because results depend on cluster size,
node pressure, storage, image locality, and API server configuration. Run it
against an environment that already has the kruntimes platform installed:

```bash
make e2e-setup
make benchmark
```

`make benchmark` creates a temporary Bash `Runtime`, submits a batch of short
`Run` objects, waits for all terminal phases, and prints a JSON report. The
default run uses 2 Runtime Pods, 4 concurrent Run slots per pod, 50 total Runs,
and 10 concurrent create requests.

Useful overrides:

```bash
KRUNTIMES_BENCHMARK_RUNS=200 \
KRUNTIMES_BENCHMARK_CONCURRENCY=25 \
KRUNTIMES_BENCHMARK_REPLICAS=4 \
KRUNTIMES_BENCHMARK_CAPACITY=8 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
make benchmark
```

The output contains:

- `latency.schedule`: time from local create request start until the scheduler
  assigns a Runtime Pod.
- `latency.dispatch`: time from local create request start until runtimed marks
  the Run started.
- `latency.complete`: time from local create request start until terminal Run
  status.
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

For release notes, record the benchmark command, cluster type, Kubernetes
version, kruntimes image tags or digests, Runtime replica/capacity settings, and
the full JSON output. Do not compare numbers across different clusters without
calling out the environment difference.
