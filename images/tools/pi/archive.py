#!/usr/bin/env python3
"""Bounded, authenticated extraction of an npm package archive; never network.

The shell owns download and the head-of-line SRI check for the selected Pi
archive; this module owns what happens to bytes that are already authentic but
may still be unsafe, and it owns the rehash the installed-byte checker needs on
every invocation. A SHA-512 match proves the bytes are the ones the lock names,
not that a tar stream is safe to hand to `tar -x`: member names, member kinds,
links, duplicates, ancestors, sizes and modes are all checked over the whole
archive before the first file is created.

Extraction strips the single `package/` root so that the result is the package
directory itself: callers compare it entry by entry with an installed package
root, so an extra `package/` level would have to be subtracted again downstream.
Only regular files, directories and contained relative symlinks are accepted.
Privilege modes are refused rather than silently cleared, and file modes are
normalised to 0644/0755 from the archive's own executable bit, because the
installed tree's a+rX publication is not a byte or mode exception.
"""
import base64
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import sys
import tarfile
from typing import TypedDict

MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_MEMBER_BYTES = 512 * 1024 * 1024
MAX_TOTAL_BYTES = 1024 * 1024 * 1024
MAX_MEMBERS = 200_000
MAX_DEPTH = 32
MAX_PATH_LENGTH = 1024
MAX_MANIFEST_BYTES = 1024 * 1024
ARCHIVE_ROOT = 'package'
READ_CHUNK = 1024 * 1024
EXECUTABLE_MODE = 0o755
REGULAR_MODE = 0o644
PRIVILEGE_BITS = 0o7000
BIN_NAME = re.compile(r'^[^\x00/\s.][^\x00/\s]*$')
PACKAGE_NAME = re.compile(r'^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$')
EXACT_VERSION = re.compile(
    r'^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)'
    r'(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
    r'(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
    r'(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$')


class ArchiveError(Exception):
    """A rejected archive or destination; the message starts with a stable code."""

    def __init__(self, code: str, detail: str) -> None:
        super().__init__(f'{code}: {detail}')
        self.code = code


class Member(TypedDict):
    name: str
    relative: str
    kind: str
    mode: int
    size: int
    linkname: str


class BinEvidence(TypedDict):
    name: str
    path: str
    sha256: str
    mode: int


class Evidence(TypedDict):
    integrity: str
    name: str
    version: str
    manifestSha256: str
    bin: dict[str, BinEvidence]


def _relative_path(path: str, label: str) -> str:
    """Return the normalised relative path, or refuse it.

    A member name is the only attacker-controlled path in the archive, so the
    rules are spelled out rather than delegated to tarfile's sanitisation:
    empty, dotted, absolute, backslashed or over-long names and names past the
    depth budget are separate refusals.
    """
    if not isinstance(path, str) or path == '':
        raise ArchiveError('unsafe-path', f'{label} is empty')
    if len(path) > MAX_PATH_LENGTH:
        raise ArchiveError('unsafe-path', f'{label} is longer than {MAX_PATH_LENGTH}')
    if path.startswith('/') or '\\' in path or '\x00' in path:
        raise ArchiveError('unsafe-path', f'{label} is not a relative posix path: {path!r}')
    parts = path.split('/')
    if len(parts) > MAX_DEPTH:
        raise ArchiveError('unsafe-path', f'{label} is deeper than {MAX_DEPTH}')
    for part in parts:
        if part in ('', '.', '..'):
            raise ArchiveError('unsafe-path', f'{label} has an empty, dot or dot-dot segment: {path!r}')
    return path


def _relative(member_name: str) -> str | None:
    """Strip the archive's single `package/` root; None for the root itself."""
    normalised = _relative_path(member_name, 'member name')
    parts = PurePosixPath(normalised).parts
    if parts[0] != ARCHIVE_ROOT:
        raise ArchiveError('unexpected-root', f'{member_name!r} is not under {ARCHIVE_ROOT}/')
    if len(parts) == 1:
        return None
    return '/'.join(parts[1:])


def _link_target(relative: str, linkname: str) -> str:
    """A symlink may only point back inside the extracted package.

    The target is resolved against the member's own directory; anything that
    leaves the package root would make the extraction a way to read or overwrite
    neighbouring scratch state, so it is refused rather than rewritten.
    """
    if linkname == '' or '\x00' in linkname or linkname.startswith('/') or '\\' in linkname:
        raise ArchiveError('link-escape', f'{relative} has link target {linkname!r}')
    parent = PurePosixPath(relative).parent
    resolved = os.path.normpath(str(parent / linkname))
    if resolved == '..' or resolved.startswith('../'):
        raise ArchiveError('link-escape', f'{relative} links outside the package: {linkname!r}')
    return linkname


def _preflight(archive: Path) -> list[Member]:
    """Read the whole archive index before extracting anything.

    A tar stream is sequential, but these decisions are whole-archive: a
    duplicate or an ancestor collision later in the stream must stop the run
    before an earlier member is already on disk.
    """
    if archive.is_symlink() or not archive.is_file():
        raise ArchiveError('not-a-file', f'{archive} is not a regular archive')
    size = archive.stat().st_size
    if size == 0 or size > MAX_ARCHIVE_BYTES:
        raise ArchiveError('archive-size', f'{archive} is {size} bytes (limit {MAX_ARCHIVE_BYTES})')
    members: list[Member] = []
    seen: set[str] = set()
    non_directories: set[str] = set()
    implied_directories: set[str] = set()
    total = 0
    try:
        with tarfile.open(archive, 'r:gz') as tar:
            for info in tar:
                relative = _relative(info.name)
                if relative is None:
                    continue
                if len(members) >= MAX_MEMBERS:
                    raise ArchiveError('member-count', f'more than {MAX_MEMBERS} members')
                if info.name in seen:
                    raise ArchiveError('duplicate-member', info.name)
                seen.add(info.name)
                if info.mode & PRIVILEGE_BITS:
                    raise ArchiveError('privilege-mode', f'{relative} carries mode {info.mode:o}')
                if info.isfile():
                    kind = 'file'
                elif info.isdir():
                    kind = 'dir'
                elif info.issym():
                    kind = 'symlink'
                else:
                    raise ArchiveError('special-member',
                                       f'{relative} is not a regular file, directory or symlink')
                if info.size < 0 or info.size > MAX_MEMBER_BYTES:
                    raise ArchiveError('member-size', f'{relative} is {info.size} bytes')
                total += info.size
                if total > MAX_TOTAL_BYTES:
                    raise ArchiveError('total-size', f'archive members exceed {MAX_TOTAL_BYTES} bytes')
                if kind != 'dir' and relative in implied_directories:
                    raise ArchiveError('ancestor-overwrite',
                                       f'{relative} is already an implied directory of another member')
                parts = relative.split('/')
                for index in range(1, len(parts)):
                    ancestor = '/'.join(parts[:index])
                    if ancestor in non_directories:
                        raise ArchiveError('ancestor-overwrite',
                                           f'{relative} is written under {ancestor}, which is not a directory')
                    implied_directories.add(ancestor)
                if kind != 'dir':
                    non_directories.add(relative)
                members.append({'name': info.name, 'relative': relative, 'kind': kind,
                                'mode': EXECUTABLE_MODE, 'size': info.size,
                                'linkname': _link_target(relative, info.linkname)
                                if kind == 'symlink' else ''})
                if kind == 'file' and info.mode & 0o111:
                    members[-1]['mode'] = EXECUTABLE_MODE
                elif kind == 'file':
                    members[-1]['mode'] = REGULAR_MODE
    except (tarfile.TarError, OSError) as error:
        raise ArchiveError('unreadable-archive', str(error)) from error
    return members


def _destination(destination: Path) -> Path:
    if destination.is_symlink():
        raise ArchiveError('destination', f'{destination} is a symlink')
    if destination.exists():
        if not destination.is_dir() or any(destination.iterdir()):
            raise ArchiveError('destination', f'{destination} already exists and is not empty')
    else:
        destination.mkdir(mode=0o700, parents=True)
    return destination


def extract(archive: Path, destination: Path) -> list[Member]:
    """Preflight, then write the package root's own entries into `destination`."""
    members = _preflight(archive)
    root = _destination(destination)
    try:
        with tarfile.open(archive, 'r:gz') as tar:
            for member in members:
                target = root / member['relative']
                if member['kind'] == 'dir':
                    target.mkdir(mode=EXECUTABLE_MODE, parents=True, exist_ok=True)
                    continue
                target.parent.mkdir(mode=EXECUTABLE_MODE, parents=True, exist_ok=True)
                if member['kind'] == 'symlink':
                    os.symlink(member['linkname'], target)
                    continue
                _write_member(tar, member, target)
    except (tarfile.TarError, OSError) as error:
        raise ArchiveError('extraction', str(error)) from error
    return members


def _write_member(tar: tarfile.TarFile, member: Member, target: Path) -> None:
    """Write exactly the header's byte count, never following a link.

    A file created with O_NOFOLLOW and O_EXCL cannot be redirected onto an
    existing path, and the read stops at the declared size so a truncated stream
    is a hard error instead of a silently short package.
    """
    source = tar.extractfile(member['name'])
    if source is None:
        raise ArchiveError('missing-member', member['relative'])
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, 'O_NOFOLLOW', 0)
    fd = os.open(target, flags, member['mode'])
    written = 0
    try:
        with os.fdopen(fd, 'wb') as handle:
            while True:
                chunk = source.read(READ_CHUNK)
                if not chunk:
                    break
                written += len(chunk)
                if written > member['size']:
                    raise ArchiveError('member-size', f'{member["relative"]} grew past its header')
                handle.write(chunk)
        if written != member['size']:
            raise ArchiveError('member-size',
                               f'{member["relative"]} read {written} of {member["size"]} bytes')
        os.chmod(target, member['mode'])
    except BaseException:
        target.unlink(missing_ok=True)
        raise


def sri_sha512(path: Path) -> str:
    digest = hashlib.sha512()
    with path.open('rb') as handle:
        for chunk in iter(lambda: handle.read(READ_CHUNK), b''):
            digest.update(chunk)
    return 'sha512-' + base64.b64encode(digest.digest()).decode()


def verify_integrity(archive: Path, integrity: str) -> str:
    """Rehash the retained tarball against the lock SRI on every invocation.

    A directory name, a filename or a receipt that merely mentions the SRI is not
    authentication; this is the only binding the checker can re-prove offline.
    """
    tokens = integrity.split()
    if not tokens or any(not token.startswith('sha512-') for token in tokens):
        raise ArchiveError('integrity-syntax', f'not a sha512 SRI: {integrity!r}')
    actual = sri_sha512(archive)
    if actual not in tokens:
        raise ArchiveError('integrity-mismatch', f'{archive} hashes to {actual}')
    return actual


def _manifest(package: Path) -> dict[str, object]:
    manifest = package / 'package.json'
    if manifest.is_symlink() or not manifest.is_file():
        raise ArchiveError('missing-manifest', 'package.json is not a regular file')
    if manifest.stat().st_size > MAX_MANIFEST_BYTES:
        raise ArchiveError('manifest-size', 'package.json exceeds 1 MiB')
    raw = json.loads(manifest.read_bytes())
    if not isinstance(raw, dict):
        raise ArchiveError('manifest-shape', 'package.json is not an object')
    return raw


def bin_mapping(manifest: dict[str, object], package: Path) -> dict[str, BinEvidence]:
    """Read the declared command mapping from the pristine manifest.

    `bin` is a string (the command name is then the package's own basename) or a
    name-to-path object. A `directories.bin` directory would be discovered rather
    than declared, so it is refused instead of silently contributing nothing.
    """
    declared = manifest.get('bin')
    if declared is None:
        if isinstance(manifest.get('directories'), dict) and 'bin' in manifest['directories']:  # type: ignore[operator]
            raise ArchiveError('unsupported-bin-directory', 'directories.bin discovery is not modelled')
        return {}
    name = manifest.get('name')
    if not isinstance(name, str) or not PACKAGE_NAME.fullmatch(name):
        raise ArchiveError('manifest-shape', 'package.json has no canonical name')
    if isinstance(declared, str):
        mapping: dict[str, object] = {name.rsplit('/', 1)[-1]: declared}
    elif isinstance(declared, dict) and declared:
        mapping = dict(declared)
    else:
        raise ArchiveError('manifest-shape', 'bin is neither a string nor a non-empty object')
    result: dict[str, BinEvidence] = {}
    for command, relative in sorted(mapping.items()):
        if not isinstance(command, str) or not BIN_NAME.fullmatch(command) or command.startswith('-'):
            raise ArchiveError('bin-name', f'invalid command name {command!r}')
        if not isinstance(relative, str):
            raise ArchiveError('bin-path', f'{command} maps to a non-string')
        target_relative = _relative_path(str(PurePosixPath(relative)), f'bin path for {command}')
        target = package / target_relative
        if target.is_symlink() or not target.is_file():
            raise ArchiveError('bin-target',
                               f'{command} points at {target_relative}, which is not a regular file')
        result[command] = {'name': command, 'path': target_relative,
                           'sha256': hashlib.sha256(target.read_bytes()).hexdigest(),
                           'mode': target.stat().st_mode & 0o777}
    return result


def evidence(archive: Path, integrity: str, destination: Path) -> Evidence:
    """Authenticate, extract and describe one archive for the byte checker.

    The checker needs exactly three pristine facts per provider: its manifest
    identity, its declared command mapping, and the bytes each command points at.
    Returning them from the same verified tarball keeps archive parsing out of
    the checker and keeps a tampered installed tree from supplying them.
    """
    verified = verify_integrity(archive, integrity)
    extract(archive, destination)
    manifest = _manifest(destination)
    name = manifest.get('name')
    version = manifest.get('version')
    if not isinstance(name, str) or not PACKAGE_NAME.fullmatch(name):
        raise ArchiveError('manifest-shape', 'package.json has no canonical name')
    if not isinstance(version, str) or not EXACT_VERSION.fullmatch(version):
        raise ArchiveError('manifest-shape', 'package.json has no exact version')
    return {'integrity': verified, 'name': name, 'version': version,
            'manifestSha256': hashlib.sha256((destination / 'package.json').read_bytes()).hexdigest(),
            'bin': bin_mapping(manifest, destination)}


def usage() -> None:
    sys.stderr.write('usage: archive.py extract TARBALL DEST | '
                     'archive.py evidence TARBALL INTEGRITY DEST\n')
    raise SystemExit(2)


def main(argv: list[str]) -> None:
    if len(argv) == 3 and argv[0] == 'extract':
        extract(Path(argv[1]), Path(argv[2]))
        return
    if len(argv) == 4 and argv[0] == 'evidence':
        sys.stdout.write(json.dumps(evidence(Path(argv[1]), argv[2], Path(argv[3])),
                                    sort_keys=True) + '\n')
        return
    usage()


if __name__ == '__main__':
    try:
        main(sys.argv[1:])
    except ArchiveError as error:
        sys.stderr.write(f'archive.py: {error}\n')
        raise SystemExit(1) from error
    except (OSError, json.JSONDecodeError, ValueError) as error:
        sys.stderr.write(f'archive.py: {type(error).__name__}: {error}\n')
        raise SystemExit(1) from error
