import sys
from pathlib import Path

import numpy as np
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from engine import MAX_STATE_BYTES, PocketTtsEngine  # noqa: E402
from builtin_voices import BUILTIN_VOICES  # noqa: E402


class FakeModel:
    """A minimal model: no torch, one deterministic chunk per generation."""

    sample_rate = 24000

    def __init__(self, samples=100):
        self.samples = samples
        self.state_calls = []

    def get_state_for_audio_prompt(self, name):
        self.state_calls.append(name)
        return name

    def generate_audio_stream(self, model_state, text_to_generate, stop=None):
        # A short sine keeps the resampler interpolation meaningful.
        yield np.sin(np.linspace(0, 2 * np.pi, self.samples)).astype("float32")


def engine_with_model(tmp_path, samples=100):
    engine = PocketTtsEngine(voice_dir=str(tmp_path))
    engine._model = FakeModel(samples)
    engine.revision = "test-revision"
    return engine


def test_validate_voice_loads_and_generates_without_torch(tmp_path):
    state = tmp_path / "jarvis.safetensors"
    state.write_bytes(b"not parsed by the fake model")
    engine = engine_with_model(tmp_path)
    assert engine.validate_voice(str(state)) == "test-revision"
    assert engine._model.state_calls == [str(state)]


def test_validate_voice_can_probe_without_caching(tmp_path):
    state = tmp_path / ".upload-123.safetensors"
    state.write_bytes(b"not parsed by the fake model")
    engine = engine_with_model(tmp_path)
    assert engine.validate_voice(str(state), cache=False) == "test-revision"
    # The staging path is not left in the process cache.
    assert engine._states == {}
    # A published file still caches when the ordinary rescan path validates it.
    published = tmp_path / "jarvis.safetensors"
    published.write_bytes(b"not parsed by the fake model")
    assert engine.validate_voice(str(published)) == "test-revision"
    assert str(published) in engine._states


def test_forget_voice_clears_a_cached_state(tmp_path):
    state = tmp_path / "jarvis.safetensors"
    state.write_bytes(b"not parsed by the fake model")
    engine = engine_with_model(tmp_path)
    engine.warm_voice(str(state))
    assert str(state) in engine._states
    engine.forget_voice(str(state))
    assert str(state) not in engine._states
    engine.warm_voice(str(state))
    assert len(engine._model.state_calls) == 2


def test_validate_voice_rejects_paths_outside_the_voice_dir(tmp_path):
    outside = tmp_path.parent / "outside.safetensors"
    outside.write_bytes(b"x")
    engine = engine_with_model(tmp_path)
    with pytest.raises(ValueError, match="outside"):
        engine.validate_voice(str(outside))


def test_validate_voice_rejects_pickle_formats(tmp_path):
    pickle = tmp_path / "voice.pt"
    pickle.write_bytes(b"x")
    engine = engine_with_model(tmp_path)
    with pytest.raises(ValueError, match="safetensors"):
        engine.validate_voice(str(pickle))


def test_validate_voice_rejects_oversized_states(tmp_path):
    state = tmp_path / "huge.safetensors"
    with state.open("wb") as handle:
        handle.truncate(MAX_STATE_BYTES + 1)
    engine = engine_with_model(tmp_path)
    with pytest.raises(ValueError, match="limit"):
        engine.validate_voice(str(state))


def test_synthesize_resamples_by_the_rate_multiplier(tmp_path):
    engine = engine_with_model(tmp_path, samples=100)
    normal = b"".join(engine.synthesize("state", "Hello.", rate=1.0))
    faster = b"".join(engine.synthesize("state", "Hello.", rate=2.0))
    slower = b"".join(engine.synthesize("state", "Hello.", rate=0.5))
    assert len(normal) == 200  # 100 mono s16 samples
    assert 90 <= len(faster) <= 104
    assert 390 <= len(slower) <= 404


def test_synthesize_rejects_an_out_of_range_rate(tmp_path):
    engine = engine_with_model(tmp_path)
    with pytest.raises(ValueError, match="rate"):
        list(engine.synthesize("state", "Hello.", rate=4.0))


def test_state_for_caches_a_voice_after_first_use(tmp_path):
    engine = engine_with_model(tmp_path)
    first = engine.state_for("/voices/jarvis.safetensors")
    second = engine.state_for("/voices/jarvis.safetensors")
    warm = engine.warm_voice("/voices/jarvis.safetensors")
    assert first == second == warm
    assert engine._model.state_calls == ["/voices/jarvis.safetensors"]


def test_catalogue_is_the_whole_builtin_set_by_default(tmp_path):
    engine = PocketTtsEngine(voice_dir=str(tmp_path))
    assert engine.voices == ["alba"]
    assert engine.catalogue == list(BUILTIN_VOICES)
    assert len(engine.catalogue) == 27


def test_catalogue_keeps_the_warm_voice_first_and_dedupes(tmp_path):
    engine = PocketTtsEngine(
        voices=["marius"], catalogue=["alba", "marius", "vera"], voice_dir=str(tmp_path)
    )
    assert engine.catalogue == ["marius", "alba", "vera"]


def test_a_cold_catalogue_voice_loads_on_first_use(tmp_path):
    """A catalogue voice is not warmed, but selecting it must still work."""
    engine = PocketTtsEngine(
        voices=["alba"], catalogue=["alba", "marius"], voice_dir=str(tmp_path)
    )
    engine._model = FakeModel()
    engine.revision = "test-revision"
    assert engine._model.state_calls == []
    assert engine.state_for("alba") == "alba"
    assert engine.state_for("marius") == "marius"
    assert engine._model.state_calls == ["alba", "marius"]
