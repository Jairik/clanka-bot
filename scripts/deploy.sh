#!/usr/bin/env bash
# Deploy the Compose stack on this host (the Pi, or a dev-box clone).
#
#   scripts/deploy.sh            pull the current branch, build SHA-tagged
#                                images, start the stack and wait for health
#   scripts/deploy.sh <commit>   check that commit out and reuse the images
#                                already built for it (no rebuild)
#
# Health checks are bounded by DEPLOY_HEALTH_TIMEOUT seconds (default 120).
# On failure the script prints `docker compose ps` and the logs of the
# unhealthy services, then exits non-zero.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HEALTH_TIMEOUT="${DEPLOY_HEALTH_TIMEOUT:-120}"
POLL_INTERVAL="${DEPLOY_POLL_INTERVAL:-2}"

log() { printf 'deploy: %s\n' "$*"; }
warn() { printf 'deploy: warning: %s\n' "$*" >&2; }
die() {
	printf 'deploy: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/deploy.sh [SHA]

Without a SHA: pull the current branch with --ff-only, build images tagged
clanka/<service>:<short SHA>, start the stack and wait for every health check.

With a SHA: check that commit out, verify its images already exist locally and
start the stack with --no-build. This is the rollback path.

Environment:
  DEPLOY_HEALTH_TIMEOUT  seconds to wait for health checks (default 120)
  DEPLOY_POLL_INTERVAL   seconds between health checks (default 2)
EOF
}

# Return to the tracked branch after a detached rollback so a later plain
# deploy can pull again. Read-only on branches other than the checkout.
ensure_on_branch() {
	local branch default
	branch="$(git symbolic-ref --quiet --short HEAD || true)"
	if [[ -n "$branch" ]]; then
		return
	fi
	default="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null || true)"
	default="${default#origin/}"
	default="${default:-main}"
	log "detached HEAD; switching back to $default"
	if git show-ref --verify --quiet "refs/heads/$default"; then
		git checkout --quiet "$default"
	elif git show-ref --verify --quiet "refs/remotes/origin/$default"; then
		git checkout --quiet -b "$default" --track "origin/$default"
	else
		die "detached HEAD and no '$default' branch to return to"
	fi
}

require_clean_worktree() {
	local changes
	changes="$(git status --porcelain --untracked-files=normal)"
	if [[ -n "$changes" ]]; then
		printf '%s\n' "$changes" >&2
		die "refusing to deploy: working tree has local changes"
	fi
}

resolve_sha() {
	local ref=$1
	git rev-parse --verify --quiet "${ref}^{commit}" >/dev/null ||
		die "unknown commit: $ref"
	git rev-parse --short "${ref}^{commit}"
}

# Print the compose ps table and the logs of the services named in "$@".
diagnose() {
	local reason=$1
	shift
	printf 'deploy: error: %s\n' "$reason" >&2
	printf 'deploy: services: %s\n' "$*" >&2
	printf -- '--- docker compose ps ---\n' >&2
	docker compose ps >&2 || true
	local entry svc
	for entry in "$@"; do
		svc="${entry%% *}"
		printf -- '--- logs: %s ---\n' "$svc" >&2
		docker compose logs --tail=100 "$svc" >&2 || true
	done
}

# Wait for every service in the checked-out compose file, not a hard-coded
# list. --remove-orphans above removes containers from a newer compose set, so
# a rollback leaves the host running only the services that commit defines.
wait_for_health() {
	local services count start deadline svc cid status
	local -a service_list failing pending
	services="$(docker compose config --services)"
	[[ -n "$services" ]] || die "docker compose config reported no services"
	mapfile -t service_list <<<"$services"
	count=${#service_list[@]}

	log "waiting up to ${HEALTH_TIMEOUT}s for health checks on ${count} service(s)"
	start=$SECONDS
	deadline=$((SECONDS + HEALTH_TIMEOUT))
	while :; do
		failing=()
		pending=()
		for svc in "${service_list[@]}"; do
			cid="$(docker compose ps -q "$svc" 2>/dev/null | head -n 1 || true)"
			if [[ -z "$cid" ]]; then
				pending+=("$svc (no container)")
				continue
			fi
			status="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || echo missing)"
			case "$status" in
			healthy | running) ;;
			unhealthy | exited | dead) failing+=("$svc ($status)") ;;
			*) pending+=("$svc (${status:-unknown})") ;;
			esac
		done

		if ((${#failing[@]} == 0 && ${#pending[@]} == 0)); then
			log "all ${count} service(s) healthy in $((SECONDS - start))s"
			return 0
		fi
		if ((${#failing[@]} > 0)); then
			diagnose "health check failed" "${failing[@]}"
			exit 1
		fi
		if ((SECONDS >= deadline)); then
			diagnose "health check wait did not pass within ${HEALTH_TIMEOUT}s" "${pending[@]}"
			exit 1
		fi
		sleep "$POLL_INTERVAL"
	done
}

main() {
	local requested="" arg sha image missing
	for arg in "$@"; do
		case "$arg" in
		-h | --help)
			usage
			exit 0
			;;
		-*)
			usage >&2
			exit 2
			;;
		*)
			if [[ -n "$requested" ]]; then
				usage >&2
				die "at most one commit argument is supported"
			fi
			requested="$arg"
			;;
		esac
	done

	[[ "$HEALTH_TIMEOUT" =~ ^[0-9]+$ ]] ||
		die "DEPLOY_HEALTH_TIMEOUT must be a whole number of seconds"
	[[ "$POLL_INTERVAL" =~ ^[0-9]+([.][0-9]+)?$ ]] ||
		die "DEPLOY_POLL_INTERVAL must be a number of seconds"

	cd "$ROOT"
	command -v docker >/dev/null 2>&1 || die "docker is required"
	docker compose version >/dev/null 2>&1 ||
		die "the docker compose v2 plugin is required"

	require_clean_worktree

	if [[ -n "$requested" ]]; then
		sha="$(resolve_sha "$requested")"
		log "rolling back to $sha"
		git checkout --quiet --detach "$requested" ||
			die "could not check out $requested"
		sha="$(git rev-parse --short HEAD)"
		export GIT_SHA="$sha"
		log "reusing images tagged clanka/<service>:$sha"

		# Fail before touching containers when a rollback image is absent.
		image=""
		missing=0
		while IFS= read -r image; do
			[[ -n "$image" ]] || continue
			if ! docker image inspect "$image" >/dev/null 2>&1; then
				printf 'deploy: error: missing image %s; build that commit on this host first\n' "$image" >&2
				missing=1
			fi
		done <<<"$(docker compose config --images)"
		((missing == 0)) || exit 1

		if ! docker compose up -d --no-build --remove-orphans; then
			printf -- '--- docker compose ps ---\n' >&2
			docker compose ps >&2 || true
			die "docker compose up failed"
		fi
	else
		ensure_on_branch
		if ! git pull --ff-only; then
			die "git pull --ff-only failed (local changes or diverged history)"
		fi
		sha="$(git rev-parse --short HEAD)"
		export GIT_SHA="$sha"
		log "building and starting clanka/<service>:$sha"
		if ! docker compose up -d --build --remove-orphans; then
			printf -- '--- docker compose ps ---\n' >&2
			docker compose ps >&2 || true
			die "docker compose up failed"
		fi
	fi

	wait_for_health
	log "deployed $sha"
}

main "$@"
