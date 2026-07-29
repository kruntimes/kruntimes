---
title: "Run Resource Accounting"
---

# Run Resource Accounting

Status: **Accepted; Scheduler implementation in progress**

Runtime capacity is not limited to concurrent Run count. `Runtime.spec.capacity.resources`
already declares named per-Runtime-Pod capacities and the Runtime controller
projects them to Pod annotations. The scheduler currently accounts only for
the built-in `runs` resource. This document defines the missing Run-side
request model needed to enforce every declared capacity consistently.

## API

Add an immutable `Run.spec.resources.requests` field of type
`corev1.ResourceList`. It declares logical Runtime resources consumed for the
Run's full active lifetime, from `Scheduled` through terminal completion or
function release.

```yaml
apiVersion: kruntimes.io/v1alpha1
kind: Run
spec:
  runtime: python
  resources:
    requests:
      runs: "1"
      example.com/gpu: "1"
  mode:
    task:
      args: ["python", "train.py"]
```

`runs` is the built-in logical resource. When `resources` or
`resources.requests.runs` is omitted, it defaults to a request of `1` so
existing Runs retain their current capacity behavior. Other resources are
optional and must be explicitly requested. These are logical Runtime resources,
not container CPU or memory requests; users continue to configure the Runtime
Pod template's Kubernetes container resources independently.

Resource quantities must be non-negative integers. A request of zero is
ignored. The initial API intentionally does not define `limits`: logical
capacity is reserved at the requested amount by both scheduler and runtimed.
Adding limits would require a separately reviewed model for overcommit,
runtime-side enforcement, sharing ratios, and runtime-specific resource
classes. A future `limits` field can be added to the same resource object only
with those semantics defined.

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
their full resource request to scheduler `usage`. Reserve/Assume and Bind
therefore use the same accounting model. The least-loaded strategy scores the
projected allocation across every advertised resource: it first minimizes the
highest utilization ratio, then the sum of utilization ratios, with Pod name as
a stable final tie break.

If a Run requests a resource that a candidate Pod does not advertise, that Pod
is infeasible. If no ready Pod satisfies all requests, the Run remains
`Pending` with a bounded insufficient-capacity reason. It is reactivated when
Runtime Pod capacity changes or active/assumed usage is released. A malformed
request is an invalid Run configuration and fails Scheduler prefilter validation
before placement.

## Runtime Admission

Scheduler accounting is not an execution authorization. runtimed must maintain
a local, watch-backed resource usage cache for the Runtime Pod it owns. Before
claiming a `Scheduled` Run, it atomically checks the Run's complete request
against that cache and reserves the request only when every resource fits the
Pod's annotated capacity.

When a request does not fit, runtimed leaves the Run `Scheduled`; it does not
start the Runtime Server call, mark the Run failed, or release the assignment.
It rechecks the queued Run when a locally active Run reaches a terminal state
or capacity changes. This makes runtimed the execution-time guard against a
stale scheduler cache, concurrent transitions, or an assignment created by an
older scheduler version. A Run whose request is larger than every eligible Pod
should normally remain `Pending` because the scheduler never assigns it.

## Compatibility And Boundaries

- Existing Runs omit `resources`; the scheduler treats them as `{runs: 1}`.
- Existing Runtime Pods without a `runs` annotation retain the current default
  `runs` capacity. No implicit capacity exists for any other resource.
- Scheduler accounting is namespace-local because Run assignments are
  namespace-local. Runtime Pods are also selected in the Run namespace.
- runtimed enforces local execution admission from a Pod-local resource cache;
  it does not choose among Pods or make placement decisions.
- This does not change Kubernetes scheduling of Runtime Pods, the Runtime Pod
  template's container requests/limits, or function invocation concurrency.

## Implementation Sequence

1. Review this API and its Pending/validation semantics.
2. Add `Run.spec.resources`, immutability validation, generated CRDs, and
   user-facing API documentation.
3. Add Runtime Pod helpers that parse complete capacity annotations.
4. Accumulate complete active and assumed Run resource usage in the scheduler,
   then filter candidates using the capacity contract above.
5. Add runtimed's atomic local admission/reservation cache. A capacity-blocked
   assigned Run remains `Scheduled` until locally reserved resources are
   released.
6. Add unit, integration, and E2E coverage for defaults, multi-resource
   placement, release/reactivation, missing capacity, and runtimed admission
   after stale or competing assignments.
