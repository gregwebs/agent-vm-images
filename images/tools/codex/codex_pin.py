#!/usr/bin/env python3
"""The committed content identity of one exact Codex release, and its preflight.

`pin.json` beside this module is the reviewed selection: the exact release, the
two maintained Linux package assets with their exact URLs and SHA-256 digests,
the original release metadata, the original `codex-package_SHA256SUMS` and both
original `.sigstore` bundles, plus the signer policy that was enforced when the
pin was made. This module is the only reader. It is stdlib-only and needs no
network, no image dependency and no payload archive to validate the closure.

Why a dedicated module instead of `images/artifacts/*.json`: Codex publishes a
*prefixless* package (`bin/codex` at the archive root) into a HOME-owned
`packages/standalone` tree with a `current` symlink, not a `usr/local`
membership. Forcing it into the Node/gh artifact model would have meant a fake
member prefix and a raised size ceiling. Only the transactional publication is
shared (script/dev/pin-artifact.py); the selection model is not.

What ordinary builds prove here: the committed release, the committed evidence
bytes (re-hashed now), the committed archive digest (re-hashed now), the package
metadata inside the archive, and the ELF machine of `bin/codex`. They do **not**
repeat cryptographic verification -- that happened once, on the maintainer host,
through `script/dev/pin-codex.py`. A well-formed `.sigstore` bundle is therefore
never described as cryptographically verified by this module; it is only checked
for the shape that makes the committed evidence a meaningful record.

CLI (all offline; nothing here resolves a release or writes a pin):

    codex_pin.py check     --pin-dir DIR --release TAG
    codex_pin.py select    --pin-dir DIR --release TAG --target TARGET
    codex_pin.py preflight --pin-dir DIR --release TAG --target TARGET --archive FILE

Exit status 1 is a refusal, 2 is an invalid invocation. This module never emits
the classified-transport status 75 and never writes a transport receipt.
"""
from __future__ import annotations

import argparse
import base64
import binascii
import datetime
import gzip
import hashlib
import io
import json
import os
import re
import stat
import sys
import tarfile
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from types import MappingProxyType
from typing import Callable, Mapping

SCHEMA = 1
NAME = 'codex'
KIND = 'codex-standalone-package'
REPOSITORY = 'openai/codex'
WORKFLOW = 'https://github.com/openai/codex/.github/workflows/rust-release.yml'
ISSUER = 'https://token.actions.githubusercontent.com'
VERIFIER_NAME = 'cosign'
# The reviewed host verifier. A different version is a maintenance decision and
# a schema change, never a command-line choice (docs/adr/0009-codex-payload-pins.md).
VERIFIER_VERSION = 'v2.4.1'
AUTHENTICATION_METHOD = 'sigstore-blob-pin-time'
TRUST_ROOT = 'sigstore-production-tuf'

PACKAGE_LAYOUT_VERSION = 1
PACKAGE_VARIANT = 'codex'
PACKAGE_ENTRYPOINT = 'bin/codex'

# The maintained platforms, and the only upstream targets each one may name.
TARGETS: Mapping[str, str] = MappingProxyType({
    'linux/amd64': 'x86_64-unknown-linux-musl',
    'linux/arm64': 'aarch64-unknown-linux-musl',
})
ELF_MACHINES: Mapping[str, int] = MappingProxyType({
    'x86_64-unknown-linux-musl': 62,
    'aarch64-unknown-linux-musl': 183,
})

METADATA_PATH = 'evidence/release.json'
CHECKSUMS_ASSET = 'codex-package_SHA256SUMS'
CHECKSUMS_PATH = 'evidence/codex-package_SHA256SUMS'
EVIDENCE_FILES = (METADATA_PATH, CHECKSUMS_PATH)

# Bound every read so a corrupt or hostile network response, a growing upstream
# asset or a compressed expansion cannot turn validation into an allocation.
# 0.159.3 ships a 160 MB compressed / 424 MB decoded package; 256 MiB and 1 GiB
# leave headroom for a normal version bump while making an accidental order-of-
# magnitude expansion a refusal instead of an out-of-memory. A larger release is
# a reviewed policy change, never an automatic widening.
MAX_COMPRESSED_BYTES = 256 << 20
MAX_DECODED_BYTES = 1 << 30
MAX_EXPANDED_REGULAR_BYTES = 1 << 30
MAX_LOGICAL_MEMBERS = 4096
MAX_PHYSICAL_HEADERS = 4096
MAX_PACKAGE_METADATA_BYTES = 64 << 10
MAX_EXTENDED_HEADER_BYTES = 64 << 10
MAX_EXTENDED_TOTAL_BYTES = 1 << 20
READ_CHUNK_BYTES = 1 << 16

# Evidence ceilings: the committed metadata is ~200 KB for a 176-asset release,
# the checksum manifest ~1.6 KB, each legacy bundle ~8.5 KB. The ceilings keep a
# rewritten upstream from turning "read the evidence" into a memory problem.
MAX_METADATA_BYTES = 8 << 20
MAX_CHECKSUMS_BYTES = 1 << 20
MAX_BUNDLE_BYTES = 4 << 20
MAX_PIN_BYTES = 64 << 10

# The binary this preflight inspects. Anything else at that path is outside the
# reviewed package layout.
CODE_ENTRYPOINT = PACKAGE_ENTRYPOINT
REQUIRED_EXECUTABLES = ('bin/codex', 'bin/codex-code-mode-host', 'codex-path/rg',
                        'codex-resources/bwrap')
PACKAGE_METADATA = 'codex-package.json'
# Upstream's other top-level roots (`codex-resources/voice`, `codex-resources/zsh`)
# sit under a reviewed root. A *new* root is a review event, not a silent pass.
TOP_LEVEL_ROOTS = frozenset({'bin', 'codex-path', 'codex-resources', PACKAGE_METADATA})
ELF_HEADER_BYTES = 20

# The owning hook's exact-slot grammar: canonical semver for the alpha/beta
# subset Codex publishes, with no leading zero in a numeric identifier. Kept
# byte-for-byte equivalent to images/tools/codex/install-codex.sh so a value the
# hook accepts cannot be a value this loader rejects, or the reverse.
RELEASE_BODY = (r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
                r'(-alpha(\.(0|[1-9][0-9]*)){0,2}|-beta(\.(0|[1-9][0-9]*))?)?')
VERSION_RE = re.compile(RELEASE_BODY)
SHA256_RE = re.compile(r'[0-9a-f]{64}')
METADATA_DIGEST_RE = re.compile(r'sha256:([0-9a-f]{64})')
CHECKSUM_LINE_RE = re.compile(r'([0-9a-fA-F]{64})[ \t]+\*?(\S.*)')
DATE_RE = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}')

ROOT_KEYS = ('schema', 'name', 'kind', 'release', 'version', 'package', 'metadata',
             'checksums', 'platforms', 'authentication')
PACKAGE_KEYS = ('layout_version', 'variant', 'entrypoint')
EVIDENCE_KEYS = ('url', 'path', 'sha256')
PLATFORM_KEYS = ('target', 'asset', 'url', 'sha256', 'bundle')
AUTHENTICATION_KEYS = ('method', 'verifier', 'trust_root', 'certificate_identity',
                       'certificate_oidc_issuer', 'verified_on')
VERIFIER_KEYS = ('name', 'version')
BUNDLE_KEYS = ('base64Signature', 'cert', 'rekorBundle')
REKOR_BUNDLE_KEYS = ('Payload', 'SignedEntryTimestamp')
REKOR_PAYLOAD_KEYS = ('body', 'integratedTime', 'logID', 'logIndex')


class Refusal(Exception):
    """A committed input is not what the reviewed pin says it is."""


class Platform(Enum):
    AMD64 = 'linux/amd64'
    ARM64 = 'linux/arm64'


class Target(Enum):
    AMD64 = 'x86_64-unknown-linux-musl'
    ARM64 = 'aarch64-unknown-linux-musl'


@dataclass(frozen=True)
class Sha256:
    value: str


@dataclass(frozen=True)
class Evidence:
    url: str
    path: str
    digest: Sha256


@dataclass(frozen=True)
class PlatformPin:
    platform: Platform
    target: Target
    asset: str
    url: str
    digest: Sha256
    bundle: Evidence


@dataclass(frozen=True)
class Authentication:
    verifier_version: str
    trust_root: str
    identity: str
    issuer: str
    verified_on: datetime.date


@dataclass(frozen=True)
class ChecksumLine:
    """One decoded `sha256sum`-style manifest row."""

    digest: str
    name: str


def parse_checksums(raw: bytes, what: str) -> tuple[ChecksumLine, ...]:
    """Decode a `sha256sum`-style manifest into typed records, exactly once.

    The offline loader (committed evidence) and the maintenance writer (fresh
    upstream bytes) both read this grammar, so a change to it cannot be made in
    one security parser and forgotten in the other. Only the grammar and its
    duplicate rejection live here: what each caller does with the records --
    corroborating the pin, or corroborating a download -- stays with the caller.
    """
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise Refusal(f'{what} are not UTF-8') from None
    rows: list[ChecksumLine] = []
    seen: set[str] = set()
    for line in text.split('\n'):
        if not line.strip():
            continue
        match = CHECKSUM_LINE_RE.fullmatch(line)
        if match is None:
            raise Refusal(f'{what} have an unreadable line: {line!r}')
        digest, name = match.group(1).lower(), match.group(2)
        if name in seen:
            raise Refusal(f'{what} repeat the entry {name!r}')
        seen.add(name)
        rows.append(ChecksumLine(digest=digest, name=name))
    return tuple(rows)


@dataclass(frozen=True)
class CodexPin:
    directory: Path
    release: str
    version: str
    metadata: Evidence
    checksums: Evidence
    platforms: Mapping[Platform, PlatformPin]
    authentication: Authentication


BinaryInspector = Callable[[bytes, Target], None]


# --- canonical derivations ---------------------------------------------------
#
# Every URL, filename and evidence path is reconstructed from the validated
# release here. Callers compare for equality, never for a prefix: a lookalike
# origin, an extra path segment or a different repository can then not satisfy
# the check by sharing a leading substring.

def api_url(release: str) -> str:
    return f'https://api.github.com/repos/{REPOSITORY}/releases/tags/{release}'


def asset_url(release: str, asset: str) -> str:
    return f'https://github.com/{REPOSITORY}/releases/download/{release}/{asset}'


def asset_name(target: Target) -> str:
    return f'codex-package-{target.value}.tar.gz'


def bundle_name(target: Target) -> str:
    return f'{asset_name(target)}.sigstore'


def bundle_path(target: Target) -> str:
    return f'evidence/{bundle_name(target)}'


def identity(release: str) -> str:
    return f'{WORKFLOW}@refs/tags/{release}'


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
    """One complete JSON document with no duplicate keys and no trailing input."""
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
    # keep `"schema": true` out.
    if type(value) is not int:
        raise Refusal(f'{what} must be an integer')
    return value


def _digest(value: object, what: str) -> Sha256:
    raw = _string(value, what)
    if not SHA256_RE.fullmatch(raw):
        raise Refusal(f'{what} must be 64 lower-case hex characters')
    return Sha256(raw)


def require_release(release: object, what: str = 'release') -> str:
    """The only exact `rust-v` spelling the hook and this module both accept."""
    raw = _string(release, what)
    if not raw.startswith('rust-v'):
        raise Refusal(f'{what} must be rust-v<version>, found {raw!r}')
    body = raw[len('rust-v'):]
    if not VERSION_RE.fullmatch(body):
        raise Refusal(f'{what} is not an exact Codex version: {raw!r}')
    return raw


def release_version(release: str) -> str:
    return release[len('rust-v'):]


def require_target(target: object, what: str = 'target') -> Target:
    raw = _string(target, what)
    for candidate in Target:
        if candidate.value == raw:
            return candidate
    raise Refusal(f'{what} must be one of {[c.value for c in Target]}, found {raw!r}')


# --- evidence files ----------------------------------------------------------

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


def _read_regular(path: Path, limit: int, what: str) -> bytes:
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


def digest_of(raw: bytes) -> Sha256:
    return Sha256(hashlib.sha256(raw).hexdigest())


def _hex(digest: Sha256) -> str:
    return digest.value


# --- schema ------------------------------------------------------------------

def _evidence(value: object, url: str, path: str, what: str) -> Evidence:
    members = _object(value, EVIDENCE_KEYS, what)
    found_url = _string(members['url'], f'{what} url')
    if found_url != url:
        raise Refusal(f'{what} url must be {url}, found {found_url!r}')
    found_path = _string(members['path'], f'{what} path')
    if found_path != path:
        raise Refusal(f'{what} path must be {path}, found {found_path!r}')
    return Evidence(url=url, path=path, digest=_digest(members['sha256'], f'{what} sha256'))


def _platform(value: object, platform: Platform, release: str) -> PlatformPin:
    what = f'{platform.value} platform'
    members = _object(value, PLATFORM_KEYS, what)
    target = require_target(members['target'], f'{what} target')
    expected_target = TARGETS[platform.value]
    if target.value != expected_target:
        raise Refusal(f'{what} target must be {expected_target}, found {target.value}')
    asset = _string(members['asset'], f'{what} asset')
    if asset != asset_name(target):
        raise Refusal(f'{what} asset must be {asset_name(target)}, found {asset!r}')
    url = _string(members['url'], f'{what} url')
    expected_url = asset_url(release, asset)
    if url != expected_url:
        raise Refusal(f'{what} url must be {expected_url}, found {url!r}')
    bundle = _evidence(members['bundle'], asset_url(release, bundle_name(target)),
                       bundle_path(target), f'{what} bundle')
    return PlatformPin(platform=platform, target=target, asset=asset, url=url,
                       digest=_digest(members['sha256'], f'{what} sha256'), bundle=bundle)


def _authentication(value: object, release: str) -> Authentication:
    what = 'authentication'
    members = _object(value, AUTHENTICATION_KEYS, what)
    method = _string(members['method'], f'{what} method')
    if method != AUTHENTICATION_METHOD:
        raise Refusal(f'{what} method must be {AUTHENTICATION_METHOD}, found {method!r}')
    verifier = _object(members['verifier'], VERIFIER_KEYS, f'{what} verifier')
    name = _string(verifier['name'], f'{what} verifier name')
    if name != VERIFIER_NAME:
        raise Refusal(f'{what} verifier must be {VERIFIER_NAME}, found {name!r}')
    version = _string(verifier['version'], f'{what} verifier version')
    if version != VERIFIER_VERSION:
        raise Refusal(f'{what} verifier version must be {VERIFIER_VERSION}, '
                      f'found {version!r}')
    trust_root = _string(members['trust_root'], f'{what} trust_root')
    if trust_root != TRUST_ROOT:
        raise Refusal(f'{what} trust_root must be {TRUST_ROOT}, found {trust_root!r}')
    found_identity = _string(members['certificate_identity'], f'{what} identity')
    if found_identity != identity(release):
        raise Refusal(f'{what} certificate identity must be {identity(release)}, '
                      f'found {found_identity!r}')
    found_issuer = _string(members['certificate_oidc_issuer'], f'{what} issuer')
    if found_issuer != ISSUER:
        raise Refusal(f'{what} OIDC issuer must be {ISSUER}, found {found_issuer!r}')
    verified_on = _string(members['verified_on'], f'{what} verified_on')
    if not DATE_RE.fullmatch(verified_on):
        raise Refusal(f'{what} verified_on must be YYYY-MM-DD, found {verified_on!r}')
    try:
        day = datetime.date.fromisoformat(verified_on)
    except ValueError:
        raise Refusal(f'{what} verified_on is not a calendar date: {verified_on!r}') from None
    if day > datetime.date.today():
        raise Refusal(f'{what} verified_on {verified_on} is in the future')
    return Authentication(verifier_version=version, trust_root=trust_root,
                          identity=found_identity, issuer=found_issuer, verified_on=day)


def parse_pin(raw: bytes, directory: Path) -> CodexPin:
    """Schema validation only. Evidence is a separate, later step."""
    members = _object(strict_json(raw, 'pin.json'), ROOT_KEYS, 'pin')
    if _integer(members['schema'], 'pin schema') != SCHEMA:
        raise Refusal(f'pin schema must be {SCHEMA}')
    if _string(members['name'], 'pin name') != NAME:
        raise Refusal(f'pin name must be {NAME}')
    if _string(members['kind'], 'pin kind') != KIND:
        raise Refusal(f'pin kind must be {KIND}')
    release = require_release(members['release'], 'pin release')
    version = _string(members['version'], 'pin version')
    if not VERSION_RE.fullmatch(version):
        raise Refusal(f'pin version is not an exact Codex version: {version!r}')
    if release != f'rust-v{version}':
        raise Refusal(f'pin release {release} does not spell pin version {version}')
    package = _object(members['package'], PACKAGE_KEYS, 'pin package')
    if _integer(package['layout_version'], 'pin package layout_version') != PACKAGE_LAYOUT_VERSION:
        raise Refusal(f'pin package layout_version must be {PACKAGE_LAYOUT_VERSION}')
    if _string(package['variant'], 'pin package variant') != PACKAGE_VARIANT:
        raise Refusal(f'pin package variant must be {PACKAGE_VARIANT}')
    if _string(package['entrypoint'], 'pin package entrypoint') != PACKAGE_ENTRYPOINT:
        raise Refusal(f'pin package entrypoint must be {PACKAGE_ENTRYPOINT}')
    metadata = _evidence(members['metadata'], api_url(release), METADATA_PATH,
                         'metadata evidence')
    checksums = _evidence(members['checksums'], asset_url(release, CHECKSUMS_ASSET),
                          CHECKSUMS_PATH, 'checksums evidence')
    platforms_members = _object(members['platforms'], tuple(p.value for p in Platform),
                               'pin platforms')
    platforms = {platform: _platform(platforms_members[platform.value], platform, release)
                 for platform in Platform}
    authentication = _authentication(members['authentication'], release)
    return CodexPin(directory=directory, release=release, version=version,
                    metadata=metadata, checksums=checksums,
                    platforms=MappingProxyType(platforms), authentication=authentication)


# --- committed evidence ------------------------------------------------------

def _check_metadata(raw: bytes, pin: CodexPin) -> None:
    """The original exact-tag API document, cross-checked against the pin.

    Only the security projection is inspected: the tag and the per-asset names
    and digests. Download counts, body text and timestamps are mutable and are
    deliberately ignored here; the maintenance writer applies the same rule when
    it decides whether a repeat run changed anything (#97 plan step 4.5).
    """
    document = strict_json(raw, 'committed release metadata')
    if not isinstance(document, dict):
        raise Refusal('committed release metadata must be a JSON object')
    tag = document.get('tag_name')
    if not isinstance(tag, str):
        raise Refusal('committed release metadata has no string tag_name')
    if tag != pin.release:
        raise Refusal(f'committed release metadata names {tag!r}, pin names {pin.release!r}')
    assets = document.get('assets')
    if not isinstance(assets, list):
        raise Refusal('committed release metadata has no assets array')
    digests: dict[str, str | None] = {}
    for entry in assets:
        if not isinstance(entry, dict):
            raise Refusal('committed release metadata has a non-object asset')
        name = entry.get('name')
        if not isinstance(name, str) or not name:
            raise Refusal('committed release metadata has an asset with no name')
        if name in digests:
            raise Refusal(f'committed release metadata repeats asset {name!r}')
        digest = entry.get('digest')
        if digest is not None:
            if not isinstance(digest, str):
                raise Refusal(f'committed release metadata asset {name} has a non-string digest')
            match = METADATA_DIGEST_RE.fullmatch(digest)
            if match is None:
                raise Refusal(f'committed release metadata asset {name} has a '
                              f'non-sha256 digest {digest!r}')
            digest = match.group(1)
        digests[name] = digest

    expected: list[tuple[str, Sha256, bool]] = [
        (CHECKSUMS_ASSET, pin.checksums.digest, True)]
    for platform_pin in pin.platforms.values():
        expected.append((platform_pin.asset, platform_pin.digest, True))
        expected.append((bundle_name(platform_pin.target), platform_pin.bundle.digest, False))
    for name, committed, digest_required in expected:
        if name not in digests:
            raise Refusal(f'committed release metadata does not list the required asset {name!r}')
        found = digests[name]
        if found is None:
            if digest_required:
                raise Refusal(f'committed release metadata has no digest for {name!r}')
            continue
        if found != _hex(committed):
            raise Refusal(f'committed release metadata digest for {name} is {found}, '
                          f'but the pin commits {_hex(committed)}')


def _check_checksums(raw: bytes, pin: CodexPin) -> None:
    """The original `SHA256SUMS`: corroboration for exactly the committed digests.

    Other upstream rows stay in the committed file as reviewed evidence but give
    no install authority, so only the two maintained payload lines are compared.
    The grammar itself is `parse_checksums`, shared with the maintenance writer.
    """
    declared = {line.name: line.digest
                for line in parse_checksums(raw, 'committed checksums')}
    for platform_pin in pin.platforms.values():
        found = declared.get(platform_pin.asset)
        if found is None:
            raise Refusal(f'committed checksums have no line for {platform_pin.asset!r}')
        if found != _hex(platform_pin.digest):
            raise Refusal(f'committed checksums disagree with the pin for '
                          f'{platform_pin.asset}: {found} vs {_hex(platform_pin.digest)}')


def _decode_base64(value: object, what: str) -> bytes:
    raw = _string(value, what)
    try:
        return base64.b64decode(raw, validate=True)
    except (binascii.Error, ValueError):
        raise Refusal(f'{what} is not base64') from None


def _check_bundle(raw: bytes, platform_pin: PlatformPin) -> None:
    """Shape and cross-field consistency of a committed legacy cosign bundle.

    This is not signature verification and this function never claims any: it
    proves the committed bytes are a legacy bundle for *this* payload digest, so
    a swapped, truncated or re-architected bundle is refused here instead of
    being carried into the pin as meaningless evidence. Cryptographic
    verification ran once on the maintainer host (script/dev/pin-codex.py).
    """
    what = f'{platform_pin.bundle.path}'
    members = _object(strict_json(raw, what), BUNDLE_KEYS, what)
    _decode_base64(members['base64Signature'], f'{what} base64Signature')
    certificate = _decode_base64(members['cert'], f'{what} cert')
    if b'-----BEGIN CERTIFICATE-----' not in certificate:
        raise Refusal(f'{what} cert is not a PEM certificate')
    rekor = _object(members['rekorBundle'], REKOR_BUNDLE_KEYS, f'{what} rekorBundle')
    _decode_base64(rekor['SignedEntryTimestamp'], f'{what} SignedEntryTimestamp')
    payload = _object(rekor['Payload'], REKOR_PAYLOAD_KEYS, f'{what} Payload')
    body = strict_json(_decode_base64(payload['body'], f'{what} Payload.body'),
                       f'{what} Payload.body')
    if not isinstance(body, dict) or body.get('kind') != 'hashedrekord':
        raise Refusal(f'{what} does not carry a hashedrekord entry')
    spec = body.get('spec')
    if not isinstance(spec, dict):
        raise Refusal(f'{what} entry has no spec')
    data = spec.get('data')
    hash_field = data.get('hash') if isinstance(data, dict) else None
    if not isinstance(hash_field, dict):
        raise Refusal(f'{what} entry names no payload hash')
    if hash_field.get('algorithm') != 'sha256':
        raise Refusal(f'{what} entry is not a sha256 hashedrekord')
    value = hash_field.get('value')
    if value != _hex(platform_pin.digest):
        raise Refusal(f'{what} entry is for {value!r}, but {platform_pin.asset} is '
                      f'{_hex(platform_pin.digest)}')


def load_pin(directory: Path) -> CodexPin:
    """Load the reviewed closure from `directory`; every evidence byte is checked."""
    directory = Path(directory)
    raw_pin = _read_regular(directory / 'pin.json', MAX_PIN_BYTES, 'pin.json')
    pin = parse_pin(raw_pin, directory)
    metadata = _read_regular(_regular_file(directory, pin.metadata.path, 'metadata evidence'),
                             MAX_METADATA_BYTES, 'committed release metadata')
    if digest_of(metadata) != pin.metadata.digest:
        raise Refusal('committed release metadata does not match the pin digest')
    _check_metadata(metadata, pin)
    checksums = _read_regular(_regular_file(directory, pin.checksums.path, 'checksums evidence'),
                             MAX_CHECKSUMS_BYTES, 'committed checksums')
    if digest_of(checksums) != pin.checksums.digest:
        raise Refusal('committed checksums do not match the pin digest')
    _check_checksums(checksums, pin)
    # Both architectures, including the one this build will not install: a pin
    # that carries stale evidence for the other platform is not a closure.
    for platform_pin in pin.platforms.values():
        bundle = _read_regular(_regular_file(directory, platform_pin.bundle.path,
                                            f'{platform_pin.platform.value} bundle evidence'),
                               MAX_BUNDLE_BYTES, f'{platform_pin.platform.value} bundle')
        if digest_of(bundle) != platform_pin.bundle.digest:
            raise Refusal(f'{platform_pin.platform.value} bundle does not match the pin digest')
        _check_bundle(bundle, platform_pin)
    return pin


def select(pin: CodexPin, release: str, target: Target) -> PlatformPin:
    """The reviewed payload for one exact release and one upstream target."""
    exact = require_release(release, 'requested release')
    if exact != pin.release:
        raise Refusal(f'{exact} is not the pinned Codex release {pin.release}; '
                      'an exact but unreviewed version needs a reviewed pin')
    for platform_pin in pin.platforms.values():
        if platform_pin.target is target:
            return platform_pin
    raise Refusal(f'{target.value} is not a maintained Codex platform')


# --- archive preflight -------------------------------------------------------

def require_elf(header: bytes, target: Target) -> None:
    """The first 20 bytes of the reviewed entrypoint must be usable on `target`.

    Checked without extraction and without execution: a wrong-machine ELF is
    refused even when a synthetic pin and checksum manifest consistently hash
    that archive, so "the checksums agree" can never stand in for "this is a
    Linux/<<arch>> binary".
    """
    if len(header) < ELF_HEADER_BYTES:
        raise Refusal('bin/codex is shorter than an ELF header')
    if header[:4] != b'\x7fELF':
        raise Refusal('bin/codex is not an ELF object')
    if header[4] != 2:
        raise Refusal('bin/codex is not a 64-bit ELF object')
    if header[5] != 1:
        raise Refusal('bin/codex is not a little-endian ELF object')
    if header[6] != 1:
        raise Refusal('bin/codex is not a current-version ELF object')
    machine = int.from_bytes(header[18:20], 'little')
    expected = ELF_MACHINES[target.value]
    if machine != expected:
        raise Refusal(f'bin/codex targets ELF machine {machine}, not {expected} '
                      f'({target.value})')


class _DecodedStream(io.RawIOBase):
    """The decompressed archive, counted before anything can consume it.

    `tarfile` in random-access mode reads through its own file object, so a limit
    applied to yielded members never sees GNU/PAX extended headers, 512-byte
    padding, or the bytes it discards while seeking. Counting the decoded stream
    bounds all of it: a small compressed archive cannot expand past
    `MAX_DECODED_BYTES` before the parser can allocate anything.

    Every `read` is capped at `READ_CHUNK_BYTES` and at the remaining allowance
    plus one byte, including a `read(-1)` or a caller asking for gigabytes, so a
    giant declared size can never turn into a giant allocation here.
    """

    def __init__(self, path: Path, what: str, limit: int | None = None) -> None:
        try:
            self._handle = open(path, 'rb')
            self._decoder = gzip.GzipFile(fileobj=self._handle, mode='rb')
        except OSError as error:
            raise Refusal(f'{what}: cannot open the archive: {error}') from None
        self._what = what
        self._limit = MAX_DECODED_BYTES if limit is None else limit
        self.decoded = 0

    def readable(self) -> bool:
        return True

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            size = READ_CHUNK_BYTES
        allowance = self._limit - self.decoded
        try:
            chunk = self._decoder.read(min(size, READ_CHUNK_BYTES, allowance + 1))
        except (OSError, EOFError) as error:
            raise Refusal(f'{self._what}: cannot decode the gzip stream: {error}') from None
        self.decoded += len(chunk)
        if self.decoded > self._limit:
            raise Refusal(f'{self._what}: decoded stream exceeds {self._limit} bytes')
        return chunk

    def drain(self) -> None:
        """Read to EOF so gzip integrity (CRC/length) and any trailing member is seen."""
        while self.read(READ_CHUNK_BYTES):
            pass

    def close(self) -> None:
        if not self.closed:
            self._decoder.close()
            self._handle.close()
        super().close()


def _discard(stream: _DecodedStream, size: int, what: str) -> None:
    remaining = size
    while remaining > 0:
        chunk = stream.read(min(remaining, READ_CHUNK_BYTES))
        if not chunk:
            raise Refusal(f'{what}: truncated archive header')
        remaining -= len(chunk)


def _read_bounded(stream: _DecodedStream, size: int, what: str) -> bytes:
    payload = bytearray()
    while len(payload) < size:
        chunk = stream.read(min(size - len(payload), READ_CHUNK_BYTES))
        if not chunk:
            raise Refusal(f'{what}: truncated archive member data')
        payload += chunk
    return bytes(payload)


def _tar_header_size(field: bytes, what: str) -> int:
    """The size field of a tar header, octal or GNU base-256."""
    if field[:1] and field[0] & 0x80:
        return int.from_bytes(field[1:], 'big')
    text = field.split(b'\x00', 1)[0].strip(b' ')
    if not text:
        return 0
    try:
        return int(text, 8)
    except ValueError:
        raise Refusal(f'{what}: unreadable tar header size field') from None


def _v7_directory_header(block: bytes, typeflag: bytes) -> bool:
    """Whether tarfile's V7 normalization reads this header as a directory."""
    if typeflag != tarfile.AREGTYPE:
        return False
    return block[:100].split(b'\x00', 1)[0].endswith(b'/')


def _pax_records(payload: bytes, what: str) -> dict[bytes, bytes]:
    records: dict[bytes, bytes] = {}
    position = 0
    while position < len(payload) and payload[position] != 0:
        space = payload.find(b' ', position)
        if space < 0:
            raise Refusal(f'{what}: unreadable PAX record at {position}')
        try:
            length = int(payload[position:space])
        except ValueError:
            raise Refusal(f'{what}: unreadable PAX record length') from None
        if length < 5 or position + length > len(payload):
            raise Refusal(f'{what}: PAX record length outside its header')
        record = payload[space + 1:position + length]
        if not record.endswith(b'\n'):
            raise Refusal(f'{what}: PAX record is not newline-terminated')
        key, separator, value = record[:-1].partition(b'=')
        if not key or separator != b'=':
            raise Refusal(f'{what}: unreadable PAX record')
        records[key] = value
        position += length
    return records


def _pax_size(pending: list[dict[bytes, bytes]], what: str) -> int | None:
    for records in pending:
        if b'size' not in records:
            continue
        try:
            value = int(records[b'size'])
        except ValueError:
            # tarfile's PAX number conversion falls back to zero, so the layout
            # tarfile will use is a zero-length member.
            return 0
        if value < 0:
            raise Refusal(f'{what}: negative PAX size record')
        return value
    return None


def _reject_sparse_metadata(records: dict[bytes, bytes], what: str) -> None:
    for key in records:
        if key.startswith(b'GNU.sparse.'):
            raise Refusal(f'{what}: sparse PAX metadata is outside the archive policy')


EXTENDED_HEADER_TYPES = {b'L': 'gnu', b'K': 'gnu', b'x': 'pax', b'X': 'pax', b'g': 'global'}


def _bound_extended_headers(stream: _DecodedStream, what: str) -> None:
    """Bound GNU/PAX extended headers before tarfile can allocate them.

    tarfile reads `L`/`K`/`x`/`g`/`X` pseudo-members itself and allocates their
    contents before yielding anything, so the member-level caps never see them.
    This pass walks the same 512-byte blocks first: each extended header must fit
    `MAX_EXTENDED_HEADER_BYTES`, all of them together `MAX_EXTENDED_TOTAL_BYTES`,
    and the whole file `MAX_PHYSICAL_HEADERS`.

    It reproduces tarfile's own offset arithmetic, because a bound that walks the
    blocks differently from the parser is not a bound: a PAX `size` record
    replaces the header's size field for the member that follows. Sparse metadata
    and a V7 directory header that claims data are refused: their layouts are not
    a raw size field, so the two walks could disagree.
    """
    total = 0
    headers = 0
    global_records: dict[bytes, bytes] = {}
    # tarfile applies a later extended header first and an earlier one last, so
    # the earliest pending record set has the final word on `size`.
    pending: list[dict[bytes, bytes]] = []
    while True:
        block = stream.read(512)
        if len(block) < 512:
            raise Refusal(f'{what}: truncated archive header')
        if block == b'\x00' * 512:
            return
        headers += 1
        if headers > MAX_PHYSICAL_HEADERS:
            raise Refusal(f'{what}: more than {MAX_PHYSICAL_HEADERS} archive headers')
        typeflag = block[156:157]
        size = _tar_header_size(block[124:136], what)
        if size < 0:
            raise Refusal(f'{what}: negative tar header size')
        kind = EXTENDED_HEADER_TYPES.get(typeflag)
        if kind is not None:
            if size > MAX_EXTENDED_HEADER_BYTES:
                raise Refusal(f'{what}: extended header of {size} bytes exceeds '
                              f'{MAX_EXTENDED_HEADER_BYTES}')
            total += size
            if total > MAX_EXTENDED_TOTAL_BYTES:
                raise Refusal(f'{what}: extended headers total {total} bytes, '
                              f'exceeding {MAX_EXTENDED_TOTAL_BYTES}')
            if kind == 'gnu':
                # The long name itself is never allocated: it is bounded already.
                _discard(stream, size, what)
                payload = b''
            else:
                payload = _read_bounded(stream, size, what)
            _discard(stream, -size % 512, what)
            if kind in ('pax', 'global'):
                records = _pax_records(payload, what)
                _reject_sparse_metadata(records, what)
                if kind == 'global':
                    global_records.update(records)
                else:
                    merged = dict(global_records)
                    merged.update(records)
                    pending.append(merged)
            continue
        if typeflag == tarfile.GNUTYPE_SPARSE:
            raise Refusal(f'{what}: GNU sparse members are outside the archive policy')
        if pending:
            override = _pax_size(pending, what)
            if override is not None:
                size = override
            pending = []
        if _v7_directory_header(block, typeflag):
            if size:
                raise Refusal(f'{what}: V7 directory header declares {size} bytes of data')
            continue
        if typeflag in tarfile.REGULAR_TYPES or typeflag not in tarfile.SUPPORTED_TYPES:
            _discard(stream, size, what)
            _discard(stream, -size % 512, what)


def _canonical_name(name: str, is_dir: bool, what: str) -> str:
    """The member name this policy accepts, with a directory's trailing `/` gone."""
    if name.startswith('/'):
        raise Refusal(f'{what}: absolute member name {name!r}')
    stripped = name[:-1] if is_dir and name.endswith('/') else name
    if not stripped or stripped != stripped.strip():
        raise Refusal(f'{what}: unusable member name {name!r}')
    parts = stripped.split('/')
    if any(part in ('', '.', '..') for part in parts):
        raise Refusal(f'{what}: non-canonical member name {name!r}')
    return stripped


def _check_member(member: tarfile.TarInfo, what: str) -> str:
    name = _canonical_name(member.name, member.isdir(), what)
    if member.mode & 0o6000:
        raise Refusal(f'{what}: setuid/setgid member {name!r}')
    if member.issym() or member.islnk():
        # A prefixless package has no reviewed link resolver: a link is a policy
        # change (whether to resolve it, and how) rather than an archive quirk.
        raise Refusal(f'{what}: link member {name!r} is outside the archive policy')
    if member.isdev() or member.isfifo() or member.issparse():
        raise Refusal(f'{what}: special member {name!r} is outside the archive policy')
    if not (member.isreg() or member.isdir()):
        raise Refusal(f'{what}: unsupported member type for {name!r}')
    return name


def _check_package_metadata(raw: bytes, pin: CodexPin, target: Target) -> None:
    members = _object(strict_json(raw, PACKAGE_METADATA),
                      ('layoutVersion', 'version', 'target', 'variant', 'entrypoint',
                       'resourcesDir', 'pathDir'), PACKAGE_METADATA)
    if _integer(members['layoutVersion'], 'package layoutVersion') != PACKAGE_LAYOUT_VERSION:
        raise Refusal(f'package layoutVersion must be {PACKAGE_LAYOUT_VERSION}')
    if _string(members['version'], 'package version') != pin.version:
        raise Refusal(f'package version is not the pinned {pin.version}')
    if _string(members['target'], 'package target') != target.value:
        raise Refusal(f'package target is not {target.value}')
    if _string(members['variant'], 'package variant') != PACKAGE_VARIANT:
        raise Refusal(f'package variant must be {PACKAGE_VARIANT}')
    if _string(members['entrypoint'], 'package entrypoint') != PACKAGE_ENTRYPOINT:
        raise Refusal(f'package entrypoint must be {PACKAGE_ENTRYPOINT}')


def _inventory(stream: _DecodedStream, pin: CodexPin, target: Target,
               what: str) -> bytes:
    """Validate every member and return the entrypoint's first 20 bytes."""
    with tarfile.open(fileobj=stream, mode='r|') as archive:
        names: dict[str, tarfile.TarInfo] = {}
        expanded = 0
        metadata: bytes | None = None
        elf_header: bytes | None = None
        for index, member in enumerate(archive):
            if index >= MAX_LOGICAL_MEMBERS:
                raise Refusal(f'{what}: more than {MAX_LOGICAL_MEMBERS} members')
            name = _check_member(member, what)
            if name in names:
                raise Refusal(f'{what}: duplicate archive member {name!r}')
            names[name] = member
            root = name.split('/', 1)[0]
            if root not in TOP_LEVEL_ROOTS:
                raise Refusal(f'{what}: unexpected top-level entry {root!r}')
            if not member.isreg():
                continue
            expanded += member.size
            if expanded > MAX_EXPANDED_REGULAR_BYTES:
                raise Refusal(f'{what}: expanded regular files total {expanded} bytes, '
                              f'exceeding {MAX_EXPANDED_REGULAR_BYTES}')
            if name == PACKAGE_METADATA:
                if member.size > MAX_PACKAGE_METADATA_BYTES:
                    raise Refusal(f'{what}: {PACKAGE_METADATA} is {member.size} bytes, '
                                  f'exceeding {MAX_PACKAGE_METADATA_BYTES}')
                handle = archive.extractfile(member)
                assert handle is not None
                metadata = _read_bounded_member(handle, member.size, what)
            elif name == CODE_ENTRYPOINT:
                handle = archive.extractfile(member)
                assert handle is not None
                elf_header = handle.read(ELF_HEADER_BYTES)
    for name in names:
        parts = name.split('/')
        for index in range(1, len(parts)):
            ancestor = names.get('/'.join(parts[:index]))
            if ancestor is not None and not ancestor.isdir():
                raise Refusal(f'{what}: {name!r} has a non-directory ancestor '
                              f'{ancestor.name!r}')
    for required in REQUIRED_EXECUTABLES:
        member = names.get(required)
        if member is None:
            raise Refusal(f'{what}: missing required file {required}')
        if not member.isreg():
            raise Refusal(f'{what}: {required} is not a regular file')
        if not member.mode & 0o111:
            raise Refusal(f'{what}: {required} is not executable')
    if PACKAGE_METADATA not in names:
        raise Refusal(f'{what}: missing {PACKAGE_METADATA}')
    if metadata is None:
        raise Refusal(f'{what}: cannot read {PACKAGE_METADATA}')
    _check_package_metadata(metadata, pin, target)
    if elf_header is None:
        raise Refusal(f'{what}: cannot read {CODE_ENTRYPOINT}')
    return elf_header


def _read_bounded_member(handle: io.BufferedIOBase, size: int, what: str) -> bytes:
    payload = bytearray()
    while len(payload) < size:
        chunk = handle.read(min(size - len(payload), READ_CHUNK_BYTES))
        if not chunk:
            raise Refusal(f'{what}: truncated archive member data')
        payload += chunk
    return bytes(payload)


def _hash_archive(path: Path, limit: int, what: str) -> Sha256:
    hasher = hashlib.sha256()
    total = 0
    with open(path, 'rb') as handle:
        while True:
            chunk = handle.read(READ_CHUNK_BYTES)
            if not chunk:
                break
            total += len(chunk)
            if total > limit:
                raise Refusal(f'{what}: compressed archive exceeds {limit} bytes')
            hasher.update(chunk)
    return Sha256(hasher.hexdigest())


def preflight_archive(pin: CodexPin, selected: PlatformPin, archive: Path,
                      *, inspect_binary: BinaryInspector = require_elf) -> None:
    """Prove the downloaded archive is the reviewed one, before anything uses it.

    Order matters: the committed digest is re-computed first, then a raw-header
    pass bounds every GNU/PAX metadata body, then a bounded streaming inventory
    checks the layout, the package metadata and the entrypoint's ELF machine.
    Nothing is extracted, chmodded or executed, so every failure above happens
    before `tar` and before the native probe.
    """
    what = selected.asset
    try:
        info = os.lstat(archive)
    except FileNotFoundError:
        raise Refusal(f'{what}: no downloaded archive at {archive}') from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise Refusal(f'{what}: {archive} is not a regular file')
    if info.st_size > MAX_COMPRESSED_BYTES:
        raise Refusal(f'{what}: compressed archive is {info.st_size} bytes, exceeding '
                      f'{MAX_COMPRESSED_BYTES}')
    actual = _hash_archive(archive, MAX_COMPRESSED_BYTES, what)
    if actual != selected.digest:
        raise Refusal(f'{what}: archive sha256 {_hex(actual)} does not match the '
                      f'committed {_hex(selected.digest)}')
    stream = _DecodedStream(archive, what)
    try:
        _bound_extended_headers(stream, what)
        stream.drain()
    finally:
        stream.close()
    stream = _DecodedStream(archive, what)
    try:
        try:
            elf_header = _inventory(stream, pin, selected.target, what)
        except (tarfile.TarError, OSError) as error:
            raise Refusal(f'{what}: cannot read the gzip archive: {error}') from None
        stream.drain()
    finally:
        stream.close()
    # The ELF check runs last, on the bytes the inventory read, so a package whose
    # metadata and archive are internally consistent but built for the other
    # machine is still refused here rather than at the native probe.
    inspect_binary(elf_header, selected.target)


# --- CLI ---------------------------------------------------------------------

class _Parser(argparse.ArgumentParser):
    """Argument errors exit 2; a refusal is a separate status handled by main."""

    def error(self, message: str) -> None:  # pragma: no cover - argparse path
        print(f'codex_pin: {message}', file=sys.stderr)
        raise SystemExit(2)


def _parser() -> argparse.ArgumentParser:
    parser = _Parser(prog='codex_pin.py', description=__doc__.split('\n')[0])
    verbs = parser.add_subparsers(dest='verb', required=True, parser_class=_Parser)
    check = verbs.add_parser('check', help='validate the committed closure offline')
    check.add_argument('--pin-dir', required=True)
    check.add_argument('--release', required=True)
    for verb in ('select', 'preflight'):
        command = verbs.add_parser(verb)
        command.add_argument('--pin-dir', required=True)
        command.add_argument('--release', required=True)
        command.add_argument('--target', required=True)
        if verb == 'preflight':
            command.add_argument('--archive', required=True)
    return parser


def main(argv: list[str], *, inspect_binary: BinaryInspector = require_elf) -> int:
    arguments = _parser().parse_args(argv)
    try:
        requested = require_release(arguments.release, 'requested release')
        pin = load_pin(Path(arguments.pin_dir))
        if requested != pin.release:
            raise Refusal(f'{requested} is not the pinned Codex release {pin.release}; '
                          'an exact but unreviewed version needs a reviewed pin')
        if arguments.verb == 'check':
            print(f'codex {pin.release}: offline committed-input check '
                  '(committed bytes and policy only; not a signature check)')
            for platform in Platform:
                platform_pin = pin.platforms[platform]
                print(f'  {platform.value} {platform_pin.target.value} '
                      f'{_hex(platform_pin.digest)}')
            print(f'  evidence: {len(EVIDENCE_FILES)} files plus '
                  f'{len(pin.platforms)} structurally consistent bundles')
            return 0
        target = require_target(arguments.target)
        selected = select(pin, requested, target)
        if arguments.verb == 'select':
            print(pin.version)
            print(selected.asset)
            print(selected.url)
            print(_hex(selected.digest))
            return 0
        preflight_archive(pin, selected, Path(arguments.archive),
                          inspect_binary=inspect_binary)
        print(f'codex {pin.version} {selected.platform.value}: committed identity, '
              f'package metadata and ELF machine verified before extraction')
        return 0
    except Refusal as error:
        print(f'codex_pin: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
