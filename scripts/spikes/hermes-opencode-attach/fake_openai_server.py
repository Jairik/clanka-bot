#!/usr/bin/env python3
"""Tiny OpenAI-compatible chat-completions stub for the #76 attach harness.

It calls no model. It exists only to exercise OpenCode's provider HTTP path:
a good bearer gets a streamed text answer, any other bearer gets 401, and a
prompt containing SLOW gets a stream that is slow enough to cancel.
"""
import argparse
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    good_token = "Bearer spike-good"
    log_path = ""

    def log_message(self, fmt, *args):
        if self.log_path:
            with open(self.log_path, "a", encoding="utf-8") as fh:
                fh.write("fake " + (fmt % args) + "\n")

    def json_body(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.path == "/v1/models":
            return self.json_body(200, {"object": "list", "data": [{"id": "fake-model", "object": "model"}]})
        return self.json_body(404, {"error": {"message": "not found"}})

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            return self.json_body(404, {"error": {"message": "not found"}})
        length = int(self.headers.get("Content-Length", "0"))
        try:
            req = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            return self.json_body(400, {"error": {"message": "bad json"}})
        if self.headers.get("Authorization") != self.good_token:
            return self.json_body(401, {"error": {"type": "authentication_error", "message": "bad spike token"}})

        text = " ".join(str(m.get("content", "")) for m in req.get("messages", []) if isinstance(m, dict))
        slow = "SLOW" in text
        chunks = ["fake", " summary", ": attach", " path", " reached", " the", " provider", "."]
        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "keep-alive")
            self.end_headers()
            base = {"id": "chatcmpl-spike", "object": "chat.completion.chunk", "created": 0, "model": "fake-model"}
            first = dict(base, choices=[{"index": 0, "delta": {"role": "assistant", "content": ""}, "finish_reason": None}])
            self.wfile.write(("data: " + json.dumps(first) + "\n\n").encode())
            self.wfile.flush()
            for chunk in chunks:
                if slow:
                    time.sleep(3)
                delta = dict(base, choices=[{"index": 0, "delta": {"content": chunk}, "finish_reason": None}])
                self.wfile.write(("data: " + json.dumps(delta) + "\n\n").encode())
                self.wfile.flush()
            final = dict(base, choices=[{"index": 0, "delta": {}, "finish_reason": "stop"}])
            self.wfile.write(("data: " + json.dumps(final) + "\n\ndata: [DONE]\n\n").encode())
            self.wfile.flush()
            return

        body = {
            "id": "chatcmpl-spike",
            "object": "chat.completion",
            "created": 0,
            "model": "fake-model",
            "choices": [
                {"index": 0, "message": {"role": "assistant", "content": "".join(chunks)}, "finish_reason": "stop"}
            ],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
        }
        return self.json_body(200, body)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--log", default="")
    parser.add_argument("--token", default="spike-good")
    args = parser.parse_args()
    Handler.good_token = "Bearer " + args.token
    Handler.log_path = args.log
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
