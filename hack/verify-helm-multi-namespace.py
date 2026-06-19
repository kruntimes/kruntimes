#!/usr/bin/env python3
"""Verify platform and runtime charts render correctly across namespaces."""

from __future__ import annotations

import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path


PLATFORM_CHART = Path("charts/kruntimes")
RUNTIMES_CHART = Path("charts/kruntimes-runtimes")
PLATFORM_RELEASE = "kruntimes"
RUNTIMES_RELEASE = "kruntimes-runtimes"
PLATFORM_NAMESPACE = "kruntimes-system"
RUNTIMES_NAMESPACE = "workloads"


@dataclass(frozen=True)
class Resource:
    chart: str
    kind: str
    name: str
    namespace: str
    text: str


def main() -> int:
    resources = [
        *parse_resources(
            "platform",
            helm_template(PLATFORM_RELEASE, PLATFORM_CHART, PLATFORM_NAMESPACE, include_crds=True),
        ),
        *parse_resources(
            "runtimes",
            helm_template(RUNTIMES_RELEASE, RUNTIMES_CHART, RUNTIMES_NAMESPACE),
        ),
    ]

    duplicates = duplicate_resource_keys(resources)
    if duplicates:
        print("duplicate rendered resource identities:", file=sys.stderr)
        for key, owners in duplicates.items():
            print(f"  {key}: {', '.join(owners)}", file=sys.stderr)
        return 1

    checks = [
        require_platform_cluster_scoped_resources(resources),
        require_namespaced_resources("platform", PLATFORM_NAMESPACE, resources),
        require_namespaced_resources("runtimes", RUNTIMES_NAMESPACE, resources),
        require_rbac_subject_namespace(resources),
        require_runtime_namespace(resources),
        require_controller_runtimed_service_account(resources),
        require_no_runtimed_chart_rbac(resources),
    ]
    if not all(checks):
        return 1
    return 0


def helm_template(release: str, chart: Path, namespace: str, include_crds: bool = False) -> str:
    command = ["helm", "template", release, str(chart), "--namespace", namespace]
    if include_crds:
        command.append("--include-crds")
    return subprocess.check_output(command, text=True)


def parse_resources(chart: str, rendered: str) -> list[Resource]:
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
        resources.append(Resource(chart=chart, kind=kind, name=name, namespace=namespace, text=doc))
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


def duplicate_resource_keys(resources: list[Resource]) -> dict[tuple[str, str, str], list[str]]:
    owners_by_key: dict[tuple[str, str, str], list[str]] = {}
    for resource in resources:
        key = (resource.namespace, resource.kind, resource.name)
        owners_by_key.setdefault(key, []).append(resource.chart)
    return {key: owners for key, owners in owners_by_key.items() if len(owners) > 1}


def require_namespaced_resources(chart: str, namespace: str, resources: list[Resource]) -> bool:
    ok = True
    for resource in resources:
        if resource.chart != chart or resource.namespace == "_cluster":
            continue
        if resource.namespace != namespace:
            print(
                f"{chart} {resource.kind}/{resource.name} rendered in {resource.namespace}, want {namespace}",
                file=sys.stderr,
            )
            ok = False
    return ok


def require_platform_cluster_scoped_resources(resources: list[Resource]) -> bool:
    expected = {
        ("CustomResourceDefinition", "runs.kruntimes.io"),
        ("CustomResourceDefinition", "runtimes.kruntimes.io"),
        ("CustomResourceDefinition", "workflows.kruntimes.io"),
        ("ClusterRole", "kruntimes-controller"),
        ("ClusterRole", "kruntimes-scheduler"),
        ("ClusterRoleBinding", "kruntimes-controller"),
        ("ClusterRoleBinding", "kruntimes-scheduler"),
    }
    actual = {
        (resource.kind, resource.name)
        for resource in resources
        if resource.chart == "platform" and resource.namespace == "_cluster"
    }
    missing = expected - actual
    if missing:
        print("platform chart missing expected cluster-scoped resources:", file=sys.stderr)
        for kind, name in sorted(missing):
            print(f"  {kind}/{name}", file=sys.stderr)
        return False
    return True


def require_rbac_subject_namespace(resources: list[Resource]) -> bool:
    ok = True
    for resource in resources:
        if resource.chart != "platform" or resource.kind != "ClusterRoleBinding":
            continue
        expected = f"    namespace: {PLATFORM_NAMESPACE}"
        if expected not in resource.text:
            print(
                f"{resource.name} subject must reference {PLATFORM_NAMESPACE}",
                file=sys.stderr,
            )
            ok = False
    return ok


def require_runtime_namespace(resources: list[Resource]) -> bool:
    runtimes = [
        resource
        for resource in resources
        if resource.chart == "runtimes" and resource.kind == "Runtime"
    ]
    expected_names = {"bash", "python"}
    actual_names = {resource.name for resource in runtimes}
    if actual_names != expected_names:
        print(f"runtime chart rendered Runtime names {actual_names}, want {expected_names}", file=sys.stderr)
        return False
    for resource in runtimes:
        if resource.namespace != RUNTIMES_NAMESPACE:
            print(
                f"Runtime/{resource.name} rendered in {resource.namespace}, want {RUNTIMES_NAMESPACE}",
                file=sys.stderr,
            )
            return False
    return True


def require_controller_runtimed_service_account(resources: list[Resource]) -> bool:
    for resource in resources:
        if resource.chart == "platform" and resource.kind == "Deployment" and resource.name == "kruntimes-controller":
            expected = "--runtimed-service-account-name=kruntimes-runtimed"
            if expected not in resource.text:
                print(f"controller deployment must pass {expected}", file=sys.stderr)
                return False
            return True
    print("missing platform controller Deployment", file=sys.stderr)
    return False


def require_no_runtimed_chart_rbac(resources: list[Resource]) -> bool:
    for resource in resources:
        if resource.chart != "runtimes":
            continue
        if resource.kind in {"ServiceAccount", "Role", "RoleBinding", "ClusterRole", "ClusterRoleBinding"}:
            print(
                f"runtimes chart must not render runtimed RBAC; got {resource.kind}/{resource.name}",
                file=sys.stderr,
            )
            return False
    return True


if __name__ == "__main__":
    raise SystemExit(main())
