import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from correct import (
    VocabTerm,
    VocabularyCorrector,
    apply_keyterms,
    correction_enabled,
    keyterm_bias_enabled,
    load_keyterms,
    load_vocab,
)


def corrector():
    return VocabularyCorrector.from_path()


def test_vocab_loads_distinctive_terms():
    terms = load_vocab()
    names = {term.canonical for term in terms}
    assert "Spotify" in names
    assert "Hermes" in names
    assert "HappyLighting" in names
    assert "monster" not in {term.canonical.casefold() for term in terms}


def test_keyterms_have_no_commas():
    terms = load_keyterms()
    assert terms
    assert all("," not in term for term in terms)


def test_split_spotify_is_joined():
    result = corrector().apply("play music on spot ify")
    assert result.text == "play music on Spotify"
    assert result.raw == "play music on spot ify"
    assert result.changed is True
    assert [item.canonical for item in result.replacements] == ["Spotify"]


def test_case_fold_canonicalizes_spotify():
    result = corrector().apply("play music on spotify")
    assert result.text == "play music on Spotify"


def test_happy_lighting_phrase():
    result = corrector().apply("set happy lighting to blue")
    assert result.text == "set HappyLighting to blue"


def test_already_canonical_is_unchanged():
    result = corrector().apply("play music on Spotify")
    assert result.text == "play music on Spotify"
    assert result.changed is False


def test_negation_is_not_stripped():
    result = corrector().apply("don't turn on the lights")
    assert result.text == "don't turn on the lights"
    assert result.changed is False


def test_do_not_cancel_is_not_rewritten():
    result = corrector().apply("do not cancel the timer")
    assert result.text == "do not cancel the timer"


def test_numbers_are_not_rewritten():
    result = corrector().apply("set a timer for 15 minutes")
    assert result.text == "set a timer for 15 minutes"
    result = corrector().apply("set lights brightness to 50 percent")
    assert result.text == "set lights brightness to 50 percent"


def test_decimal_and_time_punctuation_stay_glued():
    # Tokenising 1.5 / 1:30 used to rejoin them as "1. 5" and "1: 30".
    result = corrector().apply("set a timer for 1.5 hours")
    assert result.text == "set a timer for 1.5 hours"
    result = corrector().apply("wake me at 1:30")
    assert result.text == "wake me at 1:30"
    result = corrector().apply("1.5")
    assert result.text == "1.5"
    result = corrector().apply("1:30")
    assert result.text == "1:30"


def test_dictation_commas_are_preserved():
    spoken = "remind me to pick up milk, eggs, and bread"
    result = corrector().apply(spoken)
    assert result.text == spoken
    assert result.changed is False


def test_rewrite_keeps_surrounding_punctuation():
    result = corrector().apply("play spotify at 1.5, then stop")
    assert result.text == "play Spotify at 1.5, then stop"


def test_herman_is_not_rewritten_to_hermes():
    result = corrector().apply("text Herman I am running late")
    assert result.text == "text Herman I am running late"
    assert result.changed is False


def test_short_spot_is_not_spotify():
    result = corrector().apply("spot the difference")
    assert result.text == "spot the difference"


def test_monster_dictation_is_not_a_device():
    result = corrector().apply("there is a monster under the bed")
    assert result.text == "there is a monster under the bed"


def test_ambiguous_near_miss_is_left_alone():
    # Distance 1 from both Clanker and Clanka would be a tie if it existed;
    # "clankor" is only close to Clanker.
    result = corrector().apply("wake clankor")
    assert result.text == "wake Clanker"


def test_empty_and_blank_pass_through():
    result = corrector().apply("")
    assert result.text == ""
    result = corrector().apply("   ")
    assert result.text == "   "


def test_apply_keyterms_skips_missing_api():
    class NoBias:
        pass

    assert apply_keyterms(NoBias(), ["Spotify"]) is False


def test_apply_keyterms_calls_set_keyterms():
    class Fake:
        def __init__(self):
            self.seen = None

        def set_keyterms(self, terms):
            self.seen = list(terms)

    fake = Fake()
    assert apply_keyterms(fake, ["Spotify", "Hermes"]) is True
    assert fake.seen == ["Spotify", "Hermes"]


def test_custom_terms_do_not_rewrite_unlisted_jargon():
    small = VocabularyCorrector([VocabTerm(canonical="Spotify", aliases=("spotify",))])
    result = small.apply("ask Hermes to summarise this")
    assert result.text == "ask Hermes to summarise this"


def test_correction_and_keyterms_are_off_by_default(monkeypatch):
    monkeypatch.delenv("STT_CORRECT", raising=False)
    monkeypatch.delenv("STT_KEYTERMS", raising=False)
    assert correction_enabled() is False
    assert keyterm_bias_enabled() is False
    monkeypatch.setenv("STT_CORRECT", "")
    monkeypatch.setenv("STT_KEYTERMS", "0")
    assert correction_enabled() is False
    assert keyterm_bias_enabled() is False
    monkeypatch.setenv("STT_CORRECT", "1")
    monkeypatch.setenv("STT_KEYTERMS", "true")
    assert correction_enabled() is True
    assert keyterm_bias_enabled() is True
