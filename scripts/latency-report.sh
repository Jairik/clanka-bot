#!/usr/bin/env bash
# Summarize per-stage Response latency from a running stack's logs.
#
#   scripts/latency-report.sh                 # the current stack
#   scripts/latency-report.sh -source debug   # typed entries
#   scripts/latency-report.sh -file saved.log # a saved log instead of Docker
#
# Extra arguments are passed to `assistant/cmd/latency-report`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVICE="${LATENCY_LOG_SERVICE:-assistant}"

file_mode=0
for arg in "$@"; do
	case "$arg" in
	-file | --file | -file=* | --file=*) file_mode=1 ;;
	esac
done

if [ "$file_mode" -eq 1 ]; then
	cd "$ROOT/assistant"
	exec go run ./cmd/latency-report "$@"
fi

cd "$ROOT"
docker compose logs --no-log-prefix "$SERVICE" |
	(cd "$ROOT/assistant" && go run ./cmd/latency-report "$@")
