# Deploying the stack

The same Compose stack runs on the dev box and on the Pi. There is no registry:
the Pi tracks `main` and builds its own arm64 images (decided in
[#13](https://github.com/Jairik/clanka-bot/issues/13)). You can deploy by
running `scripts/deploy.sh` over SSH, or let the Pi do it when `main` moves and
CI is green — see [auto-deploy.md](auto-deploy.md) for the timer, its CI gate
and how to pause it. The checklist below is for provisioning a fresh host.

## Everyday commands

```sh
make build       # build every service image for this host
make up          # start the stack in the background
make test        # assistant tests + deployment tests
make check       # check compose/Dockerfile deployment conventions
make deploy      # the Pi path: pull, build, start, wait for health
make rollback SHA=<commit>
make build-arm64 # optional QEMU arm64 build of every image (not for latency)
sudo bash scripts/install-deploy-watch.sh  # opt in to the auto-deploy timer (auto-deploy.md)
```

`make deploy` and `scripts/deploy.sh` are the same path.

## What `scripts/deploy.sh` does

**No argument — deploy the current branch**

1. Refuses to run when the checkout has local changes (tracked or untracked,
   except gitignored files such as `.env`).
2. If HEAD is detached after an earlier rollback, returns to the tracked branch.
3. Runs `git pull --ff-only`; diverged history fails the deploy.
4. Runs `docker compose up -d --build --remove-orphans` with `GIT_SHA` set to
   `git rev-parse --short HEAD`, so every image is tagged
   `clanka/<service>:<sha>` and containers from a different service set are
   removed.
5. Waits up to `DEPLOY_HEALTH_TIMEOUT` seconds (default 120) for every service
   in `docker-compose.yml` to be healthy. On failure it exits non-zero, prints
   `docker compose ps` and the logs of the failing service(s).
6. Because the service list comes from Compose, the script keeps working as the
   stack grows from `assistant` alone to all eight services.

**With a commit — rollback without rebuilding**

```sh
scripts/deploy.sh <sha>      # or: make rollback SHA=<sha>
```

1. Same dirty-tree refusal, then checks the commit out (detached).
2. Checks that every image named in that commit's Compose file already exists
   locally; if one is missing it stops before touching containers and says
   which one.
3. Runs `docker compose up -d --no-build --remove-orphans`, so the existing
   SHA-tagged images are reused and containers that only exist in a newer
   commit's service set are removed. The host then runs exactly the rolled-back
   stack, and the health wait enumerates that commit's services only. Old images
   are pruned by hand (`docker image prune`).

The next plain `make deploy` switches back to the tracked branch and pulls.

Tuning and diagnostics:

```sh
DEPLOY_HEALTH_TIMEOUT=180 scripts/deploy.sh   # slower models on the Pi
docker compose ps
docker compose logs -f <service>
```

## Pi provisioning checklist

Pi OS Lite, Docker and Tailscale by hand; no Ansible or cloud-init. Raspberry Pi
OS Lite (64-bit) is assumed; the default audio stack on Trixie is unconfirmed
(see [#7](https://github.com/Jairik/clanka-bot/issues/7)).

Hardware: Pi 5, a plugged-in USB mic, and the Echo Dot over Bluetooth for
output. Only the `assistant` container mounts the audio socket (ADR-0001); the
ML services never touch audio devices.

### Base system

- [ ] Flash Raspberry Pi OS Lite (64-bit); set hostname, user, SSH key and
      network in Raspberry Pi Imager.
- [ ] Boot, then update and reboot:
      `sudo apt update && sudo apt full-upgrade -y && sudo reboot`
- [ ] Set the timezone to match `.env`:
      `sudo timedatectl set-timezone <Area/City>`. The assistant reads the live
      `timezone` setting first, then `config.yaml`'s `timezone`, then `TZ`, and
      logs a startup warning when the effective zone is UTC. It never infers a
      zone from the public IP.
- [ ] Confirm the Pi 5's 16K pages: `getconf PAGE_SIZE` prints `16384`.

### Docker

- [ ] Install Docker Engine and the Compose plugin, then enable it at boot:

      ```sh
      curl -fsSL https://get.docker.com | sh
      sudo usermod -aG docker "$USER"   # log out and back in
      sudo systemctl enable --now docker
      docker compose version
      ```

### Remote access

- [ ] Install Tailscale and join the tailnet:

      ```sh
      curl -fsSL https://tailscale.com/install.sh | sh
      sudo tailscale up
      tailscale ip -4
      ```

- [ ] SSH in over the Tailscale address (do not expose SSH to the LAN/WAN).
      `systemctl enable docker` plus `restart: unless-stopped` covers reboots;
      there are no custom systemd units.

### Dashboard over Tailscale

The Dashboard is a separate listener served by `assistant`. Its React app ships
as its own SHA-tagged artifact (`dashboard-web`, below) that the assistant
proxies on the same origin; the same build is also embedded in the binary, so a
stopped artifact still serves a working page and nothing needs the internet at
runtime. It is **off** until a Tailscale owner is configured, and it is meant to
be reached only through `tailscale serve`, never published to the LAN or the
internet.

1. **Set the owner.** Put the exact login from `tailscale status --json` (or the
   admin console) in `.env`, and keep the loopback port unless it clashes:

   ```sh
   ASSISTANT_DASHBOARD_PORT=8082
   ASSISTANT_DASHBOARD_OWNER=you@example.com
   # The exact Host tailscale serve sends, without a scheme or path.
   ASSISTANT_DASHBOARD_HOSTS=pi.tailnet-name.ts.net
   ```

   Then deploy or restart the assistant container. An empty owner disables the
   listener. `ASSISTANT_DASHBOARD_HOSTS` is required for serve: the handler
   rejects any Host outside that exact list, so a DNS-rebinding page cannot
   borrow the owner identity. It fails closed when empty (only a loopback Host
   is accepted) and logs the setup instruction. Wildcards are rejected, and
   loopback names stay allowed so the local curl smoke test keeps working. If
   you change `ASSISTANT_DASHBOARD_PORT`, use the same port in the
   `tailscale serve` command below. `assistant/config.yaml` documents
   `dashboard.owner`, `addr`, `hosts` and `health_timeout`.

2. **Enable HTTPS on the tailnet** if it is not already on. `tailscale serve`
   provisions the certificate for the Pi's `*.ts.net` name.

3. **Proxy the loopback port from the Pi host**, not from inside a container:

   ```sh
   tailscale serve --bg 8082
   tailscale serve status
   ```

   Serve sends `https://<pi-name>.<tailnet>.ts.net/` to
   `http://127.0.0.1:8082`; opening that URL from any device in the tailnet
   shows the Dashboard home. HTTPS also unlocks the browser microphone.

4. **Limit the port to the owner in the tailnet policy.** Serve traffic is
   subject to the ACL at the destination port, 443. A grant scoped to the Pi's
   host alias does that, for example:

   ```json
   {
     "hosts": {
       "pi": "100.x.y.z"
     },
     "grants": [
       {
         "src": ["you@example.com"],
         "dst": ["pi"],
         "ip": ["tcp:443"]
       }
     ]
   }
   ```

   If a broader rule already lets other members reach port 443 on the Pi, this
   port is not isolated; narrow or remove that rule. Do not use
   `tailscale funnel`: public traffic has no Tailscale identity headers, and the
   Dashboard rejects it.

**How the loopback boundary works.** Compose publishes the Dashboard on
`127.0.0.1:8082` on the host and sets `ASSISTANT_DASHBOARD_ADDR=:8082` inside
the container. Docker cannot reach a listener bound to `127.0.0.1` *inside* a
container through a published port, so the container binds all interfaces just
as the debug entry does. The host publication is still loopback-only. The
handler refuses any request whose source is not loopback or the container's
default gateway (the host) before it reads identity headers, and it ignores
`X-Forwarded-For`. A LAN or tailnet client that somehow reaches the port
directly therefore cannot authenticate by sending its own
`Tailscale-User-Login`. Never add a LAN/WAN port mapping for 8082.

`tailscale serve` strips client-supplied `Tailscale-*` headers and adds the
authenticated `Tailscale-User-Login`. The Dashboard first requires the Host to
be the configured `ASSISTANT_DASHBOARD_HOSTS` entry (or loopback), then requires
exactly one identity header matching the configured owner; other hosts,
identities, missing headers, duplicate headers and tagged devices are refused
with 403. The Spotify OAuth callback uses the same host and its own state and
binding-cookie checks.

Check the listener locally:

```sh
# No identity header: rejected.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8082/health

# The configured owner: 200 and the Health page.
curl -s -H 'Tailscale-User-Login: you@example.com' http://127.0.0.1:8082/health | head
```

The second command is only a local smoke test; from another device use the
HTTPS `*.ts.net` URL, which carries the real identity.

### Shipping a Dashboard UI-only change

The UI and the API ship separately (ADR
[0007](adr/0007-the-ui-is-a-versioned-artifact.md)). A page fix does not need a
Go rebuild or an assistant restart:

```sh
git pull --ff-only
make web-image      # build clanka/dashboard-web:<sha>, restart only that service
docker compose ps dashboard-web
```

The assistant reaches the artifact over the internal Compose network and keeps
serving the document and assets on the Dashboard origin, so identity, Host and
CSP checks stay where they are. `X-Dashboard-UI: artifact` and
`X-Dashboard-UI-Version: <sha>` on the response say which copy a page came from.
The version is the build the artifact reports about itself, so after a `--no-deps`
rollback of `dashboard-web` the header shows the old SHA without restarting the
assistant. To go back to the copy embedded in the running assistant:

```sh
docker compose stop dashboard-web
```

`make deploy` still builds both images from the same commit. The UI embeds the
API view version it was built against, and the assistant reports its own in
every view and in `X-Dashboard-API-Version`. If the two differ, the Dashboard
shows a version-mismatch page instead of rendering undefined fields; deploy both
from the same commit, or roll back the artifact with
`GIT_SHA=<old-sha> docker compose up -d --no-deps dashboard-web` after building
that commit's image.

The Dashboard also mounts the settings/Tool-override API under `/api/`
(`/api/settings`, `/api/tools`, `/api/secrets`) behind the same identity check.
Mutating API requests must be same-origin; the debug listener keeps its own
localhost-only mount of the same handler. The **Tools and settings** page at
`/settings` edits the same service: Tool enabled/Permission, the wording lists,
and any other setting the service exposes (including the sound and voice
settings, which appear without a page change). Wording settings
(`tools.<name>.control_phrases`, `tools.<name>.examples`,
`agent.request_prefixes`, `agent.filler`, `agent.unavailable_message`), the
model name (`agent.model`), `voice.name` and `voice.rate` are marked `live` and
apply to the next request or in the background (a voice/rate change also
re-synthesizes the cached Agent Filler). Connection-level settings (`http.addr`, `debug.addr`,
`dashboard.addr`, `tts.addr`, `wakeword.addr`, `stt.addr`, `embed.url`,
`capture.source`, `playback.sink`) are marked `restart_required`: the override
is stored, but only the next process start reads it, and the page says so. See
the README for the curl examples. The Settings page's **Wake word** section is
read-only: the allowlist and per-model thresholds are connection settings
(`WAKEWORD_MODELS`, `WAKEWORD_THRESHOLD`, `WAKEWORD_THRESHOLDS`), so change
`.env` and restart; the Health page's Wake word rows show what the service
advertises. The wake-free command models (#81) are shown there too, with `off`
when `WAKEWORD_COMMANDS_ENABLED` is false.

The **Activity** page at `/activity` lists recent requests from a bounded
in-memory ring plus the durable SQLite history fed by the same completion that
writes the structured `request` log line; older pages and searches read the
database, and retention trims old rows to a summary. It shows the chosen route, measured stage timings, Tool calls and
outcome; Response latency appears only when reply audio actually started. Its
typed box runs the normal pipeline text-only (no Pi speech), including a pending
Confirmation answered with `yes`/`no`, and shows the reply in the page. Local-only
Tool results are visible on this authenticated page and nowhere else: the ring
is not persisted and is never part of a model prompt, Notification or the public
health listener.

The **Voices** page at `/voices` lists built-in and custom Pocket TTS states,
uploads a `.safetensors` state (at most 32 MB; `.pt`/`.pth` are refused),
previews one in the browser, chooses the active voice and speech rate, and
records an optional source/consent note. An upload is staged and header-checked
before TTS is called, then smoke-tested and published atomically; a rejected
file leaves the library unchanged. Previews synthesize one short phrase to WAV
and never play on the Pi speaker or change the active voice. See the README for
the validation and rollback sequence.

The **Personas** page at `/personas` creates, edits and selects a persistent
bundle of voice, personality prompt, Agent Filler and Unavailable wording.
Selecting applies all four settings in one transaction after validating the
voice state, so a stale or unknown voice leaves the previous voice and wording
untouched. The personality prompt reaches the Agent path only through the one
prompt builder's labelled system part. The active voice and the active
Persona's voice both warm-load at startup; a stale Persona voice is skipped and
reported, while the speaking voice keeps #63's single fallback Announcement.
Creating or editing a Persona makes no model call, and the page needs no TTS
service to list or edit the library. See the README for the spoken examples.

The **Secrets** page at `/secrets` lists every declared secret (Tool modules
and the active Agent provider) with `set`, `last4`, update time and source
(`.env` or stored). Replace and Clear post to the same authenticated Dashboard;
a cleared stored value falls back to `.env` immediately. No value is revealed,
prefilled or echoed, and a new module declaration appears without a page edit.

GitHub uses two secrets there: `GITHUB_TOKEN` (fine-grained PAT, issues and pull
requests) and the optional `GITHUB_NOTIFICATIONS_TOKEN` (classic PAT, only the
`notifications` scope, used solely by `github.notifications`). Leaving the
second one unset is not an error; the Health page shows `github notifications`
as not configured and the other GitHub Tools stay healthy.

The **Scheduled alerts** page at `/alerts` lists every active Timer, Alarm and
Reminder with its next fire time, label or message and kind in the live
timezone. Cancel stops one alert by its manager id (a ringing Alarm stops only
that Ring). Each Alarm exposes its sound, volume and ramp overrides: blank keeps
the current value, `0` is an explicit value, and Default clears the field back
to `sounds.alarm_ring` / `ring.alarm.*` as NULL. The same page shows the newest
missed alerts from the shared local history, including a fired row with no
pending schedule. Every write needs the Dashboard identity and a same-origin
request; the forms work without JavaScript.

Notifications use the public ntfy.sh by default (`notify.url`). The ntfy topic
is generated once into that secrets table on first use, so it is never in
`config.yaml`; to connect a phone without reading the database, set
`NTFY_TOPIC` in `.env` before first use and subscribe the ntfy app to it. A
self-hosted ntfy with access control needs a server-issued token in
`NTFY_TOKEN`; a random local token is not accepted, and the assistant never
invents one. Outbound HTTPS to the configured ntfy server must be reachable
from the Pi. See the README's **Notifications** section for the security
caveat and the self-hosted token steps.

The Health page shows the seven current Compose services (`embed`, `assistant`,
`wakeword`, `stt`, `tts`, `web-search`, `soloist`), the resolved location,
each Tool module that declares a health check, and the two Spotify grants
separately. `embed` and `web-search` are checked over HTTP `/healthz`; the three
Wyoming services are checked with `describe`; `assistant` is the process
serving the page; `soloist` is checked by the heartbeat it writes to the
metadata-only status directory, and an expired build is a failure. One refresh
is bounded by
`dashboard.health_timeout` (3 s), and checks that overrun are shown as timed
out. These are endpoint/status checks, not Docker's container health field.

### Audio and the Echo Dot

- [ ] Install the host audio stack (PipeWire with its PulseAudio-compatible
      socket, plus Bluetooth support). Package names differ between Pi OS
      releases; confirm `pactl info` talks to a running server before pairing.
- [ ] Pair and trust the Echo Dot:

      ```sh
      bluetoothctl
      power on
      agent on
      default-agent
      scan on
      pair <MAC>
      trust <MAC>
      connect <MAC>
      ```

- [ ] Keep the Echo Dot connected and stop it re-announcing. The Echo says
      "Playing from <Pi name>" every time the A2DP stream reopens, and
      WirePlumber closes an idle Bluetooth output after about 5 s by default,
      so without this every reply after a pause triggers the announcement.
      Check the version with `wireplumber --version`. On 0.4 (current Pi OS),
      write `~/.config/wireplumber/bluetooth.lua.d/99-clanka-echo.lua`,
      replacing `<MAC_>` with the MAC in underscore form
      (`AA_BB_CC_DD_EE_FF`):

      ```lua
      bluez_monitor.properties["with-logind"] = false  -- headless: no seat
      table.insert(bluez_monitor.rules, {
        matches = { { { "device.name", "equals", "bluez_card.<MAC_>" } } },
        apply_properties = {
          ["bluez5.auto-connect"] = "[ a2dp_sink ]",
          ["device.profile"] = "a2dp-sink",
        },
      })
      -- Never suspend the Echo's output, so the stream stays open.
      table.insert(bluez_monitor.rules, {
        matches = { { { "node.name", "matches", "bluez_output.<MAC_>.*" } } },
        apply_properties = { ["session.suspend-timeout-seconds"] = 0 },
      })
      ```

      0.4 reads only these Lua files and ignores `wireplumber.conf.d/*.conf`
      (0.5 syntax). On 0.5, put the same settings in
      `wireplumber.conf.d/` as `monitor.bluez.rules` instead. Run
      `systemctl --user restart wireplumber`; if the Echo does not reconnect,
      run `bluetoothctl connect <MAC>`. To verify, play a sound and wait 10 s:
      `pactl list short sinks` should show the bluez output as `IDLE`, not
      `SUSPENDED`. Over SSH, `export XDG_RUNTIME_DIR=/run/user/$(id -u)` first.
      A real disconnect (reboot, Echo restart) still announces; the Echo has no
      setting to turn that off.
- [ ] Make the Echo Dot the default sink and check playback:
      `pactl set-default-sink <sink>`; `speaker-test -t wav -c 2`.
- [ ] Compose mounts the host's Pulse socket into `assistant` and `soloist`
      only (#17, ADR-0004); set `ASSISTANT_UID`/`ASSISTANT_GID` to the user
      that owns it and `ASSISTANT_PULSE_SINK` to the Echo Dot's sink. Soloist
      uses the Pulse default sink, so keep the Echo Dot as the host default (or
      set `SPOTIFY_PIPEWIRE_DEVICE` to its native PipeWire node). Never give
      the socket to the ML services.

### Spotify Connect and Web API

Spotify is optional. Pairing and Web API auth are separate; one can work
without the other.

- [ ] Create an app in the Spotify Developer Dashboard and add the exact
      redirect URI to its **Redirect URIs** allowlist:

      ```text
      https://<pi-name>.<tailnet>.ts.net/spotify/callback
      ```

      HTTPS is required except for a loopback IP literal; `localhost` is not
      accepted. Development mode is fine for one account: its owner needs
      Premium and it allows five authenticated users. There is no client
      secret; the flow uses Authorization Code with PKCE.
- [ ] Put the public client ID and the same redirect URI in `.env`:
      `SPOTIFY_CLIENT_ID`, `SPOTIFY_REDIRECT_URI`, and optionally
      `SPOTIFY_DEVICE_NAME`, `SPOTIFY_MDNS_HOSTNAME`,
      `SPOTIFY_INITIAL_VOLUME`, `SPOTIFY_CACHE_MB`, `SPOTIFY_WS_ADDR`,
      `SPOTIFY_STREAM_MATCH` and `SPOTIFY_PIPEWIRE_DEVICE`. A client ID
      without a redirect URI (or the reverse) is a startup error, not a
      silently disabled button.
- [ ] Provide the Soloist API key and binary. The official archive is
      proprietary and is not committed or baked into an image. Get a key from
      <https://developer.spotify.com/documentation/soloist>, then either put it
      at `~/.cache/clanka/soloist/api-key` (mode 0600) or import it once with
      `SOLOIST_API_KEY_FILE=/path/to/key scripts/soloist-setup.sh`. The script
      downloads the archive for this host's architecture; `make soloist-update`
      refreshes it before the build expires. `scripts/deploy.sh` runs the setup
      automatically.
- [ ] `soloist` uses **host networking**. Spotify Connect discovery is mDNS; a
      Docker bridge network hides the multicast from the LAN. No port is
      published. Allow UDP 5353 on the LAN. Soloist advertises the device
      itself, so the host needs no Avahi daemon or D-Bus socket mount.
      `SPOTIFY_MDNS_HOSTNAME` sets the container's hostname, kept distinct from
      the Pi's own `.local` name.
- [ ] Pair once: open Spotify on a phone or desktop on the same LAN, play
      something, open **Devices**, and choose `Clanka` (or
      `SPOTIFY_DEVICE_NAME`). The credentials are cached in Soloist's private
      `~/.cache/clanka/soloist/data` directory; later container restarts and
      image updates reuse them. The Assistant never mounts that directory.
- [ ] Open the Dashboard's `/spotify` page over Tailscale and click **Connect
      Web API**. The callback writes only `SPOTIFY_REFRESH_TOKEN` to the
      write-only secrets table. The same token appears on `/secrets`; the
      Spotify page never shows it.
- [ ] Check `/health`: **Connect pairing** reports paired/not paired (or
      unavailable when the heartbeat is stale), and **Web API auth** reports
      connected/configured/reconnect needed separately. A missing or stale
      Soloist heartbeat is never guessed as paired, and an expired build is an
      error with the update command. Development-mode refresh tokens last six
      months; a **reconnect needed** state means Connect again.
- [ ] Playback (#72, code-complete, live acceptance pending): after pairing and
      Web API connect, say "play Radiohead"; the Tools target only the Connect
      device named `SPOTIFY_DEVICE_NAME` (`Clanka`). Check that "skip", "pause"
      and "next" advance the track and "resume" resumes it, that music dips during a reply, a
      Timer Ring and after the wake word, and that "what's playing" answers.
      Ducking uses `pactl` against Soloist's `spotify` Pulse sink input, so it
      needs the same Pulse socket the Assistant already mounts.
- [ ] Playlists (#73, code-complete, live acceptance pending): after Web API
      connect, say "play my <one of your playlists> playlist on shuffle". The
      assistant loads your playlists in the background at startup and every 6
      hours, so a playlist made minutes ago is not found until the next refresh
      or a restart. Also try "shuffle the music", "repeat this song", "go back a
      song", "add <song> to the queue" and "like this song". An existing Web
      API grant already has the needed scopes (`playlist-read-private`,
      `user-library-modify`, `user-modify-playback-state`) if it was made with
      the #71 scope list; otherwise click **Connect Web API** again. None of
      this has run against a real account.
- [ ] `music.max_volume` (Dashboard Settings) is unset, meaning no cap, until the
      #70 spike gives a value. Set it there once measured; it caps
      `spotify.volume`, a level passed to `spotify.play`, and a louder device at
      play start.
- [ ] Troubleshoot with `docker compose logs soloist` and `docker compose ps`.
      The `~/.cache/clanka/soloist/status` directory is metadata only; `data/`
      and `api-key` hold credentials and must be treated like the secrets
      database.

### The stack

- [ ] Clone the repo: `git clone https://github.com/Jairik/clanka-bot ~/clanka-bot`
- [ ] Create the host-local `.env` and adjust it:
      `cd ~/clanka-bot && cp .env.example .env`, set `TZ`, ports,
      `ASSISTANT_PULSE_SINK` and `ASSISTANT_PULSE_SOURCE` (the `parec` source
      from `pactl list short sources`), and set `ASSISTANT_DASHBOARD_OWNER` to
      your Tailscale login if you want the Dashboard (empty disables it).
      `.env` is gitignored and never baked into an image.
- [ ] `make check`
- [ ] `make deploy` (or `scripts/deploy.sh`)
- [ ] Verify: `docker compose ps`, `curl -s localhost:8080/healthz`
- [ ] The `assistant-data` volume holds the SQLite database (ADR-0003) and the
      `sounds/` sound library (canonical WAVs uploaded or dropped there). It
      survives container restarts and image rebuilds. `assistant` is its only
      mounter. It holds settings overrides and unencrypted secrets (API keys,
      tokens) in a file mode 0600; treat a backup like the secrets it contains.
- [ ] The `web-search` (SearXNG) service is internal-only: Compose publishes no
      host port for it, runs no valkey and keeps no data volume. It reads
      `searxng/config/settings.yml` (JSON output, limiter off, three engines,
      1.5 s engine timeout) and is pinned to the official 2026.11.12 image. Set
      `SEARXNG_SECRET` in `.env` only if the host rotates internal application
      secrets; the default is a non-authenticating internal value. Troubleshoot
      with `docker compose logs web-search` and
      `docker compose exec web-search wget -qO- http://127.0.0.1:8080/healthz`.
      See `searxng/README.md` for the pinned image and the settings rationale.
- [ ] `web.fetch` makes direct public HTTP(S) connections from `assistant`:
      it ignores `HTTP_PROXY`/`HTTPS_PROXY`, refuses loopback, private,
      link-local, CGNAT/tailnet and special-purpose addresses before dialling,
      pins the validated DNS answer and revalidates every redirect. Do not put
      a transparent proxy in front of the container; that would bypass the
      dial-time address policy. The Tool reads at most
      `tools.web.fetch.max_bytes` (2 MiB) and returns at most
      `tools.web.fetch.max_chars` (4000) runes of readable text.
- [ ] The separate `assistant-voices` volume holds custom voice states at
      `/app/data/voices` in `assistant`; `tts` mounts only that volume,
      read-only, at `/voices`. Only the voices subtree is exposed, never the
      database or secrets. Back it up with `assistant-data`.
      `docker compose down -v` deletes both: back them up first if the
      settings, sounds, voices or secrets matter.
- [ ] Personal wake-phrase samples recorded on the Dashboard's
      `/voice-samples` page live in the separate `assistant-samples` volume at
      `/app/data/wake-samples`, not on `assistant-data`, so a database backup
      does not take the owner's voice with it. Finished `.tar.gz` bundles are
      written to `/exports`, bind-mounted from `./exports` in the checkout
      (`scripts/deploy.sh` creates it with the deploy user's ownership). They contain the owner's
      voice: they are gitignored, never uploaded, never logged, and are not
      part of a backup that leaves the host. To copy one to the desktop, set
      `ASSISTANT_VOICE_SAMPLES_HOST_DIR=/home/rbpi/clanka-bot/exports`,
      `ASSISTANT_EXPORT_SSH_USER=rbpi`, `ASSISTANT_EXPORT_SSH_HOST=pi` and
      optionally `ASSISTANT_EXPORT_DESKTOP=<desktop>` in `.env`; leave them
      empty to hide the copy lines and use Download. Then run
      `wakeword/train import rbpi@pi:/home/rbpi/clanka-bot/exports/<bundle>
      .` on the desktop. `docker compose down -v` deletes the takes; copy the
      bundles off first.
- [ ] `~/.cache/clanka/soloist/data` holds Spotify Connect credentials, the
      device identity and settings; `cache/` holds audio; `api-key` holds the
      Soloist key. Only `soloist` mounts them. `status/` holds a metadata-only
      heartbeat (pairing/session/device/expiry/timestamp) and is mounted
      read-only into `assistant` at `/spotify-status`; it contains no
      credentials. Removing the private directory means pairing again.
- [ ] Custom voices: upload pre-exported Pocket TTS `.safetensors` states on
      the Dashboard's `/voices` page, or copy them into the voices volume for
      example `docker compose cp ./jarvis.safetensors
      assistant:/app/data/voices/` and restart `assistant`. Only
      `.safetensors` is accepted, at most 32 MB; `.pt`/`.pth` pickle files are
      refused. The Dashboard path stages the file, runs the header check before
      any model call, smoke-tests it, records the revision it passed against,
      and publishes it atomically; it also flags a stale state for re-export and
      records the optional consent note. A manual copy is picked up by a
      restart, which runs the same checks during the library rescan. The active
      voice warm-loads; others load on first use.
- [ ] Reboot once and confirm the stack comes back without a login.

## Image tags and rollback

Every service is tagged `clanka/<service>:<short SHA>`. A deploy builds the
current SHA; a rollback checks out an older commit and reuses the images built
for it, so the matching model weights come back too. Nothing pulls over the
internet at deploy time except base images that Docker has not cached yet.

## Model weights layer

Every service is built locally from its build context, even when it wraps an
upstream image (the official openWakeWord image is the base of
`clanka/wakeword`, and the pinned official SearXNG image is the base of
`clanka/web-search`, not used as a separately tagged upstream image). That is
what gives every service the SHA tag that rollback needs. `make check` also
enforces the image tag, build context, restart policy and local-only health
check.

ML images (`wakeword`, `stt`, `tts`, `embed`) must download or copy model
weights in an early layer **before any `COPY` of application code**, so editing
Go/Python code reuses the weights layer instead of re-downloading a model.
Dependency manifests (`requirements.txt`, `pyproject.toml`, `go.mod`, …) and the
fetch script itself (`prefetch.py`) may be copied before the weights layer.
`wakeword` copies its trained `models/clanker.tflite` in that early layer, before
the `healthcheck.py` COPY.

`scripts/check-compose.sh` (run by `make check`) parses each locally built ML
Dockerfile and fails when a detected weights download comes after the first
code `COPY`; it warns when it cannot detect a weights layer at all. It
recognises `snapshot_download`, `hf_hub_download`, `from_pretrained`,
`SentenceTransformer(...)`, `RUN python prefetch.py`, `/models` and the
download CLIs.

The six-service stack remains as verified in #66. `make check` now reports OK
for eight services. The seventh is the Soloist runtime image: it builds locally
for this host and carries only shared libraries, because the proprietary binary
is downloaded by `scripts/soloist-setup.sh` and mounted read-only at deploy
time. It needs host networking and the host Pulse socket, so it is not run
under `--network none`. The eighth is `dashboard-web`, the static Dashboard
artifact: it serves files only and the assistant embeds the same build, so it
runs healthy with `--network none`.
The ML images still start healthy with `--network none`. A
code-only `stt` rebuild logs `RUN python prefetch.py` as **CACHED** and rebuilds
only the app-code `COPY`; the same holds for `tts` (prefetch layer cached) and
`embed` (`SentenceTransformer(...)` layer cached), and `wakeword` keeps its
`models/clanker.tflite` layer cached and reruns only its `healthcheck.py`
`COPY`.
`web-search` wraps the pinned upstream SearXNG image and carries no weights
layer of its own.

## arm64 builds on the dev box (optional)

`make build-arm64` builds every image with
`DOCKER_DEFAULT_PLATFORM=linux/arm64` (Docker buildx). It needs QEMU binfmt
registered once:

```sh
docker run --privileged --rm tonistiigi/binfmt --install arm64
```

Use it as a build/smoke check only. QEMU cannot reproduce Pi 5 speed, 16K-page
breakage or Bluetooth, so latency work stays on the Pi.

## Troubleshooting

- **Deploy says the tree has local changes**: commit/stash them, or move
  scratch files out of the repo. `.env` is ignored and does not count.
- **Health wait fails**: the printed `docker compose ps` shows which service;
  its last 100 log lines follow. Increase `DEPLOY_HEALTH_TIMEOUT` only after
  checking that the service is actually starting (not crash-looping).
- **Rollback says an image is missing**: that SHA was never deployed here.
  Check it out and build it once with `git checkout <sha> && make build`, or
  deploy it without the SHA argument from a clean checkout.
- **Rollback after a reboot**: containers use `restart: unless-stopped`, so a
  reboot brings back the last deployed images. To change versions, run the
  deploy script again.
