# Operations Guide

This guide covers day-two operations for the current cluster-wide kruntimes
installation model.

kruntimes is still `v0.x experimental` with `v1alpha1` APIs. Back up manifests
and values before upgrades, and read the release notes for breaking changes.

## Installation

Install the platform chart once per cluster. It installs CRDs, scheduler,
controller, cluster-scoped RBAC, metrics Services, and optional monitoring
resources.

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

Install built-in Runtime CRs into each namespace that should host Runtime Pods
and Runs:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace \
  --set bash.image=<bash-runtime-image> \
  --set python.image=<python-runtime-image>
```

Use explicit image tags or digests for shared clusters. Do not depend on
mutable local image names outside development.

## Upgrade

Before upgrading:

1. Read the release notes and `CHANGELOG.md`.
2. Check `docs/compatibility.md` for Kubernetes, Helm, Go, Python, and CLI
   compatibility changes.
3. Back up Helm values and kruntimes custom resources.
4. Run the release preflight checks if building from source.

Upgrade the platform first so CRDs and controllers are ready for any Runtime
schema or behavior changes:

```bash
helm upgrade kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --reuse-values
```

Then upgrade Runtime definitions in each workload namespace:

```bash
helm upgrade kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --reuse-values
```

After upgrading, verify the control plane and Runtime Pods:

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,runs -A
kubectl get pods -A -l runtime
```

## Uninstall

Remove built-in Runtime releases from workload namespaces before removing the
platform:

```bash
helm uninstall kruntimes-runtimes --namespace default --ignore-not-found
helm uninstall kruntimes --namespace kruntimes-system --ignore-not-found
```

The platform uninstall does not delete CRDs by default. This is intentional:
deleting CRDs deletes all `Runtime`, `Run`, and `Workflow` objects cluster-wide.
Only remove CRDs after backing up or intentionally discarding those objects:

```bash
kubectl delete crd runs.kruntimes.io runtimes.kruntimes.io workflows.kruntimes.io
```

External artifact stores, object buckets, PVC contents, and external logging
systems are not deleted by Helm uninstall.

## Backup

At minimum, back up:

- Helm release values for the platform and Runtime charts.
- `Runtime`, `Run`, and `Workflow` custom resources from namespaces you intend
  to restore.
- Secrets referenced by Runtime artifact stores or custom Runtime Pod
  templates.
- PVCs or object storage buckets used by artifact stores.

Example Kubernetes object backup:

```bash
kubectl get runtime,runs,workflows -A -o yaml > kruntimes-objects.yaml
helm get values kruntimes -n kruntimes-system --all > kruntimes-values.yaml
helm get values kruntimes-runtimes -n default --all > kruntimes-runtimes-values.yaml
```

Artifact and log backups are backend-specific. For filesystem artifact stores,
back up the referenced PVCs. For S3-compatible stores, back up the bucket or
prefix according to the provider's procedures. kruntimes does not manage log
collection storage.

## Restore

Restore in this order:

1. Restore external dependencies such as Secrets, PVCs, object buckets, and
   logging backends.
2. Install or upgrade the platform chart.
3. Restore `Runtime` objects and Runtime chart releases.
4. Restore `Run` and `Workflow` objects only when their referenced Runtime and
   artifact store configuration are available.

Artifact cleanup uses the persisted `Run.status.artifactStore` snapshot. For
old Runs that do not have this snapshot, the controller may need the original
Runtime artifact store configuration to continue cleanup.

## Troubleshooting

### Run stays Pending

Check that a Runtime with the requested name exists in the same namespace and
that its Pods are ready:

```bash
kubectl get runtime,pods -n <namespace>
kubectl describe run <run> -n <namespace>
```

The scheduler only assigns to Runtime Pods that are Kubernetes Ready,
`kruntimes.io/RuntimedReady`, and below configured capacity.

### Run is Scheduled but not Running

Check the assigned Pod and runtimed logs:

```bash
kubectl get run <run> -n <namespace> -o yaml
kubectl logs <runtime-pod> -n <namespace> -c runtimed
```

If the Runtime Pod disappeared, retry behavior depends on `Run.spec.retry`.

### Runtime Pods are not Ready

Inspect the Runtime, generated Deployment, and Pod events:

```bash
kubectl describe runtime <runtime> -n <namespace>
kubectl describe deploy runtime-<runtime> -n <namespace>
kubectl describe pod <runtime-pod> -n <namespace>
```

Common causes include image pull failures, insufficient resources, missing
Secrets, invalid Pod template fields, or a Runtime Server that does not answer
health checks.

### Artifact cleanup is stuck

Check the Run finalizers, stored artifact configuration, and runtime maintainer
Deployment:

```bash
kubectl get run <run> -n <namespace> -o yaml
kubectl get deploy,pods -n <namespace> -l app.kubernetes.io/component=runtime-maintainer
kubectl logs deploy/<runtime-maintainer-deployment> -n <namespace>
```

Missing artifact credentials, deleted PVCs, deleted buckets, or unavailable S3
endpoints can prevent finalizer cleanup until the dependency is restored.

### krt cannot read logs or artifacts

`krt logs` and artifact downloads use Kubernetes port-forwarding to a Runtime
Pod. The user needs permission to get Runs and Pods and to create
`pods/portforward` in the namespace.

```bash
kubectl auth can-i get runs -n <namespace>
kubectl auth can-i get pods -n <namespace>
kubectl auth can-i create pods/portforward -n <namespace>
```
