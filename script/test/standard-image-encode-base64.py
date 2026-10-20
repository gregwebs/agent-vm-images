#!/usr/bin/env python3
"""Write the base64 of FILE, or of stdin when no file is given.

The one encoder shared by the host predicates and the injected container
capture helper. Output is the base64 text and nothing else: no trailing newline,
because both the container audit and the self-test compare it byte for byte.
"""
from __future__ import annotations

import base64
import sys

if len(sys.argv) > 1:
    with open(sys.argv[1], 'rb') as handle:
        data = handle.read()
else:
    data = sys.stdin.buffer.read()
sys.stdout.write(base64.b64encode(data).decode())
