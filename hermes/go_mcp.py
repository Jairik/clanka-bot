"""Expose the Go-owned read catalogue through Hermes's lazy stdio MCP path."""

from functools import partial
from http.client import HTTPConnection, HTTPException
import json
import os
import re
import sys

from sample_mcp import ToolFailure, handle, serve


TOOLSET = "clanka_go"
TOKEN_ENV = "CLANKA_ADAPTER_TOKEN"
MAX_RESPONSE = 256 * 1024
INVOKE_TIMEOUT = 6
ALLOWED = {"weather.current", "weather.forecast", "web.search"}


def allowed_name(name):
    # Go owns recipe validation and the live grant for every listed Tool.
    if not isinstance(name, str):
        return False
    return name in ALLOWED or ("__" not in name and re.fullmatch(r"recipe\.[a-z][a-z0-9_]*", name) is not None)


class GoAdapter:
    def __init__(self, token):
        # The private host is fixed and http.client follows no redirects or proxies.
        if len(token) < 32:
            raise RuntimeError("Go Tool adapter token missing")
        self.token = token

    def request(self, name=None, arguments=None):
        # One request per read or invocation keeps Go's live policy in the path.
        if name is not None and not allowed_name(name):
            raise ToolFailure("Invalid Go Tool name.")
        calling = arguments is not None
        conn = HTTPConnection("assistant", 8080, timeout=INVOKE_TIMEOUT if calling else 2)
        path = "/hermes/adapter/tools"
        if name is not None:
            path += "/" + name
        headers = {"Authorization": "Bearer " + self.token}
        body = None
        if calling:
            headers["Content-Type"] = "application/json"
            body = json.dumps({"arguments": arguments}).encode()
        try:
            conn.request("POST" if calling else "GET", path, body=body, headers=headers)
            response = conn.getresponse()
            if response.status == 403:
                raise ToolFailure("Tool is no longer available.")
            if response.status != 200:
                raise ValueError
            raw = response.read(MAX_RESPONSE + 1)
            if len(raw) > MAX_RESPONSE:
                raise ValueError
            data = json.loads(raw)
            # Treat even an accidental token echo from Go as an invalid reply.
            if not isinstance(data, dict) or json.dumps(self.token)[1:-1] in json.dumps(data):
                raise ValueError
            return data
        except (HTTPException, OSError, ValueError):
            raise ToolFailure("Go Tool adapter unavailable.") from None
        finally:
            conn.close()

    def catalogue(self):
        # Use only metadata here. MCP requires an object schema even for deferred tools.
        tools = self.request().get("tools")
        if not isinstance(tools, list):
            raise ToolFailure("Invalid Go Tool catalogue.")
        return [
            {
                "name": item["name"],
                "description": item["description"],
                "inputSchema": {"type": "object", "additionalProperties": True},
                "annotations": {"readOnlyHint": True},
            }
            for item in tools
            if isinstance(item, dict) and allowed_name(item.get("name"))
            and item.get("permission") == "read" and isinstance(item.get("description"), str)
        ]

    def describe(self, name):
        # A named schema read rechecks the Tool's grant and picks up recipe edits.
        data = self.request(name)
        schema = data.get("input_schema")
        if data.get("name") != name or data.get("permission") != "read" or not isinstance(schema, dict):
            raise ToolFailure("Invalid Go Tool schema.")
        return schema

    def invoke(self, name, arguments):
        # Go validates arguments and applies read, privacy, and grant policy on every call.
        if name is None or not isinstance(arguments, dict):
            raise ToolFailure("Invalid Go Tool call.")
        data = self.request(name, arguments)
        if data.get("name") != name:
            raise ToolFailure("Invalid Go Tool reply.")
        if isinstance(data.get("error"), str):
            raise ToolFailure(data["error"])
        if "result" not in data:
            raise ToolFailure("Invalid Go Tool reply.")
        return json.dumps(data["result"], ensure_ascii=False)


def discover(adapter, tools):
    # Refresh the manifest before registration so the previous job cannot hide edits or revocations.
    from tools import mcp_schema_cache, tool_search
    from tools.mcp_tool_discovery import register_mcp_servers
    from tools.mcp_tool_schema import mcp_prefixed_tool_name, strip_schema_annotations

    config = {
        "command": sys.executable,
        "args": [os.path.abspath(__file__)],
        "env": {TOKEN_ENV: adapter.token},
        # A later grant can join the next job. First-use startup may only remove names.
        "tools": {"include": [item["name"] for item in tools]},
        "lazy": True,
        "connect_timeout": 3,
        "timeout": INVOKE_TIMEOUT + 2,
    }
    names = {mcp_prefixed_tool_name(TOOLSET, item["name"]): item["name"] for item in tools}
    mcp_schema_cache.write_cache_entry(TOOLSET, mcp_schema_cache.config_fingerprint(config), tools=tools)
    if set(register_mcp_servers({TOOLSET: config})) != names.keys():
        raise RuntimeError("Go Tools did not register exactly")

    describe = tool_search.dispatch_tool_describe

    def describe_live(*args, **kwargs):
        # v0.23.3 describes registered metadata verbatim. Resolve only the scoped hits it returns.
        result = json.loads(describe(*args, **kwargs))
        for wire_name, entry in list(result.get("tools", {}).items()):
            if wire_name in names:
                try:
                    schema = adapter.describe(names[wire_name])
                    entry["parameters"] = strip_schema_annotations(schema, wire_name)
                except ToolFailure as error:
                    del result["tools"][wire_name]
                    result.setdefault("errors", {})[wire_name] = str(error)
        return json.dumps(result)

    tool_search.dispatch_tool_describe = describe_live
    return set(names)


if __name__ == "__main__":
    # Keep the bearer out of JSON-RPC, argv, config files, and stdio diagnostics.
    adapter = GoAdapter(os.environ.pop(TOKEN_ENV, ""))
    serve(sys.stdin, sys.stdout, partial(handle, list_tools=adapter.catalogue,
                                         invoke=adapter.invoke, server_name=TOOLSET))
