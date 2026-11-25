"""Tiny stdio MCP server used to measure Hermes tool discovery.

Hermes's MCP client speaks newline-delimited JSON-RPC on stdin/stdout. This
server exposes two read-only sample tools so we can compare an empty
allowlist against lazy schema loading without pulling in Hermes built-ins.
"""

from __future__ import annotations

from datetime import datetime, timezone
import json
import sys


# Names the sidecar allowlists when HERMES_SAMPLE_MCP is on.
PING_TOOL = "sample_ping"
TIME_TOOL = "sample_time"

# Echo the client's protocol version when we can; otherwise use a stable default.
DEFAULT_PROTOCOL = "2024-11-05"


def utc_now():
    """Return the current UTC time as a compact ISO-8601 string."""
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def tool_list():
    """Describe the two sample tools for tools/list."""
    empty_object = {"type": "object", "properties": {}, "additionalProperties": False}
    return [
        {
            "name": PING_TOOL,
            "description": "Return pong. Used to prove a sample Hermes tool call.",
            "inputSchema": empty_object,
        },
        {
            "name": TIME_TOOL,
            "description": "Return the current UTC time as ISO-8601.",
            "inputSchema": empty_object,
        },
    ]


def call_tool(name, _arguments):
    """Run one sample tool. Unknown names become JSON-RPC tool errors."""
    if name == PING_TOOL:
        return "pong"
    if name == TIME_TOOL:
        return utc_now()
    raise KeyError(name)


def handle(message, list_tools=tool_list, invoke=call_tool, server_name="clanka-sample"):
    """Turn one JSON-RPC request into a response dict, or None for a notification."""
    method = message.get("method")
    msg_id = message.get("id")
    params = message.get("params") or {}

    # Notifications have no id and must not be answered.
    if msg_id is None:
        return None

    if method == "initialize":
        protocol = params.get("protocolVersion") or DEFAULT_PROTOCOL
        return {
            "jsonrpc": "2.0",
            "id": msg_id,
            "result": {
                "protocolVersion": protocol,
                "capabilities": {"tools": {}},
                "serverInfo": {"name": server_name, "version": "1"},
            },
        }

    if method == "tools/list":
        try:
            tools = list_tools()
        except ToolFailure as error:
            # Report list failures without ending the stdio session.
            return {"jsonrpc": "2.0", "id": msg_id, "error": {"code": -32603, "message": str(error)}}
        return {"jsonrpc": "2.0", "id": msg_id, "result": {"tools": tools}}

    if method == "ping":
        return {"jsonrpc": "2.0", "id": msg_id, "result": {}}

    if method == "tools/call":
        name = params.get("name")
        try:
            text = invoke(name, params.get("arguments", {}))
        except ToolFailure as error:
            # Tool-level failures are visible to the model without transport details.
            return {
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {"content": [{"type": "text", "text": str(error)}], "isError": True},
            }
        except KeyError:
            return {
                "jsonrpc": "2.0",
                "id": msg_id,
                "error": {"code": -32601, "message": f"Unknown tool {name}"},
            }
        return {
            "jsonrpc": "2.0",
            "id": msg_id,
            "result": {"content": [{"type": "text", "text": text}], "isError": False},
        }

    return {
        "jsonrpc": "2.0",
        "id": msg_id,
        "error": {"code": -32601, "message": f"Unknown method {method}"},
    }


class ToolFailure(Exception):
    # Only sanitized messages belong in MCP tool failures.
    pass


def serve(stdin, stdout, handler=handle):
    """Read NDJSON JSON-RPC from stdin and write responses to stdout."""
    for raw in stdin:
        line = raw.strip()
        if not line:
            continue
        try:
            message = json.loads(line)
        except json.JSONDecodeError:
            continue
        reply = handler(message)
        if reply is None:
            continue
        stdout.write(json.dumps(reply, separators=(",", ":")) + "\n")
        stdout.flush()


def main():
    """Run as a stdio MCP server. Logs stay off stdout so they cannot break framing."""
    serve(sys.stdin, sys.stdout)


if __name__ == "__main__":
    main()
