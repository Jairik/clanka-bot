"""Replay the STT fixture corpus through endpointing and an existing Wyoming STT.

This is the host-native measurement client for #524. It never starts or
replaces Compose services. Point --stt-uri at a Wyoming server that is already
running, or omit it to score endpointing and vocabulary correction offline.

Live Pi capture-path comparison waits on #521. Until that issue names the
capture path, reports set capture_path to "unselected" and only score the
fixture audio.

Examples:

  python3 stt/benchmark.py
  python3 stt/benchmark.py --stt-uri tcp://127.0.0.1:10300 --model small-streaming
  python3 stt/benchmark.py --audio-dir stt/fixtures/private --stt-uri tcp://127.0.0.1:10300
"""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
import time
from pathlib import Path

# Tests and `python3 stt/benchmark.py` both need the stt/ directory on sys.path.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from correct import VocabularyCorrector, correction_enabled
from endpoint import EndpointConfig, replay
from fixtures import load_manifest, pcm_for
from metrics import ClipScore, score_clip, summarize

SCHEMA = "stt-replay-benchmark/v1"
# Native blocker for the deployment comparison on the selected capture path.
BLOCKED_BY = "https://github.com/Jairik/clanka-bot/issues/521"


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
    parser.add_argument("--model", default="small-streaming", help="Wyoming transcribe name")
    parser.add_argument("--language", default="en")
    parser.add_argument(
        "--hypotheses",
        type=Path,
        default=None,
        help="optional JSON map of clip id -> fake hypothesis for offline scoring",
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


async def transcribe_clip(uri: str, model: str, language: str, pcm: bytes) -> tuple[str, float]:
    """Stream one already-cut utterance to Wyoming and time audio-stop to transcript."""
    from wyoming.asr import Transcribe, Transcript, TranscriptStop
    from wyoming.audio import AudioChunk, AudioStart, AudioStop
    from wyoming.client import AsyncTcpClient
    from wyoming.event import async_read_event

    client = AsyncTcpClient.from_uri(uri, connect_timeout=5, read_timeout=20)
    await client.connect()
    try:
        await client.write_event(Transcribe(name=model, language=language).event())
        await client.write_event(AudioStart(rate=16000, width=2, channels=1).event())
        # 32 ms frames match production.
        for offset in range(0, len(pcm), 1024):
            chunk = pcm[offset : offset + 1024]
            if chunk:
                await client.write_event(
                    AudioChunk(rate=16000, width=2, channels=1, audio=chunk).event()
                )
        started = time.monotonic()
        await client.write_event(AudioStop().event())
        text = ""
        while True:
            event = await async_read_event(client.reader)
            if event is None:
                break
            if event.type == "error":
                raise RuntimeError(event.data.get("text") or "stt error")
            if Transcript.is_type(event.type):
                text = event.data.get("text") or ""
            if TranscriptStop.is_type(event.type):
                break
        return text, (time.monotonic() - started) * 1000.0
    finally:
        await client.disconnect()


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
        finalize_ms = None
        if cut.pcm:
            raw, finalize_ms = await transcribe_clip(uri, model, language, cut.pcm)
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
                finalize_ms=finalize_ms,
                replacements=replacements,
                unsafe_if=clip.get("unsafe_if_rewritten_to"),
            )
        )
    return scores


def report_dict(
    scores: list[ClipScore],
    model: str,
    stt_uri: str,
    audio_dir: Path | None,
) -> dict:
    """JSON report with WER, entities, latency and clipping split out."""
    summary = summarize(scores)
    return {
        "schema": SCHEMA,
        "model": model,
        "stt_uri": stt_uri or None,
        "audio_dir": str(audio_dir) if audio_dir else None,
        "capture_path": "unselected",
        "blocked_by": BLOCKED_BY,
        "notes": (
            "Fixture replay only. Do not treat these numbers as the Pi deployment "
            "comparison until #521 names the capture path."
        ),
        "summary": {
            "clips": summary.clips,
            "wer": round(summary.wer, 4),
            "entity_accuracy": round(summary.entity_accuracy, 4),
            "command_success": (
                None if summary.command_success is None else round(summary.command_success, 4)
            ),
            "p50_finalize_ms": summary.p50_finalize_ms,
            "p95_finalize_ms": summary.p95_finalize_ms,
            "start_clipped": summary.start_clipped,
            "end_clipped": summary.end_clipped,
            "recognizer_errors": summary.recognizer_errors,
            "unsafe_rewrites": summary.unsafe_rewrites,
        },
        "clips": [
            {
                "id": score.clip_id,
                "expected": score.expected,
                "hypothesis": score.hypothesis,
                "raw": score.raw,
                "wer": round(score.wer, 4),
                "entity_ok": score.entity_ok,
                "start_clipped": score.start_clipped,
                "end_clipped": score.end_clipped,
                "recognizer_error": score.recognizer_error,
                "unsafe_rewrite": score.unsafe_rewrite,
                "finalize_ms": score.finalize_ms,
                "replacements": score.replacements,
            }
            for score in scores
        ],
    }


def main(argv: list[str] | None = None) -> int:
    args = parse_args(argv)
    manifest_path = args.manifest or Path(__file__).with_name("fixtures") / "manifest.json"
    clips = load_manifest(manifest_path)
    cfg = config_from_manifest(manifest_path)
    hypotheses = {}
    if args.hypotheses:
        hypotheses = json.loads(args.hypotheses.read_text())
    # Offline scoring always applies the corrector so unsafe-rewrite tests run
    # without a model. Live Wyoming scoring skips a second pass when the
    # service already corrected (STT_CORRECT=1); pass --hypotheses to force it.
    corrector = VocabularyCorrector.from_path() if correction_enabled() else None
    if args.stt_uri:
        scores = asyncio.run(
            run_wyoming(
                clips,
                cfg,
                args.audio_dir,
                args.stt_uri,
                args.model,
                args.language,
                None,
            )
        )
    else:
        scores = run_offline(clips, cfg, args.audio_dir, hypotheses, corrector)
    report = report_dict(scores, args.model, args.stt_uri, args.audio_dir)
    text = json.dumps(report, indent=2, sort_keys=False)
    if args.output:
        args.output.write_text(text + "\n")
    print(text)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
