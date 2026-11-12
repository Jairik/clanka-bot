# Deploying the stack

The same Compose stack runs on the dev box and on the Pi. There is no registry,
CI or auto-deploy: the Pi tracks `main` and builds its own arm64 images when you
SSH in and run `scripts/deploy.sh` (decided in
[#13](https://github.com/Jairik/clanka-bot/issues/13)). This ticket does not run
anything on the Pi; the checklist below is for when it moves there.

## Everyday commands

```sh
make build       # build every service image for this host
make up          # start the stack in the background
make test        # assistant tests + deployment tests
make check       # check compose/Dockerfile deployment conventions
make deploy      # the Pi path: pull, build, start, wait for health
make rollback SHA=<commit>
make build-arm64 # optional QEMU arm64 build of every image (not for latency)
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
   stack grows from `assistant` alone to all five services.

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

The Dashboard is a separate listener served by `assistant`, using
server-rendered Go templates and htmx. It is **off** until a Tailscale owner is
configured, and it is meant to be reached only through `tailscale serve`, never
published to the LAN or the internet.

1. **Set the owner.** Put the exact login from `tailscale status --json` (or the
   admin console) in `.env`, and keep the loopback port unless it clashes:

   ```sh
   ASSISTANT_DASHBOARD_PORT=8082
   ASSISTANT_DASHBOARD_OWNER=you@example.com
   ```

   Then deploy or restart the assistant container. An empty owner disables the
   listener. If you change `ASSISTANT_DASHBOARD_PORT`, use the same port in the
   `tailscale serve` command below. `assistant/config.yaml` documents
   `dashboard.owner`, `addr` and `health_timeout`.

2. **Enable HTTPS on the tailnet** if it is not already on. `tailscale serve`
   provisions the certificate for the Pi's `*.ts.net` name.

3. **Proxy the loopback port from the Pi host**, not from inside a container:

   ```sh
   tailscale serve --bg 8082
   tailscale serve status
   ```

   Serve sends `https://<pi-name>.<tailnet>.ts.net/` to
   `http://127.0.0.1:8082`; opening that URL from any device in the tailnet
   shows the Health page.

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
authenticated `Tailscale-User-Login`. The Dashboard requires exactly one such
header and compares it to the configured owner; other identities, missing
headers, duplicate headers and tagged devices are refused with 403.

Check the listener locally:

```sh
# No identity header: rejected.
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8082/health

# The configured owner: 200 and the Health page.
curl -s -H 'Tailscale-User-Login: you@example.com' http://127.0.0.1:8082/health | head
```

The second command is only a local smoke test; from another device use the
HTTPS `*.ts.net` URL, which carries the real identity.

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
the README for the curl examples.

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

The Health page shows the five current Compose services (`embed`, `assistant`,
`wakeword`, `stt`, `tts`), the resolved location and each Tool module that
declares a health check. `embed` is checked over HTTP `/healthz`; the three
Wyoming services are checked with `describe`; `assistant` is the process serving
the page. One refresh is bounded by `dashboard.health_timeout` (3 s), and checks
that overrun are shown as timed out. These are endpoint checks, not Docker's
container health field.

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

- [ ] Make the Echo Dot the default sink and check playback:
      `pactl set-default-sink <sink>`; `speaker-test -t wav -c 2`.
- [ ] Compose already mounts the host's Pulse socket into `assistant` only
      (#17); set `ASSISTANT_UID`/`ASSISTANT_GID` to the user that owns it and
      `ASSISTANT_PULSE_SINK` to the Echo Dot's sink. Never give the socket to
      the ML services.

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
- [ ] The separate `assistant-voices` volume holds custom voice states at
      `/app/data/voices` in `assistant`; `tts` mounts only that volume,
      read-only, at `/voices`. Only the voices subtree is exposed, never the
      database or secrets. Back it up with `assistant-data`.
      `docker compose down -v` deletes both: back them up first if the
      settings, sounds, voices or secrets matter.
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
`clanka/wakeword`, not used as a separately tagged upstream image). That is what
gives every service the SHA tag that rollback needs. `make check` also enforces
the image tag, build context, restart policy and local-only health check.

ML images (`wakeword`, `stt`, `tts`, `embed`) must download or copy model
weights in an early layer **before any `COPY` of application code**, so editing
Go/Python code reuses the weights layer instead of re-downloading a model.
Dependency manifests (`requirements.txt`, `pyproject.toml`, `go.mod`, …) and the
fetch script itself (`prefetch.py`) may be copied before the weights layer.

`scripts/check-compose.sh` (run by `make check`) parses each locally built ML
Dockerfile and fails when a detected weights download comes after the first
code `COPY`; it warns when it cannot detect a weights layer at all. It
recognises `snapshot_download`, `hf_hub_download`, `from_pretrained`,
`SentenceTransformer(...)`, `RUN python prefetch.py`, `/models` and the
download CLIs.

Verified across the five-service stack: `make check` reports OK, `make build-arm64`
builds `assistant`, `embed`, `tts`, `wakeword` and `stt` as `arm64`, and each
image starts healthy with `--network none`. A code-only `stt` rebuild logs
`RUN python prefetch.py` as **CACHED** and rebuilds only the app-code `COPY`;
the same holds for `tts` (prefetch layer cached) and `embed`
(`SentenceTransformer(...)` layer cached), and `wakeword` only reruns its
`healthcheck.py` `COPY` because its models come from the base image.

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
