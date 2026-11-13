#!/usr/bin/env python3
"""Desktop CLI for one wake phrase: import, synth, train, eval, install, status.

This is a thin orchestrator over ``wakeword/training/run.sh`` and the scripts
beside it. It does not fork the training pipeline: clips are Pocket TTS WAVs,
the model is openWakeWord, and the owner's takes are the same
``real/positive/real_p_00001.wav`` files ``record.py`` writes.

Usage:
  wakeword/train import bt.tar.gz | ./bundle-dir | rbpi@pi:path
  wakeword/train synth --phrase "volume up" --count 5000
  wakeword/train train --phrase "volume up" --steps 20000
  wakeword/train eval --phrase "volume up"
  wakeword/train install --phrase "volume up" [--to pi]
  wakeword/train status
  wakeword/train all --phrase "volume up" --from bt.tar.gz

Every phrase has a work directory (default
``~/.cache/clanka/wakeword-<slug>``); nothing is uploaded and no recording is
written into the checkout. ``--dry-run`` prints the commands without running
the GPU stages.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import wave
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
TRAIN = Path(__file__).resolve().parent
MODELS = REPO / "wakeword" / "models"
RUN_SH = TRAIN / "run.sh"
WORK_ROOT = Path(os.environ.get("WORK_ROOT", Path.home() / ".cache" / "clanka"))

SAMPLE_RATE = 16000
CHANNELS = 1
SAMPLE_WIDTH = 2
MIN_POSITIVES = 5
MAX_TAKES = 100
MIN_SECONDS = 0.20
MAX_SECONDS = 2.0
MAX_TAKE_BYTES = 512 * 1024

# Kept in step with run.sh. A phrase YAML may override either list.
DEFAULT_VOICES = [
    "alba", "anna", "azelma", "bill_boerst", "charles", "cosette", "daan",
    "eponine", "estelle", "eve", "fantine", "george", "giovanni", "jane",
    "javert", "jean", "juergen", "lola", "marius", "mary", "michael", "paul",
    "peter_yearsley", "rafael", "vera",
]
DEFAULT_HELD_OUT = ["caro_davy", "stuart_bell"]
# generate_clips.py only knows these factors; a range is filtered onto them.
SPEED_FACTORS = (0.9, 0.95, 1.05, 1.1)

DEFAULT_REMOTE = os.environ.get("CLANKA_PI", "rbpi@pi")
REMOTE_MODEL_DIR = os.environ.get("CLANKA_PI_MODEL_DIR", "clanka-bot/wakeword/models")
SIGN_IN_HINT = "run `ssh {remote}` once to sign in"
BUILTIN_PHRASES = {"clanker": "clanker", "volume-up": "volume up", "volume-down": "volume down"}


class CLIError(Exception):
    """A refusal with a message meant for the owner, not a traceback."""


# ---------- phrases ----------


def slug_for(phrase: str) -> str:
    """The directory/bundle name for a phrase. Strict on purpose: it is a path."""
    lower = phrase.strip().lower()
    if re.search(r"[/\\<>:\"|?*.]", lower):
        raise CLIError(f"{phrase!r} contains a character that is not safe in a file name")
    slug = re.sub(r"[^a-z0-9]+", "-", lower).strip("-")
    if not valid_slug(slug):
        raise CLIError(
            "the phrase must be 1–4 words of letters and digits "
            f"(for example 'clanker' or 'volume up'), got {phrase!r}"
        )
    return slug


def valid_slug(slug: str) -> bool:
    return (
        1 <= len(slug) <= 40
        and bool(re.fullmatch(r"[a-z0-9]+(?:-[a-z0-9]+){0,3}", slug))
        and any(c.isalpha() for c in slug)
    )


def phrase_text(slug: str) -> str:
    return slug.replace("-", " ")


def phrase_key(slug: str) -> str:
    """The openWakeWord model name / run.sh PHRASE for a Dashboard slug."""
    return slug.replace("-", "_")


def slug_from_key(key: str) -> str:
    return key.replace("_", "-")


def resolve_phrase(value: str | None, config: dict | None = None) -> tuple[str, str]:
    """Return (slug, phrase text) from a slug, a phrase, or a manifest."""
    if value:
        if valid_slug(value) and value not in BUILTIN_PHRASES:
            # A slug and the phrase with spaces are both accepted; spaces make
            # the wrong path, so try the slug shape first.
            if value.lower() == value:
                return value, phrase_text(value)
        slug = slug_for(value)
        return slug, phrase_text(slug)
    if config and config.get("phrase"):
        slug = slug_for(str(config["phrase"]))
        return slug, phrase_text(slug)
    return "clanker", "clanker"


def work_dir(slug: str, override: str | None) -> Path:
    if override:
        return Path(override).expanduser()
    return WORK_ROOT / f"wakeword-{slug}"


# ---------- phrase config ----------


def load_phrase_config(slug: str, path: str | None) -> dict:
    """Read the optional per-phrase YAML, then fill every default."""
    config: dict = {}
    candidate = Path(path).expanduser() if path else TRAIN / "phrases" / f"{slug}.yml"
    if candidate.exists():
        try:
            import yaml  # noqa: PLC0415 - optional until a phrase file is used
        except ImportError as error:  # pragma: no cover - only on a bare desktop
            raise CLIError("a phrase YAML needs PyYAML; install it or use flags only") from error
        try:
            loaded = yaml.safe_load(candidate.read_text())
        except Exception as error:  # noqa: BLE001 - a config typo is the owner's message
            raise CLIError(f"could not read {candidate}: {error}") from error
        if loaded is not None and not isinstance(loaded, dict):
            raise CLIError(f"{candidate} must be a mapping")
        config = loaded or {}
    phrase = str(config.get("phrase") or phrase_text(slug))
    config["phrase"] = phrase
    committed = committed_training_config(slug)
    trained = training_values(committed) if committed else {}
    config.setdefault("count", trained.get("n_samples", 10000))
    config.setdefault("test_count", trained.get("n_samples_val", max(1, int(config["count"]) // 5)))
    config.setdefault("negative_count", int(config["count"]))
    config.setdefault("voices", list(DEFAULT_VOICES))
    config.setdefault("held_out", list(DEFAULT_HELD_OUT))
    config.setdefault("speeds", "0.8-1.2")
    config.setdefault("speed_probability", 0.25)
    config.setdefault("steps", trained.get("steps", 50000))
    config.setdefault("real_weight", 1)
    config.setdefault("workers", 4)
    config.setdefault("seed", 1)
    return config


def committed_training_config(slug: str) -> Path | None:
    """The checked-in <phrase>.yml for a slug, when one exists."""
    key = phrase_key(slug)
    for candidate in (TRAIN / f"{key}.yml", TRAIN / f"{slug}.yml"):
        if candidate.exists():
            return candidate
    return None


def training_values(path: Path) -> dict[str, int]:
    """Read the three count/step keys without requiring PyYAML."""
    values: dict[str, int] = {}
    for line in path.read_text().splitlines():
        match = re.match(r"^(n_samples|n_samples_val|steps):\s*(\d+)\s*$", line)
        if match:
            values[match.group(1)] = int(match.group(2))
    return values


def phrase_variants(phrase: str) -> list[str]:
    """A handful of ways to say the phrase, for TTS and for near misses."""
    variants = [phrase, phrase.capitalize(), f"{phrase}.", f"{phrase}!", f"{phrase}?"]
    return list(dict.fromkeys(variants))


def generated_near_misses(phrase: str) -> list[str]:
    """Near misses for a phrase with no curated list (clanker has one)."""
    words = phrase.split()
    out = [phrase + "s", f"the {phrase}", f"{phrase} please", f"{phrase} now"]
    if len(words) > 1:
        out += [" ".join(words[:-1]), " ".join(words[1:]), words[-1]]
        out += [f"{words[0]} {words[-1]}", f"{words[0]} a {words[-1]}"]
    if len(words) == 2:
        out += [f"{words[0]} {words[1]}er", f"{words[0]} {words[1][:-1]}"]
    if phrase == "volume up":
        out += ["volume down", "turn up the volume", "the volume is up"]
    if phrase == "volume down":
        out += ["volume up", "turn down the volume", "the volume is down"]
    out += ["clank", "clanker", "okay google", "hey google"]
    return list(dict.fromkeys(p for p in out if p and p != phrase))


def phrase_lists(slug: str, config: dict) -> dict[str, list[str]]:
    """Positive and negative text for TTS, preferring the checked-in phrase set.

    #114 puts curated lists under training/phrases/<key>/; clanker keeps the
    original top-level files. A brand-new phrase gets generated variants and
    near misses, and a per-phrase YAML may override either.
    """
    phrase = config["phrase"]
    key = phrase_key(slug)
    phrase_dir = TRAIN / "phrases" / key

    def curated(name: str, fallback: Path | None = None) -> Path | None:
        specific = phrase_dir / f"{name}.txt"
        if specific.exists():
            return specific
        if fallback is not None and fallback.exists():
            return fallback
        return None

    positives = config.get("positives")
    negatives = config.get("negatives")
    near_misses = config.get("near_misses")
    if not positives:
        path = curated("positives", TRAIN / "positives.txt" if slug == "clanker" else None)
        positives = read_lines(path) if path else phrase_variants(phrase)
    if not negatives:
        path = curated("negative_phrases", TRAIN / "negative_phrases.txt" if slug == "clanker" else None)
        negatives = read_lines(path) if path else generated_near_misses(phrase)
    if not near_misses:
        path = curated("near_misses", TRAIN / "near_misses.txt" if slug == "clanker" else None)
        near_misses = read_lines(path) if path else generated_near_misses(phrase)
    ordinary_path = curated("ordinary_phrases", TRAIN / "ordinary_phrases.txt")
    ordinary = read_lines(ordinary_path) if ordinary_path else read_lines(TRAIN / "ordinary_phrases.txt")
    return {
        "positives": list(dict.fromkeys(positives)),
        "negatives": list(dict.fromkeys(negatives)),
        "near_misses": list(dict.fromkeys(near_misses)),
        "ordinary": list(dict.fromkeys(ordinary)),
    }


def read_lines(path: Path) -> list[str]:
    if not path.exists():
        return []
    return [line.strip() for line in path.read_text().splitlines() if line.strip() and not line.startswith("#")]


# ---------- tools ----------


def run(cmd: list[str], log: Path | None = None, env: dict | None = None, dry_run: bool = False) -> int:
    print("+ " + shlex.join(cmd), flush=True)
    if dry_run:
        return 0
    merged = os.environ.copy()
    if env:
        merged.update({k: str(v) for k, v in env.items()})
    if log is None:
        return subprocess.call(cmd, env=merged)
    log.parent.mkdir(parents=True, exist_ok=True)
    with log.open("a") as handle:
        handle.write(f"\n$ {shlex.join(cmd)}\n")
        handle.flush()
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, env=merged)
        assert proc.stdout is not None
        for line in proc.stdout:
            print(line, end="", flush=True)
            handle.write(line)
        return proc.wait()


def run_sh(stage: str, work: Path, key: str, log: Path | None, dry_run: bool) -> int:
    return run(["bash", str(RUN_SH), stage], log=log, env={"WORK": str(work), "PHRASE": key}, dry_run=dry_run)


def require_venv(work: Path, name: str) -> None:
    if not (work / name / "bin" / "python").exists():
        raise CLIError(
            f"{work / name} is missing; run `WORK={work} wakeword/training/run.sh setup` once "
            "(and `run.sh assets` for the training features)"
        )


def scp_hint(remote: str, result: subprocess.CompletedProcess) -> str:
    text = (result.stderr or "") + (result.stdout or "")
    if re.search(r"denied|authentication|batch mode|permission", text, re.I):
        return "\n" + SIGN_IN_HINT.format(remote=remote)
    return ""


# ---------- WAV validation ----------


def wav_pcm(path: Path) -> tuple[int, bytes]:
    """Validate one training clip and return (frames, PCM bytes).

    The PCM is the dedupe key: two files with different headers but the same
    audio are the same take.
    """
    try:
        with wave.open(str(path), "rb") as handle:
            channels = handle.getnchannels()
            width = handle.getsampwidth()
            rate = handle.getframerate()
            frames = handle.getnframes()
            pcm = handle.readframes(frames)
    except (wave.Error, EOFError) as error:
        raise CLIError(f"{path.name}: not a readable WAV ({error})") from error
    if channels != CHANNELS:
        raise CLIError(f"{path.name}: {channels} channels; training needs mono")
    if width != SAMPLE_WIDTH:
        raise CLIError(f"{path.name}: {width * 8}-bit; training needs 16-bit")
    if rate != SAMPLE_RATE:
        raise CLIError(f"{path.name}: {rate} Hz; training needs {SAMPLE_RATE} Hz")
    seconds = frames / SAMPLE_RATE
    if seconds < MIN_SECONDS:
        raise CLIError(f"{path.name}: {seconds:.2f} s is shorter than {MIN_SECONDS:.1f} s")
    if seconds > MAX_SECONDS:
        raise CLIError(f"{path.name}: {seconds:.2f} s is longer than {MAX_SECONDS:.1f} s")
    if path.stat().st_size > MAX_TAKE_BYTES:
        raise CLIError(f"{path.name}: larger than {MAX_TAKE_BYTES // 1024} KB")
    return frames, pcm


def pcm_digest(pcm: bytes) -> str:
    return hashlib.sha256(pcm).hexdigest()


# ---------- bundle input ----------


def safe_tar_members(archive: tarfile.TarFile) -> list[tarfile.TarInfo]:
    members = archive.getmembers()
    if len(members) > 400:
        raise CLIError("the bundle has too many entries")
    total = 0
    for member in members:
        name = member.name
        if name.startswith("/") or ".." in Path(name).parts:
            raise CLIError(f"the bundle contains an unsafe path: {name}")
        if member.issym() or member.islnk():
            raise CLIError(f"the bundle contains a link: {name}")
        if not member.isfile():
            continue
        if member.size > MAX_TAKE_BYTES:
            raise CLIError(f"{name} is larger than one take")
        total += member.size
        if total > 64 << 20:
            raise CLIError("the bundle is larger than 64 MB")
        parts = Path(name).parts
        if parts[-1] == "manifest.json" and len(parts) <= 2:
            continue
        if parts[-3:-2] != ("real",) or parts[-2] not in ("positive", "negative") or parts[-1] in ("", ".", ".."):
            raise CLIError(f"the bundle has an unexpected entry: {name}")
    return [m for m in members if m.isfile()]


def extract_bundle(path: Path, dest: Path) -> None:
    try:
        archive = tarfile.open(path, "r:*")
    except tarfile.TarError as error:
        raise CLIError(f"could not read {path.name}: {error}") from error
    with archive:
        for member in safe_tar_members(archive):
            target = dest / member.name
            target.parent.mkdir(parents=True, exist_ok=True)
            source = archive.extractfile(member)
            if source is None:
                continue
            with target.open("wb") as handle:
                shutil.copyfileobj(source, handle)


def bundle_root(path: Path) -> Path:
    """Find real/ (and manifest.json) at the root or one directory down."""
    candidates = [path] + sorted(p for p in path.iterdir() if p.is_dir())
    for candidate in candidates:
        if (candidate / "real").is_dir() or (candidate / "manifest.json").is_file():
            return candidate
    raise CLIError("no real/positive or manifest.json in the bundle")


def read_manifest(root: Path) -> dict:
    path = root / "manifest.json"
    if not path.exists():
        return {}
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError) as error:
        raise CLIError(f"manifest.json is not valid JSON: {error}") from error
    if not isinstance(data, dict):
        raise CLIError("manifest.json must be an object")
    return data


def resolve_source(source: str, temp: Path, remote: str) -> Path:
    """Return a local bundle file or directory, fetching a remote one with scp."""
    if ":" not in source or source.startswith("/") or Path(source).exists():
        path = Path(source).expanduser()
        if not path.exists():
            raise CLIError(f"{source} does not exist")
        return path
    user_host, _, remote_path = source.partition(":")
    if not user_host or not remote_path:
        raise CLIError(f"could not parse the remote source {source!r}; use user@host:path")
    is_archive = remote_path.endswith(".tar.gz") or remote_path.endswith(".tgz")
    target = temp / ("bundle.tar.gz" if is_archive else "bundle")
    cmd = ["scp", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15"]
    if not is_archive:
        cmd.append("-r")
    cmd += [source, str(target)]
    print("+ " + shlex.join(cmd), flush=True)
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise CLIError(
            f"could not fetch {source} with scp"
            + scp_hint(user_host, result)
            + (f"\n{result.stderr.strip()}" if result.stderr.strip() else "")
        )
    return target


# ---------- import ----------


def import_bundle(
    source: str,
    slug: str | None,
    work_override: str | None,
    remote: str,
    dry_run: bool = False,
) -> dict:
    if slug is not None:
        slug = slug_for(slug)
    with tempfile.TemporaryDirectory(prefix="clanka-import-") as tmp:
        local = resolve_source(source, Path(tmp), remote)
        if local.is_dir():
            root = bundle_root(local)
        else:
            extracted = Path(tmp) / "extracted"
            extracted.mkdir()
            extract_bundle(local, extracted)
            root = bundle_root(extracted)

        manifest = read_manifest(root)
        if slug is None:
            if manifest.get("phrase"):
                slug = slug_for(str(manifest["phrase"]))
            elif manifest.get("slug"):
                slug = slug_for(str(manifest["slug"]))
            else:
                slug = "clanker"
        manifest_phrase = manifest.get("phrase") or manifest.get("slug")
        if manifest_phrase and slug_for(str(manifest_phrase)) != slug:
            raise CLIError(
                f"the bundle is for {manifest_phrase!r}, not {phrase_text(slug)!r}; "
                "pass --phrase only for a bundle that has no manifest"
            )

        groups: dict[str, list[Path]] = {"positive": [], "negative": []}
        for kind in groups:
            directory = root / "real" / kind
            if not directory.is_dir():
                continue
            for path in sorted(directory.glob("*.wav")):
                if path.is_symlink() or not path.is_file():
                    raise CLIError(f"{path.name}: not a regular file")
                groups[kind].append(path)
        positives = groups["positive"]
        negatives = groups["negative"]
        if not MIN_POSITIVES <= len(positives) <= MAX_TAKES:
            raise CLIError(f"the bundle has {len(positives)} positive takes; a set needs {MIN_POSITIVES}–{MAX_TAKES}")
        if len(negatives) > MAX_TAKES:
            raise CLIError(f"the bundle has {len(negatives)} negative takes; the limit is {MAX_TAKES}")
        work = work_dir(slug, work_override)
        work.mkdir(parents=True, exist_ok=True)

        # Validate every clip before copying one, so a bad bundle is rejected
        # whole rather than half-imported.
        validated: dict[str, list[tuple[Path, int, bytes]]] = {}
        for kind, paths in groups.items():
            validated[kind] = []
            for path in paths:
                frames, pcm = wav_pcm(path)
                validated[kind].append((path, frames, pcm))

        existing: dict[str, set[str]] = {}
        pending: dict[str, list[tuple[Path, str]]] = {}
        for kind in ("positive", "negative"):
            directory = work / "real" / kind
            directory.mkdir(parents=True, exist_ok=True)
            seen: set[str] = set()
            for path in sorted(directory.glob("*.wav")):
                try:
                    _, pcm = wav_pcm(path)
                except CLIError:
                    continue
                seen.add(pcm_digest(pcm))
            existing[kind] = seen
            fresh: list[tuple[Path, str]] = []
            for path, _, pcm in validated[kind]:
                digest = pcm_digest(pcm)
                if digest in seen:
                    continue
                seen.add(digest)
                fresh.append((path, digest))
            pending[kind] = fresh
            # Re-importing a set that is already present is a no-op even when
            # the work directory is at the limit; only new takes count.
            if len(seen) > MAX_TAKES:
                raise CLIError(
                    f"importing would put {len(seen)} {kind} takes in {work}; "
                    f"keep at most {MAX_TAKES} (move the old set aside or use a new --work)"
                )

        report = {"phrase": phrase_text(slug), "slug": slug, "work": str(work), "source": source, "imported": {}, "skipped": {}}
        for kind, prefix in (("positive", "real_p_"), ("negative", "real_n_")):
            directory = work / "real" / kind
            numbers = [int(m.group(1)) for p in directory.glob(f"{prefix}*.wav") if (m := re.search(r"(\d+)$", p.stem))]
            highest = max(numbers, default=0)
            imported = 0
            for path, _ in pending[kind]:
                highest += 1
                target = directory / f"{prefix}{highest:05d}.wav"
                if not dry_run:
                    shutil.copyfile(path, target)
                imported += 1
            report["imported"][kind] = imported
            report["skipped"][kind] = len(validated[kind]) - imported
        if not dry_run:
            meta = {
                "phrase": phrase_text(slug),
                "slug": slug,
                "imported_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "source": source,
                "imported": report["imported"],
                "skipped": report["skipped"],
                "manifest": manifest,
            }
            (work / "import.json").write_text(json.dumps(meta, indent=2) + "\n")
        return report


# ---------- synth ----------


def parse_speeds(value: str) -> tuple[list[float], float, float]:
    match = re.fullmatch(r"\s*(\d+(?:\.\d+)?)\s*-\s*(\d+(?:\.\d+)?)\s*", value)
    if not match:
        raise CLIError(f"--speeds needs a range such as 0.8-1.2, got {value!r}")
    low, high = float(match.group(1)), float(match.group(2))
    if not 0.5 <= low < high <= 2.0:
        raise CLIError("--speeds must be a range inside 0.5–2.0 with the low end first")
    factors = [factor for factor in SPEED_FACTORS if low <= factor <= high]
    if not factors:
        raise CLIError(
            f"no supported speed factor in {value}; generate_clips.py supports "
            f"{', '.join(f'{f:g}' for f in SPEED_FACTORS)}"
        )
    return factors, low, high


def parse_voices(value: str | list, allowed: list[str]) -> list[str]:
    if isinstance(value, list):
        voices = [str(v).strip() for v in value if str(v).strip()]
    elif value.strip() == "all":
        voices = list(allowed)
    else:
        voices = [v.strip() for v in value.split(",") if v.strip()]
    if not voices:
        raise CLIError("no voices selected")
    for voice in voices:
        if not re.fullmatch(r"[a-z0-9_]+", voice):
            raise CLIError(f"voice {voice!r} is not a Pocket TTS voice name")
    return voices


def write_list(path: Path, lines: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n")


def synth(args: argparse.Namespace, slug: str, config: dict, work: Path) -> None:
    require_venv(work, "venv-tts")
    key = phrase_key(slug)
    lists = phrase_lists(slug, config)
    phrase_dir = work / "phrases" / key
    write_list(phrase_dir / "positives.txt", lists["positives"])
    write_list(phrase_dir / "negatives.txt", lists["negatives"])
    write_list(phrase_dir / "near_misses.txt", lists["near_misses"])
    write_list(phrase_dir / "ordinary.txt", lists["ordinary"])

    voices = parse_voices(args.voices if args.voices is not None else config["voices"], DEFAULT_VOICES)
    held_out = parse_voices(args.held_out if args.held_out is not None else config["held_out"], DEFAULT_VOICES + DEFAULT_HELD_OUT)
    overlap = sorted(set(voices) & set(held_out))
    if overlap:
        raise CLIError(f"held-out voice(s) are also in training: {', '.join(overlap)}")
    _, low, high = parse_speeds(args.speeds if args.speeds is not None else str(config["speeds"]))
    speed_probability = args.speed_probability if args.speed_probability is not None else float(config["speed_probability"])
    count = args.count if args.count is not None else int(config["count"])
    test_count = args.test_count if args.test_count is not None else int(config["test_count"])
    negative_count = args.negative_count if args.negative_count is not None else int(config["negative_count"])
    if count < 1 or test_count < 1 or negative_count < 1:
        raise CLIError("counts must be positive")
    if count > 200000:
        raise CLIError("--count above 200,000 is not a sensible synthetic set")
    workers = args.workers if args.workers is not None else int(config["workers"])
    seed = args.seed if args.seed is not None else int(config["seed"])
    device = args.device
    if device == "auto":
        device = "cuda" if shutil.which("nvidia-smi") else "cpu"
    log = work / "logs" / f"synth-{time.strftime('%Y%m%d-%H%M%S')}.log"
    print(f"phrase {phrase_text(slug)!r}: {count} positives, {test_count} test, "
          f"{negative_count} negatives, voices {len(voices)}, speeds {low:g}-{high:g}, device {device}")

    jobs = [
        ("positive_train", phrase_dir / "positives.txt", count, 0.25, seed),
        ("positive_test", phrase_dir / "positives.txt", test_count, 0.25, seed + 1),
        ("negative_train", phrase_dir / "negatives.txt", negative_count, 0.15, seed + 2),
        ("negative_test", phrase_dir / "negatives.txt", test_count, 0.15, seed + 3),
    ]
    for name, phrases, job_count, probability, job_seed in jobs:
        cmd = [
            str(work / "venv-tts" / "bin" / "python"), str(TRAIN / "generate_clips.py"),
            "--out-dir", str(work / "clips" / key / name),
            "--phrases", str(phrases),
            "--voices", ",".join(voices),
            "--count", str(job_count),
            "--workers", str(workers),
            "--speed-probability", str(speed_probability if name.startswith("positive") else 0.15),
            "--device", device,
            "--seed", str(job_seed),
        ]
        if run(cmd, log=log, dry_run=args.dry_run) != 0:
            raise CLIError(f"clip generation failed; see {log}")

    acceptance = work / "clips" / key / "acceptance"
    acceptance_jobs = [
        ("positive", phrase_dir / "positives.txt", 60, 0.5, seed + 6),
        ("negative", phrase_dir / "near_misses.txt", 80, 0.5, seed + 10),
        ("speech", phrase_dir / "ordinary.txt", 40, 0.3, seed + 12),
    ]
    for name, phrases, job_count, probability, job_seed in acceptance_jobs:
        cmd = [
            str(work / "venv-tts" / "bin" / "python"), str(TRAIN / "generate_clips.py"),
            "--out-dir", str(acceptance / name),
            "--phrases", str(phrases),
            "--voices", ",".join(held_out),
            "--count", str(job_count),
            "--workers", "2",
            "--speed-probability", str(probability),
            "--device", device,
            "--seed", str(job_seed),
        ]
        if run(cmd, log=log, dry_run=args.dry_run) != 0:
            raise CLIError(f"acceptance clip generation failed; see {log}")
    if not args.dry_run:
        invalidate_features(work, key)
    print(f"synthetic clips are under {work / 'clips' / key} (log: {log})")


def invalidate_features(work: Path, key: str) -> None:
    """New clips make the extracted features stale; prep_real.py does the same."""
    removed = False
    for name in (
        "positive_features_train.npy",
        "negative_features_train.npy",
        "positive_features_test.npy",
        "negative_features_test.npy",
    ):
        path = work / "clips" / key / name
        if path.exists():
            path.unlink()
            removed = True
    if removed:
        print("removed the old feature files; the next train run re-extracts from the new clips")


# ---------- train ----------


def write_training_config(work: Path, key: str, phrase: str, counts: dict, steps: int) -> Path:
    """Write the per-run openWakeWord YAML.

    When the phrase has a checked-in config (clanker, volume_up, volume_down)
    it is the template: its negative weighting, target threshold and other
    tuned values stay, while n_samples/n_samples_val are aligned with the clips
    actually generated and steps with --steps. openWakeWord only synthesizes
    when a count is below 95% of n_samples, and the stub Piper generator raises,
    so equal-or-lower values are what keep the run to its own clips.

    A new phrase gets the same defaults as the checked-in clanker.yml.
    """
    positive_train = counts["positive_train"]
    negative_train = counts["negative_train"]
    positive_test = counts["positive_test"]
    negative_test = counts["negative_test"]
    n_samples = min(positive_train, negative_train)
    n_samples_val = min(positive_test, negative_test)
    if n_samples < 1 or n_samples_val < 1:
        raise CLIError("training needs both positive and negative clips; run synth first")
    template = TRAIN / f"{key}.yml"
    if template.exists():
        text = template.read_text()
        text = re.sub(r"(?m)^n_samples:\s*\d+\s*$", f"n_samples: {n_samples}", text)
        text = re.sub(r"(?m)^n_samples_val:\s*\d+\s*$", f"n_samples_val: {n_samples_val}", text)
        text = re.sub(r"(?m)^steps:\s*\d+\s*$", f"steps: {steps}", text)
        text = re.sub(r'(?m)^model_name:\s*"[^"]*"\s*$', f'model_name: "{key}"', text)
        text = re.sub(r'(?m)^(target_phrase:\s*\n(?:\s+-\s+"[^"]*"\s*\n)+)', f'target_phrase:\n  - "{phrase}"\n', text)
    else:
        text = f"""# Generated by wakeword/train for {phrase!r}; edit the phrase YAML or flags.
model_name: "{key}"

target_phrase:
  - "{phrase}"

custom_negative_phrases: []

n_samples: {n_samples}
n_samples_val: {n_samples_val}
tts_batch_size: 1
augmentation_batch_size: 16

piper_sample_generator_path: "./stub_piper"

output_dir: "./clips"

rir_paths:
  - "./data/mit_rirs/16khz"

background_paths:
  - "./background"

background_paths_duplication_rate:
  - 1

false_positive_validation_data_path: "./data/validation_set_features.npy"

augmentation_rounds: 1

feature_data_files:
  "ACAV100M_sample": "./data/acav_subset.npy"

batch_n_per_class:
  "ACAV100M_sample": 1024
  "adversarial_negative": 50
  "positive": 50

model_type: "dnn"
layer_size: 32

steps: {steps}
max_negative_weight: 1500
target_false_positives_per_hour: 0.2
"""
    path = work / f"{key}.yml"
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    return path


def clip_counts(work: Path, key: str) -> dict:
    base = work / "clips" / key
    return {
        "positive_train": len(list((base / "positive_train").glob("*.wav"))),
        "positive_test": len(list((base / "positive_test").glob("*.wav"))),
        "negative_train": len(list((base / "negative_train").glob("*.wav"))),
        "negative_test": len(list((base / "negative_test").glob("*.wav"))),
    }


def real_stamp(work: Path, weight: int) -> str:
    pieces = []
    for kind in ("positive", "negative"):
        directory = work / "real" / kind
        if not directory.is_dir():
            continue
        for path in sorted(directory.glob("*.wav")):
            stat = path.stat()
            pieces.append(f"{kind}/{path.name}:{stat.st_size}:{stat.st_mtime_ns}")
    pieces.append(f"weight:{weight}")
    return hashlib.sha256("\n".join(pieces).encode()).hexdigest() if pieces else ""


def prepare_real(work: Path, key: str, weight: int, log: Path, dry_run: bool) -> None:
    stamp = real_stamp(work, weight)
    if not stamp:
        print("no real takes in the work directory; training stays synthetic-only")
        return
    marker = work / "logs" / "real-prepared.json"
    features = work / "clips" / key / "positive_features_train.npy"
    if not dry_run and marker.exists() and features.exists():
        try:
            previous = json.loads(marker.read_text()).get("stamp")
        except (OSError, json.JSONDecodeError):
            previous = None
        if previous == stamp:
            print("real takes are unchanged since the last split; keeping the features")
            return
    if run_sh("real", work, key, log, dry_run) != 0:
        raise CLIError(f"the real take split failed; see {log}")
    if weight > 1 and not dry_run:
        for source in sorted((work / "clips" / key / "positive_train").glob("realp_*.wav")):
            data = source.read_bytes()
            for copy in range(2, weight + 1):
                (source.parent / f"realp_w{weight}_{copy}_{source.name[len('realp_'):]}").write_bytes(data)
        print(f"duplicated each real positive {weight}× in positive_train (openWakeWord has no per-clip weight)")
    if not dry_run:
        marker.parent.mkdir(parents=True, exist_ok=True)
        marker.write_text(json.dumps({"stamp": stamp, "weight": weight, "at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}, indent=2) + "\n")


def train(args: argparse.Namespace, slug: str, config: dict, work: Path) -> None:
    key = phrase_key(slug)
    require_venv(work, "venv")
    if not (work / "openWakeWord").is_dir():
        raise CLIError("openWakeWord is not set up; run `wakeword/training/run.sh setup`")
    for required in ("data/acav_subset.npy", "data/validation_set_features.npy", "background", "data/mit_rirs/16khz"):
        if not (work / required).exists():
            raise CLIError(f"{work / required} is missing; run `wakeword/training/run.sh assets`")
    counts = clip_counts(work, key)
    if counts["positive_train"] == 0 or counts["negative_train"] == 0:
        raise CLIError("no synthetic clips for this phrase; run `wakeword/train synth --phrase ...` first")
    steps = args.steps if args.steps is not None else int(config["steps"])
    if steps < 1:
        raise CLIError("--steps must be positive")
    weight = args.real_weight if args.real_weight is not None else int(config["real_weight"])
    if weight < 1 or weight > 10:
        raise CLIError("--real-weight must be between 1 and 10")
    log = work / "logs" / f"train-{time.strftime('%Y%m%d-%H%M%S')}.log"
    print(f"phrase {phrase_text(slug)!r}: {counts['positive_train']} positive and "
          f"{counts['negative_train']} negative training clips, {steps} steps, real weight {weight}")
    prepare_real(work, key, weight, log, args.dry_run)
    write_training_config(work, key, phrase_text(slug), counts, steps)
    if run_sh("train", work, key, log, args.dry_run) != 0:
        raise CLIError(f"training failed; see {log}")
    if run_sh("convert", work, key, log, args.dry_run) != 0:
        raise CLIError(f"the ONNX to TFLite conversion failed; see {log}")
    print(f"model: {MODELS / (key + '.tflite')} (log: {log})")


# ---------- eval ----------


def threshold_rates(scores: dict, threshold: float) -> dict:
    def rates(key: str) -> dict | None:
        values = scores.get(key)
        if not isinstance(values, dict) or not values:
            return None
        peaks = []
        for frames in values.values():
            peaks.append(max(frames) if frames else 0.0)
        return {"n": len(peaks), "rate": sum(1 for peak in peaks if peak > threshold) / len(peaks)}
    return {key: rates(key) for key in ("positives", "voices", "negatives", "near_misses", "speech", "real_positive", "real_negative")}


def eval_phrase(args: argparse.Namespace, slug: str, work: Path) -> None:
    key = phrase_key(slug)
    require_venv(work, "venv")
    model = MODELS / f"{key}.tflite"
    if not model.exists():
        raise CLIError(f"{model} does not exist yet; run train first")
    log = work / "logs" / f"eval-{time.strftime('%Y%m%d-%H%M%S')}.log"
    if run_sh("evaluate", work, key, log, args.dry_run) != 0:
        raise CLIError(f"evaluation failed; see {log}")
    if args.dry_run:
        return
    result_path = work / "logs" / f"eval-{key}.json"
    if not result_path.exists():
        raise CLIError(f"evaluation wrote no {result_path}; see {log}")
    scores = json.loads(result_path.read_text())
    threshold = args.threshold
    rates = threshold_rates(scores, threshold)
    print(f"\n{phrase_text(slug)!r} at threshold {threshold:.2f}:")
    for key, label in (
        ("voices", "recall, TTS voices held out of training"),
        ("positives", "recall, TTS training voices"),
        ("real_positive", "recall, real takes held out (~20%)"),
        ("negatives", "near-miss false accepts"),
        ("speech", "ordinary-speech false accepts"),
        ("real_negative", "real negative false accepts"),
    ):
        value = rates.get(key)
        print(f"  {label}: " + ("no clips" if value is None else f"{value['rate']:.1%} of {value['n']}"))
    events = scores.get("background_events_per_hour") or {}
    per_hour = events.get(f"{threshold:.2f}") if isinstance(events, dict) else None
    print(f"  background false accepts per hour: " + ("no background audio" if per_hour is None else f"{per_hour:.2f}"))
    print(f"  threshold: {threshold:.2f} (the service's WAKEWORD_THRESHOLD)")
    if rates.get("real_positive") is None:
        print("  note: no real takes were held out; record some with the Dashboard or record.py")


# ---------- install ----------


def install(args: argparse.Namespace, slug: str) -> None:
    key = phrase_key(slug)
    model = MODELS / f"{key}.tflite"
    if not model.exists():
        raise CLIError(f"{model} does not exist; run train (which converts the ONNX) first")
    if not args.to:
        print(f"model is in place: {model}")
        print("commit it, then redeploy on the Pi (or use install --to pi):")
        print("  git add wakeword/models/" + key + ".tflite && git commit -m \"feat(wakeword): add the " + key + " model\"")
        print("  git push && ssh " + args.remote + " 'cd ~/clanka-bot && scripts/deploy.sh'")
        print(f"then set WAKEWORD_MODEL={key} in .env and restart assistant")
        return
    remote = args.remote
    remote_path = f"{remote}:{args.remote_dir}/{key}.tflite"
    cmd = ["scp", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", str(model), remote_path]
    print("+ " + shlex.join(cmd), flush=True)
    if args.dry_run:
        return
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise CLIError(
            f"could not copy the model to {remote}"
            + scp_hint(remote, result)
            + (f"\n{result.stderr.strip()}" if result.stderr.strip() else "")
        )
    print(f"model copied to {remote_path}")
    print("rebuild and restart the wakeword service on the Pi (deploy.sh refuses a")
    print("dirty checkout, so this builds the copied file directly):")
    print(f"  ssh -o BatchMode=yes {remote} 'cd ~/clanka-bot && GIT_SHA=$(git rev-parse --short HEAD) docker compose build wakeword && docker compose up -d wakeword assistant'")
    print(f"then set WAKEWORD_MODEL={key} in .env and restart assistant")
    print("to make it durable, commit the model on the desktop and run scripts/deploy.sh on the Pi")


# ---------- status ----------


def eval_summary(work: Path, key: str, threshold: float = 0.5) -> str:
    path = work / "logs" / f"eval-{key}.json"
    if not path.exists():
        return "no eval yet"
    try:
        scores = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return "eval unreadable"
    rates = threshold_rates(scores, threshold)
    parts = []
    for key, label in (("voices", "tts"), ("real_positive", "real")):
        value = rates.get(key)
        if value:
            parts.append(f"{label} {value['rate']:.0%}")
    events = scores.get("background_events_per_hour") or {}
    if isinstance(events, dict) and events.get(f"{threshold:.2f}") is not None:
        parts.append(f"fa/h {events[f'{threshold:.2f}']:.2f}")
    return ", ".join(parts) or "eval empty"


def imported_phrase(work: Path) -> str | None:
    path = work / "import.json"
    if not path.exists():
        return None
    try:
        data = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return None
    value = data.get("slug") or data.get("phrase")
    return slug_for(str(value)) if value else None


def status(args: argparse.Namespace) -> None:
    roots = []
    only_slug = None
    if args.phrase:
        only_slug, _ = resolve_phrase(args.phrase)
        roots = [work_dir(only_slug, args.work)]
    else:
        roots = sorted(path for path in WORK_ROOT.glob("wakeword-*") if path.is_dir())
    if not roots:
        print(f"no work directories under {WORK_ROOT}")
    print(f"{'phrase':16} {'work':40} {'real +':>6} {'real -':>6} {'synth +':>7} {'synth -':>7} {'model':>6}  last run")
    seen: set[str] = set()
    for work in roots:
        raw = work.name.removeprefix("wakeword-").replace("_", "-")
        slug = only_slug or imported_phrase(work) or raw
        if not valid_slug(slug):
            slug = raw if valid_slug(raw) else slug
        key = phrase_key(slug)
        seen.add(slug)
        real_pos = len(list((work / "real" / "positive").glob("*.wav")))
        real_neg = len(list((work / "real" / "negative").glob("*.wav")))
        counts = clip_counts(work, key)
        model = (MODELS / f"{key}.tflite").exists()
        logs = sorted((work / "logs").glob("train-*.log"))
        last = logs[-1].name if logs else "never"
        display = phrase_text(slug) if valid_slug(slug) else raw
        print(f"{display:16} {str(work):40} {real_pos:6} {real_neg:6} "
              f"{counts['positive_train']:7} {counts['negative_train']:7} {'yes' if model else 'no':>6}  {last}")
        print(f"{'':16} eval: {eval_summary(work, key)}")
    # A model can exist without its work directory (checked in, or built on
    # another machine); list it so install/status cannot imply it is missing.
    for model in sorted(MODELS.glob("*.tflite")):
        slug = slug_from_key(model.stem)
        if slug not in seen:
            print(f"{phrase_text(slug):16} {'(no work directory)':40} {'':6} {'':6} {'':7} {'':7} {'yes':>6}")


# ---------- all ----------


def cmd_import(args: argparse.Namespace) -> int:
    slug = None
    if args.phrase:
        slug, _ = resolve_phrase(args.phrase)
    report = import_bundle(args.source, slug, args.work, args.remote, args.dry_run)
    print(f"imported into {report['work']}:")
    for kind in ("positive", "negative"):
        print(f"  {kind}: {report['imported'][kind]} imported, {report['skipped'][kind]} already present")
    return 0


def cmd_synth(args: argparse.Namespace) -> int:
    slug, _ = resolve_phrase(args.phrase)
    config = load_phrase_config(slug, args.config)
    if args.phrase and valid_slug(args.phrase):
        config["phrase"] = phrase_text(slug)
    work = work_dir(slug, args.work)
    synth(args, slug, config, work)
    return 0


def cmd_train(args: argparse.Namespace) -> int:
    slug, _ = resolve_phrase(args.phrase)
    config = load_phrase_config(slug, args.config)
    work = work_dir(slug, args.work)
    train(args, slug, config, work)
    return 0


def cmd_eval(args: argparse.Namespace) -> int:
    slug, _ = resolve_phrase(args.phrase)
    work = work_dir(slug, args.work)
    eval_phrase(args, slug, work)
    return 0


def cmd_install(args: argparse.Namespace) -> int:
    slug, _ = resolve_phrase(args.phrase)
    install(args, slug)
    return 0


def cmd_status(args: argparse.Namespace) -> int:
    status(args)
    return 0


def cmd_all(args: argparse.Namespace) -> int:
    if args.source:
        ns = argparse.Namespace(source=args.source, phrase=args.phrase, work=args.work, remote=args.remote, dry_run=args.dry_run)
        cmd_import(ns)
    slug, _ = resolve_phrase(args.phrase)
    config = load_phrase_config(slug, args.config)
    work = work_dir(slug, args.work)
    synth(args, slug, config, work)
    train(args, slug, config, work)
    eval_phrase(args, slug, work)
    print("next: wakeword/train install --phrase " + shlex.quote(phrase_text(slug)))
    return 0


# ---------- CLI ----------


def base_parser(sub: argparse.ArgumentParser) -> None:
    sub.add_argument("--phrase", help="phrase text or slug, for example 'volume up' or volume-up")
    sub.add_argument("--work", help="work directory (default ~/.cache/clanka/wakeword-<slug>)")
    sub.add_argument("--dry-run", action="store_true", help="print the commands without running them")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="wakeword/train",
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    commands = parser.add_subparsers(dest="command", required=True)

    imp = commands.add_parser("import", help="import a sample bundle (tar.gz, directory, or user@host:path)")
    imp.add_argument("source")
    imp.add_argument("--remote", default=DEFAULT_REMOTE, help=f"Tailscale SSH host for the sign-in hint (default {DEFAULT_REMOTE})")
    base_parser(imp)
    imp.set_defaults(func=cmd_import)

    synth = commands.add_parser("synth", help="generate synthetic positives and hard negatives with Pocket TTS")
    synth.add_argument("--count", type=int, help="synthetic positive training clips (default 10000)")
    synth.add_argument("--test-count", type=int, help="synthetic test clips per kind (default count/5)")
    synth.add_argument("--negative-count", type=int, help="synthetic negative training clips (default count)")
    synth.add_argument("--voices", help="'all' or a comma-separated Pocket TTS voice list")
    synth.add_argument("--held-out", help="comma-separated voices held out of training for acceptance")
    synth.add_argument("--speeds", help="speech-rate range inside 0.5–2.0 (default 0.8-1.2)")
    synth.add_argument("--speed-probability", type=float, help="fraction of clips stretched (default 0.25)")
    synth.add_argument("--workers", type=int, help="TTS workers (default 4)")
    synth.add_argument("--seed", type=int, help="base random seed")
    synth.add_argument("--device", choices=("auto", "cpu", "cuda"), default="auto")
    synth.add_argument("--config", help="per-phrase YAML (default wakeword/training/phrases/<slug>.yml)")
    base_parser(synth)
    synth.set_defaults(func=cmd_synth)

    train = commands.add_parser("train", help="augment clips, train, and convert the ONNX to TFLite")
    train.add_argument("--steps", type=int, help="training steps (default 50000)")
    train.add_argument("--real-weight", type=int, help="duplicate each real positive N times (default 1)")
    train.add_argument("--config", help="per-phrase YAML (default wakeword/training/phrases/<slug>.yml)")
    base_parser(train)
    train.set_defaults(func=cmd_train)

    ev = commands.add_parser("eval", help="score the trained model and report the real and TTS numbers")
    ev.add_argument("--threshold", type=float, default=0.5, help="deployed threshold to report (default 0.5)")
    base_parser(ev)
    ev.set_defaults(func=cmd_eval)

    inst = commands.add_parser("install", help="place or push the .tflite model and print the redeploy command")
    inst.add_argument("--to", choices=("pi",), help="copy the model to the Pi's checkout")
    inst.add_argument("--remote", default=DEFAULT_REMOTE, help=f"Pi SSH target (default {DEFAULT_REMOTE})")
    inst.add_argument("--remote-dir", default=REMOTE_MODEL_DIR, help=f"models directory relative to the Pi home (default {REMOTE_MODEL_DIR})")
    base_parser(inst)
    inst.set_defaults(func=cmd_install)

    st = commands.add_parser("status", help="list phrases, sample counts, models, and the last evaluation")
    base_parser(st)
    st.set_defaults(func=cmd_status)

    all_cmd = commands.add_parser("all", help="import (optional), synth, train, eval")
    all_cmd.add_argument("--from", dest="source", help="optional bundle to import first")
    all_cmd.add_argument("--remote", default=DEFAULT_REMOTE, help="Tailscale SSH host for import")
    all_cmd.add_argument("--config", help="per-phrase YAML")
    all_cmd.add_argument("--steps", type=int, help="training steps")
    all_cmd.add_argument("--real-weight", type=int, help="real-positive duplication")
    all_cmd.add_argument("--count", type=int, help="synthetic positive training clips")
    all_cmd.add_argument("--test-count", type=int, help="synthetic test clips per kind")
    all_cmd.add_argument("--negative-count", type=int, help="synthetic negative training clips")
    all_cmd.add_argument("--voices", help="'all' or a comma-separated voice list")
    all_cmd.add_argument("--held-out", help="voices held out of training")
    all_cmd.add_argument("--speeds", help="speech-rate range")
    all_cmd.add_argument("--speed-probability", type=float, help="fraction of clips stretched")
    all_cmd.add_argument("--workers", type=int, help="TTS workers")
    all_cmd.add_argument("--seed", type=int, help="base random seed")
    all_cmd.add_argument("--device", choices=("auto", "cpu", "cuda"), default="auto")
    all_cmd.add_argument("--threshold", type=float, default=0.5, help="deployed threshold to report")
    base_parser(all_cmd)
    all_cmd.set_defaults(func=cmd_all)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return args.func(args)
    except CLIError as error:
        print(f"wakeword/train: {error}", file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        print("interrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
