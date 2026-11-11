"""Convert model audio samples to Wyoming's signed 16-bit little-endian PCM.

Samples are floats in [-1, 1]; bytes pass through so tests and pre-formatted
audio cost nothing.
"""

from __future__ import annotations

import struct
from typing import Any, Iterable


def to_pcm16(samples: Any) -> bytes:
    if isinstance(samples, (bytes, bytearray)):
        return bytes(samples)

    # Torch tensors expose detach(); numpy arrays reshape directly.
    if hasattr(samples, "detach"):
        samples = samples.detach().cpu().numpy()
    if hasattr(samples, "reshape") and hasattr(samples, "astype"):
        flat = samples.reshape(-1)
        clipped = flat.clip(-1.0, 1.0)
        return (clipped * 32767.0).astype("<i2").tobytes()

    values = [max(-1.0, min(1.0, float(value))) for value in samples]
    return struct.pack("<%dh" % len(values), *(int(value * 32767.0) for value in values))


def as_int16_frames(samples: Iterable[float]) -> bytes:
    return to_pcm16(list(samples))
