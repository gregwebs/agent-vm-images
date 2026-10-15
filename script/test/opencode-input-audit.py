#!/usr/bin/env python3
"""Independent oracle for the shipped OpenCode plugin inputs.

The manifest is JSON, so the policy is re-checked structurally here. The three
checked-in pnpm install inputs are reviewed bytes rather than interpreted YAML,
so this oracle re-computes their pins with hashlib: an independent implementation
of the same guarantee the JS checker enforces, which is what keeps it an oracle
instead of a restatement. What neither oracle proves is the *shape* of a freshly
resolved graph on re-pin; the installed graph is checked later by
check-claude-runtime.mjs and the egress profile, not at the re-pin step.
"""
import hashlib
import json
from pathlib import Path
import re
import sys


if not __debug__:
    raise RuntimeError("optimized Python cannot run certification predicates")

PINNED = ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml")
SDK = "@anthropic-ai/claude-agent-sdk"
PLUGIN = "@openchamber/opencode-claude"


def audit(root, version):
    manifest = json.loads((root / "package.json").read_bytes())
    assert manifest == {
        "name": "agent-vm-guest-opencode-plugins",
        "version": "0.0.0",
        "private": True,
        "type": "module",
        "packageManager": "pnpm@11.11.0",
        "dependencies": {SDK: "0.3.281", PLUGIN: version},
    }, "manifest is not the reviewed policy"
    pins = {}
    for line in (root / "pnpm-inputs.sha256").read_text().split("\n"):
        if not line:
            continue
        match = re.fullmatch(r"([0-9a-f]{64})  (\S+)", line)
        assert match, f"malformed pin line: {line!r}"
        assert match[2] not in pins, f"duplicate pin: {match[2]}"
        pins[match[2]] = match[1]
    assert sorted(pins) == sorted(PINNED), f"pins must cover exactly {PINNED}"
    for name in PINNED:
        actual = hashlib.sha256((root / name).read_bytes()).hexdigest()
        assert actual == pins[name], f"{name} does not match its reviewed pin"


if __name__ == "__main__":
    try:
        audit(Path(sys.argv[1]), sys.argv[2])
    except (AssertionError, KeyError, TypeError, ValueError, OSError) as error:
        sys.exit(f"OpenCode input audit failed: {error}")
