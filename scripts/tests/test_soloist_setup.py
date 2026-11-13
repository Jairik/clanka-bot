"""Tests for scripts/soloist-setup.sh.

The official archive is proprietary and fetched at install time, so these
tests use a local fixture archive and a stub curl. They cover architecture
selection, key import, state migration and the no-network path.
"""

from __future__ import annotations

import os
import stat
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SETUP = ROOT / "scripts" / "soloist-setup.sh"


class SoloistSetupTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.dir = Path(self.tmp.name)
        self.soloist_dir = self.dir / "soloist"
        self.binary = self.soloist_dir / "soloist"
        self.legacy = self.dir / "legacy"
        self.home = self.dir / "home"
        self.home.mkdir()
        self.env = {
            **os.environ,
            "HOME": str(self.home),
            "SOLOIST_DIR": str(self.soloist_dir),
            "SOLOIST_BINARY": str(self.binary),
            "SOLOIST_LEGACY_DIR": str(self.legacy),
        }

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def run_setup(self, **extra: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["bash", str(SETUP)],
            env={**self.env, **extra},
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
        )

    def install_fixture_binary(self) -> None:
        self.soloist_dir.mkdir(parents=True)
        self.binary.write_text("#!/bin/sh\nexit 0\n")
        self.binary.chmod(0o755)

    def test_existing_binary_is_reused_without_network(self) -> None:
        self.install_fixture_binary()
        (self.soloist_dir / "api-key").write_text("key\n")
        result = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("using existing receiver binary", result.stdout)
        self.assertEqual(stat.S_IMODE((self.soloist_dir / "api-key").stat().st_mode), 0o600)

    def test_imports_key_and_migrates_probe_state(self) -> None:
        self.install_fixture_binary()
        (self.legacy / "private" / "data" / "settings").mkdir(parents=True)
        (self.legacy / "private" / "data" / "settings" / "prefs").write_text("private")
        (self.legacy / "private" / "cache").mkdir(parents=True)
        (self.legacy / "private" / "cache" / "audio").write_text("cached")
        (self.legacy / "private" / "api-key").write_text("legacy-key\n")
        (self.legacy / "private" / "api-key").chmod(0o600)

        result = self.run_setup()
        self.assertEqual(result.returncode, 0, result.stderr)
        key = (self.soloist_dir / "api-key").read_text()
        self.assertEqual(key, "legacy-key\n")
        self.assertEqual((self.soloist_dir / "data" / "settings" / "prefs").read_text(), "private")
        self.assertEqual((self.soloist_dir / "cache" / "audio").read_text(), "cached")
        # The probe's own copy is the backup and must stay untouched.
        self.assertTrue((self.legacy / "private" / "data" / "settings" / "prefs").exists())
        self.assertIn("migrated", result.stdout)

    def test_downloads_the_archive_for_the_requested_architecture(self) -> None:
        self.soloist_dir.mkdir(parents=True)
        archive = self.dir / "fixture.tar.gz"
        payload = self.dir / "payload"
        payload.write_text("#!/bin/sh\necho soloist\n")
        payload.chmod(0o755)
        with tarfile.open(archive, "w:gz") as tar:
            tar.add(payload, arcname="soloist")

        bin_dir = self.dir / "bin"
        bin_dir.mkdir()
        calls = self.dir / "curl-calls"
        curl = bin_dir / "curl"
        curl.write_text(
            "#!/bin/sh\n"
            f"printf '%s\\n' \"$*\" >> {calls}\n"
            f"for last; do :; done\n"
            f"cp {archive} \"$last\"\n"
        )
        curl.chmod(0o755)
        # A key is required before the download is attempted.
        self.soloist_dir.mkdir(exist_ok=True)
        (self.soloist_dir / "api-key").write_text("key\n")

        result = self.run_setup(SOLOIST_ARCH="aarch64", PATH=f"{bin_dir}:{os.environ['PATH']}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("soloist_release_arm64.tar.gz", calls.read_text())
        self.assertTrue(self.binary.exists())
        self.assertIn("installed arm64 receiver binary", result.stdout)

    def test_missing_key_fails_with_actionable_text(self) -> None:
        self.install_fixture_binary()
        result = self.run_setup()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no Soloist API key", result.stderr)
        self.assertIn("developer.spotify.com/documentation/soloist", result.stderr)

    def test_unsupported_architecture_is_rejected(self) -> None:
        result = self.run_setup(SOLOIST_ARCH="riscv64")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("unsupported architecture", result.stderr)


if __name__ == "__main__":
    unittest.main()
