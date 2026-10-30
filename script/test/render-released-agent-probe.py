#!/usr/bin/env python3
"""Render maintained probe sources and committed input hashes into guest scratch."""
import argparse
import hashlib
import os
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
    # The probe is rendered into a `bash -c` string, not run from a checkout, so
    # its guest Python arrives through this same byte-verified transfer.
    "released-agent-report.py": "script/test/fixtures/released-agent-report.py",
    "released-agent-dsh-layout.py": "script/test/fixtures/released-agent-dsh-layout.py",
    "released-agent-bridge-report.py": "script/test/fixtures/released-agent-bridge-report.py",
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
    "pi": (
        "/opt/agent-vm/pi",
        ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "pnpm-inputs.sha256"),
    ),
    "pi-packages": (
        "/opt/agent-vm/pi-packages",
        ("package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "pnpm-inputs.sha256"),
    ),
}
# The committed sources of each consumer's pinned inputs.
SOURCES_OF_INPUTS = {
    "opencode": "images/tools/opencode/plugins",
    "dsh": "images/tools/dsh",
    "pi": "images/tools/pi",
    "pi-packages": "images/tools/pi/bridge",
}


def input_hashes():
    return {
        consumer: {
            name: hashlib.sha256((ROOT / SOURCES_OF_INPUTS[consumer] / name).read_bytes()).hexdigest()
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
            # `-c -` with stdout dropped is the portable spelling: GNU coreutils
            # and the BSD-derived `sha256sum` a host may carry both accept it,
            # while `--check --status` is GNU-only. The exit status is the control,
            # so dropping the OK lines weakens nothing.
            inserts.append(
                f"printf '%s  %s\\n' '{digest}' '{prefix}/{name}' | sha256sum -c - >/dev/null\n"
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
        {"pi": {"package-lock.json": "a" * 64}},
        {"pi-packages": {"pnpm-inputs.sha256": "not-a-hash"}},
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
    # shape that they depend on (four dsh inputs, no npm lock). The identity
    # assertions moved into the transferred dsh-layout helper, so check them at
    # their maintained source; the byte roundtrip above proves they are what the
    # guest reads.
    probe = (ROOT / "script/test/fixtures/released-agent-probe.sh").read_text()
    layout = (ROOT / SOURCES["released-agent-dsh-layout.py"]).read_text()
    assert 'test ! -e /opt/agent-vm/dsh/package-lock.json' in probe
    assert 'test ! -L /opt/agent-vm/dsh/package-lock.json' in probe
    # Pi and the bridge must be asserted as migrated projects, not as the retired
    # npm pair: four inputs each, and no npm lock even as a dangling link.
    assert 'for project in pi pi-packages' in probe
    assert 'test ! -e "/opt/agent-vm/$project/package-lock.json"' in probe
    assert 'test ! -L "/opt/agent-vm/$project/package-lock.json"' in probe
    assert 'test -r "/opt/agent-vm/$project/package-lock.json"' not in probe
    assert "os.readlink(link) == target" in layout
    assert "os.path.samefile(link, target)" in layout
    assert "node_modules/@deepseek-ai/dsh/lib/bin.js" in layout
    for name, file in SOURCES.items():
        assert not re.search(r"[0-9a-f]{64}", (ROOT / file).read_text()), f"pins are rendered, never committed into {name}"
    assert not re.search(r"[0-9a-f]{64}", probe), "pins are rendered, never committed into the template"
    assert "pnpm-lock.mjs" not in payload and "lockfileVersion" not in payload

    # The transferred report helper IS the released probe's report oracle, so run
    # it against scratch captures instead of the image. Its status loop reads
    # /opt/agent-vm, which this host need not have: a healthy set therefore ends
    # in FileNotFoundError there, and that is the proof every report and stderr
    # check passed first. The agy rows fail earlier, in the report itself.
    values = {'codex': 'rust-v0.159.3', 'opencode': 'v2.0.29', 'claude': '2.1.286',
              'copilot': '1.0.90', 'dsh': '0.1.5-rc.2', 'pnpm': '11.11.0',
              'pi': '1.4.0', 'agy': '1.4.11'}
    reports = {'codex': 'codex-cli 0.159.3\n', 'opencode': 'opencode v2.0.29\n',
               'claude': '2.1.286 (Claude Code)\n',
               'copilot': "GitHub Copilot CLI 1.0.90.\nRun 'copilot update' to check for updates.\n",
               'dsh': '0.1.5-rc.2\n', 'pnpm': '11.11.0\n', 'pi': '1.4.0\n', 'agy': '1.4.11\n'}

    def report_run(mutate=None):
        with tempfile.TemporaryDirectory() as private:
            root = Path(private)
            for tool, report in reports.items():
                (root / (tool + '.out')).write_bytes(report.encode())
                (root / (tool + '.err')).write_bytes(b'')
            if mutate:
                mutate(root)
            return subprocess.run(
                [sys.executable, str(ROOT / SOURCES["released-agent-report.py"]), str(root),
                 *(values[tool] for tool in ('codex', 'opencode', 'claude', 'copilot', 'dsh',
                                             'pnpm', 'pi', 'agy'))],
                timeout=30, capture_output=True, text=True)

    healthy = report_run()
    if Path('/opt/agent-vm').exists():
        print("release report oracle: healthy captures skipped (/opt/agent-vm is present here)")
    else:
        assert healthy.returncode != 0, "healthy captures reach the install-status loop"
        assert 'install-status' in healthy.stderr, healthy.stderr
    for label, report, needle in (
        ('agy wrong version', '1.4.10\n', 'agy exact report'),
        ('agy missing newline', '1.4.11', 'agy exact report'),
        ('agy extra line', '1.4.11\nextra\n', 'agy exact report'),
    ):
        def changed(root, report=report):
            (root / 'agy.out').write_bytes(report.encode())
        result = report_run(changed)
        assert result.returncode != 0 and needle in result.stderr, (label, result.stderr)
    def stray_stderr(root):
        (root / 'agy.err').write_bytes(b'warning: x\n')
    result = report_run(stray_stderr)
    assert result.returncode != 0 and 'agy stderr' in result.stderr, result.stderr

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
