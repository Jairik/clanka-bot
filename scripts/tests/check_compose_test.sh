#!/usr/bin/env bash
# Black-box tests for scripts/check-compose.sh.
#
# Each fixture is a tiny repo-shaped directory (`docker-compose.yml` plus
# optional service Dockerfiles) with the checker copied in, so the tests cover
# the conventions other tickets must follow: image tags, restarts, local-only
# health checks and the ML weights layer that must precede code COPYs.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
CHECK_SRC="$REPO_ROOT/scripts/check-compose.sh"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/check-compose-test.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""

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

new_fixture() {
	FIXTURE="$(mktemp -d "$TMP/fixture.XXXXXX")"
	mkdir -p "$FIXTURE/scripts"
	cp "$CHECK_SRC" "$FIXTURE/scripts/check-compose.sh"
	chmod +x "$FIXTURE/scripts/check-compose.sh"
}

write_valid_assistant_compose() {
	cat >"$FIXTURE/docker-compose.yml" <<'YAML'
services:
  assistant:
    image: clanka/assistant:${GIT_SHA:-dev}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]
      interval: 10s
YAML
}

run_check() {
	OUT="$TMP/output.$$"
	(cd "$FIXTURE" && bash scripts/check-compose.sh) >"$OUT" 2>&1
	STATUS=$?
}

assert_status() {
	local want=$1 desc=$2
	if [[ "$STATUS" == "$want" ]]; then pass; else fail "$desc: exit $STATUS, want $want"; fi
}

assert_out_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$OUT"; then pass; else fail "$desc: output lacks '$needle'"; fi
}

test_accepts_the_current_conventions() {
	new_fixture
	write_valid_assistant_compose
	run_check
	assert_status 0 "valid compose passes"
	assert_out_contains "checker reports success" "OK"
}

test_requires_sha_image_tag() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's|clanka/assistant:${GIT_SHA:-dev}|clanka/assistant:latest|' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "non-SHA image tag fails"
	assert_out_contains "image failure is named" "image must be clanka/assistant"
}

test_requires_restart_policy() {
	new_fixture
	write_valid_assistant_compose
	sed -i '/restart: unless-stopped/d' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "missing restart policy fails"
	assert_out_contains "restart failure is named" "restart"
}

test_requires_healthcheck() {
	new_fixture
	write_valid_assistant_compose
	sed -i '/healthcheck:/,+2d' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "missing healthcheck fails"
	assert_out_contains "healthcheck failure is named" "healthcheck"
}

test_rejects_internet_healthcheck() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's|http://127.0.0.1:8080/healthz|https://example.com/health|' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "internet healthcheck fails"
	assert_out_contains "external host is named" "example.com"
}

write_tts_service() {
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  tts:
    build:
      context: ./tts
    image: clanka/tts:${GIT_SHA:-dev}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "python", "-c", "import socket; socket.create_connection(('127.0.0.1', 10200), 1)"]
YAML
}

test_accepts_weights_before_code_copy() {
	new_fixture
	write_valid_assistant_compose
	write_tts_service
	mkdir -p "$FIXTURE/tts"
	cat >"$FIXTURE/tts/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
COPY requirements.txt /app/
RUN pip install -r /app/requirements.txt
RUN python -c "from huggingface_hub import snapshot_download; snapshot_download('kyutai/pocket-tts', local_dir='/models/tts')"
COPY app /app
DOCKER
	run_check
	assert_status 0 "weights before code passes"
	assert_out_contains "checker reports success" "OK"
}

test_rejects_code_copy_before_weights() {
	new_fixture
	write_valid_assistant_compose
	write_tts_service
	mkdir -p "$FIXTURE/tts"
	cat >"$FIXTURE/tts/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
COPY app /app
RUN python -c "from huggingface_hub import snapshot_download; snapshot_download('kyutai/pocket-tts', local_dir='/models/tts')"
DOCKER
	run_check
	assert_status 1 "code before weights fails"
	assert_out_contains "weights ordering failure is named" "weights"
}

test_warns_when_no_weights_layer_is_detected() {
	new_fixture
	write_valid_assistant_compose
	write_tts_service
	mkdir -p "$FIXTURE/tts"
	cat >"$FIXTURE/tts/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
COPY app /app
DOCKER
	run_check
	assert_status 0 "undetected weights only warns"
	assert_out_contains "warning asks for manual verification" "WARN"
}

test_accepts_ml_image_without_local_dockerfile() {
	new_fixture
	write_valid_assistant_compose
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  wakeword:
    image: clanka/wakeword:${GIT_SHA:-dev}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "python", "-c", "import socket; socket.create_connection(('127.0.0.1', 10400), 1)"]
YAML
	run_check
	assert_status 0 "an ML service without a local Dockerfile passes"
}

run_test() {
	printf '== %s\n' "$1"
	"$1"
}

run_test test_accepts_the_current_conventions
run_test test_requires_sha_image_tag
run_test test_requires_restart_policy
run_test test_requires_healthcheck
run_test test_rejects_internet_healthcheck
run_test test_accepts_weights_before_code_copy
run_test test_rejects_code_copy_before_weights
run_test test_warns_when_no_weights_layer_is_detected
run_test test_accepts_ml_image_without_local_dockerfile

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
