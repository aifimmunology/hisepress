#!/usr/bin/env python3
"""Test helper: starts a long-running child, records its pid, then sleeps.
Used to check that a timed-out exec step kills the whole process group."""
import json
import subprocess
import sys
import time

req = json.load(sys.stdin)
child = subprocess.Popen(["sleep", "30"])
with open(req["args"]["pidfile"], "w") as f:
    f.write(str(child.pid))
time.sleep(30)
