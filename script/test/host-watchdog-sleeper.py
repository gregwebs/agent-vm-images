#!/usr/bin/env python3
"""Watchdog fixture: an ordinary sleeper that records its pid and waits.

Keeps Python's default SIGTERM disposition so a forwarded TERM terminates it,
proving the watchdog signals the child group and not only the leader.
"""
from __future__ import annotations

import os
import sys
import time

with open(sys.argv[1], "w") as handle:
    handle.write(str(os.getpid()))
time.sleep(300)
