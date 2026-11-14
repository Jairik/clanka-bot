# Map 2 readiness (review fix pass)

Status: the combined actionable findings from the three independent Map 2
reviews are implemented on branch `jj/map-2-final-review-fixes`. This is **not**
a claim that Map 2 is fully accepted: the external/provider/physical gates below
remain open.

## Implemented in this pass

- **Routing compound safety.** The semantic router still rejects a second Tool
  over threshold with a configured joiner. The pipeline now also inspects each
  clause after a joiner: a whole Control phrase / Focus intent, or a clause that
  starts with a command-leading word from the live Examples, routes the request
  to the Agent path instead of executing only its first half. The rule ignores
  labels, number continuations ("an hour and a half") and schedule continuations
  ("… and end at 7"). Verified repro:
  `set a timer for five minutes and pause` / `… and resume` now route Agent,
  and so does a repeated command
  (`… and set another timer for ten minutes`).
- **Startup semantic recovery.** The router is always attached and the wording
  worker retries a failed Examples build with bounded backoff (2 s doubling to
  2 min). The old vectors stay live, a superseded build is discarded, and a
  shutdown cancels and joins the retry loop. No config edit or restart is
  needed after the embed service returns.
- **Filler retry (S3).** A transient TTS failure retries the same cache key on
  the same bounded schedule instead of waiting for another text/voice change.
  The old audio keeps playing and one Filler still plays per request.
- **Agent first-token budget.** A role-only/empty prologue no longer stops the
  first-token timer; only actual content or a Tool call starts the request
  timer.
- **Reply cancellation outcome.** A Focus "stop" completion is recorded with
  the turn context, so Activity/telemetry reports `cancelled` instead of
  `ok`/`no_audio`, without changing token-safe stop behaviour.
- **Cancel by name reaches active alerts.** Timer `CancelLabel` retains the
  label of a ringing Timer; Reminder `CancelQuery` retains the message of a
  speaking or queued Reminder (including the empty "cancel every reminder"
  query). Each counts and dedupes by id, records history as stopped/cancel, and
  leaves a newer unrelated Ring alone.
- **AgentOnly Control phrases are refused.** `pipeline.New`, `wording.Build`
  and the shipped-control-phrase conformance check reject an AgentOnly target.
  A custom AgentOnly+Speak Tool remains unreachable from the Fast/Control path.
- **Shipped policy defaults.** The code default is now 0.71/0.23, matching
  `config.yaml`; a config that omits the `semantic` block can no longer weaken
  the margin. The `.95` per-Tool floor and the Map 1 baseline are untouched.
- **Shipped Focus phrases and eval negative.** `resume` and `next` are in the
  default Focus table, the cooking question
  `how long is pasta supposed to cook` is a locked-in negative, and the routing
  docs record the measured totals.
- **Debug listener guard (S1).** The debug/API listener now requires a loopback
  `Host`, same-origin Fetch Metadata (a cross-site simple POST is refused), and
  `Content-Type: application/json` on mutation bodies, in addition to the
  existing source-address allowlist. `OPTIONS` is a side-effect-free 204, and
  the documented loopback curl still works.
- **Dashboard Host guard (S2).** The Dashboard requires an explicit
  `dashboard.hosts` entry (or a loopback Host for the local smoke test) before
  identity. A hostile Host is refused even with the owner header and a
  same-origin marker; the Spotify callback on the configured host still works.
  Empty hosts fails closed with a startup instruction.
- **Duplicate slog key.** `voice_name` and `speech_enabled` replace the two
  `voice` keys on the startup line.

## Review disposition summaries

### Standards

All six verified defects (first-token, compound, cancel-by-name, code defaults,
startup embed, stopped-reply outcome) are fixed with regression tests. The four
listed gaps (AgentOnly Control phrase, cooking negative, Filler retry, duplicate
key) are also fixed. B1–B6 remain judgment follow-ups, not this fix pass: the
three schedulers keep their own lifecycle (their snooze/repeat/message rules
differ), with the cancellation inconsistency that motivated B1 removed. S4
(LastReply snapshots the completed pipeline reply, not heard playback) remains a
documented intentional boundary; no playback tracking was invented.

### Spec

F1/P1 (Control-intent compound), F2/P2 (startup embed recovery) and F3/P3
(`resume`/`next`) are fixed. The cooking negative is now in the corpus.
`wake me at 7 with the birds sound` remains the disclosed conservative
Fast-path recall miss; no threshold or floor was relaxed. The spec wording
issues (#62 token wording, #68 notifications, the 12/18-tensor note) were not
edited to claim success. (Later amended: the #68 spec line now states the
two-token contract below.)

### Security

S1 and S2 are fixed with new tests. The previously verified boundaries
(OAuth/PKCE, SSRF, secrets views, Local-only provenance, Confirmation,
sound/voice staging, volume isolation) were not changed. No live browser,
tailnet, provider, Pi, audio, hardware or credential evidence is claimed; a
browser's Private Network Access policy may additionally block the old S1 path,
which does not remove the server-side fix.

## Test evidence

All commands run from `assistant/` unless noted; local MiniLM was
`http://192.168.192.4:8085` (health `{"status":"ok","model":
"sentence-transformers/all-MiniLM-L6-v2","dim":384}`).

| Check | Result |
| --- | --- |
| `go test ./... -count=1 -timeout 20m` | exit 0, every package ok |
| `go vet ./...` | exit 0 |
| `go test -race ./... -count=1 -timeout 30m` | exit 0, no DATA RACE |
| `make test` (repo root) | exit 0; deploy/compose/mic Python suites pass |
| `make check` (repo root) | `check: OK (7 service(s), 0 warning(s))` |
| `docker compose config -q` | exit 0 |
| real-MiniLM routing eval | precision 1.0000, recall 0.6953 (559/804), negatives 0/178, compounds 0/34, Focus 8 delivered/6 fallthrough, floor 0.950 |
| Map 1 baseline gate | tp=96 fp=0 positives=162, precision 1.0000, recall 0.5926, unchanged file |
| `ROUTING_EVAL_EMBED_URL=… go test ./tools/...` | exit 0, every module conformance incl. real Example routing |
| Process smoke (temp DB, debug+dashboard on loopback, TTS/voice off) | health 200; loopback JSON transcript 200; cross-site text/plain 403; hostile Host 403; OPTIONS 204; `/api/settings` GET 200; text/plain PUT 415; JSON PUT 200; dashboard loopback/configured-host 200, hostile Host 403, callback 303 |

The new tests are `internal/httpguard/*_test.go`, `internal/api/security_test.go`,
`internal/dashboard/host_test.go`, `internal/pipeline/clause_test.go`,
`internal/pipeline/agentonly_test.go`, `internal/wording/retry_test.go`,
`internal/wording/agentonly_test.go`,
`internal/agent` role-only-first-token test,
`internal/reply` stop-outcome test, and the timer/reminder active-cancel tests.
The protected Map 1 baseline file and the `.95` floor have an empty diff against
the review pin.

## Open gates (honest partial acceptance)

- **#62** ntfy: topic/token contract is code+fake-HTTP only; a server-issued
  token and live phone delivery remain external.
- **#68** GitHub notifications: decided as a two-token contract. The optional
  `GITHUB_NOTIFICATIONS_TOKEN` (classic PAT, `notifications` scope only) is used
  only by `github.notifications`; `GITHUB_TOKEN` (fine-grained) is never sent to
  the notifications endpoint and there is no fallback. Code + fake-GitHub tests
  assert which token reaches which endpoint. Live acceptance (a real classic
  token against api.github.com) is not done.
- **#69 / #70 / #74** physical/Pi: no RAM, AEC, microphone, speaker or
  music-volume measurement; #70 remains prep only and must not unblock #72.
- **#71** Spotify: no real account pairing, playback or real
  `credentials.json` survival evidence.
- **#72** Spotify playback: code and fake-tested only (httptest Web API, fake
  `pactl`, fake Focus holders). Tools, music Focus, the local Ducker and the
  unset-by-default `music.max_volume` are in; no live Spotify playback, real
  Pulse ducking, speaker or `music.max_volume` value (#70) exists. Ticket stays
  open for live acceptance.
- **#73** playlists/library: code and fake-tested only (httptest Web API,
  fake embedder for the name matcher, injected ticker). The playlist cache
  (startup + 6 h, bounded paging, failed refresh keeps the old list),
  `spotify.play_playlist`/`shuffle`/`repeat`/`previous`/`queue_add`/`like` and
  their routing-eval cases are in. `previous` is a plain Tool, not a Focus
  intent. No live Spotify call, real playlist or spoken run exists; scopes were
  already sufficient, so no reconnect is needed for a grant made with the #71
  scope list. Recorded cost: `spotify.next` 13 to 6 TP and `assistant.repeat`
  14 to 13 TP in the routing eval (see docs/routing-eval.md). Ticket stays open
  for live acceptance.
- **#76** Hermes/OpenCode: spike only; no Hermes/Tailscale install and no valid
  standalone Go key.
- **Live browser/tailnet and final-SHA hosted CI** were not exercised in this
  pass; the local suite is the evidence here.

## Human steps before calling the Dashboard live

1. Set `ASSISTANT_DASHBOARD_OWNER` and `ASSISTANT_DASHBOARD_HOSTS` (the exact
   `tailscale serve` Host, e.g. `pi.tailnet-name.ts.net`) in `.env`, then
   restart the assistant container. An empty hosts list logs the instruction
   and accepts only a loopback Host.
2. Confirm the loopback smoke (`curl -H 'Tailscale-User-Login: …'
   http://127.0.0.1:8082/health`) and then the HTTPS `*.ts.net` URL.
3. Run the Pi hardware/audio and provider gates on real hardware before closing
   the open tickets above. The step-by-step runbook for these gates is
   [docs/runbooks/map-2-acceptance.md](runbooks/map-2-acceptance.md).
