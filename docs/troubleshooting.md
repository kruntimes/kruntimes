# Troubleshooting

This guide covers common failures and the first commands to run.

## Run Stays Pending

Check the Run:

```bash
kubectl get run <name> -o yaml
```

Common causes:

- no Runtime with matching `spec.runtime`,
- no Runtime Pods in the Run namespace,
- Runtime Pods are not `Ready`,
- runtimed heartbeat is missing or stale,
- all Runtime Pods are at capacity.

Inspect Runtime Pods:

```bash
kubectl get pods -l runtime=<runtime-name>
kubectl describe pod -l runtime=<runtime-name>
```

## Run Is Scheduled but Not Running

Check assigned pod and runtimed logs:

```bash
kubectl get run <name> -o jsonpath='{.status.assignedPod}'
kubectl logs <assigned-pod> -c runtimed
```

Common causes:

- runtimed cannot claim the Run,
- Runtime Server is not reachable,
- Runtime Server returned a transient error,
- workspace preparation failed.

## Runtime Pods Are Not Ready

```bash
kubectl get runtime <name> -o yaml
kubectl get deploy,pods -l runtime=<name>
kubectl describe pod -l runtime=<name>
```

Common causes:

- runtime image cannot be pulled,
- container port does not match Runtime Server port,
- readiness or runtimed heartbeat is failing,
- custom ServiceAccount lacks required permissions.

## Image Pull Backoff in Local Clusters

If a local kind or minikube cluster shows `ImagePullBackOff`, confirm the
Runtime image reference matches an image available to that cluster:

```bash
kubectl describe pod <runtime-pod>
```

For local clusters, either load locally built images using the cluster tool or
configure the Helm values to use images in a registry the cluster can pull from.

## Artifact Cleanup Is Stuck

```bash
kubectl get run <name> -o yaml
kubectl get deploy -l kruntimes.io/runtime-maintainer=true
kubectl logs deploy/<runtime-maintainer-deploy>
```

Common causes:

- artifact store credentials were deleted,
- external object store is unavailable,
- old Run status lacks a durable artifact store snapshot.

Cleanup is designed to be idempotent and resume after transient failures.

## krt Cannot Read Logs or Artifacts

Check RBAC for `pods/portforward` in the Runtime namespace. Logs and artifact
access may require port-forward permission to the assigned Runtime Pod or
runtime maintainer service.

## Helm Install Fails

Render manifests locally:

```bash
helm template kruntimes ./charts/kruntimes --namespace <namespace>
```

Contributors can run chart validation from the repository; see the
[Testing Guide](testing.md).

## Generated Files Changed After Tests

Generated file workflows are contributor tasks. See the
[Development Guide](development.md) for code generation commands and commit
expectations.

## Need More Help

See [SUPPORT.md](https://github.com/kruntimes/kruntimes/blob/main/SUPPORT.md)
for support channels and expectations.
