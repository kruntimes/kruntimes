#!/usr/bin/env python3
"""Verify Helm image defaults and explicit image overrides."""

from __future__ import annotations

import subprocess
import sys


def main() -> int:
    platform = helm_template("kruntimes", "charts/kruntimes")
    runtimes = helm_template("kruntimes-runtimes", "charts/kruntimes-runtimes")
    platform_app_version = helm_chart_field("charts/kruntimes", "appVersion")
    runtimes_app_version = helm_chart_field("charts/kruntimes-runtimes", "appVersion")

    expected_defaults = [
        f"image: kruntimes-controller:{platform_app_version}",
        f"image: kruntimes-scheduler:{platform_app_version}",
        f"--default-daemon-image=kruntimes-runtimed:{platform_app_version}",
        f"image: kruntimes-bash-runtime:{runtimes_app_version}",
        f"image: kruntimes-python-runtime:{runtimes_app_version}",
    ]
    for expected in expected_defaults:
        if expected not in platform and expected not in runtimes:
            print(f"missing default appVersion image: {expected}", file=sys.stderr)
            return 1

    platform_overrides = helm_template(
        "kruntimes",
        "charts/kruntimes",
        "--set",
        "scheduler.image=example.com:5000/ns/scheduler:dev",
        "--set",
        "controller.image=repo/controller@sha256:abc",
        "--set",
        "runtimed.image=repo/runtimed:dev",
    )
    expected_platform_overrides = [
        "image: example.com:5000/ns/scheduler:dev",
        "image: repo/controller@sha256:abc",
        "--default-daemon-image=repo/runtimed:dev",
    ]
    for expected in expected_platform_overrides:
        if expected not in platform_overrides:
            print(f"missing explicit platform image override: {expected}", file=sys.stderr)
            return 1

    runtime_overrides = helm_template(
        "kruntimes-runtimes",
        "charts/kruntimes-runtimes",
        "--set",
        "bash.image=localhost:5000/bash:dev",
        "--set",
        "python.image=repo/python@sha256:def",
    )
    expected_runtime_overrides = [
        "image: localhost:5000/bash:dev",
        "image: repo/python@sha256:def",
    ]
    for expected in expected_runtime_overrides:
        if expected not in runtime_overrides:
            print(f"missing explicit runtime image override: {expected}", file=sys.stderr)
            return 1

    return 0


def helm_template(release: str, chart: str, *args: str) -> str:
    return subprocess.check_output(
        ["helm", "template", release, chart, "--namespace", "default", *args],
        text=True,
    )


def helm_chart_field(chart: str, field: str) -> str:
    chart_yaml = subprocess.check_output(["helm", "show", "chart", chart], text=True)
    for line in chart_yaml.splitlines():
        key, sep, value = line.partition(":")
        if sep and key == field:
            return value.strip().strip('"')
    raise RuntimeError(f"{chart} is missing {field}")


if __name__ == "__main__":
    raise SystemExit(main())
