# clanka-bot

My lovely always-on voice assistant for stuff written in go. Kind of like a lobotomized jarvis,

## Run the prototype

The prototype is the `assistant` Go service in Docker, with a `tts` service that
speaks its replies. `assistant` exposes a health endpoint and a localhost-only
debug entry that accepts a typed transcript (entering the pipeline just after
STT) and answers through the Tool registry.

```sh
cp .env.example .env
docker compose up --build
```

The first start downloads the Pocket TTS weights. They are licence-gated on
Hugging Face: accept the terms for `kyutai/pocket-tts`, set `HF_TOKEN` in
`.env`, and wait for the `tts` health check (it only passes once the model and
default voice are warm).

Check it and ask the time:

```sh
curl -s localhost:8080/healthz
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it"}'
```

The reply is spoken on the configured Pulse sink as it is synthesised, then the
response returns the spoken-form text, the routing path, a request id, and
whether audio played (`"spoken": true`). Anything the Fast path does not handle
goes to the Agent path, which needs an OpenRouter key in `.env`
(`OPENROUTER_API_KEY`); without one it answers with the configured Unavailable
message and the Fast path keeps working. Every provider request forces
OpenRouter's `provider.zdr` and `provider.data_collection: "deny"` in code.
Both published ports are bound to the host loopback only. The debug listener
binds all interfaces inside the container (see `docker-compose.yml`), so the
host port mapping is what keeps it local.

### Audio

Per [ADR-0001](docs/adr/0001-middleware-owns-all-audio-io.md), only the
`assistant` container mounts the host's Pulse socket; `tts` does computation
only. Compose runs `assistant` as `${ASSISTANT_UID}` (default 1000) so it can
read the host Pulse cookie, and plays through `pacat`. Set
`ASSISTANT_PULSE_SINK` to a sink name from `pactl list short sinks`; empty uses
the default sink. On the Pi this is the Echo Dot once Bluetooth A2DP is set up.

Non-secret settings live in [`assistant/config.yaml`](assistant/config.yaml);
`.env` holds deployment inputs only. An empty `tts.addr` disables spoken replies.

## Tests

```sh
cd assistant && go test ./...
python -m pytest tts/tests   # needs `pip install wyoming pytest`
```
