#!/usr/bin/env python3
"""Verify kruntimes Helm chart deployment configuration values render."""

from __future__ import annotations

import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path


CHART = Path("charts/kruntimes")
RELEASE = "kruntimes"
NAMESPACE = "kruntimes-system"

OVERRIDES = """
imagePullSecrets:
  - name: pull-secret
scheduler:
  replicas: 3
  leaderElect: false
  metricsBindAddress: ":18080"
  probeBindAddress: ":18081"
  metricsPort: 18080
  probePort: 18081
  podAnnotations:
    checksum/config: scheduler
  podSecurityContext:
    runAsUser: 1000
    seccompProfile:
      type: RuntimeDefault
  securityContext:
    readOnlyRootFilesystem: false
    runAsNonRoot: true
    capabilities:
      drop:
        - ALL
  nodeSelector:
    workload: control-plane
  tolerations:
    - key: dedicated
      operator: Equal
      value: kruntimes
      effect: NoSchedule
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kubernetes.io/os
                operator: In
                values:
                  - linux
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/component: scheduler
  priorityClassName: system-cluster-critical
controller:
  replicas: 2
  leaderElect: false
  metricsBindAddress: ":18082"
  probeBindAddress: ":18083"
  metricsPort: 18082
  probePort: 18083
  podAnnotations:
    checksum/config: controller
  podSecurityContext:
    runAsUser: 1001
    seccompProfile:
      type: RuntimeDefault
  securityContext:
    readOnlyRootFilesystem: false
    runAsNonRoot: true
    capabilities:
      drop:
        - ALL
  nodeSelector:
    workload: control-plane
  tolerations:
    - key: dedicated
      operator: Equal
      value: kruntimes
      effect: NoSchedule
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            topologyKey: kubernetes.io/hostname
            labelSelector:
              matchLabels:
                app.kubernetes.io/component: controller
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: topology.kubernetes.io/zone
      whenUnsatisfiable: ScheduleAnyway
      labelSelector:
        matchLabels:
          app.kubernetes.io/component: controller
  priorityClassName: system-cluster-critical
  artifactStore:
    filesystem:
      volumeClaimName: controller-artifacts
      mountPath: /mnt/kruntimes-artifacts
"""


@dataclass(frozen=True)
class Resource:
    kind: str
    name: str
    text: str


def main() -> int:
    rendered = helm_template_with_overrides()
    resources = parse_resources(rendered)

    checks = [
        require_deployment(
            resources,
            "kruntimes-scheduler",
            [
                "replicas: 3",
                "--leader-elect=false",
                "--metrics-bind-address=:18080",
                "--health-probe-bind-address=:18081",
                "containerPort: 18080",
                "containerPort: 18081",
                "checksum/config: scheduler",
                "name: pull-secret",
                "runAsUser: 1000",
                "readOnlyRootFilesystem: false",
                "workload: control-plane",
                "key: dedicated",
                "nodeAffinity:",
                "topologySpreadConstraints:",
                "priorityClassName: system-cluster-critical",
                "port: probes",
            ],
        ),
        require_deployment(
            resources,
            "kruntimes-controller",
            [
                "replicas: 2",
                "--leader-elect=false",
                "--metrics-bind-address=:18082",
                "--health-probe-bind-address=:18083",
                "containerPort: 18082",
                "containerPort: 18083",
                "checksum/config: controller",
                "name: pull-secret",
                "runAsUser: 1001",
                "readOnlyRootFilesystem: false",
                "workload: control-plane",
                "key: dedicated",
                "podAntiAffinity:",
                "topologySpreadConstraints:",
                "priorityClassName: system-cluster-critical",
                "port: probes",
                "--artifact-filesystem-root=/mnt/kruntimes-artifacts",
                "mountPath: /mnt/kruntimes-artifacts",
                "claimName: controller-artifacts",
            ],
        ),
    ]
    return 0 if all(checks) else 1


def helm_template_with_overrides() -> str:
    with tempfile.NamedTemporaryFile("w", suffix=".yaml") as values:
        values.write(OVERRIDES)
        values.flush()
        return subprocess.check_output(
            [
                "helm",
                "template",
                RELEASE,
                str(CHART),
                "--namespace",
                NAMESPACE,
                "-f",
                values.name,
            ],
            text=True,
        )


def require_deployment(resources: list[Resource], name: str, expected: list[str]) -> bool:
    deployment = find_resource(resources, "Deployment", name)
    if deployment is None:
        print(f"missing Deployment/{name}", file=sys.stderr)
        return False
    ok = True
    for text in expected:
        if text not in deployment.text:
            print(f"Deployment/{name} missing {text!r}", file=sys.stderr)
            ok = False
    return ok


def parse_resources(rendered: str) -> list[Resource]:
    docs = re.split(r"^---\s*$", rendered, flags=re.MULTILINE)
    resources: list[Resource] = []
    for doc in docs:
        if not doc.strip():
            continue
        kind = field_at_indent(doc, "kind", 0)
        name = metadata_field(doc, "name")
        if kind and name:
            resources.append(Resource(kind=kind, name=name, text=doc))
    return resources


def find_resource(resources: list[Resource], kind: str, name: str) -> Resource | None:
    for resource in resources:
        if resource.kind == kind and resource.name == name:
            return resource
    return None


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
