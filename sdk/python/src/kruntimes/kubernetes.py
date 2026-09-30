"""Kubernetes and local port-forward adapters for :mod:`kruntimes.sandbox`.

Install ``kruntimes-sdk[kubernetes]`` to use these helpers. The adapters keep
Kubernetes API access and Runtime gateway access separate: CustomObjects API
calls manage Runs, while the gateway receives the same caller identity token.
"""

from __future__ import annotations

import socket
import ssl
import subprocess
import time
from collections.abc import Mapping
from typing import Any
from urllib.error import HTTPError
from urllib.parse import urlparse, urlunparse
from urllib.request import Request, urlopen

from .sandbox import GatewayTransport, HTTPResponse, LogReader, RunClient, SandboxClient, SessionConnection

_GROUP = "kruntimes.io"
_VERSION = "v1alpha1"
_PLURAL = "runs"


class KubernetesRunClient(RunClient):
    """RunClient backed by Kubernetes CustomObjectsApi."""

    def __init__(self, custom_objects: Any) -> None:
        self._custom_objects = custom_objects

    def create(self, namespace: str, run: Mapping[str, Any]) -> dict[str, Any]:
        return self._custom_objects.create_namespaced_custom_object(
            _GROUP, _VERSION, namespace, _PLURAL, dict(run)
        )

    def get(self, namespace: str, name: str) -> dict[str, Any]:
        return self._custom_objects.get_namespaced_custom_object(
            _GROUP, _VERSION, namespace, _PLURAL, name
        )

    def replace(self, namespace: str, name: str, run: Mapping[str, Any]) -> dict[str, Any]:
        return self._custom_objects.replace_namespaced_custom_object(
            _GROUP, _VERSION, namespace, _PLURAL, name, dict(run)
        )

    def delete(self, namespace: str, name: str) -> None:
        self._custom_objects.delete_namespaced_custom_object(_GROUP, _VERSION, namespace, _PLURAL, name)


class KubernetesLogReader(LogReader):
    """LogReader backed by Kubernetes CoreV1Api Pod logs."""

    def __init__(self, core_v1: Any) -> None:
        self._core_v1 = core_v1

    def read(self, namespace: str, pod: str, container: str) -> str:
        return str(
            self._core_v1.read_namespaced_pod_log(
                name=pod, namespace=namespace, container=container
            )
        )


class UrllibGatewayTransport(GatewayTransport):
    """Gateway transport that uses Python's standard library HTTP client."""

    def __init__(self, *, websocket_sslopt: Mapping[str, Any] | None = None) -> None:
        self._websocket_sslopt = dict(websocket_sslopt or {})

    def request(
        self, method: str, url: str, body: bytes | None, headers: Mapping[str, str]
    ) -> HTTPResponse:
        request = Request(url, data=body, headers=dict(headers), method=method)
        try:
            with urlopen(request) as response:  # noqa: S310 - caller controls endpoint from Run status
                return HTTPResponse(response.status, response.read(1 << 20))
        except HTTPError as error:
            return HTTPResponse(error.code, error.read(1 << 20))

    def open_session(self, url: str, headers: Mapping[str, str], timeout_seconds: float | None) -> SessionConnection:
        try:
            import websocket
        except ImportError as error:
            raise ImportError("install kruntimes-sdk WebSocket dependencies") from error
        header = [f"{name}: {value}" for name, value in headers.items()]
        return websocket.create_connection(url, header=header, timeout=timeout_seconds, sslopt=self._websocket_sslopt)



class PortForwardGatewayTransport(GatewayTransport):
    """Route gateway requests through a scoped local ``kubectl port-forward``.

    The Run endpoint remains authoritative for its path and query. This adapter
    only replaces the URL scheme and authority with the local forwarded socket.
    """

    def __init__(self, upstream: GatewayTransport, local_url: str, process: subprocess.Popen[bytes] | None = None) -> None:
        parsed = urlparse(local_url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise ValueError("local gateway URL must include scheme and host")
        self._upstream = upstream
        self._local_url = local_url.rstrip("/")
        self._process = process

    @classmethod
    def start(
        cls,
        *,
        namespace: str,
        service: str,
        service_port: int,
        kubectl: str = "kubectl",
        context: str | None = None,
        kubeconfig: str | None = None,
        startup_timeout_seconds: float = 10,
        upstream: GatewayTransport | None = None,
    ) -> "PortForwardGatewayTransport":
        """Start a port-forward to one shared Runtime gateway Service."""
        local_port = _available_local_port()
        command = [kubectl]
        if context:
            command.extend(("--context", context))
        if kubeconfig:
            command.extend(("--kubeconfig", kubeconfig))
        command.extend((
            "--namespace", namespace,
            "port-forward", f"service/{service}", f"{local_port}:{service_port}",
            "--address", "127.0.0.1",
        ))
        process = subprocess.Popen(command, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        try:
            _wait_for_local_port(process, local_port, startup_timeout_seconds)
        except BaseException:
            process.terminate()
            process.wait(timeout=5)
            raise
        return cls(upstream or UrllibGatewayTransport(), f"http://127.0.0.1:{local_port}", process)

    def request(
        self, method: str, url: str, body: bytes | None, headers: Mapping[str, str]
    ) -> HTTPResponse:
        if self._process is not None and self._process.poll() is not None:
            raise RuntimeError("Runtime gateway port-forward exited")
        original = urlparse(url)
        local = urlparse(self._local_url)
        local_url = urlunparse(original._replace(scheme=local.scheme, netloc=local.netloc))
        return self._upstream.request(method, local_url, body, headers)

    def open_session(self, url: str, headers: Mapping[str, str], timeout_seconds: float | None) -> SessionConnection:
        if self._process is not None and self._process.poll() is not None:
            raise RuntimeError("Runtime gateway port-forward exited")
        original = urlparse(url)
        local = urlparse(self._local_url)
        scheme = "wss" if local.scheme == "https" else "ws"
        local_url = urlunparse(original._replace(scheme=scheme, netloc=local.netloc))
        return self._upstream.open_session(local_url, headers, timeout_seconds)

    def close(self) -> None:
        """Stop the scoped port-forward process, if this transport started one."""
        if self._process is None or self._process.poll() is not None:
            return
        self._process.terminate()
        try:
            self._process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self._process.kill()
            self._process.wait(timeout=5)

    def __enter__(self) -> "PortForwardGatewayTransport":
        return self

    def __exit__(self, exc_type: object, exc_value: object, traceback: object) -> None:
        self.close()


def from_incluster(*, gateway: GatewayTransport | None = None, poll_interval_seconds: float = 0.5) -> SandboxClient:
    """Build a SandboxClient from the pod ServiceAccount and Kubernetes APIs."""
    kubernetes = _load_kubernetes()
    kubernetes.config.load_incluster_config()
    return _from_api_client(kubernetes.client.ApiClient(), gateway, poll_interval_seconds)


def from_kube_config(
    *, gateway: GatewayTransport | None = None, context: str | None = None, kubeconfig: str | None = None,
    poll_interval_seconds: float = 0.5,
) -> SandboxClient:
    """Build a SandboxClient from the active local kubeconfig context.

    For local access, pass a ``PortForwardGatewayTransport`` returned by
    :meth:`PortForwardGatewayTransport.start` as ``gateway``.
    """
    kubernetes = _load_kubernetes()
    kubernetes.config.load_kube_config(config_file=kubeconfig, context=context)
    return _from_api_client(kubernetes.client.ApiClient(), gateway, poll_interval_seconds)


def _from_api_client(api_client: Any, gateway: GatewayTransport | None, poll_interval_seconds: float) -> SandboxClient:
    kubernetes = _load_kubernetes()
    configuration = api_client.configuration
    token = configuration.get_api_key_with_prefix("authorization") or ""
    if token.lower().startswith("bearer "):
        token = token[7:]
    return SandboxClient(
        KubernetesRunClient(kubernetes.client.CustomObjectsApi(api_client)),
        gateway or UrllibGatewayTransport(websocket_sslopt=_websocket_sslopt(configuration)),
        logs=KubernetesLogReader(kubernetes.client.CoreV1Api(api_client)),
        bearer_token=token,
        poll_interval_seconds=poll_interval_seconds,
    )


def _websocket_sslopt(configuration: Any) -> dict[str, Any]:
    """Translate Kubernetes client TLS settings for websocket-client."""
    options: dict[str, Any] = {}
    if not bool(getattr(configuration, "verify_ssl", True)):
        options["cert_reqs"] = ssl.CERT_NONE
        options["check_hostname"] = False
    if value := getattr(configuration, "ssl_ca_cert", None):
        options["ca_certs"] = value
    if value := getattr(configuration, "cert_file", None):
        options["certfile"] = value
    if value := getattr(configuration, "key_file", None):
        options["keyfile"] = value
    return options


def _load_kubernetes() -> Any:
    try:
        import kubernetes
    except ImportError as error:
        raise ImportError("install kruntimes-sdk[kubernetes] to use Kubernetes SDK adapters") from error
    return kubernetes


def _available_local_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _wait_for_local_port(process: subprocess.Popen[bytes], port: int, timeout_seconds: float) -> None:
    deadline = time.monotonic() + timeout_seconds
    while time.monotonic() < deadline:
        if process.poll() is not None:
            stderr = process.stderr.read().decode(errors="replace") if process.stderr else ""
            raise RuntimeError(f"Runtime gateway port-forward exited: {stderr.strip()}")
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.settimeout(0.1)
            if sock.connect_ex(("127.0.0.1", port)) == 0:
                return
        time.sleep(0.05)
    raise TimeoutError("timed out waiting for Runtime gateway port-forward")
