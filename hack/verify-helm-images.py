#!/usr/bin/env python3
"""Verify Helm image defaults and explicit image overrides."""

from __future__ import annotations

import subprocess
import sys


def main() -> int:
    platform = helm_template("kruntimes", "charts/kruntimes")
    runtimes = helm_template("kruntimes-runtimes", "charts/kruntimes-runtimes")

    expected_defaults = [
        "image: kruntimes-controller:0.1.0",
        "image: kruntimes-scheduler:0.1.0",
        "--default-daemon-image=kruntimes-runtimed:0.1.0",
        "image: kruntimes-bash-runtime:0.1.0",
        "image: kruntimes-python-runtime:0.1.0",
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


if __name__ == "__main__":
    raise SystemExit(main())
