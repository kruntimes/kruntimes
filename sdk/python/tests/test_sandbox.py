import base64
import json
import threading
import time
import unittest
from types import SimpleNamespace
from unittest import mock

from kruntimes.sandbox import (
    APIError,
    Command,
    CommandResult,
    DirectoryCreate,
    FileDelete,
    FileRename,
    FileWrite,
    Operation,
    OperationCanceledError,
    OperationEvent,
    OperationFailedError,
    OperationFailure,
    OperationOutput,
    OperationProgress,
    AcquireOptions,
    HTTPResponse,
    ListFilesOptions,
    SandboxClient,
    SandboxStateError,
    SessionTransportError,
)
from kruntimes.kubernetes import UrllibAPITransport


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


class FakeAPI:
    def __init__(self):
        self.requests = []
        self.response = HTTPResponse(200, b"{}")
        self.connection = None
        self.connections = []

    def request(self, method, url, body, headers):
        self.requests.append((method, url, body, headers))
        return self.response

    def open_session(self, url, headers, timeout_seconds):
        self.requests.append(("WS", url, timeout_seconds, headers))
        if self.connections:
            return self.connections.pop(0)
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
        frame = self.frames.pop(0)
        if isinstance(frame, BaseException):
            raise frame
        return json.dumps(frame)

    def close(self):
        self.closed = True


def sandbox_client(runs, transport, **kwargs):
    return SandboxClient(runs, transport, api_server="https://kube.example", **kwargs)


def ready_run():
    return {
        "metadata": {"name": "sandbox", "namespace": "default", "uid": "run-uid"},
        "spec": {"mode": {"session": {}}},
        "status": {
            "phase": "Ready",
            "assignedPod": "runtime-pod",
        },
    }


class SandboxTests(unittest.TestCase):
    def test_session_timeout_only_bounds_upgrade(self):
        connection = mock.Mock()
        dial = mock.Mock(return_value=connection)
        transport = UrllibAPITransport(api_server="https://kube.example")
        with mock.patch.dict("sys.modules", {"websocket": SimpleNamespace(create_connection=dial)}):
            self.assertIs(connection, transport.open_session("wss://kube.example/session", {}, 3))
        self.assertEqual(3, dial.call_args.kwargs["timeout"])
        connection.settimeout.assert_called_once_with(None)

    def test_transport_uses_kubernetes_tls_context(self):
        connection = mock.Mock()
        context = mock.Mock()
        dial = mock.Mock(return_value=connection)
        transport = UrllibAPITransport(api_server="https://kube.example", ssl_context=context)
        with mock.patch.dict("sys.modules", {"websocket": SimpleNamespace(create_connection=dial)}):
            session = transport.open_session("wss://kube.example/session", {}, 3)
        self.assertIs(context, dial.call_args.kwargs["sslopt"]["context"])
        self.assertIs(connection, session)
        connection.settimeout.assert_called_once_with(None)

    def test_acquire_sorts_environment(self):
        runs = FakeRuns(ready_run())
        api = FakeAPI()
        client = sandbox_client(runs, api, bearer_token="token")
        sandbox = client.runtime("default", "python").acquire_sandbox(AcquireOptions(name="sandbox", env={"B": "2", "A": "1"}))
        self.assertEqual([{"name": "A", "value": "1"}, {"name": "B", "value": "2"}], runs.created[1]["spec"]["env"])

    def test_failed_acquire_deletes_created_run(self):
        run = ready_run()
        run["status"]["phase"] = "Failed"
        runs = FakeRuns(run)
        with self.assertRaises(SandboxStateError):
            sandbox_client(runs, FakeAPI()).runtime("default", "python").acquire_sandbox(AcquireOptions(name="sandbox"))
        self.assertEqual(("default", "sandbox"), runs.deleted)

    def test_release_deletes_closed_sandbox(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        runs = FakeRuns(run)
        sandbox_client(runs, FakeAPI()).open("default", "sandbox").release()
        self.assertEqual(("default", "sandbox"), runs.deleted)

    def test_session_sends_receives_and_closes_without_releasing_sandbox(self):
        api = FakeAPI()
        connection = FakeSessionConnection([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "output", "output": {"stdout": "b2s="}},
            {"sequence": 3, "type": "completed", "completed": {"exitCode": 0}},
        ])
        api.connection = connection
        runs = FakeRuns(ready_run())
        sandbox = sandbox_client(runs, api, bearer_token="token").open("default", "sandbox")
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
        self.assertEqual("Bearer token", api.requests[0][3]["Authorization"])
        self.assertTrue(
            api.requests[0][1].startswith(
                "wss://kube.example/apis/sessions.kruntimes.io/v1alpha1/namespaces/default/runs/sandbox/session"
            )
        )

    def test_session_cancels_only_active_operation(self):
        api = FakeAPI()
        connection = FakeSessionConnection([{"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}}])
        api.connection = connection
        session = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox").open_session()
        operation_id = session.send(Operation(command=Command(shell="sleep 60")))
        session.cancel(operation_id)

        self.assertEqual({"type": "cancel", "operationID": "operation-1"}, connection.sent[1])
        with self.assertRaises(RuntimeError):
            session.cancel("operation-2")

    def test_session_error_frame_clears_active_operation(self):
        api = FakeAPI()
        api.connection = FakeSessionConnection([
            {"type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"type": "error", "error": "admission rejected"},
            {"type": "accepted", "accepted": {"operationID": "operation-2"}},
        ])
        session = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox").open_session()
        session.send(Operation(command=Command(shell="true")))
        with self.assertRaisesRegex(RuntimeError, "admission rejected"):
            session.receive()
        self.assertEqual("operation-2", session.send(Operation(command=Command(shell="true"))))

    def test_session_maintains_configured_lease_heartbeat(self):
        api = FakeAPI()
        connection = FakeSessionConnection([])
        api.connection = connection
        run = ready_run()
        run["spec"]["mode"]["session"] = {"leaseTimeoutSeconds": 1}
        session = sandbox_client(FakeRuns(run), api).open("default", "sandbox").open_session()
        try:
            deadline = time.monotonic() + 1
            while time.monotonic() < deadline and not connection.sent:
                time.sleep(0.01)
            self.assertEqual({"type": "heartbeat"}, connection.sent[0])
        finally:
            session.close()

    def test_read_file_preserves_path_for_runtime_boundary_validation(self):
        api = FakeAPI()
        api.response = HTTPResponse(200, b'{"contents":"b2s=","truncated":false}')
        sandbox = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox")

        contents, truncated = sandbox.read_file("../outside.txt")

        self.assertEqual(b"ok", contents)
        self.assertFalse(truncated)
        self.assertTrue(api.requests[0][1].endswith("/files/../outside.txt"))

    def test_list_files_uses_explicit_page_options(self):
        api = FakeAPI()
        api.response = HTTPResponse(200, b'{"entries":[{"path":"build.log","sizeBytes":12}],"nextPageToken":"next"}')
        sandbox = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox")

        page = sandbox.list_files(ListFilesOptions(directory="notes", limit=2, page_token="after-notes"))

        self.assertEqual(["build.log"], [entry.path for entry in page.entries])
        self.assertEqual("next", page.next_page_token)
        self.assertTrue(api.requests[0][1].endswith("/files?path=notes&limit=2&pageToken=after-notes"))
        with self.assertRaises(ValueError):
            sandbox.list_files(ListFilesOptions(limit=-1))

    def test_logs_use_aggregated_api(self):
        api = FakeAPI()
        api.response = HTTPResponse(200, (
            b'{"stream":"audit","message":"command","operation":"command","outcome":"succeeded","exitCode":0,"durationMilliseconds":12}\n'
            b'{"stream":"stdout","message":"hello"}\n'
        ))
        run = ready_run()
        run["status"]["assignedPodUID"] = "pod-uid"
        sandbox = sandbox_client(FakeRuns(run), api).open("default", "sandbox")

        lines = sandbox.logs(tail_lines=50)

        self.assertEqual(2, len(lines))
        self.assertEqual("audit", lines[0].stream)
        self.assertEqual("command", lines[0].message)
        self.assertEqual("run-uid", lines[0].run_uid)
        self.assertEqual("pod-uid", lines[0].assigned_pod_uid)
        self.assertEqual(0, lines[0].exit_code)
        self.assertEqual("hello", lines[1].message)
        self.assertIn("/apis/logs.kruntimes.io/v1alpha1/namespaces/default/runs/sandbox/log?tailLines=50", api.requests[0][1])

    def test_open_rejects_non_session_run(self):
        run = ready_run()
        run["spec"] = {"mode": {"task": {}}}
        with self.assertRaises(SandboxStateError):
            sandbox_client(FakeRuns(run), FakeAPI()).open("default", "sandbox")

    def test_api_error_is_typed(self):
        api = FakeAPI()
        api.response = HTTPResponse(403, b'{"error":"forbidden"}')
        sandbox = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox")
        with self.assertRaises(APIError) as error:
            sandbox.read_file("forbidden")
        self.assertEqual(403, error.exception.status_code)

    def test_close_requests_drain(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        runs = FakeRuns(run)
        sandbox = sandbox_client(runs, FakeAPI()).open("default", "sandbox")

        sandbox.close()

        self.assertEqual("Drain", runs.run["spec"]["termination"]["mode"])

    def test_cancel_requests_immediate_and_escalates_drain(self):
        run = ready_run()
        run["status"]["phase"] = "Cancelled"
        run["spec"]["termination"] = {"mode": "Drain"}
        runs = FakeRuns(run)
        sandbox = sandbox_client(runs, FakeAPI()).open("default", "sandbox")

        sandbox.cancel()

        self.assertEqual("Immediate", runs.run["spec"]["termination"]["mode"])

    def test_close_does_not_downgrade_immediate_termination(self):
        run = ready_run()
        run["status"]["phase"] = "Succeeded"
        run["spec"]["termination"] = {"mode": "Immediate"}
        runs = FakeRuns(run)
        sandbox = sandbox_client(runs, FakeAPI()).open("default", "sandbox")

        sandbox.close()

        self.assertEqual("Immediate", runs.run["spec"]["termination"]["mode"])

    def test_termination_reports_unexpected_terminal_phase(self):
        failed = ready_run()
        failed["status"]["phase"] = "Failed"
        with self.assertRaises(SandboxStateError):
            sandbox_client(FakeRuns(failed), FakeAPI()).open("default", "sandbox").close()

        succeeded = ready_run()
        succeeded["status"]["phase"] = "Succeeded"
        with self.assertRaises(SandboxStateError):
            sandbox_client(FakeRuns(succeeded), FakeAPI()).open("default", "sandbox").cancel()

    def _one_shot(self, frames):
        api = FakeAPI()
        connection = FakeSessionConnection(frames)
        api.connection = connection
        runs = FakeRuns(ready_run())
        sandbox = sandbox_client(runs, api).open("default", "sandbox")
        return api, connection, runs, sandbox

    def test_run_command_streams_output_progress_and_result(self):
        stdout_chunks = []
        stderr_chunks = []
        progress_events = []
        _, connection, runs, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "output", "output": {"stream": "stdout", "data": base64.b64encode(b"hello ").decode()}},
            {"sequence": 3, "type": "output", "output": {"stream": "stdout", "data": base64.b64encode(b"world").decode()}},
            {"sequence": 4, "type": "output", "output": {"stream": "stderr", "data": base64.b64encode(b"oops").decode()}},
            {"sequence": 5, "type": "progress", "progress": {
                "kind": "tool_call_started", "message": "running", "toolCallID": "call-1",
                "toolName": "kubectl", "data": base64.b64encode(b'{"x":1}').decode(),
                "contentType": "application/json",
            }},
            {"sequence": 6, "type": "completed", "completed": {"command": {"exitCode": 0}}},
        ])

        result = sandbox.run_command(
            Command(argv=["kubectl", "get", "pods"]),
            on_stdout=stdout_chunks.append,
            on_stderr=stderr_chunks.append,
            on_progress=progress_events.append,
        )

        self.assertEqual([b"hello ", b"world"], stdout_chunks)
        self.assertEqual([b"oops"], stderr_chunks)
        self.assertEqual(["tool_call_started"], [value.kind for value in progress_events])
        self.assertEqual("call-1", progress_events[0].tool_call_id)
        self.assertEqual("kubectl", progress_events[0].tool_name)
        self.assertEqual(b'{"x":1}', progress_events[0].data)
        self.assertEqual("application/json", progress_events[0].content_type)
        self.assertEqual(0, result.exit_code)
        self.assertEqual(b"hello world", result.stdout)
        self.assertEqual(b"oops", result.stderr)
        self.assertFalse(result.timed_out)
        self.assertEqual(1, len(connection.sent))
        self.assertEqual(["kubectl", "get", "pods"], connection.sent[0]["operation"]["command"]["argv"])
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_run_command_nonzero_exit_and_timeout_are_results(self):
        _, _, _, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "completed", "completed": {"command": {"exitCode": 9, "timedOut": True}}},
        ])

        result = sandbox.run_command(["false"])

        self.assertEqual(9, result.exit_code)
        self.assertTrue(result.timed_out)

    def test_run_command_accepts_shell_string_and_argv_sequence(self):
        api = FakeAPI()
        shell_connection = FakeSessionConnection([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "completed", "completed": {"command": {"exitCode": 0}}},
        ])
        argv_connection = FakeSessionConnection([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-2"}},
            {"sequence": 2, "type": "completed", "completed": {"command": {"exitCode": 0}}},
        ])
        api.connections = [shell_connection, argv_connection]
        sandbox = sandbox_client(FakeRuns(ready_run()), api).open("default", "sandbox")

        sandbox.run_command("echo $HOME")
        sandbox.run_command(["echo", "ok"])

        self.assertEqual("echo $HOME", shell_connection.sent[0]["operation"]["command"]["shell"])
        self.assertEqual([], shell_connection.sent[0]["operation"]["command"]["argv"])
        self.assertEqual(["echo", "ok"], argv_connection.sent[0]["operation"]["command"]["argv"])
        self.assertEqual("", argv_connection.sent[0]["operation"]["command"]["shell"])
        with self.assertRaises(TypeError):
            sandbox.run_command(42)

    def test_run_command_failed_event_raises_operation_failed(self):
        _, connection, runs, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "failed", "failed": {"code": 13, "message": "boom"}},
        ])

        with self.assertRaises(OperationFailedError) as error:
            sandbox.run_command(Command(shell="false"))

        self.assertEqual(13, error.exception.code)
        self.assertEqual("boom", error.exception.message)
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_run_command_preset_cancel_event_sends_cancel_and_raises_canceled(self):
        cancel_event = threading.Event()
        cancel_event.set()
        _, connection, runs, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "failed", "failed": {"code": 1, "message": "canceled"}},
        ])

        with self.assertRaises(OperationCanceledError):
            sandbox.run_command(Command(shell="sleep 60"), cancel_event=cancel_event)

        self.assertEqual(["send", "cancel"], [frame["type"] for frame in connection.sent])
        self.assertEqual("operation-1", connection.sent[1]["operationID"])
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_run_command_cancel_event_during_stream_sends_one_cancel_frame(self):
        cancel_event = threading.Event()
        _, connection, _, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "output", "output": {"stream": "stdout", "data": base64.b64encode(b"tick").decode()}},
            {"sequence": 3, "type": "failed", "failed": {"code": 1, "message": "canceled"}},
        ])

        with self.assertRaises(OperationCanceledError):
            sandbox.run_command(
                Command(shell="sleep 60"),
                on_stdout=lambda chunk: cancel_event.set(),
                cancel_event=cancel_event,
            )

        cancel_frames = [frame for frame in connection.sent if frame["type"] == "cancel"]
        self.assertEqual(1, len(cancel_frames))
        self.assertEqual("operation-1", cancel_frames[0]["operationID"])
        self.assertTrue(connection.closed)

    def test_run_command_transport_failure_after_accepted_is_not_replayed(self):
        _, connection, runs, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            RuntimeError("connection reset"),
        ])

        with self.assertRaises(SessionTransportError) as error:
            sandbox.run_command(Command(shell="true"))

        self.assertIsInstance(error.exception.__cause__, RuntimeError)
        self.assertIs(error.exception.cause, error.exception.__cause__)
        self.assertEqual(1, len(connection.sent))
        self.assertEqual("send", connection.sent[0]["type"])
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_run_command_rejects_not_ready_without_opening_session(self):
        run = ready_run()
        run["status"]["phase"] = "Failed"
        api = FakeAPI()
        sandbox = sandbox_client(FakeRuns(run), api).open("default", "sandbox")

        with self.assertRaises(SandboxStateError):
            sandbox.run_command("true")

        self.assertEqual([], [request for request in api.requests if request[0] == "WS"])

    def test_file_helpers_send_typed_bodies_and_return_none(self):
        def completed_connection():
            return FakeSessionConnection([
                {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
                {"sequence": 2, "type": "completed", "completed": {}},
            ])

        api = FakeAPI()
        connections = [completed_connection() for _ in range(4)]
        api.connections = list(connections)
        runs = FakeRuns(ready_run())
        sandbox = sandbox_client(runs, api).open("default", "sandbox")

        self.assertIsNone(sandbox.write_file("notes/a.txt", b"hi", create_parents=True))
        self.assertIsNone(sandbox.create_directory("notes"))
        self.assertIsNone(sandbox.delete_file("notes/a.txt", recursive=True))
        self.assertIsNone(sandbox.rename_file("notes/a.txt", "notes/b.txt", overwrite=True))

        write, create, delete, rename = connections
        self.assertEqual(
            {"writeFile": {"path": "notes/a.txt", "contents": base64.b64encode(b"hi").decode(), "createParents": True}},
            write.sent[0]["operation"],
        )
        self.assertEqual({"createDirectory": {"path": "notes"}}, create.sent[0]["operation"])
        self.assertEqual({"deleteFile": {"path": "notes/a.txt", "recursive": True}}, delete.sent[0]["operation"])
        self.assertEqual(
            {"renameFile": {"sourcePath": "notes/a.txt", "destinationPath": "notes/b.txt", "overwrite": True}},
            rename.sent[0]["operation"],
        )
        for connection in connections:
            self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_file_helper_failed_event_raises_operation_failed(self):
        _, connection, runs, sandbox = self._one_shot([
            {"sequence": 1, "type": "accepted", "accepted": {"operationID": "operation-1"}},
            {"sequence": 2, "type": "failed", "failed": {"code": 7, "message": "denied"}},
        ])

        with self.assertRaises(OperationFailedError) as error:
            sandbox.delete_file("missing")

        self.assertEqual(7, error.exception.code)
        self.assertEqual("denied", error.exception.message)
        self.assertTrue(connection.closed)
        self.assertIsNone(runs.deleted)

    def test_operation_event_typed_accessors(self):
        output = OperationEvent(2, "output", {
            "output": {"stream": "stdout", "data": base64.b64encode(b"hi").decode()},
        })
        self.assertEqual(OperationOutput("stdout", b"hi"), output.output)
        self.assertIsNone(output.progress)
        self.assertIsNone(output.command)

        progress = OperationEvent(3, "progress", {"progress": {
            "kind": "status", "message": "m", "toolCallID": "id", "toolName": "name",
            "data": base64.b64encode(b'{"a":1}').decode(), "contentType": "application/json",
        }})
        self.assertEqual(
            OperationProgress("status", "m", "id", "name", b'{"a":1}', "application/json"),
            progress.progress,
        )

        failed = OperationEvent(4, "failed", {"failed": {"code": 13, "message": "boom"}})
        self.assertEqual(OperationFailure(13, "boom"), failed.failed)
        self.assertIsNone(failed.command)

        completed = OperationEvent(5, "completed", {"completed": {"command": {
            "exitCode": 2,
            "stdout": base64.b64encode(b"out").decode(),
            "stderr": base64.b64encode(b"err").decode(),
            "timedOut": True,
        }}})
        self.assertEqual(CommandResult(2, b"out", b"err", True), completed.command)
        self.assertIsNone(completed.output)

    def test_operation_still_accepts_raw_file_mapping(self):
        body = {"path": "a.txt", "contents": base64.b64encode(b"hi").decode(), "createParents": True}

        self.assertEqual({"writeFile": body}, Operation(write_file=body).request_body())
        self.assertEqual(
            {"writeFile": FileWrite("a.txt", b"hi", True).request_body()},
            Operation(write_file=FileWrite("a.txt", b"hi", True)).request_body(),
        )
        self.assertEqual(
            {"createDirectory": DirectoryCreate("d").request_body()},
            Operation(create_directory=DirectoryCreate("d")).request_body(),
        )
        self.assertEqual(
            {"deleteFile": FileDelete("f", True).request_body()},
            Operation(delete_file=FileDelete("f", True)).request_body(),
        )
        self.assertEqual(
            {"renameFile": FileRename("a", "b", True).request_body()},
            Operation(rename_file=FileRename("a", "b", True)).request_body(),
        )



if __name__ == "__main__":
    unittest.main()
