# Run Extension API

## Status

The Kubernetes aggregated API serves Run logs, Session operations and files,
and Function invocation for every client. Console relays browser Session
operations to it and no longer embeds or exposes a Runtime gateway. The
controller no longer populates `Run.status.endpoint`; that field is deprecated.

## Problem

A Run is a logical execution, while several Runs can share one Runtime Pod.
The `runtimed` container emits structured records keyed by Run UID. Giving every
user direct `pods/log` access would expose unrelated executions and requires
Pod discovery. Dashboard and `krt logs` therefore need a Run-scoped API.

## API and authorization

The chart installs an `APIService` for `logs.kruntimes.io/v1alpha1` and the
following namespaced subresource:

```
GET /apis/logs.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/log
```

Kubernetes authenticates the caller using the normal kubeconfig mechanisms:
bearer tokens, exec credentials, and client certificates. Before proxying to
the backend, the API server authorizes:

```yaml
apiGroups: ["logs.kruntimes.io"]
resources: ["runs/log"]
verbs: ["get"]
resourceNames: ["<run-name>"]
```

The backend accepts identity headers only over the API aggregation layer's
verified request-header mTLS connection. It does not repeat a `runs get`
SubjectAccessReview: authorization of `runs/log` is the API contract.

Its ServiceAccount has the minimal `get pods/log` permission needed to open the
assigned Runtime Pod's `runtimed` log, filters records to the target Run UID,
and never exposes the Pod log stream directly to the caller.

## Response and streaming

Snapshots return JSON:

```json
{"items":[{"stream":"stdout","message":"..."}],"cursor":"..."}
```

`tailLines` defaults to 100 and is bounded to 500. Snapshot payloads are
bounded to 1 MiB. `follow=true` returns `application/x-ndjson`; each line is a
record with an opaque cursor. A reconnect passes `cursor` to resume after the
last delivered record. The backend opens the Pod log stream before writing
response headers, so an unavailable log service remains a regular HTTP error.
Every non-success response uses the Kubernetes `metav1.Status` shape, preserving
its HTTP code, reason, and actionable message for `krt` and Dashboard. In
particular, a `409 Conflict` means the Run's assigned Runtime Pod no longer
exists, so its ephemeral container logs cannot be recovered.

Go callers use `api/runextensionapi.Client`, which follows client-go request
shapes for every Run extension API group:

```go
client, err := runextensionapi.NewForConfig(restConfig)

request := client.GetRunLogs(namespace, runName, &runextensionapi.RunLogOptions{Follow: true})
stream, err := request.Stream(ctx)
raw, err := client.GetRunLogs(namespace, runName, options).Do(ctx).Raw()

// Raw client-go request builders are also available.
url := client.Sessions().Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("session").URL()
```

## Consumers

`krt logs` always uses the current kubeconfig's Kubernetes API endpoint; it has
no separate gateway URL, CA, or TLS-skip flags. Console keeps the login token in
an HttpOnly cookie and forwards it server-side to the aggregated API. Neither
client needs `runs get`, Pod discovery, `pods/log`, or Pod port-forward access
to retrieve logs.

Console has no Runtime access route and no Runtime Pod network access. Every
Run data-plane request is authorized by kube-apiserver against the matching
aggregated Run subresource.

## Session operations

The same `run-extension-api` Deployment also serves a second APIService:

```
GET /apis/sessions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/session
```

This is a WebSocket endpoint for Session operations. The API server authenticates
the kubeconfig caller and authorizes `get` on `sessions.kruntimes.io/runs/session`.
The backend accepts the forwarded identity only over verified aggregation mTLS,
loads the named ready Session Run, and streams its operations to the assigned
Runtime. `krt` also reads the Run, so its caller needs `get` on `kruntimes.io/runs`.
The existing `logs.kruntimes.io` path remains available.
The Runtime controller's NetworkPolicy permits `run-extension-api` Pods to
reach the Runtime Service's Session gRPC port.

The backend also serves bounded workspace file reads for the same Session
subresource:

```
GET /apis/sessions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/session/files
GET /apis/sessions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/session/files/{path}
```

The list endpoint selects one bounded page of direct children with `path`,
`limit`, and `pageToken` and returns `entries` and `nextPageToken`. The read
endpoint returns base64 `contents` and `truncated`, bounded by `maxBytes`. The
`files` segment stays below the `session` subresource segment, so the API server
authorizes both endpoints with the same `get` on
`sessions.kruntimes.io/runs/session`; no additional permission is required. The
backend resolves the ready Session Run and reads the file from the owning
runtimed, exactly like a Session operation.

## Function invocation

The same backend also serves a third APIService:

```
POST /apis/functions.kruntimes.io/v1alpha1/namespaces/{namespace}/runs/{name}/function
```

The API server authenticates the caller and authorizes `create` on
`functions.kruntimes.io/runs/function` for the named Run. The request body is one
bounded JSON value and the optional `X-Kruntime-Invocation-ID` header carries an
idempotency key bounded to 128 bytes. The backend loads the ready Function Run,
invokes the runtimed FunctionRuntime proxy, and returns:

```json
{"invocationId":"...","output":"<base64>","contentType":"...","outputs":{}}
```

Request and response sizes use the backend's bounded limits. A request that
cannot be authorized or admitted fails with an HTTP error before any invocation
is admitted.

## Installation

`runExtensionAPI.enabled` defaults to true.
All APIService objects are cluster-scoped, so exactly one platform release in a
cluster must own each group/version. Set `runExtensionAPI.enabled: false` for
other releases.

## Verification

E2E coverage proves that a credential with only `get` on the named
`logs.kruntimes.io/runs/log` resource can retrieve a snapshot through `krt` and
can follow a still-running one-shot Run. A credential lacking that subresource
permission is denied by the API server.
