# TTS engine: Kokoro vs Pocket TTS vs Kitten TTS

Research for [#3](https://github.com/Jairik/clanka-bot/issues/3), part of the map [#1](https://github.com/Jairik/clanka-bot/issues/1).
Researched 2026-11-11. Versions checked: Pocket TTS v4.0.1 (2026-10-21), Kokoro-82M v1.0 (`kokoro` 0.9.4 on PyPI, Apr 2025), KittenTTS 0.8.1 for the light models (Feb 2026) and `kittenml` 0.9.7 for Kitten-TTS 2.

## Question

Which text-to-speech engine should the Assistant use? The ticket compares Kokoro, Pocket TTS and Kitten TTS on these points:

- time to first audio on a Pi 5-class CPU
- streaming output
- voice quality
- voice customisation and cloning (a custom persona voice is a later goal)
- RAM
- licence
- how it is invoked (Python, ONNX or native)

## Recommendation

**Use Pocket TTS (Kyutai), the 100M English model, run as a long-lived local service.**

- It is the only one of the three built for streaming, low-latency CPU inference: ~200 ms to the first audio chunk and 2 CPU cores, per the maintainer.
- Zero-shot voice cloning is built in, which covers the later persona-voice goal without changing engines.
- There is an official native Rust binary for Linux ARM, which needs no Python or PyTorch. That suits a Docker image that has to move onto a Pi 5.
- It is actively maintained (v4.0.1, Oct 2026).

**Fallback: Kokoro-82M** if Pocket TTS turns out too slow on the real Pi or its voice quality disappoints. Kokoro has the most permissive licence (Apache-2.0 weights, no gate) and a well-known quality bar. But it has no voice cloning, and it only streams per sentence or segment. The only first-party Pi benchmark is on a Pi 4, where it runs about 3x *slower* than real time on 4 threads.

**Kitten TTS is not recommended.** Its light 0.8 ONNX models (15–80M) are tiny and Apache-2.0, but they have only 8 fixed voices, no cloning and no streaming. Kitten-TTS 2 adds cloning, but it is a 1.7B model that needs about 6 GB of RAM on CPU. Its weights use a custom community licence with a revenue/funding cap. It is the wrong size for a Pi 5 that also runs STT and the Fast path.

Pocket TTS's own small English models (18.2M / 6.9M / 1.9M, Oct 2026) are a further fallback if the 100M model misses the latency target. They run on the same engine and the same API.

## Comparison

| | **Pocket TTS** (Kyutai) | **Kokoro-82M** (hexgrad) | **Kitten TTS 0.8** (nano/micro/mini) | **Kitten-TTS 2** |
|---|---|---|---|---|
| Size | ~100M; English small variants 18.2M / 6.9M / 1.9M | 82M; ONNX model.onnx ~310 MB fp32, ~80 MB quantized | 15M / 40M / 80M; 25–80 MB | 1.7B (ternary/1-bit LM); ~950 MB weights |
| First-party CPU speed | ~6x real time on a MacBook Air M4 CPU with 2 cores; ~2.3–2.5x real time on a 4-vCPU x86 cloud VM; ~200 ms to the first chunk | No official numbers. sherpa-onnx (k2-fsa) on a **Raspberry Pi 4**: RTF 3.19 (v1.0) / 2.77 (v0.19) at 4 threads, i.e. ~3x slower than real time | "Runs without a GPU"; no numbers published | "Runs real time on a CPU" (via the llama.cpp fork); no numbers published |
| Pi 5 time to first audio | **Not measured by anyone first-party.** Estimate: Pi 5 (4x Cortex-A76) is weaker per core than an M4 or a modern x86 VM, so expect ~1–2x real time and well under 1 s to first chunk. Needs an on-device check. | Estimate: Pi 5 is ~2–3x a Pi 4, so RTF ≈ 1–1.6. A whole first sentence must finish before playback, so ~1.5–3 s for a typical reply sentence. Marginal against the 2–4 s budget. | Likely fastest (15M), but no streaming, so first audio comes after the whole utterance | Not viable on Pi 5 CPU (6 GB RAM, 1.7B params) |
| Streaming | **Yes, native audio streaming** (frame-level, autoregressive) | Per-segment generator only (`KPipeline` yields one chunk per text split) | No (returns the full array) | Chunk-level only. Text under ~380 chars yields a single chunk, so short replies don't stream. |
| Voice cloning / customisation | **Zero-shot cloning** from a WAV/MP3; export to safetensors for fast reload. 20+ catalog voices with per-voice licences. Model terms forbid cloning without consent. | None. 54 preset voices (v1.0); voice tensors can be blended; no cloning. | None. 8 fixed voices, speed control. | Cloning from 5–30 s of reference audio; 115 voices; emotion tags (beta) |
| Languages | EN, FR, DE, PT, IT, ES, NL (+ community) | EN (US/GB), ES, FR, HI, IT, JA, PT-BR, ZH | English | 20 languages |
| RAM | Not stated. A 100M model is small; the Python install pulls PyTorch (~200 MB CPU wheel; ~3 GB if the CUDA build is pulled by mistake). The Rust binary avoids PyTorch. | Not stated. ~300 MB ONNX model plus runtime. | Not stated; 25–80 MB model files | ~6 GB for CPU (stated) |
| Licence | Code MIT. Weights **CC-BY-4.0, gated on HF** (accept terms; attribution). Voices have individual licences. | Code and weights **Apache-2.0**, ungated | Code Apache-2.0; 0.8 weights **Apache-2.0** | Code Apache-2.0; weights **Stellon Labs Community License** (free under USD 1M revenue/funding; attribution) |
| Invocation | Python (`pip install pocket-tts`, PyTorch 2.5+), CLI, `pocket-tts serve` HTTP server, **official Rust binary (Linux ARM, CPU)**. Community: ONNX export, PocketTTS.cpp, sherpa-onnx (int8 release 2026-01-26, Go bindings), Wyoming container | Python `kokoro` (PyTorch + misaki G2P + espeak-ng); community ONNX (`kokoro-onnx`, MIT); sherpa-onnx (incl. Go) | Python `kittenml` + ONNX Runtime + phonemizer/espeak-ng | Python (PyTorch), `kitten_tts_cpp` (llama.cpp fork, GGUF), MLX, vLLM |
| Maintenance | Very active (v4.0.1, 2026-10-21; tech reports Jan, Sep, Oct 2026) | Repo last pushed Aug 2025; PyPI 0.9.4 Apr 2025 | Active (0.8.1 Feb 2026; repo pushed Oct 2026) | Same repo |

### Voice quality

None of the maintainers publish a head-to-head comparison of these three engines. What the primary sources do say:

- **Kokoro.** Widely used, with 12.5M+ downloads of the HF model. It has no diffusion component (StyleTTS 2 + ISTFTNet).
- **Pocket TTS.** Kyutai pitches it as "high-quality TTS with voice cloning", backed by a paper ([arXiv 2509.06926](https://arxiv.org/abs/2509.06926)). The README also warns that cloning reproduces the audio quality of the sample, so clean the sample first.
- **Kitten 0.8.** Described as "lightweight"; there is no quality claim beyond that.

Treat quality as a listening test to run during the prototype. Kyutai's web demo and the Kokoro HF Space allow side-by-side listening with no install.

## How this fits the Assistant

- **Latency budget (2–4 s from end of speech to first audio).** STT and the Fast path eat most of the budget, so TTS must start fast. A streaming engine (Pocket) starts audio after ~1 frame. A per-sentence engine (Kokoro) must synthesise the whole first sentence first. With Kokoro, the middleware would have to split the LLM output into sentences and pipeline them. That is also worth doing with Pocket, but there it is not critical.
- **Docker prototype → Pi.** Pocket's Rust binary ships for Linux x86 and ARM and needs no Python or PyTorch, so the same image layout works on the dev box and the Pi. The `serve` command keeps the model and voice state warm. Its HTTP interface is easy for Go to call; check whether it streams over HTTP, or use a thin wrapper.
- **Persona voice.** Clone from a clean reference recording, then `export-voice` to safetensors so it loads instantly. Use a voice you have rights to; the model terms prohibit non-consensual cloning.
- **Privacy.** All three run fully offline once weights are cached. The Pocket weights need a one-time HF login to accept the gate, so bake them into the image or a volume at build time.

## Open uncertainties

1. **No first-party Pi 5 numbers exist for any of the three.** The Pi 5 estimates above are extrapolated from an M4, a 4-vCPU x86 VM and a Pi 4. A 10-minute benchmark on the actual Pi 5 should confirm Pocket's RTF and first-chunk latency before locking it in. The map lists on-device benchmarks as out of scope for the prototype, so flag this for the Pi phase.
2. **RAM for each engine is not published.** Measure RSS of `pocket-tts serve` (Python and Rust) in the Docker prototype.
3. **Python vs Rust parity.** The Rust binary's audio is not bit-identical to the Python build's, and the README does not say whether it is as fast. It also does not say whether the HTTP server streams audio chunks to the client.
4. **CC-BY-4.0 gated weights.** Fine for a personal, self-hosted assistant, but each voice in the catalog carries its own licence. Check the chosen built-in voice on [kyutai/tts-voices](https://huggingface.co/kyutai/tts-voices).
5. **Small Pocket models (18M/7M/2M).** These are new (Oct 2026). Kyutai's blog post on them could not be read in full, so their quality and cloning trade-offs against the 100M model are unverified.
6. **Kokoro v1.0 vs v1.1 / int8 speed on ARM.** No first-party RTF table covers these.

## Sources

- Pocket TTS README (v4.0.1): https://github.com/kyutai-labs/pocket-tts (releases: https://github.com/kyutai-labs/pocket-tts/releases)
- Pocket TTS model card (CC-BY-4.0, gated, prohibited-use policy): https://huggingface.co/kyutai/pocket-tts
- Pocket TTS tech report (Jan 2026): https://kyutai.org/blog/2026-01-13-pocket-tts; small models (Oct 2026): https://kyutai.org/blog/2026-10-20-pocket-tts-small
- Pocket TTS voice licences: https://huggingface.co/kyutai/tts-voices
- sherpa-onnx PocketTTS docs: https://k2-fsa.github.io/sherpa/onnx/tts/pocket.html
- Kokoro-82M model card (v1.0, 2025-01-27; Apache-2.0): https://huggingface.co/hexgrad/Kokoro-82M
- Kokoro inference library: https://github.com/hexgrad/kokoro
- kokoro-onnx: https://github.com/thewh1teagle/kokoro-onnx
- sherpa-onnx Kokoro docs with Raspberry Pi 4 RTF tables: https://k2-fsa.github.io/sherpa/onnx/tts/pretrained_models/kokoro.html
- KittenTTS README (Kitten-TTS 2 and 0.8): https://github.com/KittenML/KittenTTS
- KittenTTS lightweight ONNX models doc: https://github.com/KittenML/KittenTTS/blob/main/docs/onnx-models.md
- Kitten TTS nano 0.8 card: https://huggingface.co/KittenML/kitten-tts-nano-0.8
- Kitten-TTS 2 card and Stellon Labs Community License: https://huggingface.co/KittenML/kitten-tts-2
