#!/usr/bin/env python3
"""Owned report/access fixture for the real verifier, not runtime policy proof."""
import hashlib
import json
from pathlib import Path
import shutil
import sys

FIXTURES = Path(__file__).resolve().parent


def stage(destination: Path, version: str, pnpm: Path | None = None) -> Path:
    root = Path(__file__).resolve().parents[4]
    source = destination / 'pnpm-source'
    shutil.copytree(root / 'images/pnpm', source)
    prefix = destination / 'opt/agent-vm/pnpm'
    commands = destination / 'usr/local/bin'
    node = destination / 'usr/bin/node'
    commands.mkdir(parents=True)
    (prefix / 'bin').mkdir(parents=True)
    (prefix / 'dist').mkdir()
    node.parent.mkdir(parents=True, exist_ok=True)
    shutil.copyfile(FIXTURES / 'node-stub.sh', node)
    node.chmod(0o755)
    for entry in ('pnpm.mjs', 'pnpx.mjs'):
        (prefix / 'bin' / entry).write_text('fixture entry')
        (prefix / 'bin' / entry).chmod(0o755)
    bundle = prefix / 'dist/pnpm.mjs'
    bundle.write_text('fixture pristine bundle')
    pin = json.loads((source / 'pin.json').read_text())
    pin.update(version=version, url=f'https://registry.npmjs.org/pnpm/-/pnpm-{version}.tgz',
               upstreamTag='v' + version, bundleSha256=hashlib.sha256(bundle.read_bytes()).hexdigest())
    for path in (source / 'pin.json', prefix / 'pin.json'):
        path.write_text(json.dumps(pin))
    launcher = commands / 'pnpm'
    program = (FIXTURES / 'launcher-stub.sh').read_bytes()
    if pnpm:
        program = pnpm.read_bytes()
        pnpm.unlink()
        pnpm.symlink_to(launcher)
    launcher.write_bytes(program)
    launcher.chmod(0o755)
    (source / 'launcher.sh').write_bytes(program)
    for alias in ('pnx', 'pnpx'):
        (commands / alias).symlink_to('pnpm')
    for path in [destination, *destination.rglob('*')]:
        if not path.is_symlink():
            path.chmod(0o755 if path.is_dir() or path.stat().st_mode & 0o111 else 0o644)
    return source


if __name__ == '__main__':
    stage(Path(sys.argv[1]), sys.argv[2], Path(sys.argv[3]) if len(sys.argv) == 4 else None)
