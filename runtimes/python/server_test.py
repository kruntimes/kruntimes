import tempfile
import time
import unittest
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

import grpc

from pb import runtime_pb2
from pb import runtime_pb2_grpc
from server import PythonRuntime


class TestPythonRuntime(unittest.TestCase):
    def setUp(self):
        self.work_dir = Path(tempfile.mkdtemp())
        self.server = grpc.server(ThreadPoolExecutor(max_workers=4))
        self.servicer = PythonRuntime(str(self.work_dir))
        runtime_pb2_grpc.add_RuntimeServicer_to_server(self.servicer, self.server)
        port = self.server.add_insecure_port("localhost:0")
        self.server.start()
        self.channel = grpc.insecure_channel(f"localhost:{port}")
        self.stub = runtime_pb2_grpc.RuntimeStub(self.channel)

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


if __name__ == "__main__":
    unittest.main()
