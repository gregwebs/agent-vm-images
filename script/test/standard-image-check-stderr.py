#!/usr/bin/env python3
"""Reject a tool's stderr capture that contradicts its version report.

Stderr must contain no version-banner line for NAME and no non-printable byte;
printable diagnostics are allowed. Exit 1 means "contradictory stderr", 0 means
"harmless diagnostics"; a read error is an ordinary non-zero exception.
"""
from __future__ import annotations

import re
import sys

name, path = sys.argv[1:]
data = open(path, "rb").read()
if any(not (b in (9, 10) or 32 <= b <= 126) for b in data):
    sys.exit(1)
patterns = {
    "codex": rb"codex-cli [0-9A-Za-z.+-]+",
    "opencode": rb"(?:opencode v|v?)[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*",
    "claude": rb"[0-9][0-9A-Za-z.+-]* \(Claude Code\)",
    "copilot": rb"GitHub Copilot CLI .*",
}
pattern = patterns.get(name, rb"v?[0-9]+\.[0-9]+\.[0-9]+[0-9A-Za-z.+-]*")
sys.exit(1 if any(re.fullmatch(pattern, line) for line in data.splitlines()) else 0)
