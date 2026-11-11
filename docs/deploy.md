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
4. Runs `docker compose up -d --build` with `GIT_SHA` set to
   `git rev-parse --short HEAD`, so every image is tagged
   `clanka/<service>:<sha>`.
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
3. Runs `docker compose up -d --no-build`, so the existing SHA-tagged images are
   reused. Old images are pruned by hand (`docker image prune`).

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
      `sudo timedatectl set-timezone <Area/City>`
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
      `cd ~/clanka-bot && cp .env.example .env`, set `TZ` and ports. `.env` is
      gitignored and never baked into an image.
- [ ] `make check`
- [ ] `make deploy` (or `scripts/deploy.sh`)
- [ ] Verify: `docker compose ps`, `curl -s localhost:8080/healthz`
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

Verified with `tts` (#17) and `embed` (#19) in the stack: `make check` reports
OK with no warnings, and after touching only `server.py`/`app.py` a rebuild logs
the weights layer as `CACHED` and rebuilds only the code `COPY` (`tts` layer
7/8, `embed` layer 7/8). `assistant` is not an ML image.

The `wakeword` and `stt` images arrive with
[#18](https://github.com/Jairik/clanka-bot/issues/18); `make check` covers them
as soon as they are in the Compose file. Their full-image, arm64 and offline
verification is still pending until then.

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
