"""Regression tests for clip padding in the wakeword evaluator."""

import io
import json
import sys
import tempfile
import unittest
import wave
from contextlib import redirect_stdout
from pathlib import Path
from types import ModuleType
from unittest import mock

WAKEWORD = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(WAKEWORD / "training"))
import evaluate  # noqa: E402


def short_positive(path):
    source = WAKEWORD / "tests/fixtures/positive/clanker_02.wav"
    # Keep the spoken phrase in a 1.2 s clip, shorter than the detector window.
    with wave.open(str(source), "rb") as original, wave.open(str(path), "wb") as output:
        output.setparams(original.getparams())
        output.writeframes(original.readframes(19200))


class EvaluateTests(unittest.TestCase):
    def test_cli_pads_clips_and_scores_background_as_is(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            clips = root / "clips"
            clips.mkdir()
            positive = clips / "short.wav"
            short_positive(positive)
            result = root / "result.json"

            def predict(paths, *, wakeword_models, ncpu, padding):
                # Inspect the WAVs passed to openWakeWord before cleanup.
                self.assertEqual(padding, 0)
                path = paths[0]
                if path == str(positive):
                    self.assertEqual(evaluate.wav_seconds(path), 1.2)
                else:
                    self.assertEqual(evaluate.wav_seconds(path), 4.2)
                    with wave.open(path) as padded, wave.open(str(positive)) as raw:
                        self.assertEqual(padded.readframes(16000), bytes(32000))
                        self.assertEqual(padded.readframes(19200), raw.readframes(19200))
                        self.assertEqual(padded.readframes(32000), bytes(64000))
                return {path: [{"score": 0.9985}]}

            utils = ModuleType("openwakeword.utils")
            utils.bulk_predict = mock.Mock(side_effect=predict)
            args = ["evaluate.py", "--model", "model.tflite", "--positives", str(clips),
                    "--negatives", str(clips), "--background", str(clips),
                    "--json-out", str(result)]
            with mock.patch.dict(sys.modules, {"openwakeword.utils": utils}), \
                    mock.patch.object(sys, "argv", args), redirect_stdout(io.StringIO()):
                evaluate.main()

            scores = json.loads(result.read_text())
            self.assertEqual(utils.bulk_predict.call_count, 3)
            self.assertEqual(list(scores["positives"]), [str(positive)])
            self.assertEqual(list(scores["negatives"]), [str(positive)])
            events = scores["background_events_per_hour"]
            self.assertEqual(events["0.998"], 3000)
            self.assertEqual(events["0.999"], 0)

    def test_short_positive_crosses_threshold_only_with_padding(self):
        try:
            import openwakeword.utils  # noqa: F401
        except ModuleNotFoundError as exc:
            if exc.name != "openwakeword":
                raise
            self.skipTest("requires the wakeword training environment and model assets")

        with tempfile.TemporaryDirectory() as directory:
            positive = Path(directory) / "short.wav"
            short_positive(positive)
            model = str(WAKEWORD / "models/clanker.tflite")
            raw = evaluate.frame_scores([positive], model, 1)
            padded = evaluate.frame_scores([positive], model, 1, 1, 2)
        self.assertLessEqual(max(raw[str(positive)]), 0.9)
        self.assertGreater(max(padded[str(positive)]), 0.9)
