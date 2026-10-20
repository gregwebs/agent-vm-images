#!/usr/bin/env python3
"""Extract the literal `id: bind` run block from the boot verification workflow.

The owning suite executes the extracted block instead of a copy of it, so a
workflow edit that moves or reshapes the block fails here rather than silently
testing stale shell. Output bytes must match the workflow's block exactly.
"""
from __future__ import annotations

import sys
from pathlib import Path

lines = Path(sys.argv[1]).read_text().splitlines(keepends=True)
assert lines.count('        id: bind\n') == 1
start = lines.index('        id: bind\n') + 1
while lines[start] != '        run: |\n':
    assert not lines[start].startswith('      - '), 'bind has no literal run block'
    start += 1
body = []
for line in lines[start + 1:]:
    if line.strip() and not line.startswith('          '):
        break
    body.append(line[10:] if line.strip() else line)
assert body, 'empty bind block'
Path(sys.argv[2]).write_text(''.join(body))
