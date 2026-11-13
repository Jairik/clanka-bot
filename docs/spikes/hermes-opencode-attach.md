# Hermes → OpenCode attach spike (#76)

Status: **complete**, 2026-11-13. A real Hermes job on the Pi drove OpenCode
v2.2.7 on the desktop through an authenticated `opencode serve` over Tailscale,
edited a file, ran `pytest`, and returned a summary. This document records the
measured latency, failure modes, auth findings and the version drift that a
production adapter has to handle. It also refreshes the phase 6–7 ticket
proposals; no GitHub issues were created from them.

This spike owns no production routing, MCP server, job store or phase 6/7
feature. The code paths it exercised are the upstream Hermes 0.23.1 agent, the
upstream OpenCode 2.2.7 CLI/server, and a throwaway Git repo on the desktop. The
clanka tree still has no Hermes package, no `jobs` table and no "ask Hermes"
prefix.

## Acceptance

Full acceptance for #76 was a **real Hermes job over Tailscale** that drives
OpenCode on the desktop, runs a small coding task and returns a summary.

- [x] A Hermes job ran a small coding task on the desktop through the attached
      server (`--server`, the v2.2.7 equivalent of `--attach`) and returned a
      summary. Evidence: `add.py` changed from `return 0` to `return a + b`,
      `python3 -m pytest -q` reported `1 passed`, and Hermes returned the
      OpenCode session id plus the exact test output.
- [x] Findings on latency, failure modes and auth — measured below, with
      unmeasured items called out.
- [x] A proposed ticket list for phases 6–7 — refreshed below.
- [ ] A fresh, long-lived OpenCode Go API key — **not** provisioned. The stored
      Go API key is still expired; this spike used the desktop's OpenCode
      Console OAuth session as the Hermes credential (see Auth findings). That
      is a spike workaround, not a production answer.
- [ ] A Tailscale ACL for the OpenCode port — **not** configured or verified.
      The server was bound to the desktop's tailnet address and protected by
      HTTP basic auth only.

## Environment actually used

| Component | Version / detail |
| --- | --- |
| Desktop | Arch Linux, tailnet `100.115.149.100`, Tailscale up |
| Pi | Raspberry Pi 5, Debian 6.12 aarch64, 8 GB RAM, tailnet `100.92.11.81` |
| OpenCode server/client | `opencode v2.2.7` on both ends |
| OpenCode Go model | `opencode-go/deepseek-v4.1-flash#max` |
| Hermes | `Hermes Agent v0.23.1 (2026.9.24)`, Python 3.14.9, installed under `~/.cache/hermes-spike` on the Pi |
| Hermes provider | `opencode-go`, default model `deepseek-v4.1-flash` |
| Transport | `opencode serve --hostname 100.115.149.100 --port 47822` with `OPENCODE_SERVER_PASSWORD` |
| Tailnet RTT | 4 ms `tailscale ping`; 4.6 ms average ICMP (`ping -c 3`) |
| Scratch repo | Desktop `/tmp/opencode/hermes-job`, mirrored as an empty dir on the Pi (see path note) |

The OpenCode server used the desktop's **default** database. That matters: a
scratch `OPENCODE_DB` did not see the Console credential and fell back to the
expired key in `auth.json` (see Auth findings).

The Pi install was temporary: `~/.cache/hermes-spike`, the scratch OpenCode
v2.2.7 build, the `~/.local/bin/hermes*` launchers, the `/tmp` job copies and
the installer's PATH lines in `~/.bashrc` were removed after the run. The
Commands below recreate them. No clanka container, audio device, Bluetooth
service or owner checkout was touched.

## What ran

1. **Desktop server.** `opencode serve` bound to the tailnet address only, with
   a random 0600 password file and `OPENCODE_SERVER_PASSWORD` set. No
   `0.0.0.0`, no LAN address.
2. **Pi client.** OpenCode v2.2.7 arm64 was installed under
   `~/.cache/hermes-spike/bin` because the official install script fetched
   v1.19.5 on the Pi (see Version drift). The Pi client attached to
   `http://100.115.149.100:47822` with the password from a 0600 file.
3. **Hermes on the Pi.** Installed with the upstream POSIX installer into a
   scratch prefix (`--dir`/`--hermes-home` under `~/.cache/hermes-spike`), with
   `--skip-browser --skip-computer-use --non-interactive`. Configured with
   `model.provider=opencode-go` and `model.default=deepseek-v4.1-flash`.
4. **Skill.** The upstream optional `opencode` skill (v1.2.0) documents
   `opencode run --attach` and `--variant`, which v2.2.7 rejects. A local copy
   named `opencode-remote` used the v2 command shape (`--server`, `#max`) and a
   fixed workdir. The model was told to use it and to let OpenCode do the edits.
5. **Job.** Hermes called `opencode run --server ... --format json` as a
   blocking `terminal` tool call. OpenCode read `add.py` and `test_add.py`,
   edited `add.py`, ran `python3 -m pytest -q`, and returned its summary.
   Hermes then summarised the JSONL result.
6. **Verification.** The desktop repo showed the one-line diff and
   `1 passed` on a fresh `pytest` run. The OpenCode session record reported
   `outcome: succeeded`.

## Measured results

All timings are wall-clock on this desktop/Pi pair. "Hermes e2e" is the
`hermes -z` process from exec to exit. "OpenCode leg" is the server-side session
(`Session.Info.time.created` → `idle`). They are not directly comparable: Hermes
adds startup, its own model turns and formatting.

| Case | Result | Measured |
| --- | --- | --- |
| `opencode serve` cold start to authenticated `/api/info` 200 | healthy | about 1 s (4 × 250 ms readiness polls; previous loopback run was 0.45–1.37 s) |
| Pi attach smoke (`--server`, "reply OK") | exit 0, `PI_ATTACH_OK` | **7.10 s** e2e; first→last event **139 ms** |
| Pi direct coding job (no Hermes) | exit 0, edit + `pytest` | **12.96 s** e2e; first→last event **6.28 s**; 4 tools (read, shell, edit, shell) |
| Hermes smoke (model only, no tools) | exit 0, `HERMES_OK` | **21.07 s**; 1 API call; 12,199 input / 6 output tokens |
| Hermes → OpenCode trivial delegation | exit 0, `HERMES_OPENCODE_OK` | **16.38 s**; 2 API calls; 24,798 input / 174 output tokens |
| **Hermes → OpenCode coding job (acceptance)** | exit 0, edit + `1 passed` | **41.53 s** Hermes e2e; **19.87 s** OpenCode leg; 3 Hermes API calls; 43,310 input / 1,124 output tokens |
| Same job, OpenCode-side latency | session record | user message → first assistant message **0.61 s**; → first streamed text **7.25 s**; → session idle **19.64 s** |
| Hermes stream-json trivial delegation | exit 0, `STREAM_OK` | **11.51 s** e2e; first `tool_use` **3.18 s** after the system event; final text **10.27 s** |
| Wrong password, direct client on Pi | exit 1, `rejected the password` | **0.28 s** |
| Wrong password, via Hermes | exit 0 (Hermes reports failure), exact error | **13.44 s**; no task ran, no files changed |
| Server down, direct client on Pi | exit 1, `ECONNREFUSED` | **3.85 s** |
| Server down, via Hermes | exit 0 (Hermes reports failure), diagnosis | **15.68 s**; no retry, no provider switch |
| Unreachable tailnet IP | exit 1, `EHOSTUNREACH` | **1.26 s** |
| Explicit interrupt via API | `{"interrupted":true}`, server healthy | **0.71 s** API call; client exited; no orphan `python`; `slow_spike.py` write aborted |
| Cancel through Hermes | Hermes reports `aborted` / `Session interrupted: user` | exact error reported; no files changed; no orphan client |
| Pi RSS during a Hermes delegation | peak, sampled 1 Hz | Hermes **≈ 511 MB**, OpenCode client **≈ 92 MB** |
| Desktop OpenCode server RSS | idle after jobs | **≈ 395 MB** |
| Real model TTFT (OpenCode) | measured above | **7.25 s** to first streamed text for the coding prompt |
| Pi under full clanka load | **unmeasured** | the stack was not put under scripted wake/STT/TTS load |

The 41.5 s acceptance run is dominated by Hermes startup and its own turns, not
by the edit. The OpenCode leg was ~20 s, of which ~7 s was model/tool planning.
For voice, a finished job Announcement should therefore quote the job summary,
not wait on another model call (as the spec requires).

## Auth findings

### The stored OpenCode Go API key is still expired

`opencode auth list` shows an **OpenCode Go** API key as stored. A direct call
with that key to the Go OpenAI-compatible endpoint returns HTTP 401
`api_key_expired`, before any generation. Re-signing in does not renew that key;
the Console message says to create a new one. This is unchanged from the
partial spike.

### The working credential is the OpenCode Console OAuth session

`opencode auth export` (which prints secrets — never run it in a log or CI)
shows a separate credential with `integrationID: opencode`: an OAuth **Console**
session (`st_…` access token, refreshable, expires 2026-11-23 20:43 local). The
Go inference gateway accepts that token as `Authorization: Bearer …`; a direct
`POST /inference/go/openai/v1/chat/completions` with
`deepseek-v4.1-flash` returned 200.

Hermes's built-in `opencode-go` provider accepts it too when it is supplied as
`OPENCODE_GO_API_KEY`. This is why the spike could run on the requested model
without substituting a provider:

- `model.provider: opencode-go`
- `model.default: deepseek-v4.1-flash`
- `OPENCODE_GO_API_KEY=<Console access token>` in a 0600 `$HERMES_HOME/.env`
- Hermes 0.23.1 sends the required `x-opencode-session` header (upstream fix
  landed in 0.21.6)

**Production still needs a fresh personal or service-account Go API key.** The
Console OAuth token is short-lived, tied to the desktop login, and expires on
2026-11-23. P6-01 must provision a long-lived key without logging it.

### Credential location and `OPENCODE_DB`

In v2.2.7 the Console credential lives in the main OpenCode database, not only
in `auth.json`. Starting the server with a scratch `OPENCODE_DB` made it fall
back to the expired `auth.json` key and fail with `api_key_expired`. For an
isolated server, either use the default database or export/import credentials
explicitly. Do not point a test server at a scratch DB and assume auth follows.

### Server auth and the `OPENCODE_PASSWORD` precedence trap

`opencode serve` in v2.2.7 has **no `--password` flag**. Auth is HTTP basic:

- `OPENCODE_SERVER_PASSWORD` sets the password; `OPENCODE_SERVER_USERNAME`
  overrides the default user `opencode`.
- `/api/*` returns 401 without credentials and 200 with them.
- `/` and `/global/health` serve the web UI shell (200) even without auth;
  `/api/*` is the protected surface. A readiness probe must use `/api/info`
  (or `/api/config`), not `/global/health`.
- With `OPENCODE_SERVER_PASSWORD` set, the server does **not** print the
  password. Without it, it prints a generated password on startup — do not
  capture that log.

Both the server and the CLI client read `OPENCODE_PASSWORD` **before**
`OPENCODE_SERVER_PASSWORD`. An ambient `OPENCODE_PASSWORD` in the harness
environment silently overrode the intended password and produced 401s until it
was unset. A production adapter should explicitly unset `OPENCODE_PASSWORD` and
`OPENCODE_SERVER_USERNAME`, or pass `--password`/`--username` on v1 clients.
This is a real footgun, not a theory: it caused the first server/client attempts
in this spike to fail.

## Version drift

The Pi's official install script (`https://opencode.ai/install`) fetched
**v1.19.5**, while the desktop runs **v2.2.7**. The v1 CLI is the one the
upstream Hermes `opencode` skill documents:

| Area | v1.19.5 (Pi installer) | v2.2.7 (desktop and spike Pi build) |
| --- | --- | --- |
| Attach flag | `--attach <url>` | `--server <url>` |
| Password flag | `--password`, `--username` | only `OPENCODE_SERVER_PASSWORD` / `OPENCODE_SERVER_USERNAME` env |
| Remote directory | `--dir <path on remote server>` | no `--dir`; the client sends its cwd and the server resolves that path |
| Reasoning effort | `--variant <level>` | `#max` model suffix |
| Health | `/global/health` (v1 API) | `/api/info` (v2 API; `/global/health` is the UI shell) |
| Session cancel | client kill | `POST /api/session/{id}/interrupt` |

Consequences for the adapter:

- Pin the OpenCode version on both ends. Do not mix a v1 client with a v2
  server or vice versa.
- Probe `opencode run --help` and `opencode serve --help` for the flags the
  build actually has; fail closed with a clear message if neither `--server`
  nor `--attach` exists.
- On v2, `opencode run --server` from the Pi sends the Pi's cwd. If that path
  does not exist on the desktop, the job cannot run there. This spike mirrored
  the absolute path `/tmp/opencode/hermes-job` on both hosts (the Pi copy was
  empty; the desktop copy was the repo). That is a spike trick, not a
  production design.
- The v2 HTTP API avoids the path mirroring: `POST /api/session` accepts
  `location: {directory: "<absolute path on the server>"}`, and
  `GET /api/location?location[directory]=…` resolves a server-side location.
  A production adapter should create the session through the API and never
  depend on the client's cwd. This was verified with a test session at
  `/tmp/opencode/hermes-job`; the session record carried that directory.
- The CLI `--param directory=…` does not encode the deepObject query; raw
  `location[directory]` is required. Treat the OpenAPI spec as the source of
  truth.

## Failure modes

Measured on this pair; the prior fake-provider results are still valid for the
loopback transport and are not repeated here.

| Failure | Surface | Behaviour | Time |
| --- | --- | --- | --- |
| Server not listening | direct client | exit 1, `ECONNREFUSED`; no retry storm | 3.85 s |
| Server not listening | Hermes | reports "OpenCode's server isn't listening", exact `ECONNREFUSED` event, checks the host is up, makes no retry | 15.68 s |
| Wrong basic-auth password | direct client | exit 1, `Server … rejected the password` | 0.28 s |
| Wrong basic-auth password | Hermes | reports the auth error, says no task ran and no files changed, does not retry | 13.44 s |
| Unreachable tailnet address | direct client | exit 1, `EHOSTUNREACH` | 1.26 s |
| Explicit interrupt | API + client | `{"interrupted":true}`; client exits; tool write aborted; server stays healthy | 0.71 s API |
| Interrupt seen by Hermes | Hermes | reports `type: "error"`, `type: "aborted"`, "Session interrupted: user"; no retry | — |
| Expired provider key | OpenCode | `provider.auth` 401 `api_key_expired`; OpenCode retries once then fails | 4.6 s (prior loopback run) |
| Port already in use | `opencode serve` | exits with "Failed to start server. Is port … in use?" | — |
| Server password missing from env | `opencode serve` | generates and prints a password; not suitable for automation | — |

Notes:

- An interrupt aborts the in-flight tool. In the cancel test the `write` tool
  was interrupted before execution, so `slow_spike.py` was never created. A
  tool that had already completed would leave its file change behind; the
  adapter must report the partial state rather than assume rollback.
- After the interrupt the server was healthy (200 on `/api/info`), no
  `opencode run` client remained, and no `python3 slow_spike.py` process
  remained.
- Hermes's one-shot output is not streamed to stdout by `-z`; use
  `hermes chat --oneshot --format stream-json` when per-event timing is needed.
  `-z` is still the cleanest "only the final answer" interface.

## Delegate / status / result / cancel semantics

This spike did **not** use clanka's `hermes.delegate` / `hermes.status` /
`hermes.result` Tools — they do not exist. At the Hermes layer:

- **Delegate** is a `terminal` tool call that runs `opencode run --server …`.
  It is synchronous: Hermes blocks until OpenCode exits.
- **Status** is whatever the terminal tool exposes while the command runs
  (process poll/log in interactive mode). There is no durable status in clanka
  and no job id outside the OpenCode session id in the JSONL.
- **Result** is the JSONL text plus the final `step_finish`; Hermes summarises
  it in its final answer.
- **Cancel** is either the Hermes terminal timeout or an explicit
  `POST /api/session/{id}/interrupt`. The latter is clean and fast; the former
  depends on the client being killed and the server noticing.

For the spec's "pending Confirmation → prefixes → … → Agent path" ordering and
the one-line Announcement, a production adapter needs an asynchronous job
runner and the `jobs` table. The synchronous terminal call cannot answer
`hermes.status` while the job is running, and it cannot survive a Hermes
restart. This is the main gap between the spike and P6-03/P6-04.

## Fixed-contract check against the current tree

| Contract clause | State | Evidence |
| --- | --- | --- |
| "Ask Hermes" prefix | **not implemented** | Only the Agent prefix exists: `agent.request_prefixes` defaults to `ask the agent`, `agent`, `hey agent` (`assistant/config.yaml:380-383`, `assistant/internal/config/config.go:321`). No Hermes branch. |
| `hermes.delegate` / `status` / `result` + cancel | **not implemented** | No `hermes` package, tool or handler in `assistant/`. |
| Job state in the `assistant` database | **not implemented** | `assistant/internal/store/store.go` creates `settings`, `secrets`, `timers`, `sounds`, `alert_events`, `voices`, `alarms`, `reminders`, `personas`; no `jobs` table. ADR-0003 says Hermes job state belongs there. |
| MCP surface = registry minus Local-only and `confirm` | **not implemented, seam exists** | No MCP server in the tree; `tool.Info` carries `LocalOnly` and `Permission`, so the filter is a bounded addition. `github.issue_create` is `PermissionConfirm`. |
| Draft PRs/issues only, no merge/deploy | **partial** | `github.issue_create` is a `confirm` Tool; there is no PR-create or draft-PR path. Nothing merges or deploys. |
| OpenCode Go provider | **usable via Console OAuth, not via the stored key** | The stored Go API key is expired; the Console OAuth session works and Hermes used it. A fresh long-lived key is still required. |
| Real Hermes job over Tailscale | **met** | 41.5 s acceptance run; edit + `1 passed`; Hermes summary. |

## Proposed ticket graph for phases 6–7 (refreshed)

These are proposals, not created issues. `#76` is the completed-evidence spike.
`#71` (Spotify) and `#75` (Activity) are active and are not dependencies or
write scopes for this graph. Keep the contract fixed: "ask Hermes" prefix,
`hermes.delegate`/`status`/`result` plus cancel, job state in the `assistant`
database, MCP surface = registry minus Local-only and `confirm` Tools, draft
PRs/issues only.

### Phase 6 — Hermes

| ID | Ticket | Depends on | Acceptance sketch |
| --- | --- | --- | --- |
| P6-01 | Desktop Hermes/OpenCode/Tailscale runtime | #76 | Pin **the same OpenCode major/minor on both ends** (v2.2.7 or newer); install the arm64 release, not the v1 npm latest; a private `opencode serve` on the tailnet address with `OPENCODE_SERVER_PASSWORD`; readiness on `/api/info`; a Tailscale ACL limited to the owner; a fresh long-lived OpenCode Go key provisioned without logging it; document the `OPENCODE_PASSWORD` precedence trap. |
| P6-02 | Hermes attach adapter | P6-01 | Prefer the v2 HTTP API (`POST /api/session` with `location.directory`) over `opencode run` so the workspace is server-side; if the CLI is used, require matching absolute paths on both hosts. Unset `OPENCODE_PASSWORD`; parse JSONL; treat `provider.auth` and `rejected the password` as terminal; use `POST /api/session/{id}/interrupt` for cancel; probe `--server` vs `--attach` and fail closed. |
| P6-03 | Hermes job store in the assistant DB | P6-02 | Migration for a `jobs` table (id, task, state, timestamps, summary, error, cancel request, artifact refs, OpenCode session id, cost/token fields); single-writer through `assistant`; bounded retention; tests for transitions, restart recovery and partial-write reporting. |
| P6-04 | `hermes.delegate` / `status` / `result` + cancel and "ask Hermes" | P6-03 | Registry Tools with Permission/LocalOnly declarations; prefix routing before the semantic Fast path; `delegate` returns a job id immediately; `status`/`result` read the store; cancel interrupts the OpenCode session and records the state; finish/fail Announcement uses the job summary with no extra model call. |
| P6-05 | MCP surface minus Local-only and `confirm` | P6-03 | Serve the registry over MCP with an auth boundary; filter `Info.LocalOnly` and `Info.Permission == confirm`; never expose a Local-only Tool or its known data; conformance tests prove `github.issue_create` is absent. |
| P6-06 | Outward-write policy and scoped GitHub token | P6-04, P6-05 | `hermes.delegate` is `act` for read-only/research; outward writes require `confirm` at delegation; a fine-grained PAT limited to listed repos; only branch pushes and draft PRs/issues; no merge, deploy or public post; summary names every write. |
| P6-07 | Hermes status and cancel in the Dashboard | P6-03, P6-04 | A Hermes page/status card reads jobs from the assistant API; shows state, summary, timestamps, OpenCode session id and cost/tokens; cancel is an explicit confirmed action; no direct DB access. |
| P6-08 | Hermes E2E on desktop + Tailscale | P6-01–P6-06 | One real "ask Hermes" coding job over Tailscale runs on the desktop and returns a summary; one bad-auth case and one timeout/cancel case are demonstrated; measured latency and no orphan processes; a fresh Go key, not a Console session. |
| P6-09 | Pi/Hermes RAM budget | P6-01, P6-04 | `docker stats` under a scripted wake → STT → agent → TTS load with Hermes/OpenCode; stop if peak RAM is above the agreed 6.5 GB Map 2 limit; record the exact images. This spike saw ≈ 511 MB Hermes + ≈ 92 MB OpenCode client on an otherwise idle Pi, so the headroom is real but not yet measured under load. |

### Phase 7 — OpenCode self-extension

| ID | Ticket | Depends on | Acceptance sketch |
| --- | --- | --- | --- |
| P7-01 | OpenCode workspace/checkout lifecycle | P6-02 | A server-side absolute directory per job (created through the API `location`, not the client's cwd); prepared worktree or isolated checkout; branch naming; no writes to the original dirty checkout; cleanup on success/failure; scoped filesystem and path rules. |
| P7-02 | Scaffold-guided Tool task | P7-01 | OpenCode follows `docs/agents/adding-a-tool.md` and `make new-tool`; generated module passes the conformance suite; new routing-eval cases are added; the task is bounded to one module plus registration/tests. |
| P7-03 | Draft PR/issue flow | P6-06, P7-01 | OpenCode pushes only its branch; the adapter opens a **draft** PR or issue with a scoped PAT; the summary lists the writes; nothing merges, deploys or posts publicly; a failed test blocks the draft PR. |
| P7-04 | Self-extension E2E | P7-01–P7-03 | "Make a Tool that …" → Hermes → OpenCode in a worktree → tests/conformance pass → draft PR link returned and announced; one failure path (test failure or cancel) leaves no partial branch. |
| P7-05 | OpenCode sandbox and secret boundary | P7-01 | No host secrets or SSH agent in the workspace; egress limited to the provider and GitHub; command/permission allowlist; token scopes and audit log reviewed. |
| P7-06 | Observability, cost and notifications | P6-04, P7-03 | Per-job timings, token/cost fields where available (OpenCode exposes session cost/tokens), bounded logs without prompts/secrets, finish/fail Announcement, and a Notification for long jobs. |

Dependency shape:

```text
# Draft graph; arrows mean "depends on".
#76
 └─ P6-01 ─ P6-02 ─ P6-03 ─ P6-04 ─ P6-06 ─ P6-08
                     │        │
                     ├─ P6-05 ┘
                     └─ P6-07
 P6-01 ─ P6-09
 P6-02 ─ P7-01 ─ P7-02 ─ P7-03 ─ P7-04
          │                 │
          └─ P7-05          └─ P7-06
```

## Operational notes

- Bind `opencode serve` to the tailnet address only, never `0.0.0.0` or a LAN
  address. v2.2.7 protects `/api/*` with basic auth but serves the web UI shell
  unauthenticated; the Tailscale ACL is still required (and was not tested).
- Use a unique port and a readiness check on `/api/info`; `/global/health` is
  not a health endpoint in v2.
- Keep the password in a 0600 file or an environment variable, never in a
  command line that lands in shell history or process listings, and never in a
  log. `opencode serve` prints a generated password when none is supplied.
- Do not use `opencode auth export` in CI or a log; it prints secrets.
- Treat `provider.auth` 401 `api_key_expired` and a rejected server password as
  terminal. Do not retry more than OpenCode already does, and do not fall back
  to another provider or model.
- Cancel means interrupting the session and then verifying that the client
  exited, the server is healthy, and no child process or partial file was left
  behind. Report partial writes.
- Job state belongs in the `assistant` database (ADR-0003); the Dashboard and
  Hermes must reach it through the assistant API, never by opening the SQLite
  file.
- Do not ship the fake provider, the v1 skill text or this scratch harness as
  production code. The harness proves the transport boundary and the failure
  taxonomy only.

## Repeatable commands

The checked-in fake-provider harness is unchanged and self-contained:

```sh
# Fake-provider success, auth, timeout, cancel and unreachable cases.
bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh

# Keep the per-case outputs and the generated config for inspection.
KEEP=1 bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
```

The real run used this shape (server and client on different hosts):

```sh
# Desktop, tailnet address only. Password is generated once into a 0600 file.
umask 077
openssl rand -hex 16 > ~/.cache/hermes-spike/server.pass
env -u OPENCODE_PASSWORD -u OPENCODE_CONFIG_CONTENT \
  OPENCODE_SERVER_PASSWORD="$(cat ~/.cache/hermes-spike/server.pass)" \
  opencode serve --hostname "$(tailscale ip -4)" --port 47822

# Readiness (no password is printed).
curl -fsS -u "opencode:$(cat ~/.cache/hermes-spike/server.pass)" \
  "http://$(tailscale ip -4):47822/api/info"

# Pi, OpenCode v2.2.7 arm64. The workdir must exist at the same absolute path
# on the desktop when using the CLI.
OPENCODE_SERVER_PASSWORD="$(cat ~/.cache/hermes-spike/server.pass)" \
  ~/.cache/hermes-spike/bin/opencode run \
  --server http://100.115.149.100:47822 --auto \
  -m 'opencode-go/deepseek-v4.1-flash#max' --format json \
  'Reply with exactly: PI_ATTACH_OK'

# Hermes on the Pi, with the opencode-remote skill preloaded.
HERMES_HOME="$HOME/.cache/hermes-spike/home" \
  ~/.local/bin/hermes -z 'Use the preloaded opencode-remote skill to …' \
  --skills opencode-remote --yolo

# Cancel an active OpenCode session (v2 API).
curl -fsS -u "opencode:$(cat ~/.cache/hermes-spike/server.pass)" \
  -X POST "http://100.115.149.100:47822/api/session/<session-id>/interrupt" \
  -H 'content-type: application/json' -d '{}'
```

The Hermes skill that was used is the upstream
`optional-skills/autonomous-ai-agents/opencode/SKILL.md` (v1.2.0), with the v2
command shape and a fixed workdir; it is not checked in as production code.

Repo checks used for this spike:

```sh
cd assistant && go test ./...
bash -n scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
python3 -m py_compile scripts/spikes/hermes-opencode-attach/fake_openai_server.py
bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
```

## Limits of this evidence

- One desktop and one Pi, both on the same LAN/tailnet; no WAN or mobile
  network path was measured.
- The server was protected by basic auth but no Tailscale ACL was configured or
  verified. The web UI shell was reachable without auth; only `/api/*` was
  protected.
- Hermes used the desktop's Console OAuth session, not a fresh long-lived Go
  API key. The token expires 2026-11-23.
- The Pi ran Hermes and the OpenCode client on an otherwise idle machine. No
  `docker stats`, no scripted wake/STT/TTS load, and no audio work were done.
- The acceptance job was a three-line Python file and one test. It proves the
  transport, auth, edit, test and summary path, not large-repo or long-running
  behaviour.
- The CLI path needs matching absolute paths on both hosts. The API path
  (`POST /api/session` with `location.directory`) was verified separately but
  not used for the acceptance job.
- The fake-provider timings remain transport-only; do not read them as model or
  Pi latency.
