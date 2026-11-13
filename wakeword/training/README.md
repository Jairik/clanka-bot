# The `clanker` wake word

`wakeword/models/clanker.tflite` is trained here, not downloaded. It is an
[openWakeWord](https://github.com/dscripka/openWakeWord) detector at the pinned
revision in `run.sh`, and the wakeword image bakes it in next to the upstream
built-ins. This file covers recording the owner's voice, retraining, reading the
evaluation numbers, and deploying.

Everything is reproducible from public, pinned sources: openWakeWord, two
Hugging Face feature/RIR datasets, an AudioSet shard set for background noise,
and the same Pocket TTS model `tts` uses for its voice. Nothing is uploaded; the
work directory is `$WORK` (default `~/.cache/clanka/wakeword-clanker`).

## Record your own voice (the part only you can do)

The synthetic model learns the *word*; it cannot know your voice, your mic or
your room. Record takes before trusting the detector in the house:

```bash
wakeword/training/record.py --list-sources        # Pulse source names
wakeword/training/record.py --positive 50 --negative 30
wakeword/training/record.py --device alsa_input.usb-webcam.mono-fallback
```

- Each take is 2 s at 16 kHz mono, recorded through PipeWire/PulseAudio
  (`parecord`) or ALSA (`arecord`, `--backend alsa`). The default source is the
  system default. Takes are trimmed to the speech plus 150 ms before and 300 ms
  after, and capped at 1.9 s so the training augmentation buffer always fits.
- After each take the prompt is `[p]lay / [k]eep / [r]edo / [q]uit`. `r` deletes
  the take and records again under the same number.
- Positives cycle through varied deliveries: quiet, loud, fast, slow, across the
  room, while music plays, different tones. Do not use one cadence 50 times; a
  detector that only knows your "best" pronunciation fails at the worst moment.
- Negatives are the phrases in `near_misses.txt` (clank, clunk, clinker, "clan
  car", …) followed by ordinary speech, so the model learns what is *not* the
  word. They are used as hard negatives in training and half of them (the 20%
  holdout) as an honest false-accept number.
- Recordings go to `$WORK/real/{positive,negative}` as `real_p_00001.wav` /
  `real_n_00001.wav`. Re-running continues the numbering and never overwrites a
  take. `.gitignore` blocks the directory and any stray WAV under
  `wakeword/training/`, so recordings are never committed.
- 50 positives and 30 negatives is about 15 minutes. More is better; 150
  positives across several sessions is a good target, and recording a few more
  later always beats re-recording everything.

Tips: 30 cm from the mic is a reasonable "normal" distance; keep the mic gain
where the assistant actually runs; include the noisy cases you care about
(music playing, running water, a fan). Silence is not a take — the trim will
keep the whole clip and you will hear it on playback.

## Short recording session on the Pi

Run `scripts/record-wakeword-pi.sh` in an interactive Pi terminal to collect
25 positive takes and 15 negatives with three seconds per take. It uses the
default Pulse microphone and the same play/keep/redo controls as `record.py`.
Arguments are forwarded, so `--positive`, `--negative`, `--device` and `--work`
can override these defaults. Recordings stay outside Git in the existing work
directory. Temporarily stop the assistant before recording to prevent spoken
replies contaminating takes, then start it again afterward. The helper itself
does not change service state.

## Retrain and deploy

Stages run in order, and re-running a finished stage is cheap: clips and feature
files are skipped when they already exist.

```bash
wakeword/training/run.sh setup       # venvs, openWakeWord, pinned deps  (once)
wakeword/training/run.sh assets      # ACAV features, RIRs, AudioSet background
wakeword/training/run.sh clips       # 10k + 2k synthetic positives, 10k + 2k hard negatives
wakeword/training/run.sh acceptance  # held-out TTS voices for the detection test
wakeword/training/run.sh real        # 80/20 split of your takes into training/holdout
wakeword/training/run.sh train       # augment + extract features + train (writes clips/clanker/clanker.onnx)
wakeword/training/run.sh convert     # ONNX -> float32 TFLite, checked against onnxruntime
wakeword/training/run.sh evaluate    # recall, false accepts/hour, real-voice holdout
wakeword/training/run.sh fixtures    # refresh the synthesized WAVs in wakeword/tests/fixtures
```

`run.sh all` does everything in that order. Training works with no recordings:
`real` prints a note and the pipeline stays synthetic-only.

`train` ends with an error from openWakeWord's own `onnx -> tflite` step, which
needs `onnx-tf`; the ONNX is written before that and `convert` does the
conversion with `onnx2tf`, so the error is expected and `run.sh` checks for the
ONNX instead of trusting the exit status.

Deploy is the normal path:

```bash
GIT_SHA=$(git rev-parse --short HEAD) docker compose build wakeword
docker compose up -d wakeword assistant
```

Then confirm the acceptance seam against the built image:

```bash
make wakeword-test          # or: wakeword/tests/run_detection_test.sh
```

It builds the real image, asks the real Wyoming service for `clanker`, asserts
the built-ins are still advertised, and checks a synthesized `clanker` fires
while near misses do not.

## Numbers

The committed detector: 10,000 synthetic positives and 10,000 synthetic hard
negatives from 25 Pocket TTS voices, each clip augmented once with a background
mix and one of 2,500 AudioSet background recordings plus a real room impulse
response, then 50,000 training steps (plus openWakeWord's two 5,000-step
refinement passes) with 1,024 ACAV feature rows per step from a 200k-row slice.

`evaluate.py` at the deployed threshold 0.5:

| set | what it measures | result |
| --- | --- | --- |
| 2,000 synthetic positives, training voices | recall on unseen clips | 87.5% |
| 60 synthetic positives, 2 voices held out of training | recall on an unseen voice | 96.7% |
| 60 same clips through the built image over Wyoming | the deployed path, not just bulk prediction | 95.0% (57/60) |
| 40 ordinary phrases, held-out voices | wakes on normal speech | 0.0% |
| 80 near-miss phrases, held-out voices | wakes on a similar word | 11.3% |
| 6.9 h of AudioSet background | false accepts per hour | 0.29 |

Threshold sweep (`WAKEWORD_THRESHOLD`):

| threshold | recall, training voices | recall, held-out voices | false accepts per hour |
| --- | --- | --- | --- |
| 0.3 | 89.3% | 96.7% | 0.29 |
| 0.4 | 88.2% | 96.7% | 0.29 |
| 0.5 (default) | 87.5% | 96.7% | 0.29 |
| 0.6 | 86.3% | 96.7% | 0.29 |
| 0.7 | 85.2% | 93.3% | 0.15 |

The near-miss false accepts are concentrated where you would expect them. Of the 80
clips, the 6 that are the word itself or a contraction ("clanker's", "clankers")
fire 33% of the time, and the ones that are one phoneme away ("clunker",
"clanger", "cranker", "clank her") fire most of the rest; the 74 clips that are
genuinely different words fire 9.5% of the time. "clank", "clanks", "clang",
"clinic", "clinker", "blanker", "tanker", "anchor", "plankton" and ordinary
requests scored at or below 0.16 in that run. The phrases that still fire are in
the `run.sh evaluate` breakdown, not hidden.

Read these as what they are: recall on *voices held out of training* and false
accepts per hour on *recorded background audio*, scored through the same feature
pipeline the service uses. They are not a room measurement — that needs
`record.py` and the real mic.

One quirk worth knowing when writing a test: the service scores 16 embedding
frames (1.28 s) at a time, so a WAV shorter than that sent as a complete stream
never fills a window and cannot fire. Real capture is continuous. The detection
test pads its fixtures with silence for that reason.

`evaluate.py` counts a false accept the way the service behaves: a threshold
crossing, with the 2 s refractory window the compose file passes. Pass
`--real-positive` / `--real-negative` (which `run.sh evaluate` does when a
holdout exists) to get the honest numbers for your own voice, reported
separately from the synthetic ones.

## Threshold and model choice

`WAKEWORD_THRESHOLD` (default 0.5) is the knob. Lower catches more takes and
more false wakes; raise it if the assistant wakes on the TV or on "clank". The
evaluation sweeps 0.3 to 0.7; pick from the table rather than guessing.

`WAKEWORD_MODEL` names the detector the assistant asks for. `clanker` is the
default here because it clears the bar that matters for a synthetic-only model:
96.7% recall on a voice it never heard, no wake on ordinary speech, and 0.3
false wakes per hour of background audio. `okay_nabu`, `hey_jarvis`,
`hey_mycroft`, `hey_rhasspy` and `alexa` are still served from the upstream
image and can be selected by name, so switching back is one variable. An unknown
name silently falls back to `okay_nabu` inside the service, so keep
`WAKEWORD_MODEL` and the assistant's `wakeword.model` in step.

## Not verified here

- Live room accuracy from the owner's mic: needs `record.py` takes and a real
  wake in the room. The synthetic numbers cannot stand in for that.
- Wake word over music at realistic levels: the background augmentation covers
  noise and RIRs, and the dev-box wake-over-music run used `okay_nabu`. Re-run
  `scripts/wake_over_music.py` against `clanker` before trusting it over Spotify.
- CPU cost on the Pi: the TFLite detector is the same shape as the upstream
  ones (16x96 input, two 32-unit layers), so the per-frame cost should match,
  but nothing has measured it on the Pi 5.
