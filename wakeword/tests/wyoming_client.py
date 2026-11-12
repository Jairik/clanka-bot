#!/usr/bin/env python3
"""Drive the real wakeword service over Wyoming and assert detect/no-detect.

The fixtures are synthesized Pocket TTS clips from voices held out of training,
so this checks the model and the wiring, not the owner's room.

Used by run_detection_test.sh inside the built image; needs the wyoming package
from the image's venv.
"""

import argparse
import asyncio
import sys
import wave
from pathlib import Path

from wyoming.audio import AudioChunk, AudioStart, AudioStop
from wyoming.client import AsyncTcpClient
from wyoming.info import Describe, Info
from wyoming.wake import Detection, Detect, NotDetected

RATE = 16000
CHUNK = 3200  # 100 ms of 16-bit mono audio


async def describe(addr, expect):
    async with AsyncTcpClient.from_uri(addr) as client:
        await client.write_event(Describe().event())
        while True:
            event = await client.read_event()
            if event is None:
                raise RuntimeError("service closed the connection during describe")
            if Info.is_type(event.type):
                info = Info.from_event(event)
                names = {model.name for program in info.wake for model in program.models}
                print("models:", ", ".join(sorted(names)))
                missing = [name for name in expect if name not in names]
                if missing:
                    print("MISSING", missing)
                    return False
                return True


SILENCE = b"\x00\x00" * RATE  # one second of 16 kHz mono PCM16


def streamable(audio, seconds=2.5):
    """Give a short clip the context a real stream always has.

    The service scores 16 embedding frames (1.28 s) at a time, so a bare 1 s
    fixture can never fill a window before AudioStop. Real capture is
    continuous; prepending silence and padding the tail is the same picture.
    """
    want = int(seconds * RATE) * 2
    if len(audio) >= want:
        return audio
    lead = SILENCE[: int(0.6 * RATE) * 2]
    return lead + audio + b"\x00\x00" * ((want - len(lead) - len(audio)) // 2)


async def detected(addr, model, path):
    with wave.open(str(path)) as w:
        if w.getframerate() != RATE or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise SystemExit(f"{path}: want 16 kHz mono PCM16")
        audio = streamable(w.readframes(w.getnframes()))
    async with AsyncTcpClient.from_uri(addr) as client:
        await client.write_event(Detect(names=[model]).event())
        await client.write_event(AudioStart(rate=RATE, width=2, channels=1).event())
        for i in range(0, len(audio), CHUNK):
            await client.write_event(AudioChunk(rate=RATE, width=2, channels=1, audio=audio[i:i + CHUNK]).event())
        await client.write_event(AudioStop().event())
        while True:
            event = await client.read_event()
            if event is None:
                raise RuntimeError(f"{path}: service closed the connection")
            if Detection.is_type(event.type):
                return True
            if NotDetected.is_type(event.type):
                return False


async def run(addr, model, expect, paths):
    failures = []
    for path in paths:
        hit = await asyncio.wait_for(detected(addr, model, path), timeout=30)
        print(f"{'HIT ' if hit else 'MISS'} {path.name}", flush=True)
        if hit != expect:
            failures.append(path.name)
    return failures


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--addr", default="tcp://127.0.0.1:10400")
    p.add_argument("--model", default="clanker")
    p.add_argument("--positive", default="")
    p.add_argument("--negative", default="")
    p.add_argument("--expect-models", default="", help="comma-separated names describe must advertise")
    args = p.parse_args()

    if args.expect_models:
        expect = [name for name in args.expect_models.split(",") if name]
        if not asyncio.run(describe(args.addr, expect)):
            return 1

    failures = []
    for directory, expected in ((args.positive, True), (args.negative, False)):
        if not directory:
            continue
        paths = sorted(Path(directory).glob("*.wav"))
        if not paths:
            raise SystemExit(f"{directory}: no wav fixtures")
        failures += asyncio.run(run(args.addr, args.model, expected, paths))
    if failures:
        print("FAIL", failures)
        return 1
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
