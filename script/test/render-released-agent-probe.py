#!/usr/bin/env python3
"""Render maintained probe sources and committed input hashes into guest scratch."""
import argparse
import hashlib
from pathlib import Path
import re
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
MARKER = "# OPENCODE_PROBE_SOURCES\n"
SOURCES = {
    "probe-opencode.py": "images/tools/opencode/probe-opencode.py",
    "check-claude-runtime.mjs": "images/tools/opencode/check-claude-runtime.mjs",
    "opencode-audit.py": "script/test/opencode-audit.py",
}


def render(template, sources, hashes):
    if template.count(MARKER) != 1:
        raise ValueError("expected exactly one probe-source marker")
    inserts = []
    for index, (name, source) in enumerate(sources.items()):
        delimiter = f"AGENT_VM_OPENCODE_SOURCE_{index}"
        if delimiter in source.splitlines() or not source.endswith("\n"):
            raise ValueError("heredoc delimiter collision or missing final newline")
        inserts.append(f'cat >"$tmp/{name}" <<\'{delimiter}\'\n{source}{delimiter}\n')
    for name, digest in hashes.items():
        if name not in ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml") or not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("invalid committed-input hash")
        inserts.append(f"printf '%s  %s\\n' '{digest}' '/opt/agent-vm/opencode-plugins/{name}' | sha256sum --check --status\n")
    result = template.replace(MARKER, "".join(inserts))
    if len(result.encode()) >= 64 * 1024:
        raise ValueError("rendered probe exceeds 64 KiB")
    return result


def maintained_payload():
    return render((ROOT / "script/test/fixtures/released-agent-probe.sh").read_text(),
                  {name: (ROOT / file).read_text() for name, file in SOURCES.items()},
                  {name: hashlib.sha256((ROOT / "images/tools/opencode/plugins" / name).read_bytes()).hexdigest()
                   for name in ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml")})


def self_test():
    payload = maintained_payload()
    for index, (name, file) in enumerate(SOURCES.items()):
        delimiter = f"AGENT_VM_OPENCODE_SOURCE_{index}"
        extracted = payload.split(f"<<'{delimiter}'\n", 1)[1].split(f"{delimiter}\n", 1)[0]
        assert extracted.encode() == (ROOT / file).read_bytes(), name
    for template, sources in (("", {}), (MARKER * 2, {}),
                              (MARKER, {"x": "AGENT_VM_OPENCODE_SOURCE_0\n"}),
                              (MARKER, {"x": "x" * 65536 + "\n"})):
        try:
            render(template, sources, {})
        except ValueError:
            pass
        else:
            raise AssertionError("unsafe renderer input passed")
    for hashes in ({"package.json": "not-a-hash"}, {"unsafe'name": "a" * 64}):
        try:
            render(MARKER, {}, hashes)
        except ValueError:
            pass
        else:
            raise AssertionError("unsafe hash scalar passed")
    assert "AGENT_VM_OPENCODE_HASHES" not in payload
    with tempfile.TemporaryDirectory() as private:
        fixture = Path(private) / "package.json"
        fixture.write_bytes(b'reviewed bytes\n')
        commands = render(MARKER, {}, {"package.json": hashlib.sha256(fixture.read_bytes()).hexdigest()})
        commands = commands.replace("/opt/agent-vm/opencode-plugins/", private + "/")
        assert subprocess.run(["bash", "-c", commands], timeout=10).returncode == 0
        fixture.write_bytes(b'changed bytes\n')
        assert subprocess.run(["bash", "-c", commands], timeout=10).returncode != 0
    print(f"release renderer: exact bytes, marker/delimiter/size/hash rejection; payload {len(payload.encode())} bytes")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        sys.stdout.write(maintained_payload())
