# Runtime Gateway Transport Security and Transfer Bounds

Status: **Obsolete.** The shared Runtime gateway was removed; Runtime access now
goes through the aggregated Run extension API and Console no longer serves a
data plane. This note is retained for historical context only.

## Problem

The shared Runtime gateway currently serves an authenticated but plain HTTP
cluster-local endpoint. `Run.status.endpoint` already supports `HTTP`, `HTTPS`,
and an optional PEM trust bundle, but the chart has no contract for mounting a
certificate, selecting HTTPS, or publishing trust material. The gateway also
relies on fixed implementation limits for JSON request bodies and some response
shapes instead of exposing a small operator-controlled policy.

This document defines the incremental v0.x transport contract. It deliberately
does not make the gateway an Internet-facing ingress or certificate authority.

## Goals and Non-goals

- Keep the gateway Service cluster-local and retain Kubernetes bearer-token
  authentication and per-Run authorization.
- Make the listener protocols an explicit set. Operators choose `http`,
  `https`, or both protocols.
- Generate and retain the default gateway TLS Secret in the chart, while
  allowing an operator-provided Secret and a later cert-manager producer.
- Make request and response bounds explicit with conservative defaults.
- Preserve the current plain-HTTP deployment as the default v0.x-compatible
  configuration.

This does not create an Ingress, Gateway, LoadBalancer, or external DNS record;
does not issue certificates in the Runtime controller or expose private key
material through Run status; and does not automatically change HTTP installs to
HTTPS or expose Runtime Server gRPC.

## Helm API

```yaml
gateway:

  # At least one distinct value is required. HTTPS requires a certificate.
  # When both are present, status publishes the HTTPS endpoint.
  protocols: [http] # http, https
  httpBindAddress: ":8084"
  httpsBindAddress: ":8444"
  httpServicePort: 80
  httpsServicePort: 443
  maxRequestBodyBytes: 1048576
  maxResponseBodyBytes: 1048576
  maxHeaderBytes: 1048576
  tls:
    # Empty selects the chart-managed <gateway-name>-tls Secret. A non-empty
    # name selects an existing operator-managed Secret unless certManager is
    # enabled.
    secretName: ""
    # tls.crt, tls.key, and ca.crt are mandatory for HTTPS endpoint trust.
    certificateKey: tls.crt
    privateKeyKey: tls.key
    caBundleKey: ca.crt
    certManager:
      enabled: false
      issuerRef:
        name: ""
        kind: Issuer
        group: cert-manager.io
```

`protocols: [http]` starts only the HTTP listener and publishes an `http://`
endpoint. `protocols: [https]` starts only the TLS listener and publishes
`https://`. `protocols: [http, https]` starts both listeners and exposes named
`http` and `https` Service ports. To avoid downgrading new clients, it
publishes only the HTTPS URL in
`Run.status.endpoint`; the equivalent HTTP route remains intentionally
available during the operator-controlled migration period.

When `https` is selected, an empty `secretName` selects a chart-managed
`<gateway-name>-tls` Secret. Helm uses `lookup` to retain an existing Secret
across upgrades and otherwise generates a CA plus a Service-DNS certificate.
A non-empty `secretName` uses an existing operator-managed Secret. In either
case, the key is mounted read-only and the gateway requires both certificate
and private-key files before serving TLS; the controller also requires the
configured CA-bundle key to publish a verifiable HTTPS endpoint.

With cert-manager enabled, the chart creates one namespaced
`cert-manager.io/v1 Certificate` whose `secretName` is `gateway.tls.secretName`.
Its DNS names are the gateway Service's short, namespace-qualified, and
cluster-local names. Rendering fails unless the secret name and issuer reference
are complete. The selected issuer must also write the configured CA-bundle key
to the Secret so HTTPS clients can validate `Run.status.endpoint.caBundle`.
This remains opt-in, so installations without cert-manager do not require its
CRDs.

The chart owns the default Secret, Deployment, Service, and optional
Certificate; the Runtime controller does not own, create, or rotate certificate
resources. Changing protocols rolls gateway Pods and updates the controller
flag; ready Runs retain their old endpoint until owner runtimed reconciles them.
Operators should not remove `http` until HTTP clients have migrated.

## Endpoint Trust Publication

The controller receives the selected public gateway URL and a CA bundle bounded
to 64 KiB, never a private key. Helm mounts the chart-managed CA or the required `ca.crt`
key from an existing Secret read-only into the controller and passes its file
path. The controller writes this public trust bundle to a Runtime Pod-template
annotation; runtimed projects that annotation with the Downward API and copies
it to the HTTPS `Run.status.endpoint.caBundle`. Clients may instead use their
own trust store. The controller receives no Secret-read RBAC; runtimed never
mounts the TLS Secret.

## Transfer Bounds

| Value | Default | Enforcement |
| --- | ---: | --- |
| `gateway.maxRequestBodyBytes` | 1 MiB | reject a larger JSON request with `413 Payload Too Large` before runtimed |
| `gateway.maxResponseBodyBytes` | 1 MiB | buffer JSON and return `413` before headers; never send partial JSON |
| `gateway.maxHeaderBytes` | 1 MiB | Go HTTP server rejects excessive headers before routing |

Values must be positive and are passed as explicit gateway flags. The response
limit applies to gateway-generated JSON only. Runtime Servers retain their own
direct-gRPC output/file bounds; session paging still bounds directory listings.
Larger durable results belong in ArtifactStore.

## Security, Failure, and Delivery

TLS protects only client-to-gateway traffic. When both protocols are enabled,
HTTP remains a deliberate
compatibility listener and is not a confidentiality boundary. Internal gRPC remains cluster-local
and protected by NetworkPolicy plus the authenticated gateway boundary. A
missing, unreadable, malformed, or mismatched TLS Secret must prevent readiness;
the gateway never falls back to HTTP. The key is mounted read-only only into
gateway Pods and is never copied to logs, ConfigMaps, status, or flags.

Delivery is split into independent reviewable slices:

1. Add protocol-set listener and Service configuration, chart-managed or existing
   Secret selection, endpoint CA publication, Helm/unit coverage, and an E2E
   run that verifies both protocols simultaneously.
2. Add opt-in cert-manager `Certificate` rendering and chart tests.
3. Add configurable HTTP body/header bounds, gateway unit tests, and focused
   rejection E2E coverage.
