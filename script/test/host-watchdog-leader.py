#!/usr/bin/env python3
"""Watchdog fixture: a group leader that spawns the SIGTERM-ignoring child.

The child inherits this process group on purpose (no session, no exec): the
watchdog must kill the whole group rather than only the leader.
"""
from __future__ import annotations

import subprocess
import sys
import time

subprocess.Popen([sys.executable, sys.argv[1], sys.argv[2]])
time.sleep(300)
