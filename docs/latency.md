# Response latency and per-stage timings

Every spoken request writes one `request` log line with its stage timings and a
request ID shared by the voice loop, the reply session, the Agent path and
playback. The definition of Response latency is the glossary one:

> **Response latency**: the time from the user's last spoken sound to the first
> sound of the Assistant's actual reply. Chimes and fillers played while
> waiting don't count as the reply.

The start point is the arrival time of the last frame the endpointer counted as
speech. The end point is the first PCM sample of reply text written to the
sink. A Filler is cached PCM played on a separate path and never sets the end
point; the wake chime does not either.

## Stages

| Log key (`stages`) | Owner | Measured from → to |
| --- | --- | --- |
| `endpointing_ms` | voice loop | last voiced frame → endpointer fires (the fixed silence window plus one frame) |
| `stt_finalize_ms` | voice loop | endpointer fires → final transcript |
| `routing_ms` | pipeline | transcript → path chosen (Confirmation, Control phrase, semantic match or Agent path) |
| `tool_execute_ms` | pipeline / Agent path | total time inside Tool handlers, summed over every call and Agent step |
| `agent_first_token_ms` | Agent path | model request sent → first token carrying content or a Tool call (role-only events do not count) |
| `tts_first_audio_ms` | speech | first reply text handed to TTS → first PCM sample written to the sink |

`checkpoints.first_chunk_ms` is the first reply text unit handed to TTS,
measured from the turn start. When a Filler is playing (the Agent path cue, or
the slow-reply cue after 1.5 s), the first reply text waits for it, so that wait
is included: the Filler is excluded from the end point, but not from the elapsed
time.

`response_ms` is Response latency. It is present only for a voice request whose
reply audio actually started.

## Missing stages and outcomes

A stage that was not measured is **absent**, never `0`: a zero would read as
instant instead of unknown. Comparing a stage's sample count with the path's
response sample count shows what was missing.

`outcome` classifies the turn:

| Outcome | Meaning |
| --- | --- |
| `ok` | reply audio started |
| `no_audio` | the turn finished without reply audio (typed entry, empty reply) |
| `failed` | the request failed before its answer, including a spoken Unavailable message |
| `cancelled` | the request context was cancelled (Barge-in, shutdown) |

Failed and cancelled turns keep any timing they did measure, but the summarizer
counts them only in the attempt tally; they never enter the success
percentiles. A failed Agent call also logs `agent_error=unavailable|timeout`.

The typed `/debug/transcript` entry has `source=debug`: it has no endpointing
or STT stages and no `response_ms`, because there was no speech to start from.
The authenticated Dashboard typed box has `source=dashboard` and the same
limitation: it never claims a Response latency.

## Activity page

`/activity` is a bounded in-memory ring (100 requests) fed by the same
`Timeline.Complete` event that writes the line above. It is not a second
tracing protocol and it does not tail a log file. It shows the route, the
measured stage timings, the Tool calls and the outcome. Missing stages are
shown as absent, never as `0`, and `response_ms` is shown only when the
timeline actually measured reply audio. A typed request therefore shows its
routing and Tool stages and "Not measured", not an HTTP round-trip time.

The ring also carries the reply text and each Tool call's human-facing result,
including Local-only results. That is display-only data for the authenticated
Dashboard: it is not in the request log line, not persisted, and not available
to the Agent prompt builder, history sent off the Pi, Notifications or the
public health listener. The page caps its display at 50 rows; the ring drops
the oldest entries beyond 100.

## Budgets (#12)

| Path | p50 | p95 |
| --- | --- | --- |
| Fast path (including Control phrase and Confirmation answers) | 1.2 s | 2 s |
| Agent path, no Tool calls | 2.5 s | 4 s |

If Agent `agent_first_token_ms` p50 is above 1.2 s, question the model choice
first; the report flags it automatically.

## Reading the logs

The stack's logs go to stdout as JSON. Summarize the running stack:

```sh
make latency-report
```

or point the command at a saved log:

```sh
cd assistant
go run ./cmd/latency-report -file /path/to/assistant.log -source voice
```

The command prints nearest-rank p50/p95 per path, marks each path against its
budget, and shows a sample count for every stage. Use `-source debug` for typed
entries or `-source ''` for everything.

`assistant/cmd/latency-say` and `scripts/latency/fake-provider.py` are the
measurement harness used for the dev-box report; neither is part of the running
assistant.

## Measurement scope

Timings are software-side: the reply end point is the first PCM write to
`pacat`, so Pulse/PipeWire sink buffering (configured with `--latency-msec`) is
not included. Endpointing is the arrival time of the last voiced frame, not the
moment the sound left the speaker. For a Pi-vs-dev-box comparison, note the
capture path and the Tool's network latency separately.
