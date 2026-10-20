#!/usr/bin/env python3
"""Apply one malformed-metadata variant to a fixture release document.

Each variant is a shape the inherited hand-written scanner accepted: a missing
comma, trailing garbage, a duplicate tag_name and contradictory per-asset
digests. The owning suite requires each to be a hard metadata failure.
"""
from __future__ import annotations

import json
import sys

path, variant = sys.argv[1], sys.argv[2]
text = open(path).read()
if variant == "missing-comma":
    text = text.replace(',"assets"', ' "assets"', 1)
elif variant == "trailing-garbage":
    text = text + "this is not JSON\n"
elif variant == "duplicate-tag":
    text = text.replace('"assets":', '"tag_name":"rust-v9.9.9","assets":', 1)
elif variant == "duplicate-digest":
    document = json.loads(text)
    document["assets"].append({"name": document["assets"][0]["name"], "digest": "sha256:" + "0" * 64})
    text = json.dumps(document) + "\n"
open(path, "w").write(text)
