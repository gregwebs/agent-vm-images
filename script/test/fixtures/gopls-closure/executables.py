#!/usr/bin/env python3
"""The executable allowlist a real Go child needs, in one place.

A Go build starts ``compile``, ``link``, ``asm`` and friends from GOTOOLDIR by
absolute path, and every fixture boundary that has to run one must allowlist the
same set. This module deliberately imports nothing from the code under test: a
launcher computes its allowlist *before* the guard is installed.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path


def go_executables(go_root: Path) -> list[str]:
    """The pinned go, every one of its own tools, and this interpreter, resolved."""
    allowed = [os.path.realpath(str(go_root / 'bin' / 'go')),
               os.path.realpath(sys.executable)]
    for directory in sorted((go_root / 'pkg' / 'tool').glob('*')):
        if not directory.is_dir():
            continue
        for tool in sorted(directory.iterdir()):
            if tool.is_file() and os.access(tool, os.X_OK):
                allowed.append(os.path.realpath(str(tool)))
    return sorted(set(allowed))
