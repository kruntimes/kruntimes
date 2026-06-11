# Contributing to kruntimes

Thank you for contributing to kruntimes. The project is currently experimental,
and its `v1alpha1` APIs may change while the execution and security contracts
are refined.

## Before You Start

- Search existing issues and pull requests before opening a new one.
- Use an issue to discuss large features, API changes, or behavior changes.
- Report vulnerabilities through the process in [SECURITY.md](SECURITY.md),
  not through a public issue.
- Follow the [Code of Conduct](CODE_OF_CONDUCT.md).
- Review [GOVERNANCE.md](GOVERNANCE.md) for project roles and decision-making.

## Development Setup

Required tools:

- Go version declared in `go.mod`
- Docker
- Helm 3
- kind and kubectl for E2E tests
- uv for Python Runtime development

Common commands:

```bash
make build
make test
make test-integration
make lint
make e2e
```

Run Python Runtime tests with:

```bash
cd runtimes/python
uv sync --frozen
uv run --frozen python -m unittest server_test -v
```

After changing API or protocol definitions, regenerate checked-in files:

```bash
make generate manifests proto proto-python
```

## Pull Requests

- Keep changes focused and include tests for behavior changes.
- Update user-facing documentation when APIs or behavior change.
- Do not include unrelated formatting or generated-file churn.
- Use [Conventional Commits](https://www.conventionalcommits.org/) for commit
  messages.
- Ensure generated files are current and the working tree is clean after tests.

Pull requests should explain:

- the problem being solved;
- the chosen behavior and important tradeoffs;
- how the change was tested;
- compatibility, security, or operational impact.

## API and Compatibility

The public API is currently `v1alpha1`. Avoid changing CRD fields, gRPC
contracts, execution semantics, or terminal-state behavior without tests and
an explicit compatibility assessment.

## Certificate of Origin

By submitting a contribution, you certify that you have the right to submit it
under the project's [Apache License 2.0](LICENSE).
