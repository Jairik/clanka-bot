# Hermes setup and control panel

Hermes can run explicit, model-only jobs on the Pi. The Dashboard shows whether
the runtime is configured, the **enforced** Tool allowlist, and the accounts
you can link today.

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
- enabled / Local-only / confirm
- stored secret values in arguments and results

A listing returns names, descriptions and permission only. A job loads one
Tool's schema when it asks to use that Tool. Headless Hermes is not yet wired
to this adapter; that belongs to the runtime ticket.

## Capability inventory

Every Go Tool reachable from the Agent path is classified against this adapter:

| Decision | Meaning |
| --- | --- |
| **allowlist** | On the current surface once the matching grant is on (`weather.current`, `weather.forecast`). |
| **later** | Agent-reachable `read`/`act` Tools that are not Local-only. They can join this same adapter; they are not in the first two-tool set. Examples: `github.pr_list`, `web.search`, `note.recall`, `list.read`, Spotify playback. |
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
