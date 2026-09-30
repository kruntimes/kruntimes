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

operationID, err := session.Send(ctx, sdk.Command{Argv: []string{"sh", "-c", "kubectl get pods -A"}})
if err != nil {
    return err
}
_ = operationID // retain this ID to call session.Cancel when needed
for {
    event, err := session.Receive(ctx)
    if err != nil {
        return err
    }
    if event.Completed != nil || event.Failed != nil {
        break
    }
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

For local development, forward only the shared Console Service. Pass
the returned forward as `Config.HTTPClient`; it preserves the endpoint path and
does not expose runtimed or Runtime Server gRPC ports:

```go
forward, err := sdk.StartConsolePortForward(
    ctx, restConfig, "kruntimes-system", "kruntimes-console", 443,
)
if err != nil {
    return err
}
defer forward.Close()

client, err := sdk.NewFromRESTConfig(restConfig, sdk.Config{HTTPClient: forward})
```

The same `ConsolePortForward` is used internally by `OpenSession`; callers do
not need to configure or expose a separate WebSocket endpoint.

The SDK never retries commands, file mutations, or `Close` after a transport
failure. Refresh the Run and read structured owner-runtimed logs to determine
the outcome.

When `Session.LeaseTimeoutSeconds` is set on `AcquireOptions`, an open Session
maintains its lease internally. Closing the connection stops heartbeats but
does not release the Sandbox; call `Release` to return Runtime capacity.

`Release` drains the Session Run, waits for `Succeeded`, then deletes it to
return Runtime capacity. `Close` returns successfully only after the Run
reaches `Succeeded`; `Cancel` returns successfully only after `Cancelled`.
Any other terminal phase is a typed `StateError` that retains the current Run.

The local caller needs `get` on the target Run for Console authorization. A
port-forward client also needs `get` on the shared Console Service and `get`,
`list` on its Pods in the Console namespace.
