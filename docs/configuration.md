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

## Gateway client-certificate authentication

## Aggregated Run-log API

`logAPI.enabled` defaults to `true` independently of `gateway.enabled`. It
installs the `logs.kruntimes.io/v1alpha1` APIService and a dedicated backend.
This is the default transport for `krt logs` and Dashboard: normal kubeconfig
authentication, including client certificates, is verified by the Kubernetes
API server. The caller needs both exact `get` on the Run and `get` on the
`logs.kruntimes.io` `runs/log` subresource; it never needs `pods/log`.

The APIService is cluster-scoped, so a cluster can have one aggregated log API
backend for this API group/version. When installing a second kruntimes platform
release in the same cluster, retain `logAPI.enabled: true` for the release that
owns the API and set it to `false` for every other release.

```yaml
logAPI:
  enabled: true
dashboard:
  logs:
    mode: aggregation
```

Set `dashboard.logs.mode: gateway` only to opt into the direct Gateway path.
Likewise, `krt logs --gateway-url=https://...` is an explicit direct-client
choice. The direct Gateway client-certificate configuration below remains
useful for that external endpoint, but is not needed for ordinary kubeconfig
use of the aggregated API.

The Runtime Gateway always accepts Kubernetes bearer tokens. To additionally
allow `krt logs` to use a kubeconfig client certificate, enable Gateway HTTPS
and provide the Secret key containing the CA that signs Kubernetes user
certificates:

```yaml
gateway:
  enabled: true
  protocols:
    - https
  tls:
    clientCASecretName: kubernetes-user-client-ca
    clientCAKey: ca.crt
```

The Gateway requests a client certificate but does not require one, so bearer
tokens continue to work. A presented certificate must verify against this CA;
its X.509 CN becomes the Kubernetes username and its O values become groups
for the exact-Run SubjectAccessReview. This is distinct from the Gateway server
certificate and should normally be a separately managed Secret.

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
caller's bearer token. Log requests are authorized by the Runtime Gateway
against the exact Run, so the caller needs `get` on that `runs` resource,
not `pods/log`.

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
