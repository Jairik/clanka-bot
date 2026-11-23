"""Python replica of the Go voice endpointer for offline STT replay.

The live loop in assistant/internal/voice/endpoint.go owns production
endpointing. This copy exists so a fixture can be cut with the same 500 ms
silence window, -38 dBFS threshold, 200 ms min-speech and 15 s cap without
running Go. Numbers are taken from assistant/config.yaml stt.endpoint.
"""

from __future__ import annotations

import math
import struct
from dataclasses import dataclass, field

# Production PCM layout: 16 kHz mono 16-bit little-endian, 32 ms / 1024-byte frames.
RATE = 16000
FRAME_BYTES = 1024
FRAME_MS = 32
MAX_INT16 = 32767.0

# Fixed end-of-speech rule from config.yaml / #12. Not adaptive.
DEFAULT_SILENCE_MS = 500
DEFAULT_THRESHOLD_DBFS = -38.0
DEFAULT_MIN_SPEECH_MS = 200
DEFAULT_MAX_UTTERANCE_MS = 15000
# Pre-roll bound from PrerollFrameCount: 500 ms at the production frame size.
PREROLL_MS = 500


@dataclass
class EndpointConfig:
    """One end-of-speech rule. Fields match the Go EndpointConfig names."""

    rate: int = RATE
    silence_ms: int = DEFAULT_SILENCE_MS
    threshold_dbfs: float = DEFAULT_THRESHOLD_DBFS
    min_speech_ms: int = DEFAULT_MIN_SPEECH_MS
    max_utterance_ms: int = DEFAULT_MAX_UTTERANCE_MS

    def threshold_linear(self) -> float:
        """RMS fraction of full scale that counts as speech."""
        return math.pow(10.0, self.threshold_dbfs / 20.0)


@dataclass
class EndpointResult:
    """What the endpointer decided about one clip, including clip tags."""

    ended: bool
    heard: bool
    # True when the cap fired rather than the silence window.
    hit_max: bool
    # Audio that would have been sent to STT after pre-roll.
    pcm: bytes
    sent_start_ms: int
    sent_end_ms: int
    last_speech_ms: int
    start_clipped: bool
    end_clipped: bool


@dataclass
class Endpointer:
    """Turns a stream of PCM frames into one end-of-speech decision."""

    cfg: EndpointConfig = field(default_factory=EndpointConfig)
    threshold: float = field(init=False)
    speech_ms: float = 0.0
    heard: bool = False
    silent_ms: float = 0.0
    total_ms: float = 0.0
    last_speech_ms: int = 0
    hit_max: bool = False

    def __post_init__(self) -> None:
        if self.cfg.rate <= 0:
            raise ValueError("endpoint rate must be positive")
        if self.cfg.silence_ms <= 0:
            raise ValueError("endpoint silence must be positive")
        if (
            self.cfg.max_utterance_ms > 0
            and self.cfg.max_utterance_ms < self.cfg.silence_ms
        ):
            raise ValueError("endpoint max_utterance must be at least the silence window")
        self.threshold = self.cfg.threshold_linear()

    def add(self, pcm: bytes) -> bool:
        """Consume one mono PCM16 frame. True when the utterance has ended."""
        if len(pcm) < 2:
            return False
        duration_ms = (len(pcm) // 2) / float(self.cfg.rate) * 1000.0
        self.total_ms += duration_ms

        if rms_level(pcm) >= self.threshold:
            self.speech_ms += duration_ms
            self.last_speech_ms = int(self.total_ms)
            if not self.heard and self.speech_ms >= self.cfg.min_speech_ms:
                self.heard = True
            if self.heard:
                self.silent_ms = 0.0
        elif self.heard:
            self.silent_ms += duration_ms

        if self.cfg.max_utterance_ms > 0 and self.total_ms >= self.cfg.max_utterance_ms:
            self.hit_max = True
            return True
        return self.heard and self.silent_ms >= self.cfg.silence_ms


def rms_level(pcm: bytes) -> float:
    """Frame RMS as a fraction of full scale, matching Go rmsLevel."""
    samples = len(pcm) // 2
    if samples == 0:
        return 0.0
    values = struct.unpack("<%dh" % samples, pcm[: samples * 2])
    total = 0.0
    for value in values:
        scaled = value / MAX_INT16
        total += scaled * scaled
    return math.sqrt(total / samples)


def preroll_frame_count(frame_bytes: int = FRAME_BYTES, rate: int = RATE) -> int:
    """Fixed 500 ms pre-roll in frames, matching Go PrerollFrameCount."""
    if rate <= 0 or frame_bytes < 2:
        return 0
    frame_s = (frame_bytes / 2) / float(rate)
    n = int(math.ceil(0.5 / frame_s))
    return n if n >= 1 else 1


def frames_of(pcm: bytes, frame_bytes: int = FRAME_BYTES) -> list[bytes]:
    """Split a clip into production-sized frames, dropping a short tail."""
    return [pcm[i : i + frame_bytes] for i in range(0, len(pcm) - frame_bytes + 1, frame_bytes)]


def replay(
    pcm: bytes,
    cfg: EndpointConfig | None = None,
    frame_bytes: int = FRAME_BYTES,
    speech_start_ms: int | None = None,
    speech_end_ms: int | None = None,
) -> EndpointResult:
    """Cut one clip the way the voice loop would, and tag clipped edges.

    speech_start_ms / speech_end_ms are the fixture's annotated voiced region
    in the original file. They are how we tell a clipped recording from a
    recognizer error: if the sent window dropped the onset or the tail, that
    is an endpointing problem, not a model problem.
    """
    cfg = cfg or EndpointConfig()
    endpointer = Endpointer(cfg)
    preroll_n = preroll_frame_count(frame_bytes, cfg.rate)
    preroll: list[bytes] = []
    sent: list[bytes] = []
    heard = False
    sent_start_ms = 0
    offset_ms = 0.0
    frame_ms = (frame_bytes / 2) / float(cfg.rate) * 1000.0
    ended = False

    for frame in frames_of(pcm, frame_bytes):
        ended = endpointer.add(frame)
        if not heard:
            preroll.append(frame)
            if len(preroll) > preroll_n:
                preroll.pop(0)
            if endpointer.heard:
                # First confirmed speech: dump the bounded pre-roll, which
                # includes the onset that min_speech had been accumulating.
                sent.extend(preroll)
                sent_start_ms = max(0, int(offset_ms + frame_ms - len(preroll) * frame_ms))
                heard = True
                preroll.clear()
        else:
            sent.append(frame)
        offset_ms += frame_ms
        if ended:
            break

    sent_pcm = b"".join(sent)
    sent_end_ms = sent_start_ms + int(len(sent_pcm) / 2 / cfg.rate * 1000)

    # Onset is clipped when the annotated speech started before the sent window.
    start_clipped = (
        speech_start_ms is not None and heard and speech_start_ms + 50 < sent_start_ms
    )
    # Tail is clipped when the silence window never fired (truncated recording
    # or the 15 s cap) or the sent window ended before the annotated speech.
    end_clipped = bool(endpointer.hit_max)
    if heard and not ended:
        end_clipped = True
    if speech_end_ms is not None and heard and sent_end_ms + 50 < speech_end_ms:
        end_clipped = True

    return EndpointResult(
        ended=ended,
        heard=heard,
        hit_max=endpointer.hit_max,
        pcm=sent_pcm,
        sent_start_ms=sent_start_ms,
        sent_end_ms=sent_end_ms,
        last_speech_ms=endpointer.last_speech_ms,
        start_clipped=start_clipped,
        end_clipped=end_clipped,
    )


def speech_frame(amplitude: int = 8000, frame_bytes: int = FRAME_BYTES) -> bytes:
    """One 32 ms frame at a constant amplitude, matching the Go test helper."""
    samples = frame_bytes // 2
    return struct.pack("<%dh" % samples, *([amplitude] * samples))


def silence_frame(frame_bytes: int = FRAME_BYTES) -> bytes:
    """One 32 ms frame of zeros."""
    return b"\x00" * frame_bytes


def dbfs_amplitude(dbfs: float) -> int:
    """Peak int16 for a given dBFS, used to build noise and speech fixtures."""
    return max(0, min(MAX_INT16, int(round(math.pow(10.0, dbfs / 20.0) * MAX_INT16))))
