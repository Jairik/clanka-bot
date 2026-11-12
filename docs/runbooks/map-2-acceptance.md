# Map 2 physical and live acceptance runbook

This runbook is for the gates that need a person, a Raspberry Pi, a microphone,
a speaker, a phone and real accounts. Nothing here is automated, and nothing
here has been run. Each section says what it closes, what you need, the steps,
a results table and the pass/fail rule.

Run the sections in order. Later sections assume the earlier ones passed.

## Ground rules

- Put secrets in `.env` (never committed) or in the Dashboard `/secrets` page.
  Never paste a token, a refresh token, a full `.env`, or a full environment
  dump into an issue.
- Use the Dashboard `/settings` page for live settings (for example
  `music.max_volume`). Do not edit `assistant/config.yaml` for a live test.
- Commands use `docker compose` from the repo root unless a step says otherwise.
- A box on a GitHub issue is ticked only by the owner, and only after the
  evidence is attached. This runbook does not tick boxes.
- Mark each result `PASS`, `FAIL` or `NOT RUN`. `NOT RUN` never counts as a
  pass.

## Common prerequisites

- The stack is deployed per [docs/deploy.md](../deploy.md) (`make deploy`), and
  `docker compose ps` shows every service as running or healthy.
- You have the Pi's Tailscale name and your Tailscale login (`tailscale status --json`).
- You can reach the Pi's assistant logs: `docker compose logs -f assistant`.
- A quiet room for the first run. Note the room, the device positions and the date.

Quick stack check:

```sh
make deploy
docker compose ps
docker compose logs --tail=50 assistant
```

## 1. Dashboard live on the tailnet

**Closes:** the first two "Human steps before calling the Dashboard live" in
[docs/map-2-readiness.md](../map-2-readiness.md). There is no ticket box for
this step; it is a readiness gate.

**Prerequisites:** Tailscale installed and logged in on the Pi and on your
phone or laptop. The Pi's MagicDNS name.

**Steps:**

1. Set the owner and hosts in `.env`:

   ```sh
   ASSISTANT_DASHBOARD_PORT=8082
   ASSISTANT_DASHBOARD_OWNER=you@example.com
   ASSISTANT_DASHBOARD_HOSTS=pi.tailnet-name.ts.net
   ```

2. Restart the assistant so it reads the new values:

   ```sh
   docker compose up -d assistant
   ```

3. Publish the Dashboard over Tailscale, and check the serve status:

   ```sh
   tailscale serve --bg 8082
   tailscale serve status
   ```

4. Check the loopback listener on the Pi. Without an identity header it must
   be refused (not `200`). With the owner header it must be `200`:

   ```sh
   curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8082/health
   curl -s -o /dev/null -w '%{http_code}\n' -H 'Tailscale-User-Login: you@example.com' http://127.0.0.1:8082/health
   ```

5. From your phone (on the tailnet), open `https://pi.tailnet-name.ts.net/`.
   Check these pages load for the owner: `/health`, `/settings`, `/secrets`,
   `/activity`, `/voices`, `/personas`.
6. From a device on the home LAN that is not on the tailnet, try the Pi's LAN
   address on port 8082. It must not connect.

**Results:**

| Step | Result (PASS/FAIL/NOT RUN) | Notes |
| --- | --- | --- |
| 4 loopback without header | | |
| 4 loopback with owner header | | |
| 5 tailnet HTTPS pages | | |
| 6 LAN port 8082 refused | | |

**Pass:** step 4 returns the expected codes, every page in step 5 loads for the
owner, and step 6 is refused. **Fail:** any other result. Do not publish the
Dashboard to the LAN or the internet to get past a failure.

## 2. #62 Notifications over ntfy

**Closes:** [#62](https://github.com/Jairik/clanka-bot/issues/62). Boxes:

- "send that to my phone" delivers a Notification
- A Timer or Alarm that times out unanswered sends a missed-alert Notification
- Notifications are muted during Quiet hours
- Topic and token are generated once and kept in the secrets table, not `config.yaml`
- Each new Tool uses `make new-tool`, passes the conformance suite, and adds routing-eval cases (checked by the `make test` and `make routing-eval` run)

**Prerequisites:** the ntfy app on your phone. The default server is
`https://ntfy.sh` (see the ntfy section of `assistant/config.yaml`). Choose a
random topic name. Do not reuse a public topic that others can guess.

**Steps:**

1. In the Dashboard `/secrets` page, set `NTFY_TOPIC` to your random topic.
   Set `NTFY_TOKEN` only if your ntfy server issues one. Leave it empty for
   the public server.
2. Confirm the topic is in the secrets table and not in `config.yaml`:

   ```sh
   rg -n 'NTFY_TOPIC|NTFY_TOKEN' assistant/config.yaml || echo 'not in config.yaml'
   ```

3. Subscribe to the same topic in the phone's ntfy app.
4. Restart the assistant if you changed `.env` (not needed for Dashboard secrets):

   ```sh
   docker compose up -d assistant
   ```

5. Ask a question that gets a reply, then say "send that to my phone". Note
   the time. A Notification must arrive on the phone.
6. Set a one-minute timer by voice. Do not answer it. When it times out, a
   missed-alert Notification must arrive.
7. In the Dashboard `/settings` page, turn on Quiet hours for a window that
   covers now (see the quiet section of `assistant/config.yaml`). Then repeat
   step 5. No Notification should arrive. Turn Quiet hours off afterwards.
8. Run the suites from the repo root:

   ```sh
   make test
   make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085
   ```

   `routing-eval` needs the real embed service running; see
   [docs/routing-eval.md](../routing-eval.md). The `ROUTING_EVAL_EMBED_URL`
   value above is an example. Use the port your embed service is on.

**Results:**

| Step | Result | Time sent / received | Notes |
| --- | --- | --- | --- |
| 5 send to phone | | | |
| 6 missed-alert | | | |
| 7 quiet hours muted | | | |
| 8 make test | | | |
| 8 make routing-eval | | | |

**Pass:** steps 5, 6 and 7 behave as written and step 8 exits 0. **Fail:** any
step that sends a Notification during Quiet hours, or any missing Notification.

## 3. #68 GitHub Tools

**Closes:** [#68](https://github.com/Jairik/clanka-bot/issues/68). Boxes:

- "open an issue on clanka-bot: timers don't survive restart" asks for a Confirmation, then creates it
- "any PRs waiting on me" and "what are my GitHub notifications" work
- A missing PAT is reported clearly and appears in the module health check
- Each new Tool uses `make new-tool`, passes the conformance suite, and adds routing-eval cases

**Prerequisites:**

- A fine-grained PAT for `GITHUB_TOKEN`, limited to the repos you want the
  assistant to use. Its issue write scope is needed for the issue test.
- A classic PAT with the `notifications` scope only, for `GITHUB_NOTIFICATIONS_TOKEN`.
  This token is optional. Without it, the notifications Tool says so.
- The two-token contract is in `assistant/tools/github/github.go`; the owner
  chose it on 2026-11-12 (#68).

Either set both tokens in `.env` (Compose passes them to the assistant as a
bootstrap) or in the Dashboard `/secrets` page. A value stored on `/secrets`
wins over `.env`.

**Steps:**

1. In `/secrets`, set `GITHUB_TOKEN` (fine-grained). Optionally set
   `GITHUB_NOTIFICATIONS_TOKEN` (classic, notifications only).
2. Check the Health page at `/health` over Tailscale. The GitHub module must
   show as configured.
3. Say "open an issue on clanka-bot: timers don't survive restart". The
   assistant must ask for a Confirmation before it creates anything. Say yes.
   Check that the issue exists on GitHub. Close it afterwards.
4. Say "any PRs waiting on me" and "what are my GitHub notifications". Both
   must answer from GitHub. Notifications only work with the classic token.
5. Missing PAT: in `/secrets`, delete `GITHUB_TOKEN`. Say the issue request
   again. The reply must name the missing `GITHUB_TOKEN` secret. Then `/health`
   must show the GitHub module as needing a token. Put the PAT back.
6. Run the suites from the repo root:

   ```sh
   make test
   make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085
   ```

**Results:**

| Step | Result | Notes (issue number, reply text, not the token) |
| --- | --- | --- |
| 2 health shows GitHub | | |
| 3 confirmation then issue | | |
| 4 PRs waiting on me | | |
| 4 GitHub notifications | | |
| 5 missing PAT reported and in health | | |
| 6 make test and routing-eval | | |

**Pass:** steps 3 to 5 behave as written, and step 6 exits 0. **Fail:** an
issue created without a Confirmation, a token value appearing in a reply or log,
or a missing PAT that is not reported.

## 4. #71 Spotify Connect pairing

**Closes:** [#71](https://github.com/Jairik/clanka-bot/issues/71). Boxes:

- librespot appears as a Connect device and plays through the assistant's sink
- The Connect button completes the OAuth flow and stores the refresh token as a secret
- Health shows librespot and Web API status separately
- Credentials survive container restarts

**Prerequisites:** a Spotify Premium account. A Spotify app with a client ID.
`SPOTIFY_CLIENT_ID` and `SPOTIFY_REDIRECT_URI` in `.env`. The redirect URI must
be the tailnet `/spotify/callback` URL from [docs/deploy.md](../deploy.md)
(Spotify section). The Pi and your phone must be on the same LAN for Connect
discovery. `librespot` uses host networking. Avahi runs inside its container, so
the host does not need it (see deploy.md).

**Steps:**

1. Start librespot and check it is healthy:

   ```sh
   docker compose up -d librespot
   docker compose ps librespot
   ```

2. On your phone, open Spotify, tap the Connect icon, and choose the device
   named by `SPOTIFY_DEVICE_NAME`. Play a track. Sound must come out of the
   Pi's speaker, not the phone.
3. Open `https://pi.tailnet-name.ts.net/spotify` over Tailscale. Click
   Connect and finish the OAuth flow in the browser.
4. In `/secrets`, confirm `SPOTIFY_REFRESH_TOKEN` now exists. Read the name
   only; do not copy the value.
5. Open `/health`. librespot status and Web API status must show as two
   separate lines, and both must be OK.
6. Restart and check that nothing needs pairing again:

   ```sh
   docker compose restart librespot assistant
   ```

   Wait for healthy, then check that the device still appears in Connect and
   that `SPOTIFY_REFRESH_TOKEN` is still in `/secrets`.

**Results:**

| Step | Result | Notes |
| --- | --- | --- |
| 1 librespot healthy | | |
| 2 Connect plays through Pi | | |
| 3 OAuth completes | | |
| 4 refresh token stored | | |
| 5 health split | | |
| 6 survives restart | | |

**Pass:** all six steps pass. **Fail:** any step, or a second pairing needed
after a restart.

## 5. #70 Wake word over music with echo cancel

**Closes:** [#70](https://github.com/Jairik/clanka-bot/issues/70). Boxes:

- Hit rate and false wakes recorded at three volumes, with and without echo-cancel
- A recommended `music.max_volume` (or none) and the host echo-cancel config are written up

**Prerequisites:** read [docs/spikes/wake-over-music.md](../spikes/wake-over-music.md)
in full. The spike is prep only. The protocol, the cell rules and the
exclusions are in that file, and this section does not repeat them. Get a
person to speak the wake word from the room. Do not use a loopback or a
recording through the speaker.

**Steps:**

1. Inspect the host audio (read-only):

   ```sh
   pactl info | sed -n '1,8p'
   pactl list short sinks
   pactl list short sources
   pactl list short modules | grep echo-cancel || true
   ```

2. Load the echo-cancel module with the real device names you found in step 1.
   Use the runtime command from the spike doc (section 2.2). Keep the module
   index it prints. Do not also add the persistent drop-in.
3. Verify both virtual nodes exist:

   ```sh
   pactl list short sources | grep aec_mic_source
   pactl list short sinks   | grep aec_music_sink
   ```

4. For AEC-on cells only, set `ASSISTANT_PULSE_SINK=aec_music_sink` and
   `ASSISTANT_PULSE_SOURCE=aec_mic_source` in `.env`, then run
   `docker compose up -d assistant librespot`. For AEC-off cells, point both
   back at the real sink and mic. Restart `assistant` between states.
5. Make the blank evidence file, then record one cell after each real run:

   ```sh
   python3 scripts/wake_over_music.py init --out /tmp/wake-over-music.json --volumes 25,50,75
   python3 scripts/wake_over_music.py record --file /tmp/wake-over-music.json \
     --volume 50 --aec on --trials 20 --hits 18 --false-wakes 1 \
     --exposure-min 30 \
     --started-at 2026-11-12T20:00:00+01:00 \
     --ended-at 2026-11-12T20:40:00+01:00
   ```

   The numbers above are placeholders. Use your real counts, the real volume
   you set, and the real timestamps. Count `voice: wake` lines from
   `docker compose logs assistant`. Repeat for all six cells.
6. Validate the matrix. Choose your accuracy target before you look at the
   results:

   ```sh
   python3 scripts/wake_over_music.py report --file /tmp/wake-over-music.json \
     --target-hit-rate 0.9 --target-false-per-hour 5
   ```

   The 0.9 and 5 above are examples only. Set your own targets.
7. Decide. Only after all six cells are measured and `report` says `MEASURED`,
   choose the cap or "no cap". Set `music.max_volume` in `/settings` if you
   choose a cap. Record the decision in the evidence file's `decision` block
   and in #70.
8. Roll back to the normal audio path when done (spike doc section 2.3):
   unload the module, clear the two `ASSISTANT_PULSE_*` names in `.env`, and
   restart `assistant` and `librespot`.

**Results:**

| Volume | AEC | Hits/trials | False wakes | Exposure (min) | Window | Recorded by report? |
| ---: | --- | --- | ---: | ---: | --- | --- |
| | off | | | | | |
| | on | | | | | |
| | off | | | | | |
| | on | | | | | |
| | off | | | | | |
| | on | | | | | |

Also record: PipeWire version, the module index, the real sink and source
names, and the AEC-on virtual names.

**Pass:** `report` says `MEASURED` for all six cells, and the decision block is
either `decided` with a value you chose, or still `pending` with no
`max_volume`. **Fail:** a cell with estimated counts, an AEC-on cell whose music
did not go to `aec_music_sink`, or a cap written without measurements.

## 6. #72 and #73 live voice checks

**Closes:** part of [#72](https://github.com/Jairik/clanka-bot/issues/72) (live
acceptance; the code is fake-tested only). Only the #72 checks can run now.
[#73](https://github.com/Jairik/clanka-bot/issues/73) is not built, so its
checks are listed for later and are not run.

**#72 boxes:**

- "play Radiohead" starts playback on the Pi
- "pause", "resume" and "next" act on music when it holds Focus, and on a Ring or reply first when they do
- Music ducks during replies, Rings and after the wake word, and restores after
- "what's playing" works
- `music.max_volume` is enforced
- Each new Tool uses `make new-tool`, passes the conformance suite, and adds routing-eval cases

**Prerequisites:** section 4 passed (Connect pairing and Web API). Music is
playing on the Pi. Focus order is ringing alert, then reply, then music
(`assistant/config.yaml`, Focus section).

**Steps:**

1. Say "play Radiohead". Music must start on the Pi.
2. Say "what's playing". The answer must name the current track.
3. Say "pause", then "resume", then "next" while music holds Focus. Each must act on the music.
4. Start a reply or a Ring while music plays, then say "pause". The Ring or reply
   must take the command first. Music must not pause.
5. Ducking: during a reply, a Ring, and right after the wake word, music volume
   must drop, then return to its previous level when the turn ends. Listen and
   note the level. Ducking runs only when the Spotify Web API app is
   configured (`spotify.client_id` and redirect URI).
   Stream-restore check: after a reply, say "pause", then "resume". Music must
   come back at full level, not the ducked level. Pulse's `module-stream-restore`
   can remember a stream's last volume for librespot, so also run
   `pactl list sink-inputs` while music plays and confirm the librespot input
   reads 100% between turns. The assistant resets librespot's stream to 100% at
   startup (best effort), so restart the assistant mid-duck once and confirm
   music is back to full.
6. `music.max_volume` enforcement: in `/settings`, set `music.max_volume` to a
   low test value (for example `30`). Say "set the music volume to 80". The
   volume must not go above 30. Then clear `music.max_volume` back to unset.
   This is an enforcement check, not the cap decision in section 5.
7. Run the suites from the repo root:

   ```sh
   make test
   make routing-eval ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085
   ```

**#73 boxes (not built; do not run yet):**

- "play my running playlist on shuffle" resolves via the cache and name matcher
- The cache refreshes every 6 hours and at startup
- `previous`, `shuffle`, `repeat`, `queue_add` and `like` work by voice
- Each new Tool meets the same conformance and routing-eval rules

**Results (#72):**

| Step | Result | Notes |
| --- | --- | --- |
| 1 play Radiohead | | |
| 2 what's playing | | |
| 3 pause/resume/next on music | | |
| 4 Ring or reply first | | |
| 5 ducking and restore | | |
| 6 max_volume enforced | | |
| 7 make test and routing-eval | | |

**Pass:** steps 1 to 7 pass. **Fail:** any command acting on the wrong holder,
volume above `music.max_volume`, or music that does not restore after ducking.

## 7. #69 and #74 RAM budget

**Closes:** [#69](https://github.com/Jairik/clanka-bot/issues/69) and
[#74](https://github.com/Jairik/clanka-bot/issues/74). Boxes:

- #69: peak and idle RAM per container are recorded in the repo; a go/no-go for phase 4 against the 6.5 GB threshold is stated
- #74: peak and idle RAM per container with librespot playing are recorded; a go/no-go against the 6.5 GB threshold is stated

**Prerequisites:** the stack running. Sections 4 and 6 done, so librespot and
the wake loop are real. A way to run a wake-to-reply load (speak to the
assistant).

**Steps:**

1. Idle snapshot, after the stack has been up for 10 minutes with no speech:

   ```sh
   docker stats --no-stream --format 'table {{.Name}}\t{{.MemUsage}}\t{{.MemPerc}}'
   ```

2. Start a sampler to a file, in another shell. Stop it with Ctrl-C after the
   load run. The sampler writes `timestamp, container, memory` lines:

   ```sh
   while true; do
     docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' | sed "s/^/$(date -Is) /"
     sleep 5
   done | tee ram-samples.log
   ```

   Save the file outside the repo, or name it so you do not commit it by accident.
3. Run the wake → STT → agent → TTS load: ten wake-to-reply turns with
   short questions, then five turns with a long reply.
4. Run the same load again with librespot playing (for #74). Keep the same
   turns.
5. From the sampler file, take the peak per container and the total across
   containers for each run. Idle is step 1.
6. State the go/no-go. The threshold is 6.5 GB total, as in the Map 2 spec
   (RAM budget). If the peak total is at or below 6.5 GB, the result is go.
   Above it, the result is no-go for phase 4.
7. Commit the table to the repo so the evidence lives there. Suggested path:
   `docs/evidence/map-2-ram.md`. This path does not exist yet; choose it when
   you commit.

**Results:**

| Container | Idle | Peak (no music) | Peak (librespot playing) |
| --- | ---: | ---: | ---: |
| assistant | | | |
| embed | | | |
| wakeword | | | |
| stt | | | |
| tts | | | |
| web-search | | | |
| librespot | | | |
| **Total** | | | |

Go/no-go against 6.5 GB: ____ (state the total used).

**Pass:** the peak total in both runs is at or below 6.5 GB, and both tables
are committed. **Fail:** a peak above 6.5 GB, or numbers from `docker stats`
that were not sampled during the load.

## 8. #76 Hermes and OpenCode attach spike

**Closes:** [#76](https://github.com/Jairik/clanka-bot/issues/76). Boxes:

- A Hermes job runs a small coding task on the desktop through `--attach` and returns a summary
- Findings on latency, failure modes and auth are written up
- A proposed ticket list for phases 6–7 is drafted

**Current state:** the spike is partial. The findings and ticket list are
already in [docs/spikes/hermes-opencode-attach.md](../spikes/hermes-opencode-attach.md).
The first box is **not met**. There is no Hermes install, no Tailscale on the
test machine, and the stored OpenCode Go key is expired. Re-check these before
running anything.

**Prerequisites:**

- Hermes installed on the desktop, and Tailscale up on the desktop and the
  machine that runs Hermes.
- A fresh OpenCode Go API key in the normal OpenCode auth store. **Manual step:**
  create and store the key through OpenCode's own sign-in or auth flow. Do not
  paste the key into a command, a file or an issue.
- Use `opencode run --server`. The `--attach` flag is rejected by the installed
  OpenCode build (v2.2.7 at the time of the spike). Check `opencode run --help`
  on your build before you start.

**Steps:**

1. Run the offline harness (fake provider, no real model):

   ```sh
   bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
   ```

   This must exit 0. It proves the transport and failure cases only. Do not
   report it as a Hermes job.
2. Check the key and the model without printing the key:

   ```sh
   opencode auth list
   opencode models | grep '^opencode-go/deepseek'
   ```

3. Run the authorised real coding check. It fails until the key is replaced:

   ```sh
   RUN_REAL=1 bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
   ```

4. Manual server and attach check, using a unique loopback port (from the
   spike doc, "Repeatable commands"):

   ```sh
   opencode run --server http://127.0.0.1:47653 \
     -m 'opencode-go/deepseek-v4.1-flash#max' --format json 'Reply OK.'
   ```

5. Run one real Hermes job over Tailscale that asks the desktop to make a small
   coding change and return a summary. **Manual step:** there is no helper for
   this. Record the job id, the time taken, the summary and whether the test
   passed.
6. Check the failure cases you saw: a bad key (expect `provider.auth` 401),
   and a timeout or cancel (expect the client to stop and the server to stay
   healthy).
7. Review the proposed phase 6–7 ticket list in the spike doc. The owner
   decides whether to create the issues. Nothing is created by this runbook.

**Results:**

| Step | Result | Notes |
| --- | --- | --- |
| 1 offline harness exit 0 | | |
| 2 key present, model listed | | |
| 3 real coding run | | |
| 4 manual --server reply | | |
| 5 real Hermes job over Tailscale | | |
| 6 auth and cancel behaviour | | |
| 7 ticket list reviewed | | |

**Pass:** box 1 passes only when step 5 returns a summary from a real Hermes
job over Tailscale. Boxes 2 and 3 are already written up; pass when the owner
accepts the doc. **Fail:** any result that comes only from the fake provider
being reported as a job or model latency.

## Reporting back

For each issue, comment with the following. Keep the comment short.

- Date, the commit SHA you ran (`git rev-parse --short HEAD`), and the Pi model.
- The section number and each step's result from the tables above.
- The table you filled in, with numbers, timestamps and file names. Do not
  paste hostnames, `.env` values, tokens, or full `pactl info` output.
- For #70: the completed `report` JSON summary (the operator file, not the
  synthetic fixture), the PipeWire version and the real device names.
- For #69 and #74: the committed RAM table and the go/no-go line.
- For #76: the job id and summary text, and the auth and timeout results.
- Any step you could not run, and why. Mark it `NOT RUN`.

Tick a box on the issue only if the owner has the evidence and agrees the
box is met. Do not tick a box for a step marked `NOT RUN`.
