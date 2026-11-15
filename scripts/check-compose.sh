#!/usr/bin/env bash
# Check the deployment conventions the stack relies on:
#   - every service builds locally so it has a clanka/<service>:${GIT_SHA:-dev}
#     image (upstream images are wrapped in a small Dockerfile)
#   - every service restarts unless stopped
#   - every service has a health check, and no health check calls the internet
#   - a locally built ML service downloads model weights before its first
#     COPY of application code (so code edits do not bust the weights layer)
#   - every service that only computes or serves content drops all Linux
#     capabilities, refuses privilege gain and caps its PIDs; soloist (host
#     networking) and dashboard-web (static files) are explicit exceptions
#
# The compose file is expected to keep the flat layout used here: top-level
# `services:` with two-space service keys. The script only reads files; it does
# not need Docker, so `make check` also works before the stack is built.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$ROOT/docker-compose.yml}"
ML_SERVICES="wakeword stt tts embed"
# Services that need no capability and no privilege gain. assistant handles
# untrusted text but still needs none; it keeps its system D-Bus mount for BLE.
HARDENED_SERVICES="assistant embed wakeword stt tts web-search"
# Documented exceptions: soloist already sets no-new-privileges and needs host
# networking for mDNS; dashboard-web is a static file server. Neither gains the
# new options, and a new service must be added to one list on purpose.
PRIVILEGE_EXEMPT_SERVICES="dashboard-web soloist"

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
		/^services:[[:space:]]*$/ { in_services=1; next }
		in_services && /^[^[:space:]#]/ { in_services=0 }
		in_services && /^  [A-Za-z0-9_.-]+:[[:space:]]*$/ {
			name=$0
			sub(/^  /, "", name)
			sub(/:.*/, "", name)
			print name
		}
	' "$COMPOSE_FILE"
}

service_block() {
	awk -v svc="$1" '
		/^  [A-Za-z0-9_.-]+:[[:space:]]*$/ {
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
		/^    healthcheck:[[:space:]]*$/ { in_health=1; next }
		in_health && /^    [A-Za-z0-9_.-]+:[[:space:]]*$/ { in_health=0 }
		in_health { print }
	' <<<"$1"
}

# YAML block-list items under a service key, one per line:
#   list_items security_opt "<service block>"
list_items() {
	local key=$1 block=$2
	awk -v key="$key" '
		$0 ~ "^    " key ":[[:space:]]*$" { inside=1; next }
		inside && /^    [A-Za-z0-9_.-]+:[[:space:]]*$/ { inside=0 }
		inside && /^[[:space:]]+-[[:space:]]*/ {
			sub(/^[[:space:]]+-[[:space:]]*/, "")
			print
		}
	' <<<"$block"
}

check_privileges() {
	local svc=$1 block=$2
	if ! list_items security_opt "$block" | grep -qxF 'no-new-privileges:true'; then
		fail "$svc: security_opt must include no-new-privileges:true"
	fi
	if ! list_items cap_drop "$block" | grep -qxF 'ALL'; then
		fail "$svc: cap_drop must include ALL"
	fi
	if ! grep -qE '^    pids_limit:[[:space:]]*[0-9]+[[:space:]]*$' <<<"$block"; then
		fail "$svc: pids_limit is required (a positive integer)"
	fi
}

build_context() {
	local block=$1 short
	short="$(awk '/^    build:[[:space:]]*[^[:space:]]/ {
		sub(/^    build:[[:space:]]*/, "");
		print;
		exit
	}' <<<"$block")"
	if [[ -n "$short" ]]; then
		printf '%s\n' "$short"
		return
	fi
	awk '
		/^    build:[[:space:]]*$/ { in_build=1; next }
		in_build && /^    [A-Za-z0-9_.-]+:[[:space:]]*$/ { in_build=0 }
		in_build && /^[[:space:]]+context:/ {
			sub(/^[[:space:]]+context:[[:space:]]*/, "")
			print
			exit
		}
	' <<<"$block"
}

dockerfile_name() {
	awk '
		/^    build:[[:space:]]*$/ { in_build=1; next }
		in_build && /^    [A-Za-z0-9_.-]+:[[:space:]]*$/ { in_build=0 }
		in_build && /^[[:space:]]+dockerfile:/ {
			sub(/^[[:space:]]+dockerfile:[[:space:]]*/, "")
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
	local svc=$1 block want context dockerfile dfile
	block="$(service_block "$svc")"
	want='clanka/'"$svc"':${GIT_SHA:-dev}'

	if ! grep -qF -- "image: $want" <<<"$block"; then
		fail "$svc: image must be $want"
	fi

	# Every service builds locally so its SHA tag exists for rollback. An
	# upstream image (for example openWakeWord) is wrapped in a small Dockerfile.
	context="$(build_context "$block")"
	if [[ -z "$context" ]]; then
		fail "$svc: build context is required so clanka/$svc:<sha> exists for rollback"
	fi

	if ! grep -qE '^    restart:[[:space:]]*unless-stopped[[:space:]]*$' <<<"$block"; then
		fail "$svc: restart must be unless-stopped"
	fi
	case " $HARDENED_SERVICES " in
	*" $svc "*)
		check_privileges "$svc" "$block"
		;;
	*)
		case " $PRIVILEGE_EXEMPT_SERVICES " in
		*" $svc "*) ;;
		*) fail "$svc: add it to HARDENED_SERVICES or PRIVILEGE_EXEMPT_SERVICES in scripts/check-compose.sh with a reason" ;;
		esac
		;;
	esac
	if ! grep -qE '^    healthcheck:[[:space:]]*$' <<<"$block"; then
		fail "$svc: healthcheck is required"
		return
	fi
	if ! grep -qE '^[[:space:]]+test:' <<<"$(healthcheck_block "$block")"; then
		fail "$svc: healthcheck needs a test command"
	fi
	check_health_urls "$svc" "$block"

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
