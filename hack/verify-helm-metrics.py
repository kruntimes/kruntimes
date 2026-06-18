#!/usr/bin/env python3
"""Verify kruntimes Helm chart metrics Service and ServiceMonitor rendering."""

from __future__ import annotations

import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path


CHART = Path("charts/kruntimes")
RELEASE = "kruntimes"
NAMESPACE = "kruntimes-system"


@dataclass(frozen=True)
class Resource:
    kind: str
    name: str
    namespace: str
    text: str


def main() -> int:
    rendered = helm_template()
    resources = parse_resources(rendered)

    service_names = {r.name for r in resources if r.kind == "Service"}
    expected_services = {"kruntimes-scheduler-metrics", "kruntimes-controller-metrics"}
    if not expected_services.issubset(service_names):
        print(f"missing metrics Services: {expected_services - service_names}", file=sys.stderr)
        return 1

    for service in [r for r in resources if r.kind == "Service" and r.name in expected_services]:
        if service.namespace != NAMESPACE:
            print(f"Service/{service.name} namespace = {service.namespace}, want {NAMESPACE}", file=sys.stderr)
            return 1
        if "targetPort: metrics" not in service.text:
            print(f"Service/{service.name} must target the named metrics port", file=sys.stderr)
            return 1
        component = service.name.removeprefix("kruntimes-").removesuffix("-metrics")
        if f"app.kubernetes.io/component: {component}" not in service.text:
            print(f"Service/{service.name} selector must target {component}", file=sys.stderr)
            return 1

    if any(r.kind == "ServiceMonitor" for r in resources):
        print("ServiceMonitor must be disabled by default", file=sys.stderr)
        return 1

    monitored = helm_template("--set", "metrics.serviceMonitor.enabled=true")
    monitored_resources = parse_resources(monitored)
    service_monitors = [r for r in monitored_resources if r.kind == "ServiceMonitor"]
    if len(service_monitors) != 1:
        print(f"ServiceMonitor count = {len(service_monitors)}, want 1", file=sys.stderr)
        return 1
    monitor = service_monitors[0]
    for expected in [
        "name: kruntimes-metrics",
        "port: metrics",
        "path: /metrics",
        "app.kubernetes.io/component",
        "controller",
        "scheduler",
    ]:
        if expected not in monitor.text:
            print(f"ServiceMonitor missing {expected!r}", file=sys.stderr)
            return 1

    return 0


def helm_template(*args: str) -> str:
    return subprocess.check_output(
        ["helm", "template", RELEASE, str(CHART), "--namespace", NAMESPACE, *args],
        text=True,
    )


def parse_resources(rendered: str) -> list[Resource]:
    docs = re.split(r"^---\s*$", rendered, flags=re.MULTILINE)
    resources: list[Resource] = []
    for doc in docs:
        if not doc.strip():
            continue
        kind = field_at_indent(doc, "kind", 0)
        name = metadata_field(doc, "name")
        if not kind or not name:
            continue
        namespace = metadata_field(doc, "namespace") or "_cluster"
        resources.append(Resource(kind=kind, name=name, namespace=namespace, text=doc))
    return resources


def field_at_indent(doc: str, field: str, indent: int) -> str:
    prefix = " " * indent + field + ":"
    for line in doc.splitlines():
        if line.startswith(prefix):
            return line.split(":", 1)[1].strip().strip('"')
    return ""


def metadata_field(doc: str, field: str) -> str:
    in_metadata = False
    for line in doc.splitlines():
        if line == "metadata:":
            in_metadata = True
            continue
        if in_metadata and line and not line.startswith(" "):
            return ""
        if in_metadata and line.startswith(f"  {field}:"):
            return line.split(":", 1)[1].strip().strip('"')
    return ""


if __name__ == "__main__":
    raise SystemExit(main())
