<p align="center">
  <img src="docs/logo.png" alt="kruntimes logo" />
</p>

# kruntimes

kruntimes is a Kubernetes-native warm execution pool for short-lived workloads.
It keeps Runtime Pods ready, schedules Runs inside those hot pods, and avoids
creating a new Kubernetes Pod for every execution.

The project is currently `v0.x experimental` with `v1alpha1` APIs. Built-in
Bash and Python runtimes are intended for trusted workloads in trusted
namespaces; they are not tenant-grade sandboxes.

## Why kruntimes?

Vanilla Kubernetes is a strong substrate for services, but it is expensive for
fine-grained function-style execution. Creating one Pod per invocation adds
scheduling, image, container, CNI, readiness, and controller-loop latency to the
critical path.

kruntimes splits scheduling into two layers:

- Kubernetes keeps coarse-grained Runtime Pod pools alive.
- kruntimes assigns individual Runs to healthy warm pods with available
  capacity.

This is useful for AI agents, CI/CD tasks, trusted serverless functions,
automation jobs, and other short-lived high-concurrency workloads where
sub-second dispatch matters more than per-invocation Pod isolation.

## Quick Start

Prerequisites:

- Kubernetes cluster or kind
- Go version from `go.mod`
- Docker or another compatible container tool
- Helm 3
- kubectl

Build and deploy to a local kind cluster:

```bash
make e2e-setup
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

## Common Commands

```bash
make test
make test-integration
make test-race
make test-helm
make e2e
make benchmark
```

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
