"""Agent-facing Sandboxes backed by kruntimes Session-mode Runs.

The Kubernetes Run remains authoritative. This module deliberately does not
introduce a second Sandbox resource or expose private runtimed gRPC ports.
"""

from __future__ import annotations

import base64
import json
import threading
import time
from dataclasses import dataclass, field
from typing import Any, Mapping, Protocol, Sequence
from urllib.parse import urlencode, urlparse, urlunparse


_TERMINAL_PHASES = frozenset(("Succeeded", "Failed", "Timeout", "Cancelled"))
_DEFAULT_POLL_INTERVAL_SECONDS = 0.5
_SESSION_FRAME_TYPE_SEND = "send"
_SESSION_FRAME_TYPE_CANCEL = "cancel"
_SESSION_FRAME_TYPE_HEARTBEAT = "heartbeat"


class RunClient(Protocol):
    """Kubernetes boundary used to create and manage Run custom resources."""

    def create(self, namespace: str, run: Mapping[str, Any]) -> dict[str, Any]: ...

    def get(self, namespace: str, name: str) -> dict[str, Any]: ...

    def replace(self, namespace: str, name: str, run: Mapping[str, Any]) -> dict[str, Any]: ...

    def delete(self, namespace: str, name: str) -> None: ...


class GatewayTransport(Protocol):
    """HTTP boundary used for the shared Runtime gateway."""

    def request(
        self, method: str, url: str, body: bytes | None, headers: Mapping[str, str]
    ) -> "HTTPResponse": ...

    def open_session(self, url: str, headers: Mapping[str, str], timeout_seconds: float | None) -> "SessionConnection": ...


class SessionConnection(Protocol):
    """One bidirectional Runtime gateway WebSocket connection."""

    def send(self, payload: str) -> None: ...

    def recv(self) -> str | bytes: ...

    def close(self) -> None: ...



class LogReader(Protocol):
    """Kubernetes boundary used to read the assigned runtimed container log."""

    def read(self, namespace: str, pod: str, container: str) -> str: ...


@dataclass(frozen=True)
class HTTPResponse:
    """A bounded Runtime gateway HTTP response."""

    status_code: int
    body: bytes = b""



class APIError(Exception):
    """A non-success Runtime gateway response."""

    def __init__(self, status_code: int, message: str) -> None:
        self.status_code = status_code
        self.message = message
        super().__init__(f"Runtime gateway returned HTTP {status_code}: {message}")


class SandboxStateError(Exception):
    """A Run lifecycle state that cannot satisfy a Sandbox operation."""

    def __init__(self, run: Mapping[str, Any], message: str) -> None:
        self.run = dict(run)
        super().__init__(message)


@dataclass(frozen=True)
class AcquireOptions:
    """Inputs used to acquire one ready Sandbox from a Runtime."""

    name: str | None = None
    generate_name: str | None = None
    source: Mapping[str, Any] | None = None
    artifact_inputs: Sequence[Mapping[str, Any]] = ()
    env: Mapping[str, str] = field(default_factory=dict)
    timeout_seconds: int | None = None
    session: Mapping[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class Command:
    """One workspace-relative process execution request."""

    argv: Sequence[str] = ()
    shell: str = ""
    working_directory: str = ""
    env: Mapping[str, str] = field(default_factory=dict)
    stdin: bytes = b""
    timeout_millis: int = 0

    def request_body(self) -> dict[str, Any]:
        return {
            "argv": list(self.argv),
            "shell": self.shell,
            "workingDirectory": self.working_directory,
            "env": dict(self.env),
            "stdin": base64.b64encode(self.stdin).decode(),
            "timeoutMillis": self.timeout_millis,
        }


@dataclass(frozen=True)
class CommandResult:
    """The bounded result of one command operation."""

    exit_code: int
    stdout: bytes = b""
    stderr: bytes = b""
    timed_out: bool = False


@dataclass(frozen=True)
class OperationEvent:
    sequence: int
    type: str
    value: Mapping[str, Any]


@dataclass(frozen=True)
class FileInfo:
    """A direct child of a workspace-relative directory."""

    path: str
    directory: bool
    size_bytes: int


@dataclass(frozen=True)
class ListFilesOptions:
    """Select one bounded page of direct workspace-relative children."""

    directory: str = ""
    limit: int = 0
    page_token: str = ""


@dataclass(frozen=True)
class FilePage:
    """One bounded directory listing response."""

    entries: list[FileInfo]
    next_page_token: str


@dataclass(frozen=True)
class LogLine:
    """One structured output or audit record emitted by owner runtimed."""

    run_uid: str
    stream: str
    message: str
    assigned_pod_uid: str = ""
    operation: str = ""
    outcome: str = ""
    status_code: str = ""
    exit_code: int | None = None
    timed_out: bool = False
    duration_milliseconds: int = 0


class SandboxClient:
    """Creates and manages Sandboxes backed by Session-mode Runs."""

    def __init__(
        self,
        runs: RunClient,
        gateway: GatewayTransport,
        *,
        logs: LogReader | None = None,
        bearer_token: str = "",
        poll_interval_seconds: float = _DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> None:
        if poll_interval_seconds <= 0:
            poll_interval_seconds = _DEFAULT_POLL_INTERVAL_SECONDS
        self._runs = runs
        self._gateway = gateway
        self._logs = logs
        self._bearer_token = bearer_token
        self._poll_interval_seconds = poll_interval_seconds

    def runtime(self, namespace: str, name: str) -> "Runtime":
        """Return one reusable Session Runtime pool."""
        return Runtime(self, namespace, name)

    def _create(self, namespace: str, runtime: str, options: AcquireOptions) -> "Sandbox":
        """Create the Run for Runtime.acquire_sandbox; never expose it publicly."""
        if not namespace or not runtime:
            raise ValueError("sandbox namespace and runtime are required")
        if not options.name and not options.generate_name:
            raise ValueError("sandbox name or generate_name is required")
        metadata: dict[str, Any] = {"namespace": namespace}
        if options.name:
            metadata["name"] = options.name
        if options.generate_name:
            metadata["generateName"] = options.generate_name
        spec: dict[str, Any] = {
            "runtime": runtime,
            "env": [{"name": name, "value": options.env[name]} for name in sorted(options.env)],
            "mode": {"session": dict(options.session)},
        }
        if options.source is not None:
            spec["source"] = dict(options.source)
        if options.artifact_inputs:
            spec["artifactInputs"] = [dict(value) for value in options.artifact_inputs]
        if options.timeout_seconds is not None:
            spec["timeout"] = f"{options.timeout_seconds}s"
        run = self._runs.create(
            namespace,
            {"apiVersion": "kruntimes.io/v1alpha1", "kind": "Run", "metadata": metadata, "spec": spec},
        )
        return Sandbox(self, run)

    def open(self, namespace: str, name: str) -> "Sandbox":
        """Open an existing Session Run without creating or registering it."""
        run = self._runs.get(namespace, name)
        if not _is_session_run(run):
            raise SandboxStateError(run, "Run is not a Session Run")
        return Sandbox(self, run)


class Runtime:
    """A reusable Runtime pool that atomically acquires ready Sandboxes."""

    def __init__(self, client: SandboxClient, namespace: str, name: str) -> None:
        self._client = client
        self._namespace = namespace
        self._name = name

    def acquire_sandbox(self, options: AcquireOptions, *, timeout_seconds: float | None = None) -> "Sandbox":
        if not self._namespace or not self._name:
            raise ValueError("sandbox Runtime namespace and name are required")
        sandbox = self._client._create(self._namespace, self._name, options)
        sandbox.wait(timeout_seconds)
        return sandbox


class Sandbox:
    """An opened Session-mode Run."""

    def __init__(self, client: SandboxClient, run: Mapping[str, Any]) -> None:
        self._client = client
        self._run = dict(run)

    @property
    def run(self) -> dict[str, Any]:
        """A shallow copy of the latest known Kubernetes Run object."""
        return dict(self._run)

    def refresh(self) -> None:
        """Read the latest Run status."""
        metadata = _metadata(self._run)
        self._run = self._client._runs.get(_namespace(metadata), _name(metadata))

    def wait(self, timeout_seconds: float | None = None) -> None:
        """Wait until Ready or a terminal Run phase."""
        deadline = _deadline(timeout_seconds)
        while True:
            self.refresh()
            phase = _phase(self._run)
            if phase == "Ready":
                return
            if phase in _TERMINAL_PHASES:
                raise SandboxStateError(self._run, "Session Run became terminal before it was ready")
            _sleep_until(deadline, self._client._poll_interval_seconds)

    def close(self, timeout_seconds: float | None = None) -> None:
        """Request graceful Session completion and wait for terminal lifecycle completion."""
        self._terminate("Drain", timeout_seconds)

    def cancel(self, timeout_seconds: float | None = None) -> None:
        """Request immediate Session cancellation and wait for terminal lifecycle completion."""
        self._terminate("Immediate", timeout_seconds)

    def release(self, timeout_seconds: float | None = None) -> None:
        """Drain the Session and remove its backing Run to release capacity."""
        self.close(timeout_seconds)
        metadata = _metadata(self._run)
        self._client._runs.delete(_namespace(metadata), _name(metadata))

    def _terminate(self, mode: str, timeout_seconds: float | None) -> None:
        self.refresh()
        spec = dict(self._run.get("spec", {}))
        termination = spec.get("termination") or {}
        current_mode = termination.get("mode")
        if current_mode is None or (current_mode == "Drain" and mode == "Immediate"):
            spec["termination"] = {"mode": mode}
            self._run["spec"] = spec
            metadata = _metadata(self._run)
            self._run = self._client._runs.replace(_namespace(metadata), _name(metadata), self._run)
        deadline = _deadline(timeout_seconds)
        while True:
            self.refresh()
            phase = _phase(self._run)
            if phase in _TERMINAL_PHASES:
                expected = "Succeeded" if mode == "Drain" else "Cancelled"
                if phase != expected:
                    action = "close" if mode == "Drain" else "cancel"
                    raise SandboxStateError(self._run, f"Session Run did not {action} successfully; phase is {phase}")
                return
            _sleep_until(deadline, self._client._poll_interval_seconds)

    def execute(self, command: Command) -> CommandResult:
        """Execute one command without implicit retry after transport failure."""
        response = self._operation({"command": command.request_body()})
        command_response = response.get("command")
        if not isinstance(command_response, Mapping):
            raise ValueError("gateway response did not include a command result")
        return CommandResult(
            exit_code=int(command_response.get("exitCode", 0)),
            stdout=_decode_bytes(command_response.get("stdout", "")),
            stderr=_decode_bytes(command_response.get("stderr", "")),
            timed_out=bool(command_response.get("timedOut", False)),
        )

    def open_session(self, *, timeout_seconds: float | None = None) -> "Session":
        """Open a persistent operation connection to this ready Sandbox.

        Closing the returned connection does not close this Sandbox or release
        its Runtime capacity. One command may be active on a Session at a time.
        """
        endpoint = _websocket_url(self._endpoint("operations:ws"))
        headers: dict[str, str] = {}
        if self._client._bearer_token:
            headers["Authorization"] = "Bearer " + self._client._bearer_token
        try:
            connection = self._client._gateway.open_session(endpoint, headers, timeout_seconds)
        except Exception as error:
            raise RuntimeError("open Sandbox Session") from error
        return Session(connection, _session_heartbeat_interval(self._run))

    def write_file(self, path: str, contents: bytes, *, create_parents: bool = False) -> None:
        self._operation({"writeFile": {"path": path, "contents": _encode_bytes(contents), "createParents": create_parents}})

    def create_directory(self, path: str) -> None:
        self._operation({"createDirectory": {"path": path}})

    def delete_file(self, path: str, *, recursive: bool = False) -> None:
        self._operation({"deleteFile": {"path": path, "recursive": recursive}})

    def rename_file(self, source_path: str, destination_path: str, *, overwrite: bool = False) -> None:
        self._operation({"renameFile": {"sourcePath": source_path, "destinationPath": destination_path, "overwrite": overwrite}})

    def read_file(self, path: str, *, max_bytes: int = 0) -> tuple[bytes, bool]:
        endpoint = self._endpoint("files")
        parsed = urlparse(endpoint)
        query = dict()
        if max_bytes > 0:
            query["maxBytes"] = str(max_bytes)
        file_url = urlunparse(parsed._replace(path=parsed.path.rstrip("/") + "/" + path, query=urlencode(query)))
        response = self._request("GET", file_url)
        return _decode_bytes(response.get("contents", "")), bool(response.get("truncated", False))

    def list_files(self, options: ListFilesOptions | None = None) -> FilePage:
        """Return one bounded page of direct workspace-relative children."""
        options = options or ListFilesOptions()
        if options.limit < 0:
            raise ValueError("file page limit cannot be negative")

        endpoint = self._endpoint("files")
        query: dict[str, str] = {}
        if options.directory:
            query["path"] = options.directory
        if options.limit > 0:
            query["limit"] = str(options.limit)
        if options.page_token:
            query["pageToken"] = options.page_token
        if query:
            endpoint += "?" + urlencode(query)
        response = self._request("GET", endpoint)
        entries = response.get("entries", [])
        if not isinstance(entries, list):
            raise ValueError("gateway response did not include file entries")
        return FilePage(
            entries=[FileInfo(path=str(entry.get("path", "")), directory=bool(entry.get("directory", False)), size_bytes=int(entry.get("sizeBytes", 0))) for entry in entries if isinstance(entry, Mapping)],
            next_page_token=str(response.get("nextPageToken", "")),
        )

    def logs(self) -> list[LogLine]:
        """Read structured owner-runtimed log records for this Sandbox only."""
        if self._client._logs is None:
            raise ValueError("sandbox log reader is not configured")
        self.refresh()
        status = self._run.get("status", {})
        pod = status.get("assignedPod", "") if isinstance(status, Mapping) else ""
        if not pod:
            raise SandboxStateError(self._run, "Session Run has no assigned Runtime Pod")
        raw = self._client._logs.read(_namespace(_metadata(self._run)), str(pod), "runtimed")
        run_uid = str(_metadata(self._run).get("uid", ""))
        result: list[LogLine] = []
        for line in raw.splitlines():
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if not isinstance(value, Mapping) or value.get("run_uid") != run_uid:
                continue
            result.append(LogLine(
                run_uid=run_uid,
                stream=str(value.get("stream", "")), message=str(value.get("message", "")),
                assigned_pod_uid=str(value.get("assigned_pod_uid", "")), operation=str(value.get("operation", "")),
                outcome=str(value.get("outcome", "")), status_code=str(value.get("status_code", "")),
                exit_code=value.get("exit_code"), timed_out=bool(value.get("timed_out", False)),
                duration_milliseconds=int(value.get("duration_milliseconds", 0)),
            ))
        return result

    def _operation(self, operation: Mapping[str, Any]) -> dict[str, Any]:
        return self._request("POST", self._endpoint("operations:execute"), operation)

    def _endpoint(self, suffix: str) -> str:
        status = self._run.get("status", {})
        endpoint = status.get("endpoint", {}) if isinstance(status, Mapping) else {}
        url = endpoint.get("url", "") if isinstance(endpoint, Mapping) else ""
        if _phase(self._run) != "Ready" or not isinstance(url, str) or not url:
            raise SandboxStateError(self._run, "Session Run is not ready")
        parsed = urlparse(url)
        uid = str(_metadata(self._run).get("uid", ""))
        components = [value for value in parsed.path.split("/") if value]
        if not parsed.scheme or not parsed.netloc or len(components) < 2 or components[-2:] != ["sessions", uid]:
            raise SandboxStateError(self._run, "Session Run endpoint does not match its UID")
        return urlunparse(parsed._replace(path=parsed.path.rstrip("/") + "/" + suffix))

    def _request(self, method: str, url: str, body: Mapping[str, Any] | None = None) -> dict[str, Any]:
        headers = {"Accept": "application/json"}
        encoded: bytes | None = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            encoded = json.dumps(body, separators=(",", ":")).encode()
        if self._client._bearer_token:
            headers["Authorization"] = "Bearer " + self._client._bearer_token
        response = self._client._gateway.request(method, url, encoded, headers)
        if not 200 <= response.status_code < 300:
            message = ""
            try:
                value = json.loads(response.body[: 1 << 20])
                if isinstance(value, Mapping):
                    message = str(value.get("error", ""))
            except json.JSONDecodeError:
                pass
            raise APIError(response.status_code, message)
        if not response.body:
            return {}
        try:
            value = json.loads(response.body)
        except json.JSONDecodeError as error:
            raise ValueError("decode gateway response") from error
        if not isinstance(value, dict):
            raise ValueError("gateway response must be an object")
        return value


class Session:
    """One replaceable, persistent connection to a ready Sandbox.

    This object owns only the WebSocket. Call :meth:`Sandbox.release` to
    terminate the backing Run and return its Runtime capacity.
    """

    def __init__(self, connection: SessionConnection, heartbeat_interval_seconds: float | None = None) -> None:
        self._connection = connection
        self._active_operation_id = ""
        self._closed = False
        self._write_lock = threading.Lock()
        self._heartbeat_stop = threading.Event()
        self._heartbeat_thread: threading.Thread | None = None
        if heartbeat_interval_seconds is not None:
            self._heartbeat_thread = threading.Thread(target=self._heartbeat, args=(heartbeat_interval_seconds,), daemon=True)
            self._heartbeat_thread.start()

    def send(self, command: Command, *, idempotency_key: str = "") -> str:
        """Submit one command and return after its ``accepted`` event."""
        if self._closed:
            raise RuntimeError("Sandbox Session is closed")
        if self._active_operation_id:
            raise RuntimeError("Sandbox Session already has an active operation")
        payload: dict[str, Any] = {
            "type": _SESSION_FRAME_TYPE_SEND,
            "operation": {"command": command.request_body()},
        }
        if idempotency_key:
            payload["idempotencyKey"] = idempotency_key
        self._send(payload)
        event = self._read_event()
        accepted = event.value.get("accepted") if event.type == "accepted" else None
        operation_id = accepted.get("operationID", "") if isinstance(accepted, Mapping) else ""
        if not isinstance(operation_id, str) or not operation_id:
            raise ValueError("Sandbox Session did not return an accepted operation")
        self._active_operation_id = operation_id
        return operation_id

    def receive(self) -> OperationEvent:
        """Receive the next event for the active command."""
        if self._closed:
            raise RuntimeError("Sandbox Session is closed")
        if not self._active_operation_id:
            raise RuntimeError("Sandbox Session has no active operation")
        event = self._read_event()
        if event.type in ("completed", "failed"):
            self._active_operation_id = ""
        return event

    def cancel(self, operation_id: str) -> None:
        """Request cancellation of the currently active command."""
        if self._closed or not operation_id or operation_id != self._active_operation_id:
            raise RuntimeError("Sandbox Session operation is not active")
        self._send({"type": _SESSION_FRAME_TYPE_CANCEL, "operationID": operation_id})

    def close(self) -> None:
        """Close only the connection; the backing Sandbox remains allocated."""
        if self._closed:
            return
        self._closed = True
        self._heartbeat_stop.set()
        if self._heartbeat_thread is not None and self._heartbeat_thread is not threading.current_thread():
            self._heartbeat_thread.join()
        self._connection.close()

    def _send(self, payload: Mapping[str, Any]) -> None:
        with self._write_lock:
            self._connection.send(json.dumps(payload, separators=(",", ":")))

    def _heartbeat(self, interval_seconds: float) -> None:
        while not self._heartbeat_stop.wait(interval_seconds):
            if self._closed:
                return
            try:
                self._send({"type": _SESSION_FRAME_TYPE_HEARTBEAT})
            except Exception:
                self._closed = True
                self._connection.close()
                return

    def _read_event(self) -> OperationEvent:
        raw = self._connection.recv()
        if isinstance(raw, bytes):
            raw = raw.decode()
        try:
            value = json.loads(raw)
        except (TypeError, json.JSONDecodeError) as error:
            raise ValueError("decode Sandbox Session frame") from error
        if not isinstance(value, Mapping):
            raise ValueError("Sandbox Session frame must be an object")
        if value.get("type") == "error":
            raise RuntimeError(str(value.get("error", "Sandbox Session failed")))
        return OperationEvent(int(value.get("sequence", 0)), str(value.get("type", "")), value)


def _is_session_run(run: Mapping[str, Any]) -> bool:
    spec = run.get("spec", {})
    mode = spec.get("mode", {}) if isinstance(spec, Mapping) else {}
    return isinstance(mode, Mapping) and isinstance(mode.get("session"), Mapping)


def _metadata(run: Mapping[str, Any]) -> Mapping[str, Any]:
    metadata = run.get("metadata", {})
    if not isinstance(metadata, Mapping):
        raise ValueError("Run has invalid metadata")
    return metadata


def _namespace(metadata: Mapping[str, Any]) -> str:
    namespace = metadata.get("namespace", "")
    if not isinstance(namespace, str) or not namespace:
        raise ValueError("Run has no namespace")
    return namespace


def _name(metadata: Mapping[str, Any]) -> str:
    name = metadata.get("name", "")
    if not isinstance(name, str) or not name:
        raise ValueError("Run has no name")
    return name


def _phase(run: Mapping[str, Any]) -> str:
    status = run.get("status", {})
    return str(status.get("phase", "")) if isinstance(status, Mapping) else ""


def _encode_bytes(value: bytes) -> str:
    return base64.b64encode(value).decode()


def _decode_bytes(value: Any) -> bytes:
    if not isinstance(value, str):
        return b""
    return base64.b64decode(value)


def _websocket_url(endpoint: str) -> str:
    parsed = urlparse(endpoint)
    if parsed.scheme == "http":
        return urlunparse(parsed._replace(scheme="ws"))
    if parsed.scheme == "https":
        return urlunparse(parsed._replace(scheme="wss"))
    raise ValueError("Sandbox Session endpoint must use HTTP or HTTPS")


def _session_heartbeat_interval(run: Mapping[str, Any]) -> float | None:
    spec = run.get("spec", {})
    mode = spec.get("mode", {}) if isinstance(spec, Mapping) else {}
    session = mode.get("session", {}) if isinstance(mode, Mapping) else {}
    value = session.get("leaseTimeoutSeconds") if isinstance(session, Mapping) else None
    if not isinstance(value, int) or value <= 0:
        return None
    return min(max(value / 3, 0.1), 30.0)


def _deadline(timeout_seconds: float | None) -> float | None:
    return None if timeout_seconds is None else time.monotonic() + timeout_seconds


def _sleep_until(deadline: float | None, interval: float) -> None:
    if deadline is None:
        time.sleep(interval)
        return
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise TimeoutError("sandbox operation timed out")
    time.sleep(min(interval, remaining))
