#!/bin/sh
# Start librespot as the host UID that can read the Pulse socket, then keep a
# metadata-only heartbeat for the Assistant's Health page. The cache volume is
# private: this script reports whether credentials.json exists, never its
# contents.
set -eu

LIBRESPOT_UID="${LIBRESPOT_UID:-1000}"
LIBRESPOT_GID="${LIBRESPOT_GID:-1000}"

# Avahi is the advertiser. libmdns aborts if any interface rejects the
# multicast join (ENOBUFS on a host with many Docker bridges); Avahi tolerates
# that and can be restricted to the LAN interface. The responder runs inside
# this container, so the host needs no Avahi or D-Bus access.
write_avahi_config() {
    avahi_iface="${LIBRESPOT_AVAHI_INTERFACE:-}"
    if [ -z "$avahi_iface" ]; then
        # The interface used for the default route is the LAN address a phone
        # can reach. An explicit override is useful on unusual hosts.
        avahi_iface="$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* dev \([^ ]*\).*/\1/p' | head -n 1)" || avahi_iface=""
    fi
    {
        printf '[server]\n'
        printf 'use-ipv4=yes\n'
        # Spotify on the LAN is IPv4; disabling IPv6 avoids link-local-only
        # records that point a client at an unreachable address.
        printf 'use-ipv6=no\n'
        if [ -n "$avahi_iface" ]; then
            printf 'allow-interfaces=%s\n' "$avahi_iface"
        fi
        printf 'ratelimit-interval-usec=1000000\n'
        printf 'ratelimit-burst=1000\n'
        printf '[wide-area]\n'
        printf 'enable-wide-area=no\n'
        printf '[publish]\n'
        printf 'publish-addresses=yes\n'
        printf 'publish-hinfo=no\n'
        printf 'publish-workstation=no\n'
        printf 'publish-domain=no\n'
        printf '[reflector]\n'
        printf 'enable-reflector=no\n'
    } > /etc/avahi/avahi-daemon.conf
    printf 'librespot: avahi interface=%s\n' "${avahi_iface:-all}" >&2
}

# The container starts as root only to make its volumes usable by the host UID
# and to start the tiny D-Bus/Avahi responder. Drop before librespot runs.
if [ "$(id -u)" = "0" ]; then
    mkdir -p /cache /status /run/dbus /run/avahi-daemon
    chown "$LIBRESPOT_UID:$LIBRESPOT_GID" /cache /status
    chmod 700 /cache
    chmod 755 /status
    chown messagebus:messagebus /run/dbus
    chown avahi:avahi /run/avahi-daemon
    # The system D-Bus EXTERNAL auth rejects a UID with no passwd entry. The
    # image carries UID 1000; add an entry for any other ASSISTANT_UID.
    if ! getent passwd "$LIBRESPOT_UID" >/dev/null 2>&1; then
        printf 'librespot:x:%s:%s:Librespot:/tmp:/usr/sbin/nologin\n' \
            "$LIBRESPOT_UID" "$LIBRESPOT_GID" >> /etc/passwd
    fi
    if ! getent group "$LIBRESPOT_GID" >/dev/null 2>&1; then
        printf 'librespot:x:%s:\n' "$LIBRESPOT_GID" >> /etc/group
    fi
    # A container restart leaves stale sockets and PID files in /run.
    rm -f /run/dbus/system_bus_socket /run/dbus/pid /run/avahi-daemon/pid
    if [ ! -s /var/lib/dbus/machine-id ]; then
        mkdir -p /var/lib/dbus
        dbus-uuidgen > /var/lib/dbus/machine-id
    fi
    write_avahi_config
    dbus-daemon --system --fork
    avahi-daemon --daemonize --no-chroot
    exec setpriv --reuid="$LIBRESPOT_UID" --regid="$LIBRESPOT_GID" --clear-groups "$0" "$@"
fi

umask 077
mkdir -p /cache /status

name="${LIBRESPOT_NAME:-Clanka}"
bitrate="${LIBRESPOT_BITRATE:-320}"
port="${LIBRESPOT_ZEROCONF_PORT:-40899}"
initial="${LIBRESPOT_INITIAL_VOLUME:-50}"
cache_limit="${LIBRESPOT_CACHE_LIMIT:-2G}"

# write_status is atomic: the Assistant reads either the previous complete file
# or the new one, never a partial line. It contains no credentials.
write_status() {
    paired=unpaired
    if [ -s /cache/credentials.json ]; then
        paired=paired
    fi
    session=unknown
    if [ -s /status/librespot.session ]; then
        read -r session < /status/librespot.session || session=unknown
    fi
    case "$session" in
        connected | disconnected) ;;
        *) session=unknown ;;
    esac
    {
        printf 'version=1\n'
        printf 'state=%s\n' "$paired"
        printf 'session=%s\n' "$session"
        printf 'device=%s\n' "$name"
        printf 'updated=%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } > /status/.librespot.status
    mv /status/.librespot.status /status/librespot.status
}

write_status
# The heartbeat is also a liveness signal: the Compose healthcheck fails if it
# stops updating. Ten seconds is well inside the Assistant's 45 s stale bound.
(
    while :; do
        sleep 10
        write_status
    done
) &

# One event script records session connected/disconnected. It writes only the
# state word, never USER_NAME or CONNECTION_ID.
set -- \
    --name "$name" \
    --bitrate "$bitrate" \
    --cache /cache \
    --cache-size-limit "$cache_limit" \
    --backend pulseaudio \
    --device-type speaker \
    --initial-volume "$initial" \
    --zeroconf-port "$port" \
    --zeroconf-backend avahi \
    --onevent /usr/local/bin/event.sh

if [ -n "${LIBRESPOT_SINK:-}" ]; then
    # Same named sink as the Assistant's playback.sink. Empty leaves Pulse's
    # default, which is also what the Assistant uses.
    set -- "$@" --device "$LIBRESPOT_SINK"
fi

exec librespot "$@"
