#!/usr/bin/env python3
"""Audit the launcher and every immutable distribution byte, ancestry included.

A shebang audit alone sees only /bin/sh, and the bundle hash proves content but
not that no unprivileged caller can rewrite the installed bytes later.
"""
import hashlib
import os
from pathlib import Path
import stat
import sys

from bootstrap import fixture_destination, read_pin


def distribution_entries(prefix: Path) -> list[Path]:
    """Every descendant of the installed distribution; the authenticated
    extraction contains no links, so any link is an unauthenticated redirect."""
    entries: list[Path] = []
    pending = [prefix]
    while pending:
        for entry in os.scandir(pending.pop()):
            if entry.is_symlink():
                raise ValueError('unexpected symlinked distribution entry: ' + entry.path)
            path = Path(entry.path)
            entries.append(path)
            if entry.is_dir(follow_symlinks=False):
                pending.append(path)
    return entries


def dereferenced_paths(path: Path) -> list[Path]:
    """Every path the kernel dereferences while resolving `path`, one symlink
    hop at a time. `os.path.realpath` collapses intermediate hops, so a
    directory that is merely a waypoint on the chain -- writable, and therefore
    replaceable by any uid -- would escape a final-target ancestry audit."""
    paths: list[Path] = []
    resolved = Path(path.anchor or '/')
    pending = list(path.parts)
    hops = 0
    while pending:
        candidate = resolved / pending.pop(0)
        paths.append(candidate)
        if not candidate.is_symlink():
            resolved = candidate
            continue
        hops += 1
        if hops > 40:
            raise ValueError('symlink chain too deep: ' + str(path))
        target = Path(os.readlink(candidate))
        if not target.is_absolute():
            target = resolved / target
        resolved = Path(target.anchor or '/')
        pending = list(target.parts) + pending
    return paths


def audit_paths(paths: list[Path], executable: frozenset[Path], fixture_root: Path | None) -> None:
    checked: set[Path] = set()

    def audit(part: Path) -> None:
        if fixture_root is not None and part in fixture_root.parents:
            return
        if part in checked:
            return
        checked.add(part)
        info = part.stat()
        if fixture_root is None and (info.st_uid != 0 or info.st_gid != 0):
            raise ValueError('non-image-owned distribution path: ' + str(part))
        if info.st_mode & 0o022:
            raise ValueError('group/other-writable distribution path: ' + str(part))
        required = 0o005 if stat.S_ISDIR(info.st_mode) or part in executable else 0o004
        if info.st_mode & required != required:
            raise ValueError('unreadable distribution path: ' + str(part))

    for path in paths:
        # Audit every component on the physical resolution path, including each
        # intermediate link's target and its ancestry. A writable directory that
        # appears only mid-chain must not escape the certification.
        for part in dereferenced_paths(path):
            audit(part)


def main() -> None:
    source = Path(os.environ.get('PNPM_SOURCE_DIR', '/tmp/pnpm-source'))
    fixture = os.environ.get('PNPM_FIXTURE_DESTINATION')
    if fixture:
        prefix, commands = fixture_destination(Path(fixture))
        node = Path(fixture).resolve() / 'usr/bin/node'
        fixture_root = Path(fixture).resolve()
    else:
        prefix, commands = Path('/opt/agent-vm/pnpm'), Path('/usr/local/bin')
        node = Path('/usr/bin/node')
        fixture_root = None
    launcher = commands / 'pnpm'
    if read_pin(prefix / 'pin.json') != read_pin(source / 'pin.json'):
        raise ValueError('installed pin disagrees with reviewed source')
    pin = read_pin(source / 'pin.json')
    if hashlib.sha256((prefix / 'dist/pnpm.mjs').read_bytes()).hexdigest() != pin['bundleSha256']:
        raise ValueError('pristine distribution changed')
    if launcher.is_symlink() or not launcher.is_file():
        raise ValueError('pnpm launcher must be a regular file')
    if launcher.read_bytes() != (source / 'launcher.sh').read_bytes():
        raise ValueError('pnpm launcher changed')
    for name in ('pnpm', 'pnx', 'pnpx'):
        candidates = (Path(part) / name for part in os.environ['PATH'].split(':'))
        first = next((p for p in candidates if os.path.lexists(p)), None)
        if first != commands / name:
            if not (fixture and first and os.path.samefile(first, commands / name)):
                raise ValueError('unexpected first PATH candidate for ' + name)
        if name != 'pnpm' and os.readlink(first) != 'pnpm':
            raise ValueError('alias must link relatively to pnpm')
    audit_paths([prefix, launcher, commands, node, *distribution_entries(prefix)],
                frozenset({launcher, node}), fixture_root)
    if launcher.stat().st_mode & 0o777 != 0o755:
        raise ValueError('launcher mode must be 0755')


if __name__ == '__main__':
    try:
        main()
    except (ValueError, OSError) as error:
        print('pnpm verification: ' + str(error), file=sys.stderr)
        sys.exit(1)
