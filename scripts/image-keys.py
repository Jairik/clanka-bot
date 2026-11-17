#!/usr/bin/env python3
"""Print a content key for every Compose service that builds an image.

The key changes only when something that goes into the image changes: the
committed files under the service's build context, its Dockerfile, its resolved
build args and the target platform. CI publishes arm64 images under
<registry>/<service>:key-<key>, and the Pi pulls the image whose key matches its
own checkout and .env. A commit that does not touch a service therefore reuses
the image already published for it instead of rebuilding.

    scripts/image-keys.py [--env-file FILE] [--platform P] [--json]

Default output is one "<service> <key>" line per service, sorted by name.
--json prints a list of objects with the build inputs CI needs (context,
dockerfile, args) next to each key.

Files are read from the HEAD commit, not the working tree; deploy.sh refuses a
dirty checkout and CI builds a clean one, so the two agree.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys

# Bump to invalidate every published image at once (for example after a base
# image security update, since a floating base tag is not part of the key).
KEY_VERSION = "1"

# Services whose image uses only part of their build context, as paths relative
# to that context. Anything not listed is keyed on its whole context.
IMAGE_PATHS = {
    # The UI artifact copies only web/ out of the assistant context, so Go-only
    # commits keep the published image.
    "dashboard-web": ["web"],
}


def run(*cmd: str, cwd: str) -> str:
    return subprocess.run(
        cmd, cwd=cwd, check=True, capture_output=True, text=True
    ).stdout


def tree_id(root: str, path: str) -> str:
    """The git object id of a committed path (a tree or a blob)."""
    return run("git", "rev-parse", f"HEAD:{path}", cwd=root).strip()


def compose_services(root: str, env_file: str | None) -> dict:
    cmd = ["docker", "compose"]
    if env_file:
        # Relative to the caller, not to the repo root this runs in.
        cmd += ["--env-file", os.path.abspath(env_file)]
    # Every profile, so the opt-in receiver is keyed (and published) too.
    cmd += ["--profile", "*", "config", "--format", "json"]
    # GIT_SHA only names tags; it is not a build input and must not leak into
    # a resolved value through the environment.
    env = {k: v for k, v in os.environ.items() if k != "GIT_SHA"}
    out = subprocess.run(
        cmd, cwd=root, check=True, capture_output=True, text=True, env=env
    ).stdout
    return json.loads(out).get("services") or {}


def keys(root: str, env_file: str | None, platform: str) -> list[dict]:
    root = os.path.realpath(root)
    result = []
    for name, svc in sorted(compose_services(root, env_file).items()):
        build = svc.get("build")
        if not build:
            continue
        context = os.path.relpath(os.path.realpath(build["context"]), root)
        if context.startswith(".."):
            raise SystemExit(f"image-keys: {name} builds outside the repo")
        dockerfile = build.get("dockerfile") or "Dockerfile"
        args = {k: "" if v is None else str(v) for k, v in (build.get("args") or {}).items()}
        paths = IMAGE_PATHS.get(name, ["."])
        inputs = {
            "version": KEY_VERSION,
            "service": name,
            "platform": platform,
            "context": context,
            "dockerfile": dockerfile,
            "dockerfile_id": tree_id(root, os.path.normpath(os.path.join(context, dockerfile))),
            "trees": {p: tree_id(root, os.path.normpath(os.path.join(context, p))) for p in paths},
            "args": dict(sorted(args.items())),
        }
        digest = hashlib.sha256(
            json.dumps(inputs, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()[:24]
        result.append(
            {
                "service": name,
                "key": digest,
                "context": context,
                "dockerfile": dockerfile,
                "args": inputs["args"],
            }
        )
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--env-file", help="Compose env file (default: .env in the repo, if any)")
    parser.add_argument("--platform", default="linux/arm64")
    parser.add_argument("--root", default=os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    parser.add_argument("--json", action="store_true")
    opts = parser.parse_args()
    try:
        entries = keys(opts.root, opts.env_file, opts.platform)
    except subprocess.CalledProcessError as err:
        sys.stderr.write(f"image-keys: {' '.join(err.cmd)} failed: {err.stderr.strip()}\n")
        return 1
    if opts.json:
        print(json.dumps(entries))
    else:
        for entry in entries:
            print(entry["service"], entry["key"])
    return 0


if __name__ == "__main__":
    sys.exit(main())
