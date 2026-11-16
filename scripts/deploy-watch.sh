#!/usr/bin/env bash
# One poll of main, shared by the systemd timer: if origin/main has moved past
# HEAD, and CI is not still running or red, hand over to scripts/deploy.sh.
# A checkout on another branch, or with local commits ahead of origin/main, is
# left alone: deploy.sh builds the branch that is checked out.
#
# A commit younger than DEPLOY_WATCH_MIN_AGE is not built yet. Rebuilding eight
# images takes longer than a busy main stays still, so the settle window turns a
# burst of merges into one deploy instead of one per commit.
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
# A commit has to be this old before it is built. Merges arrive faster than a
# Pi can rebuild eight images (six to thirty minutes each), so without a settle
# window every commit starts its own full rebuild and the timer never idles.
# --force and DEPLOY_WATCH_MIN_AGE=0 are the escapes.
SETTLE_MINUTES="${DEPLOY_WATCH_MIN_AGE:-15}"

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
waits for health. A checkout on another branch, or one with local commits ahead
of origin/main, is left alone without touching Docker. Safe to run from a
systemd timer: one run at a time, and it exits 0 whenever there is nothing to
do.

Flags:
  --dry-run   do every check, log what would happen, never run deploy.sh
  --force     deploy even when HEAD already equals origin/main, and ignore the
              settle window

Environment:
  DEPLOY_WATCH_CI        0 disables the CI gate (default: enabled)
  DEPLOY_WATCH_API       GitHub API base (default: https://api.github.com)
  DEPLOY_WATCH_REPO      owner/name to query (default: Jairik/clanka-bot)
  DEPLOY_WATCH_LOCK      lock file (default: /tmp/clanka-deploy-watch.lock)
  DEPLOY_WATCH_MIN_AGE   minutes a commit must have been on main before it is
                         built, so a burst of merges coalesces into one rebuild
                         (default: 15, 0 disables)

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

# Seconds since a commit landed on main, clamped at zero. A commit timestamped
# in the future is clock skew, not evidence that main has already settled, but
# it must never block a deploy for longer than the settle window either.
target_age() {
	local target=$1 committed now
	committed="$(git log -1 --format=%ct "$target" 2>/dev/null || echo 0)"
	if ! [[ "$committed" =~ ^[0-9]+$ ]]; then
		printf '0\n'
		return 0
	fi
	now="$(date +%s)"
	if ((now > committed)); then
		printf '%s\n' "$((now - committed))"
	else
		printf '0\n'
	fi
}

main() {
	local dry_run=0 force=0 arg changes err target current target_short current_short status
	local branch upstream built built_short settle_secs age

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

	# A typo here must not silently become "deploy everything immediately" or
	# "wait thirty-five hours": refuse the run instead of guessing.
	case "$SETTLE_MINUTES" in
	'' | *[!0-9]*) die "DEPLOY_WATCH_MIN_AGE must be a whole number of minutes, got '$SETTLE_MINUTES'" ;;
	esac

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

	# deploy.sh pulls and builds the checked-out branch, so only a branch that
	# tracks origin/main is deployable here. A detached HEAD is the rollback
	# path: deploy.sh's ensure_on_branch recovers it before building.
	branch="$(git symbolic-ref --quiet --short HEAD || true)"
	if [[ -n "$branch" ]]; then
		upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
		if [[ "$upstream" != "origin/main" ]]; then
			log "checkout is on '$branch' (upstream ${upstream:-none}), not origin/main; leaving it alone"
			exit 0
		fi
		# A local commit on main leaves HEAD ahead of, or diverged from,
		# origin/main. Deploying would rebuild that same HEAD every tick, so
		# leave it for a push (or an explicit rollback) instead.
		if ! git merge-base --is-ancestor "$current" "$target"; then
			log "checkout is not behind origin/main (local commits?); leaving it alone"
			exit 0
		fi
	fi

	if [[ "$target" == "$current" && "$force" -eq 0 ]]; then
		log "already on $(git rev-parse --short "$current")"
		exit 0
	fi
	target_short="$(git rev-parse --short "$target")"

	# Let a burst of merges finish before spending a rebuild on it. The age is
	# the commit's own timestamp, so a tick that starts right after a merge sees
	# a young tip and defers; the tick after the burst builds once.
	settle_secs=$((SETTLE_MINUTES * 60))
	if ((settle_secs > 0 && force == 0)); then
		age="$(target_age "$target")"
		if ((age < settle_secs)); then
			log "origin/main $target_short moved ${age}s ago; waiting ${settle_secs}s for main to settle"
			exit 0
		fi
	fi

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
		# deploy.sh builds the local HEAD, so reporting the remote SHA without
		# checking would claim a commit the host is not running.
		built="$(git rev-parse HEAD)"
		if [[ "$built" != "$target" ]]; then
			built_short="$(git rev-parse --short "$built")"
			die "deploy returned 0 but HEAD is $built_short, not $target_short; refusing to report success"
		fi
		log "deployed $target_short"
	else
		status=$?
		printf 'deploy-watch: error: deploy of %s failed (exit %s); see the deploy.sh output above\n' \
			"$target_short" "$status" >&2
		exit "$status"
	fi
}

main "$@"
