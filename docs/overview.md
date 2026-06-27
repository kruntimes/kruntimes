# Project Overview

kruntimes is a Kubernetes-native execution engine that runs serverless
functions, CI pipelines, batch workloads including AI workloads, and AI agent
tasks/sandboxes on pre-warmed runtime pools. Instead of creating a new Pod for
every execution, kruntimes reuses hot Runtime Pods and performs fine-grained
scheduling in the application layer, reducing startup latency and operational
complexity without modifying Kubernetes internals.

## Motivation

Kubernetes is a strong substrate for services and long-running jobs, but vanilla
Kubernetes is not a complete low-latency serverless or batch execution engine by
itself. Serverless, CI, automation, and agent workloads often hit the same
request-time Pod startup path:

1. create a Pod,
2. wait for Kubernetes scheduling,
3. pull or unpack images,
4. create containers,
5. initialize networking,
6. wait for readiness,
7. then finally run user work.

That path is costly when the workload is short-lived, high-concurrency,
latency-sensitive, or part of a larger batch pipeline.

Infrastructure-level optimizations can reduce parts of this cost, but they
often require ownership of the cluster scheduler, node runtime, image
distribution, CNI, or sandboxing layer. kruntimes is designed for platform teams
that need faster execution semantics while staying above Kubernetes internals.

## Approach

kruntimes uses two scheduling layers:

| Layer | Responsibility | Frequency |
| --- | --- | --- |
| Kubernetes | Schedule and keep Runtime Pod pools alive. | Coarse-grained |
| kruntimes | Assign individual Runs to warm Runtime Pods. | Fine-grained |

The Kubernetes API remains the durable control plane through CRDs. Runtime Pods
hold local execution capacity and run a `runtimed` sidecar that coordinates with
the local Runtime Server.

This is hierarchical scheduling: Kubernetes performs coarse placement for warm
Runtime pools, while kruntimes performs fine-grained Run placement inside those
pools. The model supports low-latency serverless-style execution and
high-performance batch workloads without creating one Pod per execution.

## Core Value

- Avoid request-time Pod creation.
- Keep execution environments warm.
- Preserve Kubernetes-native operations through CRDs, Helm, RBAC, metrics, and
  status conditions.
- Let teams build low-latency execution above Kubernetes without replacing the
  Kubernetes scheduler.
- Support hierarchical scheduling for workloads that need Kubernetes-level pool
  management plus fast application-level dispatch.

## Use Cases

- AI agent tools and code execution.
- AI agent tasks and sandboxes.
- Trusted serverless workloads.
- CI/CD and automation tasks.
- High-concurrency short-lived scripts.
- High-performance batch workloads that benefit from hierarchical scheduling.
- Custom Runtime pools for specialized execution environments.

## Current Limits

- The API is `v1alpha1` and experimental.
- Built-in Bash and Python runtimes are trusted-code only.
- Built-in runtimes do not provide per-Run process, network, filesystem, CPU, or
  memory isolation.
- Large logs and artifacts are stored outside etcd; Run status keeps compact
  metadata and references.

See [Security and Threat Model](security.md) for the trust boundary.
