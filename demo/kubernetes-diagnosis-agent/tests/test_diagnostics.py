import unittest
from types import SimpleNamespace

from agent import _MAX_TOOL_CALLS, execute_command, validate_tool_call, write_file
from kruntimes.sandbox import Command
from diagnostics import TOOL, command_for


class DiagnosticsTests(unittest.TestCase):
    def test_agent_uses_operation_session_for_commands_and_files(self):
        class Session:
            def __init__(self):
                self.operations = []
                self.events = [
                    SimpleNamespace(type="output", value={"output": {"stream": "stdout", "data": "b2s="}}),
                    SimpleNamespace(type="completed", value={"completed": {"command": {"exitCode": 0}}}),
                    SimpleNamespace(type="completed", value={"completed": {}}),
                ]

            def send(self, operation):
                self.operations.append(operation.request_body())

            def receive(self):
                return self.events.pop(0)

        session = Session()
        self.assertEqual(b"ok", execute_command(session, Command(argv=["echo", "ok"])))
        write_file(session, "evidence/00.json", b"ok")
        self.assertEqual(["echo", "ok"], session.operations[0]["command"]["argv"])
        self.assertEqual("evidence/00.json", session.operations[1]["writeFile"]["path"])

    def test_list_command_is_namespace_confined(self):
        self.assertEqual(
            ["kubectl", "get", "pods", "--namespace", "workloads", "--output", "json"],
            command_for("workloads", {"kind": "pods"}),
        )

    def test_named_command_uses_argv_not_shell(self):
        self.assertEqual(
            ["kubectl", "get", "deployments", "api", "--namespace", "workloads", "--output", "json"],
            command_for("workloads", {"kind": "deployments", "name": "api"}),
        )

    def test_invalid_values_are_rejected(self):
        with self.assertRaises(ValueError):
            command_for("workloads", {"kind": "pods; rm -rf /"})
        with self.assertRaises(ValueError):
            command_for("workloads", {"kind": "pods", "name": "../../secret"})

    def test_tool_name_is_fixed(self):
        self.assertEqual("collect_kubernetes_diagnostics", TOOL["name"])

    def test_agent_rejects_unknown_or_unbounded_tool_calls(self):
        with self.assertRaises(RuntimeError):
            validate_tool_call("write_file", 0)
        with self.assertRaises(RuntimeError):
            validate_tool_call(TOOL["name"], _MAX_TOOL_CALLS)


if __name__ == "__main__":
    unittest.main()
