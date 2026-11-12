# Spike: wake word over music with PipeWire echo-cancel (#70)

Status: **PARTIAL — prep only.** Date: 2026-11-12. Issue:
[Jairik/clanka-bot#70](https://github.com/Jairik/clanka-bot/issues/70).

This document, `scripts/wake_over_music.py` and its tests are the repeatable
prep for the measurement. **No physical experiment has run.** Nobody spoke a
wake word over music, no microphone was opened, no audio was played, no
PipeWire module was loaded, and no host mixer, daemon or user audio state was
changed while preparing this. There are no measured counts and no
`music.max_volume` recommendation here.

**Acceptance criteria status: NOT MET (both).** The six-cell table is blank and
the cap is undecided. Both acceptance boxes on #70 stay unchecked, and #70 stays
open.

Do not read this doc as evidence that echo cancellation holds. It only makes the
run repeatable and hard to fake.

## 1. What the spike decides

The question is whether host PipeWire `echo-cancel` keeps wake-word accuracy
usable while Spotify music plays, or whether music needs a product cap
(`music.max_volume`). ADR-0004 already puts music and speech on the same sink,
so the echo-cancel module can use the playback stream as its reference.

The spike produces six cells: three music volumes, each with AEC off and on.
Each cell records positive wake hits/trials and false wakes over music-only
exposure minutes. The operator then decides:

- AEC holds: no cap, or a cap chosen for another reason.
- AEC does not hold at the top volume: cap music, and record the measured
  volume that motivated the cap.

**This doc does not choose the cap.** A `music.max_volume` value is a product
decision that must follow the counts and an accuracy target; it is not derivable
from the prep.

## 2. Host echo-cancel path

PipeWire's `module-echo-cancel` creates four streams: a capture stream from the
real mic, a virtual sink that receives the audio to cancel (the reference), a
playback stream that forwards that audio to the real speaker, and a cleaned
virtual source for the application. Official docs:

- <https://docs.pipewire.org/page_module_echo_cancel.html>
- <https://docs.pipewire.org/page_pulse_module_echo_cancel.html>
- <https://docs.pipewire.org/page_man_pipewire-pulse-modules_7.html>

```
real mic ──> capture ──> [ echo cancel ] ──> cleaned source ──> assistant capture
                              ^
real speaker <── playback <── [ virtual sink ] <── assistant playback + librespot
```

The reference is the audio stream entering the module's virtual sink, which the
module forwards to the real speaker. The virtual source is the cleaned capture
the assistant reads. `sink_master` must be the named real sink (the Echo Dot on
the Pi), `source_master` the named real mic, and the virtual `sink_name`/
`source_name` are what the assistant and librespot use.

### 2.1 Names to use

Observed on the prep dev box on 2026-11-12 (read-only `pactl list short`):

| Role | Dev-box name | Pi placeholder |
| --- | --- | --- |
| Real playback sink (`sink_master`) | `alsa_output.pci-0000_00_1f.3.analog-stereo` | `<echo-dot-sink>` from `pactl list short sinks` |
| Real mic (`source_master`) | `alsa_input.usb-046d_0825_EC865510-02.mono-fallback` | `<usb-mic-source>` from `pactl list short sources` |
| Virtual sink (`sink_name`) | `aec_music_sink` | `aec_music_sink` |
| Cleaned source (`source_name`) | `aec_mic_source` | `aec_mic_source` |

Use the real device names, not `@DEFAULT_SINK@`/`@DEFAULT_SOURCE@`: the default
can change when the Echo Dot reconnects, and an AEC reference that silently
follows the wrong sink is worse than no measurement.

### 2.2 Apply (operator only)

These commands change host audio. The implementation agent did **not** run
them. Run them as the PipeWire user, not root, while the assistant is idle.

Inspect first:

```sh
pactl info | sed -n '1,8p'                  # server + protocol version
pactl list short sinks
pactl list short sources
pactl list short modules | grep echo-cancel || true
```

Runtime module (lost on the next `pipewire-pulse` restart):

```sh
pactl load-module module-echo-cancel \
  source_master='<usb-mic-source>' \
  sink_master='<echo-dot-sink>' \
  source_name=aec_mic_source \
  sink_name=aec_music_sink \
  rate=48000
```

`pactl load-module` prints the module index. Keep it; that index and
`module-echo-cancel` are both valid for unload. Use either the runtime command
or the persistent drop-in below, not both, or two echo-cancel instances appear.
The default AEC library is WebRTC (`aec/libspa-aec-webrtc`); leave it unset
unless the installed PipeWire documents another name. Do not add `aec_method` by
guesswork.

The module name and options above were checked against the official PipeWire
module pages on 2026-11-12. On the prep dev box, `libpipewire-module-echo-cancel.so`
and `libspa-aec-webrtc.so` are installed; the module was **not** loaded.

Persistent form (survives reboot and `pipewire-pulse` restart):

```conf
# ~/.config/pipewire/pipewire-pulse.conf.d/50-echo-cancel.conf
pulse.cmd = [
  { cmd = "load-module" args = "module-echo-cancel source_master=<usb-mic-source> sink_master=<echo-dot-sink> source_name=aec_mic_source sink_name=aec_music_sink rate=48000" flags = [ ] }
]
```

Then restart only the Pulse-compatible daemon:

```sh
systemctl --user restart pipewire-pulse
```

Verify before touching the assistant config:

```sh
pactl list short sources | grep aec_mic_source
pactl list short sinks   | grep aec_music_sink
pactl list short modules | grep echo-cancel
pactl get-sink-volume   aec_music_sink
pactl get-source-volume aec_mic_source
```

Both virtual nodes must appear, and move to `RUNNING` once the assistant is
capturing/playing. A loaded module whose nodes do not appear, or whose virtual
sink stays idle while music plays, is not wired correctly.

### 2.3 Rollback

Runtime route: stop `assistant` first (or expect its audio stream to fail), then
`pactl unload-module module-echo-cancel` (or the printed index). Persistent
route: remove `50-echo-cancel.conf` and `systemctl --user restart
pipewire-pulse`. Then clear the two virtual names from `.env` and restart
`assistant`; never leave it pointed at a source/sink that no longer exists.

Before rollback, capture the evidence the run needs: `pipewire --version`,
`pactl info` server string, module list line, both node names, and the module
index. Do not paste a full `pactl info` or environment dump into the issue; it
contains the hostname and user name.

### 2.4 Assistant and librespot wiring

Only `assistant` mounts the Pulse socket (ADR-0001). No new mount is needed for
the virtual nodes; they are ordinary Pulse names on that socket. The container
already gets `PULSE_SERVER=unix:/run/pulse/native` and the host pulse directory
read-only (`docker-compose.yml`).

For AEC-on cells, set the restart-required connection settings:

```sh
# .env on the host (never committed)
ASSISTANT_PULSE_SINK=aec_music_sink
ASSISTANT_PULSE_SOURCE=aec_mic_source
```

Then restart `assistant` (`docker compose up -d assistant`, or the normal
`scripts/deploy.sh`). For AEC-off cells, point both back at the real sink/mic.
Changing the default sink alone is not enough: the assistant names its devices.

For AEC to have a reference, **librespot's output must go to the same virtual
sink**, not straight to the Echo Dot. Spotify playback lands in #71/#72; until
that exists, a manual music source routed to `aec_music_sink` is a stand-in for
the *plumbing check only*, not for a measured cell. A music source that bypasses
the virtual sink invalidates the AEC-on cells: the mic hears music the canceller
never saw.

## 3. Measurement protocol

### 3.1 Preconditions

- Wake word is the configured `okay_nabu` (openWakeWord). Do not change it for
  the run.
- The real mic and speaker are in their normal positions. Note the speaker's
  distance/orientation if it differs between cells; keep it fixed across the
  six cells.
- Music is actually playing from the intended source into the AEC virtual sink
  (or the real sink in the AEC-off state), at the volume being tested.
- The assistant is healthy and its logs are visible:
  `docker compose logs -f assistant`. A hit is the `voice: wake` log line
  (model `okay_nabu`) and/or the wake Cue. The log line is the count of record.
- Close other wake-word sources: no TV, no second assistant, no phone hotword in
  the room.

### 3.2 The two host states

| State | Assistant capture | Assistant playback | Music output |
| --- | --- | --- | --- |
| AEC off | real mic | real sink | real sink |
| AEC on | `aec_mic_source` | `aec_music_sink` | `aec_music_sink` |

Restart `assistant` between states. Do not compare an AEC-on run whose music
went to the real sink with an AEC-off run; that measures routing, not echo
cancellation.

### 3.3 Volumes

Choose three distinct music volumes before starting and record the actual
readings with the shell command you used (for example `pactl get-sink-volume
aec_music_sink` or the Spotify/librespot volume value). They must span quiet to
loud and be reachable by the product. This doc deliberately does not set them;
"low/mid/high" is a label, not a number.

Do not change `music.max_volume` or any product default for the run. The run
measures the unmodified stack.

### 3.4 Positive trials — human or separate acoustic source

For each cell, the operator speaks the wake word once per trial from the room:

1. Start the music and let it play for at least 30 s before the first trial, so
   the canceller adapts. Record this warm-up in the cell note if you change it.
2. Say `okay nabu` once, naturally, at the normal distance.
3. A **hit** is one `voice: wake` log line caused by that utterance. Wait up to
   3 s for it; a second utterance is a new trial. A miss is a trial with no
   wake line. Do not repeat the word inside one trial. After a hit, let the
   turn finish (or cancel it) before the next trial; a wake during the reply is
   a barge-in, not a new trial.
4. Count the trials you actually attempted and the hits. Stop if the assistant
   starts a turn from something other than your utterance; note it as a false
   wake, not a hit.

**Never play the positive utterance through the AEC reference speaker.** A TTS
or recorded `okay nabu` played into `aec_music_sink` becomes part of the
reference signal and is cancelled by design; the test would measure the
canceller's own output, not a person in the room. It also sounds like a false
wake test to the assistant.

**Never substitute a digital loopback for room acoustics.** Playing a sample
into a null sink whose monitor feeds the mic is the #29 prototype pattern; it
does not include the speaker-to-mic path, the room, or Bluetooth delay, so it
cannot justify an AEC claim. The positive source must be a human voice or a
separate physical sound source in the room, acoustically coupled to the mic.

### 3.5 False wakes and exposure

For each cell, after the positive trials, leave the music playing and **do not
speak**:

- Record the contiguous music-only exposure in minutes. Stop the clock for
  dropouts, adverts, or anything that changes the playback path.
- Count every `voice: wake` line that no person caused as a false wake. If the
  assistant starts a turn, note whether it replied or errored, and pause the
  exposure clock until the turn ends so its own speech is not counted as music
  exposure.
- Exposure must be inside the cell's timestamp window. The recorder refuses
  exposure longer than `ended_at - started_at`.

False wakes are reported per hour, derived from the actual count and exposure.
A cell with zero exposure minutes is invalid, not "zero false wakes".

### 3.6 What invalidates a cell

- The utterance came from the Pi's own speaker or a loopback.
- Music bypassed the AEC virtual sink in an AEC-on cell.
- The assistant was restarted, the Bluetooth link dropped, or the source/sink
  changed mid-cell. Split the cell and record both windows, or discard it.
- The host mixer or `music.max_volume` was adjusted during the cell.
- Counts were estimated rather than taken from the log.

## 4. Recorder and validator

`scripts/wake_over_music.py` is bookkeeping only. It never plays audio, opens a
microphone, or calls `pactl`. It refuses incomplete or impossible counts and
reports `UNMEASURED` until all six cells have operator counts.

```sh
# 1. Blank template; choose the three volumes you will actually use.
python3 scripts/wake_over_music.py init --out /tmp/wake-over-music.json \
  --volumes 25,50,75

# 2. After one real cell, record the actual counts and timestamps.
python3 scripts/wake_over_music.py record --file /tmp/wake-over-music.json \
  --volume 50 --aec on --trials 20 --hits 18 --false-wakes 1 \
  --exposure-min 30 \
  --started-at 2026-11-12T20:00:00+01:00 \
  --ended-at 2026-11-12T20:40:00+01:00

# 3. Validate the whole matrix. It prints JSON and exits 2 on bad counts.
#    Without an operator target it stays decision-pending, not recommended.
python3 scripts/wake_over_music.py report --file /tmp/wake-over-music.json \
  --target-hit-rate 0.9 --target-false-per-hour 5
```

Rules the recorder enforces:

- exactly three distinct volumes in 1..100 and one cell per volume × AEC state;
- a cell is either all-blank or fully filled — partial counts are refused;
- `hits <= trials`, `trials >= 1`, `false_wakes >= 0`, `exposure_min > 0`;
- ISO timestamps with a UTC offset, `ended_at > started_at`, and exposure no
  longer than the session window;
- `kind` is `operator-measurement` or `synthetic-fixture`; a synthetic fixture
  reports `SYNTHETIC` and is never evidence;
- `decision.max_volume` must stay null while the decision is pending, and a
  measured operator file must name the host, PipeWire version, real capture
  source and real playback sink (plus the AEC source/sink when an AEC-on cell is
  measured);
- `MEASURED` only when all six cells are filled; `decision_ready` additionally
  needs both operator targets. A `decided` decision is refused unless all six
  cells are measured and both targets exist (in the file or on the CLI).

A synthetic arithmetic fixture lives at
`scripts/tests/fixtures/wake-over-music.synthetic.json`. It exists so the tests
can check percentages and false-wake rates. Its report says `SYNTHETIC`, exits
1, and must never be copied into an evidence file or the issue.

## 5. The six cells

Fill this table only from the recorder after real runs. Leave it blank until
then. `Hits/trials` and `False wakes/h` are derived by the recorder, not typed
by hand.

| Music volume | AEC | Hits/trials | False wakes | Exposure (min) | False wakes/h | Window (start–end) |
| ---: | --- | ---: | ---: | ---: | ---: | --- |
|  | off |  |  |  |  |  |
|  | on |  |  |  |  |  |
|  | off |  |  |  |  |  |
|  | on |  |  |  |  |  |
|  | off |  |  |  |  |  |
|  | on |  |  |  |  |  |

## 6. Decision — pending

**No `music.max_volume` is recommended.** The decision stays pending until:

1. all six cells have real operator counts and timestamps;
2. the operator states an accuracy target (`--target-hit-rate`, and a false-wake
   budget if one applies); and
3. the recorded host configuration shows the AEC path that produced the AEC-on
   cells.

After that, write the decision into the evidence file's `decision` block
(`status: "decided"`, `max_volume` null or an integer) and into #70. Nothing in
this repo should hard-code a default cap before those counts exist.

## 7. Versions, config and mounts to record

| Item | Prep dev box (2026-11-12) | Pi (fill at run time) |
| --- | --- | --- |
| Host / OS | Arch Linux, 12 threads | `<pi-host>` |
| PipeWire | 1.6.11 (compiled and linked) | `<pipewire --version>` |
| Pulse-compatible server | 15.0.0 (on PipeWire 1.6.11) | `<pactl info>` server line |
| Echo-cancel module | `libpipewire-module-echo-cancel.so` present, not loaded | `pactl list short modules` filtered for echo-cancel |
| AEC library | `libspa-aec-webrtc.so` present | same |
| Drop-in config | none applied | `~/.config/pipewire/pipewire-pulse.conf.d/50-echo-cancel.conf` |
| Real sink | `alsa_output.pci-0000_00_1f.3.analog-stereo` | `<echo-dot-sink>` |
| Real source | `alsa_input.usb-046d_0825_EC865510-02.mono-fallback` | `<usb-mic-source>` |
| Virtual sink/source | `aec_music_sink` / `aec_mic_source` | same |
| Assistant audio mount | `${XDG_RUNTIME_DIR}/pulse:/run/pulse:ro`, cookie read-only | same |
| Container env | `PULSE_SERVER=unix:/run/pulse/native` | same |
| Assistant image | `<GIT_SHA at deploy>` | `<GIT_SHA at deploy>` |

The dev-box names are an example of the real device names, not a Pi default.
Record the names actually in use; do not copy the dev box's ALSA names to the
Pi.

## 8. Handoff — what a human must do

1. On the Pi, inspect the real sink/source names and confirm the echo-cancel
   module and WebRTC AEC plugin exist.
2. Apply the runtime module, verify both virtual nodes, then persist the drop-in
   if the run will survive a reboot.
3. Route `assistant` and librespot to the virtual sink/source; restart
   `assistant`; confirm `voice: wake` logs appear.
4. Run the AEC-off and AEC-on cells at three volumes with a human voice in the
   room, recording counts, timestamps and exposure.
5. Run `report`; attach the JSON summary and the host/config row to #70.
6. Only then decide `music.max_volume` or "no cap", and check #70's acceptance
   boxes.

This work does not unblock #72. Spotify still needs its own implementation and
merging; this spike only prepared the measurement it depends on.
