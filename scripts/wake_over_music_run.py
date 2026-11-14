#!/usr/bin/env python3
"""Record, replay and count the #70 wake-over-music run.

The physical run is recorded once and analysed offline:

  record  one ~5 minute session at one music volume. Music plays into
          ``aec_music_sink`` (the runtime echo-cancel reference), wake clips
          play directly to the real speaker so they are not in the reference,
          and the raw mic and the cleaned ``aec_mic_source`` are captured at
          the same time. The same session therefore yields the AEC-off (raw)
          and AEC-on (cleaned) cells.
  detect  replay one recording through the wakeword service in real time and
          write every detection with its stream timestamp. Real time keeps the
          service's 2 s refractory period in play, exactly as in production.
  count   join a session manifest with its two detection files into one cell:
          hits/trials per model, false wakes outside every clip window, the
          music-only exposure and the mic RMS.

This script changes host audio (through paplay/parec/pactl) only in
``record``. ``detect`` and ``count`` are offline and never touch the speaker.
"""

from __future__ import annotations

import argparse
import array
import json
import math
import queue
import signal
import socket
import subprocess
import sys
import threading
import time
import wave
from datetime import UTC, datetime
from pathlib import Path

SCHEMA = "wake-over-music-run/v1"
CELL_SCHEMA = "wake-over-music-cell/v1"
RATE = 16000
CHUNK_MS = 100
CHUNK_BYTES = RATE * 2 * CHUNK_MS // 1000
AEC_STATES = ("off", "on")


class InputError(ValueError):
    """A bad argument or an incomplete recording."""


def now_iso(epoch: float | None = None) -> str:
    return datetime.fromtimestamp(epoch if epoch is not None else time.time(), UTC).isoformat()


def parse_thresholds(raw: str) -> dict[str, float]:
    thresholds: dict[str, float] = {}
    for entry in (raw or "").split(","):
        entry = entry.strip()
        if not entry:
            continue
        name, sep, value = entry.replace(":", "=", 1).partition("=")
        if not sep or not name.strip() or not value.strip():
            raise InputError(f"--thresholds: want name=value, got {entry!r}")
        thresholds[name.strip()] = float(value.strip())
    return thresholds


def volume_linear(percent: int) -> int:
    """Map a 0..100 percent to paplay's linear 0..65536 volume."""
    if not 0 <= percent <= 100:
        raise InputError(f"volume {percent} is outside 0..100")
    return round(65536 * percent / 100.0)


def rms_dbfs(samples: array.array) -> float:
    """RMS of 16-bit mono samples in dBFS; silence is -inf."""
    if not samples:
        raise InputError("cannot take the RMS of an empty sample buffer")
    total = 0
    for value in samples:
        total += value * value
    rms = (total / len(samples)) ** 0.5
    if rms <= 0:
        return float("-inf")
    return round(20.0 * math.log10(rms / 32768.0), 2)


def read_wav(path: Path) -> tuple[array.array, float]:
    """Read a 16 kHz mono PCM16 WAV as samples plus duration seconds."""
    with wave.open(str(path), "rb") as wav:
        if wav.getframerate() != RATE or wav.getnchannels() != 1 or wav.getsampwidth() != 2:
            raise InputError(
                f"{path}: want {RATE} Hz mono PCM16, got "
                f"{wav.getframerate()} Hz {wav.getnchannels()}ch {wav.getsampwidth() * 8}bit"
            )
        frames = wav.readframes(wav.getnframes())
    samples = array.array("h")
    samples.frombytes(frames)
    return samples, len(samples) / RATE


def music_segments(duration: float, windows: list[tuple[float, float]]) -> list[tuple[float, float]]:
    """Complement of the clip windows inside [0, duration], in seconds."""
    clipped: list[tuple[float, float]] = []
    for start, end in sorted(windows):
        start = max(0.0, start)
        end = min(duration, end)
        if end <= start:
            continue
        if clipped and start <= clipped[-1][1]:
            clipped[-1] = (clipped[-1][0], max(clipped[-1][1], end))
        else:
            clipped.append((start, end))
    segments: list[tuple[float, float]] = []
    cursor = 0.0
    for start, end in clipped:
        if start > cursor:
            segments.append((cursor, start))
        cursor = max(cursor, end)
    if cursor < duration:
        segments.append((cursor, duration))
    return segments


def rms_over_segments(samples: array.array, segments: list[tuple[float, float]]) -> float | None:
    if not segments:
        return None
    total = 0
    count = 0
    for start, end in segments:
        first = int(start * RATE)
        last = min(len(samples), int(end * RATE))
        for value in samples[first:last]:
            total += value * value
            count += 1
    if count == 0:
        return None
    rms = (total / count) ** 0.5
    if rms <= 0:
        return float("-inf")
    return round(20.0 * math.log10(rms / 32768.0), 2)


def find_sync_onset(samples: array.array, expected_s: float, search_s: float = 3.0) -> float | None:
    """First 10 ms frame of the sync tone, relative to the WAV start.

    The sync tone is played into a quiet room before the music, so the first
    frame well above the file's median level is its onset. This measures the
    parec startup delay, which is otherwise a silent offset between the
    manifest's wall-clock play times and the samples in the file.
    """
    frame = RATE // 100
    frames = [samples[index : index + frame] for index in range(0, len(samples) - frame, frame)]
    if not frames:
        return None
    levels = []
    for chunk in frames:
        total = sum(value * value for value in chunk)
        levels.append((total / len(chunk)) ** 0.5)
    ordered = sorted(levels)
    median = ordered[len(ordered) // 2]
    threshold = max(median * 3.0, 200.0)
    limit = min(len(frames), int((expected_s + search_s) * 100))
    for index in range(min(limit, len(frames))):
        if levels[index] >= threshold:
            return round(index * frame / RATE, 4)
    return None


def alignment_for(recording: dict, samples: array.array, sync: dict | None) -> float:
    """Seconds to add to a trial offset so it points at the captured clip."""
    if not sync or not sync.get("play_epoch"):
        return 0.0
    expected = float(sync["play_epoch"]) - float(recording["start_epoch"])
    onset = find_sync_onset(samples, expected)
    if onset is None:
        return 0.0
    return round(onset - expected, 4)


def match_trials(
    trials: list[dict],
    detections: list[dict],
    window_before: float,
    window_after: float,
) -> list[dict]:
    """Mark each trial hit when a same-model detection lands in its window.

    Windows never overlap because trials are spaced well beyond the window, so
    one detection can satisfy at most one trial per model.
    """
    hits: list[dict] = []
    for trial in trials:
        start = trial["offset_s"] - window_before
        end = trial["offset_s"] + window_after
        found = next(
            (
                detection
                for detection in detections
                if detection["model"] == trial["model"]
                and start <= detection["offset_s"] <= end
            ),
            None,
        )
        hits.append({**trial, "hit": found is not None, "detection_offset_s": found["offset_s"] if found else None})
    return hits


def clip_windows(trials: list[dict], window_before: float, window_after: float) -> list[tuple[float, float]]:
    return [(trial["offset_s"] - window_before, trial["offset_s"] + window_after) for trial in trials]


def false_wakes(
    detections: list[dict],
    windows: list[tuple[float, float]],
) -> list[dict]:
    """Detections outside every clip window; the music made them happen."""
    out: list[dict] = []
    for detection in detections:
        around = any(start <= detection["offset_s"] <= end for start, end in windows)
        if not around:
            out.append(detection)
    return out


# --------------------------------------------------------------------------
# Wyoming wake client (minimal, no dependency; the Pi host has no wyoming).


def encode_event(event_type: str, data: dict | None = None, payload: bytes = b"") -> bytes:
    """Wyoming event: a JSON header line, then the JSON data block, then payload.

    The header declares both lengths; the data object is not inlined in the
    header. The official client (and the service's reader) only look at the
    declared blocks, so an inline ``data`` key is silently ignored.
    """
    header: dict = {"type": event_type}
    data_bytes = b""
    if data is not None:
        data_bytes = json.dumps(data, separators=(",", ":")).encode()
        header["data_length"] = len(data_bytes)
    if payload:
        header["payload_length"] = len(payload)
    return json.dumps(header, separators=(",", ":")).encode() + b"\n" + data_bytes + payload


def read_event(stream) -> dict | None:
    line = stream.readline()
    if not line:
        return None
    header = json.loads(line)
    data_length = int(header.get("data_length") or 0)
    if data_length:
        header["data"] = json.loads(stream.read(data_length))
    payload_length = int(header.get("payload_length") or 0)
    if payload_length:
        header["payload"] = stream.read(payload_length)
    return header


def start_recording(path: Path, device: str) -> tuple[subprocess.Popen, float]:
    """Start a parec WAV capture and return its process and start epoch."""
    path.parent.mkdir(parents=True, exist_ok=True)
    started = time.time()
    proc = subprocess.Popen(
        [
            "parec",
            f"--device={device}",
            "--format=s16le",
            f"--rate={RATE}",
            "--channels=1",
            "--latency-msec=50",
            "--file-format=wav",
            str(path),
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    return proc, started


def stop_recording(proc: subprocess.Popen, timeout: float = 10.0) -> None:
    """Ask parec for a clean exit so its WAV header is finalised."""
    if proc.poll() is not None:
        return
    proc.send_signal(signal.SIGINT)
    try:
        proc.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=5)


def pactl_sink_input(pid: int) -> dict | None:
    """The Pulse sink-input an application pid owns, for logging its volume."""
    try:
        raw = subprocess.run(
            ["pactl", "-f", "json", "list", "sink-inputs"],
            check=False,
            capture_output=True,
            text=True,
            timeout=5,
        )
        if raw.returncode != 0:
            return None
        for entry in json.loads(raw.stdout):
            props = entry.get("properties") or {}
            if str(props.get("application.process.id", "")) == str(pid):
                return entry
    except (OSError, ValueError, subprocess.SubprocessError):
        return None
    return None


def describe_volume(entry: dict | None) -> str:
    if not entry:
        return ""
    volumes = entry.get("volume") or {}
    values = []
    for channel in volumes.values():
        if isinstance(channel, dict) and "value_percent" in channel:
            values.append(channel["value_percent"])
    return ",".join(values)


def cmd_record(args: argparse.Namespace) -> int:
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    raw_path = out / "raw.wav"
    aec_path = out / "aec.wav"
    for path in (raw_path, raw_path.with_suffix(".json"), aec_path, aec_path.with_suffix(".json")):
        if path.exists():
            raise InputError(f"{path} already exists; use a new --out directory")
    if not Path(args.music).is_file():
        raise InputError(f"{args.music} does not exist")
    clips = [Path(path) for path in args.clips]
    missing = [str(path) for path in clips if not path.is_file()]
    if missing:
        raise InputError(f"missing clips: {missing}")

    raw_proc, raw_started = start_recording(raw_path, args.raw_source)
    aec_proc, aec_started = start_recording(aec_path, args.aec_source)
    time.sleep(0.5)
    for proc, label in ((raw_proc, "raw"), (aec_proc, "aec")):
        if proc.poll() is not None:
            raise InputError(f"parec for {label} exited immediately: {proc.stderr.read().decode()[:300]}")

    sync_play = None
    if args.sync_tone:
        sync_started = time.time()
        sync_proc = subprocess.run(
            ["paplay", f"--device={args.clip_device}", "--volume=65536", args.sync_tone],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.PIPE,
        )
        if sync_proc.returncode != 0:
            raise InputError(f"sync tone failed: {sync_proc.stderr.decode()[:300]}")
        sync_play = sync_started
        time.sleep(1.0)

    music_proc = subprocess.Popen(
        [
            "paplay",
            f"--device={args.music_device}",
            f"--volume={volume_linear(args.volume)}",
            str(Path(args.music)),
        ],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    manifest: dict = {
        "schema": SCHEMA,
        "session": args.session,
        "volume": args.volume,
        "started_at": now_iso(min(raw_started, aec_started)),
        "music": {
            "file": str(Path(args.music)),
            "device": args.music_device,
            "requested_percent": args.volume,
            "linear": volume_linear(args.volume),
        },
        "recordings": {
            "raw": {"path": str(raw_path), "source": args.raw_source, "start_epoch": raw_started},
            "aec": {"path": str(aec_path), "source": args.aec_source, "start_epoch": aec_started},
        },
        "sync": {"path": args.sync_tone or "", "play_epoch": sync_play},
        "spacing_seconds": args.spacing,
        "warmup_seconds": args.warmup,
        "tail_seconds": args.tail,
        "trials": [],
    }

    try:
        time.sleep(args.warmup)
        if music_proc.poll() is not None:
            raise InputError(f"paplay exited during warm-up: {music_proc.stderr.read().decode()[:300]}")
        entry = pactl_sink_input(music_proc.pid)
        manifest["music"]["actual_volume"] = describe_volume(entry)
        manifest["music"]["sink_input"] = entry.get("index") if entry else None

        plan = [("clanker", args.clanker_trials), ("volume_up", args.volume_up_trials)]
        index = 0
        for model, count in plan:
            if count <= 0:
                continue
            model_clips = [path for path in clips if path.name.startswith(model)]
            if not model_clips:
                raise InputError(f"no clip named {model}*.wav in --clips")
            for _ in range(count):
                clip = model_clips[index % len(model_clips)]
                index += 1
                play = time.time()
                proc = subprocess.run(
                    [
                        "paplay",
                        f"--device={args.clip_device}",
                        "--volume=65536",
                        str(clip),
                    ],
                    check=False,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                )
                if proc.returncode != 0:
                    raise InputError(f"paplay {clip} failed: {proc.stderr.decode()[:300]}")
                manifest["trials"].append(
                    {
                        "index": len(manifest["trials"]),
                        "model": model,
                        "clip": str(clip),
                        "play_epoch": play,
                        "play_offset_s": round(play - raw_started, 4),
                    }
                )
                elapsed = time.time() - play
                time.sleep(max(0.0, args.spacing - elapsed))
        time.sleep(args.tail)
    finally:
        if music_proc.poll() is None:
            music_proc.terminate()
            try:
                music_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                music_proc.kill()
        stop_recording(raw_proc)
        stop_recording(aec_proc)
        manifest["ended_at"] = now_iso()

    for key, path in (("raw", raw_path), ("aec", aec_path)):
        try:
            samples, duration = read_wav(path)
        except (InputError, wave.Error, EOFError) as exc:
            raise InputError(f"{key} recording is not a usable WAV: {exc}") from exc
        if duration < args.warmup + sum(1 for _ in manifest["trials"]) * args.spacing:
            raise InputError(f"{key} recording is only {duration:.1f}s; the session was longer")
        manifest["recordings"][key]["duration_s"] = round(duration, 3)
        manifest["recordings"][key]["rms_dbfs"] = rms_dbfs(samples)

    path = out / "session.json"
    path.write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    print(
        f"recorded session {args.session} volume {args.volume}: "
        f"{len(manifest['trials'])} trials, raw {manifest['recordings']['raw']['duration_s']}s "
        f"({manifest['recordings']['raw']['rms_dbfs']} dBFS), "
        f"aec {manifest['recordings']['aec']['duration_s']}s "
        f"({manifest['recordings']['aec']['rms_dbfs']} dBFS) -> {path}"
    )
    return 0


def cmd_detect(args: argparse.Namespace) -> int:
    wav_path = Path(args.wav)
    samples, duration = read_wav(wav_path)
    models = [name.strip() for name in args.models.split(",") if name.strip()]
    thresholds = parse_thresholds(args.thresholds)
    events: queue.Queue = queue.Queue()
    sock = socket.create_connection((args.host, args.port), timeout=10)
    stream = sock.makefile("rb")

    def reader() -> None:
        try:
            while True:
                event = read_event(stream)
                if event is None:
                    break
                if event.get("type") == "detection":
                    data = event.get("data") or {}
                    events.put(
                        {
                            "model": data.get("name"),
                            "stream_ms": data.get("timestamp"),
                            "received_epoch": time.time(),
                        }
                    )
                elif event.get("type") == "not-detected":
                    events.put({"type": "not-detected"})
        except (OSError, ValueError):
            pass

    thread = threading.Thread(target=reader, daemon=True)
    thread.start()
    send = sock.sendall
    send(encode_event("detect", {"names": models, "thresholds": thresholds}))
    send(encode_event("audio-start", {"rate": RATE, "width": 2, "channels": 1}))
    started = time.time()
    try:
        for start in range(0, len(samples), CHUNK_BYTES):
            chunk = samples[start : start + CHUNK_BYTES]
            target = started + (start / RATE)
            delay = target - time.time()
            if delay > 0:
                time.sleep(delay)
            send(
                encode_event(
                    "audio-chunk",
                    {"rate": RATE, "width": 2, "channels": 1},
                    chunk.tobytes(),
                )
            )
        send(encode_event("audio-stop"))
        deadline = time.time() + 5
        while time.time() < deadline:
            if not events.empty():
                break
            time.sleep(0.05)
        time.sleep(0.2)
    finally:
        try:
            sock.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        sock.close()

    detections = []
    while not events.empty():
        event = events.get()
        if event.get("type"):
            continue
        detections.append(
            {
                "model": event["model"],
                "stream_ms": event["stream_ms"],
                "offset_s": round(float(event["stream_ms"]) / 1000.0, 4),
                "received_epoch": event["received_epoch"],
            }
        )
    detections.sort(key=lambda item: item["offset_s"])
    out = {
        "wav": str(wav_path),
        "duration_s": round(duration, 3),
        "models": models,
        "thresholds": thresholds,
        "realtime": True,
        "detections": detections,
    }
    Path(args.out).write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    summary = ", ".join(f"{d['model']}@{d['offset_s']:.2f}s" for d in detections) or "none"
    print(f"{wav_path.name}: {len(detections)} detections ({summary}) -> {args.out}")
    return 0


def load_session(path: Path) -> dict:
    data = json.loads(path.read_text(encoding="utf-8"))
    if data.get("schema") != SCHEMA:
        raise InputError(f"{path}: schema must be {SCHEMA!r}")
    if not data.get("trials"):
        raise InputError(f"{path}: no trials")
    for key in ("raw", "aec"):
        if key not in data.get("recordings", {}):
            raise InputError(f"{path}: recordings.{key} is missing")
    return data


def build_cell(session: dict, aec: str, detections_path: Path, window_before: float, window_after: float) -> dict:
    recording = session["recordings"]["raw" if aec == "off" else "aec"]
    recording_start = float(recording["start_epoch"])
    samples, duration = read_wav(Path(recording["path"]))
    # The cleaned source has a much lower noise floor than the raw mic, so its
    # sync tone is the reliable place to measure the capture-start delay. The
    # two parec processes start within tens of milliseconds of each other.
    aec_recording = session["recordings"]["aec"]
    aec_samples, _ = read_wav(Path(aec_recording["path"]))
    alignment = alignment_for(aec_recording, aec_samples, session.get("sync"))
    detections_doc = json.loads(detections_path.read_text(encoding="utf-8"))
    detections = [
        {
            "model": item["model"],
            "offset_s": float(item["offset_s"]),
            "stream_ms": item.get("stream_ms"),
        }
        for item in detections_doc["detections"]
    ]
    trials = [
        {
            "index": trial["index"],
            "model": trial["model"],
            "clip": Path(trial["clip"]).name,
            "offset_s": round(float(trial["play_epoch"]) - recording_start + alignment, 4),
        }
        for trial in session["trials"]
    ]
    hits = match_trials(trials, detections, window_before, window_after)
    windows = clip_windows(trials, window_before, window_after)
    false = false_wakes(detections, windows)
    segments = music_segments(duration, windows)
    exposure_min = sum(end - start for start, end in segments) / 60.0
    clip_rms = rms_over_segments(samples, windows)
    clip_peak = 0
    for start, end in windows:
        first = max(0, int(start * RATE))
        last = min(len(samples), int(end * RATE))
        for value in samples[first:last]:
            clip_peak = max(clip_peak, abs(value))
    per_model = {}
    for model in sorted({trial["model"] for trial in trials}):
        model_trials = [hit for hit in hits if hit["model"] == model]
        model_false = [item for item in false if item["model"] == model]
        per_model[model] = {
            "trials": len(model_trials),
            "hits": sum(1 for hit in model_trials if hit["hit"]),
            "hit_rate": round(sum(1 for hit in model_trials if hit["hit"]) / len(model_trials), 4)
            if model_trials
            else None,
            "false_wakes": len(model_false),
            "false_wakes_per_hour": round(len(model_false) / (exposure_min / 60.0), 2)
            if exposure_min > 0
            else None,
            "misses": [
                {"index": hit["index"], "clip": hit["clip"], "offset_s": hit["offset_s"]}
                for hit in model_trials
                if not hit["hit"]
            ],
            "false_events": [{"model": item["model"], "offset_s": item["offset_s"]} for item in model_false],
        }
    return {
        "schema": CELL_SCHEMA,
        "session": session["session"],
        "volume": session["volume"],
        "aec": aec,
        "recording": recording["path"],
        "duration_s": round(duration, 3),
        "alignment_s": alignment,
        "exposure_min": round(exposure_min, 3),
        "music_rms_dbfs": rms_over_segments(samples, segments),
        "clip_rms_dbfs": clip_rms,
        "clip_peak": clip_peak,
        "overall_rms_dbfs": rms_dbfs(samples),
        "clip_windows": [[round(start, 3), round(end, 3)] for start, end in windows],
        "detections": len(detections),
        "per_model": per_model,
        "notes": [
            f"hit window: {window_before}s before to {window_after}s after each clip onset",
            "false wakes exclude every clip window for both models",
        ],
    }


def cmd_count(args: argparse.Namespace) -> int:
    session = load_session(Path(args.session))
    out_dir = Path(args.out)
    out_dir.mkdir(parents=True, exist_ok=True)
    cells = []
    for aec, key in (("off", "raw"), ("on", "aec")):
        detections_path = Path(args.detections) / f"{key}.detections.json"
        if not detections_path.is_file():
            raise InputError(f"{detections_path} does not exist; run detect first")
        cell = build_cell(session, aec, detections_path, args.window_before, args.window_after)
        cells.append(cell)
        out = Path(args.out) / f"volume-{session['volume']}-aec-{aec}.json"
        out.write_text(json.dumps(cell, indent=2) + "\n", encoding="utf-8")
        lines = [
            (
                f"volume {session['volume']} AEC {aec}: "
                f"{cell['exposure_min']} min exposure, "
                f"music RMS {cell['music_rms_dbfs']} dBFS"
            )
        ]
        for model, stats in cell["per_model"].items():
            lines.append(
                f"  {model}: {stats['hits']}/{stats['trials']} hits "
                f"({stats['hit_rate'] * 100:.1f}%), {stats['false_wakes']} false "
                f"({stats['false_wakes_per_hour']}/h)"
            )
        print("\n".join(lines))
    Path(args.out, "cells.json").write_text(json.dumps(cells, indent=2) + "\n", encoding="utf-8")
    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    record = sub.add_parser("record", help="record one music-volume session, both mic paths")
    record.add_argument("--out", required=True)
    record.add_argument("--session", type=int, required=True)
    record.add_argument("--volume", type=int, required=True)
    record.add_argument("--music", required=True)
    record.add_argument("--music-device", default="aec_music_sink")
    record.add_argument("--clip-device", required=True)
    record.add_argument("--raw-source", required=True)
    record.add_argument("--aec-source", default="aec_mic_source")
    record.add_argument("--clips", nargs="+", required=True)
    record.add_argument("--sync-tone", default="", help="quiet-room tone played before the music")
    record.add_argument("--clanker-trials", type=int, default=24)
    record.add_argument("--volume-up-trials", type=int, default=12)
    record.add_argument("--spacing", type=float, default=5.0)
    record.add_argument("--warmup", type=float, default=20.0)
    record.add_argument("--tail", type=float, default=120.0)
    record.set_defaults(func=cmd_record)

    detect = sub.add_parser("detect", help="replay one WAV through the wakeword service")
    detect.add_argument("--wav", required=True)
    detect.add_argument("--out", required=True)
    detect.add_argument("--host", default="172.18.0.6")
    detect.add_argument("--port", type=int, default=10400)
    detect.add_argument("--models", default="clanker,volume_up")
    detect.add_argument("--thresholds", default="clanker=0.5,volume_up=0.98")
    detect.set_defaults(func=cmd_detect)

    count = sub.add_parser("count", help="join a session manifest with its detections")
    count.add_argument("--session", required=True)
    count.add_argument("--detections", required=True)
    count.add_argument("--out", required=True)
    count.add_argument("--window-before", type=float, default=0.5)
    count.add_argument("--window-after", type=float, default=2.0)
    count.set_defaults(func=cmd_count)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args))
    except InputError as exc:
        print(f"wake_over_music_run: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
