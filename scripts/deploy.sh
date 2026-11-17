#!/usr/bin/env bash
# Deploy the Compose stack on this host (the Pi, or a dev-box clone).
#
#   scripts/deploy.sh            pull the current branch, fetch the arm64
#                                images CI published for it (building any that
#                                are missing), tag them with the SHA, start the
#                                stack and wait for health
#   scripts/deploy.sh <commit>   check that commit out and reuse the images
#                                already built for it (no rebuild)
#
# Health checks are bounded by DEPLOY_HEALTH_TIMEOUT seconds (default 180).
# A slow assistant cold start (model rescan, first boot, a loaded Pi) can take
# longer than Docker's own start_period + retries, so a transient `unhealthy`
# that began within DEPLOY_COLD_START_GRACE seconds of the container's start is
# tolerated and re-checked rather than treated as a failed deploy (default 150).
# On failure the script prints `docker compose ps` and the logs of the
# unhealthy services, then exits non-zero.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
HEALTH_TIMEOUT="${DEPLOY_HEALTH_TIMEOUT:-180}"
POLL_INTERVAL="${DEPLOY_POLL_INTERVAL:-2}"
# A crash-loop (`exited`/`dead`) is a real failure and is never within grace.
COLD_START_GRACE="${DEPLOY_COLD_START_GRACE:-150}"
# Where .github/workflows/images.yml publishes <service>:key-<key>. Empty
# builds every image on this host, as before CI published them.
REGISTRY="${DEPLOY_REGISTRY-ghcr.io/jairik/clanka-bot}"

log() { printf 'deploy: %s\n' "$*"; }
warn() { printf 'deploy: warning: %s\n' "$*" >&2; }
die() {
	printf 'deploy: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/deploy.sh [SHA]

Without a SHA: pull the current branch with --ff-only, pull the images CI
published for this checkout (matched by content key, scripts/image-keys.py),
tag them clanka/<service>:<short SHA>, build only the ones that are not
published, start the stack, wait for every health check and prune old
SHA-tagged image sets (best effort; see scripts/prune-images.sh).
A branch with commits that are not on its upstream is refused: only pushed
commits are deployed.

With a SHA: check that commit out, verify its images already exist locally and
start the stack with --no-build. This is the rollback path. The prune also runs
after the health wait.

Environment:
  DEPLOY_HEALTH_TIMEOUT  seconds to wait for health checks (default 180)
  DEPLOY_POLL_INTERVAL   seconds between health checks (default 2)
  DEPLOY_COLD_START_GRACE  seconds after a container's start during which a
                          transient 'unhealthy' is retried, not failed
                          (default 150; must exceed the assistant start_period
                          plus its retries x interval)
  DEPLOY_KEEP_IMAGES     newest image sets to keep (default 3)
  DEPLOY_PINNED_SHAS     comma-separated SHA prefixes the prune must keep
  DEPLOY_PRUNE_BUILD_CACHE  1 also prunes the build cache (off by default)
  DEPLOY_REGISTRY        where CI publishes images
                          (default ghcr.io/jairik/clanka-bot); empty builds
                          every image on this host
  DEPLOY_SMOKE           run smoke turn after health wait (default 1)
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


# Warm-up preflight for host Pulse audio. Compose mounts the host Pulse socket
# as `${XDG_RUNTIME_DIR:-/run/user/1000}/pulse`; over an interactive SSH session
# the login manager sets XDG_RUNTIME_DIR, but the systemd timer unit runs with no
# session and (now) exports it explicitly. If the runtime dir or the Pulse native
# socket is missing, Docker creates an empty bind source and the assistant and
# Soloist go deaf — and /healthz never catches it. Warn rather than fail: a
# deploy that fails leaves the old (working) stack, but a deaf assistant is
# worse. Returning 0 keeps the deploy going; the caller decides success.
check_pulse() {
	local uid runtime cookie user
	uid="$(id -u 2>/dev/null || true)"
	uid="${uid:-1000}"
	runtime="${XDG_RUNTIME_DIR:-/run/user/$uid}"
	if [[ -S "$runtime/pulse/native" ]]; then
		cookie="$HOME/.config/pulse/cookie"
		if [[ ! -r "$cookie" ]]; then
			warn "Pulse cookie not readable at $cookie; the assistant may fail to authenticate to Pulse"
		fi
		return 0
	fi
	warn "Pulse native socket not found at $runtime/pulse/native"
	warn "the assistant and Soloist will mount an empty Pulse directory and have no audio."
	if [[ ! -d "/run/user/$uid" ]]; then
		user="$(id -un 2>/dev/null || echo "$uid")"
		warn "  /run/user/$uid is missing: the user's PipeWire session does not exist at boot."
		warn "  enable lingering so the session survives without a login:"
		warn "    sudo loginctl enable-linger $user"
	fi
	warn "  start PipeWire/Pulse for the deploy user, then re-run the deploy."
	return 0
}

# Wait for every service Compose actually started, not a hard-coded list.
# `docker compose ps --services` lists only running containers, so a service
# behind an inactive profile (the Spotify receiver) is excluded automatically,
# whereas `config --services` would still name it and hang the health wait.
wait_for_health() {
	local services count start deadline svc cid status
	local -a service_list failing pending
	services="$(docker compose ps --services 2>/dev/null)" ||
		die "could not list the running services"
	[[ -n "$services" ]] || die "docker compose ps reported no running services"
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
			# Read the health status and the container's start time in one
			# inspect so a slow cold start can be told apart from a crash-loop.
			raw="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}} {{.State.StartedAt}}{{else}}{{.State.Status}} {{.State.StartedAt}}{{end}}' "$cid" 2>/dev/null || echo missing)"
			status="${raw%% *}"
			started_at="${raw#* }"
			if [[ "$status" == "$started_at" ]]; then
				# No space: a bare token (e.g. "missing"). Treat start as unknown.
				started_at=""
			fi
			case "$status" in
			healthy | running) ;;
			exited | dead)
				# A crash-loop is a real failure, never inside the grace window.
				failing+=("$svc ($status)")
				;;
			unhealthy)
				if [[ -n "$started_at" ]]; then
					age=$(( $(date +%s) - $(date -d "$started_at" +%s 2>/dev/null || echo 0) ))
					if ((age < COLD_START_GRACE)); then
						log "$svc is unhealthy after ${age}s; still waiting (cold-start grace ${COLD_START_GRACE}s)"
						pending+=("$svc (unhealthy, within cold-start grace)")
						continue
					fi
				fi
				failing+=("$svc ($status)")
				;;
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

# The Soloist receiver is opt-in: only provision and start it when a key is
# configured. A key is either imported via SOLOIST_API_KEY_FILE or already
# present in the private directory. The receiver sits behind the `spotify`
# Compose profile, so when no key is present the profile stays inactive and the
# rest of the stack deploys without it.
receiver_key_present() {
	[[ -n "${SOLOIST_API_KEY_FILE:-}" ]] && return 0
	local key="${SOLOIST_DIR:-$HOME/.cache/clanka/soloist}/api-key"
	[[ -s "$key" ]]
}

# Provision the receiver (and its private directories) on this host before
# Compose starts. Only called when a key is present; a rollback to a commit
# without the service is a no-op.
ensure_receiver() {
	local services
	services="$(docker compose config --services 2>/dev/null || true)"
	if ! grep -qx 'soloist' <<<"$services"; then
		return
	fi
	bash "$ROOT/scripts/soloist-setup.sh" ||
		die "could not prepare the Soloist receiver"
}

# Make clanka/<service>:<sha> exist for every active service that builds an
# image. Each one is pulled from the registry by its content key, so a commit
# that did not touch a service reuses the image CI already published (and a
# pull of an unchanged image moves no layers). A service whose key is not
# published (CI still running, a Pi-only .env value, no network) is built here
# instead; a missing registry is never a failed deploy.
prepare_images() {
	local sha=$1 keys_out svc key ref target
	local -a active local_build=()
	local -A key_of=()
	mapfile -t active < <(docker compose config --services 2>/dev/null)
	((${#active[@]} > 0)) || die "could not list the compose services"

	if [[ -z "$REGISTRY" ]]; then
		log "DEPLOY_REGISTRY is empty; building every image on this host"
		local_build=("${active[@]}")
	elif ! keys_out="$(python3 -I "$ROOT/scripts/image-keys.py" 2>&1)"; then
		warn "could not compute image keys (${keys_out:-no output}); building every image on this host"
		local_build=("${active[@]}")
	else
		while read -r svc key; do
			[[ -n "$svc" ]] && key_of[$svc]=$key
		done <<<"$keys_out"
		for svc in "${active[@]}"; do
			key="${key_of[$svc]:-}"
			# No build section: Compose pulls the service's own image.
			[[ -n "$key" ]] || continue
			ref="$REGISTRY/$svc:key-$key"
			target="clanka/$svc:$sha"
			if docker pull --quiet "$ref" >/dev/null 2>&1 && docker tag "$ref" "$target"; then
				# Drop the registry tag; the image stays under the SHA tag,
				# which prune-images.sh manages.
				docker image rm "$ref" >/dev/null 2>&1 || true
				log "using published $svc image (key-$key)"
			else
				warn "no published $svc image for key-$key; building it on this host"
				local_build+=("$svc")
			fi
		done
	fi

	if ((${#local_build[@]} > 0)); then
		log "building ${local_build[*]}"
		if ! docker compose build "${local_build[@]}"; then
			die "docker compose build failed"
		fi
	fi
}

# Reclaim old SHA-tagged images after a healthy start. Best effort: a prune
# problem must never turn a good deployment into a failed one.
prune_old_images() {
	local sha=$1
	if ! GIT_SHA="$sha" bash "$ROOT/scripts/prune-images.sh"; then
		warn "image pruning failed; the deployment is still healthy"
	fi
}

# Post-deploy smoke turn: prove the stack answers through the shared pipeline.
# DEPLOY_SMOKE=0 disables it; a missing script warns and skips.
run_smoke() {
	if [[ "${DEPLOY_SMOKE:-1}" == "0" ]]; then
		return 0
	fi
	local smoke_script="$ROOT/scripts/smoke.sh"
	if [[ ! -f "$smoke_script" ]]; then
		warn "scripts/smoke.sh is missing; skipping smoke turn"
		return 0
	fi
	log "running post-deploy smoke turn"
	if ! bash "$smoke_script"; then
		die "post-deploy smoke turn failed"
	fi
}

main() {
	local requested="" arg sha image missing branch upstream
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
	[[ "$COLD_START_GRACE" =~ ^[0-9]+$ ]] ||
		die "DEPLOY_COLD_START_GRACE must be a whole number of seconds"

	# Preflight the host Pulse socket before any build/start. A missing socket
	# only warns: a failed deploy leaves the old stack, but a deaf assistant is
	# worse, and /healthz does not depend on audio.
	check_pulse

	cd "$ROOT"
	command -v docker >/dev/null 2>&1 || die "docker is required"
	docker compose version >/dev/null 2>&1 ||
		die "the docker compose v2 plugin is required"

	require_clean_worktree

	# Host-visible voice-sample bundles. Created before Compose so the bind
	# mount is owned by the deploy user rather than root; the directory is
	# gitignored because it holds the owner's recordings. A previous
	# `docker compose up` may have left it root-owned, which mkdir -p will not
	# repair, so fail with the exact remediation before any build runs.
	mkdir -p "$ROOT/exports"
	if [[ ! -w "$ROOT/exports" ]]; then
		die "exports is not writable (likely owned by root from a previous docker compose up); run: sudo chown -R $(id -u):$(id -g) exports"
	fi

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

		if receiver_key_present; then
			export COMPOSE_PROFILES=spotify
			ensure_receiver
		fi
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
		# A pull on a branch that is ahead of its upstream says "Already up to
		# date" without moving HEAD. Building that commit would deploy work
		# that exists only on this host, so refuse it here too.
		branch="$(git symbolic-ref --quiet --short HEAD || true)"
		if [[ -n "$branch" ]]; then
			upstream="$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null || true)"
			if [[ -n "$upstream" ]] && ! git merge-base --is-ancestor HEAD '@{u}'; then
				die "refusing to deploy: $branch has commits that are not on $upstream; push or reset first"
			fi
		fi
		sha="$(git rev-parse --short HEAD)"
		export GIT_SHA="$sha"
		if receiver_key_present; then
			export COMPOSE_PROFILES=spotify
			ensure_receiver
		fi
		prepare_images "$sha"
		log "starting clanka/<service>:$sha"
		if ! docker compose up -d --no-build --remove-orphans; then
			printf -- '--- docker compose ps ---\n' >&2
			docker compose ps >&2 || true
			die "docker compose up failed"
		fi
	fi

	wait_for_health
	run_smoke
	log "deployed $sha"
	prune_old_images "$sha"
}

main "$@"
