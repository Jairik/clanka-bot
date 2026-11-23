"""Exercise the real stdio bridge against a local mock of the private Go adapter."""

from http.client import HTTPConnection
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import select
import subprocess
import sys
from threading import Thread
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
TOKEN = "test-private-token-" * 3
BASE = "/hermes/adapter/tools"


class AdapterHandler(BaseHTTPRequestHandler):
    def handle_request(self):
        # Keep all header values so duplicate Authorization headers fail the test.
        args = None
        if self.command == "POST":
            args = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        self.server.calls.append((self.command, self.path, self.headers.get_all("Authorization"), args))
        name = self.path.removeprefix(BASE + "/")
        if self.path == BASE:
            status, body = 200, {"tools": self.server.catalogue}
        elif name in self.server.revoked:
            status, body = 403, {"error": TOKEN}
        elif self.command == "GET":
            status, body = 200, {
                "name": name, "permission": "read", "input_schema": self.server.schema,
                "output_schema": {"type": "object"},
            }
        else:
            status, body = 200, {"name": name, "result": self.server.result}
        self.send_response(status)
        self.end_headers()
        self.wfile.write(json.dumps(body).encode())

    do_GET = do_POST = handle_request

    def log_message(self, *_args):
        # Avoid noise while the mock handles requests in a background thread.
        pass


class GoMCPTest(unittest.TestCase):
    def setUp(self):
        self.mock = ThreadingHTTPServer(("127.0.0.1", 0), AdapterHandler)
        self.addCleanup(self.mock.server_close)
        self.mock.calls = []
        self.mock.catalogue = [
            {"name": "weather.current", "permission": "read", "description": "Public forecast"},
            {"name": "web.search", "permission": "confirm", "description": "Changed policy"},
            {"name": "recipe.public_facts", "permission": "read", "description": "Public facts"},
            {"name": "terminal.exec", "permission": "read", "description": "Forbidden"},
        ]
        self.mock.revoked = set()
        self.mock.result = {"summary": "Sunny"}
        self.mock.schema = {"type": "object", "properties": {"query": {"type": "string"}}}
        Thread(target=self.mock.serve_forever, daemon=True).start()
        self.addCleanup(self.mock.shutdown)

    def open_child(self):
        # Route the fixed private destination to the mock without adding a production override.
        code = f"""
import http.client, runpy
from pathlib import Path
import sys
original = http.client.HTTPConnection
def redirect(host, port, timeout):
    # Assert that production code targets the private Compose endpoint.
    assert (host, port) == ('assistant', 8080)
    return original('127.0.0.1', {self.mock.server_port}, timeout=timeout)
http.client.HTTPConnection = redirect
sys.path.insert(0, str(Path.cwd()))
runpy.run_module('go_mcp', run_name='__main__')
"""
        child = subprocess.Popen(
            [sys.executable, "-c", code], cwd=ROOT,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            text=True, env={**os.environ, "CLANKA_ADAPTER_TOKEN": TOKEN},
        )
        self.addCleanup(child.__exit__, None, None, None)
        self.addCleanup(lambda: child.kill() if child.poll() is None else None)
        return child

    def send(self, child, method, **params):
        # Exchange actual newline-delimited MCP messages with the child.
        child.stdin.write(json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params}) + "\n")
        child.stdin.flush()
        self.assertTrue(select.select([child.stdout], [], [], 5)[0], "MCP reply timed out")
        line = child.stdout.readline()
        self.assertNotIn(TOKEN, line)
        response = json.loads(line)
        self.assertEqual(response["id"], 1)
        return response["result"]

    def test_mcp_lists_thin_metadata_and_checks_go_on_each_call(self):
        child = self.open_child()
        ready = self.send(child, "initialize", protocolVersion="2024-11-05")
        self.assertEqual(ready["serverInfo"]["name"], "clanka_go")
        self.assertEqual(self.mock.calls, [])

        tools = self.send(child, "tools/list")["tools"]
        self.assertEqual([tool["name"] for tool in tools], ["weather.current", "recipe.public_facts"])
        self.assertTrue(all(tool["inputSchema"] == {"type": "object", "additionalProperties": True} for tool in tools))
        self.assertEqual(self.mock.calls, [("GET", BASE, ["Bearer " + TOKEN], None)])

        args = {"location": "Toronto"}
        result = self.send(child, "tools/call", name="weather.current", arguments=args)
        self.assertEqual(json.loads(result["content"][0]["text"]), {"summary": "Sunny"})
        self.assertFalse(result["isError"])
        self.mock.revoked.add("weather.current")
        denied = self.send(child, "tools/call", name="weather.current", arguments=args)
        self.assertTrue(denied["isError"])
        self.assertEqual(denied["content"][0]["text"], "Tool is no longer available.")
        self.assertEqual(self.mock.calls[1:], [("POST", BASE + "/weather.current", ["Bearer " + TOKEN], {"arguments": args})] * 2)

        self.mock.catalogue = [{"name": "recipe.public_facts", "permission": "read", "description": "Updated"}]
        self.assertEqual(self.send(child, "tools/list")["tools"][0]["description"], "Updated")
        self.assertTrue(self.send(child, "tools/call", name="terminal.exec", arguments={})["isError"])
        self.assertEqual(len(self.mock.calls), 4)

    def test_mcp_drops_a_token_echo_from_a_successful_adapter_response(self):
        self.mock.result = {"secret": TOKEN}
        result = self.send(self.open_child(), "tools/call", name="weather.current", arguments={})
        self.assertTrue(result["isError"])
        self.assertEqual(result["content"][0]["text"], "Go Tool adapter unavailable.")

    def test_named_schema_tracks_recipe_edits_and_live_denial(self):
        # Load the worker's HTTP client without importing Hermes on CI.
        with mock.patch.object(sys, "path", [str(ROOT), *sys.path]):
            import go_mcp

        def redirect(host, port, timeout):
            # Hold the production destination fixed while using the loopback mock.
            self.assertEqual((host, port), ("assistant", 8080))
            return HTTPConnection("127.0.0.1", self.mock.server_port, timeout=timeout)

        with mock.patch.object(go_mcp, "HTTPConnection", redirect):
            adapter = go_mcp.GoAdapter(TOKEN)
            adapter.catalogue()
            self.assertEqual(adapter.describe("recipe.public_facts"), self.mock.schema)
            revised = {"type": "object", "additionalProperties": False}
            self.mock.schema = revised
            self.assertEqual(adapter.describe("recipe.public_facts"), revised)
            self.mock.revoked.add("recipe.public_facts")
            with self.assertRaisesRegex(go_mcp.ToolFailure, "no longer available"):
                adapter.describe("recipe.public_facts")
        self.assertEqual(self.mock.calls, [("GET", BASE, ["Bearer " + TOKEN], None)] +
                         [("GET", BASE + "/recipe.public_facts", ["Bearer " + TOKEN], None)] * 3)
