# Hermes setup and control panel

Hermes runs explicit jobs on the Pi with an optional read-only tool catalogue. The Dashboard shows whether
the runtime is configured, which Tools the Go adapter currently allows, and the accounts
you can link today. Unmatched voice and typed requests still use the Go Agent path until
[the cutover](specs/go-agent-cutover.md) ([#526](https://github.com/Jairik/clanka-bot/issues/526)).

## Pi setup

Read the private machine notes before connecting to the Pi. On the Pi, add
`hermes` to the existing comma-separated `COMPOSE_PROFILES` setting and set
`HERMES_RUNTIME_TOKEN` in the private `.env` to a generated random value of at
least 32 characters. Keep any existing profiles. Deploy normally so Compose
recreates the assistant with the token and starts the private sidecar.

Use secure entry or Dashboard **Secrets** to store a separately authorized Pi
key as `HERMES_OPENCODE_GO_API_KEY`. The selected model, its dedicated key and
the explicit task go to the sidecar over the authenticated private Compose
network. The assistant sends no history, Tool results or linked-account secrets.
Hermes starts with zero enabled tools by default. The sidecar publishes no host
port and keeps its per-job runtime files in temporary storage. The image pins Hermes Agent
**v0.23.3** (`nousresearch/hermes-agent:v0.23.3@sha256:3e9fe4c6ef36490f43aa0d0d80f9df64e9c92a576eee3042f1a97c8813c05355`),
the current stable arm64/amd64 release.

Say or type **Ask Hermes** followed by the task. The assistant returns a job
number immediately. **Ask Hermes status**, **Ask Hermes result**, and
**Ask Hermes cancel** act on the latest job. The assistant stores the latest 50
job states and results in SQLite. It announces completion or failure on the Pi.
After a restart an unfinished job is marked interrupted, so it won't run again
without a fresh request.

## Local inference

To use a self-hosted OpenAI-compatible endpoint, set `HERMES_LOCAL_BASE_URL`
(including `/v1`), `HERMES_LOCAL_MODEL`, and the endpoint's dedicated
`HERMES_LOCAL_API_KEY` in the private Compose `.env`. Set all three or leave
all three blank. Only the `hermes` container receives the local key. Recreate
both `assistant` and `hermes`. Explicit **Ask Hermes** jobs use that model when Local is selected. The
tool-free job has a 45-second budget. Jobs may take up to four minutes when
Go tools are enabled. Hermes
requires the endpoint to serve a context window of at least 64,000 tokens.
For desktop inference, use an authenticated endpoint reachable from the Pi's Hermes
container over the tailnet. Token counts on that path are local latency cost,
not a billed provider invoice.

Give the endpoint a dedicated origin (scheme, host and port), and treat every
path on it as receiving the local key. Besides the configured `/v1` path, the
pinned hermes-agent probes the same origin for server type and context length
and sends the key with those requests, for example `GET /api/v1/models`,
`/api/tags`, `/props`, `/v1/props`, `/version` and `POST /api/show`. Don't put
the inference server behind a reverse proxy that routes other paths on that
origin to a different service.

## Provider and model selection

In the owner Dashboard, open **Hermes** and save a runtime selection. With no
saved selection, Hermes uses the configured local endpoint or, when the local
endpoint is unset, OpenCode Go with DeepSeek V4.1 Flash. The selected provider
and preferred OpenCode Go model persist across restarts and apply to the next
job. Each job keeps the provider and model it had at submission. Hermes
supplies the local key for Local jobs. The Assistant sends the dedicated
OpenCode Go key only for OpenCode Go jobs.

The running `hermes` container reports its local model, base URL and whether
its local key is configured. The Assistant compares the reported route with
its own settings and refuses new local jobs if the Dashboard shows a
readiness problem:

- **Unreachable:** the sidecar is down, starting, or slower than 2 seconds.
  Check its logs for incomplete local settings if it fails to start.
- **Different local settings:** the sidecar was recreated with another model
  or endpoint, or without the local settings. Recreate `assistant` and
  `hermes` together.
- **Out of date:** the `hermes` image predates sidecar-owned credentials.
  Deploy matching images together.

The check runs for a new job only while Local is selected (skipped while
another job runs, since that request is refused anyway), and when the
Dashboard loads Hermes readiness or loads or saves the runtime view. It never runs for
status, result or cancel.

OpenCode Go readiness requires its dedicated key in **Secrets**. You can save
the cloud model preference while Local is selected, including before linking
the cloud key. New jobs fail if the selected provider is unavailable. Deploy
matching assistant and Hermes images together to enable provider switching.

The OpenCode Go choices are DeepSeek V4.1 Flash, DeepSeek V4 Pro, GLM-5.3 Flash
and Kimi K3. They support tool calls on Go's Chat Completions endpoint and are
listed with zero-day request retention as of November 23, 2026. Go's DeepSeek
agreement renews monthly, and the workspace may need to enable **Global**
regions for DeepSeek. See [OpenCode Go’s privacy terms](https://opencode.ai/docs/console/go#privacy)
and [model catalog](https://opencode.ai/inference/go/v1/models).

## Headless runtime

The sidecar is the headless entry point: `AIAgent` with `enabled_toolsets=[]`,
`skip_context_files`, `skip_memory`, `skip_background_review`, and
`load_soul_identity=False`. Tool Search stays off on that empty allowlist so
the model never sees the `tool_search` / `tool_describe` / `tool_call` bridge.

To measure discovery without enabling Hermes built-ins, set `HERMES_SAMPLE_MCP=1`.
That writes a Hermes `config.yaml` with Tool Search on and a two-tool sample
MCP server (`sample_ping`, `sample_time`) marked `lazy: true`. The first live
connect fills the schema cache; later jobs register those tools without spawning
the server until a call. `HERMES_TOOL_SEARCH=off` and `HERMES_SAMPLE_MCP_LAZY=0`
are the eager comparison. Keep unused built-ins disabled; the only eager
exception that has been measured is the empty production allowlist itself.

`hermes/probe_runtime.py` captures the JSON on the wire (hosts, tool names,
prompt characters, usage tokens, wall time, RSS) without printing credentials.
Re-run it from a private `HERMES_HOME` so a live sidecar config is not replaced.

Measured 2026-11-23 against the desktop Ornith 9B endpoint
(64k context). These are local token and latency
costs, not a billed provider invoice. Hermes also probes the local endpoint
several times before the chat turn; those extra HTTP calls are included in
wall time.

| Runtime | Job | Prompt tokens | Wall | Peak RSS | Tools on the chat request |
| --- | --- | --- | --- | --- | --- |
| Pi v0.23.1 live `/run` | no-tool | n/a | 23.3 s | ~26 MB idle | none (sidecar) |
| Pi v0.23.1 in-process | no-tool | 511 | 26.3 s | 191 MB | none |
| Pi v0.23.1 | eager `todo` | 1,069 | 62.4 s | 190 MB | `todo_list` |
| Pi v0.23.1 | Tool Search `todo` | 1,074 | 71.6 s | 191 MB | `tool_search`, `tool_describe`, `tool_call` |
| Pi v0.23.1 | lazy sample MCP (cold) | 2,214 | 77.9 s | 231 MB | Tool Search bridge; `sample_ping` ran |
| Pi v0.23.1 | lazy sample MCP (warm) | 2,150 | 18.8 s | 230 MB | same; `sample_ping` ran |
| Pi v0.23.1 | eager sample MCP | 2,221 | 61.7 s | 229 MB | `sample_ping`, `sample_time`; `sample_ping` ran |
| Desktop v0.23.3 | no-tool | 510 | 20.7 s | 202 MB | none |
| Desktop v0.23.3 | eager sample MCP | 3,624 | 52.9 s | 240 MB | `sample_ping`, `sample_time` |

For the two sample tools, the bridge and eager schemas use similar prompt
tokens. v0.23.3 kept the headless constructor, Ornith transport, and tool-free
prompt size measured on v0.23.1.

## Private Tool adapter

Go owns Tool execution, grants, Confirmation and Local-only filtering. The
adapter is a private HTTP surface on the assistant health listener
(`GET/POST /hermes/adapter/tools…`), authenticated with the same
`HERMES_RUNTIME_TOKEN` the sidecar already uses. Credentials never leave Go.

Set `HERMES_GO_TOOLS=1` in the Pi's private Compose `.env` and recreate the
assistant and Hermes services to enable the bridge. Leave `HERMES_SAMPLE_MCP`
off. Configured tools are read at the start of each job. The model sees the
Tool Search bridge and loads a tool's schema when it requests it. The local MCP
process starts on the first invocation.

The initial read-only tools are `web.search` (through the existing SearXNG
service), `weather.current`, and `weather.forecast`. Weather requires its
account grant. Results sent to a cloud model follow that provider's terms.

`spotify.now_playing` was considered as the second sample and stays off this
surface: it is Local-only because it names the owner's listening
([ADR-0002](adr/0002-single-prompt-builder-is-the-only-egress.md)).

Every invocation re-checks:

- the allowlist
- the live `hermes.integrations.<id>` grant for the account that unlocks the Tool
- enabled, read-only and eligible for egress
- stored secret values in arguments and results

A listing returns names, descriptions and permission only. A job loads one
tool's schema when it asks to use that tool.

## Tool recipes

Open **Hermes → Tool recipes** on the owner dashboard. Choose a vetted base tool,
give it a name and description, and optionally fix arguments for a recurring
query. For example, a `weather_home` recipe can fix the city for
`weather.current`. Enabled recipes appear as `recipe.<name>` on the next job.
A recipe inherits its base tool's account grant and permissions. Saved recipes
can be edited, disabled or deleted, and the bridge checks the live policy at
invocation.

## Capability inventory

Every Go Tool reachable from the Agent path is classified against this adapter:

| Decision | Meaning |
| --- | --- |
| **allowlist** | On the current surface once the matching grant is on (`web.search`, `weather.current`, `weather.forecast`). |
| **later** | Other Agent-reachable Tools require individual review. The adapter currently accepts only read-only Tools. |
| **local_only** | Results must stay on the Pi. Hermes never lists or calls them. Includes `spotify.now_playing`, timers/alarms that stay local, `assistant.repeat`. |
| **confirm** | Outward writes. Go owns Confirmation; the adapter never proposes or runs them. Includes `github.issue_create` and `notify.phone`. |

New account or provider integrations stay out of this adapter unless a later
ticket adds them to the allowlist.

## What the page shows

- **Surface.** The enforced allowlist after live grants, listed without
  schemas. Allowlisted Tools whose grant is off are named as withheld. Confirm
  Tools and Local-only Tools are counted beside the list and are never callable
  here.
- **Accounts.** GitHub, the Agent model, Spotify, ntfy, WeatherAPI and Monster
  Smart Lighting. Each row names the write-only secrets it needs, the current
  status (`Linked`, `From environment`, `Partly linked`, `Not linked`), the
  Tools it unlocks, and one short note.
- **Allow Hermes.** A live setting per account
  (`hermes.integrations.<id>`). The adapter enforces it on the next list,
  schema or invoke. Settings also shows the same keys, and a reset revokes the
  grant.

## Linking

The link form writes the same secrets the Tool modules already resolve. Values
are write-only: the page shows only `set`, `source` and the last four
characters, never the value. A blank field on a link form keeps an already
stored value, so adding an optional secret never forces a required one to be
retyped. Unlink clears every **stored** value that account owns, including
optional ones. A value supplied by `.env` cannot be removed from here; the row
keeps reporting it as active (`From environment`), and the flash says only the
stored values were cleared.

Use scoped credentials. A GitHub fine-grained PAT limited to the repositories
and read/write scopes Hermes should have is the intended shape; do not paste a
classic all-repo token.

## MCP and OpenCode delegation

The spike in `docs/spikes/hermes-opencode-attach.md` covers the transport
(`opencode serve` over Tailscale), the auth findings and the measured latency.

## Security notes

- Secrets are declared by the Tool modules and the Agent provider; the link
  form can only write those names.
- A grant never exposes a secret value. The adapter reads the same secret
  service only to refuse payloads that echo a stored value; Tool handlers still
  resolve their own credentials in-process.
- Nothing on this page starts a job, opens a port or installs a package.
