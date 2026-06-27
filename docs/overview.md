# Project Overview

kruntimes is a Kubernetes-native execution system for short-lived workloads. It
keeps Runtime Pods warm and schedules Runs into those pods without creating a
new Kubernetes Pod per invocation.

## Problem

Serverless and function-style systems on Kubernetes often hit the same latency
path:

1. create a Pod,
2. wait for Kubernetes scheduling,
3. pull or unpack images,
4. create containers,
5. initialize networking,
6. wait for readiness,
7. then finally run user work.

That path is reasonable for services and long-running jobs. It is costly for
high-volume, short-lived work.

## Approach

kruntimes uses two scheduling layers:

| Layer | Responsibility | Frequency |
| --- | --- | --- |
| Kubernetes | Schedule and keep Runtime Pod pools alive. | Coarse-grained |
| kruntimes | Assign individual Runs to warm Runtime Pods. | Fine-grained |

The Kubernetes API remains the durable control plane through CRDs. Runtime Pods
hold local execution capacity and run a `runtimed` sidecar that coordinates with
the local Runtime Server.

## Core Value

- Avoid request-time Pod creation.
- Keep execution environments warm.
- Preserve Kubernetes-native operations through CRDs, Helm, RBAC, metrics, and
  status conditions.
- Let teams build low-latency execution above Kubernetes without replacing the
  Kubernetes scheduler.

## Use Cases

- AI agent tools and code execution.
- Trusted serverless workloads.
- CI/CD and automation tasks.
- High-concurrency short-lived scripts.
- Custom Runtime pools for specialized execution environments.

## Current Limits

- The API is `v1alpha1` and experimental.
- Built-in Bash and Python runtimes are trusted-code only.
- Built-in runtimes do not provide per-Run process, network, filesystem, CPU, or
  memory isolation.
- Large logs and artifacts are stored outside etcd; Run status keeps compact
  metadata and references.

See [Security and Threat Model](security.md) for the trust boundary.
