# Quick Start

This guide installs a released kruntimes build on an existing Kubernetes
cluster and executes one Bash Run.

## Prerequisites

- Kubernetes cluster with CRD support
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

Set the release version used by the Helm charts and images:

```bash
KRUNTIMES_VERSION=0.0.3
```

Install the control plane once per cluster:

```bash
helm upgrade --install kruntimes oci://ghcr.io/kruntimes/charts/kruntimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace kruntimes-system \
  --create-namespace
```

Install the built-in Runtime definitions into the namespace where Runs should
execute:

```bash
helm upgrade --install kruntimes-runtimes oci://ghcr.io/kruntimes/charts/kruntimes-runtimes \
  --version "${KRUNTIMES_VERSION}" \
  --namespace default \
  --create-namespace
```

The charts default to the published `ghcr.io/kruntimes/*` image repositories
and append the chart `appVersion` when the image value does not already include
a tag or digest.

Check the control plane and Runtime Pods:

```bash
kubectl get deploy -n kruntimes-system
kubectl get runtime,pods -n default
```

Wait until the control plane and Bash Runtime Pods are ready:

```bash
kubectl wait deployment -n kruntimes-system -l app=kruntimes-controller --for=condition=Available --timeout=120s
kubectl wait deployment -n kruntimes-system -l app=kruntimes-scheduler --for=condition=Available --timeout=120s
kubectl wait pod -n default -l runtime=bash --for=condition=Ready --timeout=120s
```

## Run a Command

```bash
kubectl apply -n default -f - <<'EOF'
apiVersion: kruntimes.io/v1alpha1
kind: Run
metadata:
  name: hello
spec:
  runtime: bash
  source:
    inline: |
      echo "hello from kruntimes"
  entrypoint: script
EOF
```

Watch status:

```bash
kubectl get run hello -n default -w
```

Inspect the final object:

```bash
kubectl get run hello -n default -o yaml
```

Inspect logs. If you have the `krt` CLI installed, use:

```bash
krt logs hello -n default
```

The `krt` install command is covered in [Installation](installation.md#krt-cli).
Without `krt`, read the assigned Runtime Pod's structured runtimed logs:

```bash
HELLO_POD="$(kubectl get run hello -n default -o jsonpath='{.status.assignedPod}')"
HELLO_UID="$(kubectl get run hello -n default -o jsonpath='{.metadata.uid}')"
kubectl logs "$HELLO_POD" -n default -c runtimed | grep "\"run_uid\":\"${HELLO_UID}\""
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
- [End-to-End Demos](demos.md)
- [Troubleshooting](troubleshooting.md)
