#!/usr/bin/env python3
"""Turn pi-ram-sample.sh JSONL into the tables #69/#74 ask for.

Usage:
  scripts/pi-ram-report.py --idle /tmp/pi-ram-idle.jsonl --load /tmp/pi-ram-load.jsonl

Idle reports the median PSS per container and the median system totals. Load
reports the peak PSS per container and the system peak, defined as the sample
with the lowest MemAvailable (the brief asks for that), plus the highest
free-style `used` seen. Every number is MiB; the JSONL stores KiB.
"""

from __future__ import annotations

import argparse
import json
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path

MIB = 1024


def load(path: Path) -> list[dict]:
    rows = []
    for line in path.read_text().splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    if not rows:
        raise SystemExit(f"{path}: no samples")
    return rows


def container_names(rows: list[dict]) -> list[str]:
    names = set()
    for row in rows:
        for c in row["containers"]:
            names.add(c["name"])
    return sorted(names)


def pss_by_container(rows: list[dict]) -> dict[str, list[int]]:
    out: dict[str, list[int]] = {}
    for name in container_names(rows):
        out[name] = [
            next((c["pss_kb"] for c in row["containers"] if c["name"] == name), 0)
            for row in rows
        ]
    return out


def rss_by_container(rows: list[dict]) -> dict[str, list[int]]:
    out: dict[str, list[int]] = {}
    for name in container_names(rows):
        out[name] = [
            next((c["rss_kb"] for c in row["containers"] if c["name"] == name), 0)
            for row in rows
        ]
    return out


def used(row: dict) -> int:
    """free's `used`: total minus free, buffers, cache and reclaimable slab."""
    if "used_kb" in row:
        return row["used_kb"]
    return (
        row["mem_total_kb"]
        - row.get("mem_free_kb", 0)
        - row.get("buffers_kb", 0)
        - row.get("cached_kb", 0)
        - row.get("sreclaimable_kb", 0)
    )


def window(rows: list[dict]) -> str:
    start = datetime.fromtimestamp(rows[0]["epoch"], tz=timezone.utc)
    end = datetime.fromtimestamp(rows[-1]["epoch"], tz=timezone.utc)
    return f"{start:%Y-%m-%dT%H:%M:%S}Z .. {end:%Y-%m-%dT%H:%M:%S}Z ({len(rows)} samples)"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--idle", type=Path, required=True)
    parser.add_argument("--load", type=Path)
    args = parser.parse_args()

    idle = load(args.idle)
    idle_pss = pss_by_container(idle)
    idle_rss = rss_by_container(idle)
    idle_medians = {name: statistics.median(values) for name, values in idle_pss.items()}
    idle_total = statistics.median(row["pss_all_kb"] for row in idle)

    print(f"## Idle\n\n{window(idle)}\n")
    print("| Container | Median PSS MiB | Median RSS MiB |")
    print("| --- | ---: | ---: |")
    for name, values in idle_pss.items():
        print(
            f"| {name} | {idle_medians[name] / MIB:.1f} "
            f"| {statistics.median(idle_rss[name]) / MIB:.1f} |"
        )
    print(f"| **Sum of containers (median)** | {sum(idle_medians.values()) / MIB:.1f} | |")
    print(f"| **All processes (median PSS)** | {idle_total / MIB:.1f} | |")
    print(f"| **System used (median, free-style)** | {statistics.median(used(r) for r in idle) / MIB:.1f} | |")
    print(f"| **MemAvailable (median)** | {statistics.median(r['mem_available_kb'] for r in idle) / MIB:.1f} | |")
    print(f"| **Swap used (median)** | "
          f"{statistics.median(r['swap_total_kb'] - r['swap_free_kb'] for r in idle) / MIB:.1f} | |")
    print()

    if not args.load:
        return 0

    load_rows = load(args.load)
    load_pss = pss_by_container(load_rows)
    load_peaks = {name: max(values) for name, values in load_pss.items()}
    peak_row = min(load_rows, key=lambda r: r["mem_available_kb"])
    peak_used_row = max(load_rows, key=used)

    print(f"## Load\n\n{window(load_rows)}\n")
    print("| Container | Peak PSS MiB | Idle median MiB | Delta MiB |")
    print("| --- | ---: | ---: | ---: |")
    for name in load_pss:
        idle_value = idle_medians.get(name, 0)
        print(
            f"| {name} | {load_peaks[name] / MIB:.1f} | {idle_value / MIB:.1f} "
            f"| {(load_peaks[name] - idle_value) / MIB:+.1f} |"
        )
    print(f"| **Sum of containers (peak)** | {sum(load_peaks.values()) / MIB:.1f} | "
          f"{sum(idle_medians.values()) / MIB:.1f} | "
          f"{(sum(load_peaks.values()) - sum(idle_medians.values())) / MIB:+.1f} |")
    print(f"| **All processes (peak PSS)** | {max(r['pss_all_kb'] for r in load_rows) / MIB:.1f} | "
          f"{idle_total / MIB:.1f} | |")
    print()
    print(
        f"System peak: lowest MemAvailable {peak_row['mem_available_kb'] / MIB:.1f} MiB at "
        f"{datetime.fromtimestamp(peak_row['epoch'], tz=timezone.utc):%H:%M:%S}Z "
        f"(used {used(peak_row) / MIB:.1f} MiB there); "
        f"highest used {used(peak_used_row) / MIB:.1f} MiB at "
        f"{datetime.fromtimestamp(peak_used_row['epoch'], tz=timezone.utc):%H:%M:%S}Z."
    )
    print(
        f"Threshold 6.5 GB = {6.5e9 / (MIB * MIB):.1f} MiB "
        f"(or {6.5 * 1024:.1f} MiB if read as 6.5 GiB): "
        f"headroom at the highest-used sample is "
        f"{6.5e9 / (MIB * MIB) - used(peak_used_row) / MIB:.1f} MiB by the decimal reading "
        f"and {6.5 * 1024 - used(peak_used_row) / MIB:.1f} MiB by the GiB reading."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
