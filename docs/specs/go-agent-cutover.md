# Go Agent path inventory and cutover contract (#526)

Status: **mapping**, 2026-11-22. Hermes is not yet the fallback. Native
blockers are [#523](https://github.com/Jairik/clanka-bot/issues/523) (runtime)
and [#525](https://github.com/Jairik/clanka-bot/issues/525) (Go Tool bridge).
[#522](https://github.com/Jairik/clanka-bot/issues/522) owns semantic matching
and is not a cutover blocker.

This file is the inventory of the current Go Agent path, the stored settings
that a later cutover must keep loading, and the acceptance coverage that must
pass before the live previous path is removed. It does not change routing.

Terms follow [the glossary](../guides/glossary.md). Privacy follows
[ADR-0002](../adr/0002-single-prompt-builder-is-the-only-egress.md).

## Current shape

Go owns Control phrases, semantic Fast path matching with deterministic gates,
Tool execution, Permission levels, Confirmation, audio, local state and
Dashboard APIs. The Agent path is a thin OpenAI-compatible tool-calling loop
inside the assistant. Hermes is already a separate explicit job path
("Ask Hermes"); it is not the fallback.

### Entry points

Every spoken or typed request reaches the same `pipeline.Handle` /
`HandleEvents` seam. The source only labels Activity:

| Source | Owner | Notes |
| --- | --- | --- |
| Voice loop | `internal/voice/loop.go` | After STT. `latency.SourceVoice`. |
| Debug transcript | `internal/api` | Loopback typed entry after STT. |
| Dashboard typed box | `internal/dashboard/activity.go` `runAsk` | `latency.SourceDashboard`. |
| Dashboard mic | `internal/dashboard/mic.go` | STT then `runAsk` with `SourceDashboardMic`. |

A smoke turn (`latency.SourceSmoke`) may inspect a route but must not call the
model, settle a Confirmation, or replace Last reply.

### Routing order

`planRoute` decides without side effects. `route` then executes. Order after
a pending Confirmation:

1. Confirmation yes (Go-owned; never a model path).
2. Explicit Hermes prefix (`ask hermes …`). Owns the prefix even when Hermes
   is disabled, so it cannot fall through to the Agent path.
3. Agent request prefixes (`agent.request_prefixes`: "ask the agent", "agent",
   "hey agent").
4. Morning briefing phrase.
5. Control phrase.
6. Focus intent (`stop` with nothing in Focus suspends; other intents fall
   through).
7. Deferred queue.
8. Chain split.
9. Semantic Fast path.
10. Agent fallback (today: the Go loop). Unmatched utterances and explicit
    reasoning requests land here.

Code: `assistant/internal/pipeline/pipeline.go`.

### Agent path internals

| Piece | Package | Role |
| --- | --- | --- |
| Tool-calling loop | `internal/agent` | Steps, Confirmation propose, ignore token, spoken budget. |
| Prompt builder | `internal/agent/prompt.go` | Sole egress (ADR-0002). Drops Local-only history turns whole. Omits Local-only Tools. Replaces a Local-only Tool result with `{"error":"unavailable"}`. |
| Provider client | `internal/agent/client.go` | One OpenAI-compatible host. Forces `provider.zdr: true` and `provider.data_collection: "deny"` after any configured routing. No failover. |
| Shared history | `internal/agent/history.go` | Fast path and Agent path append the same turns. Quiet window and turn cap. Bounds oversized Tool results. |
| Reply session | `internal/reply` | Agent path Filler, Unavailable Cue, streamed deltas, barge-in cancel. |
| Wording | `internal/wording` | Live prefixes, Filler, Unavailable message, Clarify line. |

### Hermes today

`internal/hermes` runs one explicit model-only job at a time. The pipeline
strips the prefix and does not copy Agent history or Tool results across that
boundary. Dashboard account grants (`hermes.integrations.<id>`) are stored
intent until #525's adapter enforces them. Runtime pinning and slimming are
#523.

## Settings that a cutover must keep loading

Effective value is stored override → `config.yaml` → code default
([ADR-0003](../adr/0003-one-sqlite-store-owned-by-assistant.md)). A removed
Definition leaves its SQLite row in place and does not fail startup. A
tightened validator ignores the stored row (`Ignored()`) instead of refusing
to boot.

### Live SQLite keys (`agent.*`)

These are written from the Dashboard or voice and must keep loading under the
same key, or through an explicit alias, when the Go loop is removed:

| Key | Owner | After cutover |
| --- | --- | --- |
| `agent.request_prefixes` | wording | Keep: prefixes that skip the Fast path and go to the model fallback (Hermes). |
| `agent.filler` | wording | Keep: cue while the fallback waits. |
| `agent.unavailable_message` | wording | Keep: spoken when Hermes cannot be reached. Fast path stays up. |
| `agent.clarify_message` | wording | Keep if Hermes still needs a Clarify line; otherwise leave the row as an ignored orphan. |
| `agent.model` | `agent.ModelDefinition` | Likely unused once Ornith/Hermes owns the model. Must not fail startup if the Definition is dropped. |
| `agent.spoken_max_sentences` | `agent.LimitsDefinitions` | Keep if the reply session still enforces a spoken budget on Hermes output. |
| `agent.spoken_max_chars` | same | Same as sentences. |
| `agent.max_completion_tokens` | same | Provider cost backstop; drop only with an orphan-safe removal. |

Persona selection writes `agent.filler` and `agent.unavailable_message` in the
same transaction. A cutover must not rename those keys without a dual-read.

### YAML-only Agent config (not SQLite)

`assistant/config.yaml` `agent:` block. Not stored as settings rows:

- `agent.provider` / `agent.providers.*` (`base_url`, `api_key_env`, `model`,
  `alternate_model`, `headers`, `routing`)
- `agent.system_prompt`
- `agent.history.turns` / `agent.history.quiet`
- `agent.first_token_timeout` / `agent.request_timeout` / `agent.max_steps`

Routing `zdr` / `data_collection` in YAML cannot loosen the code-forced
privacy fields. Removing the OpenRouter provider block is part of the later
path-removal step, not this mapping.

### Keys this ticket must not rename

| Key | Owner | Why |
| --- | --- | --- |
| `hermes.integrations.<id>` | #525 | Account grants. Intent today; adapter later. |
| `tools.<name>.*` | Tool policy | Enabled / Permission. Go-owned at the registry. |
| `focus.<intent>.phrases` | wording | Fast path / Focus, not the model fallback. |

## Privacy and permission invariants

These stay Go-owned through and after cutover:

- Permission levels (`read` / `act` / `confirm`) and live tighten-only overrides.
- Confirmation: a `confirm` Tool is proposed, never executed, until a spoken
  yes. Sibling read/act calls in the same step may run.
- Local-only Tools are never offered to a model, never served over MCP, and
  their turns are dropped from egress history.
- Authorized provider egress is fail-closed: ZDR and data-collection denial
  are code, not config. If no host qualifies, the Unavailable message plays.
- Secrets stay with the Tool modules. Hermes must not receive linked-account
  values in the prompt.

Eligible history for a later Hermes boundary is the same set the prompt
builder already sends: non-Local-only turns, no Local-only Tool schemas, no
private results.

## Telemetry that must survive

Activity, the structured request line, and Dashboard job status already
record path, Tool names, Local-only provenance, token counts and
`agent_error`. Keep those fields when the fallback becomes Hermes. Do not
remove Tool telemetry that the Dashboard still shows.

## Acceptance (later, blocked)

Pi verification uses the existing clanka-bot stack, shared containers and the
installed Ornith endpoint. No replacement containers. Coordinate live
restarts with the parent.

When #523 and #525 have landed, verify:

1. Deterministic Control phrase still runs with Hermes down.
2. Fallback / "ask the agent" request reaches Hermes with only eligible
   history.
3. Allowed integration (allowlisted, granted) returns the Tool result.
4. Denied / Local-only / confirm request fails closed in Go.
5. Interrupted job cancels cleanly; Go commands still work.
6. Normal short voice turn and a longer job both complete.
7. Settings overrides from the table above still apply after deploy.
8. Deployment rollback still starts the previous images; local Hermes
   configuration survives a normal Pi deploy.

Tests in this change pin the pre-cutover contract: entry points share the
pipeline, prefixes do not leak into the other path, an absent model runner
leaves Fast path working, Confirmation stays Go-owned, and stored `agent.*`
overrides still load if a Definition is removed.
