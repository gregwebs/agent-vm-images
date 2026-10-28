#!/usr/bin/env python3
"""Verify committed Rust toolchain pins and install them from a private dist layout.

Why this module exists: a rustup channel name selects a release but does not
identify the bytes that release serves. The reviewed closure in
``rust-toolchain/`` names the exact bootstrap binary, the original signed
distribution manifest and every selected component archive for both supported
architectures. Ordinary builds verify those committed bytes and then install
over an invocation-private ``file://`` dist layout, so rustup keeps its own
component bookkeeping while fetching only bytes this module has hashed and
signature-checked. Nothing here resolves "latest", rewrites the signed manifest
or repairs a missing pin: maintenance is the separate writer in
``script/dev/``.

Subcommands (all offline except ``install``, which fetches only committed URLs):

    check INPUT_DIR [--expect-channel EXACT]
    install INPUT_DIR --arch {amd64,arm64} --expect-channel EXACT
            --rustup-home DIR --cargo-home DIR [--events FILE]
    verify-installed INPUT_DIR --arch {amd64,arm64} --expect-channel EXACT
            --rustup-home DIR --cargo-home DIR
"""
from __future__ import annotations

import argparse
import contextlib
import enum
import hashlib
import http.client
import io
import json
import lzma
import os
import re
import shutil
import socket
import ssl
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import tomllib
import urllib.error
import urllib.parse
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath
from typing import Callable, Protocol, Sequence

if sys.version_info < (3, 11):
    # tomllib is the only TOML reader here; a second parser is not supported.
    print('rust_toolchain: Python >= 3.11 is required', file=sys.stderr)
    raise SystemExit(1)

# --- reviewed policy ---------------------------------------------------------
# The trust root is the Rust release signing key published at
# https://static.rust-lang.org/rust-key.gpg.ascii (primary fingerprint also
# published via https://keybase.io/rust). A different primary key or signer set
# is a reviewed policy change in the image input policy and the ADR, never
# something an update or a pin edit can introduce on its own.
REVIEWED_PRIMARY_FINGERPRINT = '108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE'
REVIEWED_SIGNING_FINGERPRINTS = ('108F66205EAEB0AAA8DD5E1C85AB96E6FA1BE5FE',)

PIN_FILE = 'pin.json'
MANIFEST_PATH = 'channel.toml'
MANIFEST_CHECKSUM_PATH = 'channel.toml.sha256'
MANIFEST_SIGNATURE_PATH = 'channel.toml.asc'
KEY_PATH = 'rust-release-key.asc'
EVIDENCE_DIR = 'evidence'

DIST_SERVER = 'https://static.rust-lang.org'
UPDATE_ROOT = 'https://static.rust-lang.org/rustup'

# Bounds. A measured review, not an automatic increase, is what changes these.
DOWNLOAD_DEADLINE_SECONDS = 300
SOCKET_TIMEOUT_SECONDS = 30
READ_CHUNK_BYTES = 1 << 16
MAX_BOOTSTRAP_BYTES = 32 << 20
MAX_ARCHIVE_BYTES = 256 << 20
MAX_EVIDENCE_BYTES = 2 << 20
MAX_EXPANDED_BYTES = 2 << 30
MAX_MEMBERS = 100000
MAX_METADATA_FILE_BYTES = 2 << 20
MAX_METADATA_TOTAL_BYTES = 8 << 20
# GNU `L`/`K` and PAX `x`/`g` extended headers are consumed by tarfile *before* a
# member is yielded, so the metadata-file caps above never see them. They are
# bounded separately, by length and cumulatively, before tarfile allocates them.
MAX_EXTENDED_HEADER_BYTES = 64 << 10
MAX_EXTENDED_TOTAL_BYTES = 256 << 10
# tarfile consumes these header kinds itself, before yielding any member: `L`/`K`
# are GNU long name/link, `x`/`X` are per-file PAX headers and `g` is global.
_EXTENDED_HEADER_TYPES = {b'L': 'gnu', b'K': 'gnu', b'x': 'pax', b'X': 'pax',
                          b'g': 'global'}
MAX_METADATA_LINES = 100000
MAX_LINE_BYTES = 4096

REQUIRED_RUSTUP_VERSION = '1.28.2'
# The audited installer whose format preference, installation argv and hash-only
# bootstrap exception this module implements. Widening this tuple is a code and
# policy review (docs/adr/0007-rust-example-toolchain-pins.md), never a CLI flag.
SUPPORTED_RUSTUP_VERSIONS = (REQUIRED_RUSTUP_VERSION,)
RUST_INSTALLER_VERSION = b'3\n'
IMAGES_PATH = '/usr/bin:/bin'
# Variables that would let the ambient environment choose or fetch a different
# toolchain than the pinned one. The child environment is built, never inherited.
AMBIENT_TOOLCHAIN_VARIABLES = (
    'RUSTUP_TOOLCHAIN', 'RUSTUP_VERSION', 'RUSTUP_USE_CURL', 'RUSTUP_USE_REQWEST',
    'RUSTUP_DIST_SERVER', 'RUSTUP_UPDATE_ROOT', 'RUSTUP_HOME', 'CARGO_HOME',
    'RUSTC', 'RUSTC_WRAPPER', 'RUSTC_WORKSPACE_WRAPPER', 'LD_PRELOAD',
    'LD_LIBRARY_PATH', 'DYLD_INSERT_LIBRARIES', 'RUSTFLAGS',
)

SHA256_RE = re.compile(r'[0-9a-f]{64}')
FINGERPRINT_RE = re.compile(r'[0-9A-F]{40}')
CHANNEL_RE = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)')
DATE_RE = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}')
# A target triple's first segment may carry a version dot (thumbv8m.main-none-eabi).
TRIPLE_RE = re.compile(r'[a-z0-9][a-z0-9_.]*(-[a-z0-9_.]+)+')
MEMBER_PREFIX_RE = re.compile(r'([A-Za-z0-9_.+-]+)/')
DIRECTIVE_KINDS = ('file:', 'dir:')


class ToolchainError(Exception):
    """A refusal: bad input, failed verification or a failed operation."""


def refuse(message: str) -> None:
    raise ToolchainError(message)


# --- validated domain types ---------------------------------------------------
@dataclass(frozen=True)
class Sha256:
    value: str

    def __str__(self) -> str:
        return self.value


@dataclass(frozen=True)
class IsoDate:
    value: str


@dataclass(frozen=True)
class Channel:
    value: str


@dataclass(frozen=True)
class Semver:
    value: str


@dataclass(frozen=True)
class Fingerprint:
    value: str


@dataclass(frozen=True)
class CanonicalUrl:
    value: str

    @property
    def filename(self) -> str:
        return self.value.rsplit('/', 1)[1]


@dataclass(frozen=True)
class EvidencePath:
    rel: str


@dataclass(frozen=True)
class Evidence:
    path: EvidencePath
    digest: Sha256


@dataclass(frozen=True)
class MemberPrefix:
    value: str


@dataclass(frozen=True)
class TargetTriple:
    value: str


class Architecture(enum.Enum):
    AMD64 = 'amd64'
    ARM64 = 'arm64'

    @property
    def platform(self) -> str:
        return f'linux/{self.value}'

    @property
    def host(self) -> TargetTriple:
        return TargetTriple(HOSTS[self])

    @property
    def musl(self) -> TargetTriple:
        return TargetTriple(MUSL_TARGETS[self])

    @property
    def elf_machine(self) -> int:
        return ELF_MACHINES[self]

    @property
    def bootstrap_target(self) -> str:
        return HOSTS[self]


HOSTS = {Architecture.AMD64: 'x86_64-unknown-linux-gnu',
         Architecture.ARM64: 'aarch64-unknown-linux-gnu'}
MUSL_TARGETS = {Architecture.AMD64: 'x86_64-unknown-linux-musl',
                Architecture.ARM64: 'aarch64-unknown-linux-musl'}
ELF_MACHINES = {Architecture.AMD64: 62, Architecture.ARM64: 183}
# Rust distribution package name -> archive filename stem. The signed manifest
# renames clippy/rustfmt to *-preview while the tarball keeps the short name.
PACKAGES = ('cargo', 'clippy-preview', 'rust-std', 'rustc', 'rustfmt-preview')
FILENAME_STEMS = {'cargo': 'cargo', 'clippy-preview': 'clippy', 'rust-std': 'rust-std',
                  'rustc': 'rustc', 'rustfmt-preview': 'rustfmt'}
# Executables that must exist in the component payload, per package.
COMPONENT_EXECUTABLES = {
    'cargo': ('bin/cargo',),
    'clippy-preview': ('bin/cargo-clippy', 'bin/clippy-driver'),
    'rustc': ('bin/rustc',),
    'rustfmt-preview': ('bin/cargo-fmt', 'bin/rustfmt'),
}
# Linux host selection: the minimal profile's toolchain packages (rust-mingw is
# not an available Linux extension), the explicitly installed clippy/rustfmt and
# the musl standard library the example needs.
CLI_COMPONENTS = ('clippy', 'rustfmt')
PROFILE = 'minimal'


def parse_sha256(text: object, what: str) -> Sha256:
    if type(text) is not str or not SHA256_RE.fullmatch(text):
        refuse(f'{what}: expected a lowercase SHA-256 hex digest')
    return Sha256(text)


def parse_fingerprint(text: object, what: str) -> Fingerprint:
    if type(text) is not str or not FINGERPRINT_RE.fullmatch(text):
        refuse(f'{what}: expected an uppercase 40-hex OpenPGP fingerprint')
    return Fingerprint(text)


def parse_channel(text: object, what: str) -> Channel:
    if type(text) is not str or not CHANNEL_RE.fullmatch(text):
        refuse(f'{what}: expected an exact X.Y.Z release channel')
    return Channel(text)


def parse_semver(text: object, what: str) -> Semver:
    if type(text) is not str or not CHANNEL_RE.fullmatch(text):
        refuse(f'{what}: expected an exact X.Y.Z version')
    return Semver(text)


def parse_date(text: object, what: str) -> IsoDate:
    if type(text) is not str or not DATE_RE.fullmatch(text):
        refuse(f'{what}: expected YYYY-MM-DD')
    year, month, day = (int(part) for part in text.split('-'))
    if not (1 <= month <= 12 and 1 <= day <= 31):
        refuse(f'{what}: not a calendar date: {text}')
    # A real calendar check, so 2026-02-30 cannot become an identity.
    import datetime
    try:
        datetime.date(year, month, day)
    except ValueError:
        refuse(f'{what}: not a calendar date: {text}')
    return IsoDate(text)


def parse_triple(text: object, what: str) -> TargetTriple:
    if type(text) is not str or len(text) > 64 or not TRIPLE_RE.fullmatch(text):
        refuse(f'{what}: expected a target triple')
    return TargetTriple(text)


def parse_url(text: object, what: str, expected: str | None = None) -> CanonicalUrl:
    if type(text) is not str or len(text) > 512:
        refuse(f'{what}: expected a URL string')
    split = urllib.parse.urlsplit(text)
    if (split.scheme != 'https' or split.hostname != 'static.rust-lang.org'
            or split.port is not None or split.username is not None
            or split.query or split.fragment or not split.path.startswith('/')):
        refuse(f'{what}: not a canonical rust-lang.org HTTPS URL: {text}')
    if '\\' in text or '//' in split.path or '/../' in split.path or split.path.endswith('/..'):
        refuse(f'{what}: URL path is not canonical: {text}')
    if expected is not None and text != expected:
        refuse(f'{what}: {text} != committed origin {expected}')
    return CanonicalUrl(text)


def parse_evidence_path(text: object, what: str) -> EvidencePath:
    if type(text) is not str or not text or len(text) > 200:
        refuse(f'{what}: expected a relative evidence path')
    if '\\' in text or '\x00' in text or text.startswith('/'):
        refuse(f'{what}: not a canonical relative path: {text}')
    parts = text.split('/')
    if any(part in ('', '.', '..') for part in parts):
        refuse(f'{what}: not a canonical relative path: {text}')
    return EvidencePath(text)


def parse_member_prefix(text: object, what: str) -> MemberPrefix:
    if type(text) is not str or not MEMBER_PREFIX_RE.fullmatch(text):
        refuse(f'{what}: expected a single archive member prefix like name/')
    return MemberPrefix(text)


def reject_control_characters(value: str, what: str) -> None:
    for char in value:
        code = ord(char)
        if code < 0x20 or 0x7f <= code <= 0x9f or 0xd800 <= code <= 0xdfff:
            refuse(f'{what}: control character in identity string')


# --- strict decoding ----------------------------------------------------------
class _JsonObject(dict):
    pass


def _reject_constant(name: str) -> None:
    refuse(f'pin.json: {name} is not valid JSON')


def _pairs_without_duplicates(pairs: Sequence[tuple[str, object]]) -> _JsonObject:
    result = _JsonObject()
    for key, value in pairs:
        if key in result:
            refuse(f'pin.json: duplicate key {key!r}')
        result[key] = value
    return result


def decode_json(raw: bytes, what: str = PIN_FILE) -> object:
    if raw.startswith(b'\xef\xbb\xbf'):
        refuse(f'{what}: leading UTF-8 BOM is not accepted')
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError as error:
        refuse(f'{what}: not valid UTF-8: {error}')
    try:
        return json.loads(text, object_pairs_hook=_pairs_without_duplicates,
                          parse_constant=_reject_constant)
    except json.JSONDecodeError as error:
        refuse(f'{what}: not valid JSON: {error}')


def decode_toml(raw: bytes, what: str) -> dict:
    if raw.startswith(b'\xef\xbb\xbf'):
        refuse(f'{what}: leading UTF-8 BOM is not accepted')
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError as error:
        refuse(f'{what}: not valid UTF-8: {error}')
    try:
        return tomllib.loads(text)
    except tomllib.TOMLDecodeError as error:
        refuse(f'{what}: not valid TOML: {error}')


def field_of(mapping: object, key: str, what: str) -> object:
    if type(mapping) is not _JsonObject and not isinstance(mapping, dict):
        refuse(f'{what}: expected an object')
    if key not in mapping:
        refuse(f'{what}: missing {key!r}')
    return mapping[key]


def exact_type(value: object, expected: type, what: str) -> object:
    if type(value) is not expected:
        refuse(f'{what}: expected {expected.__name__}')
    return value


def exact_int(value: object, what: str) -> int:
    if type(value) is not int:
        refuse(f'{what}: expected an integer')
    return value


def string_list(value: object, what: str) -> tuple[str, ...]:
    if type(value) is not list:
        refuse(f'{what}: expected a list')
    items = []
    for item in value:
        if type(item) is not str:
            refuse(f'{what}: expected strings')
        reject_control_characters(item, what)
        items.append(item)
    return tuple(items)


# --- pin model ----------------------------------------------------------------
@dataclass(frozen=True)
class Content:
    url: CanonicalUrl
    digest: Sha256


@dataclass(frozen=True)
class ComponentArchive:
    package: str
    target: TargetTriple
    content: Content
    member_prefix: MemberPrefix
    signature: Evidence


@dataclass(frozen=True)
class HostClosure:
    arch: Architecture
    host: TargetTriple
    musl: TargetTriple
    bootstrap: Content
    bootstrap_checksum: Evidence
    archives: tuple[ComponentArchive, ...]

    def archive(self, package: str, target: TargetTriple) -> ComponentArchive:
        for archive in self.archives:
            if archive.package == package and archive.target == target:
                return archive
        refuse(f'{self.arch.platform}: no selected {package} archive for {target.value}')
        raise AssertionError('unreachable')


@dataclass(frozen=True)
class BootstrapException:
    reviewed_on: IsoDate
    reviewer: str
    reason: str
    evidence: tuple[str, ...]
    reassess_when: str
    scope: dict[str, Content]

    def content_for(self, arch: Architecture) -> Content:
        return self.scope[arch.platform]


@dataclass(frozen=True)
class TrustPolicy:
    primary: Fingerprint
    signers: tuple[Fingerprint, ...]

    def allows(self, signer: Fingerprint) -> bool:
        return signer in self.signers


REVIEWED_TRUST_POLICY = TrustPolicy(
    primary=Fingerprint(REVIEWED_PRIMARY_FINGERPRINT),
    signers=tuple(Fingerprint(value) for value in REVIEWED_SIGNING_FINGERPRINTS))


@dataclass(frozen=True)
class RustPin:
    channel: Channel
    release_date: IsoDate
    profile: str
    rustup_version: Semver
    primary: Fingerprint
    signers: tuple[Fingerprint, ...]
    key_sha256: Sha256
    manifest: Content
    manifest_checksum: Content
    manifest_signature: Content
    closures: tuple[HostClosure, ...]
    bootstrap_exception: BootstrapException

    def closure(self, arch: Architecture) -> HostClosure:
        for closure in self.closures:
            if closure.arch is arch:
                return closure
        refuse(f'no committed closure for {arch.platform}')
        raise AssertionError('unreachable')

    def required_files(self) -> tuple[str, ...]:
        paths = [PIN_FILE, MANIFEST_PATH, MANIFEST_CHECKSUM_PATH, MANIFEST_SIGNATURE_PATH, KEY_PATH]
        for closure in self.closures:
            paths.append(closure.bootstrap_checksum.path.rel)
            for archive in closure.archives:
                paths.append(archive.signature.path.rel)
        return tuple(sorted(paths))


@dataclass(frozen=True)
class VerifiedManifest:
    pin: RustPin
    raw: bytes


@dataclass(frozen=True)
class VerifiedLayout:
    root: Path
    bootstrap: Path
    closure: HostClosure
    channel: Channel


@dataclass(frozen=True)
class InstallHomes:
    rustup: Path
    cargo: Path


# --- trust policy -------------------------------------------------------------
def require_reviewed_trust(primary: Fingerprint, signers: tuple[Fingerprint, ...],
                           policy: TrustPolicy) -> None:
    if primary != policy.primary:
        refuse(f'pin trust root {primary.value} is not the reviewed primary key '
               f'{policy.primary.value}; a new key needs a reviewed policy change')
    for signer in signers:
        if not policy.allows(signer):
            refuse(f'pin signing key {signer.value} is not in the reviewed signer set')


# --- manifest semantics -------------------------------------------------------
def package_component_name(package: str, target: TargetTriple) -> str:
    """rust-installer directory/component name selected for a manifest package."""
    if package == 'rust-std':
        return f'rust-std-{target.value}'
    return package


def archive_filename(channel: Channel, package: str, target: TargetTriple) -> str:
    return f'{FILENAME_STEMS[package]}-{channel.value}-{target.value}.tar.xz'


def expected_archive_url(channel: Channel, release_date: IsoDate, package: str,
                         target: TargetTriple) -> str:
    return f'{DIST_SERVER}/dist/{release_date.value}/{archive_filename(channel, package, target)}'


def expected_signature_path(arch: Architecture, package: str, target: TargetTriple) -> str:
    suffix = '-musl' if target.value.endswith('-musl') else ''
    return f'{EVIDENCE_DIR}/{arch.value}-{package}{suffix}.asc'


def expected_bootstrap_url(rustup_version: Semver, arch: Architecture) -> str:
    return (f'{DIST_SERVER}/rustup/archive/{rustup_version.value}/'
            f'{arch.bootstrap_target}/rustup-init')


def expected_bootstrap_checksum_path(arch: Architecture) -> str:
    return f'{EVIDENCE_DIR}/rustup-{arch.value}.sha256'


def selected_pairs(arch: Architecture) -> tuple[tuple[str, TargetTriple], ...]:
    host, musl = arch.host, arch.musl
    return (('cargo', host), ('clippy-preview', host), ('rust-std', host), ('rust-std', musl),
            ('rustc', host), ('rustfmt-preview', host))


def _pin_content(node: object, what: str, url_key: str = 'url',
                 digest_key: str = 'sha256') -> Content:
    url = parse_url(field_of(node, url_key, what), f'{what}.{url_key}')
    digest = parse_sha256(field_of(node, digest_key, what), f'{what}.{digest_key}')
    return Content(url, digest)


def _pin_evidence(node: object, what: str) -> Evidence:
    path = parse_evidence_path(field_of(node, 'path', what), f'{what}.path')
    digest = parse_sha256(field_of(node, 'sha256', what), f'{what}.sha256')
    return Evidence(path, digest)


def load_pin(directory: Path, policy: TrustPolicy = REVIEWED_TRUST_POLICY) -> RustPin:
    pin_file = directory / PIN_FILE
    if not pin_file.is_file() or pin_file.is_symlink():
        refuse(f'missing input: {PIN_FILE}')
    document = decode_json(pin_file.read_bytes())
    require_keys(document, ('schema', 'channel', 'release_date', 'profile', 'rustup_version',
                            'trust', 'manifest', 'platforms', 'bootstrap_exception'), (),
                 PIN_FILE)
    if exact_int(field_of(document, 'schema', PIN_FILE), 'schema') != 1:
        refuse('pin.json: unsupported schema')
    channel = parse_channel(field_of(document, 'channel', PIN_FILE), 'channel')
    release_date = parse_date(field_of(document, 'release_date', PIN_FILE), 'release_date')
    profile = exact_type(field_of(document, 'profile', PIN_FILE), str, 'profile')
    if profile != PROFILE:
        refuse(f'pin.json: profile must be {PROFILE}')
    rustup_version = parse_semver(field_of(document, 'rustup_version', PIN_FILE), 'rustup_version')
    if rustup_version.value not in SUPPORTED_RUSTUP_VERSIONS:
        # The format preference, installation argv and hash-only bootstrap
        # exception are properties of the audited installer, not of "a rustup".
        refuse(f'pin.json: rustup {rustup_version.value} is not an audited installer; only '
               f'{", ".join(SUPPORTED_RUSTUP_VERSIONS)} has a reviewed format/trust policy, '
               'so a new version needs code and policy review rather than the existing record')

    trust = require_keys(field_of(document, 'trust', PIN_FILE),
                         ('key', 'primary_fingerprint', 'signing_fingerprints'), (), 'trust')
    key = require_keys(field_of(trust, 'key', 'trust'), ('path', 'sha256'), (), 'trust.key')
    key_evidence = _pin_evidence(key, 'trust.key')
    key_sha = key_evidence.digest
    if key_evidence.path.rel != KEY_PATH:
        refuse(f'pin.json: key path must be {KEY_PATH}')
    primary = parse_fingerprint(field_of(trust, 'primary_fingerprint', 'trust'),
                                'trust.primary_fingerprint')
    signers = tuple(parse_fingerprint(value, 'trust.signing_fingerprints')
                    for value in string_list(field_of(trust, 'signing_fingerprints', 'trust'),
                                             'trust.signing_fingerprints'))
    if not signers:
        refuse('pin.json: trust.signing_fingerprints must not be empty')
    require_reviewed_trust(primary, signers, policy)

    manifest_node = require_keys(field_of(document, 'manifest', PIN_FILE),
                                 ('url', 'path', 'sha256', 'checksum', 'signature'), (),
                                 'manifest')
    manifest = _pin_content(manifest_node, 'manifest')
    if manifest.url.value != f'{DIST_SERVER}/dist/channel-rust-{channel.value}.toml':
        refuse('pin.json: manifest URL is not the canonical channel manifest origin')
    checksum_node = require_keys(field_of(manifest_node, 'checksum', 'manifest'),
                                 ('url', 'path', 'sha256'), (), 'manifest.checksum')
    signature_node = require_keys(field_of(manifest_node, 'signature', 'manifest'),
                                  ('url', 'path', 'sha256'), (), 'manifest.signature')
    manifest_checksum = _pin_content(checksum_node, 'manifest.checksum')
    manifest_signature = _pin_content(signature_node, 'manifest.signature')
    if manifest_checksum.url.value != manifest.url.value + '.sha256':
        refuse('pin.json: manifest checksum URL is not <manifest>.sha256')
    if manifest_signature.url.value != manifest.url.value + '.asc':
        refuse('pin.json: manifest signature URL is not <manifest>.asc')
    if field_of(manifest_node, 'path', 'manifest') != MANIFEST_PATH:
        refuse(f'pin.json: manifest path must be {MANIFEST_PATH}')
    if field_of(checksum_node, 'path', 'manifest.checksum') != MANIFEST_CHECKSUM_PATH:
        refuse(f'pin.json: manifest checksum path must be {MANIFEST_CHECKSUM_PATH}')
    if field_of(signature_node, 'path', 'manifest.signature') != MANIFEST_SIGNATURE_PATH:
        refuse(f'pin.json: manifest signature path must be {MANIFEST_SIGNATURE_PATH}')

    platforms_node = field_of(document, 'platforms', PIN_FILE)
    if sorted(platforms_node) != sorted(arch.platform for arch in Architecture):
        refuse('pin.json: platforms must be exactly linux/amd64 and linux/arm64')
    closures = []
    for arch in Architecture:
        node = require_keys(platforms_node[arch.platform],
                            ('host', 'musl', 'bootstrap', 'archives'), (),
                            f'platforms.{arch.platform}')
        what = f'platforms.{arch.platform}'
        host = parse_triple(field_of(node, 'host', what), f'{what}.host')
        musl = parse_triple(field_of(node, 'musl', what), f'{what}.musl')
        if host != arch.host or musl != arch.musl:
            refuse(f'{what}: host/musl triples do not match the architecture')
        bootstrap_node = require_keys(field_of(node, 'bootstrap', what),
                                      ('url', 'sha256', 'checksum'), (), f'{what}.bootstrap')
        bootstrap = _pin_content(bootstrap_node, f'{what}.bootstrap')
        if bootstrap.url.value != expected_bootstrap_url(rustup_version, arch):
            refuse(f'{what}: bootstrap URL is not the versioned rustup-init origin '
                   f'{expected_bootstrap_url(rustup_version, arch)}')
        bootstrap_checksum = _pin_evidence(
            require_keys(field_of(bootstrap_node, 'checksum', f'{what}.bootstrap'),
                         ('path', 'sha256'), (), f'{what}.bootstrap.checksum'),
            f'{what}.bootstrap.checksum')
        if bootstrap_checksum.path.rel != expected_bootstrap_checksum_path(arch):
            refuse(f'{what}: bootstrap checksum evidence path must be '
                   f'{expected_bootstrap_checksum_path(arch)}')
        archives_node = field_of(node, 'archives', what)
        if type(archives_node) is not list:
            refuse(f'{what}.archives: expected a list')
        archives = []
        for raw_archive in archives_node:
            archive_node = require_keys(raw_archive,
                                        ('package', 'target', 'url', 'sha256', 'member_prefix',
                                         'signature'), (), f'{what}.archives')
            package = exact_type(field_of(archive_node, 'package', f'{what}.archives'),
                                 str, f'{what}.archives.package')
            if package not in PACKAGES:
                refuse(f'{what}: unexpected selected archive {package!r}')
            target = parse_triple(field_of(archive_node, 'target', f'{what}.archives'),
                                  f'{what}.{package}.target')
            content = _pin_content(archive_node, f'{what}.{package}')
            if content.url.value != expected_archive_url(channel, release_date, package, target):
                refuse(f'{what}.{package}: archive URL is not the canonical dated origin')
            prefix = parse_member_prefix(field_of(archive_node, 'member_prefix', f'{what}.archives'),
                                         f'{what}.{package}.member_prefix')
            if prefix.value != archive_filename(channel, package, target)[: -len('.tar.xz')] + '/':
                refuse(f'{what}.{package}: member prefix does not match the archive name')
            signature = _pin_evidence(
                require_keys(field_of(archive_node, 'signature', f'{what}.archives'),
                             ('path', 'sha256'), (), f'{what}.{package}.signature'),
                f'{what}.{package}.signature')
            if signature.path.rel != expected_signature_path(arch, package, target):
                refuse(f'{what}.{package}: signature evidence path must be '
                       f'{expected_signature_path(arch, package, target)}')
            archives.append((ComponentArchive(package, target, content, prefix, signature), raw_archive))
        _require_selection(arch, archives)
        closures.append(HostClosure(
            arch=arch, host=host, musl=musl, bootstrap=bootstrap,
            bootstrap_checksum=bootstrap_checksum,
            archives=tuple(archive for archive, _ in archives)))

    exception_node = require_keys(
        field_of(document, 'bootstrap_exception', PIN_FILE),
        ('method', 'reviewed_on', 'reviewer', 'scope', 'reason', 'evidence', 'reassess_when'),
        (), 'bootstrap_exception')
    method = exact_type(field_of(exception_node, 'method', 'bootstrap_exception'), str,
                        'bootstrap_exception.method')
    if method != 'reviewed-hash-only':
        refuse('pin.json: bootstrap_signature policy must be the reviewed hash-only exception')
    scope_node = field_of(exception_node, 'scope', 'bootstrap_exception')
    if sorted(scope_node) != sorted(arch.platform for arch in Architecture):
        refuse('bootstrap_exception.scope: must cover exactly linux/amd64 and linux/arm64')
    scope = {}
    for arch in Architecture:
        entry = require_keys(scope_node[arch.platform], ('url', 'sha256'), (),
                             f'bootstrap_exception.scope.{arch.platform}')
        content = _pin_content(entry, f'bootstrap_exception.scope.{arch.platform}')
        if content != closures[list(Architecture).index(arch)].bootstrap:
            refuse(f'bootstrap_exception.scope.{arch.platform}: exception is not bound to the '
                   'committed bootstrap URL and digest')
        scope[arch.platform] = content
    reviewer = exact_type(field_of(exception_node, 'reviewer', 'bootstrap_exception'), str,
                          'bootstrap_exception.reviewer')
    reason = exact_type(field_of(exception_node, 'reason', 'bootstrap_exception'), str,
                        'bootstrap_exception.reason')
    reassess = exact_type(field_of(exception_node, 'reassess_when', 'bootstrap_exception'), str,
                          'bootstrap_exception.reassess_when')
    evidence = string_list(field_of(exception_node, 'evidence', 'bootstrap_exception'),
                            'bootstrap_exception.evidence')
    for value in (reviewer, reason, reassess):
        reject_control_characters(value, 'bootstrap_exception')
    if not re.fullmatch(r'[A-Za-z0-9_.-]{1,64}', reviewer):
        refuse('bootstrap_exception.reviewer: expected a simple reviewer handle')
    if not reason.strip() or not reassess.strip() or not evidence:
        refuse('bootstrap_exception: reason, reassess_when and evidence must be recorded')
    bootstrap_exception = BootstrapException(
        reviewed_on=parse_date(field_of(exception_node, 'reviewed_on', 'bootstrap_exception'),
                               'bootstrap_exception.reviewed_on'),
        reviewer=reviewer, reason=reason, evidence=evidence,
        reassess_when=reassess, scope=scope)

    return RustPin(
        channel=channel, release_date=release_date, profile=profile,
        rustup_version=rustup_version, primary=primary, signers=signers,
        key_sha256=key_sha, manifest=manifest, manifest_checksum=manifest_checksum,
        manifest_signature=manifest_signature, closures=tuple(closures),
        bootstrap_exception=bootstrap_exception)


def _require_selection(arch: Architecture,
                       archives: list[tuple[ComponentArchive, object]]) -> None:
    pairs = [(archive.package, archive.target.value) for archive, _ in archives]
    if len(pairs) != len(set(pairs)):
        refuse(f'{arch.platform}: duplicate selected archive')
    want = [(package, target.value) for package, target in selected_pairs(arch)]
    unknown = sorted(set(pairs) - set(want))
    if unknown:
        refuse(f'{arch.platform}: unexpected selected archive {unknown[0][0]} {unknown[0][1]}')
    missing = [pair for pair in want if pair not in pairs]
    if missing:
        refuse(f'{arch.platform}: selected closure is incomplete: missing {missing[0][0]} '
               f'{missing[0][1]}')
    order = [pair for pair in want]
    if pairs != order:
        refuse(f'{arch.platform}: selected archives must be in canonical order')


# --- signature verification ---------------------------------------------------
# gpg status keywords that mean the machine-readable verdict was not a good
# signature. EXPSIG/SIGEXPIRED are an expired *signature* (gpg also emits a
# VALIDSIG line and exits nonzero for them); FAILURE is gpg's own final refusal.
REJECTED_GPG_STATUSES = ('BADSIG', 'ERRSIG', 'NO_PUBKEY', 'REVKEYSIG', 'EXPKEYSIG',
                         'EXPSIG', 'SIGEXPIRED', 'FAILURE', 'NODATA')


def parse_validsig(status: str, what: str) -> list[Fingerprint]:
    signers = []
    for line in status.splitlines():
        if not line.startswith('[GNUPG:] '):
            continue
        fields = line.split()
        keyword = fields[1]
        if keyword in REJECTED_GPG_STATUSES:
            refuse(f'{what}: signature rejected: {keyword}')
        if keyword == 'VALIDSIG':
            if len(fields) < 3:
                refuse(f'{what}: malformed VALIDSIG status')
            signers.append(Fingerprint(fields[2].upper()))
    if not signers:
        refuse(f'{what}: no VALIDSIG status line')
    return signers


@dataclass(frozen=True)
class GpgProgram:
    executable: str

    def __post_init__(self) -> None:
        if not os.path.isabs(self.executable) or not os.access(self.executable, os.X_OK):
            refuse(f'gpg is not an executable absolute path: {self.executable}')


class SignatureVerifier:
    """Real gpg under an invocation-private keyring; there is no bypass mode."""

    def __init__(self, gpg: GpgProgram, key_file: Path, workdir: Path,
                 runner: 'Runner', policy: TrustPolicy) -> None:
        self._gpg = gpg
        self._key_file = key_file
        self._home = workdir / 'gnupg'
        self._runner = runner
        self._policy = policy
        self._imported = False

    def _environment(self) -> dict[str, str]:
        # A resolved gpg (Homebrew on a maintainer host, /usr/bin in the image)
        # keeps its own directory on a controlled PATH; nothing else is needed.
        path = os.path.dirname(self._gpg.executable) + ':' + IMAGES_PATH
        return {'PATH': path, 'GNUPGHOME': str(self._home), 'HOME': str(self._home.parent),
                'LC_ALL': 'C', 'TMPDIR': str(self._home.parent)}

    def _ensure_key(self) -> None:
        if self._imported:
            return
        self._home.mkdir(mode=0o700, parents=True)
        os.chmod(self._home, 0o700)
        require_success(self._runner.run(
            [self._gpg.executable, '--batch', '--no-options', '--import',
             str(self._key_file)], self._environment()), 'trust root: gpg --import')
        listing = require_success(self._runner.run(
            [self._gpg.executable, '--batch', '--no-options', '--with-colons',
             '--fingerprint', '--list-keys'], self._environment()),
            'trust root: gpg --list-keys').stdout
        primaries: list[str] = []
        signers: set[str] = set()
        pending: str | None = None
        for line in listing.splitlines():
            record = line.split(':')
            if record[0] in ('pub', 'sub'):
                pending = record[0]
            elif record[0] == 'fpr' and pending is not None and len(record) > 9:
                if pending == 'pub':
                    primaries.append(record[9])
                else:
                    signers.add(record[9])
                pending = None
        if primaries != [self._policy.primary.value]:
            refuse(f'trust root: key file must hold exactly the reviewed primary key '
                   f'{self._policy.primary.value}')
        signers.add(self._policy.primary.value)
        missing = [value.value for value in self._policy.signers if value.value not in signers]
        if missing:
            refuse(f'trust root: key file lacks reviewed signing key {missing[0]}')
        self._imported = True

    def verify(self, data: Path, signature: Path, what: str) -> None:
        self._ensure_key()
        # gpg's exit status is the verdict; the status lines are the machine
        # detail. Both must agree before the bytes count as authenticated, so a
        # nonzero exit can never be read as a verified signature.
        proc = require_success(self._runner.run(
            [self._gpg.executable, '--batch', '--no-options', '--status-fd', '1',
             '--verify', str(signature), str(data)], self._environment()),
            f'{what} check')
        signers = parse_validsig(proc.stdout, what)
        for signer in signers:
            if not self._policy.allows(signer):
                refuse(f'{what}: signer {signer.value} is not a reviewed signing key')


# --- transport ----------------------------------------------------------------
class Transport(Protocol):
    def fetch(self, url: CanonicalUrl, destination: Path, max_bytes: int) -> None:
        ...


class _RequestWatchdog:
    """Aborts blocked connection, header or body I/O at the whole-request deadline.

    Checking the clock between reads cannot bound a peer that stalls *inside*
    `read()`: the socket timeout only bounds inactivity, so a peer that trickles
    below it (or a response that simply does not return) outlives the budget.
    The watchdog therefore interrupts the *socket* from another thread with
    `shutdown(SHUT_RDWR)`; that wakes a blocked `recv` without touching the
    buffered reader's lock. Calling `HTTPResponse.close()` instead would block
    behind an in-progress read on that same lock, which is not a cancellation.

    A response object with no reachable socket (a test double) is closed as a
    fallback; the deadline still refuses it, and the real transport never takes
    that path.
    """

    def __init__(self, seconds: float) -> None:
        self.expired = threading.Event()
        self._lock = threading.Lock()
        self._sockets: list = []
        self._watched: list = []
        self._timer = threading.Timer(max(seconds, 0.0), self._expire)
        self._timer.daemon = True
        self._timer.start()

    def watch_socket(self, provider: Callable[[], object]) -> None:
        """Register a callable returning the socket to interrupt, if it exists yet."""
        with self._lock:
            self._sockets.append(provider)
        if self.expired.is_set():
            _interrupt_quietly(provider)

    def watch(self, resource):
        with self._lock:
            self._watched.append(resource)
        if self.expired.is_set():
            _abort_quietly(resource)
        return resource

    def _expire(self) -> None:
        self.expired.set()
        with self._lock:
            providers = list(self._sockets)
            watched = list(self._watched)
        interrupted = any(_interrupt_quietly(provider) for provider in providers)
        if not interrupted:
            for resource in watched:
                _abort_quietly(resource)

    def cancel(self) -> None:
        self._timer.cancel()


def _interrupt_quietly(provider: Callable[[], object]) -> bool:
    """Shut the provided socket down. True when there was one to interrupt."""
    try:
        sock = provider()
    except Exception:  # noqa: BLE001 - the provider is a best-effort accessor
        return False
    if sock is None:
        return False
    with contextlib.suppress(OSError, ValueError):
        sock.shutdown(socket.SHUT_RDWR)
    with contextlib.suppress(OSError, ValueError):
        sock.close()
    return True


def _abort_quietly(resource) -> None:
    with contextlib.suppress(Exception):
        resource.close()


def response_socket(response):
    """The socket behind a real `http.client` response, or None for a double."""
    try:
        return response.fp.raw._sock
    except AttributeError:
        return getattr(response, 'sock', None)


class HttpsTransport:
    def __init__(self, deadline_seconds: float = DOWNLOAD_DEADLINE_SECONDS,
                 clock: Callable[[], float] = time.monotonic) -> None:
        self._deadline_seconds = deadline_seconds
        self._clock = clock
        self._watchdog: _RequestWatchdog | None = None

    def _open(self, url: str, timeout: float):
        """Open a response whose socket the watchdog can interrupt.

        `http.client` is used directly rather than `urllib.request.urlopen` so the
        connection's socket is reachable while `getresponse()` is still blocked on
        headers; the canonical-origin and TLS policy live in `parse_url` and in
        the default verifying SSL context here. Redirects are never followed: a
        3xx is returned and refused by the caller.
        """
        parts = urllib.parse.urlsplit(url)
        if parts.scheme not in ('https', 'http') or not parts.hostname:
            refuse(f'fetch {url}: unsupported URL')
        if parts.scheme == 'https':
            connection = http.client.HTTPSConnection(
                parts.hostname, parts.port, timeout=timeout,
                context=ssl.create_default_context())
        else:
            connection = http.client.HTTPConnection(parts.hostname, parts.port,
                                                   timeout=timeout)
        if self._watchdog is not None:
            # Covers the connect/handshake phase: the provider sees whatever
            # socket exists when the deadline fires.
            self._watchdog.watch_socket(lambda: getattr(connection, 'sock', None))
        target = urllib.parse.urlunsplit(('', '', parts.path or '/', parts.query, ''))
        connection.request('GET', target, headers={'Connection': 'close',
                                                   'Accept-Encoding': 'identity'})
        return connection.getresponse()

    def fetch(self, url: CanonicalUrl, destination: Path, max_bytes: int) -> None:
        deadline = self._clock() + self._deadline_seconds
        partial = destination.with_name(destination.name + '.partial')
        with contextlib.suppress(FileNotFoundError):
            partial.unlink()
        watchdog = _RequestWatchdog(deadline - self._clock())
        self._watchdog = watchdog
        try:
            try:
                response = watchdog.watch(self._open(url.value, self._remaining(deadline, url)))
            except urllib.error.URLError as error:
                self._refuse_if_expired(watchdog, url)
                refuse(f'fetch {url.value}: {error}')
            except TimeoutError:
                self._refuse_if_expired(watchdog, url)
                refuse(f'fetch {url.value}: connection deadline exceeded')
            except (OSError, ValueError, http.client.HTTPException) as error:
                # A socket interrupted at the deadline raises from inside
                # `getresponse()` (a shutdown socket can surface as ESHUTDOWN, a
                # reset, or a closed buffered reader); anything else is a
                # connection failure.
                self._refuse_if_expired(watchdog, url)
                refuse(f'fetch {url.value}: connection failed: {error}')
            finally:
                self._watchdog = None
            watchdog.watch_socket(lambda: response_socket(response))
            try:
                self._refuse_if_expired(watchdog, url)
                status = getattr(response, 'status', 200)
                if status != 200:
                    refuse(f'fetch {url.value}: unexpected HTTP status {status}')
                declared = response.headers.get('Content-Length')
                written = 0
                with open(partial, 'xb') as handle:
                    while True:
                        cap_socket_timeout(response, self._remaining(deadline, url))
                        try:
                            chunk = response.read(READ_CHUNK_BYTES)
                        except (TimeoutError, OSError, ValueError) as error:
                            self._refuse_if_expired(watchdog, url)
                            refuse(f'fetch {url.value}: read failed or exceeded the '
                                   f'deadline: {error}')
                        self._refuse_if_expired(watchdog, url)
                        self._remaining(deadline, url)
                        if not chunk:
                            break
                        written += len(chunk)
                        if written > max_bytes:
                            refuse(f'fetch {url.value}: response exceeds {max_bytes} bytes')
                        handle.write(chunk)
                if declared is not None and declared.isdigit() and int(declared) != written:
                    refuse(f'fetch {url.value}: truncated stream ({written} of {declared} bytes)')
            finally:
                # Closing a response whose socket was shut down at the deadline
                # raises from the already-closed buffered reader; that must not
                # replace the deadline refusal it is unwinding.
                _abort_quietly(response)
            partial.rename(destination)
        finally:
            watchdog.cancel()

    @staticmethod
    def _refuse_if_expired(watchdog: _RequestWatchdog, url: CanonicalUrl) -> None:
        if watchdog.expired.is_set():
            refuse(f'fetch {url.value}: download deadline exceeded')

    def _remaining(self, deadline: float, url: CanonicalUrl) -> float:
        remaining = deadline - self._clock()
        if remaining <= 0:
            refuse(f'fetch {url.value}: download deadline exceeded')
        return min(remaining, SOCKET_TIMEOUT_SECONDS)


def cap_socket_timeout(response, remaining: float) -> None:
    """Best-effort: shrink the socket's own timeout to the remaining budget.

    Not every response wrapper exposes its socket (a test double need not), and
    the watchdog is the actual guarantee; this only keeps a blocking read from
    waiting out the full inactivity timeout when the budget is shorter.
    """
    try:
        sock = response.fp.raw._sock
    except AttributeError:
        return
    with contextlib.suppress(OSError, ValueError, AttributeError):
        sock.settimeout(min(remaining, SOCKET_TIMEOUT_SECONDS))


# --- command execution --------------------------------------------------------
class Runner(Protocol):
    def run(self, argv: Sequence[str], env: dict[str, str],
            cwd: Path | None = None) -> subprocess.CompletedProcess:
        ...


class ProcessRunner:
    def run(self, argv: Sequence[str], env: dict[str, str],
            cwd: Path | None = None) -> subprocess.CompletedProcess:
        return subprocess.run(list(argv), env=env, cwd=None if cwd is None else str(cwd),
                              capture_output=True, text=True)


def require_success(proc: subprocess.CompletedProcess, what: str) -> subprocess.CompletedProcess:
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or '').strip().splitlines()
        tail = detail[-1] if detail else 'no output'
        refuse(f'{what} failed with status {proc.returncode}: {tail}')
    return proc


# --- events -------------------------------------------------------------------
@dataclass
class Events:
    """Appends what actually happened, at the operation site."""

    path: Path | None = None
    lines: list[str] = field(default_factory=list)

    def record(self, message: str) -> None:
        self.lines.append(message)
        if self.path is not None:
            with open(self.path, 'a', encoding='utf-8') as handle:
                handle.write(message + '\n')


# --- input directory verification --------------------------------------------
def _walk_files(directory: Path) -> list[str]:
    files = []
    for entry in sorted(os.walk(directory)):
        root, dirs, names = entry
        base = Path(root)
        for name in sorted(dirs + names):
            rel = (base / name).relative_to(directory).as_posix()
            files.append(rel)
    return sorted(files)


def verify_input_directory(directory: Path, pin: RustPin) -> None:
    if not directory.is_dir() or directory.is_symlink():
        refuse(f'input directory is not a plain directory: {directory}')
    required = set(pin.required_files())
    expected_dirs = {str(PurePosixPath(path).parent) for path in required
                     if str(PurePosixPath(path).parent) != '.'}
    seen_files = set()
    seen_dirs = set()
    for rel in _walk_files(directory):
        path = directory / rel
        mode = os.lstat(path).st_mode
        if stat.S_ISLNK(mode):
            refuse(f'unreviewed input: symlink {rel}')
        if stat.S_ISDIR(mode):
            seen_dirs.add(rel)
        elif stat.S_ISREG(mode):
            seen_files.add(rel)
        else:
            refuse(f'unreviewed input: special file {rel}')
    extra = sorted(seen_files - required) + sorted(seen_dirs - expected_dirs)
    if extra:
        refuse(f'unreviewed input: {extra[0]}')
    missing = sorted(required - seen_files)
    if missing:
        refuse(f'missing input: {missing[0]}')
    for path in required:
        if not (directory / path).is_file():
            refuse(f'missing input: {path}')


def _read_evidence(directory: Path, rel: str, digest: Sha256) -> bytes:
    path = directory / rel
    raw = path.read_bytes()
    if hashlib.sha256(raw).hexdigest() != digest.value:
        refuse(f'{rel}: bytes differ from the committed digest')
    return raw


def _one_checksum_line(raw: bytes, what: str) -> str:
    if raw.startswith(b'\xef\xbb\xbf'):
        refuse(f'{what}: leading BOM')
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        refuse(f'{what}: not valid UTF-8')
    lines = text.split('\n')
    if len(lines) != 2 or lines[1] != '':
        refuse(f'{what}: expected exactly one trailing-newline line')
    return lines[0]


def verify_manifest_checksum_line(raw: bytes, pin: RustPin) -> None:
    line = _one_checksum_line(raw, MANIFEST_CHECKSUM_PATH)
    match = re.fullmatch(r'([0-9a-f]{64}) [ *](channel-rust-[0-9]+\.[0-9]+\.[0-9]+\.toml)', line)
    if not match:
        refuse(f'{MANIFEST_CHECKSUM_PATH}: unexpected checksum line format')
    if match.group(2) != pin.manifest.url.filename:
        refuse(f'{MANIFEST_CHECKSUM_PATH}: names {match.group(2)}, not the pinned manifest')
    if match.group(1) != pin.manifest.digest.value:
        refuse('manifest: committed digest does not match the committed upstream checksum')


def verify_bootstrap_checksum_line(raw: bytes, arch: Architecture, digest: Sha256) -> None:
    line = _one_checksum_line(raw, expected_bootstrap_checksum_path(arch))
    match = re.fullmatch(r'([0-9a-f]{64}) \*\./rustup-init', line)
    if not match:
        refuse(f'{expected_bootstrap_checksum_path(arch)}: unexpected checksum line format')
    if match.group(1) != digest.value:
        refuse(f'{expected_bootstrap_checksum_path(arch)}: checksum does not match the pin')


def verify_inputs(directory: Path, pin: RustPin, verifier: SignatureVerifier) -> VerifiedManifest:
    verify_input_directory(directory, pin)
    manifest_raw = _read_evidence(directory, MANIFEST_PATH, pin.manifest.digest)
    checksum_raw = _read_evidence(directory, MANIFEST_CHECKSUM_PATH, pin.manifest_checksum.digest)
    _read_evidence(directory, MANIFEST_SIGNATURE_PATH, pin.manifest_signature.digest)
    _read_evidence(directory, KEY_PATH, pin.key_sha256)
    verify_manifest_checksum_line(checksum_raw, pin)
    for closure in pin.closures:
        raw = _read_evidence(directory, closure.bootstrap_checksum.path.rel,
                             closure.bootstrap_checksum.digest)
        verify_bootstrap_checksum_line(raw, closure.arch, closure.bootstrap.digest)
        for archive in closure.archives:
            _read_evidence(directory, archive.signature.path.rel, archive.signature.digest)
    verifier.verify(directory / MANIFEST_PATH, directory / MANIFEST_SIGNATURE_PATH,
                    'manifest signature')
    verify_manifest_closure(manifest_raw, pin)
    return VerifiedManifest(pin=pin, raw=manifest_raw)


def verify_manifest_closure(raw: bytes, pin: RustPin) -> None:
    manifest = decode_toml(raw, MANIFEST_PATH)
    keys = require_keys(manifest, ('manifest-version', 'date', 'pkg', 'renames', 'profiles'),
                        ('artifacts',), 'manifest')
    del keys
    if manifest.get('manifest-version') != '2':
        refuse('manifest: unsupported manifest-version')
    date = parse_date(manifest.get('date'), 'manifest.date')
    if date != pin.release_date:
        refuse('manifest: release date does not match the pin')
    rust_version = manifest.get('pkg', {}).get('rust', {}).get('version')
    if type(rust_version) is not str or not rust_version.startswith(pin.channel.value + ' '):
        refuse('manifest: release version does not match the pinned channel')
    reviewed_renames(manifest)
    reviewed_profile(manifest)
    for arch in Architecture:
        closure = pin.closure(arch)
        derived = [(package, target.value) for package, target in derive_selection(manifest, arch)]
        committed = sorted((archive.package, archive.target.value) for archive in closure.archives)
        if derived != committed:
            refuse(f'{arch.platform}: the committed selection is not the manifest-derived '
                   'Linux minimal + clippy/rustfmt + musl closure')
        for archive in closure.archives:
            entry = selected_entry(manifest, archive.package, archive.target)
            if entry.get('available') is not True:
                refuse(f'manifest: {archive.package} {archive.target.value} is not available')
            if type(entry.get('xz_url')) is not str or type(entry.get('xz_hash')) is not str:
                refuse(f'manifest: {archive.package} {archive.target.value} lacks a full xz pair')
            if entry['xz_url'] != archive.content.url.value:
                refuse(f'manifest: xz_url for {archive.package} {archive.target.value} '
                       'differs from the pin')
            if entry['xz_hash'] != archive.content.digest.value:
                refuse(f'manifest: xz_hash for {archive.package} {archive.target.value} '
                       'differs from the pin')
            verify_alternative_format_pair(entry, archive)
        if rust_host_entry(manifest, arch).get('available') is not True:
            refuse(f'manifest: no available rust package for {closure.host.value}')


def verify_alternative_format_pair(entry: dict, archive: 'ComponentArchive') -> None:
    """A selected entry's gzip pair is all-or-nothing; a half pair is a trap.

    rustup would fall back to the format whose pair is complete, so a renamed or
    dropped half -- while the other half survives -- changes what a future rustup
    can fetch from bytes the pin never names.
    """
    what = f'manifest: {archive.package} {archive.target.value}'
    have_url, have_hash = 'url' in entry, 'hash' in entry
    if have_url != have_hash:
        refuse(f'{what} carries a half gzip format pair')
    if not have_url:
        return
    if type(entry['url']) is not str or type(entry['hash']) is not str:
        refuse(f'{what} gzip pair is not a string url and hash')
    expected = archive.content.url.value[: -len('.tar.xz')] + '.tar.gz'
    if entry['url'] != expected:
        refuse(f'{what} gzip url is not the same dated origin as its xz pair')
    if not SHA256_RE.fullmatch(entry['hash']):
        refuse(f'{what} gzip hash is not a lowercase SHA-256 digest')


def sha_of_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


# --- manifest selection semantics ---------------------------------------------
# rustup 1.28.2 derives an installation set from the *rust* package's per-host
# tables, not from package availability alone. `src/dist/manifest.rs` merges the
# host's `extensions` rows into its `components` vector (`From<Target>`) forcing
# `is_extension = true` on them, and every later lookup -- the minimal profile in
# `get_profile_components` and the explicit-component wildcard check in
# `src/dist/mod.rs` -- searches that single merged vector. There is no second,
# independent table and no `get_extension_components` function in 1.28.2. A check
# that models two tables, or that only asks "is this package available", can
# approve a selection rustup will never install (or miss one it would).
REVIEWED_PROFILE = ('rustc', 'cargo', 'rust-std', 'rust-mingw')
REVIEWED_RENAMES = (('clippy', 'clippy-preview'), ('rustfmt', 'rustfmt-preview'))
# rustup's `Component` is `{pkg, target, is_extension}`. `target` is optional and
# `"*"` deserializes to the target-independent `None`. The reviewed schema admits
# only `pkg`/`target`: `is_extension` changes none of the lookups modelled here
# (the merge forces it true on extensions, and the profile search ignores it), so
# a row carrying it -- or any unknown key, which rustup's deserializer silently
# drops -- is refused for review instead of being reinterpreted.
COMPONENT_KEYS = ('pkg', 'target')
SELECTED_ENTRY_KEYS = ('available', 'url', 'hash', 'xz_url', 'xz_hash')
RUST_HOST_ENTRY_KEYS = SELECTED_ENTRY_KEYS + ('components', 'extensions')


def require_keys(node: object, required: Sequence[str], optional: Sequence[str],
                 what: str) -> dict:
    """Exact-key validation for one pin/manifest object boundary."""
    if not isinstance(node, dict):
        refuse(f'{what}: expected an object')
    unknown = sorted(set(node) - set(required) - set(optional))
    if unknown:
        refuse(f'{what}: unknown keys: {", ".join(unknown)}')
    missing = [key for key in required if key not in node]
    if missing:
        refuse(f'{what}: missing {", ".join(missing)}')
    return node


def reviewed_renames(manifest: dict) -> None:
    renames = manifest.get('renames', {})
    for short, renamed in REVIEWED_RENAMES:
        entry = renames.get(short)
        if entry is None:
            refuse(f'manifest: missing rename for {short}; schema review required')
        require_keys(entry, ('to',), (), f'manifest.renames.{short}')
        if entry['to'] != renamed:
            refuse(f'manifest: unexpected rename for {short}; schema review required')


def reviewed_profile(manifest: dict) -> tuple[str, ...]:
    profiles = manifest.get('profiles')
    if not isinstance(profiles, dict):
        refuse('manifest: no profiles table')
    minimal = profiles.get('minimal')
    if minimal != list(REVIEWED_PROFILE):
        refuse('manifest: the minimal profile changed; schema/policy review required')
    return tuple(minimal)


def rust_host_entry(manifest: dict, arch: Architecture) -> dict:
    pkg = manifest.get('pkg', {}).get('rust')
    if not isinstance(pkg, dict):
        refuse('manifest: no rust package')
    entry = pkg.get('target', {}).get(arch.host.value)
    if not isinstance(entry, dict):
        refuse(f'manifest: no rust package entry for {arch.host.value}')
    return require_keys(entry, ('available', 'components', 'extensions'),
                        RUST_HOST_ENTRY_KEYS, f'manifest rust {arch.host.value}')


@dataclass(frozen=True)
class RustComponent:
    """One row of rustup's *merged* rust-package component table.

    `manifest.rs`'s `From<Target>` appends the `extensions` rows to `components`
    with `is_extension = true`, so one vector answers both lookups. `target is
    None` is rustup's target-independent `"*"` row, which stays target-independent
    (it is never rewritten to the host triple).
    """

    package: str
    target: TargetTriple | None
    is_extension: bool


def _component_rows(rows: object, what: str, is_extension: bool) -> list[RustComponent]:
    if type(rows) is not list:
        refuse(f'manifest: {what} is not a list')
    result = []
    for raw in rows:
        if isinstance(raw, dict) and 'extension' in raw:
            # rustup's field is `is_extension`, which its own merge forces true on
            # every extensions row and which no profile lookup filters on. The
            # invented singular flag cannot select or disable anything, so
            # honouring it here would diverge from the installer both ways.
            refuse(f'manifest {what}: `extension` is not a rustup component field; '
                   'schema/policy review required')
        require_keys(raw, ('pkg',), COMPONENT_KEYS, f'manifest {what}')
        package = exact_type(raw['pkg'], str, f'manifest {what}.pkg')
        if not package:
            refuse(f'manifest {what}.pkg: empty package name')
        target = None
        if 'target' in raw:
            value = exact_type(raw['target'], str, f'manifest {what}.target')
            target = None if value == '*' else parse_triple(value, f'manifest {what}.target')
        result.append(RustComponent(package, target, is_extension))
    return result


def rust_component_table(manifest: dict, arch: Architecture) -> tuple[RustComponent, ...]:
    """The host's components and extensions as rustup merges them.

    Order is rustup's: `components` first, then `extensions`, because the merged
    table is searched with `find` and the first match wins. An exact duplicate row
    is refused as a reviewed-schema error even though the first would win.
    """
    entry = rust_host_entry(manifest, arch)
    components = _component_rows(entry['components'], f'{arch.host.value} components', False)
    extensions = _component_rows(entry['extensions'], f'{arch.host.value} extensions', True)
    seen: set[tuple[str, TargetTriple | None]] = set()
    for row in components:
        key = (row.package, row.target)
        if key in seen:
            refuse(f'manifest: {row.package} is listed twice in the {arch.host.value} '
                   'components table')
        seen.add(key)
    return tuple(components) + tuple(extensions)


def _host_provides(manifest: dict, name: str, host: TargetTriple) -> bool:
    entry = manifest.get('pkg', {}).get(name, {}).get('target', {}).get(host.value)
    return isinstance(entry, dict) and entry.get('available') is True


def profile_components(table: tuple[RustComponent, ...], profile: Sequence[str],
                       host: TargetTriple) -> tuple[tuple[str, TargetTriple | None], ...]:
    """`get_profile_components`: first merged row per profile name, host or wildcard."""
    selected = []
    for name in profile:
        found = next((row for row in table
                      if row.package == name and (row.target is None or row.target == host)), None)
        if found is not None:
            # rustup keeps the found row's own target: a wildcard row stays
            # wildcard, a host row stays the host.
            selected.append((name, found.target))
    return tuple(selected)


def explicit_components(table: tuple[RustComponent, ...],
                        arch: Architecture) -> tuple[tuple[str, TargetTriple | None], ...]:
    """`dist/mod.rs`: rename the requested component, then honor a wildcard row."""
    renames = {short: renamed for short, renamed in REVIEWED_RENAMES}
    selected = []
    for short in CLI_COMPONENTS:
        renamed = renames[short]
        first = next((row for row in table if row.package == renamed), None)
        target: TargetTriple | None = arch.host
        if first is not None and first.target is None:
            target = None
        selected.append((renamed, target))
    return tuple(selected)


def rustup_selection(manifest: dict, arch: Architecture) -> frozenset[tuple[str, TargetTriple | None]]:
    """Exactly what 1.28.2 resolves for the pinned request, target-independent rows aside.

    Profile entries, the renamed explicit clippy/rustfmt components and the
    explicitly requested musl target are collected into one set keyed by
    `(package, target)` -- rustup's `HashSet<Component>` equality -- so a profile
    that already selected a package is not installed twice.
    """
    table = rust_component_table(manifest, arch)
    selected = set(profile_components(table, reviewed_profile(manifest), arch.host))
    selected.update(explicit_components(table, arch))
    selected.add(('rust-std', arch.musl))
    return frozenset(selected)


def require_extension(extensions: Sequence[RustComponent], package: str,
                      target: TargetTriple, arch: Architecture) -> None:
    matches = [row for row in extensions if row.package == package and row.target == target]
    if len(matches) != 1:
        refuse(f'manifest: {package} for {target.value} is not a single {arch.host.value} '
               f'extension; the committed closure needs it or a schema/policy review')


def derive_selection(manifest: dict, arch: Architecture) -> tuple[tuple[str, TargetTriple], ...]:
    """The reviewed closure derivable from this manifest before any fetch.

    `rustup_selection` reproduces 1.28.2 faithfully, including target-independent
    rows. The reviewed pin can only describe concrete archives, and it deliberately
    requires the explicit components/targets to be declared in the host's own
    extension table, so both kinds of shape are refused here with a diagnostic
    instead of being compared as if they were pinned.
    """
    selection = rustup_selection(manifest, arch)
    wildcard = sorted(package for package, target in selection if target is None)
    if wildcard:
        refuse('manifest: rustup would install the target-independent component(s) '
               f'{", ".join(wildcard)} for {arch.host.value}; a wildcard package is not '
               'representable in the reviewed pin and needs a schema/policy review')
    for name in reviewed_profile(manifest):
        if not any(package == name for package, _ in selection) and _host_provides(
                manifest, name, arch.host):
            refuse(f'manifest: {name} is available for {arch.host.value} but absent from its '
                   'components table; schema/policy review required')
    table = rust_component_table(manifest, arch)
    extensions = tuple(row for row in table if row.is_extension)
    for short in CLI_COMPONENTS:
        require_extension(extensions, dict(REVIEWED_RENAMES)[short], arch.host, arch)
    require_extension(extensions, 'rust-std', arch.musl, arch)
    return tuple(sorted(((package, target) for package, target in selection if target is not None),
                        key=lambda pair: (pair[0], pair[1].value)))


def selected_entry(manifest: dict, package: str, target: TargetTriple) -> dict:
    entry = manifest_entry(manifest, package, target)
    for key in ('zst_url', 'zst_hash'):
        if key in entry:
            refuse(f'manifest: {package} {target.value} advertises zstd; rustup 1.28.2 '
                   'prefers zstd, so the xz pin would be bypassed')
    return require_keys(entry, ('available',), SELECTED_ENTRY_KEYS,
                        f'manifest {package} {target.value}')


def manifest_entry(manifest: dict, package: str, target: TargetTriple) -> dict:
    pkg = manifest.get('pkg', {}).get(package)
    if not isinstance(pkg, dict):
        refuse(f'manifest: no package {package}')
    entry = pkg.get('target', {}).get(target.value)
    if not isinstance(entry, dict):
        refuse(f'manifest: no {package} entry for {target.value}')
    return entry


# --- payload verification -----------------------------------------------------
def elf_machine(path: Path) -> int:
    with open(path, 'rb') as handle:
        header = handle.read(20)
    if len(header) < 20 or header[:4] != b'\x7fELF':
        refuse(f'{path.name}: not an ELF binary')
    if header[4] != 2 or header[5] != 1:
        refuse(f'{path.name}: expected a 64-bit little-endian ELF binary')
    return int.from_bytes(header[18:20], 'little')


def hash_file(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, 'rb') as handle:
        for chunk in iter(lambda: handle.read(1 << 20), b''):
            digest.update(chunk)
    return digest.hexdigest()


def _canonical_member(name: str, what: str) -> None:
    if name.startswith('/') or '\\' in name or '\x00' in name or name.endswith('/'):
        refuse(f'{what}: non-canonical archive member {name!r}')
    parts = name.split('/')
    if any(part in ('', '.', '..') for part in parts):
        refuse(f'{what}: non-canonical archive member {name!r}')


@dataclass(frozen=True)
class _Metadata:
    component: str
    directives: tuple[tuple[str, str], ...]


def _read_metadata(tf: tarfile.TarFile, member: tarfile.TarInfo, total: list[int],
                   what: str) -> bytes:
    if not member.isfile() or member.size > MAX_METADATA_FILE_BYTES:
        refuse(f'{what}: {_shown(member.name)} is not a bounded regular file')
    total[0] += member.size
    if total[0] > MAX_METADATA_TOTAL_BYTES:
        refuse(f'{what}: rust-installer metadata exceeds {MAX_METADATA_TOTAL_BYTES} bytes')
    handle = tf.extractfile(member)
    if handle is None:
        refuse(f'{what}: cannot read {member.name}')
    return handle.read()


def _read_header(tf: tarfile.TarFile, member: tarfile.TarInfo, what: str) -> bytes:
    handle = tf.extractfile(member)
    if handle is None:
        refuse(f'{what}: cannot read {member.name}')
    return handle.read(20)


def parse_directives(raw: bytes, component: str, what: str) -> _Metadata:
    if raw.startswith(b'\xef\xbb\xbf'):
        refuse(f'{what}: leading BOM in {component}/manifest.in')
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        refuse(f'{what}: {component}/manifest.in is not valid UTF-8')
    if not text.endswith('\n') or '\r' in text or '\x00' in text:
        refuse(f'{what}: {component}/manifest.in must be LF-terminated lines')
    lines = text.split('\n')[:-1]
    if not lines or len(lines) > MAX_METADATA_LINES:
        refuse(f'{what}: {component}/manifest.in line count out of bounds')
    directives = []
    seen: set[tuple[str, str]] = set()
    for line in lines:
        if len(line.encode()) > MAX_LINE_BYTES:
            refuse(f'{what}: {component}/manifest.in line too long')
        reject_control_characters(line, f'{what}.{component}/manifest.in')
        kind = next((prefix for prefix in DIRECTIVE_KINDS if line.startswith(prefix)), None)
        if kind is None:
            refuse(f'{what}: unknown directive in {component}/manifest.in: {line[:32]!r}')
        rel = line[len(kind):]
        if (not rel or rel.startswith('/') or '\\' in rel or ':' in rel
                or any(part in ('', '.', '..') for part in rel.split('/'))):
            refuse(f'{what}: non-canonical directive path in {component}/manifest.in: {rel!r}')
        key = (kind, rel)
        if key in seen:
            refuse(f'{what}: duplicate directive in {component}/manifest.in: {line}')
        seen.add(key)
        directives.append((kind, rel))
    # Overlapping directives would install a path twice; refuse the ambiguity.
    files = {rel for kind, rel in directives if kind == 'file:'}
    dirs = {rel for kind, rel in directives if kind == 'dir:'}
    for rel in files:
        parents = rel.split('/')[:-1]
        for index in range(1, len(parents) + 1):
            if '/'.join(parents[:index]) in files:
                refuse(f'{what}: overlapping directives for {rel}')
    for outer in dirs:
        for inner in dirs:
            if inner != outer and inner.startswith(outer + '/'):
                refuse(f'{what}: overlapping directory directives {outer} and {inner}')
        if outer in files:
            refuse(f'{what}: {outer} is both file: and dir:')
    for rel in files:
        for outer in dirs:
            if rel.startswith(outer + '/'):
                refuse(f'{what}: file {rel} is covered twice (directory {outer})')
    return _Metadata(component, tuple(directives))


class _DecodedArchive(io.RawIOBase):
    """The decompressed archive, counted before tarfile can consume it.

    Random-access `tarfile` reads through its own file object, so a limit applied
    to yielded members never sees GNU/PAX extended headers, block padding or the
    bytes tarfile discards while seeking. Counting the decoded stream bounds all
    of it: a small compressed archive cannot expand past `MAX_EXPANDED_BYTES`
    before the parser can allocate anything.
    """

    def __init__(self, path: Path, what: str, limit: int | None = None) -> None:
        self._handle = open(path, 'rb')
        self._decoder = lzma.LZMADecompressor()
        self._what = what
        self._limit = MAX_EXPANDED_BYTES if limit is None else limit
        self.decoded = 0
        self._buffer = b''
        self._input_done = False
        self._done = False

    def readable(self) -> bool:
        return True

    def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            size = READ_CHUNK_BYTES
        while len(self._buffer) < size and not self._done:
            self._fill(size - len(self._buffer))
        chunk, self._buffer = self._buffer[:size], self._buffer[size:]
        return chunk

    def _fill(self, want: int) -> None:
        while want > 0 and not self._done:
            if self._decoder.eof:
                self._done = True
                return
            if self._decoder.needs_input:
                if self._input_done:
                    refuse(f'{self._what}: truncated xz stream')
                data = self._handle.read(READ_CHUNK_BYTES)
                if not data:
                    self._input_done = True
                    continue
                try:
                    chunk = self._decoder.decompress(data, max_length=want)
                except lzma.LZMAError as error:
                    refuse(f'{self._what}: cannot decode the xz stream: {error}')
            else:
                chunk = self._decoder.decompress(b'', max_length=want)
            self.decoded += len(chunk)
            if self.decoded > self._limit:
                refuse(f'{self._what}: decoded stream exceeds {self._limit} bytes, padding '
                       'and extended headers included')
            self._buffer += chunk
            want -= len(chunk)
            if self._decoder.eof:
                self._done = True

    def close(self) -> None:
        self._handle.close()
        super().close()


@contextlib.contextmanager
def _decoded_tar(path: Path, what: str):
    stream = _DecodedArchive(path, what)
    try:
        try:
            archive = tarfile.open(fileobj=stream, mode='r|')
        except (tarfile.TarError, OSError) as error:
            refuse(f'{what}: cannot read the xz archive: {error}')
        try:
            yield archive
        finally:
            archive.close()
    finally:
        stream.close()


def _shown(name: str) -> str:
    """A hostile member name must not flood a build log."""
    return name if len(name) <= 80 else f'{name[:77]}...'


def _require_safe_mode(member: tarfile.TarInfo, what: str) -> None:
    # Every member kind: a setgid directory or a privileged link is just as
    # outside the stated mode policy as a setuid file.
    if member.mode & 0o6000:
        refuse(f'{what}: setuid/setgid archive member {_shown(member.name)}')


def _require_directory_ancestors(members: dict[str, tarfile.TarInfo], what: str) -> None:
    """No member may sit under a file or a link.

    tar lists parents before children, but nothing in the format requires it, so
    this is checked against the complete map rather than the running one.
    """
    for name in members:
        parts = name.split('/')
        for index in range(1, len(parts)):
            ancestor = members.get('/'.join(parts[:index]))
            if ancestor is not None and not ancestor.isdir():
                refuse(f'{what}: {_shown(name)} has a non-directory ancestor {_shown(ancestor.name)}')


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
        refuse(f'{what}: unreadable tar header size field')


def _v7_directory_header(block: bytes, typeflag: bytes) -> bool:
    """Whether tarfile's V7 normalization reads this header as a directory.

    `TarInfo.frombuf` turns an `AREGTYPE` (`NUL`) header whose name ends in `/`
    into `DIRTYPE`, and the name it inspects is the 100-byte header field, before
    a GNU long name or a PAX `path` record is applied. The type is what decides
    the data layout, so this is the one header normalization the walk needs.
    """
    if typeflag != tarfile.AREGTYPE:
        return False
    return block[:100].split(b'\x00', 1)[0].endswith(b'/')


def _bound_extended_headers(stream, what: str) -> None:
    """Bound GNU/PAX extended headers before tarfile can allocate them.

    tarfile reads `L`/`K`/`x`/`g`/`X` pseudo-members itself and allocates their
    contents (a long name, a PAX record set) before yielding anything, so the
    per-file and total caps applied to `manifest.in` and friends cannot see them.
    This pass walks the same 512-byte header blocks first: each extended header
    must fit `MAX_EXTENDED_HEADER_BYTES` and all of them together must fit
    `MAX_EXTENDED_TOTAL_BYTES`. It is deliberately independent of the decoded
    expansion budget, so a metadata bomb is refused by the metadata policy rather
    than by exhausting a much larger allowance.

    It also reproduces tarfile's own offset arithmetic, because a bound that
    walks the blocks differently from the parser is not a bound: a PAX `size`
    record replaces the header's size field for the member that follows, so the
    bytes a naive walk treats as file data are the next header to the parser. The
    records are parsed here (they are already bounded) and the override is
    applied, so both walks see the same member boundaries. Sparse metadata and a
    V7 directory header that claims data are refused instead: their layouts are
    not a raw size field, and no header bound describes them.
    """
    total = 0
    global_records: dict[bytes, bytes] = {}
    # In tarfile a later extended header is applied first and an earlier one last,
    # so the earliest pending record set has the final word on `size`.
    pending: list[dict[bytes, bytes]] = []
    while True:
        block = stream.read(512)
        if len(block) < 512:
            refuse(f'{what}: truncated archive header')
        if block == b'\x00' * 512:
            return
        typeflag = block[156:157]
        size = _tar_header_size(block[124:136], what)
        if size < 0:
            refuse(f'{what}: negative tar header size')
        kind = _EXTENDED_HEADER_TYPES.get(typeflag)
        if kind is not None:
            if size > MAX_EXTENDED_HEADER_BYTES:
                refuse(f'{what}: extended header of {size} bytes exceeds '
                       f'{MAX_EXTENDED_HEADER_BYTES}')
            total += size
            if total > MAX_EXTENDED_TOTAL_BYTES:
                refuse(f'{what}: extended headers total {total} bytes, exceeding '
                       f'{MAX_EXTENDED_TOTAL_BYTES}')
            if kind == 'gnu':
                # The long name itself is never allocated: it is bounded already.
                _discard(stream, size, what)
                payload = b''
            else:
                payload = _read_bounded(stream, size, what)
            _discard(stream, -size % 512, what)
            if kind in ('pax', 'global'):
                records = _pax_records(payload, what)
                _reject_unsupported_metadata(records, what)
                if kind == 'global':
                    global_records.update(records)
                else:
                    merged = dict(global_records)
                    merged.update(records)
                    pending.append(merged)
            continue
        if typeflag == tarfile.GNUTYPE_SPARSE:
            refuse(f'{what}: GNU sparse members are outside the archive policy')
        if pending:
            override = _pax_size(pending, what)
            if override is not None:
                size = override
            pending = []
        if _v7_directory_header(block, typeflag):
            # tarfile's V7 normalization reads this header as a directory, so the
            # size field is not data to skip. Whether a *follow-up* header (one
            # after `L`/`K`/`x`/`g`) gets that normalization has changed inside
            # tarfile itself, so a data-carrying header of this shape is refused
            # rather than walked in a version-specific way: the bytes this walk
            # would call file data are the next header to one of the two readers,
            # which is exactly how an unbounded extended header hides.
            if size:
                refuse(f'{what}: V7 directory header declares {size} bytes of data')
            continue
        # tarfile skips data only for a regular member or a type it does not
        # support; for a directory, link or device header the size field is not
        # part of the layout and must not advance this walk either.
        if typeflag in tarfile.REGULAR_TYPES or typeflag not in tarfile.SUPPORTED_TYPES:
            _discard(stream, size, what)
            _discard(stream, -size % 512, what)


def _read_bounded(stream, size: int, what: str) -> bytes:
    payload = bytearray()
    while len(payload) < size:
        chunk = stream.read(min(size - len(payload), READ_CHUNK_BYTES))
        if not chunk:
            refuse(f'{what}: truncated archive member data')
        payload += chunk
    return bytes(payload)


def _discard(stream, size: int, what: str) -> None:
    remaining = size
    while remaining > 0:
        chunk = stream.read(min(remaining, READ_CHUNK_BYTES))
        if not chunk:
            refuse(f'{what}: truncated archive member data')
        remaining -= len(chunk)


def _pax_records(payload: bytes, what: str) -> dict[bytes, bytes]:
    """The record set of a PAX extended header, which is already size-bounded."""
    records: dict[bytes, bytes] = {}
    position = 0
    while position < len(payload) and payload[position] != 0:
        space = payload.find(b' ', position)
        if space < 0:
            refuse(f'{what}: unreadable PAX record at {position}')
        try:
            length = int(payload[position:space])
        except ValueError:
            refuse(f'{what}: unreadable PAX record length')
        if length < 5 or position + length > len(payload):
            refuse(f'{what}: PAX record length outside its header')
        record = payload[space + 1:position + length]
        if not record.endswith(b'\n'):
            refuse(f'{what}: PAX record is not newline-terminated')
        key, separator, value = record[:-1].partition(b'=')
        if not key or separator != b'=':
            refuse(f'{what}: unreadable PAX record')
        records[key] = value
        position += length
    return records


def _reject_unsupported_metadata(records: dict[bytes, bytes], what: str) -> None:
    for key in records:
        if key.startswith(b'GNU.sparse.'):
            refuse(f'{what}: sparse PAX metadata {_shown(key.decode("utf-8", "replace"))} '
                   'is outside the archive policy')


def _pax_size(pending: list[dict[bytes, bytes]], what: str) -> int | None:
    for records in pending:
        if b'size' not in records:
            continue
        try:
            value = int(records[b'size'])
        except ValueError:
            # tarfile's PAX number conversion falls back to zero, so the layout
            # tarfile uses is a zero-length member.
            return 0
        if value < 0:
            refuse(f'{what}: negative PAX size record')
        return value
    return None


def preflight_archive(path: Path, archive: ComponentArchive, events: Events) -> None:
    what = archive.content.url.filename
    # The extended-header bound is a pre-pass: tarfile would allocate a longname
    # or PAX record before any member-level check could run.
    stream = _DecodedArchive(path, what)
    try:
        _bound_extended_headers(stream, what)
    finally:
        stream.close()
    prefix = archive.member_prefix.value
    root = prefix[:-1]
    component = package_component_name(archive.package, archive.target)
    component_root = f'{prefix}{component}/'
    metadata_names = {prefix + 'rust-installer-version', prefix + 'components',
                      component_root + 'manifest.in'}
    executable_names = {component_root + rel
                        for rel in COMPONENT_EXECUTABLES.get(archive.package, ())}
    bodies: dict[str, bytes] = {}
    headers: dict[str, bytes] = {}
    members: dict[str, tarfile.TarInfo] = {}
    expanded = 0
    metadata_total = [0]
    with _decoded_tar(path, what) as tf:
        for index, member in enumerate(tf):
            if index >= MAX_MEMBERS:
                refuse(f'{what}: more than {MAX_MEMBERS} members')
            _canonical_member(member.name, what)
            _require_safe_mode(member, what)
            if member.name == root:
                # rust-installer archives carry a single top-level directory
                # entry named without the trailing slash; it is a member like
                # any other for duplicate and type purposes.
                if member.name in members:
                    refuse(f'{what}: duplicate archive member {_shown(member.name)}')
                if not member.isdir():
                    refuse(f'{what}: top-level entry must be a directory')
                members[member.name] = member
                continue
            if not member.name.startswith(prefix):
                refuse(f'{what}: member outside the expected prefix: {_shown(member.name)}')
            rel = member.name[len(prefix):]
            if member.name in members:
                refuse(f'{what}: duplicate archive member {_shown(member.name)}')
            if member.issym() or member.islnk():
                target = member.linkname
                if member.issym():
                    resolved = PurePosixPath(rel).parent / target
                else:
                    if not target.startswith(prefix):
                        refuse(f'{what}: hard link target outside the prefix: {target}')
                    resolved = PurePosixPath(target[len(prefix):])
                parts = str(resolved).split('/')
                if str(resolved).startswith('/') or '..' in parts or '' in parts:
                    refuse(f'{what}: archive link escapes its tree: {_shown(member.name)}')
                member_target = prefix + str(resolved)
                if member_target not in members and member_target != member.name:
                    refuse(f'{what}: link target not an earlier regular member: {_shown(member.name)}')
                existing = members.get(member_target)
                if existing is None or not existing.isfile():
                    refuse(f'{what}: link target is not a regular file: {_shown(member.name)}')
            elif member.isdir():
                pass
            elif member.isfile():
                expanded += member.size
                if expanded > MAX_EXPANDED_BYTES:
                    refuse(f'{what}: expanded size exceeds {MAX_EXPANDED_BYTES} bytes')
            else:
                refuse(f'{what}: special archive member {_shown(member.name)}')
            members[member.name] = member
            if member.name in metadata_names:
                bodies[member.name] = _read_metadata(tf, member, metadata_total, what)
            elif member.name in executable_names:
                headers[member.name] = _read_header(tf, member, what)
    _require_directory_ancestors(members, what)

    version = bodies.get(prefix + 'rust-installer-version')
    if version is None:
        refuse(f'{what}: missing rust-installer-version')
    if version != RUST_INSTALLER_VERSION:
        refuse(f'{what}: rust-installer-version must be 3')
    named = bodies.get(prefix + 'components')
    if named is None:
        refuse(f'{what}: missing components')
    if named != (component + '\n').encode():
        refuse(f'{what}: component list is {named!r}, expected exactly {component!r}')
    manifest_raw = bodies.get(component_root + 'manifest.in')
    if manifest_raw is None:
        refuse(f'{what}: missing {component}/manifest.in')
    metadata = parse_directives(manifest_raw, component, what)
    _verify_metadata_membership(members, prefix, component, metadata, what)
    _verify_payload_identity(members, headers, component_root, archive, what)
    events.record(f'preflight-ok {what}')


def _verify_metadata_membership(members: dict[str, tarfile.TarInfo], prefix: str,
                                component: str, metadata: _Metadata, what: str) -> None:
    root = f'{prefix}{component}/'
    files = {name for name in members
             if name.startswith(root) and name != root + 'manifest.in'
             and not members[name].isdir()}
    covered: set[str] = set()
    for kind, rel in metadata.directives:
        name = root + rel
        member = members.get(name)
        if member is None:
            refuse(f'{what}: directive names a missing payload path: {rel}')
        if kind == 'file:':
            if member.isdir():
                refuse(f'{what}: file directive names a directory: {rel}')
            covered.add(name)
            continue
        if not member.isdir():
            refuse(f'{what}: dir directive names a non-directory: {rel}')
        covered |= {other for other in files if other.startswith(name + '/')}
    uncovered = sorted(files - covered)
    if uncovered:
        refuse(f'{what}: payload path not covered by manifest.in: {uncovered[0]}')
    # Leaf coverage is the invariant: rustup installs from manifest.in, so an
    # unreferenced directory (upstream rust-std carries an empty
    # lib/self-contained) is never installed and needs no directive.


def _verify_payload_identity(members: dict[str, tarfile.TarInfo], headers: dict[str, bytes],
                             component_root: str, archive: ComponentArchive,
                             what: str) -> None:
    if archive.package == 'rust-std':
        std_root = component_root + f'lib/rustlib/{archive.target.value}/lib/'
        if not any(name.startswith(std_root) and name.endswith('.rlib') for name in members):
            refuse(f'{what}: no {archive.target.value} standard library in the archive')
        return
    expected = Architecture.AMD64 if archive.target.value.startswith('x86_64') \
        else Architecture.ARM64
    for rel in COMPONENT_EXECUTABLES[archive.package]:
        header = headers.get(component_root + rel)
        if header is None:
            refuse(f'{what}: missing executable {rel}')
        if len(header) < 20 or header[:4] != b'\x7fELF':
            refuse(f'{what}: {rel} is not an ELF binary')
        if header[4] != 2 or header[5] != 1:
            refuse(f'{what}: {rel} is not a 64-bit little-endian ELF binary')
        machine = int.from_bytes(header[18:20], 'little')
        if machine != expected.elf_machine:
            refuse(f'{what}: {rel} targets ELF machine {machine}, '
                   f'expected {expected.elf_machine}')



# --- layout and install -------------------------------------------------------
def prepare(directory: Path, pin: RustPin, arch: Architecture, work: Path,
            transport: Transport, verifier: SignatureVerifier,
            events: Events) -> VerifiedLayout:
    verified = verify_inputs(directory, pin, verifier)
    closure = pin.closure(arch)
    work.mkdir(mode=0o700, parents=True, exist_ok=True)
    downloads = work / 'downloads'
    downloads.mkdir(mode=0o700, exist_ok=True)
    dist = work / 'dist-root' / 'dist'
    dated = dist / pin.release_date.value
    dated.mkdir(mode=0o700, parents=True, exist_ok=True)
    bootstrap_dir = work / 'bootstrap'
    bootstrap_dir.mkdir(mode=0o700, exist_ok=True)

    bootstrap = bootstrap_dir / 'rustup-init'
    events.record(f'fetch {closure.bootstrap.url.value}')
    transport.fetch(closure.bootstrap.url, bootstrap, MAX_BOOTSTRAP_BYTES)
    if hash_file(bootstrap) != closure.bootstrap.digest.value:
        # The URL is the only actionable part of a withdrawn-pin failure.
        refuse(f'bootstrap: downloaded bytes at {closure.bootstrap.url.value} '
               'do not match the committed digest')
    events.record(f'hash-ok {closure.bootstrap.url.filename}')
    if elf_machine(bootstrap) != arch.elf_machine:
        refuse(f'bootstrap: ELF machine is not {arch.bootstrap_target}')
    bootstrap.chmod(0o755)

    for archive in closure.archives:
        name = archive.content.url.filename
        target_path = downloads / name
        events.record(f'fetch {archive.content.url.value}')
        transport.fetch(archive.content.url, target_path, MAX_ARCHIVE_BYTES)
        if hash_file(target_path) != archive.content.digest.value:
            refuse(f'{name}: downloaded bytes at {archive.content.url.value} '
                   'do not match the signed manifest hash')
        events.record(f'hash-ok {name}')
        verifier.verify(target_path, directory / archive.signature.path.rel,
                        f'{name} signature')
        events.record(f'signature-ok {name}')
        preflight_archive(target_path, archive, events)
        shutil.copyfile(target_path, dated / name)

    # The layout uses the upstream file names: rustup requests
    # <dist-server>/dist/channel-rust-<channel>.toml and its .sha256/.asc.
    upstream = pin.manifest.url.filename
    for name, source in ((upstream, directory / MANIFEST_PATH),
                         (upstream + '.sha256', directory / MANIFEST_CHECKSUM_PATH),
                         (upstream + '.asc', directory / MANIFEST_SIGNATURE_PATH)):
        shutil.copyfile(source, dist / name)
    if (dist / upstream).read_bytes() != verified.raw:
        refuse('layout: manifest bytes were not copied verbatim')
    (work / 'no-updates').mkdir(mode=0o700, exist_ok=True)
    events.record(f'layout-ready {arch.value}')
    return VerifiedLayout(root=work / 'dist-root', bootstrap=bootstrap, closure=closure,
                          channel=pin.channel)


def install_environment(layout: VerifiedLayout, homes: InstallHomes, scratch: Path) -> dict[str, str]:
    env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL', 'TZ')
           if key in os.environ}
    for key in AMBIENT_TOOLCHAIN_VARIABLES:
        env.pop(key, None)
    env.setdefault('PATH', IMAGES_PATH)
    env.update({
        'HOME': str(scratch / 'home'),
        'XDG_CACHE_HOME': str(scratch / 'home' / '.cache'),
        'XDG_DATA_HOME': str(scratch / 'home' / '.local' / 'share'),
        'XDG_CONFIG_HOME': str(scratch / 'home' / '.config'),
        'TMPDIR': str(scratch / 'tmp'),
        'RUSTUP_HOME': str(homes.rustup),
        'CARGO_HOME': str(homes.cargo),
        'RUSTUP_DIST_SERVER': layout.root.resolve().as_uri(),
        'RUSTUP_UPDATE_ROOT': (layout.root.parent / 'no-updates').resolve().as_uri(),
        'RUSTUP_AUTO_INSTALL': '0',
        'PYTHONDONTWRITEBYTECODE': '1',
    })
    for directory in ('home', 'tmp', 'home/.cache', 'home/.local/share', 'home/.config'):
        (scratch / directory).mkdir(mode=0o700, parents=True, exist_ok=True)
    return env


def require_absent_homes(homes: InstallHomes) -> None:
    """Both install homes must not exist before anything is fetched or run.

    A dangling symlink counts as present (``lexists``): refusing it here keeps a
    poisoned empty directory from ever becoming a retry's starting state. Every
    existing ancestor must be a real directory too, so a link cannot redirect the
    installation -- and the later failure cleanup -- outside the named tree.
    """
    for home in (homes.rustup, homes.cargo):
        if not home.is_absolute():
            refuse(f'refusing a relative install home: {home}')
        if os.path.lexists(home):
            refuse(f'refusing to install over an existing path: {home}')
        require_real_ancestors(home)


def require_real_ancestors(path: Path) -> None:
    for ancestor in path.parents:
        if not os.path.lexists(ancestor):
            continue
        mode = os.lstat(ancestor).st_mode
        if stat.S_ISLNK(mode):
            refuse(f'refusing a symlinked install-home ancestor: {ancestor}')
        if not stat.S_ISDIR(mode):
            refuse(f'refusing a non-directory install-home ancestor: {ancestor}')


def _remove_created_homes(created: list[Path]) -> None:
    """Best-effort removal that never replaces the failure being handled.

    A leftover home is only reported: raising here would hide the original
    refusal, and an unmovable path is exactly the state the next run refuses.
    """
    for home in reversed(created):
        try:
            shutil.rmtree(home)
        except OSError as error:
            print(f'rust_toolchain: could not remove the invocation-created {home}: {error}',
                  file=sys.stderr)


def install(layout: VerifiedLayout, homes: InstallHomes, arch: Architecture, pin: RustPin,
            runner: Runner, events: Events, scratch_parent: Path | None = None) -> None:
    require_absent_homes(homes)
    created_homes: list[Path] = []
    installed = False
    scratch = Path(tempfile.mkdtemp(prefix='rust-install-',
                                    dir=None if scratch_parent is None else str(scratch_parent)))
    try:
        os.chmod(scratch, 0o700)
        env = install_environment(layout, homes, scratch)
        rustup = homes.cargo / 'bin' / 'rustup'
        for home in (homes.rustup, homes.cargo):
            home.mkdir(mode=0o755)
            created_homes.append(home)
        events.record('execute bootstrap')
        require_success(runner.run([str(layout.bootstrap), '-y', '--no-modify-path',
                                    '--default-host', arch.host.value,
                                    '--default-toolchain', 'none', '--profile', PROFILE],
                                   env, layout.root.parent), 'rustup-init')
        for setting, value in (('auto-self-update', 'disable'), ('auto-install', 'disable')):
            events.record(f'execute rustup set {setting}')
            require_success(runner.run([str(rustup), 'set', setting, value], env),
                            f'rustup set {setting}')
        events.record('execute rustup toolchain install')
        require_success(runner.run(
            [str(rustup), 'toolchain', 'install', pin.channel.value, '--profile', PROFILE,
             '--component', ','.join(CLI_COMPONENTS), '--target', arch.musl.value,
             '--no-self-update'], env, layout.root.parent), 'rustup toolchain install')
        events.record('execute rustup default')
        require_success(runner.run([str(rustup), 'default', pin.channel.value], env),
                        'rustup default')
        check_installed(pin, arch, homes, runner)
        events.record(f'install-ok {arch.value}')
        installed = True
    finally:
        # Every unsuccessful exit -- a refusal, an OSError from execve, a home
        # that could not be created -- must leave the homes absent so a retry
        # starts clean. Only homes this invocation created are ever removed.
        if not installed:
            _remove_created_homes(created_homes)
        shutil.rmtree(scratch, ignore_errors=True)


# --- installed checks ---------------------------------------------------------
SETTINGS_KEYS = {'profile': PROFILE, 'auto_self_update': 'disable', 'auto_install': 'disable'}


def check_installed(pin: RustPin, arch: Architecture, homes: InstallHomes, runner: Runner,
                    events: Events | None = None) -> None:
    closure = pin.closure(arch)
    env = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL', 'TZ') if key in os.environ}
    env.setdefault('PATH', IMAGES_PATH)
    env.update({'RUSTUP_HOME': str(homes.rustup), 'CARGO_HOME': str(homes.cargo),
                'RUSTUP_AUTO_INSTALL': '0'})
    for key in AMBIENT_TOOLCHAIN_VARIABLES:
        if key not in ('RUSTUP_HOME', 'CARGO_HOME'):
            env.pop(key, None)
    rustup = homes.cargo / 'bin' / 'rustup'
    settings = homes.rustup / 'settings.toml'
    if not settings.is_file():
        refuse(f'installed: missing rustup settings at {settings}')
    values = decode_toml(settings.read_bytes(), 'rustup settings.toml')
    for key, expected in SETTINGS_KEYS.items():
        if values.get(key) != expected:
            refuse(f'installed: rustup setting {key} is {values.get(key)!r}, expected {expected!r}')
    if values.get('default_toolchain') != f'{pin.channel.value}-{closure.host.value}':
        refuse('installed: rustup default toolchain is not the pinned exact channel')
    for key in ('RUSTUP_TOOLCHAIN',):
        if key in values:
            refuse(f'installed: rustup settings override {key}')

    version = require_success(runner.run([str(rustup), '--version'], env),
                              'rustup --version').stdout
    if pin.rustup_version.value not in version:
        refuse(f'installed: rustup is not {pin.rustup_version.value}: {version.strip()}')
    toolchains = require_success(runner.run([str(rustup), 'toolchain', 'list'], env),
                                 'rustup toolchain list').stdout.strip().splitlines()
    expected_toolchain = f'{pin.channel.value}-{closure.host.value} (active, default)'
    if toolchains != [expected_toolchain]:
        refuse(f'installed: unexpected toolchains {toolchains!r}')
    components = sorted(
        require_success(runner.run([str(rustup), 'component', 'list', '--installed'], env),
                        'rustup component list').stdout.split())
    expected_components = sorted(f'{FILENAME_STEMS[package]}-{target.value}'
                                 for package, target in selected_pairs(arch))
    if components != expected_components:
        refuse(f'installed: components {components!r} != {expected_components!r}')
    targets = sorted(require_success(
        runner.run([str(rustup), 'target', 'list', '--installed'], env),
        'rustup target list').stdout.split())
    if targets != sorted([closure.host.value, closure.musl.value]):
        refuse(f'installed: targets {targets!r}')

    toolchain_dir = homes.rustup / 'toolchains' / f'{pin.channel.value}-{closure.host.value}'
    for target in (closure.host, closure.musl):
        std = toolchain_dir / 'lib' / 'rustlib' / target.value / 'lib'
        if not std.is_dir() or not any(std.glob('*.rlib')):
            refuse(f'installed: missing {target.value} standard library in {std}')

    verbose = require_success(runner.run([str(homes.cargo / 'bin' / 'rustc'), '-vV'], env),
                             'rustc -vV').stdout.splitlines()
    fields = dict(line.split(': ', 1) for line in verbose if ': ' in line)
    if fields.get('release') != pin.channel.value:
        refuse(f'installed: rustc release {fields.get("release")!r} != {pin.channel.value}')
    if fields.get('host') != closure.host.value:
        refuse(f'installed: rustc host {fields.get("host")!r} != {closure.host.value}')
    for tool in ([str(homes.cargo / 'bin' / 'cargo'), '--version'],
                 [str(homes.cargo / 'bin' / 'cargo'), 'clippy', '--version'],
                 [str(homes.cargo / 'bin' / 'rustfmt'), '--version']):
        require_success(runner.run(tool, env), ' '.join(tool[1:]))
    if events is not None:
        events.record(f'check-installed-ok {arch.value}')


# --- CLI ----------------------------------------------------------------------
@dataclass(frozen=True)
class Dependencies:
    transport: Transport
    runner: Runner
    gpg: GpgProgram
    trust: TrustPolicy = REVIEWED_TRUST_POLICY
    # None means "read the environment at the call site". Tests that run the real
    # stager in a fixture child set False because they are not CI work; the CI
    # refusal control sets True. Production always resolves the environment once.
    github_actions: bool | None = None

    @staticmethod
    def production() -> 'Dependencies':
        gpg = shutil.which('gpg') or '/usr/bin/gpg'
        return Dependencies(transport=HttpsTransport(), runner=ProcessRunner(),
                            gpg=GpgProgram(gpg), trust=REVIEWED_TRUST_POLICY,
                            github_actions=os.environ.get('GITHUB_ACTIONS') == 'true')


def in_github_actions(dependencies: Dependencies) -> bool:
    if dependencies.github_actions is not None:
        return dependencies.github_actions
    return os.environ.get('GITHUB_ACTIONS') == 'true'


def _with_verifier(arguments: argparse.Namespace, dependencies: Dependencies):
    work = Path(tempfile.mkdtemp(prefix='rust-verify-'))
    os.chmod(work, 0o700)
    verifier = SignatureVerifier(dependencies.gpg, Path(arguments.input_dir) / KEY_PATH, work,
                                 dependencies.runner, dependencies.trust)
    return work, verifier


def command_check(arguments: argparse.Namespace, dependencies: Dependencies) -> str:
    directory = Path(arguments.input_dir)
    work, verifier = _with_verifier(arguments, dependencies)
    try:
        pin = load_pin(directory, dependencies.trust)
        if arguments.expect_channel is not None and pin.channel.value != arguments.expect_channel:
            refuse(f'pin channel {pin.channel.value} != asserted channel {arguments.expect_channel}')
        verify_inputs(directory, pin, verifier)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    return pin.channel.value


def command_install(arguments: argparse.Namespace, dependencies: Dependencies) -> str:
    directory = Path(arguments.input_dir)
    if arguments.work_dir is not None and not Path(arguments.work_dir).is_dir():
        refuse(f'--work-dir must be an existing directory: {arguments.work_dir}')
    homes = InstallHomes(rustup=Path(arguments.rustup_home), cargo=Path(arguments.cargo_home))
    # Preconditions before any fetch: a build must not download the closure and
    # only then discover that it would install over an existing toolchain.
    require_absent_homes(homes)
    events = Events(Path(arguments.events) if arguments.events else None)
    work, verifier = _with_verifier(arguments, dependencies)
    scratch: Path | None = None
    try:
        pin = load_pin(directory, dependencies.trust)
        if pin.channel.value != arguments.expect_channel:
            refuse(f'pin channel {pin.channel.value} != asserted channel {arguments.expect_channel}')
        arch = Architecture(arguments.arch)
        scratch = Path(tempfile.mkdtemp(prefix='rust-prepare-', dir=arguments.work_dir))
        layout = prepare(directory, pin, arch, scratch, dependencies.transport, verifier, events)
        install(layout, homes, arch, pin, dependencies.runner, events)
    finally:
        # Downloads, the private dist layout and the bootstrap are scratch: they
        # must not become image or host state after success or failure.
        if scratch is not None:
            shutil.rmtree(scratch, ignore_errors=True)
        shutil.rmtree(work, ignore_errors=True)
    return pin.channel.value


def command_verify_installed(arguments: argparse.Namespace, dependencies: Dependencies) -> str:
    directory = Path(arguments.input_dir)
    events = Events()
    pin = load_pin(directory, dependencies.trust)
    if pin.channel.value != arguments.expect_channel:
        refuse(f'pin channel {pin.channel.value} != asserted channel {arguments.expect_channel}')
    arch = Architecture(arguments.arch)
    work, verifier = _with_verifier(arguments, dependencies)
    try:
        verify_inputs(directory, pin, verifier)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    homes = InstallHomes(rustup=Path(arguments.rustup_home), cargo=Path(arguments.cargo_home))
    check_installed(pin, arch, homes, dependencies.runner, events)
    return pin.channel.value


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    subparsers = parser.add_subparsers(dest='command', required=True)
    check = subparsers.add_parser('check', help='verify the committed pin data offline')
    check.add_argument('input_dir')
    check.add_argument('--expect-channel')
    for name in ('install', 'verify-installed'):
        command = subparsers.add_parser(name)
        command.add_argument('input_dir')
        command.add_argument('--arch', required=True, choices=[a.value for a in Architecture])
        command.add_argument('--expect-channel', required=True)
        command.add_argument('--rustup-home', required=True)
        command.add_argument('--cargo-home', required=True)
    install_parser = subparsers.choices['install']
    install_parser.add_argument('--events')
    install_parser.add_argument('--work-dir')
    return parser


def main(argv: Sequence[str] | None = None,
         dependencies: Dependencies | None = None) -> int:
    parser = build_parser()
    arguments = parser.parse_args(argv)
    dependencies = dependencies or Dependencies.production()
    handlers = {'check': command_check, 'install': command_install,
                'verify-installed': command_verify_installed}
    try:
        channel = handlers[arguments.command](arguments, dependencies)
    except ToolchainError as error:
        print(f'rust_toolchain: {error}', file=sys.stderr)
        return 1
    print(channel)
    return 0


if __name__ == '__main__':
    sys.exit(main())
