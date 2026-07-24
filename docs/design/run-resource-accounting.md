---
title: "Run Resource Accounting"
---

# Run Resource Accounting

Status: **Proposal; API review required before implementation**

Runtime capacity is not limited to concurrent Run count. `Runtime.spec.capacity.resources`
already declares named per-Runtime-Pod capacities and the Runtime controller
projects them to Pod annotations. The scheduler currently accounts only for
the built-in `runs` resource. This document defines the missing Run-side
request model needed to enforce every declared capacity consistently.

## API

Add an immutable `Run.spec.resources` field of type `corev1.ResourceList`.
It declares logical Runtime resources consumed for the Run's full active
lifetime, from `Scheduled` through terminal completion or function release.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
spec:
  runtime: python
  resources:
    runs: "1"
    example.com/gpu: "1"
  mode:
    task:
      args: ["python", "train.py"]
```

`runs` is the built-in logical resource. When omitted, it defaults to a
request of `1` so existing Runs retain their current capacity behavior. Other
resources are optional and must be explicitly requested. These are scheduler
resources, not container CPU or memory requests; users continue to configure
the Runtime Pod template's Kubernetes container resources independently.

Resource quantities must be non-negative integers. A request of zero is
ignored. The initial API does not define limits, overcommit, sharing ratios,
or runtime-specific resource classes.

## Capacity Contract

`Runtime.spec.capacity.resources` remains the declaration of per-Pod logical
capacity. The Runtime controller copies each declared value to the matching
Runtime Pod annotation:

```yaml
kruntimes.io/capacity.runs: "4"
kruntimes.io/capacity.example.com/gpu: "1"
```

For each candidate Pod, the scheduler reads the complete annotation set and
filters the Pod when any requested resource would exceed capacity:

```text
usage[pod][resource] + request[run][resource] <= capacity[pod][resource]
```

Active assigned Runs and scheduler-local assumed assignments both contribute
their full resource request to `usage`. Reserve/Assume and Bind therefore use
the same accounting model. The least-loaded strategy may continue to score on
`runs` initially; adding multi-resource scoring is a separate policy design.

If a Run requests a resource that a candidate Pod does not advertise, that Pod
is infeasible. If no ready Pod satisfies all requests, the Run remains
`Pending` with a bounded insufficient-capacity reason. It is reactivated when
Runtime Pod capacity changes or active/assumed usage is released. A malformed
request is an invalid Run configuration and fails validation before scheduling.

## Compatibility And Boundaries

- Existing Runs omit `resources`; the scheduler treats them as `{runs: 1}`.
- Existing Runtime Pods without a `runs` annotation retain the current default
  `runs` capacity. No implicit capacity exists for any other resource.
- Scheduler accounting is namespace-local because Run assignments are
  namespace-local. Runtime Pods are also selected in the Run namespace.
- runtimed enforces execution and does not make placement decisions. It need
  not understand logical resource names in this phase.
- This does not change Kubernetes scheduling of Runtime Pods, the Runtime Pod
  template's container requests/limits, or function invocation concurrency.

## Implementation Sequence

1. Review this API and its Pending/validation semantics.
2. Add `Run.spec.resources`, immutability validation, generated CRDs, and
   user-facing API documentation.
3. Add Runtime Pod helpers that parse complete capacity annotations.
4. Accumulate complete active and assumed Run resource usage in the scheduler,
   then filter candidates using the capacity contract above.
5. Add unit, integration, and E2E coverage for defaults, multi-resource
   placement, release/reactivation, and missing capacity.
