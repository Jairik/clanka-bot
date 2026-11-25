"""Parakeet ONNX model wrapper: load once at startup, buffer audio, decode once.

The heavy imports (onnx_asr, onnxruntime) happen lazily so the wire layer and
its unit tests run without the ONNX runtime installed. Model directories come
from the build-time manifest, so nothing here touches the network.
"""

from __future__ import annotations

import json
import logging
import os
import threading
from dataclasses import dataclass
from importlib.metadata import PackageNotFoundError, version as package_version
from pathlib import Path

import numpy as np

_LOGGER = logging.getLogger("stt.engine")

MODEL_ROOT = Path(os.environ.get("STT_MODEL_ROOT", "/opt/stt"))
MANIFEST = MODEL_ROOT / "models.json"

DEFAULT_MODEL = "parakeet-tdt-0.6b-v2"
# Pi 5 benchmarks favor 3 threads: leaves 1 core for wakeword/assistant and
# beats 4 threads on burst RTF (0.133x vs 0.138x).
DEFAULT_THREADS = 3
# Guard against a buggy client buffering unbounded PCM before audio-stop.
MAX_AUDIO_SECONDS = 60


@dataclass(frozen=True)
class ModelSpec:
    """One supported onnx-asr Parakeet model and its required offline files."""

    name: str
    onnx_asr_name: str
    repo_id: str
    quantization: str
    required_files: tuple[str, ...]
    allow_patterns: tuple[str, ...]


MODEL_SPECS: dict[str, ModelSpec] = {
    "parakeet-tdt-0.6b-v2": ModelSpec(
        name="parakeet-tdt-0.6b-v2",
        onnx_asr_name="nemo-parakeet-tdt-0.6b-v2",
        repo_id="istupakov/parakeet-tdt-0.6b-v2-onnx",
        quantization="int8",
        required_files=(
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
        allow_patterns=(
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    ),
    "parakeet-tdt-0.6b-v3": ModelSpec(
        name="parakeet-tdt-0.6b-v3",
        onnx_asr_name="nemo-parakeet-tdt-0.6b-v3",
        repo_id="istupakov/parakeet-tdt-0.6b-v3-onnx",
        quantization="int8",
        required_files=(
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
        allow_patterns=(
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    ),
    "parakeet-ctc-0.6b": ModelSpec(
        name="parakeet-ctc-0.6b",
        onnx_asr_name="nemo-parakeet-ctc-0.6b",
        repo_id="istupakov/parakeet-ctc-0.6b-onnx",
        quantization="int8",
        required_files=(
            "config.json",
            "model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
        allow_patterns=(
            "config.json",
            "model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    ),
}

MODEL_ALIASES: dict[str, str] = {
    spec.onnx_asr_name: name for name, spec in MODEL_SPECS.items()
}


def resolve_model_name(name: str) -> str:
    """Normalise an onnx-asr full name or short alias to its canonical key."""
    cleaned = (name or "").strip()
    return MODEL_ALIASES.get(cleaned, cleaned)


def load_manifest(path: Path = MANIFEST) -> dict:
    """Read the build-time manifest written by prefetch.py."""
    if not path.is_file():
        raise RuntimeError(f"missing model manifest {path}; rebuild the stt image")
    data = json.loads(path.read_text())
    # Keep compatibility with a flat {name: path} manifest used in ad-hoc tests.
    if "models" in data and isinstance(data["models"], dict):
        return data
    return {"models": data, "vad": {}}


def _resample_linear(samples: np.ndarray, src_rate: int, dst_rate: int) -> np.ndarray:
    """Resample 1D float32 audio when a caller sends a non-16 kHz chunk."""
    if src_rate == dst_rate or samples.size == 0:
        return samples
    out_len = max(1, int(round(samples.size * dst_rate / src_rate)))
    xp = np.linspace(0.0, samples.size - 1, num=samples.size, dtype=np.float64)
    x = np.linspace(0.0, samples.size - 1, num=out_len, dtype=np.float64)
    return np.interp(x, xp, samples).astype(np.float32)


class ParakeetStream:
    """One buffered transcription. Audio accumulates cheaply; finish() decodes once."""

    def __init__(
        self,
        recognizer,
        lock: threading.Lock,
        sample_rate: int = 16000,
        max_seconds: int = MAX_AUDIO_SECONDS,
    ) -> None:
        self._recognizer = recognizer
        self._lock = lock
        self._sample_rate = sample_rate
        self._max_samples = max(1, int(max_seconds * sample_rate))
        self._chunks: list[np.ndarray] = []
        self._total_samples = 0

    @property
    def partial(self) -> str:
        # Offline transducer: no interim partials; Go only consumes the final text.
        return ""

    def add_audio(self, samples, rate: int) -> None:
        if not samples:
            return
        if rate <= 0:
            raise ValueError(f"invalid sample rate {rate}")
        arr = np.asarray(samples, dtype=np.float32)
        if arr.ndim != 1:
            arr = arr.reshape(-1)
        if rate != self._sample_rate:
            arr = _resample_linear(arr, rate, self._sample_rate)
        remaining = self._max_samples - self._total_samples
        if remaining <= 0:
            return
        if arr.size > remaining:
            arr = arr[:remaining]
        self._chunks.append(arr)
        self._total_samples += arr.size

    def finish(self) -> str:
        if not self._chunks:
            return ""
        audio = (
            self._chunks[0]
            if len(self._chunks) == 1
            else np.concatenate(self._chunks, axis=0)
        )
        audio = np.ascontiguousarray(audio, dtype=np.float32)
        self._chunks.clear()
        self._total_samples = 0
        with self._lock:
            result = self._recognizer.recognize(audio, sample_rate=self._sample_rate)
        return _extract_text(result)

    def close(self) -> None:
        self._chunks.clear()
        self._total_samples = 0


class ParakeetEngine:
    """Owns the baked Parakeet ONNX models and hands out one stream per request."""

    supports_streaming = False

    def __init__(
        self,
        model: str = DEFAULT_MODEL,
        language: str = "en",
        manifest: Path = MANIFEST,
        threads: int | None = None,
    ) -> None:
        self.model = resolve_model_name(model) or DEFAULT_MODEL
        self.language = language
        self.manifest_path = manifest
        env_threads = int(os.environ.get("STT_THREADS", str(DEFAULT_THREADS)))
        self.threads = max(1, env_threads if threads is None else int(threads))
        self.sample_rate = 16000
        self.version = _package_version()
        self._stream_lock = threading.Lock()
        self._load_lock = threading.Lock()
        self._recognizers: dict[str, object] = {}

    def available_models(self) -> list[str]:
        manifest = load_manifest(self.manifest_path)
        return sorted(manifest["models"])

    def load(self) -> None:
        """Load and warm up the configured model. Blocks until ready."""
        recognizer = self._load(self.model)
        # Warm ONNX Runtime arenas before healthcheck reports ready.
        warmup = np.zeros(self.sample_rate // 2, dtype=np.float32)
        with self._stream_lock:
            recognizer.recognize(warmup, sample_rate=self.sample_rate)

    def _load(self, name: str):
        canonical = resolve_model_name(name)
        with self._load_lock:
            if canonical in self._recognizers:
                return self._recognizers[canonical]

            import onnx_asr
            import onnxruntime as ort

            manifest = load_manifest(self.manifest_path)
            entry = manifest["models"].get(canonical)
            if entry is None:
                raise RuntimeError(
                    f"model {name!r} is not baked into the image "
                    f"(available: {', '.join(sorted(manifest['models']))})"
                )

            if isinstance(entry, str):
                spec = MODEL_SPECS.get(canonical)
                local_path = entry
                onnx_asr_name = spec.onnx_asr_name if spec else canonical
                quantization = spec.quantization if spec else "int8"
            else:
                local_path = entry["path"]
                onnx_asr_name = entry["onnx_asr_name"]
                quantization = entry.get("quantization") or "int8"

            sess_opts = ort.SessionOptions()
            sess_opts.intra_op_num_threads = self.threads
            sess_opts.inter_op_num_threads = 1

            recognizer = onnx_asr.load_model(
                onnx_asr_name,
                path=local_path,
                quantization=quantization,
                sess_options=sess_opts,
                providers=["CPUExecutionProvider"],
            )
            self._recognizers[canonical] = recognizer
            _LOGGER.info(
                "loaded %s (%s, %s, threads=%d) from %s",
                canonical,
                onnx_asr_name,
                quantization,
                self.threads,
                local_path,
            )
            return recognizer

    def create_stream(self, model: str | None = None) -> ParakeetStream:
        """Create a buffered stream for model (or the configured default)."""
        name = resolve_model_name(model) if model else self.model
        recognizer = self._load(name)
        return ParakeetStream(
            recognizer,
            self._stream_lock,
            sample_rate=self.sample_rate,
        )


def _extract_text(result) -> str:
    """Normalise an onnx-asr recognize() result to a stripped string."""
    if result is None:
        return ""
    if isinstance(result, str):
        return result.strip()
    if isinstance(result, list):
        parts = [_extract_text(item) for item in result]
        return " ".join(p for p in parts if p).strip()
    text = getattr(result, "text", None)
    if isinstance(text, str):
        return text.strip()
    return str(result).strip()


def _package_version() -> str:
    try:
        return package_version("onnx-asr")
    except PackageNotFoundError:
        return "unknown"
