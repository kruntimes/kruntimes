---
title: "性能基准测试"
---

kruntimes 包含一个可选的 benchmark harness，用于在真实 Kubernetes 集群上测量调度延迟、
完成吞吐量、Runtime 容量行为以及控制平面请求延迟。

基准测试不属于默认 CI，因为结果取决于集群规模、节点压力、存储、镜像本地性和 API server
配置。使用与 E2E 测试相同的环境设置运行它：

```bash
make benchmark
```

`make benchmark` 创建一个临时 Bash `Runtime`，一次性提交所有 benchmark `Run` 对象，
等待所有终止阶段完成，并打印 JSON 报告。默认运行使用 2 个 Runtime Pods、每个 Pod 4 个
并发 Run 槽位、50 个总 Run 数和 10 个并发创建请求。这有意创建的 Run 数量超过默认
Runtime 容量，以便基准测试覆盖早期 Runs 完成后的积压消化过程。

`make benchmark` 依赖 `make e2e-setup`，因此它会使用最新的 E2E tag 构建镜像，
将其加载到配置的 kind 集群中，升级 platform chart，然后将精确的 `E2E_IMG_BASH_RUNTIME`
和 `E2E_IMG_RUNTIMED` tags 传递给 benchmark harness。benchmark Runtime 还将 runtime
容器镜像拉取策略设置为 `IfNotPresent`，使 kind 使用已加载的镜像而不是尝试从 registry 拉取。

常用覆盖参数：

```bash
KRUNTIMES_BENCHMARK_RUNS=200 \
KRUNTIMES_BENCHMARK_CONCURRENCY=25 \
KRUNTIMES_BENCHMARK_REPLICAS=4 \
KRUNTIMES_BENCHMARK_CAPACITY=8 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
make benchmark
```

输出包含：

- `latency.schedule`：从本地创建请求开始到调度器分配 Runtime Pod 的时间。
- `latency.dispatch`：从本地创建请求开始到 runtimed 将 Run 标记为已启动的时间。
- `latency.complete`：从本地创建请求开始到 Run 进入终止状态的时间。
- `throughput.runsPerSecond`：成功的终止 Runs 除以基准测试墙钟时间。
- `capacity.maxObservedRunningRuns`：轮询期间观察到的最大并发 Running Runs 数。
- `capacity.observedPendingAtCapacity`：在所有配置的 Runtime 槽位都被占用时是否观察到待处理 work。
- `controlPlane.apiCreate`、`controlPlane.apiList` 和 `controlPlane.apiGet`：
  基准测试期间的客户端 Kubernetes API 请求延迟。
- `controlPlane.pods`：配置的控制平面 namespace 中 scheduler/controller 的就绪状态和重启计数。

对于发布说明，记录 benchmark 命令、集群类型、Kubernetes 版本、kruntimes 镜像 tag 或
digest、Runtime 副本/容量设置以及完整的 JSON 输出。不要在不同集群之间比较数据而不说明
环境差异。

## 本地当前结果

此结果于 2026-06-27 使用 `make benchmark` 在 `make e2e-setup` 创建的本地 kind 集群上
捕获。使用默认 benchmark 设置：2 个 Runtime Pods，每个 Pod 4 个 Run 槽位，50 个总
Run 数，10 个并发创建请求，500 ms workload sleep。

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
