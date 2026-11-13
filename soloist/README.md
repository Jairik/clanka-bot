# soloist

Spotify Connect receiver for the Pi, using Spotify's official **Soloist**
binary. The Assistant keeps using the Web API for control; Soloist is the
audio path, so it plays into the same Pulse sink as speech and local ducking
and the Focus rules stay local ([ADR-0004](../docs/adr/0004-spotify-plays-through-the-pi.md)).

## Why this is not a normal image

Soloist is proprietary and its official release page does not allow
redistribution. The image therefore contains only its runtime libraries; the
operator downloads the archive once on the deployment host with
`scripts/soloist-setup.sh` (also called by `scripts/deploy.sh`), and Compose
mounts that file read-only at `/opt/soloist`. The binary, the API key and the
paired data never enter this repository or an image layer.

The Pi's host glibc is older than Soloist requires, so the container runs on a
`debian:trixie-slim` runtime. `libpulse0` provides the default output path;
`libpipewire-0.3-0` supports the optional native `-d` route.

## Install and update

```sh
bash scripts/soloist-setup.sh            # download if missing, create private dirs
SOLOIST_UPDATE=1 bash scripts/soloist-setup.sh   # replace with the current release
```

The script picks the official archive by `uname -m` (`arm64` for aarch64, or
`x86_64`). `SOLOIST_ARCH` overrides it, and `SOLOIST_API_KEY_FILE` imports an
existing key. Files live in `~/.cache/clanka/soloist/` (`SOLOIST_DIR`):

```
api-key    mode 0600, mounted read-only
data/      paired credentials, device id and settings
cache/     audio cache
status/    metadata-only heartbeat read by the Assistant
soloist    the binary
```

The setup script migrates a previous compatibility probe's `data/`, `cache/`
and key when the supported directories are still empty. Stop the probe first
for a consistent copy; the probe's own files are left untouched as a backup.

An official build carries an expiry date (the startup log says
`client expires in N days`). The heartbeat turns that into remaining days and
the Dashboard shows an actionable warning; an expired build fails the Compose
healthcheck instead of pretending the receiver is playable. Re-run the setup
script with `SOLOIST_UPDATE=1` to refresh it.

## Runtime

- **Host networking.** Spotify discovers devices over mDNS, and a Docker
  bridge hides that multicast. Soloist advertises the configured
  `SPOTIFY_DEVICE_NAME` itself; no Avahi or D-Bus service is needed.
- **Pulse by default.** `PULSE_SERVER` and the host cookie point at the same
  server as the Assistant. `PULSE_SINK` inherits `ASSISTANT_PULSE_SINK`, so
  custom speech outputs also receive music; empty uses the Pulse default.
- **Private state.** The container runs as `ASSISTANT_UID`/`ASSISTANT_GID`
  and mounts only its own `data/`, `cache/`, key file and status directory.
- **Control socket.** A loopback-only WebSocket (`SPOTIFY_WS_ADDR`, default
  `127.0.0.1:24880`) lets the heartbeat ask for login and active-device state.
  It is not used for playback control; the Assistant uses the Web API.
- **Status.** `status/soloist.status` is metadata only: `version`, `state`,
  `session`, `device`, `expires_days` and `updated`. It never contains a
  credential, account identity or track metadata.
- **Healthcheck.** The daemon must answer `ctl status`, the heartbeat must be
  younger than 60 s, and the build must not have expired. It never calls the
  internet.

## Pairing

On first run there are no credentials, and the receiver still advertises. On a
phone or desktop on the same LAN, open Spotify → **Devices** and choose the
configured name (`Clanka` by default). The credentials and device identity are
stored in `data/`, so restarts and image updates reuse them. Pairing is separate
from the Web API OAuth refresh token used for control. See the main README's
**Spotify** section and `docs/deploy.md`.
