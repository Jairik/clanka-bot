#!/usr/bin/env python3
"""Scripted OpenAI-compatible test double for the #29 prototype run.

This is NOT a model. Its replies and its latency are scripted so the Agent path
can be exercised without an OpenRouter key. Never read its numbers as model
latency, and never present its reply as a real answer.

The double picks a plan from the conversation it receives:

* a request containing "provider failure" -> HTTP 401, the same provider-error
  path a bad key takes;
* "what time is it" with the time tool offered -> one streamed `time__now` Tool
  call, then a final answer on the follow-up request that carries the result;
* "timer test" with the timer tool offered -> one streamed `timer__set` call
  (300 s, or 10 s when the utterance says "ten seconds"), so the Timer Tool and
  its alert can be exercised through the Agent path;
* "tell me a long story" -> a deliberately slow multi-sentence reply, so a
  Barge-in can land mid-reply;
* "what did I ask before" -> an answer that counts the prior user turns, which
  proves the shared history reached the provider;
* anything else -> a fixed, clearly labelled reply.

Every request is appended to FAKE_LOG as one JSON line, including the messages
it received, so the harness can verify history, prefix stripping and Tool
results from the outside.

    python3 fake_provider.py            # listens on :8000
"""

import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(os.environ.get("FAKE_PORT", "8000"))
FIRST_DELAY = float(os.environ.get("FAKE_FIRST_DELAY", "0.20"))
TOKEN_DELAY = float(os.environ.get("FAKE_TOKEN_DELAY", "0.03"))
SLOW_TOKEN_DELAY = float(os.environ.get("FAKE_SLOW_TOKEN_DELAY", "0.20"))
LOG_PATH = os.environ.get("FAKE_LOG", "")

# The default reply names the test double so a transcript can never be mistaken
# for a real model answer.
REPLY = (
    "This is a scripted test double reply. "
    "The capital of France is Paris."
)

# Short sentences so the sentence chunker reaches TTS before a Barge-in lands.
SLOW_REPLY = (
    "This is a scripted test double reply. "
    "It is deliberately slow. "
    "Each sentence is short. "
    "The test double is still talking. "
    "You can barge in now. "
    "This is the fifth sentence. "
    "And a sixth one. "
    "And a seventh one to finish."
)

_log_lock = threading.Lock()


def plan(messages, tools):
    """Decide how to answer one request. Pure apart from the clock, so tests
    can drive it directly. `tools` is the list of offered wire names."""
    last = messages[-1] if messages else {}
    role = last.get("role")

    if role == "tool":
        result = str(last.get("content", ""))
        return {
            "action": "content",
            "text": "The scripted tool call returned " + result + ".",
        }

    text = str(last.get("content", "")).lower()
    if "provider failure" in text:
        return {"action": "fail", "status": 401}
    if "long story" in text:
        return {"action": "slow", "text": SLOW_REPLY}
    if "what did i ask" in text:
        prior = [m for m in messages[:-1] if m.get("role") == "user"]
        return {
            "action": "content",
            "text": "You have asked %d question(s) before this one." % len(prior),
        }
    if "timer test" in text and "timer__set" in tools:
        seconds = 10 if "ten seconds" in text else 300
        return {
            "action": "tool_call",
            "name": "timer__set",
            "arguments": json.dumps({"duration_seconds": seconds}),
        }
    if "what time is it" in text and "time__now" in tools:
        return {"action": "tool_call", "name": "time__now", "arguments": "{}"}
    return {"action": "content", "text": REPLY}


def log_request(path, body, tools, decision):
    if not LOG_PATH:
        return
    record = {
        "time": time.time(),
        "path": path,
        "model": body.get("model"),
        "messages": body.get("messages", []),
        "tools": tools,
        "action": decision,
    }
    with _log_lock:
        with open(LOG_PATH, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(record) + "\n")
            handle.flush()


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *args):
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self._json(200, {"status": "ok"})
            return
        self._json(404, {"error": "not found"})

    def _json(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        try:
            body = json.loads(raw or b"{}")
        except ValueError:
            self._json(400, {"error": "invalid json"})
            return

        tools = []
        for tool in body.get("tools") or []:
            name = (tool.get("function") or {}).get("name")
            if name:
                tools.append(name)

        decision = plan(body.get("messages", []), tools)
        log_request(self.path, body, tools, decision)

        if decision["action"] == "fail":
            self._json(
                decision["status"],
                {
                    "error": {
                        "message": "scripted test-double provider failure",
                        "type": "invalid_request_error",
                        "code": "scripted_failure",
                    }
                },
            )
            return

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        base = {
            "id": "chatcmpl-scripted",
            "object": "chat.completion.chunk",
            "created": 0,
            "model": body.get("model", "scripted-test-double"),
        }

        def event(payload):
            try:
                self.wfile.write(
                    b"data: " + json.dumps(payload).encode() + b"\n\n"
                )
                self.wfile.flush()
            except BrokenPipeError:
                raise ConnectionAbortedError

        def delta(content="", tool_calls=None, finish_reason=None):
            payload = {**base, "choices": [{"index": 0, "delta": {}, "finish_reason": finish_reason}]}
            if content:
                payload["choices"][0]["delta"]["content"] = content
            if tool_calls:
                payload["choices"][0]["delta"]["tool_calls"] = tool_calls
            event(payload)

        try:
            delta()  # role-only prologue
            if decision["action"] == "tool_call":
                delta(
                    tool_calls=[
                        {
                            "index": 0,
                            "id": "call_scripted_1",
                            "type": "function",
                            "function": {
                                "name": decision["name"],
                                "arguments": "",
                            },
                        }
                    ]
                )
                delta(
                    tool_calls=[
                        {
                            "index": 0,
                            "function": {"arguments": decision["arguments"]},
                        }
                    ]
                )
                delta(finish_reason="tool_calls")
            else:
                time.sleep(FIRST_DELAY)
                delay = SLOW_TOKEN_DELAY if decision["action"] == "slow" else TOKEN_DELAY
                for word in decision["text"].split(" "):
                    delta(content=word + " ")
                    time.sleep(delay)
                delta(finish_reason="stop")
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionAbortedError):
            pass


class Server(ThreadingHTTPServer):
    daemon_threads = True


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else PORT
    print(
        "scripted test double on :%d (first %.2fs, token %.2fs, slow %.2fs)"
        % (port, FIRST_DELAY, TOKEN_DELAY, SLOW_TOKEN_DELAY),
        flush=True,
    )
    Server(("0.0.0.0", port), Handler).serve_forever()


if __name__ == "__main__":
    main()
