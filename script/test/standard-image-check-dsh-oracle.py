#!/usr/bin/env python3
"""Validate the independent dsh oracle's JSON report for EXPECTED_VERSION.

The oracle is a separate implementation from the image's build-time
check-runtime.mjs, so a defect in one cannot certify the other. Exit 1 on any
mismatch; malformed JSON is a mismatch, not an error.
"""
from __future__ import annotations

import json
import re
import sys

try:
    report = json.loads(open(sys.argv[1], 'rb').read())
except (OSError, ValueError):
    sys.exit(1)
if not isinstance(report, dict) or report.get('version') != sys.argv[2]:
    sys.exit(1)
version = re.compile(r'[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?')
for field in ('sandbox', 'cordis'):
    if not isinstance(report.get(field), str) or not version.fullmatch(report[field]):
        sys.exit(1)
if report.get('subprocess') != {'stdout': 'independent-dsh\n', 'exitCode': 7, 'disposed': True}:
    sys.exit(1)
