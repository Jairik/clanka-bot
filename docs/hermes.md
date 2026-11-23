# Hermes setup and control panel

Hermes can run explicit, model-only jobs on the Pi. The Dashboard shows whether
the runtime is configured, the candidate Tool surface for the future MCP
adapter, and the accounts you can link today. Unmatched voice and typed
requests still use the Go Agent path until
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
DeepSeek V4.1 Flash model and has zero enabled tools. The sidecar publishes no
host port and keeps its per-job runtime files in temporary storage.

Say or type **Ask Hermes** followed by the task. The assistant returns a job
number immediately. **Ask Hermes status**, **Ask Hermes result**, and
**Ask Hermes cancel** act on the latest job. The assistant stores the latest 50
job states and results in SQLite. It announces completion or failure on the Pi.
After a restart an unfinished job is marked interrupted, so it won't run again
without a fresh request.

## What the page shows

- **Surface.** Every registered Tool that is `read` or `act` and is **not**
  Local-only. Confirm Tools are excluded, because Hermes may never execute an
  outward write without the owner's spoken Confirmation; Local-only Tools are
  excluded because their results must never leave the Pi. The counts of the
  excluded Tools are shown beside the list.
- **Accounts.** GitHub, the Agent model, Spotify, ntfy, WeatherAPI and Monster
  Smart Lighting. Each row names the write-only secrets it needs, the current
  status (`Linked`, `From environment`, `Partly linked`, `Not linked`), the
  Tools it unlocks, and one short note.
- **Allow Hermes.** A live setting per account
  (`hermes.integrations.<id>`). It records intent only until the adapter
  exists; the page says so. Settings also shows the same keys, and a reset
  revokes the grant.

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

The Map 2 phase 6 backlog includes the MCP server, Agent-callable job Tools, and
the desktop OpenCode adapter. Account grants are stored intent until the
adapter enforces them at execution. Outward writes also require spoken
Confirmation at delegation.

The spike in `docs/spikes/hermes-opencode-attach.md` covers the transport
(`opencode serve` over Tailscale), the auth findings and the measured latency.
Accounts and grants are durable, so a future adapter can use the existing
services and keep credentials with the Tool modules that need them.

## Security notes

- Secrets are declared by the Tool modules and the Agent provider; the link
  form can only write those names.
- A grant never exposes a secret value. The adapter is expected to read the
  same secret service and pass only the credentials a granted Tool needs.
- Nothing on this page starts a job, opens a port or installs a package.
