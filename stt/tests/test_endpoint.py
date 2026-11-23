import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from endpoint import (
    EndpointConfig,
    Endpointer,
    dbfs_amplitude,
    preroll_frame_count,
    replay,
    silence_frame,
    speech_frame,
)


def test_endpointer_waits_for_the_configured_silence():
    cases = [
        (500, 16, True),
        (500, 15, False),
        (300, 10, True),
        (300, 9, False),
    ]
    for silence_ms, quiet, want_end in cases:
        endpointer = Endpointer(
            EndpointConfig(silence_ms=silence_ms, min_speech_ms=0, threshold_dbfs=-38)
        )
        endpointer.add(speech_frame())
        ended = False
        for _ in range(quiet):
            ended = endpointer.add(silence_frame())
        assert ended is want_end, (silence_ms, quiet, ended)


def test_endpointer_ignores_brief_blips():
    endpointer = Endpointer(
        EndpointConfig(silence_ms=500, min_speech_ms=200, threshold_dbfs=-38)
    )
    endpointer.add(speech_frame())
    for _ in range(20):
        assert not endpointer.add(silence_frame())
    assert endpointer.heard is False

    for _ in range(7):
        endpointer.add(speech_frame())
    assert endpointer.heard is True


def test_endpointer_ignores_silence_before_speech():
    endpointer = Endpointer(EndpointConfig(min_speech_ms=0))
    for _ in range(100):
        assert not endpointer.add(silence_frame())
    assert endpointer.heard is False


def test_endpointer_noise_below_threshold_is_not_speech():
    endpointer = Endpointer(EndpointConfig(min_speech_ms=0, threshold_dbfs=-38))
    quiet = dbfs_amplitude(-45)
    for _ in range(20):
        assert not endpointer.add(speech_frame(quiet))
    assert endpointer.heard is False


def test_endpointer_caps_runaway_sessions():
    endpointer = Endpointer(
        EndpointConfig(silence_ms=500, max_utterance_ms=1000, min_speech_ms=0)
    )
    frames = 0
    while not endpointer.add(speech_frame()):
        frames += 1
        if frames > 100:
            raise AssertionError("endpointer never hit its cap")
    assert 30 <= frames <= 32
    assert endpointer.hit_max is True


def test_preroll_frame_count_matches_go():
    assert preroll_frame_count(1024, 16000) == 16
    assert preroll_frame_count(2048, 16000) == 8
    assert preroll_frame_count(1024, 0) == 0


def test_replay_keeps_the_onset_with_production_preroll():
    # Speech from sample 0, then enough silence to fire. Pre-roll must still
    # send the first frames so this is not tagged as a clipped start.
    pcm = speech_frame() * 25 + silence_frame() * 20
    result = replay(pcm, speech_start_ms=0, speech_end_ms=800)
    assert result.heard is True
    assert result.ended is True
    assert result.start_clipped is False
    assert result.end_clipped is False
    assert result.pcm[:1024] == speech_frame()


def test_replay_tags_a_truncated_tail():
    pcm = speech_frame() * 25
    result = replay(pcm, speech_start_ms=0, speech_end_ms=800)
    assert result.heard is True
    assert result.ended is False
    assert result.end_clipped is True


def test_replay_silence_is_not_clipped():
    result = replay(silence_frame() * 40)
    assert result.heard is False
    assert result.start_clipped is False
    assert result.end_clipped is False
    assert result.pcm == b""
