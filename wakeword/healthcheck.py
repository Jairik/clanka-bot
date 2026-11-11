"""Docker health check: healthy when describe returns a wake program.

Runs with the upstream image's venv interpreter, which has wyoming installed.
The raw event data is inspected so this works across wyoming versions.
"""

from __future__ import annotations

import asyncio
import os
import sys

from wyoming.client import AsyncTcpClient
from wyoming.info import Describe


async def check(uri: str) -> bool:
    # The upstream image pins an older wyoming, whose from_uri takes only the
    # URI; Docker's health-check timeout bounds a hung connect or read.
    client = AsyncTcpClient.from_uri(uri)
    try:
        await client.connect()
        await client.write_event(Describe().event())
        event = await client.read_event()
        if event is None or event.type != "info":
            return False
        return bool((event.data or {}).get("wake"))
    finally:
        await client.disconnect()


def main() -> None:
    uri = os.environ.get("WAKEWORD_ADDR", "tcp://127.0.0.1:10400")
    try:
        healthy = asyncio.run(check(uri))
    except Exception as exc:  # connection refused, timeout, protocol error
        print(f"health check failed: {exc}", file=sys.stderr)
        sys.exit(1)
    if not healthy:
        print("health check failed: no wake info response", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
