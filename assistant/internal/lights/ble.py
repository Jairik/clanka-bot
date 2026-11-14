# Bleak uses the existing host BlueZ D-Bus; never pair, reset or power-cycle an adapter.
import asyncio
import json
import sys
from bleak import BleakClient, BleakScanner

async def write(address, characteristic, payload, client_factory=BleakClient):
    # Reconnect once after a lost link; both attempts share the caller's deadline.
    # Ten seconds matches a cold ELK-BLEDOM connect. Two seconds gave up first.
    for attempt in range(2):
        try:
            async with client_factory(address, timeout=10) as client:
                await client.write_gatt_char(characteristic, bytes.fromhex(payload), response=False)
            return
        except Exception:
            if attempt:
                raise

async def main():
    # Discovery is explicit and short so the shared speaker adapter stays available.
    if sys.argv[1] == 'scan':
        devices = await BleakScanner.discover(timeout=3)
        print(json.dumps([{'address': d.address, 'name': d.name or d.address} for d in devices]))
    else:
        await write(*sys.argv[1:])

if __name__ == '__main__':
    # Slightly inside the Go write budget so the helper can exit on its own.
    asyncio.run(asyncio.wait_for(main(), timeout=12))
