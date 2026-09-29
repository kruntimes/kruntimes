#!/usr/bin/env python3
"""Verify Console Helm rendering for every supported TLS source."""

from __future__ import annotations

import subprocess
import sys


def main() -> int:
    app_version = helm_chart_field("charts/kruntimes", "appVersion")
    default = helm_template()
    if "kruntimes-console" not in default:
        return fail("Console must be installed by default")

    generated = default
    for expected in (
        "kind: Deployment",
        "name: kruntimes-console",
        f"image: ghcr.io/kruntimes/console:{app_version}",
        "scheme: HTTPS",
        "name: kruntimes-console-tls",
    ):
        if expected not in generated:
            return fail(f"self-signed Console render is missing {expected!r}")
    if "kind: Certificate" in generated:
        return fail("self-signed Dashboard render must not create a Certificate")

    existing = helm_template(
        "--set",
        "console.tls.selfSigned=false",
        "--set",
        "console.tls.secretName=console-existing",
    )
    if "secretName: console-existing" not in existing:
        return fail("existing-secret Console render must mount the selected Secret")
    if "kind: Certificate" in existing:
        return fail("existing-secret Console render must not create a Certificate")
    if "name: kruntimes-console-tls" in existing:
        return fail("existing-secret Console render must not create a chart TLS Secret")

    cert_manager = helm_template(
        "--set",
        "console.tls.selfSigned=false",
        "--set",
        "console.tls.secretName=console-cert-manager",
        "--set",
        "console.tls.certManager.enabled=true",
        "--set",
        "console.tls.certManager.issuerRef.name=platform-ca",
    )
    for expected in (
        "kind: Certificate",
        "name: kruntimes-console",
        "secretName: console-cert-manager",
        "name: platform-ca",
    ):
        if expected not in cert_manager:
            return fail(f"cert-manager Console render is missing {expected!r}")
    if "kind: Secret\nmetadata:\n  name: console-cert-manager" in cert_manager:
        return fail("cert-manager Dashboard render must not create the target Secret")

    return expect_failure(
        "console.tls.selfSigned and console.tls.certManager.enabled are mutually exclusive",
        "--set",
        "console.tls.certManager.enabled=true",
        "--set",
        "console.tls.certManager.issuerRef.name=platform-ca",
    )


def helm_template(*args: str) -> str:
    return subprocess.check_output(
        ["helm", "template", "kruntimes", "charts/kruntimes", "--namespace", "default", *args],
        text=True,
    )


def helm_chart_field(chart: str, field: str) -> str:
    chart_yaml = subprocess.check_output(["helm", "show", "chart", chart], text=True)
    for line in chart_yaml.splitlines():
        key, sep, value = line.partition(":")
        if sep and key == field:
            return value.strip().strip('"')
    raise RuntimeError(f"{chart} is missing {field}")


def expect_failure(expected: str, *args: str) -> int:
    result = subprocess.run(
        ["helm", "template", "kruntimes", "charts/kruntimes", "--namespace", "default", *args],
        text=True,
        capture_output=True,
    )
    if result.returncode == 0:
        return fail("ambiguous Console TLS configuration unexpectedly rendered")
    if expected not in result.stderr:
        return fail(f"unexpected validation error: {result.stderr}")
    return 0


def fail(message: str) -> int:
    print(message, file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
