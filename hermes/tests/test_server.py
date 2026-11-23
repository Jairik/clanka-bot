"""Unit tests for the Hermes sidecar's env-driven slim-down settings."""

from __future__ import annotations

import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]


def load_server():
    """Import hermes/server.py as hermes_server without starting the HTTP server."""
    spec = importlib.util.spec_from_file_location("hermes_server", ROOT / "server.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class HermesServerConfigTest(unittest.TestCase):
    # Each test gets a clean environment so flags cannot leak between cases.
    def setUp(self):
        self.server = load_server()
        self.home = tempfile.TemporaryDirectory()
        self.addCleanup(self.home.cleanup)
        self.env = {
            "HERMES_HOME": self.home.name,
            "HERMES_RUNTIME_TOKEN": "t" * 32,
        }
        patcher = mock.patch.dict(os.environ, self.env, clear=True)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_empty_allowlist_is_tool_free_with_search_off(self):
        self.assertEqual(self.server.parse_toolsets(), [])
        self.assertFalse(self.server.tool_search_enabled())
        self.assertNotIn("mcp_servers", self.server.runtime_config_text())
        self.assertIn("enabled: off", self.server.runtime_config_text())

    def test_sample_mcp_adds_the_tiny_allowlist_and_lazy_server(self):
        os.environ["HERMES_SAMPLE_MCP"] = "1"
        os.environ["HERMES_SAMPLE_MCP_PATH"] = "/tmp/sample_mcp.py"
        self.assertEqual(self.server.parse_toolsets(), ["clanka_sample"])
        self.assertTrue(self.server.tool_search_enabled())
        text = self.server.runtime_config_text()
        self.assertIn("enabled: on", text)
        self.assertIn("clanka_sample:", text)
        self.assertIn("lazy: true", text)
        self.assertIn("/tmp/sample_mcp.py", text)

    def test_explicit_tool_search_off_keeps_sample_tools_eager(self):
        os.environ["HERMES_SAMPLE_MCP"] = "1"
        os.environ["HERMES_TOOL_SEARCH"] = "off"
        os.environ["HERMES_SAMPLE_MCP_LAZY"] = "0"
        self.assertFalse(self.server.tool_search_enabled())
        text = self.server.runtime_config_text()
        self.assertIn("enabled: off", text)
        self.assertIn("lazy: false", text)

    def test_arbitrary_toolsets_do_not_enable_built_ins(self):
        os.environ["HERMES_TOOLSETS"] = "todo, terminal"
        self.assertEqual(self.server.agent_kwargs("key")["enabled_toolsets"], [])

    def test_local_endpoint_uses_custom_provider(self):
        os.environ["HERMES_LOCAL_BASE_URL"] = "http://ornith.example/v1"
        os.environ["HERMES_LOCAL_MODEL"] = "ornith:9b"
        kwargs = self.server.agent_kwargs("local-key")
        self.assertEqual(kwargs["provider"], "custom")
        self.assertEqual(kwargs["base_url"], "http://ornith.example/v1")
        self.assertEqual(kwargs["model"], "ornith:9b")
        self.assertEqual(kwargs["api_key"], "local-key")
        self.assertTrue(kwargs["skip_context_files"])
        self.assertTrue(kwargs["skip_memory"])
        self.assertFalse(kwargs["load_soul_identity"])
        self.assertEqual(kwargs["max_iterations"], 1)

    def test_cloud_endpoint_when_local_is_unset(self):
        kwargs = self.server.agent_kwargs("cloud-key")
        self.assertEqual(kwargs["provider"], "opencode-go")
        self.assertEqual(kwargs["base_url"], self.server.CLOUD_BASE_URL)
        self.assertEqual(kwargs["model"], self.server.CLOUD_MODEL)

    def test_startup_rejects_a_half_configured_local_endpoint(self):
        os.environ["HERMES_LOCAL_BASE_URL"] = "http://ornith.example/v1"
        with self.assertRaises(SystemExit):
            self.server.validate_startup()

    def test_startup_writes_config_and_returns_the_token(self):
        token = self.server.validate_startup()
        self.assertEqual(token, "t" * 32)
        written = Path(self.home.name) / "config.yaml"
        self.assertTrue(written.is_file())
        self.assertIn("tool_search:", written.read_text())

    def test_worker_env_keeps_only_the_allowlisted_names(self):
        os.environ["HERMES_LOCAL_BASE_URL"] = "http://ornith.example/v1"
        os.environ["HERMES_LOCAL_API_KEY"] = "should-not-leak"
        os.environ["OPENAI_API_KEY"] = "should-not-leak"
        forwarded = self.server.worker_env()
        self.assertIn("HERMES_LOCAL_BASE_URL", forwarded)
        self.assertNotIn("HERMES_LOCAL_API_KEY", forwarded)
        self.assertNotIn("OPENAI_API_KEY", forwarded)
        self.assertNotIn("HERMES_RUNTIME_TOKEN", forwarded)

    def test_mcp_discovery_is_skipped_without_the_sample_server(self):
        self.assertEqual(self.server.discover_configured_mcp(), [])

    def test_empty_allowlist_rejects_tools(self):
        class Fake:
            tools = [{"name": "terminal"}]

        with self.assertRaisesRegex(RuntimeError, "initialized with tools"):
            self.server.assert_tool_allowlist(Fake())

    def test_sample_allowlist_requires_registered_tools(self):
        os.environ["HERMES_SAMPLE_MCP"] = "1"

        class Empty:
            tools = []

        with self.assertRaisesRegex(RuntimeError, "did not register"):
            self.server.assert_tool_allowlist(Empty())

        for names in (
            ("mcp__clanka_sample__sample_ping", "mcp__clanka_sample__sample_time"),
            ("tool_search", "tool_describe", "tool_call"),
        ):
            ready = mock.Mock(tools=[{"function": {"name": name}} for name in names])
            self.server.assert_tool_allowlist(ready)
            ready.tools.append({"function": {"name": "terminal"}})
            with self.assertRaisesRegex(RuntimeError, "did not register exactly"):
                self.server.assert_tool_allowlist(ready)

    def test_dockerfile_pins_current_stable_digest(self):
        text = (ROOT / "Dockerfile").read_text()
        self.assertIn("nousresearch/hermes-agent:v0.23.3@", text)
        self.assertIn(
            "sha256:3e9fe4c6ef36490f43aa0d0d80f9df64e9c92a576eee3042f1a97c8813c05355",
            text,
        )


if __name__ == "__main__":
    unittest.main()
