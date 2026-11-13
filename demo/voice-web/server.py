#!/usr/bin/env python3
"""Local web demo for the cloned Pocket TTS voices.

Serves one page where you type a line and hear any of the cloned voices speak
it. The heavy lifting stays in Pocket TTS's own local server (`pocket-tts
serve`); this process starts it, exposes each `.safetensors` clone to it over
loopback, and proxies speech requests with a plain-JSON API.

Only the Python standard library is used, so it runs on a bare interpreter:

    python3 demo/voice-web/server.py

Voices and the binary default to the user's clanka-bot voice bundle; override
with --voices-dir and --pocket-tts (or VOICE_DEMO_* environment variables).
"""

from __future__ import annotations

import argparse
import http.client
import json
import os
import signal
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
import wave
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Optional
from urllib.parse import unquote, urlparse

HERE = Path(__file__).resolve().parent
DEFAULT_VOICES_DIR = Path(
    os.environ.get("VOICE_DEMO_VOICES_DIR", "~/.local/share/clanka-bot/voices")
).expanduser()
DEFAULT_BINARY = Path(
    os.environ.get("VOICE_DEMO_POCKET_TTS", "~/.local/share/clanka-bot/bin/pocket-tts")
).expanduser()
MAX_TEXT = 500
_MODEL_START_TIMEOUT = 240.0
# Display names that title-casing gets wrong.
_LABELS = {"glados": "GLaDOS"}


class Voice:
    def __init__(self, name: str, voices_dir: Path) -> None:
        self.name = name
        self.path = voices_dir / f"{name}.safetensors"
        self.preview = voices_dir / f"{name}_demo.wav"
        self.prompt = voices_dir / f"{name}.wav"

    def meta(self) -> dict:
        preview_url = f"/voices/{self.preview.name}" if self.preview.is_file() else None
        return {
            "name": self.name,
            "label": _LABELS.get(self.name, self.name.replace("_", " ").title()),
            "preview_url": preview_url,
            "preview_seconds": wav_seconds(self.preview),
            "prompt_seconds": wav_seconds(self.prompt),
        }


def wav_seconds(path: Path) -> Optional[float]:
    try:
        with wave.open(str(path)) as handle:
            return round(handle.getnframes() / float(handle.getframerate()), 2)
    except (OSError, EOFError, wave.Error):
        return None


def discover_voices(voices_dir: Path) -> list[Voice]:
    names = sorted(path.stem for path in voices_dir.glob("*.safetensors"))
    return [Voice(name, voices_dir) for name in names]


def wait_for_health(url: str, process: Optional[subprocess.Popen], timeout: float) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process is not None and process.poll() is not None:
            raise RuntimeError(
                f"pocket-tts exited with status {process.returncode} while loading the model"
            )
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if response.status == 200:
                    return
        except (urllib.error.URLError, ConnectionError, TimeoutError):
            pass
        time.sleep(0.5)
    raise RuntimeError(f"pocket-tts did not become healthy at {url} within {timeout:.0f}s")


def start_model_server(binary: Path, host: str, port: int) -> subprocess.Popen:
    if not binary.is_file():
        raise SystemExit(
            f"pocket-tts binary not found at {binary}\n"
            "Pass --pocket-tts, set VOICE_DEMO_POCKET_TTS, or run with --model-url "
            "if a server is already running."
        )
    env = dict(os.environ)
    # Voices were exported offline against the cached model; never reach out.
    env.setdefault("HF_HUB_OFFLINE", "1")
    print(f"[demo] starting {binary} serve on {host}:{port} ...", flush=True)
    process = subprocess.Popen(
        [str(binary), "serve", "--host", host, "--port", str(port)],
        env=env,
        stdout=sys.stderr,
        stderr=sys.stderr,
    )
    wait_for_health(f"http://{host}:{port}/health", process, _MODEL_START_TIMEOUT)
    print("[demo] model server is warm", flush=True)
    return process


class DemoServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, handler, voices_dir: Path, model_url: str) -> None:
        super().__init__(address, handler)
        self.voices_dir = voices_dir
        self.voices = {voice.name: voice for voice in discover_voices(voices_dir)}
        self.model_url = model_url.rstrip("/")
        self.internal_host = "127.0.0.1"
        self.internal_port = self.server_address[1]
        self.engine = {"name": "Pocket TTS", "detail": "Kyutai 100M, CPU"}


class Handler(BaseHTTPRequestHandler):
    server: DemoServer
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args) -> None:
        sys.stderr.write("[demo] " + (fmt % args) + "\n")

    def do_GET(self) -> None:
        path = urlparse(self.path).path
        if path in ("/", "/index.html"):
            self.send_file(HERE / "index.html", "text/html; charset=utf-8")
        elif path == "/api/voices":
            self.send_json(
                {
                    "voices": [voice.meta() for voice in self.server.voices.values()],
                    "engine": self.server.engine,
                }
            )
        elif path == "/healthz":
            self.send_json({"ok": True})
        elif path.startswith("/voices/"):
            self.send_voice_media(unquote(path[len("/voices/") :]), expose_tensors=False)
        elif path.startswith("/internal/voices/"):
            # Only the local model server should read the voice tensors.
            if self.client_address[0] not in ("127.0.0.1", "::1"):
                self.send_error(HTTPStatus.FORBIDDEN)
                return
            self.send_voice_media(unquote(path[len("/internal/voices/") :]), expose_tensors=True)
        else:
            self.send_error(HTTPStatus.NOT_FOUND)

    def do_POST(self) -> None:
        if urlparse(self.path).path != "/api/speak":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        try:
            payload = self.read_json()
            text = str(payload.get("text", "")).strip()
            voice = self.server.voices.get(str(payload.get("voice", "")))
        except (ValueError, json.JSONDecodeError) as exc:
            self.send_json({"error": f"bad request: {exc}"}, HTTPStatus.BAD_REQUEST)
            return
        if not text:
            self.send_json({"error": "text is required"}, HTTPStatus.BAD_REQUEST)
            return
        if len(text) > MAX_TEXT:
            self.send_json(
                {"error": f"text is longer than {MAX_TEXT} characters"}, HTTPStatus.BAD_REQUEST
            )
            return
        if voice is None:
            self.send_json({"error": "unknown voice"}, HTTPStatus.BAD_REQUEST)
            return
        self.speak(voice, text)

    def read_json(self) -> dict:
        length = int(self.headers.get("Content-Length") or 0)
        if length <= 0 or length > 64 * 1024:
            raise ValueError("expected a JSON body")
        data = self.rfile.read(length)
        parsed = json.loads(data)
        if not isinstance(parsed, dict):
            raise ValueError("expected a JSON object")
        return parsed

    def speak(self, voice: Voice, text: str) -> None:
        boundary = "----voice-demo-" + uuid.uuid4().hex
        voice_url = (
            f"http://{self.server.internal_host}:{self.server.internal_port}"
            f"/internal/voices/{voice.path.name}"
        )
        parts = [
            f"--{boundary}\r\n"
            'Content-Disposition: form-data; name="text"\r\n\r\n'
            f"{text}\r\n",
            f"--{boundary}\r\n"
            'Content-Disposition: form-data; name="voice_url"\r\n\r\n'
            f"{voice_url}\r\n",
            f"--{boundary}--\r\n",
        ]
        body = "".join(parts).encode("utf-8")
        request = urllib.request.Request(
            f"{self.server.model_url}/tts",
            data=body,
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
            method="POST",
        )
        started = time.monotonic()
        headers_sent = False
        try:
            with urllib.request.urlopen(request, timeout=None) as upstream:
                content_type = upstream.headers.get("Content-Type", "audio/wav")
                self.send_response(HTTPStatus.OK)
                self.send_header("Content-Type", content_type)
                self.send_header("Cache-Control", "no-store")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                headers_sent = True
                total = 0
                while True:
                    chunk = upstream.read(16384)
                    if not chunk:
                        break
                    total += len(chunk)
                    self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
                    self.wfile.flush()
                self.wfile.write(b"0\r\n\r\n")
                self.wfile.flush()
                self.log_message(
                    "spoke %s: %d bytes in %.2fs", voice.name, total, time.monotonic() - started
                )
        except (BrokenPipeError, ConnectionResetError):
            self.log_message("client disconnected during %s", voice.name)
        except urllib.error.HTTPError as exc:
            if headers_sent:
                self.close_connection = True
                return
            detail = exc.read().decode("utf-8", "replace").strip()[:500]
            self.send_json(
                {"error": f"tts service rejected the request: {detail or exc.reason}"},
                HTTPStatus.BAD_GATEWAY,
            )
        except (urllib.error.URLError, OSError, http.client.HTTPException) as exc:
            reason = getattr(exc, "reason", exc)
            self.log_message("tts service unreachable: %s", reason)
            if headers_sent:
                # The chunked body is already underway; cut the connection so the
                # browser sees an incomplete response rather than a JSON tail.
                self.close_connection = True
                return
            self.send_json(
                {"error": "The TTS engine stopped responding. Restart the demo server."},
                HTTPStatus.BAD_GATEWAY,
            )

    def send_voice_media(self, name: str, expose_tensors: bool) -> None:
        candidate = Path(name).name
        if expose_tensors:
            voice = next(
                (v for v in self.server.voices.values() if v.path.name == candidate), None
            )
            if voice is None:
                self.send_error(HTTPStatus.NOT_FOUND)
                return
            self.send_file(voice.path, "application/octet-stream")
            return
        voice = next(
            (v for v in self.server.voices.values() if v.preview.name == candidate), None
        )
        if voice is None:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self.send_file(voice.preview, "audio/wav")

    def send_file(self, path: Path, content_type: str) -> None:
        try:
            data = path.read_bytes()
        except OSError:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        self.wfile.write(data)

    def send_json(self, payload: dict, status: HTTPStatus = HTTPStatus.OK) -> None:
        data = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Web demo for the cloned Pocket TTS voices.")
    parser.add_argument("--host", default="127.0.0.1", help="bind address (default: 127.0.0.1)")
    parser.add_argument("--port", type=int, default=8090, help="demo port (default: 8090)")
    parser.add_argument(
        "--voices-dir",
        type=Path,
        default=DEFAULT_VOICES_DIR,
        help="directory with <voice>.safetensors and <voice>_demo.wav",
    )
    parser.add_argument(
        "--pocket-tts", type=Path, default=DEFAULT_BINARY, help="pocket-tts binary"
    )
    parser.add_argument(
        "--model-port", type=int, default=8019, help="private port for pocket-tts serve"
    )
    parser.add_argument(
        "--model-url",
        default=None,
        help="use an already-running pocket-tts server instead of starting one",
    )
    parser.add_argument("--open", action="store_true", help="open the page in a browser")
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    voices_dir = args.voices_dir.expanduser().resolve()
    if not voices_dir.is_dir():
        print(f"[demo] voices directory not found: {voices_dir}", file=sys.stderr)
        return 2
    if not discover_voices(voices_dir):
        print(f"[demo] no *.safetensors voices in {voices_dir}", file=sys.stderr)
        return 2

    process: Optional[subprocess.Popen] = None
    if args.model_url:
        model_url = args.model_url
        wait_for_health(f"{model_url.rstrip('/')}/health", None, _MODEL_START_TIMEOUT)
    else:
        model_url = f"http://127.0.0.1:{args.model_port}"
        process = start_model_server(args.pocket_tts.expanduser(), "127.0.0.1", args.model_port)

    server = DemoServer((args.host, args.port), Handler, voices_dir, model_url)
    page = f"http://{args.host}:{args.port}/"
    print(
        f"[demo] {len(server.voices)} voices: {', '.join(server.voices)}\n[demo] open {page}",
        flush=True,
    )
    if args.open:
        threading.Thread(target=_open_browser, args=(page,), daemon=True).start()

    def shutdown(signum, _frame) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, shutdown)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
    return 0


def _open_browser(url: str) -> None:
    import webbrowser

    time.sleep(0.3)
    webbrowser.open(url)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
