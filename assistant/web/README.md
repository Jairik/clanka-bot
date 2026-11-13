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
- Spotify Connect/Verify/Disconnect are real form posts (Connect leaves for
  accounts.spotify.com) and return to `/spotify?flash=...`.
- The CSP allows only same-origin scripts and styles: no inline `<script>` or
  `<style>` (the theme bootstrap is `public/theme.js`).

## Look

Monochrome: near-black/white, hairline borders, square corners, serif page
titles. Color is reserved for status (ok/warn/bad) and the red record dot.
Theme tokens live in `src/index.css`; `?theme=light|dark` overrides the OS.
