# Dashboard

This document describes a target v0.x design. It is not implemented yet.

kruntimes should provide a small read-only dashboard for developers and
operators who need to understand what is running, what is stuck, and where to
find logs and artifacts without switching between multiple `kubectl` and `krt`
commands.

The dashboard is not intended to become the workflow engine or the primary
control plane. It should visualize Kubernetes-native state that already exists
in CRDs, pods, conditions, logs, and artifact references.

## Goals

- Browse Runs by namespace.
- Inspect Run phase, conditions, runtime, assigned Runtime Pod, attempts,
  timestamps, bounded outputs, and artifact references.
- Stream or retrieve Run logs through the same security boundary used by
  `krt logs`.
- Preserve Kubernetes RBAC and namespace boundaries.
- Provide an operator-friendly view for Pending, Scheduled, Running, Succeeded,
  Failed, Cancelled, and TimedOut Runs.
- Leave room for future WorkflowRun, Workflow, Action, and PersistentWorkspace
  views after those APIs stabilize.

## Non-Goals

- No create, cancel, delete, retry, or edit operations in the first version.
- No workflow editor or visual DAG builder in the first version.
- No browser access directly to Runtime Pods, Runtime Servers, or runtimed
  endpoints.
- No custom identity system that bypasses Kubernetes authentication and
  authorization.
- No replacement for Prometheus, log collection, or long-term audit storage.
- No stable public dashboard HTTP API in v0.x.

## Users

Developers use the dashboard to answer:

- did my Run start;
- which Runtime handled it;
- why is it Pending or Failed;
- what did the logs and bounded outputs say;
- where are artifacts stored.

Operators use the dashboard to answer:

- which namespaces have stuck or failing Runs;
- whether capacity, readiness, RBAC, or image/runtime problems are visible in
  Run conditions;
- which Runtime Pods are receiving work;
- whether users are asking for logs or artifact access that require additional
  RBAC.

## Architecture

The dashboard should have two components:

| Component | Role |
| --- | --- |
| Dashboard backend | Talks to the Kubernetes API, enforces the selected auth/RBAC model, reads kruntimes CRDs, and proxies log/artifact access when allowed. |
| Dashboard frontend | Read-only web UI that renders namespace, Run list, Run detail, logs, and artifact metadata. |

The first version should read the following sources:

- `Run` objects through the Kubernetes API;
- Runtime Pod metadata referenced by `Run.status.assignedPod`;
- Kubernetes Events related to Runs and Runtime Pods when available;
- runtimed log/status endpoints through a backend-controlled path;
- `Run.status.outputs` and `Run.status.artifactRefs`.

Future versions can add:

- `WorkflowRun`, `Workflow`, and `Action` list/detail pages;
- PersistentWorkspace detail pages;
- runtime pool capacity and health views;
- metrics panels backed by Prometheus or another metrics backend.

## Log Access

The dashboard backend must not expose Runtime Pods directly to browsers.

For v0.x, the expected path is:

1. The user opens logs for a Run.
2. The backend reads the Run and its assigned Runtime Pod.
3. The backend verifies the request using the configured Kubernetes auth/RBAC
   model.
4. The backend reaches runtimed using the same conceptual boundary as `krt logs`
   and streams or returns the requested log tail.

The exact transport can evolve. It may use Kubernetes port-forwarding, an
internal service, or a dedicated log proxy, but the boundary should stay the
same: users need permission to read the Run and to access runtime logs.

Structured runtimed logs should remain keyed by Run UID so the dashboard can
show the correct logs even when Runtime Pods handle multiple Runs.

## Security Model

The dashboard must be read-only by default.

The preferred production model is Kubernetes-native:

- authentication comes from Kubernetes or the cluster's identity integration;
- authorization uses Kubernetes RBAC;
- namespace visibility follows the user's permissions;
- log and artifact access require explicit permission, not just dashboard
  access;
- secrets, service account tokens, environment variables, and raw pod specs are
  hidden unless a future privileged operator view explicitly exposes them.

For local development, a kubeconfig-backed mode is acceptable, but it should be
documented as a development mode rather than the production default.

## Internal API Shape

The dashboard frontend can use an internal, versioned-for-the-binary HTTP API.
It should not be documented as a stable public API in v0.x.

Initial endpoints can be:

```text
GET /api/namespaces
GET /api/namespaces/{namespace}/runs
GET /api/namespaces/{namespace}/runs/{name}
GET /api/namespaces/{namespace}/runs/{name}/logs?tail=&follow=
```

The Run list endpoint should support server-side pagination and filter fields
where practical:

- phase;
- runtime;
- assigned pod;
- label selector;
- created-after or age window.

## User Interface

The first version should keep the UI narrow and operational:

- namespace selector;
- Run table with phase, runtime, assigned pod, age, attempts, and last
  transition reason;
- filters for phase and runtime;
- Run detail page or drawer;
- conditions timeline;
- bounded outputs and artifact references;
- logs panel with tail and follow controls;
- links to related Runtime Pod metadata when the user has permission.

It should not include mutation buttons until the read-only authorization model
is proven.

## Implementation Sequence

1. Add this design document and keep the roadmap explicit.
2. Add a dashboard backend package with read-only Kubernetes client wiring.
3. Define the production and local-development auth modes.
4. Implement Run list/detail APIs with unit tests.
5. Implement log tail/follow through a backend-controlled path.
6. Add the frontend Run list/detail/log views.
7. Add an optional Helm chart value or separate dashboard chart.
8. Add E2E smoke coverage that installs the dashboard, creates a Run, lists it,
   opens detail, and fetches logs.
9. Add WorkflowRun/Workflow/Action/PersistentWorkspace views after the
   corresponding APIs stabilize.

## Open Questions

- Should the dashboard ship in the main kruntimes chart, a separate chart, or
  both?
- Which production auth mode should be first: service account with
  impersonation, Kubernetes API proxy integration, or external auth in front of
  the dashboard?
- Should log access continue to use port-forward semantics or move to a
  dedicated cluster-internal log proxy service?
- How should artifact downloads be authorized and proxied when artifact stores
  are outside the cluster?
- What scale target should the first list/watch implementation support?
