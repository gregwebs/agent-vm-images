#!/usr/bin/env python3
"""The committed closure of the Claude native payload (#98), verified offline.

`pin.json` beside this module is the reviewed selection: the exact release, the
upstream signed manifest and its detached signature, the release signing key, and
per Linux architecture the exact executable URL, SHA-256 and byte size that the
native installer is allowed to place. This module is the only reader. It needs no
network, resolves no release and writes nothing; `claude install` is never asked
to select or fetch anything.

Verification order, because each step is only meaningful over the previous one:
the pin's structure (exact key sets, canonical URLs recomputed from the version,
typed digests and sizes), then the committed evidence bytes re-hashed now, then
the key file's single primary fingerprint, then the detached OpenPGP signature
over the committed manifest, then the signed manifest's own version, binary name,
SHA-256 and size for each maintained platform. A build then compares the fetched
executable with the pin and checks that it is an ELF object for the target
machine. What is proven is *which bytes* were selected and reviewed; nothing here
says a release is safe.

`gpg` (with its `gpgconf`) is the only external program. Every gpg invocation
runs in a private 0700 GNUPGHOME that this module creates as a new directory under
`/tmp` -- `GPG_HOME_ROOT`, never `TMPDIR` and never the user's keyring: gpg-agent's
Unix socket lives inside that directory and `sun_path` is 104 bytes on macOS / 108
on Linux, so a deeply nested ambient temp root makes the key import fail. There is
no fallback location: an unusable root is a refusal.

CLI (all offline; nothing here resolves a release or writes a pin):

    claude_pin.py version      --pin-dir DIR
    claude_pin.py digest       --pin-dir DIR --platform TOKEN
    claude_pin.py check        --pin-dir DIR --version V
    claude_pin.py select       --pin-dir DIR --version V --platform TOKEN
    claude_pin.py preflight    --pin-dir DIR --version V --platform TOKEN FILE
    claude_pin.py installed    --pin-dir DIR --version V --platform TOKEN \\
                               --prefix DIR --owner UID:GID
    claude_pin.py native-state --prefix DIR

Exit status 1 is a refusal, 2 is an invalid invocation. This module never emits
the classified-transport status 75 and never writes a transport receipt.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from types import MappingProxyType
from typing import Iterator, Mapping

SCHEMA = 1
NAME, KIND = 'claude', 'claude-native-release'
AUTHENTICATION_METHOD = 'openpgp-signed-manifest'
# The three URLs and two evidence paths are derived, never read from config: a
# pin that names another location is not this closure.
DOWNLOAD_BASE = 'https://downloads.claude.ai/claude-code-releases'
KEY_URL = 'https://downloads.claude.ai/keys/claude-code.asc'
KEY_PATH = 'claude-code-release-key.asc'
MANIFEST_PATH = 'evidence/manifest.json'
SIGNATURE_PATH = 'evidence/manifest.json.sig'
# The reviewed signer (https://code.claude.com/docs/en/setup). The build binds the
# pin's fingerprint to the committed key file and to the signature but never reads
# this constant; `script/dev/pin-claude.py` (write mode and `--check`) and the Rust
# source gate hold the pin to it.
REVIEWED_SIGNER_FINGERPRINT = '31DDDE24DDFAB679F42D7BD2BAA929FF1A7ECACE'
REVIEWED_SIGNER_UID = 'Anthropic Claude Code Release Signing <security@anthropic.com>'
# Pin platform key -> the token the signed manifest and the CDN path use.
PLATFORMS: Mapping[str, str] = MappingProxyType({
    'linux/amd64': 'linux-x64',
    'linux/arm64': 'linux-arm64',
})
TOKENS: Mapping[str, str] = MappingProxyType(
    {token: platform for platform, token in PLATFORMS.items()})
ELF_MACHINES: Mapping[str, int] = MappingProxyType({
    'linux-x64': 62,
    'linux-arm64': 183,
})
# A release executable is ~241 MB today; 512 MiB leaves room for a normal bump
# while making an accidental order-of-magnitude read a refusal.
MAX_EXECUTABLE_BYTES = 512 << 20
MAX_EVIDENCE_BYTES = 1 << 20
MAX_PIN_BYTES = 64 << 10
MAX_STATE_BYTES = 1 << 20
READ_CHUNK_BYTES = 1 << 20
ELF_HEADER_BYTES = 20

# gpg-agent's control sockets are named after GNUPGHOME (`S.gpg-agent*`, up to 20
# more characters) inside sun_path, which is 104 bytes on macOS and 108 on Linux.
# TMPDIR is often a deep per-case scratch path, so the home lives under a fixed
# short root and its length is bounded here rather than left to a gpg failure.
GPG_HOME_ROOT = Path('/tmp')
GPG_HOME_PREFIX = 'claude-pin-gpg.'
MAX_GPG_HOME_CHARS = 64

# The owning hook's exact-slot grammar, byte-for-byte the one in
# images/tools/claude/install-claude.sh: a value the hook accepts must not be a
# value this loader rejects, or the reverse.
VERSION = re.compile(
    r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
    r'(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
    r'(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?')
SHA256_RE = re.compile(r'[0-9a-f]{64}')
FINGERPRINT_RE = re.compile(r'[0-9A-F]{40}')
DATE_RE = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}')

ROOT_KEYS = ('schema', 'name', 'kind', 'version', 'manifest', 'signature', 'key',
             'platforms', 'authentication')
EVIDENCE_KEYS = ('url', 'path', 'sha256')
KEY_KEYS = ('url', 'path', 'fingerprint', 'sha256')
PLATFORM_KEYS = ('manifest_platform', 'url', 'sha256', 'size')
AUTHENTICATION_KEYS = ('method', 'verified_on', 'reason')


class Refusal(Exception):
    """A committed input is not what the reviewed pin says it is."""


@dataclass(frozen=True)
class Evidence:
    url: str
    path: str
    sha256: str


@dataclass(frozen=True)
class Key:
    url: str
    path: str
    fingerprint: str
    sha256: str


@dataclass(frozen=True)
class Executable:
    platform: str
    token: str
    url: str
    sha256: str
    size: int


@dataclass(frozen=True)
class Pin:
    directory: Path
    version: str
    manifest: Evidence
    signature: Evidence
    key: Key
    executables: Mapping[str, Executable]
    verified_on: datetime.date
    reason: str


@dataclass(frozen=True)
class Signature:
    primary_fingerprint: str
    created: str


@dataclass(frozen=True)
class Manifest:
    version: str
    commit: str | None
    build_date: str | None
    executables: Mapping[str, tuple[str, int]]


@dataclass(frozen=True)
class VerifiedPin:
    pin: Pin
    signature: Signature
    manifest: Manifest


# --- canonical derivations ---------------------------------------------------
#
# Every URL is reconstructed from the validated version here, and `parse` compares
# for equality. A lookalike origin, another version or an extra path segment can
# then not satisfy the check by sharing a leading substring.

def canonical_executable_url(version: str, token: str) -> str:
    return f'{DOWNLOAD_BASE}/{version}/{token}/claude'


def manifest_url(version: str) -> str:
    return f'{DOWNLOAD_BASE}/{version}/manifest.json'


def signature_url(version: str) -> str:
    return f'{DOWNLOAD_BASE}/{version}/manifest.json.sig'


# --- strict primitives -------------------------------------------------------

def _no_float(_text: str) -> float:
    raise Refusal('a JSON number with a fractional part is not part of this schema')


def _no_constant(text: str) -> None:
    raise Refusal(f'{text} is not a JSON number')


def _unique_pairs(pairs: list) -> dict:
    members: dict = {}
    for key, value in pairs:
        if key in members:
            raise Refusal(f'duplicate JSON key {key!r}')
        members[key] = value
    return members


def strict_json(raw: bytes, what: str) -> object:
    """One complete JSON document with no duplicate keys and no trailing input.

    `json.loads` keeps the last of two identical keys, so a document could carry
    one value for a reader that takes the first and another for us. The ~15 lines
    are duplicated from `images/artifacts/artifact_pin.py` on purpose: this is a
    standard-image tool and that is a base input, so neither may import the other.
    """
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise Refusal(f'{what} is not UTF-8') from None
    decoder = json.JSONDecoder(object_pairs_hook=_unique_pairs, parse_float=_no_float,
                               parse_constant=_no_constant)
    try:
        value, end = decoder.raw_decode(text)
    except ValueError as error:
        raise Refusal(f'{what} is not JSON: {error}') from None
    if text[end:].strip():
        raise Refusal(f'{what} has trailing input after the JSON document')
    return value


def _object(value: object, keys: tuple[str, ...], what: str) -> dict:
    if not isinstance(value, dict):
        raise Refusal(f'{what} must be a JSON object')
    if set(value) != set(keys):
        extra = sorted(set(value) - set(keys))
        missing = sorted(set(keys) - set(value))
        raise Refusal(f'{what} must have exactly {list(keys)}; '
                      f'missing {missing}, unexpected {extra}')
    return value


def _string(value: object, what: str) -> str:
    if not isinstance(value, str) or not value:
        raise Refusal(f'{what} must be a non-empty string')
    return value


def _integer(value: object, what: str) -> int:
    # `bool` is an `int` subclass, so an explicit type check is the only way to
    # keep `"size": true` out.
    if type(value) is not int:
        raise Refusal(f'{what} must be an integer')
    return value


def _sha256(value: object, what: str) -> str:
    raw = _string(value, what)
    if not SHA256_RE.fullmatch(raw):
        raise Refusal(f'{what} must be 64 lower-case hex characters')
    return raw


def require_version(value: object, what: str = 'version') -> str:
    raw = _string(value, what)
    if not VERSION.fullmatch(raw):
        raise Refusal(f'{what} is not an exact Claude Code version: {raw!r}')
    return raw


# --- schema ------------------------------------------------------------------

def _evidence(value: object, url: str, path: str, what: str) -> Evidence:
    members = _object(value, EVIDENCE_KEYS, what)
    found_url = _string(members['url'], f'{what} url')
    if found_url != url:
        raise Refusal(f'{what} url must be {url}, found {found_url!r}')
    found_path = _string(members['path'], f'{what} path')
    if found_path != path:
        raise Refusal(f'{what} path must be {path}, found {found_path!r}')
    return Evidence(url=url, path=path, sha256=_sha256(members['sha256'], f'{what} sha256'))


def _key(value: object) -> Key:
    what = 'pin key'
    members = _object(value, KEY_KEYS, what)
    url = _string(members['url'], f'{what} url')
    if url != KEY_URL:
        raise Refusal(f'{what} url must be {KEY_URL}, found {url!r}')
    path = _string(members['path'], f'{what} path')
    if path != KEY_PATH:
        raise Refusal(f'{what} path must be {KEY_PATH}, found {path!r}')
    fingerprint = _string(members['fingerprint'], f'{what} fingerprint')
    if not FINGERPRINT_RE.fullmatch(fingerprint):
        raise Refusal(f'{what} fingerprint must be 40 upper-case hex characters')
    return Key(url=url, path=path, fingerprint=fingerprint,
               sha256=_sha256(members['sha256'], f'{what} sha256'))


def _platform(value: object, platform: str, version: str) -> Executable:
    what = f'{platform} platform'
    token = PLATFORMS[platform]
    members = _object(value, PLATFORM_KEYS, what)
    found_token = _string(members['manifest_platform'], f'{what} manifest_platform')
    if found_token != token:
        raise Refusal(f'{what} manifest_platform must be {token}, found {found_token!r}')
    url = _string(members['url'], f'{what} url')
    expected = canonical_executable_url(version, token)
    if url != expected:
        raise Refusal(f'{what} url must be {expected}, found {url!r}')
    size = _integer(members['size'], f'{what} size')
    if not 1 <= size <= MAX_EXECUTABLE_BYTES:
        raise Refusal(f'{what} size must be between 1 and {MAX_EXECUTABLE_BYTES}, '
                      f'found {size}')
    return Executable(platform=platform, token=token, url=url,
                      sha256=_sha256(members['sha256'], f'{what} sha256'), size=size)


def utc_today() -> datetime.date:
    """The date `verified_on` is stamped and checked against, in UTC.

    Both sides of this comparison are UTC, never the host's local date: the
    maintainer running the writer is routinely east of the builders, so a pin
    stamped with a local date can carry a UTC date that is still in the future for
    every UTC-or-west builder. That fails closed and heals itself within a day, but
    it leaves a merged pin unbuildable for up to ~14 hours, and #94's rule is that a
    pinned image input stays buildable and boring. `script/dev/pin-claude.py`
    stamps with this same function, so there is one definition of "today".
    """
    return datetime.datetime.now(datetime.timezone.utc).date()


def _authentication(value: object) -> tuple[datetime.date, str]:
    what = 'pin authentication'
    members = _object(value, AUTHENTICATION_KEYS, what)
    method = _string(members['method'], f'{what} method')
    if method != AUTHENTICATION_METHOD:
        raise Refusal(f'{what} method must be {AUTHENTICATION_METHOD}, found {method!r}')
    verified_on = _string(members['verified_on'], f'{what} verified_on')
    if not DATE_RE.fullmatch(verified_on):
        raise Refusal(f'{what} verified_on must be YYYY-MM-DD, found {verified_on!r}')
    try:
        day = datetime.date.fromisoformat(verified_on)
    except ValueError:
        raise Refusal(f'{what} verified_on is not a calendar date: {verified_on!r}') from None
    if day > utc_today():
        raise Refusal(f'{what} verified_on {verified_on} is in the future')
    return day, _string(members['reason'], f'{what} reason')


def parse(raw: bytes, directory: Path) -> Pin:
    """Schema validation only: no evidence byte and no gpg call is read here."""
    members = _object(strict_json(raw, 'pin.json'), ROOT_KEYS, 'pin')
    if _integer(members['schema'], 'pin schema') != SCHEMA:
        raise Refusal(f'pin schema must be {SCHEMA}')
    if _string(members['name'], 'pin name') != NAME:
        raise Refusal(f'pin name must be {NAME}')
    if _string(members['kind'], 'pin kind') != KIND:
        raise Refusal(f'pin kind must be {KIND}')
    version = require_version(members['version'], 'pin version')
    manifest = _evidence(members['manifest'], manifest_url(version), MANIFEST_PATH,
                         'manifest evidence')
    signature = _evidence(members['signature'], signature_url(version), SIGNATURE_PATH,
                          'signature evidence')
    key = _key(members['key'])
    platforms = _object(members['platforms'], tuple(PLATFORMS), 'pin platforms')
    executables = {PLATFORMS[platform]: _platform(platforms[platform], platform, version)
                   for platform in PLATFORMS}
    verified_on, reason = _authentication(members['authentication'])
    return Pin(directory=Path(directory), version=version, manifest=manifest,
               signature=signature, key=key,
               executables=MappingProxyType(executables), verified_on=verified_on,
               reason=reason)


# --- committed evidence ------------------------------------------------------

def _regular_file(directory: Path, relative: str, what: str) -> Path:
    """A regular, non-symlink file reached without passing through a symlink."""
    parts = relative.split('/')
    if any(part in ('', '.', '..') for part in parts):
        raise Refusal(f'{what} path {relative!r} is not a plain relative path')
    current = directory
    for part in parts[:-1]:
        current = current / part
        if current.is_symlink():
            raise Refusal(f'{what} is reached through the symlink {current}')
        if not current.is_dir():
            raise Refusal(f'{what} ancestor {current} is not a directory')
    path = current / parts[-1]
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise Refusal(f'{what} is missing at {path}') from None
    except OSError as error:
        raise Refusal(f'{what} cannot be read at {path}: {error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise Refusal(f'{what} at {path} is not a regular file')
    return path


def _read_bounded(path: Path, limit: int, what: str) -> bytes:
    """Read a file already proven regular, refusing a swap to another filetype."""
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0)
    try:
        handle = os.open(path, flags)
    except OSError as error:
        raise Refusal(f'{what} cannot be opened at {path}: {error}') from None
    try:
        if not stat.S_ISREG(os.fstat(handle).st_mode):
            raise Refusal(f'{what} at {path} is not a regular file')
        raw = b''
        while True:
            chunk = os.read(handle, min(READ_CHUNK_BYTES, limit + 1 - len(raw)))
            if not chunk:
                break
            raw += chunk
            if len(raw) > limit:
                raise Refusal(f'{what} is larger than the {limit} byte ceiling')
        return raw
    except OSError as error:
        raise Refusal(f'{what} cannot be read at {path}: {error}') from None
    finally:
        os.close(handle)


def _sha256_of(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def evidence_bytes(pin: Pin, relative: str, expected: str) -> bytes:
    """One committed evidence file, re-hashed now, before any use."""
    path = _regular_file(pin.directory, relative, f'committed {relative}')
    raw = _read_bounded(path, MAX_EVIDENCE_BYTES, f'committed {relative}')
    if _sha256_of(raw) != expected:
        raise Refusal(f'committed {relative} does not match the pin digest')
    return raw


def load(directory: Path) -> Pin:
    """The reviewed closure's structure, with `pin.json` itself proven regular."""
    directory = Path(directory)
    raw = _read_bounded(_regular_file(directory, 'pin.json', 'pin.json'),
                        MAX_PIN_BYTES, 'pin.json')
    return parse(raw, directory)


# --- gpg home ----------------------------------------------------------------

@contextlib.contextmanager
def gpg_home() -> Iterator[dict]:
    """A private GNUPGHOME for exactly this invocation, under a fixed short root.

    `mkdtemp` creates a new 0700 directory that only this invocation owns; the
    explicit `dir` is what keeps `TMPDIR`/`tempfile.tempdir` out of it. The
    complete environment is yielded so no ambient `GNUPGHOME` or `HOME` keyring is
    ever visible to gpg.
    """
    try:
        home = Path(tempfile.mkdtemp(prefix=GPG_HOME_PREFIX, dir=GPG_HOME_ROOT))
    except OSError as error:
        raise Refusal(f'cannot create a private gpg home under {GPG_HOME_ROOT}: '
                      f'{error}') from None
    environment = {'GNUPGHOME': str(home), 'LC_ALL': 'C',
                   'PATH': os.environ.get('PATH', '/usr/bin:/bin')}
    failed = False
    try:
        if len(str(home)) > MAX_GPG_HOME_CHARS:
            raise Refusal(f'the private gpg home {home} is too long for '
                          "gpg-agent's socket")
        yield environment
    except BaseException:
        failed = True
        raise
    finally:
        _kill_agent(environment)
        try:
            shutil.rmtree(home)
        except FileNotFoundError:
            pass
        except OSError as error:
            if failed:
                print(f'claude_pin: warning: cannot remove the private gpg home '
                      f'{home}: {error}', file=sys.stderr)
            else:
                raise Refusal(f'cannot remove the private gpg home {home}: '
                              f'{error}') from None


def _kill_agent(environment: dict) -> None:
    """Stop any agent started for this home before the home disappears."""
    try:
        completed = subprocess.run(['gpgconf', '--kill', 'all'], env=environment,
                                   capture_output=True, text=True, check=False)
    except (OSError, subprocess.SubprocessError) as error:
        print(f'claude_pin: warning: cannot stop the gpg agent for '
              f'{environment["GNUPGHOME"]}: {error}', file=sys.stderr)
        return
    if completed.returncode != 0:
        print(f'claude_pin: warning: gpgconf --kill all exited '
              f'{completed.returncode}: {completed.stderr.strip()}', file=sys.stderr)


def gpg(argv: list[str], environment: dict, what: str, *,
        check: bool = True) -> subprocess.CompletedProcess:
    """One gpg call in a private home; every status is the caller's to interpret."""
    try:
        completed = subprocess.run(['gpg', *argv], env=environment,
                                   capture_output=True, text=True, check=False)
    except FileNotFoundError:
        raise Refusal('gpg is required to verify the Claude manifest') from None
    except OSError as error:
        raise Refusal(f'cannot run gpg: {error}') from None
    if check and completed.returncode != 0:
        detail = (completed.stderr or completed.stdout).strip()
        raise Refusal(f'{what} (gpg exit {completed.returncode}): {detail}')
    return completed


# --- key file and signature --------------------------------------------------

def parse_primaries(colons: str) -> list[str]:
    """The fingerprints of the key file's primary keys, in listing order.

    Only the `fpr` record that *follows a `pub` record* is a primary: the `fpr`
    after a `sub` record is a signing subkey, and a second primary would let an
    import add a signer the pin does not name. A `pub` record without its own
    `fpr` is refused rather than guessed at.

    A `sec`/`ssb` record is a refusal rather than a primary. `show-only` lists
    secret keys in their own records, so a key file that carried both the reviewed
    public key and any secret key would pass a `pub`-only count and then let the
    import install a second, unnamed signer; only the later VALIDSIG-primary
    comparison would stop a manifest signed by it. A committed release key never
    carries secret material, so this is a refusal and not a wider primary test.
    """
    primaries: list[str] = []
    pending = False
    for line in colons.splitlines():
        fields = line.split(':')
        kind = fields[0]
        if kind == 'pub':
            if pending:
                raise Refusal('the release key file has a pub record without an fpr')
            pending = True
        elif kind == 'fpr':
            if pending:
                if len(fields) < 10:
                    raise Refusal('the release key file has an unreadable fpr record')
                fingerprint = fields[9].upper()
                if not FINGERPRINT_RE.fullmatch(fingerprint):
                    raise Refusal(f'the release key file names the unusable '
                                  f'fingerprint {fields[9]!r}')
                primaries.append(fingerprint)
                pending = False
        elif kind in ('sec', 'ssb'):
            raise Refusal('the release key file carries secret key material')
        elif kind in ('sub', 'uid', 'uat'):
            if pending:
                raise Refusal('the release key file has a pub record without an fpr')
    if pending:
        raise Refusal('the release key file has a pub record without an fpr')
    return primaries


def key_primaries(key: Path, environment: dict) -> list[str]:
    listed = gpg(['--batch', '--with-colons', '--import-options', 'show-only',
                  '--import', str(key)], environment,
                 'cannot read the release key', check=True)
    return parse_primaries(listed.stdout)


def verify_signature(key: bytes, fingerprint: str, manifest: bytes,
                     signature: bytes) -> Signature:
    """The detached signature over `manifest`, made by `fingerprint`'s key.

    Both the key file and the signed bytes are copied into the private home, so
    gpg never reads through a checkout path and a later swap cannot change what
    was verified.
    """
    with gpg_home() as environment:
        home = Path(environment['GNUPGHOME'])
        key_path = home / 'release-key.asc'
        manifest_path = home / 'manifest.json'
        signature_path = home / 'manifest.json.sig'
        key_path.write_bytes(key)
        manifest_path.write_bytes(manifest)
        signature_path.write_bytes(signature)
        primaries = key_primaries(key_path, environment)
        if primaries != [fingerprint]:
            raise Refusal(f'the release key file must hold exactly the primary '
                          f'{fingerprint}, found {primaries}')
        # The import status is checked like every other gpg step: an unimported
        # key would otherwise surface as a misleading "signature does not verify".
        gpg(['--batch', '--quiet', '--import', str(key_path)], environment,
            'cannot import the release key')
        status = gpg(['--batch', '--status-fd=1', '--verify', str(signature_path),
                      str(manifest_path)], environment, 'verify the manifest signature',
                     check=False)
        tokens = {line.split()[1] for line in status.stdout.splitlines()
                  if line.startswith('[GNUPG:] ') and len(line.split()) > 1}
        if (status.returncode != 0 or 'GOODSIG' not in tokens
                or tokens & {'BADSIG', 'ERRSIG', 'EXPSIG', 'EXPKEYSIG', 'REVKEYSIG'}):
            raise Refusal('the manifest signature does not verify: '
                          f'{status.stdout.strip() or status.stderr.strip()}')
        valid = [line.split() for line in status.stdout.splitlines()
                 if line.startswith('[GNUPG:] VALIDSIG ')]
        # VALIDSIG fields: [2] the signing key (a subkey, if one signed), [3] the
        # signature date, [-1] the primary key it belongs to. The primary is the
        # pinned identity, so a signature from a reviewed subkey is accepted and a
        # pin that names a subkey is not.
        if len(valid) != 1 or valid[0][-1].upper() != fingerprint:
            found = [fields[-1].upper() for fields in valid]
            raise Refusal(f'the manifest signature is not by the primary '
                          f'{fingerprint}; found {found}')
        return Signature(primary_fingerprint=valid[0][-1].upper(), created=valid[0][3])


def verify(pin: Pin, version: str) -> VerifiedPin:
    """Everything the committed directory proves about the requested version."""
    requested = require_version(version, 'requested version')
    if requested != pin.version:
        raise Refusal(f'{requested} is not the pinned Claude Code version '
                      f'{pin.version}; an exact but unreviewed version needs a '
                      'reviewed pin')
    manifest_raw = evidence_bytes(pin, pin.manifest.path, pin.manifest.sha256)
    signature_raw = evidence_bytes(pin, pin.signature.path, pin.signature.sha256)
    key_raw = evidence_bytes(pin, pin.key.path, pin.key.sha256)
    signature = verify_signature(key_raw, pin.key.fingerprint, manifest_raw,
                                 signature_raw)
    manifest = parse_manifest(manifest_raw)
    if manifest.version != pin.version:
        raise Refusal(f'the signed manifest names {manifest.version}, but the pin '
                      f'names {pin.version}')
    for token, executable in pin.executables.items():
        signed = manifest.executables.get(token)
        if signed != (executable.sha256, executable.size):
            raise Refusal(f'the signed manifest does not carry the pinned {token} '
                          f'executable: manifest {signed}, pin '
                          f'({executable.sha256}, {executable.size})')
    return VerifiedPin(pin=pin, signature=signature, manifest=manifest)


def parse_manifest(raw: bytes) -> Manifest:
    """The security projection of the signed manifest; other content is ignored."""
    document = strict_json(raw, 'the signed Claude manifest')
    if not isinstance(document, dict):
        raise Refusal('the signed Claude manifest must be a JSON object')
    version = document.get('version')
    if not isinstance(version, str) or not VERSION.fullmatch(version):
        raise Refusal(f'the signed Claude manifest names no exact version: '
                      f'{version!r}')
    platforms = document.get('platforms')
    if not isinstance(platforms, dict):
        raise Refusal('the signed Claude manifest has no platforms object')
    executables: dict[str, tuple[str, int]] = {}
    for token in TOKENS:
        entry = platforms.get(token)
        if not isinstance(entry, dict):
            raise Refusal(f'the signed Claude manifest has no {token} platform')
        binary = entry.get('binary')
        if binary != 'claude':
            raise Refusal(f'the signed Claude manifest {token} binary is '
                          f'{binary!r}, not claude')
        checksum = entry.get('checksum')
        if not isinstance(checksum, str) or not SHA256_RE.fullmatch(checksum):
            raise Refusal(f'the signed Claude manifest {token} checksum is not 64 '
                          'lower-case hex characters')
        size = entry.get('size')
        if type(size) is not int or size <= 0:
            raise Refusal(f'the signed Claude manifest {token} size is not a '
                          'positive integer')
        executables[token] = (checksum, size)
    commit = document.get('commit')
    if commit is not None and not isinstance(commit, str):
        raise Refusal('the signed Claude manifest commit is not a string')
    build_date = document.get('buildDate')
    if build_date is not None and not isinstance(build_date, str):
        raise Refusal('the signed Claude manifest buildDate is not a string')
    return Manifest(version=version, commit=commit, build_date=build_date,
                    executables=MappingProxyType(executables))


def check_policy(pin: Pin) -> None:
    """Hold the pin to the reviewed signer (writer `--check`, Rust gate mirror)."""
    if pin.key.fingerprint != REVIEWED_SIGNER_FINGERPRINT:
        raise Refusal(f'the pin names the signer {pin.key.fingerprint}, not the '
                      f'reviewed {REVIEWED_SIGNER_FINGERPRINT}')


def pinned_executable(pin: Pin, token: str) -> Executable:
    found = pin.executables.get(token)
    if found is None:
        raise Refusal(f'{token} is not a maintained Claude Code platform')
    return found


def executable(verified: VerifiedPin, token: str) -> Executable:
    return pinned_executable(verified.pin, token)


# --- downloaded payload ------------------------------------------------------

def _hash_regular(path: Path, limit: int, what: str) -> tuple[str, int]:
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0)
    try:
        handle = os.open(path, flags)
    except OSError as error:
        raise Refusal(f'{what} cannot be opened at {path}: {error}') from None
    hasher = hashlib.sha256()
    total = 0
    try:
        if not stat.S_ISREG(os.fstat(handle).st_mode):
            raise Refusal(f'{what} at {path} is not a regular file')
        while True:
            chunk = os.read(handle, READ_CHUNK_BYTES)
            if not chunk:
                break
            total += len(chunk)
            if total > limit:
                raise Refusal(f'{what} is larger than the {limit} byte ceiling')
            hasher.update(chunk)
    except OSError as error:
        raise Refusal(f'{what} cannot be read at {path}: {error}') from None
    finally:
        os.close(handle)
    return hasher.hexdigest(), total


def check_elf(path: Path, token: str) -> None:
    """The first 20 bytes must be a 64-bit little-endian ELF for `token`.

    Checked without executing anything: a shell script that hashes correctly is
    not this release's executable, and the native installer must never run it.
    """
    machine = ELF_MACHINES.get(token)
    if machine is None:
        raise Refusal(f'{token} is not a maintained Claude Code platform')
    try:
        with open(path, 'rb') as handle:
            header = handle.read(ELF_HEADER_BYTES)
    except OSError as error:
        raise Refusal(f'{path} cannot be read: {error}') from None
    if len(header) < ELF_HEADER_BYTES:
        raise Refusal(f'{path} is shorter than an ELF header')
    if header[:4] != b'\x7fELF':
        raise Refusal(f'{path} is not an ELF object')
    if header[4] != 2:
        raise Refusal(f'{path} is not a 64-bit ELF object')
    if header[5] != 1:
        raise Refusal(f'{path} is not a little-endian ELF object')
    found = int.from_bytes(header[18:20], 'little')
    if found != machine:
        raise Refusal(f'{path} targets ELF machine {found}, not {machine} ({token})')


def preflight(verified: VerifiedPin, token: str, path: Path) -> None:
    """The downloaded bytes are the pinned ones, and an executable for this token."""
    pinned = executable(verified, token)
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise Refusal(f'no downloaded executable at {path}') from None
    except OSError as error:
        raise Refusal(f'cannot stat {path}: {error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise Refusal(f'{path} is not a regular file')
    digest, size = _hash_regular(path, MAX_EXECUTABLE_BYTES, 'the downloaded executable')
    if digest != pinned.sha256:
        raise Refusal(f'the downloaded executable sha256 {digest} does not match '
                      f'the pinned {pinned.sha256}')
    if size != pinned.size:
        raise Refusal(f'the downloaded executable is {size} bytes, but the pin '
                      f'records {pinned.size}')
    check_elf(path, token)


# --- installed layout and native state ---------------------------------------

def _not_writable_by_others(path: Path, what: str) -> None:
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise Refusal(f'{what} is missing at {path}') from None
    except OSError as error:
        raise Refusal(f'{what} cannot be read at {path}: {error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise Refusal(f'{what} at {path} is not a real directory')
    if stat.S_IMODE(info.st_mode) & 0o022:
        raise Refusal(f'{what} {path} is group- or world-writable '
                      f'(mode {stat.S_IMODE(info.st_mode):04o})')


def check_installed(verified: VerifiedPin, token: str, prefix: Path,
                    owner: tuple[int, int]) -> None:
    """The pinned payload is exactly where and what the native layout says.

    The ELF check is not repeated: it ran in the install RUN over these bytes, and
    the digest match is what binds a test fixture's shell script to this layout.
    """
    pinned = executable(verified, token)
    version = verified.pin.version
    share = prefix / '.local/share/claude'
    versions = share / 'versions'
    installed = versions / version
    _not_writable_by_others(prefix / '.local/bin', '.local/bin')
    _not_writable_by_others(share, '.local/share/claude')
    _not_writable_by_others(versions, '.local/share/claude/versions')
    try:
        entries = sorted(os.listdir(versions))
    except OSError as error:
        raise Refusal(f'cannot list {versions}: {error}') from None
    if entries != [version]:
        raise Refusal(f'{versions} holds {entries}, not exactly [{version!r}]')
    try:
        info = os.lstat(installed)
    except OSError as error:
        raise Refusal(f'the installed payload {installed} cannot be read: '
                      f'{error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise Refusal(f'the installed payload {installed} is not a regular file')
    if stat.S_IMODE(info.st_mode) != 0o755:
        raise Refusal(f'the installed payload {installed} has mode '
                      f'{stat.S_IMODE(info.st_mode):04o}, not 0755')
    if (info.st_uid, info.st_gid) != owner:
        raise Refusal(f'the installed payload {installed} is owned '
                      f'{info.st_uid}:{info.st_gid}, not {owner[0]}:{owner[1]}')
    digest, size = _hash_regular(installed, MAX_EXECUTABLE_BYTES,
                                 'the installed payload')
    if digest != pinned.sha256:
        raise Refusal(f'the installed payload sha256 {digest} does not match the '
                      f'pinned {pinned.sha256}')
    if size != pinned.size:
        raise Refusal(f'the installed payload is {size} bytes, but the pin records '
                      f'{pinned.size}')
    launcher = prefix / '.local/bin/claude'
    try:
        launcher_info = os.lstat(launcher)
    except OSError as error:
        raise Refusal(f'the launcher {launcher} cannot be read: {error}') from None
    if not stat.S_ISLNK(launcher_info.st_mode):
        raise Refusal(f'the launcher {launcher} is not a symlink')
    target = os.readlink(launcher)
    if target != str(installed):
        raise Refusal(f'the launcher {launcher} points at {target!r}, not '
                      f'{str(installed)!r}')


def _empty_or_absent(directory: Path, what: str) -> None:
    try:
        info = os.lstat(directory)
    except FileNotFoundError:
        return
    except OSError as error:
        raise Refusal(f'{what} cannot be read at {directory}: {error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISDIR(info.st_mode):
        raise Refusal(f'{what} at {directory} is not a real directory')
    try:
        entries = os.listdir(directory)
    except OSError as error:
        raise Refusal(f'cannot list {what} at {directory}: {error}') from None
    if entries:
        raise Refusal(f'{what} at {directory} is not empty: {sorted(entries)[:5]}')


def check_native_state(prefix: Path) -> None:
    """The offline native step's own state, as the image needs it to be."""
    path = prefix / '.claude.json'
    try:
        info = os.lstat(path)
    except FileNotFoundError:
        raise Refusal(f'{path} is missing') from None
    except OSError as error:
        raise Refusal(f'{path} cannot be read: {error}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise Refusal(f'{path} is not a regular file')
    document = strict_json(_read_bounded(path, MAX_STATE_BYTES, str(path)),
                           f'{path}')
    if not isinstance(document, dict):
        raise Refusal(f'{path} is not a JSON object')
    if document.get('installMethod') != 'native':
        raise Refusal(f'{path} records installMethod '
                      f'{document.get("installMethod")!r}, not "native"')
    # `is not False` is deliberate: `"false"`, `0` and a missing key are all a
    # different state from the JSON boolean the native installer writes.
    if document.get('autoUpdates') is not False:
        raise Refusal(f'{path} records autoUpdates {document.get("autoUpdates")!r}, '
                      'not false')
    _empty_or_absent(prefix / '.claude/downloads', '.claude/downloads')
    _empty_or_absent(prefix / '.cache/claude/staging', '.cache/claude/staging')


# --- CLI ---------------------------------------------------------------------

class _Parser(argparse.ArgumentParser):
    """Argument errors exit 2; a refusal is a separate status handled by main."""

    def error(self, message: str) -> None:  # pragma: no cover - argparse path
        print(f'claude_pin: {message}', file=sys.stderr)
        raise SystemExit(2)


def _parser() -> argparse.ArgumentParser:
    parser = _Parser(prog='claude_pin.py', description=__doc__.split('\n')[0])
    verbs = parser.add_subparsers(dest='verb', required=True, parser_class=_Parser)

    structural = verbs.add_parser('version', help='print the pinned version')
    structural.add_argument('--pin-dir', required=True)
    digest = verbs.add_parser('digest', help='print one platform SHA-256')
    digest.add_argument('--pin-dir', required=True)
    digest.add_argument('--platform', required=True)
    check = verbs.add_parser('check', help='verify the committed closure offline')
    check.add_argument('--pin-dir', required=True)
    check.add_argument('--version', required=True)
    for verb in ('select', 'preflight'):
        command = verbs.add_parser(verb)
        command.add_argument('--pin-dir', required=True)
        command.add_argument('--version', required=True)
        command.add_argument('--platform', required=True)
        if verb == 'preflight':
            command.add_argument('file')
    installed = verbs.add_parser('installed')
    installed.add_argument('--pin-dir', required=True)
    installed.add_argument('--version', required=True)
    installed.add_argument('--platform', required=True)
    installed.add_argument('--prefix', required=True)
    installed.add_argument('--owner', required=True)
    native = verbs.add_parser('native-state')
    native.add_argument('--prefix', required=True)
    return parser


def _parse_owner(text: str) -> tuple[int, int]:
    parts = text.split(':')
    if len(parts) != 2 or not all(part.isdigit() for part in parts):
        raise Refusal(f'--owner must be UID:GID, found {text!r}')
    return int(parts[0]), int(parts[1])


def main(argv: list[str]) -> int:
    arguments = _parser().parse_args(argv)
    try:
        if arguments.verb == 'native-state':
            check_native_state(Path(arguments.prefix))
            print(f'claude: native state at {arguments.prefix} matches the '
                  'offline install')
            return 0
        directory = Path(arguments.pin_dir)
        if arguments.verb == 'version':
            print(load(directory).version)
            return 0
        pin = load(directory)
        if arguments.verb == 'digest':
            print(pinned_executable(pin, arguments.platform).sha256)
            return 0
        verified = verify(pin, arguments.version)
        if arguments.verb == 'check':
            print(f'claude {pin.version}: committed closure verified offline -- '
                  f'signature by {verified.signature.primary_fingerprint} created '
                  f'{verified.signature.created}, manifest commit '
                  f'{verified.manifest.commit}, both platform executables pinned')
            return 0
        token = arguments.platform
        pinned = executable(verified, token)
        if arguments.verb == 'select':
            print(f'{pinned.url} {pinned.sha256} {pinned.size}')
            return 0
        if arguments.verb == 'preflight':
            preflight(verified, token, Path(arguments.file))
            print(f'claude {pin.version} {pinned.platform}: downloaded executable '
                  'matches the committed pin and is an executable for this machine')
            return 0
        if arguments.verb == 'installed':
            check_installed(verified, token, Path(arguments.prefix),
                            _parse_owner(arguments.owner))
            print(f'claude {pin.version} {pinned.platform}: installed payload, '
                  'launcher and layout match the committed pin')
            return 0
        raise SystemExit(2)
    except Refusal as error:
        print(f'claude_pin: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
