#!/usr/bin/env python3
"""Fake OpenAI-compatible SSE provider for #27 instrumentation validation.

This is NOT a real model and its latency is NOT a model measurement. It exists
only to drive the Agent path's timing hooks (first token, streaming chunks,
TTS) without an OpenRouter key.

    python3 scripts/latency/fake-provider.py [port] [first_token_s] [token_s]

Put it on the Compose network and point a copy of assistant/config.yaml at
http://<container>:<port>/v1, then mount that copy as ASSISTANT_CONFIG.
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

FIRST_DELAY = float(sys.argv[2]) if len(sys.argv) > 2 else 0.30
TOKEN_DELAY = float(sys.argv[3]) if len(sys.argv) > 3 else 0.03
REPLY = "The capital of France is Paris. It is known for the Eiffel Tower."


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(length)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        def event(payload):
            try:
                self.wfile.write(b"data: " + json.dumps(payload).encode() + b"\n\n")
                self.wfile.flush()
            except BrokenPipeError:
                raise ConnectionAbortedError

        base = {"id": "chatcmpl-fake", "object": "chat.completion.chunk",
                "created": 0, "model": "fake-model"}
        event({**base, "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]})
        try:
            time.sleep(FIRST_DELAY)
            for word in REPLY.split(" "):
                event({**base, "choices": [{"index": 0, "delta": {"content": word + " "}, "finish_reason": None}]})
                time.sleep(TOKEN_DELAY)
            event({**base, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}]})
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except ConnectionAbortedError:
            pass


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 19099
    ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
