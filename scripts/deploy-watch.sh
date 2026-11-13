#!/usr/bin/env bash
# One poll of main, shared by the systemd timer: if origin/main has moved past
# HEAD, and CI is not still running or red, hand over to scripts/deploy.sh.
#
#   scripts/deploy-watch.sh              deploy origin/main when it moves
#   scripts/deploy-watch.sh --dry-run    report what would happen, change nothing
#   scripts/deploy-watch.sh --force      redeploy even when HEAD is already there
#
# This script does not retry a failed deploy. deploy.sh pulls before it builds,
# so a build or health failure leaves HEAD already at the new commit and the
# next tick sees nothing to do; that commit stays deployed-but-broken until main
# moves again. The way back is `make rollback SHA=<commit>` or a revert, not a
# spin loop against the same SHA.
#
# All git writes happen inside deploy.sh (`git pull --ff-only`); the watcher
# itself only fetches and reads.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

LOCK_FILE="${DEPLOY_WATCH_LOCK:-/tmp/clanka-deploy-watch.lock}"
API="${DEPLOY_WATCH_API:-https://api.github.com}"
REPO="${DEPLOY_WATCH_REPO:-Jairik/clanka-bot}"
CI_ENABLED="${DEPLOY_WATCH_CI:-1}"

log() { printf 'deploy-watch: %s\n' "$*"; }
warn() { printf 'deploy-watch: warning: %s\n' "$*" >&2; }
die() {
	printf 'deploy-watch: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/deploy-watch.sh [--dry-run] [--force]

Poll origin/main once. When it is ahead of HEAD and its CI checks are green (or
have not reported at all), run scripts/deploy.sh, which pulls, rebuilds and
waits for health. Safe to run from a systemd timer: one run at a time, and it
exits 0 whenever there is nothing to do.

Flags:
  --dry-run   do every check, log what would happen, never run deploy.sh
  --force     deploy even when HEAD already equals origin/main

Environment:
  DEPLOY_WATCH_CI     0 disables the CI gate (default: enabled)
  DEPLOY_WATCH_API    GitHub API base (default: https://api.github.com)
  DEPLOY_WATCH_REPO   owner/name to query (default: Jairik/clanka-bot)
  DEPLOY_WATCH_LOCK   lock file (default: /tmp/clanka-deploy-watch.lock)

The CI gate reads the public check-runs API, so no token is used or logged.
A pending or red main is never deployed. If the API cannot be reached (or
python3 is missing) the watcher warns and deploys anyway, so a network blip
cannot wedge the timer; HEAD equality still prevents pointless rebuilds.
EOF
}

# Reads the check-runs payload on stdin and prints one word: pending, red,
# green, none or unknown. Order matters: an unfinished run outranks a failure
# from an earlier one.
CI_PARSER="$(
	cat <<'PY'
import json
import sys

RED = {"failure", "timed_out", "cancelled", "action_required", "startup_failure"}

try:
    data = json.load(sys.stdin)
except Exception:
    print("unknown")
    sys.exit(0)

runs = data.get("check_runs") or []
if any(run.get("status") != "completed" for run in runs):
    print("pending")
elif any(run.get("conclusion") in RED for run in runs):
    print("red")
elif not runs or not data.get("total_count", len(runs)):
    print("none")
else:
    print("green")
PY
)"

# Print pending|red|green|none for a commit, or unknown when the answer cannot
# be had. Never fails the caller: the caller deploys on unknown.
ci_state() {
	local sha=$1 url body state
	if ! command -v python3 >/dev/null 2>&1; then
		warn "python3 is not installed; cannot read CI status"
		printf 'unknown\n'
		return 0
	fi
	url="$API/repos/$REPO/commits/$sha/check-runs?per_page=100"
	if ! body="$(curl -fsS --max-time 10 \
		-H 'Accept: application/vnd.github+json' \
		-H 'User-Agent: clanka-deploy-watch' \
		"$url" 2>&1)"; then
		body="${body%%$'\n'*}"
		warn "CI status request failed (${body:-network error}); this is not a CI failure"
		printf 'unknown\n'
		return 0
	fi
	state="$(printf '%s' "$body" | python3 -c "$CI_PARSER" 2>/dev/null || true)"
	case "$state" in
	pending | red | green | none) printf '%s\n' "$state" ;;
	*)
		warn "could not parse the CI status payload"
		printf 'unknown\n'
		;;
	esac
}

main() {
	local dry_run=0 force=0 arg changes err target current target_short current_short status

	for arg in "$@"; do
		case "$arg" in
		-h | --help)
			usage
			exit 0
			;;
		--dry-run) dry_run=1 ;;
		--force) force=1 ;;
		*)
			usage >&2
			exit 2
			;;
		esac
	done

	# One watcher at a time. The fd stays open for the whole run; the kernel
	# drops the lock if this process dies.
	exec 9>"$LOCK_FILE" || die "cannot open lock file $LOCK_FILE"
	if ! flock -n 9; then
		log "another run is in progress"
		exit 0
	fi

	cd "$ROOT" || die "cannot enter $ROOT"
	command -v git >/dev/null 2>&1 || die "git is required"
	[[ -e "$ROOT/.git" ]] || die "$ROOT is not a git checkout"

	changes="$(git status --porcelain --untracked-files=normal)"
	if [[ -n "$changes" ]]; then
		printf '%s\n' "$changes" >&2
		log "working tree has local changes; leaving it alone"
		exit 0
	fi

	if ! err="$(git fetch --quiet origin main 2>&1)"; then
		err="${err//$'\n'/ }"
		die "git fetch origin main failed: ${err:-unknown error}"
	fi

	target="$(git rev-parse origin/main)"
	current="$(git rev-parse HEAD)"
	if [[ "$target" == "$current" && "$force" -eq 0 ]]; then
		log "already on $(git rev-parse --short "$current")"
		exit 0
	fi
	target_short="$(git rev-parse --short "$target")"

	if [[ "$CI_ENABLED" != "0" ]]; then
		case "$(ci_state "$target")" in
		pending)
			log "CI is still running for $target_short; retrying on a later tick"
			exit 0
			;;
		red)
			log "CI is red for $target_short; not deploying"
			exit 0
			;;
		none) warn "no CI checks reported for $target_short; deploying anyway" ;;
		green) log "CI is green for $target_short" ;;
		*) warn "CI status for $target_short is unknown; deploying anyway" ;;
		esac
	fi

	if ((dry_run == 1)); then
		log "would deploy $target_short"
		exit 0
	fi

	current_short="$(git rev-parse --short "$current")"
	log "deploying $target_short (was $current_short)"
	# Not `if ! status=$?`: the negation would report 0 and turn a failed
	# deploy into a success. Keep the branch form so the real status survives.
	if bash "$ROOT/scripts/deploy.sh"; then
		log "deployed $target_short"
	else
		status=$?
		printf 'deploy-watch: error: deploy of %s failed (exit %s); see the deploy.sh output above\n' \
			"$target_short" "$status" >&2
		exit "$status"
	fi
}

main "$@"
