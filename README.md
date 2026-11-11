# clanka-bot

My lovely always-on voice assistant for stuff written in go. Kind of like a lobotomized jarvis,

## Run the prototype

The prototype is the `assistant` Go service in Docker. It exposes a health
endpoint and a localhost-only debug entry that accepts a typed transcript
(entering the pipeline just after STT) and answers through the Tool registry.

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

The response is the spoken-form reply from the `time.now` Tool, plus the
routing path and a request id. Anything the Fast path does not handle goes to
the Agent path, which needs an OpenRouter key in `.env`
(`OPENROUTER_API_KEY`); without one it answers with the configured Unavailable
message and the Fast path keeps working. Every provider request forces
OpenRouter's `provider.zdr` and `provider.data_collection: "deny"` in code.
Both published ports are bound to the host loopback only. The debug listener
binds all interfaces inside the container (see `docker-compose.yml`), so the
host port mapping is what keeps it local.

Non-secret settings live in [`assistant/config.yaml`](assistant/config.yaml);
`.env` holds deployment inputs only.

Run the tests with:

```sh
make test
```

## Deploying

`make deploy` is the deployment path: it pulls the current branch, builds
SHA-tagged images, starts the stack and waits for health checks. Roll back with
`make rollback SHA=<commit>`, which reuses that commit's images without a
rebuild. The Pi provisioning checklist, image-tagging rules and the model-weights
layer convention are in [`docs/deploy.md`](docs/deploy.md).
