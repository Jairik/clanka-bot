# OpenCode tailnet ACL (#128)

Status: **prepared, not deployed; issue must remain open**.

The desktop uses `100.115.149.100:47822`; basic authentication remains required.
[Policy fragment](../../deploy/tailscale/opencode.hujson) restricts TCP on that
port to the Pi and desktop, with both IPv4 and IPv6 addresses accounted for.
Other destinations retain TCP/UDP access on every port; the desktop retains
TCP/UDP on every other port and UDP on 47822. ICMP remains available through
Tailscale's implicit ICMP allowance. This preserves the default general accept
semantics while carving out exactly the OpenCode TCP port.

## Apply the policy (tailnet owner)

1. Export the current policy from the Tailscale admin access-controls page and
   retain a private rollback copy. This fragment is **not a full replacement**.
2. Merge its `ipsets`, `acls`, and `tests` entries into the current policy.
   Replace the default general `*:*` accept. Audit **both** `acls` and `grants`:
   no other rule may allow a non-Pi/non-desktop source to desktop TCP 47822.
   A user selector for the owner includes iOS and is too broad for that port.
   Preserve existing `ssh`, `nodeAttrs`, groups, tags, and unrelated settings.
   In particular, preserve the existing Tailscale SSH permission for `rbpi`.
3. Run the admin editor's validation against the **complete merged policy**.
   All included tests must pass before saving. Tests cover iOS denial over both
   address families, allowed sources, SSH, adjacent desktop ports, and 47822 on
   the Pi. Do not treat local JSON parsing as Tailscale policy validation.
4. Save and retain the resulting revision and passing test output. Recheck
   these pinned addresses whenever a device is replaced or re-enrolled.

The fragment uses [IP set subtraction](https://tailscale.com/docs/features/tailnet-policy-file/ip-sets)
so future destinations retain general connectivity without reopening the desktop.
[Tailscale policies are additive](https://tailscale.com/docs/reference/syntax/policy-file):
a deny test is an assertion, not a deny rule. A surviving broad allow defeats it.

## Start and probe

Read the private `docs/agents/machines.md` before connecting to the Pi. The
launcher requires `~/.config/clanka/opencode-server.pass`, owned by the desktop
user with mode 0600. Provision it once without printing its contents:

```sh
# Generate only if absent; never rotate a running server's credential implicitly.
(umask 077; mkdir -p ~/.config/clanka; set -C; openssl rand -hex 32 > ~/.config/clanka/opencode-server.pass)
bash scripts/opencode-serve.sh
```

The launcher refuses missing, empty, symlinked, wrong-mode, or wrong-owner files,
unsets `OPENCODE_PASSWORD`, exports `OPENCODE_SERVER_PASSWORD` using shell
builtins, and binds the fixed desktop address and port. Do not enable shell
tracing or dump its environment. No provider key is needed for `/api/info`.
The existing OpenCode Go credential stays in its original private file.

In another desktop terminal:

```sh
# Verify binding and desktop auth without putting the password in argv.
ss -ltn '( sport = :47822 )'
python3 scripts/opencode-probe.py < ~/.config/clanka/opencode-server.pass
```

Copy only `scripts/opencode-probe.py` to the Pi, for example to
`~/.cache/clanka-acl/probe.py`, then send the password on SSH stdin:

```sh
# SSH transports the secret on stdin; no password is written on the Pi.
tailscale ssh rbpi@pi.tail9fc2ef.ts.net \
  'env -u OPENCODE_PASSWORD python3 ~/.cache/clanka-acl/probe.py' \
  < ~/.config/clanka/opencode-server.pass
```

With the server still listening and the saved policy active, use iOS (Tailscale
connected, no exit-node ambiguity) to open `http://100.115.149.100:47822/`.
Record a connection timeout/failure, then immediately repeat the Pi 200 probe.
An HTTP response of any status from iOS is a failure of isolation. An offline
phone or stopped server does not prove ACL denial. Also verify iOS can still
reach an existing general service and desktop-to-Pi SSH still works. Stop the
foreground server with Ctrl-C when testing ends. Leave it stopped until the
policy has been validated and blocked-source isolation proven.

## Evidence, 2026-11-13

- Initial desktop listener: absent. Desktop OpenCode: **v2.3.0** (newer than spike).
- Tailscale status: desktop and Pi online with the issue's IPv4 addresses;
  iOS `100.106.138.35` offline. IPv6 addresses in the fragment came from status.
- Desktop-to-Pi Tailscale SSH as `rbpi`: successful; Pi Python 3 available.
- Temporary server: `ss` showed only `100.115.149.100:47822`, no wildcard/LAN bind.
- Desktop and Pi authenticated `/api/info`: **200**, unauthenticated desktop: **401**.
- A deliberately incorrect inherited `OPENCODE_PASSWORD` was cleared by launcher;
  the real server-password probe still returned 200. Password file: **0600**.
- Temporary process terminated; final `ss` showed no listener on 47822.
- `bash -n scripts/opencode-serve.sh`, Python compilation, and
  `python3 scripts/tests/opencode_serve_test.py`: passed. The latter exercises
  missing/empty/unsafe credential rejection, env precedence, and fixed bind argv.

Remaining gates: no `TS_*`/`TAILSCALE_*` management credential is configured in
this session. The full live policy was not retrieved or changed; daemon netmap
inspection requires privileged access unavailable without a sudo password.
The owner must merge and validate the fragment using authorized management
access, then capture the active-policy revision/tests, a live iOS (or another
non-allowed node) failed connection while Pi succeeds, and post-change general
connectivity. The persistent server is not enabled. Acceptance criteria 2, 3
(post-change), and 5 remain unproven; criteria 1, 4, and 6 were evidenced only
for the temporary run. Do not close #128 or move its board card to Done yet.
