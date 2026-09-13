# Dashboard

This document describes the accepted v0.x design and its implemented initial
Dashboard surface.

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
- Browse Runtime pools, their Pod health/capacity, and assigned Runs.
- Browse WorkflowRuns, their job DAG and step-to-Run links.

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

### v0.x Decisions

The dashboard is an opt-in component of the `kruntimes` chart, with
`dashboard.enabled: false` by default. Its Deployment, ServiceAccount,
Service, and TLS resources are installed in the same Helm release as the
control plane. This preserves one upgrade and RBAC boundary without adding a
component to installations that do not need it.

Production dashboard traffic is HTTPS-only. The Service remains ClusterIP, and
a bearer-token login page is never exposed over plaintext HTTP. The chart lets
the operator choose one certificate source:

- an existing TLS Secret, normally for a certificate trusted by dashboard
  users;
- a chart-generated self-signed certificate, suitable for local development
  and explicitly trusted private deployments; or
- a cert-manager Certificate using an existing Issuer or ClusterIssuer. The
  selected issuer may itself be a cert-manager self-signed issuer.

The selected source writes the same mounted TLS Secret. The chart rejects
ambiguous combinations rather than silently choosing a certificate source.

The Helm values make that selection explicit: `dashboard.tls.selfSigned` is
the default and causes the chart to create the TLS Secret; to mount an
operator-provided Secret, set `selfSigned: false`, leave
`certManager.enabled: false`, and set `secretName`; to use cert-manager, set
`selfSigned: false` and `certManager.enabled: true` with an existing
`issuerRef`. cert-manager may write either the default Dashboard TLS Secret or
the `secretName` specified by the operator. An existing self-signed Issuer is
therefore supported without a separate dashboard-specific mode.

The backend has no ambient read authority for protected user requests. It
copies only the in-cluster transport configuration, clears the mounted
credential and installs the caller bearer token. The chart enables a
deliberately narrow public-read mode by default: the Dashboard ServiceAccount
may only get/list Namespaces, Runs, Runtimes, and WorkflowRuns, and the API
exposes only their summaries without a token. Operators can disable it with
`dashboard.publicRead.enabled=false`. Resource details and
Runtime/WorkflowRun pages remain caller-authorized.

The first version should read the following sources:

- `Run` objects through the Kubernetes API;
- Runtime Pod metadata referenced by `Run.status.assignedPod`;
- Kubernetes Events related to Runs and Runtime Pods when available;
- runtimed log/status endpoints through a backend-controlled path;
- `Run.status.outputs` and `Run.status.artifactRefs`.

Future versions can add PersistentWorkspace detail pages and metrics panels.

## Log Access

The dashboard backend must not expose Runtime Pods directly to browsers.

For v0.x, the implemented path is:

1. The user opens logs for a Run.
2. The Dashboard backend forwards the caller token only to the Kubernetes
   aggregated Run-log API.
3. The API server authenticates and authorizes `get` on the exact
   `logs.kruntimes.io/runs/log` subresource.
4. The aggregated backend resolves the assigned Runtime Pod and reads its
   `runtimed` log with its own narrow `pods/log` permission.
5. The backend streams or returns the requested UID-filtered log records.

Structured runtimed logs should remain keyed by Run UID so the dashboard can
show the correct logs even when Runtime Pods handle multiple Runs.

The Dashboard uses the in-cluster Kubernetes API Service and does not create a
browser-visible port-forward or expose Runtime Pods directly. The caller needs
`get` on the exact `runs/log` subresource, not `get pods/log`; the Log API
ServiceAccount performs the narrow Pod-log read. Artifact references are shown
as Run metadata; artifact downloads are outside the first Dashboard slice. The
complete endpoint, authorization, bounds, error, and migration contract is in
the [aggregated Run Log API design](runtime-gateway-log-api.md).

## Security Model

The dashboard must be read-only by default.

The proposed v0.x production model is Kubernetes bearer-token login:

- the user enters a Kubernetes bearer token into the Dashboard over HTTPS. The
  backend returns it in a host-only `HttpOnly`, `Secure`, `SameSite=Strict`
  session cookie with an eight-hour lifetime. JavaScript never reads or writes
  the token, and it is never written to localStorage, sessionStorage, or logs;
- the backend creates a request-scoped Kubernetes client with that bearer token,
  the in-cluster API server address, and the cluster CA. It uses this client
  for protected pages and aggregated Run-log access;
- the chart's narrowly privileged Dashboard ServiceAccount supplies tokenless
  namespace, Run, Runtime, and WorkflowRun summaries by default. It has only
  `get`/`list` on those resources and can be disabled explicitly;
- Kubernetes API authorization decides protected-page access. A token needs
  `get` on the exact `logs.kruntimes.io/runs/log` subresource to read logs;
- v0.x shows artifact references as Run metadata but does not download or proxy
  artifact content. A future artifact-download design must define its
  authorization and external-store boundary separately;
- secrets, service account tokens, environment variables, and raw pod specs are
  hidden unless a future privileged operator view explicitly exposes them.

This has the same initial user experience as Kubernetes Dashboard token login.
Cluster identity integrations may mint or exchange the bearer token outside the
dashboard, but v0.x does not define an external-auth header protocol,
impersonation model, or a custom identity provider.

For local development, `krt dashboard` starts a loopback-only proxy and
port-forwards the dashboard Service. The proxy obtains the current kubeconfig
credential and injects it only into forwarded requests; the browser never
receives the credential. It must bind only to 127.0.0.1 or another explicitly
chosen loopback address, reject non-loopback binds, not persist or log the
credential, and close the port-forward when the command exits. This is not a
production authentication mode.

### Creating a Dashboard Login Token

An operator should create a short-lived token for a least-privilege *viewer*
ServiceAccount in each namespace that a dashboard user may inspect. This is the
identity represented by the login token; it is distinct from the ServiceAccount
used by the dashboard Deployment itself. The following example grants one
namespace read-only Run, Runtime, Workflow, and log access; it does not grant
access to Secrets, workload mutation verbs, port-forwarding, or artifact
downloads:

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
login page. The Dashboard keeps it in an eight-hour HTTPS-only HttpOnly session
cookie:

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

Implemented endpoints are:

```text
GET /api/namespaces
GET /api/namespaces/{namespace}/runs
GET /api/namespaces/{namespace}/runs/{name}
GET /api/namespaces/{namespace}/runs/{name}/logs?tail=&follow=
GET /api/namespaces/{namespace}/runtimes
GET /api/namespaces/{namespace}/runtimes/{name}
GET /api/namespaces/{namespace}/workflowruns
GET /api/namespaces/{namespace}/workflowruns/{name}
POST /api/session
DELETE /api/session
```

The log endpoint reads only the assigned Pod's `runtimed` container through the
request-scoped Kubernetes client and discards records whose `run_uid` does not
match the Run's immutable UID. `tail` is bounded to 500 returned records; the
backend reads at most 500 recent container lines and 1 MiB per request. A
normal tail response is JSON. With `follow=true`, the endpoint returns filtered
newline-delimited JSON records until the caller disconnects or the Kubernetes
log stream closes. It never turns into a browser-visible Pod proxy.

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

The WorkflowRun detail page renders `spec.jobs[*].needs` as a read-only,
GitHub Actions-style staged DAG: root jobs appear in the leftmost stage, each
dependency advances a job to a later stage, and SVG edges visibly connect the
dependency to its consumer. A stage groups its parallel job rows in one card;
a node with multiple dependencies visibly joins its incoming edges. Each row
shows its observed phase and bounded `status.jobs[*].outputs.result` when
present.

Selecting a job navigates to the bookmarkable frontend route
`/namespaces/{namespace}/workflowruns/{workflowrun}/jobs/{job}`. That detail
page provides an all-jobs navigation rail and an expandable step list. Opening
a step with a child Run automatically requests the existing Run-log endpoint;
the browser never receives a Pod endpoint or a Kubernetes credential. The
graph and detail pages are views of the declared execution DAG, and must not
offer mutation or graph-editing controls. On narrow viewports the graph may
scroll in either direction instead of dropping dependency information. Its
viewport supports click-and-drag panning from non-Job canvas areas; Job nodes
remain ordinary links, and scrollbars and the mouse wheel remain available.

It should not include mutation buttons until the read-only authorization model
is proven.

The Settings page holds the global Style and Theme selectors, while the left
navigation provides a direct `/settings` link. Style offers GitHub (default),
Stripe-inspired, and Neumorphism independently of the Light, Dark, and System
Theme selector. GitHub
uses a restrained GitHub Primer Light/Dark operational palette: `#f6f8fa`
backgrounds, `#1f2328` text, `#d0d7de` borders, blue links/focus, compact 6px
corners, and a green primary action. It has no decorative grid or hover lift.
Stripe-inspired uses fine borders, light shadows, smaller corners, and restrained
purple accents; Neumorphism keeps same-material surfaces and raised/inset double
shadows. All styles cover resource pages, DAGs, Job Steps, and logs without
duplicating page components or changing routes, dependencies, or automatic Step
log loading.
The sidebar groups namespace-scoped Runs, Runtimes, and Workflow Runs under
`Kruntimes resources`; Settings and About are in the separate `Dashboard` group.
Each navigation item has a small semantic SVG icon in addition to its text label.
Icons use the selected style's tokens: GitHub and Stripe-inspired remain flat line
icons, while Neumorphism uses raised or selected-inset same-material icon shells.
Selections are browser-local, saved separately in localStorage, and applied before
React renders. Invalid style values fall back to GitHub; invalid themes
fall back to System. Blocked storage does not prevent in-page switching. System
follows the browser's color-scheme preference. Dense rows and status indicators
stay readable, with visible keyboard focus and reduced-motion support.
Stripe's Dashboard adaptation uses exact light-theme brand colors (`#635bff`,
`#0a2540`, `#f6f9fc`), a 40px background grid, multi-layer panel shadows, 12px
panel corners and 8px controls. It keeps compact operational typography rather
than marketing-page heading sizes and whitespace; prose is capped at 75ch.
Brand purple remains exact on primary controls; links on tinted surfaces use
`#554bd6` in light mode for text contrast.
Connect is a purple primary button; secondary/icon controls retain quiet surfaces.
Buttons lift 2px on hover and press to 0.98 scale with inset-only shadow, using
300ms ease-out. Reduced motion disables transforms and transitions. Graph nodes,
tables and logs never move on hover. Embedded tables stay flat; status badges use
small corners, while circular status icons and accessible dark-theme colors are
explicit exceptions to the supplied StyleKit. System fonts replace explicit Inter
in Stripe only. These are documented adaptations, not literal compliance with
the contradictory original prompt. No Helm settings or backend API changes are needed. See
`dashboard/frontend/tests/README.md` for verification and style extension guidance.

The frontend is React and TypeScript, built into static assets packaged beside
the Dashboard backend in its image and served from the same HTTPS origin as its internal API.
The source, backend, process entrypoint, and image definition live under the
top-level `dashboard/` directory. It has no separate frontend Service, no
browser-to-Kubernetes connection, and a same-origin Content Security Policy.
The bearer token is never readable by JavaScript: it is stored only in a
host-only HTTPS HttpOnly session cookie. Reload restores the session and
Disconnect removes it.

## Implementation Sequence

1. Add this design document and keep the roadmap explicit.
2. Add a dashboard backend package with read-only Kubernetes client wiring.
3. Implement the reviewed bearer-token production mode and the local-only
   kubeconfig proxy mode.
4. Implement Run list/detail APIs with unit tests.
5. Implement log tail/follow through a backend-controlled path.
6. Add the frontend Run list/detail/log views.
7. Add the optional `dashboard.enabled` resources to the `kruntimes` chart.
8. Deploy the Dashboard in the standard E2E environment. Browser-specific E2E
   coverage is deferred until there is a stable browser test harness.
9. Add PersistentWorkspace views after their API stabilizes.

## Remaining Questions

- Should log access continue to use port-forward semantics or move to a
  dedicated cluster-internal log proxy service?
- How should artifact downloads be authorized and proxied when artifact stores
  are outside the cluster?
- What scale target should the first list/watch implementation support?
