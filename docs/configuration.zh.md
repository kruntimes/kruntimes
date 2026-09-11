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

## Dashboard TLS

Dashboard 是 `kruntimes` chart 的 opt-in 组件，并且只暴露 HTTPS。对于本地开发或明确受信任的
部署，以下配置会启用 chart 生成的默认 certificate：

```yaml
dashboard:
  enabled: true
```

要挂载已有 TLS Secret，需要显式选择该来源：

```yaml
dashboard:
  enabled: true
  tls:
    selfSigned: false
    secretName: dashboard-tls
```

要由 cert-manager 签发 certificate，关闭 chart 生成并引用已有 Issuer 或 ClusterIssuer。该
issuer 本身可以是 cert-manager 的 self-signed issuer。

```yaml
dashboard:
  enabled: true
  tls:
    selfSigned: false
    secretName: dashboard-tls
    certManager:
      enabled: true
      issuerRef:
        name: platform-ca
        kind: ClusterIssuer
```

`selfSigned`、已有 Secret 和 `certManager.enabled` 是互斥选择。Service 始终为 `ClusterIP`；
ingress 或其它对外暴露需要单独配置。

## Gateway client-certificate authentication

## 聚合 Run-log API

`logAPI.enabled` 独立于 `gateway.enabled`，默认是 `true`。它安装
`logs.kruntimes.io/v1alpha1` APIService 及独立 backend。这是 `krt logs` 和 Dashboard 的默认
transport：普通 kubeconfig authentication（包括 client certificate）由 Kubernetes API server 验证。
caller 需要目标 Run 的 exact `get`，以及 `logs.kruntimes.io` `runs/log` subresource 的 `get`；不需要
`pods/log`。

该 APIService 是 cluster-scoped，因此同一集群中一个 API group/version 只能有一个聚合
Run-log API backend。若在同一集群安装第二个 kruntimes platform release，应保留拥有该 API
的 release 的 `logAPI.enabled: true`，并将其它 release 设为 `false`。

```yaml
logAPI:
  enabled: true
dashboard:
  logs:
    mode: aggregation
```

仅在需要直连 Gateway 时设置 `dashboard.logs.mode: gateway`；同样，
`krt logs --gateway-url=https://...` 是显式的直连选择。下文直连 Gateway 的 client-certificate
配置仍适用于外部 endpoint，但普通 kubeconfig 使用聚合 API 时不需要它。

Runtime Gateway 始终接受 Kubernetes bearer tokens。要让 `krt logs` 也能使用 kubeconfig
client certificate，启用 Gateway HTTPS，并提供包含签发 Kubernetes user certificate 的 CA 的
Secret key：

```yaml
gateway:
  enabled: true
  protocols:
    - https
  tls:
    clientCASecretName: kubernetes-user-client-ca
    clientCAKey: ca.crt
```

Gateway 请求 client certificate，但不强制要求它，因此 bearer tokens 仍可工作。提供的
certificate 必须由该 CA 验证；其 X.509 CN 会成为 Kubernetes username，O values 会成为用于
exact-Run SubjectAccessReview 的 groups。这与 Gateway server certificate 不同，通常应由单独管理
的 Secret 提供。

### 公开资源列表

启用 Dashboard 时，namespace、Run、Runtime 和 WorkflowRun **列表**默认无需 token 即可查看。
该行为由 `dashboard.publicRead.enabled` 控制；设置为 `false` 后，所有 API 请求都需要 bearer token：

```yaml
dashboard:
  publicRead:
    enabled: false
```

chart 只给 Dashboard ServiceAccount 授予 `namespaces`、`runs`、`runtimes` 和 `workflowruns` 的
`get`/`list` 权限。它们的详情仍必须使用 caller 的 bearer token。日志请求由 Runtime Gateway
针对 exact Run 授权，因此 caller 需要该 `runs` resource 的 `get`，而不需要 `pods/log`。

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
