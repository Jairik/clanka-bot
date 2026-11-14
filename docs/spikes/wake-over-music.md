# Spike: wake word over music with PipeWire echo-cancel (#70)

Status: **measured 2026-11-13 — negative/inconclusive on the acoustic path.**
Earlier revision: prep only, 2026-11-12. Issue:
[Jairik/clanka-bot#70](https://github.com/Jairik/clanka-bot/issues/70).

Sections 1–8 are the original prep and remain the reference for the host
echo-cancel path, the room protocol and the recorder. Section 0 is the
record-first run that replaced the six separate live cells. The measured
counts are in §5 and the decision in §6. **Both acceptance criteria are not
met:** every cell is 0/24, and the blocker is the acoustic path, not the AEC
arithmetic.

Do not read the prep as evidence that echo cancellation holds. The measured
counts live in §5 and the decision block in §6.

## 0. Run 2026-11-13 — record first, detect offline

### 0.1 Targets, fixed before any audio was played

| Measurement | Target per cell |
| --- | --- |
| `clanker` hits | **≥ 80%** of trials |
| `clanker` false wakes | **≤ 1 per 10 music-only minutes** |
| `volume up` hits | **≥ 80%** of trials |
| `volume up` false triggers | **0 per cell** |

Decision rule, also fixed before the run:

- AEC **on** meets both `clanker` targets at 100% → recommend **no
  `music.max_volume` cap**.
- Otherwise the cap is the **highest measured volume whose AEC-on cell met both
  targets**. If no AEC-on cell passes, that is a failed result, not a quiet cap:
  say so in §6 and do not ship the AEC path as the answer.

One false wake in a cell's music-only minutes would be 10× the per-10-minute
budget only if the exposure were 10 minutes; with this run's ~3 minutes per cell
the arithmetic is `60 / exposure_min` per event. The exact conversion is in the
evidence file.

### 0.2 Protocol actually used

The speaker is active for one bounded block, then silent for the rest of the
work:

1. Load the runtime `module-echo-cancel` with the real mic and the Echo Dot
   sink. Route **all** music into `aec_music_sink`; the module forwards it to
   the Echo, so the room hears the same sound in every session.
2. Three sessions, one per music volume (35%, 65%, 100% of the music stream;
   the Echo sink stays at 100%). Each session is ~5 minutes: 20 s warm-up,
   24 `clanker` clips and 12 `volume up` clips played **directly to the bluez
   sink** (outside the AEC reference) at ~5 s spacing, then 120 s of music
   only.
3. Each session records the **raw real-mic source** and **`aec_mic_source`** at
   the same time (16 kHz mono WAV). One session therefore yields the AEC-off
   and AEC-on cells together, and three sessions yield all six cells.
4. After the third session: stop playback, restore the Echo sink to its
   original unmuted 100%, unload echo-cancel, restore `.env`, restart the
   assistant and confirm the stack is healthy. The speaker stays silent from
   here on.
5. Replay both WAVs of each session through the real wakeword service over
   Wyoming, in real time so the service's 2 s refractory period behaves as in
   production, with `clanker` at its deployed 0.5 and `volume up` at its
   deployed 0.98. Count hits and false wakes offline.

`scripts/wake_over_music_run.py` implements steps 2–5 (`record`, `detect`,
`count`); `scripts/wake_over_music.py` still validates the six-cell evidence
matrix.

### 0.3 Assumptions recorded for this run

1. **Music source:** public-domain 78 rpm transfers with vocals from the
   Internet Archive Great 78 Project, downloaded to a dedicated directory on
   the Pi and concatenated into one loudness-normalised 18.3-minute bed. URL,
   licence note and the exact files are in §0.5. Audio is not committed.
2. **Positive trials are not a person in the room.** The wake clips come from
   the held-out-voice Pocket TTS fixtures already used by
   `wakeword/tests/run_detection_test.sh`, played from the Echo Dot's position.
   That tests the word, the room path and the canceller, but it is not the
   owner's voice or a human distance/turn-taking pattern.
3. **Clip routing:** clips go directly to `bluez_output.5C_41_5A_94_78_A3.1`,
   so they are not part of the AEC reference. Only the music enters
   `aec_music_sink`.
4. **Volume definition:** the three levels are the music stream's own volume
   (`paplay --volume`, linear 0–65536). The Echo sink stays unmuted at 100% and
   the clip stream stays at 100%, so the wake level is comparable across
   volumes.
5. **Hit window:** a detection counts for a clip when its stream timestamp is
   within 0.5 s before to 2.0 s after the logged clip onset. Clip onsets are
   ≥ 5 s apart, so windows never overlap. False wakes are detections outside
   every clip window (both models), so only music-only time can produce them.
6. **Statistical limit:** each cell has under 4 minutes of music-only exposure.
   A zero false-wake count cannot demonstrate a 1/hour rate; it only bounds the
   rate very loosely. The hit-rate counts are the stronger half of this run.
   The table reports the exposure so the limit is visible rather than implied.

### 0.4 Host configuration actually used

| Item | Value |
| --- | --- |
| Host | `raspberrypi`, Raspberry Pi 5, aarch64, kernel `6.12.115+rpt-rpi-2712` |
| PipeWire | 1.2.7 (compiled and linked) |
| Pulse-compatible server | `PulseAudio (on PipeWire 1.2.7)`, protocol 35, `pactl` 16.1 |
| Echo-cancel module | runtime `module-echo-cancel`, index `536870916`, no drop-in |
| AEC library | `libspa-aec-webrtc.so` (default; `aec_method` not set) |
| AEC arguments | `rate=48000` only |
| Real playback sink | `bluez_output.5C_41_5A_94_78_A3.1` (Echo Dot-3LG, A2DP SBC) |
| Real capture source | `alsa_input.usb-Generalplus_OSM09-00.mono-fallback` |
| Virtual sink / source | `aec_music_sink` / `aec_mic_source` |
| Assistant image | `42fb047` (the integration/map-2 commit deployed on the Pi) |
| Music path | music streams into `aec_music_sink`; the module forwards to the Echo |
| Clip path | clips stream directly to the bluez sink, outside the AEC reference |
| Source gain | **150%** for the run (measurement gain; restored to 100% afterwards) |
| Sink gain | **100%**, unmuted (the original state, restored afterwards) |

Runtime module command:

```sh
pactl load-module module-echo-cancel \
  source_master=alsa_input.usb-Generalplus_OSM09-00.mono-fallback \
  sink_master=bluez_output.5C_41_5A_94_78_A3.1 \
  source_name=aec_mic_source \
  sink_name=aec_music_sink \
  rate=48000
```

The module was unloaded after the run. No persistent drop-in was written. The
Bluetooth card ended on its original `a2dp-sink` profile.

### 0.5 Music, clips and session shape

The music bed is three public-domain 78 rpm vocal transfers from the Internet
Archive Great 78 Project, played in sequence twice and loudness-normalised to
`I=-18 LUFS, TP=-2` (48 kHz stereo FLAC, 18.3 minutes):

| Track | Artist / date | Item |
| --- | --- | --- |
| Swanee River Moon | Stellar Quartette | `78_swanee-river-moon_stellar-quartette-h-pitman-clark_gbia0548603b` |
| Old Folks At Home (Swanee River) | Emma Calvé, 1916 | `78_old-folks-at-home-swanee-river_emma-calv-foster_gbia0521028a` |
| Silver Swanee | Broadway Melody Makers, 1922 | `78_silver-swanee_broadway-melody-makers-eddie-cantor-jean-schwartz_gbia0421644b` |

The items are pre-1929 US recordings in the archive's `78rpm` collection. They
carry no explicit `licenseurl`; they are treated as public domain and the URLs
are recorded here. The audio itself is not committed.

Positive clips are the repo's held-out-voice Pocket TTS fixtures
(`wakeword/tests/fixtures/positive/clanker_0{0,1,2}.wav` and
`wakeword/tests/fixtures/volume_up/positive/volume_up_0{0,1,2}.wav`),
gain-normalised to about -18 dBFS mean with peaks at or below -1.5 dBFS so the
two models are comparable. They are not the owner's voice.

Each session: a 0.2 s 1 kHz sync tone into a quiet room (to measure the parec
start offset), 20 s warm-up, 24 `clanker` + 12 `volume up` clips at 5 s spacing,
then 120 s of music only. Each session was 321.7 s.

| Session | Music stream | Window (local, -05:00) | Duration |
| ---: | ---: | --- | ---: |
| 1 | 35% | 09:01:06 – 09:06:29 | 321.65 s |
| 2 | 65% | 09:06:29 – 09:11:51 | 321.70 s |
| 3 | 100% | 09:11:51 – 09:17:14 | 321.70 s |

### 0.6 Acoustic-path caveat measured during prep

Before the sessions, a full-scale 1 kHz tone through the Echo at sink 100% and
source 150% was only about **2 dB above the broadband noise floor** at the mic
(about 18 dB in a 100 Hz band around 1 kHz). AVRCP volume-up reached the Echo
and then stopped increasing, so the Echo was already at its maximum. The
recorded clips are present in the WAVs (the cleaned source shows a clear burst
against a low residual), but the dry-run detector did not fire on them even at
a 0.1 threshold. The real sessions were run anyway, at the product's 100% sink
and the measured 150% source gain, and the counts below are reported as
measured. A low hit rate in this run is evidence about the acoustic path (the
Echo's level at this mic), not proof that the AEC arithmetic works or fails;
§6 keeps that distinction.

### 0.7 Recordings

Raw and cleaned WAVs for all three sessions are on the Pi only, under
`~/wake-over-music/run/` and `~/clanka-backups/20261113-091805/wake-over-music/`.
They may contain room audio from the owner's home and were not copied off the
Pi or committed. The committed artifacts are the manifest format, the scripts
and the counts.

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
| Real playback sink (`sink_master`) | `alsa_output.pci-0000_00_1f.3.analog-stereo` | `bluez_output.5C_41_5A_94_78_A3.1` |
| Real mic (`source_master`) | `alsa_input.usb-046d_0825_EC865510-02.mono-fallback` | `alsa_input.usb-Generalplus_OSM09-00.mono-fallback` |
| Virtual sink (`sink_name`) | `aec_music_sink` | `aec_music_sink` |
| Cleaned source (`source_name`) | `aec_mic_source` | `aec_mic_source` |

Use the real device names, not `@DEFAULT_SINK@`/`@DEFAULT_SOURCE@`: the default
can change when the Echo Dot reconnects, and an AEC reference that silently
follows the wrong sink is worse than no measurement.

### 2.2 Apply (operator only)

These commands change host audio. Run them as the PipeWire user, not root,
while the assistant is idle. The 2026-11-13 run used the runtime form with the
Pi names above and unloaded it afterwards; the persistent form was not used.

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

Only `assistant` and `librespot` mount the Pulse socket (ADR-0001/0004). No
new mount is needed for the virtual nodes; they are ordinary Pulse names on
that socket. Both containers already get `PULSE_SERVER=unix:/run/pulse/native`
and the host pulse directory read-only (`docker-compose.yml`).

For AEC-on cells, set the restart-required connection settings:

```sh
# .env on the host (never committed)
ASSISTANT_PULSE_SINK=aec_music_sink
ASSISTANT_PULSE_SOURCE=aec_mic_source
```

Then restart `assistant` and `librespot` (`docker compose up -d assistant
librespot`, or the normal `scripts/deploy.sh`). For AEC-off cells, point both
back at the real sink/mic. Changing the default sink alone is not enough: both
containers name their devices.

Compose maps `LIBRESPOT_SINK` from `ASSISTANT_PULSE_SINK`, so the Connect
receiver follows the same virtual sink as the assistant's speech. That is the
ADR-0004 property the AEC run needs: both music and speech enter the reference.
If `LIBRESPOT_SINK` is ever set independently, keep it equal to
`ASSISTANT_PULSE_SINK` or the AEC-on cells are invalid. The Web API playback
Tools and ducking are #72; they do not change this audio path.

If the three volumes are set with librespot's `SPOTIFY_INITIAL_VOLUME` or the
Spotify Connect volume, record that value **and** `pactl get-sink-volume
aec_music_sink`; they are separate stages, and the measured grid must say which
one produced the level.

## 3. Measurement protocol

### 3.1 Preconditions

- Wake word is the configured single model (`WAKEWORD_MODELS` with one entry;
  the current default is `clanker`, while the original baseline was
  `okay_nabu`). Do not change it for the run. The two-model follow-up in §5.1
  is a separate measurement.
- The real mic and speaker are in their normal positions. Note the speaker's
  distance/orientation if it differs between cells; keep it fixed across the
  six cells.
- Music is actually playing from the intended source into the AEC virtual sink
  (or the real sink in the AEC-off state), at the volume being tested.
- The assistant is healthy and its logs are visible:
  `docker compose logs -f assistant`. A hit is the `voice: wake` log line
  (model `okay_nabu`) and/or the wake Cue. The log line is the count of record
  for a live human run. The 2026-11-13 record-first run counted the wakeword
  service's `detection` events at offline replay instead (§0.2), with the
  assistant stopped during the sessions.
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
wake test to the assistant. The 2026-11-13 run played the clips **directly to
the bluez sink**, outside the reference, from the Echo's position; that is a
separate physical source but not a human in the room, and §0.3 states that
caveat.

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

`scripts/wake_over_music_run.py` is the record-first harness used for the
2026-11-13 run: `record` plays the music and clips and captures both mic paths,
`detect` replays one WAV through the real service in real time, and `count`
joins the manifest and detections into a cell. Its tests are in
`scripts/tests/test_wake_over_music_run.py`.

```sh
# 1. Blank template; choose the three volumes you will actually use.
python3 scripts/wake_over_music.py init --out /tmp/wake-over-music.json \
  --volumes 25,50,75

# 2. After one real cell, record the actual counts and timestamps.
#    The numbers below are placeholders, not a measurement.
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

## 5. The six cells — measured 2026-11-13

Filled from `docs/evidence/wake-over-music-2026-11-13.json`, which
`scripts/wake_over_music.py report` accepts as MEASURED. Each cell has 24
`clanker` trials and 12 `volume up` trials in the same session; the exposure is
the time outside every clip window. Counts are detections from **offline
real-time replay** of the saved WAVs through the deployed wakeword service
(`clanker` 0.5, `volume_up` 0.98).

| Music volume | AEC | Hits/trials | False wakes | Exposure (min) | False wakes/h | Window (UTC) |
| ---: | --- | ---: | ---: | ---: | ---: | --- |
| 35 | off | 0/24 | 0 | 3.861 | 0.0 | 14:01:06–14:06:28 |
| 35 | on | 0/24 | 0 | 3.861 | 0.0 | 14:01:06–14:06:28 |
| 65 | off | 0/24 | 0 | 3.862 | 0.0 | 14:06:29–14:11:51 |
| 65 | on | 0/24 | 0 | 3.862 | 0.0 | 14:06:29–14:11:51 |
| 100 | off | 0/24 | 0 | 3.862 | 0.0 | 14:11:51–14:17:13 |
| 100 | on | 0/24 | 0 | 3.862 | 0.0 | 14:11:51–14:17:13 |

### 5.0 Levels and the shape of the negative result

| Volume | AEC | Music-only RMS | Clip-window RMS | Clip peak | Sync alignment |
| ---: | --- | ---: | ---: | ---: | ---: |
| 35 | off | -28.38 dBFS | -28.09 dBFS | 8569 | +0.200 s |
| 35 | on | -50.00 dBFS | -45.43 dBFS | 3720 | +0.200 s |
| 65 | off | -28.21 dBFS | -27.95 dBFS | 9228 | +0.270 s |
| 65 | on | -50.27 dBFS | -48.24 dBFS | 2844 | +0.270 s |
| 100 | off | -27.95 dBFS | -27.37 dBFS | 7399 | +0.270 s |
| 100 | on | -49.48 dBFS | -46.15 dBFS | 2897 | +0.270 s |

The cleaned cells show the AEC doing its job on the music: the music-only RMS
falls by about 21–22 dB and the residual is near the cleaned noise floor. But
the clips are also only about -46 dBFS in the cleaned capture, a few dB above
that residual, and far below the level at which the detector works on this
Pi. The raw cells are worse: the clip is buried in the music (clip-window RMS
within 0.6 dB of music-only).

Two controls separate the detections from the acoustic path:

- **Digital positive:** `clanker_00.wav`, padded to 3 s, detects at 1.80 s
  through the same service and threshold.
- **Real positive, amplified:** a real AEC clip amplified offline by +30 dB
  still does not detect, and the stack's STT transcribes it as "Hmm." while
  the digital fixture transcribes as "Clanker." The word is not intelligible at
  this mic/speaker placement, not merely quiet.

A zero false-wake count over 3.86 min is also a weak bound: with no events, the
95% upper bound is roughly 46 false wakes/hour. It cannot support a 1/hour
claim, and here it is doubly uninformative because the detector never fired at
all, including on the positives.

### 5.1 Two models loaded (follow-up, #79) — not run

The six cells stayed on the single default `clanker` model. The two-model
follow-up was not run; it needs its own positive and music-only exposure.

### 5.2 Wake-free command models (#81) — `volume up` measured, `volume down` absent

Same sessions, same exposure, same real-time replay. `volume up` used the
deployed per-model threshold **0.98**; commands stay disabled in the product.

| Music volume | AEC | `volume up` hits/trials | False triggers | Exposure (min) | False triggers/h | Window (UTC) |
| ---: | --- | ---: | ---: | ---: | ---: | --- |
| 35 | off | 0/12 | 0 | 3.861 | 0.0 | 14:01:06–14:06:28 |
| 35 | on | 0/12 | 0 | 3.861 | 0.0 | 14:01:06–14:06:28 |
| 65 | off | 0/12 | 0 | 3.862 | 0.0 | 14:06:29–14:11:51 |
| 65 | on | 0/12 | 0 | 3.862 | 0.0 | 14:06:29–14:11:51 |
| 100 | off | 0/12 | 0 | 3.862 | 0.0 | 14:11:51–14:17:13 |
| 100 | on | 0/12 | 0 | 3.862 | 0.0 | 14:11:51–14:17:13 |

`volume down` has no shipped model, so it has no live rows. The target was
>= 80% hits and 0 false triggers per cell; the hit half fails in every cell for
the same acoustic reason. The feature stays off (`wakeword.commands.enabled:
false`), and #81 stays open.

## 6. Decision — no cap, and the run is inconclusive on the acoustic path

**No `music.max_volume` value is recommended.** The decision rule fixed in
§0.1 requires an AEC-on cell with >= 80% hits and <= 1 false wake per 10
music-only minutes. Every cell is 0/24, so no AEC-on volume passed and there is
no measured volume to use as a cap. The zero false-wake counts are not evidence
that the detector is quiet: it never fired at all, including on the positives.

The blocker is the **acoustic path**, not the music level:

- AEC removes 21–22 dB of music (raw music-only -28 dBFS, cleaned -50 dBFS).
- The clips reach the cleaned capture at about -46 dBFS, only a few dB above
  the residual and far below the detector's usable input level.
- A digital fixture detects through the same service; a real clip amplified
  +30 dB does not, and STT cannot transcribe it.

Fix the coupling first — a louder Echo output, a closer speaker, or a
higher-gain/cleaner mic — then repeat the same record-first run. Until then:

- keep `music.max_volume` unset (no cap);
- keep `wakeword.commands.enabled: false` (`volume up` 0/12);
- do not make the echo-cancel drop-in persistent.

**Host echo-cancel config to use when the path is fixed:** the runtime command
in §0.4 with the real names `alsa_input.usb-Generalplus_OSM09-00.mono-fallback`
and `bluez_output.5C_41_5A_94_78_A3.1`, `rate=48000`, default WebRTC, and no
persistent drop-in. Keep it runtime-only until a run passes.

Both acceptance boxes on #70 stay unchecked and #70 stays open.

## 7. Versions, config and mounts recorded at the run

| Item | Value on 2026-11-13 |
| --- | --- |
| Host / OS | `raspberrypi`, Raspberry Pi 5, aarch64, kernel `6.12.115+rpt-rpi-2712` |
| PipeWire | 1.2.7 (compiled and linked) |
| Pulse-compatible server | PulseAudio 15.0.0 on PipeWire 1.2.7, protocol 35, `pactl` 16.1 |
| Echo-cancel module | runtime `module-echo-cancel`, index `536870916`, unloaded after the run |
| AEC library | `libspa-aec-webrtc.so` (default) |
| AEC args | `rate=48000` only |
| Drop-in config | none applied |
| Real sink | `bluez_output.5C_41_5A_94_78_A3.1` (Echo Dot-3LG, A2DP SBC) |
| Real source | `alsa_input.usb-Generalplus_OSM09-00.mono-fallback` |
| Virtual sink/source | `aec_music_sink` / `aec_mic_source` |
| Source gain | 150% during the run, restored to 100% |
| Sink gain | 100%, unmuted, restored to the same |
| Audio socket mounts | `assistant` + `librespot`: unchanged from the deploy |
| Container env | `PULSE_SERVER=unix:/run/pulse/native` |
| Assistant image | `42fb047` |

The dev-box names in the original prep were examples only; the table above is
the Pi as measured.

## 8. What remains

1. Fix the mic/speaker coupling: raise the Echo's physical output, move it
   closer to the OSM09 mic, or use a cleaner/higher-gain input. The current
   path delivers the clips at about -46 dBFS in the cleaned capture; the
   detector needs materially more.
2. Re-run the same three-session record-first protocol
   (`scripts/wake_over_music_run.py record` / `detect` / `count`) with that
   fixed path. The scripts and the manifest format are in the repository; the
   recordings stay on the Pi.
3. Only then decide `music.max_volume` or "no cap", check both #70 boxes and
   consider enabling `wakeword.commands.enabled`.
4. `volume down` still has no model; #81 stays open for it regardless of the
   `volume up` result.

This work does not unblock #72. Spotify still needs its own implementation and
merging; this spike only prepares the measurement it depends on.
