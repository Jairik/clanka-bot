# clanka-bot

My lovely always-on voice assistant for stuff written in go. Kind of like a lobotomized jarvis,

## Run the prototype

The prototype is the `assistant` Go service in Docker, with a `wakeword`
(openWakeWord), an `stt` (Moonshine Voice) and a `tts` (Pocket TTS) service.
`assistant` owns the microphone and speaker, and also exposes a health endpoint
and a localhost-only debug entry that accepts a typed transcript (entering the
pipeline just after STT).

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
sink as it is synthesised (`"spoken": true`). Paraphrases work too, via the
`embed` MiniLM service that Compose starts alongside the assistant:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it right now"}'
```

Weather answers on the Fast path too. The assistant resolves the city from the
Pi's public IP at startup and every 24h (only city, region and timezone are
kept; coordinates are never stored) and asks Open-Meteo for current conditions:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d "{\"text\":\"what's the weather like\"}"
```

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

Say the wake word (default `okay_nabu`), ask a question, and hear the answer:

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

`wakeword` and `stt` do computation only and never mount Pulse (ADR-0001).
Their Compose health checks are Wyoming `describe` calls, so startup waits for
the models to be warm. Moonshine weights are downloaded anonymously from
`download.moonshine.ai` at build time and baked in; the container starts
offline. Rebuild `stt` after changing `STT_MODELS` or `STT_MODEL`. Set
`ASSISTANT_PULSE_SOURCE` from `pactl list short sources` (the dev box uses the
USB webcam's mono source).

The mic endpoint threshold (`stt.endpoint.threshold_dbfs`, default -38) was set
from the dev box's measured noise floor of about -45 dBFS. Lower it if the
Assistant cuts you off, raise it if it never ends a request.

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
```

Timers are stored in the database and survive a restart. Give one a label
("set a pasta timer for 10 minutes") and ask for it later ("how long is left on
the pasta timer"); `timer.list` is Local-only because labels can be private.
On startup, a Timer up to two minutes overdue goes off immediately, while older
ones are recorded missed and summarised in one Announcement. A Timer that fires
**Rings**: it loops the assigned `timer_ring` Cue with a gap, speaks its label
according to `ring.timer.speak_label`, and keeps ringing until `stop`/`snooze`
or `ring.timer.max_ring` (default 5 min). `snooze` on a Timer acts as stop.
Reaching max_ring records the alert as missed in `alert_events`; the
`Manager.SetMissedHook` seam is where #62 will send the ntfy Notification.

A Ring waits for the current reply sentence to finish (not the whole reply),
then takes the one speaker; the reply's next sentence waits for the Ring and
resumes afterwards, so ringing never cancels an answer. Music will be ducked
through the `ring.Music` seam when Spotify lands (#72), and the restore runs on
stop, timeout and shutdown.

Ring settings are live and per kind:

| Setting | Timer default | Alarm default |
| --- | --- | --- |
| `ring.<kind>.max_ring` | 5m | 5m |
| `ring.<kind>.volume` | 80% | 80% |
| `ring.<kind>.ramp` | 0s | 30s |
| `ring.<kind>.gap` | 1s | 1s |
| `ring.<kind>.speak_label` | `start` | `start` |

`volume` is absolute: it scales the Cue PCM and is independent of the media
volume. The sound itself is the per-kind Cue role from the Sound library,
`sounds.timer_ring` or `sounds.alarm_ring`; there is no second sound key. The
same settings service the Dashboard writes through is read again at each Ring,
so a change applies to the next fire. Alarms (#53) reuse the same engine and
settings; Alarm snooze is deferred to that ticket.

The Control phrases `stop`, `pause`, `cancel`, `snooze` and `louder` are matched
exactly, so they answer without waiting on the embed service. `stop`, `pause`,
`cancel` and `snooze` are shared Focus intents: they act on a ringing Timer
first, then the current reply, then playing music. With nothing in Focus they
fall through to the semantic Fast path, so `cancel` still reaches
`timer.cancel` and `louder` still steps the volume up.

Speech the Assistant starts without being asked (a Reminder, a finished Hermes
job) goes through one Announcement queue. It waits for the current reply to
finish and for any pending Confirmation to resolve, then speaks one line at a
time. Queue one from the debug entry; posting it while a reply or a Confirmation
question is in progress is what makes it wait:

```sh
curl -s -X POST localhost:8081/debug/announce \
  -H 'Content-Type: application/json' -d '{"text":"Dinner is ready."}'
```

Quiet hours will hold and release the same queue; nothing is spoken while a
hold is on.

Routing precision is measured against a labelled corpus with the real MiniLM
model; the floor is 0.95 for every Tool, and ambiguous phrasing goes to the
Agent path. See [docs/routing-eval.md](docs/routing-eval.md) to run the
evaluation, tune threshold and margin, or promote a request-log misroute into
the fixture.

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

The Dashboard also has a server-rendered **Tools and settings** page at
`/settings`. Every Tool is listed with its effective and code-declared
Permission, Local-only flag and enabled state; Permission choices are only the
valid tightening levels. Every wording value is an ordinary text or one-entry-
per-line form with its effective value, source layer and a Reset button.
Settings the service exposes but this page does not know by name (for example a
future sound setting) appear automatically, and connection settings carry a
Restart required badge. The forms work without JavaScript and use htmx for the
same-page fragment update when it is available.

The **Sounds** page at `/sounds` is the upload/library/role/preview surface
described under Sound library and Cue roles below. Its Ring section renders the
per-kind Timer and Alarm definitions from the settings service, so a new key
appears there without a page change.

The **Secrets** page at `/secrets` lists every secret a Tool module or the
Agent provider declares, with whether it is set, its last four characters
(hidden for short values), the update time and the active source (`.env` or
stored). Each row offers Replace and, for a stored value, Clear. The forms are
write-only: no value is prefilled, echoed after a rejected submission or
returned anywhere. Clearing a stored value exposes the `.env` bootstrap
immediately and the page says which source is active. A new module declaration
appears without a page change.

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
| `tools.<name>.examples` | Semantic Fast path Examples. A change re-embeds in the background; routing keeps the old vectors until the new ones are ready, and a superseded rebuild is discarded. |
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

Per [ADR-0001](docs/adr/0001-middleware-owns-all-audio-io.md), only the
`assistant` container mounts the host's Pulse socket; `tts` does computation
only. Compose runs `assistant` as `${ASSISTANT_UID}` (default 1000) so it can
read the host Pulse cookie, and plays through `pacat`. Set
`ASSISTANT_PULSE_SINK` to a sink name from `pactl list short sinks`; empty uses
the default sink. On the Pi this is the Echo Dot once Bluetooth A2DP is set up.

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
with the cap in the error. The wake and error Cues are wired, and a Timer Ring
loops `sounds.timer_ring` through the Ring engine: it waits for the current
reply sentence, ducks music, speaks the label and stops on "stop" or "snooze".
An Alarm will use the same engine and `sounds.alarm_ring` (#53); a `none` or
missing Ring sound falls back to the old single spoken line. Reminders do not
exist yet.

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
make test   # Go tests, Compose checker and shell script tests
python -m pytest tts/tests stt/tests   # needs `pip install wyoming pytest numpy`
```

## Deploying

`make deploy` is the deployment path: it pulls the current branch, builds
SHA-tagged images, starts the stack and waits for health checks. Roll back with
`make rollback SHA=<commit>`, which reuses that commit's images without a
rebuild. The Pi provisioning checklist, image-tagging rules and the model-weights
layer convention are in [`docs/deploy.md`](docs/deploy.md).
