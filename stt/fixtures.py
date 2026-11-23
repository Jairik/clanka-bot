"""Load the STT replay corpus and build non-sensitive synthetic PCM.

Checked-in clips are synthetic (silence, speech-shaped bursts, noise). Owner
voice belongs in stt/fixtures/private/, which git ignores. A private WAV whose
stem matches a manifest id replaces the synthetic stand-in for that clip, so
the same phrases and endpointing settings can be replayed on real audio later.
"""

from __future__ import annotations

import json
import math
import struct
import wave
from pathlib import Path

from endpoint import (
    FRAME_BYTES,
    RATE,
    dbfs_amplitude,
    silence_frame,
    speech_frame,
)

DEFAULT_MANIFEST = Path(__file__).resolve().parent / "fixtures" / "manifest.json"
PRIVATE_DIR = Path(__file__).resolve().parent / "fixtures" / "private"


def load_manifest(path: Path | None = None) -> list[dict]:
    """Return the clip records. Each record is one comparable utterance."""
    path = path or DEFAULT_MANIFEST
    data = json.loads(path.read_text())
    clips = data.get("clips") if isinstance(data, dict) else data
    if not isinstance(clips, list):
        raise ValueError(f"{path} must contain a clips list")
    return clips


def pcm_for(clip: dict, audio_dir: Path | None = None) -> bytes:
    """Prefer a real WAV, then a private WAV, then the synthetic recipe."""
    clip_id = str(clip.get("id") or "")
    audio_name = clip.get("audio")
    candidates = []
    if audio_dir is not None and audio_name:
        candidates.append(Path(audio_dir) / audio_name)
    if audio_dir is not None and clip_id:
        candidates.append(Path(audio_dir) / f"{clip_id}.wav")
    if clip_id:
        candidates.append(PRIVATE_DIR / f"{clip_id}.wav")
    if audio_name:
        candidates.append(DEFAULT_MANIFEST.parent / audio_name)
    for candidate in candidates:
        if candidate.is_file():
            return read_wav(candidate)
    return synthesize(clip.get("synthetic") or {"kind": "silence", "duration_ms": 500})


def read_wav(path: Path) -> bytes:
    """Read 16 kHz mono PCM16. Other rates are refused so comparisons stay honest."""
    with wave.open(str(path), "rb") as handle:
        if handle.getnchannels() != 1:
            raise ValueError(f"{path} must be mono")
        if handle.getsampwidth() != 2:
            raise ValueError(f"{path} must be 16-bit")
        if handle.getframerate() != RATE:
            raise ValueError(f"{path} must be {RATE} Hz")
        return handle.readframes(handle.getnframes())


def write_wav(path: Path, pcm: bytes, rate: int = RATE) -> None:
    """Write a 16 kHz mono PCM16 WAV for host-native replay."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(1)
        handle.setsampwidth(2)
        handle.setframerate(rate)
        handle.writeframes(pcm)


def synthesize(spec: dict) -> bytes:
    """Build a tiny non-speech stand-in from the manifest's synthetic recipe.

    These clips exercise endpointing and the benchmark plumbing. They are not
    a WER measurement of Moonshine; attach private or TTS WAVs for that.
    """
    kind = spec.get("kind") or "silence"
    if kind == "silence":
        return _silence_ms(int(spec.get("duration_ms") or 500))
    if kind == "noise":
        return _noise_ms(int(spec.get("duration_ms") or 1000), float(spec.get("dbfs") or -45))
    if kind == "speech_then_silence":
        speech_ms = int(spec.get("speech_ms") or 800)
        silence_ms = int(spec.get("silence_ms") or 600)
        lead_ms = int(spec.get("lead_ms") or 0)
        return (
            _silence_ms(lead_ms)
            + _tone_ms(speech_ms, amplitude=int(spec.get("amplitude") or 8000))
            + _silence_ms(silence_ms)
        )
    if kind == "clipped_start":
        # Speech from sample 0, then silence. The recording itself has no onset.
        return _tone_ms(int(spec.get("speech_ms") or 800)) + _silence_ms(
            int(spec.get("silence_ms") or 600)
        )
    if kind == "clipped_end":
        # Speech with no trailing silence window, so a real endpointer would wait.
        return _tone_ms(int(spec.get("speech_ms") or 800))
    if kind == "max_utterance":
        return _tone_ms(int(spec.get("speech_ms") or 16000))
    raise ValueError(f"unknown synthetic kind {kind!r}")


def _silence_ms(duration_ms: int) -> bytes:
    frames = max(0, math.ceil(duration_ms / 32))
    return silence_frame() * frames


def _tone_ms(duration_ms: int, amplitude: int = 8000) -> bytes:
    """Constant-amplitude frames. Above the -38 dBFS threshold, like Go tests."""
    frames = max(0, math.ceil(duration_ms / 32))
    return speech_frame(amplitude) * frames


def _noise_ms(duration_ms: int, dbfs: float) -> bytes:
    """Deterministic low-level noise below the speech threshold."""
    amplitude = dbfs_amplitude(dbfs)
    samples = max(0, int(RATE * duration_ms / 1000))
    # A cheap repeating pattern so the fixture is bit-identical across runs.
    values = [int(amplitude if (i // 8) % 2 == 0 else -amplitude) for i in range(samples)]
    pcm = struct.pack("<%dh" % len(values), *values)
    # Pad to a whole frame so replay does not drop a short tail.
    leftover = len(pcm) % FRAME_BYTES
    if leftover:
        pcm += b"\x00" * (FRAME_BYTES - leftover)
    return pcm
