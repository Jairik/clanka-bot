#!/usr/bin/env python3
"""Measure idle mic noise against the endpointer threshold.

The assistant uses a fixed 500 ms end-of-speech window; it does not adapt its
threshold. This tool only measures and recommends, so a human can choose a gain
or threshold from evidence. It never changes host audio settings.

Reads raw PCM (16-bit little-endian by default) from a file or stdin and prints
per-frame RMS in dBFS. Same measurement shape as docs/prototype-run/mic-noise.
"""

from __future__ import annotations

import argparse
import json
import math
import struct
import sys


def frame_rms_dbfs(frame: bytes, width: int) -> float | None:
    """RMS of one frame in dBFS, or None for an empty frame."""
    if not frame:
        return None
    if width == 2:
        count = len(frame) // 2
        if count == 0:
            return None
        samples = struct.unpack("<%dh" % count, frame[: count * 2])
        full_scale = 32768.0
    elif width == 4:
        count = len(frame) // 4
        if count == 0:
            return None
        samples = struct.unpack("<%di" % count, frame[: count * 4])
        full_scale = 2147483648.0
    else:
        raise ValueError("only 2- or 4-byte samples are supported")
    power = sum(float(s) * float(s) for s in samples) / len(samples)
    if power <= 0:
        return -math.inf
    return 20.0 * math.log10(math.sqrt(power) / full_scale)


def analyze(pcm: bytes, rate: int, channels: int, width: int, frame_ms: int, threshold: float) -> dict:
    frame_bytes = int(rate * channels * width * frame_ms / 1000)
    frames = [pcm[i : i + frame_bytes] for i in range(0, len(pcm), frame_bytes)]
    levels = [lvl for lvl in (frame_rms_dbfs(f, width) for f in frames) if lvl is not None]
    if not levels:
        return {"frames": 0, "error": "no audio frames"}

    finite = [lvl for lvl in levels if math.isfinite(lvl)]
    if not finite:
        finite = [-100.0]
    ordered = sorted(finite)

    def percentile(p: float) -> float:
        index = min(len(ordered) - 1, max(0, int(round((len(ordered) - 1) * p))))
        return ordered[index]

    above = sum(1 for lvl in levels if lvl > threshold)
    return {
        "frames": len(levels),
        "frame_ms": frame_ms,
        "idle_rms_dbfs": round(sum(finite) / len(finite), 1),
        "frame_p50_dbfs": round(percentile(0.50), 1),
        "frame_p95_dbfs": round(percentile(0.95), 1),
        "frame_peak_dbfs": round(max(finite), 1),
        "threshold_dbfs": threshold,
        "frames_above_threshold": above,
        "percent_above_threshold": round(100.0 * above / len(levels), 1),
        "recommended_threshold_dbfs": max(-55, min(-20, math.ceil(sum(finite) / len(finite) + 6))),
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("input", nargs="?", default="-", help="raw PCM file, or - for stdin")
    parser.add_argument("--rate", type=int, default=16000)
    parser.add_argument("--channels", type=int, default=1)
    parser.add_argument("--width", type=int, default=2, choices=(2, 4))
    parser.add_argument("--frame-ms", type=int, default=32)
    parser.add_argument("--threshold", type=float, default=-38.0, help="endpointer threshold in dBFS")
    parser.add_argument("--json", action="store_true", help="print one JSON object")
    args = parser.parse_args(argv)

    if args.input == "-":
        pcm = sys.stdin.buffer.read()
    else:
        with open(args.input, "rb") as handle:
            pcm = handle.read()

    result = analyze(pcm, args.rate, args.channels, args.width, args.frame_ms, args.threshold)
    if args.json:
        print(json.dumps(result, sort_keys=True))
        return 0 if "error" not in result else 1
    if "error" in result:
        print(f"error: {result['error']}", file=sys.stderr)
        return 1

    print(f"frames: {result['frames']} x {result['frame_ms']} ms")
    print(f"idle RMS: {result['idle_rms_dbfs']:.1f} dBFS")
    print(
        "frame levels: p50 %.1f, p95 %.1f, peak %.1f dBFS"
        % (result["frame_p50_dbfs"], result["frame_p95_dbfs"], result["frame_peak_dbfs"])
    )
    print(
        "above %.0f dBFS: %d/%d frames (%.1f%%)"
        % (
            result["threshold_dbfs"],
            result["frames_above_threshold"],
            result["frames"],
            result["percent_above_threshold"],
        )
    )
    if result["percent_above_threshold"] > 50:
        print(
            "warning: more than half of idle frames are above the threshold; "
            "a wake word would open an utterance on noise"
        )
    print(
        "suggested threshold: %.0f dBFS (idle RMS + 6 dB, capped); "
        "or lower the source gain until idle RMS is around -45 dBFS"
        % result["recommended_threshold_dbfs"]
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
