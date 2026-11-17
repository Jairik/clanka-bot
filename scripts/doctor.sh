#!/usr/bin/env bash
# Preflight health and configuration checks for the deployment host.
#
# Inspects Docker, .env, PulseAudio/PipeWire sockets and cookies, runtime
# directories, exports permissions, disk space, Soloist receiver keys, D-Bus,
# git status, and Tailscale serve proxy mappings.
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FAILURES=0
WARNINGS=0

# Record and print a successful check.
check_ok() {
	local num=$1 name=$2 extra=${3:-}
	if [[ -n "$extra" ]]; then
		printf '%s. %s: ok (%s)\n' "$num" "$name" "$extra"
	else
		printf '%s. %s: ok\n' "$num" "$name"
	fi
}

# Record and print a warning with its remediation command.
check_warn() {
	local num=$1 name=$2 msg=$3 fix=$4
	printf '%s. %s: warn: %s\n' "$num" "$name" "$msg"
	printf '   fix: %s\n' "$fix"
	WARNINGS=$((WARNINGS + 1))
}

# Record and print a fatal preflight failure with its remediation command.
check_fail() {
	local num=$1 name=$2 msg=$3 fix=$4
	printf '%s. %s: fail: %s\n' "$num" "$name" "$msg"
	printf '   fix: %s\n' "$fix"
	FAILURES=$((FAILURES + 1))
}

# Verify Docker availability, Compose v2 support, and daemon responsiveness.
check_docker() {
	if ! command -v docker >/dev/null 2>&1 || ! docker --version >/dev/null 2>&1; then
		check_fail 1 "docker + compose v2" "docker is not installed" "install Docker (curl -fsSL https://get.docker.com | sh)"
		return
	fi

	local ver
	ver="$(docker compose version 2>&1 || true)"
	if [[ $? -ne 0 || -z "$ver" ]]; then
		check_fail 1 "docker + compose v2" "docker compose plugin is not installed" "sudo apt install docker-compose-plugin"
		return
	fi

	if [[ "$ver" == *"v1."* || "$ver" == *"version 1."* ]]; then
		check_fail 1 "docker + compose v2" "docker compose is v1 ($ver), compose v2 required" "sudo apt install docker-compose-plugin"
		return
	fi

	if ! docker info >/dev/null 2>&1; then
		check_fail 1 "docker + compose v2" "docker daemon is unreachable" "sudo systemctl enable --now docker && sudo usermod -aG docker $(id -un)"
		return
	fi

	check_ok 1 "docker + compose v2"
}

# Verify .env presence and dashboard owner configuration without echoing secrets.
check_dotenv() {
	local env_file="${ENV_FILE:-$ROOT/.env}"
	if [[ ! -f "$env_file" ]]; then
		check_fail 2 ".env configuration" ".env file not found" "cp .env.example .env && chmod 600 .env"
		return
	fi

	local owner
	owner="$(grep -E '^[[:space:]]*ASSISTANT_DASHBOARD_OWNER=' "$env_file" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
	if [[ -z "$owner" ]]; then
		check_warn 2 ".env configuration" "ASSISTANT_DASHBOARD_OWNER is empty (Dashboard is off; assistant still runs)" "set ASSISTANT_DASHBOARD_OWNER=<login> in .env"
		return
	fi

	check_ok 2 ".env configuration"
}

# Verify PulseAudio native socket ownership and cookie accessibility.
check_pulse() {
	local env_file="${ENV_FILE:-$ROOT/.env}"
	local target_uid="${ASSISTANT_UID:-}"
	if [[ -z "$target_uid" && -f "$env_file" ]]; then
		target_uid="$(grep -E '^[[:space:]]*ASSISTANT_UID=' "$env_file" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
	fi
	target_uid="${target_uid:-1000}"

	local target_gid="${ASSISTANT_GID:-}"
	if [[ -z "$target_gid" && -f "$env_file" ]]; then
		target_gid="$(grep -E '^[[:space:]]*ASSISTANT_GID=' "$env_file" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
	fi
	target_gid="${target_gid:-1000}"

	local runtime_dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
	local pulse_sock="$runtime_dir/pulse/native"
	local cookie="${PULSE_COOKIE:-$HOME/.config/pulse/cookie}"

	if [[ ! -e "$pulse_sock" && ! -S "$pulse_sock" ]]; then
		check_fail 3 "pulse socket and cookie" "Pulse native socket not found at $pulse_sock (the assistant is deaf)" "start PipeWire/PulseAudio session for UID $(id -u) or set XDG_RUNTIME_DIR"
		return
	fi

	local actual_stat expected_stat
	actual_stat="$(stat -c '%u:%g' "$pulse_sock" 2>/dev/null || true)"
	expected_stat="$target_uid:$target_gid"
	if [[ "$actual_stat" != "$expected_stat" ]]; then
		check_fail 3 "pulse socket and cookie" "stat mismatch: $pulse_sock is owned by $actual_stat, expected $expected_stat" "set ASSISTANT_UID=${actual_stat%:*} ASSISTANT_GID=${actual_stat#*:} in .env"
		return
	fi

	if [[ ! -r "$cookie" ]]; then
		check_fail 3 "pulse socket and cookie" "Pulse cookie not readable at $cookie" "ensure $cookie exists and is readable by deploy user"
		return
	fi

	check_ok 3 "pulse socket and cookie"
}

# Verify user runtime directory presence and session lingering.
check_runtime_dir() {
	local runtime_dir="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}"
	if [[ ! -d "$runtime_dir" ]]; then
		local curr_user
		curr_user="$(id -un 2>/dev/null || echo "$(id -u)")"
		check_warn 4 "user runtime directory" "$runtime_dir does not exist (PipeWire session will not survive reboot without login)" "sudo loginctl enable-linger $curr_user"
		return
	fi

	check_ok 4 "user runtime directory"
}

# Verify ./exports existence and deploy-user write permissions.
check_exports() {
	local exports_dir="${EXPORTS_DIR:-$ROOT/exports}"
	local curr_user
	curr_user="$(id -un 2>/dev/null || echo "$(id -u)")"

	if [[ ! -e "$exports_dir" ]]; then
		check_fail 5 "exports directory" "$exports_dir does not exist (bundle export is broken)" "mkdir -p ./exports"
		return
	fi

	if [[ ! -d "$exports_dir" ]]; then
		check_fail 5 "exports directory" "$exports_dir is not a directory (bundle export is broken)" "rm -f ./exports && mkdir -p ./exports"
		return
	fi

	if [[ ! -w "$exports_dir" ]]; then
		check_fail 5 "exports directory" "$exports_dir is not writable by deploy user $curr_user (bundle export is broken)" "sudo chown -R $(id -u):$(id -g) ./exports"
		return
	fi

	check_ok 5 "exports directory"
}

# Check Docker root filesystem headroom and quantify clanka/* image sets.
check_disk_space() {
	local docker_root
	docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
	if [[ -z "$docker_root" ]]; then
		docker_root="/var/lib/docker"
	fi

	local prune_hint
	if [[ -f "$ROOT/scripts/prune-images.sh" ]] && grep -q '^[[:space:]]*prune-images:' "$ROOT/Makefile" 2>/dev/null; then
		prune_hint="make prune-images"
	else
		prune_hint='docker image rmi $(docker image ls -q "clanka/*")'
	fi

	local image_stats
	image_stats="$(docker image ls --format '{{.Repository}}|{{.Tag}}|{{.Size}}|{{.ID}}' 2>/dev/null | awk -F'|' '
	function to_bytes(s,   v, unit) {
		v = s
		sub(/[[:alpha:]]+$/, "", v)
		unit = s
		sub(/^[0-9.]+/, "", unit)
		gsub(/[[:space:]]/, "", unit)
		unit = toupper(unit)
		if (unit == "GB" || unit == "GIB") return v * 1024 * 1024 * 1024
		if (unit == "MB" || unit == "MIB") return v * 1024 * 1024
		if (unit == "KB" || unit == "KIB") return v * 1024
		if (unit == "B") return v
		return v + 0
	}
	$1 ~ /^clanka\// {
		tags[$2] = 1
		id = $4
		if (!seen[id]) {
			seen[id] = 1
			total_bytes += to_bytes($3)
		}
	}
	END {
		tag_count = 0
		for (t in tags) tag_count++
		if (total_bytes >= 1024 * 1024 * 1024) {
			total_str = sprintf("%.1f GB", total_bytes / (1024 * 1024 * 1024))
		} else if (total_bytes >= 1024 * 1024) {
			total_str = sprintf("%.1f MB", total_bytes / (1024 * 1024))
		} else if (total_bytes >= 1024) {
			total_str = sprintf("%.1f KB", total_bytes / 1024)
		} else {
			total_str = sprintf("%d B", total_bytes)
		}
		printf "%d|%s\n", tag_count, total_str
	}' || echo "0|0 B")"

	local set_count="${image_stats%%|*}"
	local total_size="${image_stats#*|}"

	local avail_kb
	avail_kb="$(df -Pk "$docker_root" 2>/dev/null | awk 'NR==2 {print $4}' || true)"
	if [[ -z "$avail_kb" ]]; then
		avail_kb="$(df -Pk "$ROOT" 2>/dev/null | awk 'NR==2 {print $4}' || true)"
	fi

	if [[ -z "$avail_kb" ]]; then
		check_fail 6 "docker root free space" "could not determine available disk space on $docker_root" "ensure $docker_root is accessible"
		return
	fi

	local avail_gb=$((avail_kb / 1024 / 1024))
	local details="${avail_gb} GB free on ${docker_root}; ${set_count} clanka/* image set(s), ${total_size} total"

	if ((avail_gb < 5)); then
		check_fail 6 "docker root free space" "${details} (< 5 GB required)" "reclaim disk space with '${prune_hint}'"
	elif ((avail_gb < 15)); then
		check_warn 6 "docker root free space" "${details} (< 15 GB recommended)" "reclaim disk space with '${prune_hint}'"
	else
		check_ok 6 "docker root free space" "${details}; pointer: ${prune_hint}"
	fi
}

# Verify Soloist Spotify receiver configuration and certificate/build expiry.
check_soloist() {
	local soloist_dir="${SOLOIST_DIR:-$HOME/.cache/clanka/soloist}"
	local key_file="${SOLOIST_API_KEY_FILE:-$soloist_dir/api-key}"
	local env_file="${ENV_FILE:-$ROOT/.env}"

	if [[ ! -s "$key_file" && -f "$env_file" ]]; then
		local env_key_file
		env_key_file="$(grep -E '^[[:space:]]*SOLOIST_API_KEY_FILE=' "$env_file" 2>/dev/null | tail -n 1 | cut -d= -f2- | tr -d "\"'\r " || true)"
		if [[ -n "$env_key_file" && -s "$env_key_file" ]]; then
			key_file="$env_key_file"
		fi
	fi

	if [[ ! -s "$key_file" ]]; then
		check_ok 7 "soloist receiver" "not configured (profile off)"
		return
	fi

	local status_file="${SOLOIST_STATUS_FILE:-$soloist_dir/status/soloist.status}"
	local expires=""
	if [[ -f "$status_file" ]]; then
		expires="$(sed -n 's/^[[:space:]]*expires_days=//p' "$status_file" 2>/dev/null | head -n 1 | tr -d '\r ' || true)"
	fi

	if [[ -z "$expires" || "$expires" == "unknown" ]]; then
		local log_file="${SOLOIST_DATA_DIR:-$soloist_dir/data}/soloist.log"
		if [[ -f "$log_file" ]]; then
			local line stamp days expires_at now diff
			line="$(sed -n 's/^\([0-9][0-9-]* [0-9][0-9:]*\)\.[0-9]*:[[:space:]]*.*client expires in \([0-9][0-9]*\) days.*/\1|\2/p' "$log_file" 2>/dev/null | head -n 1 || true)"
			if [[ -n "$line" ]]; then
				stamp="${line%%|*}"
				days="${line#*|}"
				expires_at="$(date -d "$stamp $days days" +%s 2>/dev/null || true)"
				if [[ -n "$expires_at" ]]; then
					now="$(date +%s)"
					diff=$((expires_at - now))
					if ((diff >= 0)); then
						expires=$((diff / 86400))
					else
						expires=$(( -(( -diff + 86399) / 86400) ))
					fi
				fi
			fi
		fi
	fi

	case "$expires" in
	'' | unknown)
		check_warn 7 "soloist receiver" "key present but receiver status/expiry unknown" "run 'make soloist' to verify receiver installation"
		;;
	-[0-9]*)
		check_warn 7 "soloist receiver" "key present but build is expired (${expires} days remaining)" "run 'make soloist-update' to update receiver binary"
		;;
	*)
		if [[ "$expires" =~ ^[0-9]+$ ]]; then
			if ((expires < 7)); then
				check_warn 7 "soloist receiver" "key present but build expires in ${expires} day(s) (< 7 days)" "run 'make soloist-update' to update receiver binary"
			else
				check_ok 7 "soloist receiver" "key present, build expires in ${expires} days"
			fi
		else
			check_warn 7 "soloist receiver" "key present but invalid expires_days: ${expires}" "run 'make soloist-update' to update receiver binary"
		fi
		;;
	esac
}

# Verify system D-Bus socket availability for Bluetooth Low Energy peripherals.
check_dbus() {
	local dbus_sock="${DOCTOR_DBUS_SOCKET:-/run/dbus/system_bus_socket}"
	if [[ -e "$dbus_sock" || -S "$dbus_sock" ]]; then
		check_ok 8 "d-bus system bus socket"
	else
		check_warn 8 "d-bus system bus socket" "$dbus_sock not present (BLE lights disabled)" "sudo systemctl start dbus"
	fi
}

# Verify clean git status and main branch alignment.
check_git() {
	local dirty branch
	dirty="$(git -C "$ROOT" status --porcelain 2>/dev/null || true)"
	branch="$(git -C "$ROOT" symbolic-ref --short HEAD 2>/dev/null || git -C "$ROOT" branch --show-current 2>/dev/null || true)"

	if [[ -n "$dirty" ]]; then
		check_fail 9 "git status and branch" "working tree has uncommitted changes" "commit, stash, or discard local changes"
	elif [[ "$branch" != "main" ]]; then
		check_warn 9 "git status and branch" "on branch '${branch:-detached}', expected 'main' for production deploy" "git checkout main"
	else
		check_ok 9 "git status and branch"
	fi
}

# Verify Tailscale reverse proxy configuration for port 8082 if installed.
check_tailscale() {
	if ! command -v tailscale >/dev/null 2>&1 || ! tailscale version >/dev/null 2>&1; then
		check_ok 10 "tailscale serve mapping" "tailscale not installed"
		return
	fi

	local serve_out
	serve_out="$(tailscale serve status 2>&1 || true)"
	if grep -q '8082' <<<"$serve_out"; then
		check_ok 10 "tailscale serve mapping" "port 8082 mapped"
	else
		check_warn 10 "tailscale serve mapping" "no tailscale serve mapping for port 8082 found" "tailscale serve --bg 8082"
	fi
}

# Execute all preflight checks in order and return non-zero only on failure.
main() {
	check_docker
	check_dotenv
	check_pulse
	check_runtime_dir
	check_exports
	check_disk_space
	check_soloist
	check_dbus
	check_git
	check_tailscale

	if ((FAILURES > 0)); then
		printf '\ndoctor: %d check(s) failed, %d warning(s)\n' "$FAILURES" "$WARNINGS" >&2
		exit 1
	fi

	if ((WARNINGS > 0)); then
		printf '\ndoctor: OK (0 failures, %d warning(s))\n' "$WARNINGS"
	else
		printf '\ndoctor: OK (all checks passed)\n'
	fi
	exit 0
}

main "$@"
