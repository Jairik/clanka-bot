import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from benchmark import report_dict, run_offline
from correct import VocabularyCorrector
from endpoint import EndpointConfig
from fixtures import load_manifest, pcm_for, synthesize


def test_manifest_covers_required_categories():
    clips = load_manifest()
    categories = {clip["category"] for clip in clips}
    assert {
        "command",
        "proper_name",
        "numbers",
        "negation",
        "silence",
        "dictation",
        "noise",
    }.issubset(categories)
    ids = [clip["id"] for clip in clips]
    assert len(ids) == len(set(ids))


def test_synthetic_silence_is_repeatable():
    first = synthesize({"kind": "silence", "duration_ms": 640})
    second = synthesize({"kind": "silence", "duration_ms": 640})
    assert first == second
    assert first == b"\x00" * len(first)


def test_pcm_for_uses_the_synthetic_recipe():
    clips = {clip["id"]: clip for clip in load_manifest()}
    silence = pcm_for(clips["silence"])
    assert set(silence) == {0}
    noise = pcm_for(clips["noise-below-threshold"])
    assert noise != b"\x00" * len(noise)


def test_offline_benchmark_does_not_rewrite_negation_or_herman():
    clips = load_manifest()
    cfg = EndpointConfig()
    hypotheses = {
        "negation-lights": "don't turn on the lights",
        "dictation-herman": "text Herman I am running late",
        "name-spotify": "play music on spot ify",
        "command-time": "what time is it",
        "silence": "",
        "noise-below-threshold": "",
        "clip-end": "what time is it",
    }
    scores = run_offline(clips, cfg, None, hypotheses, VocabularyCorrector.from_path())
    by_id = {score.clip_id: score for score in scores}
    assert by_id["negation-lights"].hypothesis == "don't turn on the lights"
    assert by_id["negation-lights"].unsafe_rewrite is False
    assert by_id["dictation-herman"].hypothesis == "text Herman I am running late"
    assert by_id["dictation-herman"].unsafe_rewrite is False
    assert by_id["name-spotify"].hypothesis == "play music on Spotify"
    assert by_id["clip-end"].end_clipped is True
    assert by_id["silence"].start_clipped is False


def test_report_marks_capture_path_unselected():
    clips = load_manifest()[:1]
    scores = run_offline(clips, EndpointConfig(), None, {}, None)
    report = report_dict(scores, "small-streaming", "", None)
    assert report["schema"] == "stt-replay-benchmark/v1"
    assert report["capture_path"] == "unselected"
    assert "521" in report["blocked_by"]
    json.dumps(report)  # must be serialisable
