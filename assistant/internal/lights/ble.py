# Bleak uses the existing host BlueZ D-Bus; never pair, reset or power-cycle an adapter.
import asyncio
import json
import sys
import time

# How long a saved device stays connected after its last write.
IDLE_S = 60


def bleak_client():
    # Local import so command-loop tests can inject a fake and skip BlueZ.
    from bleak import BleakClient
    return BleakClient


def bleak_scanner():
    # Local import so command-loop tests can inject a fake and skip BlueZ.
    from bleak import BleakScanner
    return BleakScanner.discover


async def write(address, characteristic, payload, client_factory=None):
    # One-shot path: connect, write, disconnect, and retry a lost link once.
    if client_factory is None:
        client_factory = bleak_client()
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


class Session:
    # One open client and the time it was last used.
    def __init__(self, client):
        self.client = client
        self.used = time.monotonic()


def client_connected(client):
    # Bleak exposes this as a property. Anything else counts as disconnected.
    try:
        return bool(client.is_connected)
    except Exception:
        return False


async def drop_session(sessions, address):
    # Disconnect and forget one device. A missing session is fine.
    session = sessions.pop(address, None)
    if session is None:
        return
    if not client_connected(session.client):
        return
    try:
        await session.client.disconnect()
    except Exception:
        pass


async def write_kept(sessions, address, characteristic, payload, client_factory):
    # Write on the device's open client. A lost link is dropped and retried once.
    data = bytes.fromhex(payload)
    last_error = None
    for attempt in range(2):
        session = sessions.get(address)
        if session is None or not client_connected(session.client):
            await drop_session(sessions, address)
            client = client_factory(address, timeout=10)
            try:
                await client.connect()
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                last_error = exc
                if attempt:
                    raise
                continue
            session = Session(client)
            sessions[address] = session
        session.used = time.monotonic()
        try:
            await session.client.write_gatt_char(characteristic, data, response=False)
            return
        except asyncio.CancelledError:
            raise
        except Exception as exc:
            last_error = exc
            await drop_session(sessions, address)
            if attempt:
                raise
    if last_error:
        raise last_error
    raise OSError('disconnected')


def reply(write_line, cmd, ok, error='', devices=None):
    # One JSON object for one command. id is echoed so Go can match the line.
    msg = {'id': cmd.get('id'), 'ok': ok}
    if error:
        msg['error'] = error
    if devices is not None:
        msg['devices'] = devices
    write_line(msg)


async def command_loop(read_line, write_line, client_factory, scanner, idle_s=IDLE_S):
    # Read newline-delimited JSON commands and answer with one JSON line each.
    # Each device stays connected until idle_s passes or a close arrives.
    sessions = {}
    inflight = {}
    # Writes take this lock so two commands cannot share one client at once.
    write_lock = asyncio.Lock()

    async def expire_idle():
        # Wake for the soonest deadline, then drop clients nobody is using.
        while True:
            now = time.monotonic()
            delay = idle_s
            if sessions:
                for session in sessions.values():
                    remaining = idle_s - (now - session.used)
                    if remaining < delay:
                        delay = remaining
                # Poll about once a second so a fresh write is not held for two idle periods.
                if delay > 1:
                    delay = 1
            if delay < 0.01:
                delay = 0.01
            await asyncio.sleep(delay)
            now = time.monotonic()
            for address, session in list(sessions.items()):
                task = inflight.get(address)
                if task is not None and not task.done():
                    continue
                if now - session.used >= idle_s:
                    await drop_session(sessions, address)

    async def run_write(cmd):
        # Hold the lock only around the GATT write so a close can still be read.
        address = cmd.get('address') or ''
        try:
            async with write_lock:
                await write_kept(
                    sessions,
                    address,
                    cmd.get('char') or '',
                    cmd.get('hex') or '',
                    client_factory,
                )
            reply(write_line, cmd, True)
        except asyncio.CancelledError:
            reply(write_line, cmd, False, 'cancelled')
        except Exception as exc:
            reply(write_line, cmd, False, str(exc))
        finally:
            current = inflight.get(address)
            if current is asyncio.current_task():
                inflight.pop(address, None)

    async def handle(cmd):
        op = cmd.get('op')
        if op == 'write':
            if not cmd.get('address'):
                reply(write_line, cmd, False, 'missing address')
                return
            # The task runs beside the reader so a later close can cancel it.
            task = asyncio.create_task(run_write(cmd))
            inflight[cmd.get('address')] = task
            return
        if op == 'close':
            address = cmd.get('address') or ''
            await drop_session(sessions, address)
            task = inflight.get(address)
            if task is not None and not task.done():
                task.cancel()
                try:
                    await task
                except asyncio.CancelledError:
                    pass
            reply(write_line, cmd, True)
            return
        if op == 'scan':
            try:
                found = await scanner(timeout=3)
                devices = [{'address': d.address, 'name': d.name or d.address} for d in found]
                reply(write_line, cmd, True, devices=devices)
            except Exception as exc:
                reply(write_line, cmd, False, str(exc))
            return
        reply(write_line, cmd, False, 'unknown op')

    idle_task = asyncio.create_task(expire_idle())
    try:
        while True:
            line = await read_line()
            if line is None:
                return
            text = line.strip()
            if text == '':
                continue
            try:
                cmd = json.loads(text)
            except json.JSONDecodeError:
                reply(write_line, {}, False, 'invalid json')
                continue
            if not isinstance(cmd, dict):
                reply(write_line, {}, False, 'invalid json')
                continue
            await handle(cmd)
    finally:
        idle_task.cancel()
        pending = list(inflight.values())
        for task in pending:
            task.cancel()
        await asyncio.gather(idle_task, *pending, return_exceptions=True)


async def serve():
    # Long-lived helper: JSON commands on stdin, one JSON line on stdout each.
    loop = asyncio.get_running_loop()
    reader = asyncio.StreamReader()
    protocol = asyncio.StreamReaderProtocol(reader)
    await loop.connect_read_pipe(lambda: protocol, sys.stdin)

    async def read_line():
        raw = await reader.readline()
        if not raw:
            return None
        return raw.decode()

    def write_line(obj):
        sys.stdout.write(json.dumps(obj) + '\n')
        sys.stdout.flush()

    await command_loop(read_line, write_line, bleak_client(), bleak_scanner())


async def main():
    # One-shot argv mode, used when the process is not the long-lived helper.
    if sys.argv[1] == 'scan':
        devices = await bleak_scanner()(timeout=3)
        print(json.dumps([{'address': d.address, 'name': d.name or d.address} for d in devices]))
    else:
        await write(*sys.argv[1:])


if __name__ == '__main__':
    if len(sys.argv) > 1:
        # Slightly inside the Go write budget so the helper can exit on its own.
        asyncio.run(asyncio.wait_for(main(), timeout=12))
    else:
        asyncio.run(serve())
