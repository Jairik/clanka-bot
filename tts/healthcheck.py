"""Docker health check: the service is healthy when describe returns info.

This is the warm-model probe: the server only starts listening after the model
and its default voices are loaded.
"""

from __future__ import annotations

import asyncio
import os
import sys

from wyoming.client import AsyncTcpClient
from wyoming.info import Describe, Info


async def check(uri: str) -> bool:
    client = AsyncTcpClient.from_uri(uri, connect_timeout=3.0, read_timeout=3.0)
    try:
        await client.connect()
        await client.write_event(Describe().event())
        event = await client.read_event()
        return event is not None and Info.is_type(event.type)
    finally:
        await client.disconnect()


def main() -> None:
    uri = os.environ.get("TTS_ADDR", "tcp://127.0.0.1:10200")
    try:
        healthy = asyncio.run(check(uri))
    except Exception as exc:  # connection refused, timeout, protocol error
        print(f"health check failed: {exc}", file=sys.stderr)
        sys.exit(1)
    if not healthy:
        print("health check failed: no info response", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
