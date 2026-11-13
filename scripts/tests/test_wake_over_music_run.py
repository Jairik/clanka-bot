"""Tests for the #70 record-first measurement client's pure pieces."""

from __future__ import annotations

import array
import json
import tempfile
import unittest
import wave
from pathlib import Path

from scripts.wake_over_music_run import (
    RATE,
    InputError,
    alignment_for,
    build_cell,
    clip_windows,
    encode_event,
    false_wakes,
    find_sync_onset,
    match_trials,
    music_segments,
    read_event,
    rms_dbfs,
    rms_over_segments,
    volume_linear,
)


class VolumeTests(unittest.TestCase):
    def test_percent_maps_to_linear(self):
        self.assertEqual(volume_linear(0), 0)
        self.assertEqual(volume_linear(100), 65536)
        self.assertEqual(volume_linear(35), 22938)

    def test_out_of_range_is_refused(self):
        with self.assertRaises(InputError):
            volume_linear(101)


class RmsTests(unittest.TestCase):
    def test_silence_is_negative_infinity(self):
        self.assertEqual(rms_dbfs(array.array("h", [0, 0, 0])), float("-inf"))

    def test_full_scale_is_zero(self):
        self.assertEqual(rms_dbfs(array.array("h", [32767, 32767])), -0.0)

    def test_half_scale_is_about_minus_six(self):
        self.assertAlmostEqual(rms_dbfs(array.array("h", [16384] * 4)), -6.02, places=2)

    def test_empty_is_refused(self):
        with self.assertRaises(InputError):
            rms_dbfs(array.array("h"))

    def test_rms_over_segments_ignores_the_clip_windows(self):
        samples = array.array("h", [0] * 10 + [32767] * 10)
        # 1000 Hz would be 0.000625 s/sample; use the file rate instead by
        # taking the segment in sample counts through a 16-sample file.
        self.assertAlmostEqual(rms_over_segments(samples, [(10 / 16000, 20 / 16000)]), -0.0)
        self.assertEqual(rms_over_segments(samples, []), None)


class WindowTests(unittest.TestCase):
    def test_music_segments_exclude_windows(self):
        segments = music_segments(10.0, [(2.0, 3.0), (5.0, 6.0)])
        self.assertEqual(segments, [(0.0, 2.0), (3.0, 5.0), (6.0, 10.0)])

    def test_overlapping_windows_are_merged(self):
        self.assertEqual(music_segments(10.0, [(2.0, 5.0), (4.0, 6.0)]), [(0.0, 2.0), (6.0, 10.0)])

    def test_window_past_the_end_is_clipped(self):
        self.assertEqual(music_segments(5.0, [(4.0, 8.0)]), [(0.0, 4.0)])

    def test_clip_windows_use_both_edges(self):
        trials = [{"offset_s": 10.0}]
        self.assertEqual(clip_windows(trials, 0.5, 2.0), [(9.5, 12.0)])


class MatchTests(unittest.TestCase):
    def setUp(self):
        self.trials = [
            {"index": 0, "model": "clanker", "offset_s": 10.0},
            {"index": 1, "model": "clanker", "offset_s": 20.0},
            {"index": 2, "model": "volume_up", "offset_s": 30.0},
        ]

    def test_same_model_inside_window_hits(self):
        hits = match_trials(
            self.trials,
            [{"model": "clanker", "offset_s": 11.4}, {"model": "volume_up", "offset_s": 30.8}],
            0.5,
            2.0,
        )
        self.assertEqual([hit["hit"] for hit in hits], [True, False, True])

    def test_wrong_model_does_not_hit(self):
        hits = match_trials(self.trials, [{"model": "volume_up", "offset_s": 10.5}], 0.5, 2.0)
        self.assertEqual([hit["hit"] for hit in hits], [False, False, False])

    def test_outside_the_window_misses(self):
        hits = match_trials(self.trials, [{"model": "clanker", "offset_s": 12.1}], 0.5, 2.0)
        self.assertFalse(hits[0]["hit"])

    def test_false_wakes_are_only_outside_every_window(self):
        windows = clip_windows(self.trials, 0.5, 2.0)
        detections = [
            {"model": "clanker", "offset_s": 10.5},
            {"model": "clanker", "offset_s": 15.0},
            {"model": "volume_up", "offset_s": 30.5},
            {"model": "volume_up", "offset_s": 40.0},
        ]
        false = false_wakes(detections, windows)
        self.assertEqual([item["offset_s"] for item in false], [15.0, 40.0])


class EventTests(unittest.TestCase):
    def test_event_header_data_and_payload(self):
        encoded = encode_event("audio-chunk", {"rate": 16000}, b"\x01\x02")
        header, body = encoded.split(b"\n", 1)
        parsed = json.loads(header)
        self.assertEqual(parsed["type"], "audio-chunk")
        self.assertEqual(parsed["data_length"], len(b'{"rate":16000}'))
        self.assertEqual(parsed["payload_length"], 2)
        self.assertEqual(body[: parsed["data_length"]], b'{"rate":16000}')
        self.assertEqual(body[parsed["data_length"] :], b"\x01\x02")

    def test_event_without_data_or_payload_has_no_lengths(self):
        header, _, rest = encode_event("audio-stop").partition(b"\n")
        self.assertEqual(json.loads(header), {"type": "audio-stop"})
        self.assertEqual(rest, b"")

    def test_read_event_consumes_both_blocks(self):
        import io

        encoded = encode_event("detection", {"name": "clanker"}, b"")
        event = read_event(io.BytesIO(encoded))
        self.assertEqual(event["type"], "detection")
        self.assertEqual(event["data"], {"name": "clanker"})


class SyncTests(unittest.TestCase):
    def test_sync_onset_finds_the_first_loud_frame(self):
        samples = array.array("h", [0] * (RATE // 2))
        samples.extend(array.array("h", [12000] * (RATE // 5)))
        samples.extend(array.array("h", [0] * RATE))
        onset = find_sync_onset(samples, expected_s=0.5)
        self.assertIsNotNone(onset)
        self.assertAlmostEqual(onset, 0.5, delta=0.02)

    def test_alignment_measures_the_capture_delay(self):
        samples = array.array("h", [0] * (RATE // 2))
        samples.extend(array.array("h", [12000] * (RATE // 5)))
        samples.extend(array.array("h", [0] * RATE))
        # The tone was played at the recording's nominal start, but parec
        # started 0.5 s late, so the manifest offset must be shifted by +0.5.
        recording = {"start_epoch": 100.0}
        self.assertAlmostEqual(alignment_for(recording, samples, {"play_epoch": 100.0}), 0.5, delta=0.02)

    def test_no_sync_means_no_alignment(self):
        self.assertEqual(alignment_for({"start_epoch": 100.0}, array.array("h", [0] * 100), None), 0.0)


class BuildCellTests(unittest.TestCase):
    def test_join_counts_hits_false_wakes_and_music_exposure(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            wav_path = root / "raw.wav"
            samples = array.array("h", [0] * (20 * 16000))
            with wave.open(str(wav_path), "wb") as wav:
                wav.setnchannels(1)
                wav.setsampwidth(2)
                wav.setframerate(16000)
                wav.writeframes(samples.tobytes())
            session = {
                "schema": "wake-over-music-run/v1",
                "session": 1,
                "volume": 35,
                "recordings": {
                    "raw": {"path": str(wav_path), "start_epoch": 1000.0},
                    "aec": {"path": str(wav_path), "start_epoch": 1000.0},
                },
                "trials": [
                    {"index": 0, "model": "clanker", "clip": "clanker_00.wav", "play_epoch": 1002.0},
                    {"index": 1, "model": "clanker", "clip": "clanker_01.wav", "play_epoch": 1005.0},
                    {"index": 2, "model": "clanker", "clip": "clanker_02.wav", "play_epoch": 1012.0},
                    {"index": 3, "model": "volume_up", "clip": "volume_up_00.wav", "play_epoch": 1018.0},
                ],
            }
            detections_path = root / "raw.detections.json"
            detections_path.write_text(
                json.dumps(
                    {
                        "detections": [
                            {"model": "clanker", "offset_s": 6.0, "stream_ms": 6000},
                            {"model": "volume_up", "offset_s": 15.0, "stream_ms": 15000},
                        ]
                    }
                ),
                encoding="utf-8",
            )
            cell = build_cell(session, "off", detections_path, 0.5, 2.0)
            self.assertEqual(cell["per_model"]["clanker"]["hits"], 1)
            self.assertEqual(cell["per_model"]["clanker"]["trials"], 3)
            self.assertEqual(cell["per_model"]["volume_up"]["false_wakes"], 1)
            self.assertAlmostEqual(cell["exposure_min"], 10.0 / 60.0, places=3)
            self.assertEqual(cell["volume"], 35)
            self.assertEqual(cell["aec"], "off")


if __name__ == "__main__":
    unittest.main()
