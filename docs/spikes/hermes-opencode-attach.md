# Hermes → OpenCode attach spike (#76)

Status: **partial**, 2026-11-12. The attach transport, auth failure classes,
timeout/cancel behaviour and a fake-provider coding path were exercised on this
desktop. The full acceptance case — a real Hermes job over Tailscale driving
`opencode` on the desktop and returning a coding summary — was **not** run:
this machine has no Hermes install, no Tailscale CLI/daemon/interface, and the
stored OpenCode Go key is expired. Details and the missing dependency are below.

This spike owns no production routing, MCP server, job store or phase 6/7
feature. It adds one harness, one fake provider and this write-up. The proposed
ticket graph is a draft; no GitHub issues were created.

## What was tested

- Installed client: `opencode v2.2.7` at `/home/jj/.opencode/bin/opencode`.
- `opencode serve` on `127.0.0.1` with a unique port, a private data DB and a
  project-local config. No global OpenCode settings were changed.
- `opencode run --server http://127.0.0.1:<port>` against that server. This is
  the v2.2.7 equivalent of the ticket's `opencode run --attach`: `--attach` is
  not a flag in this build (see the version drift below).
- A prepared scratch workspace with a tiny Go module whose `Add` is a stub and
  whose test fails (`Add(2,3) = 0, want 5`). The real coding prompt was sent to
  the required model; it failed at auth before any code change.
- Fake-provider cases for success, wrong bearer, missing bearer, timeout,
  interrupt and an unreachable server. The fake provider is an HTTP stub, not
  a model.

Read-only environment probes (no secret values were read or printed):

| Probe | Result |
| --- | --- |
| `opencode --version` | `opencode v2.2.7` |
| `opencode auth list` | `OpenCode Go  API key  stored` (other providers also stored) |
| `opencode models` | `opencode-go/deepseek-v4.1-flash` is listed |
| Real model call | `401 provider.auth`, code `api_key_expired` |
| `command -v tailscale` | absent |
| `tailscaled` / `/var/run/tailscale/tailscaled.sock` | inactive / absent |
| Tailscale interface | absent |
| `command -v hermes` | absent |
| Hermes container | absent |
| Hermes code in repo | absent (only two announcement test strings) |

## Version drift: `--attach` is not in this build

The ticket and the Hermes `opencode` skill say `opencode run --attach
http://host:port`. In v2.2.7 that flag is rejected:

```sh
# Expected in the ticket/skill, but rejected by this build.
opencode run --attach http://127.0.0.1:47653 -m 'opencode-go/deepseek-v4.1-flash#max' 'hi'
# ERROR: Unrecognized flag: --attach in command opencode run
```

The v2.2.7 CLI exposes `--server` ("Connect to a server URL instead of the
background service") and `--standalone` ("Run with a private server"). The
spike uses `--server`; a production adapter must probe the installed version
rather than assume `--attach` exists.

## Fixed-contract check against the current tree

| Contract clause | State | Evidence |
| --- | --- | --- |
| "Ask Hermes" prefix | **not implemented** | Only the Agent prefix exists: `agent.request_prefixes` defaults to `ask the agent`, `agent`, `hey agent` (`assistant/config.yaml:380-383`, `assistant/internal/config/config.go:321`). No Hermes branch. |
| `hermes.delegate` / `status` / `result` + cancel | **not implemented** | No `hermes` package, tool or handler in `assistant/`. |
| Job state in the `assistant` database | **not implemented** | `assistant/internal/store/store.go` creates `settings`, `secrets`, `timers`, `sounds`, `alert_events`, `voices`, `alarms`, `reminders`, `personas`; no `jobs` table. ADR-0003 says Hermes job state belongs there. |
| MCP surface = registry minus Local-only and `confirm` | **not implemented, seam exists** | No MCP server in the tree (`rg -i mcp assistant` finds only a comment in `assistant/tools/assistant/tools.go:91`). `tool.Info` carries `LocalOnly` and `Permission`, so the filter is a bounded addition. `github.issue_create` is `PermissionConfirm` (`assistant/tools/github/github.go:319-322`). |
| Draft PRs/issues only, no merge/deploy | **partial** | `github.issue_create` is a `confirm` Tool and can create issues; there is no PR-create or draft-PR path. Nothing merges or deploys. |
| OpenCode Go provider | **configured, not usable** | The model is listed, but the stored key is expired (`api_key_expired`); one real call was attempted and failed before any file changed. |

## Measured results

All numbers are wall-clock on this desktop, loopback only, with the local fake
provider unless the row says otherwise. They are **not** Pi, Tailscale or real
model latency.

| Case | Result | Measured |
| --- | --- | --- |
| `opencode serve` cold start | healthy HTTP 200 | 0.45–1.37 s across 6 valid samples (0.45, 0.46, 0.48, 0.82, 1.10, 1.37 s); one later attempt was invalid because the port was already held |
| Fake attach round trip | exit 0, stub summary returned | 0.87–6.82 s per fresh session; includes OpenCode session setup and 8 streamed chunks, not model time |
| Real model, short reply | exit 1, `provider.auth` 401 `api_key_expired` | 1.41 s (retry attempt 2 included) |
| Real model, coding prompt | exit 1, same 401; `add.go` hash unchanged; `go test` still fails | 4.94 s |
| Wrong bearer (fake) | exit 1, `provider.auth` 401 `bad spike token` | 3.86 s |
| Missing bearer (fake) | exit 1, `provider.auth` 401 `bad spike token` | 3.27 s |
| Client timeout (fake slow stream) | exit 124; server still healthy | 6.03 s with a 6 s limit |
| Client interrupt (fake slow stream) | exit 124; server still healthy | 5.03 s with a 5 s limit |
| Unreachable server | exit 1, `ECONNREFUSED` | 75–480 ms |
| Real model TTFT / tokens/s | **unmeasured** | auth blocked before generation |
| Hermes job latency / Pi RAM | **unmeasured** | no Hermes and no Pi in this run |
| Tailscale RTT | **unmeasured** | no Tailscale on this machine |

The fake-provider success is a transport proof only. It must not be reported as
a Hermes job, a model latency measurement or a completed coding task.

## Failure modes

- **Expired auth**: `provider.auth` 401 with `api_key_expired`; OpenCode retries
  once (`attempt: 2`) and then fails. A Hermes adapter should treat this as a
  terminal configuration error, not a transient retry. Surface "OpenCode Go key
  expired; create a new key" and stop the job.
- **Missing/wrong bearer**: `provider.auth` 401. The fake provider returns a
  generic `bad spike token`; a real provider returns its own auth error. Do not
  log the token or the request body.
- **Provider not active**: with an empty data directory the model is not
  routable at all (`provider.no-route`, "Model unavailable"). This is a
  different failure from a 401 and should map to "provider not configured".
- **Unreachable attach server**: immediate `unknown` error with
  `ECONNREFUSED`; no retry storm observed.
- **Timeout / cancel**: killing the `opencode run` client (`TERM` or `INT`)
  leaves `opencode serve` healthy; the fake upstream sees a connection reset.
  There is no explicit CLI cancel verb in v2.2.7; cancellation is the client
  process ending. A Hermes adapter must also stop the child and verify the
  server's health rather than assume the job stopped.
- **Port already in use**: `opencode serve` exits with `Failed to start server.
  Is port <n> in use?` (observed on a shared desktop). The adapter needs a
  unique port, a readiness check and a bounded retry.
- **No server auth on loopback**: `GET /` returns 200 with no credentials. That
  is acceptable only on `127.0.0.1`; never bind `0.0.0.0` or an unauthenticated
  LAN/tailnet address. If Tailscale is used, an ACL plus a private address is
  required.

## Auth finding and missing dependency

`opencode auth list` shows a stored **OpenCode Go** API key, and
`opencode models` lists `opencode-go/deepseek-v4.1-flash`. The key itself is
expired. The provider's error is explicit:

```text
# The real call fails before generation; no secret is printed.
provider.auth 401 api_key_expired
This API key has expired. Create a new API key in OpenCode Console and replace it...
Signing in again will not renew this key.
```

Missing dependency for the parent: **a fresh OpenCode Go API key in the normal
OpenCode auth store**. This spike does not request, print, copy or replace the
key, and it does not fall back to another provider or model. The required model
for every invocation is `opencode-go/deepseek-v4.1-flash#max`.

## Partial acceptance criterion

Full acceptance for #76 requires a **real Hermes job over Tailscale** that
drives `opencode` on the desktop, runs a small coding task and returns a
summary.

- [ ] A Hermes job runs a small coding task on the desktop through `--attach`
      and returns a summary — **not met**. No Hermes, no Tailscale, expired key.
- [x] Findings on latency, failure modes and auth — **met** (measured vs
      unmeasured separated above).
- [x] A proposed ticket list for phases 6–7 — **met** (draft below).

This is a **partial** result. The transport/failure evidence is real for this
desktop and the fake provider; the Hermes/Tailscale/model part is not. Do not
mark #76 complete on the strength of the fake-provider smoke alone.

## Repeatable commands

The checked-in harness is self-contained. It uses a temporary workspace, a
fake provider and a loopback `opencode serve`; it does not create a Git
worktree, change global settings, or print a secret. Fake cases keep OpenCode
config, data, state and cache under that temporary directory via the XDG
variables; `RUN_REAL=1` deliberately uses the normal auth store.

```sh
# Fake-provider success, auth, timeout, cancel and unreachable cases.
bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh

# Keep the per-case outputs and the generated config for inspection.
KEEP=1 bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh

# Optional authorised real coding run; currently fails on the expired key.
RUN_REAL=1 bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
```

Manual server/attach shape, using a unique loopback port:

```sh
# Make a scratch workspace so the server has a private project config and state.
mkdir -p /tmp/opencode/hermes-attach-manual
cd /tmp/opencode/hermes-attach-manual

# Start a private server; keep the log and PID in the scratch directory.
OPENCODE_DB="$PWD/state.db" \
  opencode serve --hostname 127.0.0.1 --port 47653 > serve.log 2>&1 &
echo $! > serve.pid

# Use --server; the ticket's --attach flag is rejected by v2.2.7.
opencode run --server http://127.0.0.1:47653 \
  -m 'opencode-go/deepseek-v4.1-flash#max' --format json 'Reply OK.'

# Stop only the server this shell started.
kill "$(cat serve.pid)"
```

Read-only environment checks (no values are printed):

```sh
# Client and model availability.
opencode --version
opencode models | grep '^opencode-go/deepseek'

# Provider names only; this command does not print secret values.
opencode auth list

# Hermes/Tailscale presence; expected absent on this desktop.
command -v tailscale || echo 'no tailscale CLI'
command -v hermes || echo 'no hermes CLI'
```

Repo checks used for this spike:

```sh
# The spike does not change Go code, but the assistant suite must stay green.
cd assistant && go test ./...

# Static checks for the harness itself.
bash -n scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
python3 -m py_compile scripts/spikes/hermes-opencode-attach/fake_openai_server.py

# End-to-end harness smoke; exits 0 when all fake-provider assertions pass.
bash scripts/spikes/hermes-opencode-attach/run-attach-smoke.sh
```

## Proposed ticket graph for phases 6–7

These are proposals, not created issues. `#76` is the completed-evidence
spike; `#71` (Spotify) and `#75` (Activity) are active and are not dependencies
or write scopes for this graph. Keep the contract fixed: "ask Hermes" prefix,
`hermes.delegate`/`status`/`result` plus cancel, job state in the `assistant`
database, MCP surface = registry minus Local-only and `confirm` Tools, draft
PRs/issues only.

### Phase 6 — Hermes

| ID | Ticket | Depends on | Acceptance sketch |
| --- | --- | --- | --- |
| P6-01 | Desktop Hermes/OpenCode/Tailscale runtime | #76 | Pinned OpenCode and Hermes versions; a private `opencode serve` on a unique loopback port; Tailscale ACL for the desktop port; a documented health probe; a fresh OpenCode Go key is provisioned without logging it. |
| P6-02 | Hermes attach adapter | P6-01 | `opencode serve` lifecycle (start, readiness, stop, cleanup); `opencode run --server` client; JSON event parsing; version probe for `--attach` vs `--server`; timeout, cancel, one-shot auth retry policy; no fallback provider/model. |
| P6-03 | Hermes job store in the assistant DB | P6-02 | Migration for a `jobs` table (id, task, state, timestamps, summary, error, cancel request, artifact refs); single-writer through `assistant`; bounded retention; tests for transitions and restart recovery. |
| P6-04 | `hermes.delegate` / `status` / `result` + cancel and "ask Hermes" | P6-03 | Registry Tools with Permission/ LocalOnly declarations; prefix routing before the semantic Fast path; `delegate` returns a job id immediately; `status`/`result` read the store; cancel stops the child and records the state; finish/fail Announcement uses the job summary with no extra model call. |
| P6-05 | MCP surface minus Local-only and `confirm` | P6-03 | Serve the registry over MCP with an auth boundary; filter `Info.LocalOnly` and `Info.Permission == confirm`; never expose a Local-only Tool or its known data; conformance tests prove `github.issue_create` is absent. |
| P6-06 | Outward-write policy and scoped GitHub token | P6-04, P6-05 | `hermes.delegate` is `act` for read-only/research; outward writes require `confirm` at delegation; a fine-grained PAT limited to listed repos; only branch pushes and draft PRs/issues; no merge, deploy or public post; summary names every write. |
| P6-07 | Hermes status and cancel in the Dashboard | P6-03, P6-04 | A Hermes page/status card reads jobs from the assistant API; shows state, summary and timestamps; cancel is an explicit confirmed action; no direct DB access. |
| P6-08 | Hermes E2E on desktop + Tailscale | P6-01–P6-06 | One real "ask Hermes" coding job over Tailscale runs on the desktop and returns a summary; one bad-auth case and one timeout/cancel case are demonstrated; measured latency and no orphan processes. |
| P6-09 | Pi/Hermes RAM budget | P6-01, P6-04 | `docker stats` under a scripted wake → STT → agent → TTS load with Hermes/OpenCode; stop if peak RAM is above the agreed 6.5 GB Map 2 limit; record the exact images. |

### Phase 7 — OpenCode self-extension

| ID | Ticket | Depends on | Acceptance sketch |
| --- | --- | --- | --- |
| P7-01 | OpenCode worktree/checkout lifecycle | P6-02 | Prepared worktree or isolated checkout per job; branch naming; no writes to the original dirty checkout; cleanup on success/failure; scoped filesystem and path rules. |
| P7-02 | Scaffold-guided Tool task | P7-01 | OpenCode follows `docs/agents/adding-a-tool.md` and `make new-tool`; generated module passes the conformance suite; new routing-eval cases are added; the task is bounded to one module plus registration/tests. |
| P7-03 | Draft PR/issue flow | P6-06, P7-01 | OpenCode pushes only its branch; the adapter opens a **draft** PR or issue with a scoped PAT; the summary lists the writes; nothing merges, deploys or posts publicly; a failed test blocks the draft PR. |
| P7-04 | Self-extension E2E | P7-01–P7-03 | "Make a Tool that …" → Hermes → OpenCode in a worktree → tests/conformance pass → draft PR link returned and announced; one failure path (test failure or cancel) leaves no partial branch. |
| P7-05 | OpenCode sandbox and secret boundary | P7-01 | No host secrets or SSH agent in the workspace; egress limited to the provider and GitHub; command/permission allowlist; token scopes and audit log reviewed. |
| P7-06 | Observability, cost and notifications | P6-04, P7-03 | Per-job timings, token/cost fields where available, bounded logs without prompts/secrets, finish/fail Announcement, and a Notification for long jobs. |

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

## Operational notes for the implementation tickets

- Keep `opencode serve` on `127.0.0.1` with a unique port and a private state
  DB/config. There is no built-in auth on that listener; do not expose it to a
  LAN or an unauthenticated tailnet address.
- The adapter must tolerate the version drift: probe `opencode run --help` for
  `--server`/`--attach`, and fail closed with a clear message if neither is
  available.
- Treat `provider.auth` 401 `api_key_expired` as terminal. Do not retry more
  than OpenCode already does, and do not choose another provider or model.
- Cancel means stopping the attach client and confirming the server health and
  child cleanup. The fake-provider run showed a connection reset, not a
  server-side job cancellation API.
- Job state belongs in the `assistant` database (ADR-0003); the Dashboard and
  Hermes must reach it through the assistant API, never by opening the SQLite
  file.
- The MCP surface must be computed from `tool.Info` (`LocalOnly`, `Permission`)
  at registration/serving time so a Local-only or `confirm` Tool cannot be
  added later without being filtered.
- Do not ship the fake provider or the harness as production code. The harness
  proves the transport boundary and the failure taxonomy only.

## Limits of this evidence

- No real model call completed; every "real" row is an auth failure.
- No Hermes process, container or Tailscale path existed.
- No Pi, no `docker stats`, no RAM or CPU measurement.
- No real coding task was executed by a model; the only code change attempted
  was the stub workspace, and it stayed unchanged after the 401.
- The fake provider's timings measure OpenCode session/attach overhead plus a
  scripted stream, not inference latency.
- One desktop, warm and cold caches vary; treat the ranges as order-of-magnitude
  planning inputs, not service-level numbers.
