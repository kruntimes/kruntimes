# Installation

kruntimes is installed with Helm. The current model is cluster-wide platform
installation plus namespace-local Runtime definitions.

## Requirements

- Kubernetes cluster with CRD support.
- Helm 3.
- kubectl configured for the target cluster.
- Published kruntimes images for production installs, or locally loaded images
  for kind development.

See [Compatibility Matrix](compatibility.md) for the versions intentionally
tested by the project.

## Local Development Install

For kind-based development:

```bash
make e2e-setup
```

This target:

1. generates CRDs,
2. builds scheduler, controller, runtimed, Bash runtime, and Python runtime
   images,
3. creates or reuses a kind cluster,
4. loads images into kind,
5. applies CRDs,
6. installs the platform chart.

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

## Built-In Runtime Chart

Install built-in Runtime CRs into namespaces where Runs should execute:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace
```

## Image Configuration

Use immutable image tags or digests for shared environments. Avoid mutable
`latest` tags outside local development.

For local make targets:

```bash
IMG_SCHEDULER=ghcr.io/kruntimes/scheduler:v0.1.0 \
IMG_CONTROLLER=ghcr.io/kruntimes/controller:v0.1.0 \
IMG_RUNTIMED=ghcr.io/kruntimes/runtimed:v0.1.0 \
make deploy
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
