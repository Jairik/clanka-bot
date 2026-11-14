import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from builtin_voices import BUILTIN_VOICES, catalogue_names  # noqa: E402


def test_the_catalogue_is_the_twenty_seven_pocket_tts_embeddings():
    assert len(BUILTIN_VOICES) == 27
    assert len(set(BUILTIN_VOICES)) == 27
    assert BUILTIN_VOICES[:2] == ("alba", "anna")
    assert BUILTIN_VOICES == tuple(sorted(BUILTIN_VOICES))


def test_all_and_empty_mean_the_whole_catalogue():
    assert catalogue_names("all") == list(BUILTIN_VOICES)
    assert catalogue_names(" ALL ") == list(BUILTIN_VOICES)
    assert catalogue_names("") == list(BUILTIN_VOICES)
    assert catalogue_names(None) == list(BUILTIN_VOICES)


def test_a_subset_is_parsed_and_deduped_in_order():
    assert catalogue_names(" vera, marius ,vera ") == ["vera", "marius"]
