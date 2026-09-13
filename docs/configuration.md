# Configuration

This page summarizes the most common configuration surfaces.

## Helm Values

The platform chart configures:

- scheduler and controller replicas,
- image repositories, tags, and pull policy,
- imagePullSecrets,
- leader election,
- service accounts and RBAC,
- security contexts,
- metrics Services,
- optional ServiceMonitor,
- node selectors, tolerations, and affinity.

Render chart output before applying:

```bash
helm template kruntimes ./charts/kruntimes --namespace kruntimes-system
```

Contributor-only Make variables and chart validation commands are documented in
the [Development Guide](development.md) and [Testing Guide](testing.md).

## Dashboard TLS

The Dashboard is an opt-in component of the `kruntimes` chart and exposes
HTTPS only. Enable it with the default chart-generated certificate for local or
explicitly trusted deployments:

```yaml
dashboard:
  enabled: true
```

To mount an existing TLS Secret, select that source explicitly:

```yaml
dashboard:
  enabled: true
  tls:
    selfSigned: false
    secretName: dashboard-tls
```

To have cert-manager issue the certificate, disable chart generation and
reference an existing Issuer or ClusterIssuer. The issuer may itself be a
self-signed cert-manager issuer.

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

`selfSigned`, an existing Secret, and `certManager.enabled` are mutually
exclusive choices. The Service is always `ClusterIP`; configure ingress or
other external exposure separately.

## Aggregated Run-log API

`logAPI.enabled` defaults to `true` independently of `gateway.enabled`. It
installs the `logs.kruntimes.io/v1alpha1` APIService and a dedicated backend.
This is the default transport for `krt logs` and Dashboard: normal kubeconfig
authentication, including client certificates, is verified by the Kubernetes
API server. The caller needs `get` on the `logs.kruntimes.io` `runs/log`
subresource; it never needs `kruntimes.io/runs get` or `pods/log`.

The APIService is cluster-scoped, so a cluster can have one aggregated log API
backend for this API group/version. When installing a second kruntimes platform
release in the same cluster, retain `logAPI.enabled: true` for the release that
owns the API and set it to `false` for every other release.

```yaml
logAPI:
  enabled: true
```

Dashboard and `krt logs` always use this aggregated API. The Runtime Gateway
continues to serve Session and Function endpoints, but does not serve Run logs.

### Public resource lists

When the Dashboard is enabled, namespace, Run, Runtime, and WorkflowRun
*lists* are available without a token by default. This is controlled by
`dashboard.publicRead.enabled`; set it to `false` to require a bearer token for
every API request:

```yaml
dashboard:
  publicRead:
    enabled: false
```

The chart grants the Dashboard ServiceAccount only `get`/`list` on
`namespaces`, `runs`, `runtimes`, and `workflowruns`. Their details require the
caller's bearer token. Log requests require `get` on the
`logs.kruntimes.io` `runs/log` subresource.

## Runtime Capacity

Runtime capacity is declared on the Runtime CRD:

```yaml
spec:
  capacity:
    resources:
      runs: 4
      gpu: 1
```

The controller copies declared static capacity to Runtime Pod annotations. The
scheduler tracks fast-changing active usage from Run state.

## Runtime Pod Template

Runtime Pod customization lives in `Runtime.spec.template`.

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

The controller reserves fields needed by kruntimes. Do not override the
injected `runtimed` container or kruntimes-managed labels and annotations.

## Artifact Stores

Artifacts are written below `$KRUNTIME_ARTIFACTS_DIR` by workloads and persisted
through the Runtime artifact store.

Supported backends:

- filesystem/PVC,
- S3-compatible object storage.

Run status stores bounded metadata in `status.artifactRefs`, not full artifact
contents.

## Environment Variables Exposed to Runs

| Variable | Purpose |
| --- | --- |
| `KRUNTIME_OUTPUTS` | File where workloads write bounded `KEY=VALUE` outputs. |
| `KRUNTIME_ARTIFACTS_DIR` | Directory where workloads write files and directories to persist as artifacts. |

## Benchmark Variables

| Variable | Default | Description |
| --- | --- | --- |
| `KRUNTIMES_BENCHMARK_RUNS` | `50` | Number of Runs created by the benchmark harness. |
| `KRUNTIMES_BENCHMARK_CONCURRENCY` | `10` | Concurrent Kubernetes create requests. |
| `KRUNTIMES_BENCHMARK_REPLICAS` | `2` | Runtime replica count. |
| `KRUNTIMES_BENCHMARK_CAPACITY` | `4` | Runs capacity per Runtime Pod. |
| `KRUNTIMES_BENCHMARK_SLEEP` | `500ms` | Workload sleep duration. |

See [Performance Benchmarks](benchmarks.md).
