#!/usr/bin/env bash
# Collect the owner's reduced wake-word dataset using the Pi's default Pulse mic.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"

# Keep the interactive play/keep/redo controls and allow caller overrides.
exec python3 "$ROOT/wakeword/training/record.py" \
    --positive 25 --negative 15 --seconds 3 "$@"
