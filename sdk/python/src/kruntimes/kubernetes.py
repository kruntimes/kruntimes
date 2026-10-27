"""Kubernetes adapters for :mod:`kruntimes.sandbox`.

Install ``kruntimes-sdk[kubernetes]`` to use these helpers. The adapters keep
Kubernetes API access on the kubeconfig identity: CustomObjects API calls manage
Runs, and the aggregated Run extension API is reached through kube-apiserver with
the same caller token or client certificate.
"""

from __future__ import annotations

import ssl
from collections.abc import Mapping
from typing import Any
from urllib.error import HTTPError
from urllib.request import Request, urlopen

from .sandbox import APITransport, HTTPResponse, LogReader, RunClient, SandboxClient, SessionConnection

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


class UrllibAPITransport(APITransport):
    """Run extension API transport using the Kubernetes REST configuration.

    Every request goes through kube-apiserver, so the transport reuses the
    kubeconfig CA, client certificate, and caller bearer token instead of a Run
    endpoint or a Runtime gateway.
    """

    def __init__(self, *, api_server: str, ssl_context: ssl.SSLContext | None = None) -> None:
        if not api_server:
            raise ValueError("Kubernetes API server URL is required")
        self._api_server = api_server.rstrip("/")
        self._ssl_context = ssl_context

    @property
    def api_server(self) -> str:
        return self._api_server

    def request(
        self, method: str, url: str, body: bytes | None, headers: Mapping[str, str]
    ) -> HTTPResponse:
        request = Request(url, data=body, headers=dict(headers), method=method)
        try:
            with urlopen(request, context=self._ssl_context) as response:  # noqa: S310 - kube-apiserver URL from kubeconfig
                return HTTPResponse(response.status, response.read(1 << 20))
        except HTTPError as error:
            return HTTPResponse(error.code, error.read(1 << 20))

    def open_session(self, url: str, headers: Mapping[str, str], timeout_seconds: float | None) -> SessionConnection:
        try:
            import websocket
        except ImportError as error:
            raise ImportError("install kruntimes-sdk WebSocket dependencies") from error
        header = [f"{name}: {value}" for name, value in headers.items()]
        sslopt: dict[str, Any] = {}
        if self._ssl_context is not None:
            sslopt["context"] = self._ssl_context
        connection = websocket.create_connection(url, header=header, timeout=timeout_seconds, sslopt=sslopt)
        # The timeout bounds only the upgrade handshake. A long-running
        # operation must not inherit it as a per-receive idle timeout.
        connection.settimeout(None)
        return connection


def from_incluster(*, poll_interval_seconds: float = 0.5) -> SandboxClient:
    """Build a SandboxClient from the pod ServiceAccount and Kubernetes APIs."""
    kubernetes = _load_kubernetes()
    kubernetes.config.load_incluster_config()
    return _from_api_client(kubernetes.client.ApiClient(), poll_interval_seconds)


def from_kube_config(
    *, context: str | None = None, kubeconfig: str | None = None,
    poll_interval_seconds: float = 0.5,
) -> SandboxClient:
    """Build a SandboxClient from the active local kubeconfig context."""
    kubernetes = _load_kubernetes()
    kubernetes.config.load_kube_config(config_file=kubeconfig, context=context)
    return _from_api_client(kubernetes.client.ApiClient(), poll_interval_seconds)


def _from_api_client(api_client: Any, poll_interval_seconds: float) -> SandboxClient:
    kubernetes = _load_kubernetes()
    configuration = api_client.configuration
    api_server = str(getattr(configuration, "host", "") or "")
    token = configuration.get_api_key_with_prefix("authorization") or ""
    if token.lower().startswith("bearer "):
        token = token[7:]
    return SandboxClient(
        KubernetesRunClient(kubernetes.client.CustomObjectsApi(api_client)),
        UrllibAPITransport(api_server=api_server, ssl_context=_ssl_context(configuration)),
        api_server=api_server,
        logs=KubernetesLogReader(kubernetes.client.CoreV1Api(api_client)),
        bearer_token=token,
        poll_interval_seconds=poll_interval_seconds,
    )


def _ssl_context(configuration: Any) -> ssl.SSLContext:
    """Build one TLS context shared by HTTP and WebSocket requests."""
    if bool(getattr(configuration, "verify_ssl", True)):
        context = ssl.create_default_context(cafile=getattr(configuration, "ssl_ca_cert", None) or None)
    else:
        context = ssl._create_unverified_context()  # noqa: S323 - explicit kubeconfig verify_ssl=false
    if cert_file := getattr(configuration, "cert_file", None):
        context.load_cert_chain(cert_file, getattr(configuration, "key_file", None))
    return context


def _load_kubernetes() -> Any:
    try:
        import kubernetes
    except ImportError as error:
        raise ImportError(
            "install kruntimes-sdk[kubernetes] to use from_kube_config or from_incluster"
        ) from error
    return kubernetes
