"""Docker health check: healthy when describe returns an ASR program.

The server only starts listening after the model is warm, so this is also the
warm-model probe.
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
        if event is None or not Info.is_type(event.type):
            return False
        return bool(Info.from_event(event).asr)
    finally:
        await client.disconnect()


def main() -> None:
    uri = os.environ.get("STT_ADDR", "tcp://127.0.0.1:10300")
    try:
        healthy = asyncio.run(check(uri))
    except Exception as exc:  # connection refused, timeout, protocol error
        print(f"health check failed: {exc}", file=sys.stderr)
        sys.exit(1)
    if not healthy:
        print("health check failed: no asr info response", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
