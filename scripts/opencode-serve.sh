#!/usr/bin/env bash
# Keep credentials out of argv and refuse automatic password generation.
set -euo pipefail
set +x
password_file="${HOME}/.config/clanka/opencode-server.pass"
if [[ ! -f "$password_file" || -L "$password_file" || $(stat -c '%a:%u' "$password_file") != "600:$(id -u)" ]]; then
  echo 'Expected an owned, regular 0600 ~/.config/clanka/opencode-server.pass' >&2
  exit 1
fi
unset OPENCODE_PASSWORD
OPENCODE_SERVER_PASSWORD=$(cat "$password_file")
[[ -n "$OPENCODE_SERVER_PASSWORD" ]] || { echo 'Server password is empty' >&2; exit 1; }
export OPENCODE_SERVER_PASSWORD
exec opencode serve --hostname 100.115.149.100 --port 47822
