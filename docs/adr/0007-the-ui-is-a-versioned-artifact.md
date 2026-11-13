# The Dashboard UI is a versioned artifact, with the embedded copy as fallback

The Dashboard's React app ships as its own SHA-tagged Compose service
(`dashboard-web`) rather than only inside the assistant image. The assistant
proxies to it on the Dashboard origin: `/` and every asset request try the
artifact first and fall back to the bundle embedded in the binary on any
failure. A page-only fix is a rebuild and restart of that one service; a stopped
artifact still serves a working Dashboard with no internet.

Why: the UI changes far more often than the Go API, and rebuilding the assistant
image to fix a layout meant restarting the process that owns audio, the voice
loop and the SQLite store. Two artifacts need a contract, so the server reports
an API view version (`X-Dashboard-API-Version`, and `APIVersion` in the session
view), the UI embeds the version it was built against, and a mismatch renders a
visible version page instead of half-populated fields. The Go test suite pins
the two constants together.

## Consequences

- The browser still talks to one origin: assets, document, JSON and SSE all come
  from the assistant. Identity, Host and same-origin checks are unchanged and
  the artifact service is never published to the host.
- The same Vite build is embedded at assistant build time, so offline and
  artifact-stopped operation is the fallback path, not a second UI.
- `X-Dashboard-UI` and `X-Dashboard-UI-Version` name the copy that answered,
  which is how a UI-only deploy and a rollback are checked.
- A view change that an older UI would misread requires bumping `APIVersion` in
  the same commit as the Go change and in `web/src/api-version.ts`.
- `make web-image` ships a UI-only change; `make deploy` and `scripts/deploy.sh`
  still build and health-check both images from one commit.
