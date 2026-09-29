# Go Sandbox SDK

`sandbox` is the Go client for Session-mode Runs. A Sandbox is backed by the
existing Kubernetes `Run` resource; it is not a second resource type.

In a cluster, create the client from the caller's Kubernetes REST config:

```go
client, err := sandbox.NewFromRESTConfig(restConfig, sandbox.Config{})
if err != nil {
    return err
}
session, err := client.Create(ctx, sandbox.CreateOptions{
    Namespace: "agents",
    Name:      "diagnose-api",
    Runtime:   "python-session",
})
if err != nil {
    return err
}
if err := session.Wait(ctx); err != nil {
    return err
}
result, err := session.Execute(ctx, sandbox.Command{Argv: []string{"sh", "-c", "kubectl get pods -A"}})
```

File listings are explicitly paginated. Continue while `NextPageToken` is not
empty; a listing is not a filesystem snapshot, so restart from an empty token
when a fresh view is required:

```go
page, err := session.ListFiles(ctx, sandbox.ListFilesOptions{Directory: "output", Limit: 100})
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
    page, err = session.ListFiles(ctx, sandbox.ListFilesOptions{
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
forward, err := sandbox.StartConsolePortForward(
    ctx, restConfig, "kruntimes-system", "kruntimes-console", 443,
)
if err != nil {
    return err
}
defer forward.Close()

client, err := sandbox.NewFromRESTConfig(restConfig, sandbox.Config{HTTPClient: forward})
```

The SDK never retries commands, file mutations, or `Close` after a transport
failure. Refresh the Run and read structured owner-runtimed logs to determine
the outcome.

`Close` returns successfully only after the Run reaches `Succeeded`; `Cancel`
returns successfully only after `Cancelled`. Any other terminal phase is a
typed `StateError` that retains the current Run.

The local caller needs `get` on the target Run for Console authorization. A
port-forward client also needs `get` on the shared Console Service and `get`,
`list` on its Pods in the Console namespace.
