# Kruntimes Console

Kruntimes Console is the always-installed browser UI and read API. It combines
the browser console, Kubernetes resource views, Run logs, and an authenticated
token-forwarding proxy for Run data-plane actions. Console is not a data plane:
it never authorizes Runtime operations itself and never dials a Runtime Service.

```
browser / krt / SDK
        |
        v
Kubernetes API server ---> Run extension API ---> runtimed (owner Runtime Pod)
        ^
        |  (browser only, same-origin proxy)
Kruntimes Console (HTTPS)
  |- /api/...  Console resource and log APIs
  |- /api/.../session  proxied Session WebSocket
```

## Data plane

The Kubernetes aggregated Run extension API in `run-extension-api` owns the Run
data plane: Run logs, Session operations and files, and Function invocation.
kube-apiserver authenticates every caller with normal kubeconfig mechanisms and
authorizes the matching Run subresource before proxying the request to the
backend. Console never performs a TokenReview/SubjectAccessReview for data-plane
requests, never dials a Runtime Service, and holds no Runtime Pod network
access.

Console's Service is not written to `Run.status.endpoint`; that field is
deprecated for Function and Session Runs. Clients derive the aggregated API
endpoint from their Kubernetes REST configuration.

## Authentication

Console accepts normal `Authorization: Bearer ...` credentials. The browser
login exchanges a pasted bearer token for the host-only, `HttpOnly`, `Secure`,
`SameSite=Strict` Console session cookie, and JavaScript never receives the
credential. Explicit Authorization headers from `krt` and SDK clients take
precedence. Console uses `TokenReview` only to validate a login credential and
display its Kubernetes username; it does not make authorization decisions.

## Session operations in the browser

The detail page for a ready Session Run provides a shell-command panel. Console
relays the same-origin Session WebSocket to the Kubernetes aggregated Session
API at
`/apis/sessions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/session`
using the caller's Kubernetes credential. kube-apiserver authenticates the
caller and authorizes `get` on `sessions.kruntimes.io/runs/session`; Console
performs no Session-operation authorization decision itself. The panel sends
the command as its first client frame and renders ordered accepted, output,
progress, and terminal events as they arrive. Binary output is decoded from the
protocol's JSON base64 representation before display.

The panel is disabled until the Session Run is `Ready`. Its Cancel control
sends `{"type":"cancel","operationID":"..."}` over the WebSocket. The Console client sends
`{"type":"send","operation":...}` when it opens the connection and, when the
Run configures a lease, private heartbeat frames for as long as that connection
remains open. Closing it does not release the backing Session Run. A transport error or an early
connection close is displayed in the panel. Selecting **Run again** always
starts a new operation; the current WebSocket protocol does not provide an
operation-ID resume frame or a separate NDJSON fallback.

## Helm configuration

Console is always installed. `console.image`, `console.replicas`,
`console.publicRead`, `console.tls`, and the standard pod scheduling/security
values configure it. `console.tls` supports chart-managed self-signed TLS, an
existing Secret, or cert-manager. The Runtime access limits, authorization
cache, and client-certificate options were removed together with the embedded
Runtime gateway.
