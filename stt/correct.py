"""Bounded deterministic vocabulary correction for Moonshine transcripts.

Moonshine 0.1.6 can bias decoding toward keyterms, but that still leaves
near-miss splits such as "spot ify". This module rewrites only distinctive
device and product names after decode. It never touches numbers, negation,
short common words or free-form dictation, and it keeps the raw string so a
bad rewrite can be diagnosed without logging the utterance.

Both features stay off until a measured corpus says they help. Set
STT_CORRECT=1 for post-decode rewrites and STT_KEYTERMS=1 for decoder
bias. Empty, missing, or any other value leaves production text unchanged.
"""

from __future__ import annotations

import json
import os
import re
import unicodedata
from dataclasses import dataclass, field
from pathlib import Path

DEFAULT_VOCAB = Path(__file__).with_name("vocab.json")

# Tokens we will never rewrite, even if they sit one edit from a vocab term.
# Includes function words, number words, negation, and English words that are
# also product-ish (monster, lights) so dictation cannot become a Tool call.
COMMON_WORDS = frozenset(
    """
    a an the and or of to for from with on off in at by as is it if so
    i you we they me my your our this that these those here there
    turn set play pause stop cancel skip next back resume start
    lights light timer alarm reminder volume music song songs
    what time is date weather please could can would
    monster monsters
    do does did doing done
    not no nor none never neither nobody nothing
    dont don't didn't didnt wont won't cant can't
    off on up down
    one two three four five six seven eight nine ten
    eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen
    twenty thirty forty fifty sixty seventy eighty ninety hundred thousand
    half quarter
    minute minutes hour hours second seconds
    percent
    """.split()
)

# Whole tokens that mark negation. A rewrite that drops one of these is unsafe.
NEGATION = frozenset(
    {
        "not",
        "no",
        "never",
        "none",
        "neither",
        "nobody",
        "nothing",
        "dont",
        "don't",
        "didn't",
        "didnt",
        "won't",
        "wont",
        "can't",
        "cant",
        "neither",
    }
)

NUMBER_RE = re.compile(r"^\d+([.,]\d+)?$")
TOKEN_RE = re.compile(r"[A-Za-z0-9+']+|[^\sA-Za-z0-9+']+")

# Fuzzy match is only for distinctive terms of this length or more.
MIN_FUZZY_CHARS = 5
MAX_EDIT_DISTANCE = 1


@dataclass(frozen=True)
class VocabTerm:
    """One allowed rewrite target and the surface forms we accept for it."""

    canonical: str
    aliases: tuple[str, ...]


@dataclass
class Replacement:
    """One span that changed. Logged as counts, never as the utterance."""

    source: str
    canonical: str
    kind: str


@dataclass
class Correction:
    """Result of one pass. raw is the model output; text is what routing sees."""

    raw: str
    text: str
    replacements: list[Replacement] = field(default_factory=list)

    @property
    def changed(self) -> bool:
        return self.raw != self.text


def load_vocab(path: Path | None = None) -> list[VocabTerm]:
    """Read the checked-in term list. Missing file means no correction."""
    path = path or Path(os.environ.get("STT_VOCAB", DEFAULT_VOCAB))
    if not path.is_file():
        return []
    data = json.loads(path.read_text())
    terms = []
    for entry in data.get("terms") or []:
        canonical = str(entry.get("canonical") or "").strip()
        if not canonical:
            continue
        aliases = tuple(
            str(alias).strip()
            for alias in (entry.get("aliases") or [])
            if str(alias).strip()
        )
        terms.append(VocabTerm(canonical=canonical, aliases=aliases))
    return terms


def load_keyterms(path: Path | None = None) -> list[str]:
    """Bias list for Moonshine set_keyterms. Terms must not contain commas."""
    path = path or Path(os.environ.get("STT_VOCAB", DEFAULT_VOCAB))
    if not path.is_file():
        return []
    data = json.loads(path.read_text())
    listed = data.get("keyterms")
    if listed:
        return [str(term).strip() for term in listed if str(term).strip() and "," not in str(term)]
    return [term.canonical for term in load_vocab(path) if "," not in term.canonical]


# Only these strings turn a gate on. Default, empty, and anything else stay off
# so a half-set env cannot enable rewrites before the corpus is measured.
_ON_VALUES = frozenset({"1", "true", "on", "yes"})


def _env_flag(name: str, default: str = "0") -> bool:
    """True only for an explicit on-value. Default and empty are off."""
    raw = os.environ.get(name, default)
    if raw is None:
        return False
    return raw.strip().lower() in _ON_VALUES


def correction_enabled() -> bool:
    """STT_CORRECT=1 enables post-decode rewrites. Off by default."""
    return _env_flag("STT_CORRECT")


def keyterm_bias_enabled() -> bool:
    """STT_KEYTERMS=1 enables Moonshine set_keyterms. Off by default."""
    return _env_flag("STT_KEYTERMS")


class VocabularyCorrector:
    """Applies the longest safe match first, then a unique edit-distance-1 pass."""

    def __init__(self, terms: list[VocabTerm] | None = None) -> None:
        self.terms = list(terms or [])
        # Phrase forms keyed by lowercase whitespace-normalized text.
        self.phrases: dict[str, VocabTerm] = {}
        for term in self.terms:
            for form in (term.canonical, *term.aliases):
                key = _norm(form)
                if key:
                    self.phrases[key] = term
        self.max_words = 1
        for key in self.phrases:
            self.max_words = max(self.max_words, len(key.split()))

    @classmethod
    def from_path(cls, path: Path | None = None) -> "VocabularyCorrector":
        return cls(load_vocab(path))

    def apply(self, raw: str) -> Correction:
        """Return a corrected transcript, or the original when nothing is safe.

        Matches replace only their original character span. Punctuation,
        decimals, times, and unmatched dictation are copied byte-for-byte so
        a pass that changes "spot ify" cannot turn "1.5" into "1. 5" or
        rewrite commas in a reminder.
        """
        text = raw or ""
        if not text.strip() or not self.terms:
            return Correction(raw=raw, text=raw, replacements=[])

        # finditer keeps the offsets TOKEN_RE.findall would throw away.
        spans = list(TOKEN_RE.finditer(text))
        tokens = [match.group(0) for match in spans]
        replacements: list[Replacement] = []
        pieces: list[str] = []
        cursor = 0
        index = 0
        while index < len(tokens):
            # Punctuation-only tokens are never match starts; they stay in the
            # unmatched gap and are copied from the original string.
            if not any(ch.isalnum() for ch in tokens[index]):
                index += 1
                continue

            matched, consumed, kind = self._match(tokens, index)
            if matched is not None and consumed > 0:
                start = spans[index].start()
                end = spans[index + consumed - 1].end()
                pieces.append(text[cursor:start])
                pieces.append(matched.canonical)
                replacements.append(
                    Replacement(
                        source=text[start:end],
                        canonical=matched.canonical,
                        kind=kind,
                    )
                )
                cursor = end
                index += consumed
                continue
            index += 1

        if not replacements:
            # No safe rewrite: keep the model text, including its punctuation.
            return Correction(raw=raw, text=raw, replacements=[])

        pieces.append(text[cursor:])
        return Correction(raw=raw, text="".join(pieces), replacements=replacements)

    def _match(self, tokens: list[str], index: int) -> tuple[VocabTerm | None, int, str]:
        """Longest exact phrase, then a unique fuzzy single-token match."""
        word_runs = _word_runs(tokens, index)
        if not word_runs:
            return None, 0, ""

        # Longest phrase first so "happy lighting" wins over "happy".
        for word_count in range(min(self.max_words, len(word_runs)), 0, -1):
            consumed, form = _consume_words(tokens, index, word_count)
            if consumed == 0:
                continue
            if _protected(form):
                continue
            term = self.phrases.get(_norm(form))
            if term is None:
                continue
            # Already the canonical spelling: keep the user's text as-is.
            if _norm(form) == _norm(term.canonical) and form == term.canonical:
                return None, 0, ""
            return term, consumed, "exact"

        # Concatenate two adjacent word tokens ("spot" + "ify") into one term.
        if len(word_runs) >= 2:
            first, second = word_runs[0][1], word_runs[1][1]
            glued = first + second
            term = self.phrases.get(_norm(glued))
            if term is not None and not _protected(first) and not _protected(second):
                consumed = word_runs[1][0] + 1
                return term, consumed, "split"

        # Fuzzy: only a single distinctive token, unique winner, not a common word.
        _, first_word = word_runs[0]
        if (
            len(first_word) >= MIN_FUZZY_CHARS
            and not _protected(first_word)
            and _norm(first_word) not in COMMON_WORDS
        ):
            winner = self._fuzzy(first_word)
            if winner is not None:
                return winner, 1, "fuzzy"
        return None, 0, ""

    def _fuzzy(self, token: str) -> VocabTerm | None:
        """Edit distance 1 against canonical forms and aliases; skip ties."""
        needle = _norm(token)
        found: list[VocabTerm] = []
        seen: set[str] = set()
        for form, term in self.phrases.items():
            if " " in form:
                continue
            if abs(len(form) - len(needle)) > MAX_EDIT_DISTANCE:
                continue
            if _edit_distance(needle, form) <= MAX_EDIT_DISTANCE:
                if term.canonical not in seen:
                    found.append(term)
                    seen.add(term.canonical)
        if len(found) == 1:
            return found[0]
        return None


def apply_keyterms(transcriber, terms: list[str]) -> bool:
    """Bias the next Moonshine stream if this version exposes set_keyterms."""
    if not terms:
        return False
    setter = getattr(transcriber, "set_keyterms", None)
    if setter is None:
        return False
    setter(list(terms))
    return True


def _norm(text: str) -> str:
    """Case-fold and collapse whitespace for matching."""
    folded = unicodedata.normalize("NFKC", text).casefold()
    return " ".join(folded.split())


def _protected(token: str) -> bool:
    """True for numbers, negation and other tokens we must not rewrite."""
    folded = _norm(token)
    if not folded:
        return True
    if NUMBER_RE.match(folded.replace(",", "")):
        return True
    if folded in NEGATION or folded in COMMON_WORDS:
        return True
    return False


def _word_runs(tokens: list[str], index: int) -> list[tuple[int, str]]:
    """Upcoming word tokens as (token_index_offset, word) pairs from index."""
    runs: list[tuple[int, str]] = []
    offset = 0
    while index + offset < len(tokens) and len(runs) < 8:
        token = tokens[index + offset]
        if not any(ch.isalnum() for ch in token):
            break
        runs.append((offset, token))
        offset += 1
        if offset > 12:
            break
    return runs


def _consume_words(tokens: list[str], index: int, word_count: int) -> tuple[int, str]:
    """Take consecutive word tokens up to the next punctuation boundary."""
    seen = 0
    last = 0
    words: list[str] = []
    for offset, token in enumerate(tokens[index:]):
        if any(ch.isalnum() for ch in token):
            seen += 1
            words.append(token)
            last = offset
            if seen == word_count:
                return last + 1, " ".join(words)
        else:
            return 0, ""
    return 0, ""


def _edit_distance(left: str, right: str) -> int:
    """Levenshtein distance with a tiny bound; callers already length-check."""
    if left == right:
        return 0
    if abs(len(left) - len(right)) > MAX_EDIT_DISTANCE:
        return MAX_EDIT_DISTANCE + 1
    previous = list(range(len(right) + 1))
    for i, lch in enumerate(left, start=1):
        current = [i]
        for j, rch in enumerate(right, start=1):
            insert = current[j - 1] + 1
            delete = previous[j] + 1
            replace = previous[j - 1] + (0 if lch == rch else 1)
            current.append(min(insert, delete, replace))
        previous = current
    return previous[-1]
