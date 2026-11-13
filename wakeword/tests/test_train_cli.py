#!/usr/bin/env python3
"""Tests for the desktop wake-phrase CLI. No GPU, no TTS, no network."""

import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
import unittest
import wave
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

TRAIN = Path(__file__).resolve().parents[1] / "training"
sys.path.insert(0, str(TRAIN))
import train_cli  # noqa: E402


def write_wav(path, seconds=1.0, rate=16000, channels=1, width=2, freq=440.0, junk=False):
    """Write a deterministic synthetic clip; never a real recording."""
    import math

    frames = int(seconds * rate)
    samples = []
    for i in range(frames):
        value = int(math.sin(2 * math.pi * freq * i / rate) * 6000)
        if width == 1:
            samples.append(bytes([(value >> 8) + 128]))
        else:
            samples.append(int(value).to_bytes(width, "little", signed=True) * channels)
    data = b"".join(samples)
    path.parent.mkdir(parents=True, exist_ok=True)
    if junk:
        # A JUNK chunk changes the bytes but not the PCM, which is the dedupe
        # key the CLI uses.
        fmt = b"fmt " + (16).to_bytes(4, "little") + (1).to_bytes(2, "little")
        fmt += channels.to_bytes(2, "little") + rate.to_bytes(4, "little")
        fmt += (rate * channels * width).to_bytes(4, "little") + (channels * width).to_bytes(2, "little")
        fmt += (width * 8).to_bytes(2, "little")
        junk_chunk = b"JUNK" + (4).to_bytes(4, "little") + b"junk"
        body = b"WAVE" + fmt + junk_chunk + b"data"
        body += len(data).to_bytes(4, "little") + data
        path.write_bytes(b"RIFF" + len(body).to_bytes(4, "little") + body)
        return
    with wave.open(str(path), "wb") as handle:
        handle.setnchannels(channels)
        handle.setsampwidth(width)
        handle.setframerate(rate)
        handle.writeframes(data)


def make_bundle(root, positives=6, negatives=2, phrase="volume up", extra_manifest=None):
    for i in range(positives):
        write_wav(root / "real" / "positive" / f"real_p_{i + 1:05d}.wav", seconds=1.0 + i / 100, freq=300 + i)
    for i in range(negatives):
        write_wav(root / "real" / "negative" / f"real_n_{i + 1:05d}.wav", seconds=1.1 + i / 100, freq=200 + i)
    manifest = {
        "format": 1,
        "phrase": phrase,
        "slug": phrase.replace(" ", "-"),
        "source": "dashboard",
        "positives": positives,
        "negatives": negatives,
        "sample_rate": 16000,
        "channels": 1,
        "sample_width": 2,
        "clip_seconds": 1.9,
    }
    if extra_manifest:
        manifest.update(extra_manifest)
    (root / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
    return manifest


def make_tar(source_dir, archive):
    with tarfile.open(archive, "w:gz") as handle:
        for path in sorted(source_dir.rglob("*")):
            if path.is_file():
                handle.add(path, arcname=str(path.relative_to(source_dir)))
    return archive


class PhraseTests(unittest.TestCase):
    def test_slug_for_normalises_and_rejects_paths(self):
        self.assertEqual(train_cli.slug_for("Volume Up"), "volume-up")
        self.assertEqual(train_cli.slug_for("  hey clanka "), "hey-clanka")
        for bad in ("../clanker", "a/b", "a.b", "", "one two three four five"):
            with self.assertRaises(train_cli.CLIError):
                train_cli.slug_for(bad)

    def test_resolve_phrase(self):
        self.assertEqual(train_cli.resolve_phrase("volume-up")[1], "volume up")
        self.assertEqual(train_cli.resolve_phrase("volume up")[0], "volume-up")
        self.assertEqual(train_cli.resolve_phrase(None)[0], "clanker")

    def test_phrase_lists_use_the_curated_clanker_files(self):
        config = {"phrase": "clanker"}
        lists = train_cli.phrase_lists("clanker", config)
        self.assertIn("clanker", lists["positives"])
        self.assertIn("clank", lists["negatives"])
        self.assertIn("turn on the lights", lists["near_misses"])

    def test_generated_near_misses_include_the_sibling_command(self):
        near = train_cli.generated_near_misses("volume up")
        self.assertIn("volume down", near)
        self.assertNotIn("volume up", near)


class WAVTests(unittest.TestCase):
    def test_accepts_a_canonical_clip(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "take.wav"
            write_wav(path)
            frames, pcm = train_cli.wav_pcm(path)
            self.assertEqual(frames, 16000)
            self.assertEqual(len(pcm), 32000)

    def test_rejects_wrong_range_channels_and_rate(self):
        with tempfile.TemporaryDirectory() as tmp:
            for name, kwargs in {
                "rate": {"rate": 44100},
                "stereo": {"channels": 2},
                "width": {"width": 1},
                "short": {"seconds": 0.1},
                "long": {"seconds": 2.5},
            }.items():
                path = Path(tmp) / f"{name}.wav"
                write_wav(path, **kwargs)
                with self.subTest(name), self.assertRaises(train_cli.CLIError):
                    train_cli.wav_pcm(path)

    def test_pcm_digest_ignores_an_extra_header_chunk(self):
        with tempfile.TemporaryDirectory() as tmp:
            plain = Path(tmp) / "plain.wav"
            junk = Path(tmp) / "junk.wav"
            write_wav(plain, junk=False)
            write_wav(junk, junk=True)
            self.assertNotEqual(plain.read_bytes(), junk.read_bytes())
            self.assertEqual(train_cli.wav_pcm(plain)[1], train_cli.wav_pcm(junk)[1])


class ImportTests(unittest.TestCase):
    def run_import(self, source, phrase=None, work=None):
        with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
            return train_cli.import_bundle(source, phrase, work, "rbpi@pi")

    def test_imports_a_directory_into_the_recorder_layout(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root)
            work = Path(tmp) / "work"
            report = self.run_import(str(root), work=str(work))
            self.assertEqual(report["slug"], "volume-up")
            self.assertEqual(report["imported"], {"positive": 6, "negative": 2})
            self.assertEqual(len(list((work / "real" / "positive").glob("real_p_*.wav"))), 6)
            self.assertEqual(len(list((work / "real" / "negative").glob("real_n_*.wav"))), 2)
            self.assertTrue((work / "import.json").exists())

    def test_reimport_dedupes_by_pcm(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root)
            work = Path(tmp) / "work"
            first = self.run_import(str(root), work=str(work))
            second = self.run_import(str(root), work=str(work))
            self.assertEqual(first["imported"]["positive"], 6)
            self.assertEqual(second["imported"]["positive"], 0)
            self.assertEqual(second["skipped"]["positive"], 6)
            self.assertEqual(len(list((work / "real" / "positive").glob("*.wav"))), 6)

    def test_imports_a_tar_gz_with_a_top_level_directory(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root, phrase="clanker")
            wrapped = Path(tmp) / "wrapped"
            (wrapped / "clanker").mkdir(parents=True)
            for path in root.rglob("*"):
                if path.is_file():
                    target = wrapped / "clanker" / path.relative_to(root)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(path.read_bytes())
            archive = make_tar(wrapped, Path(tmp) / "bundle.tar.gz")
            work = Path(tmp) / "work"
            report = self.run_import(str(archive), work=str(work))
            self.assertEqual(report["slug"], "clanker")
            self.assertEqual(report["imported"]["positive"], 6)

    def test_imports_an_anonymous_directory_without_a_manifest(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root)
            (root / "manifest.json").unlink()
            work = Path(tmp) / "work"
            report = self.run_import(str(root), phrase="volume up", work=str(work))
            self.assertEqual(report["slug"], "volume-up")

    def test_rejects_a_phrase_mismatch(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root, phrase="volume up")
            with self.assertRaises(train_cli.CLIError):
                self.run_import(str(root), phrase="clanker", work=str(Path(tmp) / "work"))

    def test_rejects_too_few_and_too_many_positives(self):
        with tempfile.TemporaryDirectory() as tmp:
            few = Path(tmp) / "few"
            make_bundle(few, positives=4)
            with self.assertRaisesRegex(train_cli.CLIError, "5–100"):
                self.run_import(str(few), work=str(Path(tmp) / "work"))
            many = Path(tmp) / "many"
            make_bundle(many, positives=101)
            with self.assertRaisesRegex(train_cli.CLIError, "5–100"):
                self.run_import(str(many), work=str(Path(tmp) / "work"))

    def test_rejects_a_bad_clip_before_copying_anything(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root)
            write_wav(root / "real" / "positive" / "real_p_00003.wav", rate=44100)
            work = Path(tmp) / "work"
            with self.assertRaisesRegex(train_cli.CLIError, "16000"):
                self.run_import(str(root), work=str(work))
            self.assertFalse((work / "real" / "positive").exists())

    def test_rejects_unsafe_tar_entries(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive = Path(tmp) / "evil.tar.gz"
            with tarfile.open(archive, "w:gz") as handle:
                data = b"x" * 10
                for name in ("../escape.wav", "/absolute.wav", "real/positive/../../escape.wav"):
                    info = tarfile.TarInfo(name)
                    info.size = len(data)
                    handle.addfile(info, io.BytesIO(data))
            with self.assertRaisesRegex(train_cli.CLIError, "unsafe path"):
                self.run_import(str(archive), work=str(Path(tmp) / "work"))

    def test_refuses_to_exceed_the_work_directory_limit(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root)
            work = Path(tmp) / "work"
            for i in range(train_cli.MAX_TAKES - 2):
                write_wav(work / "real" / "positive" / f"real_p_{i + 1:05d}.wav", seconds=0.5, freq=100 + i)
            with self.assertRaisesRegex(train_cli.CLIError, "at most 100"):
                self.run_import(str(root), work=str(work))

    def test_reimporting_a_full_set_is_a_no_op(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "bundle"
            make_bundle(root, positives=train_cli.MAX_TAKES, negatives=0)
            work = Path(tmp) / "work"
            first = self.run_import(str(root), work=str(work))
            second = self.run_import(str(root), work=str(work))
            self.assertEqual(first["imported"]["positive"], train_cli.MAX_TAKES)
            self.assertEqual(second["imported"]["positive"], 0)
            self.assertEqual(second["skipped"]["positive"], train_cli.MAX_TAKES)

    def test_scp_hint_names_the_ssh_command(self):
        result = subprocess.CompletedProcess(["scp"], 255, "", "Permission denied (tailscale).")
        hint = train_cli.scp_hint("rbpi@pi", result)
        self.assertIn("run `ssh rbpi@pi` once to sign in", hint)


class ConfigTests(unittest.TestCase):
    def test_volume_up_uses_its_committed_config_and_lists(self):
        config = train_cli.load_phrase_config("volume-up", None)
        self.assertEqual(config["count"], 6000)
        self.assertEqual(config["test_count"], 1200)
        self.assertEqual(config["steps"], 30000)
        lists = train_cli.phrase_lists("volume-up", config)
        self.assertEqual(lists["positives"], ["volume up"])
        self.assertIn("volume down", lists["negatives"])
        self.assertIn("volume", lists["near_misses"])
        self.assertTrue(lists["ordinary"])

    def test_training_config_bounds_generation_to_the_existing_clips(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            counts = {"positive_train": 5000, "negative_train": 4000, "positive_test": 1000, "negative_test": 900}
            path = train_cli.write_training_config(work, "volume_up", "volume up", counts, 1234)
            text = path.read_text()
            self.assertIn('model_name: "volume_up"', text)
            self.assertIn('- "volume up"', text)
            self.assertIn("n_samples: 4000", text)
            self.assertIn("n_samples_val: 900", text)
            self.assertIn("steps: 1234", text)
            self.assertIn("ACAV100M_sample", text)

    def test_speeds_map_onto_generate_clips_factors(self):
        factors, low, high = train_cli.parse_speeds("0.8-1.2")
        self.assertEqual(factors, list(train_cli.SPEED_FACTORS))
        self.assertEqual((low, high), (0.8, 1.2))
        with self.assertRaises(train_cli.CLIError):
            train_cli.parse_speeds("1.2-0.8")
        with self.assertRaises(train_cli.CLIError):
            train_cli.parse_speeds("0.91-0.92")
        with self.assertRaises(train_cli.CLIError):
            train_cli.parse_speeds("fast")

    def test_voice_names_cannot_be_paths(self):
        with self.assertRaises(train_cli.CLIError):
            train_cli.parse_voices("../../etc/passwd", train_cli.DEFAULT_VOICES)
        self.assertEqual(train_cli.parse_voices("all", ["alba"]), ["alba"])

    def test_new_clips_invalidate_the_feature_files(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            directory = work / "clips" / "volume_up"
            directory.mkdir(parents=True)
            for name in ("positive_features_train.npy", "negative_features_train.npy", "unrelated.npy"):
                (directory / name).write_bytes(b"x")
            out = io.StringIO()
            with redirect_stdout(out):
                train_cli.invalidate_features(work, "volume_up")
            self.assertFalse((directory / "positive_features_train.npy").exists())
            self.assertFalse((directory / "negative_features_train.npy").exists())
            self.assertTrue((directory / "unrelated.npy").exists())
            self.assertIn("feature files", out.getvalue())

    def test_real_stamp_changes_with_the_set_and_weight(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            write_wav(work / "real" / "positive" / "real_p_00001.wav")
            first = train_cli.real_stamp(work, 1)
            self.assertEqual(first, train_cli.real_stamp(work, 1))
            self.assertNotEqual(first, train_cli.real_stamp(work, 2))
            write_wav(work / "real" / "positive" / "real_p_00002.wav", seconds=1.1)
            self.assertNotEqual(first, train_cli.real_stamp(work, 1))


class DryRunTests(unittest.TestCase):
    def test_synth_dry_run_prints_without_writing_clips(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            (work / "venv-tts" / "bin").mkdir(parents=True)
            (work / "venv-tts" / "bin" / "python").write_text("#!/bin/sh\n")
            args = train_cli.build_parser().parse_args(
                ["synth", "--phrase", "volume up", "--count", "40", "--test-count", "8",
                 "--negative-count", "40", "--workers", "1", "--dry-run", "--work", str(work)]
            )
            out = io.StringIO()
            with redirect_stdout(out):
                self.assertEqual(args.func(args), 0)
            self.assertIn("generate_clips.py", out.getvalue())
            self.assertIn("--count 40", out.getvalue())
            self.assertEqual(list((work / "clips").glob("volume-up/*")), [])
            self.assertTrue((work / "phrases" / "volume_up" / "positives.txt").exists())
            self.assertTrue((work / "phrases" / "volume_up" / "near_misses.txt").exists())

    def test_train_dry_run_writes_the_config_and_runs_no_gpu(self):
        with tempfile.TemporaryDirectory() as tmp:
            work = Path(tmp)
            for path in ("venv/bin/python", "venv-convert/bin/python"):
                target = work / path
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_text("#!/bin/sh\n")
            (work / "openWakeWord").mkdir()
            (work / "data" / "mit_rirs" / "16khz").mkdir(parents=True)
            (work / "background").mkdir()
            (work / "data" / "acav_subset.npy").write_bytes(b"x")
            (work / "data" / "validation_set_features.npy").write_bytes(b"x")
            for kind in ("positive_train", "negative_train", "positive_test", "negative_test"):
                directory = work / "clips" / "clanker" / kind
                directory.mkdir(parents=True)
                write_wav(directory / "clip.wav", seconds=0.5)
            args = train_cli.build_parser().parse_args(
                ["train", "--phrase", "clanker", "--steps", "42", "--dry-run", "--work", str(work)]
            )
            out = io.StringIO()
            with redirect_stdout(out):
                self.assertEqual(args.func(args), 0)
            config = (work / "clanker.yml").read_text()
            self.assertIn("steps: 42", config)
            self.assertIn("n_samples: 1", config)
            self.assertIn("run.sh", out.getvalue())
            self.assertFalse((work / "clips" / "clanker" / "positive_features_train.npy").exists())


class InstallTests(unittest.TestCase):
    def test_desktop_install_prints_the_redeploy_command(self):
        with tempfile.TemporaryDirectory() as tmp:
            models = Path(tmp)
            (models / "volume_up.tflite").write_bytes(b"tflite")
            args = train_cli.build_parser().parse_args(["install", "--phrase", "volume up"])
            out = io.StringIO()
            with mock.patch.object(train_cli, "MODELS", models), redirect_stdout(out):
                self.assertEqual(args.func(args), 0)
            text = out.getvalue()
            self.assertIn("wakeword/models/volume_up.tflite", text)
            self.assertIn("scripts/deploy.sh", text)
            self.assertIn("WAKEWORD_MODEL=volume_up", text)

    def test_missing_model_is_a_clear_refusal(self):
        with tempfile.TemporaryDirectory() as tmp:
            args = train_cli.build_parser().parse_args(["install", "--phrase", "clanker"])
            with mock.patch.object(train_cli, "MODELS", Path(tmp)), self.assertRaises(train_cli.CLIError):
                args.func(args)

    def test_push_dry_run_uses_batchmode(self):
        with tempfile.TemporaryDirectory() as tmp:
            models = Path(tmp)
            (models / "clanker.tflite").write_bytes(b"tflite")
            args = train_cli.build_parser().parse_args(
                ["install", "--phrase", "clanker", "--to", "pi", "--dry-run"]
            )
            out = io.StringIO()
            with mock.patch.object(train_cli, "MODELS", models), redirect_stdout(out):
                self.assertEqual(args.func(args), 0)
            self.assertIn("scp -o BatchMode=yes", out.getvalue())


if __name__ == "__main__":
    unittest.main()
