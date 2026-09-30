# kruntimes Python SDK

`kruntimes-sdk` creates and controls agent-facing Sandboxes backed by
Session-mode `Run` resources. A Sandbox is not another Kubernetes resource:
the `Run` lifecycle remains authoritative.

Install the Kubernetes adapters when the SDK manages Runs directly:

```bash
pip install 'kruntimes-sdk[kubernetes]'
```

In a Pod with a ServiceAccount that is authorized for the target `Run` and
Runtime gateway, construct the client directly:

```python
from kruntimes.kubernetes import from_incluster
from kruntimes.sandbox import AcquireOptions, Command

client = from_incluster()
sandbox = client.runtime("agents", "python-session").acquire_sandbox(AcquireOptions(
    name="diagnose-api",
), timeout_seconds=60)
try:
    session = sandbox.open_session()
    try:
        operation_id = session.send(Command(argv=["sh", "-c", "kubectl get pods -A"]))
        while True:
            event = session.receive()
            if event.type in ("completed", "failed"):
                break
    finally:
        session.close()
finally:
    sandbox.release(timeout_seconds=30)
```

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

For local development, forward only the shared Runtime gateway Service. The
forward preserves each Run endpoint path and does not expose runtimed or
Runtime Server gRPC ports:

```python
from kruntimes.kubernetes import PortForwardGatewayTransport, from_kube_config

with PortForwardGatewayTransport.start(
    namespace="kruntimes-system",
    service="kruntimes-gateway",
    service_port=80,
) as gateway:
    client = from_kube_config(gateway=gateway)
    sandbox = client.open("agents", "diagnose-api")  # reconnect to an existing Run; no allocation
```

`Session.send`, file mutations, and `close()` are never retried automatically. A
transport failure has an unknown execution outcome; refresh the Run and use
the structured owner-runtimed logs to determine what happened.

When the `session` mapping in `AcquireOptions` sets `leaseTimeoutSeconds`, an
open Session maintains that lease internally. Closing the connection stops
heartbeats but does not release the Sandbox; call `release()` to return Runtime
capacity.

`release()` drains the Session Run, waits for `Succeeded`, then deletes it to
return Runtime capacity. `close()` returns successfully only after the Run
reaches `Succeeded`; `cancel()` returns successfully only after `Cancelled`.
Any other terminal phase raises `SandboxStateError` with the current Run.

The local caller needs `get` access to the target Run for gateway
authorization. Starting the port-forward also needs `get` on the shared gateway
Service and `get`, `list` on its Pods in the gateway namespace.
