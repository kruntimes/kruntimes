import os
import signal
import threading
import time

from pb import runtime_pb2


class FunctionInvocation:
    def __init__(self):
        self.done = threading.Event()
        self._cancelled = threading.Event()
        self._process = None
        self._process_lock = threading.Lock()

    def cancel(self):
        self._cancelled.set()
        with self._process_lock:
            if self._process is not None:
                terminate_process_group(self._process, signal.SIGTERM)

    def cancelled(self):
        return self._cancelled.is_set()

    def set_process(self, process):
        with self._process_lock:
            self._process = process
            if self._cancelled.is_set():
                terminate_process_group(process, signal.SIGTERM)


class FunctionEntry:
    def __init__(self, registration, attempt, digest, working_dir, handler, env):
        self.lifecycle_lock = threading.Lock()
        self._lock = threading.RLock()
        self._registration = registration
        self._attempt = attempt
        self._digest = digest
        self.working_dir = working_dir
        self.handler = handler
        self.env = dict(env)
        self._state = runtime_pb2.FUNCTION_REGISTRATION_STATE_READY
        self._invocation = None
        self._last_activity_unix_nano = 0

    def registration_action(self, attempt, digest):
        with self._lock:
            if attempt < self._attempt:
                return "stale", None
            if attempt == self._attempt:
                if digest != self._digest:
                    return "different-digest", None
                return "current", None

            self._state = runtime_pb2.FUNCTION_REGISTRATION_STATE_DRAINING
            return "replace", self._invocation

    def registration_response(self):
        with self._lock:
            return runtime_pb2.RegisterFunctionResponse(
                registration=clone_registration(self._registration),
                state=self._state,
            )

    def status_response(self):
        with self._lock:
            return runtime_pb2.FunctionStatusResponse(
                registration=clone_registration(self._registration),
                state=self._state,
                in_flight=1 if self._invocation is not None else 0,
                last_activity_unix_nano=self._last_activity_unix_nano,
            )

    def matches_registration(self, registration_id):
        with self._lock:
            return self._registration.registration_id == registration_id

    def start_invocation(self):
        with self._lock:
            if self._state != runtime_pb2.FUNCTION_REGISTRATION_STATE_READY:
                return None, "not-ready"
            if self._invocation is not None:
                return None, "busy"
            invocation = FunctionInvocation()
            self._invocation = invocation
            self._last_activity_unix_nano = time.time_ns()
            return invocation, ""

    def finish_invocation(self, invocation):
        with self._lock:
            if self._invocation is not invocation:
                return
            self._invocation = None
            self._last_activity_unix_nano = time.time_ns()
            invocation.done.set()

    def begin_drain(self, registration_id):
        with self._lock:
            if self._registration.registration_id != registration_id:
                return None, "stale"
            self._state = runtime_pb2.FUNCTION_REGISTRATION_STATE_DRAINING
            return self._invocation, ""


def clone_registration(registration):
    return runtime_pb2.FunctionRegistration(
        run_uid=registration.run_uid,
        registration_id=registration.registration_id,
    )


def terminate_process_group(process, sig):
    try:
        os.killpg(process.pid, sig)
    except ProcessLookupError:
        pass
