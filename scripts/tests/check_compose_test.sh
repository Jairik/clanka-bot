#!/usr/bin/env bash
# Black-box tests for scripts/check-compose.sh.
#
# Each fixture is a tiny repo-shaped directory (`docker-compose.yml` plus
# optional service Dockerfiles) with the checker copied in, so the tests cover
# the conventions other tickets must follow: image tags, restarts, local-only
# health checks, the least-privilege block and the ML weights layer that must
# precede code COPYs. One test runs the checker against the real compose file.
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
	mkdir -p "$FIXTURE/assistant"
	printf 'FROM scratch\n' >"$FIXTURE/assistant/Dockerfile"
	cat >"$FIXTURE/docker-compose.yml" <<'YAML'
services:
  assistant:
    build: ./assistant
    image: clanka/assistant:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]
      interval: 10s
YAML
}

# The two services the privilege convention exempts on purpose.
write_privilege_exception_services() {
	mkdir -p "$FIXTURE/soloist" "$FIXTURE/dashboard"
	printf 'FROM scratch\n' >"$FIXTURE/soloist/Dockerfile"
	printf 'FROM scratch\n' >"$FIXTURE/dashboard/Dockerfile"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  soloist:
    build: ./soloist
    image: clanka/soloist:${GIT_SHA:-dev}
    restart: unless-stopped
    network_mode: host
    security_opt:
      - no-new-privileges:true
    healthcheck:
      test: ["CMD", "/usr/local/bin/healthcheck.sh"]
      interval: 10s
  dashboard-web:
    build: ./dashboard
    image: clanka/dashboard-web:${GIT_SHA:-dev}
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
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
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

test_accepts_prefetch_script_weights_layer() {
	new_fixture
	write_valid_assistant_compose
	write_tts_service
	mkdir -p "$FIXTURE/tts"
	cat >"$FIXTURE/tts/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
COPY requirements.txt .
RUN pip install -r requirements.txt
COPY prefetch.py .
RUN python prefetch.py
COPY pcm.py engine.py server.py ./
DOCKER
	run_check
	assert_status 0 "prefetch.py weights before code passes"
	assert_out_contains "checker reports success" "OK"
}

test_rejects_prefetch_script_after_code() {
	new_fixture
	write_valid_assistant_compose
	write_tts_service
	mkdir -p "$FIXTURE/tts"
	cat >"$FIXTURE/tts/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
COPY pcm.py engine.py server.py ./
COPY prefetch.py .
RUN python prefetch.py
DOCKER
	run_check
	assert_status 1 "prefetch.py after code fails"
	assert_out_contains "weights ordering failure is named" "weights"
}

test_accepts_sentence_transformer_weights_layer() {
	new_fixture
	write_valid_assistant_compose
	mkdir -p "$FIXTURE/embed"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  embed:
    build: ./embed
    image: clanka/embed:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8085/healthz')"]
YAML
	cat >"$FIXTURE/embed/Dockerfile" <<'DOCKER'
FROM python:3.12-slim
ENV HF_HOME=/models
RUN python -c "from sentence_transformers import SentenceTransformer; SentenceTransformer('all-MiniLM-L6-v2')"
COPY app.py /app/app.py
DOCKER
	run_check
	assert_status 0 "SentenceTransformer weights before code passes"
	assert_out_contains "checker reports success" "OK"
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

test_requires_build_context() {
	new_fixture
	write_valid_assistant_compose
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  wakeword:
    image: clanka/wakeword:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "python", "-c", "import socket; socket.create_connection(('127.0.0.1', 10400), 1)"]
YAML
	run_check
	assert_status 1 "an image without a local build fails"
	assert_out_contains "build context failure is named" "build context"
}

test_requires_dockerfile_in_build_context() {
	new_fixture
	write_valid_assistant_compose
	mkdir -p "$FIXTURE/stt"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  stt:
    build: ./stt
    image: clanka/stt:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "python", "-c", "import socket; socket.create_connection(('127.0.0.1', 10300), 1)"]
YAML
	run_check
	assert_status 1 "a build context without a Dockerfile fails"
	assert_out_contains "missing Dockerfile is named" "has no Dockerfile"
}

test_requires_cap_drop() {
	new_fixture
	write_valid_assistant_compose
	sed -i '/cap_drop:/,+1d' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "missing cap_drop fails"
	assert_out_contains "cap_drop failure is named" "cap_drop must include ALL"
}

test_requires_no_new_privileges() {
	new_fixture
	write_valid_assistant_compose
	sed -i '/security_opt:/,+1d' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "missing security_opt fails"
	assert_out_contains "no-new-privileges failure is named" "no-new-privileges:true"
}

test_requires_pids_limit() {
	new_fixture
	write_valid_assistant_compose
	sed -i '/pids_limit:/d' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "missing pids_limit fails"
	assert_out_contains "pids_limit failure is named" "pids_limit is required"
}

test_accepts_documented_privilege_exceptions() {
	new_fixture
	write_valid_assistant_compose
	write_privilege_exception_services
	run_check
	assert_status 0 "soloist and dashboard-web need no privilege block"
	assert_out_contains "checker reports success" "OK"
}

test_rejects_unclassified_service() {
	new_fixture
	write_valid_assistant_compose
	mkdir -p "$FIXTURE/metrics"
	printf 'FROM scratch\n' >"$FIXTURE/metrics/Dockerfile"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  metrics:
    build: ./metrics
    image: clanka/metrics:${GIT_SHA:-dev}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:9090/healthz"]
      interval: 10s
YAML
	run_check
	assert_status 1 "an unclassified service fails"
	assert_out_contains "classification failure names the lists" "HARDENED_SERVICES"
}

test_rejects_cap_drop_value() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^      - ALL$/      - NET_RAW/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "cap_drop with the wrong capability fails"
	assert_out_contains "cap_drop value is named" "cap_drop must include ALL"
}

test_rejects_cap_add_after_cap_drop() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^    cap_drop:$/    cap_add:\n      - NET_RAW\n    cap_drop:/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "cap_add defeats cap_drop ALL"
	assert_out_contains "cap_add failure is named" "cap_add is not allowed"
}

test_rejects_no_new_privileges_false() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^      - no-new-privileges:true$/      - no-new-privileges:false/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "no-new-privileges:false fails"
	assert_out_contains "the required value is named" "no-new-privileges:true"
}

test_rejects_overriding_security_opt_entry() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^      - no-new-privileges:true$/      - no-new-privileges:true\n      - no-new-privileges:false/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a later no-new-privileges override fails"
	assert_out_contains "the override is shown" "no override"
}

test_rejects_duplicate_security_opt_key() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^    cap_drop:$/    security_opt:\n      - no-new-privileges:false\n    cap_drop:/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a duplicate security_opt key fails"
	assert_out_contains "duplicate key is named" "security_opt must include no-new-privileges:true exactly once"
}

test_rejects_duplicate_cap_drop_key() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^    pids_limit: 256$/    cap_drop:\n      - NET_RAW\n    pids_limit: 256/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a duplicate cap_drop key fails"
	assert_out_contains "duplicate cap_drop is named" "cap_drop must include ALL"
}

test_rejects_privileged() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^    pids_limit: 256$/    privileged: true\n    pids_limit: 256/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "privileged:true defeats the capability drop"
	assert_out_contains "privileged failure is named" "privileged is not allowed"
}

test_rejects_pids_limit_values() {
	local value
	for value in 0 1 999999; do
		new_fixture
		write_valid_assistant_compose
		sed -i "s/^    pids_limit: 256$/    pids_limit: $value/" "$FIXTURE/docker-compose.yml"
		run_check
		assert_status 1 "pids_limit $value fails"
		assert_out_contains "pids_limit $value is named" "pids_limit must be 256, not '$value'"
	done
}

test_rejects_duplicate_pids_limit() {
	new_fixture
	write_valid_assistant_compose
	sed -i 's/^    pids_limit: 256$/    pids_limit: 256\n    pids_limit: 0/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a duplicate pids_limit key fails"
	assert_out_contains "duplicate pids_limit is named" "pids_limit must appear exactly once"
}

test_accepts_commented_privilege_keys() {
	new_fixture
	write_valid_assistant_compose
	sed -i \
		-e 's|^    build: \./assistant$|    build: ./assistant   # local context|' \
		-e 's/^    restart: unless-stopped$/    restart: unless-stopped   # deploy policy/' \
		-e 's/^    security_opt:$/    security_opt:   # no privilege gain/' \
		-e 's/^      - no-new-privileges:true$/      - no-new-privileges:true   # exec only/' \
		-e 's/^    cap_drop:$/    cap_drop:   # no capabilities/' \
		-e 's/^      - ALL$/      - ALL   # drop every capability/' \
		-e 's/^    pids_limit: 256$/    pids_limit: 256   # explicit/' \
		-e 's/^    healthcheck:$/    healthcheck:   # required/' \
		"$FIXTURE/docker-compose.yml"
	run_check
	assert_status 0 "commented keys and list items pass"
	assert_out_contains "checker reports success" "OK"
}

write_commented_web_search() {
	mkdir -p "$FIXTURE/searxng"
	printf 'FROM scratch\n' >"$FIXTURE/searxng/Dockerfile"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  web-search:  # internal search, added later
    build: ./searxng
    image: clanka/web-search:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "true"]
YAML
}

test_commented_service_key_is_enumerated() {
	new_fixture
	write_valid_assistant_compose
	write_commented_web_search
	# The commented service is the one missing cap_drop; a hidden service would pass.
	sed -i '/^  web-search:  #/,$ { /^    cap_drop:$/,+1d; }' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a commented service key is still checked"
	assert_out_contains "the commented service is named" "web-search: cap_drop must include ALL"
}

test_commented_service_key_cannot_lend_options() {
	new_fixture
	write_valid_assistant_compose
	write_commented_web_search
	# assistant lacks cap_drop; the next service must not supply it by merge.
	sed -i '/^  assistant:/,/^  web-search:/{/^    cap_drop:$/d;/^      - ALL$/d;}' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "assistant still fails without its own cap_drop"
	assert_out_contains "assistant did not absorb the next service" "assistant: cap_drop must include ALL"
}

test_rejects_unclassified_commented_service() {
	new_fixture
	write_valid_assistant_compose
	mkdir -p "$FIXTURE/metrics"
	printf 'FROM scratch\n' >"$FIXTURE/metrics/Dockerfile"
	cat >>"$FIXTURE/docker-compose.yml" <<'YAML'
  metrics:  # internal telemetry, added later
    build: ./metrics
    image: clanka/metrics:${GIT_SHA:-dev}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "true"]
YAML
	run_check
	assert_status 1 "an unclassified service with a commented key fails"
	assert_out_contains "classification failure names the lists" "metrics: add it to HARDENED_SERVICES"
}

test_list_parsing_stops_at_other_keys() {
	new_fixture
	write_valid_assistant_compose
	# Only security_opt is false; the only true entry belongs to cap_drop. A scan
	# that runs past a commented sibling key would credit it to security_opt.
	sed -i 's/^      - no-new-privileges:true$/      - no-new-privileges:false/' "$FIXTURE/docker-compose.yml"
	sed -i 's/^    cap_drop:$/    cap_drop:   # hardened/' "$FIXTURE/docker-compose.yml"
	sed -i 's/^      - ALL$/      - no-new-privileges:true/' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "a list item from the next key is not borrowed"
	assert_out_contains "security_opt stops at the commented cap_drop key" "security_opt must include no-new-privileges:true"
	assert_out_contains "cap_drop is still parsed" "cap_drop must include ALL"
}

test_requires_no_new_privileges_on_soloist() {
	new_fixture
	write_valid_assistant_compose
	write_privilege_exception_services
	sed -i '/^  soloist:/,/^  dashboard-web:/{/^    security_opt:$/d;/^      - no-new-privileges:true$/d;}' "$FIXTURE/docker-compose.yml"
	run_check
	assert_status 1 "soloist must keep its documented no-new-privileges"
	assert_out_contains "soloist is named" "soloist: security_opt must include no-new-privileges:true"
}

test_real_compose_file_passes() {
	OUT="$TMP/output.real"
	(cd "$REPO_ROOT" && bash scripts/check-compose.sh) >"$OUT" 2>&1
	STATUS=$?
	assert_status 0 "the checked-in compose file passes"
	assert_out_contains "the full eight-service stack is checked" "8 service(s)"
}

write_assistant_web_search_health_dep() {
	mkdir -p "$FIXTURE/assistant"
	printf 'FROM scratch\n' >"$FIXTURE/assistant/Dockerfile"
	cat >"$FIXTURE/docker-compose.yml" <<'YAML'
services:
  assistant:
    build: ./assistant
    image: clanka/assistant:${GIT_SHA:-dev}
    restart: unless-stopped
    security_opt:
      - no-new-privileges:true
    cap_drop:
      - ALL
    pids_limit: 256
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8080/healthz"]
      interval: 10s
    depends_on:
      web-search:
        condition: service_healthy
YAML
}

test_rejects_web_search_health_dependency() {
	new_fixture
	write_assistant_web_search_health_dep
	run_check
	assert_status 1 "assistant with a web-search health gate fails"
	assert_out_contains "the reintroduced web-search dependency is named" "web-search must not be a depends_on"
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
run_test test_accepts_prefetch_script_weights_layer
run_test test_rejects_prefetch_script_after_code
run_test test_accepts_sentence_transformer_weights_layer
run_test test_warns_when_no_weights_layer_is_detected
run_test test_requires_build_context
run_test test_requires_dockerfile_in_build_context
run_test test_requires_cap_drop
run_test test_requires_no_new_privileges
run_test test_requires_pids_limit
run_test test_accepts_documented_privilege_exceptions
run_test test_rejects_unclassified_service
run_test test_rejects_cap_drop_value
run_test test_rejects_cap_add_after_cap_drop
run_test test_rejects_no_new_privileges_false
run_test test_rejects_overriding_security_opt_entry
run_test test_rejects_duplicate_security_opt_key
run_test test_rejects_duplicate_cap_drop_key
run_test test_rejects_privileged
run_test test_rejects_pids_limit_values
run_test test_rejects_duplicate_pids_limit
run_test test_accepts_commented_privilege_keys
run_test test_commented_service_key_is_enumerated
run_test test_commented_service_key_cannot_lend_options
run_test test_rejects_unclassified_commented_service
run_test test_list_parsing_stops_at_other_keys
run_test test_requires_no_new_privileges_on_soloist
run_test test_real_compose_file_passes
run_test test_rejects_web_search_health_dependency

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
