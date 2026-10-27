#!/usr/bin/env python3
"""Build the go-dev example's gopls from a reviewed, committed module closure.

Why a committed closure instead of ``go install golang.org/x/tools/gopls@V``:
that command resolves gopls's module graph *during the build* and accepts
whatever checksums the checksum database returns at that moment. No repository
review ever sees those sums, and the ambient environment (``GOFLAGS``,
``GOPROXY`` with its implicit ``,direct`` fallback, ``GONOSUMDB``, a ``go env -w``
file) can change what is resolved. This module makes the build accept only the
bytes a reviewer selected.

The reviewed selection lives in ``gopls-closure/`` beside this file:

* ``go.mod`` — a wrapper module whose single ``tool`` is gopls and whose
  requirements name every module of the build closure at an exact version.
* ``go.sum`` — the complete ``go mod tidy`` output, so every zip and every
  ``go.mod`` the build can touch already has a reviewed content hash.
* ``gopls.go.mod`` — the upstream root ``go.mod`` bytes the proxy served,
  bound to the committed ``go.sum`` ``/go.mod`` hash.
* ``THIRD_PARTY_LICENSES`` — the reviewed notice files, length-framed.
* ``pin.json`` — the selection, per-file digests, provenance, reviewed
  authentication determination and the review record.

``check`` proves that data offline. ``install`` then runs exactly one
``go build -mod=readonly`` inside a constructed environment and proves the
result from the binary's own build information. Nothing here regenerates the
graph; ``script/dev/upgrade-gopls.sh`` is the only writer.

The authentication story is a reviewed *determination of the module channel*,
not a claim that source signatures do not exist: sum.golang.org under the
explicit verifier key below plus the committed ``go.sum`` authenticate module
content, and available VCS release signatures are inventoried in ``pin.json``
but are not used or verified for module content here. A per-module VCS signer
policy remains in https://github.com/gregwebs/agent-vm-images/issues/115.
"""
from __future__ import annotations

import argparse
import base64
import datetime
import enum
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import urllib.parse
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping, Sequence

# --- reviewed constants -------------------------------------------------------
# The full sum.golang.org verifier key, so the key identity is reviewed rather
# than inherited from the compiler or the host environment.
SUMDB = 'sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8'
# One origin, no `,direct`: a withdrawn module fails the build instead of silently
# switching to a VCS fetch.
PROXY = 'https://proxy.golang.org'
ROOT = 'golang.org/x/tools/gopls'
WRAPPER = 'agent-vm.invalid/go-dev/gopls-closure'
DATA_FILES = ('go.mod', 'go.sum', 'gopls.go.mod', 'pin.json', 'THIRD_PARTY_LICENSES')
PIN_FILE = 'pin.json'
LICENSE_FILE = 'THIRD_PARTY_LICENSES'
ROOT_GO_MOD_FILE = 'gopls.go.mod'
GO_MOD_FILE = 'go.mod'
GO_SUM_FILE = 'go.sum'

DETERMINATION = 'module-channel-sumdb-v1'
VCS_SIGNATURE_POLICY = 'inventoried-not-used-for-module-content'
NOTICE_POLICY = 'reviewed-root-and-nested-evidence-v1'
# The policies this code can verify, including superseded ones: a committed
# record stays readable while maintenance renews it to the current value. Adding a
# value here is a code review, not a maintenance-run decision.
SUPPORTED_DETERMINATIONS = (DETERMINATION,)
SUPPORTED_VCS_SIGNATURE_POLICIES = (VCS_SIGNATURE_POLICY,)
SUPPORTED_NOTICE_POLICIES = (NOTICE_POLICY,)
AUTH_FOLLOW_UP_URL = 'https://github.com/gregwebs/agent-vm-images/issues/115'
AMENDMENT_URL = ('https://github.com/gregwebs/agent-vm-images/issues/103'
                 '#issuecomment-6422547991')
DETERMINATION_REASON = (
    'For this selected closure, the reviewed authentication channel for Go module '
    'content is sum.golang.org under the explicit verifier key plus the committed '
    'go.sum. Maintenance verifies newly proposed sums through that database; '
    'ordinary builds accept only committed sums. The log establishes consistency, '
    'not publisher authorization. VCS release-tag or release-commit signatures are '
    'available for some selected modules (see the scoped inventory), but are not the '
    "module system's content-authentication channel and are not used or verified "
    'here. A per-module VCS signature policy, trusted identities and binding to '
    "module zip content remain in the linked #94 follow-up under #103's owner "
    'amendment. Pinning does not establish that the release is safe. License files '
    'are preserved as evidence, not a legal audit.')
DETERMINATION_REASSESS = (
    'Renew on a change of root, go.mod, go.sum, selected notice layout, signing '
    'inventory, rationale/policy or checksum-database verifier key; review '
    'supported gopls signing/attestation changes and the linked follow-up '
    'explicitly. Never automatically extend this determination to new selected '
    'source-signature evidence.')

# Availability inventory of VCS release objects for three selected modules. It
# records that the objects exist and what they sign; it does not verify them and
# makes no absence claim about the modules it omits.
KNOWN_VCS_SIGNATURES = (
    {'path': 'github.com/modelcontextprotocol/go-sdk', 'version': 'v1.6.0',
     'repository': 'https://github.com/modelcontextprotocol/go-sdk',
     'ref': 'refs/tags/v1.6.0',
     'tag_object': '2c142340a561f6699d4d94d7774e1788c0381d3f',
     'commit': 'f5f2015413f17f578fbacc839d2c05d0fdb3f1cc',
     'signed_object': 'tag', 'format': 'ssh', 'verified': False},
    {'path': 'mvdan.cc/gofumpt', 'version': 'v0.10.0',
     'repository': 'https://github.com/mvdan/gofumpt',
     'ref': 'refs/tags/v0.10.0',
     'tag_object': 'dfdc8f312f71cb1ae35fd355e2862b4ac3132e9e',
     'commit': '14050252a6d591bb7a3f0167c925c3d6e6f9bdf8',
     'signed_object': 'tag', 'format': 'ssh', 'verified': False},
    {'path': 'github.com/fsnotify/fsnotify', 'version': 'v1.9.0',
     'repository': 'https://github.com/fsnotify/fsnotify',
     'ref': 'refs/tags/v1.9.0',
     'tag_object': '09cce6e5c9ddf78d3dc5bbf3d823ab4d5a2e2c40',
     'commit': 'ae0e7923765f64fb8061396db7edebb558cf6093',
     'signed_object': 'commit', 'format': 'ssh', 'verified': False},
)
SIGNATURE_EVIDENCE_URLS = (
    'https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.6.0',
    'https://github.com/mvdan/gofumpt/releases/tag/v0.10.0',
    'https://github.com/fsnotify/fsnotify/releases/tag/v1.9.0',
)

# Notice files the adoption review inspected beyond each module's root file. A
# nonempty nested set that is not listed here is refused: the layout needs
# interpretation, and silently blessing an unfamiliar notice is how a copied
# third-party file loses its evidence. The regex below is an evidence transport,
# never a claim of legal completeness.
NOTICE_NAME_RE = re.compile(r'(LICEN[CS]E|COPYING|NOTICE|PATENTS)([.-].*)?$', re.IGNORECASE)
REVIEWED_NESTED_NOTICES = {
    ('github.com/BurntSushi/toml', 'v1.6.0'): (
        'cmd/toml-test-decoder/COPYING', 'cmd/toml-test-encoder/COPYING',
        'cmd/tomlv/COPYING'),
    ('github.com/segmentio/encoding', 'v0.5.4'): ('json/fuzz/LICENSE',),
    ('honnef.co/go/tools', 'v0.8.0-rc.1'): ('go/gcsizes/LICENSE', 'go/ir/LICENSE'),
}

LICENSE_HEADER = (
    '# Third-party license files for {root} {version} and its build closure.\n'
    '# Generated by script/dev/upgrade-gopls.sh; verified against the module cache '
    'at build time.\n')

# Every variable the child environment may contain. The list is an interface: a
# test asserts the exact key set, and a key that is not here is not inherited.
CHILD_ENV_KEYS = (
    'PATH', 'HOME', 'GOPATH', 'GOMODCACHE', 'GOCACHE', 'TMPDIR', 'GOENV', 'GOWORK',
    'GOFLAGS', 'GOTOOLCHAIN', 'GOPROXY', 'GOSUMDB', 'GONOSUMDB', 'GONOPROXY',
    'GOPRIVATE', 'GOINSECURE', 'GOAUTH', 'GOVCS', 'CGO_ENABLED', 'GOOS', 'GOARCH')
# Transport is not content: the bytes a build downloads are still checked against
# the committed hashes, so a proxy or a build CA may be configured.
TRANSPORT_KEYS = ('HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY', 'http_proxy', 'https_proxy',
                  'no_proxy', 'SSL_CERT_FILE', 'SSL_CERT_DIR')
IMAGE_PATH = '/usr/bin:/bin'

MODULE_PATH_RE = re.compile(r'[A-Za-z0-9._~+-]+(/[A-Za-z0-9._~+-]+)+')
MODULE_VERSION_RE = re.compile(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
                               r'(-[0-9A-Za-z.-]+)?(\+incompatible)?')
RELEASE_VERSION_RE = re.compile(r'v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)')
GO_VERSION_RE = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)')
GO_LANGUAGE_RE = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\.(0|[1-9][0-9]*))?')
H1_RE = re.compile(r'h1:[A-Za-z0-9+/]{43}=')
SHA256_RE = re.compile(r'[0-9a-f]{64}')
ISO_DATE_RE = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}')
OBJECT_ID_RE = re.compile(r'[0-9a-f]{40}')
REVIEWER_RE = re.compile(r'[A-Za-z0-9_.-]{1,64}')


class ClosureError(Exception):
    """A refusal: bad input, failed verification or a failed operation."""


def refuse(message: str) -> None:
    raise ClosureError(message)


# --- validated domain types ---------------------------------------------------
@dataclass(frozen=True)
class ModulePath:
    value: str


@dataclass(frozen=True)
class ModuleVersion:
    value: str


@dataclass(frozen=True)
class ReleaseVersion:
    value: str


@dataclass(frozen=True)
class GoVersion:
    value: str

    def toolchain(self) -> str:
        return f'go{self.value}'


@dataclass(frozen=True)
class GoLanguage:
    value: str


@dataclass(frozen=True)
class H1:
    value: str


@dataclass(frozen=True)
class Sha256:
    value: str


@dataclass(frozen=True)
class IsoDate:
    value: str


@dataclass(frozen=True)
class ModuleId:
    path: ModulePath
    version: ModuleVersion

    def __str__(self) -> str:
        return f'{self.path.value}@{self.version.value}'


@dataclass(frozen=True)
class RelativeNoticePath:
    value: str


@dataclass(frozen=True)
class LicenseFile:
    name: RelativeNoticePath
    sha256: Sha256


@dataclass(frozen=True)
class LicenseSection:
    id: ModuleId
    name: RelativeNoticePath
    sha256: Sha256
    raw: bytes


@dataclass(frozen=True)
class ModuleIdentity:
    id: ModuleId
    h1: H1


@dataclass(frozen=True)
class ModuleSelection:
    modules: tuple[ModuleIdentity, ...]

    def root(self) -> ModuleIdentity:
        for module in self.modules:
            if module.id.path.value == ROOT:
                return module
        refuse(f'the reviewed closure does not select {ROOT}')
        raise AssertionError('unreachable')

    def dependencies(self) -> dict[str, ModuleIdentity]:
        return {module.id.path.value: module for module in self.modules
                if module.id.path.value != ROOT}

    def identity_of(self, path: ModulePath) -> ModuleIdentity:
        for module in self.modules:
            if module.id.path == path:
                return module
        refuse(f'{path.value} is not in the reviewed closure')
        raise AssertionError('unreachable')


class Architecture(enum.Enum):
    AMD64 = 'amd64'
    ARM64 = 'arm64'

    @property
    def platform(self) -> str:
        return f'linux/{self.value}'


class GoCommand(enum.Enum):
    """The closed set of Go invocations an ordinary build may make.

    There is deliberately no ``install``, ``get``, ``mod tidy``, ``mod download``
    or ``mod vendor`` member: ordinary builds cannot regenerate the graph, and a
    caller cannot express one.
    """

    GOVERSION = ('env', 'GOVERSION')
    GOROOT = ('env', 'GOROOT')
    BUILD = ('build',)
    BUILD_INFO = ('version', '-m', '-json')

    @property
    def argv(self) -> tuple[str, ...]:
        return self.value


class SignedObject(enum.Enum):
    TAG = 'tag'
    COMMIT = 'commit'


def parse_module_path(text: object, what: str) -> ModulePath:
    if type(text) is not str or len(text) > 200 or not MODULE_PATH_RE.fullmatch(text):
        refuse(f'{what}: expected a Go module path')
    if any(part in ('.', '..') for part in text.split('/')):
        refuse(f'{what}: module path contains a relative element')
    return ModulePath(text)


def parse_module_version(text: object, what: str) -> ModuleVersion:
    if type(text) is not str or len(text) > 100 or not MODULE_VERSION_RE.fullmatch(text):
        refuse(f'{what}: expected a semantic module version')
    return ModuleVersion(text)


def parse_release_version(text: object, what: str) -> ReleaseVersion:
    if type(text) is not str or not RELEASE_VERSION_RE.fullmatch(text):
        refuse(f'{what}: expected an exact vX.Y.Z release version')
    return ReleaseVersion(text)


def parse_go_version(text: object, what: str) -> GoVersion:
    if type(text) is not str or not GO_VERSION_RE.fullmatch(text):
        refuse(f'{what}: expected an exact X.Y.Z Go version')
    return GoVersion(text)


def parse_go_language(text: object, what: str) -> GoLanguage:
    if type(text) is not str or not GO_LANGUAGE_RE.fullmatch(text):
        refuse(f'{what}: expected a Go language version such as 1.26')
    return GoLanguage(text)


def parse_h1(text: object, what: str) -> H1:
    if type(text) is not str or not H1_RE.fullmatch(text):
        refuse(f'{what}: expected an h1: checksum')
    return H1(text)


def parse_sha256(text: object, what: str) -> Sha256:
    if type(text) is not str or not SHA256_RE.fullmatch(text):
        refuse(f'{what}: expected a lowercase SHA-256 hex digest')
    return Sha256(text)


def parse_date(text: object, what: str) -> IsoDate:
    if type(text) is not str or not ISO_DATE_RE.fullmatch(text):
        refuse(f'{what}: expected YYYY-MM-DD')
    year, month, day = (int(part) for part in text.split('-'))
    try:
        datetime.date(year, month, day)
    except ValueError:
        refuse(f'{what}: not a calendar date: {text}')
    return IsoDate(text)


def parse_notice_path(text: object, what: str) -> RelativeNoticePath:
    if type(text) is not str or not text or len(text) > 240:
        refuse(f'{what}: expected a relative notice path')
    if '\\' in text or '\x00' in text or text.startswith('/'):
        refuse(f'{what}: not a canonical relative path: {text}')
    parts = text.split('/')
    if any(part in ('', '.', '..') for part in parts):
        refuse(f'{what}: not a canonical relative path: {text}')
    if any(char.isspace() for char in text):
        refuse(f'{what}: notice path contains whitespace: {text}')
    return RelativeNoticePath(text)


def reject_control_characters(value: str, what: str) -> None:
    for char in value:
        code = ord(char)
        if code < 0x20 or 0x7f <= code <= 0x9f:
            refuse(f'{what}: control character in an identity string')


# --- strict decoding ----------------------------------------------------------
class _JsonObject(dict):
    pass


def _reject_constant(name: str) -> None:
    refuse(f'{PIN_FILE}: {name} is not valid JSON')


def _pairs_without_duplicates(pairs: Sequence[tuple[str, object]]) -> _JsonObject:
    result = _JsonObject()
    for key, value in pairs:
        if key in result:
            refuse(f'{PIN_FILE}: duplicate key {key!r}')
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


def decode_text(raw: bytes, what: str) -> str:
    if raw.startswith(b'\xef\xbb\xbf'):
        refuse(f'{what}: leading UTF-8 BOM is not accepted')
    try:
        return raw.decode('utf-8')
    except UnicodeDecodeError as error:
        refuse(f'{what}: not valid UTF-8: {error}')


def require_keys(node: object, required: Sequence[str], optional: Sequence[str],
                 what: str) -> dict:
    if not isinstance(node, dict):
        refuse(f'{what}: expected an object')
    keys = set(node)
    missing = [key for key in required if key not in keys]
    if missing:
        refuse(f'{what}: missing {missing[0]!r}')
    unknown = sorted(keys - set(required) - set(optional))
    if unknown:
        refuse(f'{what}: unknown key {unknown[0]!r}')
    return node


def field_of(mapping: object, key: str, what: str) -> object:
    if not isinstance(mapping, dict):
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


# --- wrapper / root go.mod ----------------------------------------------------
FORBIDDEN_DIRECTIVES = ('replace', 'exclude', 'retract', 'toolchain', 'godebug', 'ignore')


@dataclass(frozen=True)
class WrapperGoMod:
    go: GoLanguage
    requires: Mapping[str, ModuleVersion]


@dataclass(frozen=True)
class RootGoMod:
    go: GoLanguage
    requires: Mapping[str, ModuleVersion]


def render_go_mod(go: GoLanguage, requires: Mapping[str, ModuleVersion]) -> bytes:
    lines = [f'module {WRAPPER}', '', f'go {go.value}', '', f'tool {ROOT}', '', 'require (']
    for path in sorted(requires):
        lines.append(f'\t{path} {requires[path].value} // indirect')
    lines.extend([')', ''])
    return '\n'.join(lines).encode()


def _directive_of(line: str) -> str:
    return line.split(' ', 1)[0]


def _parse_require_entry(line: str, what: str, allow_indirect: bool) -> tuple[str, str]:
    match = re.fullmatch(r'(\S+) (\S+) // indirect', line)
    if match:
        if not allow_indirect:
            refuse(f'{what} is not in the reviewed canonical form')
        return match.group(1), match.group(2)
    match = re.fullmatch(r'(\S+) (\S+)', line)
    if not match:
        refuse(f'{what} is not in the reviewed canonical form')
    return match.group(1), match.group(2)


def parse_wrapper_go_mod(raw: bytes, what: str = GO_MOD_FILE) -> WrapperGoMod:
    text = decode_text(raw, what)
    if not text.endswith('\n'):
        refuse(f'{what} is not in the reviewed canonical form')
    module: str | None = None
    go: GoLanguage | None = None
    tools: list[str] = []
    requires: dict[str, ModuleVersion] = {}
    in_block = False
    for line in text.split('\n')[:-1]:
        if in_block:
            if line == ')':
                in_block = False
                continue
            # A block entry is indented one tab; anything else is not the
            # canonical form and fails the byte-equality check below.
            entry = line[1:] if line.startswith('\t') else line
            path, version = _parse_require_entry(entry, what, allow_indirect=True)
            if path in requires:
                refuse(f'{what} is not in the reviewed canonical form')
            requires[path] = parse_module_version(version, f'{what} {path}')
            continue
        if line == '':
            continue
        directive = _directive_of(line)
        if directive in FORBIDDEN_DIRECTIVES:
            refuse(f'directive not allowed: {directive}')
        if directive == 'module':
            module = line[len('module '):]
        elif directive == 'go':
            go = parse_go_language(line[len('go '):], f'{what} go directive')
        elif directive == 'tool':
            tools.append(line[len('tool '):])
        elif directive == 'require':
            if line == 'require (':
                in_block = True
            else:
                path, version = _parse_require_entry(line[len('require '):], what,
                                                     allow_indirect=False)
                if path in requires:
                    refuse(f'{what} is not in the reviewed canonical form')
                requires[path] = parse_module_version(version, f'{what} {path}')
        else:
            refuse(f'{what} is not in the reviewed canonical form')
    if in_block:
        refuse(f'{what} is not in the reviewed canonical form')
    if module is None or go is None:
        refuse(f'{what} is not in the reviewed canonical form')
    parse_module_path(module, f'{what} module directive')
    if module != WRAPPER:
        refuse(f'{what}: module directive must be {WRAPPER}')
    if tools != [ROOT]:
        refuse(f'{what}: tool directive must be exactly {ROOT}')
    return WrapperGoMod(go=go, requires=requires)


def parse_root_go_mod(raw: bytes, what: str = ROOT_GO_MOD_FILE) -> RootGoMod:
    """Lenient: the upstream file is evidence, not our canonical format."""
    text = decode_text(raw, what)
    go: GoLanguage | None = None
    requires: dict[str, ModuleVersion] = {}
    in_block = False
    for line in text.split('\n'):
        stripped = line.strip()
        if in_block:
            if stripped == ')':
                in_block = False
                continue
            if not stripped or stripped.startswith('//'):
                continue
            body = stripped.split('//', 1)[0].strip()
            path, version = _parse_require_entry(body, what, allow_indirect=False)
            requires[path] = parse_module_version(version, f'{what} {path}')
            continue
        if not stripped or stripped.startswith('//'):
            continue
        directive = stripped.split(' ', 1)[0]
        if directive in ('replace', 'exclude'):
            refuse(f'root module declares {directive}; review required')
        if directive in ('retract', 'godebug', 'ignore'):
            refuse(f'root module declares {directive}; review required')
        if directive == 'go':
            go = parse_go_language(stripped[len('go '):], f'{what} go directive')
        elif directive == 'require':
            if stripped == 'require (':
                in_block = True
            else:
                path, version = _parse_require_entry(stripped[len('require '):], what,
                                                     allow_indirect=False)
                requires[path] = parse_module_version(version, f'{what} {path}')
    if go is None:
        refuse(f'{what}: missing the go directive')
    return RootGoMod(go=go, requires=requires)


# --- go.sum -------------------------------------------------------------------
@dataclass(frozen=True)
class GoSum:
    zip: Mapping[ModuleId, H1]
    mod: Mapping[ModuleId, H1]


def parse_go_sum(raw: bytes, what: str = GO_SUM_FILE) -> GoSum:
    text = decode_text(raw, what)
    zip_sums: dict[ModuleId, H1] = {}
    mod_sums: dict[ModuleId, H1] = {}
    keys: list[tuple] = []
    for line in text.split('\n'):
        if line == '':
            continue
        fields = line.split(' ')
        if len(fields) != 3:
            refuse(f'{what}: malformed line: {line!r}')
        path_text, version_text, hash_text = fields
        path = parse_module_path(path_text, f'{what} line')
        is_mod = version_text.endswith('/go.mod')
        version = parse_module_version(version_text[:-len('/go.mod')] if is_mod else version_text,
                                       f'{what} line')
        digest = parse_h1(hash_text, f'{what} line')
        module = ModuleId(path, version)
        keys.append((path.value, semver_sort_key(version.value), 1 if is_mod else 0))
        target = mod_sums if is_mod else zip_sums
        if module in target:
            kind = 'go.mod hash' if is_mod else 'h1'
            refuse(f'{what}: conflicting {kind} for {path.value} {version.value}')
        target[module] = digest
    if keys != sorted(keys):
        refuse(f'{what}: unsorted or duplicated entries')
    return GoSum(zip=zip_sums, mod=mod_sums)


def semver_sort_key(version: str) -> tuple:
    """``go.sum`` is written in ``semver.Compare`` order, not string order.

    ``v0.12.0`` sorts before ``v0.8.0`` as a string and after it as a version, and
    a go.sum that is not in Go's own order is a hand edit rather than tidy output.
    """
    text = version[1:] if version.startswith('v') else version
    text = text.split('+', 1)[0]
    core, _, pre = text.partition('-')
    numbers = tuple(int(part) for part in core.split('.'))
    if not pre:
        return (numbers, 1, ())
    parts = tuple((0, int(part), '') if part.isdigit() else (1, 0, part)
                  for part in pre.split('.'))
    return (numbers, 0, parts)


def h1_go_mod(raw: bytes) -> H1:
    inner = hashlib.sha256(raw).hexdigest()
    line = f'{inner}  go.mod\n'.encode()
    digest = hashlib.sha256(line).digest()
    return H1('h1:' + base64.b64encode(digest).decode())


def escape_module(text: str) -> str:
    """Go's module-cache escaping: an uppercase letter becomes ``!`` + lowercase."""
    escaped = []
    for char in text:
        escaped.append(f'!{char.lower()}' if char.isupper() else char)
    return ''.join(escaped)


# --- licenses -----------------------------------------------------------------
def parse_licenses(raw: bytes, what: str = LICENSE_FILE) -> tuple[LicenseSection, ...]:
    """Byte-framed: the recorded length is a byte count, so a UTF-8 notice body
    cannot desynchronise the parser. A truncated or drifted section is refused,
    never partially accepted."""
    text = decode_text(raw, what)
    header = LICENSE_HEADER.format(root=ROOT, version=_header_version(text)).encode()
    if not raw.startswith(header):
        refuse(f'{what}: unexpected header')
    sections: list[LicenseSection] = []
    position = len(header)
    while position < len(raw):
        end = raw.find(b'\n', position)
        if end < 0:
            end = len(raw)
        marker = raw[position:end].decode('utf-8', 'replace')
        position = end + 1
        if marker == '':
            if position >= len(raw):
                break
            refuse(f'{what}: empty section separator')
        fields = marker.split(' ')
        if len(fields) != 6 or fields[0] != '===' or not fields[4].startswith('bytes=') \
                or not fields[5].startswith('sha256='):
            refuse(f'{what}: malformed section header: {marker!r}')
        path = parse_module_path(fields[1], f'{what} section')
        version = parse_module_version(fields[2], f'{what} section')
        name = parse_notice_path(fields[3], f'{what} section')
        size_text = fields[4][len('bytes='):]
        if not size_text.isdigit():
            refuse(f'{what}: malformed byte count: {marker!r}')
        size = int(size_text)
        digest = parse_sha256(fields[5][len('sha256='):], f'{what} section')
        body = raw[position:position + size]
        if len(body) != size:
            refuse(f'{what}: truncated section for {path.value} {name.value}')
        position += size
        if raw[position:position + 1] != b'\n':
            refuse(f'{what}: section is not newline-terminated')
        position += 1
        if hashlib.sha256(body).hexdigest() != digest.value:
            refuse(f'license evidence differs from the committed digest: '
                   f'{path.value} {name.value}')
        sections.append(LicenseSection(ModuleId(path, version), name, digest, body))
    if not sections:
        refuse(f'{what}: no license sections')
    return tuple(sections)


def _header_version(text: str) -> str:
    match = re.match(re.escape('# Third-party license files for ') + re.escape(ROOT)
                     + r' (v[^\s]+) and its build closure\.\n', text)
    if not match:
        refuse(f'{LICENSE_FILE}: unexpected header')
    return match.group(1)


def render_licenses(sections: Sequence[LicenseSection], gopls: ReleaseVersion) -> bytes:
    chunks = [LICENSE_HEADER.format(root=ROOT, version=gopls.value).encode()]
    for section in sections:
        header = (f'=== {section.id.path.value} {section.id.version.value} '
                  f'{section.name.value} bytes={len(section.raw)} '
                  f'sha256={section.sha256.value}\n')
        chunks.append(header.encode())
        chunks.append(section.raw)
        chunks.append(b'\n')
    return b''.join(chunks)


def reviewed_nested_notices(module: ModuleId) -> tuple[str, ...]:
    return REVIEWED_NESTED_NOTICES.get((module.path.value, module.version.value), ())


# --- pin / closure model ------------------------------------------------------
@dataclass(frozen=True)
class PinnedModule:
    id: ModuleId
    h1: H1
    go_mod_h1: H1
    licenses: tuple[LicenseFile, ...]


@dataclass(frozen=True)
class Provenance:
    authenticated: bool
    source: str
    vcs: str
    url: str
    subdir: str
    ref: str
    hash: str


@dataclass(frozen=True)
class VcsSignatureEvidence:
    id: ModuleId
    repository: str
    ref: str
    tag_object: str
    commit: str
    signed_object: SignedObject
    format: str
    verified: bool


@dataclass(frozen=True)
class Review:
    reviewed_on: IsoDate
    reviewer: str
    root: ModuleId
    go_mod: Sha256
    go_sum: Sha256
    determination: str
    reason: str
    evidence: tuple[str, ...]
    reassess_when: str
    vcs_signature_policy: str
    known_vcs_signatures: tuple[VcsSignatureEvidence, ...]
    notice_policy: str
    follow_up: str


@dataclass(frozen=True)
class Pin:
    gopls: ReleaseVersion
    go: GoVersion
    module: str
    files: Mapping[str, Sha256]
    modules: tuple[PinnedModule, ...]
    provenance: Provenance
    review: Review


@dataclass(frozen=True)
class Closure:
    directory: Path
    pin: Pin
    go_mod: WrapperGoMod
    go_sum: GoSum
    root: RootGoMod
    licenses: tuple[LicenseSection, ...]


def known_signatures_for(selection: ModuleSelection) -> tuple[VcsSignatureEvidence, ...]:
    """Project the reviewed inventory onto the selected modules.

    A selected path at another version is not an absence of signatures; it is a
    change the inventory has not seen, so it refuses instead of silently omitting
    the row. Unlisted paths make no claim either way.
    """
    selected = {module.id.path.value: module.id.version.value for module in selection.modules}
    projected = []
    for record in KNOWN_VCS_SIGNATURES:
        path = record['path']
        if path not in selected:
            continue
        if selected[path] != record['version']:
            refuse(f'signature inventory needs review for {path}@{selected[path]}')
        projected.append(VcsSignatureEvidence(
            id=ModuleId(ModulePath(path), ModuleVersion(record['version'])),
            repository=record['repository'], ref=record['ref'],
            tag_object=record['tag_object'], commit=record['commit'],
            signed_object=SignedObject(record['signed_object']), format=record['format'],
            verified=record['verified']))
    return tuple(projected)


def validate_signature_scope(signatures: Sequence[VcsSignatureEvidence],
                             selection: ModuleSelection) -> None:
    """The committed inventory is evidence, so only its scope is structural.

    "Partial" means a selected module may be *omitted*: an unlisted module makes no
    claim either way. It does not mean unrelated evidence is admissible -- a record
    has to name an identity this closure actually selects, at that version, because
    the pin describes this closure and nothing else. Equality with today's source
    inventory is deliberately not required: that is the stager's renewal comparison,
    and enforcing it in the checker would make a reviewed evidence change impossible
    to renew without hand-editing the committed record.
    """
    selected = {module.id.path.value: module.id.version.value for module in selection.modules}
    seen: set[ModuleId] = set()
    for signature in signatures:
        if signature.id in seen:
            refuse(f'authentication.known_vcs_signatures: duplicate {signature.id}')
        seen.add(signature.id)
        version = selected.get(signature.id.path.value)
        if version is None:
            refuse(f'signature inventory names {signature.id}, which the closure does not '
                   'select; review required')
        if version != signature.id.version.value:
            refuse(f'signature inventory names {signature.id} but the closure selects '
                   f'{signature.id.path.value}@{version}; review required')


def expected_settings(arch: Architecture) -> dict[str, str]:
    """The build settings this closure must show for a Linux cross-build.

    Unknown keys are a compatibility tripwire: a Go release that adds one needs a
    deliberate change here and in ADR 0008, never automatic acceptance.
    """
    settings = {
        '-buildmode': 'exe', '-compiler': 'gc', '-trimpath': 'true',
        'DefaultGODEBUG': 'tracebacklabels=0,x509sslcertoverrideplatform=0',
        'CGO_ENABLED': '0', 'GOOS': 'linux', 'GOARCH': arch.value,
    }
    if arch is Architecture.AMD64:
        settings['GOAMD64'] = 'v1'
    else:
        settings['GOARM64'] = 'v8.0'
    return settings


# --- pin.json -----------------------------------------------------------------
PIN_KEYS = ('schema', 'gopls', 'go', 'module', 'files', 'modules', 'provenance',
            'authentication')
MODULE_KEYS = ('path', 'version', 'h1', 'go_mod_h1', 'licenses')
LICENSE_KEYS = ('name', 'sha256')
PROVENANCE_KEYS = ('authenticated', 'source', 'vcs', 'url', 'subdir', 'ref', 'hash')
AUTH_KEYS = ('method', 'sumdb', 'proxy', 'determination', 'reviewed_on', 'reviewer',
             'scope', 'reason', 'vcs_signature_policy', 'known_vcs_signatures',
             'notice_policy', 'follow_up', 'evidence', 'reassess_when')
SCOPE_KEYS = ('root', 'go.mod', 'go.sum')
SIGNATURE_KEYS = ('path', 'version', 'repository', 'ref', 'tag_object', 'commit',
                  'signed_object', 'format', 'verified')
FILE_KEYS = ('go.mod', 'go.sum', 'gopls.go.mod', 'THIRD_PARTY_LICENSES')


def _parse_provenance(node: object) -> Provenance:
    require_keys(node, PROVENANCE_KEYS, (), 'provenance')
    authenticated = field_of(node, 'authenticated', 'provenance')
    if authenticated is not False:
        refuse('provenance.authenticated: the proxy origin is unauthenticated metadata')
    values = {key: exact_type(field_of(node, key, 'provenance'), str, f'provenance.{key}')
              for key in ('source', 'vcs', 'url', 'subdir', 'ref', 'hash')}
    for key, value in values.items():
        reject_control_characters(value, f'provenance.{key}')
    return Provenance(authenticated=False, **values)


def _parse_signature(node: object) -> VcsSignatureEvidence:
    require_keys(node, SIGNATURE_KEYS, (), 'authentication.known_vcs_signatures')
    path = parse_module_path(field_of(node, 'path', 'signature'), 'signature.path')
    version = parse_module_version(field_of(node, 'version', 'signature'), 'signature.version')
    repository = exact_type(field_of(node, 'repository', 'signature'), str, 'signature.repository')
    parsed = urllib.parse.urlsplit(repository)
    if parsed.scheme != 'https' or not parsed.hostname or parsed.query or parsed.fragment:
        refuse(f'signature.repository: expected an HTTPS repository URL: {repository}')
    ref = exact_type(field_of(node, 'ref', 'signature'), str, 'signature.ref')
    if not ref.startswith('refs/tags/'):
        refuse('signature.ref: expected a tag ref')
    tag_object = field_of(node, 'tag_object', 'signature')
    commit = field_of(node, 'commit', 'signature')
    for value, what in ((tag_object, 'tag_object'), (commit, 'commit')):
        if type(value) is not str or not OBJECT_ID_RE.fullmatch(value):
            refuse(f'signature.{what}: expected a 40-hex object id')
    signed_text = exact_type(field_of(node, 'signed_object', 'signature'), str,
                             'signature.signed_object')
    try:
        signed_object = SignedObject(signed_text)
    except ValueError:
        refuse(f'signature.signed_object: unsupported signed object {signed_text!r}')
    signature_format = exact_type(field_of(node, 'format', 'signature'), str, 'signature.format')
    if signature_format != 'ssh':
        refuse(f'signature.format: unsupported signature format {signature_format!r}')
    verified = field_of(node, 'verified', 'signature')
    if verified is not False:
        # This record is an availability inventory. Claiming verification here
        # would be a policy claim no code in this repository made.
        refuse('signature.verified: this repository does not verify VCS signatures; '
               'record the finding under the linked follow-up instead')
    return VcsSignatureEvidence(
        id=ModuleId(path, version), repository=repository, ref=ref, tag_object=tag_object,
        commit=commit, signed_object=signed_object, format=signature_format, verified=False)


def _parse_review(node: object, root: ModuleId, go_mod: Sha256, go_sum: Sha256) -> Review:
    require_keys(node, AUTH_KEYS, (), 'authentication')
    method = exact_type(field_of(node, 'method', 'authentication'), str, 'authentication.method')
    if method != 'go-checksum-database':
        refuse('module-channel determination: unsupported authentication method')
    sumdb = exact_type(field_of(node, 'sumdb', 'authentication'), str, 'authentication.sumdb')
    if sumdb != SUMDB:
        refuse('module-channel determination: sumdb is not the reviewed verifier key')
    proxy = exact_type(field_of(node, 'proxy', 'authentication'), str, 'authentication.proxy')
    if proxy != PROXY:
        refuse('module-channel determination: proxy is not the reviewed origin')
    determination = exact_type(field_of(node, 'determination', 'authentication'), str,
                               'authentication.determination')
    if determination not in SUPPORTED_DETERMINATIONS:
        refuse('module-channel determination does not cover this closure: '
               f'unsupported determination {determination!r}')
    scope = require_keys(field_of(node, 'scope', 'authentication'), SCOPE_KEYS, (),
                         'authentication.scope')
    scope_root = exact_type(field_of(scope, 'root', 'authentication.scope'), str,
                            'authentication.scope.root')
    if scope_root != f'{root.path.value}@{root.version.value}':
        refuse('module-channel determination does not cover this closure: scope root '
               f'{scope_root} != {root.path.value}@{root.version.value}')
    scope_go_mod = parse_sha256(field_of(scope, 'go.mod', 'authentication.scope'),
                                'authentication.scope.go.mod')
    scope_go_sum = parse_sha256(field_of(scope, 'go.sum', 'authentication.scope'),
                                'authentication.scope.go.sum')
    if scope_go_mod != go_mod or scope_go_sum != go_sum:
        refuse('module-channel determination does not cover this closure: the reviewed '
               'go.mod/go.sum digests differ from the committed files')
    reviewed_on = parse_date(field_of(node, 'reviewed_on', 'authentication'),
                             'authentication.reviewed_on')
    reviewer = exact_type(field_of(node, 'reviewer', 'authentication'), str,
                          'authentication.reviewer')
    if not REVIEWER_RE.fullmatch(reviewer):
        refuse('authentication.reviewer: expected a reviewer handle')
    reason = exact_type(field_of(node, 'reason', 'authentication'), str, 'authentication.reason')
    reassess = exact_type(field_of(node, 'reassess_when', 'authentication'), str,
                          'authentication.reassess_when')
    for value in (reason, reassess):
        reject_control_characters(value, 'authentication')
    if not reason.strip() or not reassess.strip():
        refuse('module-channel determination: reason and reassess_when must be recorded')
    policy = exact_type(field_of(node, 'vcs_signature_policy', 'authentication'), str,
                        'authentication.vcs_signature_policy')
    if policy not in SUPPORTED_VCS_SIGNATURE_POLICIES:
        refuse('module-channel determination: unsupported VCS signature policy')
    notice_policy = exact_type(field_of(node, 'notice_policy', 'authentication'), str,
                               'authentication.notice_policy')
    if notice_policy not in SUPPORTED_NOTICE_POLICIES:
        refuse('module-channel determination: unsupported notice policy')
    follow_up = exact_type(field_of(node, 'follow_up', 'authentication'), str,
                           'authentication.follow_up')
    if follow_up != AUTH_FOLLOW_UP_URL:
        refuse('module-channel determination: follow-up must be the filed #94 sub-issue')
    evidence = string_list(field_of(node, 'evidence', 'authentication'), 'authentication.evidence')
    if not evidence:
        refuse('module-channel determination: evidence must be recorded')
    for url in evidence:
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme not in ('https',) or not parsed.hostname:
            refuse(f'authentication.evidence: expected an HTTPS URL: {url}')
    raw_signatures = field_of(node, 'known_vcs_signatures', 'authentication')
    if type(raw_signatures) is not list:
        refuse('authentication.known_vcs_signatures: expected a list')
    signatures = tuple(_parse_signature(item) for item in raw_signatures)
    seen: set[ModuleId] = set()
    for signature in signatures:
        if signature.id in seen:
            refuse(f'authentication.known_vcs_signatures: duplicate {signature.id}')
        seen.add(signature.id)
    return Review(reviewed_on=reviewed_on, reviewer=reviewer, root=root, go_mod=go_mod,
                  go_sum=go_sum, determination=determination, reason=reason, evidence=evidence,
                  reassess_when=reassess, vcs_signature_policy=policy,
                  known_vcs_signatures=signatures, notice_policy=notice_policy,
                  follow_up=follow_up)


def load_closure(directory: Path, expect_gopls: ReleaseVersion | None = None,
                 expect_go: GoVersion | None = None) -> Closure:
    directory = Path(directory)
    if not directory.is_dir() or directory.is_symlink():
        refuse(f'not a directory: {directory}')
    present = {entry.name for entry in directory.iterdir()}
    for name in DATA_FILES:
        if name not in present:
            refuse(f'missing input: {name}')
    for name in sorted(present - set(DATA_FILES)):
        refuse(f'unreviewed input: {name}')
    raw: dict[str, bytes] = {}
    for name in DATA_FILES:
        path = directory / name
        if path.is_symlink():
            refuse(f'symlink {name}')
        mode = path.lstat().st_mode
        if not stat.S_ISREG(mode):
            refuse(f'not a regular file: {name}')
        raw[name] = path.read_bytes()

    document = decode_json(raw[PIN_FILE])
    require_keys(document, PIN_KEYS, (), PIN_FILE)
    if exact_int(field_of(document, 'schema', PIN_FILE), 'schema') != 1:
        refuse('pin.json: unsupported schema')
    gopls = parse_release_version(field_of(document, 'gopls', PIN_FILE), 'gopls')
    go = parse_go_version(field_of(document, 'go', PIN_FILE), 'go')
    wrapper_module = exact_type(field_of(document, 'module', PIN_FILE), str, 'module')
    if wrapper_module != WRAPPER:
        refuse(f'pin.json: module must be {WRAPPER}')

    files_node = require_keys(field_of(document, 'files', PIN_FILE), FILE_KEYS, (), 'files')
    files: dict[str, Sha256] = {}
    for name in FILE_KEYS:
        files[name] = parse_sha256(field_of(files_node, name, 'files'), f'files.{name}')
        actual = hashlib.sha256(raw[name]).hexdigest()
        if files[name].value != actual:
            refuse(f'{name}: bytes differ from the committed digest')

    go_mod = parse_wrapper_go_mod(raw[GO_MOD_FILE])
    if render_go_mod(go_mod.go, go_mod.requires) != raw[GO_MOD_FILE]:
        refuse('go.mod is not in the reviewed canonical form')
    root_path = ModulePath(ROOT)
    if root_path.value not in go_mod.requires:
        refuse(f'go.mod: the tool module {ROOT} is not required')
    if go_mod.requires[ROOT].value != gopls.value:
        refuse(f'go.mod: {ROOT} is {go_mod.requires[ROOT].value}, not {gopls.value}')

    go_sum = parse_go_sum(raw[GO_SUM_FILE])
    root_go_mod = parse_root_go_mod(raw[ROOT_GO_MOD_FILE])
    root_id = ModuleId(root_path, ModuleVersion(gopls.value))
    if root_id not in go_sum.mod:
        refuse(f'go.sum lacks go.mod hash for {root_id}')
    if h1_go_mod(raw[ROOT_GO_MOD_FILE]) != go_sum.mod[root_id]:
        refuse('root go.mod evidence does not match go.sum')

    for path in sorted(go_mod.requires):
        if path == ROOT:
            continue
        version = go_mod.requires[path]
        check_adoption_restriction(ModulePath(path), version, root_go_mod.requires)

    if go_mod.go != root_go_mod.go:
        refuse(f'go directive {go_mod.go.value} does not equal the root module\'s '
               f'{root_go_mod.go.value}')
    if not go_language_at_most(go_mod.go, go):
        refuse(f'go directive {go_mod.go.value} is newer than the pinned compiler go{go.value}')

    modules_node = field_of(document, 'modules', PIN_FILE)
    if type(modules_node) is not list or not modules_node:
        refuse('pin.json: modules must be a non-empty list')
    modules: list[PinnedModule] = []
    for item in modules_node:
        node = require_keys(item, MODULE_KEYS, (), 'modules')
        path = parse_module_path(field_of(node, 'path', 'modules'), 'modules.path')
        version = parse_module_version(field_of(node, 'version', 'modules'),
                                       'modules.version')
        licenses_node = field_of(node, 'licenses', 'modules')
        if type(licenses_node) is not list or not licenses_node:
            refuse(f'license evidence missing for {path.value} {version.value}')
        licenses = []
        for license_node in licenses_node:
            require_keys(license_node, LICENSE_KEYS, (), 'modules.licenses')
            licenses.append(LicenseFile(
                name=parse_notice_path(field_of(license_node, 'name', 'modules.licenses'),
                                       'modules.licenses.name'),
                sha256=parse_sha256(field_of(license_node, 'sha256', 'modules.licenses'),
                                    'modules.licenses.sha256')))
        modules.append(PinnedModule(
            id=ModuleId(path, version),
            h1=parse_h1(field_of(node, 'h1', 'modules'), 'modules.h1'),
            go_mod_h1=parse_h1(field_of(node, 'go_mod_h1', 'modules'), 'modules.go_mod_h1'),
            licenses=tuple(licenses)))
    if len({module.id for module in modules}) != len(modules):
        refuse('pin.json: duplicate module')
    if [module.id.path.value for module in modules] != sorted(
            module.id.path.value for module in modules):
        refuse('pin.json: modules must be sorted by path')
    pinned = {module.id.path.value: module for module in modules}
    if set(pinned) != set(go_mod.requires):
        refuse('pin modules disagree with go.mod: the reviewed sets must be identical')
    for path, pinned_module in pinned.items():
        if pinned_module.id.version != go_mod.requires[path]:
            refuse(f'{path}: pinned {pinned_module.id.version.value} != go.mod '
                   f'{go_mod.requires[path].value}')
    for pinned_module in modules:
        if pinned_module.id not in go_sum.zip:
            refuse(f'go.sum lacks h1 for {pinned_module.id}')
        if pinned_module.id not in go_sum.mod:
            refuse(f'go.sum lacks go.mod hash for {pinned_module.id}')
        if go_sum.zip[pinned_module.id] != pinned_module.h1:
            refuse(f'{pinned_module.id} h1 differs from go.sum')
        if go_sum.mod[pinned_module.id] != pinned_module.go_mod_h1:
            refuse(f'{pinned_module.id} go.mod h1 differs from go.sum')
    for root_module in modules:
        if root_module.id != root_id:
            continue
        if root_module.id.path.value == ROOT and root_module.h1.value != go_sum.zip[root_id].value:
            refuse('root module h1 differs from go.sum')
    pins_by_path = {pinned_module.id.path: pinned_module for pinned_module in modules}
    if pins_by_path[root_path].id.version.value != gopls.value:
        refuse('pin.json: the root module version is not the pinned gopls release')

    if _header_version(decode_text(raw[LICENSE_FILE], LICENSE_FILE)) != gopls.value:
        refuse('license evidence header disagrees with the pinned release')
    sections = parse_licenses(raw[LICENSE_FILE])
    committed = {(section.id.path.value, section.id.version.value, section.name.value):
                 section.sha256 for section in sections}
    reviewed = {}
    for pinned_module in modules:
        nested = reviewed_nested_notices(pinned_module.id)
        names = [license.name for license in pinned_module.licenses]
        roots = [name for name in names if '/' not in name.value]
        nested_names = [name.value for name in names if '/' in name.value]
        if not roots:
            refuse(f'{pinned_module.id}: unsupported license layout; review notices and '
                   'update the ADR/layout policy before staging')
        if sorted(nested_names) != sorted(nested):
            refuse(f'{pinned_module.id}: unsupported license layout; review notices and '
                   'update the ADR/layout policy before staging')
        for license in pinned_module.licenses:
            if not NOTICE_NAME_RE.fullmatch(license.name.value.split('/')[-1]):
                refuse(f'{pinned_module.id}: unsupported license layout; review notices and '
                       'update the ADR/layout policy before staging')
            reviewed[(pinned_module.id.path.value, pinned_module.id.version.value,
                      license.name.value)] = license.sha256
    if set(committed) != set(reviewed):
        missing = sorted(set(reviewed) - set(committed))
        extra = sorted(set(committed) - set(reviewed))
        detail = missing[0] if missing else extra[0]
        refuse(f'license evidence does not match pin.json for {detail}')
    for key, digest in reviewed.items():
        if committed[key] != digest:
            refuse(f'license evidence differs from the committed digest: {key[0]} {key[2]}')

    provenance = _parse_provenance(field_of(document, 'provenance', PIN_FILE))
    if provenance.source != f'{PROXY}/{ROOT}/@v/{gopls.value}.info':
        refuse('provenance.source: not the canonical proxy .info origin for this release')
    if provenance.vcs != 'git' or not provenance.hash or not provenance.ref:
        refuse('provenance: incomplete unauthenticated origin record')

    review = _parse_review(field_of(document, 'authentication', PIN_FILE), root_id,
                           files[GO_MOD_FILE], files[GO_SUM_FILE])
    validate_signature_scope(review.known_vcs_signatures, ModuleSelection(tuple(
        ModuleIdentity(pinned_module.id, pinned_module.h1) for pinned_module in modules)))

    if expect_gopls is not None and gopls != expect_gopls:
        refuse(f'asserted gopls version {expect_gopls.value} != pinned {gopls.value}')
    if expect_go is not None and go != expect_go:
        refuse(f'pin generated with go{go.value}; the layer installs go{expect_go.value}')

    return Closure(directory=directory,
                   pin=Pin(gopls=gopls, go=go, module=wrapper_module, files=files,
                           modules=tuple(modules), provenance=provenance, review=review),
                   go_mod=go_mod, go_sum=go_sum, root=root_go_mod, licenses=sections)


def check_adoption_restriction(path: ModulePath, version: ModuleVersion,
                               root_requires: Mapping[str, ModuleVersion]) -> None:
    """The reviewed current-pin adoption restriction (ADR 0008).

    Every module the wrapper selects must be named by the *root's own* go.mod at
    the same version. This is a reviewed restriction of this record, not Go's MVS
    invariant: a legitimate root -> dep -> leaf selection can be refused here, and
    that is deliberate. The closure proof is the committed selection, both
    checksum kinds, isolated readonly resolution and exact post-build identity.
    """
    remediation = ('current-pin adoption restriction changed; inspect upstream go.mod and '
                   'both Linux build lists; update the rule and ADR in a reviewed proposal')
    if path.value not in root_requires:
        refuse(f'{path.value} is not required by the root module; review required\n'
               f'{remediation}')
    if root_requires[path.value] != version:
        refuse(f'{path.value} {version.value} differs from the root module\'s requirement '
               f'{root_requires[path.value].value}; review required\n{remediation}')


def go_language_at_most(language: GoLanguage, version: GoVersion) -> bool:
    parts = [int(part) for part in language.value.split('.')]
    pinned = [int(part) for part in version.value.split('.')]
    while len(parts) < 3:
        parts.append(0)
    return tuple(parts) <= tuple(pinned)


# --- child environment --------------------------------------------------------
@dataclass(frozen=True)
class BuildPaths:
    scratch: Path
    build_cache: Path | None = None

    @property
    def home(self) -> Path:
        return self.scratch / 'home'

    @property
    def gopath(self) -> Path:
        return self.scratch / 'gopath'

    @property
    def module_cache(self) -> Path:
        return self.gopath / 'pkg' / 'mod'

    @property
    def go_cache(self) -> Path:
        return self.build_cache if self.build_cache is not None else self.scratch / 'gocache'

    @property
    def tmp(self) -> Path:
        return self.scratch / 'tmp'

    def prepare(self) -> None:
        for directory in (self.home, self.gopath, self.go_cache, self.tmp):
            directory.mkdir(parents=True, exist_ok=True)


@dataclass(frozen=True)
class Target:
    go_root: Path
    bin_dir: Path
    share_dir: Path
    arch: Architecture
    # ``verify-installed`` inspects this exact path when it is set; installation
    # always writes ``bin_dir/gopls``.
    binary: Path | None = None


@dataclass(frozen=True)
class GoEnvironment:
    """The whole Go environment a build may run in; nothing is inherited."""

    proxy: str
    sumdb: str

    @classmethod
    def production(cls) -> 'GoEnvironment':
        return cls(PROXY, SUMDB)

    @classmethod
    def offline(cls) -> 'GoEnvironment':
        return cls('off', SUMDB)

    def child_env(self, target: Target, paths: BuildPaths,
                  transport: Mapping[str, str]) -> dict[str, str]:
        env = {
            'PATH': f'{target.go_root}/bin:{IMAGE_PATH}',
            'HOME': str(paths.home),
            'GOPATH': str(paths.gopath),
            'GOMODCACHE': str(paths.module_cache),
            'GOCACHE': str(paths.go_cache),
            'TMPDIR': str(paths.tmp),
            'GOENV': 'off',
            'GOWORK': 'off',
            'GOFLAGS': '',
            'GOTOOLCHAIN': 'local',
            'GOPROXY': self.proxy,
            'GOSUMDB': self.sumdb,
            'GONOSUMDB': '',
            'GONOPROXY': '',
            'GOPRIVATE': '',
            'GOINSECURE': '',
            'GOAUTH': 'off',
            'GOVCS': '*:off',
            'CGO_ENABLED': '0',
            'GOOS': 'linux',
            'GOARCH': target.arch.value,
        }
        for key in TRANSPORT_KEYS:
            if key in transport:
                env[key] = transport[key]
        assert set(env) - set(TRANSPORT_KEYS) == set(CHILD_ENV_KEYS)
        return env


def production_transport() -> dict[str, str]:
    return {key: os.environ[key] for key in TRANSPORT_KEYS if key in os.environ}


# --- build info ---------------------------------------------------------------
@dataclass(frozen=True)
class BuildInfo:
    go_version: GoVersion
    main: ModuleIdentity
    deps: tuple[ModuleIdentity, ...]
    settings: Mapping[str, str]


def parse_build_info(raw: str) -> BuildInfo:
    try:
        document = json.loads(raw)
    except json.JSONDecodeError as error:
        refuse(f'build info: not valid JSON: {error}')
    require_keys(document, ('GoVersion', 'Main', 'Deps', 'Settings'), ('Path',), 'build info')
    go_version_text = exact_type(field_of(document, 'GoVersion', 'build info'), str,
                                 'build info GoVersion')
    if go_version_text.startswith('go'):
        go_version_text = go_version_text[len('go'):]
    go_version = parse_go_version(go_version_text, 'build info GoVersion')
    main = _parse_module_identity(field_of(document, 'Main', 'build info'), 'build info Main')
    deps_node = field_of(document, 'Deps', 'build info')
    if type(deps_node) is not list:
        refuse('build info Deps: expected a list')
    deps = tuple(_parse_module_identity(item, 'build info Deps') for item in deps_node)
    paths = [dep.id.path.value for dep in deps]
    if len(set(paths)) != len(paths):
        refuse('build info: duplicate dependency')
    settings_node = field_of(document, 'Settings', 'build info')
    if type(settings_node) is not list:
        refuse('build info Settings: expected a list')
    settings: dict[str, str] = {}
    for item in settings_node:
        node = require_keys(item, ('Key', 'Value'), (), 'build info Settings')
        key = exact_type(field_of(node, 'Key', 'settings'), str, 'settings.Key')
        value = exact_type(field_of(node, 'Value', 'settings'), str, 'settings.Value')
        if key in settings:
            refuse(f'build info: duplicate setting {key}')
        settings[key] = value
    return BuildInfo(go_version=go_version, main=main, deps=deps, settings=settings)


def _parse_module_identity(node: object, what: str) -> ModuleIdentity:
    require_keys(node, ('Path', 'Version', 'Sum'), ('Replace',), what)
    if 'Replace' in node:
        refuse(f'{what}: module replacement is not part of the reviewed closure')
    path = parse_module_path(field_of(node, 'Path', what), f'{what}.Path')
    version = parse_module_version(field_of(node, 'Version', what), f'{what}.Version')
    digest = parse_h1(field_of(node, 'Sum', what), f'{what}.Sum')
    return ModuleIdentity(ModuleId(path, version), digest)


def verify_module_build_info(selection: ModuleSelection, info: BuildInfo, arch: Architecture,
                             go: GoVersion) -> None:
    if info.go_version != go:
        refuse(f'compiler is go{info.go_version.value}, want go{go.value}')
    root = selection.root()
    if info.main != root:
        refuse(f'build used {info.main.id} {info.main.h1.value}, not the reviewed root '
               f'{root.id} {root.h1.value}')
    expected = selection.dependencies()
    actual = {dep.id.path.value: dep for dep in info.deps}
    unknown = sorted(set(actual) - set(expected))
    if unknown:
        refuse(f'build used {actual[unknown[0]].id}, not in the reviewed closure')
    missing = sorted(set(expected) - set(actual))
    if missing:
        refuse(f'build omitted {missing[0]} from the reviewed closure')
    for path, module in expected.items():
        if actual[path].id.version != module.id.version:
            refuse(f'build used {actual[path].id}, not in the reviewed closure')
        if actual[path].h1 != module.h1:
            refuse(f'build used {path} {actual[path].h1.value}, not the reviewed content')
    expected_settings_map = expected_settings(arch)
    if dict(info.settings) != expected_settings_map:
        refuse('build settings differ from the reviewed Linux target settings: '
               f'{dict(info.settings)} != {expected_settings_map}')


# --- Go process execution -----------------------------------------------------
Runner = Callable[[GoCommand, Sequence[str], Mapping[str, str], Path],
                  subprocess.CompletedProcess]


def run_go(command: GoCommand, extra: Sequence[str], env: Mapping[str, str],
           cwd: Path) -> subprocess.CompletedProcess:
    go = os.path.join(env['PATH'].split(os.pathsep)[0], 'go')
    argv = [go, *command.argv, *extra]
    return subprocess.run(argv, cwd=str(cwd), env=dict(env), capture_output=True, text=True)


def _require_success(proc: subprocess.CompletedProcess, what: str) -> subprocess.CompletedProcess:
    if proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or '').strip()
        refuse(f'{what} failed (status {proc.returncode}): {detail}')
    return proc


def _env_output(proc: subprocess.CompletedProcess, what: str) -> str:
    return _require_success(proc, what).stdout.strip()


def require_pinned_compiler(runner: Runner, env: Mapping[str, str], cwd: Path,
                            go: GoVersion, target: Target) -> None:
    """Refuse before any fetch if the pinned compiler is not the one that will run."""
    version = _env_output(runner(GoCommand.GOVERSION, (), env, cwd), 'go env GOVERSION')
    if version != go.toolchain():
        refuse(f'compiler is {version}, want {go.toolchain()}')
    goroot = _env_output(runner(GoCommand.GOROOT, (), env, cwd), 'go env GOROOT')
    if os.path.realpath(goroot) != os.path.realpath(str(target.go_root)):
        refuse(f'GOROOT is {goroot}, want the pinned {target.go_root}')


def compile_module(work: Path, selection: ModuleSelection, go: GoVersion, target: Target,
                   env: Mapping[str, str], runner: Runner, out: Path) -> BuildInfo:
    """One readonly build, then proof from the binary's own build information.

    Shared by the ordinary build and by the maintenance stager so both agree on
    what "the selected closure was compiled" means. The produced binary is never
    executed here: it is a foreign-architecture Linux program on a macOS host.
    """
    require_pinned_compiler(runner, env, work, go, target)
    out.parent.mkdir(parents=True, exist_ok=True)
    _require_success(runner(GoCommand.BUILD,
                            ('-mod=readonly', '-trimpath', '-buildvcs=false',
                             '-o', str(out), ROOT),
                            env, work), 'go build')
    raw = _env_output(runner(GoCommand.BUILD_INFO, (str(out),), env, work),
                      'go version -m -json')
    info = parse_build_info(raw)
    verify_module_build_info(selection, info, target.arch, go)
    return info


def selection_from_closure(closure: Closure) -> ModuleSelection:
    modules = [ModuleIdentity(module.id, module.h1) for module in closure.pin.modules]
    modules.sort(key=lambda module: module.id.path.value)
    return ModuleSelection(tuple(modules))


def verify_build_info(closure: Closure, info: BuildInfo, arch: Architecture,
                      go: GoVersion) -> None:
    verify_module_build_info(selection_from_closure(closure), info, arch, go)


# --- licenses vs the module cache ---------------------------------------------
def collect_notices(directory: Path, module: ModuleId) -> tuple[tuple[str, bytes], ...]:
    """The notice files of one module directory, by canonical relative path.

    One rule for both consumers: the stager transports these bytes and the build
    re-reads them from the cache to prove the build used the same ones. Whether the
    resulting layout is *reviewed* is decided by the caller with
    ``reviewed_nested_notices``, not here.
    """
    if not directory.is_dir():
        refuse(f'license evidence missing for {module}: the cache has no {directory}')
    found = []
    for path in sorted(directory.rglob('*')):
        if path.is_symlink():
            refuse(f'{module}: unsupported license layout; a notice path is a symlink')
        if not path.is_file() or not NOTICE_NAME_RE.fullmatch(path.name):
            continue
        found.append((path.relative_to(directory).as_posix(), path.read_bytes()))
    return tuple(found)


def verified_licenses(closure: Closure) -> bytes:
    """The committed notice bytes, re-proved against the pinned digest.

    Publication writes these bytes, never a fresh read of a mutable directory, and
    the same digest is what ``verify-installed`` checks: what was verified is what
    is shipped.
    """
    raw = (closure.directory / LICENSE_FILE).read_bytes()
    if hashlib.sha256(raw).hexdigest() != closure.pin.files[LICENSE_FILE].value:
        refuse(f'{LICENSE_FILE}: bytes differ from the committed digest')
    if parse_licenses(raw) != closure.licenses:
        refuse(f'{LICENSE_FILE}: parsed sections differ from the reviewed evidence')
    return raw


def verify_module_licenses(closure: Closure, module_cache: Path) -> None:
    """The cache bytes the build actually used must equal the committed notices."""
    committed = {(section.id.path.value, section.id.version.value, section.name.value):
                 section.raw for section in closure.licenses}
    actual: dict[tuple[str, str, str], bytes] = {}
    for module in closure.pin.modules:
        directory = (module_cache
                     / f'{escape_module(module.id.path.value)}'
                       f'@{escape_module(module.id.version.value)}')
        for relative, raw in collect_notices(directory, module.id):
            actual[(module.id.path.value, module.id.version.value, relative)] = raw
    if set(actual) != set(committed):
        missing = sorted(set(committed) - set(actual))
        extra = sorted(set(actual) - set(committed))
        detail = missing[0] if missing else extra[0]
        refuse(f'license evidence does not match the build cache for {detail[0]} {detail[2]}')
    for key, raw in committed.items():
        if actual[key] != raw:
            refuse(f'license evidence differs from the build cache: {key[0]} {key[2]}')


# --- ordinary build -----------------------------------------------------------
@dataclass(frozen=True)
class BuiltGopls:
    path: Path
    # The notice bytes this build verified against the module cache. Publication
    # writes exactly these, so a later change to the data directory cannot ship
    # bytes that were never checked.
    licenses: bytes


def build(closure: Closure, target: Target, environment: GoEnvironment, runner: Runner,
          paths: BuildPaths, *, transport: Mapping[str, str]) -> BuiltGopls:
    paths.prepare()
    work = paths.scratch / 'module'
    work.mkdir()
    for name in (GO_MOD_FILE, GO_SUM_FILE):
        # The verified bytes are copied, never re-read from a mutable checkout.
        raw = (closure.directory / name).read_bytes()
        if hashlib.sha256(raw).hexdigest() != closure.pin.files[name].value:
            refuse(f'{name}: bytes differ from the committed digest')
        (work / name).write_bytes(raw)
    env = environment.child_env(target, paths, transport)
    out = paths.scratch / 'out' / 'gopls'
    compile_module(work, selection_from_closure(closure), closure.pin.go, target, env, runner,
                   out)
    verify_module_licenses(closure, Path(env['GOMODCACHE']))
    return BuiltGopls(out, verified_licenses(closure))


def _remove_scratch(paths: BuildPaths) -> None:
    """Remove the invocation's scratch tree, reporting anything it cannot remove.

    A retained scratch tree is a resource leak, not a verification failure, so it is
    reported with the exact path instead of being swallowed -- or turned into a
    second exception that would hide the failure being handled.
    """
    failures: list[str] = []

    def make_writable_and_remove(function, target, _exception) -> None:
        try:
            os.chmod(target, stat.S_IWUSR | stat.S_IRUSR | stat.S_IXUSR
                     if os.path.isdir(target) else stat.S_IWUSR | stat.S_IRUSR)
            function(target)
        except OSError as error:
            failures.append(f'{target}: {error}')

    if not paths.scratch.exists():
        return
    for directory, _names, _files in os.walk(paths.scratch):
        try:
            os.chmod(directory, 0o700)
        except OSError as error:
            failures.append(f'{directory}: {error}')
    shutil.rmtree(paths.scratch, onerror=make_writable_and_remove)
    if paths.scratch.exists() or failures:
        print(f'gopls-closure: could not remove the invocation scratch tree '
              f'{paths.scratch}: {"; ".join(failures) or "still present"}', file=sys.stderr)


def install(closure: Closure, target: Target, environment: GoEnvironment | None = None,
            runner: Runner | None = None, paths: BuildPaths | None = None, *,
            transport: Mapping[str, str] | None = None) -> None:
    environment = environment or GoEnvironment.production()
    runner = runner or run_go
    transport = production_transport() if transport is None else transport
    binary = target.bin_dir / 'gopls'
    for existing in (binary, target.share_dir):
        if os.path.lexists(existing):
            refuse(f'refusing to install over an existing path: {existing}')
    created_scratch: BuildPaths | None = None
    if paths is None:
        created_scratch = BuildPaths(Path(tempfile.mkdtemp(prefix='gopls-closure-')))
        paths = created_scratch
    published: list[Path] = []
    partial: Path | None = None

    def owned(path: Path) -> Path:
        """Claim a destination *before* the first fallible operation on it.

        Registering after the write leaves a half-created file that a later failure
        cannot withdraw -- exactly the case a metadata error produces (the bytes
        exist, the mode change does not), so ownership is taken first.
        """
        published.append(path)
        return path

    try:
        built = build(closure, target, environment, runner, paths, transport=transport)
        target.bin_dir.mkdir(parents=True, exist_ok=True)
        target.share_dir.mkdir(parents=True, exist_ok=True)
        partial = target.bin_dir / '.gopls.new'
        shutil.copyfile(built.path, partial)
        os.chmod(partial, 0o755)
        owned(binary)
        os.replace(partial, binary)
        licenses = owned(target.share_dir / LICENSE_FILE)
        licenses.write_bytes(built.licenses)
        os.chmod(licenses, 0o644)
    except BaseException:
        # The primary failure is what the caller must see; a rollback problem is
        # reported with the retained paths so a half-published tool cannot be
        # mistaken for a clean refusal.
        withdrawn: list[str] = []
        for path in ([partial] if partial is not None else []) + published:
            if not os.path.lexists(path):
                continue
            try:
                os.unlink(path)
            except OSError as error:
                withdrawn.append(f'{path}: {error}')
        if withdrawn:
            print('gopls-closure: could not withdraw every published path; retained: '
                  + '; '.join(withdrawn), file=sys.stderr)
        raise
    finally:
        if created_scratch is not None:
            _remove_scratch(created_scratch)


def verify_installed(closure: Closure, target: Target, runner: Runner | None = None) -> None:
    runner = runner or run_go
    # Exactly the requested path: a sibling with the expected name is never a
    # substitute, or the command's success would say nothing about its argument.
    binary = target.binary or (target.bin_dir / 'gopls')
    if binary.is_symlink() or not binary.is_file():
        refuse(f'installed: missing {binary}')
    mode = binary.stat().st_mode & 0o777
    if mode != 0o755:
        refuse(f'installed: {binary} has mode {mode:o}, want 755')
    licenses = target.share_dir / LICENSE_FILE
    if licenses.is_symlink() or not licenses.is_file():
        refuse(f'installed: missing {licenses}')
    installed = licenses.read_bytes()
    if hashlib.sha256(installed).hexdigest() != closure.pin.files[LICENSE_FILE].value:
        refuse(f'installed: {licenses} does not match the pinned license digest')
    if parse_licenses(installed) != closure.licenses:
        refuse(f'installed: {licenses} does not carry the reviewed notice sections')
    paths = BuildPaths(Path(tempfile.mkdtemp(prefix='gopls-verify-')))
    try:
        paths.prepare()
        env = GoEnvironment.offline().child_env(target, paths, {})
        work = paths.scratch / 'module'
        work.mkdir()
        (work / GO_MOD_FILE).write_bytes((closure.directory / GO_MOD_FILE).read_bytes())
        (work / GO_SUM_FILE).write_bytes((closure.directory / GO_SUM_FILE).read_bytes())
        require_pinned_compiler(runner, env, work, closure.pin.go, target)
        raw = _env_output(runner(GoCommand.BUILD_INFO, (str(binary),), env, work),
                          'go version -m -json')
        verify_build_info(closure, parse_build_info(raw), target.arch, closure.pin.go)
    finally:
        _remove_scratch(paths)


# --- CLI ----------------------------------------------------------------------
def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    subparsers = parser.add_subparsers(dest='command', required=True)
    check = subparsers.add_parser('check', help='verify the committed closure offline')
    check.add_argument('data_dir')
    check.add_argument('--expect-gopls')
    check.add_argument('--expect-go')
    current = subparsers.add_parser('current', help='print the committed selection')
    current.add_argument('data_dir')
    for name in ('install', 'verify-installed'):
        command = subparsers.add_parser(name)
        command.add_argument('data_dir')
        command.add_argument('--expect-gopls', required=True)
        command.add_argument('--expect-go', required=True)
        command.add_argument('--go-root', required=True)
        command.add_argument('--arch', required=True, choices=[arch.value for arch in Architecture])
        if name == 'install':
            command.add_argument('--bin-dir', required=True)
            command.add_argument('--share-dir', required=True)
        else:
            command.add_argument('--binary', required=True)
            command.add_argument('--share-dir', required=True)
    return parser


def _expected(arguments: argparse.Namespace) -> tuple[ReleaseVersion | None, GoVersion | None]:
    gopls = (parse_release_version(arguments.expect_gopls, '--expect-gopls')
             if getattr(arguments, 'expect_gopls', None) else None)
    go = (parse_go_version(arguments.expect_go, '--expect-go')
          if getattr(arguments, 'expect_go', None) else None)
    return gopls, go


def _target(arguments: argparse.Namespace, arch: Architecture, share_dir: str,
            bin_dir: str | None) -> Target:
    go_root = Path(arguments.go_root)
    if not go_root.is_absolute() or not (go_root / 'bin' / 'go').is_file():
        refuse(f'--go-root must be an absolute Go root with bin/go: {go_root}')
    if bin_dir is None:
        binary = Path(arguments.binary)
        if not binary.is_absolute():
            refuse(f'--binary must be absolute: {binary}')
        if binary.name != 'gopls':
            refuse(f'--binary must name the installed tool gopls, not {binary.name!r}')
        return Target(go_root=go_root, bin_dir=binary.parent, share_dir=Path(share_dir),
                      arch=arch, binary=binary)
    return Target(go_root=go_root, bin_dir=Path(bin_dir), share_dir=Path(share_dir), arch=arch)


def main(argv: Sequence[str] | None = None) -> int:
    arguments = build_parser().parse_args(argv)
    try:
        gopls, go = _expected(arguments)
        closure = load_closure(Path(arguments.data_dir), gopls, go)
        if arguments.command == 'check':
            print(closure.pin.gopls.value)
            return 0
        if arguments.command == 'current':
            print(f'gopls={closure.pin.gopls.value}')
            print(f'go={closure.pin.go.value}')
            return 0
        arch = Architecture(arguments.arch)
        if arguments.command == 'install':
            if not gopls or not go:
                refuse('install requires --expect-gopls and --expect-go')
            target = _target(arguments, arch, arguments.share_dir, arguments.bin_dir)
            install(closure, target)
        else:
            if not gopls or not go:
                refuse('verify-installed requires --expect-gopls and --expect-go')
            target = _target(arguments, arch, arguments.share_dir, None)
            verify_installed(closure, target)
        print(closure.pin.gopls.value)
        return 0
    except ClosureError as error:
        print(f'gopls-closure: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
