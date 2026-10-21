#!/usr/bin/env python3
"""Watchdog fixture: a descendant that ignores SIGTERM and records its pid.

Escalation must not depend on the group leader: this process only dies to the
watchdog's SIGKILL, so its pidfile proves the kill reached descendants.
"""
from __future__ import annotations

import os
import signal
import sys
import time

signal.signal(signal.SIGTERM, signal.SIG_IGN)
with open(sys.argv[1], "w") as handle:
    handle.write(str(os.getpid()))
time.sleep(300)
