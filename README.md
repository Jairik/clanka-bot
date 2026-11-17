# clanka-bot

My lovely always-on voice assistant for stuff written in go. Kind of like a lobotomized jarvis. Idc if you fork but I would advise not to, this is extremely jank and fragile. I'm sure there are better open-source alternatives, I just wanted to make my own. 

## Quick start

The prototype runs the `assistant` Go service in Docker alongside the
`wakeword`, `stt`, `tts`, `web-search` and `soloist` services.

```sh
cp .env.example .env
docker compose up --build
```

Check it and ask the time:

```sh
curl -s localhost:8080/healthz
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it"}'
```

The full setup walkthrough, voice pipeline, GitHub and Spotify integrations,
Dashboard, Secrets and latency numbers are in the [main guide](docs/guides/README.md).

## Documentation

- [Main guide](docs/guides/README.md) — running the prototype and using every feature
- [Glossary](docs/guides/glossary.md) — the project's domain vocabulary
- [Deployment](docs/deploy.md) and [auto-deploy](docs/auto-deploy.md)
- [Architecture decisions](docs/adr/), [specs](docs/specs/), and [research](docs/research/)
- [Runbooks](docs/runbooks/), [spikes](docs/spikes/), and [evidence](docs/evidence/)
- [Agent workflow docs](docs/agents/)

## Tests

```sh
make test
```

## Deploying

`make deploy` pulls the current branch, builds SHA-tagged images, starts the
stack and waits for health checks. See [docs/deploy.md](docs/deploy.md) for the
Pi provisioning checklist and rollback procedure.
