# Map 2 brief: after the prototype

Status: **ideation draft** (2026-11-11). Input for a grilling session, then `/to-tickets`.
Assumes Map 1 ([#1](https://github.com/Jairik/clanka-bot/issues/1)) is done: working voice pipeline, Tool registry, Fast path / Agent path routing, Pocket TTS, deploy to the Pi.

## Destination

The Assistant can do almost everything by voice. **Common actions are Tools** (fast, local and predictable). **Everything else goes to Hermes**, which runs on the Pi with broad integrations and can hand coding work to OpenCode. A **Dashboard** reachable over Tailscale configures it all without SSH.

## Three tiers

Priorities, in order: **speed, cost, then least complexity**.

| Tier | What | Model / provider | Handles | Examples |
|---|---|---|---|---|
| 1. Deterministic | Control phrases + semantic Fast path (Map 1) | none | Short, frequent actions | timers, Spotify controls, voice change, volume |
| 2. Conversational agent | The existing Go **Agent path** loop (`internal/agent`, #20) | OpenRouter, fast cheap model (DeepSeek V4.1 Flash) | Quick lookups, back-and-forth chat, summarising Hermes results, fallback for anything tier 1 misses, handing off to tier 3 | "who won the game last night", "what did Hermes change?" |
| 3. Hermes | Sidecar agent on the Pi | **OpenCode Go** | Multi-step, slow or integration-heavy work | "open an issue on X", "add a Tool that does Y", deep research |
| 3b. OpenCode (called by Hermes) | Coding agent on the desktop or Pi | **OpenCode Go** | Code changes | new Tool → PR |

**Decided (2026-11-11):** everything Hermes and OpenCode do uses the OpenCode Go subscription. The Go loop stays on OpenRouter. See §6 for the terms reasoning.

---

## 1. Modular Tools (foundation, do first)

Today: each domain package exposes `Register`, `tools/tools.go` wires them through one `Options` struct, Control phrases live in `config.yaml`, and `Examples` (Fast path phrasings) are hardcoded in Go.

Goals:
- **Adding a Tool touches one package plus one registration line.** No edits spread across the pipeline, config loader or router.
- **Wording is config, not code.** Control phrases, semantic `Examples`, `Speak` templates (maybe) and Agent request prefixes can all be overridden in config. Code defaults stay as the fallback.
- **Per-Tool enable/disable and Permission override in config.** The Dashboard needs this, and so does the safety story for Tools that Hermes writes.
- **Per-domain config sections** (`tools.spotify.*`, `tools.timer.*`) owned by their package, so `tools.Options` doesn't grow into a god struct.
- **A scaffold**: `make new-tool name=foo.bar` (or a template dir) that generates the package, test and registration. It also gives OpenCode a fixed recipe when Hermes asks for a new Tool.
- A **"what can you do"** Tool generated from the registry (names, descriptions, example phrasings).

Questions to grill:
- Does a domain package own a "Tool module" (Register + config schema + secrets list + health check), or do Tools stay as they are with config bolted on?
- Is the override layer `config.yaml` only, or `config.yaml` plus a runtime settings store that the Dashboard writes to (see §7)?
- Should Tools hot-reload phrasings/enabled state, or is a restart acceptable?
- Is a compile-time registry still right once Hermes writes Tools? (It's probably fine: PR → review → deploy.)

## 2. Timers and alarms

**Already in Map 1:** [#22](https://github.com/Jairik/clanka-bot/issues/22) (open, branch `jj/timers-and-audio-controls`) lands `date.today`, `timer.set`/`cancel`/`list`, `audio.volume_set`, `audio.stop`, a shared Go time/number parser (`internal/parse`), an audible alert, and the Control phrases "stop", "pause", "cancel" and "louder". Map 2 *extends* that work: alarms, labels/messages, `timer.remaining`, snooze, persistence and reminders. Re-read #22's final shape before grilling this section.

- **Timer**: a duration ("set a 10 minute timer for the pasta").
- **Alarm**: a clock time ("wake me up at 7", "alarm at 6:30 tomorrow, message: gym").
- Both take an **optional label/message** that is spoken when they fire.
- Tools: `timer.set`, `timer.list`, `timer.cancel`, `timer.remaining` ("how long left on the pasta timer"), `alarm.set`, `alarm.list`, `alarm.cancel`, plus a Control phrase for stop/snooze while one is ringing.
- Maybe: recurring alarms (weekdays), a **reminder** ("remind me at 5 to call mom"; it speaks once instead of ringing).

Questions to grill:
- Persistence: timers/alarms must survive a restart or deploy. This forces the SQLite decision Map 1 deferred.
- Firing behaviour: chime loop until "stop"? Max ring duration? What happens if it fires during a reply or Barge-in, or while music is playing (duck Spotify)?
- Is a firing the first **Notification**? Should it also go to the phone (ntfy) if nobody says stop?
- Local-only? Labels can be personal. Probably `LocalOnlyYes` for list Tools, or keep timers entirely on the Fast path.
- Timezone source: the IP-derived location from Map 1.

## 3. Voice change

- `voice.set` ("switch to the British voice"), `voice.list`, `voice.current`, possibly `voice.preview`.
- A catalog of named voices: Pocket TTS built-ins, plus cloned voices from wav files later (requires the voice-cloning bundle, which Map 1 avoided because it's gated).
- Persisted choice (same settings store as the Dashboard).
- Stretch: **Persona** = voice + system-prompt personality + Filler/unavailable wording (the "villain voice" idea from Map 1's Personality note).
- Maybe: speech rate, and a volume Tool (`audio.volume_set`, "louder"/"quieter" Control phrases).

Questions to grill: Is a voice switch per request ("say it in the pirate voice") or only a persistent setting? Is Persona in scope for Map 2?

## 4. Spotify

Tools (Spotify Web API, OAuth refresh token in `.env`):
- `spotify.play` (resume, or play a search: track / artist / album / playlist / "my liked songs"), `spotify.pause`, `spotify.next`, `spotify.previous`
- `spotify.shuffle` on/off, `spotify.repeat`, `spotify.volume`
- `spotify.now_playing` ("what song is this"), `spotify.queue_add`, `spotify.like`
- `spotify.play_playlist` with shuffle ("shuffle my gym playlist")
- Control phrases: "pause", "skip", "next song", "resume"

Questions to grill (Map 1 already flagged the first):
- **Where does audio play?** (a) The Echo Dot as its own Spotify Connect device, (b) librespot/Raspotify on the Pi routed to the same Pulse sink, or (c) any device the user picks ("play on my desktop"). This choice affects ducking and wake-word reliability.
- **Ducking**: lower music on wake word and during replies. That's easy if music goes through the Pi's Pulse sink, and needs an API volume call if it plays on the Echo Dot directly.
- Wake-word detection over loud music (no echo cancellation on the webcam mic).
- Requires Spotify Premium for playback control (confirm the account has it).
- Fuzzy matching of spoken names to playlists: a local cache of the user's playlists plus embed similarity?

## 5. Conversational agent and web search (tier 2)

A talk-to-it agent that doesn't go through Hermes: fast, cheap, good for looking things up and for conversation. It's the **existing Agent path loop**, extended. It isn't a new component.

**Decision direction: no agent framework.** Keep the hand-rolled `openai-go` loop already merged in #20. Reasons:
- It already exists and is small.
- The Tool registry is already the tool layer (schemas, permissions, timeouts).
- ADR-0002 makes the single prompt builder the only way data leaves the Pi. A framework such as Eino, Genkit Go or LangChainGo builds its own requests and would have to be bent around that.
- A framework adds dependency weight and indirection but saves very little code here.

Revisit only if we later need multi-agent graphs, which is what Hermes is for anyway.

New Tools the tier 2 agent calls (all ordinary registry Tools, `LocalOnlyNo`):
- **`web.search`** `{query, max_results}` → top N `{title, url, snippet}`. It sits behind a `Provider` interface like `location`, so the backend is a config switch:
  - **SearXNG**, **chosen as the default (2026-11-11)** because it runs on the device. It's a metasearch *proxy*, so queries still go out to upstream engines, but there's no key, no third-party API and no cost. Always-on container. Rough budget: about 200–500 MB RAM idle (keep 2 workers), near-zero idle CPU, a short single-core burst per query, and a roughly 250 MB image with no index. The Valkey limiter is off because the instance is private. Latency is about 0.5–1.5 s, all of it upstream: enable 3–4 fast engines (DuckDuckGo, Brave, Bing, Wikipedia; Google is optional because of CAPTCHA risk), set `request_timeout` to about 1.5 s, and use a keep-alive HTTP client in Go. Measure with `docker stats` on the Pi.
  - **Optional fully offline knowledge: Kiwix** (`kiwix-serve`, arm64, full-text search API) over a Wikipedia ZIM: about 12 GB mini, about 50 GB without pictures, 100 GB+ full (approximate; needs an SSD). It would be a separate `wiki.lookup` Tool that keeps working without internet, but it only knows things up to its snapshot date. Grill: worth the disk, and which ZIM?
  - SearXNG risk: upstream engines (Google especially) may rate-limit or CAPTCHA a self-hosted instance; fewer, faster engines reduce this.
  - **Brave Search API** (not chosen; an off-device fallback only if SearXNG proves unreliable). Fast and reliable. As of 2026 the free tier is gone, replaced by about $5/month of free credit (roughly 1k searches) and then about $5 per 1k. Needs a key (Dashboard secret).
  - **OpenRouter `openrouter:web_search` server tool** (not chosen: off-device). OpenRouter runs the search *inside* the model request, so there's no extra client round trip. That makes it potentially the fastest and needs zero code, at about $0.005–0.007 per search. Downside: it bypasses our registry, so there's no logging or permission layer and it can't be turned off per Tool.
- **`web.fetch`** `{url}` → readable page text (Go readability extraction, truncated). It's for "read me that article" and for following up a search result.
- **`hermes.delegate`** `{task}` → starts a Hermes job asynchronously and returns at once with a job id. The agent says something like "I've handed that to Hermes." Permission is probably `act`. Hermes itself gates its outward writes.
- **`hermes.status`** / **`hermes.result`** → the tier 2 agent turns Hermes' long output into a short spoken summary ("what did Hermes change?"). That covers the "summarise Hermes stuff" role.

Routing (extends #10): pending Confirmation → prefixes ("ask the agent…" → tier 2, **"ask Hermes…" → tier 3 directly**) → Control phrase → semantic Fast path → **tier 2 by default**, which may call `hermes.delegate` itself when a task is clearly long or multi-step.

Speed notes:
- Every Tool call costs one extra model round trip. That makes a search answer roughly 2.5–4 s, covered by the Map 1 Filler.
- Keep search results short (snippets only, about 5 results), so input tokens and time to first token stay low.
- Results stream sentence-by-sentence into TTS, as in Map 1.

Questions to grill:
- SearXNG tuning: which engines, worker count and timeout give the best p50 latency and answer quality on about 20 real questions measured on the Pi? Does Kiwix earn its disk?
- When Hermes finishes a job, does the tier 2 agent proactively speak a summary (a model call the user didn't make *right now*, but which comes from a request they did make)? Or does it just chime and wait for "what did Hermes do?"
- Does tier 2 get a "deep think" switch to the alternate model (V4 Pro) for harder questions, or do those go to Hermes?
- Does tier 2 get any memory beyond the 10-turn history (for example, reading Hermes' notes)?

## 6. Hermes agent

Picks up Map 1's "Hermes sidecar" note: serve the Tool registry over MCP (Local-only Tools excluded), run Hermes on OpenCode Go, follow the same Agent path privacy policy.

What it should do:
- Run **on the Pi** as a Compose sidecar with its own integrations (GitHub, web search, calendar/email when they exist, notes, etc.).
- **Call OpenCode** for coding work, either:
  - on the Pi (an `opencode serve` container with repo checkouts), or
  - on the **desktop** over Tailscale (faster, has the real dev environment; needs the desktop on, so maybe a Wake-on-LAN Tool).
- **Self-extension**: "make a Tool that tells me the ISS position" → Hermes → OpenCode in a branch/worktree of clanka-bot using the §1 scaffold → tests pass → **opens a PR**. It never merges or deploys by itself.
- **Issues and PRs on any of the user's repos**: "open an issue on clanka-bot: timers don't survive restart". This could be a plain Tool (`github.issue_create`, `confirm`) instead of Hermes, since it's a leaf action.
- **Async jobs**: long tasks return immediately ("on it"), then report back with a Notification or spoken message when done. Also "what's Hermes working on" and "cancel that".

Research findings (2026-11-11, Nous docs at hermes-agent.nousresearch.com):
- Hermes is a **general autonomous agent**, not a dedicated coding agent. It has `terminal`, `read_file`, `patch` and `execute_code`, so it *can* code, but its own docs ship skills that **delegate coding to external CLIs**: `claude-code` and `codex` are bundled, and `opencode` is an optional install (`hermes skills install official/autonomous-ai-agents/opencode`) that drives `opencode run '…'` / `--format json`.
- **Decision direction: Hermes orchestrates, OpenCode codes.** For the desktop target, run `opencode serve` on the desktop and have Hermes call `opencode run --attach http://<desktop>.<tailnet>:<port> …`. `--attach` is in the skill, but a serve-based workflow isn't documented there, so prototype it. Hermes' `ssh` terminal backend exists, but it moves *all* terminal commands to the remote host, which is too broad.
- Hermes exposes an **OpenAI-compatible API server** (`/v1/chat/completions`, `/v1/runs`, SSE, bearer key, port 8642), so the Go middleware can call it with the same `openai-go` client it already has. It's an **MCP client** (our registry served over MCP plugs in), has `opencode-go` as a built-in provider, ships **arm64 Docker images**, and has gateway **cron**.
- OpenCode Go docs list **Hermes (v0.21.6+) as a validated client**.

Questions to grill:
- **Routing**: which requests go to Hermes and which to the Go loop? Options: an explicit prefix ("ask Hermes…"), a Go-loop Tool `hermes.delegate` that the model calls for long tasks, or Hermes replacing the Go loop entirely.
- **OpenCode Go terms** (Map 1, #15; re-read 2026-11-11). The "processes that run or are activated while you are not logged into the Services" line is item 10 of the general Terms of Use. It sits beside "Maillist, Listserv" and "auto-responder", covers all of OpenCode's hosted services, and "logged in" is undefined. The rules that apply specifically to Go are in the Go docs: "designed for OpenCode and other coding agents", "send typical coding agent traffic", a session ID header, single-member workspace, "own internal use", and "traffic is monitored for abuse". **Decided: Hermes and OpenCode run entirely on OpenCode Go.** User-initiated work through a validated client fits Go's intended use, and quick conversation stays on the Go loop (OpenRouter), so Go traffic is mostly agent- and coding-shaped. Remaining risk is accepted: scheduled Hermes cron jobs that run while nobody is asking are the closest fit to the clause. If those are added, consider giving them their own provider, or ask OpenCode support. The worst case is losing the $10/mo plan and changing a config value.
- Trust boundary: what can Hermes do without Confirmation? Suggestion: anything that writes outside the Pi (PR, issue, message) needs `confirm`, and OpenCode runs in a sandbox with a scoped GitHub token (fine-grained PAT, specific repos, no admin).
- Pi resources: Hermes + OpenCode + builds alongside STT/TTS on a Pi 5. Is the desktop the default for OpenCode?
- Privacy: Hermes sees what the Go loop sees (same history filtering). Does it get its own memory/notes store, and is that Local-only?

## 7. Dashboard (remote web UI)

Reachable from the Pi and the desktop. Proposal: served by the `assistant` binary (or a small sibling service) and exposed on the tailnet only with `tailscale serve`. Tailscale identity doubles as auth, with no public exposure.

Pages (first ideas):
- **Voice and persona**: pick and preview voices.
- **Tools**: enable/disable, Permission override, edit Control phrases and example phrasings, per-Tool test button.
- **Models**: Agent path provider/model/alternate, Hermes model, unavailable message, Filler text.
- **Hermes**: running and recent jobs/sessions, their logs, cancel; open PRs it created.
- **Timers and alarms**: list and cancel.
- **Activity**: recent requests with route (Fast/Agent/Hermes), latency, Tool calls (the structured log line already exists).
- **Health**: status of each Compose service, Bluetooth/Echo Dot connection, location, Spotify auth.
- **Typed requests**: the debug transcript entry as a chat box (it already exists, localhost-only).

Questions to grill:
- **Config model**: `config.yaml` stays the checked-in default. The Dashboard writes an override layer (SQLite or `config.local.yaml`). Which one, and which settings apply live vs need a restart?
- Is this the same web app as the phone **Satellite** PWA from Map 1's list? Building one app with a settings area plus push-to-talk would save effort.
- Frontend stack (Map 1 left React vs Svelte open). Server-rendered Go + htmx is a lighter option for a settings UI.
- **Secrets (wanted):** paste API keys and tokens (Spotify, GitHub PAT, OpenRouter, OpenCode Go, …) into the Dashboard so a new integration never needs SSH. Proposed shape: **write-only** fields that show "set · last 4 chars · updated <date>", with replace and clear but no reveal. Keys are stored in a secrets file/store on the Pi owned by the `assistant` service (not `config.yaml`, not git), and the integration reloads or restarts after a change. OAuth integrations (Spotify) get a "Connect" button that runs the browser flow instead of asking for a pasted token.
  - To grill: where secrets live (a 0600 file on a Docker volume vs SQLite vs encrypted-at-rest with a key in `.env`); how Compose services other than `assistant` (Hermes, OpenCode) receive them; whether `.env` stays the bootstrap for the first key; and whether Tailscale identity is enough auth to *write* secrets, or a Tailscale ACL should limit the Dashboard to your own devices or user.

---

## 8. More Tool ideas (to triage)

Ranked roughly by value for effort. All are leaf operations that fit the registry.

**Quick wins (local, no API)**
- ~~`audio.volume`~~ and `date.today` are already in Map 1 (#22); add "quieter" and mute if #22 doesn't
- `assistant.repeat` ("what did you say?") and `assistant.stop`
- `assistant.help` ("what can you do"), generated from the registry
- `date.today`, `time.in_city` ("what time is it in Tokyo"), `date.countdown` ("how many days until Christmas")
- `calc.evaluate` and `units.convert` (cups to grams, °F to °C): faster and more accurate than the model
- `random.flip` / `random.roll` / `random.pick`
- `system.status`: Pi temperature, uptime, disk, service health ("are you okay?")
- **Do not disturb / quiet hours**: suppress Notifications, lower volume at night

**Personal data (likely Local-only)**
- `list.add` / `list.read` / `list.remove`: shopping list, to-do list
- `note.remember` / `note.recall` ("remember I parked on level 3")
- `reminder.set` (from §2)

**Network**
- `weather.forecast` (today/tomorrow/week, "will it rain later") beyond `weather.current`, and `sun.times` (sunrise/sunset)
- `news.headlines` (RSS, configurable feeds)
- `notify.phone` ("send this to my phone"), which also lays the groundwork for Notifications (ntfy)
- `desktop.wake` (Wake-on-LAN), which pairs with OpenCode-on-desktop
- `github.issue_create` / `github.pr_list` / `github.notifications` (`confirm` for writes)
- Home Assistant bridge (lights, plugs) if smart-home hardware appears; one generic `home.*` adapter rather than per-device Tools

**Composed above the registry (not Tools)**
- **Routines**: "good night" = DND + alarm check + pause music. Tools are leaves, so a routine is a Briefing-like composer, configured in the Dashboard.
- **Briefings** (already in Map 1's backlog): morning = weather + alarms/calendar + news.

## 9. Proposed order

1. **Modular Tools + settings store** (§1, plus the persistence decision). Everything else depends on it.
2. **Quick-win Tools + timers/alarms + voice change.** All local, they prove the module pattern, and they make it useful day-to-day.
3. **`web.search` + `web.fetch`** for the tier 2 agent (small; makes the agent useful for lookups straight away).
4. **Spotify** (needs the audio-routing/ducking decision).
5. **Dashboard v1** (read-mostly: health, activity, timers; then settings and secrets editing).
6. **Hermes sidecar**: MCP adapter, "ask Hermes" prefix, `hermes.delegate`/`status`/`result`, async jobs, GitHub issue/PR.
7. **OpenCode integration + self-extension** (Hermes writes Tools → PR).

## Glossary candidates

New terms to settle with `domain-modeling`: **Timer**, **Alarm**, **Reminder**, **Voice**, **Persona**, **Dashboard**, **Tool module**, **Settings store / override**, **Hermes job**, **Hermes request** (the "ask Hermes" prefix, like Agent request), **Routine**, **Ducking**. Note: the glossary already says the Agent path means the Go loop and lists "Hermes route" under _Avoid_, so the tier 2 / tier 3 split fits the existing terms.

## Cross-cutting open questions

- Persistence choice (SQLite?) and what lives there: timers, settings overrides, lists/notes, Hermes job state.
- Unprompted speech: timers, Hermes completions and Notifications all speak without a request. They need one policy for interrupting replies, music and quiet hours.
- Secrets management grows: Spotify OAuth, GitHub PAT, OpenCode/Hermes keys. Is `.env` still enough?
- Tailscale on the Pi becomes a hard dependency (Dashboard, desktop OpenCode, Satellites).
- **Shared words across domains**: "stop", "pause", "next" and "cancel" now mean different things for timers, Spotify and replies. Today a Control phrase maps to exactly one Tool. Proposal: a small arbiter that sends the phrase to whatever is most active (ringing alarm/timer → current reply → playing music).
- **RAM budget on the Pi 5 (8 GB, confirmed 2026-11-11)**: always-on wake word, STT, TTS, embed, assistant, SearXNG and Hermes. **Decision direction: OpenCode runs on the desktop, not the Pi**, so `desktop.wake` (Wake-on-LAN) becomes a requirement for self-extension, not a nice-to-have. A Pi-local OpenCode is a fallback for tiny jobs only, if it fits. Measure the real total with `docker stats` once SearXNG and Hermes are added.
- **Scope**: one Map 2, phased like Map 1. Phases 1–5 in §9 get concrete enough for tickets; Hermes and OpenCode (phases 6–7) are decided at interface level only (the "ask Hermes" prefix, the `hermes.delegate` contract, OpenCode Go as provider), with detail later in the same map.
