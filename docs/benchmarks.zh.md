---
title: "性能基准测试"
---

kruntimes 包含一个可选的 benchmark harness，用于在真实 Kubernetes 集群上测量 scheduler
latency、完成吞吐量、Runtime capacity 行为和 control-plane request latency。

benchmark 数字会受到集群规模、节点压力、存储、镜像本地性、API server 配置以及 benchmark
参数影响。如果要在 release notes 或对比中使用这些数字，请同时记录命令、集群类型、
Kubernetes 版本、kruntimes image tags 或 digests、Runtime replica/capacity 设置，以及完整
JSON 输出。

## 如何运行 Benchmark

### 环境

benchmark 运行在 Kubernetes 上。可以使用：

- 项目 E2E setup 创建的本地 kind 集群，或
- GitHub Actions 中的 `Benchmark` workflow。

本地运行可以使用与 E2E 测试相同的 setup path：

```bash
make benchmark
```

`make benchmark` 会先运行 `make e2e-setup`。它会 build 最新本地镜像、加载到配置的 kind
集群、升级 platform chart，然后使用加载到 kind 中的精确镜像运行 benchmark。

如果环境已经存在，只运行 harness：

```bash
make benchmark-run
```

GitHub `Benchmark` workflow 会在 kind 中运行同样的 `e2e-setup` 环境，并在 workflow
summary 中记录默认 hot-path benchmark。

### 默认参数

默认 benchmark 是 no-sleep hot-path case：

| Parameter | Default |
| --- | ---: |
| `KRUNTIMES_BENCHMARK_RUNS` | `50` |
| `KRUNTIMES_BENCHMARK_CONCURRENCY` | `25` |
| `KRUNTIMES_BENCHMARK_REPLICAS` | `2` |
| `KRUNTIMES_BENCHMARK_CAPACITY` | `64` |
| `KRUNTIMES_BENCHMARK_SLEEP` | `0s` |
| `KRUNTIMES_BENCHMARK_POLL_INTERVAL` | `50ms` |
| `KRUNTIMES_BENCHMARK_CAPACITY_PROBE` | `false` |

总 Runtime capacity 有意高于 Run 数量，因此默认结果不会主要被 capacity queueing 主导。

### 带参数示例

如果要运行带 workload sleep 且限制 capacity 的 backlog/drain case，可以从外部传入参数：

```bash
KRUNTIMES_BENCHMARK_RUNS=50 \
KRUNTIMES_BENCHMARK_CONCURRENCY=10 \
KRUNTIMES_BENCHMARK_REPLICAS=2 \
KRUNTIMES_BENCHMARK_CAPACITY=4 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
KRUNTIMES_BENCHMARK_CAPACITY_PROBE=true \
make benchmark-run
```

这个 case 会有意创建超过 Runtime capacity 的 Runs，以覆盖早期 Runs 完成后的 backlog
drain。

### 输出字段

- `latency.schedule`：benchmark observer 从本地 create request start 到 scheduler 分配
  Runtime Pod 的观测时间。
- `latency.dispatch`：benchmark observer 从本地 create request start 到 runtimed 将 Run
  标记为 started 的观测时间。
- `latency.execution`：benchmark observer 从 Run start 到 Run 进入 terminal status 的观测
  时间。它不包含 runtimed 启动 Run 前的 benchmark backlog queueing，但仍受 benchmark
  poll interval 影响。
- `latency.complete`：benchmark observer 从本地 create request start 到 terminal Run status
  的观测时间。这是端到端 latency；当 benchmark 受 capacity 限制时，它会包含等待 Runtime
  capacity 的时间。
- `throughput.runsPerSecond`：成功 terminal Runs 除以 benchmark wall time。
- `capacity.maxObservedRunningRuns`：polling 期间观察到的最大并发 Running Runs 数。
- `capacity.observedPendingAtCapacity`：当所有配置的 Runtime slots 被占用时，是否观察到
  pending work。
- `controlPlane.apiCreate`、`controlPlane.apiList` 和 `controlPlane.apiGet`：benchmark
  期间客户端 Kubernetes API request latency。
- `controlPlane.pods`：配置的 control-plane namespace 中 scheduler/controller readiness
  和 restart counts。

## 本地结果

### 默认 Hot-Path 结果

命令：

```bash
make benchmark-run
```

参数：50 个 Runs、2 个 Runtime Pods、每个 Pod 64 个 Run slots、25 个并发创建请求、无
workload sleep、50 ms polling，并关闭 capacity probe。

| Metric | p50 | p95 | Notes |
| --- | ---: | ---: | --- |
| `latency.schedule` | 207.361 ms | 302.429 ms | benchmark 观测的本地创建开始到分配 Pod |
| `latency.dispatch` | 207.456 ms | 322.593 ms | benchmark 观测的本地创建开始到 Running |
| `latency.execution` | 372.785 ms | 449.617 ms | benchmark 观测的 Running 到 terminal |
| `latency.complete` | 580.422 ms | 627.611 ms | benchmark 观测的本地创建开始到 terminal |

其它观察结果：

- successful Runs：50
- throughput：11.30 Runs/s
- configured Runtime capacity：128 Run slots
- max observed Running Runs：50
- pending at capacity：false

### 带参数的 Backlog/Drain 结果

命令：

```bash
KRUNTIMES_BENCHMARK_RUNS=50 \
KRUNTIMES_BENCHMARK_CONCURRENCY=10 \
KRUNTIMES_BENCHMARK_REPLICAS=2 \
KRUNTIMES_BENCHMARK_CAPACITY=4 \
KRUNTIMES_BENCHMARK_SLEEP=500ms \
KRUNTIMES_BENCHMARK_CAPACITY_PROBE=true \
make benchmark-run
```

参数：50 个 Runs、2 个 Runtime Pods、每个 Pod 4 个 Run slots、10 个并发创建请求、500 ms
workload sleep，并启用 capacity probe。

| Metric | p50 | p95 | Notes |
| --- | ---: | ---: | --- |
| `latency.schedule` | 4419.550 ms | 5830.094 ms | 包含 backlog queueing |
| `latency.dispatch` | 4615.892 ms | 6224.137 ms | 包含 backlog queueing |
| `latency.execution` | N/A | N/A | 旧结果未采集该字段 |
| `latency.complete` | 5112.102 ms | 6625.569 ms | 端到端 backlog/drain latency |

其它观察结果：

- successful Runs：50
- throughput：5.05 Runs/s
- configured Runtime capacity：8 Run slots
- max observed Running Runs：8
- pending at capacity：true
