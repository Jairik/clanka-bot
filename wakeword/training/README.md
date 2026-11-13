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
From the desktop, launch it with
`ssh -t rbpi@pi 'cd ~/clanka-bot && scripts/record-wakeword-pi.sh'`;
capture still happens on the Pi.
Arguments are forwarded, so `--positive`, `--negative`, `--device` and `--work`
can override these defaults. Recordings stay outside Git in the existing work
directory. Temporarily stop the assistant before recording to prevent spoken
replies contaminating takes, then start it again afterward. The helper itself
does not change service state.

## Record in the Dashboard, train on the desktop

The Pi records and exports; the desktop GPU trains. `wakeword/train` is the
desktop entry point and a thin orchestrator over `run.sh` and the scripts in
this directory.

```sh
wakeword/train status                       # phrases, real/synthetic counts, last eval
wakeword/train import rbpi@pi:/home/rbpi/clanka-bot/exports/clanker-20261112-093000.tar.gz
wakeword/train synth  --phrase "volume up" --count 6000
wakeword/train train  --phrase "volume up" --steps 50000 --real-weight 3
wakeword/train eval   --phrase "volume up"
wakeword/train install --phrase "volume up" --to pi
wakeword/train all    --phrase "volume up" --from ./volume-up-20261112-093000.tar.gz
```

Each phrase has a work directory, default
`~/.cache/clanka/wakeword-<slug>`. The `clanker` path is the existing
`wakeword-clanker` work directory, so the commands above keep using the assets
and clips already there. A new phrase's directory needs its own second-stage
setup once (the venvs and pinned assets are per work directory):

```sh
WORK=~/.cache/clanka/wakeword-volume-up wakeword/training/run.sh setup assets
```

Nothing is uploaded, and takes copied into the work directory never enter the
checkout.

**import** accepts a Dashboard bundle (`.tar.gz`), a directory, or
`user@host:path`. It fetches a remote path with
`scp -o BatchMode=yes`, validates every clip as 16 kHz mono 16-bit WAV between
0.2 s and 2.0 s, requires 5–100 positives, and continues the
`real_p_00001.wav` numbering. Re-importing the same set is a no-op: the PCM
digest is the dedupe key, so a different WAV header does not create a second
take. A tailnet policy in `check` mode makes scp fail with a sign-in hint; run
`ssh rbpi@pi` once in a terminal, then repeat the import. The tar reader refuses
absolute paths, `..`, links and unexpected entries.

**synth** writes positive and negative clips with Pocket TTS, plus the
held-out-voice acceptance set, into `$WORK/clips/<phrase>/` and
`$WORK/clips/<phrase>/acceptance/` (`<phrase>` is the underscore model name,
e.g. `volume_up`). The defaults come from the checked-in phrase config:
`clanker` uses 10k/2k clips and 50k steps; `volume_up`/`volume_down` use
6k/1.2k and 30k. Flags override them: `--count`, `--test-count`,
`--negative-count`, `--voices all|<list>`, `--held-out <list>`,
`--speeds 0.8-1.2`, `--speed-probability`, `--workers`, `--seed`, `--device`.
`generate_clips.py` only knows the factors 0.9, 0.95, 1.05 and 1.1, so a range
is mapped onto those. An optional CLI config at
`wakeword/training/phrases/<slug>.yml` (hyphen slug) may carry the same values
plus curated text lists; it is separate from the openWakeWord `<phrase>.yml`
and the `phrases/<phrase>/` list directories. Without one, a new phrase gets
generated text variants and near misses and the shared ordinary-speech list.
`clanker`, `volume_up` and `volume_down` keep their curated positives,
negatives, near misses and ordinary speech. A `--dry-run` prints every command
without touching the GPU.

**train** writes `$WORK/<phrase>.yml` from the checked-in config and the
requested `--steps` (the clip counts become `n_samples`, so openWakeWord never
asks the stub Piper generator for more), splits the real takes 80/20 through
`prep_real.py`, runs `run.sh train convert`, and logs to
`$WORK/logs/train-<timestamp>.log`. `--real-weight N` duplicates each real
positive N times in `positive_train`, which is the only weighting
openWakeWord's per-clip generator exposes. The split is skipped, and the
extracted features kept, when the real set and weight are unchanged, so an
interrupted run resumes at feature extraction rather than starting over. A
changed YAML `steps` retrains the model but reuses the features.

**eval** runs `run.sh evaluate` and reads `$WORK/logs/eval-<phrase>.json` to print, at
the deployed threshold, recall on the held-out TTS voices and on the ~20% of
real takes held out separately, near-miss and ordinary-speech false accepts,
and false accepts per hour over the background set.

**install** without `--to pi` verifies `wakeword/models/<phrase>.tflite` (the underscore model name, e.g. `volume_up.tflite`) and
prints the commit and redeploy commands. `install --to pi` copies it to
`~/clanka-bot/wakeword/models/` on `rbpi@pi` with `BatchMode=yes` and prints a
`docker compose build wakeword && up -d` command, because `deploy.sh` refuses a
dirty checkout and the copied file is untracked. For a durable install, commit
the model on the desktop and deploy normally. Either way, set
`WAKEWORD_MODEL=<phrase>` in `.env` and restart `assistant`.

**status** lists every `~/.cache/clanka/wakeword-*` work directory with its
real and synthetic counts, whether its model exists, and the last eval summary.

The Dashboard's `/voice-samples` page writes the same layout: keeps go straight
to `real/positive` and `real/negative`, and Submit writes
`<slug>-<date>-<time>.tar.gz` with a `manifest.json` next to `real/`. Playback
in the page is from a blob for an unkept take and from an authenticated
same-origin route for a kept one, which is why the app document's CSP carries
`media-src 'self' blob:`.

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
`real` prints a note and the pipeline stays synthetic-only. Acceptance clips
and the real-take holdout now live under `$WORK/clips/<phrase>/`, so an older
work directory needs one `acceptance` run before `evaluate`.

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

`WAKEWORD_THRESHOLD` (default 0.5) is the default threshold; `WAKEWORD_THRESHOLDS`
sets it per model (`clanker:0.6,okay_nabu:0.5`). Lower catches more takes and
more false wakes; raise it if the assistant wakes on the TV or on "clank". The
evaluation sweeps 0.3 to 0.7; pick from the table rather than guessing.

`WAKEWORD_MODELS` is the comma-separated list of detectors the assistant asks
for; the older `WAKEWORD_MODEL` still names one. `clanker` is the default here
because it clears the bar that matters for a synthetic-only model:
96.7% recall on a voice it never heard, no wake on ordinary speech, and 0.3
false wakes per hour of background audio. `okay_nabu`, `hey_jarvis`,
`hey_mycroft`, `hey_rhasspy` and `alexa` are still served from the upstream
image and can be selected by name, so switching back is one variable. The
detect request carries an explicit allowlist and a threshold map: a model that
is not listed is never loaded for that stream, and a detection from any other
name is ignored by the assistant. Two models work in one service and share the
feature extractor, but their false wakes add up; keep the default to the one
trained word unless the second is earning its keep.

## Not verified here

- Live room accuracy from the owner's mic: needs `record.py` takes and a real
  wake in the room. The synthetic numbers cannot stand in for that.
- Wake word over music at realistic levels: the background augmentation covers
  noise and RIRs, and the dev-box wake-over-music run used `okay_nabu`. Re-run
  `scripts/wake_over_music.py` against `clanker` before trusting it over Spotify.
- CPU cost on the Pi: the TFLite detector is the same shape as the upstream
  ones (16x96 input, two 32-unit layers), so the per-frame cost should match,
  but nothing has measured it on the Pi 5.

## Wake-free command models (#81)

`volume_up` and `volume_down` are the same kind of detector as `clanker`, but a
detection does not start a turn. It runs one bounded volume step and a short Cue
with no STT and no routing, and it only acts while music or a reply holds Focus.
The feature ships **off** (`WAKEWORD_COMMANDS_ENABLED=false`) until models
trained with the owner's takes and the measurement below exist.

The pipeline is phrase-parameterized. `PHRASE=volume_up` selects
`volume_up.yml` and the lists under `phrases/volume_up/`; `clanker` keeps the
original top-level files:

```bash
# Assets and the TTS venv are shared with the clanker work directory.
wakeword/training/run.sh setup
wakeword/training/run.sh assets

# A smaller starter set is enough for a two-word phrase.
PHRASE=volume_up N_TRAIN=6000 N_TEST=1200 wakeword/training/run.sh clips
PHRASE=volume_up wakeword/training/run.sh acceptance
PHRASE=volume_up wakeword/training/run.sh real       # only if takes exist
PHRASE=volume_up wakeword/training/run.sh train
PHRASE=volume_up wakeword/training/run.sh convert
PHRASE=volume_up wakeword/training/run.sh evaluate
```

Repeat with `PHRASE=volume_down`. Clips land in `$WORK/clips/<phrase>/`, the
ONNX in `$WORK/clips/<phrase>.onnx`, the detector in
`wakeword/models/<phrase>.tflite`, and the evaluation JSON in
`$WORK/logs/eval-<phrase>.json`. The `real` stage reads `$WORK/real/`, so give
each phrase its own work directory (`WORK=~/.cache/clanka/wakeword-volume_up`)
once real takes are involved, or import that phrase's takes before running it.

The phrase configs are deliberately strict:

- **Only the bare phrase is positive** (`phrases/<phrase>/positives.txt`).
  "turn the volume up" and "pump up the volume" are negatives; they are common
  in lyrics and must not change the volume by themselves.
- **The other command is a hard negative** (`volume_down` in the `volume_up`
  set and vice versa), so one cannot masquerade as the other.
- **Near misses** (`near_misses.txt`) are the held-out false-accept set:
  "volume", "value up", "volume off", "column up", "volunteer", …
- **Negative phrases** (`negative_phrases.txt`) combine the near misses, their
  variants, ordinary requests and lyric-like lines. They become the
  hard-negative set in training.
- **Music background**: openWakeWord mixes every WAV under `$WORK/background`
  into augmented clips; AudioSet already contains music. For a music-heavy
  run, drop a few 16 kHz mono WAVs of representative songs with vocals into
  `$WORK/background` before `train`. Do not commit them.

`volume_up.yml` / `volume_down.yml` use 6k positive training clips, 30k steps
and a `target_false_positives_per_hour` of 0.1. `evaluate.py` reports recall on
held-out voices, near-miss false accepts and false accepts per hour of
background. A synthetic-only model can be committed only if that number is
sane; otherwise keep the configs and record the missing measurement.

The command default threshold is 0.7 (`wakeword.commands.threshold`) for a
model without an override. The shipped synthetic `volume_up` starter is an
exception: it needs **0.98** to meet the false-trigger budget, set per model in
`assistant/config.yaml` (`wakeword.thresholds.volume_up`). Override any model
with `WAKEWORD_THRESHOLDS=...`. The wakeword image copies every
`wakeword/models/*.tflite`, so adding a committed model needs a rebuild, not a
Dockerfile change. The false-trigger protocol for the command models is §5.2 of
[`docs/spikes/wake-over-music.md`](../../docs/spikes/wake-over-music.md).

### `volume_up` starter: provenance and numbers (2026-11-13)

| | |
| --- | --- |
| File | `wakeword/models/volume_up.tflite` |
| SHA256 | `f701c5316b6e4dbab8fac1e665d40a977a1c181b5fc2cc31d63f3fb000f910d3` |
| Trained | 2026-11-13 on the desktop RTX 2070, nice 10 |
| openWakeWord | `368c03716d1e92591906a84949bc477f3a834455` (pinned in `run.sh`) |
| Positives | 6,000 train + 1,200 test clips, 25 Pocket TTS voices, "volume up" only |
| Negatives | 6,000 train + 1,200 test clips from `phrases/volume_up/negative_phrases.txt` |
| Training | 30,000 steps plus openWakeWord's refinement passes, dnn layer 32, target 0.1 FA/h |
| Reproduce | `PHRASE=volume_up N_TRAIN=6000 N_TEST=1200 GEN_WORKERS=2 wakeword/training/run.sh ...` (`clips`, `acceptance`, `train`, `convert`) |

Evaluation on 2026-11-13 (`evaluate.py --threshold 0.98 --background-max 2500`;
2,500 AudioSet files = 413 min; held-out TTS voices):

| Set | Result at 0.98 |
| --- | ---: |
| positives, training voices (1,200 clips) | 96.92% recall |
| positives, held-out voices (60 clips) | 95.00% recall |
| training negatives (1,200 clips) | 1.50% false accept |
| held-out near misses (80 clips) | 5.00% false accept |
| ordinary speech (40 clips) | 0.00% |
| background audio (413 min) | 0.29 false accepts/hour (2 events) |

The operating point is tight: at 0.97 the background rate is 2.47/h; only 0.98
meets the ≤1/h budget with ≥80% held-out recall. The four held-out near-miss
false accepts are "volume off" variants; "volume off" is the known confusion.
The `.tflite` is synthetic-only: the owner's real takes and #70's music
measurement are still required before enabling the feature.

`volume_down` was trained with the same 6k/30k recipe and **not committed**: at
0.98 it held 100% held-out recall but fired on 16.25% of held-out near misses
and 5.52 background events/hour; 0.995 still gave 2.72/h, while 0.996 dropped
held-out recall to 48.3%. No threshold met both ≥80% recall and ≤1/h, so the
model and its SHA256 (`4c999d2caf19659aa556f0261bea2358d58a26d47b0880ec6bf56c2201993bed`)
are recorded here only. A 50k-step hard-negative retrain of `volume_up` was
also rejected: at 0.7 it produced 94 background events/hour. Retraining both
with the owner's samples (#80/#77) is the next step.
