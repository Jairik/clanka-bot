# clanka-bot

My lovely always-on voice assistant for stuff written in go. Kind of like a lobotomized jarvis.

## Run the prototype

The prototype is the `assistant` Go service in Docker, with a `wakeword`
(openWakeWord), an `stt` (Moonshine Voice), a `tts` (Pocket TTS) service, an
internal `web-search` (SearXNG) service for Agent-path lookups, and a
`soloist` Spotify Connect receiver.
`assistant` owns the microphone and speaker, and also exposes a health endpoint
and a localhost-only debug entry that accepts a typed transcript (entering the
pipeline just after STT). Soloist is the one music exception: it plays into
the same Pulse sink as the Assistant's speech (ADR-0004).

```sh
cp .env.example .env
docker compose up --build
```

The image downloads the Pocket TTS weights and default voice at build time,
anonymously, from the public `kyutai/pocket-tts-without-voice-cloning` bundle
and `kyutai/tts-voices`. Nothing is downloaded on start, so `tts` comes up
offline; the health check only passes once the model and default voice are warm.
Rebuild `tts` (not just restart it) if you change `TTS_LANGUAGE` or `TTS_VOICE`.

Check it and ask the time:

```sh
curl -s localhost:8080/healthz
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it"}'
```

The response is the spoken-form reply from the `time.now` Tool, plus the
routing path and a request id, and the reply is spoken on the configured Pulse
sink as it is synthesised (`"spoken": true`). The debug and `/api/` listeners
accept only a loopback `Host`, require `Content-Type: application/json` on a
mutation body, and reject a browser cross-site request; the documented
`localhost` curl calls above are the supported client. Paraphrases work too, via the
`embed` MiniLM service that Compose starts alongside the assistant:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it right now"}'
```

Weather answers on the Fast path too. The assistant resolves the city from the
Pi's public IP at startup and every 24h (only city, region and timezone are
kept; coordinates are never stored) and asks the weather providers for current
conditions:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d "{\"text\":\"what's the weather like\"}"
```

Weather uses [WeatherAPI](https://www.weatherapi.com/docs/) when
`WEATHERAPI_API_KEY` is set in `.env` or Dashboard > Secrets, with Open-Meteo
and [wttr.in](https://github.com/chubin/wttr.in#json-output) as independent
fallbacks. Without a key, the two keyless providers still work. Each provider
gets 250 ms before the next fallback also starts; the first successful answer
wins and all share a 1 s Tool deadline to leave time for the voice reply.
Successful answers, including fallback answers, are cached for 15 minutes and
refreshed on the next request after expiry; failures say "Weather is
unavailable right now." The complete voice turn still depends on STT/TTS
latency and upstream availability.

If the IP lookup fails, the last known place is used, then the optional
`location.fallback` values in config, then no place at all.

Anything the Fast path does not handle goes to the Agent path. Its OpenRouter
key resolves from the write-only secrets store first, then the
`OPENROUTER_API_KEY` environment variable in `.env`; either alone is enough,
and the stored value wins. A key stored from the Dashboard takes effect on the
next turn without a restart. Without any key the Agent path answers with the
configured Unavailable message and the Fast path keeps working. Every provider
request forces OpenRouter's `provider.zdr` and
`provider.data_collection: "deny"` in code.

### Web search

The Agent can call `web.search` when an answer needs current information, for
example "who won the match last night". The Tool asks the local `web-search`
(SearXNG) service for five results by default (up to ten) and gives the model
only each result's title, URL and a short snippet; the answer is grounded in
those results instead of a guess. It is an Agent-only Tool: open-ended lookups must not claim a
semantic Fast path route, so no Example can send one there.

The `web-search` container is internal-only: Compose publishes no host port,
runs no valkey and mounts no database. It runs two Granian workers with the
limiter off, keeps three fast engines (DuckDuckGo, Brave, Bing) and a 1.5 s
engine timeout in `searxng/config/settings.yml`, and enables SearXNG's JSON
output for the Tool. The image is pinned to
`docker.io/searxng/searxng:2026.11.12-de722ab1b`. `tools.web.backend` selects
the Provider implementation; only `searxng` exists and an unknown value fails
startup. If SearXNG is down, times out, returns invalid JSON or an oversize
body, the Tool fails cleanly and the Agent can say it couldn't search. Results
are bounded (query, count, title, URL and snippet) and the HTTP client keeps
connections alive between calls. See [`searxng/README.md`](searxng/README.md)
for the pinned image, worker/server choice and a manual smoke test.

### Reading a page

The Agent can also call `web.fetch` to read one public page, for example
"read me that article" after a search. It resolves the host once, refuses any
loopback, private, link-local, CGNAT/tailnet, multicast or special-purpose
address, pins the validated IPs for the request, and revalidates every
redirect. Environment HTTP proxies are ignored so a proxy cannot bypass that
dial policy. Responses must be HTML, the decompressed body is bounded
(`tools.web.fetch.max_bytes`, 2 MiB) and the readable article text is
truncated to `tools.web.fetch.max_chars` (4000 runes) with a `truncated` flag.
Non-HTML, oversize, timeout and unreadable pages fail with a fixed line; the
source URL and page text are untrusted facts in the Agent prompt. The
extraction uses the maintained Readability.js port
`codeberg.org/readeck/go-readability/v2`. It is Agent-only and never routes on
the Fast path.

### GitHub

The Agent can answer "any PRs waiting on me", read "what are my GitHub
notifications", and open an issue after a spoken Confirmation. All three are
Agent-only Tools: an open-ended GitHub request has no honest semantic Fast path
route, and an issue write is a `confirm` Tool that the Agent loop arms as a
one-turn Confirmation. Nothing is sent until the user says a clear yes; "no", an
unrelated request, or a Barge-in drops the pending call. The create request is
sent exactly once, with no automatic retry, so a timeout cannot duplicate an
issue.

There are two write-only secrets (Dashboard `/secrets` or `.env`). `GITHUB_TOKEN`
is a fine-grained PAT for `issue_create` and `pr_list`: give it access to the
repositories in `tools.github.allowed_repos` and "Issues: Read and write" for
`issue_create`; the review-requested search needs no extra fine-grained
permission. `github.pr_list` asks GitHub for
`is:open is:pr review-requested:@me` — the "waiting on me" semantics, not every
open pull request — and needs no identity request of its own.

Notifications use a second, **optional** secret, `GITHUB_NOTIFICATIONS_TOKEN`:
a classic personal access token with only the `notifications` scope. GitHub's
notifications endpoints do not accept fine-grained PATs, so
`github.notifications` uses that token exclusively, and `issue_create` and
`pr_list` never touch it. The fine-grained `GITHUB_TOKEN` is never sent to the
notifications endpoint, even as a fallback. If the classic secret is unset,
`github.notifications` says aloud that a classic notifications token is needed;
the Health page shows a separate `github notifications` row as not configured
(not an error), and issue and pull-request use stay healthy. The Tool is
read-only and never marks a thread read.

`tools.github.default_repo` (shipped as `Jairik/clanka-bot`) is operator config;
the Tool code hardcodes no target. `tools.github.allowed_repos` is the write
allowlist: an issue for any other repository is refused before a request leaves
the Pi, and an empty list refuses every write. `pr_list` accepts an optional
repository filter and checks it against the same allowlist. Calls use the
documented `X-GitHub-Api-Version: 2026-03-10` header, one keep-alive HTTP
client, bounded response bodies, at most two pages of ten items, and a short
spoken result. One call is bounded end to end by `tools.github.timeout`
(4 s including a cold DNS lookup and the TLS handshake), so a slow Tailscale
MagicDNS lookup is not cut off mid-connect. A failed call logs its HTTP status
and a bounded failure kind (`dns`, `connect`, `tls`, `http`, `rate-limit`,
`config`), never a status of 0 for a response GitHub actually sent; it also
never logs the token, issue title/body, pull-request titles or notification
titles. A refused issue write names the permission the fine-grained token
needs (`Issues: write`). A missing token appears in the module health check and
the Secrets page. GitHub text is labelled to the model as untrusted facts.

### Spotify

Spotify has two separate grants, and the Dashboard's `/spotify` page and Health
panel show them separately:

- **Connect pairing** is the `soloist` container, Spotify's official Soloist
  receiver. It advertises a Connect device over mDNS, plays into the same
  Pulse sink as the Assistant's speech, and keeps its credentials in a private
  host directory (`~/.cache/clanka/soloist/data`). The Assistant mounts only a
  metadata-only status directory.
- **Web API auth** is an OAuth refresh token in the write-only secrets table
  (`SPOTIFY_REFRESH_TOKEN`). It is used only for control calls such as
  play/pause, search, queue and now-playing; the audio itself always comes from
  Soloist on the Pi.

**Pair once.** Put the phone or desktop on the same LAN, open Spotify, play
something, open **Devices**, and choose `Clanka` (or `SPOTIFY_DEVICE_NAME`).
The first connection stores credentials in Soloist's private data directory,
so later restarts reconnect without pairing again. Soloist uses host networking
because Docker's bridge network hides mDNS from the LAN; no port is published.
It handles discovery itself, so the host needs no Avahi or D-Bus mount. The
proprietary binary is downloaded on the deployment host by
`scripts/soloist-setup.sh` (deploy runs it) and mounted read-only; it is never
committed or baked into an image. Re-run `make soloist-update` before the
official build expires; Health warns first and reports an expired build as an
error.

**Register the Web API app.** Create an app in the Spotify Developer Dashboard
(development mode is fine; its owner needs Premium and it allows five users),
then add this exact redirect URI to the app's **Redirect URIs** allowlist:

```
https://<pi-name>.<tailnet>.ts.net/spotify/callback
```

Spotify requires HTTPS except for a loopback IP literal (`127.0.0.1` or
`[::1]`), and does not accept `localhost`. Set the public client ID and the
same URI in `.env` (`SPOTIFY_CLIENT_ID`, `SPOTIFY_REDIRECT_URI`), then deploy
or restart. There is no client secret: the flow is Authorization Code with
PKCE. The requested scopes are only what playback and playlists need:
`user-read-playback-state`, `user-modify-playback-state`,
`user-read-currently-playing`, `playlist-read-private`,
`playlist-read-collaborative`, `user-library-read` and `user-library-modify`.
No email, profile or Web Playback SDK scope is requested.

Open the Dashboard over Tailscale and click **Connect Web API** on `/spotify`.
The assistant starts the flow with a random one-use `state`, an S256 PKCE
verifier and a separate HttpOnly `SameSite=Lax` binding cookie, all bound to the
Dashboard owner and expiring after ten minutes. The callback rejects a wrong,
missing, expired or replayed state and never reflects callback text into the
page. The refresh token is written only through `secrets.Set`; access tokens
stay in memory. A refresh is single-flight, and a rotated refresh token is
persisted before the access token is used. The page and logs contain no token,
code, verifier or state. Spotify development-mode refresh tokens have a
six-month lifetime; when the page says **reconnect needed**, click Connect
again. Refreshing an access token does not extend that lifetime.

**Troubleshooting.** `docker compose logs soloist` shows discovery and login
errors; `docker compose ps` shows the daemon/heartbeat/expiry healthcheck. The
Health page reports a stale or missing heartbeat as unavailable rather than
guessing. A receiver with no credentials still advertises the device, so
"not paired" is a normal first-run state. The private data directory contains
credentials and the API key: treat it like the secrets database and do not
copy it around.

**Playback Tools (#72).** `spotify.play` (resume, or search a track, artist,
album, playlist or "my liked songs"), `spotify.pause`, `spotify.next`,
`spotify.volume` and `spotify.now_playing` drive the Pi's own Connect device
(the one named by `spotify.device_name` / `SPOTIFY_DEVICE_NAME`, `Clanka` by
default) through the Web API client above; a phone or desktop elsewhere on the
account is never controlled. Starting music claims Focus at the lowest
(`music`) priority, so "skip", "pause", "next", "resume" and "stop" reach it
as shared Focus intents when no Ring or reply holds a higher priority. "skip"
and "pause" both use the next intent, so they advance the track; "next" does
the same. Paused music keeps
that claim so a bare "resume" still works; "stop" or a missing device releases
it. Playing, pausing and volume need Spotify Premium and a paired device.

Music ducks locally (ADR-0004): while a reply, a Ring or the turn after the
wake word is active, the Assistant lowers Soloist's Pulse stream
(`pactl set-sink-input-volume`, matched by application name `spotify`) to 25%
and restores it to 100% afterwards. That is separate from Spotify's own volume,
which `spotify.volume` sets over the Web API.

`music.max_volume` (Dashboard Settings, live) caps Spotify's volume. It is
**unset by default (0 = no cap)**; the #70 hardware spike picks a value. When
set it limits `spotify.volume`, any level passed to `spotify.play`, and a
device that is already louder is lowered to it when play starts.

**Playlist and library Tools (#73).** `spotify.play_playlist` ("play my running
playlist on shuffle"), `spotify.shuffle`, `spotify.repeat` (this song, the
playlist or album, or off), `spotify.previous`, `spotify.queue_add` and
`spotify.like` use the same Player, auth client and Soloist device as above;
`play_playlist` also claims music Focus and honours `music.max_volume` like
`spotify.play`. The assistant keeps a local cache of your playlists (`GET
/me/playlists`, at most 20 pages of 50), loaded in the background at startup
and refreshed every 6 hours; startup never waits for it, a failed refresh keeps
the old list and logs only the HTTP status, and shutdown cancels and joins the
refresh. A spoken name resolves through the shared name matcher (same threshold
as sounds and voices; a name that matches nothing, or two playlists equally
well, is "I couldn't find a playlist called ...", never a guess). A playlist
created in the last 6 hours is not visible until the next refresh. "my <name>
playlist" is `play_playlist`; "the playlist <name>" still searches Spotify
publicly through `spotify.play`. Plain "play" does not touch the shuffle state;
only an explicit "on shuffle" turns it on. `spotify.like` saves the account's
current track and says so rather than guess when nothing is playing.
"previous" is a plain Tool and not a shared Focus intent: the intent vocabulary
is fixed by the spec (stop, pause, resume, next, cancel, snooze) and "previous"
means nothing to a Ring or a reply.

Scopes: the #71 request already includes everything #73 needs
(`playlist-read-private`, `playlist-read-collaborative`, `user-library-modify`
for like, `user-modify-playback-state`), so `auth.go` is unchanged. A grant made
with that scope list needs no reconnect; a grant made before it existed must
click **Connect Web API** again.

Status: code and tests only, exercised against an httptest fake Web API, a fake
`pactl` and fake Focus holders. The receiver's live pairing and playback are
accepted on the Pi for #71; live Pulse ducking, voice commands and the #73
playlist cache still need their own acceptance.


Agent path replies are spoken as they are generated: the first chunk is flushed
at a clause boundary or after about twelve words, then each sentence is sent to
TTS while the rest of the answer is still streaming. The configured
`agent.filler` ("One moment." by default) is synthesized once at startup and
cached, so it plays the moment a request is routed to the Agent path; an empty
Filler is a startup error. The same cached phrase covers a slow Fast path Tool:
if nothing has been said 1.5 seconds after you stop talking, it plays once. A
request never plays it twice, and it never talks over an answer.
All three published ports are bound to the host loopback only. The debug and
Dashboard listeners bind all interfaces inside the container (see
`docker-compose.yml`), so the host port mapping and their source-address checks
are what keep them local. The Dashboard is disabled until
`ASSISTANT_DASHBOARD_OWNER` is set and is meant to be reached through
`tailscale serve`; see `docs/deploy.md`.

### Voice

Say the wake word (**"clanker"**, the default), ask a question, and hear the answer:

1. `assistant` captures the named Pulse source (`ASSISTANT_PULSE_SOURCE`, empty
   uses the host default) with `parec` and streams 16 kHz mono PCM to
   `wakeword`.
2. A detection plays a short generated chime and opens a transcription with
   `stt` (Moonshine Voice `small-streaming`, or `tiny-streaming` via
   `STT_MODEL`). The chime does not play again when the request is accepted.
3. End of speech is a fixed **500 ms** silence window (`stt.endpoint.silence`
   in `assistant/config.yaml`), not adaptive. A `max_utterance` cap stops a
   false wake from listening forever.
4. The transcript goes through the normal pipeline; the reply is spoken.
   Wake word detection stays live while the Assistant speaks: say the wake
   word again to stop the answer, drop a waiting confirmation question, and
   start a new request with the chime (Barge-in). The logged `stop_ms` is the
   measure of how quickly playback ended.

The webcam mic has no hardware echo cancellation, so the Assistant can in
principle hear itself. The dev-box run, with its own reply looped back at full
level, produced no false barge-in; if that changes in a louder room,
`WAKEWORD_THRESHOLD` is the knob.

`WAKEWORD_MODELS` is an explicit, comma-separated wake-word allowlist; every
model in it is requested from `wakeword`, and a detection from any other name is
ignored before it can start a turn. The default is the one trained word, because
each extra model adds its own false wakes. To listen for two, set:

```sh
WAKEWORD_MODELS=clanker,okay_nabu
WAKEWORD_THRESHOLDS=clanker:0.6,okay_nabu:0.5
```

`WAKEWORD_THRESHOLD` is the default probability for every model without an
entry in `WAKEWORD_THRESHOLDS` (or `wakeword.thresholds` in
`assistant/config.yaml`). Higher is stricter. `wakeword`'s own `--threshold` is
only a fallback for clients that do not send thresholds: the Assistant sends
the complete per-model map on every detect stream, and the wrapper image patches
the pinned upstream handler to apply it per model. The old single-value
`WAKEWORD_MODEL` (and `wakeword.model`) is still accepted.

**Wake-free volume commands (#81)** are off by default. With
`WAKEWORD_COMMANDS_ENABLED=true`, the Assistant also asks `wakeword` for the
`volume_up` and `volume_down` models (baked from `wakeword/models/*.tflite`)
and handles a detection directly: one volume step (`WAKEWORD_COMMAND_STEP`,
default 10%) on the same Pulse sink as `audio.volume_set`, then a short Cue, with
no STT and no routing. They act only while music or the current reply holds
Focus, at a stricter per-model threshold (default 0.7, overridable through
`WAKEWORD_THRESHOLDS=volume_up:0.75,volume_down:0.75`), and with a cooldown
(`WAKEWORD_COMMAND_COOLDOWN`, default 1.5 s). The target is the shared Pulse
sink that `audio.volume_set` and the Dashboard slider drive, so it carries
replies and Cues as well as music; Quiet hours and Ring volumes stay software
gains and are not rewritten. While music holds Focus, an up step stops at
`music.max_volume`; a down step is never blocked. The feature stays off
until models trained on this house's samples exist and the false-trigger
measurement in `docs/spikes/wake-over-music.md` meets its target.

`wakeword` and `stt` do computation only and never mount Pulse (ADR-0001).
Their Compose health checks are Wyoming `describe` calls, so startup waits for
the service, not for a model: openWakeWord loads a detector on the first
`Detect` for it. `wakeword` bakes the locally trained `clanker` detector next to
the upstream built-ins, so `WAKEWORD_MODELS` can name `okay_nabu` (or another
upstream name) too; see `wakeword/training/README.md`
for how it is trained and how to add the owner's own voice. The Dashboard's
Settings page lists the configured models and thresholds, and its Health page
checks that the service advertises each one. Moonshine weights are downloaded anonymously from
`download.moonshine.ai` at build time and baked in; the container starts
offline. Rebuild `stt` after changing `STT_MODELS` or `STT_MODEL`. Set
`ASSISTANT_PULSE_SOURCE` from `pactl list short sources` (the dev box uses the
USB webcam's mono source).

The mic endpoint threshold (`stt.endpoint.threshold_dbfs`, default -38) depends
on source gain. The C270 was calibrated to 32% gain on 2026-11-12: idle RMS
-47.4 dBFS, zero sampled idle frames above threshold, and a human-spoken turn
ended after 501 ms of silence. See [the calibration record](docs/mic-calibration-2026-11-12.md)
and [the measurement guide](docs/mic-calibration.md) before changing it.

The date, timer and audio Tools run on the Fast path too:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what day is it"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"set a timer for five minutes"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"how long is left on my timer"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"turn the volume to 40"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"say that again"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what can you do"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"are you ok"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"mute"}'
```

The utility Tools answer common questions without the Agent path:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what time is it in Tokyo"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"how many days until Christmas"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what is 15% of 80"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"convert 5 miles to km"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"pick between pizza and tacos"}'
```

`time.in_city` uses a shipped city-to-IANA table and embedded tzdata, never a
network lookup; `date.countdown` counts calendar days in the effective
`timezone`; `calc.evaluate` uses a bounded arithmetic grammar rather than code
execution; `units.convert` has a fixed unit table with no currency;
`random.flip`/`roll`/`pick` use `crypto/rand` and preserve the words the user
gave. Coverage, grammar and limits are in
[docs/utility-tools.md](docs/utility-tools.md).

`assistant.repeat` says the last completed reply again from an in-memory store.
An error or Unavailable message counts because the user heard it; a blank or
cancelled turn does not erase the previous one. The Tool is Local-only because
the previous reply may itself have come from a Local-only Tool, so the repeat
never becomes model-visible Tool output. "What can you do" is `assistant.help`,
generated from the enabled registry on every call: a disabled Tool and its
description disappear from the next answer, and no feature list lives in code.
`system.status` reads the same bounded service/module snapshot as the Dashboard
Health page, so a timed-out or unreachable service is reported honestly rather
than as healthy.

Timers are stored in the database and survive a restart. Give one a label
("set a pasta timer for 10 minutes") and ask for it later ("how long is left on
the pasta timer"); `timer.list` is Local-only because labels can be private.
On startup, a Timer up to two minutes overdue goes off immediately, while older
ones are recorded missed and summarised in one Announcement. A Timer that fires
**Rings**: it loops the assigned `timer_ring` Cue with a gap, speaks its label
according to `ring.timer.speak_label`, and keeps ringing until `stop`/`snooze`
or `ring.timer.max_ring` (default 5 min). `snooze` on a Timer acts as stop.
Reaching max_ring records the alert as missed in `alert_events` and sends one
Notification if `notify.missed_alerts` is on (see **Notifications** below).
Startup recovery reports an open row left by a crash and an old overdue Timer
the same way, before the scheduler starts. A missed Notification is dropped,
not delayed, during Quiet hours.
Inside Quiet hours a Timer Ring and its label use `quiet.volume`; an Alarm is
exempt and keeps its own Ring volume.

A **Reminder** is the same Scheduled-alert machinery with a message instead of a
Ring. Set it by duration ("remind me in 20 minutes to check the oven") or by
clock time in the effective `timezone` ("remind me at 5 pm to call mum"); a
clock time already passed means the next day, so the two forms share one
future-if-past policy with Alarms. `reminder.set` and `reminder.cancel` are
available to the Agent path, while `reminder.list` is Local-only because the
messages can be personal. Reminders are stored in the database and follow the
same downtime rule as Timers: up to two minutes overdue speaks at startup,
older ones become one summary Announcement, and a row is deleted in the same
transaction that opens its alert-history row so a restart never repeats a
message.

When a Reminder is due, the `sounds.reminder` Cue plays immediately before the
message as **one** queued Announcement item. It waits for the current reply and
a pending Confirmation, but inside Quiet hours it still sounds at
`quiet.volume` instead of joining the held morning summary; ordinary
Announcements keep waiting. The selected voice and `voice.rate` apply because
the message uses the shared speaker; there is no model call on the way out.

A Ring waits for the current reply sentence to finish (not the whole reply),
then takes the one speaker; the reply's next sentence waits for the Ring and
resumes afterwards, so ringing never cancels an answer. Music is ducked
through the `ring.Music` seam (the shared `internal/music` Ducker, #72), and the
restore runs on stop, timeout and shutdown. Real Pulse ducking is unverified on
hardware.

Ring settings are live and per kind:

| Setting | Timer default | Alarm default |
| --- | --- | --- |
| `ring.<kind>.max_ring` | 5m | 5m |
| `ring.<kind>.volume` | 80% | 80% |
| `ring.<kind>.ramp` | 0s | 30s |
| `ring.<kind>.gap` | 1s | 1s |
| `ring.<kind>.speak_label` | `start` | `start` |
| `ring.<kind>.snooze` | — | 9m |

`volume` is absolute: it scales the Cue PCM and is independent of the media
volume. The sound itself is the per-kind Cue role from the Sound library,
`sounds.timer_ring` or `sounds.alarm_ring`; there is no second sound key. The
same settings service the Dashboard writes through is read again at each Ring,
so a change applies to the next fire. Timers and Alarms share the engine and
the per-kind settings.

Alarms are clock-time Scheduled alerts and survive a restart. "Wake me at 7"
sets a one-off (today, or tomorrow when 7 has already passed); "set an alarm
for 6:30 on weekdays" repeats Monday to Friday, and weekends or chosen weekdays
("on monday and thursday") work too. `alarm.list` is Local-only because labels
can be private; "cancel my 7 am alarm" removes the matching Alarm. Each
schedule is interpreted in the live `timezone` setting. A spring-forward wall
time that does not exist fires after the jump (02:30 becomes 03:30), and a
fall-back wall time that happens twice fires at its first occurrence. Alarms
use the same Ring engine as a Timer, with a 30 s ramp by default. "snooze"
re-rings after `ring.alarm.snooze` (default 9 min) without disturbing the
repeat schedule; "stop" ends the current Ring, and a repeating Alarm schedules
its next occurrence. On startup, an Alarm up to two minutes overdue fires now;
older ones are recorded missed and summarised in one Announcement, and a
repeating Alarm then schedules its next occurrence. A max_ring or recovered
missed Alarm also sends one Notification when `notify.missed_alerts` is on.

Each Alarm can override its Ring `sound`, `volume` and `ramp` for itself:
"wake me at 7 with the birds sound" or "change my 7 am alarm to the birds
sound". The sound name goes through the same shared name matcher as the Sound
library; a name that does not match stores the kind default and the reply says
so instead of guessing another sound. Volume and ramp are optional per-Alarm
values, so an explicit 0% volume or 0 s ramp stays different from the kind
default. `alarm.update` changes only those overrides; it does not move the
time, label or repeat. "clear my alarm sound" or "reset my alarm volume"
returns that field to the kind default. The overrides live in the Alarm row, so
they survive a restart and a snooze, and the Ring engine applies them to that
one Ring without changing `ring.alarm.*` or another Alarm. The Scheduled
alerts page (#58) uses the same `alarm.Manager.Update` write, so voice and the
page cannot disagree.

The Control phrases `stop`, `pause`, `cancel`, `snooze`, `louder`, `quieter`
and `mute` are matched exactly, so they answer without waiting on the embed
service. `stop`, `pause`, `cancel` and `snooze` are shared Focus intents: they
act on a ringing alert first, then the current reply, then playing music. With
nothing in Focus they fall through to the semantic Fast path, so `cancel` still
reaches `timer.cancel` and `louder` still steps the volume up. A volume phrase
runs the Tool's extraction even though it matched exactly, so `quieter` steps
down and `mute` sets the sink volume to zero; `unmute` steps up because there is
no stored pre-mute level. These drive the actual Pulse sink through `pactl`.
`mute` is an explicit master mute of the host's default sink, not a per-stream
music volume, so it silences every stream on that sink: the next spoken reply
and any Ring are inaudible until the volume is raised again. Nothing restores
the previous level automatically, because the mute was a direct user command.
The volume change does not rewrite `quiet.volume` or any `ring.<kind>.volume`:
those configured levels stay as they were, and a Ring still applies its own
volume once the sink is audible. Music ducking and its restore belong to the
Ring's `ring.Music` seam (#72), not to this volume control.

Speech the Assistant starts without being asked (a Reminder, a finished Hermes
job) goes through one Announcement queue. It waits for the current reply to
finish and for any pending Confirmation to resolve, then speaks one line at a
time. Queue one from the debug entry; posting it while a reply or a Confirmation
question is in progress is what makes it wait:

```sh
curl -s -X POST localhost:8081/debug/announce \
  -H 'Content-Type: application/json' -d '{"text":"Dinner is ready."}'
```

Quiet hours holds and releases the same queue; ordinary text items wait, while
a due Reminder is an urgent item that keeps its own slot. It still waits for a
reply or a pending Confirmation, and its Cue plus message remain one item.

#### Quiet hours

Quiet hours is one daily window in the effective `timezone` (an end earlier
than the start crosses midnight), a timed do-not-disturb period, and an
`I'm awake` bypass. All three write the same live settings the Dashboard edits:

| Setting | Default | Meaning |
| --- | --- | --- |
| `quiet.enabled` | `false` | Turn the daily window on or off |
| `quiet.start` | `22:30` | Local window start, `HH:MM` |
| `quiet.end` | `07:00` | Local window end, `HH:MM` |
| `quiet.volume` | `35` | Absolute reply/Timer/Reminder volume percent |
| `quiet.dnd_until` | empty | Timed do-not-disturb end, RFC3339 |
| `quiet.awake_until` | empty | End of a bypassed window, RFC3339 |

Inside it, text Announcements stay queued and are released as **one** summary
line when the window ends; a model is not called to write it. Dashboard audio
previews keep their own queue slots and are played, never narrated as text. The
summary and previews still wait for the current reply, a pending Confirmation
and a ringing alert, and Barge-in cancels them like any other unprompted sound.
Notifications are muted through `quiet.Service.AllowDelivery` before any
network request; a suppressed missed Notification is dropped, not retried when
the window ends. Replies, Fillers, the Unavailable cue, Timer Rings and
Reminders use `quiet.volume` as an **absolute** software gain; the host media
volume is never changed, so there is nothing to restore. Alarms Ring at their
normal volume, including a Ring recovered during startup.

Say any of these; each is an `act` Local-only Tool that confirms what changed:

```text
set quiet hours from 10:30 to 7
turn off quiet hours
do not disturb for two hours
I'm awake
```

`quiet.set` treats the reversed "from 10:30 to 7" as 10:30 pm to 7 am. `I'm
awake` clears do-not-disturb and skips only the rest of the current window; the
daily schedule stays on and holds again the next night. Outside a window it
just clears do-not-disturb. `turn off quiet hours` disables the schedule and
clears both overrides. The Dashboard Settings page groups the same six values
under **Quiet hours**; a saved change applies within a second without a
request, because one small controller reevaluates at the next boundary or every
second and never replaces the wording manager's settings observer.

#### Notifications

`notify.phone` sends a Notification to your phone through
[ntfy](https://ntfy.sh). "Send that to my phone" sends the Assistant's previous
reply; an explicit message ("send buy milk to my phone") is sent instead. The
reply is resolved in memory with its provenance, and a reply that came from a
**Local-only** Tool is refused, never forwarded off the Pi. The Tool is `act`,
not `confirm`: the recipient is your own phone rather than a third party, and
the delivered message is the reply you just heard or the text you dictated.
The Tool itself is not Local-only because the Notification leaves the Pi by
design; its result is only "Sent to your phone."

| Setting | Default | Meaning |
| --- | --- | --- |
| `notify.url` | `https://ntfy.sh` | ntfy server base URL, live overridable |
| `notify.missed_alerts` | `true` | Push a Notification for a missed Timer or Alarm |
| `notify.reminders` | `true` | Push a Notification when a Reminder is due |

The **topic is generated once** on first use and kept in the write-only
secrets table as `NTFY_TOPIC`; it never appears in `config.yaml`, the settings
API, a log line, or a prefilled form field. It is stable across restarts and
concurrent first sends. `NTFY_TOKEN` is optional and is **never generated**:
official ntfy access tokens are issued by the server. On the public ntfy.sh,
leaving the token empty publishes to the random topic without an
`Authorization` header and works out of the box; the random topic is an
unguessable name, **not access control**, because anyone who learns it can
subscribe.

To connect the phone, install the ntfy app and subscribe to the topic. The
safest bootstrap is to choose the topic yourself before first use:

```sh
# A hard-to-guess topic the phone app and the assistant both know.
python3 -c 'import secrets; print("clanka-" + secrets.token_hex(16))'
# Put that value in .env as NTFY_TOPIC, then restart.
```

If you leave `NTFY_TOPIC` empty, the assistant generates one; the
write-only API does not reveal it, so set it explicitly when you need to
configure a phone. The unencrypted database is owned by the service account, so
the owner can retrieve a generated value with `sqlite3 data/assistant.db` if
needed.

For real access control, self-host ntfy with `auth-file` and
`auth-default-access: deny-all`, then issue a token on that server
(`ntfy token add`) and store it:

```sh
curl -s -X PUT localhost:8081/api/secrets/NTFY_TOKEN \
  -H 'Content-Type: application/json' -d '{"value":"tk_..."}'
```

Set `notify.url` to the self-hosted base URL with
`PUT /api/settings/notify.url` (or the config layer). `notify.url` is the
server root, not a topic URL: JSON publishing posts to that root and carries
the topic in the body. A locally invented token is not accepted by ntfy and is
never fabricated here. Primary sources:
[ntfy publishing](https://docs.ntfy.sh/publish/) (topic as password, JSON
publish at the root URL, Bearer tokens) and
[access control](https://docs.ntfy.sh/config/#access-control) (server-issued
tokens via `ntfy token add` / `auth-tokens`).

A missed Timer or Alarm sends one high-priority Notification with its label and
reason (`max_ring`, `restart` or `downtime`). Quiet hours suppress it before any
network request and do not schedule a retry. A due Reminder also sends one
high-priority Notification with the message, alongside the spoken reminder;
`notify.reminders` turns that off, and Quiet hours mute it the same way.
`notify.phone` answers "Notifications are muted during quiet hours." instead.
Requests are bounded by a 10 s HTTP timeout, redirects are refused so the bearer
token stays on the configured host, and response bodies are never logged.

The Dashboard is the desktop surface for a due Reminder while it is open: the
Alerts page schedules one browser Notification per active Reminder after the
owner grants the browser permission. It does not add a second preference. The
server puts the same decision in the view (`DesktopNotify`: `notify.reminders`
on and Quiet hours allowing), and a cancelled Reminder clears its timer on the
next view update. The notification text is never stored or sent anywhere else.
A closed tab cannot receive one; Web Push is the transport to add if that
becomes a requirement.

#### Voice library and rate

Voice is a persistent setting, not a per-request choice. The library is the
built-in Pocket TTS catalogue the `tts` image carries (`TTS_CATALOGUE`, default
`all`: 27 voices, each with a precomputed embedding baked in at build time) plus
pre-exported `.safetensors` voice states copied into `voices/` on the separate
`assistant-voices` volume. Compose mounts that volume read-only into `tts` at
`/voices`, so `tts` sees only voice states and never the database or secrets;
files are never baked into an image or committed. `voice.dir` may name another
volume path, and `voice.tts_dir` is the same folder's path inside `tts`.

Only `.safetensors` is accepted: `.pt`, `.pth` and other pickle-backed torch
files are refused and never opened. A file is at most 32 MB and is checked in
two stages, whether it arrives through the Dashboard or a manual copy:

1. A pure-Go header check: one `self_attn/cache` `F32 [2,1,N,H,64]` tensor per
   FlowLM layer with `1 <= N <= 400` and `1 <= H <= 32`, one matching `I64 [1]`
   `offset` per layer, and an optional `pad` per layer (2 or 3 entries per
   layer), with N and H consistent across layers and contiguous in-file data
   offsets. The layer and head counts depend on the model (100M: 6x16,
   18M and 7M: 8x8, 2M: 8x3). This is a header parse only; a pickle payload
   never runs.
2. A real TTS model smoke test (`voice-validate`) that loads the state and
   generates audio. The model revision it passed against is recorded.

The Dashboard's **Voices** page at `/voices` uploads a state without shell
access. The upload is copied to a hidden staging file on the voices volume, the
header is checked before any model call, and TTS validates that staging file
with `cache=false` so the temporary path is never kept in the model-process
cache. Only then is the file renamed into place; the final path's cache entry is
evicted first, the library rescans, and the recorded revision makes the voice
selectable. A failure—bad extension, oversize body, malformed header, model
rejection or a rescan error—leaves no file and no index row, and restores the
previous export when replacing. Replacement needs the explicit **Replace** tick
and is atomic; an existing file is never overwritten silently.

The page lists every built-in and custom entry with its status (`Built-in`,
`Ready`, `Stale`, `Unvalidated`), validation revision and dates. A stale entry
says to re-export it on the desktop and disables selection and preview until it
passes again. Each custom entry also has an optional source/consent note: it is
stored and shown escaped, but recorded only, never enforced. The browser
preview is an authenticated GET that synthesizes one fixed short phrase at the
current rate and returns WAV; it never opens the Pi speaker and never changes
`voice.name`. Selection and the rate form write the same live settings the
voice Tools use, so the cached Agent Filler is re-synthesized in the background.

`voice.set`, `voice.list`, `voice.current` and `voice.rate` are Local-only
Tools; `voice.set` resolves the spoken name through the shared name matcher.
`voice.name` and `voice.rate` are ordinary live settings, so the Dashboard and
voice write the same stored override:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"use the jarvis voice"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what voices do you have"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"speak faster"}'
curl -s -X PUT localhost:8081/api/settings/voice.rate \
  -H 'Content-Type: application/json' -d '{"value":1.2}'
```

The active voice warm-loads at startup; every other state loads on first use
and stays cached in the `tts` process. If `tts` starts on a different model
revision, unchanged custom voices are marked stale. If the active voice is one
of them, startup falls back to the default built-in and queues exactly one
Announcement; the stale entry stays listed so it can be re-exported on the
desktop.

`voice.rate` is a multiplier (`0.5`–`2.0`, default `1.0`). Pocket TTS 4.0.1
has no native speed control, so the `tts` wrapper resamples its output: faster
speech also raises pitch, and slower speech lowers it. There is no
time-stretch; the bounded range keeps the artefact small. Selecting a different
voice or rate re-synthesizes the cached Agent Filler in the background, so the
spoken cue never keeps the old voice.

#### Personas

A **Persona** bundles a ready voice, a personality prompt and the Agent Filler
and Unavailable wording under one spoken name. The **Personas** page at
`/personas` creates and edits the bundle and selects it; `persona.switch`,
`persona.list` and `persona.current` are Local-only voice Tools, and
`persona.switch` resolves the spoken name through the shared name matcher. For
example, after creating a Persona named GLaDOS:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"switch to the GLaDOS persona"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"which persona are you using"}'
```

Selecting writes `voice.name`, `agent.filler`, `agent.unavailable_message` and
`persona.active` in one settings transaction. Every field and the voice state
are validated before that write, so a stale, unvalidated or missing voice
leaves the previous voice and wording untouched rather than applying half a
Persona. The reply is synthesized after the write, so the spoken confirmation
already uses the new voice.

The personality prompt never becomes a second request path. The single prompt
builder reads it once per turn and adds it to the one labelled system message
(ADR-0002); history and Tool results are unchanged. A switch during a model
turn cannot rewrite the system part that turn already received. The active
Persona's voice and the active voice are both warm-loaded at startup through
the one TTS client (deduplicated when they are the same state); a Persona voice
that has gone stale is logged and skipped, while #63's single fallback
Announcement and safe default handle the speaking voice.

Editing an active Persona updates the stored record. Its personality prompt
applies on the next turn; the stored voice and wording apply when it is
selected again. If `voice.set`, the Settings page or a manual wording edit
changes one of those values later, `persona.current` and the Personas page say
what differs instead of claiming the exact bundle. Clearing the active Persona
removes only the `persona.active` marker; the applied voice and wording stay as
they are.

#### Phrase book

Every Fast path Tool still renders its own full sentence. The **Phrase book**
page at `/phrasebook` replaces the acknowledgements with the shortest useful
line: `timer.set` → "Set.", `spotify.play` → "Playing.", and so on. A Persona
can override any of them; **Generate from persona** asks the one Agent provider
for a complete in-character set (1–4 words per Tool) and the page lets you
review, edit or reset every row before the next request. Content-bearing replies
(the time, the weather, a list, what is playing) are deliberately not
overridable. The shipped defaults apply when no Persona is active, and the
registry hook is installed once at startup, so no restart is needed after an
edit. See [docs/phrasebook.md](docs/phrasebook.md).

`personas/starter.json` holds the starter set (Scuzz, Sneer, Bark, Biff, Blank);
load it with `python3 scripts/seed-personas.py --base http://127.0.0.1:8082`.

#### Routes and Hermes

The **Routes** page at `/routes` is the deterministic-surface audit: every Tool,
whether the Fast path may choose it, whether only the Agent may call it, whether
it stays on the Pi, whether it asks for a Confirmation, and how many live
Control phrases and Examples it has. The **Hermes** page at `/hermes` shows the
surface an MCP adapter would publish (read/act, not Local-only, not confirm)
and links the accounts Hermes may use. Hermes itself is not installed in this
build, and the page says so; a grant is stored intent until the adapter exists.
See [docs/hermes.md](docs/hermes.md).

Routing precision is measured against a labelled corpus with the real MiniLM
model; the floor is 0.95 for every Tool, and ambiguous phrasing goes to the
Agent path. See [docs/routing-eval.md](docs/routing-eval.md) to run the
evaluation, tune threshold and margin, or promote a request-log misroute into
the fixture. The model comparison (MiniLM vs BGE, Arctic and the MiniLM
variants) and the measured trade-offs are in
[docs/embeddings.md](docs/embeddings.md).

### Settings and Tool overrides

All persistent state lives in one SQLite database, opened only by the
`assistant` process ([ADR-0003](docs/adr/0003-one-sqlite-store-owned-by-assistant.md)).
Compose keeps it on the `assistant-data` volume, so restarts and rebuilds keep
the data; the file is created mode 0600 and migrations run at startup.

Every setting resolves as **stored override → `config.yaml` → code default**;
`DELETE` removes the override and the lower layer applies again. Overridable
settings include each Tool's `enabled` flag and Permission level, the wording
below and the connection-level values. A stored Permission may only tighten
(`read` → `act` → `confirm`), never loosen, and Local-only is code-declared and
cannot be overridden.

The `timezone` setting is a live IANA name for clock-time Alarms and Reminders.
An empty value falls back to the container's `TZ`, then UTC, and UTC logs a
startup warning. The same settings service the Dashboard writes through is the
one the alert scheduler reads, so a change applies without a restart. No
timezone is ever guessed from an IP address.

The JSON API is mounted under `/api/` on the localhost debug listener, never on
the public health listener. The authenticated Dashboard (#47) mounts the
same handler behind Tailscale identity. Settings, Tool overrides and Secrets
share that handler; writes there also need a same-origin fetch.

The Dashboard is a React app (`assistant/web`, HeroUI) built into the Go binary,
so the Pi serves it with no CDN. Its home page at `/` is a chat: overview cards
over a prompt box that takes typed requests or, over HTTPS, the browser
microphone. The top bar has the Pi speaker's volume (click to mute, a slider in
5% steps). Each page reads one JSON view under `/api/ui/` and writes through the
same form routes as before. Build it with `make web`; the Docker image does this
itself. See `assistant/web/README.md` for the dev server.

The **Settings** page at
`/settings`. Every Tool is listed with its effective and code-declared
Permission, Local-only flag and enabled state; Permission choices are only the
valid tightening levels. Every wording value is an ordinary text or one-entry-
per-line form with its effective value, source layer and a Reset button.
Settings the service exposes but this page does not know by name (for example a
future sound setting) appear automatically, and connection settings carry a
Restart required badge.

Agent wording has a live preview. Each value shows the phrase as it will be
used, following the unsaved draft rather than the saved value, and the Filler
and Unavailable message add a Play button that synthesizes that draft in the
current voice. Play is an authenticated GET that returns WAV bytes: it never
plays through the Pi speaker and never writes `voice.name`. A draft longer than
240 characters, or blank, is refused before the speech service is called, so a
rejected draft is a `400` rather than a failed synthesis. Request prefixes
preview what they match instead of how they sound, because a prefix is matched
in what you say, never spoken by the assistant.

The **Sounds** page at `/sounds` is the upload/library/role/preview surface
described under Sound library and Cue roles below. Its Ring section renders the
per-kind Timer and Alarm definitions from the settings service, so a new key
appears there without a page change.

The **Scheduled alerts** page at `/alerts` lists every active Timer, Alarm and
Reminder with its next fire time, label or message and kind, in the live
timezone. Cancel stops one alert through its own manager id, so a ringing
Alarm stops only that Ring. Each Alarm has an editor for its sound, volume and
ramp overrides: a blank field keeps the current value, `0` is an explicit mute
or no-ramp, and Default (or the reset button) clears the field back to
`sounds.alarm_ring` and `ring.alarm.*` as NULL, never 0. The page calls the
same `alarm.Manager.Update` as `alarm.update`, so voice and Dashboard cannot
disagree. It also shows the newest missed Alerts and Timers from the shared
local history (no pending row is required), bounded to the recent window.
Every write needs the Dashboard identity plus a same-origin request.

The **Notes** page at `/notes` owns the two fixed Lists (`shopping` and
`to-do`) and the personal Notes. Add, edit, tick and remove list items; add,
edit and delete notes. The rows come from the same managers the voice Tools use,
so the Dashboard and a request cannot disagree. The page needs the Dashboard
identity and a same-origin POST for every write.

`list.add`, `list.read` and `list.remove` are the voice surface; `list.remove`
resolves a spoken item through the shared Name matcher and refuses an ambiguous
match rather than removing the wrong row. `note.remember` and `note.recall`
keep and find short facts; recall ranks every note by meaning with the local
MiniLM service, or says truthfully that there is no note. `note.forget` is
`confirm` and Agent-only, so deletion needs a spoken yes. A note's vector is
captured when it is written or edited, so a recall embeds only the query.
Unlike most personal data, Notes and Lists are deliberately **not Local-only**:
the owner wants the Agent path and later Hermes to read and edit them, and their
text leaves the Pi only through the one prompt builder
([ADR-0005](docs/adr/0005-notes-and-lists-are-agent-visible.md)).

The **Secrets** page at `/secrets` lists every secret a Tool module or the
Agent provider declares, with whether it is set, its last four characters
(hidden for short values), the update time and the active source (`.env` or
stored). Each row offers Replace and, for a stored value, Clear. The forms are
write-only: no value is prefilled, echoed after a rejected submission or
returned anywhere. Clearing a stored value exposes the `.env` bootstrap
immediately and the page says which source is active. A new module declaration
appears without a page change.

The **Activity** page at `/activity` lists the recent requests, grouped by hour,
with search and Voice/Typed/Problems filters; the home chat shows the same
history as a conversation. Its bounded in-memory ring is fed by the same completion that
writes the one structured `request` log line: the route the pipeline chose
(Control phrase/Fast path, Agent path, Confirmation, Focus intent or an
unrouted fall-through), the stage timings that were actually measured, the Tool
calls and the outcome. Response latency is shown only when reply audio actually
started, so a typed request reports its routing and Tool stages without an
invented number. Nothing is persisted, and the page reads the ring rather than
tail-ing container logs.

The prompt box replaces the localhost-only debug entry for Dashboard users but
uses the same pipeline, pending Confirmation, Permission checks, Tool policy,
Local-only rules and shared history. It is deliberately **text-only**: the
reply appears in the page and is not spoken on the Pi speaker. The mic button
records in the browser, resamples to 16 kHz PCM and uploads the utterance to
`POST /api/ui/ask/audio`, which transcribes it with the same STT service and
then answers it like a typed request (source "Dashboard mic"); the audio is not
stored. Browsers only allow the mic over HTTPS, which `tailscale serve`
provides. A waiting
Confirmation is settled by typing `yes` or `no`, and anything else cancels it
and routes the new request normally. One typed request runs at a time so two
turns cannot interleave. Local-only Tool results are visible on this
authenticated page, with a Local-only badge, and are never sent to a model,
Agent prompt, Notification or any other egress path; the public health listener
does not know about the ring at all.

```sh
# List every effective value and the layer it came from.
curl -s localhost:8081/api/settings

# Disable a Tool, raise its Permission, or both at once.
curl -s -X PUT localhost:8081/api/tools/weather.current \
  -H 'Content-Type: application/json' -d '{"enabled":false}'
curl -s -X PUT localhost:8081/api/tools/weather.current \
  -H 'Content-Type: application/json' -d '{"permission":"confirm"}'

# Reset both: config.yaml, then the code declaration, applies again.
curl -s -X DELETE localhost:8081/api/tools/weather.current
```

#### Live wording

The user-facing wording is overridable through the same settings API. Every
value reports the layer that won and a `kind`: `live` values are read per turn;
`restart_required` values are only read at startup and need a process restart.
There is no general hot-reload framework.

| Key | Live effect |
| --- | --- |
| `tools.<name>.control_phrases` | The exact phrases that route to that Tool. A replacement applies to the next request. |
| `focus.<intent>.phrases` | Exact phrases for one shared Focus intent (`stop`, `pause`, `resume`, `next`, `cancel`, `snooze`). They act on a ringing alert, the current reply or music; with nothing in Focus they fall through to the semantic Fast path. |
| `tools.<name>.examples` | Semantic Fast path Examples. A change re-embeds in the background; routing keeps the old vectors until the new ones are ready, a superseded rebuild is discarded, and a failed rebuild retries with bounded backoff (a startup failure recovers without a restart). |
| `agent.request_prefixes` | Prefixes that go straight to the Agent path. A change applies to the next request. |
| `agent.filler` | The phrase played while the Agent path waits. A change re-synthesizes the cached audio in the background; the old audio plays until the new clip is ready, and at most one Filler still plays per request. |
| `agent.unavailable_message` | The phrase spoken when the model is unreachable. A change applies to the next failure. |
| `agent.model` | The model name sent to the configured provider. A change applies to the next turn (the model is resolved per request); there is no automatic failover. |
| `http.addr`, `debug.addr`, `dashboard.addr`, `tts.addr`, `wakeword.addr`, `stt.addr`, `embed.url`, `capture.source`, `playback.sink` | Connection-level settings, reported as `restart_required`. A stored override is applied on the next process start. |

```sh
# Route two new phrases to the clock Tool, then reset to config.yaml.
curl -s -X PUT localhost:8081/api/settings/tools.time.now.control_phrases \
  -H 'Content-Type: application/json' \
  -d '{"value":["what time is it","current time"]}'
curl -s -X DELETE localhost:8081/api/settings/tools.time.now.control_phrases

# Move "stop" to a different shared intent phrase, then reset it.
curl -s -X PUT localhost:8081/api/settings/focus.stop.phrases \
  -H 'Content-Type: application/json' -d '{"value":["stop","halt"]}'
curl -s -X DELETE localhost:8081/api/settings/focus.stop.phrases

# Change the Agent cue and the model-unreachable line.
curl -s -X PUT localhost:8081/api/settings/agent.filler \
  -H 'Content-Type: application/json' -d '{"value":"Just a moment."}'
curl -s -X PUT localhost:8081/api/settings/agent.unavailable_message \
  -H 'Content-Type: application/json' -d '{"value":"I can't reach my brain right now."}'
```

A blank phrase, a duplicate within one list, an unknown Tool, an unknown
intent, or a Control phrase pointing at a confirm Tool is rejected at write
time. If two owners ever claim the same normalized phrase (two Tools, two
intents, or a Tool and an intent; possible only through separate overrides),
the phrase is dropped from the Control phrase table so it falls through to the
semantic or Agent path instead of choosing an owner at random.

#### Name matching

Sounds, voices and Spotify playlists resolve spoken names through one shared
matcher (`internal/namematch`). Exact and normalised names win first and make
no embed request. Normalisation lowercases, drops punctuation and a leading
`the`/`a`/`an`/`my` and a trailing `sound`/`voice`/`playlist`, so
`The Lo-Fi Beats Playlist` is the same name as `lo fi beats`. Anything else is
compared with MiniLM embedding similarity against every candidate; the best
name at or above the threshold wins, and everything else is no match. A name is
never forced: empty input, an embed failure, malformed vectors, a score below
the threshold, two names that normalise to the same words and an exact score
tie all return no match. A caller then keeps its existing value and, where the
Tool promises it, says aloud that the name did not match.

`name_match.threshold` is the live similarity floor, default `0.60`, with the
same override → `config.yaml` → code default layers as every other setting. It
is read per lookup, so a Dashboard change applies without a restart; it
deliberately installs no second settings observer. At `0.60` the real MiniLM
model scores "the bird sound" → `birds` at about 0.85 and unrelated requests
below 0.45.

```sh
# Tighten the name matcher, then return to config.yaml.
curl -s -X PUT localhost:8081/api/settings/name_match.threshold \
  -H 'Content-Type: application/json' -d '{"value":0.65}'
curl -s -X DELETE localhost:8081/api/settings/name_match.threshold
```

### Secrets

Secrets are write-only. A stored value wins over the `.env` bootstrap with the
same name, and only metadata comes back: whether it is set, the last four
characters (only for values of at least 12 characters), the update date and the
active source (`store` or `env`). Values never appear in the settings API,
logs or error responses. The database file is mode 0600, but it is unencrypted:
back it up accordingly. The authenticated Dashboard's `/secrets` page renders
the same metadata for every declared name with replace and clear forms; it
never reveals or prefills a value.

```sh
# Store or rotate the OpenRouter key. The value is never returned.
curl -s -X PUT localhost:8081/api/secrets/OPENROUTER_API_KEY \
  -H 'Content-Type: application/json' -d '{"value":"sk-or-v1-..."}'

# Clear the stored value; the .env value applies again when present.
curl -s -X DELETE localhost:8081/api/secrets/OPENROUTER_API_KEY
```

The secrets route is part of the same `/api/` handler as settings; it is mounted
only on the localhost debug listener and behind the authenticated Dashboard.

### Audio

Per [ADR-0001](docs/adr/0001-middleware-owns-all-audio-io.md) and
[ADR-0004](docs/adr/0004-spotify-plays-through-the-pi.md), only the `assistant`
and `soloist` containers mount the host's Pulse socket; `tts` and the other
model services do computation only. Compose runs both audio containers as
`${ASSISTANT_UID}` (default 1000) so they can read the host Pulse cookie.
`assistant` plays replies through `pacat`; Soloist uses the Pulse default
sink, which is the same named sink the Assistant uses. Set `ASSISTANT_PULSE_SINK` to a sink name
from `pactl list short sinks`; empty uses the default sink for both. On the Pi
this is the Echo Dot once Bluetooth A2DP is set up.

Non-secret settings live in [`assistant/config.yaml`](assistant/config.yaml).
`.env` holds deployment inputs and the secret bootstrap; a value stored through
`/api/secrets` wins. An empty `tts.addr` disables spoken replies.

#### Sound library and Cue roles

A **Cue** is a non-speech sound with a fixed role: `wake`, `timer_ring`,
`alarm_ring`, `reminder` (before the Reminder is spoken) and `error` (before the
Unavailable message). The built-in `chime` is synthesised in Go and ships in the
image. Any `.wav` file dropped into `data/sounds/` is indexed at startup with
its name, duration and first-added date, using pure-Go decoding (ADR-0001, no
ffmpeg).

The Dashboard **Sounds** page (`/sounds`) uploads `.wav` or `.mp3` files up to
10 MB. The assistant decodes them in Go (MP3 via `hajimehoshi/go-mp3` v0.3.4,
Apache-2.0, pinned in `go.mod`; no runtime ffmpeg), downmixes
to mono, resamples to the speaker/capture format, normalises loudness and stores
one canonical PCM16 WAV in `data/sounds/`. A failed or undecodable upload leaves
no file or index row. Uploads are not length-capped: the role caps below apply
when a sound is assigned, so one long recording can still serve a 60 s Ring.

Each role is a live setting named `sounds.<role>`:
`GET /api/settings` lists them and `PUT` changes one. A value is a library name
such as `birds`, a path under the data volume such as `sounds/birds.wav`, a
built-in name, or `none` to stay silent. `config.yaml` may set the same values
for a headless setup; a stored override wins. The Sounds page shows the library,
assigns roles and edits the per-kind Timer/Alarm Ring settings that the Ring
modules define.

A browser preview streams the canonical WAV from `GET /sounds/audio/<name>`
(authenticated, `audio/wav`, Range supported). **Play on Pi** queues the sound
through the same Announcement queue as unprompted speech: it waits for the
current reply, a pending Confirmation and a ringing alert, and a new turn
cancels it so a reply is never cut.

```sh
# Assign a dropped file to the error Cue, then reset to config.yaml.
curl -s -X PUT localhost:8081/api/settings/sounds.error \
  -H 'Content-Type: application/json' -d '{"value":"birds"}'
curl -s -X DELETE localhost:8081/api/settings/sounds.error
```

Length caps are enforced when a sound is assigned and never trim the file:
`wake` 1.0 s, `reminder`/`error` 3 s, Rings 60 s. A longer sound is rejected
with the cap in the error. The wake, error and reminder Cues are wired, and a
Timer or Alarm Ring loops `sounds.timer_ring` or `sounds.alarm_ring` through the
Ring engine: it waits for the current reply sentence, ducks music, speaks the
label and stops on "stop" or "snooze". A `none` or missing Ring sound falls
back to the old single spoken line. A Reminder plays `sounds.reminder` once
through the urgent Announcement item, then speaks its message; `none` leaves
only the message.

A per-Alarm sound override is a library name too, resolved by the shared name
matcher and checked against the same 60 s Ring cap when it is assigned. It is
applied to that Alarm's Ring only; `sounds.alarm_ring` remains the fallback
when an Alarm has no override or its stored sound is no longer in the library.

### Latency

Every request writes one JSON `request` line with per-stage timings
(endpointing, STT finalise, routing, Tool execution, Agent first token, first
chunk, TTS first audio) and Response latency from the user's last voiced frame
to the first actual reply sample. Summarize the running stack with:

```sh
make latency-report
```

Stage definitions, the budget from #12 and how to read missing or failed stages
are in [`docs/latency.md`](docs/latency.md). The latest dev-box numbers are in
[`docs/latency-dev-box.md`](docs/latency-dev-box.md).

## Tests

```sh
make test       # Go tests, Compose checker and shell script tests
make tts-test   # Wyoming/pcm Python tests; needs `pip install wyoming pytest numpy`
python -m pytest tts/tests stt/tests
```

## Deploying

`make deploy` is the deployment path: it pulls the current branch, builds
SHA-tagged images, starts the stack and waits for health checks. Roll back with
`make rollback SHA=<commit>`, which reuses that commit's images without a
rebuild. The Pi provisioning checklist, image-tagging rules and the model-weights
layer convention are in [`docs/deploy.md`](docs/deploy.md).
