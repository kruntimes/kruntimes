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

The proposed v0.x production model is Kubernetes bearer-token login:

- the browser sends a Kubernetes bearer token only over the dashboard's HTTPS
  origin; the backend does not create a dashboard-specific identity or session;
- the backend creates a request-scoped Kubernetes client with that bearer token,
  the in-cluster API server address, and the cluster CA. It never uses the
  dashboard ServiceAccount to read resources on a user's behalf;
- Kubernetes API authorization, rather than dashboard-maintained policy,
  decides namespace visibility and read access;
- the initial UI may offer a best-effort namespace list. If the token cannot
  list Namespace objects, the UI must let the user enter a namespace name and
  show the API's normal authorization result;
- log access needs the same token to read the Run and its assigned Pod, create
  the Pod `portforward` subresource used by `krt logs`, and read the `log`
  subresource when runtimed log fallback is needed;
- artifact access requires the Run read permission plus the artifact-store
  permission defined by the selected backend;
- secrets, service account tokens, environment variables, and raw pod specs are
  hidden unless a future privileged operator view explicitly exposes them.

This has the same initial user experience as Kubernetes Dashboard token login.
Cluster identity integrations may mint or exchange the bearer token outside the
dashboard, but v0.x does not define an external-auth header protocol,
impersonation model, or a custom identity provider.

For local development, `krt` can port-forward the dashboard and supply the
current kubeconfig credential to a local-only proxy. That convenience path is
not a production authentication mode and must not make the browser retain the
kubeconfig credential or token after the local session ends.

### Creating a Dashboard Login Token

An operator should create a short-lived token for a least-privilege ServiceAccount
in each namespace that a dashboard user may inspect. The following example
grants one namespace read-only Run, Runtime, Workflow, and log access; it does
not grant access to Secrets or any write verb:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
rules:
  - apiGroups: ["kruntimes.io"]
    resources: ["runs", "runtimes", "workflowruns", "workflows", "actions", "persistentworkspaces"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/portforward"]
    verbs: ["create"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: kruntimes-dashboard-viewer
  namespace: team-a
subjects:
  - kind: ServiceAccount
    name: kruntimes-dashboard-viewer
    namespace: team-a
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: kruntimes-dashboard-viewer
```

Apply the manifest, then mint a bounded token and paste it into the dashboard
login page:

```bash
kubectl apply -f dashboard-viewer.yaml
kubectl -n team-a create token kruntimes-dashboard-viewer --duration=1h
```

`kubectl create token` requires Kubernetes 1.24 or later. Do not use a
cluster-admin credential for routine dashboard access. Cluster identity systems
may provide an equivalent user token instead; the dashboard treats both as a
standard Kubernetes bearer token. To browse multiple namespaces, create
equivalent namespace-scoped bindings or explicitly grant the additional
cluster-level read access after reviewing its scope.

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
3. Implement the reviewed bearer-token production mode and the local-only
   kubeconfig proxy mode.
4. Implement Run list/detail APIs with unit tests.
5. Implement log tail/follow through a backend-controlled path.
6. Add the frontend Run list/detail/log views.
7. Add an optional Helm chart value or separate dashboard chart.
8. Add E2E smoke coverage that installs the dashboard, creates a Run, lists it,
   opens detail, and fetches logs.
9. Add WorkflowRun/Workflow/Action/PersistentWorkspace views after the
   corresponding APIs stabilize.

## Remaining Questions

- Should the dashboard ship in the main kruntimes chart, a separate chart, or
  both?
- Should log access continue to use port-forward semantics or move to a
  dedicated cluster-internal log proxy service?
- How should artifact downloads be authorized and proxied when artifact stores
  are outside the cluster?
- What scale target should the first list/watch implementation support?
