#!/usr/bin/env python3
"""Build-only artifact authentication and publication; never edits upstream bytes."""
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from typing import TypedDict, cast


class Pin(TypedDict):
    version: str
    url: str
    integrity: str
    sha512: str
    bundleSha256: str
    upstreamTag: str
    upstreamCommit: str


def read_pin(path: Path) -> Pin:
    raw = json.loads(path.read_text())
    fields = Pin.__annotations__
    if not isinstance(raw, dict) or set(raw) != set(fields):
        raise ValueError('invalid pin fields')
    if not all(isinstance(raw[key], str) for key in fields):
        raise ValueError('pin fields must be strings')
    pin = cast(Pin, raw)
    if not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', pin['version']):
        raise ValueError('pin must be exact stable version')
    if pin['url'] != f"https://registry.npmjs.org/pnpm/-/pnpm-{pin['version']}.tgz":
        raise ValueError('unexpected archive URL')
    for key, length in [('sha512', 128), ('bundleSha256', 64), ('upstreamCommit', 40)]:
        if not re.fullmatch('[0-9a-f]{' + str(length) + '}', pin[key]):
            raise ValueError('invalid digest/commit: ' + key)
    if pin['integrity'] != 'sha512-' + base64.b64encode(bytes.fromhex(pin['sha512'])).decode():
        raise ValueError('SRI and hex digest disagree')
    if pin['upstreamTag'] != 'v' + pin['version']:
        raise ValueError('upstream tag disagrees')
    return pin


def authenticate(archive: Path, pin: Pin) -> None:
    if hashlib.sha512(archive.read_bytes()).hexdigest() != pin['sha512']:
        raise ValueError('archive SHA-512 mismatch')


def extract(archive: Path, scratch: Path, pin: Pin) -> Path:
    with tarfile.open(archive, 'r:gz') as tar:
        members = tar.getmembers()
        seen: set[str] = set()
        for member in members:
            parts = PurePosixPath(member.name).parts
            if (not parts or parts[0] != 'package' or '..' in parts
                    or member.name.startswith('/') or member.name in seen
                    or not (member.isfile() or member.isdir())):
                raise ValueError('unsafe archive member: ' + member.name)
            seen.add(member.name)
        # No links or special files can redirect extraction outside owned scratch.
        tar.extractall(scratch, members=members)
    package = scratch / 'package'
    metadata = json.loads((package / 'package.json').read_text())
    if metadata.get('name') != 'pnpm' or metadata.get('version') != pin['version']:
        raise ValueError('package selection mismatch')
    for name in ('bin/pnpm.mjs', 'bin/pnpx.mjs', 'dist/pnpm.mjs', 'LICENSE'):
        path = package / name
        if not path.is_file() or path.is_symlink() or not path.stat().st_size:
            raise ValueError('missing regular upstream file: ' + name)
    if 'MIT' not in (package / 'LICENSE').read_text():
        raise ValueError('upstream MIT license missing')
    if hashlib.sha256((package / 'dist/pnpm.mjs').read_bytes()).hexdigest() != pin['bundleSha256']:
        raise ValueError('pristine bundle SHA-256 mismatch')
    return package


def fixture_destination(root: Path) -> tuple[Path, Path]:
    fixture = Path(os.environ['AGENT_VM_FIXTURE_ROOT']).resolve(strict=True)
    if fixture == Path('/') or fixture.stat().st_uid != os.getuid():
        raise ValueError('fixture root must be invocation-owned')
    resolved = root.resolve(strict=True)
    if fixture not in resolved.parents:
        raise ValueError('destination outside fixture root')
    prefix, commands = resolved / 'opt/agent-vm/pnpm', resolved / 'usr/local/bin'
    # Reject redirects in existing descendants before mkdir/copy/chmod can follow them.
    for target in (prefix, commands, *(commands / name for name in ('pnpm', 'pnx', 'pnpx'))):
        for path in (target, *target.parents):
            if path == resolved:
                break
            if path.is_symlink():
                # Alias links are permitted for verification only when confined to the root.
                physical = path.resolve(strict=False)
                if resolved not in physical.parents:
                    raise ValueError('fixture descendant escapes destination: ' + str(path))
            physical = path.resolve(strict=False)
            if resolved not in physical.parents:
                raise ValueError('fixture descendant escapes destination: ' + str(path))
    return prefix, commands


def main() -> None:
    if len(sys.argv) == 3 and sys.argv[1] == '--validate-fixture':
        fixture_destination(Path(sys.argv[2]))
        return
    if len(sys.argv) != 1:
        raise ValueError('no production positional arguments supported')
    source = Path(os.environ.get('PNPM_SOURCE_DIR', '/tmp/pnpm-source'))
    pin = read_pin(source / 'pin.json')
    destination = os.environ.get('PNPM_FIXTURE_DESTINATION')
    if destination:
        prefix, commands = fixture_destination(Path(destination))
    else:
        prefix, commands = Path('/opt/agent-vm/pnpm'), Path('/usr/local/bin')
        if os.getuid() != 0:
            raise ValueError('production bootstrap requires root')
    for path in (prefix, *(commands / name for name in ('pnpm', 'pnx', 'pnpx'))):
        if os.path.lexists(path):
            raise ValueError('refusing conflicting destination: ' + str(path))
    with tempfile.TemporaryDirectory(prefix='pnpm-bootstrap-') as directory:
        scratch = Path(directory)
        archive = scratch / 'pnpm.tgz'
        subprocess.run(['curl', '--fail', '--silent', '--show-error', '--location',
                        '--proto', '=https', '--connect-timeout', '15', '--max-time', '180',
                        '--retry', '2', '--retry-max-time', '360', pin['url'], '-o', str(archive)],
                       check=True)
        authenticate(archive, pin)
        package = extract(archive, scratch, pin)
        node = '/usr/bin/node'
        if not destination:
            subprocess.run([node, str(source / 'node-version-check.js')], check=True)
        prefix.parent.mkdir(parents=True, exist_ok=True)
        commands.mkdir(parents=True, exist_ok=True)
        shutil.copytree(package, prefix)
        for path in (prefix, *prefix.rglob('*')):
            mode = path.stat().st_mode
            path.chmod(0o755 if path.is_dir() or mode & 0o111 else 0o644)
        for name in ('pnpm.mjs', 'pnpx.mjs'):
            (prefix / 'bin' / name).chmod(0o755)
        shutil.copyfile(source / 'pin.json', prefix / 'pin.json')
        (prefix / 'pin.json').chmod(0o644)
        for parent in (prefix.parent, prefix.parent.parent, commands):
            parent.chmod(0o755)
        shutil.copyfile(source / 'launcher.sh', commands / 'pnpm')
        (commands / 'pnpm').chmod(0o755)
        for name in ('pnx', 'pnpx'):
            (commands / name).symlink_to('pnpm')
        if not destination:
            subprocess.run([str(commands / 'pnpm'), '--version'], check=True)


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError, tarfile.TarError, subprocess.CalledProcessError) as error:
        print('pnpm bootstrap: ' + str(error), file=sys.stderr)
        sys.exit(1)
