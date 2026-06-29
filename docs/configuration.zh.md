---
title: "配置"
---

本页总结最常见的配置面。

## Helm Values

platform chart 配置：

- scheduler 和 controller replicas，
- image repositories、tags 和 pull policy，
- imagePullSecrets，
- leader election，
- service accounts 和 RBAC，
- security contexts，
- metrics Services，
- 可选 ServiceMonitor，
- node selectors、tolerations 和 affinity。

应用前可以先渲染 chart 输出：

```bash
helm template kruntimes ./charts/kruntimes --namespace kruntimes-system
```

仅贡献者使用的 Make variables 和 chart validation commands 见
[Development Guide](development.md) 和 [Testing Guide](testing.md)。

## Runtime Capacity

Runtime capacity 在 Runtime CRD 中声明：

```yaml
spec:
  capacity:
    resources:
      runs: 4
      gpu: 1
```

controller 会把声明的静态 capacity 复制到 Runtime Pod annotations。scheduler 会从 Run
state 跟踪快速变化的 active usage。

## Runtime Pod Template

Runtime Pod 自定义配置位于 `Runtime.spec.template`。

```yaml
spec:
  template:
    spec:
      serviceAccountName: custom-runtime-sa
      nodeSelector:
        workload: kruntimes
      tolerations:
        - key: dedicated
          operator: Equal
          value: runtimes
          effect: NoSchedule
```

controller 保留 kruntimes 需要的字段。不要覆盖注入的 `runtimed` container，也不要覆盖
kruntimes 管理的 labels 和 annotations。

## Artifact Stores

workloads 将 artifacts 写到 `$KRUNTIME_ARTIFACTS_DIR` 下，并通过 Runtime artifact store
持久化。

支持的 backends：

- filesystem/PVC，
- S3-compatible object storage。

Run status 在 `status.artifactRefs` 中存储有界 metadata，而不是完整 artifact contents。

## 暴露给 Runs 的环境变量

| Variable | Purpose |
| --- | --- |
| `KRUNTIME_OUTPUTS` | workloads 写入有界 `KEY=VALUE` outputs 的文件。 |
| `KRUNTIME_ARTIFACTS_DIR` | workloads 写入需要持久化的文件和目录的位置。 |

## Benchmark Variables

| Variable | Default | Description |
| --- | --- | --- |
| `KRUNTIMES_BENCHMARK_RUNS` | `50` | benchmark harness 创建的 Runs 数量。 |
| `KRUNTIMES_BENCHMARK_CONCURRENCY` | `10` | 并发 Kubernetes create requests 数量。 |
| `KRUNTIMES_BENCHMARK_REPLICAS` | `2` | Runtime replica count。 |
| `KRUNTIMES_BENCHMARK_CAPACITY` | `4` | 每个 Runtime Pod 的 Runs capacity。 |
| `KRUNTIMES_BENCHMARK_SLEEP` | `500ms` | workload sleep 时长。 |

见 [Performance Benchmarks](benchmarks.md)。
