import json
import os
import re
import secrets
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

import grpc
from pb import runtime_pb2
from pb import runtime_pb2_grpc
from function import FunctionEntry, clone_registration, terminate_process_group


DEFAULT_OUTPUT_LIMIT_BYTES = 1024 * 1024
OUTPUT_TRUNCATED_MARKER = "\n[output truncated]\n"
DEFAULT_FUNCTION_INVOKE_TIMEOUT_SECONDS = 30
MAX_FUNCTION_INVOKE_TIMEOUT_SECONDS = 5 * 60
DEFAULT_FUNCTION_DRAIN_TIMEOUT_SECONDS = 30
MAX_FUNCTION_DRAIN_TIMEOUT_SECONDS = 5 * 60
HANDLER_COMPONENT = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")

FUNCTION_HANDLER_SCRIPT = """
import contextlib
import importlib
import json
import sys

module_name, func_name = sys.argv[1].rsplit(".", 1)
event = json.loads(sys.stdin.read())
with contextlib.redirect_stdout(sys.stderr):
    result = getattr(importlib.import_module(module_name), func_name)(event)
json.dump(result, sys.stdout)
sys.stdout.write("\\n")
"""

FUNCTION_VALIDATION_SCRIPT = """
import importlib
import sys

module_name, func_name = sys.argv[1].rsplit(".", 1)
handler = getattr(importlib.import_module(module_name), func_name)
if not callable(handler):
    raise TypeError(f"handler {sys.argv[1]!r} is not callable")
"""


class BoundedBuffer:
    def __init__(self, limit=DEFAULT_OUTPUT_LIMIT_BYTES):
        self._limit = limit
        self._buffer = bytearray()
        self._size = 0
        self._truncated = False

    def write(self, chunk):
        if not chunk:
            return
        if self._limit <= 0:
            self._truncated = True
            return
        remaining = self._limit - self._size
        if remaining > 0:
            retained = chunk[:remaining]
            self._buffer.extend(retained)
            self._size += len(retained)
        if len(chunk) > remaining:
            self._truncated = True

    def snapshot(self):
        output = self._buffer.decode("utf-8", errors="replace")
        if self._truncated:
            output += OUTPUT_TRUNCATED_MARKER
        return output

    @property
    def truncated(self):
        return self._truncated


class PythonRuntime(
    runtime_pb2_grpc.RuntimeServicer,
    runtime_pb2_grpc.FunctionRuntimeServicer,
):
    def __init__(self, work_dir="/workspace", output_limit=DEFAULT_OUTPUT_LIMIT_BYTES):
        self.base_dir = Path(work_dir)
        self.base_dir.mkdir(parents=True, exist_ok=True)
        self.output_limit = output_limit
        self._tasks = {}
        self._lock = threading.Lock()
        self._functions = {}
        self._functions_lock = threading.Lock()

    def Execute(self, request, context):
        task_id = request.id
        with self._lock:
            if task_id in self._tasks:
                context.set_code(grpc.StatusCode.ALREADY_EXISTS)
                context.set_details(f"task {task_id} already exists")
                return runtime_pb2.ExecuteResponse(id=task_id)
            self._tasks[task_id] = {
                "state": runtime_pb2.EXECUTION_STATE_RUNNING,
                "stdout": BoundedBuffer(self.output_limit),
                "stderr": BoundedBuffer(self.output_limit),
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
            if proc is not None:
                self._signal_process_group(proc, signal.SIGTERM)

        if not done.wait(timeout=2):
            with self._lock:
                proc = task.get("_proc")
                if proc is not None:
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

    def RegisterFunction(self, request, context):
        try:
            working_dir, handler = self._validate_function_registration(request)
        except ValueError as error:
            context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(error))

        while True:
            entry = self._function(request.run_uid)
            if entry is None:
                registration = runtime_pb2.FunctionRegistration(
                    run_uid=request.run_uid,
                    registration_id=self._new_function_id("reg_"),
                )
                candidate = FunctionEntry(
                    registration,
                    request.registration_attempt,
                    request.registration_digest,
                    working_dir,
                    handler,
                    request.env,
                )
                if self._add_function_if_absent(request.run_uid, candidate):
                    return candidate.registration_response()
                continue

            with entry.lifecycle_lock:
                if not self._is_current_function(request.run_uid, entry):
                    continue
                action, invocation = entry.registration_action(
                    request.registration_attempt,
                    request.registration_digest,
                )
                if action == "stale":
                    context.abort(
                        grpc.StatusCode.FAILED_PRECONDITION,
                        "registration attempt is stale",
                    )
                if action == "different-digest":
                    context.abort(
                        grpc.StatusCode.ALREADY_EXISTS,
                        "registration attempt already exists with a different digest",
                    )
                if action == "current":
                    return entry.registration_response()

                if invocation is not None:
                    invocation.cancel()
                    self._wait_for_invocation(
                        context,
                        invocation,
                        DEFAULT_FUNCTION_DRAIN_TIMEOUT_SECONDS,
                    )

                registration = runtime_pb2.FunctionRegistration(
                    run_uid=request.run_uid,
                    registration_id=self._new_function_id("reg_"),
                )
                replacement = FunctionEntry(
                    registration,
                    request.registration_attempt,
                    request.registration_digest,
                    working_dir,
                    handler,
                    request.env,
                )
                if self._replace_function_if_current(request.run_uid, entry, replacement):
                    return replacement.registration_response()

    def FunctionStatus(self, request, context):
        entry = self._match_function(request.registration, context)
        return entry.status_response()

    def InvokeFunction(self, request, context):
        if len(request.invocation_id) > 128:
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "invocation id must be no larger than 128 bytes",
            )
        if request.content_type != "application/json":
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "Python functions support only application/json input",
            )
        if (
            not request.input
            or len(request.input) > DEFAULT_OUTPUT_LIMIT_BYTES
        ):
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "input must be valid JSON no larger than 1 MiB",
            )
        try:
            input_payload = request.input.decode("utf-8")
            json.loads(input_payload)
        except (TypeError, UnicodeDecodeError, ValueError):
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "input must be valid JSON no larger than 1 MiB",
            )

        entry = self._match_function(request.registration, context)
        invocation, reason = entry.start_invocation()
        if reason == "not-ready":
            context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                "function registration is not ready",
            )
        if reason == "busy":
            context.abort(
                grpc.StatusCode.RESOURCE_EXHAUSTED,
                "function registration already has an in-flight invocation",
            )

        context.add_callback(invocation.cancel)
        try:
            output = self._invoke_function(
                entry,
                invocation,
                input_payload,
                request,
                context,
            )
        finally:
            entry.finish_invocation(invocation)

        return runtime_pb2.InvokeFunctionResponse(
            registration=clone_registration(request.registration),
            invocation_id=request.invocation_id or self._new_function_id("inv_"),
            output=output,
            content_type="application/json",
        )

    def UnregisterFunction(self, request, context):
        registration = request.registration
        if not registration or not registration.run_uid or not registration.registration_id:
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "registration run uid and id are required",
            )

        while True:
            entry = self._function(registration.run_uid)
            if entry is None:
                return runtime_pb2.UnregisterFunctionResponse(
                    registration=clone_registration(registration),
                )
            with entry.lifecycle_lock:
                if not self._is_current_function(registration.run_uid, entry):
                    continue
                invocation, reason = entry.begin_drain(registration.registration_id)
                if reason == "stale":
                    context.abort(
                        grpc.StatusCode.FAILED_PRECONDITION,
                        "function registration is stale",
                    )
                if invocation is not None:
                    if request.cancel_in_flight:
                        invocation.cancel()
                    drain_timeout = self._drain_timeout_seconds(
                        request.drain_timeout_millis,
                    )
                    self._wait_for_invocation(context, invocation, drain_timeout)
                self._delete_function_if_current(registration.run_uid, entry)
                return runtime_pb2.UnregisterFunctionResponse(
                    registration=clone_registration(registration),
                )

    @staticmethod
    def _status_response(task_id, task):
        return runtime_pb2.StatusResponse(
            id=task_id,
            state=task["state"],
            exit_code=task["exit_code"],
            stdout=task["stdout"].snapshot(),
            stderr=task["stderr"].snapshot(),
            error_message=task["error_message"],
        )

    def _function(self, run_uid):
        with self._functions_lock:
            return self._functions.get(run_uid)

    def _add_function_if_absent(self, run_uid, entry):
        with self._functions_lock:
            if run_uid in self._functions:
                return False
            self._functions[run_uid] = entry
            return True

    def _is_current_function(self, run_uid, entry):
        with self._functions_lock:
            return self._functions.get(run_uid) is entry

    def _replace_function_if_current(self, run_uid, current, replacement):
        with self._functions_lock:
            if self._functions.get(run_uid) is not current:
                return False
            self._functions[run_uid] = replacement
            return True

    def _delete_function_if_current(self, run_uid, entry):
        with self._functions_lock:
            if self._functions.get(run_uid) is entry:
                del self._functions[run_uid]

    def _match_function(self, registration, context):
        if registration is None or not registration.run_uid or not registration.registration_id:
            context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "registration run uid and id are required",
            )
        entry = self._function(registration.run_uid)
        if entry is None:
            context.abort(
                grpc.StatusCode.NOT_FOUND,
                "function registration not found",
            )
        if not entry.matches_registration(registration.registration_id):
            context.abort(
                grpc.StatusCode.FAILED_PRECONDITION,
                "function registration is stale",
            )
        return entry

    def _validate_function_registration(self, request):
        if (
            not request.run_uid
            or request.registration_attempt < 1
            or not request.registration_digest
        ):
            raise ValueError(
                "run uid, positive registration attempt, and registration digest are required",
            )
        if len(request.registration_digest) > 128:
            raise ValueError("registration digest must be no larger than 128 bytes")
        working_dir = self._function_working_dir(request.working_dir)
        handler = self._parse_python_handler(request.handler)
        self._validate_python_handler(working_dir, handler, request.env)
        return working_dir, handler

    def _function_working_dir(self, working_dir):
        if not working_dir:
            raise ValueError("working directory is required")
        try:
            base_dir = self.base_dir.resolve(strict=True)
            candidate = Path(working_dir).resolve(strict=True)
            candidate.relative_to(base_dir)
        except (OSError, ValueError):
            raise ValueError("working directory must be within the runtime workspace")
        if not candidate.is_dir():
            raise ValueError("working directory must be a directory")
        return candidate

    @staticmethod
    def _parse_python_handler(handler):
        if not handler or "." not in handler:
            raise ValueError("handler must use module.function form")
        module_name, function_name = handler.rsplit(".", 1)
        components = module_name.split(".")
        if (
            not module_name
            or not all(HANDLER_COMPONENT.fullmatch(component) for component in components)
            or not HANDLER_COMPONENT.fullmatch(function_name)
        ):
            raise ValueError("handler must use module.function form")
        return handler

    @staticmethod
    def _python_handler_path(working_dir, handler):
        module_name, _ = handler.rsplit(".", 1)
        module_path = Path(*module_name.split("."))
        candidates = (
            working_dir / module_path.with_suffix(".py"),
            working_dir / module_path / "__init__.py",
        )
        for candidate in candidates:
            if not candidate.is_file():
                continue
            try:
                resolved = candidate.resolve(strict=True)
                resolved.relative_to(working_dir)
            except (OSError, ValueError):
                continue
            return resolved
        raise ValueError("handler module must be a file within the working directory")

    def _validate_python_handler(self, working_dir, handler, function_env):
        self._python_handler_path(working_dir, handler)
        env = os.environ.copy()
        env.update(function_env)
        try:
            result = subprocess.run(
                [sys.executable, "-c", FUNCTION_VALIDATION_SCRIPT, handler],
                cwd=str(working_dir),
                env=env,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                timeout=DEFAULT_FUNCTION_DRAIN_TIMEOUT_SECONDS,
            )
        except subprocess.TimeoutExpired as error:
            raise ValueError("handler validation timed out") from error
        if result.returncode != 0:
            details = result.stderr.decode("utf-8", errors="replace").strip()
            raise ValueError(f"handler {handler!r} is not importable: {details[:4096]}")

    @staticmethod
    def _new_function_id(prefix):
        return f"{prefix}{secrets.token_hex(16)}"

    @staticmethod
    def _drain_timeout_seconds(timeout_millis):
        if timeout_millis <= 0:
            return DEFAULT_FUNCTION_DRAIN_TIMEOUT_SECONDS
        return min(
            timeout_millis / 1000,
            MAX_FUNCTION_DRAIN_TIMEOUT_SECONDS,
        )

    @staticmethod
    def _wait_for_invocation(context, invocation, timeout_seconds):
        deadline = time.monotonic() + timeout_seconds
        remaining = context.time_remaining()
        if remaining is not None:
            deadline = min(deadline, time.monotonic() + max(remaining, 0))
        if invocation.done.wait(max(deadline - time.monotonic(), 0)):
            return
        context.abort(
            grpc.StatusCode.DEADLINE_EXCEEDED,
            "function invocation did not drain before the deadline",
        )

    def _invoke_function(self, entry, invocation, input_payload, request, context):
        env = os.environ.copy()
        env.update(entry.env)
        try:
            process = subprocess.Popen(
                [
                    sys.executable,
                    "-c",
                    FUNCTION_HANDLER_SCRIPT,
                    entry.handler,
                ],
                cwd=str(entry.working_dir),
                env=env,
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                start_new_session=True,
            )
        except OSError as error:
            context.abort(grpc.StatusCode.INTERNAL, f"invoke handler: {error}")

        invocation.set_process(process)
        try:
            process.stdin.write(input_payload.encode("utf-8"))
            process.stdin.close()
        except BrokenPipeError:
            # A cancelled invocation may terminate the process before its
            # request body is fully written. The normal cancellation path
            # below returns the client-visible status.
            pass
        stdout = BoundedBuffer(self.output_limit)
        stderr = BoundedBuffer(self.output_limit)
        stdout_reader = threading.Thread(
            target=self._read_function_stream,
            args=(stdout, process.stdout),
            daemon=True,
        )
        stderr_reader = threading.Thread(
            target=self._read_function_stream,
            args=(stderr, process.stderr),
            daemon=True,
        )
        stdout_reader.start()
        stderr_reader.start()

        timeout = self._function_invoke_timeout_seconds(request.timeout_millis, context)
        deadline = time.monotonic() + timeout
        deadline_expired = False
        while process.poll() is None:
            if invocation.cancelled():
                self._stop_process(process)
                break
            if time.monotonic() >= deadline:
                deadline_expired = True
                self._stop_process(process)
                break
            time.sleep(0.01)

        stdout_reader.join()
        stderr_reader.join()
        if invocation.cancelled() and not deadline_expired:
            context.abort(grpc.StatusCode.CANCELLED, "function invocation was cancelled")
        if deadline_expired:
            context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "function invocation timed out")
        if process.returncode != 0:
            context.abort(
                grpc.StatusCode.INTERNAL,
                f"invoke handler: {stderr.snapshot()[:4096]}",
            )
        if stdout.truncated:
            context.abort(
                grpc.StatusCode.RESOURCE_EXHAUSTED,
                "function response exceeds the configured output limit",
            )
        return stdout.snapshot().encode("utf-8")

    @staticmethod
    def _read_function_stream(buffer, stream):
        try:
            while True:
                chunk = stream.read(4096)
                if not chunk:
                    return
                buffer.write(chunk)
        finally:
            stream.close()

    @staticmethod
    def _function_invoke_timeout_seconds(timeout_millis, context):
        timeout = DEFAULT_FUNCTION_INVOKE_TIMEOUT_SECONDS
        if timeout_millis > 0:
            timeout = timeout_millis / 1000
        timeout = min(timeout, MAX_FUNCTION_INVOKE_TIMEOUT_SECONDS)
        remaining = context.time_remaining()
        if remaining is not None:
            timeout = min(timeout, max(remaining, 0))
        return timeout

    @staticmethod
    def _stop_process(process):
        terminate_process_group(process, signal.SIGTERM)
        try:
            process.wait(timeout=2)
        except subprocess.TimeoutExpired:
            terminate_process_group(process, signal.SIGKILL)
            process.wait()

    def _finish_task(self, task_id, **updates):
        with self._lock:
            task = self._tasks.get(task_id)
            if task is not None:
                task.update(updates)
                task["_proc"] = None
                task["_done"].set()

    @staticmethod
    def _signal_process_group(proc, sig):
        try:
            os.killpg(proc.pid, sig)
        except ProcessLookupError:
            pass

    def _read_stream(self, task_id, stream_name, stream):
        try:
            while True:
                chunk = stream.read(4096)
                if not chunk:
                    return
                with self._lock:
                    task = self._tasks.get(task_id)
                    if task is None:
                        return
                    task[stream_name].write(chunk)
        finally:
            stream.close()

    @staticmethod
    def _resolve_entrypoint(entrypoint, fallback="script"):
        candidate = Path(entrypoint or fallback)
        if candidate.is_absolute():
            raise ValueError("entrypoint must be a relative path within the workspace")
        cleaned = Path(os.path.normpath(str(candidate)))
        if cleaned == Path(".") or ".." in cleaned.parts:
            raise ValueError("entrypoint must be a relative path within the workspace")
        return cleaned

    def _execute(self, task_id, task_dir, request):
        try:
            if request.handler:
                result = self._run_handler(task_id, task_dir, request)
            else:
                result = self._run_entrypoint(task_id, task_dir, request)
        except Exception as e:
            result = {
                "state": runtime_pb2.EXECUTION_STATE_FAILED,
                "error_message": str(e),
            }
        self._finish_task(task_id, **(result or {}))

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
        return self._run_process(task_id, task_dir, request, cmd)

    def _run_entrypoint(self, task_id, task_dir, request):
        entrypoint = self._resolve_entrypoint(request.entrypoint)
        script = task_dir / entrypoint
        if script.exists():
            cmd = [sys.executable, str(script)] + list(request.args)
        elif request.args:
            cmd = [sys.executable] + list(request.args)
        else:
            return {
                "state": runtime_pb2.EXECUTION_STATE_FAILED,
                "error_message": "no script or args provided",
            }

        return self._run_process(task_id, task_dir, request, cmd)

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
                    start_new_session=True,
                )
                task["_proc"] = proc
            stdout_reader = threading.Thread(
                target=self._read_stream,
                args=(task_id, "stdout", proc.stdout),
                daemon=True,
            )
            stderr_reader = threading.Thread(
                target=self._read_stream,
                args=(task_id, "stderr", proc.stderr),
                daemon=True,
            )
            stdout_reader.start()
            stderr_reader.start()

            proc.wait(timeout=timeout)
            stdout_reader.join()
            stderr_reader.join()
            return {
                "exit_code": proc.returncode,
                "state": (
                    runtime_pb2.EXECUTION_STATE_SUCCEEDED
                    if proc.returncode == 0
                    else runtime_pb2.EXECUTION_STATE_FAILED
                ),
            }
        except subprocess.TimeoutExpired:
            self._signal_process_group(proc, signal.SIGKILL)
            proc.wait()
            stdout_reader.join()
            stderr_reader.join()
            return {
                "state": runtime_pb2.EXECUTION_STATE_FAILED,
                "error_message": "timeout",
            }
