# kruntimes Python SDK

`kruntimes-sdk` creates and controls agent-facing Sandboxes backed by
Session-mode `Run` resources. A Sandbox is not another Kubernetes resource:
the `Run` lifecycle remains authoritative.

Install the Kubernetes adapters when the SDK manages Runs directly:

```bash
pip install 'kruntimes-sdk[kubernetes]'
```

In a Pod with a ServiceAccount that is authorized for the target `Run` and its
aggregated Session API subresource, construct the client directly:

```python
from kruntimes.kubernetes import from_incluster
from kruntimes.sandbox import AcquireOptions, Command, Operation

client = from_incluster()
sandbox = client.runtime("agents", "python-session").acquire_sandbox(AcquireOptions(
    name="diagnose-api",
), timeout_seconds=60)
try:
    session = sandbox.open_session()
    try:
        operation_id = session.send(Operation(command=Command(argv=["sh", "-c", "kubectl get pods -A"])))
        while True:
            event = session.receive()
            if event.type in ("completed", "failed"):
                break
    finally:
        session.close()
finally:
    sandbox.release(timeout_seconds=30)
```

## One-shot helpers

For a single operation, `Sandbox` provides helpers that open and close their
own short-lived Session connection. They validate that the Run is `Ready`,
submit exactly one operation, consume the ordered event stream, and close only
their connection; they never release or close the backing Sandbox:

```python
from kruntimes.sandbox import Command

result = sandbox.run_command(
    "kubectl get pods -A",
    on_stdout=lambda chunk: print(chunk.decode(), end=""),
    on_stderr=lambda chunk: print(chunk.decode(), end=""),
)
print(result.exit_code, result.timed_out)

sandbox.write_file("report.md", b"# Report\n", create_parents=True)
sandbox.create_directory("evidence")
sandbox.rename_file("report.md", "evidence/report.md", overwrite=True)
sandbox.delete_file("evidence", recursive=True)
```

`run_command` accepts a shell string, an argv sequence, or a `Command`. A
non-zero exit code or `timed_out` is a normal returned `CommandResult`, not an
exception. `on_progress` receives each typed `OperationProgress` event. Pass a
`threading.Event` as `cancel_event` to cancel the active operation: one cancel
frame is sent and the helper raises `OperationCanceledError`.

The helpers distinguish three failure modes:

- `OperationFailedError` — the Runtime Server reported a terminal `failed`
  event (`code`, `message`); the operation ran, so do not replay it.
- `SessionTransportError` — the connection or stream failed and the outcome is
  unknown; the helper never replays the operation.
- `OperationCanceledError` — the caller's `cancel_event` was set.

The typed request dataclasses `FileWrite`, `DirectoryCreate`, `FileDelete`, and
`FileRename` can also be passed to `Operation`; raw wire mappings still work.

File listings are explicitly paginated. Continue while `next_page_token` is
not empty; a listing is not a filesystem snapshot, so restart from an empty
token when a fresh view is required:

```python
from kruntimes.sandbox import ListFilesOptions

options = ListFilesOptions(directory="output", limit=100)
while True:
    page = sandbox.list_files(options)
    for entry in page.entries:
        print(entry.path)
    if not page.next_page_token:
        break
    options = ListFilesOptions(
        directory="output", limit=100, page_token=page.next_page_token,
    )
```

For local development, use the active kubeconfig directly. Session connections
and file requests go through kube-apiserver, which applies the kubeconfig CA,
client certificate, and bearer token; no Console or Runtime Pod port-forward is
needed:

```python
from kruntimes.kubernetes import from_kube_config

client = from_kube_config(context="kind-kruntimes-e2e")
sandbox = client.open("agents", "diagnose-api")  # reconnect to an existing Run; no allocation
```

`Session.send`, the one-shot helpers, file mutations, and `close()` are never
retried automatically. A transport failure has an unknown execution outcome;
refresh the Run and use the structured owner-runtimed logs to determine what
happened.

When the `session` mapping in `AcquireOptions` sets `leaseTimeoutSeconds`, an
open Session maintains that lease internally. Closing the connection stops
heartbeats but does not release the Sandbox; call `release()` to return Runtime
capacity.

`release()` drains the Session Run, waits for `Succeeded`, then deletes it to
return Runtime capacity. `close()` returns successfully only after the Run
reaches `Succeeded`; `cancel()` returns successfully only after `Cancelled`.
Any other terminal phase raises `SandboxStateError` with the current Run.

The caller needs `get` access to the target `Run`, `get` on
`sessions.kruntimes.io` `runs/session` for the Session connection and file
operations, and `get` on `logs.kruntimes.io` `runs/log` for sandbox logs. The
SDK never requests `pods` or `pods/log`.
