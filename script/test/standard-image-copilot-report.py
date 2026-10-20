#!/usr/bin/env python3
"""Accept copilot's genuine --version record for VERSION and nothing else.

The owning gate accepts the documented update footer on every line after the
banner. Mirror that exact record instead of demanding a single line, while still
rejecting any other extra line.
"""
from __future__ import annotations

import sys

data = open(sys.argv[1], "rb").read()
version = sys.argv[2].encode()
# Reject non-printable bytes: a NUL would be erased by command substitution.
if any(not (b in (9, 10) or 32 <= b <= 126) for b in data):
    sys.exit(1)
lines = data.split(b"\n")
if lines and lines[-1] == b"":
    lines.pop()
banner = b"GitHub Copilot CLI " + version + b"."
footer = b"Run 'copilot update' to check for updates."
if not lines or lines[0] != banner:
    sys.exit(1)
sys.exit(0 if all(line in (b"", footer) for line in lines[1:]) else 1)
