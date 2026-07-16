<p align="center">
  <img src="docs/logo.svg" alt="kruntimes logo" />
</p>

# kruntimes

kruntimes is a Kubernetes-native execution engine that runs serverless
functions, CI pipelines, batch workloads including AI workloads, and AI agent
tasks/sandboxes on pre-warmed runtime pools. Instead of creating a new Pod for
every execution, kruntimes reuses hot Runtime Pods and performs fine-grained
scheduling in the application layer, reducing startup latency and operational
complexity without modifying Kubernetes internals.

The project is currently `v0.x experimental` with `v1alpha1` APIs. Built-in
Bash and Python runtimes are intended for trusted workloads in trusted
namespaces; they are not tenant-grade sandboxes.

## Motivation

Vanilla Kubernetes is a strong substrate for services and long-running jobs, but
it is not a complete low-latency execution engine by itself. A request-time Pod
startup path includes Kubernetes scheduling, image distribution, container
creation, CNI setup, readiness checks, and controller reconciliation before user
code can run.

That overhead is acceptable for many services. It becomes expensive for
serverless functions, CI steps, AI agent tools, sandboxes, and high-performance
batch workloads where many short executions need to start quickly.

Cold-start optimizations such as image caching, lazy image loading, node
pre-warming, Firecracker, or custom runtimes can help, but they often require
infrastructure-level ownership and the budget to operate those optimizations
over time. Many platform teams can deploy applications on Kubernetes, but
cannot replace or continuously maintain custom behavior in the cluster
scheduler, CNI, CRI, snapshotter, or node image policy.

kruntimes keeps Kubernetes as the coarse-grained resource substrate and moves
fine-grained execution scheduling into the application layer:

- Kubernetes keeps coarse-grained Runtime Pod pools alive.
- kruntimes assigns individual Runs to healthy warm pods with available
  capacity.

This hierarchical scheduling model reduces startup latency and operational
complexity without modifying Kubernetes internals.

Typical use cases include trusted serverless functions, CI/CD pipelines,
automation jobs, AI agent tasks and sandboxes, short-lived high-concurrency
workloads, and high-performance batch workloads that benefit from a
Kubernetes-level pool scheduler plus a faster application-level Run scheduler.

## Quick Start

Prerequisites:

- Kubernetes cluster
- Helm 3
- kubectl

Install the platform chart:

```bash
helm upgrade --install kruntimes ./charts/kruntimes \
  --namespace kruntimes-system \
  --create-namespace
```

Install built-in Runtime definitions into a workload namespace:

```bash
helm upgrade --install kruntimes-runtimes ./charts/kruntimes-runtimes \
  --namespace default \
  --create-namespace
```

Create a Run:

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

Watch it finish:

```bash
kubectl get run hello -w
```

See the full guide in [Quick Start](docs/quickstart.md).

## Documentation

| Audience | Start here |
| --- | --- |
| New users | [Overview](docs/overview.md), [Quick Start](docs/quickstart.md) |
| Operators | [Installation](docs/installation.md), [Operations](docs/operations.md), [Configuration](docs/configuration.md) |
| Runtime authors | [Custom Runtime Guide](docs/custom-runtime.md), [API Reference](docs/api.md) |
| Contributors | [Development Guide](docs/development.md), [Testing Guide](docs/testing.md), [Contributing](CONTRIBUTING.md) |
| Security reviewers | [Security and Threat Model](docs/security.md), [Security Policy](SECURITY.md) |
| Release managers | [Release Process](docs/release.md), [Compatibility Matrix](docs/compatibility.md), [Changelog](CHANGELOG.md) |

Core documentation:

- [Overview](docs/overview.md)
- [Quick Start](docs/quickstart.md)
- [Installation](docs/installation.md)
- [Usage Guide](docs/usage.md)
- [Configuration](docs/configuration.md)
- [Architecture](docs/architecture.md)
- [API Reference](docs/api.md)
- [Troubleshooting](docs/troubleshooting.md)
- [FAQ](docs/faq.md)
- [Roadmap](docs/roadmap.md)
- [Community and Governance](docs/community.md)

## Project Status

kruntimes is actively developed as an experimental `v0.x` project. APIs are
`v1alpha1` and may change before a stable release. See [Roadmap](docs/roadmap.md)
and [Open Source Readiness](docs/open-source-readiness.md) for current status.

## Community

- Report bugs and request features with GitHub Issues.
- Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a PR.
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md).
- Report vulnerabilities through the private channel described in
  [SECURITY.md](SECURITY.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
