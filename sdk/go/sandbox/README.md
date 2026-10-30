# Go Sandbox SDK

`sandbox` is the Go client for Session-mode Runs. A Sandbox is backed by the
existing Kubernetes `Run` resource; it is not a second resource type.

In a cluster, create the client from the caller's Kubernetes REST config:

```go
import sdk "github.com/kruntimes/kruntimes/sdk/go/sandbox"

client, err := sdk.NewFromRESTConfig(restConfig, sdk.Config{})
if err != nil {
    return err
}
runtime := client.Runtime("agents", "python-session")
sandbox, err := runtime.AcquireSandbox(ctx, sdk.AcquireOptions{
    Name: "diagnose-api",
})
if err != nil {
    return err
}
defer sandbox.Release(ctx)
session, err := sandbox.OpenSession(ctx)
if err != nil {
    return err
}
defer session.Close()

operationID, err := session.Send(ctx, sdk.Operation{Command: &sdk.Command{Argv: []string{"sh", "-c", "kubectl get pods -A"}}})
if err != nil {
    return err
}
_ = operationID // retain this ID to call session.Cancel when needed
for event := range session.Receive() {
    if event.Completed != nil || event.Failed != nil {
        break
    }
}
if err := session.Err(); err != nil {
    return err
}
```

File listings are explicitly paginated. Continue while `NextPageToken` is not
empty; a listing is not a filesystem snapshot, so restart from an empty token
when a fresh view is required:

```go
page, err := sandbox.ListFiles(ctx, sdk.ListFilesOptions{Directory: "output", Limit: 100})
if err != nil {
    return err
}
for {
    for _, entry := range page.Entries {
        fmt.Println(entry.Path)
    }
    if page.NextPageToken == "" {
        break
    }
    page, err = sandbox.ListFiles(ctx, sdk.ListFilesOptions{
        Directory: "output", Limit: 100, PageToken: page.NextPageToken,
    })
    if err != nil {
        return err
    }
}
```

Session operations and Session file access use the Kubernetes aggregated Run
extension API (`sessions.kruntimes.io/v1alpha1`) through the caller's kubeconfig
credentials. The SDK does not use `Run.status.endpoint`, a Console port-forward,
or any Runtime Pod port-forward. API-server authentication applies as usual:
bearer tokens, client certificates, token files, and exec credentials.

The caller needs `get` on the named `kruntimes.io/runs` resource to read the Run
and `get` on `sessions.kruntimes.io/runs/session` for Session operations and
file access.

The SDK never retries commands, file mutations, or `Close` after a transport
failure. Refresh the Run and read structured owner-runtimed logs to determine
the outcome.

When `AcquireOptions.Session.LeaseTimeoutSeconds` is set, an open Session
maintains its lease internally. Closing the connection stops heartbeats but
does not release the Sandbox; call `Release` to return Runtime capacity.

`Release` drains the Session Run, waits for `Succeeded`, then deletes it to
return Runtime capacity. `Close` returns successfully only after the Run
reaches `Succeeded`; `Cancel` returns successfully only after `Cancelled`.
Any other terminal phase is a typed `StateError` that retains the current Run.

The caller does not need `pods`, `pods/portforward`, `pods/log`, or Console
access to operate a Sandbox. Run logs still use the aggregated Run-log API (or a
custom `LogReader`).
