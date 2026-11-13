#!/usr/bin/env python3
# Read the password from stdin so SSH probes never place it in argv or remote files.
import base64
import sys
import urllib.error
import urllib.request

password = sys.stdin.read().rstrip("\n")
if not password:
    sys.exit("Missing password on stdin")
request = urllib.request.Request("http://100.115.149.100:47822/api/info")
request.add_header("Authorization", "Basic " + base64.b64encode(
    ("opencode:" + password).encode()).decode())
try:
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=10) as response:
        print("Authenticated /api/info:", response.status)
        sys.exit(0 if response.status == 200 else 1)
except (urllib.error.URLError, TimeoutError):
    sys.exit("Authenticated /api/info failed (connection or HTTP error)")
