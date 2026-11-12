#!/bin/sh
# librespot --onevent target. Only session state is kept; the event's
# USER_NAME and CONNECTION_ID are deliberately ignored so the status volume
# carries no account identity.
set -eu

case "${PLAYER_EVENT:-}" in
    session_connected) state=connected ;;
    session_disconnected) state=disconnected ;;
    *) exit 0 ;;
esac

umask 077
printf '%s\n' "$state" > /status/.librespot.session
mv /status/.librespot.session /status/librespot.session
