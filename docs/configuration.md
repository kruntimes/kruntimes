# Configuration

This page summarizes the most common configuration surfaces.

## Make Variables

| Variable | Default | Description |
| --- | --- | --- |
| `NAMESPACE` | `default` | Namespace used by deploy and E2E helpers. |
| `CONTAINER_TOOL` | `docker` | Container CLI used for image builds and pushes. |
| `HELM` | `helm` | Helm binary. |
| `IMG_SCHEDULER` | `kruntimes-scheduler:latest` | Scheduler image for build/deploy targets. |
| `IMG_CONTROLLER` | `kruntimes-controller:latest` | Controller image. |
| `IMG_RUNTIMED` | `kruntimes-runtimed:latest` | runtimed sidecar image. |
| `IMG_BASH_RUNTIME` | `kruntimes-bash-runtime:latest` | Bash Runtime image. |
| `IMG_PYTHON_RUNTIME` | `kruntimes-python-runtime:latest` | Python Runtime image. |
| `KIND_CLUSTER_NAME` | `kruntimes-e2e` | kind cluster used by E2E helpers. |
| `E2E_IMAGE_TAG` | `latest` | Image tag loaded into kind for E2E setup. |

Example:

```bash
CONTAINER_TOOL=podman NAMESPACE=kruntimes-system make e2e-setup
```

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

Validate chart changes:

```bash
make test-helm
```

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
| `KRUNTIMES_BENCHMARK_RUNS` | `50` | Number of Runs created by `make benchmark`. |
| `KRUNTIMES_BENCHMARK_CONCURRENCY` | `10` | Concurrent Kubernetes create requests. |
| `KRUNTIMES_BENCHMARK_REPLICAS` | `2` | Runtime replica count. |
| `KRUNTIMES_BENCHMARK_CAPACITY` | `4` | Runs capacity per Runtime Pod. |
| `KRUNTIMES_BENCHMARK_SLEEP` | `500ms` | Workload sleep duration. |

See [Performance Benchmarks](benchmarks.md).
