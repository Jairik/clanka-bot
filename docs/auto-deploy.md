# Auto-deploy on the Pi

The Pi turns `main` into the running stack by itself: a systemd timer polls the
remote, and when `main` moves to a commit whose CI checks are green it runs the
normal `scripts/deploy.sh` path. This reverses #13's "no auto-deploy", which
assumed nothing could check a commit before it landed. CI now runs on every
push, and the checks API is readable without a token for this public repo, so
the timer can refuse a red `main` instead of putting it in the living room.

Nothing is exposed to the internet and no token is stored on the Pi. The timer
only makes an outbound `git fetch` and one unauthenticated HTTPS request; the
push itself still comes from the desktop.

## What runs

- `scripts/deploy-watch.sh` — one poll: fetch, compare, gate on CI, then
  `scripts/deploy.sh`. Safe to run at any time and a no-op when nothing moved.
- `scripts/install-deploy-watch.sh` — writes and enables the systemd units.
- `/etc/systemd/system/clanka-deploy.service` + `clanka-deploy.timer` — the
  timer fires the oneshot service 3 min after boot and every 5 min after that.

Each tick, in order:

1. Take a non-blocking `flock`; a second tick during a build exits immediately,
   so a long rebuild never stacks up runs.
2. Refuse to act on a dirty checkout (it would fail `deploy.sh` anyway) and
   report it rather than failing the unit.
3. `git fetch origin main`. If `HEAD` already is `origin/main`, stop.
4. Ask GitHub for the commit's check runs. Still-running checks mean "try
   again next tick"; a failure, timeout or cancellation means "never deploy
   this commit" — the running stack stays as it is until a new commit lands.
   A missing or unreachable API is a warning, not a block: `HEAD` equality
   still prevents redundant builds.
5. Run `scripts/deploy.sh` unchanged: `git pull --ff-only`, build the
   SHA-tagged images, `docker compose up -d --build --remove-orphans`, then the
   bounded health wait.

`deploy.sh` stays the only thing that builds or starts containers. The watcher
decides *when*, never *how*, so a manual `make deploy`, a rollback and a timer
tick produce the same result.

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
than the 120 s default.

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
| `working tree has local changes` | the checkout was edited by hand; commit, stash or move the file |
| `CI is still running for <sha>` | a check has not finished; the next tick retries |
| `CI is red for <sha>` | the commit failed a check; it will not be deployed |
| `no CI checks reported for <sha>; deploying anyway` | the commit has no check runs at all |
| `CI status for <sha> is unknown; deploying anyway` | the API could not be reached or parsed; see "a network blip is not a CI failure" below |
| `deploying <sha> (was <sha>)` | the deploy started |
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
- **Pi reboots**: `restart: unless-stopped` brings the last deployed images
  back, and the timer resumes 3 min after boot, so a reboot never silently
  pulls a new commit while audio is still initialising.
- **GitHub unreachable**: the tick logs `CI status ... is unknown` and deploys
  anyway. A network blip is not a CI failure, and a commit that is already at
  `HEAD` is skipped regardless, so this cannot cause a redundant rebuild loop.

## Cost and what it interrupts

A deploy replaces containers, so an auto-deploy in the middle of a conversation
cuts the reply and reloads the models. That is the price of tracking `main`;
pause the timer when you are demoing. Rebuilds are incremental, but a change
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
| `DEPLOY_WATCH_REPO` | `Jairik/clanka-bot` | repository whose check runs gate the deploy |
| `DEPLOY_WATCH_API` | `https://api.github.com` | API base; tests point it at a stub |
| `DEPLOY_WATCH_LOCK` | `/tmp/clanka-deploy-watch.lock` | single-run lock |
| `DEPLOY_HEALTH_TIMEOUT` | `120` (`300` in the unit) | passed through to `deploy.sh` |

The poll interval is the timer's `OnUnitActiveSec`. Edit the unit (or the
installer and re-run it) to change it; the watcher itself is stateless, so any
interval works.
