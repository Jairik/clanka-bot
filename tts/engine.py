"""Pocket TTS model wrapper: load once at startup, stay resident, stream PCM.

Torch and the model are imported lazily so the wire layer and its tests never
need the heavy dependencies. The wrapper also owns the custom voice-state cache
and its model revision, and applies the speech-rate multiplier by resampling.
"""

from __future__ import annotations

import logging
import os
import threading
from importlib.metadata import PackageNotFoundError, version as package_version
from pathlib import Path
from typing import Iterator, Optional

import numpy as np

from pcm import to_pcm16

_LOGGER = logging.getLogger("tts.engine")

# Voice states are pre-exported safetensors files. 32 MB matches the assistant's
# header gate; .pt/.pth/pickle files are never opened.
MAX_STATE_BYTES = 32 * 1024 * 1024
MIN_RATE = 0.5
MAX_RATE = 2.0

# Wyoming advertises ISO codes; Pocket TTS names its model configs in English.
_LANGUAGE_CODES = {
    "english": "en",
    "french": "fr",
    "german": "de",
    "portuguese": "pt",
    "italian": "it",
    "spanish": "es",
    "dutch": "nl",
}


def language_code(name: Optional[str]) -> str:
    if not name:
        return "en"
    prefix = name.split("_", 1)[0].lower()
    return _LANGUAGE_CODES.get(prefix, prefix)


class StreamingResampler:
    """Linear-interpolation rate change with state across audio chunks.

    Pocket TTS 4.0.1 has no speed control, so the effective rate is produced by
    resampling: faster speech has fewer samples at the same 24 kHz rate. This
    changes pitch with speed (no time-stretch); the bounded 0.5-2.0 range keeps
    the artefact acceptable. The trailing fractional sample is dropped, which is
    inaudible at a chunk boundary.
    """

    def __init__(self, rate: float) -> None:
        if not MIN_RATE <= rate <= MAX_RATE:
            raise ValueError(f"rate must be between {MIN_RATE} and {MAX_RATE}")
        # One output sample every `rate` input samples: 2.0 is twice as fast.
        self.step = float(rate)
        self.tail: Optional[np.ndarray] = None
        self.offset = 0.0

    def process(self, samples) -> np.ndarray:
        if hasattr(samples, "detach"):
            samples = samples.detach().cpu().numpy()
        current = np.asarray(samples, dtype=np.float32).reshape(-1)
        if self.tail is not None:
            current = np.concatenate([self.tail, current])
        if current.size < 2:
            self.tail = current
            return np.zeros(0, dtype=np.float32)

        last = current.size - 1
        if self.offset > last:
            self.tail = current
            return np.zeros(0, dtype=np.float32)
        count = int(np.floor((last - self.offset) / self.step)) + 1
        positions = self.offset + np.arange(count, dtype=np.float64) * self.step
        output = np.interp(positions, np.arange(current.size, dtype=np.float64), current)
        consumed = int(np.floor(positions[-1]))
        self.tail = current[consumed:]
        self.offset = positions[-1] + self.step - consumed
        return output.astype(np.float32)


class PocketTtsEngine:
    """One warmed Pocket TTS model plus cached voice states."""

    def __init__(
        self,
        language: Optional[str] = None,
        voices=("alba",),
        voice_dir: Optional[str] = None,
    ) -> None:
        self.language_name = language
        self.language = language_code(language)
        self.voices = list(voices) or ["alba"]
        self.default_voice = self.voices[0]
        self.sample_rate = 24000
        self.version = _package_version()
        # The revision is the stale-voice key. It changes when the package or
        # the pinned model weights change, so a state exported against another
        # build is not silently treated as current.
        self.revision = f"pocket-tts {self.version} {self.language_name or 'default'}"
        self.voice_dir = Path(voice_dir or os.environ.get("TTS_VOICE_DIR", "/voices"))
        self._model = None
        self._states: dict[str, object] = {}

    def load(self) -> None:
        """Load the model and warm the default voices. Blocks until ready."""
        from pocket_tts import TTSModel

        self._model = TTSModel.load_model(language=self.language_name)
        self.sample_rate = int(self._model.sample_rate)
        self.revision = self._revision_from_model()
        for name in self.voices:
            self._states[name] = self._model.get_state_for_audio_prompt(name)
            _LOGGER.info("voice %s warm", name)
        _LOGGER.info("model revision %s", self.revision)

    def _revision_from_model(self) -> str:
        revision = f"pocket-tts {self.version} {self.language_name or 'default'}"
        config = getattr(self._model, "config", None)
        source = ""
        if config is not None:
            source = (
                getattr(config, "weights_path_without_voice_cloning", None)
                or getattr(config, "weights_path", None)
                or ""
            )
        if "@" in str(source):
            revision = f"{revision} {str(source).rsplit('@', 1)[1][:12]}"
        return revision

    def state_for(self, voice: Optional[str]):
        if self._model is None:
            raise RuntimeError("model is not loaded")
        name = voice or self.default_voice
        state = self._states.get(name)
        if state is None:
            # An uncached built-in or a custom .safetensors path is loaded once
            # and stays cached for the process lifetime.
            state = self._model.get_state_for_audio_prompt(name)
            self._states[name] = state
        return state

    def warm_voice(self, voice: Optional[str]) -> str:
        """Load a voice into the cache without generating audio."""
        name = voice or self.default_voice
        self.state_for(name)
        _LOGGER.info("voice %s warm", name)
        return name

    def validate_voice(self, path: str) -> str:
        """Load a copied state and prove it generates audio.

        The caller holds the server lock. The path must resolve inside
        voice_dir; only .safetensors files are accepted. The state stays cached
        after a successful smoke test.
        """
        resolved = self._allowed_state_path(path)
        state = self.state_for(str(resolved))
        # Drain the short smoke phrase. Stopping after the first chunk can leave
        # PyTorch's decoder thread alive, so the validation consumes the whole
        # generation (a few hundred milliseconds on CPU) and returns cleanly.
        chunks = 0
        bytes_out = 0
        for chunk in self._model.generate_audio_stream(
            model_state=state,
            text_to_generate="Voice check.",
        ):
            if chunk is not None:
                chunks += 1
                bytes_out += len(chunk)
        if chunks == 0 or bytes_out == 0:
            raise RuntimeError("voice state produced no audio")
        _LOGGER.info("voice state validated: %s", resolved.name)
        return self.revision

    def _allowed_state_path(self, raw: str) -> Path:
        root = self.voice_dir.resolve()
        candidate = Path(raw).resolve()
        if not candidate.is_relative_to(root):
            raise ValueError(f"voice state path is outside {root}")
        if candidate.suffix.lower() != ".safetensors":
            raise ValueError("only .safetensors voice states are supported")
        if not candidate.is_file():
            raise ValueError(f"voice state not found: {candidate.name}")
        size = candidate.stat().st_size
        if size > MAX_STATE_BYTES:
            raise ValueError(
                f"voice state is {size} bytes; the limit is {MAX_STATE_BYTES >> 20} MB"
            )
        return candidate

    def synthesize(
        self,
        state,
        text: str,
        stop: Optional[threading.Event] = None,
        rate: float = 1.0,
    ) -> Iterator[bytes]:
        # Resampling happens per chunk and keeps its state, so streaming still
        # starts before the whole reply is generated.
        resampler = StreamingResampler(rate) if abs(rate - 1.0) > 0.001 else None
        for chunk in self._model.generate_audio_stream(
            model_state=state, text_to_generate=text, stop=stop
        ):
            if stop is not None and stop.is_set():
                return
            if resampler is not None:
                chunk = resampler.process(chunk)
                if len(chunk) == 0:
                    continue
            yield to_pcm16(chunk)


def _package_version() -> str:
    try:
        return package_version("pocket-tts")
    except PackageNotFoundError:
        return "unknown"
