"""Moonshine Voice model wrapper: load once at startup, stream one request.

The heavy import happens lazily so the wire layer and its tests run without the
ONNX runtime. Model directories come from the build-time manifest, so nothing
here touches the network; a fallback model is loaded on first use.
"""

from __future__ import annotations

import json
import threading
from importlib.metadata import PackageNotFoundError, version as package_version
from pathlib import Path

MODEL_ROOT = Path("/opt/moonshine")
MANIFEST = MODEL_ROOT / "models.json"

# Partial transcripts are refreshed at this interval while audio streams in.
# The Go middleware only needs the final text; the partials are for observability.
PARTIAL_INTERVAL = 0.2


def load_manifest(path: Path = MANIFEST) -> dict[str, str]:
    if not path.is_file():
        raise RuntimeError(f"missing model manifest {path}; rebuild the stt image")
    return json.loads(path.read_text())


class MoonshineStream:
    """One streaming transcription. Calls are serialised by the engine lock."""

    def __init__(self, transcriber, lock: threading.Lock) -> None:
        self._transcriber = transcriber
        self._lock = lock
        self._partial = ""
        self._stream = None

    def _ensure_stream(self):
        if self._stream is None:
            # 0 requests a pass on every add_audio; PARTIAL_INTERVAL keeps the
            # partial text fresh without transcribing every 32 ms frame.
            self._stream = self._transcriber.create_stream(update_interval=PARTIAL_INTERVAL)
            self._stream.add_listener(self._on_event)
            self._stream.start()
        return self._stream

    def _on_event(self, event) -> None:
        # LineTextChanged carries the in-progress text for the current line.
        text = getattr(getattr(event, "line", None), "text", "")
        if text:
            self._partial = text

    @property
    def partial(self) -> str:
        return self._partial

    def add_audio(self, samples, rate: int) -> None:
        with self._lock:
            self._ensure_stream().add_audio(list(samples), rate)

    def finish(self) -> str:
        with self._lock:
            stream = self._ensure_stream()
            transcript = stream.stop()
            text = transcript_text(transcript)
            stream.close()
            self._stream = None
            return text

    def close(self) -> None:
        with self._lock:
            if self._stream is not None:
                self._stream.close()
                self._stream = None


class MoonshineEngine:
    """Owns the baked models and hands out one stream per request."""

    def __init__(
        self,
        model: str = "small-streaming",
        language: str = "en",
        manifest: Path = MANIFEST,
    ) -> None:
        self.model = model
        self.language = language
        self.manifest_path = manifest
        self.sample_rate = 16000
        self.version = _package_version()
        # Serialises stream work: one mic, one transcription at a time.
        self._stream_lock = threading.Lock()
        # Loading is guarded separately so a fallback model can warm mid-run.
        self._load_lock = threading.Lock()
        self._transcribers: dict[str, object] = {}

    def available_models(self) -> list[str]:
        return sorted(load_manifest(self.manifest_path))

    def load(self) -> None:
        """Load the configured model. Blocks until ready so health gates on it."""
        self._load(self.model)

    def _load(self, name: str) -> None:
        with self._load_lock:
            if name in self._transcribers:
                return
            from moonshine_voice import Transcriber
            from moonshine_voice.moonshine_api import string_to_model_arch

            manifest = load_manifest(self.manifest_path)
            path = manifest.get(name)
            if path is None:
                raise RuntimeError(
                    f"model {name!r} is not baked into the image "
                    f"(available: {', '.join(sorted(manifest))})"
                )
            self._transcribers[name] = Transcriber(path, string_to_model_arch(name))

    def create_stream(self, model: str | None = None):
        """Create a stream for model (or the configured default)."""
        name = model or self.model
        self._load(name)
        return MoonshineStream(self._transcribers[name], self._stream_lock)


def transcript_text(transcript) -> str:
    """Join the completed lines of a Moonshine Transcript into one string."""
    lines = getattr(transcript, "lines", None) or []
    parts = [line.text.strip() for line in lines if getattr(line, "text", "").strip()]
    return " ".join(parts).strip()


def _package_version() -> str:
    try:
        return package_version("moonshine-voice")
    except PackageNotFoundError:
        return "unknown"
