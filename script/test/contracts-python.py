#!/usr/bin/env python3
"""Parse every tracked repository Python source under images/, script/ and examples/.

Run from the repository root: the walk is relative to the current directory.
Generated scratch (``target``) and bytecode caches are skipped so an example's
build-only helper and its tests cannot escape syntax coverage.
The two assertions below keep the retired layouts from reappearing: no
tool-local `contract` helper and no per-tool product Dockerfile.
"""
from __future__ import annotations

import ast
from pathlib import Path

for root in ('images', 'script', 'examples'):
    for p in Path(root).rglob('*.py'):
        if 'target' not in p.parts and '__pycache__' not in p.parts:
            ast.parse(p.read_text(), filename=str(p))
assert not list(Path('images/tools').glob('*/contract')), 'tool-local helpers must not reappear'
assert not list(Path('images/tools').glob('*/Dockerfile')), 'no per-tool products'
