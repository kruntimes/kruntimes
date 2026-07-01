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

The released charts default to images published under `ghcr.io/kruntimes/`. For
local clusters using locally built images, override the image values with tags
that are available inside the cluster.

## Platform Chart

Install the platform chart once per cluster:

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace
```

The platform chart installs:

- CRDs,
- controller,
- scheduler,
- platform RBAC,
- metrics Services,
- optional ServiceMonitor.

## krt CLI

The `krt` CLI is optional for basic Kubernetes operation, but it is the easiest
way to inspect Run logs, download artifacts, cancel Runs, and follow Run
status. The end-to-end demos use `krt logs` alongside equivalent `kubectl`
commands.

Install a released CLI archive for Linux or macOS:

```bash
KRUNTIMES_VERSION=0.0.3
OS="$(uname | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "${ARCH}" in
  x86_64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) echo "unsupported architecture: ${ARCH}" >&2; exit 1 ;;
esac

curl -L -o /tmp/krt.tar.gz \
  "https://github.com/kruntimes/kruntimes/releases/download/v${KRUNTIMES_VERSION}/krt_v${KRUNTIMES_VERSION}_${OS}_${ARCH}.tar.gz"
tar -xzf /tmp/krt.tar.gz -C /tmp
sudo install /tmp/krt /usr/local/bin/krt
krt --help
```

For Windows, download `krt_v${KRUNTIMES_VERSION}_windows_amd64.tar.gz` from the
GitHub release page and place `krt.exe` on `PATH`.

Checksum and provenance verification are covered in
[Release Process](release.md#krt-cli).

## Built-In Runtime Chart

Install built-in Runtime CRs into namespaces where Runs should execute:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace
```

## Image Configuration

Use immutable image tags or digests for shared environments. Avoid mutable tags
outside local development.

Override chart image values only when you need custom images:

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace \
  --set scheduler.image=<scheduler-image> \
  --set controller.image=<controller-image> \
  --set runtimed.image=<runtimed-image>
```

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
