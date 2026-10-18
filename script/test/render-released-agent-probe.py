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
# Consumer-qualified input pins. The guest path is a literal whitelist, so a
# hash scalar can never steer a check at another file or directory.
INPUTS = {
    "opencode": (
        "/opt/agent-vm/opencode-plugins",
        ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"),
    ),
    "dsh": (
        "/opt/agent-vm/dsh",
        ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "pnpm-inputs.sha256"),
    ),
}


def input_hashes():
    sources = {"opencode": "images/tools/opencode/plugins", "dsh": "images/tools/dsh"}
    return {
        consumer: {
            name: hashlib.sha256((ROOT / sources[consumer] / name).read_bytes()).hexdigest()
            for name in INPUTS[consumer][1]
        }
        for consumer in INPUTS
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
    for consumer, inputs in hashes.items():
        if consumer not in INPUTS:
            raise ValueError("unknown probe consumer")
        prefix, allowed = INPUTS[consumer]
        for name, digest in inputs.items():
            if name not in allowed or not re.fullmatch(r"[0-9a-f]{64}", digest):
                raise ValueError("invalid committed-input hash")
            inserts.append(
                f"printf '%s  %s\\n' '{digest}' '{prefix}/{name}' | sha256sum --check --status\n"
            )
    result = template.replace(MARKER, "".join(inserts))
    if len(result.encode()) >= 64 * 1024:
        raise ValueError("rendered probe exceeds 64 KiB")
    return result


def maintained_payload():
    return render((ROOT / "script/test/fixtures/released-agent-probe.sh").read_text(),
                  {name: (ROOT / file).read_text() for name, file in SOURCES.items()},
                  input_hashes())


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
    unsafe = [
        {"opencode": {"package.json": "not-a-hash"}},
        {"dsh": {"unsafe'name": "a" * 64}},
        {"dsh": {"package-lock.json": "a" * 64}},
        {"opencode": {"pnpm-inputs.sha256": "a" * 64}},
        {"unmigrated": {"package.json": "a" * 64}},
    ]
    for hashes in unsafe:
        try:
            render(MARKER, {}, hashes)
        except ValueError:
            pass
        else:
            raise AssertionError("unsafe hash scalar passed")

    # The guest assertions themselves cannot run on this host: pin the probe
    # shape that they depend on (four dsh inputs, no npm lock, direct bin).
    probe = (ROOT / "script/test/fixtures/released-agent-probe.sh").read_text()
    assert "for project in dsh pi pi-packages" not in probe
    assert 'test ! -e /opt/agent-vm/dsh/package-lock.json' in probe
    assert 'test ! -L /opt/agent-vm/dsh/package-lock.json' in probe
    assert "os.readlink(link) == target" in probe
    assert "os.path.samefile(link, target)" in probe
    assert "node_modules/@deepseek-ai/dsh/lib/bin.js" in probe
    assert not re.search(r"[0-9a-f]{64}", probe), "pins are rendered, never committed into the template"
    assert "pnpm-lock.mjs" not in payload and "lockfileVersion" not in payload

    def hash_probe(mutate=None):
        with tempfile.TemporaryDirectory() as private:
            root = Path(private)
            for consumer, (_, names) in INPUTS.items():
                (root / consumer).mkdir()
                for name in names:
                    (root / consumer / name).write_bytes(
                        f"reviewed {consumer} {name}\n".encode())
            hashes = {consumer: {name: hashlib.sha256((root / consumer / name).read_bytes()).hexdigest()
                                 for name in names}
                      for consumer, (_, names) in INPUTS.items()}
            if mutate:
                mutate(root)
            commands = render(MARKER, {}, hashes)
            for consumer, (prefix, _) in INPUTS.items():
                commands = commands.replace(prefix + "/", str(root / consumer) + "/")
            return subprocess.run(["bash", "-c", "set -euo pipefail\n" + commands],
                                  timeout=10, capture_output=True).returncode

    assert hash_probe() == 0, "healthy hashes must pass"
    for consumer, (_, names) in INPUTS.items():
        for name in names:
            def changed(root, consumer=consumer, name=name):
                (root / consumer / name).write_bytes(b"drifted bytes\n")
            assert hash_probe(changed) != 0, f"{consumer}/{name} drift must fail"
            def removed(root, consumer=consumer, name=name):
                (root / consumer / name).unlink()
            assert hash_probe(removed) != 0, f"missing {consumer}/{name} must fail"
    print(f"release renderer: exact bytes, marker/delimiter/size/hash rejection; payload {len(payload.encode())} bytes")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    else:
        sys.stdout.write(maintained_payload())
