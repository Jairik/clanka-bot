#!/bin/sh
# Compose healthcheck. The status heartbeat proves the entrypoint's writer is
# alive; the process scan proves librespot and its Avahi responder are still
# running. No network call, so a dropped LAN does not make the stack flap.
set -eu

for name in librespot avahi-daemon dbus-daemon; do
    found=0
    for comm in /proc/[0-9]*/comm; do
        if [ -r "$comm" ] && [ "$(cat "$comm" 2>/dev/null)" = "$name" ]; then
            found=1
            break
        fi
    done
    [ "$found" = "1" ] || exit 1
done

status=/status/librespot.status
[ -s "$status" ] || exit 1
now="$(date +%s)"
mtime="$(stat -c %Y "$status")"
[ $((now - mtime)) -lt 60 ] || exit 1
