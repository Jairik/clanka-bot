#!/usr/bin/env python3
"""Add per-model detection thresholds to the pinned openWakeWord service.

The upstream handler compares every detector's probability against one global
``--threshold``. The Assistant needs a threshold per model: the wake word and
#81's non-wake command models have different false-trigger budgets. The
Assistant therefore sends an optional map with each detect request::

    {"type": "detect",
     "data": {"names": ["clanker"], "thresholds": {"clanker": 0.6}}}

A service that predates the patch ignores the extra key, so the wire change is
backward compatible for other clients. This script rewrites the installed
handler inside the image at build time. The base image is pinned by digest, and
every anchor must match exactly once: a bumped upstream image either patches or
fails the build loudly, never silently drops the feature.
"""

from __future__ import annotations

import sys
from pathlib import Path

HANDLER = Path("/usr/src/wyoming_openwakeword/handler.py")

REPLACEMENTS = [
    (
        "        self.threshold = threshold\n",
        "        self.threshold = threshold\n"
        "        # Per-model overrides from the current Detect request.\n"
        "        self.thresholds: Dict[str, float] = {}\n",
    ),
    (
        "        if Detect.is_type(event.type):\n"
        "            detect = Detect.from_event(event)\n",
        "        if Detect.is_type(event.type):\n"
        "            detect = Detect.from_event(event)\n"
        '            self.thresholds = _parse_thresholds((event.data or {}).get("thresholds"))\n',
    ),
    (
        "                        if prob <= self.threshold:\n",
        "                        if prob <= self.thresholds.get(detector.id, self.threshold):\n",
    ),
]

HELPER = '''

def _parse_thresholds(raw) -> Dict[str, float]:
    """Read the optional per-model threshold map from a Detect event."""
    if not isinstance(raw, dict):
        return {}

    parsed: Dict[str, float] = {}
    for name, value in raw.items():
        try:
            threshold = float(value)
        except (TypeError, ValueError):
            _LOGGER.warning("Ignoring threshold for %s: %r is not a number", name, value)
            continue
        if not 0.0 <= threshold <= 1.0:
            _LOGGER.warning("Ignoring threshold for %s: %r is outside 0..1", name, threshold)
            continue
        parsed[str(name)] = threshold

    return parsed
'''


def main() -> int:
    text = HANDLER.read_text()
    for old, new in REPLACEMENTS:
        count = text.count(old)
        if count != 1:
            print(
                f"per-model threshold patch: anchor found {count} times, want 1: {old!r}",
                file=sys.stderr,
            )
            return 1
        text = text.replace(old, new)

    text += HELPER
    HANDLER.write_text(text)
    print(f"patched {HANDLER}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
