# Raspberry Pi 5 Dictation / STT Model Benchmark

**Date:** 2026-11-24  
**Host:** Raspberry Pi 5 (4× Cortex-A76 @ 2.4 GHz, 8 GB RAM, `aarch64`, Debian/Linux `6.12.115+rpt-rpi-2712`)  
**Condition:** Measured directly on the Pi 5 alongside the live `clanka-bot` Compose stack (9 healthy containers; `wakeword` continuously using ~29% of 1 core, `assistant` ~4.5%). Each model ran in an isolated subprocess with a warm-up pass, and cached weights were cleared between models.  
**Raw JSON data (desktop):** `/tmp/clanka-pi-stt-benchmark-2026-11-24-raw/`

---

## Executive Summary

- **Winner: NVIDIA Parakeet TDT 0.6B v2 (INT8) via `onnx-asr` at 3 threads.**
  - **0.45% overall WER** — **8× fewer word errors** than current `moonshine-small-streaming` (3.57% WER), with **0.0% WER** on room-noisy human speech, assistant commands, and long technical dictation, and **0/3** silence/noise hallucinations.
  - **0.133× RTF (7.5× faster than real time)** — **429 ms** short commands (p50), **664 ms** medium sentences (p50), **1,402 ms** long 8–14 s dictation (p50). Even without streaming, its short-command finalize time beats `moonshine-small-streaming`'s *paced live-streaming* finalize latency (552 ms p50 / 792 ms p95).
  - **1,148 MB peak RAM** (1,001 MB warm RSS) and **631 MB** model weights on disk (`encoder-model.int8.onnx` 622 MB + `decoder_joint-model.int8.onnx` 8.6 MB + `vocab.txt` 9 KB), well within the Pi 5's 4.5–5.0 GiB available RAM.
- **Low-RAM / Ultra-Fast Runner-Up: NVIDIA Parakeet TDT 110M (INT8) via `sherpa-onnx` at 2 threads.**
  - **2.23% overall WER** (beats both Moonshine Small 3.57% and Medium 2.53%), **0.052× RTF (19.2× faster than real time)** — **137 ms** short / **252 ms** medium / **614 ms** long — in **429 MB peak RAM** (321 MB warm RSS, less than Moonshine Small's 505 MB) and **130 MB disk**.

---

## Test Corpus (45 clips, 256.5 s total, 16 kHz mono PCM16)

1. **`clean_human` (12 clips):** Real human speech from LibriSpeech `test-clean` across distinct speakers (4 short 1.8–3.8 s, 4 medium 3.8–7.5 s, 4 long 7.5–14.5 s).
2. **`hard_human` (8 clips):** Real human speech from LibriSpeech `test-other` (harder accents, cadence, articulation).
3. **`room_noisy_human` (8 clips):** 4 `clean` + 4 `other` real human clips degraded with `ffmpeg` band-limiting (120–7200 Hz), mild room reflection (`aecho`), attenuation (~-24 dBFS), and OSM09-like pink room noise (~11–13 dB SNR).
4. **`assistant_cmd` (11 clips) & `dictation_long` (3 clips):** `#524` commands, proper names (`Spotify`, `Hermes`, `HappyLighting`, `Duo Plus`), numbers, negation, and 3 multi-clause technical dictation paragraphs (`GitHub`, `Tailscale`, `PostgreSQL`, `TypeScript`, `WebAssembly`, `PipeWire`, `Raspberry Pi`), synthesized offline via the live `clanka-bot-tts-1` Pocket TTS (`alba`).
5. **`noise_only` (3 clips):** 3 s digital silence, 4 s OSM09-like -39 dBFS pink noise, and 5 s -32 dBFS 60 Hz HVAC hum + brown noise to test hallucinations.

---

## Full Benchmark Results

| Model (Engine) | Threads | Overall WER | Clean / Hard / Noisy / Cmd / Dict | Burst RTF | Short p50 / p95 | Med p50 / p95 | Long p50 / p95 | Paced Finalize (p50 / p95 / overrun) | Peak RAM (Warm RSS) | Disk | Punct / Case | Noise Halluc. |
|---|---:|---:|---|---:|---|---|---|---|---:|---:|---|---:|
| **Parakeet TDT 0.6B v2 int8 (`onnx-asr`)** | **3** | **0.45%** | **0.4 / 1.7 / 0.0 / 0.0 / 0.0%** | **0.133** | **429 / 524 ms** | **664 / 1008 ms** | **1402 / 1836 ms** | Offline | **1148 MB** (1001) | 631 MB | Yes / Yes | **0/3** |
| Parakeet TDT 0.6B v2 int8 (`onnx-asr`) | 4 | 0.45% | 0.4 / 1.7 / 0.0 / 0.0 / 0.0% | 0.138 | 423 / 559 ms | 720 / 842 ms | 1463 / 2100 ms | Offline | 1145 MB (998) | 631 MB | Yes / Yes | 0/3 |
| Parakeet TDT 0.6B v2 int8 (`onnx-asr`) | 2 | 0.45% | 0.4 / 1.7 / 0.0 / 0.0 / 0.0% | 0.143 | 393 / 503 ms | 716 / 1007 ms | 1730 / 2128 ms | Offline | 1152 MB (1005) | 631 MB | Yes / Yes | 0/3 |
| Parakeet TDT 0.6B v2 int8 (`sherpa-onnx`) | 4 | 0.74% | 0.4 / 1.7 / 0.0 / 3.6 / 0.0% | 0.197 | 508 / 618 ms | 950 / 1701 ms | 2416 / 2993 ms | Offline | 1242 MB (1034) | 631 MB | Yes / Yes | 0/3 |
| **Parakeet TDT 110M int8 (`sherpa-onnx`)** | **2** | **2.23%** | **2.9 / 4.2 / 0.6 / 0.0 / 2.6%** | **0.052** | **137 / 186 ms** | **252 / 378 ms** | **614 / 789 ms** | Offline | **429 MB** (321) | 130 MB | Yes / Yes | **0/3** |
| Parakeet TDT 110M int8 (`sherpa-onnx`) | 3 | 2.23% | 2.9 / 4.2 / 0.6 / 0.0 / 2.6% | 0.054 | 153 / 213 ms | 264 / 462 ms | 613 / 809 ms | Offline | 477 MB (369) | 130 MB | Yes / Yes | 0/3 |
| Parakeet TDT 110M int8 (`sherpa-onnx`) | 4 | 2.23% | 2.9 / 4.2 / 0.6 / 0.0 / 2.6% | 0.062 | 175 / 226 ms | 317 / 412 ms | 767 / 966 ms | Offline | 424 MB (316) | 130 MB | Yes / Yes | 0/3 |
| Parakeet TDT 0.6B v3 int8 (`sherpa-onnx`) | 4 | 2.38% | 2.1 / 5.9 / 1.1 / 3.6 / 0.0% | 0.197 | 501 / 1003 ms | 930 / 1673 ms | 2124 / 3480 ms | Offline | 1296 MB (1089) | 640 MB | Yes / Yes | 0/3 |
| Moonshine Medium Streaming | 4 | 2.53% | 3.8 / 2.5 / 1.7 / 3.6 / 0.0% | 1.076 | 2543 / 3720 ms | 6270 / 11230 ms | 11665 / 16571 ms | 1159 / 2269 / **+3259 ms** | 836 MB (730) | 257 MB | Yes / Yes | 0/3 |
| Parakeet CTC 0.6B int8 (`onnx-asr`) | 4 | 2.53% | 1.7 / 3.4 / 2.2 / 5.4 / 2.6% | 0.121 | 362 / 464 ms | 571 / 729 ms | 1431 / 1701 ms | Offline | 1256 MB (1172) | 630 MB | No / No | 0/3 |
| Nemotron 3.5 Streaming 0.6B 560ms (`sherpa`) | 4 | 2.68% | 2.5 / 2.5 / 2.8 / 3.6 / 2.6% | 0.568 | 1527 / 2514 ms | 2928 / 4567 ms | 6401 / 7866 ms | 11 / 410 / +265 ms | 1247 MB (1247) | 653 MB | Yes / Yes | 0/3 |
| `whisper.cpp` `small.en-q5_1` | 4 | 3.12% | 3.8 / 5.9 / 1.7 / 3.6 / 0.0% | 1.811 | 10164 / 10401 ms | 10453 / 10849 ms | 11409 / 11687 ms | Offline (30 s pad) | 380 MB (346) | 181 MB | Yes / Yes | 3/3 |
| Parakeet TDT 0.6B Redux int8 (`sherpa`) | 4 | 3.42% | 3.8 / 3.4 / 2.2 / 3.6 / 5.1% | 0.186 | 487 / 621 ms | 893 / 1250 ms | 2215 / 2959 ms | Offline | 1258 MB (1051) | 640 MB | Yes / Yes | 0/3 |
| **Moonshine Small Streaming (current live)** | **4** | **3.57%** | **5.0 / 3.4 / 2.8 / 1.8 / 2.6%** | **0.891** | **2056 / 2959 ms** | **4919 / 8462 ms** | **11055 / 14269 ms** | **552 / 792 / +792 ms** | **505 MB** (443) | 136 MB | Yes / Yes | **0/3** |
| Moonshine Small (`update_interval=30s`) | 4 | 3.57% | 5.0 / 5.1 / 1.7 / 1.8 / 2.6% | 0.458 | 1000 / 1391 ms | 2580 / 3532 ms | 5518 / 7498 ms | 2143 / 7490 / +0.2 ms | 528 MB (434) | 136 MB | Yes / Yes | 0/3 |
| `faster-whisper` `distil-small.en` int8 | 4 | 3.72% | 2.9 / 5.9 / 2.2 / 0.0 / 9.0% | 1.173 | 6000 / 6378 ms | 6839 / 7605 ms | 8700 / 9156 ms | Offline | 756 MB (605) | 957 MB | Yes / Yes | 1/3 |
| Zipformer Kroko 2025 Streaming (`sherpa`) | 4 | 4.46% | 5.8 / 7.6 / 1.7 / 3.6 / 2.6% | 0.098 | 252 / 332 ms | 598 / 674 ms | 1197 / 1419 ms | 1.4 / 97 / +60 ms | 205 MB (203) | 68 MB | Yes / Yes | 0/3 |
| NeMo Streaming FastConformer 480ms (`sherpa`) | 4 | 4.76% | 2.9 / 7.6 / 1.1 / 10.7 / 10.3% | 0.204 | 516 / 736 ms | 1126 / 1892 ms | 2434 / 3210 ms | 1.3 / 86 / +35 ms | 331 MB (331) | 131 MB | No / No | 0/3 |
| `whisper.cpp` `base.en-q5_1` | 4 | 5.51% | 5.0 / 9.3 / 4.4 / 7.1 / 2.6% | 0.494 | 2688 / 3002 ms | 2761 / 3039 ms | 3140 / 3619 ms | Offline | 188 MB (150) | 57 MB | Yes / Yes | 2/3 |
| `faster-whisper` `base.en` int8 | 4 | 5.65% | 5.0 / 12.7 / 3.9 / 3.6 / 2.6% | 0.523 | 2535 / 2756 ms | 3030 / 4026 ms | 4314 / 4719 ms | Offline | 541 MB (432) | 420 MB | Yes / Yes | 1/3 |
| Moonshine Tiny Streaming | 4 | 7.74% | 7.9 / 15.2 / 6.1 / 1.8 / 3.9% | 0.381 | 727 / 1374 ms | 1936 / 2802 ms | 4618 / 7123 ms | 207 / 457 / +65 ms | 245 MB (199) | 43 MB | Yes / Yes | 0/3 |
| SenseVoice Small int8 (`sherpa`) | 4 | 20.39% | 17.1 / 15.2 / 13.9 / 12.5 / 59.0% | 0.066 | 170 / 256 ms | 308 / 548 ms | 780 / 1057 ms | Offline | 565 MB (539) | 233 MB | No / No | 3/3 |
| `whisper.cpp` `small.en-q5_1` (`-ac 512`) | 4 | 28.57% | 35.4 / 15.2 / 46.1 / 3.6 / 5.1% | 0.974 | 3099 / 3362 ms | 3423 / 3728 ms | 5429 / 28852 ms | Offline (breaks >10 s) | 347 MB (312) | 181 MB | Yes / Yes | 3/3 |

---

## Root Causes of Current Lackluster Dictation

1. **Moonshine internal VAD line-splitting & stuttering:** On multi-clause speech, both `small-streaming` and `medium-streaming` split sentences mid-utterance and duplicate words across line boundaries (e.g., `"on the part of his enemy. His enemies, that he knew..."`, `"the French, in return, in. and invited..."`), and drop/garble words under room noise (`"amidst the tents"` -> `"and makes the tents"`, `"Duo Plus"` -> `"dual plus"`, `"PipeWire"` -> `"Pipwire"`).
2. **`PARTIAL_INTERVAL = 0.2` overhead (`stt/moonshine_engine.py:20`):** Go's Wyoming client ignores partial transcripts except as an error fallback, yet Moonshine runs full autoregressive decodes every 200 ms. That doubles burst/Dashboard latency (`0.891×` vs `0.458×` RTF; 11.1 s vs 5.5 s on long clips) and causes 792 ms CPU overrun during live streaming.
3. **Why other model families lost:**
   - **Whisper (`whisper.cpp` / `faster-whisper`):** Fixed 30 s encoder window costs 2.5–10.2 s even on 2 s commands, plus `[BLANK_AUDIO]` / noise hallucinations. `--audio-ctx 512` breaks WER (28.6%).
   - **Streaming Transducers (Nemotron 3.5 0.6B, FastConformer, Zipformer):** Fast end-of-speech finalize (1–11 ms), but 6×–10× higher WER than Parakeet TDT 0.6B v2 and slower burst speed.
   - **Parakeet v3 / Redux:** Worse English WER (2.38% / 3.42%) than v2 (0.45%) at identical cost.
   - **SenseVoice Small int8:** Fast (`0.066×` RTF) but unusable on English dictation (20.4% overall / 59.0% long dictation WER, 3/3 hallucinations).
4. **Pi endpointing mismatch (`assistant/internal/voice/endpoint.go`):** Raw OSM09 idle floor is `-39.2 dBFS` (peaks to `-28.2 dBFS`), just 1.2 dB below the fixed `-38 dBFS` RMS endpointer, with a 500 ms silence cutoff. That both cuts off thinking pauses during dictation and feeds ambient room noise into STT (observed in live Pi logs).

---

## Side-by-Side Transcript Samples

- **`ls_clean_04` (13 s human):**
  - Expected: `...on the part of his enemies that he knew were in no degree inferior to his own`
  - **Parakeet 0.6B v2 (`1402 ms`):** `...on the part of his enemies that he knew were in no degree inferior to his own.` *(0.0% WER)*
  - **Moonshine Small (`13599 ms`):** `...on the part of his enemy. His enemies, that he knew were in no degree inferior to his own.` *(5.0% WER)*
- **`noisy_ls_clean_01` (room reverb + noise):**
  - Expected: `Concord returned to its place amidst the tents`
  - **Parakeet 0.6B v2 (`428 ms`):** `Concord returned to its place amidst the tents.` *(0.0% WER)*
  - **Moonshine Small (`2833 ms`):** `Concord returned to its place and makes the tents.` *(25.0% WER)*
- **`dict_technical_3` (technical dictation):**
  - Expected: `Search for open source TypeScript projects using WebAssembly for local audio processing, specifically anything compatible with PipeWire on Linux.`
  - **Parakeet 0.6B v2 (`858 ms`):** `Search for open source TypeScript projects using WebAssembly for local audio processing, specifically anything compatible with Pipewire on Linux.` *(0.0% WER)*
  - **Parakeet 110M (`378 ms`):** `Search for open source type script projects using Web Assembly for local audio processing, specifically anything compatible with Pipewire on Linux.` *(0.0% norm WER)*
  - **Moonshine Small (`8462 ms`):** `...specifically anything compatible with Pipwire on Linux.` *(9.1% WER)*
- **`dict_technical_1` (long technical dictation):**
  - Expected: `...GitHub pull request for Tailscale authentication and check the PostgreSQL migration logs before deploying to the Raspberry Pi.`
  - **Parakeet 0.6B v2 (`1100 ms`):** `...GitHub pull request for Tailscale authentication and check the PostgreSQL migration logs before deploying to the Raspberry Pi.` *(0.0% WER)*
  - **Parakeet 110M (`478 ms`):** `...GitHub Pull request for tailscale authentication and check the postgrescool migration logs...` *(6.7% WER)*

---

## Recommended Next Course of Action

1. **Migrate `stt/` to `onnx-asr` + `nemo-parakeet-tdt-0.6b-v2` (INT8, `intra_op_num_threads=3`)** behind the existing Wyoming server in `stt/stt_server.py` (buffer `audio-chunk` PCM until `audio-stop`, run one `recognize()` call; bake the 631 MB weights at image build time). Keep **`sherpa-onnx` Parakeet TDT 110M int8** (2 threads, 429 MB RAM, 130 MB disk) as the low-RAM fallback.
2. **Prune Pi Docker build cache before deploying:** Pi `/` is 89% full (6.4 GB free) with 16.6 GB Docker build cache (9.6 GB reclaimable). Run `docker builder prune` / `scripts/prune-images.sh` before building the new STT image.
3. **Upgrade endpointing (`assistant/internal/voice/endpoint.go`):** Replace or gate the fixed `-38 dBFS` RMS detector with **Silero VAD** (~1.8 MB ONNX, <1 ms/frame) and increase trailing silence (700–900 ms or utterance-length-adaptive) so dictation pauses are not clipped and room noise is rejected.
4. **Owner-voice verification (#524):** Record 10–15 OSM09 room clips into `stt/fixtures/private/`, run `stt/benchmark.py`, and confirm whether `STT_CORRECT=1` is still needed for custom names (`HappyLighting`, `Duo+`).
