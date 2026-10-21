#!/usr/bin/env python3
"""Assert the bridge registered exactly one canonical provider notification.

BRIDGE_OUT is the probe's `pi --mode rpc` capture: JSON notification lines plus
the guest warning. A malformed notification must raise, never be skipped.
"""
from __future__ import annotations

import json
import re
import sys

lines = open(sys.argv[1]).read().splitlines()
marks = []
for line in lines:
    if line.startswith('{'):
        line = json.loads(line).get('message', '')
    if 'RELEASE-BRIDGE' in line:
        marks.append(line)
assert len(marks) == 1 and re.fullmatch(r'RELEASE-BRIDGE models=[1-9][0-9]*', marks[0])
assert any('agent-vm: signing in here' in line for line in lines)
