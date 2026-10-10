"""Use OpenAI tool calls and a kruntimes Session Run to diagnose one namespace."""

from __future__ import annotations

import argparse
import base64
import json
import os
from typing import Any

from diagnostics import TOOL, command_for
from kruntimes.kubernetes import PortForwardGatewayTransport, from_incluster, from_kube_config
from kruntimes.sandbox import AcquireOptions, Command, Operation

_MAX_TOOL_CALLS = 8


def parse_arguments() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--namespace", required=True, help="namespace to diagnose")
    parser.add_argument("--runtime", default="diagnosis-python", help="Session Runtime name")
    parser.add_argument("--model", default=os.environ.get("OPENAI_MODEL", "gpt-4o-mini"))
    parser.add_argument("--in-cluster", action="store_true")
    parser.add_argument("--gateway-namespace", default="default")
    parser.add_argument("--gateway-service", default="kruntimes-console")
    parser.add_argument("--gateway-port", type=int, default=443)
    return parser.parse_args()


def main() -> None:
    arguments = parse_arguments()
    if arguments.in_cluster:
        client = from_incluster()
        run_diagnosis(client, arguments.namespace, arguments.runtime, arguments.model)
        return
    with PortForwardGatewayTransport.start(
        namespace=arguments.gateway_namespace,
        service=arguments.gateway_service,
        service_port=arguments.gateway_port,
    ) as gateway:
        client = from_kube_config(gateway=gateway)
        run_diagnosis(client, arguments.namespace, arguments.runtime, arguments.model)


def run_diagnosis(client: Any, namespace: str, runtime: str, model: str) -> None:
    """Run a bounded tool-call loop and preserve its evidence in one session."""
    openai = _openai_client()
    sandbox = client.runtime(namespace, runtime).acquire_sandbox(AcquireOptions(
        generate_name="kube-diagnose-",
        session={"leaseTimeoutSeconds": 300, "operationTimeout": "30s"},
    ), timeout_seconds=90)
    try:
        session = sandbox.open_session()
        try:
            _run_diagnosis_session(openai, sandbox, session, namespace, model)
        finally:
            session.close()
    finally:
        sandbox.release(timeout_seconds=30)


def _run_diagnosis_session(openai: Any, sandbox: Any, session: Any, namespace: str, model: str) -> None:
    response = openai.responses.create(
        model=model,
        tools=[TOOL],
        input=(
            f"Diagnose namespace {namespace}. Use only the provided diagnostic tool. "
            "Collect the minimum evidence needed, then summarize likely issues and next steps."
        ),
    )
    evidence_index = 0
    while calls := [item for item in response.output if item.type == "function_call"]:
        tool_outputs = []
        for call in calls:
            validate_tool_call(call.name, evidence_index)
            command = command_for(namespace, json.loads(call.arguments))
            output = execute_command(session, Command(argv=command, timeout_millis=30_000))[:16_384]
            evidence_path = f"evidence/{evidence_index:02d}.json"
            write_file(session, evidence_path, output)
            evidence_index += 1
            tool_outputs.append({
                "type": "function_call_output",
                "call_id": call.call_id,
                "output": output.decode(errors="replace"),
            })
        response = openai.responses.create(
            model=model,
            tools=[TOOL],
            previous_response_id=response.id,
            input=tool_outputs,
        )
    write_file(session, "report.md", response.output_text.encode())
    report, _ = sandbox.read_file("report.md", max_bytes=16_384)
    print(report.decode(errors="replace"))


def execute_command(session: Any, command: Command) -> bytes:
    session.send(Operation(command=command))
    output = bytearray()
    while True:
        event = session.receive()
        if event.type == "output":
            value = event.value.get("output", {})
            if value.get("stream") in ("stdout", "stderr") and value.get("data"):
                output.extend(base64.b64decode(value["data"]))
        elif event.type == "completed":
            result = event.value.get("completed", {}).get("command", {})
            if result.get("exitCode", 0) != 0 or result.get("timedOut", False):
                raise RuntimeError(f"diagnostic command failed: {result}")
            return bytes(output)
        elif event.type == "failed":
            raise RuntimeError(f"diagnostic command failed: {event.value.get('failed')}")


def write_file(session: Any, path: str, contents: bytes) -> None:
    session.send(Operation(write_file={
        "path": path, "contents": base64.b64encode(contents).decode(), "createParents": True,
    }))
    while True:
        event = session.receive()
        if event.type == "completed":
            return
        if event.type == "failed":
            raise RuntimeError(f"write {path} failed: {event.value.get('failed')}")


def _openai_client() -> Any:
    try:
        from openai import OpenAI
    except ImportError as error:
        raise RuntimeError("install openai before running this example") from error
    if not os.environ.get("OPENAI_API_KEY"):
        raise RuntimeError("OPENAI_API_KEY is required")
    return OpenAI()


def validate_tool_call(name: str, evidence_index: int) -> None:
    """Keep model output inside the declared tool and bounded workspace plan."""
    if name != TOOL["name"]:
        raise RuntimeError(f"unsupported tool call {name!r}")
    if evidence_index >= _MAX_TOOL_CALLS:
        raise RuntimeError(f"diagnosis exceeded {_MAX_TOOL_CALLS} tool calls")


if __name__ == "__main__":
    main()
