#!/bin/sh
# Write Soloist's metadata-only heartbeat. It never reads the paired data or
# the API key; the only fields are the daemon's login/active state, the device
# name, the build's remaining days and a timestamp. The Assistant mounts the
# status directory read-only.
set -eu

data_dir="${SOLOIST_DATA_DIR:-/data}"
status_dir="${SOLOIST_STATUS_DIR:-/status}"
status_file="${SOLOIST_STATUS_FILE:-$status_dir/soloist.status}"
log_file="${SOLOIST_LOG_FILE:-$data_dir/soloist.log}"
binary="${SOLOIST_BINARY:-/opt/soloist}"
device="${SOLOIST_DEVICE_NAME:-Clanka}"
interval="${SOLOIST_HEARTBEAT_INTERVAL:-10}"

# The build prints "client expires in N days" once at startup, with a
# timestamp. Turn that into remaining days at read time, so a long-running
# process does not keep reporting the value it saw on its first day.
expiry_days() {
    line="$(sed -n 's/^\([0-9][0-9-]* [0-9][0-9:]*\)\.[0-9]*:[[:space:]]*.*client expires in \([0-9][0-9]*\) days.*/\1|\2/p' "$log_file" 2>/dev/null | head -n 1)"
    if [ -z "$line" ]; then
        printf 'unknown'
        return
    fi
    stamp="${line%%|*}"
    days="${line#*|}"
    expires_at="$(date -d "$stamp $days days" +%s 2>/dev/null)" || {
        printf 'unknown'
        return
    }
    if [ -z "$expires_at" ]; then
        printf 'unknown'
        return
    fi
    now="$(date +%s)"
    diff=$((expires_at - now))
    if [ "$diff" -ge 0 ]; then
        left=$((diff / 86400))
    else
        # Floor a past expiry, so any time past the date reads as expired.
        left=$(( -(( -diff + 86399) / 86400) ))
    fi
    printf '%s' "$left"
}

write_status() {
    state=unavailable
    session=unknown
    if status="$("$binary" ctl status -D "$data_dir" 2>/dev/null)"; then
        case "$status" in
            *"logged in: yes"*) state=paired ;;
            *"logged in: no"*) state=unpaired ;;
        esac
        case "$status" in
            *"active device: yes"*) session=active ;;
            *"active device: no"*) session=idle ;;
        esac
    fi

    umask 077
    mkdir -p "$status_dir"
    # Atomic replace: the Assistant reads either the previous complete file or
    # the new one, never a partial write.
    {
        printf 'version=2\n'
        printf 'state=%s\n' "$state"
        printf 'session=%s\n' "$session"
        printf 'device=%s\n' "$device"
        printf 'expires_days=%s\n' "$(expiry_days)"
        printf 'updated=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } >"$status_file.tmp"
    mv "$status_file.tmp" "$status_file"
    chmod 644 "$status_file"
}

write_status
if [ "${SOLOIST_ONESHOT:-0}" = "1" ]; then
    exit 0
fi
case "$interval" in
    ''|*[!0-9]*) interval=10 ;;
esac
while :; do
    sleep "$interval"
    write_status
done
