"""Build-time prefetch of the Pocket TTS weights, tokenizer and voices.

The public `kyutai/pocket-tts-without-voice-cloning` bundle and
`kyutai/tts-voices` embeddings need no token, so the image can carry them and
start with no network. Runtime resolution keeps the library's own fallback:
the licence-gated bundle is tried first and fails, then this cached bundle loads.
"""

from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Callable, Mapping, Sequence

from builtin_voices import catalogue_names

# Voice names are warmable only when Pocket TTS ships a precomputed embedding.
# A URL or local file voice is fetched by the user's own path, not baked here.


def voice_names() -> list[str]:
    """Voices named by TTS_VOICES plus the single TTS_VOICE, de-duplicated.

    The catalogue is included because a cold catalogue voice is still selectable
    at runtime, and the image runs with HF_HUB_OFFLINE=1: an embedding that is
    not baked in here cannot be loaded later.
    """
    raw = os.environ.get("TTS_VOICES", "") + "," + os.environ.get("TTS_VOICE", "")
    raw += "," + ",".join(catalogue_names(os.environ.get("TTS_CATALOGUE")))
    names = [name.strip() for name in raw.split(",") if name.strip()]
    return list(dict.fromkeys(names))


def files_for(
    config: Mapping, voices: Sequence[str], voice_source: Callable[[str], str]
) -> list[str]:
    """hf:// files one language config needs: bundle, tokenizer, voice states."""
    lookup = (config.get("flow_lm") or {}).get("lookup_table") or {}
    files = [
        config.get("weights_path_without_voice_cloning"),
        lookup.get("tokenizer_path"),
    ]
    files = [path for path in files if path]
    files.extend(voice_source(name) for name in voices)
    return files


def main(argv: Sequence[str]) -> int:
    """Fetch every file, or with --check prove they all resolve offline."""
    import yaml
    from pocket_tts.utils.config import CONFIGS_DIR
    from pocket_tts.utils.utils import (
        _ORIGINS_OF_PREDEFINED_VOICES,
        download_if_necessary,
        get_predefined_voice,
    )

    check_only = "--check" in argv
    language = (os.environ.get("TTS_LANGUAGE") or "english").strip()
    config_path = CONFIGS_DIR / f"{language}.yaml"
    if not config_path.exists():
        print(f"unknown TTS_LANGUAGE {language!r}: {config_path} is missing", file=sys.stderr)
        return 2

    config = yaml.safe_load(config_path.read_text())
    voices = voice_names()
    predefined = [name for name in voices if name in _ORIGINS_OF_PREDEFINED_VOICES]
    for name in voices:
        if name not in predefined:
            print(f"skip voice {name!r}: not a predefined voice", file=sys.stderr)

    files = files_for(config, predefined, lambda name: get_predefined_voice(language, name))
    if not config.get("weights_path_without_voice_cloning"):
        print(f"config for {language!r} has no public weight bundle", file=sys.stderr)
        return 2

    for path in files:
        resolved = Path(download_if_necessary(path))
        if not resolved.is_file() or resolved.stat().st_size == 0:
            print(f"missing or empty after prefetch: {path}", file=sys.stderr)
            return 1
        verb = "offline" if check_only else "cached"
        print(f"{verb} {resolved.stat().st_size:>12}  {path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
