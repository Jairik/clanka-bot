#!/bin/sh
# Compose healthcheck. It proves the daemon answers on its loopback control
# socket and that the metadata heartbeat is fresh. An unpaired receiver is
# still healthy (it advertises for pairing); an expired build degrades playback
# and is surfaced via the Dashboard rather than failing Compose, so an expired
# build does not break full-stack deploys. No network call, so a dropped LAN
# does not make it flap.
set -eu

binary="${SOLOIST_BINARY:-/opt/soloist}"
data_dir="${SOLOIST_DATA_DIR:-/data}"
status="${SOLOIST_STATUS_FILE:-${SOLOIST_STATUS_DIR:-/status}/soloist.status}"

status_out="$("$binary" ctl status -D "$data_dir" 2>/dev/null)" || exit 1
case "$status_out" in
    *"running"*) ;;
    *) exit 1 ;;
esac

[ -s "$status" ] || exit 1
now="$(date +%s)"
mtime="$(stat -c %Y "$status")"
[ $((now - mtime)) -lt 60 ] || exit 1

expires="$(sed -n 's/^expires_days=//p' "$status" | head -n 1)"
case "$expires" in
    ''|unknown) ;;
    -[0-9]*[!0-9]*) exit 1 ;;
    # Expired builds (negative days) remain healthy for Compose; product
    # surfaces (Health page / Dashboard) report the expired state.
    -[0-9]*) ;;
    *[!0-9]*) exit 1 ;;
    # Zero means less than a day remains; non-negative numbers are valid expiry.
    *) [ "$expires" -ge 0 ] 2>/dev/null || exit 1 ;;
esac
