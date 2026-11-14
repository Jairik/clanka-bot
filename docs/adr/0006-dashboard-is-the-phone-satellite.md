# The Dashboard is the phone Satellite; there is no second app

The Map 2 brief asked whether the phone Satellite PWA and the Dashboard should be one app. The owner decided (2026-11-13, recorded on #95) that they are the same app: the installable Dashboard **is** the phone Satellite. It shares the assistant's settings, Activity, chat and browser push-to-talk instead of a second client with its own settings surface.

Why: the Dashboard is already one document served from the assistant origin with self-hosted fonts and no CDN, which is most of an installable app. A separate Satellite would need the same API, auth and settings twice, and the two would drift the moment one gained a control the other did not. The phone gesture is a pointer-type decision in the one composer, not a second client.

## Consequences

- `assistant/web` ships `manifest.webmanifest`, generated icons and a service worker that precaches the shell. Installing it on a phone opens the same pages the desktop uses.
- The service worker keeps API reads network-first and answers an unreachable Pi with the cached shell (or `offline.html`), so an installed app reports the outage instead of a browser error.
- Push-to-talk is the composer's touch/pen gesture (hold to record, release to send); a mouse keeps click-to-start.
- Anything added to the Dashboard is automatically on the phone. A page that only makes sense on a desktop needs responsive treatment, not a second app.
- The one-origin rule is unchanged: the manifest, worker, icons and every asset come from the assistant origin, and the worker only fetches same-origin.
