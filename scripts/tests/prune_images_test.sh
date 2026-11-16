#!/usr/bin/env bash
# Black-box tests for scripts/prune-images.sh.
#
# Docker is a test double on PATH; the inventory, running-container list, rmi
# results and build-cache call are all scripted. No case touches a real Docker
# daemon, so no host image or build cache can be removed by this suite.
set -uo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$TESTS_DIR/../.." && pwd)"
PRUNE_SRC="$REPO_ROOT/scripts/prune-images.sh"

TMP_BASE="${TMPDIR:-/tmp/opencode}"
mkdir -p "$TMP_BASE"
TMP="$(mktemp -d "$TMP_BASE/prune-images-test.XXXXXX")"
STUB_BIN="$TMP/bin"
mkdir -p "$STUB_BIN"
trap 'rm -rf "$TMP"' EXIT

PASS=0
FAIL=0
STATUS=0
OUT=""
CASE=""
GIT_SHA_TEST=""

pass() { PASS=$((PASS + 1)); }

fail() {
	FAIL=$((FAIL + 1))
	printf 'FAIL: %s\n' "$*"
	if [[ -f "$OUT" ]]; then
		printf -- '--- output ---\n'
		tail -n 30 "$OUT"
		printf -- '--------------\n'
	fi
}

write_docker_stub() {
	cat >"$STUB_BIN/docker" <<'STUB'
#!/usr/bin/env bash
# Scripted Docker CLI for the prune tests.
set -uo pipefail
dir="${STUB_DOCKER_DIR:?STUB_DOCKER_DIR is required}"
printf 'docker %s\n' "$*" >>"$dir/calls"

cmd="${1:-}"
shift || true
case "$cmd" in
image)
	sub="${1:-}"
	shift || true
	case "$sub" in
	ls)
		if [[ -f "$dir/image-ls-fails" ]]; then
			echo "Error response from daemon: cannot list images" >&2
			exit 1
		fi
		cat "$dir/images" 2>/dev/null || true
		;;
	rmi)
		ref="${1:-}"
		if [[ -f "$dir/rmi-fails" ]] && grep -qxF -- "$ref" "$dir/rmi-fails"; then
			echo "Error response from daemon: conflict: image is being used by a container" >&2
			exit 1
		fi
		printf 'Untagged: %s\n' "$ref"
		;;
	*)
		echo "stub image $sub $*"
		;;
	esac
	;;
ps)
	if [[ -f "$dir/ps-fails" ]]; then
		echo "Error response from daemon: cannot list containers" >&2
		exit 1
	fi
	cat "$dir/running" 2>/dev/null || true
	;;
system)
	if [[ "${1:-}" == "df" ]]; then
		if [[ -f "$dir/df-fails" ]]; then
			echo "Error response from daemon: cannot read disk usage" >&2
			exit 1
		fi
		printf 'TYPE\tTOTAL\tACTIVE\tSIZE\tRECLAIMABLE\nImages\t10GB\t5GB\t8GB\t3GB\n'
	else
		echo "stub system $*"
	fi
	;;
builder)
	printf '%s\n' "$*" >>"$dir/builder"
	if [[ -f "$dir/builder-fails" ]]; then
		echo "Error response from daemon: builder prune failed" >&2
		exit 1
	fi
	printf 'Total:\t0B\n'
	;;
*)
	echo "stub docker $cmd $*"
	;;
esac
STUB
	chmod +x "$STUB_BIN/docker"
}

write_docker_stub

new_case() {
	CASE="$(mktemp -d "$TMP/case.XXXXXX")"
	: >"$CASE/images"
	: >"$CASE/running"
	: >"$CASE/calls"
	: >"$CASE/builder"
	unset DEPLOY_KEEP_IMAGES DEPLOY_PINNED_SHAS DEPLOY_PRUNE_BUILD_CACHE DEPLOY_BUILD_CACHE_AGE GIT_SHA
	GIT_SHA_TEST=""
}

add_image() {
	printf '%s|%s|%s|%s\n' "$1" "$2" "$3" "$4" >>"$CASE/images"
}

set_running() {
	printf '%s\n' "$@" >"$CASE/running"
}

fail_rmi() {
	printf '%s\n' "$@" >"$CASE/rmi-fails"
}

run_prune() {
	OUT="$TMP/output.$$"
	(
		cd "$CASE" || exit 99
		if [[ -n "$GIT_SHA_TEST" ]]; then
			export GIT_SHA="$GIT_SHA_TEST"
		else
			unset GIT_SHA
		fi
		PATH="$STUB_BIN:$PATH" STUB_DOCKER_DIR="$CASE" bash "$PRUNE_SRC" "$@"
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

assert_out_count() {
	local desc=$1 needle=$2 want=$3 got
	got="$(grep -cF -- "$needle" "$OUT" 2>/dev/null || true)"
	[[ -n "$got" ]] || got=0
	if [[ "$got" == "$want" ]]; then pass; else fail "$desc: found $got '$needle', want $want"; fi
}

assert_calls_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$CASE/calls"; then pass; else fail "$desc: docker calls lack '$needle'"; fi
}

assert_calls_not_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$CASE/calls"; then fail "$desc: docker calls unexpectedly contain '$needle'"; else pass; fi
}

assert_builder_contains() {
	local desc=$1 needle=$2
	if grep -qF -- "$needle" "$CASE/builder"; then pass; else fail "$desc: builder calls lack '$needle'"; fi
}

assert_builder_empty() {
	local desc=$1
	if [[ -s "$CASE/builder" ]]; then
		fail "$desc: builder was called"
	else
		pass
	fi
}

test_help() {
	new_case
	run_prune --help
	assert_status 0 "help exits zero"
	assert_out_contains "help prints usage" "Usage: scripts/prune-images.sh"
	assert_calls_not_contains "help does not touch docker" "docker"
}

test_rejects_unknown_argument() {
	new_case
	run_prune --force
	assert_nonzero "an unknown argument is refused"
	assert_out_contains "unknown argument prints usage" "Usage: scripts/prune-images.sh"
	assert_calls_not_contains "unknown argument does not touch docker" "docker"
}

test_retention_keeps_deployed_and_newest_sets() {
	new_case
	GIT_SHA_TEST=bbbbbbb
	export DEPLOY_KEEP_IMAGES=2
	add_image clanka/assistant ccccccc aaaaaaaaaaaa "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/tts ccccccc aaaaaaaaaaab "2026-11-01 10:01:00 +0000 UTC"
	add_image clanka/assistant bbbbbbb bbbbbbbbbbbb "2026-11-02 10:00:00 +0000 UTC"
	add_image clanka/assistant aaaaaaa cccccccccccc "2026-11-03 10:00:00 +0000 UTC"
	add_image clanka/tts 9999999 dddddddddddd "2026-11-04 10:00:00 +0000 UTC"
	add_image clanka/assistant dev eeeeeeeeeeee "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/tts arm64-2026-11-05 ffffffffffff "2026-11-05 10:01:00 +0000 UTC"
	add_image searxng/searxng 2026.11.12-de722ab1b 111111111111 "2026-11-05 10:02:00 +0000 UTC"
	add_image docker.io/library/redis latest 222222222222 "2026-11-05 10:03:00 +0000 UTC"
	run_prune
	assert_status 0 "a normal prune succeeds"
	assert_calls_not_contains "deployed assistant tag is kept" "docker image rmi clanka/assistant:bbbbbbb"
	assert_calls_not_contains "deployed tts tag is kept" "docker image rmi clanka/tts:bbbbbbb"
	assert_calls_not_contains "newest set survives" "docker image rmi clanka/assistant:aaaaaaa"
	assert_calls_not_contains "second newest set survives" "docker image rmi clanka/tts:9999999"
	assert_calls_contains "oldest assistant tag is removed" "docker image rmi clanka/assistant:ccccccc"
	assert_calls_contains "oldest tts tag is removed" "docker image rmi clanka/tts:ccccccc"
	assert_out_contains "removal is printed" "removed clanka/assistant:ccccccc"
	assert_out_contains "deployed set is named" "keeping bbbbbbb (deployed)"
	assert_out_contains "newest retention is named" "keeping 9999999 (newest)"
	assert_out_contains "second newest retention is named" "keeping aaaaaaa (newest)"
	assert_calls_not_contains "the dev tag is never touched" ":dev"
	assert_calls_not_contains "an arm64 tag is never touched" "arm64-"
	assert_calls_not_contains "an unrelated repository is never touched" "searxng"
	assert_calls_not_contains "another unrelated repository is never touched" "redis"
}

test_current_sha_can_be_a_full_sha_and_newest() {
	new_case
	GIT_SHA_TEST=7777777777777777777777777777777777777777
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant 6666666 1234567890ab "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/assistant 7777777 abcdef123456 "2026-11-02 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "a full GIT_SHA matches its short tag"
	assert_out_contains "the full SHA is kept through its short tag" "keeping 7777777 (deployed)"
	assert_calls_not_contains "the deployed set survives KEEP=1" "docker image rmi clanka/assistant:7777777"
	assert_calls_contains "KEEP=1 removes every older set" "docker image rmi clanka/assistant:6666666"
}

test_zero_keep_retains_only_the_deployed_set() {
	new_case
	GIT_SHA_TEST=bbbbbbb
	export DEPLOY_KEEP_IMAGES=0
	add_image clanka/assistant bbbbbbb 1234567890ab "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc abcdef123456 "2026-11-02 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "KEEP=0 is accepted"
	assert_calls_not_contains "the deployed set survives KEEP=0" "docker image rmi clanka/assistant:bbbbbbb"
	assert_calls_contains "KEEP=0 drops every newer set too" "docker image rmi clanka/assistant:ccccccc"
}

test_keeps_a_whole_set_used_by_a_running_container() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 100000000001 "2026-11-05 10:00:00 +0000 UTC"
	# A running tag protects its whole set, not only the one running service.
	add_image clanka/assistant ddddddd 100000000002 "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/tts ddddddd 100000000003 "2026-11-01 10:01:00 +0000 UTC"
	# A digest-style running reference protects the image identity too.
	add_image clanka/assistant eeeeeee abcdef123456 "2026-11-02 10:00:00 +0000 UTC"
	add_image clanka/tts eeeeeee 100000000004 "2026-11-02 10:01:00 +0000 UTC"
	set_running "clanka/tts:ddddddd" \
		"sha256:abcdef123456abcdef123456abcdef123456abcdef123456abcdef123456abcd"
	run_prune
	assert_status 0 "a running set does not fail the prune"
	assert_out_contains "the running tag is reported" "keeping clanka/tts:ddddddd (used by a running container)"
	assert_out_contains "the running set is kept whole" "keeping ddddddd (running)"
	assert_out_contains "the image identity is kept too" "keeping eeeeeee (running)"
	assert_calls_not_contains "the running assistant tag is kept" "docker image rmi clanka/assistant:ddddddd"
	assert_calls_not_contains "the running tts tag is kept" "docker image rmi clanka/tts:ddddddd"
	assert_calls_not_contains "the running assistant image is kept" "docker image rmi clanka/assistant:eeeeeee"
	assert_calls_not_contains "the running tts image is kept" "docker image rmi clanka/tts:eeeeeee"
}

test_failed_rmi_only_warns() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 200000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 200000000002 "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/tts ccccccc 200000000003 "2026-11-01 10:01:00 +0000 UTC"
	fail_rmi clanka/assistant:ccccccc
	run_prune
	assert_status 0 "a failed rmi does not fail the prune"
	assert_out_contains "the failed tag is named" "could not remove clanka/assistant:ccccccc"
	assert_out_contains "the failure is a warning" "prune: warning"
	assert_calls_contains "the other tag is still attempted" "docker image rmi clanka/tts:ccccccc"
	assert_out_contains "the successful tag is removed" "removed clanka/tts:ccccccc"
	assert_out_contains "the summary counts the failure" "1 tag(s) could not be removed"
}

test_invalid_retention_and_pin_input_is_refused() {
	new_case
	add_image clanka/assistant ffffff0 300000000001 "2026-11-05 10:00:00 +0000 UTC"
	export DEPLOY_KEEP_IMAGES=abc
	run_prune
	assert_nonzero "a non-numeric KEEP is refused"
	assert_out_contains "KEEP failure is named" "DEPLOY_KEEP_IMAGES must be a whole number"
	assert_calls_not_contains "invalid KEEP removes nothing" "docker image rmi"

	new_case
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PINNED_SHAS=nothex
	run_prune
	assert_nonzero "a non-SHA pin is refused"
	assert_out_contains "pin failure is named" "DEPLOY_PINNED_SHAS entry"
	assert_calls_not_contains "invalid pin removes nothing" "docker image rmi"

	new_case
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PRUNE_BUILD_CACHE=yes
	run_prune
	assert_nonzero "a non-boolean cache switch is refused"
	assert_out_contains "cache switch failure is named" "DEPLOY_PRUNE_BUILD_CACHE must be 0 or 1"
	assert_calls_not_contains "invalid cache switch removes nothing" "docker image rmi"

	new_case
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PRUNE_BUILD_CACHE=1
	export DEPLOY_BUILD_CACHE_AGE=one-week
	run_prune
	assert_nonzero "an invalid cache age is refused"
	assert_out_contains "cache age failure is named" "DEPLOY_BUILD_CACHE_AGE must be a duration"
	assert_calls_not_contains "invalid cache age removes nothing" "docker image rmi"
}

test_incomplete_inventory_fails_closed() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 400000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 400000000002 "2026-11-01 10:00:00 +0000 UTC"
	: >"$CASE/image-ls-fails"
	run_prune
	assert_nonzero "a failed image list is refused"
	assert_out_contains "image list failure is named" "could not list clanka images"
	assert_calls_not_contains "a failed image list removes nothing" "docker image rmi"

	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 400000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 400000000002 "2026-11-01 10:00:00 +0000 UTC"
	: >"$CASE/ps-fails"
	run_prune
	assert_nonzero "a failed running-container list is refused"
	assert_out_contains "running list failure is named" "could not list running containers"
	assert_calls_not_contains "a failed running list removes nothing" "docker image rmi"

	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 400000000001 "2026-11-05 10:00:00 +0000 UTC"
	# A row with no creation time cannot be ordered, so it is never a candidate.
	add_image clanka/assistant ccccccc 400000000002 ""
	run_prune
	assert_status 0 "an incomplete row is ignored"
	assert_out_contains "the incomplete row is reported" "ignoring incomplete inventory row"
	assert_calls_not_contains "the incomplete row is never removed" "docker image rmi"
}

test_missing_deployed_sha_refuses_to_prune() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ccccccc 500000000001 "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/assistant ddddddd 500000000002 "2026-11-02 10:00:00 +0000 UTC"
	run_prune
	assert_nonzero "an unknown deployed SHA is refused"
	assert_out_contains "missing deployed set is named" "has no clanka/* tags"
	assert_calls_not_contains "an unknown deployed SHA removes nothing" "docker image rmi"
}

test_pinned_sha_survives() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PINNED_SHAS="ccccccc, ddddddd"
	add_image clanka/assistant ffffff0 600000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 600000000002 "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/assistant ddddddd 600000000003 "2026-11-02 10:00:00 +0000 UTC"
	add_image clanka/assistant eeeeeee 600000000004 "2026-11-03 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "a pinned prune succeeds"
	assert_out_contains "the first pin is kept" "keeping ccccccc (pinned)"
	assert_out_contains "the second pin is kept" "keeping ddddddd (pinned)"
	assert_calls_not_contains "a pinned tag is never removed" "docker image rmi clanka/assistant:ccccccc"
	assert_calls_not_contains "the second pinned tag is never removed" "docker image rmi clanka/assistant:ddddddd"
	assert_calls_contains "an unpinned old set is removed" "docker image rmi clanka/assistant:eeeeeee"
}

test_never_uses_a_broad_prune_command() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 700000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 700000000002 "2026-11-01 10:00:00 +0000 UTC"
	add_image clanka/tts ccccccc 700000000003 "2026-11-01 10:01:00 +0000 UTC"
	run_prune
	assert_status 0 "the narrow prune succeeds"
	assert_calls_contains "only explicit tags are removed" "docker image rmi clanka/assistant:ccccccc"
	assert_calls_not_contains "never runs docker image prune" "docker image prune"
	assert_calls_not_contains "never runs docker system prune" "docker system prune"
	assert_calls_not_contains "never runs docker volume prune" "docker volume prune"
	assert_calls_not_contains "never prunes the builder by default" "docker builder prune"
	assert_calls_not_contains "never asks for all images" "--all"
}

test_build_cache_prune_is_opt_in_and_best_effort() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 800000000001 "2026-11-05 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "the default prune succeeds"
	assert_builder_empty "the build cache is untouched by default"

	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PRUNE_BUILD_CACHE=1
	add_image clanka/assistant ffffff0 800000000002 "2026-11-05 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "the opt-in cache prune succeeds"
	assert_calls_contains "the opt-in calls builder prune" "docker builder prune"
	assert_builder_contains "the cache prune is bounded by age" "prune -f --filter until=168h"

	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PRUNE_BUILD_CACHE=1
	export DEPLOY_BUILD_CACHE_AGE=24h
	add_image clanka/assistant ffffff0 800000000003 "2026-11-05 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "a custom cache age is accepted"
	assert_builder_contains "the custom age reaches the filter" "until=24h"

	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	export DEPLOY_PRUNE_BUILD_CACHE=1
	add_image clanka/assistant ffffff0 800000000004 "2026-11-05 10:00:00 +0000 UTC"
	: >"$CASE/builder-fails"
	run_prune
	assert_status 0 "a failed cache prune never fails the run"
	assert_out_contains "the cache failure is a warning" "build cache prune failed"
}

test_disk_usage_is_shown_before_and_after() {
	new_case
	GIT_SHA_TEST=ffffff0
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant ffffff0 900000000001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant ccccccc 900000000002 "2026-11-01 10:00:00 +0000 UTC"
	run_prune
	assert_status 0 "the prune with disk usage succeeds"
	assert_out_count "disk usage is printed before and after" "prune: disk usage" 2
	assert_calls_contains "df runs once before and once after" "docker system df"
}

test_window_counts_commits_not_tags() {
	# One deployed commit spelled 7/12/40 ways, plus two older complete sets.
	# With KEEP=3 the aliases must collapse into a single slot so both older
	# rollback sets survive the window.
	new_case
	GIT_SHA_TEST=e351f16
	export DEPLOY_KEEP_IMAGES=3
	add_image clanka/assistant e351f16 aaaaaaa00001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant e351f161cf39 aaaaaaa00002 "2026-11-05 10:02:00 +0000 UTC"
	add_image clanka/assistant e351f161cf39d4a3ae87f7a58f2430872c221fa4 aaaaaaa00003 "2026-11-05 10:04:00 +0000 UTC"
	add_image clanka/assistant e591cf0 bbbbbbb00001 "2026-11-04 10:00:00 +0000 UTC"
	add_image clanka/tts e591cf0 bbbbbbb00002 "2026-11-04 10:01:00 +0000 UTC"
	add_image clanka/assistant a6e373d ccccccc00001 "2026-11-03 10:00:00 +0000 UTC"
	add_image clanka/tts a6e373d ccccccc00002 "2026-11-03 10:01:00 +0000 UTC"
	run_prune
	assert_status 0 "the multi-spelling deploy prunes nothing"
	assert_calls_not_contains "the 7-char deployed tag survives" "docker image rmi clanka/assistant:e351f16"
	assert_calls_not_contains "the 12-char deployed alias survives" "docker image rmi clanka/assistant:e351f161cf39"
	assert_calls_not_contains "the 40-char deployed alias survives" "docker image rmi clanka/assistant:e351f161cf39d4a3ae87f7a58f2430872c221fa4"
	assert_calls_not_contains "the first older set survives" "docker image rmi clanka/assistant:e591cf0"
	assert_calls_not_contains "the first older set's tts survives" "docker image rmi clanka/tts:e591cf0"
	assert_calls_not_contains "the second older set survives" "docker image rmi clanka/assistant:a6e373d"
	assert_calls_not_contains "the second older set's tts survives" "docker image rmi clanka/tts:a6e373d"
	assert_out_contains "the first older set is reported kept" "keeping e591cf0 (newest)"
	assert_out_contains "the second older set is reported kept" "keeping a6e373d (newest)"
	assert_out_contains "the deployed commit is reported kept" "(deployed)"
}

test_single_service_alias_does_not_evict_older_set() {
	# A newer commit carries a complete set (8-char) plus a 12-char partial-build
	# alias for one service. The alias must not consume a second window slot and
	# evict an older, complete rollback set.
	new_case
	GIT_SHA_TEST=77777777
	export DEPLOY_KEEP_IMAGES=3
	add_image clanka/assistant 77777777 111111100001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/tts 77777777 111111100002 "2026-11-05 10:01:00 +0000 UTC"
	add_image clanka/assistant 777777771234 111111100003 "2026-11-05 10:02:00 +0000 UTC"
	add_image clanka/assistant 8888888 222222200001 "2026-11-04 10:00:00 +0000 UTC"
	add_image clanka/tts 8888888 222222200002 "2026-11-04 10:01:00 +0000 UTC"
	add_image clanka/assistant 9999999 333333300001 "2026-11-03 10:00:00 +0000 UTC"
	add_image clanka/tts 9999999 333333300002 "2026-11-03 10:01:00 +0000 UTC"
	run_prune
	assert_status 0 "the alias-and-older-set prune succeeds"
	assert_calls_not_contains "the deployed 8-char tag survives" "docker image rmi clanka/assistant:77777777"
	assert_calls_not_contains "the partial alias survives" "docker image rmi clanka/assistant:777777771234"
	assert_calls_not_contains "the first older set survives" "docker image rmi clanka/assistant:8888888"
	assert_calls_not_contains "the first older set's tts survives" "docker image rmi clanka/tts:8888888"
	assert_calls_not_contains "the second older set survives" "docker image rmi clanka/assistant:9999999"
	assert_calls_not_contains "the second older set's tts survives" "docker image rmi clanka/tts:9999999"
	assert_out_contains "the first older set is reported kept" "keeping 8888888 (newest)"
	assert_out_contains "the second older set is reported kept" "keeping 9999999 (newest)"
}

test_every_tag_of_a_kept_commit_survives() {
	# KEEP=1: the deployed commit has three spellings; all of them must survive
	# even though the window would only admit one "set" by the old tag-counting
	# rule, and the single older commit must be the one removed.
	new_case
	GIT_SHA_TEST=e351f16
	export DEPLOY_KEEP_IMAGES=1
	add_image clanka/assistant e351f16 aaaaaaa00001 "2026-11-05 10:00:00 +0000 UTC"
	add_image clanka/assistant e351f161cf39 aaaaaaa00002 "2026-11-05 10:02:00 +0000 UTC"
	add_image clanka/assistant e351f161cf39d4a3ae87f7a58f2430872c221fa4 aaaaaaa00003 "2026-11-05 10:04:00 +0000 UTC"
	add_image clanka/assistant e591cf0 bbbbbbb00001 "2026-11-04 10:00:00 +0000 UTC"
	add_image clanka/tts e591cf0 bbbbbbb00002 "2026-11-04 10:01:00 +0000 UTC"
	run_prune
	assert_status 0 "the kept-commit spellings prune succeeds"
	assert_calls_not_contains "the 7-char deployed tag survives" "docker image rmi clanka/assistant:e351f16"
	assert_calls_not_contains "the 12-char deployed alias survives" "docker image rmi clanka/assistant:e351f161cf39"
	assert_calls_not_contains "the 40-char deployed alias survives" "docker image rmi clanka/assistant:e351f161cf39d4a3ae87f7a58f2430872c221fa4"
	assert_calls_contains "the older set is the only thing removed" "docker image rmi clanka/assistant:e591cf0"
	assert_calls_contains "the older set's tts is removed too" "docker image rmi clanka/tts:e591cf0"
}

run_test() {
	local name=$1
	printf '== %s\n' "$name"
	"$name"
}

run_test test_help
run_test test_rejects_unknown_argument
run_test test_retention_keeps_deployed_and_newest_sets
run_test test_current_sha_can_be_a_full_sha_and_newest
run_test test_zero_keep_retains_only_the_deployed_set
run_test test_keeps_a_whole_set_used_by_a_running_container
run_test test_failed_rmi_only_warns
run_test test_invalid_retention_and_pin_input_is_refused
run_test test_incomplete_inventory_fails_closed
run_test test_missing_deployed_sha_refuses_to_prune
run_test test_pinned_sha_survives
run_test test_never_uses_a_broad_prune_command
run_test test_build_cache_prune_is_opt_in_and_best_effort
run_test test_disk_usage_is_shown_before_and_after
run_test test_window_counts_commits_not_tags
run_test test_single_service_alias_does_not_evict_older_set
run_test test_every_tag_of_a_kept_commit_survives

printf '\n%d passed, %d failed\n' "$PASS" "$FAIL"
[[ "$FAIL" -eq 0 ]]
