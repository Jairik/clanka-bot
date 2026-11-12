#!/usr/bin/env bash
# Passive idle-noise check for the configured capture source. Read-only: it
# records a few seconds, prints the level, and never changes host audio.
#
#   scripts/mic-noise-check.sh [source] [seconds]
#
# Source is a Pulse/PipeWire source name; empty uses the default source. The
# endpointer threshold lives in assistant/config.yaml (stt.endpoint). The fixed
# 500 ms window is deliberate and is not changed by this check.
set -euo pipefail

SOURCE="${1:-}"
SECONDS_TO_RECORD="${2:-5}"
RATE=16000
THRESHOLD="${MIC_NOISE_THRESHOLD:--38}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

command -v parec >/dev/null 2>&1 || {
	echo "mic-noise-check: parec is required (pulseaudio-utils)" >&2
	exit 2
}
command -v python3 >/dev/null 2>&1 || {
	echo "mic-noise-check: python3 is required" >&2
	exit 2
}

tmp="$(mktemp "${TMPDIR:-/tmp}/clanka-mic.XXXXXX.raw")"
trap 'rm -f "$tmp"' EXIT

args=(--rate="$RATE" --channels=1 --format=s16le --raw)
if [[ -n "$SOURCE" ]]; then
	args+=(--device="$SOURCE")
	echo "recording $SECONDS_TO_RECORD s from $SOURCE (stay quiet) ..."
else
	echo "recording $SECONDS_TO_RECORD s from the default source (stay quiet) ..."
fi

# timeout is expected to exit 124 after the recording window.
timeout "$SECONDS_TO_RECORD" parec "${args[@]}" >"$tmp" 2>/dev/null || true
if [[ ! -s "$tmp" ]]; then
	echo "mic-noise-check: no audio captured" >&2
	exit 1
fi

python3 "$SCRIPT_DIR/mic_noise.py" "$tmp" --rate "$RATE" --threshold "$THRESHOLD"

echo
echo "To lower the source gain, inspect the current volume and choose a value:"
echo "  pactl get-source-volume ${SOURCE:-@DEFAULT_SOURCE@}"
echo "  pactl set-source-volume ${SOURCE:-@DEFAULT_SOURCE@} 32%   # example only"
echo "Re-run this check and record the chosen gain and threshold in the ticket."
