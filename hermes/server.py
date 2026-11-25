"""Serve one explicitly requested Hermes turn at a time.

Jobs can attach the current Go read catalogue or the sample MCP bench tools.
"""

from __future__ import annotations

import contextlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import select
import subprocess
import sys
import threading
import time


MAX_BODY = 16 * 1024
RUN_TIMEOUT = 45
GO_TOOLS_RUN_TIMEOUT = 240
CLOUD_BASE_URL = "https://opencode.ai/inference/go/openai/v1"
CLOUD_MODEL = "deepseek-v4.1-flash"
# Match the Go dashboard allowlist at the authenticated worker boundary.
CLOUD_MODELS = {CLOUD_MODEL, "deepseek-v4-pro", "glm-5.3-flash", "kimi-k3"}
REPLY_INSTRUCTION = "Reply in plain language in one or two concise sentences."
SAMPLE_TOOLSET = "clanka_sample"
SAMPLE_NAMES = {"mcp__clanka_sample__sample_ping", "mcp__clanka_sample__sample_time"}
TOOL_SEARCH_NAMES = {"tool_search", "tool_describe", "tool_call"}
GO_TOOLSET = "clanka_go"
BRIDGE_TOKEN_ENV = "CLANKA_ADAPTER_TOKEN"
DEFAULT_SAMPLE_MCP = "/opt/clanka/sample_mcp.py"
DEFAULT_SAMPLE_PYTHON = "/opt/hermes/.venv/bin/python"

# Worker processes only inherit the names they need to reach the model and tools.
WORKER_ENV_NAMES = (
    "PATH",
    "HOME",
    "PYTHONPATH",
    "PYTHONDONTWRITEBYTECODE",
    "HERMES_HOME",
    "HERMES_WRITE_SAFE_ROOT",
    "HERMES_LOCAL_BASE_URL",
    "HERMES_LOCAL_MODEL",
    "HERMES_TOOL_SEARCH",
    "HERMES_GO_TOOLS",
    "HERMES_SAMPLE_MCP",
    "HERMES_SAMPLE_MCP_LAZY",
    "HERMES_SAMPLE_MCP_PATH",
    "HERMES_SAMPLE_MCP_PYTHON",
)


def env(name, default=""):
    """Read a stripped environment value, treating unset as the default."""
    value = os.environ.get(name)
    if value is None:
        return default
    return value.strip()


def truthy(name):
    """True when the named env var is a common on-flag."""
    return env(name).lower() in ("1", "true", "yes", "on")


def run_timeout():
    # Deferred tool discovery and calls add model round trips to a Go job.
    return GO_TOOLS_RUN_TIMEOUT if truthy("HERMES_GO_TOOLS") else RUN_TIMEOUT


def local_base_url():
    """OpenAI-compatible base URL for a self-hosted endpoint, or empty."""
    return env("HERMES_LOCAL_BASE_URL")


def local_model():
    """Model id for the self-hosted endpoint, or empty."""
    return env("HERMES_LOCAL_MODEL")


def sample_mcp_enabled():
    """True when the sidecar should attach the two-tool sample MCP server."""
    return truthy("HERMES_SAMPLE_MCP")


def sample_mcp_lazy():
    """True when the sample MCP server should connect on first tool use."""
    raw = env("HERMES_SAMPLE_MCP_LAZY")
    if raw == "":
        return True
    return raw.lower() in ("1", "true", "yes", "on")


def parse_toolsets():
    """Keep the deployed runtime tool-free except for the explicit sample."""
    return [SAMPLE_TOOLSET] if sample_mcp_enabled() else []


def tool_search_enabled():
    """Whether Tool Search should wrap deferred tools.

    Default is on only when an allowlist exists, so a no-tool job never pays
    for the three bridge schemas. An explicit on/off flag wins.
    """
    raw = env("HERMES_TOOL_SEARCH").lower()
    if raw in ("on", "off"):
        return raw == "on"
    return bool(parse_toolsets())


def sample_mcp_path():
    """Path to the sample MCP script inside the container (overridable in tests)."""
    return env("HERMES_SAMPLE_MCP_PATH", DEFAULT_SAMPLE_MCP)


def sample_mcp_python():
    """Interpreter used to spawn the sample MCP server."""
    return env("HERMES_SAMPLE_MCP_PYTHON", DEFAULT_SAMPLE_PYTHON)


def runtime_config_text(go_names=()):
    """YAML for HERMES_HOME/config.yaml: Tool Search plus optional lazy MCP."""
    lines = [
        "# Written by the clanka Hermes sidecar. Do not store secrets here.",
        "tools:",
        "  tool_search:",
        f"    enabled: {'on' if go_names or tool_search_enabled() else 'off'}",
    ]
    if sample_mcp_enabled():
        lines.extend(
            [
                "mcp_servers:",
                f"  {SAMPLE_TOOLSET}:",
                f"    command: {sample_mcp_python()}",
                "    args:",
                f"      - {sample_mcp_path()}",
                f"    lazy: {'true' if sample_mcp_lazy() else 'false'}",
            ]
        )
    return "\n".join(lines) + "\n"


def hermes_home():
    """Directory Hermes reads config.yaml from."""
    return env("HERMES_HOME") or env("HOME") or "/opt/data"


def write_runtime_config(home=None, *, go_names=()):
    """Replace config.yaml in HERMES_HOME so workers see the slim-down settings."""
    root = home or hermes_home()
    os.makedirs(root, exist_ok=True)
    path = os.path.join(root, "config.yaml")
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as handle:
        handle.write(runtime_config_text(go_names))
    os.replace(tmp, path)
    return path


def discover_configured_mcp():
    """Load the sample MCP server into Hermes before the agent snapshots tools.

    AIAgent does not discover MCP on its own; the CLI and gateway do. The
    sidecar has to call this so lazy schema registration and first-use connect
    actually run on the headless path.
    """
    if not sample_mcp_enabled():
        return []
    from tools.mcp_tool_discovery import discover_mcp_tools

    return discover_mcp_tools(allowed_mcp_names=[SAMPLE_TOOLSET])


def model_route(provider=None, model=None):
    """Resolve a submitted model against fixed routes, including legacy probes."""
    url = local_base_url()
    configured_model = local_model()
    if provider is None and model is None:
        provider, model = ("local", configured_model) if url else ("opencode-go", CLOUD_MODEL)
    if provider == "local" and url and configured_model and model == configured_model:
        return "custom", model, url
    if provider == "opencode-go" and model in CLOUD_MODELS:
        return provider, model, CLOUD_BASE_URL
    raise ValueError("invalid Hermes provider or model")


def agent_kwargs(api_key, go_names=(), *, provider=None, model=None):
    """Keyword arguments for AIAgent that match this job's allowlist."""
    toolsets = [GO_TOOLSET] if go_names else parse_toolsets()
    provider, model, base_url = model_route(provider, model)
    return {
        "provider": provider,
        "model": model,
        "base_url": base_url,
        "api_mode": "chat_completions",
        "api_key": api_key,
        "fallback_model": {},
        "enabled_toolsets": toolsets,
        "skip_context_files": True,
        "skip_memory": True,
        "skip_background_review": True,
        "load_soul_identity": False,
        "quiet_mode": True,
        "save_trajectories": False,
        # Budget search, describe, two calls, and a final reply under the outer deadline.
        "max_iterations": 5 if go_names else 4 if toolsets else 1,
        "max_tokens": 768,
        "run_budget_seconds": run_timeout() - 5,
        "ephemeral_system_prompt": REPLY_INSTRUCTION,
    }


def system_message(go_names=()):
    """Give each turn the instructions for its configured Tool surface."""
    if go_names:
        return (
            REPLY_INSTRUCTION
            + " For live facts, find, describe, and call a relevant tool. If unavailable, say so."
            " For web searches, prefer at most three results and cite the sources you use."
        )
    if parse_toolsets():
        return (
            REPLY_INSTRUCTION
            + " When asked to ping or tell the time, call the matching sample tool."
        )
    return REPLY_INSTRUCTION


def assert_tool_allowlist(agent, go_names=()):
    """Fail closed if unexpected tools reach the model."""
    tools = list(agent.tools or [])
    if go_names:
        from model_tools import get_tool_definitions

        # Check both model-visible wrappers and everything reachable via tool_call.
        raw = get_tool_definitions(enabled_toolsets=[GO_TOOLSET], quiet_mode=True, skip_tool_search_assembly=True)
        if ({entry["function"]["name"] for entry in tools} != TOOL_SEARCH_NAMES
                or {entry["function"]["name"] for entry in raw} != go_names):
            raise RuntimeError("Hermes Go Tool scope did not register exactly")
        return
    if not sample_mcp_enabled():
        if tools:
            raise RuntimeError("Hermes initialized with tools")
        return
    names = {entry.get("function", {}).get("name") for entry in tools}
    if names == SAMPLE_NAMES or tool_search_enabled() and names == TOOL_SEARCH_NAMES:
        return
    raise RuntimeError("Hermes sample tools did not register exactly")


def worker_env():
    """Environment the per-job worker is allowed to see."""
    allowed = {name: value for name, value in os.environ.items() if name in WORKER_ENV_NAMES}
    if truthy("HERMES_GO_TOOLS"):
        allowed[BRIDGE_TOKEN_ENV] = env("HERMES_RUNTIME_TOKEN")
    return allowed


def validate_startup():
    """Validate the runtime token and initialize the per-job configuration."""
    if truthy("HERMES_GO_TOOLS") and sample_mcp_enabled():
        raise SystemExit("Choose Go Tools or sample MCP")
    token = env("HERMES_RUNTIME_TOKEN")
    if len(token) < 32:
        raise SystemExit("Hermes requires a private runtime token of at least 32 characters")
    # Partial local configuration must never fall back to OpenCode Go.
    local_settings = (local_base_url(), local_model(), env("HERMES_LOCAL_API_KEY"))
    if any(local_settings) and not all(local_settings):
        raise SystemExit("Set all three Hermes local settings: HERMES_LOCAL_BASE_URL, HERMES_LOCAL_MODEL and HERMES_LOCAL_API_KEY")
    write_runtime_config()
    return token


def model_turn():
    """Run one AIAgent job from an authenticated request on stdin."""
    import io

    started = time.monotonic()
    request = json.load(sys.stdin)
    # Keep upstream diagnostics off the reply channel, including on model errors.
    with contextlib.redirect_stdout(io.StringIO()):
        go_names = set()
        if truthy("HERMES_GO_TOOLS"):
            from go_mcp import GoAdapter, discover

            # The worker keeps the bearer in memory and passes it explicitly to the stdio child.
            adapter = GoAdapter(os.environ.pop(BRIDGE_TOKEN_ENV, ""))
            catalogue = adapter.catalogue()
            write_runtime_config(go_names=catalogue)
            if catalogue:
                go_names = discover(adapter, catalogue)
        else:
            write_runtime_config()
            discover_configured_mcp()
        from run_agent import AIAgent

        agent = AIAgent(**agent_kwargs(
            request["key"], go_names, provider=request.get("provider"), model=request.get("model"),
        ))
        with contextlib.closing(agent):
            assert_tool_allowlist(agent, go_names)
            if go_names:
                # Hermes starts counting at run_conversation. Charge discovery and initialization too.
                remaining = agent.run_budget_seconds - (time.monotonic() - started)
                if remaining <= 0:
                    raise RuntimeError("Go Tool setup exhausted the Hermes run budget")
                agent.run_budget_seconds = remaining
            result = agent.run_conversation(
                request["task"],
                system_message=system_message(go_names),
            )
    text = (result.get("final_response") or "").strip()
    if not text or result.get("failed") or result.get("success") is False:
        raise RuntimeError("Hermes did not return a reply")
    print(json.dumps({"text": text[:1000]}))


class RuntimeServer(ThreadingHTTPServer):
    def __init__(self, address, token):
        self.authorization = ("Bearer " + token).encode()
        # Health checks can run concurrently without allowing overlapping jobs.
        self.job_lock = threading.Lock()
        super().__init__(address, Handler)


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
            # Report readiness over the private network without disclosing the key.
            url = local_base_url()
            model = local_model() if url else ""
            self.send_json(200, {
                "status": "ok",
                "local_model": model,
                "local_base_url": url,
                "local_ready": bool(url and model and env("HERMES_LOCAL_API_KEY")),
            })
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
            if not isinstance(request, dict) or set(request) not in (
                {"task", "key"}, {"task", "key", "provider", "model"},
            ) or any(not isinstance(value, str) for value in request.values()) or not request["task"].strip():
                raise ValueError
            if "provider" not in request:
                # Legacy local probes must never send an unclassified key to the cloud.
                request.update(provider="local", model=local_model())
            provider, _, _ = model_route(request["provider"], request["model"])
            if provider == "custom":
                # Only the sidecar chooses the credential for the local route.
                request["key"] = env("HERMES_LOCAL_API_KEY")
            if not request["key"].strip():
                raise ValueError
        except (ValueError, TypeError, OSError):
            self.send_error(400)
            return

        if not self.server.job_lock.acquire(blocking=False):
            self.send_response(503)
            self.send_header("Retry-After", "1")
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        try:
            self.run_worker(request)
        finally:
            self.server.job_lock.release()

    def run_worker(self, request):
        payload = json.dumps(request).encode()
        deadline = time.monotonic() + run_timeout()
        worker = subprocess.Popen(
            [sys.executable, __file__, "--worker"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            env=worker_env(),
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
        token = validate_startup()
        with RuntimeServer(("0.0.0.0", 8642), token) as server:
            server.serve_forever()
