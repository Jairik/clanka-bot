# Prototype run on the dev PC (#29)

Date: 2026-11-11. Branch: `jj/voice-pipeline-loopback-test` (integration
`7b16587` plus the #29 harness). Box: Intel i7-8700, 12 threads, Arch Linux,
Docker 29.9.3, PipeWire 1.6.11.

## What this run proves, and what it does not

Real: all five Compose services — `assistant`, openWakeWord `okay_nabu`, Moonshine
`small-streaming`, Pocket TTS `alba`, MiniLM `embed` — plus the wake word, the
500 ms endpointer, STT, both routing paths, Tool execution, the Agent loop,
sentence-streamed TTS, Pulse playback, and one real Barge-in cancellation.

Not real:

- **No human spoke.** Every utterance is Pocket TTS audio played into a null
  sink whose monitor feeds the assistant's capture source. This is synthesized
  loopback audio, **not human speech**. The human-spoken pass is still the
  acceptance run.
- **No OpenRouter key.** The Agent path talks to
  `scripts/prototype-run/fake_provider.py`, a scripted OpenAI-compatible test
  double on the Compose network. Its replies and delays are scripted, and the
  **Agent-path latencies below are instrumentation, not model latency**.
- **No one listened.** Reply audio was captured from the output null sink and
  measured for presence and duration. The routing verdicts come from the
  assistant's request log and the test double's request log, not from hearing.

The mic was only checked passively for availability and idle noise; see
[`prototype-run/mic-noise.txt`](prototype-run/mic-noise.txt) and #36.

## Isolation and honesty of the setup

The run used its own Compose project `clanka-e2e29`, loopback ports 19180/19181
(assistant), 19200 (TTS) and 19300 (STT), and its own two null sinks
`clanka29_in` (capture) and `clanka29_out` (playback). It never stopped a
container it did not start, never changed the host default sink or source, and
tore its project and sinks down at the end. The fake provider is a sixth,
clearly labelled service in the test overlay; the generated config points the
Agent path at `http://fakeopenai:8000/v1` and nowhere else.

## Scenario results

20 scripted turns: **17 pass, 3 fail**. `Observed route` is
`path/tool/outcome`; `Response` is Response latency from the request log.

| Scenario | Said (synthesized) | Observed route | STT transcript | Response | #12 budget | Reply audio | Verdict |
| --- | --- | --- | --- | ---: | --- | ---: | --- |
| control-time | what time is it | control_phrase/time.now/ok 'What time is it?' | What time is it? | 628 ms | within p50 | 0.74 s | PASS |
| semantic-time | what time is it right now | fast_path/time.now/ok 'What time is it right now?' | What time is it right now? | 827 ms | within p50 | 0.74 s | PASS |
| date | what's today's date | fast_path/date.today/ok "What's today's date?" | What's today's date? | 836 ms | within p50 | 2.37 s | PASS |
| timer-set-words | set a timer for ten minutes | agent_path/-/ok 'Set a timer for ten minutes.' | Set a timer for ten minutes. | 2167 ms | within p50 | 2.30 s | FAIL |
| timer-set-seconds-miss | set a timer for ten seconds | agent_path/-/ok 'Set a timer for ten seconds.' | Set a timer for ten seconds. | 2208 ms | within p50 | 1.92 s | FAIL |
| agent-timer-set | agent please run a timer test for five minutes | agent_path/timer.set/ok 'Agent, please run a timer test for five minutes.' | Agent, please run a timer test for five minutes. | 2109 ms | within p50 | 7.97 s | PASS |
| timer-list | how long is left on my timer | fast_path/timer.list/ok 'How long is left on my timer?' | How long is left on my timer? | 901 ms | within p50 | 1.86 s | PASS |
| timer-cancel | cancel the timer | fast_path/timer.cancel/ok 'Cancel the timer.' | Cancel the timer. | 896 ms | within p50 | 0.77 s | PASS |
| volume-set | set the volume to thirty percent | fast_path/audio.volume_set/ok 'Set the volume to thirty percent.' | Set the volume to thirty percent. | 822 ms | within p50 | 1.09 s | PASS |
| volume-up | louder | control_phrase/audio.volume_set/ok 'Louder.' | Louder. | 715 ms | within p50 | 0.22 s | PASS |
| audio-stop | stop | control_phrase/audio.stop/ok 'Stop.' | Stop. | 674 ms | within p50 | 0.29 s | PASS |
| weather-control | what's the weather like | control_phrase/weather.current/failed "What's the weather like?" | What's the weather like? | 6462 ms | OVER p95 | 1.44 s | FAIL |
| weather-city | what's the weather in Berlin | fast_path/weather.current/ok "What's the weather in Berlin?" | What's the weather in Berlin? | 896 ms | within p50 | 0.35 s | PASS |
| agent-question | agent what is the capital of France | agent_path/-/ok 'Agent, what is the capital of France?' | Agent, what is the capital of France? | 2235 ms | within p50 | 2.59 s | PASS |
| agent-history | agent what did I ask before | agent_path/-/ok 'Agent, what did I ask before?' | Agent, what did I ask before? | 2145 ms | within p50 | 1.57 s | PASS |
| agent-tool | agent what time is it | agent_path/time.now/ok 'Agent. What time is it?' | Agent. What time is it? | 2174 ms | within p50 | 4.13 s | PASS |
| barge-in | tell me a long story | agent_path/-/cancelled 'Tell me a long story.'; control_phrase/time.now/ok 'What time is it?' | What time is it? | 2219/656 ms | within p50 | 4.42 s | PASS |
| provider-failure | agent trigger a provider failure | agent_path/-/failed 'Agent trigger a provider failure.' | Agent trigger a provider failure. | 2143 ms | within p50 | 1.02 s | PASS |
| embed-stopped | what's the temperature outside | agent_path/-/ok "What's the temperature outside?" | What's the temperature outside? | 2232 ms | within p50 | 2.18 s | PASS |
| agent-timer-fire | agent please run a timer test for ten seconds | agent_path/timer.set/ok 'Agent, please run a timer test for ten seconds.' | Agent, please run a timer test for ten seconds. | 2138 ms | within p50 | 8.45 s | PASS |

Notes:

- **barge-in** has two request lines: the slow Agent reply was cancelled 2219 ms
  after the last voiced frame (reply audio had started); the wake word that
  barged in also started the next turn, a 656 ms Control-phrase reply. The
  `voice: barge-in stopped` line recorded `stop_ms=0`.
- **provider-failure** is an HTTP 401 from the test double, the same
  provider-error path a bad key takes. The turn is `outcome=failed` with
  `agent_error=unavailable` and the configured Unavailable message was spoken.
- **embed-stopped** ran with the `embed` container stopped:
  `fallthrough_reason=embed_failed`, then the Agent path answered.
- **agent-timer-set / agent-timer-fire** exercise the Timer Tool and its alert
  through the Agent path because the Fast path cannot reach `timer.set` with
  STT punctuation (see F2). The Timer manager logged `timer set` (300 s),
  `timer cancelled`, then `timer set` (10 s) and `timer fired` ten seconds
  later.
- The fake double's final tool answers include the Tool result JSON, which is
  why `agent-timer-*` reply audio is long. That text is the double's, not a
  model's.

## Response latency against the #12 budgets

From [`prototype-run/latency-report.txt`](prototype-run/latency-report.txt);
only `ok` requests with reply audio are sampled. Failed, cancelled and
audio-less turns are counted separately.

| Path | n | p50 | p95 | Budget p50 / p95 |
| --- | ---: | ---: | ---: | --- |
| Control phrase | 4 | 656 ms | 715 ms | 1.2 s / 2 s |
| Semantic Fast path | 6 | 836 ms | 901 ms | 1.2 s / 2 s |
| Agent path (fake provider) | 8 | 2167 ms | 2235 ms | 2.5 s / 4 s |

Excluded: the weather timeout (6462 ms, failed), the barge-in cancellation
(2219 ms, cancelled), and the provider failure (2143 ms, failed).

The pipeline is well inside the budgets on this box. Endpointing is the floor:
p50 ~505 ms, over half the Fast path p50 budget. STT finalisation is 137 ms,
routing 0–21 ms, TTS first audio 111–125 ms. The weather Tool is the only
component that can miss the budget on its own (#35).

**The Agent numbers are not model latency.** The first token p50 of 201 ms is
the delay the fake provider was told to inject; the first chunk p50 of 2.065 s
is the cached "One moment." Filler plus the first scripted sentence. A live
model still has to be measured (blocked, below).

## Findings

### F1 — the Agent request prefix fails when STT adds a full stop (#33)

"Agent. What time is it?" did **not** take the prefix branch. The request line
`a6f73832b1f24909c0ebfd7300186a96` has `fallthrough_reason=below_threshold`
and `top_tools[0]=time.now 0.689`; a prefix-routed turn has an empty reason.
The test double received "Agent. What time is it?" unchanged. A comma
transcript ("Agent, what is it?") matches and is stripped, so the same
mechanism passed and failed within this run depending on punctuation. The
prefix is supposed to guarantee the Agent path; today it only looks right when
the Fast path independently misses.

### F2 — `timer.set` cannot reach the Fast path with a trailing period (#34)

`timer.set` is semantic-only. "Set a timer for ten minutes." scored 0.786 with
margin 0.122 < 0.23; "…ten seconds." scored 0.736 with margin 0.063. Without
the full stop the first scores 1.000 with margin 0.359. Moonshine adds a full
stop to every transcript, so no timer phrasing clears the margin in this
voice path. The two observed transcripts are now in the routing fixture; the
real-MiniLM evaluation reproduces them as misses while every Tool keeps
precision 1.0000 ([`prototype-run/routing-eval.txt`](prototype-run/routing-eval.txt)).

### F3 — `weather.current` timed out on Open-Meteo (#35)

The weather Control phrase ran the Tool for 5654 ms and failed at 6462 ms
(`context deadline exceeded` for the forecast call); the Berlin request in the
same run succeeded in 896 ms. This is the network-bound Tool, not routing, and
it exceeds the Fast path p95 on its own.

### F4 — idle webcam-mic noise is above the endpointer threshold (#36)

Passive samples of the Webcam C270 Mono measured idle RMS of -37.9, -33.6 and
-29.7 dBFS; the 4 s sample had 120/125 32 ms frames above the configured
-38 dBFS. The source is at 100% volume (base 32% / -30 dB). Every sample's mean
is above the threshold, even the quietest one
([`prototype-run/mic-noise-samples.txt`](prototype-run/mic-noise-samples.txt)).
A human wake on this setting would open an utterance on noise and only stop at
`max_utterance` (15 s). This blocks the human acceptance run until the gain or
threshold is fixed.

### F5 — a Barge-in cancellation is logged as `agent_error=unavailable`

Request `a4fd88e351050afcac033a8c3f0e1adb` is a real Barge-in
(`outcome=cancelled`, `error: speech: context canceled`, 2219 ms) but also
carries `agent_error=unavailable`, and the Agent logs
`ERROR msg="agent request failed" status=0 error="context canceled"`
([`prototype-run/assistant-evidence.log`](prototype-run/assistant-evidence.log)).
`agent.failure` treats any non-timeout stream error as unavailable, including
`context.Canceled`, so an interruption is indistinguishable from a provider
failure in the log. One-line fix for the single final fix agent: return a
cancellation result for `context.Canceled`/`DeadlineExceeded` in
`agent.failure` and cover it with a test. Not filed as a separate issue
because the scenario itself passed.

## Routing fixture and the real-MiniLM evaluation

Both observed timer misses were added to `positives.timer.set` exactly as STT
produced them (`Set a timer for ten minutes.`, `Set a timer for ten seconds.`),
so the corpus reproduces the live miss rather than hiding behind unpunctuated
text. Real `sentence-transformers/all-MiniLM-L6-v2`, threshold 0.71 / margin
0.23:

| Tool | Positives | TP | FP | FN | Precision | Recall |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| `time.now` | 20 | 13 | 0 | 7 | 1.0000 | 0.650 |
| `date.today` | 20 | 18 | 0 | 2 | 1.0000 | 0.900 |
| `weather.current` | 20 | 11 | 0 | 9 | 1.0000 | 0.550 |
| `audio.volume_set` | 20 | 15 | 0 | 5 | 1.0000 | 0.750 |
| `audio.stop` | 20 | 9 | 0 | 11 | 1.0000 | 0.450 |
| `timer.set` | 22 | 8 | 0 | 14 | 1.0000 | 0.364 |
| `timer.cancel` | 20 | 15 | 0 | 5 | 1.0000 | 0.750 |
| `timer.list` | 20 | 5 | 0 | 15 | 1.0000 | 0.250 |
| **Total** | **162** | **94** | **0** | **68** | **1.0000** | **0.580** |

Negatives: 0 of 48 misrouted. Compounds: 0 of 8 misrouted. The 0.95 per-Tool
precision floor holds; recall is deliberately not gated, and the two new cases
lower `timer.set` recall from 0.40 to 0.364 rather than being deleted.

## Blocked on a human or a live key

| Item | State | Why |
| --- | --- | --- |
| Human-spoken scripted set | **BLOCKED** | No person available; idle mic noise is above the endpointer threshold (#36) |
| Live Agent path on OpenRouter | **BLOCKED** | No key supplied (the secure request timed out); fake-provider numbers are not model latency |
| Pi 5 + Echo Dot run | Out of scope | Later step, not #29 |

### Human checklist

1. Fix the mic margin first: `pactl set-source-volume
   alsa_input.usb-046d_0825_EC865510-02.mono-fallback 32%` (or a measured
   value), then re-run the passive check until idle RMS is at most about
   -45 dBFS:
   `parec --device=<source> --rate=16000 --channels=1 --format=s16le --raw | …`
   Record the chosen gain/threshold in the ticket.
2. Start the real stack with the real mic and speakers:
   `make up` (leave `ASSISTANT_PULSE_SOURCE`/`SINK` empty). Ask the 20 scripted
   utterances from the table above, plus at least one unscripted follow-up.
3. After the pass, `make latency-report` and compare with the budgets above.
   A human pass should confirm endpointing and the Fast path p50/p95, not just
   the routing.
4. Exercise a real-room Barge-in: wake the assistant, let the reply start, then
   say "okay nabu" and a second request. Check `voice: barge-in stopped` and
   `outcome=cancelled`, and listen for the follow-up.

### Live-key checklist

1. Put a real `OPENROUTER_API_KEY` in `.env` and use the unmodified
   `assistant/config.yaml` (do **not** mount the generated fake-provider
   config).
2. `make up`, then ask an Agent question and a follow-up that needs history.
3. Confirm `agent_first_token_ms` p50 ≤ 1.2 s and the Agent response p50 ≤
   2.5 s, and that `agent_error` is absent. If the first token is over
   budget, question the model choice before anything else (#12).
4. Repeat the provider-failure test with a deliberately bad key; the spoken
   Unavailable message and `agent_error=unavailable` should match this run.

## Reproduce

```sh
# Harness unit tests (pure seams only)
python3 -m unittest discover -s scripts/prototype-run/tests -v

# Full isolated run; artifacts land in scripts/prototype-run/.run/
python3 scripts/prototype-run/prototype_run.py --publish docs/prototype-run

# Real-MiniLM routing evaluation (see docs/routing-eval.md for the embed start)
cd assistant
ROUTING_EVAL_EMBED_URL=http://127.0.0.1:18085 \
  go test ./internal/pipeline -run TestRoutingEvalAgainstMiniLM -count=1 -v
```

The harness creates and removes its own null sinks and Compose project. It
prints the per-scenario verdicts, writes `scenario-results.{md,json}`,
`latency-report.txt`, `requests.jsonl`, `assistant-evidence.log`,
`fake-requests.jsonl` and `mic-noise.txt`, and exits non-zero if any scenario
fails.

## Evidence

All in [`docs/prototype-run/`](prototype-run/):

- `scenario-results.md` / `scenario-results.json` — per-scenario verdicts and timings
- `requests.jsonl` — every request line from this run (correlated by `request_id`)
- `assistant-evidence.log` — request, voice, Agent and Timer lifecycle lines
- `fake-requests.jsonl` — the test double's view, including history and Tool results
- `latency-report.txt` — the #12 p50/p95 summary
- `routing-eval.txt` — real-MiniLM evaluation with the two new cases
- `mic-noise.txt` — passive mic availability/noise sample
- `mic-noise-samples.txt` — all four passive samples from the session
