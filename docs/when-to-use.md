# When to Use kruntimes

kruntimes is a warm execution substrate for Kubernetes. It is useful when a
team wants low-latency, high-throughput execution while keeping Kubernetes as
the durable control plane.

It does not replace every execution platform around Kubernetes. Use it when the
problem is request-time Pod startup, burst dispatch, or custom warm Runtime
pools. Do not use it when the real requirement is a full serverless product, a
workflow engine, a cluster batch scheduler, or a hostile-code sandbox.

## Use kruntimes When

### You Need Fast Dispatch for Short Work

kruntimes fits short-lived Runs where creating a new Pod per execution is too
slow or too expensive. Common examples include automation scripts, CI
micro-steps, serverless-style functions, and internal code execution tools.

The main value is avoiding request-time Pod creation. Kubernetes keeps Runtime
Pods warm; kruntimes assigns individual Runs to those warm Pods.

### You Own the Runtime Environment

kruntimes works best when a platform team controls the Runtime image, runtime
protocol, security posture, and resource model. This is common for internal
developer platforms, CI infrastructure, AI agent infrastructure, and trusted
automation systems.

The model is not "bring arbitrary untrusted code and isolate it per Run" by
default. Built-in Runtimes are trusted-code examples.

### You Need Hierarchical Scheduling

kruntimes is useful when Kubernetes should still make coarse placement
decisions, but individual executions need faster application-level dispatch.

Kubernetes schedules Runtime Pods across nodes. kruntimes schedules Runs inside
those pools based on Runtime health and capacity. This is a good fit for
high-concurrency short tasks and batch workloads, including AI batch workloads,
where the application layer has more context than the default Kubernetes Pod
scheduler.

### You Need Custom Warm Pools

Use kruntimes when each workload family needs a different warm execution
environment: Bash, Python, model tooling, build tools, internal SDKs, or
domain-specific runtime servers.

Runtime CRDs make those pools Kubernetes-native while the Runtime Server
protocol leaves room for specialized execution logic.

## When Not to Use kruntimes

### You Need a Full Serverless Platform

If you need event sources, autoscaling-to-zero, traffic routing, revisions,
rollouts, request serving, and production FaaS lifecycle management, use a
serverless platform such as Knative.

kruntimes focuses on execution dispatch into warm Runtime pools. It is a lower
level building block.

### You Need a Mature General-Purpose Workflow Platform

If the main problem is DAG orchestration, retries across many steps,
artifact-driven workflow composition, templates, approvals, or UI-driven run
history, use a workflow system such as Argo Workflows or Tekton.

kruntimes can execute individual steps and has basic Workflow CRDs. Future
workflow work is expected to focus on faster CI/CD task execution on top of warm
Runtime pools. That is different from replacing mature workflow platforms that
provide broad ecosystem integrations, rich UI, approvals, and full workflow
lifecycle management.

### You Need a Cluster Batch Scheduler Replacement

If the main requirement is gang scheduling, queue fairness, quota management,
topology-aware placement, preemption, or multi-tenant batch policy, use a
batch scheduler such as Volcano or the native Kubernetes scheduling ecosystem.

kruntimes complements Kubernetes scheduling by reusing warm pools. It does not
replace cluster-level scheduling policy.

### You Need Strong Isolation for Hostile Code

Do not use the built-in Bash or Python Runtimes to run hostile code. They do
not provide per-Run process, network, filesystem, CPU, or memory isolation.

For hostile-code execution, pair kruntimes with a Runtime implementation that
uses an appropriate sandboxing layer, such as microVMs, containers with strict
policy, language sandboxes, or another isolation technology suitable for your
threat model.

### A Plain Deployment and Worker Queue Is Enough

If a small queue consumer Deployment already provides acceptable latency,
operability, and isolation, kruntimes may be unnecessary. The project adds
value when Kubernetes-native Run objects, status, scheduling, runtime pools,
artifacts, and CLI workflows matter.

## Decision Checklist

Use kruntimes if most of these are true:

- Pod startup latency is part of the user-visible or pipeline-critical path.
- Workloads are trusted or isolated by a custom Runtime you control.
- The team wants Kubernetes CRDs, RBAC, metrics, and Helm-based operations.
- Runtime Pods can be kept warm without wasting unacceptable resources.
- Application-level scheduling can make better dispatch decisions than
  creating one Pod per execution.

Be cautious if any of these are true:

- The workload is long-running and Pod startup is not meaningful.
- Strong untrusted-code isolation is required but no sandboxed Runtime exists.
- Mature workflow platform features or batch queue policy are the main product
  need.
- The team does not want to operate Runtime pools.
- A simpler worker queue already satisfies the latency and observability goals.

## Typical First Wedge

The strongest early use case is trusted internal code execution where cold
starts hurt developer experience or platform throughput. Examples include AI
agent tools, internal code sandboxes, CI micro-steps, and automation tasks.

These workloads benefit from warm environments, bounded status, artifact
references, structured logs, and Kubernetes-native ownership without requiring
kruntimes to be a complete serverless, workflow, batch, or sandboxing platform.
