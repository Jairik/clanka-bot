"""Capture one headless Hermes turn against an OpenAI-compatible endpoint.

The script is for measurement only. It never prints secrets: Authorization
headers are stripped, and the API key is read from an env var or a file whose
contents are not echoed. Use a private HERMES_HOME so a live sidecar's
config.yaml is not overwritten.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path
import resource
import sys
import time


ROOT = Path(__file__).resolve().parent


def load_server():
    """Import the sidecar module next to this file."""
    spec = importlib.util.spec_from_file_location("hermes_server", ROOT / "server.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def read_key(path):
    """Read an API key from a file without logging the value."""
    return Path(path).read_text(encoding="utf-8").strip()


def rss_kb():
    """Current process RSS in kilobytes (Linux ru_maxrss is already KB)."""
    return int(resource.getrusage(resource.RUSAGE_SELF).ru_maxrss)


def approx_tokens(text):
    """Rough token estimate used only when the endpoint omits usage."""
    if not text:
        return 0
    return max(1, len(text) // 4)


def tool_names(body):
    """Collect tool names from a chat-completions request body."""
    names = []
    for tool in body.get("tools") or []:
        if not isinstance(tool, dict):
            continue
        fn = tool.get("function") if isinstance(tool.get("function"), dict) else tool
        name = fn.get("name")
        if name:
            names.append(name)
    return names


def prompt_chars(body):
    """Count characters in the messages array so we can spot prompt bloat."""
    total = 0
    for message in body.get("messages") or []:
        content = message.get("content")
        if isinstance(content, str):
            total += len(content)
        elif isinstance(content, list):
            for part in content:
                if isinstance(part, dict) and isinstance(part.get("text"), str):
                    total += len(part["text"])
    return total


def install_http_capture(captures):
    """Record outbound HTTP JSON without printing Authorization values."""
    import httpx

    original_send = httpx.Client.send

    def send(self, request, *args, **kwargs):
        record = {
            "method": request.method,
            "host": request.url.host,
            "path": request.url.path,
            "tools": [],
            "tool_count": 0,
            "prompt_chars": 0,
            "prompt_tokens": None,
            "completion_tokens": None,
            "model": None,
        }
        try:
            body = json.loads(request.content.decode("utf-8")) if request.content else {}
        except (UnicodeDecodeError, json.JSONDecodeError, AttributeError):
            body = {}
        if isinstance(body, dict):
            names = tool_names(body)
            record["tools"] = names
            record["tool_count"] = len(names)
            record["prompt_chars"] = prompt_chars(body)
            record["model"] = body.get("model")
        response = original_send(self, request, *args, **kwargs)
        try:
            payload = response.json()
        except Exception:
            payload = None
        if isinstance(payload, dict):
            usage = payload.get("usage") or {}
            record["prompt_tokens"] = usage.get("prompt_tokens")
            record["completion_tokens"] = usage.get("completion_tokens")
        captures.append(record)
        return response

    httpx.Client.send = send
    return original_send


def main():
    """Run one captured AIAgent turn and print a secret-free metrics JSON object."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--task", required=True, help="User task for this turn")
    parser.add_argument("--key-file", help="File holding the endpoint API key")
    parser.add_argument(
        "--label",
        default="run",
        help="Name stored on the metrics object (no-tool, eager, lazy, ...)",
    )
    args = parser.parse_args()

    key = os.environ.get("HERMES_PROBE_API_KEY", "").strip()
    if args.key_file:
        key = read_key(args.key_file)
    if not key:
        raise SystemExit("probe needs HERMES_PROBE_API_KEY or --key-file")

    server = load_server()
    server.write_runtime_config()
    server.discover_configured_mcp()
    kwargs = server.agent_kwargs(key)

    captures = []
    original = install_http_capture(captures)
    started = time.monotonic()
    rss_before = rss_kb()
    text = ""
    failed = False
    try:
        from run_agent import AIAgent
        import contextlib

        agent = AIAgent(**kwargs)
        with contextlib.closing(agent):
            server.assert_tool_allowlist(agent)
            result = agent.run_conversation(args.task, system_message=server.system_message())
            text = (result.get("final_response") or "").strip()
            failed = bool(result.get("failed") or result.get("success") is False)
    finally:
        import httpx

        httpx.Client.send = original

    elapsed_ms = int((time.monotonic() - started) * 1000)
    prompt_tokens = sum(item["prompt_tokens"] or 0 for item in captures)
    completion_tokens = sum(item["completion_tokens"] or 0 for item in captures)
    if prompt_tokens == 0:
        prompt_tokens = sum(approx_tokens("x" * item["prompt_chars"]) for item in captures)

    metrics = {
        "label": args.label,
        "ok": bool(text) and not failed,
        "reply_chars": len(text),
        "wall_ms": elapsed_ms,
        "rss_kb_before": rss_before,
        "rss_kb_max": rss_kb(),
        "http_calls": len(captures),
        "prompt_tokens": prompt_tokens,
        "completion_tokens": completion_tokens,
        "prompt_chars": sum(item["prompt_chars"] for item in captures),
        "tool_names_on_wire": [item["tools"] for item in captures],
        "tool_count_first_call": captures[0]["tool_count"] if captures else 0,
        "hosts": sorted({item["host"] for item in captures if item["host"]}),
        "model": captures[0]["model"] if captures else kwargs.get("model"),
        "allowlist": server.parse_toolsets(),
        "tool_search": server.tool_search_enabled(),
        "sample_mcp": server.sample_mcp_enabled(),
        "sample_mcp_lazy": server.sample_mcp_lazy(),
        "billed": False,
        "cost_note": "local Ornith tokens and latency; no paid provider",
    }
    json.dump(metrics, sys.stdout, indent=2)
    sys.stdout.write("\n")
    if not metrics["ok"]:
        raise SystemExit(2)


if __name__ == "__main__":
    main()
