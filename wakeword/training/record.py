#!/usr/bin/env python3
"""Record the owner's own "clanker" takes for training and evaluation.

Positive takes are your voice saying the wake word with deliberate variation;
negative takes are near misses and ordinary speech, so the model learns what is
*not* the word. Recordings go to ``$WORK/real/{positive,negative}`` and are
never written into the git checkout.

Takes are trimmed to the speech plus a short pre-roll, capped so the training
augmentation buffer (2 s) always fits, and numbered so a re-run continues where
the last one stopped. Nothing is ever overwritten.
"""

import argparse
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import wave
from pathlib import Path

import numpy as np

SR = 16000
CHANNELS = 1
SAMPLE_WIDTH = 2
DEFAULT_WORK = Path(os.environ.get("WORK", Path.home() / ".cache/clanka/wakeword-clanker"))

# Varied delivery: a detector that only knows one loudness and one distance
# fails exactly where a voice assistant is used.
POSITIVE_PROMPTS = [
    "normal voice, your usual distance from the mic",
    "normal voice, slightly further back",
    "quiet voice, close to the mic (late at night)",
    "loud voice, as if calling across a room",
    "fast, clipped delivery",
    "slow, drawn out: clannn-ker",
    "flat, bored tone",
    "bright, cheerful tone",
    "while music is playing in the room",
    "while you are washing up / moving around",
    "across the room, normal voice",
    "across the room, raised voice",
    "turning your head away from the mic",
    "sitting down, relaxed",
    "just after waking up (rough voice)",
    "as a question: clanker?",
    "while a fan or extractor is on",
    "with a hand near your face",
    "normal voice again",
    "any way you would actually say it",
]

ORDINARY_MUNDANE = [
    "clank the dishes together",
    "the robot is a clanker",
    "that clanker broke again",
    "clanker, stop",
    "hey clanker",
    "okay clanker",
    "clanker please",
    "what time is it",
    "turn on the kitchen light",
    "set a timer for ten minutes",
    "play some music",
    "stop the music",
    "what's the weather like",
    "lock the front door",
    "remind me to buy milk",
]


def read_lines(path):
    if not path.exists():
        return []
    return [line.strip() for line in path.read_text().splitlines() if line.strip() and not line.startswith("#")]


def negative_prompts(training_dir):
    """Near misses first (they matter most), then ordinary speech, de-duplicated."""
    seen, phrases = set(), []
    for phrase in read_lines(training_dir / "near_misses.txt") + read_lines(training_dir / "negative_phrases.txt") + ORDINARY_MUNDANE:
        key = phrase.lower()
        if key not in seen:
            seen.add(key)
            phrases.append(phrase)
    return phrases


def run(cmd, **kwargs):
    return subprocess.run(cmd, check=True, **kwargs)


def list_sources():
    if not shutil.which("pactl"):
        return
    out = subprocess.run(["pactl", "list", "short", "sources"], capture_output=True, text=True)
    for line in out.stdout.splitlines():
        print("  " + line)


def record(backend, device, seconds):
    """Record one take and return it as a float32 mono array at 16 kHz.

    Raw PCM is captured and wrapped here rather than asking the recorder for a
    WAV: parecord leaves a header-only file when it is interrupted, and the
    header is the one thing we do not need it for.
    """
    with tempfile.NamedTemporaryFile(suffix=".pcm", delete=False) as handle:
        raw = handle.name
    try:
        if backend == "pulse":
            # --latency-msec keeps the server-side buffer small: with the
            # default, a 2 s capture comes back as an empty file because the
            # first chunks are still queued when we stop.
            cmd = ["parec", "--format=s16le", f"--rate={SR}", f"--channels={CHANNELS}",
                   "--latency-msec=100", f"--device={device or '@DEFAULT_SOURCE@'}", "--raw", raw]
        else:
            cmd = ["arecord", "-q", "-f", "S16_LE", "-r", str(SR), "-c", str(CHANNELS), "-t", "raw", raw]
            if device:
                cmd[1:1] = ["-D", device]
        proc = subprocess.Popen(cmd, stderr=subprocess.PIPE)
        try:
            time.sleep(seconds)
        finally:
            proc.send_signal(signal.SIGINT)
            try:
                _, err = proc.communicate(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                _, err = proc.communicate()
        pcm = np.fromfile(raw, dtype=np.int16)
        if pcm.size == 0:
            message = (err or b"").decode(errors="replace").strip()
            raise RuntimeError(f"the recorder produced no audio: {message or cmd[0] + ' failed'}")
        return pcm.astype(np.float32) / 32768.0
    finally:
        Path(raw).unlink(missing_ok=True)


def trim(audio, pre_seconds=0.15, post_seconds=0.30, max_seconds=1.9, floor_db=-45.0):
    """Trim to the speech, keeping a short pre-roll, and cap the clip length.

    The gate is relative to the room: a webcam mic idles around -41 dBFS, so an
    absolute floor would call the fan a phrase. Returns (clip, found_speech); a
    take with nothing above the noise is kept whole and reported, so the
    operator can redo it instead of silently training on silence.
    """
    if audio.size == 0:
        return audio, False
    frame = int(0.02 * SR)
    usable = (audio.size // frame) * frame
    if usable == 0:
        return audio, False
    rms = np.sqrt(np.mean(audio[:usable].reshape(-1, frame) ** 2, axis=1) + 1e-12)
    db = 20 * np.log10(rms + 1e-12)
    threshold = max(floor_db, float(np.percentile(db, 10)) + 8.0)
    loud = np.flatnonzero(db > threshold)
    if loud.size == 0:
        return audio, False
    start = max(0, int(loud[0] * frame - pre_seconds * SR))
    end = min(audio.size, int((loud[-1] + 1) * frame + post_seconds * SR))
    clip = audio[start:end]
    cap = int(max_seconds * SR)
    if clip.size > cap:
        # Keep the onset: that is what the detector keys on, and the trailing
        # post-roll is the cheapest thing to lose.
        clip = clip[:cap]
    return clip, True


def write_wav(path, audio):
    pcm = (np.clip(audio, -1, 1) * 32767).astype(np.int16)
    with wave.open(str(path), "wb") as w:
        w.setnchannels(CHANNELS)
        w.setsampwidth(SAMPLE_WIDTH)
        w.setframerate(SR)
        w.writeframes(pcm.tobytes())


def play(path):
    for player in (["paplay"], ["aplay", "-q"], ["ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet"]):
        if shutil.which(player[0]):
            subprocess.run(player + [str(path)], check=False)
            return True
    print("  (no player found: install pulseaudio-utils or alsa-utils to play takes back)")
    return False


def next_index(directory, prefix):
    """Continue numbering; never reuse or overwrite an existing take."""
    highest = 0
    for path in directory.glob(f"{prefix}*.wav"):
        match = re.search(r"(\d+)$", path.stem)
        if match:
            highest = max(highest, int(match.group(1)))
    return highest + 1


def take(work, kind, index, total, prompt, backend, device, seconds, assume_yes):
    directory = work / "real" / kind
    directory.mkdir(parents=True, exist_ok=True)
    prefix = "real_p_" if kind == "positive" else "real_n_"
    while True:
        number = next_index(directory, prefix)
        path = directory / f"{prefix}{number:05d}.wav"
        print(f"\n[{kind} {index}/{total}] {prompt}")
        print("  recording...", end="", flush=True)
        started = time.monotonic()
        audio = record(backend, device, seconds)
        print(f" {time.monotonic() - started:.1f}s captured")
        clip, found = trim(audio)
        write_wav(path, clip)
        peak = float(np.max(np.abs(clip))) if clip.size else 0.0
        print(f"  {clip.size / SR:.2f}s kept -> {path.name}")
        if not found:
            print("  warning: no speech above the room noise; check the source, then redo")
        elif peak < 0.02:
            print("  warning: very quiet; move closer or raise the mic gain")
        if assume_yes:
            return "keep"
        while True:
            answer = input("  [p]lay / [k]eep / [r]edo / [q]uit: ").strip().lower()
            if answer in ("p", ""):
                play(path)
                continue
            if answer == "k":
                return "keep"
            if answer == "r":
                path.unlink(missing_ok=True)
                break
            if answer == "q":
                path.unlink(missing_ok=True)
                return "quit"
            print("  answer p, k, r or q")


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--work", default=str(DEFAULT_WORK), help="work directory (default $WORK or ~/.cache/clanka/wakeword-clanker)")
    p.add_argument("--positive", type=int, default=50, help="how many new positive takes (default 50)")
    p.add_argument("--negative", type=int, default=30, help="how many new negative takes (default 30)")
    p.add_argument("--seconds", type=float, default=2.0, help="fixed take length in seconds (default 2.0)")
    p.add_argument("--backend", choices=("pulse", "alsa"), default="pulse", help="parecord (default) or arecord")
    p.add_argument("--device", default="", help="Pulse source name or ALSA device; empty uses the default")
    p.add_argument("--list-sources", action="store_true", help="print Pulse sources and exit")
    p.add_argument("--yes", action="store_true", help="accept every take without playing it back (no prompts)")
    args = p.parse_args()

    if args.list_sources:
        list_sources()
        return 0

    work = Path(args.work).expanduser()
    training_dir = Path(__file__).resolve().parent
    if not shutil.which("parecord") and not shutil.which("arecord"):
        print("error: neither parecord nor arecord is installed", file=sys.stderr)
        return 1
    if not args.yes and not sys.stdin.isatty():
        print("error: playback/keep/redo prompts need a terminal; pass --yes to keep every take", file=sys.stderr)
        return 1

    (work / "real" / "positive").mkdir(parents=True, exist_ok=True)
    (work / "real" / "negative").mkdir(parents=True, exist_ok=True)
    device = args.device
    if args.backend == "pulse" and not device and shutil.which("pactl"):
        default = subprocess.run(["pactl", "get-default-source"], capture_output=True, text=True)
        device = default.stdout.strip()
    print(f"recording to {work / 'real'} with {args.backend}{' (' + device + ')' if device else ' (system default)'}")
    print(f"device must be 16 kHz mono capable; takes are trimmed and capped at 1.9 s")

    stats = {"keep": 0, "quit": 0}
    positives = [POSITIVE_PROMPTS[i % len(POSITIVE_PROMPTS)] for i in range(args.positive)]
    negatives = negative_prompts(training_dir)
    negatives = [negatives[i % len(negatives)] for i in range(args.negative)]

    for index, prompt in enumerate(positives, 1):
        result = take(work, "positive", index, len(positives), prompt, args.backend, device, args.seconds, args.yes)
        stats[result] += 1
        if result == "quit":
            break
    else:
        for index, prompt in enumerate(negatives, 1):
            result = take(work, "negative", index, len(negatives), prompt, args.backend, device, args.seconds, args.yes)
            stats[result] += 1
            if result == "quit":
                break

    positive = len(list((work / "real" / "positive").glob("*.wav")))
    negative = len(list((work / "real" / "negative").glob("*.wav")))
    print(f"\nkept {stats['keep']} take(s) this run; {positive} positive and {negative} negative take(s) in {work / 'real'}")
    print("next: wakeword/training/run.sh real   (splits a 20% holdout and wires the rest into training)")
    print("      wakeword/training/run.sh train convert evaluate")
    return 0


if __name__ == "__main__":
    sys.exit(main())
