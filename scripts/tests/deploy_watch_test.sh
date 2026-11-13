#!/usr/bin/env bash
# Black-box tests for scripts/deploy-watch.sh.
#
# No network and no Docker: `docker` and `curl` are scripted doubles on PATH,
# while git is real, with a bare "origin" so fetches and HEAD comparisons mean
# something. The CI gate reads a JSON fixture written by each case.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
WATCH_SRC="$REPO_ROOT/scripts/deploy-watch.sh"
DEPLOY_SRC="$REPO_ROOT/scripts/deploy.sh"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/deploy-watch-test.XXXXXX")"
STUB_BIN="$TMP/bin"
mkdir -p "$STUB_BIN"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""
SCENARIO=""
NEW_SHA=""

pass() { PASS=$((PASS + 1)); }

fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
	if [[ -f "$OUT" ]]; then
		printf -- '--- output ---\n'
		tail -n 25 "$OUT"
		printf -- '--------------\n'
	fi
}

# Just enough Docker CLI for deploy.sh: version probe, service list, ps -q,
# inspect for health and compose up. Every call is recorded.
write_docker_stub() {
	cat >"$STUB_BIN/docker" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_DOCKER_DIR:?STUB_DOCKER_DIR is required}"
printf 'docker %s\n' "$*" >>"$dir/calls"
printf 'GIT_SHA=%s\n' "${GIT_SHA-}" >>"$dir/env"

cmd="${1:-}"
shift || true
case "$cmd" in
compose)
	sub="${1:-}"
	shift || true
	case "$sub" in
	version) echo "Docker Compose version v2.test" ;;
	config)
		case "${1:-}" in
		--services) cat "$dir/services" ;;
		--images) cat "$dir/images" 2>/dev/null || true ;;
		*) echo "services: {}" ;;
		esac
		;;
	ps)
		if [[ "${1:-}" == "-q" ]]; then
			if grep -qxF "${2:-}" "$dir/containers" 2>/dev/null; then
				echo "cid-${2:-}"
			fi
		else
			echo "stub compose ps"
		fi
		;;
	up)
		if [[ -f "$dir/up-fail" ]]; then
			echo "stub compose up failed" >&2
			exit 1
		fi
		echo "stub compose up ok"
		;;
	logs) echo "stub logs" ;;
	*) echo "stub compose $sub" ;;
	esac
	;;
inspect) echo healthy ;;
*) echo "stub docker $cmd $*" ;;
esac
STUB
	chmod +x "$STUB_BIN/docker"
}

# Serves a per-scenario JSON body, or fails like curl does for a non-2xx.
write_curl_stub() {
	cat >"$STUB_BIN/curl" <<'STUB'
#!/usr/bin/env bash
set -uo pipefail
dir="${STUB_CURL_DIR:?STUB_CURL_DIR is required}"
printf 'curl %s\n' "$*" >>"$dir/curl-calls"
if [[ -f "$dir/curl-fail" ]]; then
	echo "curl: (22) The requested URL returned error: 503" >&2
	exit 22
fi
cat "$dir/curl-body" 2>/dev/null || printf '{"total_count":0,"check_runs":[]}\n'
STUB
	chmod +x "$STUB_BIN/curl"
}

write_docker_stub
write_curl_stub

new_scenario() {
	SCENARIO="$(mktemp -d "$TMP/scenario.XXXXXX")"
	printf 'assistant\n' >"$SCENARIO/services"
	printf 'assistant\n' >"$SCENARIO/containers"
	printf 'clanka/assistant:test\n' >"$SCENARIO/images"
	: >"$SCENARIO/calls"
	: >"$SCENARIO/env"
	: >"$SCENARIO/curl-calls"
	set_ci_green
}

set_ci_green() {
	printf '{"total_count":1,"check_runs":[{"status":"completed","conclusion":"success"}]}\n' \
		>"$SCENARIO/curl-body"
}

set_ci_pending() {
	printf '{"total_count":1,"check_runs":[{"status":"in_progress","conclusion":null}]}\n' \
		>"$SCENARIO/curl-body"
}

set_ci_red() {
	printf '{"total_count":2,"check_runs":[{"status":"completed","conclusion":"success"},{"status":"completed","conclusion":"failure"}]}\n' \
		>"$SCENARIO/curl-body"
}

set_ci_none() {
	printf '{"total_count":0,"check_runs":[]}\n' >"$SCENARIO/curl-body"
}

git_config() {
	git -C "$1" config user.email "deploy-watch-test@example.com"
	git -C "$1" config user.name "deploy watch test"
}

new_repo() {
	local repo remote
	repo="$(mktemp -d "$TMP/repo.XXXXXX")"
	mkdir -p "$repo/scripts"
	cp "$WATCH_SRC" "$repo/scripts/deploy-watch.sh"
	cp "$DEPLOY_SRC" "$repo/scripts/deploy.sh"
	cp "$REPO_ROOT/scripts/soloist-setup.sh" "$repo/scripts/soloist-setup.sh"
	chmod +x "$repo/scripts/deploy-watch.sh" "$repo/scripts/deploy.sh"
	printf 'exports/\n' >"$repo/.gitignore"
	cat >"$repo/docker-compose.yml" <<'YAML'
services:
  assistant:
    image: clanka/assistant:${GIT_SHA:-dev}
YAML
	git -C "$repo" init -q -b main
	git_config "$repo"
	echo one >"$repo/tracked.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm "initial"
	remote="$(mktemp -d "$TMP/remote.XXXXXX")"
	git init -q --bare -b main "$remote"
	git -C "$repo" remote add origin "$remote"
	git -C "$repo" push -q -u origin main
	echo "$repo"
}

# Land a new commit on origin/main (via a second clone) and record its full SHA
# in NEW_SHA. The local checkout deliberately stays behind.
advance_origin() {
	local repo=$1 remote other
	remote="$(git -C "$repo" remote get-url origin)"
	other="$(mktemp -d "$TMP/other.XXXXXX")"
	git clone -q "$remote" "$other"
	git_config "$other"
	echo "advance-$RANDOM-$$" >"$other/advance-$(date +%s%N).txt"
	git -C "$other" add -A
	git -C "$other" commit -qm "advance origin"
	git -C "$other" push -q origin main
	NEW_SHA="$(git -C "$other" rev-parse HEAD)"
}

run_watch() {
	local repo=$1
	shift
	OUT="$TMP/output.$$"
	(
		cd "$repo" || exit 99
		STUB_DOCKER_DIR="$SCENARIO" \
			STUB_CURL_DIR="$SCENARIO" \
			PATH="$STUB_BIN:$PATH" \
			DEPLOY_WATCH_LOCK="$SCENARIO/watch.lock" \
			DEPLOY_WATCH_API="http://api.test" \
			DEPLOY_WATCH_REPO="example/clanka-bot" \
			DEPLOY_HEALTH_TIMEOUT="${DEPLOY_HEALTH_TIMEOUT:-5}" \
			DEPLOY_POLL_INTERVAL="0.1" \
			bash scripts/deploy-watch.sh "$@"
	) >"$OUT" 2>&1
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

assert_out_not_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$OUT"; then fail "$desc: output unexpectedly contains '$needle'"; else pass; fi
}

assert_calls_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$SCENARIO/calls"; then pass; else fail "$desc: docker calls lack '$needle'"; fi
}

assert_calls_empty() {
	local desc=$1
	if [[ ! -s "$SCENARIO/calls" ]]; then pass; else fail "$desc: docker was called: $(tr '\n' ';' <"$SCENARIO/calls")"; fi
}

assert_env_contains() {
	local desc=$1 needle=$2
	if grep -qxF -- "$needle" "$SCENARIO/env"; then pass; else fail "$desc: docker env lacks '$needle'"; fi
}

assert_curl_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$SCENARIO/curl-calls"; then pass; else fail "$desc: curl calls lack '$needle'"; fi
}

test_help() {
	local repo
	repo=$(new_repo)
	new_scenario
	run_watch "$repo" --help
	assert_status 0 "help exits zero"
	assert_out_contains "help prints usage" "Usage: scripts/deploy-watch.sh"
	assert_calls_empty "help does not touch docker"
}

test_unknown_flag() {
	local repo
	repo=$(new_repo)
	new_scenario
	run_watch "$repo" --nonsense
	assert_status 2 "unknown flag exits two"
	assert_calls_empty "unknown flag does not touch docker"
}

test_no_new_commit() {
	local repo
	repo=$(new_repo)
	new_scenario
	run_watch "$repo"
	assert_status 0 "up-to-date poll exits zero"
	assert_out_contains "up-to-date poll says so" "already on"
	assert_calls_empty "up-to-date poll never reaches docker"
}

test_ci_green_deploys() {
	local repo short
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	short="$(git -C "$repo" rev-parse --short "$NEW_SHA")"
	run_watch "$repo"
	assert_status 0 "green deploy succeeds"
	assert_out_contains "green CI is reported" "CI is green for $short"
	assert_curl_contains "CI gate queries the new commit" "commits/$NEW_SHA/check-runs"
	assert_calls_contains "green deploy builds the stack" "docker compose up -d --build --remove-orphans"
	assert_env_contains "images are tagged with the new SHA" "GIT_SHA=$short"
	if [[ "$(git -C "$repo" rev-parse HEAD)" == "$NEW_SHA" ]]; then pass; else fail "deploy moves HEAD to origin/main"; fi
}

test_ci_failed_deploy_is_reported() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	: >"$SCENARIO/up-fail"
	run_watch "$repo"
	if [[ "$STATUS" != 0 ]]; then pass; else fail "failed deploy must exit non-zero, got $STATUS"; fi
	assert_out_contains "failed deploy names the sha" "deploy of"
	assert_out_contains "failed deploy says it failed" "failed"
	assert_out_not_contains "failed deploy is not reported as success" "deployed "
}

test_ci_pending_waits() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	set_ci_pending
	run_watch "$repo"
	assert_status 0 "pending CI is not an error"
	assert_out_contains "pending CI is logged" "still running"
	assert_calls_empty "pending CI never deploys"
}

test_ci_red_does_not_deploy() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	set_ci_red
	run_watch "$repo"
	assert_status 0 "red CI is not an error"
	assert_out_contains "red CI is logged" "CI is red"
	assert_calls_empty "red CI never replaces the running stack"
}

test_ci_disabled_deploys() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	set_ci_red
	DEPLOY_WATCH_CI=0 run_watch "$repo"
	assert_status 0 "deploy with CI gating off succeeds"
	assert_calls_contains "CI gating off still deploys" "docker compose up -d --build --remove-orphans"
	assert_out_not_contains "CI gating off skips the gate" "CI is red"
}

test_ci_unreachable_still_deploys() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	: >"$SCENARIO/curl-fail"
	run_watch "$repo"
	assert_status 0 "unreachable CI API is not fatal"
	assert_out_contains "unreachable CI API warns" "CI status request failed"
	assert_calls_contains "unreachable CI API still deploys" "docker compose up -d --build --remove-orphans"
}

test_ci_malformed_payload_still_deploys() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	printf 'not json at all\n' >"$SCENARIO/curl-body"
	run_watch "$repo"
	assert_status 0 "malformed CI payload is not fatal"
	assert_out_contains "malformed CI payload warns" "could not parse"
	assert_calls_contains "malformed CI payload still deploys" "docker compose up -d --build --remove-orphans"
}

test_no_ci_checks_warns_and_deploys() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	set_ci_none
	run_watch "$repo"
	assert_status 0 "no CI checks still deploys"
	assert_out_contains "no CI checks warns" "no CI checks reported"
	assert_calls_contains "no CI checks deploys anyway" "docker compose up -d --build --remove-orphans"
}

test_dirty_tree_is_left_alone() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	echo dirty >>"$repo/tracked.txt"
	run_watch "$repo"
	assert_status 0 "dirty tree is not an error"
	assert_out_contains "dirty tree is explained" "local changes"
	assert_calls_empty "dirty tree never deploys"
}

test_dry_run_changes_nothing() {
	local repo
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	run_watch "$repo" --dry-run
	assert_status 0 "dry run exits zero"
	assert_out_contains "dry run says what it would do" "would deploy"
	assert_calls_empty "dry run never deploys"
	if [[ "$(git -C "$repo" rev-parse HEAD)" != "$NEW_SHA" ]]; then pass; else fail "dry run leaves HEAD alone"; fi
}

test_lock_held_skips() {
	local repo holder
	repo=$(new_repo)
	new_scenario
	advance_origin "$repo"
	(
		exec 8>"$SCENARIO/watch.lock"
		flock 8
		sleep 10
	) &
	holder=$!
	local waited=0
	while ((waited < 100)); do
		if ! flock -n "$SCENARIO/watch.lock" true 2>/dev/null; then break; fi
		sleep 0.05
		waited=$((waited + 1))
	done
	run_watch "$repo"
	assert_status 0 "busy lock is not an error"
	assert_out_contains "busy lock is logged" "another run is in progress"
	assert_calls_empty "busy lock never deploys"
	kill "$holder" 2>/dev/null || true
	wait "$holder" 2>/dev/null || true
}

test_force_deploys_when_current() {
	local repo sha
	repo=$(new_repo)
	new_scenario
	sha="$(git -C "$repo" rev-parse --short HEAD)"
	run_watch "$repo" --force
	assert_status 0 "forced deploy succeeds"
	assert_calls_contains "forced deploy rebuilds the stack" "docker compose up -d --build --remove-orphans"
	assert_env_contains "forced deploy tags the current SHA" "GIT_SHA=$sha"
	assert_out_contains "forced deploy names the SHA" "deploying $sha"
}

run_test() {
	local name=$1
	printf '== %s\n' "$name"
	"$name"
}

run_test test_help
run_test test_unknown_flag
run_test test_no_new_commit
run_test test_ci_green_deploys
run_test test_ci_failed_deploy_is_reported
run_test test_ci_pending_waits
run_test test_ci_red_does_not_deploy
run_test test_ci_disabled_deploys
run_test test_ci_unreachable_still_deploys
run_test test_ci_malformed_payload_still_deploys
run_test test_no_ci_checks_warns_and_deploys
run_test test_dirty_tree_is_left_alone
run_test test_dry_run_changes_nothing
run_test test_lock_held_skips
run_test test_force_deploys_when_current

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
