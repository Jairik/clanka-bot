#!/usr/bin/env bash
# POST the post-deploy smoke endpoint to prove the assistant pipeline answers.
# Exercises routing and Tool execution with no model call and no audio playback.
#
#   scripts/smoke.sh
#
# Environment:
#   ASSISTANT_DASHBOARD_OWNER  owner login for Tailscale-User-Login (read from .env if unset)
#   ASSISTANT_DASHBOARD_PORT   port on loopback (default 8082, read from .env if unset)
#   SMOKE_URL                  full endpoint URL override (default: http://127.0.0.1:<port>/api/ui/smoke)
#   SMOKE_TIMEOUT              curl max-time in seconds (default 10)
#   SMOKE_ENV_FILE             path to .env file (default $ROOT/.env)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ENV_FILE="${SMOKE_ENV_FILE:-$ROOT/.env}"

owner="${ASSISTANT_DASHBOARD_OWNER:-}"
if [[ -z "$owner" && -f "$ENV_FILE" ]]; then
	owner="$(grep -E '^[[:space:]]*ASSISTANT_DASHBOARD_OWNER=' "$ENV_FILE" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
fi

if [[ -z "$owner" ]]; then
	printf 'smoke: warning: ASSISTANT_DASHBOARD_OWNER is empty (Dashboard is off); skipping smoke turn\n' >&2
	exit 0
fi

port="${ASSISTANT_DASHBOARD_PORT:-}"
if [[ -z "$port" && -f "$ENV_FILE" ]]; then
	port="$(grep -E '^[[:space:]]*ASSISTANT_DASHBOARD_PORT=' "$ENV_FILE" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
fi
port="${port:-8082}"

url="${SMOKE_URL:-http://127.0.0.1:${port}/api/ui/smoke}"
timeout="${SMOKE_TIMEOUT:-10}"

tmp_body="$(mktemp "${TMPDIR:-/tmp}/smoke-body.XXXXXX")"
trap 'rm -f "$tmp_body"' EXIT

http_code=""
if ! http_code="$(curl -sS -o "$tmp_body" -w '%{http_code}' \
	--max-time "$timeout" \
	-X POST \
	-H "Tailscale-User-Login: $owner" \
	-H "Sec-Fetch-Site: same-origin" \
	-H "Accept: application/json" \
	"$url" 2>&1)"; then
	printf 'smoke: error: request to %s failed: %s\n' "$url" "$http_code" >&2
	if [[ -s "$tmp_body" ]]; then
		printf 'smoke: response body: %s\n' "$(<"$tmp_body")" >&2
	fi
	exit 1
fi

if [[ "$http_code" != "200" ]]; then
	printf 'smoke: error: unexpected HTTP status %s from %s\n' "$http_code" "$url" >&2
	if [[ -s "$tmp_body" ]]; then
		printf 'smoke: response body: %s\n' "$(<"$tmp_body")" >&2
	fi
	exit 1
fi

if ! command -v python3 >/dev/null 2>&1; then
	printf 'smoke: error: python3 is required to validate the response\n' >&2
	exit 1
fi

python3 -I - "$tmp_body" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    raw = f.read()

try:
    data = json.loads(raw)
except Exception as e:
    sys.stderr.write(f"smoke: error: invalid JSON response: {e}\nresponse body: {raw}\n")
    sys.exit(1)

route = data.get("route", "")
tool = data.get("tool", "")
reply = data.get("reply", "")
stages = data.get("stages", {}) or {}
routing_ms = stages.get("routing_ms", 0)
tool_ms = stages.get("tool_ms", 0)

# The owner may have a Control phrase for the same read-only clock Tool.
if route not in {"fast_path", "control_phrase"}:
    msg = f"smoke: error: expected a clock route (fast_path or control_phrase), got {route!r}"
    if route == "agent_path":
        msg += " (pipeline fell through to agent; embed service may be stopped)"
    sys.stderr.write(f"{msg}; tool={tool!r} reply={reply!r}\nresponse body: {raw}\n")
    sys.exit(1)

if tool != "time.now":
    sys.stderr.write(
        f"smoke: error: expected tool 'time.now', got {tool!r} (tool may be disabled); reply={reply!r}\n"
        f"response body: {raw}\n"
    )
    sys.exit(1)

if not str(reply).strip():
    sys.stderr.write(
        f"smoke: error: reply is empty; route={route!r} tool={tool!r}\n"
        f"response body: {raw}\n"
    )
    sys.exit(1)

print(f"smoke: ok route={route} tool={tool} routing={routing_ms}ms tool={tool_ms}ms reply={reply}")
PY
