"""Tests for the Soloist healthcheck script.

Proves that Compose health indicates process liveness and heartbeat freshness,
allowing expired receiver builds to exit 0 without failing deploys, while
keeping expires_days intact in the status file and failing on stale heartbeats,
daemon errors, or unparseable expiry values.
"""

from __future__ import annotations

import os
import subprocess
import tempfile
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HEALTHCHECK = ROOT / "soloist" / "healthcheck.sh"


class SoloistHealthcheckTest(unittest.TestCase):
    # Set up a sandbox directory with a fake soloist binary and status directory.
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.data = self.dir / "data"
        self.status_dir = self.dir / "status"
        self.data.mkdir()
        self.status_dir.mkdir()
        self.status_file = self.status_dir / "soloist.status"
        self.binary = self.dir / "soloist"
        self.write_fake("running", exit_code=0)
        self.env = {
            **os.environ,
            "SOLOIST_BINARY": str(self.binary),
            "SOLOIST_DATA_DIR": str(self.data),
            "SOLOIST_STATUS_DIR": str(self.status_dir),
            "SOLOIST_STATUS_FILE": str(self.status_file),
        }

    # Clean up the temporary test environment.
    def tearDown(self) -> None:
        self.tmp.cleanup()

    # Create an executable stub soloist CLI returning the requested state and exit status.
    def write_fake(self, state: str = "running", exit_code: int = 0) -> None:
        self.binary.write_text(
            "#!/bin/sh\n"
            f"printf '%s\\n' 'soloist: {state} (pid 1)' '  ws: listening on 127.0.0.1:24880'\n"
            f"exit {exit_code}\n"
        )
        self.binary.chmod(0o755)

    # Write a well-formed status file with the given expires_days value and optional age.
    def write_status(self, expires_days: str | int, age_seconds: int = 0) -> None:
        content = (
            "version=2\n"
            "state=paired\n"
            "session=idle\n"
            "device=Clanka\n"
            f"expires_days={expires_days}\n"
            "updated=2026-11-17T11:00:00Z\n"
        )
        self.status_file.write_text(content)
        if age_seconds > 0:
            past = time.time() - age_seconds
            os.utime(self.status_file, (past, past))

    # Execute healthcheck.sh in the stubbed environment and return its exit code.
    def run_healthcheck(self) -> int:
        return subprocess.run(
            ["sh", str(HEALTHCHECK)],
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ).returncode

    # An expired build with expires_days=-1 must exit 0 and leave the status file unchanged.
    def test_expired_negative_one_exits_zero_and_preserves_status(self) -> None:
        self.write_status(-1)
        original_content = self.status_file.read_text()
        rc = self.run_healthcheck()
        self.assertEqual(rc, 0)
        # Verify status file was not modified or rewritten to 0.
        self.assertEqual(self.status_file.read_text(), original_content)

    # Arbitrary negative expiry days must exit 0 and leave the status file unchanged.
    def test_expired_arbitrary_negative_exits_zero(self) -> None:
        self.write_status(-7)
        original_content = self.status_file.read_text()
        rc = self.run_healthcheck()
        self.assertEqual(rc, 0)
        self.assertEqual(self.status_file.read_text(), original_content)

    # Zero expiry indicates less than a day remaining and must exit 0.
    def test_zero_expiry_exits_zero(self) -> None:
        self.write_status(0)
        self.assertEqual(self.run_healthcheck(), 0)

    # Positive expiry indicates healthy days remaining and must exit 0.
    def test_positive_expiry_exits_zero(self) -> None:
        self.write_status(2)
        self.assertEqual(self.run_healthcheck(), 0)

    # A status file that has not been updated within 60 seconds must fail the healthcheck.
    def test_stale_heartbeat_exits_one(self) -> None:
        self.write_status(89, age_seconds=120)
        self.assertEqual(self.run_healthcheck(), 1)

    # A daemon failure or control socket error must fail the healthcheck.
    def test_daemon_error_exits_one(self) -> None:
        self.write_fake("running", exit_code=1)
        self.write_status(89)
        self.assertEqual(self.run_healthcheck(), 1)

    # A daemon that is not reported as running must fail the healthcheck.
    def test_daemon_not_running_exits_one(self) -> None:
        self.write_fake("stopped", exit_code=0)
        self.write_status(89)
        self.assertEqual(self.run_healthcheck(), 1)

    # Malformed or unparseable expiry days must fail the healthcheck.
    def test_unparseable_expiry_exits_one(self) -> None:
        for malformed in ["invalid", "-1abc", "12a", "--1", "-"]:
            with self.subTest(expiry=malformed):
                self.write_status(malformed)
                self.assertEqual(self.run_healthcheck(), 1)


if __name__ == "__main__":
    unittest.main()
