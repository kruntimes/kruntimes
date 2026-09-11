# Runtime Gateway Run Log API

Status: **Implemented — direct Gateway and Kubernetes aggregated API**

## Problem

Run output is written as structured records to the owner Runtime Pod's
`runtimed` container log. Before this API, Dashboard read that Pod log with
the caller's Kubernetes credential, while `krt logs` port-forwarded to the
Runtime Pod and could fall back to the Pod-log subresource. Consequently, a
user who could read a Run could not necessarily read its logs: Dashboard
required `get pods/log`, while `krt logs` also needed Pod discovery and
`create pods/portforward`.

Those are Kubernetes implementation permissions, not the user-facing
capability that kruntimes intends to offer: *read the logs of this Run*.

## Decision

The Runtime Gateway remains the direct, external HTTP Run-log API. It does not
become an aggregation server and remains useful for independently deployed
clients. In addition, the chart installs a separate `runtime-log-apiserver`
and registers `logs.kruntimes.io/v1alpha1` through Kubernetes API aggregation.
It does not create a log store: both paths read the same existing Kubernetes
container-log stream and expose only filtered, bounded Run records.

`krt logs` uses the aggregated endpoint by default, so the user's ordinary
kubeconfig (bearer token, exec credential, or client certificate) is
authenticated by the API server. Supplying `--gateway-url` deliberately opts
into the direct Gateway contract. Dashboard likewise uses aggregation by
default; its chart can explicitly select the direct Gateway mode.

The initial endpoint is:

```text
GET /v1/namespaces/{namespace}/runtimes/{runtime}/runs/{runUID}/logs?tailLines={lines}&limitBytes={bytes}&follow={true|false}
```

`namespace`, `runtime`, and `runUID` identify the same immutable Run selection
used by the existing Gateway APIs. `runUID` is never replaced with a mutable
Run name, so a deleted-and-recreated Run cannot receive records from its
predecessor.

The aggregated endpoint is intentionally a distinct API group so it cannot
take ownership of the CRD's `kruntimes.io/v1alpha1` group/version:

```text
GET /apis/logs.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/log?tailLines={lines}&follow={true|false}&cursor={opaque}&timeoutSeconds={seconds}
```

It is protected by Kubernetes `get` on `logs.kruntimes.io/runs/log`, and the
backend also submits an exact SubjectAccessReview for `get` on the selected
`kruntimes.io/runs` object. The aggregated server trusts identity headers only
after mTLS verification of the Kubernetes aggregation proxy. At startup it
reads the API-server-owned `kube-system/extension-apiserver-authentication`
ConfigMap, including its request-header client CA and allowed proxy identity.

## Request and Response Contract

The query shape deliberately follows the relevant Kubernetes `pods/log`
semantics. `tailLines` is optional, defaults to 100, and must be an integer
from 1 through 500. For a non-following request, `limitBytes` is optional,
defaults to 1 MiB, and must be a positive integer no greater than 1 MiB. The
Gateway starts the `runtimed` container-log read at the Run's `status.startTime`
(falling back to its creation time), filters by the requested Run UID, and then
applies `tailLines` to the selected records. It applies `limitBytes` to the
non-following Kubernetes source read. This prevents later activity on a shared
Runtime Pod from pushing a still-retained selected Run out of a global Pod-log
tail. It still does not promise durable or complete log retention: a busy Pod
can exceed the source bound, and durable retention remains the responsibility
of the cluster log collector.

`limitBytes` is not accepted with `follow=true`. Kubernetes applies
`PodLogOptions.LimitBytes` to the entire followed source stream; applying the
1 MiB snapshot bound there would silently end an otherwise healthy follow
connection. A follow stream is bounded by Gateway concurrency, client
cancellation, and the 2 MiB per-record limit instead.

With `follow` omitted or `false`, the response is `application/json`:

```json
{
  "items": [
    {
      "timestamp": "2026-09-02T10:00:00Z",
      "stream": "stdout",
      "message": "hello",
      "invocationId": "optional-invocation-id",
      "operation": "execute",
      "outcome": "succeeded"
    }
  ]
}
```

Fields that are absent from a structured runtimed record are omitted. The
stable, safe fields are the timestamp, stream, message, Run UID-derived
selection, invocation ID, operation, outcome, status code, exit code,
timeout marker, and duration. Raw Kubernetes log metadata and records for
other Runs are never returned.

`follow=true` is a first-class streaming operation, analogous to Kubernetes
`pods/log?follow=true`, rather than a polling convention. The response is
`application/x-ndjson`; after headers are sent, the Gateway flushes each
matching record as it arrives and does not wait to build a complete response.
The stream reads from the Run start rather than a global Pod tail, so matching
records still retained for the selected Run are not hidden by later Runs.
Records for other Runs are discarded without writing an empty line. The stream
ends when the caller disconnects, the Pod log stream closes, or the Gateway
shuts down. `follow` does not turn the Gateway
into an unbounded persistence service; request-concurrency limits continue to
apply for the lifetime of the stream.

The endpoint is read-only. It supports task, function, and session Runs when
they have an assigned Runtime Pod. A Run without an assigned Pod, or one whose
recorded assigned Runtime Pod has since been deleted, returns `409 Conflict`.
A missing Run or a Run which no longer belongs to the specified Runtime returns
`404 Not Found`.

## Streaming Implementation

The Gateway implements the stream itself; it does not redirect the caller to
the Kubernetes API or proxy an arbitrary Pod-log URL.

1. Parse and validate the route and query before writing a response.
2. Resolve the immutable Run from the Gateway cache, verify its UID and
   specified Runtime, then authorize the caller for that exact Run.
3. Use the Gateway ServiceAccount's typed core client to open exactly one
   Kubernetes log stream:

   ```go
   coreClient.Pods(run.Namespace).GetLogs(run.Status.AssignedPod, &corev1.PodLogOptions{
       Container:  "runtimed",
       Follow:     follow,
       SinceTime:  run.Status.StartTime, // or creation time if it is unset
       // LimitBytes is set only when Follow is false.
   }).Stream(ctx)
   ```

   The stream must open successfully before the Gateway writes HTTP headers;
   this permits a normal bounded JSON error for an unavailable Pod-log service.
4. Decode the Kubernetes stream one newline-delimited record at a time with a
   bounded reader. A structured record is limited to 2 MiB (the 1 MiB raw-log
   request limit plus JSON framing headroom). A larger line is discarded
   without allocating unbounded memory. Invalid JSON and records whose
   `run_uid` differs from the selected UID are discarded.
5. For a matching record, encode only the documented safe fields as one JSON
   line, write it to the response, and call `http.NewResponseController(w).Flush()`.
   The response has `Content-Type: application/x-ndjson` and no content length;
   HTTP/1.1 uses chunked transfer while HTTP/2 streams data frames normally.
6. Stop on `request.Context().Done()`, source EOF, or a source read error, then
   close the Kubernetes stream. Once streaming headers have been sent, a later
   source error cannot be converted into an HTTP error response; the Gateway
   ends the stream and records the error server-side. Clients treat an
   unexpected EOF as an interrupted follow and may reconnect with a new bounded
   tail request.

For `follow=false`, the same decoder collects at most `tailLines` matching
records in a bounded ring and writes the documented JSON envelope only after
the source stream closes. This is intentionally separate from the streaming
writer so an ordinary tail response remains valid JSON and never becomes a
partially written document.

## One-Shot Runtime Output Projection

One-shot `Runtime.Execute` is asynchronous: it creates the local execution and
returns its ID without waiting for command completion. The Runtime Server's
unary `Status` response contains the current bounded cumulative stdout and
stderr buffers while that execution is running; exit code and final state are
only authoritative once it becomes terminal.

Owner runtimed polls that existing status while a one-shot Run is active and
projects only newly observed **complete lines** into its structured container
log. It retains a cursor and an unfinished trailing line independently for
stdout and stderr. A final terminal status flushes a remaining unterminated
line after the Run terminal status update succeeds. Consequently a Gateway
`follow=true` reader, Dashboard, and `krt logs --follow` can observe command
output before the command exits, without placing log contents in `Run.status`
or introducing a second log transport.

This projection is deliberately at-least-once. The cursor is local to one
runtimed process; after runtimed or Runtime Server recovery, retained output
can be emitted again rather than silently lost. Clients that reconnect to the
Gateway follow stream must likewise tolerate duplicate retained records. The
Runtime Server's per-stream output limit remains the upper bound: once its
bounded buffer truncates output, the truncation marker is emitted but later
discarded bytes cannot be recovered. Kubernetes log collection remains the
durable retention mechanism.

## Client Streaming Contract

Clients stream logs with an ordinary authenticated HTTP `GET`; there is no
WebSocket, Server-Sent Events protocol, polling endpoint, or client-visible Pod
connection. For example, a command-line client sends:

```sh
curl --no-buffer \
  --header "Authorization: Bearer $TOKEN" \
  --header "Accept: application/x-ndjson" \
  "${GATEWAY_URL}/v1/namespaces/team-a/runtimes/python/runs/${RUN_UID}/logs?tailLines=100&follow=true"
```

`--no-buffer` is important for a terminal client: it makes curl print each
NDJSON record as the response body arrives. A Go client retains the response
body and decodes one JSON value at a time until its context is cancelled or the
server closes the body:

```go
request, err := http.NewRequestWithContext(ctx, http.MethodGet, logsURL, nil)
if err != nil { /* handle */ }
request.Header.Set("Authorization", "Bearer "+token)
request.Header.Set("Accept", "application/x-ndjson")

response, err := httpClient.Do(request)
if err != nil { /* handle */ }
defer response.Body.Close()
if response.StatusCode != http.StatusOK { /* decode bounded error */ }

decoder := json.NewDecoder(response.Body)
for {
    var record RunLogRecord
    if err := decoder.Decode(&record); errors.Is(err, io.EOF) { break
    } else if err != nil { /* interrupted or malformed stream */ }
    render(record)
}
```

The Dashboard browser does not call the Gateway directly: its login token is
HttpOnly and must not be exposed to JavaScript. Instead, the Dashboard backend
opens this exact Gateway stream with the session token and relays the NDJSON
body through its same-origin internal log endpoint. The React frontend consumes
that response's `ReadableStream`, incrementally splits newline-delimited JSON,
and renders each record. This preserves the browser's existing cookie boundary
while retaining end-to-end streaming rather than polling.

The Gateway base URL is deployment configuration, not a Pod address. The
Dashboard backend uses the in-cluster Gateway Service. An in-cluster `krt`
client may use that Service DNS name. An external `krt` client must be given an
operator-managed reachable Gateway URL and its TLS trust material (for example
`--gateway-url` plus the normal system trust store or `--gateway-ca-file`). It
does not silently create a Runtime-Pod or Gateway Pod port-forward, since that
would reintroduce a caller `pods/portforward` requirement. The chart continues
to keep the Gateway ClusterIP by default; choosing an external exposure is a
separate operator deployment decision.

`krt logs` uses the kubeconfig-selected bearer token or exec-token credential
for both its initial `get runs` request and the Gateway request:

```sh
krt logs my-run -n team-a --gateway-url https://gateway.example \
  --gateway-ca-file ./gateway-ca.crt --tail 100 --follow
```

It uses an independent Gateway TLS transport. It trusts the normal system
store or `--gateway-ca-file`; `--gateway-insecure-skip-tls-verify` is an
explicit development-only escape hatch. The transport preserves a kubeconfig
client certificate when present. To authorize that certificate, an operator
sets `gateway.tls.clientCASecretName` to the CA Secret that signs Kubernetes
user certificates. The HTTPS Gateway verifies mTLS and submits the verified
certificate CN as the Kubernetes username and O values as groups in its
SubjectAccessReview. Without that Gateway client-CA configuration, bearer and
exec tokens remain the supported authentication methods. `--tail` defaults
to 100 and accepts 1 through 500. The CLI decodes the documented JSON/NDJSON
records and writes only `stdout` and `stderr` records to their corresponding
terminal streams.

The Dashboard chart prefers the Gateway's in-cluster HTTP Service port when
HTTP is enabled (including when both protocols are enabled). In HTTPS-only
deployments it mounts the configured Gateway CA-bundle key read-only and uses
that bundle to verify the Gateway Service certificate. The Dashboard does not
mount a Gateway private key or grant its ServiceAccount `pods/log`.

### HTTP Protocol and Server Requirements

`ReadableStream` is a Fetch response-body feature, not an HTTP upgrade
protocol. It works with an HTTP/1.1 response streamed with chunked transfer,
an HTTP/2 response streamed in DATA frames, or HTTP/3. The initial Gateway
slice must support HTTP/1.1, and its HTTPS listener must negotiate HTTP/2 via
ALPN (`h2`, with `http/1.1` fallback). A client never sends
`Upgrade: websocket` and the Gateway never returns `101 Switching Protocols`.

The Go handler must set `Content-Type: application/x-ndjson` and
`Cache-Control: no-cache`, omit `Content-Length` and `Content-Encoding`, call
`WriteHeader(http.StatusOK)` only after opening the Kubernetes log stream, and
then encode/write/flush each matching record. Go's `net/http` automatically
uses chunked transfer for the HTTP/1.1 case when no content length is present;
the handler must not set `Transfer-Encoding` itself.

For the TLS listener, the Gateway must configure its `http.Server` and use
`server.ServeTLS` on the raw TCP listener (or equivalently configure
`TLSConfig.NextProtos` and HTTP/2 before serving). Wrapping a listener in TLS
and calling a generic `Serve` without ALPN setup is insufficient to promise
HTTP/2. A long-lived log response must not be subject to a server-wide
`WriteTimeout`; the Gateway keeps that value disabled for the streaming
listener, while retaining header and request-concurrency bounds.

The chart's ClusterIP Service is an L4 hop and does not buffer responses. If an
operator later places an ingress, reverse proxy, or service mesh proxy in
front of the Gateway, that proxy must preserve streaming: disable response
buffering/compression for this route and configure an idle timeout appropriate
for `follow=true`. This is an operator exposure concern, not an alternate API
transport.

## Authentication, Authorization, and RBAC

The Gateway requires an `Authorization: Bearer` token. It authenticates the
token with Kubernetes `TokenReview`, then creates a `SubjectAccessReview` for
`get` on the resolved, exact `kruntimes.io` `runs` resource in its namespace.
The resource name used in the review is the resolved Run name; the immutable
UID is used for endpoint selection and log filtering.

After this decision succeeds, the Gateway—not the caller—derives
`Run.status.assignedPod` and reads only that Pod's `runtimed` container. The
caller cannot select a Pod, container, namespace, or UID other than the Run
selected by the route. The Gateway ServiceAccount needs:

```yaml
- apiGroups: [""]
  resources: ["pods/log"]
  verbs: ["get"]
```

in addition to its existing Run cache, TokenReview, and SubjectAccessReview
permissions. The Gateway must not receive broad Pod `get`, `list`, `watch`,
or write permissions for this feature.

Thus a dashboard user or CLI user needs `get` on the relevant Run, but no
longer needs `get pods/log`, `get/list pods`, or `create pods/portforward` just
to read that Run's logs. Other Dashboard detail pages retain their separately
documented authorization policy.

## Errors

| Condition | HTTP status |
| --- | ---: |
| Missing or invalid bearer token | 401 |
| Authenticated caller lacks `get` on the resolved Run | 403 |
| Route has invalid `tailLines`, `limitBytes`, or `follow` | 400 |
| No matching Run / runtime | 404 |
| Matching Run has no assigned Pod, or its assigned Runtime Pod no longer exists | 409 |
| Gateway log reader is not configured or Kubernetes log service is unavailable | 503 |
| Gateway request concurrency limit reached | 429 |

The response body is a bounded Gateway error object. It must not reveal an
unselected Pod name, container name, token, or Kubernetes authorization detail.

## Client Migration and Delivery

The Dashboard endpoint remains an internal Dashboard API. Its backend calls
this Gateway endpoint with the login token. `krt logs` calls the same endpoint
instead of port-forwarding Runtime Pods or falling back to direct `pods/log`.
Neither client retains the old privileged path.

Because the Gateway chart component is opt-in, Dashboard and `krt logs`
return a clear configuration error when this API is unavailable; they do not
fall back to direct Pod access. Operators enabling Dashboard log access under
this model must also enable and make the Gateway reachable. The standard E2E
installation enables both components.

The implementation was split into independently reviewable commits:

1. Gateway route, structured-record filtering and bounds, ServiceAccount
   `pods/log` permission, HTTP/1.1 plus HTTPS/HTTP/2 streaming setup,
   follow-stream flush/disconnect tests, and Helm coverage.
2. Dashboard migration to the Gateway client, removal of its caller-scoped
   Pod-log reader and corresponding RBAC, plus focused tests.
3. `krt logs` migration to the Gateway client, removal of Runtime-Pod
   port-forward and direct Pod-log fallback, plus focused tests.
4. End-to-end coverage proving a token with only `get runs` can read its Run
   logs while a token lacking that permission cannot.
