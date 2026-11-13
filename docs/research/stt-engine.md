# STT engine: Moonshine vs whisper.cpp

Research for [#2](https://github.com/Jairik/clanka-bot/issues/2). Researched 2026-11-11.
Versions checked: Moonshine Voice **v0.1.6** (2026-11-06, models on CDN `quantized_26_11_02`),
whisper.cpp **v1.9.5** (2026-10-06).

## Question

Which speech-to-text engine and model size should the voice pipeline use? Compare Moonshine
and whisper.cpp (and anything clearly better) on accuracy for short commands, end-of-speech to
transcript latency on a Pi 5, RAM, streaming support, invocation (native lib / CLI / Python),
and arm64 Docker friendliness for the local prototype.

## Recommendation

**Use Moonshine Voice with the English Small Streaming model (123M params) as the default,
with Tiny Streaming (34M) as the fallback if CPU headroom on the Pi gets tight.** Run it as a
small Python service (`pip install moonshine-voice`) that the Go middleware talks to over a
local socket/HTTP, rather than binding the C API through cgo for the first prototype.

Why:

- It is the only option of the two built for live, phrase-length speech. Streaming models
  encode audio while the user is still talking and cache encoder/decoder state, so the work left
  after end-of-speech is small. whisper.cpp runs Whisper, which works on a fixed 30 s window and
  re-does all the work for each call.
- Moonshine's own Pi 5 numbers put end-of-speech-to-transcript at **527 ms for Small Streaming**
  and **237 ms for Tiny Streaming**. Whisper of comparable accuracy measured the same way is
  in the 5–10 s range on a Pi 5. The 2–4 s end-to-end target can't absorb that.
- Accuracy is equal or better at each size: Small Streaming 7.84% WER vs Whisper Small 8.59%;
  Tiny Streaming 12.00% vs Whisper Tiny 12.81%. Medium Streaming (6.65%) beats Whisper Large v3
  (7.44%).
- Prebuilt arm64 Linux wheels and shared libraries are published, and there's a Pi-specific
  quickstart. The library includes a VAD segmenter, so end-of-speech detection comes with it.
- It's MIT-licensed (library and the English streaming models).

Medium Streaming (802 ms on Pi 5) is the step up if Small's accuracy turns out not to be good
enough for commands. It still fits the budget, but it leaves less CPU free for TTS and the LLM.

Keep **whisper.cpp** as the fallback only. It has a mature C API, official Go bindings,
multi-arch Docker images and a built-in HTTP server. But it isn't a streaming model, and its
latency on Pi-class CPUs is several seconds for anything above tiny.

## Comparison

| Criterion | Moonshine Voice (Small / Tiny Streaming) | whisper.cpp (Whisper tiny/base/small) |
|---|---|---|
| Accuracy (Open ASR Leaderboard avg WER, English) | Medium 6.65%, Small 7.84%, Tiny 12.00% [1][2] | Whisper Large v3 7.44%, Small 8.59%, Tiny 12.81% (as reported by Moonshine) [1] |
| Pi 5 latency, end-of-speech → final transcript | Medium 802 ms, Small 527 ms, Tiny 237 ms [1] | Measured by Moonshine using **faster-whisper**, not whisper.cpp: Small 10,397 ms, Tiny 5,863 ms [1][3]. No first-party Pi 5 whisper.cpp numbers found. Older community `bench` results: Pi 4 tiny.en encode ~7.9–9.5 s, RK3588 (roughly Pi 5 class, A76 cores) tiny ~2.7–2.9 s, base ~6.2 s [5] |
| RAM | Not documented. Estimate from int8 weights: Tiny ~35 MB, Small ~125 MB, Medium ~250 MB of weights. Models are memory-mapped `.ort` files [2][4] | Documented: tiny ~273 MB, base ~388 MB, small ~852 MB, medium ~2.1 GB (quantized models use less) [6] |
| Streaming | Native. Incremental audio, cached encoder and partial decoder state, partial transcripts as you speak, default update interval 0.5 s [1][3] | No streaming model. `whisper-stream` example re-transcribes a sliding window (e.g. `--step 500 --length 5000`) [6] |
| VAD / endpointing | Built-in VAD segmenter, used in its own benchmarks [3] | `--vad` with a Silero VAD model (e.g. silero-v6.2.0) [6] |
| Invocation | Python (`pip install moonshine-voice`, CLI `moonshine-voice mic`); portable C++ core with a C API; Swift, Java/Android, WASM bindings. No official Go binding (cgo over the C API would be needed) [4][7] | C/C++ lib, `whisper-cli`, `whisper-server` (HTTP, OpenAI-like API), **official Go bindings** (`bindings/go`) [6] |
| arm64 / Docker | PyPI wheels for `manylinux_2_31_aarch64`, `manylinux_2_34_aarch64`, `manylinux_2_34_x86_64` (v0.1.6) [8]; prebuilt arm64 + x86_64 Linux shared libs [7]. No official Docker image, but `python:3.x-slim` plus pip should work on both arches | Official multi-arch image `ghcr.io/ggml-org/whisper.cpp:main` (`linux/amd64` + `linux/arm64`) [6] |
| Runtime | ONNX Runtime, CPU [1] | ggml, CPU with NEON; optional BLAS / GPU backends [6] |
| License | MIT (library + all streaming models) [4] | MIT (code); Whisper weights MIT |

### Anything clearly better?

**sherpa-onnx** ([k2-fsa/sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)) is the main
alternative worth knowing about. It's a runtime, not a model. It has official Go bindings, runs
on Raspberry Pi, and supports streaming Zipformer transducers as well as non-streaming Moonshine
(v1 tiny) and Parakeet TDT 0.6B [9]. Its Go API could remove the Python sidecar. But its
Moonshine support is the older non-streaming v1 model, and its streaming English Zipformers have
no published accuracy numbers comparable to the ones above. It isn't clearly better for this
project. Consider it only if the Python sidecar becomes a problem. Parakeet TDT 0.6B is
accurate but non-streaming and about 2.5x the size of Moonshine Medium, so it isn't a good fit
for Pi latency.

## Implications for the pipeline

- The 2–4 s budget becomes about 0.25–0.8 s for STT, which leaves most of the budget for the
  LLM's first token and TTS.
- Moonshine's VAD can provide end-of-speech detection, so a separate VAD may not be needed
  (still to be decided in the voice pipeline ticket).
- For the Docker prototype, run the same Python service image natively on x86_64 for dev and on
  arm64 on the Pi. Wheels exist for both, so no QEMU emulation is needed during development.
- Moonshine also ships TTS (Piper, Kokoro) and an `AgentFlow` conversational layer [10]. These
  aren't in scope here, but they're relevant to the TTS ticket.

## Open uncertainties

- **Pi 5 numbers are vendor-published and possibly stale.** Moonshine's docs say their Linux
  x86 and Pi 5 columns "were last taken before the build-optimization fix ... so they read
  pessimistically" [3]. They were measured on `two_cities.wav` (read speech), not short
  commands, and not under concurrent LLM/TTS load.
- **No first-party whisper.cpp Pi 5 benchmark exists.** The Whisper column in Moonshine's table
  uses faster-whisper, and whisper.cpp could be somewhat faster. Nothing published suggests it
  closes a 10–20x gap, though. whisper.cpp's `--audio-ctx` (shrinking the 30 s window) is an
  unbenchmarked way to lower its latency.
- **Moonshine RAM is not published.** The figures above are estimates from parameter count ×
  int8. Measure RSS on the Pi.
- **Short-command accuracy** (names, "turn on X", music titles) isn't covered by the
  leaderboard WER. Moonshine supports domain customization and LoRA fine-tuning if needed [10].
- **Go integration path** (Python sidecar vs cgo vs sherpa-onnx) is an interface decision for
  the middleware ticket, not settled here.
- Moonshine is pre-1.0 (v0.1.x) and changes quickly (6 releases since 2026-08). Pin the
  version and the model CDN directory.

## Sources

1. Moonshine, "When should you choose Moonshine over Whisper?" (WER, params, per-device latency table) — https://github.com/moonshine-ai/moonshine/blob/main/docs/moonshine-vs-whisper.md (read 2026-11-11, repo at v0.1.6)
2. Moonshine, Available Models — https://github.com/moonshine-ai/moonshine/blob/main/docs/models/available-models.md
3. Moonshine, Benchmarks (methodology: end-of-speech-to-transcript latency, faster-whisper on CPU for the Whisper rows, staleness note for Pi 5) — https://github.com/moonshine-ai/moonshine/blob/main/docs/using/benchmarks.md
4. Moonshine README and Quantization docs (MIT license, int8 per-channel quantization, memory-mappable `.ort`) — https://github.com/moonshine-ai/moonshine and https://github.com/moonshine-ai/moonshine/blob/main/docs/models/quantization.md ; CHANGELOG https://github.com/moonshine-ai/moonshine/blob/main/CHANGELOGS.md
5. whisper.cpp benchmark results thread (community `bench` numbers, Pi 4 / RK3588) — https://github.com/ggml-org/whisper.cpp/issues/89 (2022–2023 comments; old builds)
6. whisper.cpp README (memory table, whisper-stream, whisper-server, VAD, Docker arches, Go bindings, quantization) — https://github.com/ggml-org/whisper.cpp ; releases https://github.com/ggml-org/whisper.cpp/releases (v1.9.5, 2026-10-06)
7. Moonshine Quickstart (Linux arm64/x86_64 prebuilt libs, Raspberry Pi pip install) — https://github.com/moonshine-ai/moonshine/blob/main/docs/quickstart.md ; docs site https://moonshine-voice.readthedocs.io/
8. PyPI `moonshine-voice` 0.1.6 wheel list — https://pypi.org/project/moonshine-voice/
9. sherpa-onnx README (languages incl. Go, Raspberry Pi, streaming models, Moonshine tiny, Parakeet) — https://github.com/k2-fsa/sherpa-onnx
10. Moonshine papers: v1 https://arxiv.org/abs/2410.15608 ; "Flavors of Moonshine" https://arxiv.org/abs/2509.02523 ; domain customization https://github.com/moonshine-ai/moonshine/blob/main/docs/models/domain-customization.md
