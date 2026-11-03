import unittest
from types import SimpleNamespace

from agent import _MAX_TOOL_CALLS, execute_command, validate_tool_call
from kruntimes.sandbox import Command
from diagnostics import TOOL, command_for


class DiagnosticsTests(unittest.TestCase):
    def test_agent_uses_one_shot_command_helper(self):
        class Sandbox:
            def __init__(self, result):
                self.commands = []
                self._result = result

            def run_command(self, command):
                self.commands.append(command)
                return self._result

        sandbox = Sandbox(SimpleNamespace(exit_code=0, stdout=b"ok", stderr=b"", timed_out=False))
        self.assertEqual(b"ok", execute_command(sandbox, Command(argv=["echo", "ok"])))
        self.assertEqual(["echo", "ok"], sandbox.commands[0].argv)

    def test_agent_rejects_failed_diagnostic_command(self):
        class Sandbox:
            def run_command(self, command):
                return SimpleNamespace(exit_code=1, stdout=b"", stderr=b"denied", timed_out=False)

        with self.assertRaises(RuntimeError):
            execute_command(Sandbox(), Command(argv=["kubectl", "get", "pods"]))

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
