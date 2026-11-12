"""Offline tests for the #70 wake-over-music recorder (#70)."""

from __future__ import annotations

import importlib.util
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPT = Path(__file__).resolve().parents[1] / "wake_over_music.py"
FIXTURE = Path(__file__).parent / "fixtures" / "wake-over-music.synthetic.json"
spec = importlib.util.spec_from_file_location("wake_over_music", SCRIPT)
wake = importlib.util.module_from_spec(spec)
assert spec.loader is not None
spec.loader.exec_module(wake)

VOLUMES = [25, 50, 75]


def run_cli(*args: str) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        check=False,
        capture_output=True,
        text=True,
    )


def fill_host(doc: dict) -> None:
    doc["host"].update(
        {
            "device": "test box",
            "pipewire": "1.6.11",
            "pipewire_pulse": "15.0.0",
            "capture_source": "aec_mic_source",
            "playback_sink": "aec_music_sink",
            "aec_source": "aec_mic_source",
            "aec_sink": "aec_music_sink",
        }
    )


def fill_cells(doc: dict, hits: int = 18, false_wakes: int = 1) -> None:
    for index, cell in enumerate(doc["cells"]):
        cell.update(
            {
                "trials": 20,
                "hits": hits,
                "false_wakes": false_wakes,
                "exposure_min": 30,
                "started_at": f"2026-11-12T{9 + index:02d}:00:00+01:00",
                "ended_at": f"2026-11-12T{9 + index:02d}:40:00+01:00",
                "note": "",
            }
        )


class RecorderTest(unittest.TestCase):
    def test_init_writes_six_unmeasured_cells(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            completed = run_cli("init", "--out", str(path), "--volumes", "25,50,75")
            self.assertEqual(completed.returncode, 0, completed.stderr)
            doc = json.loads(path.read_text())
            self.assertEqual(len(doc["cells"]), 6)
            self.assertTrue(all(cell["trials"] is None for cell in doc["cells"]))
            summary = wake.summarize(doc)
            self.assertEqual(summary["status"], "UNMEASURED")
            self.assertFalse(summary["evidence"])
            self.assertEqual(summary["measured_cells"], 0)
            report = run_cli("report", "--file", str(path))
            self.assertEqual(report.returncode, 0, report.stderr)
            self.assertEqual(json.loads(report.stdout)["status"], "UNMEASURED")

    def test_init_refuses_duplicate_volumes(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            completed = run_cli(
                "init", "--out", str(Path(tmp) / "evidence.json"),
                "--volumes", "30,30,90",
            )
            self.assertEqual(completed.returncode, 2)
            self.assertIn("repeated", completed.stderr)

    def test_record_accepts_one_real_count_and_stays_unmeasured(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            completed = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "on",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            summary = wake.summarize(json.loads(path.read_text()))
            self.assertEqual(summary["status"], "UNMEASURED")
            self.assertEqual(summary["measured_cells"], 1)
            self.assertEqual(summary["cells"][1]["hit_rate"], 0.9)
            self.assertEqual(summary["cells"][1]["false_wakes_per_hour"], 0.0)

    def test_record_refuses_hits_over_trials(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            completed = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "11", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(completed.returncode, 2)
            self.assertIn("exceeds trials", completed.stderr)
            self.assertTrue(
                all(cell["trials"] is None for cell in json.loads(path.read_text())["cells"])
            )

    def test_record_refuses_zero_exposure_and_backwards_time(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            zero = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "0", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(zero.returncode, 2)
            backward = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:10:00Z",
                "--ended-at", "2026-11-12T09:00:00Z",
            )
            self.assertEqual(backward.returncode, 2)
            self.assertIn("after started_at", backward.stderr)

    def test_record_refuses_naive_timestamp_and_unknown_volume(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            naive = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00",
                "--ended-at", "2026-11-12T09:10:00",
            )
            self.assertEqual(naive.returncode, 2)
            self.assertIn("UTC offset", naive.stderr)
            unknown = run_cli(
                "record", "--file", str(path), "--volume", "40", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(unknown.returncode, 2)
            self.assertIn("not one of the configured", unknown.stderr)

    def test_record_refuses_exposure_longer_than_the_session(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(
                run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode,
                0,
            )
            completed = run_cli(
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "30", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(completed.returncode, 2)
            self.assertIn("longer than the", completed.stderr)

    def test_record_refuses_to_overwrite_without_replace(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            args = (
                "record", "--file", str(path), "--volume", "25", "--aec", "off",
                "--trials", "10", "--hits", "9", "--false-wakes", "0",
                "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00Z",
                "--ended-at", "2026-11-12T09:10:00Z",
            )
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            self.assertEqual(run_cli(*args).returncode, 0)
            refused = run_cli(*args)
            self.assertEqual(refused.returncode, 2)
            self.assertIn("--replace", refused.stderr)
            self.assertEqual(run_cli(*args, "--replace").returncode, 0)

    def test_record_refuses_a_synthetic_fixture(self) -> None:
        completed = run_cli(
            "record", "--file", str(FIXTURE), "--volume", "25", "--aec", "off",
            "--trials", "10", "--hits", "9", "--false-wakes", "0",
            "--exposure-min", "5", "--started-at", "2026-11-12T09:00:00Z",
            "--ended-at", "2026-11-12T09:10:00Z",
        )
        self.assertEqual(completed.returncode, 2)
        self.assertIn("never collect operator evidence", completed.stderr)

    def test_report_refuses_a_partially_filled_cell(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode, 0)
            doc = json.loads(path.read_text())
            doc["cells"][0]["trials"] = 10
            path.write_text(json.dumps(doc))
            completed = run_cli("report", "--file", str(path))
            self.assertEqual(completed.returncode, 2)
            self.assertIn("incomplete", completed.stderr)

    def test_synthetic_fixture_is_labelled_and_never_evidence(self) -> None:
        summary = wake.summarize(json.loads(FIXTURE.read_text()))
        self.assertEqual(summary["status"], "SYNTHETIC")
        self.assertFalse(summary["evidence"])
        self.assertEqual(summary["measured_cells"], 6)
        by_key = {(cell["volume"], cell["aec"]): cell for cell in summary["cells"]}
        self.assertEqual(by_key[(75, "off")]["hit_rate"], 0.4)
        self.assertEqual(by_key[(75, "off")]["false_wakes_per_hour"], 10.0)
        self.assertEqual(by_key[(75, "on")]["hit_rate"], 0.9)
        self.assertEqual(by_key[(75, "on")]["false_wakes_per_hour"], 4.0)
        completed = run_cli("report", "--file", str(FIXTURE))
        self.assertEqual(completed.returncode, 1)
        self.assertEqual(json.loads(completed.stdout)["status"], "SYNTHETIC")

    def test_full_operator_matrix_is_measured_but_decision_stays_pending(self) -> None:
        doc = wake.new_document(VOLUMES)
        fill_host(doc)
        fill_cells(doc)
        summary = wake.summarize(doc, target_hit_rate=0.9, target_false_per_hour=5)
        self.assertEqual(summary["status"], "MEASURED")
        self.assertTrue(summary["evidence"])
        self.assertTrue(summary["decision_ready"])
        self.assertTrue(all(cell["meets_hit_rate_target"] for cell in summary["cells"]))
        self.assertIsNone(summary["decision"]["max_volume"])
        self.assertEqual(summary["decision"]["status"], "pending")

    def test_decided_decision_needs_both_targets(self) -> None:
        doc = wake.new_document(VOLUMES)
        fill_host(doc)
        fill_cells(doc)
        doc["decision"] = {
            "status": "decided",
            "max_volume": None,
            "note": "no cap",
        }
        with self.assertRaises(wake.InputError) as caught:
            wake.summarize(doc)
        self.assertIn("both operator targets", str(caught.exception))
        summary = wake.summarize(doc, target_hit_rate=0.9, target_false_per_hour=5)
        self.assertEqual(summary["status"], "MEASURED")
        self.assertTrue(summary["decision_ready"])

    def test_measured_file_without_host_fields_is_refused(self) -> None:
        doc = wake.new_document(VOLUMES)
        fill_cells(doc)
        with self.assertRaises(wake.InputError) as caught:
            wake.summarize(doc)
        self.assertIn("host.device", str(caught.exception))

    def test_pending_decision_cannot_carry_a_max_volume(self) -> None:
        doc = wake.new_document(VOLUMES)
        doc["decision"]["max_volume"] = 60
        errors = wake.validate_document(doc)
        self.assertTrue(any("must stay null" in error for error in errors))

    def test_full_cli_flow_writes_measured_report(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "evidence.json"
            self.assertEqual(
                run_cli("init", "--out", str(path), "--volumes", "25,50,75").returncode,
                0,
            )
            doc = json.loads(path.read_text())
            fill_host(doc)
            path.write_text(json.dumps(doc))
            for index, (volume, aec) in enumerate(
                (volume, aec) for volume in VOLUMES for aec in wake.AEC_STATES
            ):
                completed = run_cli(
                    "record", "--file", str(path), "--volume", str(volume),
                    "--aec", aec, "--trials", "20", "--hits", "18",
                    "--false-wakes", "1", "--exposure-min", "30",
                    "--started-at", f"2026-11-12T{9 + index:02d}:00:00+01:00",
                    "--ended-at", f"2026-11-12T{9 + index:02d}:40:00+01:00",
                )
                self.assertEqual(completed.returncode, 0, completed.stderr)
            completed = run_cli(
                "report", "--file", str(path),
                "--target-hit-rate", "0.9", "--target-false-per-hour", "5",
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            summary = json.loads(completed.stdout)
            self.assertEqual(summary["status"], "MEASURED")
            self.assertTrue(summary["evidence"])
            self.assertTrue(summary["decision_ready"])
            self.assertIsNone(summary["decision"]["max_volume"])


if __name__ == "__main__":
    unittest.main()
