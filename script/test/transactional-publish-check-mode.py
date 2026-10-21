#!/usr/bin/env python3
"""Assert a restored destination kept the original file mode.

Byte equality is checked separately by the caller's `cmp`; a rollback that
republished the right bytes with the wrong mode is still a regression.
"""
from __future__ import annotations

import os
import stat
import sys

assert stat.S_IMODE(os.stat(sys.argv[1]).st_mode) == stat.S_IMODE(os.stat(sys.argv[2]).st_mode)
