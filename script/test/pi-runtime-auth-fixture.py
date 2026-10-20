#!/usr/bin/env python3
"""Write a Codex auth fixture of exactly TOTAL_BYTES bytes.

The extension refuses credentials above 65536 bytes, so the suite needs an
inclusive fencepost at exactly 64 KiB and one byte over. 64 KiB of padding is
not reviewable fixture data, so it is generated here with no trailing newline.
"""
from __future__ import annotations

import json
import sys

path, total = sys.argv[1], int(sys.argv[2])
head = json.dumps({"tokens": {
    "access_token": "msb-openai-placeholder-a-v2",
    "refresh_token": "msb-openai-placeholder-r-v2",
    "account_id": "acct-~~~?",
}})[:-1] + ', "pad": "'
tail = '"}'
pad = total - len(head) - len(tail)
assert pad > 0, pad
with open(path, "w") as fh:
    fh.write(head + "x" * pad + tail)
