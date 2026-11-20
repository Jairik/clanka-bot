# Exercise reconnects and the long-lived command loop without a Bluetooth adapter.
import asyncio
import json
import unittest
import ble


class Client:
    calls = []
    failures = 0
    def __init__(self, address, timeout):
        self.address = address
    async def __aenter__(self):
        return self
    async def __aexit__(self, *args):
        pass
    async def write_gatt_char(self, characteristic, data, response):
        self.calls.append((self.address, characteristic, data, response))
        if self.failures:
            type(self).failures -= 1
            raise OSError('disconnected')


class DriverTests(unittest.IsolatedAsyncioTestCase):
    async def test_reconnect_and_bytes(self):
        Client.calls = []
        Client.failures = 1
        for characteristic, payload in [('ffd9', 'cc2333'), ('fff3', '7e0004f00001ff00ef')]:
            Client.calls = []
            Client.failures = 1
            await ble.write('address', characteristic, payload, Client)
            self.assertEqual(Client.calls, [('address', characteristic, bytes.fromhex(payload), False)] * 2)
    async def test_missing_device_fails(self):
        Client.failures = 2
        with self.assertRaises(OSError):
            await ble.write('missing', 'fff3', '00', Client)


class Kept:
    # A client the command loop can connect, write and leave open.
    made = []
    fail_connect = 0
    fail_writes = 0

    def __init__(self, address, timeout):
        self.address = address
        self.timeout = timeout
        self.connected = False
        self.writes = []
        self.began = asyncio.Event()
        type(self).made.append(self)

    async def connect(self):
        self.began.set()
        if type(self).fail_connect:
            type(self).fail_connect -= 1
            raise OSError('disconnected')
        self.connected = True

    async def disconnect(self):
        self.connected = False

    @property
    def is_connected(self):
        return self.connected

    async def write_gatt_char(self, characteristic, data, response):
        self.writes.append((characteristic, data, response))
        if type(self).fail_writes:
            type(self).fail_writes -= 1
            self.connected = False
            raise OSError('disconnected')


class Found:
    def __init__(self, address, name):
        self.address = address
        self.name = name


def reset_kept():
    Kept.made = []
    Kept.fail_connect = 0
    Kept.fail_writes = 0


async def empty_scan(timeout=3):
    return []


async def drive(commands, client_factory, scanner=None, idle_s=60):
    # Send the next command only after the previous one has answered, which is
    # how the Go side uses the helper except when a deadline sends close.
    replies = []
    turn = asyncio.Event()
    turn.set()
    index = {'n': 0}

    async def read_line():
        await turn.wait()
        if index['n'] >= len(commands):
            return None
        command = commands[index['n']]
        index['n'] += 1
        turn.clear()
        return json.dumps(command)

    def write_line(obj):
        replies.append(obj)
        if len(replies) >= index['n']:
            turn.set()

    await asyncio.wait_for(
        ble.command_loop(read_line, write_line, client_factory, scanner or empty_scan, idle_s=idle_s),
        timeout=2,
    )
    return replies


class CommandLoopTests(unittest.IsolatedAsyncioTestCase):
    async def test_second_write_reuses_the_open_client(self):
        reset_kept()
        replies = await drive([
            {'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'},
            {'id': 2, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '02'},
        ], Kept)
        self.assertEqual([reply['ok'] for reply in replies], [True, True])
        self.assertEqual(len(Kept.made), 1)
        self.assertTrue(Kept.made[0].is_connected)
        self.assertEqual(Kept.made[0].writes, [
            ('fff3', bytes.fromhex('01'), False),
            ('fff3', bytes.fromhex('02'), False),
        ])

    async def test_write_failure_reconnects_once(self):
        reset_kept()
        Kept.fail_writes = 1
        replies = await drive([
            {'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'},
        ], Kept)
        self.assertEqual(replies[0]['ok'], True)
        self.assertEqual(len(Kept.made), 2)
        self.assertEqual(Kept.made[1].writes, [('fff3', bytes.fromhex('01'), False)])

    async def test_two_failures_report_the_error(self):
        reset_kept()
        Kept.fail_writes = 2
        replies = await drive([
            {'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'},
        ], Kept)
        self.assertEqual(replies[0]['ok'], False)
        self.assertIn('disconnected', replies[0]['error'])

    async def test_close_drops_the_client(self):
        reset_kept()
        replies = await drive([
            {'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'},
            {'id': 2, 'op': 'close', 'address': 'AA'},
            {'id': 3, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '02'},
        ], Kept)
        self.assertEqual([reply['ok'] for reply in replies], [True, True, True])
        self.assertFalse(Kept.made[0].is_connected)
        self.assertEqual(len(Kept.made), 2)
        self.assertTrue(Kept.made[1].is_connected)

    async def test_close_cancels_a_stuck_connect(self):
        # The first connect waits until disconnect. Close must answer anyway.
        reset_kept()

        class Hanging(Kept):
            async def connect(self):
                self.began.set()
                self.connected = True
                try:
                    await asyncio.Event().wait()
                finally:
                    self.connected = False

        replies = []
        done = asyncio.Event()
        step = {'n': 0}

        async def read_line():
            step['n'] += 1
            if step['n'] == 1:
                return json.dumps({'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'})
            if step['n'] == 2:
                # The client is constructed before connect, so wait until it exists and blocks.
                while not Hanging.made:
                    await asyncio.sleep(0.01)
                await Hanging.made[0].began.wait()
                return json.dumps({'id': 2, 'op': 'close', 'address': 'AA'})
            await done.wait()
            return None

        def write_line(obj):
            replies.append(obj)
            if len(replies) >= 2:
                done.set()

        await asyncio.wait_for(
            ble.command_loop(read_line, write_line, Hanging, empty_scan, idle_s=60),
            timeout=2,
        )
        kinds = {(reply.get('id'), reply.get('ok'), reply.get('error')) for reply in replies}
        self.assertIn((1, False, 'cancelled'), kinds)
        self.assertIn((2, True, None), kinds)
        self.assertFalse(Hanging.made[0].is_connected)

    async def test_scan_uses_the_injected_scanner(self):
        seen = {}

        async def scanner(timeout):
            seen['timeout'] = timeout
            return [Found('AA:BB:CC:DD:EE:01', ''), Found('AA:BB:CC:DD:EE:02', 'Desk')]

        replies = await drive([{'id': 1, 'op': 'scan'}], Kept, scanner=scanner)
        self.assertEqual(seen['timeout'], 3)
        self.assertEqual(replies[0]['devices'], [
            {'address': 'AA:BB:CC:DD:EE:01', 'name': 'AA:BB:CC:DD:EE:01'},
            {'address': 'AA:BB:CC:DD:EE:02', 'name': 'Desk'},
        ])

    async def test_idle_closes_the_client(self):
        reset_kept()
        started = asyncio.Event()

        async def read_line():
            if not read_line.sent:
                read_line.sent = True
                return json.dumps({'id': 1, 'op': 'write', 'address': 'AA', 'char': 'fff3', 'hex': '01'})
            await started.wait()
            # The idle task needs a moment while this read is blocked.
            await asyncio.sleep(0.2)
            return None

        read_line.sent = False
        replies = []

        def write_line(obj):
            replies.append(obj)
            started.set()

        await asyncio.wait_for(
            ble.command_loop(read_line, write_line, Kept, empty_scan, idle_s=0.05),
            timeout=2,
        )
        self.assertEqual(replies[0]['ok'], True)
        self.assertFalse(Kept.made[0].is_connected)

    async def test_unknown_op_and_bad_json_are_errors(self):
        reset_kept()
        pending = asyncio.Queue()
        pending.put_nowait('not-json')
        pending.put_nowait(json.dumps({'id': 2, 'op': 'nope'}))
        replies = []
        done = asyncio.Event()

        async def read_line():
            if not pending.empty():
                return await pending.get()
            await done.wait()
            return None

        def write_line(obj):
            replies.append(obj)
            if len(replies) >= 2:
                done.set()

        await asyncio.wait_for(
            ble.command_loop(read_line, write_line, Kept, empty_scan),
            timeout=2,
        )
        self.assertEqual(replies[0]['ok'], False)
        self.assertEqual(replies[0]['error'], 'invalid json')
        self.assertEqual(replies[1]['error'], 'unknown op')


if __name__ == '__main__':
    unittest.main()
