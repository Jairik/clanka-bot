# Dashboard web app

The Dashboard UI: React 19, HeroUI v3 (Tailwind 4) and Vite. `pnpm build`
writes to `../internal/dashboard/ui/dist`, which the Go binary embeds, so the Pi
serves everything itself (fonts included) with no CDN.

```sh
pnpm install
pnpm build        # typecheck + production bundle (what `make web` runs)
pnpm test         # vitest: logic and component tests, no network or devices
pnpm dev          # Vite on :5173, proxying to a local assistant
```

The build also writes `.gz` and `.br` siblings of the text assets (JS, CSS,
HTML, SVG, JSON). The Go handler serves the smallest encoding the client
accepts; the JSON views, which cannot be precompressed, are gzipped on the fly.

## Offline behaviour and caching

The service worker (`src/sw-template.js`) exists so an installed app opens and
says the Pi is unreachable; it is not an offline copy of the Dashboard.

- **Install** precaches the shell only: `index.html`, `offline.html`,
  `theme.js`, the manifest, the icons, the fonts, and the JS/CSS that
  `index.html` itself references (the entry, the shell and Home). A lazy route
  chunk is never precached, so a first visit does not download Settings, Voice
  Samples and Health before the user opens them. The build fails if a route
  chunk reappears in the list, and prints the list and its size.
- **Navigations** stay network-first: the document comes from the Pi so a new
  build is picked up, and the last good copy opens when it is not reachable.
- **Subresources** are cache-first only where the bytes are immutable:
  content-hashed build output under `/assets/`, `/icons/` and `/fonts/`, plus
  the versioned shell files (the cache name includes the build hash). Every
  other same-origin GET is network-only, with no `respondWith`.
- **Never cached**: any URL with a query string or `destination: "audio"`, so
  the wording preview (`?text=`), a voice preview, a sound preview and a
  recorded take are always fetched fresh. Those URLs do not carry the voice or
  the rate, the bytes are the owner's own, and Cache Storage is origin-scoped:
  a cached take would replay the wrong voice or reach the next Tailscale user
  on a shared profile. The Go handlers send `Cache-Control: no-store` as well.
  This also keeps Range requests away from the worker (Safari).

The cache decision is `src/sw-cache-policy.js`, inlined verbatim into the built
`sw.js` and unit-tested in `src/__tests__/sw-cache-policy.test.ts`; the install
list is `src/precache.ts`, tested in `src/__tests__/precache.test.ts`.

Route chunks load on demand. The shell warms one page on hover/focus, and
`prefetchNear` warms only the page either side of the current one after idle —
never all sixteen, and nothing at all when `navigator.connection.saveData` is
set or the link reports `slow-2g`/`2g`. To check a changed preview by hand:
open Settings > Wording, preview the Filler, switch the voice, preview the same
text again; the second take must be in the new voice (and a Network panel
entry for `/settings/wording/preview` must say `no-store`, never
`(disk cache)`).

`pnpm dev` proxies `/api/*` and the write routes to `DASHBOARD_URL`
(default `http://127.0.0.1:8082`) and adds `Tailscale-User-Login:
$DASHBOARD_OWNER` (default `dev`), standing in for `tailscale serve`. Start the
assistant with `ASSISTANT_DASHBOARD_OWNER=dev`.

## How it talks to Go

- Reads: `GET /api/ui/<page>` returns the page's Go view model as JSON. Labels,
  source rules and action URLs come from the server. `src/types.ts` is
  **generated** from the Go view structs by `assistant/cmd/gentypes` (run by
  `make web` and checked in CI), so the two cannot drift; the handful of types
  with no Go counterpart live in `src/types.manual.ts`.
- Writes: form-encoded `POST` to the action URL in the view. The answer is the
  refreshed view, with `Error` and a 4xx/5xx status when the write was refused.
- Spotify Verify/Disconnect are form posts and return to `/spotify?flash=...`.
  Connect is a GET link to `/spotify/connect`, which redirects to
  accounts.spotify.com; a navigation is not a form submission, so the document
  policy keeps `form-action 'self'`.
- The CSP allows only same-origin scripts and styles: no inline `<script>` or
  `<style>` (the theme bootstrap is `public/theme.js`).

## Look

Monochrome: near-black/white, hairline borders, square corners, serif page
titles. Color is reserved for status (ok/warn/bad) and the red record dot.
Theme tokens live in `src/index.css`; `?theme=light|dark` overrides the OS.
