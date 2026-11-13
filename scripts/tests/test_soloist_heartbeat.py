"""Offline tests for the Soloist status heartbeat and healthcheck.

The receiver binary is proprietary, so these tests drive the shell scripts
with a fake `soloist` CLI. They cover the metadata-only status file, expiry
maths and the Compose health decision without Docker or a Spotify account.
"""

from __future__ import annotations

import os
import subprocess
import tempfile
import time
import unittest
from datetime import datetime, timedelta
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HEARTBEAT = ROOT / "soloist" / "heartbeat.sh"
HEALTHCHECK = ROOT / "soloist" / "healthcheck.sh"


class SoloistStatusTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.data = self.dir / "data"
        self.status_dir = self.dir / "status"
        self.data.mkdir()
        self.status_dir.mkdir()
        self.log = self.data / "soloist.log"
        self.binary = self.dir / "soloist"
        self.write_fake("running", logged_in=True, active=True)
        self.env = {
            **os.environ,
            "SOLOIST_BINARY": str(self.binary),
            "SOLOIST_DATA_DIR": str(self.data),
            "SOLOIST_STATUS_DIR": str(self.status_dir),
            "SOLOIST_STATUS_FILE": str(self.status_dir / "soloist.status"),
            "SOLOIST_LOG_FILE": str(self.log),
            "SOLOIST_DEVICE_NAME": "Clanka",
            "SOLOIST_ONESHOT": "1",
        }

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def write_fake(self, state: str, *, logged_in: bool, active: bool, exit_code: int = 0) -> None:
        self.binary.write_text(
            "#!/bin/sh\n"
            f"printf '%s\\n' 'soloist: {state} (pid 1)' '  ws: listening on 127.0.0.1:24880' "
            f"'  logged in: {'yes' if logged_in else 'no'}' "
            f"'  active device: {'yes' if active else 'no'}'\n"
            f"exit {exit_code}\n"
        )
        self.binary.chmod(0o755)

    def write_expiry(self, when: datetime, days: int) -> None:
        stamp = when.strftime("%Y-%m-%d %H:%M:%S")
        self.log.write_text(
            f"{stamp}.000: soloist 1.3.10.70 build 1794571308 (20261113) (g19412df6ea) (linux/aarch64)\n"
            f"{stamp}.001: client expires in {days} days\n"
            f"{stamp}.002: waiting for login\n"
        )

    def run_heartbeat(self) -> str:
        subprocess.run(
            ["sh", str(HEARTBEAT)],
            check=True,
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )
        return (self.status_dir / "soloist.status").read_text()

    def fields(self) -> dict[str, str]:
        body = (self.status_dir / "soloist.status").read_text()
        return dict(line.split("=", 1) for line in body.splitlines() if "=" in line)

    def run_healthcheck(self) -> int:
        return subprocess.run(
            ["sh", str(HEALTHCHECK)],
            env=self.env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        ).returncode

    def test_paired_active_heartbeat_is_metadata_only(self) -> None:
        self.write_expiry(datetime.now(), 89)
        self.run_heartbeat()
        body = self.run_heartbeat()
        fields = self.fields()
        self.assertEqual(fields["version"], "2")
        self.assertEqual(fields["state"], "paired")
        self.assertEqual(fields["session"], "active")
        self.assertEqual(fields["device"], "Clanka")
        self.assertEqual(fields["expires_days"], "89")
        self.assertTrue(fields["updated"])
        # The status file never carries an account id, token or track name.
        for forbidden in ("user", "token", "track", "radiohead"):
            self.assertNotIn(forbidden, body.lower())

    def test_unpaired_receiver_is_reported_honestly(self) -> None:
        self.write_fake("running", logged_in=False, active=False)
        self.write_expiry(datetime.now(), 89)
        self.run_heartbeat()
        fields = self.fields()
        self.assertEqual(fields["state"], "unpaired")
        self.assertEqual(fields["session"], "idle")

    def test_control_socket_failure_is_unavailable_not_unpaired(self) -> None:
        self.write_fake("running", logged_in=False, active=False, exit_code=1)
        self.run_heartbeat()
        fields = self.fields()
        self.assertEqual(fields["state"], "unavailable")
        self.assertEqual(fields["session"], "unknown")

    def test_expiry_is_relative_to_the_original_startup_line(self) -> None:
        self.write_expiry(datetime.now() - timedelta(days=5), 3)
        self.run_heartbeat()
        self.assertLess(int(self.fields()["expires_days"]), 0)

    def test_missing_expiry_line_is_unknown(self) -> None:
        self.run_heartbeat()
        self.assertEqual(self.fields()["expires_days"], "unknown")

    def test_healthcheck_fails_on_a_stale_or_expired_heartbeat(self) -> None:
        self.write_expiry(datetime.now(), 89)
        self.run_heartbeat()
        self.assertEqual(self.run_healthcheck(), 0)

        # An expired build cannot play, even though the daemon answers.
        self.write_expiry(datetime.now() - timedelta(days=5), 3)
        self.run_heartbeat()
        self.assertNotEqual(self.run_healthcheck(), 0)

        # A heartbeat that stopped updating is a real failure.
        self.write_expiry(datetime.now(), 89)
        self.run_heartbeat()
        old = time.time() - 120
        os.utime(self.status_dir / "soloist.status", (old, old))
        self.assertNotEqual(self.run_healthcheck(), 0)

    def test_healthcheck_fails_when_the_daemon_does_not_answer(self) -> None:
        self.write_expiry(datetime.now(), 89)
        self.run_heartbeat()
        self.write_fake("running", logged_in=True, active=True, exit_code=1)
        self.assertNotEqual(self.run_healthcheck(), 0)


if __name__ == "__main__":
    unittest.main()
