# Research: Wake word engine (openWakeWord with the webcam mic)

Ticket: [#4](https://github.com/Jairik/clanka-bot/issues/4) · Map: [#1](https://github.com/Jairik/clanka-bot/issues/1) · Researched 2026-11-11

## Question

Is openWakeWord the right wake-word engine, and what does it take to run it on a webcam microphone? Cover CPU cost on a Pi 5, training or using a custom wake word, false-trigger behaviour, alternatives (Porcupine, microWakeWord), and how to get mic audio into a Docker container on a Linux desktop for local development.

## Recommendation

**Use openWakeWord, run as its own container through the maintained `rhasspy/wyoming-openwakeword` image.** It runs on the `pyopen-wakeword` TFLite runtime and is reached over the Wyoming TCP protocol on port 10400. The Go middleware streams 16 kHz mono 16-bit PCM to it and gets back a "detected" event. Treat the engine as swappable: `rhasspy/wyoming-microwakeword` exposes the same protocol, so microWakeWord can be dropped in later if it measures cheaper or more accurate.

- Use a pre-trained model for the prototype: `hey_jarvis`, `alexa`, `hey_mycroft` or `okay_nabu`. Train a custom name later with the openWakeWord Colab notebook, which produces a `.tflite` file that you mount with `--custom-model-dir`.
- Don't use Porcupine. Picovoice is B2B, has no personal free plan, has no Go SDK, and validates the AccessKey online. That conflicts with the privacy and offline goals.
- On the desktop prototype, get the webcam mic into containers by bind-mounting the host's PulseAudio/PipeWire socket. This is the pattern the official Linux Voice Assistant compose file uses. `--device /dev/snd` with raw ALSA is the fallback.

## Comparison

| | openWakeWord | microWakeWord | Porcupine (Picovoice) |
|---|---|---|---|
| License | Code Apache-2.0; **pre-trained models CC BY-NC-SA 4.0** (non-commercial) [1] | Apache-2.0 [5][6] | SDK Apache-2.0, but the engine needs a Picovoice AccessKey [8][9] |
| Latest | Upstream lib v0.6.0 (2024-02-11). Last commit 2025-12-30, which moved tflite to `ai-edge-litert` and required Python 3.10+ [2]. Runtime fork `pyopen-wakeword` v1.1.0 (2025-10-28) [3] | `pymicro-wakeword` v2.5.0 (2026-10-27) [6]; models released through 2026-07 [5] | v4.0 (2025-12-11) [8] |
| Pi CPU | "A single core of a Raspberry Pi 3 can run 15-20 openWakeWord models simultaneously in real-time" [1]. No official Pi 5 figure | No published figure. Built for microcontrollers (ESP32-S3 in Home Assistant Voice PE), so it is lighter than oWW [5] | Vendor benchmark measures on Pi 5, but the chart only covers PocketSphinx and Snowboy, not oWW [10] |
| Accuracy target | <5% false-reject, <0.5 false-accepts/hour after threshold tuning. FA measured on ~5.5 h of the DiPCo far-field corpus [1] | No published numbers. The author says good models are hard to train [5] | Vendor claims 11x more accurate than PocketSphinx/Snowboy [8]. oWW's own README charts `alexa` and `hey_mycroft` against Porcupine [1] |
| Custom word | Colab notebook, "<1 hour", synthetic TTS data. An advanced notebook (`automatic_model_training.ipynb`) is available for better models [1][2] | Training notebook exists but "will likely not produce a usable model" without expertise [5] | Picovoice Console (web, account required) [8] |
| Docker/protocol | `rhasspy/wyoming-openwakeword` v2.1.0, Wyoming on :10400, `--custom-model-dir`, `--refractory-seconds` [4] | `rhasspy/wyoming-microwakeword` v2.1.0, same port and protocol [7] | No first-party container. No Go SDK (SDKs: Python, C, Node, Java, .NET, Rust and others) [8] |
| Offline/privacy | Fully local | Fully local | Inference on-device, but the AccessKey is validated against Picovoice servers [9][11] |

## Key facts

### CPU cost on a Pi 5
- The only first-party number is the Pi 3 one above [1]. The Pi 5's Cortex-A76 cores are much faster than the Pi 3's A53 cores, so a single model should use a small fraction of one core. **This is an inference; nobody has measured it here.** The map defers on-device benchmarking.
- Frame size trades efficiency against latency. oWW expects 16 kHz 16-bit PCM in multiples of 80 ms, and longer frames are cheaper but add detection latency [1]. `pyopen-wakeword` and `pymicro-wakeword` take 10 ms (160-sample) chunks [3][6].
- openWakeWord is *not* suited to microcontrollers [1]. That doesn't matter for a Pi.

### Custom wake word
- The openWakeWord Colab notebook is quick and needs no development experience, but the README warns that "performance of the model may be low in some deployment scenarios." The shipped models used ~30,000 hours of negative audio, and the README recommends thousands of positive examples [1].
- A community `.tflite` collection (fwartner/home-assistant-wakewords-collection) is linked from the Wyoming README. Wyoming loads custom models with `--custom-model-dir /custom` [4].
- **License caveat:** the shipped oWW models are CC BY-NC-SA 4.0 [1]. That is fine for a personal assistant, but it matters if the project is ever commercialised. Models you train yourself avoid this.

### False triggers
- The default threshold is 0.5. The README recommends tuning it per environment [1].
- Built-in mitigations [1]:
  - Optional Silero VAD gate (`vad_threshold`), which suppresses activations on non-speech noise.
  - Optional Speex noise suppression (`enable_speex_noise_suppression=True`), Linux x86/arm64 only, which needs libspeexdsp.
- Wyoming adds `--refractory-seconds` to stop the detector re-firing right after a detection [4].
- A webcam mic is a single, near-field-tuned mic with no hardware echo cancellation. The Linux Voice Assistant docs recommend far-field arrays with an XMOS DSP (AEC, noise suppression, AGC) for good results [12]. Expect more false rejects across a room and false triggers from the Assistant's own TTS. Mute or ignore wake detection while TTS is playing.

### Alternatives
- **microWakeWord** is Home Assistant's on-device engine and the newest active work: `okay_nabu` multilingual model, 2026 releases [5][6]. It is a good drop-in candidate with the same Wyoming interface, but it has fewer models and is harder to train custom.
- **Porcupine**:
  - Picovoice says "there are no dedicated free or paid plans for personal or non-commercial use"; the Free Trial is one-time [9].
  - The AccessKey is checked with Picovoice servers [11].
  - A Home Assistant community thread reports that free-tier AccessKeys stopped working after 2026-06-30 [13]. This is a secondary source and unverified.
  - It is ruled out on privacy, offline and cost grounds.
- **Linux Voice Assistant** (OHF-Voice, v1.1.18, 2026-10-29) supports both oWW and mWW in Docker on amd64/aarch64 [12]. It speaks the ESPHome API to Home Assistant, not to a custom Go middleware, so treat it as a reference implementation, not a dependency. Its predecessor `wyoming-satellite` was archived on 2026-01-27 [14].

### Webcam mic into Docker (Linux desktop)
1. **PulseAudio/PipeWire socket (preferred).** This is what the official LVA `docker-compose.yml` does [12]:
   - mount `/run/user/1000` into the container
   - set `PULSE_SERVER=/run/user/1000/pulse/native` and `PULSE_COOKIE=/run/user/1000/pulse/cookie`
   - add `group_add: [audio]` and run as uid 1000

   PipeWire's `pipewire-pulse` serves the same socket. The host keeps sharing the webcam mic with other apps, and the sound server handles resampling.
2. **Raw ALSA (fallback).** Use `--device /dev/snd` plus the `audio` group, and capture with `arecord -D plughw:<card> -r 16000 -c 1 -f S16_LE -t raw` (`plughw:` resamples; list devices with `arecord -L`) [14]. This takes exclusive control of the device away from the desktop.
3. Only one container should own the mic: an audio-capture service or the middleware. It streams PCM to the wake-word container over Wyoming TCP. The wake-word container itself needs no audio device.

## Open uncertainties
- **Pi 5 CPU, latency and false-accept rate with this webcam** have not been measured. They need a short on-device test, which the map currently defers.
- **Who owns the mic.** Either the Go middleware captures audio (for example with an ALSA/Pulse library) and streams to Wyoming, or a small capture sidecar does. This affects the voice-pipeline contract in #1.
- **Go Wyoming client.** No official Go library was found. The protocol is simple (JSON header line plus binary payload over TCP), but a client has to be written or found.
- **Echo/barge-in.** How to suppress self-triggering from TTS without hardware AEC (mute during playback, or software AEC via PipeWire's echo-cancel module).
- **Upstream openWakeWord cadence.** The last tagged release was 2024-02. The maintained runtime path is the Rhasspy/OHF fork (`pyopen-wakeword`), which supports pre-trained and custom `.tflite` models. Confirm that models from the upstream training notebook load unchanged.
- **Porcupine free-tier end date** is from a forum post, not an official Picovoice statement.

## Sources
1. openWakeWord README: https://github.com/dscripka/openWakeWord (v0.6.0, accessed 2026-11-11)
2. openWakeWord releases/commits: https://github.com/dscripka/openWakeWord/releases ; https://github.com/dscripka/openWakeWord/commits/main
3. pyopen-wakeword: https://github.com/rhasspy/pyopen-wakeword (v1.1.0, 2025-10-28)
4. wyoming-openwakeword README + CHANGELOG: https://github.com/rhasspy/wyoming-openwakeword (v2.1.0, 2025-10-28)
5. microWakeWord: https://github.com/kahrendt/microWakeWord (model releases 2026-07)
6. pymicro-wakeword: https://github.com/OHF-Voice/pymicro-wakeword (v2.5.0, 2026-10-27)
7. wyoming-microwakeword: https://github.com/rhasspy/wyoming-microwakeword (v2.1.0, 2025-10-30)
8. Porcupine: https://github.com/Picovoice/porcupine (v4.0, 2025-12-11)
9. Picovoice FAQ: https://picovoice.ai/docs/faq/general/
10. Picovoice wake-word benchmark (vendor): https://github.com/Picovoice/wake-word-benchmark
11. Picovoice AccessKey validation: https://picovoice.ai/docs/faq/general/ (as surfaced by search; also quoted in [13])
12. Linux Voice Assistant README + docker-compose.yml: https://github.com/OHF-Voice/linux-voice-assistant (v1.1.18, 2026-10-29)
13. HA community (secondary): https://community.home-assistant.io/t/fyi-picovoice-confirmed-free-tier-accesskeys-will-stop-working-after-june-30-2026/1012744
14. wyoming-satellite (archived 2026-01-27): https://github.com/rhasspy/wyoming-satellite
