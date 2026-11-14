#!/usr/bin/env python3
"""Record and validate the #70 wake-over-music evidence matrix.

This is host-side bookkeeping only: it reads and writes JSON and never opens
the microphone, plays audio, or touches PulseAudio/PipeWire settings. A cell is
"measured" only after an operator runs the room protocol in
docs/spikes/wake-over-music.md and types the actual counts.

Statuses:
  UNMEASURED  valid file, at least one cell is still blank
  MEASURED    all six cells (three volumes x AEC off/on) have operator counts
  SYNTHETIC   a labelled fixture; valid math, never evidence
"""

from __future__ import annotations

import argparse
import json
import sys
from datetime import datetime
from pathlib import Path

SCHEMA = "wake-over-music/v1"
KIND_OPERATOR = "operator-measurement"
KIND_SYNTHETIC = "synthetic-fixture"
KINDS = (KIND_OPERATOR, KIND_SYNTHETIC)
AEC_STATES = ("off", "on")
COUNT_FIELDS = (
    "trials",
    "hits",
    "false_wakes",
    "exposure_min",
    "started_at",
    "ended_at",
)
STATUS_UNMEASURED = "UNMEASURED"
STATUS_MEASURED = "MEASURED"
STATUS_SYNTHETIC = "SYNTHETIC"


class InputError(ValueError):
    """An operator input or evidence file that must be refused."""


def is_int(value: object) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def is_number(value: object) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def parse_volumes(text: str) -> list[int]:
    """Parse the three distinct music-volume percentages for the grid."""
    parts = [part.strip() for part in text.split(",")]
    if len(parts) != 3:
        raise InputError("--volumes needs exactly three comma-separated integers")
    volumes: list[int] = []
    for part in parts:
        try:
            volume = int(part)
        except ValueError as exc:
            raise InputError(f"volume {part!r} is not an integer") from exc
        if not 1 <= volume <= 100:
            raise InputError(f"volume {volume} is outside 1..100")
        if volume in volumes:
            raise InputError(f"volume {volume} is repeated")
        volumes.append(volume)
    return sorted(volumes)


def blank_cell(volume: int, aec: str) -> dict:
    """One unmeasured grid cell; every count stays null until an operator run."""
    return {
        "volume": volume,
        "aec": aec,
        "trials": None,
        "hits": None,
        "false_wakes": None,
        "exposure_min": None,
        "started_at": None,
        "ended_at": None,
        "note": "",
    }


def new_document(volumes: list[int]) -> dict:
    """A blank six-cell template. It deliberately records no result."""
    return {
        "schema": SCHEMA,
        "kind": KIND_OPERATOR,
        "issue": 70,
        "wake_word": "okay_nabu",
        "host": {
            "device": "",
            "pipewire": "",
            "pipewire_pulse": "",
            "capture_source": "",
            "playback_sink": "",
            "aec_source": "",
            "aec_sink": "",
            "aec_method": "webrtc",
            "aec_args": "",
            "note": "",
        },
        "volumes": volumes,
        "targets": {"hit_rate": None, "false_wakes_per_hour": None},
        "cells": [blank_cell(volume, aec) for volume in volumes for aec in AEC_STATES],
        "decision": {
            "status": "pending",
            "max_volume": None,
            "note": "No decision until all six cells and an operator target exist.",
        },
    }


def cell_measured(cell: dict) -> bool:
    """True when the operator filled every count and timestamp in a cell."""
    return all(cell.get(field) is not None for field in COUNT_FIELDS)


def cell_partially_filled(cell: dict) -> bool:
    values = [cell.get(field) is not None for field in COUNT_FIELDS]
    return any(values) and not all(values)


def parse_timestamp(value: object, label: str) -> datetime:
    """Parse an ISO timestamp; a missing UTC offset makes an unattributable run."""
    if not isinstance(value, str) or not value.strip():
        raise InputError(f"{label} must be a non-empty ISO 8601 timestamp")
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError as exc:
        raise InputError(f"{label} {value!r} is not ISO 8601") from exc
    if parsed.tzinfo is None:
        raise InputError(f"{label} {value!r} needs a UTC offset, e.g. +01:00")
    return parsed


def validate_cell(cell: object, index: int, volumes: list[int]) -> list[str]:
    """Return every defect in one cell, or [] for blank/valid measured cells."""
    where = f"cell[{index}]"
    if not isinstance(cell, dict):
        return [f"{where} must be an object"]
    errors: list[str] = []
    volume = cell.get("volume")
    aec = cell.get("aec")
    if not is_int(volume) or volume not in volumes:
        errors.append(f"{where}.volume {volume!r} is not one of {volumes}")
    if aec not in AEC_STATES:
        errors.append(f"{where}.aec {aec!r} must be 'off' or 'on'")
    if cell_partially_filled(cell):
        errors.append(f"{where} is incomplete; fill every count or leave the cell blank")
        return errors
    if not cell_measured(cell):
        return errors

    trials = cell.get("trials")
    hits = cell.get("hits")
    false_wakes = cell.get("false_wakes")
    exposure_min = cell.get("exposure_min")
    if not is_int(trials) or trials < 1:
        errors.append(f"{where}.trials must be an integer >= 1")
    if not is_int(hits) or hits < 0:
        errors.append(f"{where}.hits must be an integer >= 0")
    if is_int(hits) and is_int(trials) and trials >= 1 and hits > trials:
        errors.append(f"{where}.hits {hits} exceeds trials {trials}")
    if not is_int(false_wakes) or false_wakes < 0:
        errors.append(f"{where}.false_wakes must be an integer >= 0")
    if not is_number(exposure_min) or exposure_min <= 0:
        errors.append(f"{where}.exposure_min must be a number > 0")
    if not isinstance(cell.get("note"), str):
        errors.append(f"{where}.note must be a string")

    try:
        started = parse_timestamp(cell.get("started_at"), f"{where}.started_at")
        ended = parse_timestamp(cell.get("ended_at"), f"{where}.ended_at")
    except InputError as exc:
        errors.append(str(exc))
        return errors
    if ended <= started:
        errors.append(f"{where}.ended_at must be after started_at")
    elif is_number(exposure_min) and exposure_min > 0:
        window_min = (ended - started).total_seconds() / 60.0
        if exposure_min > window_min + 1.0 / 60.0:
            errors.append(
                f"{where}.exposure_min {exposure_min} is longer than the "
                f"{window_min:.2f} min session"
            )
    return errors


def validate_document(doc: object) -> list[str]:
    """Schema, grid and count validation. Blank cells are valid but unmeasured."""
    if not isinstance(doc, dict):
        return ["the evidence file must contain a JSON object"]
    errors: list[str] = []
    if doc.get("schema") != SCHEMA:
        errors.append(f"schema must be {SCHEMA!r}")
    if doc.get("kind") not in KINDS:
        errors.append(f"kind must be one of {list(KINDS)}")
    if doc.get("issue") != 70:
        errors.append("issue must be 70")

    volumes = doc.get("volumes")
    if not isinstance(volumes, list) or len(volumes) != 3:
        errors.append("volumes must list exactly three percentages")
        volumes = []
    else:
        clean: list[int] = []
        for volume in volumes:
            if not is_int(volume) or not 1 <= volume <= 100:
                errors.append(f"volume {volume!r} is outside 1..100")
            elif volume in clean:
                errors.append(f"volume {volume} is repeated")
            else:
                clean.append(volume)
        volumes = clean

    cells = doc.get("cells")
    if not isinstance(cells, list):
        errors.append("cells must be a list")
        cells = []
    elif len(cells) != 6:
        errors.append(f"cells must have six entries, found {len(cells)}")
    seen: set[tuple[object, object]] = set()
    for index, cell in enumerate(cells):
        errors.extend(validate_cell(cell, index, volumes))
        if isinstance(cell, dict):
            key = (cell.get("volume"), cell.get("aec"))
            if key in seen:
                errors.append(f"cell[{index}] repeats {key}")
            seen.add(key)
    if len(volumes) == 3 and len(cells) == 6:
        expected = {(volume, aec) for volume in volumes for aec in AEC_STATES}
        missing = sorted(expected - seen)
        extra = sorted(seen - expected)
        if missing:
            errors.append(f"missing cells: {missing}")
        if extra:
            errors.append(f"unexpected cells: {extra}")

    host = doc.get("host")
    if not isinstance(host, dict):
        errors.append("host must be an object")
    else:
        for field in ("device", "pipewire", "pipewire_pulse", "capture_source",
                      "playback_sink", "aec_source", "aec_sink", "aec_method",
                      "aec_args", "note"):
            if not isinstance(host.get(field), str):
                errors.append(f"host.{field} must be a string")

    targets = doc.get("targets")
    if not isinstance(targets, dict):
        errors.append("targets must be an object")
    else:
        for field in ("hit_rate", "false_wakes_per_hour"):
            if field not in targets:
                errors.append(f"targets.{field} is required (null when unset)")
        rate = targets.get("hit_rate")
        if rate is not None and (not is_number(rate) or not 0 < rate <= 1):
            errors.append("targets.hit_rate must be null or a fraction in (0, 1]")
        false_rate = targets.get("false_wakes_per_hour")
        if false_rate is not None and (not is_number(false_rate) or false_rate < 0):
            errors.append("targets.false_wakes_per_hour must be null or >= 0")

    decision = doc.get("decision")
    if not isinstance(decision, dict):
        errors.append("decision must be an object")
    else:
        for field in ("status", "max_volume", "note"):
            if field not in decision:
                errors.append(f"decision.{field} is required")
        if decision.get("status") not in ("pending", "decided"):
            errors.append("decision.status must be 'pending' or 'decided'")
        max_volume = decision.get("max_volume")
        if decision.get("status") == "pending" and max_volume is not None:
            errors.append("decision.max_volume must stay null while the decision is pending")
        if max_volume is not None and (not is_int(max_volume) or not 0 <= max_volume <= 100):
            errors.append("decision.max_volume must be null or an integer in 0..100")
        if not isinstance(decision.get("note"), str):
            errors.append("decision.note must be a string")
    return errors


def require_evidence_fields(doc: dict) -> list[str]:
    """A measured run must name the host, source, sink and AEC path it used."""
    host = doc["host"]
    required = ["device", "pipewire", "capture_source", "playback_sink"]
    if any(cell.get("aec") == "on" and cell_measured(cell) for cell in doc["cells"]):
        required.extend(["aec_source", "aec_sink", "aec_method"])
    missing = [field for field in required if not host.get(field, "").strip()]
    return [f"host.{field} is required before this matrix can be evidence" for field in missing]


def summarize(
    doc: dict,
    target_hit_rate: float | None = None,
    target_false_per_hour: float | None = None,
) -> dict:
    """Validate the file and derive rates from counts; never invent a result."""
    errors = validate_document(doc)
    if errors:
        raise InputError("; ".join(errors))

    cells = doc["cells"]
    measured = [cell for cell in cells if cell_measured(cell)]
    complete = len(measured) == len(cells) == 6
    if doc["kind"] == KIND_OPERATOR and complete:
        errors = require_evidence_fields(doc)
        if errors:
            raise InputError("; ".join(errors))

    file_targets = doc["targets"]
    hit_target = target_hit_rate if target_hit_rate is not None else file_targets["hit_rate"]
    false_target = (
        target_false_per_hour
        if target_false_per_hour is not None
        else file_targets["false_wakes_per_hour"]
    )
    targets_supplied = hit_target is not None and false_target is not None
    if doc["decision"]["status"] == "decided":
        if doc["kind"] == KIND_SYNTHETIC:
            raise InputError("a synthetic fixture cannot carry a decided product decision")
        if not complete:
            raise InputError("a decided decision needs all six measured cells")
        if not targets_supplied:
            raise InputError(
                "a decided decision needs both operator targets; write them in "
                "the file or pass --target-hit-rate and --target-false-per-hour"
            )

    out_cells = []
    for cell in cells:
        entry = {
            "volume": cell["volume"],
            "aec": cell["aec"],
            "status": "measured" if cell_measured(cell) else "unmeasured",
        }
        if cell_measured(cell):
            hit_rate = cell["hits"] / cell["trials"]
            false_per_hour = cell["false_wakes"] / (cell["exposure_min"] / 60.0)
            entry.update(
                {
                    "trials": cell["trials"],
                    "hits": cell["hits"],
                    "hit_rate": round(hit_rate, 4),
                    "false_wakes": cell["false_wakes"],
                    "exposure_min": cell["exposure_min"],
                    "false_wakes_per_hour": round(false_per_hour, 2),
                    "started_at": cell["started_at"],
                    "ended_at": cell["ended_at"],
                }
            )
            if hit_target is not None:
                entry["meets_hit_rate_target"] = hit_rate >= hit_target
            if false_target is not None:
                entry["meets_false_wakes_target"] = false_per_hour <= false_target
        out_cells.append(entry)

    kind = doc["kind"]
    if kind == KIND_SYNTHETIC:
        status = STATUS_SYNTHETIC
    elif complete:
        status = STATUS_MEASURED
    else:
        status = STATUS_UNMEASURED

    targets_supplied = hit_target is not None and false_target is not None
    return {
        "schema": SCHEMA,
        "issue": 70,
        "kind": kind,
        "status": status,
        "evidence": kind == KIND_OPERATOR and complete,
        "measured_cells": len(measured),
        "required_cells": 6,
        "targets": {
            "hit_rate": hit_target,
            "false_wakes_per_hour": false_target,
            "supplied": targets_supplied,
        },
        "decision_ready": kind == KIND_OPERATOR and complete and targets_supplied,
        "cells": out_cells,
        "decision": doc["decision"],
        "note": (
            "Synthetic fixture: arithmetic only, never evidence."
            if kind == KIND_SYNTHETIC
            else (
                "All six cells measured."
                if complete
                else "Unmeasured: no cell counts exist until an operator run is recorded."
            )
        ),
    }


def load_document(path: Path) -> dict:
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError as exc:
        raise InputError(f"{path} does not exist; run 'init' first") from exc
    except json.JSONDecodeError as exc:
        raise InputError(f"{path} is not valid JSON: {exc}") from exc
    errors = validate_document(data)
    if errors:
        raise InputError("; ".join(errors))
    return data


def write_document(path: Path, doc: dict) -> None:
    """Write atomically so a refused edit cannot leave a half file."""
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
    tmp.replace(path)


def cmd_init(args: argparse.Namespace) -> int:
    path = Path(args.out)
    if path.exists():
        raise InputError(f"{path} already exists; move it aside before re-initialising")
    doc = new_document(parse_volumes(args.volumes))
    write_document(path, doc)
    print(f"wrote {path}: 6 blank cells, status UNMEASURED")
    return 0


def cmd_record(args: argparse.Namespace) -> int:
    path = Path(args.file)
    doc = load_document(path)
    if doc["kind"] != KIND_OPERATOR:
        raise InputError(
            f"{path} is a {doc['kind']}; fixtures never collect operator evidence"
        )
    volumes = doc["volumes"]
    if args.volume not in volumes:
        raise InputError(f"volume {args.volume} is not one of the configured {volumes}")
    index = next(
        (
            position
            for position, cell in enumerate(doc["cells"])
            if cell["volume"] == args.volume and cell["aec"] == args.aec
        ),
        None,
    )
    if index is None:
        raise InputError(f"no cell for volume {args.volume} and AEC {args.aec}")
    existing = doc["cells"][index]
    if cell_measured(existing) and not args.replace:
        raise InputError(
            f"cell volume={args.volume} aec={args.aec} is already measured; "
            "pass --replace only after checking the run"
        )

    cell = {
        "volume": args.volume,
        "aec": args.aec,
        "trials": args.trials,
        "hits": args.hits,
        "false_wakes": args.false_wakes,
        "exposure_min": args.exposure_min,
        "started_at": args.started_at,
        "ended_at": args.ended_at,
        "note": args.note,
    }
    errors = validate_cell(cell, index, volumes)
    if errors:
        raise InputError("; ".join(errors))
    doc["cells"][index] = cell
    errors = validate_document(doc)
    if errors:
        raise InputError("; ".join(errors))
    write_document(path, doc)
    print(
        f"recorded volume={args.volume} aec={args.aec}: "
        f"{args.hits}/{args.trials} hits, {args.false_wakes} false wakes "
        f"in {args.exposure_min} min"
    )
    return 0


def cmd_report(args: argparse.Namespace) -> int:
    doc = load_document(Path(args.file))
    if args.target_hit_rate is not None and not 0 < args.target_hit_rate <= 1:
        raise InputError("--target-hit-rate must be a fraction in (0, 1]")
    if args.target_false_per_hour is not None and args.target_false_per_hour < 0:
        raise InputError("--target-false-per-hour must be >= 0")
    summary = summarize(doc, args.target_hit_rate, args.target_false_per_hour)
    print(json.dumps(summary, indent=2))
    return 1 if summary["status"] == STATUS_SYNTHETIC else 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest="command", required=True)

    init = sub.add_parser("init", help="write a blank six-cell evidence template")
    init.add_argument("--out", required=True, help="path for the JSON evidence file")
    init.add_argument(
        "--volumes",
        required=True,
        help="three distinct music-volume percentages, e.g. 25,50,75",
    )
    init.set_defaults(func=cmd_init)

    record = sub.add_parser("record", help="record one operator-measured cell")
    record.add_argument("--file", required=True)
    record.add_argument("--volume", type=int, required=True)
    record.add_argument("--aec", choices=AEC_STATES, required=True)
    record.add_argument("--trials", type=int, required=True)
    record.add_argument("--hits", type=int, required=True)
    record.add_argument("--false-wakes", type=int, required=True)
    record.add_argument("--exposure-min", type=float, required=True)
    record.add_argument("--started-at", required=True)
    record.add_argument("--ended-at", required=True)
    record.add_argument("--note", default="")
    record.add_argument("--replace", action="store_true")
    record.set_defaults(func=cmd_record)

    report = sub.add_parser("report", help="validate and summarize the matrix")
    report.add_argument("--file", required=True)
    report.add_argument("--target-hit-rate", type=float, default=None)
    report.add_argument("--target-false-per-hour", type=float, default=None)
    report.set_defaults(func=cmd_report)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args))
    except InputError as exc:
        print(f"wake_over_music: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
