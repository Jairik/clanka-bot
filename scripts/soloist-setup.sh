#!/usr/bin/env bash
# Install the operator-downloaded Soloist receiver and prepare its private
# state. Called by scripts/deploy.sh and `make up`; safe to run repeatedly.
#
# The official archive is fetched here on the deployment host. It is
# proprietary and must not be committed, baked into an image or redistributed.
# Existing state and the API key are never overwritten.
set -euo pipefail

log() { printf 'soloist-setup: %s\n' "$*"; }
warn() { printf 'soloist-setup: warning: %s\n' "$*" >&2; }
die() {
    printf 'soloist-setup: error: %s\n' "$*" >&2
    exit 1
}

SOLOIST_DIR="${SOLOIST_DIR:-$HOME/.cache/clanka/soloist}"
SOLOIST_BINARY="${SOLOIST_BINARY:-$SOLOIST_DIR/soloist}"
LEGACY_DIR="${SOLOIST_LEGACY_DIR:-$HOME/.cache/clanka/soloist-compat}"
BASE_URL="${SOLOIST_BASE_URL:-https://soloist-builds.spotifycdn.com}"

case "${SOLOIST_ARCH:-$(uname -m)}" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=x86_64 ;;
*) die "unsupported architecture ${SOLOIST_ARCH:-$(uname -m)}; set SOLOIST_ARCH=arm64 or x86_64" ;;
esac

umask 077
mkdir -p "$SOLOIST_DIR" "$SOLOIST_DIR/data" "$SOLOIST_DIR/cache" "$SOLOIST_DIR/status"
chmod 700 "$SOLOIST_DIR" "$SOLOIST_DIR/data" "$SOLOIST_DIR/cache"
# The status file is metadata only and the Assistant reads it as its own UID.
chmod 755 "$SOLOIST_DIR/status"

api_key="$SOLOIST_DIR/api-key"
if [ ! -s "$api_key" ]; then
    if [ -n "${SOLOIST_API_KEY_FILE:-}" ]; then
        [ -s "$SOLOIST_API_KEY_FILE" ] ||
            die "SOLOIST_API_KEY_FILE points at a missing file: $SOLOIST_API_KEY_FILE"
        install -m 600 "$SOLOIST_API_KEY_FILE" "$api_key"
        log "imported the API key from SOLOIST_API_KEY_FILE"
    elif [ -s "$LEGACY_DIR/private/api-key" ]; then
        install -m 600 "$LEGACY_DIR/private/api-key" "$api_key"
        log "migrated the API key from the previous receiver"
    else
        die "no Soloist API key. Get one from https://developer.spotify.com/documentation/soloist and either place it at $api_key (mode 0600) or set SOLOIST_API_KEY_FILE to an existing private file. Never commit it."
    fi
fi
chmod 600 "$api_key"

probe_running() {
    command -v docker >/dev/null 2>&1 || return 1
    docker ps --format '{{.Names}}' 2>/dev/null | grep -qx 'clanka-soloist-probe'
}

# Move the compatibility probe's paired data into the supported directory
# once. The probe's own copy stays on disk as the backup.
migrate_state() {
    local name=$1 src="$LEGACY_DIR/private/$1" dest="$SOLOIST_DIR/$1"
    [ -d "$src" ] || return 0
    [ -n "$(ls -A "$src" 2>/dev/null)" ] || return 0
    [ -z "$(ls -A "$dest" 2>/dev/null)" ] || return 0
    if probe_running; then
        die "the compatibility probe is running; stop it first for a consistent copy, then re-run"
    fi
    cp -a "$src/." "$dest/"
    log "migrated $name from the previous receiver (the probe copy is untouched)"
}

migrate_state data
migrate_state cache

if [ -x "$SOLOIST_BINARY" ] && [ "${SOLOIST_UPDATE:-0}" != "1" ]; then
    log "using existing receiver binary $SOLOIST_BINARY"
    exit 0
fi

command -v curl >/dev/null 2>&1 || die "curl is required to download Soloist"
command -v tar >/dev/null 2>&1 || die "tar is required to unpack Soloist"

tmp="$(mktemp -d "${TMPDIR:-/tmp}/soloist.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
url="$BASE_URL/soloist_release_${arch}.tar.gz"
log "downloading $url"
curl -fsSL --retry 3 --retry-delay 2 "$url" -o "$tmp/soloist.tar.gz" ||
    die "download failed; check the network and the official Soloist release page"
tar -xzf "$tmp/soloist.tar.gz" -C "$tmp" soloist ||
    die "the archive did not contain the soloist binary"
install -m 755 "$tmp/soloist" "$SOLOIST_BINARY.tmp"
mv -f "$SOLOIST_BINARY.tmp" "$SOLOIST_BINARY"
log "installed $arch receiver binary at $SOLOIST_BINARY"
