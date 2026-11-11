"""Convert Wyoming PCM16 payloads to the float samples Moonshine expects."""

from __future__ import annotations

import numpy as np


def to_floats(pcm: bytes) -> list[float]:
    """Signed 16-bit little-endian PCM to floats in [-1, 1]."""
    if not pcm:
        return []
    samples = np.frombuffer(pcm, dtype="<i2").astype(np.float32)
    return (samples / 32768.0).tolist()
