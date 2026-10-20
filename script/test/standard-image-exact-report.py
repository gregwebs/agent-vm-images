#!/usr/bin/env python3
"""Require FILE to be exactly EXPECTED, optionally followed by one newline.

A contradictory trailing line, a second line or an extra blank line must be
rejected: the container audit emits complete reports, so a first-line-only
comparison would certify a report the tool never produced.
"""
from __future__ import annotations

import sys

data = open(sys.argv[1], "rb").read()
expected = sys.argv[2].encode()
sys.exit(0 if data in (expected, expected + b"\n") else 1)
