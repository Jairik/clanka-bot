# Dev-box Response latency check (#27)

Date: 2026-11-11. Branch: `jj/request-stage-latency-observability` at
`5fbaab5` plus the #27 changes.

| | |
| --- | --- |
| Box | Intel i7-8700, 12 threads, Arch Linux, kernel 7.2.15, Docker 29.9.3 |
| Audio | host PipeWire 1.6.11 through its PulseAudio socket |
| Services | all five Compose services from this tree, real wakeword (openWakeWord `okay_nabu`), STT (Moonshine `small-streaming`), TTS (Pocket TTS `alba`) and assistant |
| Requests | 21 successful Fast path turns (15 main batch + 6 extra weather), 1 failed Fast path turn, 6 Agent path turns against a fake provider |
| Speaker | **none**: the "user" is Pocket TTS audio routed into the assistant's capture source |

## How it was measured

There was no human available, so the spoken requests were synthesized and
looped back through the real services. Two host null sinks were created with
`pactl load-module module-null-sink`: `clanka_lat_in` fed the assistant's
capture source (`clanka_lat_in.monitor`) and `clanka_lat_out` received playback,
so no audio reached the room and the assistant never heard its own reply.

`assistant/cmd/latency-say` synthesizes a phrase through the real TTS service
and writes raw PCM. Wake phrase and query were played into the null sink as:

```
"okay nabu"  +  1.6 s silence  +  <query>  +  1.5 s silence
```

The silence gap lets the wake chime finish before the query starts, because the
endpointer ignores audio until the chime drains.

The stack ran as an isolated Compose project (`clanka-lat27`, host ports 19080
and 19081) from images tagged `lat27`. The wake-word threshold was lowered to
`0.05` so TTS-spoken "okay nabu" triggers reliably; spoken-query audio and the
wake model are the only concessions. Everything from wake detection onward is
the production code path.

Queries: "what time is it", "what's the time", "tell me the time" (Control
phrase), "what time is it right now" (semantic Fast path), "what's the weather
like" (Fast path Tool over the network).

The Agent path has no OpenRouter key on this box, so it was driven by a fake
OpenAI-compatible SSE provider on the Compose network: 300 ms to the first
token, then one token every 30 ms, a fixed two-sentence answer. **The Agent
numbers below are instrumentation evidence, not model latency.**

Raw request lines and the summarizer output for each batch are in
[`docs/latency/2026-11-11/`](latency/2026-11-11/).

## Fast path

15 successful requests (one more, a weather timeout, is counted as failed and
is excluded from these percentiles):

| Group | n | response p50 | response p95 | budget p50 / p95 |
| --- | --- | --- | --- | --- |
| Time, Control phrase | 9 | 782 ms | 840 ms | 1.2 s / 2 s |
| Time, semantic match | 3 | 744 ms | 780 ms | 1.2 s / 2 s |
| Weather Tool (two batches) | 9 | 1.144 s | 1.181 s | 1.2 s / 2 s |
| **All successful Fast path** | **15** | **782 ms** | **1.158 s** | **1.2 s / 2 s** |

Stage breakdown for the 15 successful requests:

| Stage | n | p50 | p95 |
| --- | --- | --- | --- |
| endpointing | 15 | 512 ms | 522 ms |
| stt_finalize | 15 | 165 ms | 200 ms |
| routing | 15 | 0 ms | 65 ms |
| tool_execute | 15 | 0 ms | 274 ms |
| tts_first_audio | 15 | 106 ms | 214 ms |
| first_chunk (checkpoint) | 15 | 678 ms | 987 ms |
| **response_ms** | **15** | **782 ms** | **1.158 s** |

The stages add up: for the median time request, 512 + 165 + 0 + 0 + 106 ≈ 783 ms,
which matches `response_ms` 782 ms. There is no material gap between stages.

The fixed 500 ms endpointing window is about 65 % of the Fast path budget. STT
finalisation, TTS startup and local Tool execution are all under 220 ms at p95.

### Over budget: the weather Tool, not the pipeline

One of four weather requests in the main batch hit the Tool's 8 s timeout and
is logged `outcome=failed` with `tool_execute_ms=8000`; it is not in the table
above. The committed request line is
[`failure-request.jsonl`](latency/2026-11-11/failure-request.jsonl) and
[`fast-requests.jsonl`](latency/2026-11-11/fast-requests.jsonl) holds the
timeout.

The weather successes in the final run were ~1.1–1.2 s, but earlier in the same
session the same Open-Meteo call returned in 5.1–5.6 s, pushing Response latency
to 5.9–6.4 s. The Tool is network-bound and can blow the 2 s p95 on its own, so
**the Fast path budget is conditional on Tool network latency**. The pipeline's
own contribution stays under 1 s regardless. Fixing or caching weather is out of
scope for #27.

## Agent path (fake provider)

6 requests, all `agent_path`, all with reply audio:

| Stage | n | p50 | p95 |
| --- | --- | --- | --- |
| endpointing | 6 | 512 ms | 522 ms |
| stt_finalize | 6 | 118 ms | 252 ms |
| routing | 6 | 0 ms | 0 ms |
| agent_first_token | 6 | **301 ms** | 303 ms |
| first_chunk (checkpoint) | 6 | 1.965 s | 2.076 s |
| tts_first_audio | 6 | 98 ms | 116 ms |
| **response_ms** | **6** | **2.081 s** | **2.189 s** |

This is within the 2.5 s / 4 s Agent budget, but it says nothing about a real
model: 301 ms is the delay the fake provider was configured to inject. The
measured first chunk is dominated by the cached Agent path Filler (~1.36 s of
"One moment.") that plays before the first generated sentence; any real
time-to-first-token lands on top of that.

**Model choice is not assessed.** There is no provider key on this box, so the
`agent_first_token_ms` p50 > 1.2 s model-selection check has not been run
against a real model. The instrumentation will flag it, and it stays open as
acceptance work.

## Missing stages, failed and cancelled turns

Missing stages are absent from `stages`, never zero. A failed model call with a
spoken Unavailable message demonstrates both:

```json
{"level":"ERROR","msg":"request","request_id":"9359aaf6e0cfe305de7b7991274f4ad2",
 "path":"agent_path","outcome":"failed","response_ms":2909,
 "stages":{"endpointing_ms":501,"routing_ms":0,"stt_finalize_ms":77,"tts_first_audio_ms":115},
 "checkpoints":{"first_chunk_ms":2793},"agent_error":"unavailable"}
```

`agent_first_token_ms` is missing rather than 0, the turn is `failed` even
though the apology was spoken, and `response_ms` is kept as real evidence while
being excluded from the success percentiles.

Cancellation (Barge-in) cannot be triggered on this branch because #25 is still
on a sibling branch; it is covered by unit tests that cancel a turn mid-playback
and assert `outcome=cancelled` with no `response_ms`.

## Acceptance status

- [x] Every request logs per-stage timings, correlated by `request_id` across
  voice, session, Agent path and playback.
- [x] Response latency start/end points match the glossary: last voiced frame to
  first actual reply sample, with chimes and Fillers excluded.
- [x] A small command summarizes p50/p95 from the logs:
  `make latency-report` (or `go run ./cmd/latency-report -file <log>`).
- [x] Dev-box numbers above, with the weather Tool called out as the
  over-budget stage.
- [ ] Human-spoken request set: this run uses synthesized loopback audio. The
  stages it exercises are real, but a human pass is still the acceptance run.
- [ ] Live Agent path against a real model: blocked on an OpenRouter key; the
  fake-provider numbers above must not be read as model latency.

## Reproduce

```sh
# Isolated project, alternate ports, null-sink loopback:
pactl load-module module-null-sink sink_name=clanka_lat_in
pactl load-module module-null-sink sink_name=clanka_lat_out
GIT_SHA=lat27 ASSISTANT_HTTP_PORT=19080 ASSISTANT_DEBUG_PORT=19081 \
  ASSISTANT_PULSE_SOURCE=clanka_lat_in.monitor ASSISTANT_PULSE_SINK=clanka_lat_out \
  WAKEWORD_THRESHOLD=0.05 docker compose -p clanka-lat27 up -d --build

# Summarize (from the repo root):
make latency-report
```

Synthesize the wake phrase and a query (publish the TTS port with a local
Compose override first, `127.0.0.1:20200:10200`):

```sh
cd assistant
go run ./cmd/latency-say -addr 127.0.0.1:20200 -text "okay nabu" -out wake.pcm
go run ./cmd/latency-say -addr 127.0.0.1:20200 -text "what time is it" -out q.pcm
```

For the Agent path, put `scripts/latency/fake-provider.py` on the Compose
network and point a copy of `assistant/config.yaml` at
`http://<fake>:8000/v1` (mount it read-only as `ASSISTANT_CONFIG`, with
`FAKE_KEY` set); it is a test double only, never a source of model numbers.
