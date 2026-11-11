"""Bake the Moonshine STT weights into the image at build time.

The models come from the public `download.moonshine.ai` CDN with an anonymous
request, so the build needs no token and the container starts with no network.
The manifest written here is what the runtime engine reads; it never calls the
download path.
"""

from __future__ import annotations

import json
import os
import sys
from pathlib import Path
from typing import Sequence

MODEL_ROOT = Path(os.environ.get("MOONSHINE_MODEL_ROOT", "/opt/moonshine"))
MANIFEST = MODEL_ROOT / "models.json"

# The image carries the default and the low-CPU fallback (#2).
DEFAULT_MODELS = "small-streaming,tiny-streaming"

# Files a streaming model must have on disk to be usable offline.
REQUIRED_FILES = ("encoder.ort", "decoder_kv.ort", "tokenizer.bin")


def wanted_models() -> list[str]:
    raw = os.environ.get("STT_MODELS") or DEFAULT_MODELS
    return [name.strip() for name in raw.split(",") if name.strip()]


def check(models: Sequence[str]) -> int:
    if not MANIFEST.is_file():
        print(f"missing {MANIFEST}; run without --check first", file=sys.stderr)
        return 1
    manifest = json.loads(MANIFEST.read_text())
    for name in models:
        path = manifest.get(name)
        if not path:
            print(f"model {name!r} is not in {MANIFEST}", file=sys.stderr)
            return 1
        for filename in REQUIRED_FILES:
            candidate = Path(path) / filename
            if not candidate.is_file() or candidate.stat().st_size == 0:
                print(f"missing or empty: {candidate}", file=sys.stderr)
                return 1
        print(f"offline {name}: {path}")
    return 0


def prefetch(models: Sequence[str]) -> int:
    from moonshine_voice import get_model_for_language
    from moonshine_voice.moonshine_api import string_to_model_arch

    MODEL_ROOT.mkdir(parents=True, exist_ok=True)
    manifest = {}
    for name in models:
        path, arch = get_model_for_language("en", string_to_model_arch(name), cache_root=MODEL_ROOT)
        manifest[name] = str(path)
        print(f"cached {name} ({arch.name}) at {path}")
    MANIFEST.write_text(json.dumps(manifest, indent=2, sort_keys=True))
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
