# Assistant

A self-hosted personal voice assistant centred on a Raspberry Pi 5: it hears requests, resolves them, and answers by voice or on screen. (Working name; the product may be renamed later.)

## Language

### Requests

**Tool**:
A single named operation the Assistant can perform, such as `calendar.get_events`, available to both the Fast path and the Agent path. Tools are leaf operations: a Tool never calls another Tool.
_Avoid_: Skill, command, action, plugin

**Tool module**:
A package that contributes one or more related Tools together with their settings, the secrets they need and an optional health check.
_Avoid_: Plugin, integration, domain package

**Permission level**:
How much a Tool may change the world: `read` (no side effects), `act` (harmless or easily undone side effects), or `confirm` (hard to undo or outward-facing, needs a Confirmation).
_Avoid_: Scope, access level, risk tier

**Local-only Tool**:
A Tool whose results must never leave the Pi, so the Agent path can neither see nor call it.
_Avoid_: Private tool, offline tool, sensitive tool

**Agent-only Tool**:
A Tool the semantic Fast path must never choose, so only the Agent path can call it. It declares `AgentOnly` and has no matching Examples or Extract; the name is listed for the Agent's tool set, not the Fast path index. `LocalOnlyYes` with `AgentOnly` is rejected at registration, because then no path could call it.
_Avoid_: Hidden Tool, private tool, non-voice Tool

**Confirmation**:
The user's spoken yes that a `confirm` Tool call needs before it runs. It lasts one turn, and anything other than a clear yes drops the call.
_Avoid_: Approval, consent, verification

**Fast path**:
Handling a request by matching it straight to a Tool on the device, without the general-purpose agent. The match may be by meaning, not only exact wording; a Control phrase is also part of the Fast path.
_Avoid_: Deterministic route, local intent

**Control phrase**:
A short fixed utterance, such as "stop" or "louder", that maps directly to a Tool call without matching by meaning.
_Avoid_: Hotword, command word

**Agent path**:
Handling a request by handing it to the general-purpose agent, which may call Tools as part of its answer.
_Avoid_: LLM fallback, Hermes route

**Agent request**:
An utterance that starts with a fixed prefix such as "ask the agent", which sends the request straight to the Agent path.
_Avoid_: Escape phrase, override

**Focus**:
The activity a shared Control phrase such as "stop" or "pause" acts on: a ringing Scheduled alert first, then the Assistant's current reply, then playing music.
_Avoid_: Active context, target, foreground

**Focus intent**:
One of the fixed shared Control-phrase actions — `stop`, `pause`, `resume`, `next`, `cancel`, `snooze` — that goes to whatever holds Focus instead of to one named Tool.
_Avoid_: Command, action, gesture

**Hermes request**:
An utterance that starts with the prefix "ask Hermes", which sends the request straight to Hermes as a Hermes job, skipping the Agent path.
_Avoid_: Hermes route, tier 3 request

**Hermes job**:
A unit of work Hermes runs in the background on the user's behalf, with a status and a one-line summary when it finishes.
_Avoid_: Task, run, session

**Barge-in**:
Interrupting the Assistant mid-reply by saying the wake word, which abandons the current reply and starts a new request.
_Avoid_: Interrupt, cut-off

**Response latency**:
The time from the user's last spoken sound to the first sound of the Assistant's actual reply. Chimes and fillers played while waiting don't count as the reply.
_Avoid_: Lag, delay, turnaround

**Filler**:
A short phrase, such as "one moment", spoken while the user waits: when a reply is slow to start, or as soon as a request goes to the Agent path, so the user knows it is leaving the device. At most one per request.
_Avoid_: Holding message, stall phrase

**Unavailable message**:
A configurable phrase spoken when the Agent path can't reach its model, so the user knows the request wasn't handled. The Fast path keeps working.
_Avoid_: Error message, brain-offline line

**Last reply**:
The most recent completed Assistant answer kept in memory, with the Tool that produced it and whether that result was Local-only. Follow-ups ("say that again", "send that to my phone") read it; a cancelled or blank turn does not replace it.
_Avoid_: Previous output, last message, say-again buffer

**Name matcher**:
The one lookup that resolves a spoken library name — a sound, a voice or a Spotify playlist — first by exact or normalised spelling and then by embedding similarity above a threshold. It returns one name or no match, never a forced guess.
_Avoid_: Fuzzy search, autocomplete

**Calendar countdown**:
The number of whole calendar days from today to a fixed date such as Christmas, counted in local dates in the effective timezone. A DST change does not add or remove a day. It is not a Timer.
_Avoid_: Timer, elapsed time

**City time**:
The current local time in a named city, resolved from a shipped city-to-IANA table with embedded tzdata and no network lookup.
_Avoid_: World clock, timezone guess

### Devices

**Satellite**:
Any device other than the Pi that a user speaks, types to, or listens through: the phone web app, a desktop client, or a future wireless microphone.
_Avoid_: Client, endpoint, remote

### Outputs

**Briefing**:
A spoken or displayed summary composed from the results of several Tools, such as the morning schedule plus important email.
_Avoid_: Digest, report, summary

**Notification**:
An alert the Assistant pushes unprompted to a Satellite or display, subject to cooldowns. A Notification may also be spoken as an Announcement.
_Avoid_: Alert, push, ping

**Announcement**:
Speech the Assistant starts without the user having just asked, such as a Reminder or a finished Hermes job. It waits for a gap between turns. A ringing Timer or Alarm is not an Announcement.
_Avoid_: Unprompted speech, proactive message

**Quiet hours**:
A window during which the Assistant holds Announcements, mutes Notifications and speaks quietly,
while Alarms still Ring. It can follow a daily schedule in the effective
timezone, a timed do-not-disturb period, or an "I'm awake" bypass that ends only
the current window. Held Announcements are released as one summary.
_Avoid_: Do not disturb, DND, night mode, sleep mode

### Scheduled alerts

**Scheduled alert**:
Any Timer, Alarm or Reminder: something the user sets now that goes off later.
_Avoid_: Alert, schedule, job

**Timer**:
A Scheduled alert set as a duration ("10 minutes") that rings when it runs out.
_Avoid_: Countdown

**Alarm**:
A Scheduled alert set for a clock time, possibly repeating on chosen days, that rings until stopped or snoozed.
_Avoid_: Wake-up, timer

**Alarm override**:
A per-Alarm choice of Ring sound, volume or ramp that replaces the kind defaults for that Alarm only. `alarm.set` accepts it, `alarm.update` changes it, and the Dashboard Scheduled alerts page writes the same row; an empty value falls back to `sounds.alarm_ring` and `ring.alarm.*`.
_Avoid_: Custom alarm, alarm theme

**Snooze**:
The request to silence a ringing Alarm and ring it again after `ring.alarm.snooze` (default 9 minutes). On a Timer it acts as stop.
_Avoid_: Nap, delay

**Reminder**:
A Scheduled alert that speaks its message once instead of ringing, whether it was set for a duration or a clock time.
_Avoid_: Note, alarm with message

**Label**:
A short spoken name attached to a Scheduled alert, such as "pasta", so a later request can name it ("how long is left on the pasta timer").
_Avoid_: Name, tag, title

**Ring**:
The repeating sound a Timer or Alarm makes when it goes off, until it is stopped, snoozed or times out.
_Avoid_: Alert sound

**Missed alert**:
A Ring that reached its `max_ring` without stop or snooze. It stays in the alert history and is what a Notification reports.
_Avoid_: Ignored alarm, expired timer

**Cue**:
A non-speech sound the Assistant plays for a fixed role, such as on wake detection or as a Ring. Which sound fills each role is the user's choice.
_Avoid_: Chime, earcon, beep, notification sound

**Sound library**:
The built-in sounds that ship in the image plus the canonical WAV files in `sounds/` on the data volume, indexed by name, duration and added date. Uploading a WAV or MP3 through the Dashboard stores one downmixed, resampled and loudness-normalised WAV; a Cue role names one library sound or `none`.
_Avoid_: Sound bank, audio collection

**Voice library**:
The built-in Pocket TTS voices warmed in the `tts` image plus the pre-exported `.safetensors` voice states in `voices/` on the voices volume. The active entry is one persistent setting; other entries load on first use.
_Avoid_: Voice bank, voice pack

**Voice state**:
A pre-exported Pocket TTS `.safetensors` file holding the streaming model state for one voice. It is validated against a model revision and becomes stale when that revision changes; `.pt`/`.pth` pickle files are never accepted.
_Avoid_: Voice file, voice model, embedding

**Speech rate**:
The `voice.rate` multiplier that changes how fast the Assistant speaks: `1.0` is normal, values above it are faster. It is implemented by resampling, so pitch moves with speed.
_Avoid_: Speed, tempo, speaking pace

**Persona**:
A named bundle of a voice, a personality prompt and the Filler and Unavailable wording. Selecting one applies the voice and wording through the live settings and lets the personality prompt enter only the single prompt builder's labelled system part; a later manual voice or wording change is reported as a difference rather than claimed as the bundle.
_Avoid_: Character, mode, skin, profile

**Spotify Connect pairing**:
The one-time grant where the Spotify app hands a Connect receiver its cached credentials over zeroconf. The `librespot` container keeps those credentials on its own volume; it is separate from Web API auth and survives container restarts.
_Avoid_: Login, Spotify password, account link

**Spotify Web API auth**:
The Authorization Code with PKCE grant whose refresh token is stored in the write-only secrets table. It authorises control calls such as play, pause, search and queue, not the audio path; the refresh token is never a config value or a file.
_Avoid_: Spotify login, API key, client secret

**Librespot**:
The Spotify Connect receiver container. It advertises a device over mDNS on the host network and plays into the Assistant's Pulse sink ([ADR-0004](docs/adr/0004-spotify-plays-through-the-pi.md)).
_Avoid_: Spotify player, Spotify daemon
