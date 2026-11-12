# Final fix pass — review resolution

Scope: the two review notes for map-1 (#16–#29), plus the tracked #33–#36
defects. This is a product/test/docs pass; it does not claim the human-mic,
live-OpenRouter or Pi acceptance runs.

## Spec-axis findings

| Finding | Disposition |
| --- | --- |
| **S1** malformed Tool arguments become `{}` and can run an act | Fixed at both layers. The Agent client keeps invalid streamed JSON out of the handler, marks the call (`CallRef.ArgsError`) and returns a JSON-valid `null` placeholder; `Registry.Execute`/`Propose` validate the generated input schema (type, required, unknown fields) before the handler. Empty arguments remain the legitimate no-argument default, so `timer.cancel {}` still cancels the timer and `audio.volume_set {}` still steps up. Regression tests cover truncated JSON, wrong type, missing field and unknown field, with a counter proving the handler never runs. |
| **S2** cancelled Agent turn logged as a provider failure | Fixed. `agent.failure` classifies a cancelled parent context as `Result.Cancelled`; `pipeline.callAgent` sets no `agent_error` and does not `MarkFailed`. The session outcome stays `cancelled`. Regression tests at the Agent and pipeline layers. Tracked as #37. |
| **S3** a confirm proposal abandons sibling calls | Fixed. The first confirm call is armed, all sibling read/act calls in the same model step run (reads concurrently, acts in model order), and the turn still stops with one pending confirmation. A realistic mixed-call test proves the siblings ran and the confirm handler did not. |
| **S4** Filler gated on text, not reply audio | Fixed. The sound slot is claimed immediately before the first PCM submission (a per-request hook on the speaker), not on a text delta. The slow-reply cue plays directly through the serial sink, so text at 1.4 s with TTS audio at 2.5 s gets one Filler at 1.5 s and no overlap; reply audio before 1.5 s suppresses it. Tests use a timed serial sink. |
| **S5** Response latency ends before the PCM is written | Fixed. `speech.Speaker.Say` marks the stage and `MarkReplyAudio` only after the first `Write` succeeds. A failed first write marks no audio. The stage comment, package comment and `docs/latency-dev-box.md` now state that the timestamp is the sink accepting the chunk, not the DAC emitting it; pacat's 100 ms buffer and host latency are unmeasured by this plumbing. |
| **S6** buffered chime echo can open the question | Fixed. Capture frames are timestamped. Frames captured while the chime plays are discarded, including frames still buffered when `chimeDone` closes; only frames captured after the recorded chime end are fed to STT and the endpointer. The 500 ms fixed endpointing window and post-chime user audio are unchanged. Test proves no utterance opens on buffered echo. |
| **S7** deploy/rollback leaves orphan services | Fixed. Both `up -d --build` and rollback `up -d --no-build` now pass `--remove-orphans`; the health wait already enumerates the checked-out compose file. A black-box deploy test rolls a three-service host back to a one-service commit and asserts orphan removal and the one-service wait. |
| **S8** debug listener reachable by siblings | Fixed with an explicit topology: the container still binds `:8081` because the host loopback port is published through Docker's DNAT, but the debug handler accepts only loopback or the container's default gateway (the host). `X-Forwarded-For` is ignored and the container subnet is not trusted. Verified in a real Compose project: host loopback 200, in-container loopback 200, sibling container 403. Unit tests cover loopback, gateway and sibling addresses. |
| **S9** routing eval skipped by default | Fixed. `make routing-eval ROUTING_EVAL_EMBED_URL=…` is the explicit integration target and fails when the URL is missing. The normal offline suite still skips the model eval deliberately. `docs/routing-eval.md` documents both. |
| **S10** embed image has no offline env vars | Fixed. `embed/Dockerfile` keeps the build-time prefetch and then sets `HF_HUB_OFFLINE=1` and `TRANSFORMERS_OFFLINE=1`, matching stt/tts. |

## Tracked defects

| Issue | Disposition |
| --- | --- |
| **#33** prefix punctuation | Fixed by sharing one normaliser and accepting ` `, `,`, `.`, `!`, `?` as separators after the prefix. "Agent. What time is it?" now strips the prefix and routes to the Agent path. Tests cover `.`, `!` and `?`. |
| **#34** timer punctuation | Fixed by trimming trailing sentence punctuation and collapsing whitespace before embedding. Case is preserved so names keep their signal. The two observed STT transcripts now pass; `make routing-eval` reports `timer.set` precision 1.0000, recall 0.4545 (was 0.40), and total precision 1.0000. |
| **#35** weather timeout | **Mitigated, not eliminated; the issue stays open.** Bounded at a 1.5 s per-call HTTP timeout and a 2 s total Tool budget (the `weather.New` defaults), with truthful failure text for timeouts/5xx and a small in-process TTL cache (10 min geocode, 1 min forecast) — no new persistence. Tests cover a stalled upstream, a 503 and a cached repeat. A live run answered Berlin inside the budget, but the final 20-scenario rerun still failed `weather-control` at the 2 s bound (`tool_execute_ms=2000`), so upstream availability is not claimed and weather is not "reliable" now. |
| **#36** noisy dev-box mic | Fixed as a configuration/calibration gap, not an adaptive endpointer. Added `scripts/mic-noise-check.sh` and `scripts/mic_noise.py`, a read-only measurement tool with offline tests, plus `docs/mic-calibration.md`. The fixed 500 ms window and -38 dBFS default are unchanged; the stale -45 dBFS comment is corrected. The human mic run remains outstanding. |

## Standards-axis judgement calls

- **Timeline.Response/responseLocked** duplicate: fixed; `Response` locks and
  calls `responseLocked`.
- **Two normalisers** (`pipeline.NormalizePhrase` vs `agent.normalisePrefixText`):
  replaced by `internal/phrase` (`Normalize` for routing comparisons,
  `TrimSentence` for the embedder so case is preserved).
- **`Pipeline.HandleStream`**: removed; the one test calls
  `HandleEvents{Delta: …}`.
- **`Registry.DefaultTimeout`**: removed; it had no caller.
- **`Provider.AlternateModel`**: kept, because ticket #20 explicitly records
  `deepseek/deepseek-v4-pro` as the *written* alternate. It is documented in
  `config.go` and `config.yaml` as a manual-switch value, never a runtime
  failover. This is the "documentation is better than deletion" case the brief
  allowed.
- **Stale comments** in `audio/sink.go` and `pipeline.go`: corrected. The
  `main.go` local `config` shadow was renamed `voiceCfg`.
- **Duplicated scenario table** in `docs/prototype-run.md`: kept as the
  historical narrative, with the generated file linked as authoritative; the
  final pass adds separate fixed evidence rather than rewriting history.
- **Two child-process wrappers, two Wyoming connection scaffolds, two bounded
  GET helpers, the 135-line `voice.Loop.session`, and the large
  `prototype_run.Run`**: retained. Each would need a real shared abstraction or
  a risky split; the duplication is mechanical and the fix pass prioritised the
  correctness findings. The one cheap duplication (three harness history
  helpers) was collapsed into `_has_message`.
- **Debug source check**: a concrete host-gateway topology was chosen over a
  token or header check because it does not require a secret, is not spoofable
  over TCP, and keeps `curl` on the host working. It is documented in
  `docker-compose.yml` and the API package.
