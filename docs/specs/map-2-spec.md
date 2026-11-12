# Map 2 spec

Status: **grilled, ready for `/to-tickets`** (2026-11-11). Built from [map-2-brief.md](./map-2-brief.md). Terms follow [GLOSSARY.md](../../GLOSSARY.md).

## Scope

- One Map 2, phased. Phases 1–5 are specified to ticket depth.
- **Phase order**: (1) Tool modules + store; (2) Dashboard core (auth, settings, secrets, sound/voice uploads, health) + quick wins + Scheduled alerts + Sounds + Voice/Persona, possibly split 2a/2b at ticket time; (3) web search; (4) Spotify; (5) Dashboard extras (activity, Hermes page, typed requests); (6) Hermes; (7) OpenCode.
- Phases 6–7 (Hermes, OpenCode self-extension) are fixed at the contract level only: the "ask Hermes" prefix, `hermes.delegate`/`status`/`result`, and OpenCode Go as the provider. Their tickets come after a prototype spike of `opencode run --attach`.

## Decisions

### Persistence and settings
- One SQLite database owned by `assistant` ([ADR-0003](../adr/0003-one-sqlite-store-owned-by-assistant.md)).
- Effective setting = stored override → `config.yaml` → code default. The Dashboard never writes `config.yaml`, and each override can be reset.
- Live vs restart: settings read per turn apply live (Tool enabled/Permission, Control phrases, voice, Filler/Unavailable wording, model names). Semantic `Examples` re-embed in the background and swap in when ready. Connection-level settings (audio devices, Wyoming endpoints, ports) are marked "restart required". There is no general hot-reload framework.

### Shared words and unprompted speech
- Shared Control phrases act on the current **Focus**, in a fixed priority order defined in code: ringing Scheduled alert > current reply > playing music. If nothing has Focus, the utterance falls through to the semantic Fast path.
- **Announcements** queue and play between turns, never mid-reply or during a Confirmation. A Ring ducks music and waits for the current reply sentence to finish. Quiet hours suppress or delay everything except Alarms.

- **Secrets**: a write-only `secrets` table in the same database (file 0600, unencrypted). `.env` is the bootstrap; a stored value wins over `.env`, and the Dashboard shows which source is active. If the database is ever backed up off the Pi, revisit encryption at rest.

### Tool modules
- Each domain package exports one **Tool module**: name (= config section `tools.<name>`), typed config struct with defaults, required secret names, optional health check, and `Register(r, deps)`.
- Modules receive a shared `Deps` (clock, logger, store, speaker, Announcement queue) plus their own config section. This replaces the per-domain fields of `tools.Options`. Adding a Tool = one package + one line in the module list.
- Overridable wording in Map 2: Control phrases, semantic `Examples`, Agent request / "ask Hermes" prefixes, Filler, Unavailable message. `Speak` replies stay Go code (no templates).
- Control phrases map either to a Tool (single-purpose, e.g. "louder") or to a shared **intent** from a fixed list in code: `stop`, `pause`, `resume`, `next`, `cancel`, `snooze`. Each Tool module declares which intents it handles when it holds Focus.
- **Scaffold** (phase 1): `make new-tool name=<module>.<tool>` generates a Tool module package (module value, config struct, one Tool, a test file wired to the conformance suite, starter `Examples`) and adds the line to the module list. Every phase 2 Tool is built with it, so it's proven before OpenCode uses it in phase 7.
- **Conformance suite** (phase 1): a shared test helper every Tool module calls with one line. It checks that the module registers cleanly, that each Tool declares Local-only and a valid Permission level, that config defaults validate, that `Speak` returns non-empty text for a sample output, and that the Tool's `Examples` route to it on the semantic Fast path. Module-specific behaviour tests go beside it.
- **Agent guidance** (phase 1): `docs/agents/adding-a-tool.md`, linked from `AGENTS.md`. It's the step-by-step recipe: run the scaffold, choose Permission and Local-only, write `Examples` and negative cases, add eval cases, run the conformance suite, and update the glossary if a new term appears. The scaffold's generated files point to it, and Hermes/OpenCode get it as the recipe for self-extension.
- **Routing eval**: every Tool ticket adds its `Examples` and a few negative cases to the #26 eval set. Phase 1 adds Focus-intent cases ("stop" during a Ring vs a reply vs music). CI fails if accuracy drops below the Map 1 baseline. Updating the eval is part of the definition of done for any Tool ticket.
- **Settings by voice**: a voice Tool that changes a setting writes the same stored override as the Dashboard, is `act`, and says what it changed. In Map 2 that covers voice, rate, Quiet hours, per-Alarm sound/volume/ramp, and Persona. Everything else is Dashboard-only.

### Scheduled alerts
- Three kinds: Timer (duration, rings), Alarm (clock time, can repeat, rings), Reminder (either kind of time, speaks once).
- In scope: labels, `timer.remaining`, one-off and repeating Alarms (daily, weekdays, weekends, chosen weekdays), Reminders, snooze, persistence. Out: calendar-linked or location-based alerts, Ringing on the phone.
- Timezone: a `timezone` setting (overridable). If unset, `TZ` is used; UTC logs a startup warning. No IP auto-detection.
- Ring: alert sound looped, label spoken once at the start, until "stop"/"snooze" or a max ring time (default 5 min). After the max ring time the alert is recorded as missed and a Notification is sent if configured. Snooze (default 9 min) applies to Alarms only; "snooze" on a Timer acts as "stop".
- Downtime: on startup, alerts overdue by ≤ 2 min go off now. Older ones become one summary Announcement. Repeating Alarms then schedule their next occurrence.
- Local-only: `*.set`, `*.cancel` and `timer.remaining` are `LocalOnlyNo`. `timer.list`, `alarm.list` and `reminder.list` are `LocalOnlyYes`, so they need strong `Examples` for the Fast path.
- User-customisable sounds and Ring behaviour: see Sounds.

### Sounds
- **Cue** roles (fixed in code): `wake`, `timer_ring`, `alarm_ring`, `reminder` (before a Reminder is spoken), `error` (before the Unavailable message). Each role is a setting naming a library sound, or `none`. Built-ins ship in the image; the current synthesised wake chime is the `wake` default.
- Library: uploaded sounds are files in `sounds/` on the data volume, and the database holds the index (name, duration, uploaded date) and role assignments. `config.yaml` may name a built-in or a volume path, so headless setups work.
- Upload: `.wav` and `.mp3`, ≤ 10 MB. Decoded with pure-Go decoders (no ffmpeg), downmixed, resampled to the sink format, loudness-normalised, stored as canonical WAV. Undecodable files are rejected with a clear error.
- Length caps, checked when a sound is assigned to a role (not at upload): `wake` ≤ 1.0 s (endpointing waits for the wake Cue to finish), `reminder`/`error` ≤ 3 s, Rings ≤ 60 s. Too long is rejected, never trimmed.
- Ring settings per kind (Timer, Alarm): `sound`, `max_ring` (5 min), `snooze` (Alarm only, 9 min), `volume` (absolute, independent of media volume), `ramp` (Timer 0 s, Alarm 30 s), `gap` (1 s), `speak_label` (`start` | `every_loop` | `never`, default `start`).
- Per-Alarm overrides of `sound`, `volume` and `ramp`: in the Dashboard **and by voice** ("wake me at 7 with the birds sound").
- Preview: the Dashboard plays a Cue in the browser and on the Pi speaker; the Pi preview obeys the Focus/Announcement rules.
- By voice: `alarm.set` takes an optional `sound`, and `alarm.update` changes an existing Alarm ("change my 7 am alarm to the birds sound"). If the name doesn't match, the Alarm is set with the default sound and the reply says so.
- One shared **name matcher** (normalise, then embedding similarity with a threshold) resolves spoken names for sounds, voices and Spotify playlists.

### Voice and Persona
- Voice is a persistent setting only (no per-request voice): `voice.set`, `voice.list`, `voice.current`, plus a `rate` setting. Previews happen in the Dashboard.
- **Persona** (voice + personality prompt + Filler/Unavailable wording) is in Map 2, as the last ticket of phase 2.
- Custom cloned voices are supplied as pre-exported Pocket TTS `.safetensors` voice states. Cloning from WAV on the Pi stays out: it needs the gated weights.
- Voice library: only `.safetensors` (no `.pt`/`.pth`), ≤ 32 MB, stored in `voices/` on the data volume and mounted read-only into `tts`. Never baked into an image or committed. Config may point at a volume path, so files can be copied in without the Dashboard.
- Upload validation: header structure check (12 tensors; `self_attn/cache` F32 `[2,1,N,16,64]` with N ≤ ~400, `offset` I64), then a `tts` smoke synthesis, then the model revision it passed against is recorded.
- Stale voices: if `tts` starts on a different model revision, the voice is marked stale. If it was active, fall back to the default built-in voice and make one Announcement. The Dashboard flags it for re-export on the desktop. pocket-tts stays pinned.
- Warm-loading: the active voice and the active Persona's voice load at startup; others load on first use and stay cached.
- Each voice has an optional free-text source/consent note (recorded, not enforced).

### Spotify
- Account has Premium (confirmed 2026-11-11).
- Playback runs on the Pi through librespot into the same Pulse sink ([ADR-0004](../adr/0004-spotify-plays-through-the-pi.md)). "Play on <device>" transfer is a later extra.
- Tools: `spotify.play` (resume, or search: track/artist/album/playlist/liked songs), `pause`, `next`, `previous`, `shuffle`, `repeat`, `volume`, `now_playing`, `queue_add`, `like`, `play_playlist` (with shuffle). Playlist names resolve through a local cache of the user's playlists (refreshed at startup and every 6 hours) and the shared name matcher. "pause"/"next"/"resume" reach Spotify through Focus intents, not their own Control phrases.
- Wake word over music: a spike ticket comes first in phase 4 and measures wake-word hit rate at three music volumes with PipeWire `echo-cancel` on the host (sink as reference). `music.max_volume` is the fallback cap if AEC can't hold accuracy.
- Auth: librespot pairs once by zeroconf and keeps its cached credentials on the volume. The Web API OAuth refresh token is stored in the secrets table through the Dashboard Connect button. The health page shows the two separately.

### Quick wins (phase 2)
- In: `assistant.repeat`, `assistant.help` (generated from the registry), `time.in_city`, `date.countdown`, `calc.evaluate`, `units.convert`, `random.flip`/`roll`/`pick`, `system.status`, quieter/mute for `audio.volume`, Quiet hours, `notify.phone`.
- Deferred: lists and notes ([#32](https://github.com/Jairik/clanka-bot/issues/32)), `news.headlines`, `sun.times`, `weather.forecast`, the Home Assistant bridge. `desktop.wake` moves to phase 7. Routines and Briefings are out of Map 2 ("good night" routine is a candidate first Map 3 ticket).

### Safety
- Permission overrides only tighten (`read` → `act` → `confirm`, or disable), never loosen below the code-declared level. Local-only can't be overridden.

### Quiet hours
- A daily window setting (e.g. 22:30–07:00). Inside it, Announcements are held and then summarised as one, Notifications are muted, and replies use `quiet.volume`. Alarms Ring as normal, and Timers and Reminders still go off at quiet volume.
- Configurable **by voice and in the Dashboard**, live: "set quiet hours from 10:30 to 7", "turn off quiet hours", "do not disturb for two hours", "I'm awake". The voice Tools are `act` and write the same stored override as the Dashboard.

### Notifications
- ntfy is the only push channel in Map 2, landing in phase 2. It defaults to ntfy.sh with a random topic, and the URL is a setting. The topic is generated once on first use and kept in the secrets table. A token is optional: ntfy only accepts tokens its server issues, so a self-hosted ntfy with access control takes a server-issued token through the Secrets page (or `.env`) and the Assistant never invents one. On public ntfy.sh the random topic is obscurity, not access control.

### Tier 2 (Agent path)
- SearXNG `web.search` + `web.fetch`, as in the brief. Kiwix, a "deep think" model switch and memory beyond the 10-turn history are out of Map 2.
- GitHub as plain Tool modules in phase 3, using two tokens from the secrets table: a fine-grained PAT (`GITHUB_TOKEN`) for `github.issue_create` (`confirm`) and `github.pr_list` (`read`), and a separate OPTIONAL classic PAT with only the `notifications` scope (`GITHUB_NOTIFICATIONS_TOKEN`) used exclusively by `github.notifications` (`read`, never marks threads read). GitHub's notifications API does not accept fine-grained PATs. The fine-grained token is never sent to the notifications endpoint; without the classic secret, `github.notifications` says a classic notifications token is needed and its health entry reads not-configured, while issue and PR use stay healthy.

### RAM budget
- A measurement ticket at the end of phases 3, 4 and 6: `docker stats` on the Pi under a scripted load (wake → STT → agent → TTS with music playing). If peak RAM is above 6.5 GB, the next phase waits until something is trimmed.

### Hermes (contract level)
- A finished or failed job produces a one-line Announcement from the job's own summary, with no extra model call. The tier 2 agent summarises further only when asked.
- Routing: a **Hermes request** ("ask Hermes …") starts a Hermes job directly, alongside the existing Agent request prefix. Otherwise the tier 2 agent may call `hermes.delegate` itself. Order: pending Confirmation → prefixes → Control phrase / Focus intent → semantic Fast path → Agent path.
- Hermes job state lives in the `assistant` database. Tools: `hermes.delegate`, `hermes.status`, `hermes.result`, plus cancel.
- MCP surface: the registry minus Local-only Tools **and minus `confirm` Tools**, so Hermes sees `read` and `act` Tools only.
- Outward writes: `hermes.delegate` is `act` for read-only or research jobs. A job that writes outside the Pi needs a `confirm` at delegation, and its summary names the writes. Even then Hermes can only push branches and open **draft** PRs and issues with a scoped PAT. Nothing merges, deploys or posts publicly without the user.

### Dashboard
- Served by `assistant` on its own listener bound to localhost, exposed only through `tailscale serve`. Server-rendered Go templates + htmx, no JS build step. A future phone Satellite app is separate and shares the API.
- Auth: Tailscale identity only. A Tailscale ACL limits the port to the owner, and `assistant` checks the identity headers. No app password; secrets and uploads get the same access as everything else.

## Open (for ticketing, not decisions)
- Whether phase 2 splits into 2a/2b.
- Hermes and OpenCode tickets come after the `opencode run --attach` prototype spike.
