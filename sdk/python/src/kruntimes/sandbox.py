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
from typing import Any, Callable, Mapping, Protocol, Sequence
from urllib.parse import urlencode, urlparse, urlunparse


_TERMINAL_PHASES = frozenset(("Succeeded", "Failed", "Timeout", "Cancelled"))
_DEFAULT_POLL_INTERVAL_SECONDS = 0.5
_SESSION_FRAME_TYPE_SEND = "send"
_SESSION_FRAME_TYPE_CANCEL = "cancel"
_SESSION_FRAME_TYPE_HEARTBEAT = "heartbeat"
# The watcher polls because threading.Event cannot wait on two events at once.
_CANCEL_POLL_INTERVAL_SECONDS = 0.02


class RunClient(Protocol):
    """Kubernetes boundary used to create and manage Run custom resources."""

    def create(self, namespace: str, run: Mapping[str, Any]) -> dict[str, Any]: ...

    def get(self, namespace: str, name: str) -> dict[str, Any]: ...

    def replace(self, namespace: str, name: str, run: Mapping[str, Any]) -> dict[str, Any]: ...

    def delete(self, namespace: str, name: str) -> None: ...


class APITransport(Protocol):
    """HTTP and WebSocket boundary for the aggregated Run extension API."""

    def request(
        self, method: str, url: str, body: bytes | None, headers: Mapping[str, str]
    ) -> "HTTPResponse": ...

    def open_session(self, url: str, headers: Mapping[str, str], timeout_seconds: float | None) -> "SessionConnection": ...


class SessionConnection(Protocol):
    """One bidirectional aggregated Session API WebSocket connection."""

    def send(self, payload: str) -> None: ...

    def recv(self) -> str | bytes: ...

    def close(self) -> None: ...



@dataclass(frozen=True)
class HTTPResponse:
    """A bounded Run extension API HTTP response."""

    status_code: int
    body: bytes = b""



class APIError(Exception):
    """A non-success Run extension API response."""

    def __init__(self, status_code: int, message: str) -> None:
        self.status_code = status_code
        self.message = message
        super().__init__(f"Run extension API returned HTTP {status_code}: {message}")


class SandboxStateError(Exception):
    """A Run lifecycle state that cannot satisfy a Sandbox operation."""

    def __init__(self, run: Mapping[str, Any], message: str) -> None:
        self.run = dict(run)
        super().__init__(message)


class OperationFailedError(Exception):
    """A terminal ``failed`` event emitted by the Runtime Server.

    This is distinct from :class:`SessionTransportError`: the operation ran and
    the Runtime Server reported a terminal failure, so replaying it would repeat
    side effects.
    """

    def __init__(self, code: int, message: str) -> None:
        self.code = code
        self.message = message
        super().__init__(f"Session operation failed ({code}): {message}")


class SessionTransportError(Exception):
    """A Session connection failure outside a terminal operation event.

    The operation outcome is unknown after this error; callers must not replay
    the operation. It is distinct from :class:`OperationFailedError` (a Runtime
    Server terminal event) and :class:`OperationCanceledError`.
    """

    def __init__(self, message: str, cause: BaseException | None = None) -> None:
        self.cause = cause
        super().__init__(message)


class OperationCanceledError(Exception):
    """A Session operation canceled through the caller's cancel event.

    Cancellation is requested from another thread by setting the caller's
    ``threading.Event``; the terminal failure is reported with this type rather
    than :class:`OperationFailedError` or :class:`SessionTransportError`.
    """

    def __init__(self, operation_id: str = "") -> None:
        self.operation_id = operation_id
        super().__init__("Session operation canceled")


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
class FileWrite:
    """Write one workspace-relative file."""

    path: str
    contents: bytes = b""
    create_parents: bool = False

    def request_body(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "contents": _encode_bytes(self.contents),
            "createParents": self.create_parents,
        }


@dataclass(frozen=True)
class DirectoryCreate:
    """Create one workspace-relative directory."""

    path: str

    def request_body(self) -> dict[str, Any]:
        return {"path": self.path}


@dataclass(frozen=True)
class FileDelete:
    """Delete one workspace-relative path."""

    path: str
    recursive: bool = False

    def request_body(self) -> dict[str, Any]:
        return {"path": self.path, "recursive": self.recursive}


@dataclass(frozen=True)
class FileRename:
    """Rename one workspace-relative path."""

    source_path: str
    destination_path: str
    overwrite: bool = False

    def request_body(self) -> dict[str, Any]:
        return {
            "sourcePath": self.source_path,
            "destinationPath": self.destination_path,
            "overwrite": self.overwrite,
        }


@dataclass(frozen=True)
class Operation:
    """One mutation submitted through a persistent Session connection.

    The file fields accept either the typed dataclasses or a raw wire mapping;
    the raw mapping form is kept for backward compatibility.
    """

    command: Command | None = None
    write_file: FileWrite | Mapping[str, Any] | None = None
    create_directory: DirectoryCreate | Mapping[str, Any] | None = None
    delete_file: FileDelete | Mapping[str, Any] | None = None
    rename_file: FileRename | Mapping[str, Any] | None = None

    def request_body(self) -> dict[str, Any]:
        values: dict[str, Any] = {}
        if self.command is not None:
            values["command"] = self.command.request_body()
        if self.write_file is not None:
            values["writeFile"] = _request_body(self.write_file)
        if self.create_directory is not None:
            values["createDirectory"] = _request_body(self.create_directory)
        if self.delete_file is not None:
            values["deleteFile"] = _request_body(self.delete_file)
        if self.rename_file is not None:
            values["renameFile"] = _request_body(self.rename_file)
        if len(values) != 1:
            raise ValueError("exactly one Session operation is required")
        return values


@dataclass(frozen=True)
class OperationOutput:
    """Bounded stdout or stderr bytes carried by one ``output`` event."""

    stream: str
    data: bytes = b""


@dataclass(frozen=True)
class OperationProgress:
    """One Runtime-defined bounded progress event."""

    kind: str
    message: str = ""
    tool_call_id: str = ""
    tool_name: str = ""
    data: bytes = b""
    content_type: str = ""


@dataclass(frozen=True)
class OperationFailure:
    """A terminal gRPC-compatible operation failure."""

    code: int
    message: str = ""


@dataclass(frozen=True)
class OperationEvent:
    sequence: int
    type: str
    value: Mapping[str, Any]

    @property
    def output(self) -> OperationOutput | None:
        """The typed payload of an ``output`` event, if present."""
        payload = self.value.get("output")
        if not isinstance(payload, Mapping):
            return None
        return OperationOutput(
            stream=str(payload.get("stream", "")),
            data=_decode_bytes(payload.get("data")),
        )

    @property
    def progress(self) -> OperationProgress | None:
        """The typed payload of a ``progress`` event, if present."""
        payload = self.value.get("progress")
        if not isinstance(payload, Mapping):
            return None
        return OperationProgress(
            kind=str(payload.get("kind", "")),
            message=str(payload.get("message", "")),
            tool_call_id=str(payload.get("toolCallID", "")),
            tool_name=str(payload.get("toolName", "")),
            data=_decode_bytes(payload.get("data")),
            content_type=str(payload.get("contentType", "")),
        )

    @property
    def failed(self) -> OperationFailure | None:
        """The typed payload of a ``failed`` event, if present."""
        payload = self.value.get("failed")
        if not isinstance(payload, Mapping):
            return None
        return OperationFailure(
            code=int(payload.get("code", 0)),
            message=str(payload.get("message", "")),
        )

    @property
    def command(self) -> CommandResult | None:
        """The command result carried by a ``completed`` event, if any."""
        completed = self.value.get("completed")
        if not isinstance(completed, Mapping):
            return None
        payload = completed.get("command")
        if not isinstance(payload, Mapping):
            return None
        return CommandResult(
            exit_code=int(payload.get("exitCode", 0)),
            stdout=_decode_bytes(payload.get("stdout")),
            stderr=_decode_bytes(payload.get("stderr")),
            timed_out=bool(payload.get("timedOut", False)),
        )


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
        transport: APITransport,
        *,
        api_server: str,
        bearer_token: str = "",
        poll_interval_seconds: float = _DEFAULT_POLL_INTERVAL_SECONDS,
    ) -> None:
        if not api_server:
            raise ValueError("Kubernetes API server URL is required")
        if poll_interval_seconds <= 0:
            poll_interval_seconds = _DEFAULT_POLL_INTERVAL_SECONDS
        self._runs = runs
        self._transport = transport
        self._api_server = api_server.rstrip("/")
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
        try:
            sandbox.wait(timeout_seconds)
        except Exception as error:
            metadata = _metadata(sandbox.run)
            try:
                self._client._runs.delete(_namespace(metadata), _name(metadata))
            except Exception as cleanup_error:
                raise RuntimeError(
                    f"acquire Sandbox failed and deleting Run {_name(metadata)} failed: {cleanup_error}"
                ) from error
            raise
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

    def open_session(self, *, timeout_seconds: float | None = None) -> "Session":
        """Open a persistent operation connection to this ready Sandbox.

        Closing the returned connection does not close this Sandbox or release
        its Runtime capacity. One command may be active on a Session at a time.
        """
        endpoint = _websocket_url(self._session_base())
        headers: dict[str, str] = {}
        if self._client._bearer_token:
            headers["Authorization"] = "Bearer " + self._client._bearer_token
        try:
            connection = self._client._transport.open_session(endpoint, headers, timeout_seconds)
        except Exception as error:
            raise RuntimeError("open Sandbox Session") from error
        return Session(connection, _session_heartbeat_interval(self._run))

    def run_command(
        self,
        command: Command | str | Sequence[str],
        *,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_progress: Callable[[OperationProgress], None] | None = None,
        cancel_event: threading.Event | None = None,
    ) -> CommandResult:
        """Run exactly one command on a short-lived Session and return its result.

        A string is submitted as a shell command and a sequence as argv. Output
        is accumulated while the callbacks receive each chunk incrementally. A
        non-zero exit code or a command timeout is a normal result, not an
        exception. A ``failed`` event raises :class:`OperationFailedError`, a
        broken connection raises :class:`SessionTransportError` (the outcome is
        unknown; the operation is never replayed), and a cancel event raises
        :class:`OperationCanceledError`.

        This helper opens and closes only its own connection. It never closes or
        releases the backing Sandbox.
        """
        result = self._execute_operation(
            Operation(command=_normalize_command(command)),
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_progress=on_progress,
            cancel_event=cancel_event,
        )
        if result is None:
            # A command operation must terminalize with a command result; a
            # completed event without one is a protocol violation, not a
            # transport failure.
            raise RuntimeError("Session command completed without a command result")
        return result

    def write_file(self, path: str, contents: bytes, *, create_parents: bool = False) -> None:
        """Write one file through a short-lived Session; never releases the Sandbox."""
        self._execute_operation(Operation(write_file=FileWrite(path, bytes(contents), create_parents)))

    def create_directory(self, path: str) -> None:
        """Create one directory through a short-lived Session; never releases the Sandbox."""
        self._execute_operation(Operation(create_directory=DirectoryCreate(path)))

    def delete_file(self, path: str, *, recursive: bool = False) -> None:
        """Delete one path through a short-lived Session; never releases the Sandbox."""
        self._execute_operation(Operation(delete_file=FileDelete(path, recursive)))

    def rename_file(self, source: str, destination: str, *, overwrite: bool = False) -> None:
        """Rename one path through a short-lived Session; never releases the Sandbox."""
        self._execute_operation(Operation(rename_file=FileRename(source, destination, overwrite)))

    def _execute_operation(
        self,
        operation: Operation,
        *,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_progress: Callable[[OperationProgress], None] | None = None,
        cancel_event: threading.Event | None = None,
    ) -> CommandResult | None:
        """Submit one operation over a one-shot Session and consume its stream.

        The Sandbox was already opened; this helper closes only the new
        connection. A cancellation request is delivered as one cancel frame from
        a daemon watcher thread, and the operation is never replayed, including
        after an uncertain transport outcome.
        """
        # Validate lifecycle state before opening the connection so callers keep
        # the typed SandboxStateError boundary from the existing API.
        self._session_base()
        try:
            session = self.open_session()
        except SessionTransportError:
            raise
        except SandboxStateError:
            raise
        except Exception as error:
            raise SessionTransportError("open Sandbox Session", error) from error

        stop = threading.Event()
        cancelled = threading.Event()
        cancel_lock = threading.Lock()
        stdout = bytearray()
        stderr = bytearray()
        operation_id = ""
        watcher: threading.Thread | None = None

        def request_cancel() -> None:
            with cancel_lock:
                if cancelled.is_set():
                    return
                cancelled.set()
            try:
                session.cancel(operation_id)
            except Exception:
                # The connection may already be gone. The local cancellation
                # intent still determines the error the caller observes.
                pass

        try:
            try:
                operation_id = session.send(operation)
            except Exception as error:
                raise SessionTransportError("send Sandbox Session operation", error) from error
            if cancel_event is not None:
                if cancel_event.is_set():
                    request_cancel()
                watcher = threading.Thread(
                    target=_watch_cancel,
                    args=(cancel_event, stop, request_cancel),
                    daemon=True,
                )
                watcher.start()
            while True:
                if cancel_event is not None and cancel_event.is_set():
                    request_cancel()
                try:
                    event = session.receive()
                except Exception as error:
                    if cancelled.is_set():
                        raise OperationCanceledError(operation_id) from error
                    raise SessionTransportError("receive Sandbox Session event", error) from error
                output = event.output
                if output is not None:
                    if output.stream == "stdout":
                        stdout.extend(output.data)
                        if on_stdout is not None:
                            on_stdout(output.data)
                    elif output.stream == "stderr":
                        stderr.extend(output.data)
                        if on_stderr is not None:
                            on_stderr(output.data)
                progress = event.progress
                if progress is not None and on_progress is not None:
                    on_progress(progress)
                failed = event.failed
                if event.type == "failed" or failed is not None:
                    if cancelled.is_set():
                        raise OperationCanceledError(operation_id)
                    failure = failed if failed is not None else OperationFailure(0, "Session operation failed")
                    raise OperationFailedError(failure.code, failure.message)
                if event.type == "completed":
                    if cancelled.is_set():
                        raise OperationCanceledError(operation_id)
                    result = event.command
                    if result is None:
                        return None
                    # Runtime Servers stream command output, so the terminal
                    # event may omit bytes already delivered as output events.
                    return CommandResult(
                        exit_code=result.exit_code,
                        stdout=bytes(stdout) if stdout else result.stdout,
                        stderr=bytes(stderr) if stderr else result.stderr,
                        timed_out=result.timed_out,
                    )
                # accepted and unknown events carry no terminal meaning.
        finally:
            stop.set()
            if watcher is not None:
                watcher.join()
            try:
                session.close()
            except Exception:
                # Closing only this connection is best-effort; it must never
                # mask the operation's terminal error or result.
                pass

    def read_file(self, path: str, *, max_bytes: int = 0) -> tuple[bytes, bool]:
        endpoint = self._session_base() + "/files"
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

        endpoint = self._session_base() + "/files"
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
            raise ValueError("Run extension API response did not include file entries")
        return FilePage(
            entries=[FileInfo(path=str(entry.get("path", "")), directory=bool(entry.get("directory", False)), size_bytes=int(entry.get("sizeBytes", 0))) for entry in entries if isinstance(entry, Mapping)],
            next_page_token=str(response.get("nextPageToken", "")),
        )

    def logs(self, *, tail_lines: int = 100) -> list[LogLine]:
        """Read structured Run log records through the aggregated logs API."""
        self.refresh()
        if tail_lines <= 0:
            raise ValueError("log tail_lines must be positive")
        metadata = _metadata(self._run)
        status = self._run.get("status", {})
        assigned_pod_uid = str(status.get("assignedPodUID", "")) if isinstance(status, Mapping) else ""
        url = (
            f"{self._client._api_server}/apis/logs.kruntimes.io/v1alpha1"
            f"/namespaces/{_namespace(metadata)}/runs/{_name(metadata)}/log?tailLines={tail_lines}"
        )
        headers = {"Accept": "application/x-ndjson"}
        if self._client._bearer_token:
            headers["Authorization"] = "Bearer " + self._client._bearer_token
        response = self._client._transport.request("GET", url, None, headers)
        if not 200 <= response.status_code < 300:
            message = ""
            try:
                value = json.loads(response.body[: 1 << 20])
                if isinstance(value, Mapping):
                    message = str(value.get("error", "") or value.get("message", ""))
            except json.JSONDecodeError:
                pass
            raise APIError(response.status_code, message)
        run_uid = str(metadata.get("uid", ""))
        result: list[LogLine] = []
        for raw in response.body.decode().splitlines():
            try:
                value = json.loads(raw)
            except json.JSONDecodeError:
                continue
            if not isinstance(value, Mapping):
                continue
            result.append(LogLine(
                run_uid=run_uid,
                assigned_pod_uid=assigned_pod_uid,
                stream=str(value.get("stream", "")),
                message=str(value.get("message", "")),
                operation=str(value.get("operation", "")),
                outcome=str(value.get("outcome", "")),
                status_code=str(value.get("statusCode", "")),
                exit_code=value.get("exitCode"),
                timed_out=bool(value.get("timedOut", False)),
                duration_milliseconds=int(value.get("durationMilliseconds", 0)),
            ))
        return result

    def _session_base(self) -> str:
        metadata = _metadata(self._run)
        if _phase(self._run) != "Ready":
            raise SandboxStateError(self._run, "Session Run is not ready")
        return (
            f"{self._client._api_server}/apis/sessions.kruntimes.io/v1alpha1"
            f"/namespaces/{_namespace(metadata)}/runs/{_name(metadata)}/session"
        )

    def _request(self, method: str, url: str, body: Mapping[str, Any] | None = None) -> dict[str, Any]:
        headers = {"Accept": "application/json"}
        encoded: bytes | None = None
        if body is not None:
            headers["Content-Type"] = "application/json"
            encoded = json.dumps(body, separators=(",", ":")).encode()
        if self._client._bearer_token:
            headers["Authorization"] = "Bearer " + self._client._bearer_token
        response = self._client._transport.request(method, url, encoded, headers)
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
            raise ValueError("decode Run extension API response") from error
        if not isinstance(value, dict):
            raise ValueError("Run extension API response must be an object")
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

    def send(self, operation: Operation, *, idempotency_key: str = "") -> str:
        """Submit one operation and return after its ``accepted`` event."""
        if self._closed:
            raise RuntimeError("Sandbox Session is closed")
        if self._active_operation_id:
            raise RuntimeError("Sandbox Session already has an active operation")
        payload: dict[str, Any] = {
            "type": _SESSION_FRAME_TYPE_SEND,
            "operation": operation.request_body(),
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
        try:
            event = self._read_event()
        except RuntimeError:
            self._active_operation_id = ""
            raise
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
        self._connection.close()
        if self._heartbeat_thread is not None and self._heartbeat_thread is not threading.current_thread():
            self._heartbeat_thread.join()

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


def _normalize_command(command: Command | str | Sequence[str]) -> Command:
    if isinstance(command, Command):
        return command
    if isinstance(command, str):
        return Command(shell=command)
    if isinstance(command, Sequence) and not isinstance(command, (bytes, bytearray)):
        return Command(argv=list(command))
    raise TypeError("command must be a Command, str, or sequence of strings")


def _request_body(
    value: FileWrite | DirectoryCreate | FileDelete | FileRename | Mapping[str, Any],
) -> dict[str, Any]:
    if isinstance(value, (FileWrite, DirectoryCreate, FileDelete, FileRename)):
        return value.request_body()
    if isinstance(value, Mapping):
        return dict(value)
    raise TypeError("Session file operation must be a typed request or a mapping")


def _watch_cancel(
    cancel_event: threading.Event,
    stop_event: threading.Event,
    request_cancel: Callable[[], None],
) -> None:
    while not stop_event.wait(_CANCEL_POLL_INTERVAL_SECONDS):
        if cancel_event.is_set():
            request_cancel()
            return


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
