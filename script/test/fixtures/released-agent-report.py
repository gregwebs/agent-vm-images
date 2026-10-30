#!/usr/bin/env python3
"""Assert the guest tools printed their exact committed version records.

TMP is the probe's private scratch directory holding `TOOL.out`/`TOOL.err`; the
ten selections follow in codex, opencode, claude, copilot, dsh, pnpm, pi, agy
order (bridge and opencode-claude are checked by their own helpers). The
released probe requires a final newline on every report, which is stricter than
the finished-image audit, so these helpers stay separate.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
codex, opencode, claude, copilot, dsh, pnpm, pi, agy = sys.argv[2:]
expected = {'codex': 'codex-cli ' + codex.removeprefix('rust-v'),
            'opencode': 'opencode ' + opencode, 'claude': claude + ' (Claude Code)',
            'dsh': dsh, 'pnpm': pnpm, 'pi': pi, 'agy': agy}
for tool, value in expected.items():
    assert (root / (tool + '.out')).read_bytes() == (value + '\n').encode(), tool + ' exact report'
lines = (root / 'copilot.out').read_bytes().splitlines()
assert lines and lines[0] == ('GitHub Copilot CLI ' + copilot + '.').encode()
assert all(x in (b'', b"Run 'copilot update' to check for updates.") for x in lines[1:])
for tool in (*expected, 'copilot'):
    data = (root / (tool + '.err')).read_bytes()
    assert all(x in (9, 10) or 32 <= x <= 126 for x in data), tool + ' stderr'
    assert not any(re.fullmatch(rb'(codex-cli .*|GitHub Copilot CLI .*|.* \(Claude Code\)|opencode v[0-9]+\.[0-9]+\.[0-9]+.*|v?[0-9]+\.[0-9]+\.[0-9]+.*)', x)
                   for x in data.splitlines()), tool + ' conflicting stderr'
# agy writes nothing on stderr at all: its build gate refuses a printable
# diagnostic beside a correct version, so the released probe does too.
assert (root / 'agy.err').read_bytes() == b'', 'agy stderr'
for tool in ('codex', 'opencode', 'claude', 'copilot', 'dsh', 'pi', 'agy',
             'pi-claude-bridge', 'opencode-claude'):
    assert Path('/opt/agent-vm/install-status', tool).read_bytes() == b'installed\n', tool + ' status'
