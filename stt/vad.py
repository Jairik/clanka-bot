"""Silero VAD v5 ONNX frame scorer for live voice endpointing.

Runs independently of Parakeet ASR: each utterance gets its own lightweight
RNN state ((2, 1, 128)) and 64-sample context tail, scoring 512-sample (32 ms)
16 kHz frames in <0.5 ms on a single Cortex-A76 core.

onnxruntime is imported lazily so wire-level unit tests run without it.
"""

from __future__ import annotations

import logging
import threading
from pathlib import Path

import numpy as np

from parakeet_engine import MANIFEST, load_manifest

_LOGGER = logging.getLogger("stt.vad")

SILERO_REPO_ID = "istupakov/silero-vad-onnx"
SILERO_FILENAME = "silero_vad.onnx"
SILERO_ALLOW_PATTERNS = ("config.json", SILERO_FILENAME)

# Silero VAD v5 fixed frame geometry at 16 kHz (matches Clanka's 32 ms frame).
SAMPLE_RATE_16K = 16000
HOP_SAMPLES_16K = 512
CONTEXT_SAMPLES_16K = 64


class SileroVadSession:
    """Per-utterance stateful Silero VAD v5 scorer."""

    def __init__(self, ort_session, lock: threading.Lock, sample_rate: int = SAMPLE_RATE_16K) -> None:
        if sample_rate != SAMPLE_RATE_16K:
            raise ValueError(f"Silero VAD expects {SAMPLE_RATE_16K} Hz audio, got {sample_rate}")
        self._session = ort_session
        self._lock = lock
        self._sample_rate = sample_rate
        self._sr_input = np.array([sample_rate], dtype=np.int64)
        self._state = np.zeros((2, 1, 128), dtype=np.float32)
        self._context = np.zeros((1, CONTEXT_SAMPLES_16K), dtype=np.float32)
        self._pending = np.empty(0, dtype=np.float32)
        self._last_prob = 0.0

    def reset(self) -> None:
        self._state.fill(0.0)
        self._context.fill(0.0)
        self._pending = np.empty(0, dtype=np.float32)
        self._last_prob = 0.0

    def score(self, samples, sample_rate: int = SAMPLE_RATE_16K) -> float:
        """Score one PCM float32 chunk and return its speech probability in [0, 1]."""
        if sample_rate != self._sample_rate:
            raise ValueError(f"expected {self._sample_rate} Hz chunk, got {sample_rate}")
        arr = np.asarray(samples, dtype=np.float32)
        if arr.ndim != 1:
            arr = arr.reshape(-1)
        if arr.size == 0:
            return self._last_prob

        if self._pending.size > 0:
            arr = np.concatenate((self._pending, arr))

        offset = 0
        n = arr.size
        max_prob: float | None = None
        while offset + HOP_SAMPLES_16K <= n:
            hop = arr[offset : offset + HOP_SAMPLES_16K].reshape(1, HOP_SAMPLES_16K)
            prob = self._run_hop(hop)
            max_prob = prob if max_prob is None else max(max_prob, prob)
            offset += HOP_SAMPLES_16K

        self._pending = arr[offset:].copy() if offset < n else np.empty(0, dtype=np.float32)
        if max_prob is not None:
            self._last_prob = max_prob
        return self._last_prob

    def _run_hop(self, hop: np.ndarray) -> float:
        frame = np.concatenate((self._context, hop), axis=1)
        self._context = frame[:, -CONTEXT_SAMPLES_16K:].copy()
        with self._lock:
            output, new_state = self._session.run(
                ["output", "stateN"],
                {
                    "input": frame,
                    "state": self._state,
                    "sr": self._sr_input,
                },
            )
        self._state = np.asarray(new_state, dtype=np.float32)
        prob = float(np.asarray(output, dtype=np.float32).reshape(-1)[0])
        if prob < 0.0:
            return 0.0
        if prob > 1.0:
            return 1.0
        return prob


class SileroVadEngine:
    """Loads the baked silero_vad.onnx once and spawns per-utterance sessions."""

    def __init__(self, model_path: Path | None = None, manifest: Path = MANIFEST) -> None:
        self._explicit_path = model_path
        self.manifest_path = manifest
        self._session = None
        self._lock = threading.Lock()
        self._load_lock = threading.Lock()

    def _resolve_path(self) -> Path:
        if self._explicit_path is not None:
            return self._explicit_path
        manifest = load_manifest(self.manifest_path)
        raw = (manifest.get("vad") or {}).get("silero")
        if not raw:
            raise RuntimeError(
                f"silero VAD model missing from {self.manifest_path}; rebuild the stt image"
            )
        return Path(raw)

    def load(self) -> None:
        """Load the ONNX session and run one warm-up hop."""
        with self._load_lock:
            if self._session is not None:
                return
            import onnxruntime as ort

            path = self._resolve_path()
            if not path.is_file():
                raise RuntimeError(f"missing Silero VAD weights at {path}")
            opts = ort.SessionOptions()
            opts.intra_op_num_threads = 1
            opts.inter_op_num_threads = 1
            self._session = ort.InferenceSession(
                str(path),
                sess_options=opts,
                providers=["CPUExecutionProvider"],
            )
            _LOGGER.info("loaded Silero VAD from %s", path)

        # Warm up once on silence so the first live turn pays no allocation jitter.
        self.create_session(SAMPLE_RATE_16K).score(
            np.zeros(HOP_SAMPLES_16K, dtype=np.float32), SAMPLE_RATE_16K
        )

    def create_session(self, sample_rate: int = SAMPLE_RATE_16K) -> SileroVadSession:
        if self._session is None:
            self.load()
        return SileroVadSession(self._session, self._lock, sample_rate=sample_rate)
