# Kruntimes Console

Kruntimes Console is the single public access component installed by the
platform chart. It combines the browser console, Kubernetes resource views,
Run logs, and the versioned Runtime access API. There is no separately
deployed Runtime Gateway.

```
browser / krt / SDK
        |
        v
Kruntimes Console (HTTPS)
  |- /api/...  Console resource and log APIs
  |- /v1/...   Runtime access handler
        |
        v
runtimed in the selected Runtime Pod
```

## Runtime access

`internal/gateway.Server` remains the protocol adapter for `/v1/` routes. It
is an internal `http.Handler`, registered by Console; it has no listener,
Deployment, Service, TLS configuration, or independent lifecycle. It resolves
the ready Run, performs Kubernetes TokenReview and SubjectAccessReview, then
relays to the owning Runtime Service.

The Console Service is the endpoint stored in `Run.status.endpoint` for ready
Function and Session Runs. The controller copies the configured Console CA
bundle to the endpoint and Runtime Pods mount it through the existing downward
API path, so runtimed can trust the Console HTTPS Service.

## Authentication

Console accepts normal `Authorization: Bearer ...` credentials and, when
`console.tls.clientCASecretName` is configured, verified Kubernetes client
certificates. The browser login exchanges a pasted bearer token for the
host-only, `HttpOnly`, `Secure`, `SameSite=Strict` Console session cookie.
For `/v1/` requests made by the browser, Console injects that cookie's token
into the internal handler; JavaScript never receives the credential. Explicit
Authorization headers from `krt` and SDK clients take precedence.

## Helm configuration

Console is always installed. `console.image`, `console.replicas`,
`console.publicRead`, `console.tls`, and `console.access` configure it.
`console.tls` supports chart-managed self-signed TLS, an existing Secret, or
cert-manager. A Secret used for a Runtime endpoint must include
`console.tls.caBundleKey` (default `ca.crt`) in addition to its certificate and
private key. `console.access` contains authorization-cache and bounded request,
response, and header settings formerly attached to the standalone Gateway.
