import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prefetch  # noqa: E402

CONFIG = {
    "weights_path": "hf://kyutai/pocket-tts/languages/english/model.safetensors@abc",
    "weights_path_without_voice_cloning": "hf://public/model.safetensors@abc",
    "flow_lm": {"lookup_table": {"tokenizer_path": "hf://public/tokenizer.json@abc"}},
}


def voice_source(name):
    return f"hf://public/embeddings/{name}.safetensors@abc"


def test_files_for_bakes_the_public_bundle_tokenizer_and_voices():
    files = prefetch.files_for(CONFIG, ["alba", "fantine"], voice_source)
    assert files == [
        "hf://public/model.safetensors@abc",
        "hf://public/tokenizer.json@abc",
        "hf://public/embeddings/alba.safetensors@abc",
        "hf://public/embeddings/fantine.safetensors@abc",
    ]


def test_files_for_never_bakes_the_gated_bundle():
    files = prefetch.files_for(CONFIG, [], voice_source)
    assert files == ["hf://public/model.safetensors@abc", "hf://public/tokenizer.json@abc"]


def test_files_for_tolerates_a_config_without_optional_paths():
    assert prefetch.files_for({}, [], voice_source) == []


def test_voice_names_merges_the_list_and_the_default(monkeypatch):
    monkeypatch.setenv("TTS_VOICES", "alba, fantine")
    monkeypatch.setenv("TTS_VOICE", "fantine")
    assert prefetch.voice_names() == ["alba", "fantine"]

    monkeypatch.delenv("TTS_VOICES")
    monkeypatch.delenv("TTS_VOICE")
    assert prefetch.voice_names() == []
