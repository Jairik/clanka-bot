"""Score the trained detector and report recall and false accepts per hour.

The synthetic sets are Pocket TTS clips from voices held out of training, so a
high score there only says the model learned the word, not that it hears a
particular room. When the owner's own takes exist, pass --real-positive and
--real-negative to report them as a separate, honest number.

False accepts per hour are counted the way the service behaves: one detection
event per threshold crossing, with a 2 s refractory window (the compose
--refractory-seconds default), over continuous background audio.
"""

import argparse
import json
import wave
from pathlib import Path

import numpy as np
from openwakeword.utils import bulk_predict

SWEEP = (0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.85, 0.9, 0.95, 0.98)
# openWakeWord scores one frame per 1280 samples (80 ms) at 16 kHz.
FRAME_SECONDS = 1280 / 16000
REFRACTORY_SECONDS = 2.0


def paths(directory):
    return sorted(Path(directory).glob("*.wav")) if directory else []


def manifest(directory):
    """{filename: phrase} from a clip set's per-worker manifests, when present."""
    out = {}
    if not directory:
        return out
    for path in Path(directory).glob("manifest*.tsv"):
        for line in path.read_text().splitlines():
            name, _, phrase = line.partition("\t")
            if name:
                out[name] = phrase
    return out


def phrase_breakdown(name, scores, phrases, threshold, target):
    """Split a near-miss set into phrases that contain the word and phrases that do not."""
    if not phrases:
        return
    groups = {"contains the word": ([], []), "other phrases": ([], [])}
    for path, frames in scores.items():
        phrase = phrases.get(Path(path).name, "").lower()
        key = "contains the word" if target in phrase else "other phrases"
        bucket = groups[key]
        bucket[0].append(max(frames) if frames.size else 0.0)
        bucket[1].append(phrase)
    print(f"  {name} breakdown at {threshold:.2f}:")
    for label, (peaks, _) in groups.items():
        if peaks:
            rates = float((np.array(peaks) > threshold).mean())
            print(f"    {label}: {rates:.2%} of {len(peaks)} clips fire")
    worst = {}
    for path, frames in scores.items():
        phrase = phrases.get(Path(path).name, "?")
        score = max(frames) if frames.size else 0.0
        worst[phrase] = max(worst.get(phrase, 0.0), score)
    top = sorted(worst.items(), key=lambda kv: -kv[1])[:6]
    print("    worst phrases: " + ", ".join(f"{phrase!r} {score:.2f}" for phrase, score in top))


def sample(files, limit):
    """Evenly spaced subset, so a long background set is scored from start to end."""
    if limit <= 0 or len(files) <= limit:
        return files
    step = len(files) / limit
    return [files[int(i * step)] for i in range(limit)]


def frame_scores(files, model, ncpu):
    """Return {path: per-frame score array} for every WAV in ``files``.

    ``bulk_predict`` returns one dict per 80 ms frame (model name -> score).
    """
    if not files:
        return {}
    ncpu = max(1, min(ncpu, len(files)))
    preds = bulk_predict([str(f) for f in files], wakeword_models=[model], ncpu=ncpu)
    out = {}
    for path, per_frame in preds.items():
        values = [float(next(iter(frame.values()))) if isinstance(frame, dict) else float(frame)
                  for frame in per_frame]
        out[path] = np.asarray(values, dtype=np.float64).ravel()
    return out


def wav_seconds(path):
    with wave.open(str(path)) as w:
        return w.getnframes() / w.getframerate()


def report_clips(name, scores, thresholds, expect_hit):
    peaks = np.array([f.max() if f.size else 0.0 for f in scores.values()])
    if not peaks.size:
        print(f"{name}: no clips")
        return []
    kind = "recall" if expect_hit else "false-accept rate"
    print(f"{name}: n={peaks.size} peak mean={peaks.mean():.3f} p50={np.median(peaks):.3f} max={peaks.max():.3f}")
    for t in thresholds:
        print(f"  {kind} at {t:.2f}: {float((peaks > t).mean()):.2%}")
    return peaks


def report_events(name, scores, thresholds):
    """Detection events per hour over continuous audio."""
    hours = sum(wav_seconds(path) for path in scores) / 3600.0
    if hours == 0:
        print(f"{name}: no audio")
        return {}
    out = {}
    for t in thresholds:
        events = 0
        for path, frames in scores.items():
            last = -1e9
            t_frame = 0.0
            for score in frames:
                if score > t and (t_frame - last) >= REFRACTORY_SECONDS:
                    events += 1
                    last = t_frame
                t_frame += FRAME_SECONDS
        per_hour = events / hours
        out[f"{t:.2f}"] = per_hour
        print(f"  {name} false accepts per hour at {t:.2f}: {per_hour:.2f} "
              f"({events} events in {hours * 60:.1f} min of audio)")
    return out


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--model", required=True, help="path to the .tflite detector")
    p.add_argument("--positives", required=True, help="synthetic positive clips from training voices")
    p.add_argument("--negatives", required=True, help="synthetic near-miss negative clips from training voices")
    p.add_argument("--voices", default="", help="positive clips from voices held out of training")
    p.add_argument("--near-misses", default="", help="acceptance near-miss clips, if generated")
    p.add_argument("--speech", default="", help="ordinary speech clips; any fire is a false accept")
    p.add_argument("--background", default="", help="continuous background audio for false-accepts/hour")
    p.add_argument("--background-max", type=int, default=400, help="score at most this many background files (evenly spaced)")
    p.add_argument("--real-positive", default="", help="owner's held-out real takes")
    p.add_argument("--real-negative", default="", help="owner's held-out real negative takes")
    p.add_argument("--threshold", type=float, default=0.5, help="the threshold the service runs at")
    p.add_argument("--target", default="clanker", help="the wake word, for the phrase breakdown")
    p.add_argument("--ncpu", type=int, default=8)
    p.add_argument("--json-out", default="")
    args = p.parse_args()

    thresholds = sorted(set(SWEEP) | {args.threshold})
    results = {}

    for name, key, directory, expect in (
        ("positives (training voices, unseen clips)", "positives", args.positives, True),
        ("positives (voices held out of training)", "voices", args.voices, True),
        ("negatives", "negatives", args.negatives, False),
        ("near-misses", "near_misses", args.near_misses, False),
        ("ordinary speech", "speech", args.speech, False),
        ("real positives (held out)", "real_positive", args.real_positive, True),
        ("real negatives (held out)", "real_negative", args.real_negative, False),
    ):
        scores = frame_scores(paths(directory), args.model, args.ncpu)
        if scores:
            report_clips(name, scores, thresholds, expect)
            if not expect:
                phrase_breakdown(name, scores, manifest(directory), args.threshold, args.target)
            results[key] = {path: frames.tolist() for path, frames in scores.items()}

    background = frame_scores(sample(paths(args.background), args.background_max), args.model, args.ncpu)
    if background:
        print("background audio (continuous):")
        results["background_events_per_hour"] = report_events("background", background, thresholds)

    def rate(key, expect):
        if key not in results:
            return None
        peaks = np.array([max(f) if f else 0.0 for f in results[key].values()])
        return float((peaks > args.threshold).mean())

    print(f"at the deployed threshold {args.threshold:.2f}:")
    for label, key, expect in (
        ("recall, training voices (unseen clips)", "positives", True),
        ("recall, voices held out of training", "voices", True),
        ("training-negative false-accept rate", "negatives", False),
        ("near-miss false-accept rate", "near_misses", False),
        ("ordinary-speech false-accept rate", "speech", False),
        ("real held-out recall", "real_positive", True),
        ("real negative false-accept rate", "real_negative", False),
    ):
        value = rate(key, expect)
        if value is not None:
            print(f"  {label}: {value:.2%}")
    events = results.get("background_events_per_hour", {})
    if events:
        print(f"  background false accepts per hour: {events[f'{args.threshold:.2f}']:.2f}")

    if results.get("near_misses"):
        worst = sorted(results["near_misses"].items(), key=lambda kv: -max(kv[1]) if kv[1] else 0)[:10]
        print("highest near-miss scores:")
        for path, frames in worst:
            print(f"  {max(frames):.3f} {Path(path).name}")

    if args.json_out:
        Path(args.json_out).write_text(json.dumps(results, indent=2))
        print(f"wrote {args.json_out}")


if __name__ == "__main__":
    main()
