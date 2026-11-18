#!/usr/bin/env bash
# Black-box tests for scripts/deploy.sh.
#
# Docker is a test double on PATH; git is real. Each case gets a scratch repo
# (with a bare "origin" so `git pull --ff-only` is meaningful) and a scripted
# Docker state, so the tests exercise the deploy CLI's observable behaviour:
# refusals, tagging, build vs rollback, bounded health waits and diagnostics.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
DEPLOY_SRC="$REPO_ROOT/scripts/deploy.sh"
MAKE_SRC="$REPO_ROOT/Makefile"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/deploy-test.XXXXXX")"
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
		tail -n 25 "$OUT"
		printf -- '--------------\n'
	fi
}

write_docker_stub() {
	cat >"$STUB_BIN/docker" <<'STUB'
#!/usr/bin/env bash
# Scripted Docker CLI for the deploy tests.
set -uo pipefail
dir="${STUB_DOCKER_DIR:?STUB_DOCKER_DIR is required}"
printf 'docker %s\n' "$*" >>"$dir/calls"
printf 'GIT_SHA=%s\nDOCKER_DEFAULT_PLATFORM=%s\nCOMPOSE_PROJECT_NAME=%s\nCOMPOSE_PROFILES=%s\nCLANKA_TAG_ASSISTANT=%s\n' \
	"${GIT_SHA-}" "${DOCKER_DEFAULT_PLATFORM-}" "${COMPOSE_PROJECT_NAME-}" "${COMPOSE_PROFILES-}" "${CLANKA_TAG_ASSISTANT-}" >>"$dir/env"

cmd="${1:-}"
shift || true
case "$cmd" in
compose)
	# Global flags before the subcommand (image-keys.py passes --profile).
	while [[ "${1:-}" == --profile || "${1:-}" == --env-file ]]; do shift 2; done
	sub="${1:-}"
	shift || true
	case "$sub" in
	version) echo "Docker Compose version v2.test" ;;
	config)
		case "${1:-}" in
		--services) cat "$dir/services" ;;
		--images) cat "$dir/images" ;;
		--environment) cat "$dir/compose-environment" 2>/dev/null || true ;;
		--format) cat "$dir/config.json" 2>/dev/null || echo '{"services":{}}' ;;
		*) echo "services: {}" ;;
		esac
		;;
	ps)
		if [[ "${1:-}" == "-q" ]]; then
			svc="${2:-}"
			if grep -qxF "$svc" "$dir/containers" 2>/dev/null; then
				echo "cid-$svc"
			fi
		elif [[ "${1:-}" == "--services" ]]; then
			# Only running, defined services: `docker compose ps --services`
			# drops orphans (not in the current compose) and profiled
			# services that were not started because their profile is
			# inactive. Model that as the intersection of the container
			# set with the service set.
			grep -xF -f "$dir/services" "$dir/containers" 2>/dev/null || true
		else
			echo "stub compose ps"
		fi
		;;

	up)
		if [[ -f "$dir/up-fails" ]]; then
			echo "stub compose up failed" >&2
			exit 1
		fi
		echo "stub compose up ok"
		;;
	logs)
		if [[ "${1:-}" == --tail=* ]]; then shift; fi
		echo "stub logs for ${1:-unknown}"
		;;
	*) echo "stub compose $sub" ;;
	esac
	;;
inspect)
	for last; do :; done
	# Per-cid call counter so a fixture can flip a service's status after the
	# first poll (e.g. an assistant that becomes healthy once it has warmed up).
	cfile="$dir/inspect-count/$last"
	mkdir -p "$dir/inspect-count"
	cnt=$(cat "$cfile" 2>/dev/null || echo 0)
	cnt=$((cnt + 1))
	echo "$cnt" >"$cfile"
	if [[ -f "$dir/flip/$last" ]]; then
		after=$(cat "$dir/flip-after/$last" 2>/dev/null || echo 1)
		if ((cnt <= after)); then
			cat "$dir/flip/$last"
			exit 0
		fi
	fi
	if [[ -f "$dir/health/$last" ]]; then
		cat "$dir/health/$last"
	else
		cat "$dir/health/default" 2>/dev/null || echo starting
	fi
	;;
image)
	sub="${1:-}"
	shift || true
	case "$sub" in
	inspect)
		if [[ -f "$dir/images-missing" ]]; then
			echo "Error response from daemon: No such image" >&2
			exit 1
		fi
		# local-images, when present, lists the only refs on this host;
		# `docker tag` adds to it.
		if [[ -f "$dir/local-images" ]] && ! grep -qxF -- "${1:-}" "$dir/local-images"; then
			echo "Error response from daemon: No such image" >&2
			exit 1
		fi
		echo "[]"
		;;
	ls)
		if [[ -f "$dir/image-ls-fails" ]]; then
			echo "Error response from daemon: cannot list images" >&2
			exit 1
		fi
		if [[ -f "$dir/images-list" ]]; then
			cat "$dir/images-list"
		else
			printf 'clanka/assistant:%s|2026-01-01 00:00:00 +0000 UTC|0123456789ab\n' "${GIT_SHA:-dev}"
		fi
		;;
	rmi)
		if [[ -f "$dir/rmi-fails" ]] && grep -qxF -- "${1:-}" "$dir/rmi-fails"; then
			echo "Error response from daemon: conflict: image is being used by a container" >&2
			exit 1
		fi
		printf 'Untagged: %s\n' "${1:-}"
		;;
	*) echo "stub image $sub $*" ;;
	esac
	;;
ps)
	cat "$dir/running" 2>/dev/null || true
	;;
tag)
	[[ -f "$dir/local-images" ]] && printf '%s\n' "${2:-}" >>"$dir/local-images"
	echo "tagged"
	;;
pull)
	if [[ -f "$dir/pull-fails" ]]; then
		echo "Error response from daemon: manifest unknown" >&2
		exit 1
	fi
	echo "pulled"
	;;
system)
	if [[ "${1:-}" == "df" ]]; then
		echo "Images 1GB"
	else
		echo "stub system $*"
	fi
	;;
builder)
	echo "stub builder $*"
	;;
*)
	echo "stub docker $cmd $*"
	;;
esac
STUB
	chmod +x "$STUB_BIN/docker"
}

write_docker_stub

new_scenario() {
	SCENARIO="$(mktemp -d "$TMP/scenario.XXXXXX")"
	mkdir -p "$SCENARIO/health"
	set_services assistant
	printf 'clanka/assistant:test\n' >"$SCENARIO/images"
	# The receiver setup must never reach the network in a test. Pre-create
	# its binary and key in the scenario directory.
	mkdir -p "$SCENARIO/soloist"
	printf '#!/bin/sh\nexit 0\n' >"$SCENARIO/soloist/soloist"
	chmod +x "$SCENARIO/soloist/soloist"
	printf 'test-key\n' >"$SCENARIO/soloist/api-key"
	chmod 600 "$SCENARIO/soloist/api-key"
	echo healthy >"$SCENARIO/health/default"
	# The fixture repo's assistant builds from ./assistant (see new_repo).
	printf '{"services":{"assistant":{"build":{"context":"assistant","dockerfile":"Dockerfile"}}}}\n' >"$SCENARIO/config.json"
	: >"$SCENARIO/calls"
	: >"$SCENARIO/env"
}

set_services() {
	printf '%s\n' "$@" >"$SCENARIO/services"
	printf '%s\n' "$@" >"$SCENARIO/containers"
}

set_health() {
	local svc=$1 status=$2 started=${3:-}
	if [[ -n "$started" ]]; then
		echo "$status $started" >"$SCENARIO/health/cid-$svc"
	else
		echo "$status" >"$SCENARIO/health/cid-$svc"
	fi
}

# Return `status startedAt` from `flip/cid-$svc` for the first `after` inspect
# calls (default 1), then fall through to the normal health file. Used to model a
# container that is still warming up on the first poll but healthy on the next.
set_health_flip() {
	local svc=$1 status=$2 started=$3 after=${4:-1}
	mkdir -p "$SCENARIO/flip" "$SCENARIO/flip-after"
	echo "$status $started" >"$SCENARIO/flip/cid-$svc"
	echo "$after" >"$SCENARIO/flip-after/cid-$svc"
}

git_config() {
	git -C "$1" config user.email "deploy-test@example.com"
	git -C "$1" config user.name "deploy test"
}

new_repo() {
	local repo remote
	repo="$(mktemp -d "$TMP/repo.XXXXXX")"
	mkdir -p "$repo/scripts"
	cp "$DEPLOY_SRC" "$repo/scripts/deploy.sh"
	cp "$MAKE_SRC" "$repo/Makefile" 2>/dev/null || true
	cp "$REPO_ROOT/scripts/soloist-setup.sh" "$repo/scripts/soloist-setup.sh"
	cp "$REPO_ROOT/scripts/prune-images.sh" "$repo/scripts/prune-images.sh"
	cp "$REPO_ROOT/scripts/image-keys.py" "$repo/scripts/image-keys.py"
	cp "$REPO_ROOT/scripts/smoke.sh" "$repo/scripts/smoke.sh"
	mkdir -p "$repo/assistant"
	echo 'FROM scratch' >"$repo/assistant/Dockerfile"
	chmod +x "$repo/scripts/deploy.sh" "$repo/scripts/soloist-setup.sh" "$repo/scripts/prune-images.sh" "$repo/scripts/smoke.sh"
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

run_deploy() {
	local repo=$1
	shift
	OUT="$TMP/output.$$"
	(
		cd "$repo" || exit 99
		STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" \
			DEPLOY_HEALTH_TIMEOUT="${DEPLOY_HEALTH_TIMEOUT:-5}" \
			DEPLOY_POLL_INTERVAL="${DEPLOY_POLL_INTERVAL:-0.1}" \
			DEPLOY_COLD_START_GRACE="${DEPLOY_COLD_START_GRACE:-150}" \
			SOLOIST_DIR="$SCENARIO/soloist" \
			SOLOIST_BINARY="$SCENARIO/soloist/soloist" \
			SOLOIST_LEGACY_DIR="$SCENARIO/legacy" \
			bash scripts/deploy.sh "$@"
	) >"$OUT" 2>&1
	STATUS=$?
}

run_make() {
	local repo=$1 target=$2
	OUT="$TMP/output.$$"
	(
		cd "$repo" || exit 99
		# The outer `make test` exports GIT_SHA for its own repo; the fixture
		# Makefile must derive its own SHA.
		unset GIT_SHA DOCKER_DEFAULT_PLATFORM
		STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" \
			SOLOIST_DIR="$SCENARIO/soloist" \
			SOLOIST_BINARY="$SCENARIO/soloist/soloist" \
			SOLOIST_LEGACY_DIR="$SCENARIO/legacy" \
			make "$target"
	) >"$OUT" 2>&1
	STATUS=$?
}

assert_status() {
	local want=$1 desc=$2
	if [[ "$STATUS" == "$want" ]]; then pass; else fail "$desc: exit $STATUS, want $want"; fi
}

assert_nonzero() {
	local desc=$1
	if [[ "$STATUS" -ne 0 ]]; then pass; else fail "$desc: expected a non-zero exit, got 0"; fi
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

assert_calls_not_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$SCENARIO/calls"; then fail "$desc: docker calls unexpectedly contain '$needle'"; else pass; fi
}

assert_env_contains() {
	local desc=$1 needle=$2
	if grep -qxF -- "$needle" "$SCENARIO/env"; then pass; else fail "$desc: docker env lacks '$needle'"; fi
}

assert_calls_order() {
	local desc=$1 first=$2 second=$3 first_line second_line
	first_line="$(grep -nF -- "$first" "$SCENARIO/calls" | head -n 1 | cut -d: -f1 || true)"
	second_line="$(grep -nF -- "$second" "$SCENARIO/calls" | head -n 1 | cut -d: -f1 || true)"
	if [[ -n "$first_line" && -n "$second_line" ]] && ((first_line < second_line)); then
		pass
	else
		fail "$desc: '$first' (line ${first_line:-none}) is not before '$second' (line ${second_line:-none})"
	fi
}

assert_elapsed_under() {
	local desc=$1 limit=$2 start=$3
	local elapsed=$(($(date +%s) - start))
	if ((elapsed < limit)); then pass; else fail "$desc: took ${elapsed}s, want under ${limit}s"; fi
}

test_help() {
	local repo
	repo=$(new_repo)
	new_scenario
	run_deploy "$repo" --help
	assert_status 0 "help exits zero"
	assert_out_contains "help prints usage" "Usage: scripts/deploy.sh"
	assert_calls_not_contains "help does not touch docker" "docker compose up"
}

test_refuses_dirty_worktree() {
	local repo
	repo=$(new_repo)
	new_scenario
	echo dirty >>"$repo/tracked.txt"
	run_deploy "$repo"
	assert_nonzero "dirty worktree is refused"
	assert_out_contains "dirty worktree message" "local changes"
	assert_calls_not_contains "dirty worktree never reaches compose up" "docker compose up"
}

test_deploys_current_branch() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	run_deploy "$repo"
	assert_status 0 "clean deploy succeeds"
	assert_out_contains "deploy reports healthy services" "healthy"
	assert_out_contains "deploy pulls before building" "Already up to date"
	assert_calls_contains "deploy starts the stack from prepared images" "docker compose up -d --no-build --remove-orphans"
	assert_env_contains "images are tagged with the short SHA" "GIT_SHA=$sha"
	if [[ "$(git -C "$repo" rev-parse --abbrev-ref HEAD)" == "main" ]]; then pass; else fail "deploy leaves the tracked branch checked out"; fi
}

test_deploys_behind_upstream() {
	local repo other remote sha
	repo=$(new_repo)
	remote=$(git -C "$repo" remote get-url origin)
	other="$(mktemp -d "$TMP/other.XXXXXX")"
	git clone -q "$remote" "$other"
	git_config "$other"
	echo remote >"$other/remote.txt"
	git -C "$other" add -A
	git -C "$other" commit -qm remote
	git -C "$other" push -q origin main
	sha=$(git -C "$other" rev-parse --short HEAD)
	new_scenario
	run_deploy "$repo"
	assert_status 0 "behind branch deploys"
	assert_out_contains "deploy fast-forwards the branch" "Fast-forward"
	assert_env_contains "pulled images are tagged with the new SHA" "GIT_SHA=$sha"
	if [[ "$(git -C "$repo" rev-parse HEAD)" == "$(git -C "$other" rev-parse HEAD)" ]]; then pass; else fail "deploy reaches the new upstream"; fi
}

test_waits_for_every_service() {
	local repo
	repo=$(new_repo)
	new_scenario
	set_services assistant tts
	set_health assistant healthy
	set_health tts healthy
	run_deploy "$repo"
	assert_status 0 "multi-service deploy succeeds"
	assert_out_contains "deploy waits for all services" "all 2 service"
	assert_calls_contains "deploy checks the second service" "docker compose ps -q tts"
}

test_provisions_the_receiver_before_compose_up() {
	local repo
	repo=$(new_repo)
	new_scenario
	set_services assistant soloist
	set_health assistant healthy
	set_health soloist healthy
	run_deploy "$repo"
	assert_status 0 "deploy with a receiver succeeds"
	assert_out_contains "deploy prepares the receiver" "using existing receiver binary"
	assert_calls_contains "receiver setup runs before compose up" "docker compose up -d --no-build --remove-orphans"
}

test_deploy_without_spotify_key_skips_receiver() {
	local repo
	repo=$(new_repo)
	new_scenario
	# The checked-in compose defines soloist behind the spotify profile, but
	# with no key the profile is inactive: config --services still names it,
	# yet ps --services (after up) must not.
	set_services assistant soloist
	printf 'assistant\n' >"$SCENARIO/containers"
	rm -f "$SCENARIO/soloist/api-key"
	run_deploy "$repo"
	assert_status 0 "deploy without a Spotify key succeeds"
	assert_out_not_contains "deploy never provisions the receiver" "using existing receiver binary"
	assert_calls_not_contains "deploy never health-checks the profiled service" "docker compose ps -q soloist"
	assert_out_contains "deploy waits only for the running service" "all 1 service"
}

test_deploy_with_spotify_key_enables_profile() {
	local repo
	repo=$(new_repo)
	new_scenario
	# Key present (new_scenario) -> profile active, receiver provisioned, and
	# ps --services includes soloist after up.
	set_services assistant soloist
	set_health assistant healthy
	set_health soloist healthy
	run_deploy "$repo"
	assert_status 0 "deploy with a Spotify key succeeds"
	assert_env_contains "deploy exports the spotify profile" "COMPOSE_PROFILES=spotify"
	assert_out_contains "deploy provisions the receiver" "using existing receiver binary"
	assert_calls_contains "deploy health-checks the receiver" "docker compose ps -q soloist"
	assert_out_contains "deploy waits for both services" "all 2 service"
}

test_receiver_preserves_selected_profiles() {
	local repo
	repo=$(new_repo)
	new_scenario
	set_services assistant soloist hermes
	set_health assistant healthy
	set_health soloist healthy
	set_health hermes healthy
	printf 'COMPOSE_PROFILES=hermes\n' >"$SCENARIO/compose-environment"
	run_deploy "$repo"
	assert_status 0 "deploy with Hermes and Spotify succeeds"
	assert_env_contains "deploy preserves Hermes when enabling Spotify" "COMPOSE_PROFILES=hermes,spotify"
}

test_make_up_runs_optional_setup_without_key() {
	local repo
	repo=$(new_repo)
	new_scenario
	rm -f "$SCENARIO/soloist/api-key"
	run_make "$repo" up
	assert_status 0 "make up succeeds with no Spotify key"
	assert_out_contains "make up skips the receiver" "Spotify receiver not configured; skipping"
	assert_calls_not_contains "make up does not force the profile" "docker compose up -d --profile"
}

test_rollback_removes_orphans_from_a_newer_service_set() {
	local repo first short
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	echo two >"$repo/second.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm second
	short=$(git -C "$repo" rev-parse --short "$first")
	new_scenario
	# The host still has the newer three-service set; the rollback commit
	# defines only assistant. compose ps can still see the old containers.
	set_services assistant
	printf 'assistant\ntts\nembed\n' >"$SCENARIO/containers"
	printf 'clanka/assistant:%s\n' "$short" >"$SCENARIO/images"
	run_deploy "$repo" "$first"
	assert_status 0 "rollback with orphan containers succeeds"
	assert_calls_contains "rollback removes newer services" "docker compose up -d --no-build --remove-orphans"
	assert_out_contains "rollback waits only for its own service set" "all 1 service"
	assert_calls_contains "rollback still checks its service" "docker compose ps -q assistant"
}

test_reports_timeout_with_diagnostics() {
	local repo start
	repo=$(new_repo)
	new_scenario
	echo starting >"$SCENARIO/health/default"
	start=$(date +%s)
	DEPLOY_HEALTH_TIMEOUT=1 run_deploy "$repo"
	assert_nonzero "unhealthy wait exits non-zero"
	assert_out_contains "timeout is reported" "did not pass"
	assert_out_contains "diagnostics include compose ps" "stub compose ps"
	assert_out_contains "diagnostics include service logs" "stub logs for assistant"
	assert_elapsed_under "health wait is bounded" 8 "$start"
}

test_reports_unhealthy_service() {
	local repo start old
	repo=$(new_repo)
	new_scenario
	# Started two hours ago, so even the cold-start grace has long expired:
	# the deploy must still fail immediately with the existing diagnostics.
	old="$(date -u -d '2 hours ago' +%Y-%m-%dT%H:%M:%SZ)"
	echo "unhealthy $old" >"$SCENARIO/health/default"
	start=$(date +%s)
	DEPLOY_HEALTH_TIMEOUT=30 run_deploy "$repo"
	assert_nonzero "unhealthy service fails the deploy"
	assert_out_contains "failure names the unhealthy state" "unhealthy"
	assert_out_contains "failure dumps the failing service logs" "stub logs for assistant"
	assert_elapsed_under "unhealthy state is not waited out" 8 "$start"
}

test_cold_start_grace_then_healthy() {
	local repo start recent
	repo=$(new_repo)
	new_scenario
	# The assistant started 5s ago and is briefly unhealthy while it warms up
	# (loads settings, probes TTS, warms the voice, starts Tools): that is
	# inside DEPLOY_COLD_START_GRACE, so wait_for_health must keep polling
	# rather than fail. The fixture flips it to healthy on the second poll.
	recent="$(date -u -d '5 seconds ago' +%Y-%m-%dT%H:%M:%SZ)"
	set_health assistant healthy "$recent"
	set_health_flip assistant unhealthy "$recent"
	start=$(date +%s)
	DEPLOY_HEALTH_TIMEOUT=30 run_deploy "$repo"
	assert_status 0 "unhealthy within the cold-start grace then healthy succeeds"
	assert_out_contains "cold-start grace keeps waiting" "still waiting"
	assert_out_contains "deploy reports all services healthy" "all 1 service"
	assert_elapsed_under "deploy completes after the flip" 30 "$start"
}

test_crash_loop_fails_immediately() {
	local repo start recent
	repo=$(new_repo)
	new_scenario
	# A crash-loop reports 'exited'/'dead' regardless of how recently it
	# started; wait_for_health must never treat that as a slow cold start.
	recent="$(date -u -d '2 seconds ago' +%Y-%m-%dT%H:%M:%SZ)"
	echo "exited $recent" >"$SCENARIO/health/cid-assistant"
	start=$(date +%s)
	DEPLOY_HEALTH_TIMEOUT=30 run_deploy "$repo"
	assert_nonzero "a crashed container fails the deploy immediately"
	assert_out_contains "failure names the crash state" "exited"
	assert_elapsed_under "crash-loop is not waited out" 8 "$start"
}

test_warns_when_pulse_socket_missing() {
	local repo
	repo=$(new_repo)
	new_scenario
	# Point XDG_RUNTIME_DIR at a directory with no Pulse socket so the preflight
	# warns. The deploy must still succeed: a deaf assistant is worse than a
	# failed deploy, and /healthz does not need Pulse.
	XDG_RUNTIME_DIR="$SCENARIO/no-pulse" run_deploy "$repo"
	assert_status 0 "missing Pulse socket only warns, the deploy still succeeds"
	assert_out_contains "preflight warns about the missing Pulse socket" "Pulse native socket not found"
}

test_rollback_reuses_images_without_build() {
	local repo first short
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	echo two >"$repo/second.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm second
	short=$(git -C "$repo" rev-parse --short "$first")
	new_scenario
	printf 'clanka/assistant:%s\n' "$short" >"$SCENARIO/images"
	run_deploy "$repo" "$first"
	assert_status 0 "rollback succeeds"
	assert_out_contains "rollback says it reuses images" "reusing"
	assert_calls_contains "rollback brings the stack up without building" "docker compose up -d --no-build --remove-orphans"
	assert_calls_not_contains "rollback never builds" "compose build"
	assert_calls_not_contains "rollback never pulls" "docker pull"
	assert_env_contains "rollback tags with the rollback SHA" "GIT_SHA=$short"
	if [[ "$(git -C "$repo" rev-parse HEAD)" == "$first" ]]; then pass; else fail "rollback checks out the requested commit"; fi
	if [[ "$(cat "$repo/.git/deployed-sha")" == "$first" ]]; then pass; else fail "rollback records its full SHA"; fi
}

test_rollback_refuses_missing_image() {
	local repo first
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	new_scenario
	: >"$SCENARIO/images-missing"
	run_deploy "$repo" "$first"
	assert_nonzero "rollback without an image is refused"
	assert_out_contains "missing image is named" "missing image"
	assert_calls_not_contains "missing image never starts containers" "docker compose up"
}

test_rollback_refuses_unknown_commit() {
	local repo
	repo=$(new_repo)
	new_scenario
	run_deploy "$repo" doesnotexist
	assert_nonzero "unknown commit is refused"
	assert_out_contains "unknown commit is named" "unknown commit"
	assert_calls_not_contains "unknown commit never starts containers" "docker compose up"
}

test_rollback_refuses_dirty_worktree() {
	local repo first before
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	before=$(git -C "$repo" rev-parse HEAD)
	new_scenario
	echo dirty >>"$repo/tracked.txt"
	run_deploy "$repo" "$first"
	assert_nonzero "dirty rollback is refused"
	assert_out_contains "dirty rollback message" "local changes"
	if [[ "$(git -C "$repo" rev-parse HEAD)" == "$before" ]]; then pass; else fail "dirty rollback leaves HEAD alone"; fi
	assert_calls_not_contains "dirty rollback never starts containers" "docker compose up"
}

test_default_deploy_recovers_from_detached_rollback() {
	local repo first tip
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	echo two >"$repo/second.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm second
	tip=$(git -C "$repo" rev-parse HEAD)
	# deploy.sh refuses a branch ahead of its upstream; this case is about
	# returning from detached HEAD, so keep main pushed.
	git -C "$repo" push -q origin main
	new_scenario
	run_deploy "$repo" "$first" || true
	run_deploy "$repo"
	assert_status 0 "deploy after a rollback succeeds"
	if [[ "$(git -C "$repo" rev-parse --abbrev-ref HEAD)" == "main" ]]; then pass; else fail "deploy returns from detached HEAD to the branch"; fi
	if [[ "$(git -C "$repo" rev-parse HEAD)" == "$tip" ]]; then pass; else fail "deploy moves the branch back to its tip"; fi
}

test_refuses_non_fast_forward_pull() {
	local repo other remote
	repo=$(new_repo)
	remote=$(git -C "$repo" remote get-url origin)
	other="$(mktemp -d "$TMP/other.XXXXXX")"
	git clone -q "$remote" "$other"
	git_config "$other"
	echo remote >"$other/remote.txt"
	git -C "$other" add -A
	git -C "$other" commit -qm remote
	git -C "$other" push -q origin main
	echo local >"$repo/local.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm local
	new_scenario
	run_deploy "$repo"
	assert_nonzero "diverged pull is refused"
	assert_out_contains "fast-forward refusal is visible" "fast-forward"
	assert_calls_not_contains "diverged pull never starts containers" "docker compose up"
}

test_refuses_unpushed_local_commit() {
	local repo
	repo=$(new_repo)
	echo local >"$repo/local.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm local
	new_scenario
	run_deploy "$repo"
	assert_nonzero "unpushed local commit is refused"
	assert_out_contains "refusal names the upstream" "not on origin/main"
	assert_out_contains "refusal says what to do" "push or reset first"
	assert_calls_not_contains "unpushed commit never builds" "docker compose up"
}

test_deploys_pushed_feature_branch() {
	local repo sha
	repo=$(new_repo)
	git -C "$repo" checkout -q -b feature
	git -C "$repo" push -q -u origin feature
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	run_deploy "$repo"
	assert_status 0 "pushed feature branch deploys"
	assert_calls_contains "feature branch deploys the stack" "docker compose up -d --no-build --remove-orphans"
	assert_env_contains "feature branch tags its own SHA" "GIT_SHA=$sha"
}

test_make_build_arm64() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	run_make "$repo" build-arm64
	assert_status 0 "make build-arm64 succeeds"
	assert_calls_contains "make build-arm64 builds every compose image" "docker compose build"
	assert_env_contains "make build-arm64 targets arm64" "DOCKER_DEFAULT_PLATFORM=linux/arm64"
	assert_env_contains "make build-arm64 tags with the SHA" "GIT_SHA=$sha"
}

test_make_prune_images() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	run_make "$repo" prune-images
	assert_status 0 "make prune-images succeeds"
	assert_calls_contains "make prune-images reads the image inventory" "docker image ls"
	assert_calls_not_contains "make prune-images keeps the deployed set" "docker image rmi clanka/assistant:$sha"
}

test_prunes_old_images_after_health() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	printf 'clanka/assistant|%s|aaaaaaaaaaaa|2026-01-03 00:00:00 +0000 UTC\n' "$sha" >"$SCENARIO/images-list"
	printf 'clanka/assistant|abcdef1|bbbbbbbbbbbb|2026-01-01 00:00:00 +0000 UTC\n' >>"$SCENARIO/images-list"
	DEPLOY_KEEP_IMAGES=1 run_deploy "$repo"
	assert_status 0 "a deploy that prunes succeeds"
	assert_calls_contains "an old SHA tag is removed" "docker image rmi clanka/assistant:abcdef1"
	assert_calls_not_contains "the deployed SHA tag is kept" "docker image rmi clanka/assistant:$sha"
	assert_calls_order "pruning runs after the health wait" "docker compose ps -q assistant" "docker image rmi clanka/assistant:abcdef1"
	assert_out_contains "the deploy reports the removal" "removed clanka/assistant:abcdef1"
	assert_out_contains "the deploy still reports success" "deployed $sha"
}

test_prune_rmi_failure_does_not_fail_deploy() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	printf 'clanka/assistant|%s|aaaaaaaaaaaa|2026-01-03 00:00:00 +0000 UTC\n' "$sha" >"$SCENARIO/images-list"
	printf 'clanka/assistant|abcdef2|bbbbbbbbbbbb|2026-01-01 00:00:00 +0000 UTC\n' >>"$SCENARIO/images-list"
	printf 'clanka/assistant:abcdef2\n' >"$SCENARIO/rmi-fails"
	DEPLOY_KEEP_IMAGES=1 run_deploy "$repo"
	assert_status 0 "a failed rmi leaves the deploy green"
	assert_out_contains "the rmi failure is a warning" "could not remove clanka/assistant:abcdef2"
	assert_out_contains "the deploy still reports success" "deployed $sha"
}

test_prune_script_failure_does_not_fail_deploy() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	: >"$SCENARIO/image-ls-fails"
	run_deploy "$repo"
	assert_status 0 "an inventory failure leaves the deploy green"
	assert_out_contains "the deploy names the tolerated prune failure" "image pruning failed; the deployment is still healthy"
	assert_out_contains "the deploy still reports success" "deployed $sha"
}

test_does_not_prune_when_health_fails() {
	local repo
	repo=$(new_repo)
	new_scenario
	echo unhealthy >"$SCENARIO/health/default"
	DEPLOY_HEALTH_TIMEOUT=30 run_deploy "$repo"
	assert_nonzero "a failed health wait still fails the deploy"
	assert_calls_not_contains "a failed deploy does not list images" "docker image ls"
	assert_calls_not_contains "a failed deploy does not remove images" "docker image rmi"
}

test_refuses_non_writable_exports() {
	local repo
	repo=$(new_repo)
	new_scenario
	# A previous `docker compose up` can leave ./exports root-owned; mkdir -p
	# will not repair it, so deploy must fail with the exact remediation
	# before any build runs. Root can write regardless of mode, so skip there.
	if [[ "$(id -u)" == "0" ]]; then
		skip "running as root: a non-writable exports cannot be created"
		return
	fi
	mkdir -p "$repo/exports"
	chmod 0555 "$repo/exports"
	run_deploy "$repo"
	assert_nonzero "non-writable exports is refused"
	assert_out_contains "non-writable exports names the chown fix" "sudo chown -R $(id -u):$(id -g) exports"
}

test_deploy_uses_published_images() {
	local repo sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	: >"$SCENARIO/local-images"
	run_deploy "$repo"
	assert_status 0 "deploy with a published image succeeds"
	assert_calls_contains "deploy pulls the image by its content key" "docker pull --quiet ghcr.io/jairik/clanka-bot/assistant:key-"
	assert_calls_contains "the pulled image runs under its key tag" "docker tag ghcr.io/jairik/clanka-bot/assistant:key-"
	assert_calls_contains "the pulled image also gets the deploy SHA tag" "clanka/assistant:$sha"
	if grep -q '^CLANKA_TAG_ASSISTANT=key-[0-9a-f]\{24\}$' "$SCENARIO/env"; then pass; else fail "compose runs the key tag"; fi
	assert_calls_not_contains "a published image is never rebuilt" "compose build"
	assert_calls_order "images are ready before compose up" "docker pull" "docker compose up"
}

test_deploy_builds_an_unpublished_image() {
	local repo
	repo=$(new_repo)
	new_scenario
	: >"$SCENARIO/local-images"
	touch "$SCENARIO/pull-fails"
	run_deploy "$repo"
	assert_status 0 "a missing published image still deploys"
	assert_out_contains "deploy says why it builds" "no published assistant image"
	assert_calls_contains "deploy builds only the missing service" "docker compose build assistant"
	assert_calls_order "the build happens before compose up" "docker compose build" "docker compose up"
}

test_deploy_without_registry_builds_everything() {
	local repo
	repo=$(new_repo)
	new_scenario
	: >"$SCENARIO/local-images"
	DEPLOY_REGISTRY="" run_deploy "$repo"
	assert_status 0 "registry-less deploy succeeds"
	assert_calls_not_contains "no registry means no pull" "docker pull"
	assert_calls_contains "every service builds on the host" "docker compose build assistant"
}

test_deploy_reuses_a_local_key_image() {
	local repo key sha
	repo=$(new_repo)
	sha=$(git -C "$repo" rev-parse --short HEAD)
	new_scenario
	key="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py | awk '{print $2}')"
	printf 'clanka/assistant:key-%s\n' "$key" >"$SCENARIO/local-images"
	run_deploy "$repo"
	assert_status 0 "deploy with the image already on the host succeeds"
	assert_out_contains "deploy says it reuses the local image" "reusing local assistant image"
	assert_calls_not_contains "an image already on the host is not pulled" "docker pull"
	assert_calls_not_contains "an image already on the host is not built" "compose build"
	assert_calls_contains "the reused image is still recorded under the SHA" "docker tag clanka/assistant:key-$key clanka/assistant:$sha"
	assert_env_contains "compose runs the unchanged key tag" "CLANKA_TAG_ASSISTANT=key-$key"
}

test_rollback_restores_a_pruned_key_tag() {
	local repo first short
	repo=$(new_repo)
	first=$(git -C "$repo" rev-parse HEAD)
	short=$(git -C "$repo" rev-parse --short "$first")
	echo two >"$repo/second.txt"
	git -C "$repo" add -A
	git -C "$repo" commit -qm second
	new_scenario
	printf 'clanka/assistant:%s\n' "$short" >"$SCENARIO/local-images"
	printf 'clanka/assistant:%s\n' "$short" >"$SCENARIO/images"
	run_deploy "$repo" "$first"
	assert_status 0 "rollback with only the SHA ledger tag succeeds"
	assert_calls_contains "rollback re-creates the key tag from the SHA tag" "docker tag clanka/assistant:$short clanka/assistant:key-"
	assert_calls_not_contains "rollback never pulls" "docker pull"
}

test_same_commit_and_args_give_the_same_key() {
	local repo first second third
	repo=$(new_repo)
	new_scenario
	first="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py)"
	echo unrelated >"$repo/tracked.txt"
	git -C "$repo" commit -qam "outside the build context"
	second="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py)"
	echo 'FROM busybox' >"$repo/assistant/Dockerfile"
	git -C "$repo" commit -qam "inside the build context"
	third="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py)"
	if [[ "$first" == assistant\ * && "$first" == "$second" ]]; then pass; else fail "a change outside the context keeps the key: '$first' vs '$second'"; fi
	if [[ "$second" != "$third" ]]; then pass; else fail "a change inside the context must change the key"; fi
}

# A UI-only source change must keep the assistant's running image tag.
test_dashboard_change_only_updates_its_image_key() {
	local repo before after
	repo=$(new_repo)
	new_scenario
	mkdir -p "$repo/assistant/web"
	echo 'FROM scratch' >"$repo/assistant/web/Dockerfile.web"
	echo 'old dashboard' >"$repo/assistant/web/index.html"
	git -C "$repo" add assistant/web
	git -C "$repo" commit -qm "initial dashboard"
	printf '%s\n' '{"services":{"assistant":{"build":{"context":"assistant","dockerfile":"Dockerfile"}},"dashboard-web":{"build":{"context":"assistant","dockerfile":"web/Dockerfile.web"}}}}' >"$SCENARIO/config.json"
	before="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py)"
	echo 'new dashboard' >"$repo/assistant/web/index.html"
	git -C "$repo" commit -qam "dashboard-only change"
	after="$(cd "$repo" && STUB_DOCKER_DIR="$SCENARIO" PATH="$STUB_BIN:$PATH" python3 -I scripts/image-keys.py)"
	[[ "$(printf '%s\n' "$before" | grep '^assistant ')" == "$(printf '%s\n' "$after" | grep '^assistant ')" ]] &&
		pass || fail "a web-only file changed the assistant image key"
	[[ "$(printf '%s\n' "$before" | grep '^dashboard-web ')" != "$(printf '%s\n' "$after" | grep '^dashboard-web ')" ]] &&
		pass || fail "a web-only file did not change the dashboard image key"
}

test_deploy_runs_smoke_turn() {
	local repo
	repo=$(new_repo)
	new_scenario
	cat >"$repo/scripts/smoke.sh" <<'EOF'
#!/bin/sh
echo "smoke: ok route=fast_path tool=time.now routing=10ms tool=2ms reply=test"
exit 0
EOF
	chmod +x "$repo/scripts/smoke.sh"
	git -C "$repo" commit -qam "smoke stub"
	git -C "$repo" push -q origin main
	run_deploy "$repo"
	assert_status 0 "deploy with successful smoke succeeds"
	assert_out_contains "smoke turn ran" "running post-deploy smoke turn"
	assert_out_contains "smoke turn output printed" "smoke: ok route=fast_path"
}

test_deploy_smoke_failure_fails_deploy() {
	local repo
	repo=$(new_repo)
	new_scenario
	cat >"$repo/scripts/smoke.sh" <<'EOF'
#!/bin/sh
echo "smoke: error: pipeline fell through" >&2
echo "smoke: response body: {\"route\":\"agent_path\"}" >&2
exit 1
EOF
	chmod +x "$repo/scripts/smoke.sh"
	git -C "$repo" commit -qam "failing smoke stub"
	git -C "$repo" push -q origin main
	run_deploy "$repo"
	assert_nonzero "deploy with failed smoke fails"
	assert_out_contains "smoke failure is reported" "post-deploy smoke turn failed"
	[[ ! -e "$repo/.git/deployed-sha" ]] && pass || fail "failed smoke must not record a deployed SHA"
	assert_out_contains "response body is in log" "smoke: response body: {\"route\":\"agent_path\"}"
}

test_deploy_smoke_disabled_skips() {
	local repo
	repo=$(new_repo)
	new_scenario
	cat >"$repo/scripts/smoke.sh" <<'EOF'
#!/bin/sh
echo "should not be called" >&2
exit 1
EOF
	chmod +x "$repo/scripts/smoke.sh"
	git -C "$repo" commit -qam "broken smoke stub"
	git -C "$repo" push -q origin main
	DEPLOY_SMOKE=0 run_deploy "$repo"
	assert_status 0 "deploy with DEPLOY_SMOKE=0 succeeds"
	assert_out_not_contains "smoke turn did not run" "running post-deploy smoke turn"
}

# A partial or colliding backup must never be published as a successful copy.
test_backup_is_private_and_atomic() {
	local repo out
	repo=$(new_repo)
	new_scenario
	mkdir "$SCENARIO/bin"
	cat >"$SCENARIO/bin/curl" <<'EOF'
#!/usr/bin/env bash
# curl's final argument is its output file.
printf '%s' "${BACKUP_BODY:?}" >"${!#}"
exit "${BACKUP_STATUS:-0}"
EOF
	cat >"$SCENARIO/bin/date" <<'EOF'
#!/usr/bin/env bash
# Use a fixed name to exercise same-second backups.
echo 20261117-010203
EOF
	chmod +x "$SCENARIO/bin/"{curl,date}
	out="$repo/backups/assistant-20261117-010203.db"
	OUT="$SCENARIO/backup-output"
	(
		cd "$repo" || exit
		umask 022
		export PATH="$SCENARIO/bin:$PATH" OWNER=owner@example.test
		BACKUP_BODY=partial BACKUP_STATUS=18 make backup && exit 1
		[[ -z "$(find backups -mindepth 1 -print -quit)" ]] || exit 1
		BACKUP_BODY=complete make backup || exit 1
		[[ "$(cat "$out")" == complete && "$(stat -c '%a' "$out")" == 600 ]] || exit 1
		if BACKUP_BODY=duplicate make backup; then
			exit 1
		fi
		[[ "$(cat "$out")" == complete ]]
	) >"$OUT" 2>&1
	STATUS=$?
	assert_status 0 "backup publishes only a complete mode-0600 file"
	[[ "$(grep -c '^Wrote ' "$OUT")" == 1 ]] && pass || fail "only the complete backup reports success"
}

run_test() {
	local name=$1
	printf '== %s\n' "$name"
	"$name"
}

run_test test_help
run_test test_refuses_dirty_worktree
run_test test_deploys_current_branch
run_test test_deploys_behind_upstream
run_test test_waits_for_every_service
run_test test_provisions_the_receiver_before_compose_up
run_test test_deploy_without_spotify_key_skips_receiver
run_test test_deploy_with_spotify_key_enables_profile
run_test test_receiver_preserves_selected_profiles
run_test test_make_up_runs_optional_setup_without_key
run_test test_rollback_removes_orphans_from_a_newer_service_set
run_test test_reports_timeout_with_diagnostics
run_test test_reports_unhealthy_service
run_test test_cold_start_grace_then_healthy
run_test test_crash_loop_fails_immediately
run_test test_warns_when_pulse_socket_missing
run_test test_rollback_reuses_images_without_build
run_test test_rollback_refuses_missing_image
run_test test_rollback_refuses_unknown_commit
run_test test_rollback_refuses_dirty_worktree
run_test test_default_deploy_recovers_from_detached_rollback
run_test test_refuses_non_fast_forward_pull
run_test test_refuses_unpushed_local_commit
run_test test_refuses_non_writable_exports
run_test test_deploys_pushed_feature_branch
run_test test_make_build_arm64
run_test test_make_prune_images
run_test test_prunes_old_images_after_health
run_test test_prune_rmi_failure_does_not_fail_deploy
run_test test_prune_script_failure_does_not_fail_deploy
run_test test_does_not_prune_when_health_fails
run_test test_deploy_uses_published_images
run_test test_deploy_builds_an_unpublished_image
run_test test_deploy_without_registry_builds_everything
run_test test_deploy_reuses_a_local_key_image
run_test test_rollback_restores_a_pruned_key_tag
run_test test_same_commit_and_args_give_the_same_key
run_test test_dashboard_change_only_updates_its_image_key
run_test test_deploy_runs_smoke_turn
run_test test_deploy_smoke_failure_fails_deploy
run_test test_deploy_smoke_disabled_skips
run_test test_backup_is_private_and_atomic

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
