import json
import os
import signal
import subprocess
import sys
import threading
from pathlib import Path

import grpc
from pb import runtime_pb2
from pb import runtime_pb2_grpc


class PythonRuntime(runtime_pb2_grpc.RuntimeServicer):
    def __init__(self, work_dir="/workspace"):
        self.base_dir = Path(work_dir)
        self.base_dir.mkdir(parents=True, exist_ok=True)
        self._tasks = {}
        self._lock = threading.Lock()

    def Execute(self, request, context):
        task_id = request.id
        with self._lock:
            if task_id in self._tasks:
                context.set_code(grpc.StatusCode.ALREADY_EXISTS)
                context.set_details(f"task {task_id} already exists")
                return runtime_pb2.ExecuteResponse(id=task_id)
            self._tasks[task_id] = {
                "state": runtime_pb2.EXECUTION_STATE_RUNNING,
                "stdout": "",
                "stderr": "",
                "exit_code": 0,
                "error_message": "",
                "_cancelled": False,
                "_done": threading.Event(),
            }

        task_dir = Path(request.working_dir) if request.working_dir else (self.base_dir / task_id)
        task_dir.mkdir(parents=True, exist_ok=True)

        threading.Thread(
            target=self._execute, args=(task_id, task_dir, request),
            daemon=True,
        ).start()

        return runtime_pb2.ExecuteResponse(id=task_id)

    def Status(self, request, context):
        with self._lock:
            task = self._tasks.get(request.id)
            if task is None:
                context.abort(
                    grpc.StatusCode.NOT_FOUND,
                    f"execution {request.id} not found",
                )
            return self._status_response(request.id, task)

    def List(self, request, context):
        with self._lock:
            entries = []
            for task_id, task in self._tasks.items():
                entries.append(self._status_response(task_id, task))
        return runtime_pb2.ListResponse(entries=entries)

    def Cancel(self, request, context):
        with self._lock:
            task = self._tasks.get(request.id)
            if task is None:
                context.abort(
                    grpc.StatusCode.NOT_FOUND,
                    f"execution {request.id} not found",
                )
            task["_cancelled"] = True
            proc = task.get("_proc")
            done = task["_done"]
            if proc is not None and proc.poll() is None:
                self._signal_process_group(proc, signal.SIGTERM)

        if not done.wait(timeout=2):
            with self._lock:
                proc = task.get("_proc")
                if proc is not None and proc.poll() is None:
                    self._signal_process_group(proc, signal.SIGKILL)
            done.wait()

        with self._lock:
            self._tasks.pop(request.id, None)
        return runtime_pb2.CancelResponse()

    def Forget(self, request, context):
        with self._lock:
            task = self._tasks.get(request.id)
            if task is None:
                context.abort(
                    grpc.StatusCode.NOT_FOUND,
                    f"execution {request.id} not found",
                )
            if task["state"] not in (
                runtime_pb2.EXECUTION_STATE_SUCCEEDED,
                runtime_pb2.EXECUTION_STATE_FAILED,
            ):
                context.abort(
                    grpc.StatusCode.FAILED_PRECONDITION,
                    f"execution {request.id} is still running",
                )
            self._tasks.pop(request.id)
        return runtime_pb2.ForgetResponse()

    def Health(self, request, context):
        return runtime_pb2.HealthResponse(healthy=True)

    @staticmethod
    def _status_response(task_id, task):
        return runtime_pb2.StatusResponse(
            id=task_id,
            state=task["state"],
            exit_code=task["exit_code"],
            stdout=task["stdout"],
            stderr=task["stderr"],
            error_message=task["error_message"],
        )

    def _update_task(self, task_id, **updates):
        with self._lock:
            task = self._tasks.get(task_id)
            if task is not None:
                task.update(updates)

    @staticmethod
    def _signal_process_group(proc, sig):
        try:
            os.killpg(proc.pid, sig)
        except ProcessLookupError:
            pass

    def _execute(self, task_id, task_dir, request):
        try:
            if request.handler:
                self._run_handler(task_id, task_dir, request)
            else:
                self._run_entrypoint(task_id, task_dir, request)
        except Exception as e:
            self._update_task(
                task_id,
                state=runtime_pb2.EXECUTION_STATE_FAILED,
                error_message=str(e),
            )
        finally:
            with self._lock:
                task = self._tasks.get(task_id)
                if task is not None:
                    task["_done"].set()

    def _run_handler(self, task_id, task_dir, request):
        handler_script = """
import importlib
import json
import sys

module_name, func_name = sys.argv[1].rsplit(".", 1)
event = json.loads(sys.argv[2])
result = getattr(importlib.import_module(module_name), func_name)(event)
if result is not None:
    print(json.dumps(result))
"""
        cmd = [
            sys.executable,
            "-c",
            handler_script,
            request.handler,
            json.dumps({"args": list(request.args)}),
        ]
        self._run_process(task_id, task_dir, request, cmd)

    def _run_entrypoint(self, task_id, task_dir, request):
        entrypoint = request.entrypoint or "script"
        script = task_dir / entrypoint
        if script.exists():
            cmd = [sys.executable, str(script)] + list(request.args)
        elif request.args:
            cmd = [sys.executable] + list(request.args)
        else:
            self._update_task(
                task_id,
                state=runtime_pb2.EXECUTION_STATE_FAILED,
                error_message="no script or args provided",
            )
            return

        self._run_process(task_id, task_dir, request, cmd)

    def _run_process(self, task_id, task_dir, request, cmd):
        env = os.environ.copy()
        env.update(request.env)
        timeout = request.timeout_seconds or None
        try:
            with self._lock:
                task = self._tasks.get(task_id)
                if task is None or task["_cancelled"]:
                    return
                proc = subprocess.Popen(
                    cmd, cwd=str(task_dir), env=env,
                    stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                    text=True,
                    start_new_session=True,
                )
                task["_proc"] = proc
            stdout, stderr = proc.communicate(timeout=timeout)
            self._update_task(
                task_id,
                stdout=stdout,
                stderr=stderr,
                exit_code=proc.returncode,
                state=(
                    runtime_pb2.EXECUTION_STATE_SUCCEEDED
                    if proc.returncode == 0
                    else runtime_pb2.EXECUTION_STATE_FAILED
                ),
            )
        except subprocess.TimeoutExpired:
            self._signal_process_group(proc, signal.SIGKILL)
            stdout, stderr = proc.communicate()
            self._update_task(
                task_id,
                stdout=stdout or "",
                stderr=stderr or "",
                state=runtime_pb2.EXECUTION_STATE_FAILED,
                error_message="timeout",
            )
