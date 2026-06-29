# Quick Start

This guide installs kruntimes on an existing Kubernetes cluster and executes one
Bash Run.

## Prerequisites

- Kubernetes cluster with CRD support.
- kubectl
- Helm 3

kruntimes runs on Kubernetes. The cluster can be a production cluster or a local
development cluster such as
[kind](https://kind.sigs.k8s.io/docs/user/quick-start/) or
[minikube](https://minikube.sigs.k8s.io/docs/start/). Follow the cluster
provider's setup guide first, then confirm access:

```bash
kubectl cluster-info
```

## Install kruntimes

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

Install the built-in Runtime definitions into the namespace where Runs should
execute:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace \
  --set bash.image=<bash-runtime-image> \
  --set python.image=<python-runtime-image>
```

Use image references that your cluster can pull. For local kind or minikube
clusters, either load locally built images into the cluster or point the chart at
images in a registry the cluster can access.

Check the control plane and Runtime Pods:

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,pods -n default
```

Wait until the Bash Runtime Pods are ready:

```bash
kubectl get pods -n default -l runtime=bash -w
```

## Run a Command

```bash
kubectl apply -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  args:
    - echo
    - hello from kruntimes
EOF
```

Watch status:

```bash
kubectl get run hello -w
```

Inspect the final object:

```bash
kubectl get run hello -o yaml
```

## Clean Up

```bash
helm uninstall kruntimes-runtimes --namespace default --ignore-not-found
helm uninstall kruntimes --namespace kruntimes-system --ignore-not-found
```

Helm uninstall does not remove kruntimes CRDs. See
[Installation](installation.md) and [Operations Guide](operations.md) for
cluster uninstall details.

## Next Steps

- [Installation](installation.md)
- [Usage Guide](usage.md)
- [Troubleshooting](troubleshooting.md)
