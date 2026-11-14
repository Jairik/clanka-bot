#!/usr/bin/env python3
# Verify credential guards and the process boundary with a fake OpenCode binary.
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / 'opencode-serve.sh'


class ServeTest(unittest.TestCase):
    def test_credentials_and_bind(self):
        with tempfile.TemporaryDirectory() as directory:
            home = Path(directory)
            config = home / '.config/clanka'
            config.mkdir(parents=True)
            password = config / 'opencode-server.pass'
            fake = home / 'opencode'
            fake.write_text('''#!/usr/bin/env python3
# Assert the child receives the server password only in its environment.
import os, sys
assert 'OPENCODE_PASSWORD' not in os.environ
assert os.environ['OPENCODE_SERVER_PASSWORD'] == 'test-secret'
assert sys.argv[1:] == ['serve', '--hostname', '100.115.149.100', '--port', '47822']
print('safe launch')
''')
            fake.chmod(0o700)
            env = dict(os.environ, HOME=str(home), PATH=f'{home}:' + os.environ['PATH'],
                       OPENCODE_PASSWORD='wrong-password', OPENCODE_SERVER_PASSWORD='inherited')

            def run():
                return subprocess.run(['bash', str(SCRIPT)], env=env, capture_output=True, text=True)

            self.assertNotEqual(run().returncode, 0)
            password.write_text('test-secret\n')
            password.chmod(0o644)
            self.assertNotEqual(run().returncode, 0)
            password.chmod(0o600)
            result = run()
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout.strip(), 'safe launch')
            self.assertNotIn('test-secret', result.stderr)
            password.write_text('')
            self.assertNotEqual(run().returncode, 0)
            password.unlink()
            password.symlink_to(fake)
            self.assertNotEqual(run().returncode, 0)


if __name__ == '__main__':
    unittest.main()
