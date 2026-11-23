"""Scoring helpers for the STT replay benchmark.

WER is word-level Levenshtein. Entity accuracy is a per-clip all-or-nothing
check against the fixture's annotated values. Clipping is tagged by the
endpointer replay, not inferred from WER, so a clipped onset is not counted
as a recognizer error on its own.
"""

from __future__ import annotations

import math
import re
from dataclasses import dataclass

WORD_RE = re.compile(r"[A-Za-z0-9+']+")


@dataclass
class ClipScore:
    """One clip's recognizer and endpointing outcome."""

    clip_id: str
    expected: str
    hypothesis: str
    raw: str
    wer: float
    raw_wer: float
    entity_ok: bool
    start_clipped: bool
    end_clipped: bool
    recognizer_error: bool
    finalize_ms: float | None = None
    replacements: int = 0
    unsafe_rewrite: bool = False


@dataclass
class Summary:
    """Roll-up for a model on one fixture set."""

    clips: int
    wer: float
    wer_raw: float
    wer_final: float
    entity_accuracy: float
    p50_finalize_ms: float | None
    p95_finalize_ms: float | None
    start_clipped: int
    end_clipped: int
    recognizer_errors: int
    unsafe_rewrites: int
    exact_transcript_match: float | None = None


def tokenize(text: str) -> list[str]:
    """Lowercased word tokens for WER. Punctuation does not count."""
    return [word.casefold() for word in WORD_RE.findall(text or "")]


def word_error_rate(expected: str, hypothesis: str) -> float:
    """Word error rate; empty expected vs empty hypothesis is 0, else 1."""
    ref = tokenize(expected)
    hyp = tokenize(hypothesis)
    if not ref:
        return 0.0 if not hyp else 1.0
    distance = _levenshtein(ref, hyp)
    return distance / len(ref)


def entity_match(expected_entities: dict | None, hypothesis: str) -> bool:
    """True when every annotated entity still appears in the hypothesis.

    Numbers must appear as digits or as the original token. Booleans only
    check presence of negation words when the fixture marked negation.
    Missing annotations are a pass so command-only clips still score.
    """
    if not expected_entities:
        return True
    text = hypothesis or ""
    folded = text.casefold()
    for key, value in expected_entities.items():
        if key == "negation":
            if value and not _has_negation(folded):
                return False
            continue
        if value is None or value is True:
            continue
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            as_int = str(int(value)) if float(value).is_integer() else str(value)
            if as_int not in folded and str(value).casefold() not in folded:
                # Accept a small set of number words for the same value.
                words = NUMBER_WORDS.get(int(value), ()) if float(value).is_integer() else ()
                if not any(word in folded.split() for word in words):
                    return False
            continue
        needle = str(value).casefold()
        if needle and needle not in folded:
            return False
    return True


def percentile(values: list[float], p: float) -> float | None:
    """Nearest-rank percentile; empty list is None."""
    if not values:
        return None
    ordered = sorted(values)
    index = min(len(ordered) - 1, max(0, int(round((len(ordered) - 1) * p))))
    return ordered[index]


def summarize(scores: list[ClipScore]) -> Summary:
    """Average WER, entity accuracy, latency percentiles and error splits."""
    if not scores:
        return Summary(
            clips=0,
            wer=0.0,
            wer_raw=0.0,
            wer_final=0.0,
            entity_accuracy=1.0,
            p50_finalize_ms=None,
            p95_finalize_ms=None,
            start_clipped=0,
            end_clipped=0,
            recognizer_errors=0,
            unsafe_rewrites=0,
        )
    # Final WER is what routing would see; raw WER is the model before rewrite.
    wer_final = sum(score.wer for score in scores) / len(scores)
    wer_raw = sum(score.raw_wer for score in scores) / len(scores)
    wer = wer_final
    entity_ok = sum(1 for score in scores if score.entity_ok) / len(scores)
    latencies = [score.finalize_ms for score in scores if score.finalize_ms is not None]
    success = None
    comparable_clips = [score for score in scores if not math.isnan(score.wer)]
    if comparable_clips:
        # Count exact transcript matches without drawing conclusions about routing.
        matched = [
            score
            for score in comparable_clips
            if score.wer == 0.0 and not score.unsafe_rewrite
        ]
        success = len(matched) / len(comparable_clips)
    return Summary(
        clips=len(scores),
        wer=wer,
        wer_raw=wer_raw,
        wer_final=wer_final,
        entity_accuracy=entity_ok,
        p50_finalize_ms=percentile(latencies, 0.50),
        p95_finalize_ms=percentile(latencies, 0.95),
        start_clipped=sum(1 for score in scores if score.start_clipped),
        end_clipped=sum(1 for score in scores if score.end_clipped),
        recognizer_errors=sum(1 for score in scores if score.recognizer_error),
        unsafe_rewrites=sum(1 for score in scores if score.unsafe_rewrite),
        exact_transcript_match=success,
    )


def score_clip(
    clip_id: str,
    expected: str,
    hypothesis: str,
    raw: str,
    entities: dict | None,
    start_clipped: bool,
    end_clipped: bool,
    finalize_ms: float | None,
    replacements: int,
    unsafe_if: list[str] | None,
) -> ClipScore:
    """Tag a clip as clipped, a recognizer miss, an unsafe rewrite, or clean."""
    wer = word_error_rate(expected, hypothesis)
    raw_wer = word_error_rate(expected, raw)
    unsafe = False
    folded_hyp = (hypothesis or "").casefold()
    for banned in unsafe_if or []:
        if banned.casefold() == folded_hyp:
            unsafe = True
    # Recognizer error is the model text, not the post-decode rewrite.
    recognizer_error = raw_wer > 0.0 and not start_clipped and not end_clipped
    return ClipScore(
        clip_id=clip_id,
        expected=expected,
        hypothesis=hypothesis,
        raw=raw,
        wer=wer,
        raw_wer=raw_wer,
        entity_ok=entity_match(entities, hypothesis),
        start_clipped=start_clipped,
        end_clipped=end_clipped,
        recognizer_error=recognizer_error,
        finalize_ms=finalize_ms,
        replacements=replacements,
        unsafe_rewrite=unsafe,
    )


def _levenshtein(ref: list[str], hyp: list[str]) -> int:
    """Word-level edit distance."""
    previous = list(range(len(hyp) + 1))
    for i, ref_word in enumerate(ref, start=1):
        current = [i]
        for j, hyp_word in enumerate(hyp, start=1):
            insert = current[j - 1] + 1
            delete = previous[j] + 1
            replace = previous[j - 1] + (0 if ref_word == hyp_word else 1)
            current.append(min(insert, delete, replace))
        previous = current
    return previous[-1]


def _has_negation(folded: str) -> bool:
    tokens = set(WORD_RE.findall(folded))
    return bool(
        tokens
        & {
            "not",
            "no",
            "never",
            "don't",
            "dont",
            "didn't",
            "won't",
            "can't",
            "none",
        }
    )


# Spoken forms accepted when a fixture annotates an integer entity.
NUMBER_WORDS = {
    0: ("zero",),
    1: ("one",),
    2: ("two",),
    3: ("three",),
    4: ("four",),
    5: ("five",),
    15: ("fifteen",),
    20: ("twenty",),
    30: ("thirty",),
    50: ("fifty",),
}
