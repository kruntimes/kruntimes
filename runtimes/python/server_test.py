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


if __name__ == "__main__":
    unittest.main()
