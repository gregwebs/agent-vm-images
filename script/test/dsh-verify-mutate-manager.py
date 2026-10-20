#!/usr/bin/env python3
"""Point a staged dsh package.json at a disagreeing packageManager version.

The image layer asserts that packageManager equals the base pin, so this
mutated manifest must fail the real gate rather than a rewritten copy of it.
"""
from __future__ import annotations

import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
manifest = json.loads(path.read_text())
manifest['packageManager'] = 'pnpm@11.6.0'
path.write_text(json.dumps(manifest, indent=2) + '\n')
