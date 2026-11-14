# Pi RAM measurements after phase 3 and phase 4 — #69 / #74

Measured on the Raspberry Pi 5 (8 GB) on 2026-11-13, stack deployed from
`integration/map-2` at commit `593d636`, seven Compose services healthy. The
owner was asleep, so the run had no human in the room and no audio playback (see
[Audio safety](#audio-safety)).

This closes the memory-accounting blocker recorded in
[#69](https://github.com/Jairik/clanka-bot/issues/69) and
[#74](https://github.com/Jairik/clanka-bot/issues/74) without changing the
kernel command line or rebooting the Pi.

## Why the numbers come from /proc and not `docker stats`

The Pi boots with `cgroup_disable=memory` on the kernel command line, so the
cgroup v2 memory controller is absent from `cgroup.controllers` and every
container's `memory.current` is missing. `docker stats` therefore reports `0B`
for every service; the earlier diagnostic in
[#69](https://github.com/Jairik/clanka-bot/issues/69) and
`docs/evidence/map-2-pi-performance.md` record that. Turning the controller back
on needs a boot-config edit and a reboot, which this run was not allowed to do.

The measurement instead attributes host processes to containers and reads the
kernel's own per-process rollup:

1. `docker ps --no-trunc` gives each running container's full id and name.
2. Every `/proc/<pid>/cgroup` is read and matched against
   `/system.slice/docker-<id>.scope`. Matching here rather than with
   `docker top` avoids PID-namespace numbering: `docker top` prints PIDs as the
   container sees them, which need not be host PIDs.
3. For each host PID that belongs to a container, `Pss` (and `Rss`, and the
   `Private_*` fields) are summed from `/proc/<pid>/smaps_rollup`. That file is
   root-readable only, so the sampler runs under `sudo`.
4. `MemTotal`, `MemFree`, `MemAvailable`, `Buffers`, `Cached`, `SReclaimable`
   and swap are read from `/proc/meminfo` in the same sample.

PSS is the right per-container figure: it splits every shared page between all
of the processes that map it, in proportion to how much each maps. Summing RSS
across a container double-counts the shared libraries and the model pages that
forked workers map; summing PSS does not. A container's PSS total is therefore
comparable to a system-wide "memory in use" figure, and the sum of all process
PSS reconciles with the system totals in the same sample (see
[Reconciliation](#reconciliation)).

`docker stats` would also have hidden the memory that lives outside a container
cgroup (dockerd, containerd, the kernel itself). `/proc/meminfo` in the same
sample keeps that in view.

## Method

The scripts are committed so the run can be repeated:

- `scripts/pi-ram-sample.sh` — samples per-container PSS plus system totals as
  one JSON object per line. Runs on the Pi, re-executes itself through `sudo`,
  and takes `--interval`, `--duration`, `--project` and `--out`.
- `scripts/pi-ram-load.sh` — drives the scripted load and runs the sampler at a
  2 s interval for the duration of the turns. It refuses to start unless the
  Echo sink is muted (see below) and never plays audio.
- `scripts/pi-ram-load-client.py` — the Wyoming client the load script runs in
  a throwaway container on the Compose network (no model service publishes a
  host port).
- `scripts/pi-ram-report.py` — turns the two JSONL files into the tables below
  (median for idle, peak for load).

Commands, on the Pi:

```sh
# Idle: the stack had no request for more than ten minutes beforehand.
sudo scripts/pi-ram-sample.sh --interval 10 --duration 600 \
  --project clanka-bot --out /tmp/pi-ram-idle.jsonl

# Load: wake -> STT -> typed ask -> TTS, fifteen turns, sampler at 2 s.
TURNS=15 scripts/pi-ram-load.sh

python3 scripts/pi-ram-report.py --idle /tmp/pi-ram-idle.jsonl \
  --load /tmp/pi-ram-<timestamp>/load.jsonl
```

### The load, one turn

1. **Wake word.** A positive `clanker` fixture from
   `wakeword/tests/fixtures/positive/` is streamed to `wakeword:10400` over
   Wyoming with the repo's own padding rule (lead silence plus tail padding, so
   the detector sees a continuous stream). All fifteen turns were detected.
2. **Speech to text.** A fixture saying "what time is it?" is streamed to
   `stt:10300` over Wyoming. The repo has no STT fixture, so it was made once,
   before the measured window, by synthesizing that sentence with the stack's
   own TTS service at 24 kHz and resampling to 16 kHz mono PCM16. It transcribes
   as "What time is it?".
3. **Typed request.** `POST http://127.0.0.1:8082/activity/ask` from the Pi
   host, with the identity header `tailscale serve` injects and the same-origin
   marker a browser fetch sends. The typed path has no speaker attached, so it
   answers with outcome `no_audio` — the same path the Dashboard uses. Questions
   alternate Fast-path (`what time is it`, `what's the date today`,
   `what time is it in Tokyo`, arithmetic) and Agent-path (`what's the weather
   in Berlin`, `explain what a heat pump does in two sentences`,
   `explain photosynthesis in two sentences`).
4. **TTS.** The reply text from the activity view is synthesized at
   `tts:10200` over Wyoming and the PCM is counted and discarded. This is the
   only step that makes the TTS model produce audio, and it never reaches a
   sink.

Each turn's wake, STT and TTS step runs in a throwaway client container on
`clanka-bot_default`; the sampler filters to the `clanka-bot` Compose project, so
the client never appears in the per-container table. Its memory is still inside
`pss_all_kb`. Images built by Compose carry `com.docker.compose.project` as an
image label, so a plain `docker run` from one inherits it and a label filter
cannot separate the two; the load script instead marks its client with
`clanka.ram.exclude=1`, which the sampler skips whatever else the container
inherits.

### Audio safety

The owner was asleep near the speakers, and the Pi's Echo sink
(`bluez_output.5C_41_5A_94_78_A3.1`) was muted for the whole run. The load
script asserts `pactl get-sink-mute` is `yes` before it does anything and prints
the state again at the end; both runs report `yes` before and after. Nothing in
this run called `paplay`/`aplay`, the `/debug/transcript` endpoint, or created a
timer, alarm or reminder. The TTS step writes to no device.

## Idle

61 samples at a 10 s interval, after the stack had handled no request for more
than ten minutes. Values are the median of the window, in MiB.

| Container | Median PSS | Median RSS |
| --- | ---: | ---: |
| assistant | 34.9 | 34.9 |
| embed | 510.6 | 520.8 |
| wakeword | 49.0 | 50.3 |
| stt | 451.4 | 460.4 |
| tts | 552.8 | 563.0 |
| web-search | 172.6 | 216.5 |
| librespot | 11.9 | 18.6 |
| **Sum of the seven containers** | **1783.2** | |
| All processes on the box (PSS) | 2590.6 | |
| System `used` (free-style) | 2089.4 | |
| MemAvailable | 5710.7 | |
| Swap used | 104.0 | |

The three model services (embed, stt, tts) hold 1515 MiB of the 1783 MiB, and
all three were already warm: TTS had loaded its voice state and the wake-word
detector had loaded its model in the turns before the idle window, which is the
steady state the household actually runs in. A freshly started stack would idle
lower, never higher.

## Scripted load

48 samples at a 2 s interval across the fifteen turns (2026-11-13
05:49:44Z .. 05:51:28Z). Each container's column is its own peak within the
window, in MiB; the delta is against the idle median.

| Container | Peak PSS | Idle median | Delta |
| --- | ---: | ---: | ---: |
| assistant | 35.8 | 34.9 | +1.0 |
| embed | 514.4 | 510.6 | +3.9 |
| wakeword | 53.6 | 49.0 | +4.6 |
| stt | 489.0 | 451.4 | +37.6 |
| tts | 625.6 | 552.8 | +72.8 |
| web-search | 172.7 | 172.6 | +0.0 |
| librespot | 11.9 | 11.9 | -0.1 |
| **Sum of the seven containers (peak)** | **1903.0** | 1783.2 | +119.8 |
| All processes on the box (peak PSS) | 2757.4 | 2590.6 | |
| System peak: `used` at the lowest MemAvailable | 2277.7 | 2089.4 | |
| MemAvailable at that sample | 5522.2 | 5710.7 | |

What the fifteen turns exercised:

- 15/15 wake-word detections on the three positive `clanker` fixtures.
- 15 STT transcriptions of the synthesized "what time is it?" fixture.
- 15 typed Dashboard requests: 6 Fast path, 1 Control phrase, 8 Agent path
  (seven agent-path answers plus one weather Fast-path request). 14 answered;
  the weather request for Berlin returned outcome `failed` ("Weather is
  unavailable right now.") because the deployed `593d636` predates the provider
  fallback merged in #35. One failed request does not change the memory picture,
  and the TTS step still synthesized its sentence.
- 15 TTS syntheses, 96.4 s of audio in total (2.1 s to 14.8 s per reply), all
  of it counted and discarded. This is the memory-heavy step on this box: a
  14.8 s reply cost the most, and TTS's peak is the largest delta of the run.

The peak of all-process PSS and the lowest MemAvailable land on the same sample
(05:51:01Z), during the longest TTS synthesis, so the system peak is not an
artefact of combining maxima from different instants.

## Phase 4 with music (#74)

Music playback could not be produced on this Pi. There is no Spotify sign-in,
and the account's audio keys are refused upstream (#71), so librespot never
receives a track and never opens the sink. It was resident for the whole run —
Phase 3 and Phase 4 deploy the same seven services, so the "with music" run and
the run above are the same process set:

| librespot | PSS |
| --- | ---: |
| Idle median | 11.9 |
| Peak during the scripted load | 11.9 |
| Delta | -0.1 (noise) |

Decoding adds bounded buffers, not a second model. Spotify supplies 320 kbps
Ogg Vorbis; librespot's player decodes to 44.1 kHz stereo f32, which is
0.35 MB per second of audio, and it holds its in-flight packets rather than a
track (the pinned v0.8.0 build exposes no buffer-size or prefetch knob in
`librespot --help`, so the pipeline length is fixed by the player, not by
configuration). Even a generous 30 s of decoded audio in flight is ~10 MB plus
one audio thread — under 1 % of the measured headroom below. It is still an
inference: what is measured is librespot resident and idle, not librespot
decoding.

## Reconciliation

One idle sample, in MiB, showing that the three views of the same machine agree:

| Reading | MiB |
| --- | ---: |
| MemTotal | 8059 |
| `used` (free-style: total − free − buffers − cache − reclaimable slab) | 2089 |
| MemTotal − MemAvailable | 2348 |
| Sum of the seven containers' PSS | 1783 |
| Sum of every process's PSS | 2591 |
| Implied host and non-Compose PSS | 808 |

The host share is real and mostly the desktop session this Pi image carries:
dockerd ~116 MiB, `wf-panel-pi` ~112 MiB, containerd ~72 MiB, `pcmanfm` ~50 MiB,
tailscaled ~36 MiB, `labwc` ~33 MiB, wayvnc ~31 MiB, journald ~19 MiB, PipeWire
~16 MiB, plus two long-lived `docker compose logs -f` readers at ~50 MiB
combined. Kernel memory, page tables and slab are in `used` but in no process,
which is why the two sums are not identical.

PSS counts resident file pages that `free` puts in cache, so the all-process sum
is the largest of the three readings (~0.5 GiB above `used`). The go/no-go uses
that largest figure, because a budget should not assume a page a running process
still maps can be reclaimed on demand.

## Go / no-go

Threshold from the Map 2 spec: peak RAM at or below **6.5 GB**. `6.5 GB` is
6198.9 MiB if read as decimal GB and 6656 MiB if read as GiB; both readings are
checked and the stricter (GiB) one is what the decision uses.

| Reading at the system peak | MiB | Headroom vs 6.5 GB (decimal) | Headroom vs 6.5 GiB |
| --- | ---: | ---: | ---: |
| `used` (free-style) | 2277.7 | 3921.2 | 4378.3 |
| All processes' PSS (conservative) | 2757.4 | 3441.5 | 3898.6 |
| Sum of the seven containers | 1903.0 | 4295.9 | 4753.0 |

- **#69, go for phase 4.** The peak system usage is 2.6 GiB (all-process PSS) of
  the Pi's 7.9 GiB, leaving 3.4 GiB of headroom against the decimal threshold
  and 3.8 GiB against the stricter GiB reading. Phase 4 does not need anything
  trimmed.
- **#74, go for the next phase, with a caveat.** Measured headroom is far above
  the 1.5 GB bar, and librespot's resident cost is 12 MiB. The caveat is that
  librespot was resident but never decoded (#71 blocks playback), so its decode
  buffers are argued from the PCM rate rather than measured. If a future run can
  sign in and play, re-run `scripts/pi-ram-load.sh` with music playing and
  replace the librespot row with a measured decode peak; the budget has room for
  it either way.

## Caveats

- The kernel still boots with `cgroup_disable=memory`. This method measures
  processes, not cgroups: it cannot see a container's page cache the way
  `memory.stat` would, and it is only as complete as `/proc`. It is a workaround
  for the measurement, not a repair. Enabling the memory controller at the next
  planned reboot would let `docker stats` answer the same question directly, and
  would also let the stack set memory limits; nothing in this run changed boot
  configuration.
- Sampling is 2 s during the load and 10 s idle. A spike shorter than 2 s could
  hide between samples; every allocation observed here (model warm-up, TTS
  growing with reply length) lasts several seconds, so the peak is bracketed
  rather than missed.
- The stack ran `593d636` for the whole measurement. `integration/map-2` has
  moved since (weather provider fallback #35, multi-wake-word #110, notes/lists
  #112); none of those adds a model process, so the budget conclusion holds, but
  the per-container numbers belong to `593d636`.
- The Dashboard typed path is the proven no-audio route. Its requests have
  outcome `no_audio` because no speaker is attached to that path, so the measured
  load has the real routing, Tools, Agent model calls and TTS synthesis, but no
  playback, ducking or sink work. Voice playback on the Echo would add the
  PulseAudio client and the sink path in the assistant container, which idles at
  35 MiB; nothing in the ADR-0001 split puts a model there.
- One weather request failed on the deployed build (see above) and one Agent
  reply was cut at 200 characters in the log line shown here; both were
  synthesized normally and are included in the TTS peaks.
