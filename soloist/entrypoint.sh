#!/bin/bash
# Start Spotify's Soloist receiver as the host UID that owns the Pulse socket,
# and keep a metadata-only heartbeat for the Assistant's Health page.
#
# The API key is read from a mounted private file at runtime. It is passed to
# Soloist on its command line (the binary requires -k) but never appears in the
# Compose file, an image layer or this script's output.
set -euo pipefail

binary="${SOLOIST_BINARY:-/opt/soloist}"
key_file="${SOLOIST_API_KEY_FILE:-/run/secrets/soloist}"
data_dir="${SOLOIST_DATA_DIR:-/data}"
cache_dir="${SOLOIST_CACHE_DIR:-/cache}"
status_dir="${SOLOIST_STATUS_DIR:-/status}"
log_file="${SOLOIST_LOG_FILE:-$data_dir/soloist.log}"

die() {
    printf 'soloist: error: %s\n' "$*" >&2
    exit 1
}

[ -x "$binary" ] ||
    die "receiver binary missing at $binary; run scripts/soloist-setup.sh on the host"
[ -r "$key_file" ] ||
    die "API key file missing at $key_file; run scripts/soloist-setup.sh on the host"
key="$(cat "$key_file")"
[ -n "$key" ] || die "API key file $key_file is empty"

umask 077
mkdir -p "$data_dir" "$cache_dir" "$status_dir"

# Keep one previous log; it is private (the data directory is 0700) and only
# the heartbeat parses it. Console output is tailed from this file.
if [ -f "$log_file" ]; then
    mv -f "$log_file" "$log_file.1"
fi
: >"$log_file"
chmod 600 "$log_file"

name="${SOLOIST_DEVICE_NAME:-Clanka}"
initial="${SOLOIST_INITIAL_VOLUME:-50}"
cache_mb="${SOLOIST_CACHE_MB:-2048}"
ws_addr="${SOLOIST_WS_ADDR:-127.0.0.1:24880}"

if [[ ! "$initial" =~ ^[0-9]+$ ]] || ((initial > 100)); then
    die "SOLOIST_INITIAL_VOLUME must be 0-100"
fi
if [[ ! "$cache_mb" =~ ^[0-9]+$ ]] || ((cache_mb < 100)); then
    die "SOLOIST_CACHE_MB must be at least 100 (Soloist's minimum)"
fi

set -- -n "$name" -k "$key" -D "$data_dir" -C "$cache_dir" \
    -z "$cache_mb" -i "$initial" -w "$ws_addr"

# Optional native PipeWire routing. Empty keeps Soloist on the Pulse default
# sink, which is the same sink the Assistant speaks into.
if [ -n "${SOLOIST_PIPEWIRE_DEVICE:-}" ]; then
    set -- "$@" -d "$SOLOIST_PIPEWIRE_DEVICE"
fi

# Metadata-only heartbeat, separate from the credentials in data_dir.
SOLOIST_DATA_DIR="$data_dir" \
    SOLOIST_STATUS_DIR="$status_dir" \
    SOLOIST_STATUS_FILE="$status_dir/soloist.status" \
    SOLOIST_LOG_FILE="$log_file" \
    SOLOIST_DEVICE_NAME="$name" \
    SOLOIST_HEARTBEAT_INTERVAL="${SOLOIST_HEARTBEAT_INTERVAL:-10}" \
    heartbeat.sh &
heartbeat_pid=$!

# Soloist logs the build's client-expiry line at startup. Keep its output in
# the private log for the heartbeat, and mirror it to the container log.
"$binary" "$@" >>"$log_file" 2>&1 &
soloist_pid=$!
tail -n +1 -F "$log_file" &
tail_pid=$!

shutdown() {
    kill -TERM "$soloist_pid" 2>/dev/null || true
    wait "$soloist_pid" 2>/dev/null || true
    kill "$tail_pid" "$heartbeat_pid" 2>/dev/null || true
}
trap shutdown TERM INT

status=0
wait "$soloist_pid" || status=$?
shutdown
exit "$status"
