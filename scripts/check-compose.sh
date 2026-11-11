#!/usr/bin/env bash
# Check the deployment conventions the stack relies on:
#   - every service has image clanka/<service>:${GIT_SHA:-dev}
#   - every service restarts unless stopped
#   - every service has a health check, and no health check calls the internet
#   - a locally built ML service downloads model weights before its first
#     COPY of application code (so code edits do not bust the weights layer)
#
# The compose file is expected to keep the flat layout used here: top-level
# `services:` with two-space service keys. The script only reads files; it does
# not need Docker, so `make check` also works before the stack is built.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE_FILE="${COMPOSE_FILE:-$ROOT/docker-compose.yml}"
ML_SERVICES="wakeword stt tts embed"

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
			if ($0 ~ /(requirements[^[:space:]]*\.txt|pyproject\.toml|uv\.lock|poetry\.lock|package(-lock)?\.json|go\.(mod|sum))/) next
			if ($0 ~ /(^|[[:space:]\/])(models|weights)\//) next
			print NR
			exit
		}
	' "$dockerfile")"
	weights_line="$(awk '
		BEGIN { IGNORECASE=1 }
		/^[[:space:]]*RUN[[:space:]]/ && /(snapshot_download|hf_hub_download|from_pretrained|\/models|--weights|weights\/|wget[^#]*model|curl[^#]*model|ollama[[:space:]]+pull)/ {
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
	if ! grep -qE '^    restart:[[:space:]]*unless-stopped[[:space:]]*$' <<<"$block"; then
		fail "$svc: restart must be unless-stopped"
	fi
	if ! grep -qE '^    healthcheck:[[:space:]]*$' <<<"$block"; then
		fail "$svc: healthcheck is required"
		return
	fi
	if ! grep -qE '^[[:space:]]+test:' <<<"$(healthcheck_block "$block")"; then
		fail "$svc: healthcheck needs a test command"
	fi
	check_health_urls "$svc" "$block"

	case " $ML_SERVICES " in
	*" $svc "*)
		context="$(build_context "$block")"
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
		check_weights_layer "$svc" "$dockerfile"
		;;
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
