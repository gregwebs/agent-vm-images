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
Containment is decided over the whole archive, not per member: a symlink target is
resolved against every other member's link, and a chain that leaves the package
or never terminates is refused. That resolution is by *filesystem identity*, not
by bytes: two names that differ only in case or in Unicode normalisation form are
the same entry on APFS/HFS+, so a member or a link target spelled the other way
reaches an entry some other member owns. Both are refused (`name-collision`) and
both are resolved (`_folded`) before containment is decided, and the write walk
opens every parent component with `O_NOFOLLOW`, so a write cannot pass through a
link even if a future spelling slips past the index. The decompressed stream is
read through a budget, because a header's declared size reaches `tarfile` before
any member is visible; extended headers get their own between-members budget for
the same reason, and the archive as a whole gets a total one because the window
resets while `tarfile` keeps what it read. Privilege modes are refused rather than
silently cleared, and file modes are normalised to 0644/0755 from the archive's own
executable bit, because the installed tree's a+rX publication is not a byte or mode
exception.
"""
import base64
import contextlib
import errno
import gzip
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import sys
import tarfile
from typing import IO, Iterator, TypedDict
import unicodedata

MAX_ARCHIVE_BYTES = 128 * 1024 * 1024
MAX_MEMBER_BYTES = 512 * 1024 * 1024
MAX_TOTAL_BYTES = 1024 * 1024 * 1024
MAX_DECOMPRESSED_BYTES = MAX_TOTAL_BYTES + 64 * 1024 * 1024
MAX_READ_BYTES = 16 * 1024 * 1024
# Extended headers (PAX records, GNU long names) are read *between* members, in
# chunks of their own, so MAX_READ_BYTES never sees them: a 64 MiB PAX `comment`
# arrived as 1 MiB reads and was accepted with a 366 MiB peak. No real package
# needs more than a few KiB of header per member.
MAX_HEADER_BYTES = 1024 * 1024
# A per-member window that resets is no bound on the archive, and `tarfile` retains
# each member's `pax_headers`: 1,000 zero-byte members carrying a 1,040,000-byte
# `comment` record each stayed under MAX_HEADER_BYTES in every window, compressed to
# 1.1 MB and peaked at 1.16 GB resident. The total is charged over the whole stream
# and is never reset, and it is what bounds the member count in practice -- a plain
# member's 512-byte header block is charged too, so ~16,000 members fit and
# MAX_MEMBERS stays as a backstop rather than as the ceiling.
MAX_TOTAL_HEADER_BYTES = 8 * 1024 * 1024
MAX_MEMBERS = 200_000
MAX_DEPTH = 32
MAX_LINK_HOPS = 40
MAX_PATH_LENGTH = 1024
MAX_MANIFEST_BYTES = 1024 * 1024
ARCHIVE_ROOT = 'package'
READ_CHUNK = 1024 * 1024
DIRECTORY_FLAGS = os.O_RDONLY | os.O_DIRECTORY | getattr(os, 'O_NOFOLLOW', 0)
LINK_REFUSALS = (errno.ELOOP, errno.ENOTDIR)
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


def _folded(path: str) -> str:
    """The one name a case/normalisation-insensitive filesystem stores this as.

    APFS and HFS+ compare names after canonical decomposition and ignore case, so
    `d/X` and `d/x`, or `d/\u00e9` and `d/e\u0301`, are the *same* entry there:
    one member can write through a link another member planted while every byte of
    both names and of every link target differs. Casefolding is broader than the
    filesystem's own simple folding, which is the safe direction -- the only
    archives it refuses are ones an APFS host could not have extracted anyway.
    """
    return unicodedata.normalize('NFD', path).casefold()


def _claim_name(claimed: dict[str, str], path: str) -> None:
    """Reserve one filesystem identity; two spellings of it are a refusal.

    Called for every member path *and* every implied ancestor, because the
    collision that matters is usually between a link and the directory another
    member writes under: a check over member names alone would still let
    `e/Y -> ../d/x/..` be followed by `e/y/PWNED`.
    """
    key = _folded(path)
    owner = claimed.get(key)
    if owner is not None and owner != path:
        raise ArchiveError('name-collision',
                           f'{path!r} and {owner!r} are one entry on a case- or '
                           'normalisation-insensitive filesystem')
    claimed[key] = path


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
    """Check the syntax of a symlink target; containment is a whole-graph question.

    Whether a target leaves the package cannot be decided from this one string:
    `b -> x/..` is contained only if `x` is a directory, and escapes when another
    member makes `x` a symlink to `.`. `_validate_links` therefore resolves every
    target against all members once the archive index is complete.
    """
    if linkname == '' or '\x00' in linkname or linkname.startswith('/') or '\\' in linkname:
        raise ArchiveError('link-escape', f'{relative} has link target {linkname!r}')
    return linkname


def _resolve(entries: dict[str, Member], components: list[str], origin: str,
             hops: int = 0) -> list[str]:
    """Resolve `components` from the package root, following member symlinks.

    This is the virtual filesystem the archive describes, not the directory tree
    beside it: `..` past the root and a link chain that does not terminate are
    both refusals, so a later member can never change the meaning of an earlier
    one after extraction. Lookups are by `_folded` name, because that is how the
    filesystem the archive is extracted onto will resolve them: a byte-exact
    lookup would read `d/x` as a plain missing directory while APFS follows the
    link the archive spelled `d/X`.
    """
    if hops > MAX_LINK_HOPS:
        raise ArchiveError('link-cycle', f'{origin} follows more than {MAX_LINK_HOPS} symlinks')
    resolved: list[str] = []
    pending = list(components)
    while pending:
        part = pending.pop(0)
        if part in ('', '.'):
            continue
        if part == '..':
            if not resolved:
                raise ArchiveError('link-escape', f'{origin} leaves {ARCHIVE_ROOT}/')
            resolved.pop()
            continue
        resolved.append(part)
        entry = entries.get(_folded('/'.join(resolved)))
        if entry is not None and entry['kind'] == 'symlink':
            translated = [piece for piece in entry['linkname'].split('/') if piece != '']
            return _resolve(entries, resolved[:-1] + translated + pending, origin, hops + 1)
    return resolved


def _validate_links(members: list[Member]) -> None:
    """Refuse a symlink that leaves the package once every member is known."""
    entries = {_folded(member['relative']): member for member in members}
    for member in members:
        if member['kind'] != 'symlink':
            continue
        _resolve(entries, member['relative'].split('/'),
                 f'{member["relative"]} -> {member["linkname"]!r}')


class _BoundedStream:
    """The decompressed archive, refused past its budget before the allocation.

    `tarfile` reads whatever an extended header declares before any member is
    visible to the caller, so a PAX record is allocated and parsed with nothing
    yet checked: a 32 MiB record header measured 105 MiB of peak RSS and the
    archive was accepted. Both bounds are compared with the requested size while
    the caller still only has the declaration, so the declaration is what is
    refused rather than the allocation that follows it.

    A large declaration is not the only way in. Since Python 3.15 `_safe_read`
    reads an extended header of any size in 1 MiB chunks, so the per-read bound
    never fires and the whole record is still allocated (a 64 MiB PAX `comment`
    was accepted with a 366 MiB peak; 600 MiB measured 3.4 GB), and a chain of
    headers recurses through `tarfile.next()` once per header. The preflight
    therefore also charges every byte read *between* two yielded members to
    `MAX_HEADER_BYTES`; member payload cannot appear in that window because it is
    skipped by seeking, never read. The window resets at each member, so the two
    are also charged to `MAX_TOTAL_HEADER_BYTES`, which never resets: `tarfile`
    retains every member's `pax_headers`, so a spread of legal-sized records is a
    bound only once the archive, not the window, is what is counted.
    """

    def __init__(self, handle: IO[bytes], label: str, *, bound_headers: bool) -> None:
        self._handle = handle
        self._label = label
        # The first member's headers are read by `tarfile.open` itself, before the
        # caller can arm anything, so the window has to start here.
        self._headers = bound_headers
        self._header_bytes = 0
        self._total_header_bytes = 0

    @property
    def name(self) -> str:
        return self._label

    def member_visible(self) -> None:
        """Close the between-members window; the archive total keeps counting."""
        self._header_bytes = 0

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            raise ArchiveError('read-size', f'{self._label}: read of the whole stream')
        # The extended-header budget is decided before the per-read cap. Whether a
        # header block arrives as one large read (CPython 3.12) or as chunks (3.15+)
        # is an interpreter detail, and a diagnostic that changed with it would make
        # the refusal depend on the Python the image happens to ship. Inside the
        # between-members window every byte is header bytes, so an oversized read
        # there is a header-budget refusal either way.
        if self._headers:
            if self._total_header_bytes + size > MAX_TOTAL_HEADER_BYTES:
                raise ArchiveError('header-bytes',
                                   f'{self._label}: more than {MAX_TOTAL_HEADER_BYTES} bytes of '
                                   'extended headers in the archive')
            if size > MAX_HEADER_BYTES:
                raise ArchiveError('header-bytes',
                                   f'{self._label}: a single extended-header read of {size} bytes '
                                   f'exceeds {MAX_HEADER_BYTES}')
        if size > MAX_READ_BYTES:
            raise ArchiveError('read-size', f'{self._label}: a single read of {size} bytes '
                                            f'exceeds {MAX_READ_BYTES}')
        if self._handle.tell() + size > MAX_DECOMPRESSED_BYTES:
            raise ArchiveError('decompressed-size',
                               f'{self._label}: read past {MAX_DECOMPRESSED_BYTES} decompressed bytes')
        data = self._handle.read(size)
        if self._headers:
            self._header_bytes += len(data)
            self._total_header_bytes += len(data)
            if self._header_bytes > MAX_HEADER_BYTES:
                raise ArchiveError('header-bytes',
                                   f'{self._label}: more than {MAX_HEADER_BYTES} bytes of extended '
                                   'headers between two members')
        return data

    def seek(self, offset: int, whence: int = os.SEEK_SET) -> int:
        if whence == os.SEEK_SET:
            target = offset
        elif whence == os.SEEK_CUR:
            target = self._handle.tell() + offset
        else:
            raise ArchiveError('read-size', f'{self._label}: seek from the stream end')
        if target < 0 or target > MAX_DECOMPRESSED_BYTES:
            raise ArchiveError('decompressed-size',
                               f'{self._label}: seek to {target} past {MAX_DECOMPRESSED_BYTES} '
                               'decompressed bytes')
        return self._handle.seek(target, os.SEEK_SET)

    def tell(self) -> int:
        return self._handle.tell()


@contextlib.contextmanager
def _open_tar(archive: Path, *, bound_headers: bool = False) -> Iterator[tuple[tarfile.TarFile, _BoundedStream]]:
    """Open the archive for reading, charging extended headers when asked to.

    Only the preflight wants the header window: the extraction pass reads member
    payload through the same stream, where the budget would charge bytes that are
    not headers at all.
    """
    with archive.open('rb') as raw, gzip.GzipFile(fileobj=raw, mode='rb') as expanded:
        stream = _BoundedStream(expanded, os.fspath(archive), bound_headers=bound_headers)
        with tarfile.open(name=os.fspath(archive), fileobj=stream, mode='r:') as tar:
            yield tar, stream


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
    claimed: dict[str, str] = {}
    non_directories: set[str] = set()
    implied_directories: set[str] = set()
    headers = 0
    total = 0
    try:
        with _open_tar(archive, bound_headers=True) as (tar, stream):
            # The `package/` root header is skipped for extraction but is a header
            # like any other: repeated copies are duplicates, and its declared size
            # or type may not bypass the budgets that bind every other member.
            for info in tar:
                try:
                    headers += 1
                    if headers > MAX_MEMBERS:
                        raise ArchiveError('member-count', f'more than {MAX_MEMBERS} headers')
                    if info.name in seen:
                        raise ArchiveError('duplicate-member', info.name)
                    seen.add(info.name)
                    if info.mode & PRIVILEGE_BITS:
                        raise ArchiveError('privilege-mode', f'{info.name} carries mode {info.mode:o}')
                    if info.isfile():
                        kind = 'file'
                    elif info.isdir():
                        kind = 'dir'
                    elif info.issym():
                        kind = 'symlink'
                    else:
                        raise ArchiveError('special-member',
                                           f'{info.name} is not a regular file, directory or symlink')
                    if info.size < 0 or info.size > MAX_MEMBER_BYTES:
                        raise ArchiveError('member-size', f'{info.name} is {info.size} bytes')
                    total += info.size
                    if total > MAX_TOTAL_BYTES:
                        raise ArchiveError('total-size',
                                           f'archive members exceed {MAX_TOTAL_BYTES} bytes')
                    relative = _relative(info.name)
                    if relative is None:
                        if kind != 'dir' or info.size != 0:
                            raise ArchiveError('unexpected-root',
                                               f'{ARCHIVE_ROOT!r} is a {kind} of {info.size} bytes, '
                                               'not an empty directory')
                        continue
                    if kind != 'dir' and relative in implied_directories:
                        raise ArchiveError('ancestor-overwrite',
                                           f'{relative} is already an implied directory of another member')
                    parts = relative.split('/')
                    _claim_name(claimed, relative)
                    for index in range(1, len(parts)):
                        ancestor = '/'.join(parts[:index])
                        if ancestor in non_directories:
                            raise ArchiveError('ancestor-overwrite',
                                               f'{relative} is written under {ancestor}, which is not a directory')
                        implied_directories.add(ancestor)
                        _claim_name(claimed, ancestor)
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
                finally:
                    # A member is now visible, so every header read from here on
                    # belongs to the next one.
                    stream.member_visible()
    except RecursionError as error:
        # A chain of extended headers recurses through `tarfile.next()` once per
        # header, and the traceback used to escape every caller's handlers.
        raise ArchiveError('header-chain',
                           'extended headers nest deeper than this interpreter allows') from error
    except (tarfile.TarError, OSError, EOFError) as error:
        raise ArchiveError('unreadable-archive', str(error)) from error
    _validate_links(members)
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
    tree = _ExtractionTree(_destination(destination))
    try:
        with _open_tar(archive) as (tar, _):
            for member in members:
                if member['kind'] == 'dir':
                    tree.directory(member['relative'])
                elif member['kind'] == 'symlink':
                    tree.symlink(member['relative'], member['linkname'])
                else:
                    _write_member(tar, member, tree)
    except (tarfile.TarError, OSError) as error:
        raise ArchiveError('extraction', str(error)) from error
    finally:
        tree.close()
    return members


class _ExtractionTree:
    """Writes archive entries under one directory handle, one component at a time.

    An archive path is never handed to the filesystem whole: every parent
    component is opened with `O_DIRECTORY | O_NOFOLLOW` relative to the handle
    above it, and the leaf is created by `dir_fd`. A write therefore cannot pass
    through a symlink at all, so the case/normalisation collision `_preflight`
    refuses stays refused even if a spelling it did not notice ever reaches here --
    and the refusal is a property of this walk, not of the host's name folding.
    """

    def __init__(self, root: Path) -> None:
        self._root = os.open(os.fspath(root), DIRECTORY_FLAGS)

    def close(self) -> None:
        os.close(self._root)

    def directory(self, relative: str) -> None:
        """Create `relative` as a directory, leaving an existing one as it is."""
        parent, name = self._parent(relative)
        try:
            os.close(self._open_directory(parent, name))
        finally:
            os.close(parent)

    def symlink(self, relative: str, target: str) -> None:
        parent, name = self._parent(relative)
        try:
            os.symlink(target, name, dir_fd=parent)
        finally:
            os.close(parent)

    def file(self, relative: str, mode: int) -> int:
        """An exclusive, unfollowed, already-`mode`d descriptor for `relative`."""
        parent, name = self._parent(relative)
        try:
            fd = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL
                         | getattr(os, 'O_NOFOLLOW', 0), mode, dir_fd=parent)
        finally:
            os.close(parent)
        os.fchmod(fd, mode)
        return fd

    def unlink(self, relative: str) -> None:
        """Remove the entry named `relative` itself; a link there is not followed."""
        parent, name = self._parent(relative)
        try:
            with contextlib.suppress(FileNotFoundError):
                os.unlink(name, dir_fd=parent)
        finally:
            os.close(parent)

    def _parent(self, relative: str) -> tuple[int, str]:
        """The descriptor of `relative`'s parent, with every missing level created."""
        parts = relative.split('/')
        held = os.dup(self._root)
        try:
            for part in parts[:-1]:
                child = self._open_directory(held, part)
                os.close(held)
                held = child
            return held, parts[-1]
        except BaseException:
            os.close(held)
            raise

    @staticmethod
    def _open_directory(parent: int, name: str) -> int:
        """The directory `name` below `parent`, created if missing, never a link."""
        try:
            return os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
        except FileNotFoundError:
            pass
        except OSError as error:
            raise _walk_refusal(name, error) from error
        try:
            os.mkdir(name, EXECUTABLE_MODE, dir_fd=parent)
        except FileExistsError:
            pass
        except OSError as error:
            raise _walk_refusal(name, error) from error
        try:
            return os.open(name, DIRECTORY_FLAGS, dir_fd=parent)
        except OSError as error:
            raise _walk_refusal(name, error) from error


def _walk_refusal(name: str, error: OSError) -> ArchiveError:
    """Name a parent component that is not a directory we may write through.

    ELOOP and ENOTDIR are what `O_NOFOLLOW` reports for a symlink or a non-directory
    on the way down, which is the escape this walk exists to make impossible;
    anything else is an ordinary extraction failure and keeps its own errno.
    """
    if error.errno in LINK_REFUSALS:
        return ArchiveError('link-traversal',
                            f'{name!r} is not a directory in the extraction tree: {error.strerror}')
    return ArchiveError('extraction', str(error))


def _write_member(tar: tarfile.TarFile, member: Member, tree: _ExtractionTree) -> None:
    """Write exactly the header's byte count, never following a link.

    A file created with O_NOFOLLOW and O_EXCL cannot be redirected onto an
    existing path, and the read stops at the declared size so a truncated stream
    is a hard error instead of a silently short package.
    """
    source = tar.extractfile(member['name'])
    if source is None:
        raise ArchiveError('missing-member', member['relative'])
    fd = tree.file(member['relative'], member['mode'])
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
    except BaseException:
        tree.unlink(member['relative'])
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


def _contained_file(root: Path, relative: str, label: str) -> Path:
    """Walk a declared path from the root one real component at a time.

    `Path.is_symlink()` only inspects the final component, so a bin target of
    `b/review52-sentinel` reached `b` as a link and hashed bytes outside the
    extraction. Every component is therefore required to be the kind it claims:
    directories for the ancestors, a regular file at the end, and no symlink
    anywhere on the way. A bin command the archive reaches through a link would
    give the archive, not the checker, the last word on the bytes hashed.
    """
    current = root
    parts = relative.split('/')
    for index, part in enumerate(parts):
        current = current / part
        status = os.lstat(current)
        final = index == len(parts) - 1
        if final and stat.S_ISREG(status.st_mode):
            continue
        if not final and stat.S_ISDIR(status.st_mode):
            continue
        raise ArchiveError('bin-target',
                           f'{label} points at {relative}, whose component {part!r} is not '
                           + ('a regular file' if final else 'a directory'))
    return current


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
        try:
            target = _contained_file(package, target_relative, command)
        except FileNotFoundError as error:
            raise ArchiveError('bin-target',
                               f'{command} points at {target_relative}, which is missing') from error
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
