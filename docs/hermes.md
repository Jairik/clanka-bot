# Hermes setup and control panel

Hermes can run explicit, model-only jobs on the Pi. The Dashboard shows whether
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
key as `HERMES_OPENCODE_GO_API_KEY`. The key and explicit task go to the sidecar
over the authenticated private Compose network. The assistant sends no history,
Tool results or linked-account secrets. Hermes uses OpenCode Go's
DeepSeek V4.1 Flash model unless a local endpoint is configured, and has zero
enabled tools by default. The sidecar publishes no host port and keeps its
per-job runtime files in temporary storage. The image pins Hermes Agent
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
`HERMES_LOCAL_API_KEY` in the private Compose `.env`. Recreate both
`assistant` and `hermes`. Explicit **Ask Hermes** jobs use that model. The
default job still runs with zero tools and must finish within 45 seconds. Hermes
requires the endpoint to serve a context window of at least 64,000 tokens.
For desktop inference, use an authenticated endpoint reachable from the Pi's Hermes
container over the tailnet. Token counts on that path are local latency cost,
not a billed provider invoice.

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

For two sample tools the bridge's prompt cost is in the same band as eager
schemas. Keep production jobs on the empty allowlist. Use Tool Search and lazy
MCP when more servers are attached; the empty allowlist is the measured eager
exception. v0.23.3 is pinned because the headless constructor, Ornith
transport, and no-tool prompt size match v0.23.1 while picking up the current
stable security fix.

## Private Tool adapter

Go owns Tool execution, grants, Confirmation and Local-only filtering. The
adapter is a private HTTP surface on the assistant health listener
(`GET/POST /hermes/adapter/tools…`), authenticated with the same
`HERMES_RUNTIME_TOKEN` the sidecar already uses. Credentials never leave Go.

The current allowlist is two read-only weather Tools:

- `weather.current`
- `weather.forecast`

`spotify.now_playing` was considered as the second sample and stays off this
surface: it is Local-only because it names the owner's listening
([ADR-0002](adr/0002-single-prompt-builder-is-the-only-egress.md)).

Every invocation re-checks:

- the allowlist
- the live `hermes.integrations.<id>` grant for the account that unlocks the Tool
- enabled, read-only and eligible for egress
- stored secret values in arguments and results

A listing returns names, descriptions and permission only. A job loads one
Tool's schema when it asks to use that Tool. Headless Hermes is not yet wired
to this adapter; that work is tracked in #525.

## Capability inventory

Every Go Tool reachable from the Agent path is classified against this adapter:

| Decision | Meaning |
| --- | --- |
| **allowlist** | On the current surface once the matching grant is on (`weather.current`, `weather.forecast`). |
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

The sidecar still runs model-only jobs. Wiring this adapter into headless
Hermes (lazy MCP / Tool Search, sample calls on Ornith) is a separate runtime
ticket. Outward writes remain behind Go-owned Confirmation.

The spike in `docs/spikes/hermes-opencode-attach.md` covers the transport
(`opencode serve` over Tailscale), the auth findings and the measured latency.

## Security notes

- Secrets are declared by the Tool modules and the Agent provider; the link
  form can only write those names.
- A grant never exposes a secret value. The adapter reads the same secret
  service only to refuse payloads that echo a stored value; Tool handlers still
  resolve their own credentials in-process.
- Nothing on this page starts a job, opens a port or installs a package.
