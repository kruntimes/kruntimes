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
        self.work_dir = Path(work_dir)
        self.work_dir.mkdir(parents=True, exist_ok=True)
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

        task_dir = self.work_dir / task_id
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

    def _execute(self, task_id, task_dir, request, state):
        try:
            if request.source_inline:
                script_path = self._prepare_inline(task_dir, request)
            elif request.source_repo_url:
                script_path = self._clone_repo(task_dir, request)
            else:
                script_path = None

            if request.entrypoint:
                self._run_entrypoint(task_id, task_dir, script_path, request, state)
            elif script_path:
                self._run_script(task_id, task_dir, script_path, request, state)
            else:
                self._run_script(task_id, task_dir, None, request, state)

        except Exception as e:
            state["state"] = runtime_pb2.EXECUTION_STATE_FAILED
            state["error_message"] = str(e)

    def _prepare_inline(self, task_dir, request):
        script_path = task_dir / "script.py"
        script_path.write_text(request.source_inline)
        return script_path

    def _clone_repo(self, task_dir, request):
        clone_dir = task_dir / "repo"
        subprocess.run(
            ["git", "clone", request.source_repo_url, str(clone_dir)],
            cwd=str(task_dir), capture_output=True, check=True, timeout=120,
        )
        if request.source_commit_sha:
            subprocess.run(
                ["git", "checkout", request.source_commit_sha],
                cwd=str(clone_dir), capture_output=True, check=True, timeout=30,
            )

        req_file = clone_dir / "requirements.txt"
        if req_file.exists():
            subprocess.run(
                [sys.executable, "-m", "pip", "install", "-r", str(req_file)],
                cwd=str(clone_dir), capture_output=True, timeout=120,
            )
        return clone_dir

    def _run_entrypoint(self, task_id, task_dir, script_path, request, state):
        src_dir = script_path.parent if script_path else task_dir
        sys.path.insert(0, str(src_dir))

        try:
            module_name, func_name = request.entrypoint.rsplit(".", 1)
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

    def _run_script(self, task_id, task_dir, script_path, request, state):
        cmd = [sys.executable]
        if script_path:
            cmd.append(str(script_path))
        elif request.args:
            args = list(request.args)
            cmd.append(args[0])
            cmd.extend(args[1:])
        else:
            state["state"] = runtime_pb2.EXECUTION_STATE_FAILED
            state["error_message"] = "no script or args provided"
            return

        env = os.environ.copy()
        env.update(request.env)
        timeout = request.timeout_seconds or 600

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
