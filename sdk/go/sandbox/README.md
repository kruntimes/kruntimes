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

`Session.Exec` submits one operation, streams its events, and returns the
terminal result:

```go
result, err := session.Exec(ctx, sdk.Operation{Command: &sdk.Command{Argv: []string{"sh", "-c", "kubectl get pods -A"}}}, &sdk.ExecOptions{
    OnEvent: func(event sdk.OperationEvent) error {
        // accepted, output, progress, completed, and failed events in order
        return nil
    },
})
if err != nil {
    var failed *sdk.OperationFailedError
    if errors.As(err, &failed) {
        return fmt.Errorf("command failed (%d): %s", failed.Code, failed.Message)
    }
    return err
}
if result.Command.TimedOut {
    return errors.New("command timed out")
}
if result.Command.ExitCode != 0 {
    return fmt.Errorf("command exited with status %d", result.Command.ExitCode)
}
```

A non-zero exit code or a timed-out command is a terminal result, not an error;
a `failed` event is an `*OperationFailedError` and a broken stream is a
`*TransportError`. A canceled context returns an error matching
`errors.Is(err, context.Canceled)`. `Exec` sends the cancel frame before
returning on cancellation, never closes the Session Run, and never replays an
operation. Use `Send`/`Receive` directly for interactive multi-operation work.

For a single command or file mutation, `Sandbox` provides one-shot helpers that
open their own short-lived Session connection, submit exactly one operation, and
close that connection. They never release the Sandbox, and they never replay an
operation after an uncertain transport outcome:

```go
result, err := sandbox.RunCommand(ctx, sdk.Command{Argv: []string{"sh", "-c", "git status"}}, &sdk.CommandOptions{
    OnStdout: func(data []byte) error { _, err := os.Stdout.Write(data); return err },
    OnStderr: func(data []byte) error { _, err := os.Stderr.Write(data); return err },
    OnProgress: func(event sdk.OperationProgress) error {
        // Runtime-defined progress; OnStdout/OnStderr cover command output.
        return nil
    },
})
if err != nil {
    return err
}

if err := sandbox.WriteFile(ctx, "report.md", []byte("# Report\n"), &sdk.WriteFileOptions{CreateParents: true}); err != nil {
    return err
}
if err := sandbox.CreateDirectory(ctx, "evidence"); err != nil {
    return err
}
if err := sandbox.RenameFile(ctx, "report.md", "evidence/report.md", &sdk.RenameFileOptions{Overwrite: true}); err != nil {
    return err
}
if err := sandbox.DeleteFile(ctx, "evidence", &sdk.DeleteFileOptions{Recursive: true}); err != nil {
    return err
}
```

`RunCommand` returns the same terminal contract as `Session.Exec`: a non-zero
exit code or a timed-out command is a `CommandResult`, not an error, while a
`failed` event returns `*OperationFailedError`, a broken stream returns
`*TransportError`, and a canceled context returns an error matching
`errors.Is(err, context.Canceled)`. The file helpers return those same typed
errors on failure. A callback that returns an error stops the operation and is
returned from `RunCommand`. Helpers close only their own connection; call
`Release` to return Runtime capacity.

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

The caller does not need `pods`, `pods/portforward`, or Console access to
operate a Sandbox. `Sandbox.Logs` reads the aggregated
`logs.kruntimes.io/runs/log` subresource, so grant `get` on that subresource for
the named Run; it never requests `pods/log`.
