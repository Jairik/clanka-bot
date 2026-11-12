# librespot

Spotify Connect receiver for the Pi, pinned to upstream **v0.8.0**
(`d36f9f1907e8cc9d68a93f8ebc6b627b1bf7267d`). It exists so music plays through
the same Pulse sink as the Assistant's speech, which keeps ducking and the
Focus rules local ([ADR-0004](../docs/adr/0004-spotify-plays-through-the-pi.md)).

## Build

```sh
docker build -t clanka/librespot:dev .
docker buildx build --platform linux/arm64 -t clanka/librespot:arm64 .
```

The builder runs on the build host and cross-compiles for `arm64`, so the arm64
build does not run the Rust compiler under QEMU. Cargo features:

- `pulseaudio-backend` — output goes to the host Pulse sink.
- `with-avahi` — a container-local Avahi responder. libmdns aborts when any
  interface rejects the multicast join (ENOBUFS on hosts with many Docker
  bridges); Avahi tolerates it and is restricted to the LAN interface.
- `rustls-tls-webpki-roots` — self-contained trust store and no OpenSSL
  cross-compile, as upstream recommends for containers.

`Cargo.lock` is used with `--locked`; the source URL is the commit, not the
moving tag.

## Runtime

- **Host networking.** Spotify discovers devices over mDNS; Docker's bridge
  network hides that multicast from the LAN. No port is published. If the host
  is multi-homed, set `LIBRESPOT_AVAHI_INTERFACE` to the LAN interface name.
- **Advertiser.** The container runs its own `dbus-daemon` and `avahi-daemon`
  with host networking. Avahi is restricted to the default-route interface
  (`ip route get 1.1.1.1`) and IPv4, so it does not advertise on Docker
  bridges or Tailscale. The host does not need Avahi or a D-Bus socket mount.
- **Pulse.** `PULSE_SERVER` points at the mounted host socket and
  `PULSE_COOKIE` at the host cookie. `LIBRESPOT_SINK` names the same sink as
  the Assistant; empty uses the default for both.
- **Cache.** `librespot-cache:/cache` holds `credentials.json` and audio files.
  Only this container mounts it. The cache directory is 0700 and the process
  writes with `umask 077`.
- **Status.** `librespot-status:/status` holds `librespot.status`, a
  metadata-only heartbeat (`version`, `state`, `session`, `device`, `updated`).
  The Assistant mounts it read-only. It never contains credentials or account
  identity.
- **Privileges.** The entrypoint starts as root only to `chown` the two volumes
  and start the local D-Bus/Avahi responder, then drops with `setpriv` before
  librespot runs. Compose sets the UID/GID from
  `ASSISTANT_UID`/`ASSISTANT_GID` and adds `no-new-privileges`.
- **Healthcheck.** `librespot`, `avahi-daemon` and `dbus-daemon` presence plus
  a heartbeat younger than 60 s. It never calls the internet, so a dropped LAN
  does not make the stack flap.

## Pairing

The first run has no credentials and still advertises the device. On a phone or
desktop on the same LAN, open Spotify → **Devices** and choose the configured
name (`Clanka` by default). librespot writes `credentials.json`; later restarts
reuse it. Pairing is separate from the Web API OAuth refresh token used for
control. See the main README's **Spotify** section and `docs/deploy.md` for the
human steps.
