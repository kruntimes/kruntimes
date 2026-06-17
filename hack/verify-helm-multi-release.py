#!/usr/bin/env python3
"""Verify two kruntimes Helm releases can render into one namespace safely."""

from __future__ import annotations

import re
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path


CHART = Path("charts/kruntimes")
NAMESPACE = "kruntimes-system"
RELEASES = ("kruntimes-a", "kruntimes-b")


@dataclass(frozen=True)
class Resource:
    release: str
    kind: str
    name: str
    namespace: str
    deployment_selector: dict[str, str]
    text: str


def main() -> int:
    resources: list[Resource] = []
    for release in RELEASES:
        rendered = subprocess.check_output(
            ["helm", "template", release, str(CHART), "--namespace", NAMESPACE],
            text=True,
        )
        resources.extend(parse_resources(release, rendered))

    duplicates = duplicate_resource_keys(resources)
    if duplicates:
        print("duplicate rendered resource identities:", file=sys.stderr)
        for key, owners in duplicates.items():
            print(f"  {key}: {', '.join(owners)}", file=sys.stderr)
        return 1

    bad_selectors = [
        r
        for r in resources
        if r.kind == "Deployment"
        and r.deployment_selector.get("app.kubernetes.io/instance") != r.release
    ]
    if bad_selectors:
        print("deployment selectors must include their release instance:", file=sys.stderr)
        for resource in bad_selectors:
            print(
                f"  {resource.release}/{resource.name}: {resource.deployment_selector}",
                file=sys.stderr,
            )
        return 1

    for release in RELEASES:
        expected = f"--runtimed-service-account-name={release}-runtimed"
        controller = find_resource(resources, release, "Deployment", f"{release}-controller")
        if controller is None or expected not in controller.text:
            print(
                f"controller deployment for {release} must pass {expected}",
                file=sys.stderr,
            )
            return 1

    return 0


def parse_resources(release: str, rendered: str) -> list[Resource]:
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
        resources.append(
            Resource(
                release=release,
                kind=kind,
                name=name,
                namespace=namespace,
                deployment_selector=deployment_selector(doc) if kind == "Deployment" else {},
                text=doc,
            )
        )
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


def deployment_selector(doc: str) -> dict[str, str]:
    labels: dict[str, str] = {}
    in_selector = False
    in_match_labels = False
    for line in doc.splitlines():
        if line == "  selector:":
            in_selector = True
            continue
        if in_selector and line == "    matchLabels:":
            in_match_labels = True
            continue
        if in_match_labels:
            if not line.startswith("      "):
                break
            key, _, value = line.strip().partition(":")
            labels[key] = value.strip().strip('"')
    return labels


def duplicate_resource_keys(resources: list[Resource]) -> dict[tuple[str, str, str], list[str]]:
    owners_by_key: dict[tuple[str, str, str], list[str]] = {}
    for resource in resources:
        key = (resource.namespace, resource.kind, resource.name)
        owners_by_key.setdefault(key, []).append(resource.release)
    return {key: owners for key, owners in owners_by_key.items() if len(owners) > 1}


def find_resource(
    resources: list[Resource],
    release: str,
    kind: str,
    name: str,
) -> Resource | None:
    for resource in resources:
        if resource.release == release and resource.kind == kind and resource.name == name:
            return resource
    return None


if __name__ == "__main__":
    raise SystemExit(main())
