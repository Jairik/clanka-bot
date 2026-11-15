#!/usr/bin/env python3
"""Load the starter Personas and their phrase books into a running Dashboard.

The Dashboard owns the only credentials and the only SQLite file, so this
script uses the same authenticated JSON/form API the web app uses. It never
opens the database and never prints a secret.

Usage:
    python3 scripts/seed-personas.py [--base http://127.0.0.1:8082]
        [--owner Jairik@github] [--file personas/starter.json]
        [--activate NAME] [--dry-run]

The owner defaults to ASSISTANT_DASHBOARD_OWNER. A loopback base URL is the
documented local smoke test; the tailnet URL works through `tailscale serve`.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request
from typing import Any

REQUEST_TIMEOUT = 30


def request(
    base: str,
    owner: str,
    method: str,
    path: str,
    form: dict[str, str] | None = None,
) -> tuple[int, Any]:
    url = base.rstrip("/") + path
    data = urllib.parse.urlencode(form).encode() if form is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Accept", "application/json")
    req.add_header("Tailscale-User-Login", owner)
    # The Dashboard's same-origin guard accepts this marker on a write.
    req.add_header("Sec-Fetch-Site", "same-origin")
    if data is not None:
        req.add_header("Content-Type", "application/x-www-form-urlencoded")
    try:
        with urllib.request.urlopen(req, timeout=REQUEST_TIMEOUT) as resp:
            body = resp.read()
            return resp.status, json.loads(body) if body else None
    except urllib.error.HTTPError as err:
        body = err.read()
        try:
            return err.code, json.loads(body) if body else None
        except json.JSONDecodeError:
            return err.code, body.decode("utf-8", "replace")


def path_name(name: str) -> str:
    return urllib.parse.quote(name, safe="")


def load_personas(path: str) -> list[dict[str, Any]]:
    with open(path, encoding="utf-8") as handle:
        data = json.load(handle)
    personas = data.get("personas") if isinstance(data, dict) else None
    if not isinstance(personas, list) or not personas:
        raise ValueError("expected a non-empty {\"personas\": [...]} object")
    for persona in personas:
        for field in ("name", "voice", "personality", "filler", "unavailable"):
            if not str(persona.get(field, "")).strip():
                raise ValueError(f"persona {persona.get('name')!r} is missing {field}")
        phrases = persona.get("phrases")
        if not isinstance(phrases, dict) or not phrases:
            raise ValueError(f"persona {persona.get('name')!r} has no phrases")
    return personas


def existing_personas(base: str, owner: str) -> set[str]:
    status, view = request(base, owner, "GET", "/api/ui/personas")
    if status != 200 or not isinstance(view, dict):
        raise SystemExit(f"could not read personas (HTTP {status})")
    if not view.get("HasService"):
        raise SystemExit("the persona library is not configured in this build")
    return {str(row.get("Name", "")) for row in view.get("Rows") or []}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", default="http://127.0.0.1:8082")
    parser.add_argument("--owner", default=os.environ.get("ASSISTANT_DASHBOARD_OWNER", ""))
    parser.add_argument("--file", default="personas/starter.json")
    parser.add_argument("--activate", default="", help="select this Persona when done")
    parser.add_argument("--dry-run", action="store_true")
    args = parser.parse_args()

    if not args.owner:
        parser.error("--owner or ASSISTANT_DASHBOARD_OWNER is required")
    try:
        personas = load_personas(args.file)
        existing = existing_personas(args.base, args.owner)
    except (OSError, ValueError) as err:
        print(f"error: {err}", file=sys.stderr)
        return 2

    print(f"loaded {len(personas)} personas from {args.file}; {len(existing)} already stored")
    failures = 0
    for persona in personas:
        name = str(persona["name"]).strip()
        form = {
            "name": name,
            "voice": str(persona["voice"]),
            "personality": str(persona["personality"]),
            "filler": str(persona["filler"]),
            "unavailable": str(persona["unavailable"]),
        }
        if args.dry_run:
            print(f"would save persona {name} with {len(persona['phrases'])} phrases")
            continue
        if name in existing:
            status, view = request(args.base, args.owner, "POST", f"/personas/{path_name(name)}", form)
            action = "updated"
        else:
            status, view = request(args.base, args.owner, "POST", "/personas", form)
            action = "created"
        if status != 200:
            message = view.get("Error") if isinstance(view, dict) else view
            print(f"FAILED {action} {name}: HTTP {status} {message}", file=sys.stderr)
            failures += 1
            continue
        print(f"{action} persona {name}")
        for tool, phrase in sorted(persona["phrases"].items()):
            status, view = request(
                args.base,
                args.owner,
                "POST",
                f"/phrasebook/{path_name(name)}",
                {"tool": str(tool), "phrase": str(phrase)},
            )
            if status != 200:
                message = view.get("Error") if isinstance(view, dict) else view
                print(f"FAILED phrase {name}/{tool}: HTTP {status} {message}", file=sys.stderr)
                failures += 1

    if args.activate and not args.dry_run:
        status, view = request(
            args.base, args.owner, "POST", f"/personas/{path_name(args.activate)}/select"
        )
        if status != 200:
            message = view.get("Error") if isinstance(view, dict) else view
            print(f"FAILED activate {args.activate}: HTTP {status} {message}", file=sys.stderr)
            failures += 1
        else:
            print(f"activated persona {args.activate}")

    if failures:
        print(f"{failures} write(s) failed", file=sys.stderr)
        return 1
    print("done")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
