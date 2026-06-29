# FAQ

## Is kruntimes a Kubernetes scheduler replacement?

No. Kubernetes still schedules Runtime Pods. kruntimes schedules individual Runs
inside those warm Runtime Pods.

## Does kruntimes create one Pod per Run?

No. A Run is a CRD object. The scheduler assigns it to an existing Runtime Pod
with available capacity.

## Are built-in runtimes safe for untrusted code?

No. Built-in Bash and Python runtimes are trusted-code only. They do not provide
per-Run process, filesystem, network, CPU, memory, or ServiceAccount isolation.

Use namespace separation or custom runtimes with stronger isolation for
untrusted workloads.

## Where are logs stored?

Full stdout and stderr are emitted as structured runtimed logs keyed by Run UID.
They are not copied wholesale into `Run.status.message`.

## Where are artifacts stored?

Artifacts are written to `$KRUNTIME_ARTIFACTS_DIR` and persisted outside etcd
through the configured ArtifactStore. `Run.status.artifactRefs` stores compact
metadata.

## Why is a Run Pending?

Usually because no matching healthy Runtime Pod has available capacity. See
[Troubleshooting](troubleshooting.md).

## What happens on timeout?

Timeouts end in the `Timeout` terminal phase. They are not reported as generic
`Failed`.

## What execution guarantee does kruntimes provide?

Execution is at-least-once. Runtime Servers must make duplicate `Execute`
delivery deterministic and safe.

## Can a Runtime use a custom ServiceAccount?

Yes. Set `Runtime.spec.template.spec.serviceAccountName`. The Runtime
controller creates namespace-scoped RBAC for runtimed permissions.

## Is the API stable?

Not yet. The project is `v0.x experimental` and CRDs are `v1alpha1`.

## How do I benchmark the scheduler?

See [Performance Benchmarks](benchmarks.md). Benchmark execution is a
contributor workflow and assumes a local development environment.
