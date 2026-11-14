# Local Pi emulation with Docker

Research for issue #7 (parent map: #1). Researched 2026-11-11. Sources were read on that date; versions are noted where the source gives them.

## Question

How should the local development environment emulate the Pi 5 so a working prototype runs on the Linux desktop (Arch) first? Cover:

- arm64 via QEMU/buildx vs native amd64 images built from the same Dockerfiles
- the performance cost of emulation for ML inference
- getting microphone input and speaker output into containers (ALSA, PulseAudio, PipeWire)
- what can't be emulated (Bluetooth to the Echo Dot)
- a docker compose layout for the Go and ML services

## Recommendation

**Run native amd64 images day to day. Use arm64 under QEMU only as a build and smoke-test check, and don't use it for the hot path.**

1. **One set of multi-arch Dockerfiles.** Write every Dockerfile so `docker buildx build --platform linux/amd64,linux/arm64` works:
   - Go service: cross-compile with `FROM --platform=$BUILDPLATFORM` plus `GOOS=$TARGETOS GOARCH=$TARGETARCH`. No emulation is needed.
   - Python and ML services: use multi-arch base images (Debian trixie, to match Raspberry Pi OS) and wheels that publish both `x86_64` and `aarch64` builds.
2. **Default dev loop: amd64 native.** Run `docker compose up` on the desktop at full speed. You'll be iterating on wake word, STT, TTS and pipeline logic, and that needs to be fast.
3. **arm64 check: emulated.** Register QEMU with binfmt_misc on the desktop, then run `DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose up` (or set `platform: linux/arm64` per service). This proves the arm64 images build, start and pass a functional smoke test. It says nothing about Pi latency. Don't run timing work under QEMU.
4. **Audio: share the host PipeWire through its PulseAudio-compatible socket.** Bind-mount `$XDG_RUNTIME_DIR/pulse/native` into the audio-facing containers and set `PULSE_SERVER=unix:/run/pulse/native`. Leave `/dev/snd` passthrough as a fallback only.
5. **Bluetooth, Echo Dot and the real mic gain chain are out of scope for the emulator.** In compose, treat audio output as "a PulseAudio sink" so the same code works whether the sink is desktop speakers or, on the Pi, a BlueZ A2DP sink.

## Key facts

- Docker documents three multi-platform strategies: QEMU emulation, multiple native nodes, and cross-compilation. It warns: "Emulation with QEMU can be much slower than native builds." [1]
- QEMU is registered on Linux with `docker run --privileged --rm tonistiigi/binfmt --install all` (or `--install arm64`). This needs kernel 4.8 or later. [1][2] On Arch, the packaged option is `qemu-user-static` plus `qemu-user-static-binfmt` (version 11.1.3-1 at time of reading), which installs persistent binfmt rules through systemd-binfmt. [3]
- The cross-compilation pattern uses BuildKit's predefined `BUILDPLATFORM`, `TARGETPLATFORM`, `TARGETOS` and `TARGETARCH`. Pinning a stage to `$BUILDPLATFORM` avoids emulation. [1]
- `DOCKER_DEFAULT_PLATFORM` is the "Default platform for commands that take the `--platform` flag" [4]. Compose's per-service `platform` "defines the target platform the containers for the service run on." [5]
- **The Pi 5 kernel uses 16K pages.** Firmware on Pi 5 "defaults to loading `kernel_2712.img` because this image contains optimisations specific to those models (for example, 16K page-size)." [6] QEMU user-mode on an x86 host runs with the host's 4K pages, so the emulator cannot catch 16K-page bugs. Some arm64 binaries, notably those bundling jemalloc built for 4K pages, fail on 16K-page kernels. This is the biggest gap between emulation and the real Pi.
- Pi 5 CPU: BCM2712, a quad-core Arm Cortex-A76 at up to 2.4 GHz with 512 KB L2 per core and 2 MB shared L3. [7] Current Raspberry Pi OS is based on Debian Trixie; the previous release was Bookworm. [8] Docker Engine supports Debian Trixie 13 and Bookworm 12 on arm64. [9]
- How QEMU user-mode works: it translates guest syscalls to host syscalls and maps each guest thread to a host thread. ptrace is not implemented, and memory-model differences between host and guest can break software that assumes one. [10]

## Options considered

### A. arm64 everywhere via QEMU (rejected as the default)

- **Pros:** the images are byte-for-byte what will run on the Pi.
- **Cons:**
  - ML inference (whisper.cpp/faster-whisper, Piper or ONNX Runtime, openWakeWord, embeddings) is SIMD-heavy. Under QEMU TCG, NEON is translated in software. Docker only says "much slower" [1]; we found no first-party benchmark. Expect roughly an order of magnitude slower for inference (unverified estimate), which would make wake word and STT feel broken and mislead tuning.
  - It still doesn't emulate 16K pages, the Pi's memory bandwidth or its thermals [6].
- **Verdict:** use it for build verification and a smoke test only.

### B. Native amd64 from the same multi-arch Dockerfiles (recommended for dev)

- **Pros:** full speed, and the same compose file and code paths as on the Pi.
- **Cons:** different wheels and binaries per arch, so an arm64-only failure (missing wheel, 16K pages) shows up later.
- **Mitigation:** run option A's smoke test in CI or before deploy, then a real-Pi check.

### C. Full-system emulation (qemu-system-aarch64 booting Raspberry Pi OS)

- **Cons:** slower still. There's no faithful Pi 5 machine model in QEMU (the `raspi` boards cover older models; not verified for Pi 5 in this pass), and audio and Bluetooth passthrough get harder.
- **Verdict:** not worth it for the prototype.

### D. Remote arm64 buildx node (the Pi itself as a native builder)

Docker's "multiple native nodes" strategy [1]. Worth adding once the Pi is on the network. It gives native arm64 builds and a real 16K-page runtime test. Optional later.

## Audio into containers

The desktop runs PipeWire on Arch. pipewire-pulse implements the PulseAudio native protocol. Its default listen address is `unix:native`, and it can also listen on `unix:/abs/path` or `tcp:4713` [11]. The socket directory comes from `PULSE_RUNTIME_PATH` or `XDG_RUNTIME_DIR` [12], which resolves to `$XDG_RUNTIME_DIR/pulse/native` in practice.

| Approach | How | Pros | Cons |
|---|---|---|---|
| **PulseAudio socket (recommended)** | Mount `${XDG_RUNTIME_DIR}/pulse/native:/run/pulse/native`, set `PULSE_SERVER=unix:/run/pulse/native`, run the container as the host UID (`user: "1000:1000"`) and mount `~/.config/pulse/cookie` if auth complains | Shares devices with the desktop (no exclusive grab). Works with the same libpulse/PortAudio/`sounddevice` code on the Pi. Device selection is by sink/source name. | The container needs libpulse (or pipewire-alsa/pulse ALSA plugin) installed. Restricted permissions apply for TCP clients [11]. |
| ALSA device passthrough | `devices: ["/dev/snd:/dev/snd"]`, `group_add: [audio]` [5][13] | No sound server needed | Exclusive access to the card (it fights the desktop's PipeWire). Card names differ between desktop and Pi. |
| PipeWire native socket | Mount `$XDG_RUNTIME_DIR/pipewire-0` | Native PipeWire API | Fewer client libraries. Doesn't help on Pi OS Lite unless PipeWire is installed there. |
| TCP (`tcp:4713`) | Load the pulse TCP address and set `PULSE_SERVER=tcp:host.docker.internal:4713` | Works across a VM boundary | Network clients get restricted permissions [11]. Avoid `--privileged` [13]. |

On the Pi, the same "PulseAudio client to the host sound server" contract holds if the Pi runs PipeWire (or PulseAudio) with a BlueZ A2DP sink. Raspberry Pi OS Lite's default audio stack wasn't confirmed by the docs read (see Open uncertainties).

## What can't be emulated

- **Bluetooth A2DP to the Echo Dot.** BlueZ exposes adapters on D-Bus as `org.bluez` at `/org/bluez/hciX` [14]. Pairing, reconnection and A2DP latency are host-level concerns tied to the real radio. In dev, the "speaker" is just the default Pulse sink, so the Bluetooth sink is transparent to the app. Test Bluetooth reconnection on the real Pi.
- **The 16K page size** [6], **Pi CPU, memory bandwidth and thermal throttling**, the **real webcam mic's gain and noise**, and **GPIO/RP1 peripherals**.
- **Wake-word and STT latency budgets.** These must be measured on the Pi (on-device benchmarking is out of scope for the map, per #1).

## Suggested compose layout

```yaml
# compose.yaml — one file for desktop and Pi; arch chosen by DOCKER_DEFAULT_PLATFORM or the host
x-audio: &audio
  user: "${UID:-1000}:${GID:-1000}"
  environment:
    PULSE_SERVER: unix:/run/pulse/native
  volumes:
    - ${XDG_RUNTIME_DIR}/pulse/native:/run/pulse/native

services:
  assistant:            # Go middleware: orchestration, Tool registry, Fast path / Agent path
    build: { context: ./assistant }   # FROM --platform=$BUILDPLATFORM, cross-compiles
    ports: ["8080:8080"]
    depends_on: [wakeword, stt, tts, embed]
  wakeword:             # owns the mic; streams detections/audio to assistant
    <<: *audio
    build: ./services/wakeword
  stt:
    build: ./services/stt
  tts:                  # plays to default sink (desktop speakers / Echo Dot via BlueZ on Pi)
    <<: *audio
    build: ./services/tts
  embed:
    build: ./services/embed
    volumes: ["models:/models"]
volumes:
  models: {}
```

Usage:

- `docker compose up` runs native amd64 for dev.
- `DOCKER_DEFAULT_PLATFORM=linux/arm64 docker compose up --build` runs the emulated arm64 smoke test.
- `docker buildx build --platform linux/arm64 --push` (or a native Pi builder node) produces deployable images.

Compose `profiles` [5] can gate optional services, such as a `pi-only` Bluetooth helper or a `dev` mock speaker. Only the services that touch audio get the Pulse socket. STT and embeddings stay pure network services, which keeps them testable without audio.

## Open uncertainties

- **No first-party QEMU inference benchmark.** The "about an order of magnitude" slowdown is an estimate. Measure once with the chosen STT engine if it matters.
- **Pi OS Lite (Trixie) default audio stack.** The OS docs read don't say whether PipeWire is preinstalled on Lite [8]. The Pi deployment may need `pipewire pipewire-pulse wireplumber libspa-0.2-bluetooth` installed explicitly. This needs confirming during the Hardware/deploy ticket.
- **16K-page compatibility of the chosen ML wheels** (onnxruntime, ctranslate2, torch if used, and anything bundling jemalloc) on Pi 5. This can only be verified on the Pi or a 16K-page arm64 VM.
- **Arch Wiki pages couldn't be fetched** (bot-blocked). The Arch binfmt guidance comes from the Arch package page [3] plus general systemd-binfmt behaviour.
- **QEMU `raspi` machine support for Pi 5** wasn't verified. It only matters if option C is revisited.

## Sources

1. Docker Docs, "Multi-platform builds": https://docs.docker.com/build/building/multi-platform/
2. tonistiigi/binfmt README: https://github.com/tonistiigi/binfmt
3. Arch Linux package `qemu-user-static-binfmt` 11.1.3-1: https://archlinux.org/packages/extra/x86_64/qemu-user-static-binfmt/
4. Docker CLI reference, environment variables: https://docs.docker.com/reference/cli/docker/#environment-variables
5. Compose file reference, services (`platform`, `devices`, `group_add`, `profiles`): https://docs.docker.com/reference/compose-file/services/
6. Raspberry Pi docs, config.txt (`kernel`, `kernel_2712.img`, 16K pages): https://www.raspberrypi.com/documentation/computers/config_txt.html
7. Raspberry Pi docs, Processors (BCM2712): https://www.raspberrypi.com/documentation/computers/processors.html
8. Raspberry Pi docs, Raspberry Pi OS: https://www.raspberrypi.com/documentation/computers/os.html
9. Docker Docs, Install Docker Engine on Debian: https://docs.docker.com/engine/install/debian/
10. QEMU docs, User Mode Emulation: https://www.qemu.org/docs/master/user/main.html
11. PipeWire docs, module-protocol-pulse: https://docs.pipewire.org/page_module_protocol_pulse.html
12. PipeWire docs, pipewire-pulse(1): https://docs.pipewire.org/page_man_pipewire-pulse_1.html
13. Docker CLI reference, `docker container run` (`--device`, `--group-add`, `--privileged`): https://docs.docker.com/reference/cli/docker/container/run/
14. BlueZ, org.bluez.Adapter: https://github.com/bluez/bluez/blob/master/doc/org.bluez.Adapter.rst
