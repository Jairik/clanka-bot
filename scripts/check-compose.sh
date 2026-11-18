#!/usr/bin/env bash
# Check the deployment conventions the stack relies on:
#   - every service builds locally so it has a
#     clanka/<service>:${CLANKA_TAG_<SERVICE>:-${GIT_SHA:-dev}} image (upstream
#     images are wrapped in a small Dockerfile); deploy.sh sets the per-service
#     tag to the content key so an unchanged service is not recreated
#   - every service restarts unless stopped
#   - every service has a health check, and no health check calls the internet
#   - a locally built ML service downloads model weights before its first
#     COPY of application code (so code edits do not bust the weights layer)
#   - every service that only computes or serves content drops all Linux
#     capabilities, refuses privilege gain and caps its PIDs at 256; soloist is
#     out of scope for that block and dashboard-web cannot drop capabilities,
#     so both are explicit exceptions
#
# The compose file is expected to keep the flat layout used here: top-level
# `services:` with two-space service keys. Keys may carry a trailing comment.
# The script only reads files; it does not need Docker, so `make check` also
# works before the stack is built.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$ROOT/docker-compose.yml}"
ML_SERVICES="wakeword stt tts embed"
# Services that need no capability and no privilege gain. assistant handles
# untrusted text but still needs none; it keeps its system D-Bus mount for BLE.
HARDENED_SERVICES="assistant embed wakeword stt tts web-search hermes"
# Soloist is out of scope for #241's cap_drop/pids_limit block, but its existing
# no-new-privileges is still a documented convention and is enforced here.
# Host networking is not a Linux capability, and it runs as the host UID.
NNP_ONLY_SERVICES="soloist"
# The one full exception: nginx's root master needs CAP_CHOWN to chown
# /var/cache/nginx and CAP_SETUID/CAP_SETGID to drop workers to uid 101, so
# cap_drop: ALL makes it exit. A new service must join a list on purpose.
FULLY_EXEMPT_SERVICES="dashboard-web"

FAILURES=0
WARNINGS=0

fail() {
	printf 'check: FAIL: %s\n' "$*" >&2
	FAILURES=$((FAILURES + 1))
}

warn() {
	printf 'check: WARN: %s\n' "$*" >&2
	WARNINGS=$((WARNINGS + 1))
}

service_names() {
	awk '
		/^services:[[:space:]]*(#.*)?$/ { in_services=1; next }
		in_services && /^[^[:space:]#]/ { in_services=0 }
		in_services && /^  [A-Za-z0-9_.-]+:[[:space:]]*(#.*)?$/ {
			name=$0
			sub(/^  /, "", name)
			sub(/:.*/, "", name)
			print name
		}
	' "$COMPOSE_FILE"
}

service_block() {
	awk -v svc="$1" '
		/^  [A-Za-z0-9_.-]+:[[:space:]]*(#.*)?$/ {
			name=$0
			sub(/^  /, "", name)
			sub(/:.*/, "", name)
			inside=(name == svc)
			next
		}
		inside { print }
	' "$COMPOSE_FILE"
}

healthcheck_block() {
	awk '
		/^    healthcheck:[[:space:]]*(#.*)?$/ { in_health=1; next }
		in_health && /^    [A-Za-z0-9_.-]+:/ { in_health=0 }
		in_health { print }
	' <<<"$1"
}

# YAML block-list items under a service key, one per line. Stop at any sibling
# key, including one that carries a trailing comment, so a later list cannot be
# credited to an earlier key.
#   list_items security_opt "<service block>"
list_items() {
	local key=$1 block=$2
	awk -v key="$key" '
		$0 ~ "^    " key ":[[:space:]]*(#.*)?$" { inside=1; next }
		inside && /^    [A-Za-z0-9_.-]+:/ { inside=0 }
		inside && /^[[:space:]]+-[[:space:]]*/ {
			sub(/^[[:space:]]+-[[:space:]]*/, "")
			sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, "")
			print
		}
	' <<<"$block"
}

# Count the service-level occurrences of a key, with or without a comment.
key_count() {
	local key=$1 block=$2
	grep -cE "^    $key:" <<<"$block" || true
}

# First value for a service-level key, without its optional trailing comment.
key_value() {
	local key=$1 block=$2
	awk -v key="$key" '
		$0 ~ "^    " key ":" {
			sub("^    " key ":[[:space:]]*", "")
			sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, "")
			print
			exit
		}
	' <<<"$block"
}

# One line for a list, so an override or duplicate entry is visible in a message.
join_items() {
	list_items "$1" "$2" | awk 'NR > 1 { printf " " } { printf "%s", $0 }'
}

check_no_new_privileges() {
	local svc=$1 block=$2 count opt
	count="$(key_count security_opt "$block")"
	if [[ "$count" -ne 1 ]]; then
		fail "$svc: security_opt must include no-new-privileges:true exactly once (found $count key(s))"
		return
	fi
	opt="$(join_items security_opt "$block")"
	if [[ "$opt" != "no-new-privileges:true" ]]; then
		fail "$svc: security_opt must include no-new-privileges:true and no override (found '${opt:-none}')"
	fi
}

check_privileges() {
	local svc=$1 block=$2 count drops pids
	check_no_new_privileges "$svc" "$block"

	# cap_add re-adds a capability on top of cap_drop: ALL; privileged grants
	# the whole set. Hardened services must do neither.
	if [[ "$(key_count cap_add "$block")" -ne 0 ]]; then
		fail "$svc: cap_add is not allowed on a hardened service; document an exception instead"
	fi
	if [[ "$(key_count privileged "$block")" -ne 0 ]]; then
		fail "$svc: privileged is not allowed on a hardened service"
	fi

	count="$(key_count cap_drop "$block")"
	drops="$(join_items cap_drop "$block")"
	if [[ "$count" -ne 1 || "$drops" != "ALL" ]]; then
		fail "$svc: cap_drop must include ALL and nothing else exactly once (found $count key(s): '${drops:-none}')"
	fi

	count="$(key_count pids_limit "$block")"
	if [[ "$count" -eq 0 ]]; then
		fail "$svc: pids_limit is required (must be exactly 256; 0 and -1 mean unlimited in Docker)"
	elif [[ "$count" -ne 1 ]]; then
		fail "$svc: pids_limit must appear exactly once (found $count keys)"
	else
		pids="$(key_value pids_limit "$block")"
		if [[ "$pids" != "256" ]]; then
			fail "$svc: pids_limit must be 256, not '${pids:-none}' (0 and -1 mean unlimited in Docker)"
		fi
	fi
}

build_context() {
	local block=$1 short
	# Inline form, with an optional trailing comment on the key or the value.
	short="$(awk '
		/^    build:/ {
			sub(/^    build:[[:space:]]*/, "")
			sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, "")
			if ($0 != "") print
			exit
		}
	' <<<"$block")"
	if [[ -n "$short" ]]; then
		printf '%s\n' "$short"
		return
	fi
	awk '
		/^    build:[[:space:]]*(#.*)?$/ { in_build=1; next }
		in_build && /^    [A-Za-z0-9_.-]+:/ { in_build=0 }
		in_build && /^[[:space:]]+context:/ {
			sub(/^[[:space:]]+context:[[:space:]]*/, "")
			sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, "")
			print
			exit
		}
	' <<<"$block"
}

dockerfile_name() {
	awk '
		/^    build:[[:space:]]*(#.*)?$/ { in_build=1; next }
		in_build && /^    [A-Za-z0-9_.-]+:/ { in_build=0 }
		in_build && /^[[:space:]]+dockerfile:/ {
			sub(/^[[:space:]]+dockerfile:[[:space:]]*/, "")
			sub(/[[:space:]]+#.*$/, "")
			sub(/[[:space:]]+$/, "")
			print
			exit
		}
	' <<<"$1"
}

check_health_urls() {
	local svc=$1 block=$2 health url host
	health="$(healthcheck_block "$block")"
	while IFS= read -r url; do
		[[ -n "$url" ]] || continue
		host=""
		if [[ "$url" =~ ^https?://(\[[^]]+\]|[^/:]+) ]]; then
			host="${BASH_REMATCH[1]}"
		fi
		case "$host" in
		localhost | 127.0.0.1 | "[::1]") ;;
		*) fail "$svc: health check must not depend on the internet ($url)" ;;
		esac
	done < <(grep -oE 'https?://[^"'"'"' ]+' <<<"$health" || true)
}

check_weights_layer() {
	local svc=$1 dockerfile=$2 code_line weights_line
	# First COPY that ships application code: ignore dependency manifests and
	# weights copied in from another stage/directory.
	code_line="$(awk '
		BEGIN { IGNORECASE=1 }
		/^[[:space:]]*COPY[[:space:]]/ {
			if ($0 ~ /--from=/) next
			if ($0 ~ /(requirements[^[:space:]]*\.txt|pyproject\.toml|uv\.lock|poetry\.lock|package(-lock)?\.json|go\.(mod|sum)|prefetch\.py)/) next
			if ($0 ~ /(^|[[:space:]\/])(models|weights)\//) next
			print NR
			exit
		}
	' "$dockerfile")"
	weights_line="$(awk '
		BEGIN { IGNORECASE=1 }
		/^[[:space:]]*RUN[[:space:]]/ && /(snapshot_download|hf_hub_download|from_pretrained|SentenceTransformer\(|prefetch\.py|\/models|--weights|weights\/|wget[^#]*model|curl[^#]*model|ollama[[:space:]]+pull)/ {
			print NR
			exit
		}
	' "$dockerfile")"

	if [[ -z "$weights_line" ]]; then
		warn "$svc: no model-weights layer detected in $dockerfile; verify weights are baked before code COPY"
		return
	fi
	if [[ -n "$code_line" ]] && ((weights_line > code_line)); then
		fail "$svc: weights layer (line $weights_line) must come before the first code COPY (line $code_line) in $dockerfile"
	fi
}

check_service() {
	local svc=$1 block want var context dockerfile dfile
	block="$(service_block "$svc")"
	var="CLANKA_TAG_$(tr 'a-z-' 'A-Z_' <<<"$svc")"
	want='clanka/'"$svc"':${'"$var"':-${GIT_SHA:-dev}}'

	if ! grep -qF -- "image: $want" <<<"$block"; then
		fail "$svc: image must be $want"
	fi

	# Every service builds locally so its SHA tag exists for rollback. An
	# upstream image (for example openWakeWord) is wrapped in a small Dockerfile.
	context="$(build_context "$block")"
	if [[ -z "$context" ]]; then
		fail "$svc: build context is required so clanka/$svc:<sha> exists for rollback"
	fi

	if ! grep -qE '^    restart:[[:space:]]*unless-stopped[[:space:]]*(#.*)?$' <<<"$block"; then
		fail "$svc: restart must be unless-stopped"
	fi
	case " $HARDENED_SERVICES " in
	*" $svc "*)
		check_privileges "$svc" "$block"
		;;
	*)
		case " $NNP_ONLY_SERVICES " in
		*" $svc "*) check_no_new_privileges "$svc" "$block" ;;
		*)
			case " $FULLY_EXEMPT_SERVICES " in
			*" $svc "*) ;;
			*) fail "$svc: add it to HARDENED_SERVICES, NNP_ONLY_SERVICES or FULLY_EXEMPT_SERVICES in scripts/check-compose.sh with a reason" ;;
			esac
			;;
		esac
		;;
	esac
	if ! grep -qE '^    healthcheck:[[:space:]]*(#.*)?$' <<<"$block"; then
		fail "$svc: healthcheck is required"
		return
	fi
	if ! grep -qE '^[[:space:]]+test:' <<<"$(healthcheck_block "$block")"; then
		fail "$svc: healthcheck needs a test command"
	fi
	check_health_urls "$svc" "$block"
	if [[ "$svc" == "assistant" ]]; then
		check_assistant_search_dependency "$block"
	fi

	if [[ -z "$context" ]]; then
		return
	fi
	[[ "$context" == /* ]] || context="$ROOT/$context"
	dfile="$(dockerfile_name "$block")"
	[[ -n "$dfile" ]] || dfile="Dockerfile"
	dockerfile="$context/$dfile"
	if [[ ! -f "$dockerfile" ]]; then
		fail "$svc: build context $context has no $dfile"
		return
	fi

	case " $ML_SERVICES " in
	*" $svc "*) check_weights_layer "$svc" "$dockerfile" ;;
	esac
}

# The depends_on sub-block of a service (long form), one line per indented
# entry. Stop at the next service-level or sibling key so later blocks are not
# credited to depends_on.
depends_on_block() {
	awk '
		/^    depends_on:[[:space:]]*(#.*)?$/ { in_dep=1; next }
		in_dep && /^    [A-Za-z0-9_.-]+:/ { in_dep=0 }
		in_dep { print }
	' <<<"$1"
}

# web-search is an Agent-only, runtime-optional Tool (web.search). The runtime
# already fails that Tool cleanly when SearXNG is down, so gating the assistant
# on its health would block the whole stack on an optional service. A
# copy-paste that reintroduces the dependency must fail the check.
check_assistant_search_dependency() {
	local block=$1 dep
	dep="$(depends_on_block "$block")"
	if grep -qE '^[[:space:]]*web-search[[:space:]]*(:|[[:space:]#])' <<<"$dep"; then
		fail "assistant: web-search must not be a depends_on entry; it is an Agent-only, runtime-optional Tool (web.search) that fails cleanly when SearXNG is unhealthy. Drop it so an unhealthy SearXNG no longer blocks the assistant"
	fi
}

main() {
	[[ -f "$COMPOSE_FILE" ]] || {
		fail "missing compose file $COMPOSE_FILE"
		exit 1
	}
	local services svc
	services="$(service_names)"
	if [[ -z "$services" ]]; then
		fail "no services found in $COMPOSE_FILE"
	else
		while IFS= read -r svc; do
			[[ -n "$svc" ]] && check_service "$svc"
		done <<<"$services"
	fi

	if ((FAILURES > 0)); then
		printf 'check: %d failure(s), %d warning(s)\n' "$FAILURES" "$WARNINGS" >&2
		exit 1
	fi
	printf 'check: OK (%d service(s), %d warning(s))\n' \
		"$(wc -w <<<"$services" | tr -d ' ')" "$WARNINGS"
}

main "$@"
