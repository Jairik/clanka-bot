"""Bake the Parakeet STT and Silero VAD ONNX weights into the image at build time.

Self-contained by design: Dockerfile runs this before copying application code
so code edits never bust the ~660 MB weights layer.

Models download anonymously from Hugging Face Hub directly into local
directories (with HF cache metadata removed afterwards so the image layer
contains only the exact ONNX/vocab files). The manifest written here is what
the runtime engines read; runtime starts with HF_HUB_OFFLINE=1.
"""

from __future__ import annotations

import json
import os
import shutil
import sys
from pathlib import Path
from typing import Sequence

MODEL_ROOT = Path(os.environ.get("STT_MODEL_ROOT", "/opt/stt"))
MANIFEST = MODEL_ROOT / "models.json"
DEFAULT_MODELS = "parakeet-tdt-0.6b-v2"

SILERO_REPO_ID = "istupakov/silero-vad-onnx"
SILERO_FILENAME = "silero_vad.onnx"
SILERO_ALLOW_PATTERNS = ("config.json", SILERO_FILENAME)

MODEL_SPECS: dict[str, dict] = {
    "parakeet-tdt-0.6b-v2": {
        "onnx_asr_name": "nemo-parakeet-tdt-0.6b-v2",
        "repo_id": "istupakov/parakeet-tdt-0.6b-v2-onnx",
        "quantization": "int8",
        "files": (
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    },
    "parakeet-tdt-0.6b-v3": {
        "onnx_asr_name": "nemo-parakeet-tdt-0.6b-v3",
        "repo_id": "istupakov/parakeet-tdt-0.6b-v3-onnx",
        "quantization": "int8",
        "files": (
            "config.json",
            "encoder-model.int8.onnx",
            "decoder_joint-model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    },
    "parakeet-ctc-0.6b": {
        "onnx_asr_name": "nemo-parakeet-ctc-0.6b",
        "repo_id": "istupakov/parakeet-ctc-0.6b-onnx",
        "quantization": "int8",
        "files": (
            "config.json",
            "model.int8.onnx",
            "nemo128.onnx",
            "vocab.txt",
        ),
    },
}

MODEL_ALIASES: dict[str, str] = {
    spec["onnx_asr_name"]: name for name, spec in MODEL_SPECS.items()
}


def resolve_name(raw: str) -> str:
    cleaned = (raw or "").strip()
    return MODEL_ALIASES.get(cleaned, cleaned)


def wanted_models() -> list[str]:
    raw = os.environ.get("STT_MODELS") or DEFAULT_MODELS
    models: list[str] = []
    for part in raw.split(","):
        cleaned = resolve_name(part)
        if cleaned and cleaned not in models:
            models.append(cleaned)
    return models


def _clean_hf_cache(directory: Path) -> None:
    """Drop Hugging Face local_dir metadata so layers stay small."""
    cache_dir = directory / ".cache"
    if cache_dir.exists():
        shutil.rmtree(cache_dir, ignore_errors=True)


def check(models: Sequence[str]) -> int:
    if not MANIFEST.is_file():
        print(f"missing {MANIFEST}; run without --check first", file=sys.stderr)
        return 1
    manifest = json.loads(MANIFEST.read_text())
    model_entries = manifest.get("models") or {}
    for name in models:
        spec = MODEL_SPECS.get(name)
        if spec is None:
            print(f"unsupported model {name!r}", file=sys.stderr)
            return 1
        entry = model_entries.get(name)
        if not entry or not isinstance(entry, dict) or not entry.get("path"):
            print(f"model {name!r} is not in {MANIFEST}", file=sys.stderr)
            return 1
        model_dir = Path(entry["path"])
        for filename in spec["files"]:
            candidate = model_dir / filename
            if not candidate.is_file() or candidate.stat().st_size == 0:
                print(f"missing or empty: {candidate}", file=sys.stderr)
                return 1
        print(f"offline {name}: {model_dir}")

    vad_path_raw = (manifest.get("vad") or {}).get("silero")
    if not vad_path_raw:
        print(f"missing vad.silero in {MANIFEST}", file=sys.stderr)
        return 1
    vad_path = Path(vad_path_raw)
    if not vad_path.is_file() or vad_path.stat().st_size == 0:
        print(f"missing or empty Silero VAD: {vad_path}", file=sys.stderr)
        return 1
    print(f"offline silero-vad: {vad_path}")

    # Prove offline loading works with HF_HUB_OFFLINE=1 (zero hub lookups).
    import numpy as np
    import onnx_asr
    import onnxruntime as ort

    opts = ort.SessionOptions()
    opts.intra_op_num_threads = 1
    opts.inter_op_num_threads = 1

    vad_sess = ort.InferenceSession(
        str(vad_path), sess_options=opts, providers=["CPUExecutionProvider"]
    )
    out, _ = vad_sess.run(
        ["output", "stateN"],
        {
            "input": np.zeros((1, 576), dtype=np.float32),
            "state": np.zeros((2, 1, 128), dtype=np.float32),
            "sr": np.array([16000], dtype=np.int64),
        },
    )
    assert out.size == 1

    primary = models[0]
    entry = model_entries[primary]
    recognizer = onnx_asr.load_model(
        entry["onnx_asr_name"],
        path=entry["path"],
        quantization=entry.get("quantization") or "int8",
        sess_options=opts,
        providers=["CPUExecutionProvider"],
    )
    recognizer.recognize(np.zeros(8000, dtype=np.float32), sample_rate=16000)
    print(f"offline load verified: {primary} + silero-vad")
    return 0


def prefetch(models: Sequence[str]) -> int:
    from huggingface_hub import snapshot_download

    models_dir = MODEL_ROOT / "models"
    models_dir.mkdir(parents=True, exist_ok=True)

    manifest_models: dict[str, dict[str, str]] = {}
    for name in models:
        spec = MODEL_SPECS.get(name)
        if spec is None:
            supported = ", ".join(sorted(MODEL_SPECS))
            print(
                f"unsupported STT model {name!r} (supported: {supported})",
                file=sys.stderr,
            )
            return 2
        dest = models_dir / name
        dest.mkdir(parents=True, exist_ok=True)
        snapshot_download(
            repo_id=spec["repo_id"],
            local_dir=str(dest),
            allow_patterns=list(spec["files"]),
        )
        _clean_hf_cache(dest)
        manifest_models[name] = {
            "onnx_asr_name": spec["onnx_asr_name"],
            "path": str(dest),
            "quantization": spec["quantization"],
        }
        print(f"cached {name} ({spec['repo_id']}) at {dest}")

    vad_dir = models_dir / "silero-vad"
    vad_dir.mkdir(parents=True, exist_ok=True)
    snapshot_download(
        repo_id=SILERO_REPO_ID,
        local_dir=str(vad_dir),
        allow_patterns=list(SILERO_ALLOW_PATTERNS),
    )
    _clean_hf_cache(vad_dir)
    vad_file = vad_dir / SILERO_FILENAME
    print(f"cached silero-vad ({SILERO_REPO_ID}) at {vad_file}")

    manifest = {
        "models": manifest_models,
        "vad": {"silero": str(vad_file)},
    }
    MANIFEST.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    return 0


def main(argv: Sequence[str]) -> int:
    models = wanted_models()
    if not models:
        print("STT_MODELS is empty", file=sys.stderr)
        return 2
    if "--check" in argv:
        return check(models)
    return prefetch(models)


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
