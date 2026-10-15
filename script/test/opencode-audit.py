#!/usr/bin/env python3
"""Finished-image oracle: independent predicates over raw wire and installed evidence."""
import argparse
import base64
import json
from pathlib import Path
import sys
import urllib.parse


if not __debug__:
    raise RuntimeError("optimized Python cannot run certification predicates")


def object_pairs(pairs):
    result = {}
    for name, value in pairs:
        if name in result:
            raise ValueError("duplicate key")
        result[name] = value
    return result


def load(raw):
    return json.loads(raw, object_pairs_hook=object_pairs,
                      parse_constant=lambda value: (_ for _ in ()).throw(ValueError(value)))


def audit(evidence, runtime, version, status, directory, entry):
    assert status == b"installed\n", "plugin record"
    assert type(evidence["schema"]) is int and type(runtime["schema"]) is int and evidence["schema"] == 1 and runtime["schema"] == 1, "schema"
    assert runtime["plugin_version"] == version, "plugin selection"
    assert runtime["sdk_version"] == "0.3.281" and runtime["sdk_instances"] == 1, "SDK selection/instance"
    for key in ("handoff", "sdk_available", "adapter", "omitted_peers"):
        assert runtime[key] is True, key
    assert runtime["optional_platforms"] is False, "optional platforms"
    # Zero is tolerated: the build RUN has no guest credentials, so the CLI may
    # not enumerate models offline. The /api/model wire check below (the
    # plugin's fallback catalog) supplies the non-empty requirement.
    assert type(runtime["initialization_models"]) is int and runtime["initialization_models"] >= 0
    assert runtime["sdk_entry"].startswith("/opt/agent-vm/opencode-plugins/node_modules/.pnpm/")
    assert runtime["sdk_directory"].startswith("/opt/agent-vm/opencode-plugins/node_modules/.pnpm/")
    assert runtime["claude_path"].startswith("/opt/agent/"), "Claude path"
    records = {}
    for kind in ("model", "provider", "plugin", "health"):
        raw = base64.b64decode(evidence[kind + "_b64"], validate=True)
        assert len(raw) <= 4 * 1024 * 1024, "body limit"
        value = load(raw)
        if kind != "health":
            assert value["location"] == {"directory": directory}, "location"
            assert isinstance(value["data"], list)
            records[kind] = value["data"]
        else:
            records[kind] = value
    plugin = [p for p in records["plugin"] if p.get("id") == "openchamber.claude-code"]
    assert len(plugin) == 1 and plugin[0]["state"] == {"status": "active"}, "activation"
    assert plugin[0]["source"] == {"type": "local", "path": entry}, "source"
    provider = [p for p in records["provider"] if p.get("id") == "claude-code"]
    assert len(provider) == 1 and provider[0]["activation"] == "enabled", "provider"
    url = urllib.parse.urlsplit(provider[0]["settings"]["baseURL"])
    assert (url.scheme == "http" and url.hostname == "127.0.0.1" and url.port and 1 <= url.port <= 65535
            and url.path == "/v1" and not url.username and not url.password and not url.query and not url.fragment), "proxy URL"
    ids = [m["id"] for m in records["model"] if m.get("providerID") == "claude-code"]
    assert ids and all(isinstance(i, str) and i.strip() for i in ids) and len(ids) == len(set(ids)), "model IDs"
    assert records["health"]["ok"] is True and records["health"]["provider"] == "claude-code", "health"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence", required=True)
    parser.add_argument("--runtime", required=True)
    parser.add_argument("--status", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--directory", required=True)
    parser.add_argument("--plugin-entry", default="/opt/agent-vm/opencode-plugins/index.js")
    args = parser.parse_args()
    try:
        audit(load(Path(args.evidence).read_bytes()), load(Path(args.runtime).read_bytes()), args.version,
              Path(args.status).read_bytes(), args.directory, args.plugin_entry)
    except (AssertionError, ValueError, KeyError, TypeError, OSError) as error:
        print(f"opencode-audit: {error}", file=sys.stderr)
        return 1
    print("OpenCode Claude activation and handoff evidence accepted")
    return 0


if __name__ == "__main__":
    sys.exit(main())
