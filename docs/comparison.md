# Comparison Guide

This guide explains how kruntimes relates to adjacent Kubernetes execution
systems. The goal is to help users choose the right tool, not to position
kruntimes as a universal replacement.

kruntimes is an early-stage Kubernetes-native warm execution project. Its
current implementation keeps Runtime Pods warm and schedules individual Runs
inside those pools. The longer-term direction overlaps with parts of
serverless, CI/CD, and batch execution platforms, but kruntimes is not yet a
mature replacement for those systems.

## Summary

| Option | Primary Job | Best Fit | kruntimes Relationship |
| --- | --- | --- | --- |
| kruntimes | Dispatch Runs into warm Runtime Pods. | Early adopters exploring low-latency trusted execution, CI micro-steps, automation, AI agent tools, and batch work that benefits from warm pools. | Experimental. Some future goals overlap with serverless, CI/CD, and batch platforms, but current maturity is much lower. |
| Knative | Build and run serverless services on Kubernetes. | Request-serving applications, event sources, revisions, traffic splitting, and scale-to-zero. | A mature serverless platform. kruntimes may eventually cover some adjacent execution needs, but does not currently provide Knative's platform features. |
| Argo Workflows | Orchestrate Kubernetes-native workflow DAGs. | Multi-step workflows, templates, artifact-driven pipelines, retries, and workflow UI/history. | A mature workflow engine. kruntimes has early Workflow CRDs but is not a replacement for Argo's workflow lifecycle or ecosystem. |
| Tekton | Define CI/CD pipelines as Kubernetes resources. | Software delivery pipelines, tasks, workspaces, catalogs, and pipeline integrations. | A mature CI/CD framework. kruntimes may grow toward faster CI/CD task execution, but does not currently replace Tekton. |
| Volcano | Schedule batch workloads at cluster level. | Queue fairness, gang scheduling, quota, preemption, and compute-intensive batch policy. | A cluster-level batch scheduler. kruntimes currently provides warm-pool dispatch, not cluster scheduling policy. |
| Worker queue on a Deployment | Consume queued work from long-running Pods. | Simple internal jobs with application-owned queue semantics. | Often the simpler and more mature choice unless Kubernetes-native Run objects and Runtime pools matter. |

## kruntimes vs Knative

Knative is a mature serverless platform for cloud-native applications. It
focuses on serving, eventing, revisions, traffic routing, autoscaling, and
scale-to-zero.

Some long-term kruntimes goals sit near the serverless space: fast function-like
execution, warm runtime pools, and Kubernetes-native execution APIs. Today,
however, kruntimes does not provide Knative's production platform surface. Use
kruntimes only if you are comfortable with an experimental execution substrate
and your immediate problem is warm trusted execution rather than a complete
serverless application platform.

Choose Knative when you need:

- HTTP serving and request routing.
- Revisions, traffic splitting, and rollout behavior.
- Event sources and event delivery.
- Scale-to-zero semantics for services.

Consider kruntimes when you are experimenting with:

- Fast execution of trusted commands, functions, or code snippets.
- Runtime pools that stay warm for repeated short work.
- Kubernetes-native Run status, artifacts, logs, and scheduling.
- Custom Runtime Servers for specialized execution environments.

## kruntimes vs Argo Workflows

Argo Workflows is a mature workflow engine. It models DAGs, templates, steps,
parameters, artifacts, retries, and workflow-level history.

kruntimes currently sits lower in the stack. It executes Runs and keeps the
execution path warm. Its Workflow CRDs are early and should not be treated as a
replacement for Argo's workflow lifecycle, UI, ecosystem, or operational model.

Choose Argo Workflows when you need:

- Rich DAG orchestration and workflow templates.
- Workflow UI, history, resubmission, and operational tooling.
- Artifact-driven step composition.
- Broad ecosystem integrations around Kubernetes workflows.

Consider kruntimes when you are experimenting with:

- Lower-latency execution for individual trusted steps.
- Warm Runtime pools for repeated short commands or scripts.
- Fine-grained scheduling inside pre-warmed Kubernetes Pods.
- Future workflow behavior focused on faster CI/CD-style task execution rather
  than full workflow platform parity.

## kruntimes vs Tekton

Tekton is a mature Kubernetes-native CI/CD framework. It provides Pipeline, Task,
Workspace, catalog, trigger, and supply-chain-oriented integrations for software
delivery systems.

kruntimes may grow toward faster CI/CD task execution, especially for short
trusted steps that suffer from Pod startup latency. Today it is not a CI/CD
framework and does not provide the pipeline authoring, catalog, triggers,
workspace model, or delivery ecosystem that Tekton provides.

Choose Tekton when you need:

- CI/CD pipeline semantics and reusable Tasks.
- Workspaces, pipeline resources, triggers, and delivery integrations.
- A standard model for software delivery automation.
- Ecosystem compatibility with existing Tekton tooling.

Consider kruntimes when you are experimenting with:

- A fast execution substrate for small CI steps or automation commands.
- A custom Runtime image that is kept warm for repeated work.
- Status and artifact references on Run objects rather than full pipeline
  lifecycle management.
- Early design space for warm CI/CD execution, not a mature Tekton replacement.

## kruntimes vs Volcano

Volcano is a batch scheduling system for Kubernetes. It focuses on cluster-level
queueing and scheduling policy for compute-intensive workloads.

kruntimes does not replace the Kubernetes scheduler or Volcano. It uses
Kubernetes to place Runtime Pods, then dispatches Runs within those warm pools.
This can be useful for a different layer of the problem, but it is not a
cluster-level batch scheduling policy system.

Choose Volcano when you need:

- Gang scheduling or co-scheduling.
- Queue fairness, quotas, priority, and preemption.
- Cluster-level policy for compute-intensive batch workloads.
- Scheduling behavior that must coordinate multiple Pods.

Consider kruntimes when you are experimenting with:

- Fast repeated execution inside already-placed Runtime Pods.
- Application-level dispatch based on Runtime health and capacity.
- Hierarchical scheduling: Kubernetes handles pool placement, kruntimes handles
  Run placement inside the pool.
- Batch workloads where per-execution Pod startup is the bottleneck.

## kruntimes vs a Worker Queue on a Deployment

A Deployment plus a queue is often the simplest way to run background work. If
that model gives acceptable latency, status, security, and operational behavior,
it may be the right choice.

kruntimes adds Kubernetes-native execution objects, scheduling, artifacts,
bounded status, structured logs, and Runtime pool management. Those features may
be useful when the execution API should be visible and operable through
Kubernetes rather than hidden inside one application queue. They also add
operational surface area, and the project is still early.

Choose a worker queue when you need:

- The simplest possible architecture.
- Application-owned queue semantics.
- Long-running workers that do not need Kubernetes-native Run objects.
- Minimal CRDs and platform surface area.

Consider kruntimes when you are experimenting with:

- Run CRDs with status, terminal conditions, retries, timeouts, and cancellation.
- Per-Runtime capacity and health-aware scheduling.
- Artifact references and bounded outputs in Run status.
- A reusable execution substrate for multiple teams or tools, accepting that the
  project is still maturing.

## Decision Heuristic

Start with the primary problem:

- If the problem is serving traffic, look at Knative.
- If the problem is workflow orchestration, look at Argo Workflows.
- If the problem is CI/CD pipeline authoring, look at Tekton.
- If the problem is cluster-level batch policy, look at Volcano.
- If the problem is simple background work, consider a worker queue.
- If the problem is low-latency trusted execution on Kubernetes with warm pools
  and you can tolerate an early-stage project, evaluate kruntimes.

Do not assume existing systems such as Argo Workflows or Tekton will adapt to
kruntimes as an execution backend. A realistic first adoption path is a custom
internal platform, CLI, or automation service that directly creates Runs.
