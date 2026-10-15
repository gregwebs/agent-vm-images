#!/usr/bin/env python3
"""Exact frozen-policy metadata routes, separate from native archive traffic."""
import json
from pathlib import Path
import re
import sys


def rules(manifest):
    dependencies = manifest["dependencies"]
    plugin = dependencies["@openchamber/opencode-claude"]
    sdk = dependencies["@anthropic-ai/claude-agent-sdk"]
    platforms = ["darwin-arm64", "darwin-x64", "linux-arm64", "linux-arm64-musl", "linux-x64", "linux-x64-musl", "win32-arm64", "win32-x64"]
    names = ["@openchamber/opencode-claude", "@anthropic-ai/claude-agent-sdk", "zod"]
    names += ["@anthropic-ai/claude-agent-sdk-" + suffix for suffix in platforms]
    versions = {"@openchamber/opencode-claude": plugin, "@anthropic-ai/claude-agent-sdk": sdk, "zod": "4.6.7"}
    versions.update({"@anthropic-ai/claude-agent-sdk-" + suffix: sdk for suffix in platforms})
    paths = ["/" + name.replace("/", "%2F") for name in names]
    for name, version in (("@openchamber/opencode-claude", plugin), ("@anthropic-ai/claude-agent-sdk", sdk), ("zod", "4.6.7")):
        paths.append(f"/{name}/-/{name.split('/')[-1]}-{version}.tgz")
    # Provenance bundles for every package in the locked graph. pnpm fetches these
    # while it enforces the base trust policy, so denying them would mean either a
    # failing audit or a weakened policy. The registry spells the scope separator
    # literally on this route, unlike the %2F packument paths above, and the native
    # platform tarballs stay denied: only their metadata and provenance are needed.
    for name, version in versions.items():
        paths.append(f"/-/npm/v1/attestations/{name}@{version}")
    return {"allow": [{"host": "registry.npmjs.org", "path": "^" + re.escape(path) + "$", "method": "GET"} for path in paths],
            "deny": [], "https_only": True, "learn_redirects": False, "deny_redirects": True}


if __name__ == "__main__":
    print(json.dumps(rules(json.loads(Path(sys.argv[1]).read_bytes()))))
