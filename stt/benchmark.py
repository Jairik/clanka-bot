"""Replay the STT fixture corpus through endpointing and an existing Wyoming STT.

This is the host-native measurement client for #524. It never starts or
replaces Compose services. Point --stt-uri at a Wyoming server that is already
running, or omit it to score endpointing and vocabulary correction offline.

#521 selected the raw OSM09 USB source. Reports label that capture path. The
issue stays the native blocker for live deploy until wake evidence is done.
Checked-in audio is synthetic tones; attach TTS or private WAVs for WER.

Examples:

  python3 stt/benchmark.py
  python3 stt/benchmark.py --stt-uri tcp://127.0.0.1:10300 --model small-streaming
  python3 stt/benchmark.py --audio-dir stt/fixtures/private --stt-uri tcp://127.0.0.1:10300
  python3 stt/benchmark.py --correct --include-transcripts
"""

from __future__ import annotations

import argparse
import array
import asyncio
import json
import sys
import time
from pathlib import Path

# Tests and `python3 stt/benchmark.py` both need the stt/ directory on sys.path.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from correct import VocabularyCorrector
from endpoint import FRAME_BYTES, RATE, EndpointConfig, replay, silence_frame
from fixtures import PRIVATE_DIR, load_manifest, pcm_for, write_wav
from metrics import ClipScore, score_clip, summarize

SCHEMA = "stt-replay-benchmark/v1"
# Formal #521 blocker remains for deploy/closure even though the capture path
# is already named. Wake evidence is still in progress.
BLOCKED_BY = "https://github.com/Jairik/clanka-bot/issues/521"
# Raw USB mic selected by the #521 audit. Do not point STT at aec_mic_source.
DEFAULT_CAPTURE_PATH = "alsa_input.usb-Generalplus_OSM09-00.mono-fallback"
# Trailing quiet so the replica endpointer can fire the 500 ms silence window.
TRAILING_SILENCE_MS = 640

SIMULATED_LIMITS = (
    "Checked-in clips are synthetic tones, not speech, so tone-only WER is not "
    "a recognizer measurement. TTS replay uses Pocket TTS through the existing "
    "service and is not owner voice or a live OSM09 room capture (playback "
    "would wake the shared assistant). Live STT is small-streaming; Medium is "
    "a side process in the existing container when measured, not a Compose "
    "replacement. whisper.cpp is not installed on the Pi. Wake-loop impact is "
    "CPU/RAM sampling during STT, not a live wake-and-command. Keyterm bias "
    "and decoder correction stay off in production until a measured win."
)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    """CLI for an already-running STT, or an offline correction/endpoint pass."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, default=None, help="fixture manifest JSON")
    parser.add_argument(
        "--audio-dir",
        type=Path,
        default=None,
        help="directory of 16 kHz mono WAVs named <id>.wav; private clips preferred",
    )
    parser.add_argument(
        "--stt-uri",
        default="",
        help="Wyoming URI of an already-running STT (tcp://host:port). Empty = offline.",
    )
    parser.add_argument(
        "--tts-uri",
        default="",
        help="Wyoming URI of an already-running TTS. Synthesizes missing WAVs into --audio-dir.",
    )
    parser.add_argument("--model", default="parakeet-tdt-0.6b-v2", help="Wyoming transcribe name")
    parser.add_argument(
        "--native-manifest",
        type=Path,
        default=None,
        help="models.json for an in-process Parakeet decode; does not change the live server",
    )
    parser.add_argument("--language", default="en")
    parser.add_argument(
        "--hypotheses",
        type=Path,
        default=None,
        help="optional JSON map of clip id -> fake hypothesis for offline scoring",
    )
    parser.add_argument(
        "--correct",
        action="store_true",
        help="score a client-side vocabulary pass; does not change the live service",
    )
    parser.add_argument(
        "--include-transcripts",
        action="store_true",
        help="include clip text in the report; ignored for private owner-voice dirs",
    )
    parser.add_argument(
        "--capture-path",
        default=DEFAULT_CAPTURE_PATH,
        help="capture source the numbers are attributed to",
    )
    parser.add_argument("--output", type=Path, default=None, help="write the report JSON here")
    return parser.parse_args(argv)


def config_from_manifest(manifest_path: Path) -> EndpointConfig:
    """Use the manifest's endpoint block so every model is cut the same way."""
    data = json.loads(manifest_path.read_text())
    block = data.get("endpoint") or {}
    return EndpointConfig(
        rate=int(block.get("rate") or 16000),
        silence_ms=int(block.get("silence_ms") or 500),
        threshold_dbfs=float(block.get("threshold_dbfs") or -38),
        min_speech_ms=int(block.get("min_speech_ms") or 200),
        max_utterance_ms=int(block.get("max_utterance_ms") or 15000),
    )


def is_private_audio_dir(audio_dir: Path | None) -> bool:
    """True when reports must not print owner-voice transcripts."""
    if audio_dir is None:
        return PRIVATE_DIR.is_dir() and any(PRIVATE_DIR.glob("*.wav"))
    parts = {part.lower() for part in audio_dir.resolve().parts}
    return "private" in parts


def run_offline(
    clips: list[dict],
    cfg: EndpointConfig,
    audio_dir: Path | None,
    hypotheses: dict[str, str],
    corrector: VocabularyCorrector | None,
) -> list[ClipScore]:
    """Score endpointing plus optional canned hypotheses. No network."""
    scores = []
    for clip in clips:
        pcm = pcm_for(clip, audio_dir)
        cut = replay(
            pcm,
            cfg,
            speech_start_ms=clip.get("speech_start_ms"),
            speech_end_ms=clip.get("speech_end_ms"),
        )
        expected = clip.get("expected_text") or ""
        raw = hypotheses.get(clip["id"], expected if cut.heard else "")
        correction = corrector.apply(raw) if corrector is not None else None
        text = correction.text if correction is not None else raw
        replacements = len(correction.replacements) if correction is not None else 0
        scores.append(
            score_clip(
                clip_id=clip["id"],
                expected=expected,
                hypothesis=text,
                raw=raw,
                entities=clip.get("entities"),
                start_clipped=cut.start_clipped,
                end_clipped=cut.end_clipped,
                finalize_ms=None,
                replacements=replacements,
                unsafe_if=clip.get("unsafe_if_rewritten_to"),
            )
        )
    return scores


async def transcribe_clip(
    uri: str, model: str, language: str, pcm: bytes
) -> tuple[str, str, float]:
    """Stream one already-cut utterance to Wyoming and time audio-stop to transcript.

    Uses AsyncTcpClient.read_event, the public client API. The loop requires a
    transcript-stop (or error) before it returns; a dropped connection is a
    failure, not an empty transcript.
    """
    from wyoming.asr import Transcribe, Transcript, TranscriptStop
    from wyoming.audio import AudioChunk, AudioStart, AudioStop
    from wyoming.client import AsyncTcpClient

    client = AsyncTcpClient.from_uri(uri, connect_timeout=5, read_timeout=20)
    await client.connect()
    try:
        await client.write_event(Transcribe(name=model, language=language).event())
        await client.write_event(AudioStart(rate=16000, width=2, channels=1).event())
        # 32 ms frames match production.
        for offset in range(0, len(pcm), FRAME_BYTES):
            chunk = pcm[offset : offset + FRAME_BYTES]
            if chunk:
                await client.write_event(
                    AudioChunk(rate=16000, width=2, channels=1, audio=chunk).event()
                )
        started = time.monotonic()
        await client.write_event(AudioStop().event())
        text = ""
        raw = ""
        got_final = False
        while True:
            event = await client.read_event()
            if event is None:
                break
            if event.type == "error":
                raise RuntimeError(event.data.get("text") or "stt error")
            if Transcript.is_type(event.type):
                text = event.data.get("text") or ""
                raw = event.data.get("raw_text") or text
            if TranscriptStop.is_type(event.type):
                got_final = True
                break
        if not got_final:
            raise RuntimeError("STT closed without a transcript-stop")
        return raw, text, (time.monotonic() - started) * 1000.0
    finally:
        await client.disconnect()


def _finalize_pair(
    raw: str, server_text: str, corrector: VocabularyCorrector | None
) -> tuple[str, str, int]:
    """Pick raw vs routing text. A client-side --correct pass never mutates the server."""
    if corrector is None:
        return raw, server_text, 0
    # Live STT without STT_CORRECT has no raw_text; apply the candidate locally.
    if raw == server_text:
        correction = corrector.apply(raw)
        return raw, correction.text, len(correction.replacements)
    # Server already rewrote; keep its pair and still count local replacements
    # only when the strings differ.
    return raw, server_text, 0 if raw == server_text else 1


def run_native(
    clips: list[dict],
    cfg: EndpointConfig,
    audio_dir: Path | None,
    model: str,
    manifest: Path,
    corrector: VocabularyCorrector | None,
) -> list[ClipScore]:
    """Decode inside this process using the baked Parakeet ONNX manifest."""
    from parakeet_engine import ParakeetEngine
    from pcm16 import to_floats

    engine = ParakeetEngine(model=model, manifest=manifest)
    engine.load()
    scores = []
    for clip in clips:
        pcm = pcm_for(clip, audio_dir)
        cut = replay(
            pcm,
            cfg,
            speech_start_ms=clip.get("speech_start_ms"),
            speech_end_ms=clip.get("speech_end_ms"),
        )
        expected = clip.get("expected_text") or ""
        raw = ""
        finalize_ms = None
        if cut.pcm:
            stream = engine.create_stream(model)
            started = time.monotonic()
            stream.add_audio(to_floats(cut.pcm), RATE)
            raw = stream.finish()
            finalize_ms = (time.monotonic() - started) * 1000.0
            stream.close()
        raw, text, replacements = _finalize_pair(raw, raw, corrector)
        scores.append(
            score_clip(
                clip_id=clip["id"],
                expected=expected,
                hypothesis=text,
                raw=raw,
                entities=clip.get("entities"),
                start_clipped=cut.start_clipped,
                end_clipped=cut.end_clipped,
                finalize_ms=finalize_ms,
                replacements=replacements,
                unsafe_if=clip.get("unsafe_if_rewritten_to"),
            )
        )
    return scores


async def run_wyoming(
    clips: list[dict],
    cfg: EndpointConfig,
    audio_dir: Path | None,
    uri: str,
    model: str,
    language: str,
    corrector: VocabularyCorrector | None,
) -> list[ClipScore]:
    """Replay each clip against an already-running STT. Does not start one."""
    scores = []
    for clip in clips:
        pcm = pcm_for(clip, audio_dir)
        cut = replay(
            pcm,
            cfg,
            speech_start_ms=clip.get("speech_start_ms"),
            speech_end_ms=clip.get("speech_end_ms"),
        )
        expected = clip.get("expected_text") or ""
        raw = ""
        server_text = ""
        finalize_ms = None
        if cut.pcm:
            raw, server_text, finalize_ms = await transcribe_clip(
                uri, model, language, cut.pcm
            )
        raw, text, replacements = _finalize_pair(raw, server_text, corrector)
        scores.append(
            score_clip(
                clip_id=clip["id"],
                expected=expected,
                hypothesis=text,
                raw=raw,
                entities=clip.get("entities"),
                start_clipped=cut.start_clipped,
                end_clipped=cut.end_clipped,
                finalize_ms=finalize_ms,
                replacements=replacements,
                unsafe_if=clip.get("unsafe_if_rewritten_to"),
            )
        )
    return scores


def _clip_metrics(score: ClipScore) -> dict:
    """Per-clip scores with no transcript text."""
    return {
        "id": score.clip_id,
        "wer": round(score.wer, 4),
        "wer_raw": round(score.raw_wer, 4),
        "wer_final": round(score.wer, 4),
        "entity_ok": score.entity_ok,
        "start_clipped": score.start_clipped,
        "end_clipped": score.end_clipped,
        "recognizer_error": score.recognizer_error,
        "unsafe_rewrite": score.unsafe_rewrite,
        "finalize_ms": score.finalize_ms,
        "replacements": score.replacements,
    }


def report_dict(
    scores: list[ClipScore],
    model: str,
    stt_uri: str,
    audio_dir: Path | None,
    capture_path: str = DEFAULT_CAPTURE_PATH,
    include_transcripts: bool = False,
) -> dict:
    """JSON report. Clip text is omitted unless include_transcripts is set."""
    summary = summarize(scores)
    private = is_private_audio_dir(audio_dir)
    show_text = include_transcripts and not private
    clips_out = []
    for score in scores:
        row = _clip_metrics(score)
        if show_text:
            row["expected"] = score.expected
            row["hypothesis"] = score.hypothesis
            row["raw"] = score.raw
        clips_out.append(row)
    return {
        "schema": SCHEMA,
        "model": model,
        "stt_uri": stt_uri or None,
        "audio_dir": str(audio_dir) if audio_dir else None,
        "capture_path": capture_path,
        "blocked_by": BLOCKED_BY,
        "transcripts": "included" if show_text else "aggregates",
        "notes": SIMULATED_LIMITS,
        "summary": {
            "clips": summary.clips,
            "wer": round(summary.wer, 4),
            "wer_raw": round(summary.wer_raw, 4),
            "wer_final": round(summary.wer_final, 4),
            "entity_accuracy": round(summary.entity_accuracy, 4),
            "exact_transcript_match": (
                None if summary.exact_transcript_match is None else round(summary.exact_transcript_match, 4)
            ),
            "p50_finalize_ms": summary.p50_finalize_ms,
            "p95_finalize_ms": summary.p95_finalize_ms,
            "start_clipped": summary.start_clipped,
            "end_clipped": summary.end_clipped,
            "recognizer_errors": summary.recognizer_errors,
            "unsafe_rewrites": summary.unsafe_rewrites,
        },
        "clips": clips_out,
    }


def resample_pcm16(pcm: bytes, src_rate: int, dst_rate: int = RATE) -> bytes:
    """Linear-interpolate mono PCM16. Host pytest does not need numpy."""
    if src_rate == dst_rate or not pcm:
        return pcm
    samples = array.array("h")
    samples.frombytes(pcm)
    if sys.byteorder != "little":
        samples.byteswap()
    out_len = int(len(samples) * dst_rate / src_rate)
    out = array.array("h")
    for index in range(out_len):
        pos = index * src_rate / dst_rate
        left = int(pos)
        frac = pos - left
        first = samples[left]
        second = samples[left + 1] if left + 1 < len(samples) else first
        out.append(int(first + (second - first) * frac))
    if sys.byteorder != "little":
        out.byteswap()
    return out.tobytes()


def trailing_silence() -> bytes:
    """Quiet tail matching the production 500 ms window, plus a little slack."""
    frames = max(1, TRAILING_SILENCE_MS // 32)
    return silence_frame() * frames


async def synthesize_wavs(uri: str, clips: list[dict], audio_dir: Path) -> None:
    """Fill --audio-dir from an already-running TTS. Skips silence/noise clips."""
    from wyoming.audio import AudioChunk, AudioStart, AudioStop
    from wyoming.client import AsyncTcpClient
    from wyoming.tts import Synthesize

    audio_dir.mkdir(parents=True, exist_ok=True)
    for clip in clips:
        expected = (clip.get("expected_text") or "").strip()
        if not expected:
            continue
        dest = audio_dir / f"{clip['id']}.wav"
        if dest.is_file():
            continue
        client = AsyncTcpClient.from_uri(uri, connect_timeout=5, read_timeout=60)
        await client.connect()
        try:
            await client.write_event(Synthesize(text=expected).event())
            rate = 24000
            pcm = bytearray()
            got_final = False
            while True:
                event = await client.read_event()
                if event is None:
                    break
                if event.type == "error":
                    raise RuntimeError(event.data.get("text") or "tts error")
                if AudioStart.is_type(event.type):
                    rate = AudioStart.from_event(event).rate or rate
                elif AudioChunk.is_type(event.type):
                    pcm.extend(AudioChunk.from_event(event).audio)
                elif AudioStop.is_type(event.type):
                    got_final = True
                    break
            if not got_final:
                raise RuntimeError("TTS closed without audio-stop")
        finally:
            await client.disconnect()
        speech = resample_pcm16(bytes(pcm), rate, RATE)
        write_wav(dest, speech + trailing_silence(), RATE)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    manifest_path = args.manifest or Path(__file__).with_name("fixtures") / "manifest.json"
    clips = load_manifest(manifest_path)
    cfg = config_from_manifest(manifest_path)
    hypotheses = {}
    if args.hypotheses:
        hypotheses = json.loads(args.hypotheses.read_text())
    # Client-side scoring only. Production STT_CORRECT / STT_KEYTERMS stay off.
    corrector = VocabularyCorrector.from_path() if args.correct else None
    if args.tts_uri:
        if args.audio_dir is None:
            raise SystemExit("--tts-uri requires --audio-dir to write 16 kHz WAVs")
        asyncio.run(synthesize_wavs(args.tts_uri, clips, args.audio_dir))
    if args.native_manifest is not None:
        scores = run_native(
            clips,
            cfg,
            args.audio_dir,
            args.model,
            args.native_manifest,
            corrector,
        )
    elif args.stt_uri:
        scores = asyncio.run(
            run_wyoming(
                clips,
                cfg,
                args.audio_dir,
                args.stt_uri,
                args.model,
                args.language,
                corrector,
            )
        )
    else:
        scores = run_offline(clips, cfg, args.audio_dir, hypotheses, corrector)
    report = report_dict(
        scores,
        args.model,
        args.stt_uri,
        args.audio_dir,
        capture_path=args.capture_path,
        include_transcripts=args.include_transcripts,
    )
    text = json.dumps(report, indent=2, sort_keys=False)
    if args.output:
        args.output.write_text(text + "\n")
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
