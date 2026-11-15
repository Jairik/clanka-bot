#!/usr/bin/env bash
# Reclaim old clanka/<service>:<sha> images without touching rollback sets.
#
# Every deploy tags a complete image set with the commit, so old sets are
# tagged, not dangling: `docker image prune` reclaims nothing and
# `docker image prune -a` would delete the tags rollback needs.
#
# The deployed SHA (GIT_SHA, or HEAD), the newest DEPLOY_KEEP_IMAGES sets and
# DEPLOY_PINNED_SHAS survive, as does any set with a tag used by a running
# container. Only tags that look like a commit SHA are candidates: `dev`,
# `arm64-*` and other local tags are never touched. A failed `rmi` warns; an
# incomplete inventory removes nothing.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
KEEP="${DEPLOY_KEEP_IMAGES:-3}"
PINNED_RAW="${DEPLOY_PINNED_SHAS:-}"
BUILD_CACHE="${DEPLOY_PRUNE_BUILD_CACHE:-0}"
BUILD_CACHE_AGE="${DEPLOY_BUILD_CACHE_AGE:-168h}"
SHA_PATTERN='^[0-9a-f]{7,40}$'

log() { printf 'prune: %s\n' "$*"; }
warn() { printf 'prune: warning: %s\n' "$*" >&2; }
die() {
	printf 'prune: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/prune-images.sh [--help]

Remove old clanka/<service>:<sha> image sets, keeping the deployed SHA, the
newest DEPLOY_KEEP_IMAGES sets (default 3) and any pinned SHA. Non-SHA tags and
sets used by running containers are never touched.

Environment:
  GIT_SHA                    deployed commit (default: git rev-parse --short HEAD)
  DEPLOY_KEEP_IMAGES         newest SHA sets to keep (default 3); the deployed
                             and pinned sets survive even when they are older
  DEPLOY_PINNED_SHAS         comma- or space-separated SHA prefixes to keep
  DEPLOY_PRUNE_BUILD_CACHE   1 also prunes build cache older than the age
                             below; host-wide, opt-in, off by default
  DEPLOY_BUILD_CACHE_AGE     age bound for that prune (default 168h)
EOF
}

# True when two commit abbreviations name the same commit: equal, or one is a
# prefix of the other (a short tag vs a full GIT_SHA).
same_commit() {
	local a=$1 b=$2
	[[ "$a" == "$b" || "$a" == "$b"* || "$b" == "$a"* ]]
}

ref_is_running() {
	local ref=$1 r
	for r in "${running_refs[@]}"; do
		[[ "$r" == "$ref" ]] && return 0
	done
	return 1
}

id_is_running() {
	local id=$1 r short
	[[ -n "$id" ]] || return 1
	for r in "${running_ids[@]}"; do
		short="${r:0:12}"
		[[ "$id" == "$r" || "$id" == "$short" || "${id:0:12}" == "$short" ]] && return 0
	done
	return 1
}

show_disk_usage() {
	local label=$1
	log "disk usage $label"
	if ! docker system df; then
		warn "docker system df failed; continuing"
	fi
}

for arg in "$@"; do
	case "$arg" in
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
done

command -v docker >/dev/null 2>&1 || die "docker is required"

current="${GIT_SHA:-}"
if [[ -z "$current" ]]; then
	current="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || true)"
fi
[[ -n "$current" ]] || die "no GIT_SHA and git could not name HEAD; refusing to prune"
[[ "$current" =~ $SHA_PATTERN ]] ||
	die "GIT_SHA '$current' is not a commit SHA; refusing to prune"
[[ "$KEEP" =~ ^[0-9]+$ ]] ||
	die "DEPLOY_KEEP_IMAGES must be a whole number, got '$KEEP'"
case "$BUILD_CACHE" in
0 | 1) ;;
*) die "DEPLOY_PRUNE_BUILD_CACHE must be 0 or 1, got '$BUILD_CACHE'" ;;
esac
[[ "$BUILD_CACHE_AGE" =~ ^[0-9]+(s|m|h)$ ]] ||
	die "DEPLOY_BUILD_CACHE_AGE must be a duration such as 168h, got '$BUILD_CACHE_AGE'"

pinned=()
if [[ -n "$PINNED_RAW" ]]; then
	IFS=', ' read -r -a pinned <<<"$PINNED_RAW"
	for pin in "${pinned[@]}"; do
		[[ -n "$pin" ]] || continue
		[[ "$pin" =~ $SHA_PATTERN ]] ||
			die "DEPLOY_PINNED_SHAS entry '$pin' is not a 7-40 character lowercase hex SHA"
	done
fi

TMP="$(mktemp -d "${TMPDIR:-/tmp}/clanka-prune.XXXXXX")" || exit 1
trap 'rm -rf "$TMP"' EXIT

show_disk_usage before

# An inventory that does not list cleanly cannot be pruned safely: a failed
# listing looks exactly like "no old images" and would hide the tags we must
# keep. Stop before removing anything.
if ! inventory="$(docker image ls \
	--format '{{.Repository}}|{{.Tag}}|{{.ID}}|{{.CreatedAt}}' 2>"$TMP/inventory.err")"; then
	[[ -s "$TMP/inventory.err" ]] && cat "$TMP/inventory.err" >&2
	die "could not list clanka images; refusing to prune"
fi
if ! running="$(docker ps --format '{{.Image}}' 2>"$TMP/running.err")"; then
	[[ -s "$TMP/running.err" ]] && cat "$TMP/running.err" >&2
	die "could not list running containers; refusing to prune"
fi

declare -A set_newest=()
declare -A set_tags=()
declare -A tag_id=()
while IFS='|' read -r repo tag id created; do
	if [[ -z "$repo" || -z "$tag" || -z "$id" || -z "$created" ]]; then
		[[ -n "$repo" || -n "$tag" ]] &&
			warn "ignoring incomplete inventory row: $repo|$tag|$id|$created"
		continue
	fi
	[[ "$repo" == clanka/* ]] || continue
	[[ "$tag" =~ $SHA_PATTERN ]] || continue
	ref="$repo:$tag"
	tag_id["$ref"]="$id"
	set_tags["$tag"]+="$ref"$'\n'
	if [[ -z "${set_newest[$tag]:-}" || "$created" > "${set_newest[$tag]}" ]]; then
		set_newest["$tag"]="$created"
	fi
done <<<"$inventory"

running_refs=()
running_ids=()
while IFS= read -r ref; do
	[[ -n "$ref" ]] || continue
	running_refs+=("$ref")
	case "$ref" in
	sha256:*) running_ids+=("${ref#sha256:}") ;;
	*)
		if [[ "$ref" =~ ^[0-9a-f]{12,64}$ ]]; then
			running_ids+=("$ref")
		fi
		;;
	esac
done <<<"$running"

if ((${#set_newest[@]} == 0)); then
	log "no clanka SHA-tagged images found; nothing to prune"
	show_disk_usage after
	exit 0
fi

# Newest set first. Every row is formatted by the same daemon in the same
# local zone, so the CreatedAt strings sort consistently.
mapfile -t ordered < <(
	for sha in "${!set_newest[@]}"; do
		printf '%s|%s\n' "${set_newest[$sha]}" "$sha"
	done | sort -r | cut -d'|' -f2
)

current_found=0
for sha in "${ordered[@]}"; do
	if same_commit "$sha" "$current"; then
		current_found=1
		break
	fi
done
if ((current_found == 0)); then
	die "deployed SHA $current has no clanka/* tags; refusing to prune"
fi

declare -A keep=()
for sha in "${ordered[@]}"; do
	if same_commit "$sha" "$current"; then
		keep["$sha"]="deployed"
	fi
	for pin in "${pinned[@]}"; do
		[[ -n "$pin" ]] || continue
		if same_commit "$sha" "$pin"; then
			keep["$sha"]="pinned"
		fi
	done
done

newest=0
for sha in "${ordered[@]}"; do
	((newest < KEEP)) || break
	if [[ -z "${keep[$sha]:-}" ]]; then
		keep["$sha"]="newest"
	fi
	newest=$((newest + 1))
done

# Keep a whole set when any of its tags is in use, not just that one tag: a
# partially deleted set cannot be rolled back to.
for sha in "${ordered[@]}"; do
	[[ -z "${keep[$sha]:-}" ]] || continue
	while IFS= read -r ref; do
		[[ -n "$ref" ]] || continue
		if ref_is_running "$ref" || id_is_running "${tag_id[$ref]:-}"; then
			keep["$sha"]="running"
			log "keeping $ref (used by a running container)"
			break
		fi
	done <<<"${set_tags[$sha]}"
done

removed=0
failed=0
kept_sets=0
pruned_sets=0
for sha in "${ordered[@]}"; do
	if [[ -n "${keep[$sha]:-}" ]]; then
		kept_sets=$((kept_sets + 1))
		log "keeping $sha (${keep[$sha]})"
		continue
	fi
	pruned_sets=$((pruned_sets + 1))
	while IFS= read -r ref; do
		[[ -n "$ref" ]] || continue
		if docker image rmi "$ref"; then
			removed=$((removed + 1))
			log "removed $ref"
		else
			failed=$((failed + 1))
			warn "could not remove $ref; leaving it in place"
		fi
	done <<<"${set_tags[$sha]}"
done

# Build cache is shared by the whole Docker daemon and cannot be scoped to this
# stack, so it is opt-in and best effort. A cache cleanup never fails deploy.
if [[ "$BUILD_CACHE" == "1" ]]; then
	log "pruning build cache older than $BUILD_CACHE_AGE (host-wide, opt-in)"
	if docker builder prune -f --filter "until=$BUILD_CACHE_AGE"; then
		log "build cache pruned"
	else
		warn "build cache prune failed; leaving it in place"
	fi
fi

show_disk_usage after
log "done: removed $removed tag(s) from $pruned_sets set(s), kept $kept_sets set(s)"
if ((failed > 0)); then
	warn "$failed tag(s) could not be removed; the next run will retry"
fi
exit 0
