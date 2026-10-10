import json
import base64
import time
import unittest
from types import SimpleNamespace
from unittest import mock

from kruntimes.sandbox import (
    APIError,
    Command,
    Operation,
    AcquireOptions,
    HTTPResponse,
    ListFilesOptions,
    SandboxClient,
    SandboxStateError,
)
from kruntimes.kubernetes import PortForwardGatewayTransport, UrllibGatewayTransport


class FakeRuns:
    def __init__(self, run):
        self.run = run
        self.created = None
        self.deleted = None

    def create(self, namespace, run):
        self.created = (namespace, run)
        metadata = run["metadata"]
        self.run["metadata"]["name"] = metadata.get("name", "sandbox")
        self.run["metadata"]["namespace"] = metadata["namespace"]
        return self.run

    def get(self, namespace, name):
        return self.run

    def replace(self, namespace, name, run):
        self.run = run
        return run

    def delete(self, namespace, name):
        self.deleted = (namespace, name)


class FakeGateway:
    def __init__(self):
        self.requests = []
        self.response = HTTPResponse(200, b"{}")
        self.connection = None

    def request(self, method, url, body, headers):
        self.requests.append((method, url, body, headers))
        return self.response

    def open_session(self, url, headers, timeout_seconds):
        self.requests.append(("WS", url, timeout_seconds, headers))
        if self.connection is None:
            raise AssertionError("test did not configure a Session connection")
        return self.connection


class FakeSessionConnection:
    def __init__(self, frames):
        self.frames = list(frames)
        self.sent = []
        self.closed = False

    def send(self, payload):
        self.sent.append(json.loads(payload))

    def recv(self):
        if not self.frames:
            raise AssertionError("unexpected Session receive")
        return json.dumps(self.frames.pop(0))

    def close(self):
        self.closed = True


class FakeLogs:
    def read(self, namespace, pod, container):
        return '{"run_uid":"run-uid","stream":"audit","message":"command"}\n{"run_uid":"other","stream":"audit","message":"ignored"}'


def ready_run():
    return {
        "metadata": {"name": "sandbox", "namespace": "default", "uid": "run-uid"},
        "spec": {"mode": {"session": {}}},
        "status": {
            "phase": "Ready",
            "assignedPod": "runtime-pod",
            "endpoint": {"url": "https://gateway/v1/namespaces/default/runtimes/python/sessions/run-uid"},
        },
    }


class SandboxTests(unittest.TestCase):
    def test_session_timeout_only_bounds_upgrade(self):
        connection = mock.Mock()
        dial = mock.Mock(return_value=connection)
        with mock.patch.dict("sys.modules", {"websocket": SimpleNamespace(create_connection=dial)}):
            self.assertIs(connection, UrllibGatewayTransport().open_session("ws://gateway/session", {}, 3))
        self.assertEqual(3, dial.call_args.kwargs["timeout"])
        connection.settimeout.assert_called_once_with(None)

    def test_session_uses_run_endpoint_ca_bundle(self):
        run = ready_run()
        run["status"]["endpoint"]["caBundle"] = base64.b64encode(b"test CA").decode()
        connection = mock.Mock()
        context = mock.Mock()
        dial = mock.Mock(return_value=connection)
        with mock.patch.dict("sys.modules", {"websocket": SimpleNamespace(create_connection=dial)}), \
                mock.patch("ssl.create_default_context", return_value=context) as create_context:
            sandbox = SandboxClient(FakeRuns(run), UrllibGatewayTransport()).open("default", "sandbox")
            session = sandbox.open_session(timeout_seconds=3)
        create_context.assert_called_once_with(cadata="test CA")
        self.assertIs(context, dial.call_args.kwargs["sslopt"]["context"])
        session.close()

    def test_acquire_sorts_environment(self):
        runs = FakeRuns(ready_run())
        gateway = FakeGateway()
        client = SandboxClient(runs, gateway, bearer_token="token")
        sandbox = client.runtime("default", "python").acquire_sandbox(AcquireOptions(name="sandbox", env={"B": "2", "A": "1"}))
        self.assertEqual([{"name": "A", "value": "1"}, {"name": "B", "value": "2"}], runs.created[1]["spec"]["env"])

    def test_failed_acquire_deletes_created_run(self):
        run = ready_run()
        run["status"]["phase"] = "Failed"
        runs = FakeRuns(run)
        with self.assertRaises(SandboxStateError):
            SandboxClient(runs, FakeGateway()).runtime("default", "python").acquire_sandbox(AcquireOptions(name="sandbox"))
        self.assertEqual(("default", "sandbox"), runs.deleted)

    def test_release_deletes_closed_sandbox(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        runs = FakeRuns(run)
        SandboxClient(runs, FakeGateway()).open("default", "sandbox").release()
        self.assertEqual(("default", "sandbox"), runs.deleted)

    def test_session_sends_receives_and_closes_without_releasing_sandbox(self):
        gateway = FakeGateway()
        connection = FakeSessionConnection([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "output", "output": {"stdout": "b2s="}},
            {"sequence": 3, "type": "completed", "completed": {"exitCode": 0}},
        ])
        gateway.connection = connection
        runs = FakeRuns(ready_run())
        sandbox = SandboxClient(runs, gateway, bearer_token="token").open("default", "sandbox")
        session = sandbox.open_session(timeout_seconds=3)

        operation_id = session.send(Operation(command=Command(argv=["echo", "ok"])), idempotency_key="operation-1")
        output = session.receive()
        completed = session.receive()
        session.close()

        self.assertEqual("operation-1", operation_id)
        self.assertEqual("output", output.type)
        self.assertEqual("completed", completed.type)
        self.assertEqual("send", connection.sent[0]["type"])
        self.assertEqual("operation-1", connection.sent[0]["idempotencyKey"])
        self.assertEqual(["echo", "ok"], connection.sent[0]["operation"]["command"]["argv"])
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)
        self.assertEqual("Bearer token", gateway.requests[0][3]["Authorization"])
        self.assertTrue(gateway.requests[0][1].startswith("wss://gateway/"))

    def test_session_cancels_only_active_operation(self):
        gateway = FakeGateway()
        connection = FakeSessionConnection([{"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}}])
        gateway.connection = connection
        session = SandboxClient(FakeRuns(ready_run()), gateway).open("default", "sandbox").open_session()
        operation_id = session.send(Operation(command=Command(shell="sleep 60")))
        session.cancel(operation_id)

        self.assertEqual({"type": "cancel", "operationID": "operation-1"}, connection.sent[1])
        with self.assertRaises(RuntimeError):
            session.cancel("operation-2")

    def test_session_error_frame_clears_active_operation(self):
        gateway = FakeGateway()
        gateway.connection = FakeSessionConnection([
            {"type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"type": "error", "error": "admission rejected"},
            {"type": "accepted", "accepted": {"operationID": "operation-2"}},
        ])
        session = SandboxClient(FakeRuns(ready_run()), gateway).open("default", "sandbox").open_session()
        session.send(Operation(command=Command(shell="true")))
        with self.assertRaisesRegex(RuntimeError, "admission rejected"):
            session.receive()
        self.assertEqual("operation-2", session.send(Operation(command=Command(shell="true"))))

    def test_session_maintains_configured_lease_heartbeat(self):
        gateway = FakeGateway()
        connection = FakeSessionConnection([])
        gateway.connection = connection
        run = ready_run()
        run["spec"]["mode"]["session"] = {"leaseTimeoutSeconds": 1}
        session = SandboxClient(FakeRuns(run), gateway).open("default", "sandbox").open_session()
        try:
            deadline = time.monotonic() + 1
            while time.monotonic() < deadline and not connection.sent:
                time.sleep(0.01)
            self.assertEqual({"type": "heartbeat"}, connection.sent[0])
        finally:
            session.close()

    def test_read_file_preserves_path_for_runtime_boundary_validation(self):
        gateway = FakeGateway()
        gateway.response = HTTPResponse(200, b'{"contents":"b2s=","truncated":false}')
        sandbox = SandboxClient(FakeRuns(ready_run()), gateway).open("default", "sandbox")

        contents, truncated = sandbox.read_file("../outside.txt")

        self.assertEqual(b"ok", contents)
        self.assertFalse(truncated)
        self.assertTrue(gateway.requests[0][1].endswith("/files/../outside.txt"))

    def test_list_files_uses_explicit_page_options(self):
        gateway = FakeGateway()
        gateway.response = HTTPResponse(200, b'{"entries":[{"path":"build.log","sizeBytes":12}],"nextPageToken":"next"}')
        sandbox = SandboxClient(FakeRuns(ready_run()), gateway).open("default", "sandbox")

        page = sandbox.list_files(ListFilesOptions(directory="notes", limit=2, page_token="after-notes"))

        self.assertEqual(["build.log"], [entry.path for entry in page.entries])
        self.assertEqual("next", page.next_page_token)
        self.assertTrue(gateway.requests[0][1].endswith("/files?path=notes&limit=2&pageToken=after-notes"))
        with self.assertRaises(ValueError):
            sandbox.list_files(ListFilesOptions(limit=-1))

    def test_logs_filter_run_uid(self):
        sandbox = SandboxClient(FakeRuns(ready_run()), FakeGateway(), logs=FakeLogs()).open("default", "sandbox")
        lines = sandbox.logs()
        self.assertEqual(1, len(lines))
        self.assertEqual("command", lines[0].message)

    def test_open_rejects_non_session_run(self):
        run = ready_run()
        run["spec"] = {"mode": {"task": {}}}
        with self.assertRaises(SandboxStateError):
            SandboxClient(FakeRuns(run), FakeGateway()).open("default", "sandbox")

    def test_gateway_error_is_typed(self):
        gateway = FakeGateway()
        gateway.response = HTTPResponse(403, b'{"error":"forbidden"}')
        sandbox = SandboxClient(FakeRuns(ready_run()), gateway).open("default", "sandbox")
        with self.assertRaises(APIError) as error:
            sandbox.read_file("forbidden")
        self.assertEqual(403, error.exception.status_code)

    def test_close_requests_drain(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        runs = FakeRuns(run)
        sandbox = SandboxClient(runs, FakeGateway()).open("default", "sandbox")

        sandbox.close()

        self.assertEqual("Drain", runs.run["spec"]["termination"]["mode"])

    def test_cancel_requests_immediate_and_escalates_drain(self):
        run = ready_run()
        run["status"]["phase"] = "Cancelled"
        run["spec"]["termination"] = {"mode": "Drain"}
        runs = FakeRuns(run)
        sandbox = SandboxClient(runs, FakeGateway()).open("default", "sandbox")

        sandbox.cancel()

        self.assertEqual("Immediate", runs.run["spec"]["termination"]["mode"])

    def test_close_does_not_downgrade_immediate_termination(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        run["spec"]["termination"] = {"mode": "Immediate"}
        runs = FakeRuns(run)
        sandbox = SandboxClient(runs, FakeGateway()).open("default", "sandbox")

        sandbox.close()

        self.assertEqual("Immediate", runs.run["spec"]["termination"]["mode"])

    def test_termination_reports_unexpected_terminal_phase(self):
        failed = ready_run()
        failed["status"]["phase"] = "Failed"
        with self.assertRaises(SandboxStateError):
            SandboxClient(FakeRuns(failed), FakeGateway()).open("default", "sandbox").close()

        succeeded = ready_run()
        succeeded["status"]["phase"] = "Succeeded"
        with self.assertRaises(SandboxStateError):
            SandboxClient(FakeRuns(succeeded), FakeGateway()).open("default", "sandbox").cancel()

    def test_port_forward_preserves_endpoint_path(self):
        gateway = FakeGateway()
        transport = PortForwardGatewayTransport(gateway, "http://127.0.0.1:19090")

        transport.request("GET", "https://gateway/v1/namespaces/default/runtimes/python/sessions/run-uid/files?maxBytes=10", None, {})

        self.assertEqual(
            "http://127.0.0.1:19090/v1/namespaces/default/runtimes/python/sessions/run-uid/files?maxBytes=10",
            gateway.requests[0][1],
        )

    def test_port_forward_rewrites_websocket_endpoint(self):
        gateway = FakeGateway()
        gateway.connection = FakeSessionConnection([])
        transport = PortForwardGatewayTransport(gateway, "http://127.0.0.1:19090")

        connection = transport.open_session("wss://gateway/v1/namespaces/default/runtimes/python/sessions/run-uid/operations:ws", {}, 2)

        self.assertIs(connection, gateway.connection)
        self.assertEqual("ws://127.0.0.1:19090/v1/namespaces/default/runtimes/python/sessions/run-uid/operations:ws", gateway.requests[0][1])


if __name__ == "__main__":
    unittest.main()
