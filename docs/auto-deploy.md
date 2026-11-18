# Auto-deploy on the Pi

The Pi turns `main` into the running stack by itself: a systemd timer polls the
remote, and when `main` has moved to a commit whose CI checks are green, it runs the normal `scripts/deploy.sh`
path. This reverses #13's "no auto-deploy", which assumed nothing could check a
commit before it landed. CI now runs on every push, and the checks API is
readable without a token for this public repo, so the timer can refuse a red
`main` instead of putting it in the living room.

Nothing is exposed to the internet and no token is stored on the Pi. The timer
only makes an outbound `git fetch` and one unauthenticated HTTPS request; the
push itself still comes from the desktop.

## What runs

- `scripts/deploy-watch.sh` — one poll: fetch, compare, gate on CI, then
  `scripts/deploy.sh`. Safe to run at any time and a no-op when nothing moved.
- `scripts/install-deploy-watch.sh` — writes and enables the systemd units.
- `/etc/systemd/system/clanka-deploy.service` + `clanka-deploy.timer` — the
  timer fires the oneshot service 3 min after boot and every 30 s after that,
  so a green `main` is live about half a minute after CI finishes.

Each tick, in order:

1. Take a non-blocking `flock`; a second tick during a build exits immediately,
   so a long rebuild never stacks up runs.
2. Refuse to act on a dirty checkout (it would fail `deploy.sh` anyway) and
   report it rather than failing the unit.
3. `git fetch origin main`. A checkout on another branch (its upstream is not
   `origin/main`, or it has none) is left alone without touching Docker. So is
   a checkout whose `HEAD` is not an ancestor of `origin/main` — local commits
   on `main`, or diverged history. If `HEAD` already is `origin/main`, stop.
4. Optionally wait for `main` to settle. A commit younger than
   `DEPLOY_WATCH_MIN_AGE` minutes (default 0, so off) is not deployed yet. The
   age is the commit's own timestamp. The window is off because the CI gate
   already coalesces a burst of merges: CI cancels a superseded run, so only
   the newest tip ever goes green. It was 15 minutes while the Pi rebuilt every
   image itself; raise it again if you are landing changes the Pi has to build
   (see "Build load" below). `--force` and a manual `make deploy` skip it.
5. Ask GitHub for the commit's check runs. Still-running checks mean "try
   again next tick"; a failure, timeout or cancellation means "never deploy
   this commit" — the running stack stays as it is until a new commit lands.
   A missing or unreachable API is a warning, not a block: `HEAD` equality
   still prevents redundant builds.
6. Wait while the Assistant is busy. The watcher asks `GET /busy` on the
   host-published health port (`127.0.0.1:8080`). While music is playing (from
   a voice request or from a phone through the receiver), a reply is being
   spoken or an alarm is ringing, the tick logs the reason and exits; the next
   tick asks again. A deploy is held back at most `DEPLOY_WATCH_MAX_DEFER`
   minutes (default 120) past the settle window, so music left on all day
   cannot stall `main` forever. An assistant that does not answer never blocks
   a deploy, and `--force` skips this wait.
7. Run `scripts/deploy.sh` unchanged: `git pull --ff-only`, reuse or pull the
   images CI published for the commit (building only any that are missing),
   run them by content key, `docker compose up -d --no-build --remove-orphans`,
   then the bounded health wait and the best-effort old-image prune. Only the
   services whose image or config changed are recreated. A prune warning
   leaves the deployed stack green. The CI gate in step 5 includes the
   **Pi images** workflow, so the images exist by the time this runs.
8. Re-read `HEAD` and report `deployed <sha>` only when it is the commit the
   watcher asked for. A deploy that exits 0 but leaves `HEAD` elsewhere is an
   error that names both SHAs, so the journal never claims a build the host is
   not running.

`deploy.sh` stays the only thing that builds or starts containers. The watcher
decides *when*, never *how*, so a manual `make deploy`, a rollback and a timer
tick produce the same result.

For the timer, the checkout must stay on a branch that tracks `origin/main`
(normally `main`). The watcher enforces this: a branch whose upstream is not
`origin/main`, or a branch with local commits ahead of `origin/main`, is logged
and left alone without touching Docker. A manual `make deploy` applies the same
rule in `deploy.sh` after its pull: a branch with commits that are not on its
upstream is refused, so only pushed commits are built. A feature branch that is
pushed and tracks its own remote branch still deploys by hand. After a
rollback, `deploy.sh` checks the old commit out detached and the next plain
deploy returns to the tracked branch; leave it on `main` afterwards. If the
checkout loses its upstream, restore it with
`git branch --set-upstream-to=origin/main main`.

## Install on the Pi

Over Tailscale SSH, in the checkout the stack already runs from:

```sh
cd ~/clanka-bot
sudo bash scripts/install-deploy-watch.sh   # or: sudo make deploy-watch-install
```

Pass `--user <name>` only when `SUDO_USER` cannot name the account that should
run deploys (the installer refuses rather than guessing).

The installer reads the run-as user from `SUDO_USER` and the repo path from its
own location, writes both units, then `daemon-reload` and
`enable --now clanka-deploy.timer`. It sets `SupplementaryGroups=docker`
because a system unit started with `User=` does not inherit the account's
group memberships, so without it `docker` is unreachable for the deploy.
`DEPLOY_HEALTH_TIMEOUT=300` is set in the unit because a Pi rebuild is slower
than the 120 s default. So are `COMPOSE_PARALLEL_LIMIT=1` (one image at a time,
so a rebuild cannot starve the host) and `TimeoutStartSec=1800` (a build that
has not finished in half an hour has hung, not run slow — see "Build load").

Re-running the installer updates the units and restarts the timer. Pause or
remove:

```sh
sudo systemctl stop clanka-deploy.timer       # pause, keep the units
sudo systemctl start clanka-deploy.timer      # resume
sudo bash scripts/install-deploy-watch.sh --status
sudo bash scripts/install-deploy-watch.sh --uninstall
```

`--status` prints the timer's next fire time and the service's last result;
stopping the timer leaves the stack running, so a paused deploy keeps serving
the last good commit.

## Watch it work

```sh
systemctl list-timers clanka-deploy.timer
journalctl -u clanka-deploy -f
```

Every line is prefixed `deploy-watch:` and the deploy it starts keeps its own
`deploy:` lines in the same journal stream:

| Line | Meaning |
| --- | --- |
| `already on <sha>` | nothing new; the normal quiet case |
| `origin/main <sha> moved <n>s ago; waiting <n>s for main to settle` | the tip is younger than `DEPLOY_WATCH_MIN_AGE`; a merge burst is still arriving, so no build yet |
| `working tree has local changes` | the checkout was edited by hand; commit, stash or move the file. Try-out wake models belong in `wakeword/models/local/`, which is gitignored |
| `checkout is on '<branch>' (upstream <upstream>), not origin/main; leaving it alone` | the checkout is on another branch (or has no upstream); the timer only tracks `main`; set the upstream with `git branch --set-upstream-to=origin/main <branch>` |
| `checkout is not behind origin/main (local commits?); leaving it alone` | `HEAD` is ahead of or diverged from `origin/main`; push the commit or roll back |
| `CI is still running for <sha>` | a check has not finished; the next tick retries |
| `assistant is busy (<reasons>); deferring <sha> to a later tick` | music, a reply or a ringing alarm is in progress; the next tick asks again |
| `assistant is still busy (<reasons>) after <n>m of deferral; deploying <sha> anyway` | the deferral cap was reached; the deploy goes ahead |
| `CI is red for <sha>` | the commit failed a check; it will not be deployed |
| `no CI checks reported for <sha>; deploying anyway` | the commit has no check runs at all |
| `CI status for <sha> is unknown; deploying anyway` | the API could not be reached or parsed; see "a network blip is not a CI failure" below |
| `deploying <sha> (was <sha>)` | the deploy started |
| `deploy returned 0 but HEAD is <sha>, not <sha>; refusing to report success` | `deploy.sh` succeeded but left the checkout on another commit; investigate before trusting the build |
| `image pruning failed; the deployment is still healthy` | the old-image cleanup could not read its inventory; the stack is up, and the next deploy retries |
| `deploy of <sha> failed (exit <n>)` | the build or the health wait failed; see the `deploy:` lines and `docker compose ps` |

A one-off check without deploying:

```sh
./scripts/deploy-watch.sh --dry-run
```

## Failure behaviour

- **Red CI**: the commit is skipped, not reverted. A later commit that is green
  deploys normally; to override, run `./scripts/deploy-watch.sh --force` or
  `make deploy` by hand.
- **Failed build or unhealthy stack**: the new containers are already up, and
  the health wait only reports the failure. The timer does not roll back — go
  back explicitly with `make rollback SHA=<previous commit>` (see
  `docs/deploy.md`).
- **Dirty checkout**: the timer keeps skipping every tick and logs why. This is
  the one steady-state fault worth an alert; the journal makes it obvious.
- **Local commits or another branch**: the timer skips every tick with one
  explanatory line and never rebuilds, because `deploy.sh` builds the local
  `HEAD`. Push the commit, switch back to `main`, or push the branch and give
  it its own upstream to deploy it by hand. `--force` does not override this
  refusal; it only redeploys a checkout that already equals `origin/main`.
- **Pi reboots**: `restart: unless-stopped` brings the last deployed images
  back, and the timer resumes 3 min after boot, so a reboot never silently
  pulls a new commit while audio is still initialising.
- **GitHub unreachable**: the tick logs `CI status ... is unknown` and deploys
  anyway. A network blip is not a CI failure, and a commit that is already at
  `HEAD` is skipped regardless, so this cannot cause a redundant rebuild loop.

## Build load

Images are built by GitHub Actions, not on the Pi ([deploy.md](deploy.md#images-from-ci)),
so a normal deploy is a pull. The Pi only builds a service itself when no
image is published for its key: CI failed to publish it, the Pi's `.env` build
args differ from `deploy/pi-images.env`, or the registry is unreachable. That
fallback has the old cost (from three minutes with a warm layer cache to half an
hour), so the guards below still matter:

- `DEPLOY_WATCH_MIN_AGE` (the settle window, off by default) refuses to
  deploy a tip younger than that many minutes. The CI gate already folds a
  burst of merges into one deploy, but if you are landing changes the Pi will
  have to build itself, set it to 15 (the old default) so each build covers
  the whole burst.
- The unit sets `COMPOSE_PARALLEL_LIMIT=1`. Compose builds services
  concurrently by default, and eight parallel builds on an 8 GB Pi 5 is enough
  to thrash swap hard: `tailscaled` and `sshd` stopped getting scheduled for
  tens of seconds at a time, so the host looked offline (no handshake, no SSH
  banner) while the kernel still answered pings.

If a build does hang, `TimeoutStartSec=1800` fails the unit half an hour in
instead of holding the flock for a full hour, and the journal's last BuildKit
step names what was running. `journalctl -k -b -1 | grep -i 'killed process'`
separates an OOM kill from an I/O stall; a box that answers ICMP but never
sends an SSH banner is starved, not down.

## Disk space

Each deploy tags a `clanka/<service>:<sha>` set (the ledger) next to the
`clanka/<service>:key-<key>` tags Compose runs, so `deploy.sh` prunes the older
sets after the health wait, then any key tag no kept commit still uses. It keeps the deployed set, the newest
`DEPLOY_KEEP_IMAGES` **commits** (default 3) and any pinned ones, never touches
non-SHA tags or sets used by running containers, and only warns if the cleanup
fails. The window counts distinct commits, not tags, so a commit tagged several
ways (a short deploy tag plus its longer spellings, or a partial-build tag) uses
a single slot and every tag of a kept commit survives. The full policy and the
corrected manual commands are in
[deploy.md](deploy.md#image-retention-and-disk-space); never use
`docker image prune -a` or `docker system prune`, because rollback needs the
older tags.

## Cost and what it interrupts

A deploy recreates only the containers whose image or config changed. A commit
that touches only the dashboard restarts `dashboard-web` (the assistant serves
its embedded copy meanwhile); one that touches the assistant restarts the
assistant, which is unreachable for the seconds its health check takes, and
cuts any audio it was playing. The busy wait (step 6) keeps that from landing
mid-song or mid-reply, but it cannot see a conversation you are about to start;
pause the timer when you are demoing. True zero downtime for the assistant is
not on offer: it is the one container that owns the microphone and speaker
(ADR-0001), so two cannot run side by side while traffic switches. Rebuilds are incremental, but a change
that invalidates a weights layer re-downloads that model, which on the Pi is
minutes, not seconds — the flock makes the ticks wait for it rather than queue.

Only `main` is watched. Branches and pull requests never reach the Pi, and
nothing here publishes images to a registry.

## Knobs

`deploy-watch.sh` reads these from the environment (the unit sets
`DEPLOY_HEALTH_TIMEOUT`; the rest default):

| Variable | Default | Meaning |
| --- | --- | --- |
| `DEPLOY_WATCH_CI` | `1` | `0` deploys without asking GitHub about the checks |
| `DEPLOY_WATCH_MIN_AGE` | `0` | minutes a commit must have been on `main` before it is deployed; `0` (the default) disables the settle window, `--force` skips it |
| `DEPLOY_WATCH_BUSY_URL` | `http://127.0.0.1:8080/busy` | the Assistant's busy probe; empty disables the busy wait |
| `DEPLOY_WATCH_MAX_DEFER` | `120` | minutes past the settle window a busy Assistant may hold a deploy back |
| `DEPLOY_WATCH_REPO` | `Jairik/clanka-bot` | repository whose check runs gate the deploy |
| `DEPLOY_WATCH_API` | `https://api.github.com` | API base; tests point it at a stub |
| `DEPLOY_WATCH_LOCK` | `/tmp/clanka-deploy-watch.lock` | single-run lock |
| `DEPLOY_HEALTH_TIMEOUT` | `120` (`300` in the unit) | passed through to `deploy.sh` |
| `DEPLOY_KEEP_IMAGES` | `3` | newest SHA image sets to keep; the deployed and pinned sets always survive |
| `DEPLOY_PINNED_SHAS` | empty | comma-separated SHA prefixes the image prune must keep |
| `DEPLOY_PRUNE_BUILD_CACHE` | `0` | `1` also prunes the host-wide build cache during the deploy, best effort |
| `DEPLOY_BUILD_CACHE_AGE` | `168h` | age bound for the opt-in build-cache prune |

The poll interval is the timer's `OnUnitActiveSec` (30 s, with `AccuracySec=1s`
so systemd does not round it up to a minute). An idle tick is one `git fetch`;
GitHub is asked only after `main` moves, so even this interval stays far under
the 60 requests an hour GitHub allows without a token. Edit the unit (or the
installer and re-run it) to change it; the watcher itself is stateless, so any
interval works.
