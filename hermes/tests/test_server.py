"""Key routing and local-config checks for the Hermes sidecar."""

import importlib.util
import json
import pathlib
import threading
import unittest
import urllib.error
import urllib.request
from http.server import HTTPServer
from unittest import mock

SERVER = pathlib.Path(__file__).resolve().parents[1] / "server.py"
TOKEN = "t" * 32


def load(env):
    """Import server.py fresh with the given environment."""
    with mock.patch.dict("os.environ", env, clear=True):
        spec = importlib.util.spec_from_file_location("hermes_server", SERVER)
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
    return module


class LocalConfigTest(unittest.TestCase):
    def test_all_or_none(self):
        server = load({})
        self.assertIsNone(server.local_config_error("", "", ""))
        self.assertIsNone(server.local_config_error("http://x/v1", "m", "k"))
        for partial in [("http://x/v1", "m", ""), ("http://x/v1", "", "k"), ("", "m", "k"), ("", "", "k"), ("http://x/v1", "m", "  ")]:
            self.assertIsNotNone(server.local_config_error(*partial), partial)


class JobKeyTest(unittest.TestCase):
    def test_local_mode_ignores_the_sent_key(self):
        server = load({})
        self.assertEqual(server.job_key("opencode-key", "http://x/v1", "local-key"), "local-key")
        self.assertEqual(server.job_key("", "http://x/v1", "local-key"), "local-key")

    def test_cloud_mode_needs_the_sent_key(self):
        server = load({})
        self.assertEqual(server.job_key("opencode-key", "", "local-key"), "opencode-key")
        self.assertIsNone(server.job_key("", "", "local-key"))
        self.assertIsNone(server.job_key("  ", "", ""))


class HandlerTest(unittest.TestCase):
    """End to end through /run with the worker replaced by a stub."""

    def run_job(self, env, sent_key):
        server = load(env)
        sent = {}

        class Worker:
            returncode = 0

            def __init__(self, *_args, **kwargs):
                sent["env"] = kwargs["env"]

            def communicate(self, input=None, timeout=None):
                sent["payload"] = json.loads(input)
                return b'{"text": "ok"}', b""

            def poll(self):
                return 0

            def kill(self):
                pass

            def wait(self):
                pass

        httpd = HTTPServer(("127.0.0.1", 0), server.Handler)
        httpd.authorization = ("Bearer " + TOKEN).encode()
        thread = threading.Thread(target=httpd.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        request = urllib.request.Request(
            f"http://127.0.0.1:{httpd.server_address[1]}/run",
            data=json.dumps({"task": "Explain HTTP", "key": sent_key}).encode(),
            headers={"Authorization": "Bearer " + TOKEN, "Content-Type": "application/json"},
        )
        with mock.patch.object(server.subprocess, "Popen", Worker):
            try:
                with urllib.request.urlopen(request, timeout=5) as response:
                    return response.status, sent
            except urllib.error.HTTPError as err:
                err.close()
                return err.code, sent

    def test_local_mode_sends_only_the_local_key_to_the_worker(self):
        env = {"HERMES_LOCAL_BASE_URL": "http://desk:8000/v1", "HERMES_LOCAL_MODEL": "m",
               "HERMES_LOCAL_API_KEY": "local-key"}
        status, sent = self.run_job(env, "opencode-key")
        self.assertEqual(status, 200)
        self.assertEqual(sent["payload"]["key"], "local-key")
        # The key travels on stdin, never in the worker's environment.
        self.assertNotIn("HERMES_LOCAL_API_KEY", sent["env"])

    def test_cloud_mode_rejects_an_empty_key(self):
        status, sent = self.run_job({}, "")
        self.assertEqual(status, 400)
        self.assertNotIn("payload", sent)

    def test_cloud_mode_forwards_the_sent_key(self):
        status, sent = self.run_job({}, "opencode-key")
        self.assertEqual(status, 200)
        self.assertEqual(sent["payload"]["key"], "opencode-key")


if __name__ == "__main__":
    unittest.main()
