# Performance Benchmarks

kruntimes includes an opt-in benchmark harness for measuring scheduler latency,
completion throughput, Runtime capacity behavior, and control-plane request
latency against a real Kubernetes cluster.

The benchmark is not part of default CI because results depend on cluster size,
node pressure, storage, image locality, and API server configuration. Run it
with the same environment setup used by E2E tests:

```bash
make benchmark
```

`make benchmark` creates a temporary Bash `Runtime`, submits all benchmark
`Run` objects up front, waits for all terminal phases, and prints a JSON report.
The default run uses 2 Runtime Pods, 4 concurrent Run slots per pod, 50 total
Runs, and 10 concurrent create requests. This intentionally creates more Runs
than the default Runtime capacity so the benchmark covers backlog drain after
earlier Runs finish.

`make benchmark` depends on `make e2e-setup`, so it builds images with the
fresh E2E tag, loads them into the configured kind cluster, upgrades the
platform chart, and then passes the exact `E2E_IMG_BASH_RUNTIME` and
`E2E_IMG_RUNTIMED` tags to the benchmark harness. The benchmark Runtime also
sets the runtime container image pull policy to `IfNotPresent`, so kind uses the
loaded images instead of trying to pull them from a registry.

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

## Current Local Result

This result was captured on 2026-06-27 with `make benchmark` against a local
kind cluster created by `make e2e-setup`. It uses the default benchmark settings:
2 Runtime Pods, 4 Run slots per pod, 50 total Runs, 10 concurrent create
requests, and 500 ms workload sleep.

```json
{
  "benchmarkID": "bench-1782529296",
  "startedAt": "2026-06-27T11:01:36.424462547+08:00",
  "completedAt": "2026-06-27T11:01:46.315089871+08:00",
  "options": {
    "namespace": "default",
    "controlPlaneNamespace": "default",
    "runtimeName": "benchmark-bash",
    "runs": 50,
    "concurrency": 10,
    "replicas": 2,
    "capacityPerPod": 4,
    "sleepSeconds": 0.5,
    "timeoutSeconds": 300
  },
  "latency": {
    "schedule": {
      "count": 50,
      "minMs": 33.222,
      "p50Ms": 4419.55,
      "p95Ms": 5830.094,
      "maxMs": 6226.511
    },
    "dispatch": {
      "count": 50,
      "minMs": 3034.152,
      "p50Ms": 4615.892,
      "p95Ms": 6224.137,
      "maxMs": 6226.511
    },
    "complete": {
      "count": 50,
      "minMs": 3436.66,
      "p50Ms": 5112.102,
      "p95Ms": 6625.569,
      "maxMs": 7026.797
    }
  },
  "throughput": {
    "successfulRuns": 50,
    "failedRuns": 0,
    "wallSeconds": 9.890627325,
    "runsPerSecond": 5.055291070730945
  },
  "capacity": {
    "readyRuntimePods": 2,
    "configuredTotalRunSlots": 8,
    "maxObservedRunningRuns": 8,
    "maxObservedRunningByPod": {
      "runtime-benchmark-bash-785b988db8-7gp57": 4,
      "runtime-benchmark-bash-785b988db8-qkxfk": 4
    },
    "observedPendingAtCapacity": true,
    "assignedRunsByPod": {
      "runtime-benchmark-bash-785b988db8-7gp57": 24,
      "runtime-benchmark-bash-785b988db8-qkxfk": 26
    },
    "runtimePodNames": [
      "runtime-benchmark-bash-785b988db8-7gp57",
      "runtime-benchmark-bash-785b988db8-qkxfk"
    ],
    "runtimePodRestarts": {
      "runtime-benchmark-bash-785b988db8-7gp57": 0,
      "runtime-benchmark-bash-785b988db8-qkxfk": 0
    }
  },
  "controlPlane": {
    "apiCreate": {
      "count": 50,
      "minMs": 2.546,
      "p50Ms": 4.694,
      "p95Ms": 9.901,
      "maxMs": 10.801
    },
    "apiList": {
      "count": 36,
      "minMs": 3.812,
      "p50Ms": 5.173,
      "p95Ms": 6.5,
      "maxMs": 9.064
    },
    "apiGet": {
      "count": 50,
      "minMs": 1.182,
      "p50Ms": 1.442,
      "p95Ms": 1.886,
      "maxMs": 2.288
    },
    "pods": [
      {
        "name": "kruntimes-controller-cb4df56ff-lfq8x",
        "ready": true,
        "restarts": 0,
        "component": "controller"
      },
      {
        "name": "kruntimes-scheduler-7476d58d74-585gj",
        "ready": true,
        "restarts": 0,
        "component": "scheduler"
      },
      {
        "name": "kruntimes-scheduler-7476d58d74-q88bv",
        "ready": true,
        "restarts": 0,
        "component": "scheduler"
      }
    ]
  },
  "terminalPhase": {
    "Succeeded": 50
  }
}
```
