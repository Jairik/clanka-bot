import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from metrics import entity_match, score_clip, summarize, word_error_rate


def test_wer_is_zero_for_an_exact_match():
    assert word_error_rate("what time is it", "what time is it") == 0.0
    assert word_error_rate("What time is it?", "what time is it") == 0.0


def test_wer_counts_substitutions():
    assert word_error_rate("turn on the lights", "turn off the lights") == 1 / 4


def test_empty_expected_with_empty_hypothesis_is_zero():
    assert word_error_rate("", "") == 0.0
    assert word_error_rate("", "noise") == 1.0


def test_entity_match_keeps_numbers_and_negation():
    assert entity_match({"percent": 50}, "set lights brightness to 50 percent")
    assert entity_match({"duration_minutes": 5}, "set a timer for five minutes")
    assert entity_match({"negation": True}, "don't turn on the lights")
    assert not entity_match({"negation": True}, "turn on the lights")
    assert not entity_match({"percent": 50}, "set lights brightness to 80 percent")


def test_clipped_mismatch_is_not_a_recognizer_error():
    score = score_clip(
        clip_id="clip-end",
        expected="what time is it",
        hypothesis="what time",
        raw="what time",
        entities={},
        start_clipped=False,
        end_clipped=True,
        finalize_ms=40.0,
        replacements=0,
        unsafe_if=None,
    )
    assert score.recognizer_error is False
    assert score.end_clipped is True
    assert score.wer > 0


def test_uncut_mismatch_is_a_recognizer_error():
    score = score_clip(
        clip_id="command-time",
        expected="what time is it",
        hypothesis="what time is this",
        raw="what time is this",
        entities={},
        start_clipped=False,
        end_clipped=False,
        finalize_ms=20.0,
        replacements=0,
        unsafe_if=None,
    )
    assert score.recognizer_error is True


def test_unsafe_rewrite_is_flagged():
    score = score_clip(
        clip_id="negation-lights",
        expected="don't turn on the lights",
        hypothesis="turn on the lights",
        raw="don't turn on the lights",
        entities={"negation": True},
        start_clipped=False,
        end_clipped=False,
        finalize_ms=None,
        replacements=1,
        unsafe_if=["turn on the lights"],
    )
    assert score.unsafe_rewrite is True
    assert score.entity_ok is False


def test_raw_and_final_wer_are_scored_separately():
    score = score_clip(
        clip_id="name-spotify",
        expected="play music on Spotify",
        hypothesis="play music on Spotify",
        raw="play music on spot ify",
        entities={"name": "Spotify"},
        start_clipped=False,
        end_clipped=False,
        finalize_ms=30.0,
        replacements=1,
        unsafe_if=None,
    )
    assert score.wer == 0.0
    assert score.raw_wer > 0.0
    assert score.recognizer_error is True
    summary = summarize([score])
    assert summary.wer_final == 0.0
    assert summary.wer_raw > 0.0


def test_summarize_splits_clipping_from_recognizer_errors():
    scores = [
        score_clip("a", "one two", "one two", "one two", {}, False, False, 10, 0, None),
        score_clip("b", "one two", "one", "one", {}, False, True, 20, 0, None),
        score_clip("c", "one two", "one three", "one three", {}, False, False, 40, 0, None),
    ]
    summary = summarize(scores)
    assert summary.clips == 3
    assert summary.end_clipped == 1
    assert summary.recognizer_errors == 1
    assert summary.p50_finalize_ms == 20
    assert summary.p95_finalize_ms == 40
