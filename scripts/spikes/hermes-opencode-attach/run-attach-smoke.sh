#!/usr/bin/env bash
# Repeatable #76 attach harness: private loopback server, fake provider cases,
# and an optional real OpenCode Go coding run (RUN_REAL=1). It never prints a
# secret and never changes global OpenCode settings.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${OPENCODE_BIN:-opencode}"
MODEL="${MODEL:-opencode-go/deepseek-v4.1-flash#max}"
FAKE_TOKEN="spike-good"
TMP_BASE="${TMPDIR:-/tmp}/opencode"
mkdir -p "$TMP_BASE"
BASE="$(mktemp -d "$TMP_BASE/hermes-attach.XXXXXX")"
WORK="$BASE/work"
KEEP="${KEEP:-0}"
SERVE_PID=""
FAKE_PID=""

if ! command -v "$BIN" >/dev/null 2>&1; then
  echo "opencode not found: set OPENCODE_BIN" >&2
  exit 2
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo "python3 is required for the fake provider" >&2
  exit 2
fi

# Pick two ephemeral loopback ports, then release them for the children.
read -r FAKE_PORT SERVE_PORT < <(python3 - <<'PY'
import socket
socks = []
for _ in range(2):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    socks.append(s)
print(socks[0].getsockname()[1], socks[1].getsockname()[1])
for s in socks:
    s.close()
PY
)

mkdir -p "$WORK"
cat > "$WORK/go.mod" <<'EOF'
module example.com/spike

go 1.22
EOF
cat > "$WORK/add.go" <<'EOF'
package spike

// Add returns the sum of a and b.
func Add(a, b int) int {
	return 0
}
EOF
cat > "$WORK/add_test.go" <<'EOF'
package spike

import "testing"

func TestAdd(t *testing.T) {
	if got := Add(2, 3); got != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", got)
	}
}
EOF
cat > "$WORK/opencode.json" <<EOF
{
  "\$schema": "https://opencode.ai/config.json",
  "providers": {
    "spike": {
      "name": "Spike Fake",
      "env": ["SPIKE_FAKE_KEY"],
      "package": "@opencode/ai/providers/openai-compatible",
      "settings": { "baseURL": "http://127.0.0.1:$FAKE_PORT/v1" },
      "models": { "fake-model": { "name": "Fake Model", "capabilities": { "tools": true } } }
    }
  }
}
EOF

# OpenCode discovers the project-local opencode.json from the working directory.
cd "$WORK"

cleanup() {
  if [[ -n "$SERVE_PID" ]]; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
  fi
  if [[ -n "$FAKE_PID" ]]; then
    kill "$FAKE_PID" 2>/dev/null || true
    wait "$FAKE_PID" 2>/dev/null || true
  fi
  if [[ "$KEEP" == "1" ]]; then
    echo "kept $BASE"
  else
    rm -rf "$BASE"
  fi
}
trap cleanup EXIT

start_fake() {
  python3 "$HERE/fake_openai_server.py" --port "$FAKE_PORT" --log "$BASE/fake.log" --token "$FAKE_TOKEN" \
    > "$BASE/fake.out" 2>&1 &
  FAKE_PID=$!
  for _ in $(seq 1 100); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$FAKE_PORT/v1/models" 2>/dev/null; then
      return 0
    fi
    sleep 0.05
  done
  echo "fake provider did not start" >&2
  return 1
}

start_serve() {
  local key="${1:-}"
  if [[ -n "$key" ]]; then
    SPIKE_FAKE_KEY="$key" OPENCODE_DB="$BASE/state.db" "$BIN" serve \
      --hostname 127.0.0.1 --port "$SERVE_PORT" > "$BASE/serve.log" 2>&1 &
  else
    env -u SPIKE_FAKE_KEY OPENCODE_DB="$BASE/state.db" "$BIN" serve \
      --hostname 127.0.0.1 --port "$SERVE_PORT" > "$BASE/serve.log" 2>&1 &
  fi
  SERVE_PID=$!
  for _ in $(seq 1 100); do
    if curl -sS -m 1 -o /dev/null "http://127.0.0.1:$SERVE_PORT/" 2>/dev/null; then
      return 0
    fi
    sleep 0.1
  done
  echo "opencode serve did not start; log follows" >&2
  cat "$BASE/serve.log" >&2
  return 1
}

stop_serve() {
  if [[ -n "$SERVE_PID" ]]; then
    kill "$SERVE_PID" 2>/dev/null || true
    wait "$SERVE_PID" 2>/dev/null || true
    SERVE_PID=""
  fi
}

# run_attach <name> <model> <prompt> <seconds> <signal>
LAST_RC=0
LAST_OUT=""
run_attach() {
  local name="$1" model="$2" prompt="$3" limit="$4" signal="$5"
  local start end rc
  start=$(date +%s%N)
  set +e
  timeout -s "$signal" "$limit" "$BIN" run --server "http://127.0.0.1:$SERVE_PORT" \
    -m "$model" --format json "$prompt" > "$BASE/$name.out" 2>&1
  rc=$?
  set -e
  end=$(date +%s%N)
  LAST_RC="$rc"
  LAST_OUT="$BASE/$name.out"
  printf '%-16s rc=%-3s wall_ms=%s\n' "$name" "$rc" "$(( (end - start) / 1000000 ))" >> "$BASE/cases.txt"
}

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

start_fake
start_serve "$FAKE_TOKEN"

run_attach fake-ok spike/fake-model "Say something brief." 60 TERM
[[ "$LAST_RC" -eq 0 ]] || fail "fake success did not exit 0"
[[ $(grep -c 'fake summary: attach path reached the provider' "$LAST_OUT") -ge 1 ]] \
  || fail "fake success did not return the stub summary"

run_attach fake-timeout spike/fake-model "SLOW please take your time" 6 TERM
[[ "$LAST_RC" -eq 124 ]] || fail "timeout did not exit 124"
curl -sS -m 2 -o /dev/null "http://127.0.0.1:$SERVE_PORT/" || fail "server unhealthy after timeout"

run_attach fake-interrupt spike/fake-model "SLOW cancel me" 5 INT
[[ "$LAST_RC" -eq 124 ]] || fail "interrupt did not exit 124"
curl -sS -m 2 -o /dev/null "http://127.0.0.1:$SERVE_PORT/" || fail "server unhealthy after interrupt"

stop_serve
start_serve wrong
run_attach fake-badtoken spike/fake-model "Say something brief." 60 TERM
[[ "$LAST_RC" -ne 0 ]] || fail "bad token unexpectedly succeeded"
grep -q 'bad spike token' "$LAST_OUT" || fail "bad token was not surfaced"
stop_serve

start_serve ""
run_attach fake-missingkey spike/fake-model "Say something brief." 60 TERM
[[ "$LAST_RC" -ne 0 ]] || fail "missing key unexpectedly succeeded"
grep -q 'bad spike token' "$LAST_OUT" || fail "missing key was not surfaced"
stop_serve

run_attach unreachable spike/fake-model "Say something brief." 10 TERM
[[ "$LAST_RC" -ne 0 ]] || fail "unreachable server unexpectedly succeeded"
grep -q 'Could not reach server' "$LAST_OUT" || fail "unreachable server error was not surfaced"

if [[ "${RUN_REAL:-0}" == "1" ]]; then
  start_serve ""
  local_start=$(date +%s%N)
  set +e
  timeout "${REAL_TIMEOUT:-180}" "$BIN" run --server "http://127.0.0.1:$SERVE_PORT" \
    -m "$MODEL" --auto --format json \
    "Implement Add in add.go so it returns a+b. Do not change add_test.go. Run go test ./... and reply with a one-line summary." \
    > "$BASE/real-task.out" 2>&1
  real_rc=$?
  set -e
  local_end=$(date +%s%N)
  printf '%-16s rc=%-3s wall_ms=%s\n' real-task "$real_rc" "$(( (local_end - local_start) / 1000000 ))" >> "$BASE/cases.txt"
  if [[ $real_rc -ne 0 ]]; then
    echo "real model run failed (auth or model); see $BASE/real-task.out" >&2
    fail "RUN_REAL=1 requested but the real run did not complete"
  fi
  (cd "$WORK" && go test ./...)
fi

if [[ "$KEEP" == "1" ]]; then
  echo "evidence kept in $BASE"
else
  echo "all fake-provider assertions passed"
fi
cat "$BASE/cases.txt"
