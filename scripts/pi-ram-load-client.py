#!/usr/bin/env python3
"""Drive one step of the #69/#74 scripted load against the real Wyoming services.

Runs in a throwaway container on the Compose network (the stack publishes no
wakeword/STT/TTS ports), so it needs the wyoming package the model images
already carry. Nothing here opens an audio device: TTS PCM is counted and
thrown away, never written to a sink.

Steps:
  wake <wav> [model]        one wake-word detection over Wyoming
  stt <pcm-file>            transcribe 16 kHz mono PCM16 over Wyoming
  tts <text>                synthesize and discard the reply audio
  fixture <text> <path>     synthesize <text> and write 16 kHz mono PCM16,
                            used to make the STT fixture out of a known voice
                            (file only, still no playback)

Each step prints one JSON line with what the service actually returned.
"""

from __future__ import annotations

import argparse
import array
import asyncio
import json
import sys
import wave
from pathlib import Path

from wyoming.asr import Transcribe, Transcript, TranscriptStop
from wyoming.audio import AudioChunk, AudioStart, AudioStop
from wyoming.client import AsyncTcpClient
from wyoming.tts import Synthesize
from wyoming.wake import Detection, Detect, NotDetected

RATE = 16000
CHUNK = 3200  # 100 ms of 16 kHz mono PCM16
SILENCE = b"\x00\x00" * RATE


def streamable(audio: bytes, seconds: float = 2.5) -> bytes:
    """Give a short clip the continuous-stream context a real capture has.

    The detector scores 16 embedding frames at a time, so a bare 1 s fixture
    can never fill a window before AudioStop. This mirrors
    wakeword/tests/wyoming_client.py, which is the scripted test the repo
    already trusts for this service.
    """
    want = int(seconds * RATE) * 2
    if len(audio) >= want:
        return audio
    lead = SILENCE[: int(0.6 * RATE) * 2]
    return lead + audio + b"\x00\x00" * ((want - len(lead) - len(audio)) // 2)


async def wake(addr: str, path: Path, model: str) -> int:
    with wave.open(str(path)) as w:
        if w.getframerate() != RATE or w.getnchannels() != 1 or w.getsampwidth() != 2:
            raise SystemExit(f"{path}: want 16 kHz mono PCM16")
        audio = streamable(w.readframes(w.getnframes()))
    detected = False
    async with AsyncTcpClient.from_uri(addr) as client:
        await client.write_event(Detect(names=[model]).event())
        await client.write_event(AudioStart(rate=RATE, width=2, channels=1).event())
        for i in range(0, len(audio), CHUNK):
            await client.write_event(
                AudioChunk(rate=RATE, width=2, channels=1, audio=audio[i : i + CHUNK]).event()
            )
        await client.write_event(AudioStop().event())
        while True:
            event = await client.read_event()
            if event is None:
                raise RuntimeError("wakeword service closed the connection")
            if Detection.is_type(event.type):
                detected = True
            elif NotDetected.is_type(event.type):
                detected = False
            else:
                continue
            break
    print(json.dumps({"step": "wake", "fixture": path.name, "detected": detected}))
    return 0 if detected else 1


async def stt(addr: str, path: Path, language: str) -> int:
    audio = path.read_bytes()
    text = ""
    async with AsyncTcpClient.from_uri(addr) as client:
        await client.write_event(Transcribe(language=language).event())
        await client.write_event(AudioStart(rate=RATE, width=2, channels=1).event())
        for i in range(0, len(audio), CHUNK):
            await client.write_event(
                AudioChunk(rate=RATE, width=2, channels=1, audio=audio[i : i + CHUNK]).event()
            )
        await client.write_event(AudioStop().event())
        while True:
            event = await client.read_event()
            if event is None:
                raise RuntimeError("stt service closed the connection")
            if Transcript.is_type(event.type):
                text = Transcript.from_event(event).text or ""
            elif TranscriptStop.is_type(event.type):
                break
            elif event.type == "error":
                raise RuntimeError(event.data.get("text", "stt error"))
    print(json.dumps({"step": "stt", "bytes": len(audio), "transcript": text}))
    return 0 if text.strip() else 1


async def _synth(addr: str, text: str) -> tuple[int, bytes]:
    rate = 0
    pcm = bytearray()
    async with AsyncTcpClient.from_uri(addr) as client:
        await client.write_event(Synthesize(text=text).event())
        while True:
            event = await client.read_event()
            if event is None:
                raise RuntimeError("tts service closed the connection")
            if AudioStart.is_type(event.type):
                rate = AudioStart.from_event(event).rate
            elif AudioChunk.is_type(event.type):
                pcm += AudioChunk.from_event(event).audio
            elif AudioStop.is_type(event.type):
                break
            elif event.type == "error":
                raise RuntimeError(event.data.get("text", "tts error"))
    return rate, bytes(pcm)


def _resample(pcm: bytes, rate: int, out_rate: int = RATE) -> bytes:
    """Linear-interpolate mono PCM16 to out_rate. Good enough for a fixture."""
    if rate == out_rate:
        return pcm
    samples = array.array("h")
    samples.frombytes(pcm)
    if sys.byteorder != "little":
        samples.byteswap()
    out_len = int(len(samples) * out_rate / rate)
    out = array.array("h")
    for i in range(out_len):
        pos = i * rate / out_rate
        j = int(pos)
        frac = pos - j
        a = samples[j]
        b = samples[j + 1] if j + 1 < len(samples) else a
        out.append(int(a + (b - a) * frac))
    if sys.byteorder != "little":
        out.byteswap()
    return out.tobytes()


async def tts(addr: str, text: str, out: Path | None) -> int:
    rate, pcm = await _synth(addr, text)
    written = 0
    if out is not None:
        out.write_bytes(pcm)
        written = len(pcm)
    seconds = len(pcm) / (rate * 2) if rate else 0.0
    print(
        json.dumps(
            {
                "step": "tts",
                "rate": rate,
                "pcm_bytes": len(pcm),
                "seconds": round(seconds, 2),
                "played": False,
                "written": written,
            }
        )
    )
    return 0


async def fixture(addr: str, text: str, out: Path) -> int:
    rate, pcm = await _synth(addr, text)
    converted = _resample(pcm, rate)
    out.write_bytes(converted)
    print(
        json.dumps(
            {
                "step": "fixture",
                "source_rate": rate,
                "text": text,
                "out": str(out),
                "pcm_bytes": len(converted),
                "seconds": round(len(converted) / (RATE * 2), 2),
            }
        )
    )
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tts", default="tcp://tts:10200")
    parser.add_argument("--stt", default="tcp://stt:10300")
    parser.add_argument("--wake", default="tcp://wakeword:10400")
    parser.add_argument("--model", default="clanker")
    parser.add_argument("--language", default="en")
    sub = parser.add_subparsers(dest="step", required=True)

    p = sub.add_parser("wake")
    p.add_argument("wav", type=Path)

    p = sub.add_parser("stt")
    p.add_argument("pcm", type=Path)

    p = sub.add_parser("tts")
    p.add_argument("text")

    p = sub.add_parser("fixture")
    p.add_argument("text")
    p.add_argument("out", type=Path)

    args = parser.parse_args()
    if args.step == "wake":
        return asyncio.run(wake(args.wake, args.wav, args.model))
    if args.step == "stt":
        return asyncio.run(stt(args.stt, args.pcm, args.language))
    if args.step == "tts":
        return asyncio.run(tts(args.tts, args.text, None))
    return asyncio.run(fixture(args.tts, args.text, args.out))


if __name__ == "__main__":
    sys.exit(main())
