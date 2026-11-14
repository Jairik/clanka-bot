# Hermes control panel

The Dashboard's **Hermes** page configures the surface Hermes will get and the
accounts it may use. It is deliberately honest about the runtime: **this build
does not install or run Hermes.** The page shows "not installed", the exact
Tool surface an adapter would publish, and the accounts you can link today.

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

## What is still missing

The Map 2 spec's phase 6 items are not implemented in this tree:

1. an MCP server that publishes the surface above;
2. a `hermes.delegate` / `status` / `result` Tool set and the durable job table;
3. the "ask Hermes" request prefix and its routing slot;
4. the Hermes container/sidecar and its OpenCode Go credential.

The spike in `docs/spikes/hermes-opencode-attach.md` covers the transport
(`opencode serve` over Tailscale), the auth findings and the measured latency.
The control panel is the Dashboard half of that work: accounts and grants are
durable now, so the adapter can read them when it lands.

## Security notes

- Secrets are declared by the Tool modules and the Agent provider; the link
  form can only write those names.
- A grant never exposes a secret value. The adapter is expected to read the
  same secret service and pass only the credentials a granted Tool needs.
- Nothing on this page starts a job, opens a port or installs a package.
