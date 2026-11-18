"""Serve one explicitly requested, tool-free Hermes turn at a time."""

import contextlib
import hmac
from http.server import BaseHTTPRequestHandler, HTTPServer
import json
import os
import select
import subprocess
import sys
import time

MAX_BODY = 16 * 1024
RUN_TIMEOUT = 45


def model_turn():
    import io

    request = json.load(sys.stdin)
    # Keep upstream diagnostics off the reply channel, including on model errors.
    with contextlib.redirect_stdout(io.StringIO()):
        from run_agent import AIAgent

        agent = AIAgent(
            provider="opencode-go",
            model="deepseek-v4.1-flash",
            base_url="https://opencode.ai/inference/go/openai/v1",
            api_key=request["key"],
            enabled_toolsets=[],
            skip_context_files=True,
            skip_memory=True,
            skip_background_review=True,
            quiet_mode=True,
            max_iterations=1,
            max_tokens=768,
            run_budget_seconds=RUN_TIMEOUT - 5,
        )
        with contextlib.closing(agent):
            if agent.tools:
                raise RuntimeError("Hermes initialized with tools")
            result = agent.run_conversation(
                request["task"],
                system_message="Reply in plain language in one or two concise sentences.",
            )
    text = (result.get("final_response") or "").strip()
    if not text or result.get("failed") or result.get("success") is False:
        raise RuntimeError("Hermes did not return a reply")
    print(json.dumps({"text": text[:1000]}))


class Handler(BaseHTTPRequestHandler):
    timeout = RUN_TIMEOUT + 5

    def log_message(self, *_args):
        # Neither input nor upstream errors belong in container logs.
        return

    def send_json(self, status, body):
        payload = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        if self.path == "/healthz":
            self.send_json(200, {"status": "ok"})
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path != "/run":
            self.send_error(404)
            return
        auth = self.headers.get("Authorization", "").encode()
        if not hmac.compare_digest(auth, self.server.authorization):
            self.send_error(401)
            return
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= MAX_BODY:
                raise ValueError
            request = json.loads(self.rfile.read(size))
            if set(request) != {"task", "key"} or any(
                not isinstance(request[name], str) or not request[name].strip()
                for name in ("task", "key")
            ):
                raise ValueError
        except (ValueError, TypeError, OSError):
            self.send_error(400)
            return

        payload = json.dumps(request).encode()
        deadline = time.monotonic() + RUN_TIMEOUT
        worker = subprocess.Popen(
            [sys.executable, __file__, "--worker"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env={name: value for name, value in os.environ.items()
                 if name in ("PATH", "HOME", "PYTHONPATH", "PYTHONDONTWRITEBYTECODE", "HERMES_HOME", "HERMES_WRITE_SAFE_ROOT")},
        )
        try:
            while time.monotonic() < deadline:
                try:
                    output, _ = worker.communicate(input=payload, timeout=0.25)
                    break
                except subprocess.TimeoutExpired:
                    payload = None
                    # A cancelled assistant request closes its private connection.
                    if select.select([self.connection], [], [], 0)[0]:
                        return
            else:
                self.send_error(504)
                return
            if worker.returncode != 0:
                self.send_error(502)
                return
            self.send_json(200, json.loads(output))
        except (OSError, ValueError):
            with contextlib.suppress(OSError):
                self.send_error(502)
        finally:
            if worker.poll() is None:
                worker.kill()
            worker.wait()


if __name__ == "__main__":
    if sys.argv[1:] == ["--worker"]:
        model_turn()
    else:
        token = os.environ["HERMES_RUNTIME_TOKEN"]
        if len(token) < 32:
            raise SystemExit("Hermes requires a private runtime token of at least 32 characters")
        with HTTPServer(("0.0.0.0", 8642), Handler) as server:
            server.authorization = ("Bearer " + token).encode()
            server.serve_forever()
