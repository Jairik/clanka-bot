# Exercise reconnects and characteristic writes without a Bluetooth adapter.
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

if __name__ == '__main__':
    unittest.main()
