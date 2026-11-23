"""Black-box tests for the newline-delimited sample MCP server."""

from __future__ import annotations

import io
import json
from pathlib import Path
import subprocess
import sys
import unittest


ROOT = Path(__file__).resolve().parents[1]


def load_sample():
    """Import sample_mcp.py by path so the tests do not need a package."""
    import importlib.util

    spec = importlib.util.spec_from_file_location("sample_mcp", ROOT / "sample_mcp.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class SampleMCPTest(unittest.TestCase):
    def setUp(self):
        self.mcp = load_sample()

    def test_lists_the_two_sample_tools(self):
        names = [tool["name"] for tool in self.mcp.tool_list()]
        self.assertEqual(names, ["sample_ping", "sample_time"])

    def test_ping_returns_pong(self):
        self.assertEqual(self.mcp.call_tool("sample_ping", {}), "pong")

    def test_time_returns_utc(self):
        stamp = self.mcp.call_tool("sample_time", {})
        self.assertRegex(stamp, r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$")

    def test_stdio_initialize_list_and_call(self):
        # Drive the real stdio loop with the same NDJSON framing Hermes uses.
        messages = [
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "initialize",
                "params": {"protocolVersion": "2024-11-05", "capabilities": {}},
            },
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
            {
                "jsonrpc": "2.0",
                "id": 3,
                "method": "tools/call",
                "params": {"name": "sample_ping", "arguments": {}},
            },
        ]
        stdin = io.StringIO("".join(json.dumps(item) + "\n" for item in messages))
        stdout = io.StringIO()
        self.mcp.serve(stdin, stdout)
        replies = [json.loads(line) for line in stdout.getvalue().splitlines()]
        self.assertEqual(len(replies), 3)
        self.assertEqual(replies[0]["result"]["serverInfo"]["name"], "clanka-sample")
        listed = [tool["name"] for tool in replies[1]["result"]["tools"]]
        self.assertEqual(listed, ["sample_ping", "sample_time"])
        self.assertEqual(replies[2]["result"]["content"][0]["text"], "pong")

    def test_process_speaks_ndjson(self):
        payload = json.dumps(
            {
                "jsonrpc": "2.0",
                "id": 1,
                "method": "tools/call",
                "params": {"name": "sample_ping", "arguments": {}},
            }
        )
        proc = subprocess.run(
            [sys.executable, str(ROOT / "sample_mcp.py")],
            input=payload + "\n",
            capture_output=True,
            text=True,
            check=True,
        )
        reply = json.loads(proc.stdout.strip())
        self.assertEqual(reply["result"]["content"][0]["text"], "pong")
        self.assertEqual(proc.stderr, "")


if __name__ == "__main__":
    unittest.main()
