"""Offline tests for the mic-noise calibration analyser (#36)."""

from __future__ import annotations

import importlib.util
import json
import math
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "mic_noise.py"
spec = importlib.util.spec_from_file_location("mic_noise", SCRIPT)
mic_noise = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(mic_noise)


def pcm16(values: list[int]) -> bytes:
    return b"".join(int(v).to_bytes(2, "little", signed=True) for v in values)


class MicNoiseTest(unittest.TestCase):
    def test_silence_is_below_the_threshold(self) -> None:
        result = mic_noise.analyze(pcm16([0] * 16000), 16000, 1, 2, 32, -38.0)
        self.assertGreater(result["frames"], 0)
        self.assertEqual(result["frames_above_threshold"], 0)
        self.assertLess(result["idle_rms_dbfs"], -38)

    def test_loud_frames_are_counted(self) -> None:
        loud = pcm16([8000] * 16000)
        result = mic_noise.analyze(loud, 16000, 1, 2, 32, -38.0)
        self.assertGreater(result["frames_above_threshold"], 0)
        self.assertGreater(result["percent_above_threshold"], 50)
        self.assertTrue(math.isclose(result["frame_peak_dbfs"], -12.3, abs_tol=0.2))
        # Idle RMS + 6 dB is the suggested headroom, capped so speech can
        # still clear it. It is not the old -38 default.
        self.assertGreater(result["recommended_threshold_dbfs"], -38)
        self.assertLessEqual(result["recommended_threshold_dbfs"], -20)

    def test_cli_prints_json(self) -> None:
        with tempfile.NamedTemporaryFile(suffix=".raw") as handle:
            handle.write(pcm16([1000] * 1600))
            handle.flush()
            completed = subprocess.run(
                [sys.executable, str(SCRIPT), handle.name, "--json"],
                check=True,
                capture_output=True,
                text=True,
            )
        result = json.loads(completed.stdout)
        self.assertIn("idle_rms_dbfs", result)
        self.assertNotIn("error", result)


if __name__ == "__main__":
    unittest.main()
