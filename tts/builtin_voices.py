"""The built-in Pocket TTS voices this image can load offline.

`kyutai/pocket-tts-without-voice-cloning` publishes one precomputed embedding
per voice under `languages/<language>/embeddings/<name>.safetensors`, and the
set is identical for every language. That set is the catalogue: a name Pocket
TTS ships no embedding for cannot be warmed with `HF_HUB_OFFLINE=1`, so listing
it would only produce an entry that fails on selection.

The catalogue is what the TTS service advertises as selectable. It is not the
warm set: `TTS_VOICES` names the voices loaded at startup, and a catalogue
voice still cold is loaded on first use and cached for the process lifetime.

`TTS_CATALOGUE` accepts `all` (the default) or a comma-separated subset.
"""

# Sorted for a stable Dashboard order; alba leads because it is the default.
BUILTIN_VOICES: tuple[str, ...] = (
    "alba",
    "anna",
    "azelma",
    "bill_boerst",
    "caro_davy",
    "charles",
    "cosette",
    "daan",
    "eponine",
    "estelle",
    "eve",
    "fantine",
    "george",
    "giovanni",
    "jane",
    "javert",
    "jean",
    "juergen",
    "lola",
    "marius",
    "mary",
    "michael",
    "paul",
    "peter_yearsley",
    "rafael",
    "stuart_bell",
    "vera",
)

# ALL is the TTS_CATALOGUE value meaning every built-in voice.
ALL = "all"


def catalogue_names(raw: str | None) -> list[str]:
    """Resolve a TTS_CATALOGUE value to names.

    An unset, empty or `all` value means the whole catalogue; anything else is a
    comma-separated subset, de-duplicated in the order given.
    """
    value = (raw or "").strip()
    if not value or value.lower() == ALL:
        return list(BUILTIN_VOICES)
    names = [name.strip() for name in value.split(",") if name.strip()]
    return list(dict.fromkeys(names))
