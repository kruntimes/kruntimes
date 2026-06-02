import json
import os
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
            }

        task_dir = Path(request.working_dir) if request.working_dir else (self.base_dir / task_id)
        task_dir.mkdir(parents=True, exist_ok=True)

        state = self._tasks[task_id]
        threading.Thread(
            target=self._execute, args=(task_id, task_dir, request, state),
            daemon=True,
        ).start()

        return runtime_pb2.ExecuteResponse(id=task_id)

    def Status(self, request, context):
        with self._lock:
            task = self._tasks.get(request.id)
        if task is None:
            context.set_code(grpc.StatusCode.NOT_FOUND)
            context.set_details(f"task {request.id} not found")
            return runtime_pb2.StatusResponse()
        return runtime_pb2.StatusResponse(
            id=request.id,
            state=task["state"],
            exit_code=task["exit_code"],
            stdout=task["stdout"],
            stderr=task["stderr"],
            error_message=task["error_message"],
        )

    def List(self, request, context):
        with self._lock:
            entries = []
            for task_id, task in self._tasks.items():
                entries.append(runtime_pb2.StatusResponse(
                    id=task_id,
                    state=task["state"],
                    exit_code=task["exit_code"],
                    stdout=task["stdout"],
                    stderr=task["stderr"],
                    error_message=task["error_message"],
                ))
        return runtime_pb2.ListResponse(entries=entries)

    def Cancel(self, request, context):
        with self._lock:
            task = self._tasks.get(request.id)
        if task is None:
            context.set_code(grpc.StatusCode.NOT_FOUND)
            context.set_details(f"task {request.id} not found")
            return runtime_pb2.CancelResponse()

        proc = task.get("_proc")
        if proc:
            proc.terminate()
        with self._lock:
            self._tasks.pop(request.id, None)
        return runtime_pb2.CancelResponse()

    def Health(self, request, context):
        return runtime_pb2.HealthResponse(healthy=True)

    def _execute(self, task_id, task_dir, request, state):
        try:
            if request.handler:
                self._run_handler(task_id, task_dir, request, state)
            else:
                self._run_entrypoint(task_id, task_dir, request, state)
        except Exception as e:
            state["state"] = runtime_pb2.EXECUTION_STATE_FAILED
            state["error_message"] = str(e)

    def _run_handler(self, task_id, task_dir, request, state):
        sys.path.insert(0, str(task_dir))
        try:
            module_name, func_name = request.handler.rsplit(".", 1)
            import importlib
            mod = importlib.import_module(module_name)
            func = getattr(mod, func_name)

            event = {"args": list(request.args)}
            result = func(event)
            if result is not None:
                state["stdout"] = json.dumps(result)
            state["state"] = runtime_pb2.EXECUTION_STATE_SUCCEEDED
        finally:
            sys.path.pop(0)

    def _run_entrypoint(self, task_id, task_dir, request, state):
        entrypoint = request.entrypoint or "script"
        script = task_dir / entrypoint
        if script.exists():
            cmd = [sys.executable, str(script)] + list(request.args)
        elif request.args:
            cmd = [sys.executable] + list(request.args)
        else:
            state["state"] = runtime_pb2.EXECUTION_STATE_FAILED
            state["error_message"] = "no script or args provided"
            return

        env = os.environ.copy()
        env.update(request.env)
        timeout = request.timeout_seconds or None

        try:
            proc = subprocess.Popen(
                cmd, cwd=str(task_dir), env=env,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                text=True,
            )
            state["_proc"] = proc
            stdout, stderr = proc.communicate(timeout=timeout)
            state["stdout"] = stdout
            state["stderr"] = stderr
            state["exit_code"] = proc.returncode
            state["state"] = (
                runtime_pb2.EXECUTION_STATE_SUCCEEDED
                if proc.returncode == 0
                else runtime_pb2.EXECUTION_STATE_FAILED
            )
        except subprocess.TimeoutExpired:
            proc.kill()
            stdout, stderr = proc.communicate()
            state["stdout"] = stdout or ""
            state["stderr"] = stderr or ""
            state["state"] = runtime_pb2.EXECUTION_STATE_FAILED
            state["error_message"] = "timeout"
