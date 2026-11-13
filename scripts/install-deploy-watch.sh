#!/usr/bin/env bash
# Install (or remove) the systemd system unit + timer that polls main on the Pi
# and deploys clanka-bot whenever it moves.
#
#   sudo scripts/install-deploy-watch.sh            install or update the timer
#   sudo scripts/install-deploy-watch.sh --status   show unit and timer state
#   sudo scripts/install-deploy-watch.sh --uninstall
#
# The timer runs scripts/deploy-watch.sh every five minutes as the account that
# owns this checkout, so a plain `git pull` deploy needs no root and no
# password. The unit is a *system* unit: a system unit with User= does not
# inherit the account's group memberships, hence the explicit
# SupplementaryGroups=docker — without it `docker compose` cannot reach the
# daemon socket.
#
# SYSTEMD_UNIT_DIR and SYSTEMCTL exist so the installer can be exercised
# without root against a scratch directory; the defaults are the real ones.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

UNIT_DIR="${SYSTEMD_UNIT_DIR:-/etc/systemd/system}"
SYSTEMCTL="${SYSTEMCTL:-systemctl}"
SERVICE_NAME="clanka-deploy.service"
TIMER_NAME="clanka-deploy.timer"
SERVICE_UNIT="$UNIT_DIR/$SERVICE_NAME"
TIMER_UNIT="$UNIT_DIR/$TIMER_NAME"

log() { printf 'deploy-watch-install: %s\n' "$*"; }
warn() { printf 'deploy-watch-install: warning: %s\n' "$*" >&2; }
die() {
	printf 'deploy-watch-install: error: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: scripts/install-deploy-watch.sh [--user NAME]
       scripts/install-deploy-watch.sh --status
       scripts/install-deploy-watch.sh --uninstall

Install (or refresh) clanka-deploy.service + clanka-deploy.timer, which run
scripts/deploy-watch.sh every five minutes to deploy main when it moves.
Must be run with sudo; the timer runs as the invoking user.

Options:
  --user NAME   run the timer as NAME (default: $SUDO_USER)
  --uninstall   stop and remove the timer and service units
  --status      show whether the units exist, are enabled and when they fire
  -h, --help    show this help

Environment:
  SYSTEMD_UNIT_DIR  where to write the units (default /etc/systemd/system)
  SYSTEMCTL         systemctl binary to call (default systemctl)
EOF
}

require_root() {
	if [[ -n "${SYSTEMD_UNIT_DIR:-}" ]]; then
		return
	fi
	[[ "$EUID" -eq 0 ]] || die "must run with sudo (or set SYSTEMD_UNIT_DIR for a dry install)"
}

# The account the timer runs as. $SUDO_USER is the person who invoked sudo;
# without it we refuse rather than silently guess.
resolve_user() {
	local requested=$1 owner bare
	if [[ -n "$requested" ]]; then
		printf '%s\n' "$requested"
		return
	fi
	if [[ -n "${SUDO_USER:-}" ]]; then
		printf '%s\n' "$SUDO_USER"
		return
	fi
	owner="$(stat -c '%U' "$REPO_ROOT" 2>/dev/null || true)"
	if [[ -n "$owner" && "$owner" != "root" && "$owner" != "UNKNOWN" ]]; then
		warn "SUDO_USER is unset; the checkout is owned by '$owner'"
	fi
	bare="${owner:-unknown}"
	die "cannot tell which user to run as; pass --user <name> (checkout owner: $bare)"
}

check_repo() {
	local path=$1
	[[ -d "$REPO_ROOT/.git" ]] || die "$REPO_ROOT is not a git checkout"
	[[ -x "$REPO_ROOT/scripts/deploy-watch.sh" ]] ||
		die "$REPO_ROOT/scripts/deploy-watch.sh is missing or not executable"
	case "$path" in
	*[[:space:]]* | *%*) die "repo path contains whitespace or '%', which systemd units cannot express: $path" ;;
	esac
}

install_units() {
	local user=$1
	mkdir -p "$UNIT_DIR" || die "cannot create $UNIT_DIR"

	{
		printf '%s\n' '[Unit]'
		printf '%s\n' 'Description=clanka-bot auto-deploy (deploy main when it moves)'
		printf '%s\n' 'Wants=network-online.target docker.service'
		printf '%s\n' 'After=network-online.target docker.service'
		printf '\n%s\n' '[Service]'
		printf '%s\n' 'Type=oneshot'
		printf 'User=%s\n' "$user"
		# A system unit with User= keeps none of the account's groups, so the
		# docker group must be granted here or compose cannot reach the socket.
		printf '%s\n' 'SupplementaryGroups=docker'
		printf 'WorkingDirectory=%s\n' "$REPO_ROOT"
		printf 'ExecStart=%s/scripts/deploy-watch.sh\n' "$REPO_ROOT"
		printf '%s\n' 'Environment=DEPLOY_HEALTH_TIMEOUT=300'
		printf '%s\n' 'TimeoutStartSec=3600'
	} >"$SERVICE_UNIT" || die "cannot write $SERVICE_UNIT"

	{
		printf '%s\n' '[Unit]'
		printf '%s\n' 'Description=Poll main and deploy clanka-bot when it moves'
		printf '\n%s\n' '[Timer]'
		printf '%s\n' 'OnBootSec=3min'
		printf '%s\n' 'OnUnitActiveSec=5min'
		printf '%s\n' 'RandomizedDelaySec=30'
		printf '%s\n' 'Unit=clanka-deploy.service'
		printf '\n%s\n' '[Install]'
		printf '%s\n' 'WantedBy=timers.target'
	} >"$TIMER_UNIT" || die "cannot write $TIMER_UNIT"

	log "wrote $SERVICE_UNIT and $TIMER_UNIT (run as $user)"

	"$SYSTEMCTL" daemon-reload || die "systemctl daemon-reload failed"
	"$SYSTEMCTL" enable --now "$TIMER_NAME" || die "could not enable $TIMER_NAME"
	# Re-running the installer picks up edited units; restart so the next tick
	# is scheduled from now.
	"$SYSTEMCTL" restart "$TIMER_NAME" || die "could not restart $TIMER_NAME"

	cat <<EOF
deploy-watch-install: installed; main is now polled every 5 minutes.
  next runs      systemctl list-timers $TIMER_NAME
  follow a run   journalctl -u clanka-deploy -f
  one run now    sudo systemctl start clanka-deploy.service
  pause deploys  systemctl stop $TIMER_NAME  (re-enable with: systemctl start $TIMER_NAME)
EOF
}

uninstall_units() {
	"$SYSTEMCTL" disable --now "$TIMER_NAME" 2>/dev/null || true
	rm -f "$TIMER_UNIT" "$SERVICE_UNIT"
	"$SYSTEMCTL" daemon-reload || true
	log "removed $TIMER_UNIT and $SERVICE_UNIT"
}

show_status() {
	local unit
	for unit in "$SERVICE_UNIT" "$TIMER_UNIT"; do
		if [[ -f "$unit" ]]; then
			log "present: $unit"
		else
			log "missing: $unit"
		fi
	done
	if ! command -v "$SYSTEMCTL" >/dev/null 2>&1; then
		warn "systemctl not found ($SYSTEMCTL); skipping unit state"
		return
	fi
	printf -- '--- is-enabled / is-active ---\n'
	"$SYSTEMCTL" is-enabled "$TIMER_NAME" 2>&1 || true
	"$SYSTEMCTL" is-active "$TIMER_NAME" 2>&1 || true
	printf -- '--- list-timers ---\n'
	"$SYSTEMCTL" list-timers "$TIMER_NAME" --no-pager 2>&1 || true
}

main() {
	local action=install user=""

	while (($# > 0)); do
		case "$1" in
		-h | --help)
			usage
			exit 0
			;;
		--uninstall)
			action=uninstall
			shift
			;;
		--status)
			action=status
			shift
			;;
		--user)
			[[ $# -ge 2 ]] || die "--user needs a name"
			user=$2
			shift 2
			;;
		--user=*)
			user=${1#--user=}
			shift
			;;
		*)
			usage >&2
			exit 2
			;;
		esac
	done

	case "$action" in
	status)
		show_status
		return 0
		;;
	uninstall)
		require_root
		uninstall_units
		return 0
		;;
	esac

	require_root
	check_repo "$REPO_ROOT"
	user="$(resolve_user "$user")"
	[[ -n "$user" ]] || die "the run-as user is empty"
	if [[ "$user" == "root" || "$user" == "0" ]]; then
		die "refusing to run the deploy timer as root; pass --user <name>"
	fi
	id -u "$user" >/dev/null 2>&1 || die "no such user: $user"
	if command -v getent >/dev/null 2>&1 && ! getent group docker >/dev/null 2>&1; then
		warn "the docker group does not exist; the unit will not be able to reach the daemon"
	fi

	install_units "$user"
}

main "$@"
