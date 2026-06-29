# Installation

kruntimes is installed with Helm. The current model is cluster-wide platform
installation plus namespace-local Runtime definitions.

## Requirements

- Kubernetes cluster with CRD support.
- Helm 3.
- kubectl configured for the target cluster.
- kruntimes images that the cluster can pull.

See [Compatibility Matrix](compatibility.md) for the versions intentionally
tested by the project.

## Kubernetes Cluster

kruntimes runs on Kubernetes. Start with a cluster you can administer:

- a production or shared Kubernetes cluster, or
- a local cluster such as
  [kind](https://kind.sigs.k8s.io/docs/user/quick-start/) or
  [minikube](https://minikube.sigs.k8s.io/docs/start/).

Follow the provider's setup guide, then verify access:

```bash
kubectl cluster-info
```

For local clusters, make sure the kruntimes images referenced by Helm values are
available inside the cluster. For example, use a registry reachable from the
cluster or load locally built images using your cluster tool.

## Platform Chart

Install the platform chart once per cluster:

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

The platform chart installs:

- CRDs,
- controller,
- scheduler,
- platform RBAC,
- metrics Services,
- optional ServiceMonitor.

## Built-In Runtime Chart

Install built-in Runtime CRs into namespaces where Runs should execute:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace \
  --set bash.image=<bash-runtime-image> \
  --set python.image=<python-runtime-image>
```

## Image Configuration

Use immutable image tags or digests for shared environments. Avoid mutable tags
outside local development.

## Multiple Releases

The Helm charts use release fullnames for generated resource names. Multi-release
and multi-namespace rendering is covered by chart tests.

## Uninstall

Remove Runtime releases first, then the platform:

```bash
helm uninstall kruntimes-runtimes --namespace default
helm uninstall kruntimes --namespace kruntimes-system
```

See [Operations Guide](operations.md) for upgrade, backup, restore, and
troubleshooting procedures.
