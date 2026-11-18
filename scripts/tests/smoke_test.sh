#!/usr/bin/env bash
# Black-box tests for scripts/smoke.sh.
#
# Stubs curl on PATH and exercises all success and failure branches:
#   - skips with warning when owner is unset
#   - succeeds when endpoint returns valid fast_path + time.now JSON
#   - verifies Tailscale-User-Login and Sec-Fetch-Site headers are sent
#   - fails when route is agent_path (pipeline fall-through)
#   - fails when tool is not time.now
#   - fails when reply is empty
#   - fails on HTTP non-200 status
#   - fails on curl network error
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
SMOKE_SRC="$REPO_ROOT/scripts/smoke.sh"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/smoke-test.XXXXXX")"
STUB_BIN="$TMP/bin"
mkdir -p "$STUB_BIN"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""
SCENARIO=""

pass() { PASS=$((PASS + 1)); }

fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
	if [[ -f "$OUT" ]]; then
		printf -- '--- output ---\n'
		cat "$OUT"
		printf -- '--------------\n'
	fi
}

assert_status() {
	local want=$1 desc=$2
	if [[ "$STATUS" == "$want" ]]; then
		pass
	else
		fail "$desc: exit $STATUS, want $want"
	fi
}

assert_nonzero() {
	local desc=$1
	if [[ "$STATUS" -ne 0 ]]; then
		pass
	else
		fail "$desc: exit $STATUS, expected non-zero"
	fi
}

assert_out_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$OUT"; then
		pass
	else
		fail "$desc: output lacks '$needle'"
	fi
}

assert_out_not_contains() {
	local desc=$1 needle=$2
	if ! grep -qF -- "$needle" "$OUT"; then
		pass
	else
		fail "$desc: output unexpectedly contains '$needle'"
	fi
}

assert_calls_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$SCENARIO/curl_args"; then
		pass
	else
		fail "$desc: curl calls lack '$needle'"
	fi
}

write_curl_stub() {
	cat >"$STUB_BIN/curl" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" >>"${STUB_DIR:-.}/curl_args"

# Fail immediately if configured
if [[ -f "${STUB_DIR:-.}/curl_exit" ]]; then
	code="$(cat "${STUB_DIR:-.}/curl_exit")"
	echo "curl: (7) Failed to connect" >&2
	exit "$code"
fi

out_file=""
args=("$@")
for ((i=0; i<${#args[@]}; i++)); do
	if [[ "${args[i]}" == "-o" && $((i+1)) -lt ${#args[@]} ]]; then
		out_file="${args[i+1]}"
	fi
done

if [[ -n "$out_file" && -f "${STUB_DIR:-.}/response_body" ]]; then
	cat "${STUB_DIR:-.}/response_body" >"$out_file"
fi

http_code="${STUB_HTTP_CODE:-200}"
if [[ -f "${STUB_DIR:-.}/response_code" ]]; then
	http_code="$(cat "${STUB_DIR:-.}/response_code")"
fi
printf '%s' "$http_code"
STUB
	chmod +x "$STUB_BIN/curl"
}

write_curl_stub

new_scenario() {
	SCENARIO="$(mktemp -d "$TMP/scenario.XXXXXX")"
	: >"$SCENARIO/curl_args"
	echo "200" >"$SCENARIO/response_code"
	cat >"$SCENARIO/response_body" <<'JSON'
{"route":"fast_path","tool":"time.now","reply":"It's 3:04 PM.","stages":{"routing_ms":12,"tool_ms":2}}
JSON
}

run_smoke() {
	local env_file="${1:-$SCENARIO/.env}"
	shift || true
	OUT="$(mktemp "$TMP/out.XXXXXX")"
	STATUS=0
	(
		export PATH="$STUB_BIN:$PATH"
		export STUB_DIR="$SCENARIO"
		export SMOKE_ENV_FILE="$env_file"
		"$SMOKE_SRC" "$@"
	) >"$OUT" 2>&1 || STATUS=$?
}

test_skips_when_owner_is_empty() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=" >"$SCENARIO/.env"
	run_smoke "$SCENARIO/.env"
	assert_status 0 "smoke skips when owner empty"
	assert_out_contains "skipping warning is printed" "ASSISTANT_DASHBOARD_OWNER is empty"
	assert_out_contains "skipping is documented" "skipping smoke turn"
	if [[ -s "$SCENARIO/curl_args" ]]; then
		fail "curl was called when owner was empty"
	else
		pass
	fi
}

test_successful_smoke_turn() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	run_smoke "$SCENARIO/.env"
	assert_status 0 "successful smoke exits 0"
	assert_out_contains "reports fast_path" "route=fast_path"
	assert_out_contains "reports time.now" "tool=time.now"
	assert_out_contains "reports routing timing" "routing=12ms"
	assert_out_contains "reports tool timing" "tool=2ms"
	assert_out_contains "reports reply" "reply=It's 3:04 PM."
	assert_calls_contains "sets owner header" "Tailscale-User-Login: alice@example.com"
	assert_calls_contains "sets same-origin header" "Sec-Fetch-Site: same-origin"
}

# Live installations can give the smoke phrase an exact clock route.
test_clock_control_phrase_succeeds() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	cat >"$SCENARIO/response_body" <<'JSON'
{"route":"control_phrase","tool":"time.now","reply":"It's 3:04 PM."}
JSON
	run_smoke "$SCENARIO/.env"
	assert_status 0 "control phrase smoke exits 0"
	assert_out_contains "reports control_phrase" "route=control_phrase"
}

test_wrong_route_fails_script() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	cat >"$SCENARIO/response_body" <<'JSON'
{"route":"agent_path","tool":"","reply":"Agent fallback answer","stages":{"routing_ms":45,"tool_ms":0}}
JSON
	run_smoke "$SCENARIO/.env"
	assert_nonzero "wrong route fails"
	assert_out_contains "clear error message on wrong route" "expected a clock route (fast_path or control_phrase), got 'agent_path'"
	assert_out_contains "identifies embed may be stopped" "embed service may be stopped"
	assert_out_contains "includes response body" '{"route":"agent_path"'
}

test_wrong_tool_fails_script() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	cat >"$SCENARIO/response_body" <<'JSON'
{"route":"fast_path","tool":"other.tool","reply":"other answer","stages":{"routing_ms":10,"tool_ms":2}}
JSON
	run_smoke "$SCENARIO/.env"
	assert_nonzero "wrong tool fails"
	assert_out_contains "clear error message on wrong tool" "expected tool 'time.now', got 'other.tool'"
	assert_out_contains "identifies tool may be disabled" "tool may be disabled"
	assert_out_contains "includes response body" '{"route":"fast_path"'
}

test_empty_reply_fails_script() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	cat >"$SCENARIO/response_body" <<'JSON'
{"route":"fast_path","tool":"time.now","reply":"","stages":{"routing_ms":10,"tool_ms":2}}
JSON
	run_smoke "$SCENARIO/.env"
	assert_nonzero "empty reply fails"
	assert_out_contains "clear error message on empty reply" "reply is empty"
	assert_out_contains "includes response body" '{"route":"fast_path"'
}

test_http_status_failure() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	echo "403" >"$SCENARIO/response_code"
	cat >"$SCENARIO/response_body" <<'JSON'
{"error":"forbidden"}
JSON
	run_smoke "$SCENARIO/.env"
	assert_nonzero "HTTP 403 fails"
	assert_out_contains "reports HTTP status" "unexpected HTTP status 403"
	assert_out_contains "reports response body" '{"error":"forbidden"}'
}

test_curl_network_failure() {
	new_scenario
	echo "ASSISTANT_DASHBOARD_OWNER=alice@example.com" >"$SCENARIO/.env"
	echo "7" >"$SCENARIO/curl_exit"
	run_smoke "$SCENARIO/.env"
	assert_nonzero "curl failure fails"
	assert_out_contains "reports failure" "failed"
}

run_test() {
	local name=$1
	printf '== %s\n' "$name"
	"$name"
}

run_test test_skips_when_owner_is_empty
run_test test_successful_smoke_turn
run_test test_clock_control_phrase_succeeds
run_test test_wrong_route_fails_script
run_test test_wrong_tool_fails_script
run_test test_empty_reply_fails_script
run_test test_http_status_failure
run_test test_curl_network_failure

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
