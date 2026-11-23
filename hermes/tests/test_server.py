"""Unit tests for the Hermes sidecar's env-driven slim-down settings."""

from __future__ import annotations

from http.client import HTTPConnection
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import threading
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

    def healthz(self):
        token = self.env["HERMES_RUNTIME_TOKEN"]
        with self.server.RuntimeServer(("127.0.0.1", 0), token) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            probe = HTTPConnection(*server.server_address, timeout=2)
            try:
                probe.request("GET", "/healthz")
                response = probe.getresponse()
                self.assertEqual(response.status, 200)
                return json.loads(response.read())
            finally:
                probe.close()
                server.shutdown()
                thread.join(timeout=2)

    def test_healthz_reports_the_local_model_it_routes(self):
        # The Assistant compares this with its own local model before it
        # reports Local as ready.
        self.assertEqual(self.healthz()["local_model"], "")
        os.environ["HERMES_LOCAL_MODEL"] = "ornith:9b"
        self.assertEqual(self.healthz()["local_model"], "", "a model without a URL is not routable")
        os.environ["HERMES_LOCAL_BASE_URL"] = " http://desk:8000/v1 "
        os.environ["HERMES_LOCAL_MODEL"] = " ornith:9b "
        self.assertEqual(self.healthz()["local_model"], "ornith:9b")
        self.assertNotIn("HERMES_LOCAL_API_KEY", json.dumps(self.healthz()))

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

    def test_incomplete_local_route_never_falls_back(self):
        os.environ["HERMES_LOCAL_BASE_URL"] = "http://ornith.example/v1"
        self.assertEqual(self.server.validate_startup(), self.env["HERMES_RUNTIME_TOKEN"])
        with self.assertRaises(ValueError):
            self.server.agent_kwargs("local-key")
        self.assertEqual(
            self.server.agent_kwargs("cloud-key", provider="opencode-go", model="glm-5.3-flash")["base_url"],
            self.server.CLOUD_BASE_URL,
        )

    def test_explicit_cloud_models_ignore_the_local_configuration(self):
        os.environ.update(HERMES_LOCAL_BASE_URL="http://ornith.example/v1", HERMES_LOCAL_MODEL="ornith:9b")
        for model in ("deepseek-v4.1-flash", "deepseek-v4-pro", "glm-5.3-flash", "kimi-k3"):
            with self.subTest(model=model):
                kwargs = self.server.agent_kwargs("cloud-key", provider="opencode-go", model=model)
                self.assertEqual(kwargs["base_url"], self.server.CLOUD_BASE_URL)
                self.assertEqual(kwargs["model"], model)
                self.assertEqual(kwargs["provider"], "opencode-go")
                self.assertEqual(kwargs["api_mode"], "chat_completions")
                self.assertEqual(kwargs["api_key"], "cloud-key")
                self.assertEqual(kwargs["fallback_model"], {})

    def test_run_validates_route_before_starting_any_worker(self):
        os.environ.update(HERMES_LOCAL_BASE_URL="http://ornith.example/v1", HERMES_LOCAL_MODEL="ornith:9b")
        token = self.env["HERMES_RUNTIME_TOKEN"]
        cloud = {"task": "test", "key": "cloud-key", "provider": "opencode-go", "model": "kimi-k3"}
        local = {"task": "test", "key": "local-key", "provider": "local", "model": "ornith:9b"}
        legacy = {"task": "test", "key": "local-key"}

        def run_worker(handler, _request):
            handler.send_json(200, {"text": "accepted"})

        with self.server.RuntimeServer(("127.0.0.1", 0), token) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            connection = HTTPConnection(*server.server_address, timeout=2)
            try:
                with mock.patch.object(self.server.Handler, "run_worker", autospec=True, side_effect=run_worker) as worker:
                    for payload, bearer, expected in (
                        (cloud, "", 401),
                        (dict(cloud, base_url="https://other.example"), token, 400),
                        (dict(cloud, provider="auto"), token, 400),
                        (dict(cloud, model="any"), token, 400),
                        (dict(cloud, model=None), token, 400),
                        (dict(cloud, key=" "), token, 400),
                        (dict(local, model="any"), token, 400),
                        (cloud, token, 200),
                        (local, token, 200),
                        (legacy, token, 200),
                    ):
                        connection.request("POST", "/run", json.dumps(payload), {"Authorization": "Bearer " + bearer})
                        response = connection.getresponse()
                        self.assertEqual(response.status, expected)
                        response.read()
                    self.assertEqual([call.args[1] for call in worker.call_args_list], [cloud, local, local])
                    os.environ.pop("HERMES_LOCAL_BASE_URL")
                    connection.request("POST", "/run", json.dumps(legacy), {"Authorization": "Bearer " + token})
                    response = connection.getresponse()
                    self.assertEqual(response.status, 400)
                    response.read()
                    self.assertEqual(worker.call_count, 3)
            finally:
                connection.close()
                server.shutdown()
                thread.join(timeout=2)

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
        self.assertNotIn("CLANKA_ADAPTER_TOKEN", forwarded)

    def test_only_go_workers_receive_the_bridge_token(self):
        # The HTTP runtime credential gets a single explicit route to the bridge.
        os.environ["HERMES_GO_TOOLS"] = "1"
        forwarded = self.server.worker_env()
        self.assertEqual(forwarded["CLANKA_ADAPTER_TOKEN"], self.env["HERMES_RUNTIME_TOKEN"])
        self.assertNotIn("HERMES_RUNTIME_TOKEN", forwarded)
        self.assertNotIn(self.env["HERMES_RUNTIME_TOKEN"], self.server.runtime_config_text({"weather.current"}))

    def test_go_catalogue_controls_search_and_the_iteration_budget(self):
        # An empty grant set leaves the one-turn text-only path intact.
        empty = self.server.agent_kwargs("key", set())
        self.assertEqual(empty["enabled_toolsets"], [])
        self.assertEqual(empty["max_iterations"], 1)
        self.assertEqual(self.server.run_timeout(), 45)
        self.assertEqual(empty["run_budget_seconds"], 40)
        self.assertIn("enabled: off", self.server.runtime_config_text(set()))

        names = {"mcp__clanka_go__weather_current"}
        os.environ.update(HERMES_TOOL_SEARCH="off", HERMES_GO_TOOLS="1")
        self.assertIn("enabled: on", self.server.runtime_config_text(names))
        ready = self.server.agent_kwargs("key", names)
        self.assertEqual(ready["enabled_toolsets"], ["clanka_go"])
        self.assertGreaterEqual(ready["max_iterations"], 4)
        self.assertEqual(self.server.run_timeout(), 240)
        self.assertEqual(ready["run_budget_seconds"], 235)
        self.assertEqual(ready["fallback_model"], {})

    def test_health_and_busy_responses_while_worker_runs(self):
        started = threading.Event()
        popen = subprocess.Popen
        workers = []

        def start_worker(_command, **kwargs):
            # A real child exercises the disconnect cleanup without an upstream model.
            worker = popen([sys.executable, "-c", "import time; time.sleep(10)"], **kwargs)
            workers.append(worker)
            started.set()
            return worker

        token = self.env["HERMES_RUNTIME_TOKEN"]
        with self.server.RuntimeServer(("127.0.0.1", 0), token) as server:
            thread = threading.Thread(target=server.serve_forever, daemon=True)
            thread.start()
            address = server.server_address
            running = HTTPConnection(*address, timeout=2)
            probe = HTTPConnection(*address, timeout=2)
            headers = {"Authorization": "Bearer " + token}
            payload = '{"task":"test","key":"key","provider":"opencode-go","model":"deepseek-v4.1-flash"}'
            try:
                with mock.patch.object(self.server.subprocess, "Popen", side_effect=start_worker):
                    running.request("POST", "/run", payload, headers)
                    self.assertTrue(started.wait(2))
                    probe.request("GET", "/healthz")
                    response = probe.getresponse()
                    self.assertEqual(response.status, 200)
                    response.read()
                    probe.request("POST", "/run", payload, headers)
                    response = probe.getresponse()
                    self.assertEqual(response.status, 503)
                    self.assertEqual(response.getheader("Retry-After"), "1")
                    response.read()
                    self.assertEqual(len(workers), 1)
                    running.close()
                    self.assertLess(workers[0].wait(timeout=2), 0)
            finally:
                running.close()
                probe.close()
                server.shutdown()
                thread.join(timeout=2)

    def test_go_and_sample_modes_are_mutually_exclusive(self):
        # The sample server never contributes tools to the production catalogue.
        os.environ.update(HERMES_GO_TOOLS="1", HERMES_SAMPLE_MCP="1")
        with self.assertRaisesRegex(SystemExit, "Choose Go Tools or sample MCP"):
            self.server.validate_startup()

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
