#!/usr/bin/env python3
"""Prove the finished PATH selects the exact files the fixed-path gates check.

Selection labels and readable files are not proof. The first existing PATH
candidate wins even when it is a broken symlink or unusable, so a bad shadow is
never skipped in favour of a good later entry. Invoked by verify-standard.sh
with the five exact targets in dsh, codex, opencode, claude, pi order.
"""
from __future__ import annotations

import os
import stat
import sys

for name, verified in zip(("dsh", "codex", "opencode", "claude", "pi"), sys.argv[1:]):
    candidate = next(
        (os.path.join(directory, name)
         for directory in os.environ.get("PATH", "").split(os.pathsep)
         if os.path.lexists(os.path.join(directory, name))),
        None,
    )
    try:
        if candidate is None:
            raise ValueError("missing command")
        if not stat.S_ISREG(os.stat(candidate).st_mode) or not os.access(candidate, os.X_OK):
            raise ValueError("not a usable executable regular file")
        if not os.path.samefile(candidate, verified):
            raise ValueError("PATH selects a different file than the exact-version verifier")
    except (OSError, ValueError) as error:
        print(f"verify-standard.sh: {name}: PATH candidate {candidate!r}, verified {verified!r}: {error}", file=sys.stderr)
        sys.exit(1)
    print(f"  {name}: PATH {candidate} -> verified {os.path.realpath(verified)}")
