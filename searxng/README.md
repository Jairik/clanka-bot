# web-search: internal SearXNG

SearXNG backs the Agent-path `web.search` Tool. It is not a user-facing search
page: Compose publishes no host port and only the `assistant` container talks
to it on the Compose network at `http://web-search:8080`.

## What is pinned

- Image: `docker.io/searxng/searxng:2026.11.12-de722ab1b` (official multi-arch
  release; `searxng/Dockerfile` wraps it so the stack gets a
  `clanka/web-search:<sha>` tag for rollback).
- Configuration: `searxng/config/settings.yml` and the default bot-detection
  file `searxng/config/limiter.toml`, mounted read-only at `/etc/searxng`.
- Server: Granian, the current official container server. `GRANIAN_WORKERS=2`
  is the ticket's Pi-sized bound; the old uWSGI variables (`UWSGI_WORKERS`,
  `UWSGI_THREADS`) are not used by this image.

## Deliberate shape

- **Limiter off, no valkey.** The instance is internal and single-caller. The
  limiter would need a valkey service; neither is part of this stack.
- **No published port and no data volume.** The assistant reaches it over the
  Compose network only. SearXNG keeps no search history; the favicon cache is
  not needed, and ADR-0003 keeps the SQLite database exclusive to `assistant`.
- **Three fast general engines** (DuckDuckGo, Brave, Bing) with a 1.5 s
  per-engine timeout, so one slow engine cannot hold the whole search open. On
  a network where an engine is degraded, SearXNG logs that engine as
  unresponsive (Brave was seen timing out from the dev box) and still returns
  the results the other engines produced.
- **JSON enabled** under `search.formats`, because `web.search` reads
  `format=json`. HTML remains for a manual check; an unlisted format is a 403.

## Manual smoke test

```sh
docker compose build web-search
docker compose up -d web-search
docker compose exec web-search wget -qO- http://127.0.0.1:8080/healthz
# A search from inside the network:
docker compose exec web-search wget -qO- \
  'http://127.0.0.1:8080/search?q=searxng&format=json&categories=general'
```

The `X-Forwarded-For nor X-Real-IP header is set!` line is expected: this is a
direct internal caller, not a reverse-proxied public instance. It is logged
once per process and does not reject the request.

## Sources

- Container guide and environment variables:
  <https://docs.searxng.org/admin/installation-docker.html>
- Granian options (the official container's server):
  <https://docs.searxng.org/admin/installation-granian.html>
- `settings.yml` (`use_default_settings` / `engines.keep_only`, `search.formats`,
  `outgoing.request_timeout`):
  <https://docs.searxng.org/admin/settings/settings.html>
- JSON search API:
  <https://docs.searxng.org/dev/search_api.html>
