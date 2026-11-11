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

The image downloads the Pocket TTS weights and default voice at build time,
anonymously, from the public `kyutai/pocket-tts-without-voice-cloning` bundle
and `kyutai/tts-voices`. Nothing is downloaded on start, so `tts` comes up
offline; the health check only passes once the model and default voice are warm.
Rebuild `tts` (not just restart it) if you change `TTS_LANGUAGE` or `TTS_VOICE`.

Check it and ask the time:

```sh
curl -s localhost:8080/healthz
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it"}'
```

The response is the spoken-form reply from the `time.now` Tool, plus the
routing path and a request id, and the reply is spoken on the configured Pulse
sink as it is synthesised (`"spoken": true`). Paraphrases work too, via the
`embed` MiniLM service that Compose starts alongside the assistant:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d '{"text":"what time is it right now"}'
```

Weather answers on the Fast path too. The assistant resolves the city from the
Pi's public IP at startup and every 24h (only city, region and timezone are
kept; coordinates are never stored) and asks Open-Meteo for current conditions:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' \
  -d "{\"text\":\"what's the weather like\"}"
```

If the IP lookup fails, the last known place is used, then the optional
`location.fallback` values in config, then no place at all.

Anything the Fast path does not handle goes to the Agent path, which needs an
OpenRouter key in `.env` (`OPENROUTER_API_KEY`); without one it answers with
the configured Unavailable message and the Fast path keeps working. Every
provider request forces OpenRouter's `provider.zdr` and
`provider.data_collection: "deny"` in code.
Both published ports are bound to the host loopback only. The debug listener
binds all interfaces inside the container (see `docker-compose.yml`), so the
host port mapping is what keeps it local.

The date, timer and audio Tools run on the Fast path too:

```sh
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"what day is it"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"set a timer for five minutes"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"how long is left on my timer"}'
curl -s -X POST localhost:8081/debug/transcript \
  -H 'Content-Type: application/json' -d '{"text":"turn the volume to 40"}'
```

Timers speak an alert through the same TTS voice when they fire. The Control
phrases `stop`, `pause`, `cancel` and `louder` are matched exactly, so they
answer without waiting on the embed service (`cancel` cancels the timer and
`louder` steps the volume up).

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
make test
python -m pytest tts/tests   # needs `pip install wyoming pytest numpy`
```

## Deploying

`make deploy` is the deployment path: it pulls the current branch, builds
SHA-tagged images, starts the stack and waits for health checks. Roll back with
`make rollback SHA=<commit>`, which reuses that commit's images without a
rebuild. The Pi provisioning checklist, image-tagging rules and the model-weights
layer convention are in [`docs/deploy.md`](docs/deploy.md).
