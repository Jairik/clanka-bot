"""Pocket TTS model wrapper: load once at startup, stay resident, stream PCM.

Torch and the model are imported lazily so the wire layer and its tests never
need the heavy dependencies.
"""

from __future__ import annotations

import logging
import threading
from importlib.metadata import PackageNotFoundError, version as package_version
from typing import Iterator, Optional

from pcm import to_pcm16

_LOGGER = logging.getLogger("tts.engine")

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


class PocketTtsEngine:
    """One warmed Pocket TTS model plus cached voice states."""

    def __init__(self, language: Optional[str] = None, voices=("alba",)) -> None:
        self.language_name = language
        self.language = language_code(language)
        self.voices = list(voices) or ["alba"]
        self.default_voice = self.voices[0]
        self.sample_rate = 24000
        self.version = _package_version()
        self._model = None
        self._states: dict[str, object] = {}

    def load(self) -> None:
        """Load the model and warm the default voices. Blocks until ready."""
        from pocket_tts import TTSModel

        self._model = TTSModel.load_model(language=self.language_name)
        self.sample_rate = int(self._model.sample_rate)
        for name in self.voices:
            self._states[name] = self._model.get_state_for_audio_prompt(name)
            _LOGGER.info("voice %s warm", name)

    def state_for(self, voice: Optional[str]):
        if self._model is None:
            raise RuntimeError("model is not loaded")
        name = voice or self.default_voice
        state = self._states.get(name)
        if state is None:
            # An uncached voice (a file or HuggingFace URL) is encoded once.
            state = self._model.get_state_for_audio_prompt(name)
            self._states[name] = state
        return state

    def synthesize(self, state, text: str, stop: Optional[threading.Event] = None) -> Iterator[bytes]:
        for chunk in self._model.generate_audio_stream(
            model_state=state, text_to_generate=text, stop=stop
        ):
            if stop is not None and stop.is_set():
                return
            yield to_pcm16(chunk)


def _package_version() -> str:
    try:
        return package_version("pocket-tts")
    except PackageNotFoundError:
        return "unknown"
