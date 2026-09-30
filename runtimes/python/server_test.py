import json
import signal
import subprocess
import tempfile
import time
import unittest
from unittest import mock
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import grpc

from pb import runtime_pb2
from pb import runtime_pb2_grpc
from server import OUTPUT_TRUNCATED_MARKER, PythonRuntime


class TestPythonRuntime(unittest.TestCase):
    def setUp(self):
        self.work_dir = Path(tempfile.mkdtemp())
        self.server = grpc.server(ThreadPoolExecutor(max_workers=4))
        self.servicer = PythonRuntime(str(self.work_dir))
        runtime_pb2_grpc.add_RuntimeServicer_to_server(self.servicer, self.server)
        runtime_pb2_grpc.add_FunctionRuntimeServicer_to_server(
            self.servicer,
            self.server,
        )
        runtime_pb2_grpc.add_SessionRuntimeServicer_to_server(
            self.servicer,
            self.server,
        )
        port = self.server.add_insecure_port("localhost:0")
        self.server.start()
        self.channel = grpc.insecure_channel(f"localhost:{port}")
        self.stub = runtime_pb2_grpc.RuntimeStub(self.channel)
        self.function_stub = runtime_pb2_grpc.FunctionRuntimeStub(self.channel)
        self.session_stub = runtime_pb2_grpc.SessionRuntimeStub(self.channel)

    def tearDown(self):
        self.server.stop(0)
        import shutil
        shutil.rmtree(str(self.work_dir))

    def _wait(self, task_id, timeout=10):
        deadline = time.time() + timeout
        while time.time() < deadline:
            resp = self.stub.Status(runtime_pb2.StatusRequest(id=task_id))
            if resp.state in (
                runtime_pb2.EXECUTION_STATE_SUCCEEDED,
                runtime_pb2.EXECUTION_STATE_FAILED,
            ):
                return resp
            time.sleep(0.05)
        self.fail(f"timed out waiting for {task_id}")

    def _prepare_inline(self, code, filename="script"):
        td = Path(tempfile.mkdtemp(dir=str(self.work_dir)))
        (td / filename).write_text(code)
        return str(td)

    def _register_function(self, working_dir, run_uid="function-run", attempt=1,
                           digest="sha256:initial"):
        response = self.function_stub.RegisterFunction(
            runtime_pb2.RegisterFunctionRequest(
                run_uid=run_uid,
                registration_attempt=attempt,
                working_dir=working_dir,
                handler="app.handler",
                registration_digest=digest,
            )
        )
        self.assertEqual(
            response.state,
            runtime_pb2.FUNCTION_REGISTRATION_STATE_READY,
        )
        return response.registration

    def _register_session(self, working_dir, run_uid="session-run", pod_uid="pod-a", env=None):
        return self.session_stub.RegisterSession(
            runtime_pb2.RegisterSessionRequest(
                identity=runtime_pb2.SessionIdentity(
                    run_uid=run_uid,
                    assigned_pod_uid=pod_uid,
                ),
                working_dir=working_dir,
                env=env or {},
            )
        )

    def test_session_registration_status_close_and_assignment_fencing(self):
        session_dir = self._prepare_inline("# session")
        registered = self._register_session(session_dir)
        self.assertEqual(registered.state, runtime_pb2.SESSION_STATE_READY)
        self.assertGreater(registered.last_activity_unix_nano, 0)

        # The same Run assignment may be retried without creating new state.
        self._register_session(session_dir)
        with self.assertRaises(grpc.RpcError) as ctx:
            self._register_session(session_dir, pod_uid="pod-b")
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

        identity = runtime_pb2.SessionIdentity(
            run_uid="session-run",
            assigned_pod_uid="pod-a",
        )
        current = self.session_stub.GetSessionStatus(
            runtime_pb2.GetSessionStatusRequest(identity=identity)
        )
        self.assertEqual(current.state, runtime_pb2.SESSION_STATE_READY)
        lease_before = current.last_lease_heartbeat_unix_nano
        current = self.session_stub.TouchSession(
            runtime_pb2.TouchSessionRequest(identity=identity)
        )
        self.assertGreater(current.last_lease_heartbeat_unix_nano, lease_before)
        self.session_stub.CloseSession(runtime_pb2.CloseSessionRequest(identity=identity))
        self.session_stub.CloseSession(runtime_pb2.CloseSessionRequest(identity=identity))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.session_stub.GetSessionStatus(
                runtime_pb2.GetSessionStatusRequest(identity=identity)
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_session_registration_rejects_escaping_workspace(self):
        with self.assertRaises(grpc.RpcError) as ctx:
            self._register_session(tempfile.mkdtemp())
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    def test_session_executes_commands_and_confines_files(self):
        session_dir = self._prepare_inline("# session")
        self._register_session(
            session_dir, env={"KRUNTIMES_SESSION_DEFAULT": "session"}
        )
        identity = runtime_pb2.SessionIdentity(
            run_uid="session-run",
            assigned_pod_uid="pod-a",
        )
        with self.assertRaises(grpc.RpcError) as ctx:
            self.session_stub.ExecuteSessionOperation(
                runtime_pb2.ExecuteSessionOperationRequest(identity=identity)
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)
        self.session_stub.ExecuteSessionOperation(
            runtime_pb2.ExecuteSessionOperationRequest(
                identity=identity,
                create_directory=runtime_pb2.SessionDirectoryCreate(
                    path="scratch/nested",
                ),
            )
        )
        self.session_stub.ExecuteSessionOperation(
            runtime_pb2.ExecuteSessionOperationRequest(
                identity=identity,
                write_file=runtime_pb2.SessionFileWrite(
                    path="notes/hello.txt",
                    contents=b"hello\n",
                    create_parents=True,
                ),
            )
        )
        read = self.session_stub.ReadSessionFile(
            runtime_pb2.ReadSessionFileRequest(
                identity=identity,
                path="notes/hello.txt",
                max_bytes=1024,
            )
        )
        self.assertEqual(read.contents, b"hello\n")
        self.assertFalse(read.truncated)
        listed = self.session_stub.ListSessionFiles(
            runtime_pb2.ListSessionFilesRequest(identity=identity)
        )
        notes = next((entry for entry in listed.entries if entry.path == "notes"), None)
        self.assertIsNotNone(notes)
        self.assertTrue(notes.directory)
        operation = self.session_stub.ExecuteSessionOperation(
            runtime_pb2.ExecuteSessionOperationRequest(
                identity=identity,
                command=runtime_pb2.SessionCommand(
                    argv=["bash", "-c", "printf '%s:%s:' \"$KRUNTIMES_SESSION_DEFAULT\" \"$KRUNTIMES_SESSION_TEST\"; cat notes/hello.txt"],
                    env={"KRUNTIMES_SESSION_TEST": "environment"},
                ),
            )
        )
        self.assertEqual(operation.command.exit_code, 0)
        self.assertEqual(operation.command.stdout, b"session:environment:hello\n")
        self.assertFalse(operation.command.timed_out)
        self.session_stub.ExecuteSessionOperation(
            runtime_pb2.ExecuteSessionOperationRequest(
                identity=identity,
                rename_file=runtime_pb2.SessionFileRename(
                    source_path="notes/hello.txt",
                    destination_path="notes/greeting.txt",
                ),
            )
        )
        self.session_stub.ExecuteSessionOperation(
            runtime_pb2.ExecuteSessionOperationRequest(
                identity=identity,
                delete_file=runtime_pb2.SessionFileDelete(
                    path="notes",
                    recursive=True,
                ),
            )
        )
        with self.assertRaises(grpc.RpcError) as ctx:
            self.session_stub.ReadSessionFile(
                runtime_pb2.ReadSessionFileRequest(
                    identity=identity,
                    path="../outside",
                    max_bytes=1,
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    def test_session_file_listing_pages_are_bounded_and_cross_runtime(self):
        session_dir = self.work_dir / "session-pages"
        session_dir.mkdir()
        self._register_session(str(session_dir))
        identity = runtime_pb2.SessionIdentity(
            run_uid="session-run",
            assigned_pod_uid="pod-a",
        )
        for name in ("z", "b", "a", "\u00e9"):
            (session_dir / name).write_text(name)

        first = self.session_stub.ListSessionFiles(
            runtime_pb2.ListSessionFilesRequest(identity=identity, limit=2)
        )
        self.assertEqual([entry.path for entry in first.entries], ["a", "b"])
        self.assertTrue(first.next_page_token)

        second = self.session_stub.ListSessionFiles(
            runtime_pb2.ListSessionFilesRequest(
                identity=identity,
                limit=2,
                page_token=first.next_page_token,
            )
        )
        self.assertEqual([entry.path for entry in second.entries], ["z", "\u00e9"])
        self.assertFalse(second.next_page_token)

        # This token is encoded as the documented cross-runtime JSON cursor. A
        # Bash Runtime Server must accept the same unpadded base64url value.
        bash_token = "eyJ2IjoxLCJwYXRoIjoiIiwiYWZ0ZXIiOiJiIn0"
        from_bash = self.session_stub.ListSessionFiles(
            runtime_pb2.ListSessionFilesRequest(
                identity=identity,
                limit=2,
                page_token=bash_token,
            )
        )
        self.assertEqual([entry.path for entry in from_bash.entries], ["z", "\u00e9"])

        for request in (
            runtime_pb2.ListSessionFilesRequest(identity=identity, limit=1001),
            runtime_pb2.ListSessionFilesRequest(identity=identity, page_token="not-a-token"),
            runtime_pb2.ListSessionFilesRequest(
                identity=identity,
                path="other",
                page_token=first.next_page_token,
            ),
        ):
            with self.assertRaises(grpc.RpcError) as ctx:
                self.session_stub.ListSessionFiles(request)
            self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    def _wait_for_function_in_flight(self, registration, timeout=5):
        deadline = time.time() + timeout
        while time.time() < deadline:
            status = self.function_stub.FunctionStatus(
                runtime_pb2.FunctionStatusRequest(registration=registration)
            )
            if status.in_flight:
                return
            time.sleep(0.05)
        self.fail("timed out waiting for function invocation")

    def _prepare_process_tree(self, child_pid_file):
        child_code = f"""
import os
import signal
import time
from pathlib import Path

signal.signal(signal.SIGTERM, signal.SIG_IGN)
Path({str(child_pid_file)!r}).write_text(str(os.getpid()))
time.sleep(30)
"""
        return self._prepare_inline(f"""
import subprocess
import sys
import time

subprocess.Popen([sys.executable, "-c", {child_code!r}])
time.sleep(30)
""")

    def _wait_for_file(self, path, timeout=5):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if path.exists():
                return
            time.sleep(0.05)
        self.fail(f"timed out waiting for {path}")

    def _assert_process_exits(self, pid, timeout=5):
        deadline = time.time() + timeout
        stat_path = Path(f"/proc/{pid}/stat")
        while time.time() < deadline:
            try:
                stat = stat_path.read_text()
            except FileNotFoundError:
                return
            if stat.rsplit(")", 1)[1].split()[0] == "Z":
                return
            time.sleep(0.05)
        self.fail(f"process {pid} is still running")

    def test_inline_success(self):
        wd = self._prepare_inline("print(42)")
        resp = self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="test1",
            working_dir=wd,
        ))
        self.assertEqual(resp.id, "test1")
        status = self._wait("test1")
        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_SUCCEEDED)
        self.assertIn("42", status.stdout)

    def test_inline_failure(self):
        wd = self._prepare_inline("raise ValueError('boom')")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="test2",
            working_dir=wd,
        ))
        status = self._wait("test2")
        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_FAILED)

    def test_handler_mode(self):
        wd = self._prepare_inline("""
def handler(event):
    return {"status": "ok", "args": event.get("args", [])}
""", filename="app.py")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="test3",
            working_dir=wd,
            entrypoint="app.py",
            handler="app.handler",
            args=["hello", "world"],
        ))
        status = self._wait("test3")
        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_SUCCEEDED)
        self.assertIn("ok", status.stdout)

    def test_function_runtime_register_invoke_and_unregister(self):
        wd = self._prepare_inline("""
def handler(event):
    print("handler log")
    return {"status": "ok", "value": event["value"]}
""", filename="app.py")
        registration = self._register_function(wd)

        initial_status = self.function_stub.FunctionStatus(
            runtime_pb2.FunctionStatusRequest(registration=registration)
        )
        self.assertGreater(initial_status.last_activity_unix_nano, 0)

        response = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                invocation_id="invoke-1",
                content_type="application/json",
                input=b'{"value":"hello"}',
            )
        )
        self.assertEqual(response.invocation_id, "invoke-1")
        self.assertEqual(response.content_type, "application/json")
        self.assertEqual(response.output, b'{"status": "ok", "value": "hello"}\n')

        generated = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                content_type="application/json",
                input=b'{"value":"generated"}',
            )
        )
        self.assertTrue(generated.invocation_id.startswith("inv_"))

        self.function_stub.UnregisterFunction(
            runtime_pb2.UnregisterFunctionRequest(registration=registration)
        )
        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.FunctionStatus(
                runtime_pb2.FunctionStatusRequest(registration=registration)
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_function_runtime_preserves_json_response_for_logs_and_none(self):
        wd = self._prepare_inline("""
def handler(event):
    print("handler log")
    return event.get("result")
""", filename="app.py")
        registration = self._register_function(wd)

        response = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                content_type="application/json",
                input=b'{"result":{"status":"ok"}}',
            )
        )
        self.assertEqual(response.output, b'{"status": "ok"}\n')

        null_response = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                content_type="application/json",
                input=b'{"result":null}',
            )
        )
        self.assertEqual(null_response.output, b"null\n")

    def test_function_runtime_returns_outputs_from_handler_object(self):
        wd = self._prepare_inline("""
def handler(event):
    return {"result": event["value"], "outputs": {"name": "demo", "region": "us-east"}}
""", filename="app.py")
        registration = self._register_function(wd)

        response = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                content_type="application/json",
                input=b'{"value":"ok"}',
            )
        )
        self.assertEqual(
            response.output,
            b'{"result": "ok", "outputs": {"name": "demo", "region": "us-east"}}\n',
        )
        self.assertEqual(dict(response.outputs), {"name": "demo", "region": "us-east"})

    def test_function_runtime_rejects_non_string_return_outputs(self):
        wd = self._prepare_inline("""
def handler(event):
    return {"outputs": {"count": 1}}
""", filename="app.py")
        registration = self._register_function(wd)

        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.InvokeFunction(
                runtime_pb2.InvokeFunctionRequest(
                    registration=registration,
                    content_type="application/json",
                    input=b'{}',
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INTERNAL)

    def test_function_runtime_accepts_one_mebibyte_input(self):
        wd = self._prepare_inline("""
def handler(event):
    return {"size": len(event["payload"])}
""", filename="app.py")
        registration = self._register_function(wd)
        payload = json.dumps({"payload": "x" * (1024 * 1024 - 64)}).encode()
        self.assertLessEqual(len(payload), 1024 * 1024)

        response = self.function_stub.InvokeFunction(
            runtime_pb2.InvokeFunctionRequest(
                registration=registration,
                content_type="application/json",
                input=payload,
            )
        )
        self.assertEqual(
            response.output,
            f'{{"size": {1024 * 1024 - 64}}}\n'.encode(),
        )

    def test_function_runtime_registration_fencing(self):
        wd = self._prepare_inline("""
def handler(event):
    return event
""", filename="app.py")
        first = self._register_function(wd, digest="sha256:first")
        second = self._register_function(
            wd,
            attempt=2,
            digest="sha256:second",
        )
        self.assertNotEqual(first.registration_id, second.registration_id)

        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.InvokeFunction(
                runtime_pb2.InvokeFunctionRequest(
                    registration=first,
                    content_type="application/json",
                    input=b'{}',
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    def test_function_runtime_rejects_invalid_input_and_bounds_output(self):
        self.servicer.output_limit = 128
        wd = self._prepare_inline("""
def handler(event):
    return "x" * 4096
""", filename="app.py")
        registration = self._register_function(wd)

        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.InvokeFunction(
                runtime_pb2.InvokeFunctionRequest(
                    registration=registration,
                    content_type="text/plain",
                    input=b'{}',
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.InvokeFunction(
                runtime_pb2.InvokeFunctionRequest(
                    registration=registration,
                    content_type="application/json",
                    input=b"\xff",
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

        with self.assertRaises(grpc.RpcError) as ctx:
            self.function_stub.InvokeFunction(
                runtime_pb2.InvokeFunctionRequest(
                    registration=registration,
                    content_type="application/json",
                    input=b'{}',
                )
            )
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.RESOURCE_EXHAUSTED)

    def test_function_runtime_unregister_cancels_in_flight_invocation(self):
        wd = self._prepare_inline("""
import time

def handler(event):
    time.sleep(30)
    return {"status": "late"}
""", filename="app.py")
        registration = self._register_function(wd)

        with ThreadPoolExecutor(max_workers=1) as executor:
            future = executor.submit(
                self.function_stub.InvokeFunction,
                runtime_pb2.InvokeFunctionRequest(
                    registration=registration,
                    content_type="application/json",
                    input=b'{}',
                ),
            )
            self._wait_for_function_in_flight(registration)
            self.function_stub.UnregisterFunction(
                runtime_pb2.UnregisterFunctionRequest(
                    registration=registration,
                    cancel_in_flight=True,
                )
            )
            with self.assertRaises(grpc.RpcError) as ctx:
                future.result(timeout=10)
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.CANCELLED)

    def test_list_and_cancel(self):
        wd = self._prepare_inline("import time; time.sleep(30)")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="test4",
            working_dir=wd,
        ))
        lst = self.stub.List(runtime_pb2.ListRequest())
        self.assertGreaterEqual(len(lst.entries), 1)
        self.stub.Cancel(runtime_pb2.CancelRequest(id="test4"))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Status(runtime_pb2.StatusRequest(id="test4"))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_cancel_terminates_handler(self):
        wd = self._prepare_inline("""
import time

def handler(event):
    time.sleep(30)
    return {"status": "late"}
""", filename="app.py")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="cancel-handler",
            working_dir=wd,
            handler="app.handler",
        ))
        self.stub.Cancel(runtime_pb2.CancelRequest(id="cancel-handler"))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Status(runtime_pb2.StatusRequest(id="cancel-handler"))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_cancel_terminates_process_tree_and_waits(self):
        child_pid_file = self.work_dir / "cancel-child.pid"
        wd = self._prepare_process_tree(child_pid_file)
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="cancel-process-tree",
            working_dir=wd,
        ))
        self._wait_for_file(child_pid_file)
        child_pid = int(child_pid_file.read_text())

        self.stub.Cancel(
            runtime_pb2.CancelRequest(id="cancel-process-tree"),
            timeout=5,
        )

        self._assert_process_exits(child_pid)
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Status(runtime_pb2.StatusRequest(id="cancel-process-tree"))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_timeout_terminates_process_tree_and_waits(self):
        child_pid_file = self.work_dir / "timeout-child.pid"
        wd = self._prepare_process_tree(child_pid_file)
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="timeout-process-tree",
            working_dir=wd,
            timeout_seconds=1,
        ))
        self._wait_for_file(child_pid_file)
        child_pid = int(child_pid_file.read_text())

        status = self._wait("timeout-process-tree")

        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_FAILED)
        self.assertEqual(status.error_message, "timeout")
        self._assert_process_exits(child_pid)

    def test_concurrent_status_and_list_observe_consistent_results(self):
        task_count = 20
        for index in range(task_count):
            wd = self._prepare_inline(f"print('result-{index}')")
            self.stub.Execute(runtime_pb2.ExecuteRequest(
                id=f"concurrent-{index}",
                working_dir=wd,
            ))

        def read_status(index):
            status = self._wait(f"concurrent-{index}")
            return index, status

        def read_list():
            while True:
                entries = self.stub.List(runtime_pb2.ListRequest()).entries
                if all(
                    entry.state != runtime_pb2.EXECUTION_STATE_RUNNING
                    for entry in entries
                ):
                    return entries
                time.sleep(0.01)

        with ThreadPoolExecutor(max_workers=12) as executor:
            list_future = executor.submit(read_list)
            results = list(executor.map(read_status, range(task_count)))
            listed = list_future.result(timeout=10)

        self.assertEqual(len(listed), task_count)
        for index, status in results:
            self.assertEqual(
                status.state,
                runtime_pb2.EXECUTION_STATE_SUCCEEDED,
            )
            self.assertEqual(status.stdout, f"result-{index}\n")
            self.assertEqual(status.stderr, "")

    def test_output_is_bounded(self):
        output_limit = 128
        self.servicer.output_limit = output_limit
        wd = self._prepare_inline("""
import sys

sys.stdout.write("x" * 4096)
sys.stderr.write("y" * 4096)
""")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="bounded-output",
            working_dir=wd,
        ))

        status = self._wait("bounded-output")

        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_SUCCEEDED)
        self.assertTrue(status.stdout.endswith(OUTPUT_TRUNCATED_MARKER))
        self.assertEqual(
            len(status.stdout.removesuffix(OUTPUT_TRUNCATED_MARKER)),
            output_limit,
        )
        self.assertTrue(status.stderr.endswith(OUTPUT_TRUNCATED_MARKER))
        self.assertEqual(
            len(status.stderr.removesuffix(OUTPUT_TRUNCATED_MARKER)),
            output_limit,
        )

    def test_duplicate_id(self):
        wd = self._prepare_inline("print(1)")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="dup",
            working_dir=wd,
        ))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Execute(runtime_pb2.ExecuteRequest(
                id="dup",
                working_dir=wd,
            ))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.ALREADY_EXISTS)

    def test_forget_terminal_execution(self):
        wd = self._prepare_inline("print(1)")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="forget-terminal",
            working_dir=wd,
        ))
        self._wait("forget-terminal")
        self.stub.Forget(runtime_pb2.ForgetRequest(id="forget-terminal"))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Status(runtime_pb2.StatusRequest(id="forget-terminal"))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.NOT_FOUND)

    def test_forget_rejects_running_execution(self):
        wd = self._prepare_inline("import time; time.sleep(30)")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="forget-running",
            working_dir=wd,
        ))
        with self.assertRaises(grpc.RpcError) as ctx:
            self.stub.Forget(runtime_pb2.ForgetRequest(id="forget-running"))
        self.assertEqual(ctx.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        self.stub.Cancel(runtime_pb2.CancelRequest(id="forget-running"))

    def test_rejects_escaping_entrypoint(self):
        wd = self._prepare_inline("print(1)")
        self.stub.Execute(runtime_pb2.ExecuteRequest(
            id="bad-entrypoint",
            working_dir=wd,
            entrypoint="../escape.py",
        ))
        status = self._wait("bad-entrypoint")
        self.assertEqual(status.state, runtime_pb2.EXECUTION_STATE_FAILED)
        self.assertIn("entrypoint", status.error_message)


class TestSessionTermination(unittest.TestCase):
    def test_session_termination_grace_is_configured(self):
        runtime = PythonRuntime(
            tempfile.mkdtemp(),
            session_termination_grace_seconds=0.25,
        )
        process = mock.Mock()
        process.wait.side_effect = [subprocess.TimeoutExpired("command", 0.25), None]

        with mock.patch("server.terminate_process_group") as terminate:
            runtime._stop_session_process(process)

        self.assertEqual(process.wait.call_args_list[0], mock.call(timeout=0.25))
        self.assertEqual(process.wait.call_count, 2)
        self.assertEqual(terminate.call_count, 2)
        self.assertEqual(terminate.call_args_list[0][0][1], signal.SIGTERM)
        self.assertEqual(terminate.call_args_list[1][0][1], signal.SIGKILL)

    def test_session_termination_grace_must_be_positive(self):
        with self.assertRaisesRegex(ValueError, "must be positive"):
            PythonRuntime(tempfile.mkdtemp(), session_termination_grace_seconds=0)


if __name__ == "__main__":
    unittest.main()
