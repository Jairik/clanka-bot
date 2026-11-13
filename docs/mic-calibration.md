# Mic calibration before a human-mic run (#36)

The endpointer is deliberately fixed: a 500 ms silence window, a configurable
dBFS threshold, and a 15 s utterance cap. It does **not** adapt to the room.
That makes the threshold/gain check a human step, not product logic.

The dev-box correction and human check passed on 2026-11-12: **32% source gain,
-47.4 dBFS idle RMS, 501 ms speech endpoint**. See the
[calibration record](mic-calibration-2026-11-12.md). Re-measure after changing the
device, gain or room conditions.

The #29 run measured the dev-box Webcam C270 Mono at 100 % source volume
(base 32 % / -30 dB) with idle RMS **-37.9, -33.6 and -29.7 dBFS**; the 4 s
sample had 120/125 frames above the shipped -38 dBFS threshold. A wake word on
that setting opens an utterance on noise and only ends at `max_utterance`.
The measurements are in
[`prototype-run/mic-noise.txt`](prototype-run/mic-noise.txt) and
[`prototype-run/mic-noise-samples.txt`](prototype-run/mic-noise-samples.txt).

## Measure, do not guess

`scripts/mic-noise-check.sh` records the configured source (read-only) and
prints the same frame RMS statistics:

```sh
# Default source, 5 seconds; stay quiet.
scripts/mic-noise-check.sh

# A named source and a longer window.
scripts/mic-noise-check.sh alsa_input.usb-046d_0825_EC865510-02.mono-fallback 8
```

It reports idle RMS, frame p50/p95/peak, frames above the threshold, and a
suggested threshold at idle RMS + 6 dB (capped so speech can still clear it).
It never changes the host source volume.

## Choose a correction

Either is acceptable if re-measured:

- **Lower the source gain** so idle RMS is around -45 dBFS, then keep the
  product default `-38`. Example (inspect first, do not paste blindly):
  `pactl set-source-volume <source> 32%`.
- **Raise `stt.endpoint.threshold_dbfs`** in `assistant/config.yaml` to at
  least the measured idle RMS + 6 dB, and record the value and the measurement.

Do not change the fixed 500 ms silence window to compensate; that is a latency
budget decision (#12), not a noise fix. Do not commit a dev-box-only gain as a
product default without a measured human-mic run.

## Human check after calibration

This tool proves the level; it cannot prove the assistant hears a person. The
human-spoken #29 acceptance run still requires its full scenario set. The
single wake-and-spoken-turn check for #36 is recorded above. Record the chosen
gain/threshold and the new measurement before another run.
