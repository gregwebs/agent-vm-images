#!/usr/bin/env python3
"""Offline case table for the committed Codex payload closure (#97).

Everything here is hermetic: no network, no Docker, no payload archive and no
execution of fixture binaries. It drives the real production modules
(`images/tools/codex/codex_pin.py` and `script/dev/pin-codex.py`) against
synthetic pins, synthetic tar/gzip streams and injected host dependencies, and
covers the three things the shell suites cannot:

  1. the strict pin/evidence schema, the independently derived URLs and the
     strict corroboration (metadata + SHA256SUMS) — including every substitution
     a jointly rewritten release would have to make;
  2. real ELF inspection (synthetic but well-formed ELF headers, never executed)
     and the bounded archive parser: member policy, package metadata, decoded
     stream/extended-header/allocation budgets, and gzip integrity;
  3. the owner writer's staging rules and its transport/verifier limits, with
     the seams injected rather than relaxed.

`--real-signatures` additionally runs the real reviewed cosign against retained
payloads. That mode is opt-in, needs network access to Sigstore and is never part
of the hermetic aggregate.

usage: python3 script/test/codex-pins.py [--real-signatures --archive-dir DIR --cosign FILE]
"""
from __future__ import annotations

import argparse
import base64
import errno
import gzip
import hashlib
import http.server
import importlib.util
import io
import json
import os
import re
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import urllib.error
from pathlib import Path
from typing import Mapping

sys.dont_write_bytecode = True

REPO_ROOT = Path(__file__).resolve().parents[2]
CODEX_DIR = REPO_ROOT / 'images' / 'tools' / 'codex'
RELEASE = 'rust-v0.159.3'
VERSION = '0.159.3'
NEXT_RELEASE = 'rust-v0.159.4'
TARGETS = {'linux/amd64': 'x86_64-unknown-linux-musl',
           'linux/arm64': 'aarch64-unknown-linux-musl'}
MACHINES = {'x86_64-unknown-linux-musl': 62, 'aarch64-unknown-linux-musl': 183}
ISSUER = 'https://token.actions.githubusercontent.com'

failures: list[str] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: object) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which a
    # bare module_from_spec does not populate (Python 3.12 raises there).
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


codex_pin = _load('codex_pin_under_test', CODEX_DIR / 'codex_pin.py')
pin_codex = _load('pin_codex_under_test', REPO_ROOT / 'script' / 'dev' / 'pin-codex.py')
# One codex_pin instance for the whole table: the writer must see the same
# patched budgets and limits the cases below exercise.
pin_codex._modules[(CODEX_DIR / 'codex_pin.py').resolve()] = codex_pin
# The loader and the writer each declare their own refusal type; a case must
# accept either, because the production call chain raises both.
REFUSALS = (pin_codex.Refusal, codex_pin.Refusal)


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


# --- synthetic fixtures ------------------------------------------------------

def elf_header(machine: int, *, elf_class: int = 2, data: int = 1,
               version: int = 1, magic: bytes = b'\x7fELF') -> bytes:
    return (magic + bytes([elf_class, data, version, 0]) + b'\x00' * 8
            + (3).to_bytes(2, 'little') + machine.to_bytes(2, 'little') + b'\x00' * 44)


def certificate() -> bytes:
    return b'-----BEGIN CERTIFICATE-----\nMIIBfixtureCertificate\n-----END CERTIFICATE-----\n'


def make_bundle(payload_digest: str, *, signature: bytes = b'fixture-signature',
                body: dict | None = None, cert: bytes | None = None) -> bytes:
    """A structurally valid legacy cosign bundle for one payload digest."""
    cert = certificate() if cert is None else cert
    if body is None:
        body = {
            'apiVersion': '0.0.1',
            'kind': 'hashedrekord',
            'spec': {
                'data': {'hash': {'algorithm': 'sha256', 'value': payload_digest}},
                'signature': {'content': base64.b64encode(signature).decode(),
                              'publicKey': {'content': base64.b64encode(cert).decode()}},
            },
        }
    return json.dumps({
        'base64Signature': base64.b64encode(signature).decode(),
        'cert': base64.b64encode(cert).decode(),
        'rekorBundle': {
            'SignedEntryTimestamp': base64.b64encode(b'fixture-set').decode(),
            'Payload': {'body': base64.b64encode(json.dumps(body).encode()).decode(),
                        'integratedTime': 1, 'logID': 'a' * 64, 'logIndex': 1},
        },
    }, indent=2).encode() + b'\n'


def make_metadata(digests: Mapping[str, str] | None = None, *, release: str = RELEASE,
                  extra_assets: tuple = (), download_count: int = 1,
                  tag: str | None = None) -> bytes:
    assets = [{'name': name, 'digest': f'sha256:{digest}', 'download_count': download_count}
              for name, digest in digests.items()]
    assets += list(extra_assets)
    return json.dumps({'tag_name': release if tag is None else tag,
                       'name': release, 'assets': assets}, indent=2).encode() + b'\n'


def make_checksums(digests: Mapping[str, str], extra: tuple = ()) -> bytes:
    lines = [f'{digest}  {name}' for name, digest in digests.items()]
    lines += [f'{digest}  {name}' for name, digest in extra]
    return ('\n'.join(lines) + '\n').encode()


def package_document(version: str = VERSION, target: str = TARGETS['linux/amd64'],
                     **overrides) -> dict:
    document = {'layoutVersion': 1, 'version': version, 'target': target,
                'variant': 'codex', 'entrypoint': 'bin/codex',
                'resourcesDir': 'codex-resources', 'pathDir': 'codex-path'}
    document.update(overrides)
    return document


def tar_header(name: str, size: int = 0, typeflag: bytes = b'0', mode: int = 0o644,
               linkname: str = '') -> bytes:
    block = bytearray(b'\x00' * 512)
    encoded = name.encode()
    assert len(encoded) <= 100, name
    block[0:len(encoded)] = encoded

    def octal(value: int, width: int) -> bytes:
        return format(value, 'o').rjust(width - 1, '0').encode() + b'\x00'

    block[100:108] = octal(mode, 8)
    block[108:116] = octal(0, 8)
    block[116:124] = octal(0, 8)
    block[124:136] = octal(size, 12)
    block[136:148] = octal(0, 12)
    block[148:156] = b' ' * 8
    block[156:157] = typeflag
    link = linkname.encode()
    block[157:157 + len(link)] = link
    block[257:265] = b'ustar\x0000'
    block[265:297] = b'root'.ljust(32, b'\x00')
    block[297:329] = b'root'.ljust(32, b'\x00')
    block[329:337] = octal(0, 8)
    block[337:345] = octal(0, 8)
    block[148:156] = format(sum(block), '06o').encode() + b'\x00 '
    return bytes(block)


def tar_member(name: str, data: bytes = b'', typeflag: bytes = b'0',
               mode: int = 0o644, linkname: str = '') -> bytes:
    return (tar_header(name, len(data), typeflag, mode, linkname) + data
            + b'\x00' * (-len(data) % 512))


def pax_record(key: str, value: str) -> bytes:
    body = f' {key}={value}\n'.encode()
    length = len(body) + 1
    while len(str(length).encode()) + len(body) != length:
        length = len(str(length).encode()) + len(body)
    return str(length).encode() + body


def gzip_bytes(raw: bytes) -> bytes:
    handle = io.BytesIO()
    with gzip.GzipFile(fileobj=handle, mode='wb', mtime=0) as stream:
        stream.write(raw)
    return handle.getvalue()


def default_members(*, version: str = VERSION,
                    target: str = TARGETS['linux/amd64'],
                    package: dict | None = None,
                    machine: int | None = None,
                    elf: bytes | None = None) -> list[tuple[str, bytes, bytes, int]]:
    """(name, data, typeflag, mode) in tar order."""
    document = package_document(version, target) if package is None else package
    header = elf if elf is not None else elf_header(
        MACHINES[target] if machine is None else machine)
    return [
        ('bin', b'', tarfile.DIRTYPE, 0o755),
        ('bin/codex', header + b'\x00' * 64, tarfile.REGTYPE, 0o755),
        ('bin/codex-code-mode-host', b'host\n', tarfile.REGTYPE, 0o755),
        ('codex-package.json', json.dumps(document).encode(), tarfile.REGTYPE, 0o644),
        ('codex-path', b'', tarfile.DIRTYPE, 0o755),
        ('codex-path/rg', b'rg\n', tarfile.REGTYPE, 0o755),
        ('codex-resources', b'', tarfile.DIRTYPE, 0o755),
        ('codex-resources/bwrap', b'bwrap\n', tarfile.REGTYPE, 0o755),
    ]


def build_archive(path: Path, members: list[tuple[str, bytes, bytes, int]] | None = None,
                  **kwargs) -> Path:
    entries = default_members(**kwargs) if members is None else members
    raw = b''
    for name, data, typeflag, mode in entries:
        raw += tar_member(name, data, typeflag, mode)
    raw += b'\x00' * 1024
    path.write_bytes(gzip_bytes(raw))
    return path


class Fixture:
    """A complete synthetic closure: pin.json plus its four evidence files."""

    def __init__(self, directory: Path, *, release: str = RELEASE,
                 archives: Mapping[str, bytes] | None = None) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.release = release
        version = release[len('rust-v'):]
        self.archives = dict(archives or self._default_archives(version))
        self.checksums = make_checksums({
            'codex-package-x86_64-unknown-linux-musl.tar.gz':
                sha256(self.archives['linux/amd64']),
            'codex-package-aarch64-unknown-linux-musl.tar.gz':
                sha256(self.archives['linux/arm64']),
        }, extra=(('decoy', '1111111111111111111111111111111111111111111111111111111111111111'),))
        self.bundles = {
            platform: make_bundle(sha256(raw)) for platform, raw in self.archives.items()}
        self.metadata = make_metadata(release=release, digests={
            'codex-package-x86_64-unknown-linux-musl.tar.gz': sha256(
                self.archives['linux/amd64']),
            'codex-package-aarch64-unknown-linux-musl.tar.gz': sha256(
                self.archives['linux/arm64']),
            'codex-package_SHA256SUMS': sha256(self.checksums),
            'codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore':
                sha256(self.bundles['linux/amd64']),
            'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore':
                sha256(self.bundles['linux/arm64']),
        })
        self.pin = self._pin()
        self.write()

    @staticmethod
    def _default_archives(version: str) -> dict[str, bytes]:
        """Byte-deterministic per platform, so a repeat is genuinely a repeat.

        `tarfile.open(mode='w:gz')` stamps the current time into the gzip header,
        which would make every fixture a different identity and hide exactly the
        repeat/unchanged behaviour these cases assert.
        """
        archives = {}
        for platform, target in TARGETS.items():
            raw = b''
            for name, data, typeflag, mode in default_members(version=version,
                                                              target=target):
                raw += tar_member(name, data, typeflag, mode)
            raw += b'\x00' * 1024
            archives[platform] = gzip_bytes(raw)
        return archives

    def _pin(self) -> dict:
        def evidence(url: str, path: str, raw: bytes) -> dict:
            return {'url': url, 'path': path, 'sha256': sha256(raw)}

        def asset_url(asset: str) -> str:
            return f'https://github.com/openai/codex/releases/download/{self.release}/{asset}'

        platforms = {}
        for platform, target in TARGETS.items():
            asset = f'codex-package-{target}.tar.gz'
            platforms[platform] = {
                'target': target,
                'asset': asset,
                'url': asset_url(asset),
                'sha256': sha256(self.archives[platform]),
                'bundle': evidence(asset_url(f'{asset}.sigstore'),
                                   f'evidence/{asset}.sigstore',
                                   self.bundles[platform]),
            }
        return {
            'schema': 1, 'name': 'codex', 'kind': 'codex-standalone-package',
            'release': self.release, 'version': self.release[len('rust-v'):],
            'package': {'layout_version': 1, 'variant': 'codex',
                        'entrypoint': 'bin/codex'},
            'metadata': evidence(
                f'https://api.github.com/repos/openai/codex/releases/tags/{self.release}',
                'evidence/release.json', self.metadata),
            'checksums': evidence(asset_url('codex-package_SHA256SUMS'),
                                 'evidence/codex-package_SHA256SUMS', self.checksums),
            'platforms': platforms,
            'authentication': {
                'method': 'sigstore-blob-pin-time',
                'verifier': {'name': 'cosign', 'version': 'v2.4.1'},
                'trust_root': 'sigstore-production-tuf',
                'certificate_identity':
                    'https://github.com/openai/codex/.github/workflows/rust-release.yml'
                    f'@refs/tags/{self.release}',
                'certificate_oidc_issuer': ISSUER,
                'verified_on': '2026-01-02',
            },
        }

    def write(self, pin: dict | None = None) -> None:
        pin = self.pin if pin is None else pin
        (self.directory / 'evidence').mkdir(parents=True, exist_ok=True)
        (self.directory / 'pin.json').write_text(json.dumps(pin, indent=2) + '\n')
        (self.directory / 'evidence' / 'release.json').write_bytes(self.metadata)
        (self.directory / 'evidence' / 'codex-package_SHA256SUMS').write_bytes(self.checksums)
        for platform, target in TARGETS.items():
            (self.directory / 'evidence' /
             f'codex-package-{target}.tar.gz.sigstore').write_bytes(self.bundles[platform])

    def archive(self, work: Path, platform: str = 'linux/amd64') -> Path:
        directory = Path(work)
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / f'archive-{platform.replace("/", "-")}.tar.gz'
        path.write_bytes(self.archives[platform])
        return path

    def loaded(self):
        return codex_pin.load_pin(self.directory)


def expect_refusal(label: str, call, *fragments: str) -> None:
    """A refusal, not an accident: the message must name the reason."""
    try:
        call()
    except codex_pin.Refusal as error:
        text = str(error)
        missing = [fragment for fragment in fragments if fragment not in text]
        if missing:
            fail(label, f'the refusal does not mention {missing}: {text}')
            return
        report(label)
        return
    except REFUSALS as error:
        text = str(error)
        missing = [fragment for fragment in fragments if fragment not in text]
        if missing:
            fail(label, f'the refusal does not mention {missing}: {text}')
            return
        report(label)
        return
    fail(label, 'no refusal')


class guard:
    """Trip any attempt to resolve a name, open a socket or start a process."""

    def __enter__(self):
        self._socket = socket.socket
        self._create = socket.create_connection
        self._popen = subprocess.Popen

        def refuse(*_args, **_kwargs):
            raise AssertionError('the offline path attempted network or a subprocess')
        socket.socket = refuse
        socket.create_connection = refuse
        subprocess.Popen = refuse
        return self

    def __exit__(self, *_exception):
        socket.socket = self._socket
        socket.create_connection = self._create
        subprocess.Popen = self._popen
        return False


def lowered(module, **values):
    """Set named module budgets for one case and restore them afterwards."""
    class _Scope:
        def __enter__(self):
            self.saved = {name: getattr(module, name) for name in values}
            for name, value in values.items():
                setattr(module, name, value)
            return self

        def __exit__(self, *_exception):
            for name, value in self.saved.items():
                setattr(module, name, value)
            return False
    return _Scope()


# --- case groups -------------------------------------------------------------

def case_loader(work: Path) -> None:
    label = 'valid_pin_both_platforms'
    fixture = Fixture(work / 'valid')
    try:
        loaded = fixture.loaded()
    except codex_pin.Refusal as error:
        fail(label, error)
    else:
        if loaded.release != RELEASE:
            fail(label, f'the loaded release is {loaded.release}')
        else:
            for platform in codex_pin.Platform:
                selected = codex_pin.select(loaded, RELEASE, codex_pin.Target[platform.name])
                if selected.target.value != TARGETS[platform.value]:
                    fail(label, f'{platform.value} selected {selected.target.value}')
                    return
            report(label)

    label = 'select_record_is_four_exact_fields'
    output = io.StringIO()
    saved = sys.stdout
    sys.stdout = output
    try:
        status = codex_pin.main(['select', '--pin-dir', str(fixture.directory),
                                 '--release', RELEASE,
                                 '--target', 'x86_64-unknown-linux-musl'])
    finally:
        sys.stdout = saved
    fields = output.getvalue().split('\n')[:-1]
    expected = [VERSION, 'codex-package-x86_64-unknown-linux-musl.tar.gz',
                f'https://github.com/openai/codex/releases/download/{RELEASE}/'
                'codex-package-x86_64-unknown-linux-musl.tar.gz',
                sha256(fixture.archives['linux/amd64'])]
    if status != 0 or fields != expected:
        fail(label, f'{status}: {fields}')
    else:
        report(label)

    label = 'offline_check_no_network'
    with guard():
        saved_out, saved_err = sys.stdout, sys.stderr
        sys.stdout, sys.stderr = io.StringIO(), io.StringIO()
        try:
            status = codex_pin.main(['check', '--pin-dir', str(fixture.directory),
                                     '--release', RELEASE])
            stderr = sys.stderr.getvalue()
        finally:
            sys.stdout, sys.stderr = saved_out, saved_err
    if status != 0:
        fail(label, f'{status}: {stderr}')
    else:
        report(label)

    label = 'check_requested_release_mismatch'
    saved_err = sys.stderr
    sys.stderr = io.StringIO()
    try:
        status = codex_pin.main(['check', '--pin-dir', str(fixture.directory),
                                 '--release', NEXT_RELEASE])
        stderr = sys.stderr.getvalue()
        captured = sys.stdout
    finally:
        sys.stderr = saved_err
    del captured
    if status != 1 or NEXT_RELEASE not in stderr:
        fail(label, f'{status}: {stderr}')
    else:
        report(label)

    label = 'default_budgets_are_the_documented_constants'
    documented = {'MAX_COMPRESSED_BYTES': 256 << 20, 'MAX_DECODED_BYTES': 1 << 30,
                  'MAX_EXPANDED_REGULAR_BYTES': 1 << 30, 'MAX_LOGICAL_MEMBERS': 4096,
                  'MAX_PHYSICAL_HEADERS': 4096,
                  'MAX_PACKAGE_METADATA_BYTES': 64 << 10,
                  'MAX_EXTENDED_HEADER_BYTES': 64 << 10,
                  'MAX_EXTENDED_TOTAL_BYTES': 1 << 20,
                  'MAX_METADATA_BYTES': 8 << 20, 'MAX_CHECKSUMS_BYTES': 1 << 20,
                  'MAX_BUNDLE_BYTES': 4 << 20, 'READ_CHUNK_BYTES': 64 << 10}
    wrong = {name: getattr(codex_pin, name, None) for name, value in documented.items()
             if getattr(codex_pin, name, None) != value}
    if wrong:
        fail(label, wrong)
    else:
        report(label)


def _pin_with(fixture: Fixture, mutate) -> dict:
    pin = json.loads(json.dumps(fixture.pin))
    mutate(pin)
    return pin


def case_schema(work: Path) -> None:
    fixture = Fixture(work / 'schema')

    def reload(pin: dict):
        return lambda: codex_pin.parse_pin(
            (json.dumps(pin) + '\n').encode(), fixture.directory)

    cases = [
        ('pin_duplicate_key', 'duplicate JSON key',
         lambda: codex_pin.parse_pin(
             (json.dumps(fixture.pin)[:-1] + ', "schema": 1}\n').encode(),
             fixture.directory)),
        ('pin_unknown_field', 'unexpected',
         reload({**fixture.pin, 'extra': 1})),
        ('pin_bool_schema', 'integer',
         reload({**fixture.pin, 'schema': True})),
        ('pin_schema_float', 'fractional',
         lambda: codex_pin.parse_pin(
             json.dumps(fixture.pin).replace('"schema": 1', '"schema": 1.0').encode()
             + b'\n', fixture.directory)),
        ('pin_trailing_input', 'trailing input',
         lambda: codex_pin.parse_pin(
             (json.dumps(fixture.pin) + '\n{}\n').encode(), fixture.directory)),
        ('pin_non_object_root', 'JSON object',
         lambda: codex_pin.parse_pin(b'[]\n', fixture.directory)),
        ('pin_missing_platform', 'must have exactly',
         reload(_pin_with(fixture, lambda pin: pin['platforms'].pop('linux/arm64')))),
        ('pin_extra_platform', 'must have exactly',
         reload(_pin_with(fixture, lambda pin: pin['platforms'].update(
             {'linux/armv7': dict(pin['platforms']['linux/arm64'])})))),
        ('pin_unknown_kind', 'kind must be',
         reload({**fixture.pin, 'kind': 'release-archive'})),
        ('pin_unknown_name', 'name must be',
         reload({**fixture.pin, 'name': 'gh'})),
        ('release_alias_or_range', 'exact Codex version',
         lambda: codex_pin.require_release('rust-v0.159')),
        ('release_leading_zero', 'exact Codex version',
         lambda: codex_pin.require_release('rust-v01.2.3')),
        ('release_latest', 'must be rust-v<version>',
         lambda: codex_pin.require_release('latest')),
        ('release_pin_mismatch', 'not the pinned Codex release',
         lambda: codex_pin.select(fixture.loaded(), NEXT_RELEASE,
                                  codex_pin.Target.AMD64)),
        ('pin_release_version_disagreement', 'does not spell pin version',
         reload({**fixture.pin, 'release': NEXT_RELEASE})),
        ('platform_target_mismatch', 'target must be',
         reload(_pin_with(fixture, lambda pin: pin['platforms']['linux/amd64'].update(
             {'target': TARGETS['linux/arm64']})))),
        ('authentication_wrong_identity', 'certificate identity must be',
         reload(_pin_with(fixture, lambda pin: pin['authentication'].update(
             {'certificate_identity': pin['authentication']['certificate_identity']
              .replace('rust-release.yml', 'other.yml')})))),
        ('authentication_wrong_issuer', 'OIDC issuer must be',
         reload(_pin_with(fixture, lambda pin: pin['authentication'].update(
             {'certificate_oidc_issuer': 'https://accounts.google.com'})))),
        ('authentication_wrong_verifier', 'verifier version must be',
         reload(_pin_with(fixture, lambda pin: pin['authentication']['verifier'].update(
             {'version': 'v2.2.0'})))),
        ('authentication_future_date', 'in the future',
         reload(_pin_with(fixture, lambda pin: pin['authentication'].update(
             {'verified_on': '2999-01-01'})))),
        ('authentication_bad_date', 'not a calendar date',
         reload(_pin_with(fixture, lambda pin: pin['authentication'].update(
             {'verified_on': '2026-02-30'})))),
        ('package_layout_bool', 'integer',
         reload({**fixture.pin, 'package': {'layout_version': True,
                                           'variant': 'codex',
                                           'entrypoint': 'bin/codex'}})),
        ('package_variant', 'variant must be',
         reload({**fixture.pin, 'package': {'layout_version': 1,
                                           'variant': 'app-server',
                                           'entrypoint': 'bin/codex'}})),
        ('package_entrypoint', 'entrypoint must be',
         reload({**fixture.pin, 'package': {'layout_version': 1,
                                           'variant': 'codex',
                                           'entrypoint': 'codex'}})),
        ('digest_uppercase', 'lower-case hex',
         reload(_pin_with(fixture, lambda pin: pin['platforms']['linux/amd64'].update(
             {'sha256': pin['platforms']['linux/amd64']['sha256'].upper()})))),
    ]
    for name, fragment, call in cases:
        expect_refusal(name, call, fragment)


def case_urls(work: Path) -> None:
    fixture = Fixture(work / 'urls')
    mutations = {
        'traversal': lambda url: url.replace('/rust-v0.159.3/', '/rust-v0.159.3/../x/'),
        'extra_segment': lambda url: url.replace('/download/', '/download/extra/'),
        'wrong_repo': lambda url: url.replace('openai/codex', 'openai/codexx'),
        'wrong_host': lambda url: url.replace('github.com', 'github.example'),
        'lookalike_host': lambda url: url.replace('github.com', 'github.com.evil'),
        'wrong_port': lambda url: url.replace('github.com', 'github.com:444'),
        'query': lambda url: url + '?raw=1',
        'fragment': lambda url: url + '#x',
        'credentials': lambda url: url.replace('https://', 'https://user:pw@'),
        'other_tag': lambda url: url.replace('rust-v0.159.3', 'rust-v0.159.2'),
    }
    for name, mutation in mutations.items():
        for key in ('url', 'bundle'):
            def make(key=key, mutation=mutation):
                def run():
                    pin = json.loads(json.dumps(fixture.pin))
                    entry = pin['platforms']['linux/amd64']
                    if key == 'url':
                        entry['url'] = mutation(entry['url'])
                    else:
                        entry['bundle']['url'] = mutation(entry['bundle']['url'])
                    codex_pin.parse_pin((json.dumps(pin) + '\n').encode(),
                                        fixture.directory)
                return run
            expect_refusal(f'url_mutations/{name}/{key}', make())
    expect_refusal('url_mutations/metadata_url',
                   lambda: codex_pin.parse_pin(
                       (json.dumps(_pin_with(fixture, lambda pin: pin['metadata'].update(
                           {'url': 'https://api.github.com/repos/openai/codex/releases/'
                                   'tags/rust-v0.159.3?x=1'}))) + '\n').encode(),
                       fixture.directory),
                   'metadata evidence url must be')


def case_evidence(work: Path) -> None:
    """Every evidence file must be a regular non-symlink file with its pin digest."""

    def fresh(name: str) -> Fixture:
        return Fixture(Path(work) / name)

    def check(name: str, mutate, fragment: str, rewrite: bool = False) -> None:
        fixture = fresh(name)
        mutate(fixture)
        if rewrite:
            fixture.write()
        expect_refusal(name, fixture.loaded, fragment)

    check('evidence_missing',
          lambda fixture: (fixture.directory / 'evidence' / 'release.json').unlink(),
          'missing at')

    def symlink_leaf(fixture):
        target = fixture.directory / 'evidence' / 'release.json'
        moved = fixture.directory / 'release.json.moved'
        shutil.move(str(target), str(moved))
        target.symlink_to(moved)
    check('evidence_symlink', symlink_leaf, 'not a regular file')

    def symlink_ancestor(fixture):
        evidence = fixture.directory / 'evidence'
        moved = fixture.directory / 'evidence-moved'
        shutil.move(str(evidence), str(moved))
        evidence.symlink_to(moved)
    check('evidence_ancestor_symlink', symlink_ancestor, 'symlink')

    check('evidence_missing_bundle',
          lambda fixture: (fixture.directory / 'evidence' /
                           'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore'
                           ).unlink(),
          'missing at')

    def corrupt_other_arch_bundle(fixture):
        (fixture.directory / 'evidence' /
         'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore').write_bytes(
            b'{"base64Signature":')
    check('evidence_hash_mismatch', corrupt_other_arch_bundle,
          'does not match the pin digest')

    def swap_other_arch_bundle(fixture):
        # A valid bundle, but for the wrong payload: caught structurally, never
        # presented as a cryptographic result.
        other = make_bundle('f' * 64)
        (fixture.directory / 'evidence' /
         'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore').write_bytes(other)
        fixture.bundles['linux/arm64'] = other
        fixture.pin['platforms']['linux/arm64']['bundle']['sha256'] = sha256(other)
        fixture.metadata = make_metadata(_metadata_digests(fixture))
        fixture.pin['metadata']['sha256'] = sha256(fixture.metadata)
    check('evidence_bundle_for_other_payload', swap_other_arch_bundle, 'entry is for',
          rewrite=True)


def case_corroboration(work: Path) -> None:
    def with_metadata(mutate, label, fragment):
        fixture = Fixture(work / f'corr-{label}')
        fixture.metadata = mutate(fixture)
        fixture.pin['metadata']['sha256'] = sha256(fixture.metadata)
        fixture.write()
        expect_refusal(label, fixture.loaded, fragment)

    def duplicate_tag(fixture):
        document = json.loads(fixture.metadata)
        return (json.dumps({'tag_name': RELEASE, 'assets': document['assets']})
                [:-1] + ', "tag_name": "%s"}\n' % RELEASE).encode()
    with_metadata(duplicate_tag, 'metadata_duplicate_tag', 'duplicate JSON key')

    def duplicate_asset(fixture):
        document = json.loads(fixture.metadata)
        document['assets'].append(dict(document['assets'][0]))
        return (json.dumps(document) + '\n').encode()
    with_metadata(duplicate_asset, 'metadata_duplicate_asset', 'repeats asset')

    with_metadata(lambda fixture: make_metadata(
        _metadata_digests(fixture), release=NEXT_RELEASE),
        'metadata_wrong_tag', 'names')

    def bad_digest(fixture):
        document = json.loads(fixture.metadata)
        document['assets'][0]['digest'] = 'sha256:nothex'
        return (json.dumps(document) + '\n').encode()
    with_metadata(bad_digest, 'metadata_bad_digest', 'non-sha256 digest')

    def wrong_digest(fixture):
        document = json.loads(fixture.metadata)
        document['assets'][0]['digest'] = 'sha256:' + '0' * 64
        return (json.dumps(document) + '\n').encode()
    with_metadata(wrong_digest, 'metadata_digest_disagrees', 'but the pin commits')

    def missing_asset(fixture):
        document = json.loads(fixture.metadata)
        document['assets'] = [entry for entry in document['assets']
                              if 'aarch64' not in entry['name']]
        return (json.dumps(document) + '\n').encode()
    with_metadata(missing_asset, 'metadata_missing_asset', 'does not list the required')

    def null_payload_digest(fixture):
        document = json.loads(fixture.metadata)
        for entry in document['assets']:
            if entry['name'].startswith('codex-package-aarch64'):
                entry['digest'] = None
        return (json.dumps(document) + '\n').encode()
    with_metadata(null_payload_digest, 'metadata_null_payload_digest',
                  'no digest for')

    def with_checksums(mutate, label, fragment):
        fixture = Fixture(work / f'corr-{label}')
        fixture.checksums = mutate(fixture)
        fixture.metadata = make_metadata(_metadata_digests(fixture, checksums=True))
        fixture.pin['checksums']['sha256'] = sha256(fixture.checksums)
        fixture.pin['metadata']['sha256'] = sha256(fixture.metadata)
        fixture.pin['platforms']['linux/amd64']['bundle']['sha256'] = sha256(
            fixture.bundles['linux/amd64'])
        fixture.pin['platforms']['linux/arm64']['bundle']['sha256'] = sha256(
            fixture.bundles['linux/arm64'])
        fixture.write()
        expect_refusal(label, fixture.loaded, fragment)

    def missing_line(fixture):
        return b'\n'.join(line for line in fixture.checksums.split(b'\n')
                          if b'aarch64' not in line)
    with_checksums(missing_line, 'checksums_missing_line', 'no line for')

    def duplicate_line(fixture):
        lines = [line for line in fixture.checksums.split(b'\n') if b'x86_64' in line]
        return (fixture.checksums.rstrip(b'\n') + b'\n' + b'\n'.join(lines) + b'\n')
    with_checksums(duplicate_line, 'checksums_duplicate_line', 'repeat the entry')

    def disagree(fixture):
        return b'\n'.join((b'0' * 64 + line[64:]) if b'aarch64' in line else line
                          for line in fixture.checksums.split(b'\n'))
    with_checksums(disagree, 'checksums_disagree_with_pin', 'disagree with the pin')

    def unreadable(fixture):
        return fixture.checksums + b'this is not a checksum record\n'
    with_checksums(unreadable, 'checksums_unreadable_line', 'unreadable line')

    # Bundles: shape and cross-field consistency, never a cryptographic claim.
    bundle_cases = {
        'bundle_malformed': (b'{"base64Signature":', 'not JSON'),
        'bundle_tampered': (None, 'entry is for'),
        'bundle_wrong_shape': (None, 'must have exactly'),
        'bundle_not_a_certificate': (None, 'not a PEM certificate'),
        'bundle_not_hashedrekord': (None, 'hashedrekord'),
    }
    for name, (_unused, fragment) in bundle_cases.items():
        fixture = Fixture(work / f'bundle-{name}')
        target = 'codex-package-x86_64-unknown-linux-musl.tar.gz'
        if name == 'bundle_tampered':
            raw = make_bundle('f' * 64)
        elif name == 'bundle_wrong_shape':
            raw = json.dumps({'signature': 'x'}).encode()
        elif name == 'bundle_not_a_certificate':
            raw = make_bundle(sha256(fixture.archives['linux/amd64']), cert=b'not a pem')
        elif name == 'bundle_not_hashedrekord':
            raw = make_bundle(sha256(fixture.archives['linux/amd64']),
                              body={'kind': 'intoto', 'spec': {}})
        else:
            raw = bundle_cases[name][0]
        fixture.bundles['linux/amd64'] = raw
        fixture.pin['platforms']['linux/amd64']['bundle']['sha256'] = sha256(raw)
        fixture.metadata = make_metadata(_metadata_digests(fixture))
        fixture.pin['metadata']['sha256'] = sha256(fixture.metadata)
        fixture.write()
        expect_refusal(name, fixture.loaded, fragment)


def _metadata_digests(fixture: Fixture, *, checksums: bool = True,
                      bundles: bool = True) -> dict:
    digests = {
        'codex-package-x86_64-unknown-linux-musl.tar.gz':
            sha256(fixture.archives['linux/amd64']),
        'codex-package-aarch64-unknown-linux-musl.tar.gz':
            sha256(fixture.archives['linux/arm64']),
    }
    if checksums:
        digests['codex-package_SHA256SUMS'] = sha256(fixture.checksums)
    if bundles:
        digests['codex-package-x86_64-unknown-linux-musl.tar.gz.sigstore'] = sha256(
            fixture.bundles['linux/amd64'])
        digests['codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore'] = sha256(
            fixture.bundles['linux/arm64'])
    return digests


def case_preflight(work: Path) -> None:
    fixture = Fixture(work / 'preflight')

    def refuse(label: str, source: Fixture, platform: str, archive: Path,
               fragment: str) -> None:
        loaded = source.loaded()
        chosen = codex_pin.Target['AMD64' if platform == 'linux/amd64' else 'ARM64']
        selected = codex_pin.select(loaded, RELEASE, chosen)
        expect_refusal(label,
                       lambda: codex_pin.preflight_archive(loaded, selected, archive),
                       fragment)

    def consistent(label: str, builder, fragment: str,
                   platform: str = 'linux/amd64') -> None:
        """A pin whose digest names the mutant archive, so the policy is the refuser."""
        directory = Path(work) / ('pf-' + label.replace('/', '-'))
        directory.mkdir(parents=True, exist_ok=True)
        archive = builder(directory)
        archives = dict(fixture.archives)
        archives[platform] = archive.read_bytes()
        source = Fixture(directory / 'fixture', archives=archives)
        refuse(label, source, platform, archive, fragment)

    def build(**kwargs):
        def builder(directory: Path) -> Path:
            directory.mkdir(parents=True, exist_ok=True)
            return build_archive(directory / 'archive.tar.gz', **kwargs)
        return builder

    consistent('wrong_package_version', build(version='0.159.4'), 'version is not the pinned')
    consistent('wrong_package_target', build(target='aarch64-unknown-linux-musl'),
               'target is not')
    consistent('wrong_package_variant', build(package=package_document(variant='daemon')),
               'variant must be')
    consistent('wrong_package_layout',
               build(package=package_document(layoutVersion=2)), 'layoutVersion must be')
    consistent('wrong_entrypoint', build(package=package_document(entrypoint='codex')),
               'entrypoint must be')
    consistent('wrong_package_layout_bool',
               build(package=package_document(layoutVersion=True)), 'integer')
    consistent('wrong_elf_machine', build(machine=183), 'ELF machine')
    consistent('wrong_elf_class', build(elf=elf_header(62, elf_class=1)), '64-bit ELF')
    consistent('wrong_elf_data', build(elf=elf_header(62, data=2)), 'little-endian')
    consistent('not_an_elf', build(elf=b'#!/bin/sh\necho codex\n'), 'not an ELF')

    # Archive identity: a consistently-rewritten server still has to match the
    # committed digest.
    loaded = fixture.loaded()
    expected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    directory = Path(work) / 'pf-identity-alt'
    directory.mkdir(parents=True, exist_ok=True)
    different = build_archive(
        directory / 'different.tar.gz',
        members=[entry if entry[0] != 'bin/codex'
                 else ('bin/codex', elf_header(62) + b'\x00' * 128, tarfile.REGTYPE,
                       0o755)
                 for entry in default_members()])
    expect_refusal('preflight_identity_mismatch',
                   lambda: codex_pin.preflight_archive(loaded, expected, different),
                   'does not match the committed')

    def member_cases():
        base = default_members()
        cases = {
            'absolute': ('/bin/codex', 'absolute member name',
                         base + [('/etc/passwd', b'x', tarfile.REGTYPE, 0o644)]),
            'traversal': ('../evil', 'non-canonical member name',
                          base + [('../evil', b'x', tarfile.REGTYPE, 0o644)]),
            'dot_spelling': ('./extra', 'non-canonical member name',
                             base + [('./extra', b'x', tarfile.REGTYPE, 0o644)]),
            'dot_member': ('.', 'non-canonical member name',
                           [('.', b'', tarfile.DIRTYPE, 0o755)] + base),
            'duplicate': ('bin/codex', 'duplicate archive member',
                          base + [('bin/codex', b'y', tarfile.REGTYPE, 0o755)]),
            'device': ('dev/null', 'special member',
                       base + [('dev/null', b'', tarfile.CHRTYPE, 0o644)]),
            'fifo': ('pipe', 'special member',
                     base + [('pipe', b'', tarfile.FIFOTYPE, 0o644)]),
            'setuid': ('bin/suid', 'setuid/setgid member',
                       base + [('bin/suid', b'x', tarfile.REGTYPE, 0o4755)]),
            'link': ('bin/link', 'link member',
                     base + [('bin/link', b'', tarfile.SYMTYPE, 0o777)]),
            'parent_conflict': ('codex-path/rg', 'non-directory ancestor',
                                [('codex-path', b'x', tarfile.REGTYPE, 0o755)]
                                + [entry for entry in base
                                   if entry[0] not in ('codex-path', 'codex-path/rg')]
                                + [('codex-path/rg', b'rg\n', tarfile.REGTYPE, 0o755)]),
            'extra_top_root': ('lib/codex.so', 'unexpected top-level',
                               base + [('lib', b'', tarfile.DIRTYPE, 0o755),
                                       ('lib/codex.so', b'x', tarfile.REGTYPE, 0o644)]),
            'missing_required': ('bin/codex-code-mode-host', 'missing required file',
                                 [entry for entry in base
                                  if entry[0] != 'bin/codex-code-mode-host']),
            'not_executable': ('bin/codex', 'is not executable',
                               [entry if entry[0] != 'bin/codex'
                                else ('bin/codex', entry[1], tarfile.REGTYPE, 0o644)
                                for entry in base]),
            'missing_package_metadata': ('codex-package.json', 'missing codex-package.json',
                                        [entry for entry in base
                                         if entry[0] != 'codex-package.json']),
        }
        return cases

    for name, (_target, fragment, members) in member_cases().items():
        consistent(f'archive_member_controls/{name}',
                   (lambda directory, members=members: build_archive(
                       directory / 'archive.tar.gz', members)), fragment)

    # Oversized package metadata (64 KiB default, no patching).
    def oversized_metadata(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        document = package_document()
        document['padding'] = 'x' * (codex_pin.MAX_PACKAGE_METADATA_BYTES + 1)
        return build_archive(directory / 'archive.tar.gz', package=document)
    consistent('oversized_package_metadata', oversized_metadata, 'exceeding')

    def budget_case(label: str, builder, fragment: str, **budgets) -> None:
        directory = work / f'budget-{label}'
        archive = builder(directory)
        fixture_local = Fixture(directory / 'fixture', archives={
            **fixture.archives, 'linux/amd64': archive.read_bytes()})
        loaded = fixture_local.loaded()
        selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
        with lowered(codex_pin, **budgets):
            expect_refusal(label, lambda: codex_pin.preflight_archive(
                loaded, selected, archive), fragment)

    budget_case('archive_member_controls/logical_member_budget',
                lambda directory: (directory.mkdir(parents=True, exist_ok=True),
                                   build_archive(directory / 'archive.tar.gz'))[1],
                'more than 3 members', MAX_LOGICAL_MEMBERS=3)
    budget_case('archive_member_controls/regular_file_total_budget',
                lambda directory: (directory.mkdir(parents=True, exist_ok=True),
                                   build_archive(directory / 'archive.tar.gz'))[1],
                'expanded regular files total', MAX_EXPANDED_REGULAR_BYTES=10)

    # The decoded-stream budget is independent of member sizes: the same small
    # compressed input is refused for its decoded length, at limit + 1.
    def small_archive(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        return build_archive(directory / 'archive.tar.gz')

    budget_case('gzip_expansion_bomb', small_archive, 'decoded stream exceeds',
                MAX_DECODED_BYTES=2048)
    budget_case('tar_padding_budget', small_archive, 'decoded stream exceeds',
                MAX_DECODED_BYTES=4096)

    # Bytes after tar's end marker are still decoded bytes: only the drain finds
    # them, so this case proves the drain is bounded.
    def trailing(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        raw = b''
        for name, data, typeflag, mode in default_members():
            raw += tar_member(name, data, typeflag, mode)
        raw += b'\x00' * 1024
        decoded = len(raw)
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw + b'\x00' * 200000))
        (directory / 'decoded').write_text(str(decoded))
        return path
    directory = work / 'budget-trailing'
    archive = trailing(directory)
    decoded = int((directory / 'decoded').read_text())
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': archive.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    with lowered(codex_pin, MAX_DECODED_BYTES=decoded + 1000):
        expect_refusal('trailing_decoded_budget',
                       lambda: codex_pin.preflight_archive(loaded, selected, archive),
                       'decoded stream exceeds')

    # Concatenated gzip members: the second member's bytes count too.
    def concatenated(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        raw = b''
        for name, data, typeflag, mode in default_members():
            raw += tar_member(name, data, typeflag, mode)
        raw += b'\x00' * 1024
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw) + gzip_bytes(b'\x00' * 200000))
        (directory / 'decoded').write_text(str(len(raw)))
        return path
    directory = work / 'budget-concat'
    archive = concatenated(directory)
    decoded = int((directory / 'decoded').read_text())
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': archive.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    with lowered(codex_pin, MAX_DECODED_BYTES=decoded + 1000):
        expect_refusal('trailing_decoded_budget/concatenated_gzip_member',
                       lambda: codex_pin.preflight_archive(loaded, selected, archive),
                       'decoded stream exceeds')

    # Extended-header allocation bombs: refused in the raw pass, before tarfile
    # or this module allocates the declared body.
    def raw_case(label: str, raw: bytes, fragment: str, **budgets) -> None:
        directory = work / f'raw-{label}'
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw))
        fixture_local = Fixture(directory / 'fixture', archives={
            **fixture.archives, 'linux/amd64': path.read_bytes()})
        loaded = fixture_local.loaded()
        selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
        with lowered(codex_pin, **budgets):
            expect_refusal(label, lambda: codex_pin.preflight_archive(
                loaded, selected, path), fragment)

    raw_case('pax_single_header_bomb', tar_header('PaxHeaders/x', 1 << 20, b'x'),
             'extended header of')
    raw_case('gnu_long_header_bomb', tar_header('././@LongLink', 1 << 20, b'L'),
             'extended header of')
    raw_case('pax_aggregate_bomb',
             b''.join(tar_member('PaxHeaders/%d' % index,
                                 pax_record('comment', 'x' * 40), b'x')
                      for index in range(6)),
             'extended headers total', MAX_EXTENDED_TOTAL_BYTES=128)
    raw_case('physical_header_budget',
             b''.join(tar_member('spare%d' % index, b'') for index in range(10)),
             'more than 3 archive headers', MAX_PHYSICAL_HEADERS=3)
    raw_case('sparse_or_ambiguous_raw_layout/gnu_sparse',
             tar_header('sparse', 0, b'S'), 'GNU sparse members')
    raw_case('sparse_or_ambiguous_raw_layout/v7_directory',
             tar_header('dir/', 512, b'\x00') + b'\x00' * 512,
             'V7 directory header declares')
    raw_case('sparse_or_ambiguous_raw_layout/pax_sparse',
             tar_member('PaxHeaders/x', pax_record('GNU.sparse.size', '512'), b'x')
             + tar_member('spare', b''), 'sparse PAX metadata')

    # A PAX size override must move both walks identically: this archive is legal
    # and must pass, so a passing case here is what makes the bomb cases mean
    # something.
    def pax_size_override(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        payload = pax_record('size', '1024')
        raw = b''
        raw += tar_member('PaxHeaders/codex-package.json', payload, b'x')
        raw += tar_header('codex-package.json', 0, b'0', 0o644)
        raw += json.dumps(package_document()).encode().ljust(1024, b' ')
        for name, data, typeflag, mode in default_members():
            if name == 'codex-package.json':
                continue
            raw += tar_member(name, data, typeflag, mode)
        raw += b'\x00' * 1024
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw))
        return path
    directory = work / 'pax-override'
    archive = pax_size_override(directory)
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': archive.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    try:
        codex_pin.preflight_archive(loaded, selected, archive)
    except codex_pin.Refusal as error:
        fail('pax_size_override_alignment', f'legal PAX override refused: {error}')
    else:
        report('pax_size_override_alignment')

    # global `g` then local `x`: the earliest pending set wins for `size`, and
    # both walks must agree on where the member body ends.
    def pax_precedence(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        raw = b''
        raw += tar_member('PaxHeaders/global', pax_record('comment', 'g'), b'g')
        raw += tar_member('PaxHeaders/local', pax_record('size', '1024'), b'x')
        raw += tar_header('codex-package.json', 0, b'0', 0o644)
        raw += json.dumps(package_document()).encode().ljust(1024, b' ')
        for name, data, typeflag, mode in default_members():
            if name == 'codex-package.json':
                continue
            raw += tar_member(name, data, typeflag, mode)
        raw += b'\x00' * 1024
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw))
        return path
    directory = work / 'pax-precedence'
    archive = pax_precedence(directory)
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': archive.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    try:
        codex_pin.preflight_archive(loaded, selected, archive)
    except codex_pin.Refusal as error:
        fail('pax_global_local_precedence', f'legal global+local PAX refused: {error}')
    else:
        report('pax_global_local_precedence')

    # A hidden bomb: the PAX override hides a large region from a non-override
    # walk. Lower the decoded budget so the refusal proves every byte counted.
    def hidden_bomb(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        raw = b''
        raw += tar_member('PaxHeaders/codex-package.json',
                          pax_record('size', str(400000)), b'x')
        raw += tar_header('codex-package.json', 0, b'0', 0o644)
        raw += json.dumps(package_document()).encode().ljust(400000, b' ')
        for name, data, typeflag, mode in default_members():
            if name == 'codex-package.json':
                continue
            raw += tar_member(name, data, typeflag, mode)
        raw += b'\x00' * 1024
        path = directory / 'archive.tar.gz'
        path.write_bytes(gzip_bytes(raw))
        return path
    budget_case('hidden_pax_override_bomb', hidden_bomb, 'decoded stream exceeds',
                MAX_DECODED_BYTES=100000)

    # gzip integrity: a truncated stream is a refusal, not a short archive.
    def truncated(directory: Path) -> Path:
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / 'archive.tar.gz'
        good = gzip_bytes(b''.join(
            tar_member(name, data, typeflag, mode)
            for name, data, typeflag, mode in default_members()) + b'\x00' * 1024)
        path.write_bytes(good[:len(good) // 2])
        return path
    budget_case('gzip_truncated', truncated, 'gzip')

    # The bounded reader never asks the decoder for a caller-sized buffer.
    label = 'gzip_expansion_bomb/bounded_underlying_reads'
    directory = work / 'bounded-reads'
    directory.mkdir(parents=True, exist_ok=True)
    archive = build_archive(directory / 'archive.tar.gz')
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': archive.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    requested: list[int] = []
    original = gzip.GzipFile.read

    def recording(self, size=-1):
        requested.append(size)
        return original(self, size)
    gzip.GzipFile.read = recording
    try:
        with lowered(codex_pin, MAX_DECODED_BYTES=2048):
            try:
                codex_pin.preflight_archive(loaded, selected, archive)
            except codex_pin.Refusal:
                pass
    finally:
        gzip.GzipFile.read = original
    oversized = [size for size in requested
                 if size is None or size < 0 or size > codex_pin.READ_CHUNK_BYTES]
    if not requested:
        fail(label, 'the decoded stream was never read')
    elif oversized:
        fail(label, f'oversized decoded reads requested: {oversized[:5]}')
    else:
        report(label)

    # A declared giant size is refused before the body is read: the fixture is a
    # header claiming 1 GiB with no body at all.
    label = 'pax_single_header_bomb/no_body_allocated'
    directory = work / 'no-body'
    directory.mkdir(parents=True, exist_ok=True)
    path = directory / 'archive.tar.gz'
    path.write_bytes(gzip_bytes(tar_header('PaxHeaders/x', 1 << 30, b'x')))
    fixture_local = Fixture(directory / 'fixture', archives={
        **fixture.archives, 'linux/amd64': path.read_bytes()})
    loaded = fixture_local.loaded()
    selected = codex_pin.select(loaded, RELEASE, codex_pin.Target.AMD64)
    started = time.monotonic()
    expect_refusal(label, lambda: codex_pin.preflight_archive(loaded, selected, path),
                   'extended header of')
    if time.monotonic() - started > 10:
        fail(label, 'the refusal took longer than a bounded read allows')


# --- writer / staging --------------------------------------------------------

# The fixture closure's `codex_pin.py` is a checked-in adapter -- a fixture file,
# never a string in this table -- that loads the real loader and adapts exactly
# one seam: a fixture `bin/codex` is a recording shell script, not an ELF, and the
# adapter accepts a `#!/bin/sh` header. The marker below therefore has to keep
# that prefix; `stage_valid_both_platforms` proves the adapter accepts it through
# the real preflight. No production switch exists for any of this.
CODEX_FIXTURES = REPO_ROOT / 'script' / 'test' / 'fixtures' / 'codex'
ADAPTER_FIXTURE = CODEX_FIXTURES / 'codex_pin_adapter.py'
COSIGN_STUB = CODEX_FIXTURES / 'cosign-stub.py'
FIXTURE_SCRIPT_MARKER: bytes = b'#!/bin/sh\n# fixture\n'
assert FIXTURE_SCRIPT_MARKER.startswith(b'#!/bin/sh\n')
assert len(FIXTURE_SCRIPT_MARKER) == 20


def write_installer_fixture(directory: Path, release: str, target: str,
                            native_script: Path, package_version: str | None,
                            package_target: str | None) -> None:
    """A prefixless package whose `bin/codex` is a recording shell script.

    The archive layout is the real one (four explicit top-level names, no `.`
    member), the package metadata is real, and the pin/evidence hashes are
    recomputed from these exact bytes — only the entrypoint's ELF machine check is
    adapted, because a fixture script is not an ELF.
    """
    release = codex_pin.require_release(release)
    version = release[len('rust-v'):]
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    body = Path(native_script).read_bytes()
    script = FIXTURE_SCRIPT_MARKER + body
    archives: dict[str, bytes] = {}
    for platform, platform_target in TARGETS.items():
        document = package_document(package_version or version,
                                   package_target or platform_target)
        members = [
            ('bin', b'', tarfile.DIRTYPE, 0o755),
            ('bin/codex', script, tarfile.REGTYPE, 0o755),
            ('bin/codex-code-mode-host', b'#!/bin/sh\nexit 0\n', tarfile.REGTYPE, 0o755),
            ('codex-package.json', json.dumps(document).encode(), tarfile.REGTYPE, 0o644),
            ('codex-path', b'', tarfile.DIRTYPE, 0o755),
            ('codex-path/rg', b'#!/bin/sh\nexit 0\n', tarfile.REGTYPE, 0o755),
            ('codex-resources', b'', tarfile.DIRTYPE, 0o755),
            ('codex-resources/bwrap', b'#!/bin/sh\nexit 0\n', tarfile.REGTYPE, 0o755),
        ]
        raw = b''.join(tar_member(*member) for member in members) + b'\x00' * 1024
        archives[platform] = gzip_bytes(raw)
    fixture = Fixture(directory / 'source', release=release, archives=archives)
    selected = next(platform for platform, name in TARGETS.items() if name == target)
    # Both payloads are exported under their canonical asset names, so a test can
    # answer the writer's exact URLs from a directory without re-deriving
    # anything. `archive.tgz` stays the selected platform for the vendored suite.
    assets = directory / 'assets'
    assets.mkdir(parents=True, exist_ok=True)
    for platform, name in TARGETS.items():
        asset = f'codex-package-{name}.tar.gz'
        (assets / asset).write_bytes(archives[platform])
        (assets / f'{asset}.sigstore').write_bytes(fixture.bundles[platform])
    (assets / 'release.json').write_bytes(fixture.metadata)
    (assets / 'codex-package_SHA256SUMS').write_bytes(fixture.checksums)
    (directory / 'archive.tgz').write_bytes(archives[selected])
    # The checked-in adapter, copied under the name production code imports, plus
    # its explicit sidecar: the one varying value (where the real loader lives).
    (directory / 'source' / 'codex_pin.py').write_bytes(ADAPTER_FIXTURE.read_bytes())
    (directory / 'source' / 'codex-pin-adapter.json').write_text(
        json.dumps({'module': str(CODEX_DIR / 'codex_pin.py')}) + '\n')
    del fixture


DOCKERFILE_FIXTURE = (
    'FROM scratch\n'
    f'ARG AGENT_VERSION_CODEX={RELEASE}\n'
    'LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"\n'
    'ARG AGENT_VERSION_OPENCODE=v2.0.29\n'
    'ARG AGENT_VERSION_CLAUDE=2.1.286\n'
    'ARG AGENT_VERSION_COPILOT=1.0.90\n'
)


class FixtureRepo:
    """A minimal repository root that the real writer can operate on."""

    def __init__(self, root: Path) -> None:
        self.root = Path(root)
        shutil.rmtree(self.root, ignore_errors=True)
        codex = self.root / 'images' / 'tools' / 'codex'
        codex.mkdir(parents=True)
        (self.root / 'images' / 'standard').mkdir(parents=True)
        (self.root / 'script' / 'dev').mkdir(parents=True)
        # ONE loader instance for the whole table: the fixture repository is
        # handed the same module object the cases assert against, so a refusal
        # raised by the real loader is the refusal class the table catches.
        pin_codex._modules[(codex / 'codex_pin.py').resolve()] = codex_pin
        shutil.copyfile(REPO_ROOT / 'script' / 'dev' / 'pin-artifact.py',
                        self.root / 'script' / 'dev' / 'pin-artifact.py')
        shutil.copyfile(REPO_ROOT / 'script' / 'dev' / 'pin-codex.py',
                        self.root / 'script' / 'dev' / 'pin-codex.py')
        # The shared publisher imports the reviewed artifact model, so the fixture
        # repository needs the real package present (unused by Codex).
        shutil.copytree(REPO_ROOT / 'images' / 'artifacts',
                        self.root / 'images' / 'artifacts', dirs_exist_ok=True)
        self.dockerfile = self.root / 'images' / 'standard' / 'Dockerfile'
        self.dockerfile.write_text(DOCKERFILE_FIXTURE)
        self.pin_directory = codex

    def install(self, fixture: Fixture) -> None:
        for relative in ('pin.json', 'evidence/release.json',
                         'evidence/codex-package_SHA256SUMS'):
            destination = self.root / 'images' / 'tools' / 'codex' / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes((fixture.directory / relative).read_bytes())
        for platform, target in TARGETS.items():
            name = f'evidence/codex-package-{target}.tar.gz.sigstore'
            destination = self.root / 'images' / 'tools' / 'codex' / name
            destination.write_bytes((fixture.directory / name).read_bytes())

    def snapshot(self) -> dict[str, bytes]:
        return {str(path.relative_to(self.root)): path.read_bytes()
                for path in sorted(self.root.rglob('*'))
                if path.is_file() and '__pycache__' not in path.parts}

    def module(self):
        return pin_codex.codex_pin(self.root)


def fake_fetch(served: Mapping[str, bytes], calls: list):
    def fetch(url: str, output: Path, limit: int) -> None:
        calls.append(url)
        raw = served.get(url)
        if raw is None:
            raise pin_codex.Refusal(f'cannot fetch {url}: HTTP 404 (fixture)')
        if len(raw) > limit:
            raise pin_codex.Refusal(f'{url} is larger than the {limit} byte ceiling')
        Path(output).write_bytes(raw)
    return fetch


def served_urls(fixture: Fixture, *, release: str = RELEASE,
                archives: Mapping[str, bytes] | None = None) -> dict[str, bytes]:
    archives = archives or fixture.archives
    module = pin_codex.codex_pin(REPO_ROOT)
    served = {
        module.api_url(release): fixture.metadata,
        module.asset_url(release, 'codex-package_SHA256SUMS'): fixture.checksums,
    }
    for platform, target in TARGETS.items():
        asset = f'codex-package-{target}.tar.gz'
        served[module.asset_url(release, asset)] = archives[platform]
        served[module.asset_url(release, f'{asset}.sigstore')] = \
            fixture.bundles[platform]
    return served


class Verifier:
    def __init__(self, *, error: Exception | None = None) -> None:
        self.calls: list[tuple] = []
        self.error = error

    def __call__(self, archive, bundle, identity, issuer, work) -> None:
        self.calls.append((Path(archive).name, Path(bundle).name, identity, issuer, Path(work)))
        if self.error is not None:
            raise self.error


def _stage(repo: FixtureRepo, work: Path, fixture: Fixture, verifier, *,
           served: Mapping[str, bytes] | None = None, release: str = RELEASE):
    module = repo.module()
    calls: list = []
    staged = pin_codex.stage_codex(
        release, repo.root, work,
        fetch=fake_fetch(served or served_urls(fixture, release=release), calls),
        verify=verifier)
    return staged, calls


def case_staging(work: Path) -> None:
    fixture = Fixture(work / 'fixture-stage')

    label = 'stage_valid_both_platforms'
    repo = FixtureRepo(work / 'repo-valid')
    verifier = Verifier()
    try:
        staged, calls = _stage(repo, work / 'stage-valid', fixture, verifier)
    except Exception as error:  # noqa: BLE001 - reported, not swallowed
        fail(label, error)
    else:
        expected_files = {
            'images/tools/codex/pin.json',
            'images/tools/codex/evidence/release.json',
            'images/tools/codex/evidence/codex-package_SHA256SUMS',
            'images/tools/codex/evidence/codex-package-x86_64-unknown-linux-musl'
            '.tar.gz.sigstore',
            'images/tools/codex/evidence/codex-package-aarch64-unknown-linux-musl'
            '.tar.gz.sigstore',
            'images/standard/Dockerfile',
        }
        # Both architectures fetched exactly once each, and both verified under
        # the exact signer policy.
        module = pin_codex.codex_pin(REPO_ROOT)
        payload_calls = [url for url in calls if 'download' in url and 'sigstore' not in url
                         and 'SHA256SUMS' not in url]
        if set(staged.files) != expected_files:
            fail(label, sorted(staged.files))
        elif len(verifier.calls) != 2:
            fail(label, f'{len(verifier.calls)} verification calls')
        elif payload_calls != [module.asset_url(RELEASE,
                                                f'codex-package-{TARGETS[p]}.tar.gz')
                               for p in ('linux/amd64', 'linux/arm64')]:
            fail(label, payload_calls)
        elif {call[2] for call in verifier.calls} != {module.identity(RELEASE)}:
            fail(label, verifier.calls)
        elif {call[3] for call in verifier.calls} != {module.ISSUER}:
            fail(label, verifier.calls)
        else:
            probe = FixtureRepo(work / 'repo-valid-probe')
            pin_codex.write_stage(work / 'stage-valid-pin', staged)
            try:
                pin_codex.codex_pin(probe.root).load_pin(work / 'stage-valid-pin')
            except Exception as error:  # noqa: BLE001
                fail(label, f'the staged proposal does not load: {error}')
            else:
                report(label)

    label = 'stage_bad_second_arch'
    repo = FixtureRepo(work / 'repo-second-arch')
    repo.install(fixture)
    before = repo.snapshot()
    archives = dict(fixture.archives)
    archives['linux/arm64'] = archives['linux/arm64'][:-10] + b'0123456789'
    try:
        _stage(repo, work / 'stage-bad-second', fixture,
               Verifier(error=pin_codex.Refusal('cosign refused')),
               served=served_urls(fixture, archives=archives))
    except REFUSALS:
        if repo.snapshot() != before:
            fail(label, 'the failed staging changed the repository')
        else:
            report(label)
    else:
        fail(label, 'a failing second architecture staged a proposal')

    label = 'stage_missing_bundle'
    repo = FixtureRepo(work / 'repo-missing-bundle')
    served = served_urls(fixture)
    module = pin_codex.codex_pin(REPO_ROOT)
    del served[module.asset_url(
        RELEASE, 'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore')]
    try:
        _stage(repo, work / 'stage-missing-bundle', fixture, Verifier(), served=served)
    except REFUSALS as error:
        if '404' not in str(error):
            fail(label, error)
        else:
            report(label)
    else:
        fail(label, 'a missing bundle staged a proposal')

    label = 'stage_checksum_disagreement'
    repo = FixtureRepo(work / 'repo-checksum')
    broken = Fixture(work / 'fixture-checksum', archives=fixture.archives)
    broken.checksums = make_checksums({
        'codex-package-x86_64-unknown-linux-musl.tar.gz': '0' * 64,
        'codex-package-aarch64-unknown-linux-musl.tar.gz': '0' * 64})
    try:
        _stage(repo, work / 'stage-checksum', broken, Verifier(),
               served=served_urls(broken))
    except REFUSALS as error:
        if 'SHA256SUMS' not in str(error):
            fail(label, error)
        else:
            report(label)
    else:
        fail(label, 'a checksum disagreement staged a proposal')

    label = 'stage_verifier_nonzero'
    repo = FixtureRepo(work / 'repo-verifier')
    repo.install(fixture)
    before = repo.snapshot()
    try:
        _stage(repo, work / 'stage-verifier', fixture,
               Verifier(error=pin_codex.Refusal('cosign refused the committed bundle')))
    except REFUSALS:
        if repo.snapshot() != before:
            fail(label, 'a refusing verifier changed the repository')
        else:
            report(label)
    else:
        fail(label, 'a refusing verifier staged a proposal')

    label = 'stage_ci_refusal'
    repo = FixtureRepo(work / 'repo-ci')
    before = repo.snapshot()
    saved = os.environ.get('GITHUB_ACTIONS')
    os.environ['GITHUB_ACTIONS'] = 'true'
    saved_err = sys.stderr
    sys.stderr = io.StringIO()
    try:
        with guard():
            status = pin_codex.main(['--version', RELEASE, '--repo-root', str(repo.root),
                                     '--cosign', '/nonexistent/cosign'])
        stderr = sys.stderr.getvalue()
    finally:
        sys.stderr = saved_err
        if saved is None:
            os.environ.pop('GITHUB_ACTIONS', None)
        else:
            os.environ['GITHUB_ACTIONS'] = saved
    if status != 1 or 'GitHub Actions' not in stderr or repo.snapshot() != before:
        fail(label, f'{status}: {stderr}')
    else:
        report(label)

    label = 'stage_partial_old_state'
    repo = FixtureRepo(work / 'repo-partial')
    (repo.pin_directory / 'evidence').mkdir(parents=True, exist_ok=True)
    (repo.pin_directory / 'evidence' / 'release.json').write_bytes(b'{}\n')
    try:
        _stage(repo, work / 'stage-partial', fixture, Verifier())
    except REFUSALS as error:
        if 'partial old state' not in str(error):
            fail(label, error)
        else:
            report(label)
    else:
        fail(label, 'a partial old state staged a proposal')

    # Same release, changed payload: refused, never re-pinned.
    label = 'stage_same_release_rewrite'
    repo = FixtureRepo(work / 'repo-rewrite')
    repo.install(fixture)
    before = repo.snapshot()
    # A *valid* package with different bytes: the rewrite must be refused by the
    # committed identity, not by a later preflight accident.
    rewrite_dir = work / 'fixture-rewrite-archive'
    rewrite_dir.mkdir(parents=True, exist_ok=True)
    rewritten_archive = build_archive(
        rewrite_dir / 'archive.tar.gz',
        members=default_members() + [
            ('codex-resources/voice', b'', tarfile.DIRTYPE, 0o755),
            ('codex-resources/voice/note.txt', b'rewritten\n', tarfile.REGTYPE, 0o644)])
    rewritten = Fixture(work / 'fixture-rewrite',
                        archives={**fixture.archives,
                                  'linux/amd64': rewritten_archive.read_bytes()})
    try:
        _stage(repo, work / 'stage-rewrite', rewritten, Verifier(),
               served=served_urls(rewritten))
    except REFUSALS as error:
        if 'changed for the already-pinned' not in str(error):
            fail(label, error)
        elif repo.snapshot() != before:
            fail(label, 'the refusal changed the repository')
        else:
            report(label)
    else:
        fail(label, 'a same-release payload rewrite was re-pinned')

    # Same release, same identities: byte-identical, original date preserved.
    label = 'stage_repeat_unchanged'
    repo = FixtureRepo(work / 'repo-repeat')
    repo.install(fixture)
    verifier = Verifier()
    try:
        staged, _calls = _stage(repo, work / 'stage-repeat', fixture, verifier)
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        original = {relative: (repo.root / relative).read_bytes()
                    for relative in staged.files}
        if not pin_codex.unchanged(repo.root, staged):
            fail(label, 'the repeat proposal is not byte-identical')
        elif original['images/tools/codex/pin.json'] != \
                staged.files['images/tools/codex/pin.json']:
            fail(label, 'the repeat proposal rewrote the pin')
        else:
            report(label)

    # The same release with only mutable metadata fields changed is a repeat, not
    # an upstream rewrite.
    label = 'stage_metadata_counter_only_change'
    repo = FixtureRepo(work / 'repo-counter')
    repo.install(fixture)
    recountered = Fixture(work / 'fixture-counter')
    recountered.metadata = make_metadata(_metadata_digests(recountered),
                                         download_count=99999)
    try:
        staged, _calls = _stage(repo, work / 'stage-counter', recountered, Verifier(),
                                served=served_urls(recountered))
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        committed = (repo.root / 'images/tools/codex/evidence/release.json').read_bytes()
        if not pin_codex.unchanged(repo.root, staged):
            fail(label, 'a download-count-only change is not a repeat')
        elif staged.files['images/tools/codex/evidence/release.json'] != committed:
            fail(label, 'the repeat replaced the committed raw metadata')
        else:
            report(label)

    # A changed security projection at the same tag is refused.
    label = 'stage_same_tag_identity_change'
    repo = FixtureRepo(work / 'repo-identity')
    repo.install(fixture)
    shifted = Fixture(work / 'fixture-identity')
    document = json.loads(shifted.metadata)
    document['assets'].append({'name': 'codex-decoy-asset',
                               'digest': 'sha256:' + '1' * 64})
    shifted.metadata = (json.dumps(document) + '\n').encode()
    try:
        _stage(repo, work / 'stage-identity', shifted, Verifier(),
               served=served_urls(shifted))
    except REFUSALS as error:
        if 'security projection' not in str(error):
            fail(label, error)
        else:
            report(label)
    else:
        fail(label, 'a same-tag identity change was accepted')


def case_verifier(work: Path) -> None:
    label = 'stage_verifier_argv_environment'
    directory = work / 'verifier-argv'
    directory.mkdir(parents=True, exist_ok=True)
    cosign = directory / 'cosign'
    # A checked-in fixture executable, not a program embedded in this table: the
    # child observes its own argv and environment, so it has to be a real file.
    shutil.copyfile(COSIGN_STUB, cosign)
    cosign.chmod(0o755)
    archive = directory / 'payload.tar.gz'
    archive.write_bytes(b'payload')
    bundle = directory / 'payload.bundle'
    bundle.write_bytes(b'bundle')
    verify = pin_codex.cosign_blob_verifier(cosign)
    try:
        verify(archive, bundle, 'ident', 'issuer', directory / 'run')
    except REFUSALS as error:
        fail(label, error)
        return
    # The observation is written into the private HOME the writer constructed for
    # the child, which is exactly the point: nothing is inherited from this test.
    record = directory / 'run' / 'home' / 'observations.jsonl'
    if not record.is_file():
        fail(label, f'the verifier was not run with the constructed environment: {record}')
        return
    recorded = json.loads(record.read_text().splitlines()[0])
    expected = ['verify-blob', '--bundle', str(bundle), '--certificate-identity', 'ident',
                '--certificate-oidc-issuer', 'issuer', str(archive)]
    if recorded['argv'] != expected:
        fail(label, recorded['argv'])
    elif recorded['env'] != ['HOME', 'LC_ALL', 'PATH', 'XDG_CACHE_HOME']:
        fail(label, recorded['env'])
    elif recorded['inherited_trust']:
        fail(label, f'inherited trust configuration: {recorded["inherited_trust"]}')
    else:
        home = directory / 'run' / 'home'
        if not home.is_dir():
            fail(label, f'the private HOME was not created: {home}')
        else:
            report(label)

    label = 'stage_verifier_nonzero_exit_is_refused'
    failing = directory / 'cosign-fail'
    failing.write_text('#!/bin/sh\nexit 1\n')
    failing.chmod(0o755)
    expect_refusal(label, lambda: pin_codex.cosign_blob_verifier(failing)(
        archive, bundle, 'ident', 'issuer', directory / 'run2'), 'cosign refused')

    label = 'stage_verifier_timeout'
    hanging = directory / 'cosign-hang'
    hanging.write_text('#!/bin/sh\nexec sleep 60\n')
    hanging.chmod(0o755)
    saved = pin_codex.VERIFIER_TIMEOUT_SECONDS
    pin_codex.VERIFIER_TIMEOUT_SECONDS = 1
    started = time.monotonic()
    try:
        expect_refusal(label, lambda: pin_codex.cosign_blob_verifier(hanging)(
            archive, bundle, 'ident', 'issuer', directory / 'run3'), 'did not finish')
    finally:
        pin_codex.VERIFIER_TIMEOUT_SECONDS = saved
    if time.monotonic() - started > 15:
        fail('stage_verifier_timeout/bounded', 'the bounded run was not bounded')

    label = 'stage_wrong_identity'
    repo = FixtureRepo(work / 'repo-wrong-identity')
    wrong = Verifier(error=pin_codex.Refusal('the bundle does not verify'))

    def wrong_identity() -> None:
        module = repo.module()
        original = module.identity

        def shifted(release: str) -> str:
            return original(release).replace('rust-release.yml', 'other.yml')
        module.identity = shifted
        try:
            _stage(repo, work / 'stage-wrong-identity', Fixture(work / 'fixture-wrong'),
                   wrong)
        finally:
            module.identity = original
    expect_refusal(label, wrong_identity, 'does not verify')
    if len(wrong.calls) != 1:
        fail(label, f'the verifier was called {len(wrong.calls)} times before refusing')

    label = 'cosign_version_is_enforced'
    repo = FixtureRepo(work / 'repo-cosign-version')
    other_dir = directory / 'other'
    other_dir.mkdir(parents=True, exist_ok=True)
    other = other_dir / 'cosign'
    shutil.copyfile(COSIGN_STUB, other)
    other.chmod(0o755)
    # The same executable reports an incompatible version when asked to, so the
    # refusal is proved without a second copy of the stub.
    (other_dir / 'cosign-version').write_text('v2.2.0\n')
    expect_refusal(label, lambda: pin_codex.require_cosign(other, repo.root),
                   'reviewed verifier is v2.4.1')
    expect_refusal('cosign_probe_refuses_other_version',
                   lambda: pin_codex.probe_cosign(other, repo.root),
                   'reviewed verifier is v2.4.1')
    if pin_codex.probe_cosign(cosign, repo.root) == 0:
        report('cosign_probe_accepts_reviewed_version')
    else:
        fail('cosign_probe_accepts_reviewed_version', 'a reviewed verifier was refused')


class FakeSocket:
    """The socket the transport clamps before every read."""

    def __init__(self) -> None:
        self.timeouts: list[float] = []

    def settimeout(self, seconds: float) -> None:
        self.timeouts.append(seconds)


class FakeResponse:
    """One HTTP response whose reads behave exactly as the test asks.

    `read1` is the single-chunk primitive the production transport must use;
    counting its calls is how the table proves a trickling peer cannot pin one
    read open. `timeout` is what a stalled socket raises when the clamped budget
    runs out.
    """

    def __init__(self, chunks, *, error=None, delay=0.0, timeout_after=None):
        self.chunks = list(chunks)
        self.error = error
        self.delay = delay
        self.timeout_after = timeout_after
        self.closed = False
        self.read_calls = 0
        self.read1_calls = 0
        self.socket = FakeSocket()

    def _serve(self) -> bytes:
        if self.delay:
            time.sleep(self.delay)
        if self.error is not None:
            raise self.error
        if self.timeout_after is not None and self.read1_calls > self.timeout_after:
            raise socket.timeout('timed out')
        if not self.chunks:
            return b''
        return self.chunks.pop(0)

    def read(self, size=-1):
        self.read_calls += 1
        return self._serve()

    def read1(self, size=-1):
        self.read1_calls += 1
        return self._serve()

    @property
    def fp(self):
        # The `http.client` shape the transport clamps: fp.raw._sock.
        class _Raw:
            _sock = self.socket

        class _Fp:
            raw = _Raw()

        return _Fp()

    def __enter__(self):
        return self

    def __exit__(self, *_exception):
        self.closed = True
        return False


class FakeOpener:
    """Answers `open` with the queued responses, or with a `_Follow`."""

    def __init__(self, *answers):
        self.answers = list(answers)
        self.timeouts: list[float] = []
        self.urls: list[str] = []

    def open(self, request, timeout=None):
        self.timeouts.append(timeout)
        self.urls.append(request.full_url)
        answer = self.answers.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer


def with_opener(*answers):
    """Patch `build_opener`; returns the opener and a restore callable."""
    opener = FakeOpener(*answers)
    saved = pin_codex.urllib.request.build_opener
    pin_codex.urllib.request.build_opener = lambda *_a, **_k: opener

    def restore() -> None:
        pin_codex.urllib.request.build_opener = saved
    return opener, restore


def fetch_with_budget(budget: float, *answers):
    """Run one `https_fetch` under `answers`; returns (elapsed, failure, opener)."""
    saved = pin_codex.FETCH_TIMEOUT_SECONDS
    pin_codex.FETCH_TIMEOUT_SECONDS = budget
    opener, restore = with_opener(*answers)
    started = time.monotonic()
    failure = None
    try:
        pin_codex.https_fetch(URL, OUTPUT, 4096)
    except Exception as error:  # noqa: BLE001 - the case reports what happened
        failure = error
    finally:
        restore()
        pin_codex.FETCH_TIMEOUT_SECONDS = saved
    return time.monotonic() - started, failure, opener


URL = 'https://github.com/openai/codex/releases/download/x/y.tar.gz'
BASE = ''                        # the loopback origin, set by the socket case
CDN_URL = ('https://release-assets.githubusercontent.com/x/y.tar.gz'
           '?sig=REVIEW-SIGNED-SECRET&token=REVIEW-TOKEN')
OUTPUT = Path(os.devnull)          # replaced per case, see case_transport


def case_transport(work: Path) -> None:
    global OUTPUT
    directory = work / 'transport'
    directory.mkdir(parents=True, exist_ok=True)
    OUTPUT = directory / 'out.bin'
    output = OUTPUT

    label = 'transport_limits'
    path_limit = 16
    _opener, restore = with_opener(FakeResponse([b'x' * (path_limit + 1)]))
    try:
        expect_refusal(label,
                       lambda: pin_codex.https_fetch(URL, output, path_limit),
                       'byte ceiling')
    finally:
        restore()
    if output.exists() and output.stat().st_size > path_limit:
        fail('transport_limits/written', 'the over-limit body reached the output file')

    label = 'transport_truncated'
    _opener, restore = with_opener(
        FakeResponse([], error=OSError(errno.ECONNRESET, 'reset')))
    try:
        expect_refusal(label, lambda: pin_codex.https_fetch(URL, output, 1024),
                       'cannot read')
    finally:
        restore()

    label = 'transport_http_error'
    error = urllib.error.HTTPError(URL, 404, 'Not Found', {}, None)
    _opener, restore = with_opener(error)
    try:
        expect_refusal(label, lambda: pin_codex.https_fetch(URL, output, 1024),
                       'HTTP 404')
    finally:
        restore()

    label = 'transport_origin_refused'
    expect_refusal(label,
                   lambda: pin_codex.https_fetch('https://evil.example/x', output, 16),
                   'not on a GitHub origin')

    # --- the whole-request deadline -------------------------------------
    # A blocking read is what the old code could not interrupt: it checked the
    # budget only after a read returned, so a peer dripping bytes (or one that
    # never finished a 64 KiB read) held the request far past it. Each case below
    # therefore asserts ELAPSED TIME, not merely that a refusal happened.
    label = 'transport_deadline_drip'
    drip = FakeResponse([b'a'] * 40, delay=0.05)
    elapsed, failure, opener = fetch_with_budget(0.2, drip)
    if not isinstance(failure, REFUSALS):
        fail(label, f'the drip was not refused: {failure!r}')
    elif 'deadline expired' not in str(failure):
        fail(label, failure)
    elif elapsed > 2:
        fail(label, f'the drip held the request for {elapsed:.2f}s on a 0.2s budget')
    elif drip.read_calls:
        fail(label, 'the transport used `read` (which loops until the whole size '
                    f'arrives), not one socket-sized chunk ({drip.read_calls} reads)')
    elif drip.read1_calls < 2:
        fail(label, f'the transport made {drip.read1_calls} single-chunk reads; the '
                    'budget was checked between chunks at most once')
    else:
        report(label)

    label = 'transport_deadline_stalled_read'
    # A socket that never delivers: the clamped timeout is the only thing that
    # can end it, and the refusal must name the deadline.
    elapsed, failure, _opener = fetch_with_budget(
        0.2, FakeResponse([b'a'] * 40, timeout_after=0))
    if not isinstance(failure, REFUSALS):
        fail(label, f'a stalled read was not refused: {failure!r}')
    elif 'deadline expired' not in str(failure):
        fail(label, failure)
    elif elapsed > 2:
        fail(label, f'a stalled read held the request for {elapsed:.2f}s')
    else:
        report(label)

    label = 'transport_deadline_eof_after_budget'
    # End-of-body does not extend the budget: the reviewer's probe was an EOF
    # arriving after the budget had already expired, which the old code accepted.
    # The watchdog now ends the request AT the budget instead of at the late EOF,
    # which is the whole point of it -- so the assertion is that the refusal
    # arrives near the budget, not 0.35 s later when the EOF does.
    elapsed, failure, _opener = fetch_with_budget(
        0.05, FakeResponse([], delay=0.4))
    if not isinstance(failure, REFUSALS):
        fail(label, f'an EOF after the budget was accepted: {failure!r}')
    elif 'deadline expired' not in str(failure):
        fail(label, failure)
    elif elapsed >= 0.3:
        fail(label, f'the request waited {elapsed:.2f}s for a late EOF on a 0.05s budget')
    elif output.exists() and output.stat().st_size:
        fail(label, f'a late EOF still published {output.stat().st_size} bytes')
    else:
        report(label)

    label = 'transport_deadline_eof_check_without_watchdog'
    # The inner check is a second, independent layer: with the watchdog replaced
    # by a no-op the late EOF is still a refusal, not a small file.
    class _NoWatchdog:
        def __init__(self, _seconds):
            pass

        def __enter__(self):
            return self

        def __exit__(self, *_exception):
            return None

    saved_watchdog = pin_codex._RequestWatchdog
    pin_codex._RequestWatchdog = _NoWatchdog
    try:
        elapsed, failure, _opener = fetch_with_budget(
            0.05, FakeResponse([], delay=0.4))
    finally:
        pin_codex._RequestWatchdog = saved_watchdog
    if not isinstance(failure, REFUSALS):
        fail(label, f'an EOF after the budget was accepted: {failure!r}')
    elif 'deadline expired' not in str(failure):
        fail(label, failure)
    elif elapsed < 0.3:
        fail(label, f'the case did not reach the EOF ({elapsed:.2f}s)')
    else:
        report(label)

    label = 'transport_watchdog_is_armed_for_the_whole_request'
    # The connection itself is covered: an `open` that never returns is ended by
    # the alarm, not by a socket timeout that only exists after it returns.
    def stalled_open(_self, _request, timeout=None):
        assert timeout is not None
        time.sleep(2)
        raise OSError('the connect never returned')

    saved_open = FakeOpener.open
    FakeOpener.open = stalled_open
    try:
        elapsed, failure, _opener = fetch_with_budget(0.1, FakeResponse([]))
    finally:
        FakeOpener.open = saved_open
    if not isinstance(failure, REFUSALS):
        fail(label, f'a stalled connect was not refused: {failure!r}')
    elif 'deadline expired' not in str(failure):
        fail(label, failure)
    elif elapsed > 1:
        fail(label, f'a stalled connect held the request for {elapsed:.2f}s on a 0.1s '
                    'budget')
    else:
        report(label)

    label = 'transport_watchdog_restores_the_previous_handler_and_timer'
    # An inner caller's alarm must survive the fetch: the fetch disarms its own
    # timer, hands the handler back and re-arms what was left of the outer one.
    fired = []

    def outer(_signum, _frame):
        fired.append('outer')

    saved_handler = signal.getsignal(signal.SIGALRM)
    saved_timer = signal.setitimer(signal.ITIMER_REAL, 0)
    signal.signal(signal.SIGALRM, outer)
    signal.setitimer(signal.ITIMER_REAL, 30)
    try:
        _opener, restore = with_opener(FakeResponse([b'payload']))
        try:
            pin_codex.https_fetch(URL, OUTPUT, 1024)
        finally:
            restore()
        handler_after = signal.getsignal(signal.SIGALRM)
        remaining, interval = signal.setitimer(signal.ITIMER_REAL, 0)
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, saved_handler)
        if saved_timer[0] > 0 or saved_timer[1] > 0:
            signal.setitimer(signal.ITIMER_REAL, *saved_timer)
    if handler_after is not outer:
        fail(label, f'the caller\'s SIGALRM handler was not restored: {handler_after!r}')
    elif not remaining or remaining < 25 or interval != 0:
        fail(label, f'the caller\'s timer was not restored (remaining={remaining}, '
                    f'interval={interval})')
    elif fired:
        fail(label, 'the caller\'s alarm fired during the fetch')
    else:
        report(label)

    label = 'transport_deadline_socket_timeout_is_remaining'
    response = FakeResponse([b'a'] * 40, delay=0.05)
    _elapsed, _failure, _opener = fetch_with_budget(0.2, response)
    if not response.socket.timeouts:
        fail(label, 'the transport never clamped the socket timeout')
    elif min(response.socket.timeouts) <= 0 or max(response.socket.timeouts) > 0.25:
        fail(label, f'socket timeouts outside the budget: {response.socket.timeouts}')
    else:
        report(label)

    # --- redirects -------------------------------------------------------
    label = 'transport_follows_approved_redirect'
    redirect_response = FakeResponse([b'payload'])
    opener, restore = with_opener(pin_codex._Follow(CDN_URL), redirect_response)
    try:
        pin_codex.https_fetch(URL, output, 1024)
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        if output.read_bytes() != b'payload':
            fail(label, output.read_bytes())
        elif opener.urls != [URL, CDN_URL]:
            fail(label, opener.urls)
        else:
            report(label)
    finally:
        restore()

    label = 'transport_redirect_hop_bounded'
    # Every hop is opened under what is LEFT of the budget, not the first hop's
    # timeout: urllib would reuse the first timeout for the whole chain.
    _opener, restore = with_opener(pin_codex._Follow(CDN_URL), FakeResponse([b'p']))
    saved = pin_codex.FETCH_TIMEOUT_SECONDS
    pin_codex.FETCH_TIMEOUT_SECONDS = 30
    try:
        pin_codex.https_fetch(URL, output, 1024)
        timeouts = _opener.timeouts
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        if len(timeouts) != 2 or any(value is None or value > 30 for value in timeouts):
            fail(label, timeouts)
        else:
            report(label)
    finally:
        pin_codex.FETCH_TIMEOUT_SECONDS = saved
        restore()

    label = 'transport_too_many_redirects'
    hops = [pin_codex._Follow(CDN_URL)] * (pin_codex.MAX_REDIRECTS + 1)
    _opener, restore = with_opener(*hops)
    try:
        expect_refusal(label, lambda: pin_codex.https_fetch(URL, output, 1024),
                       'redirected more than')
    finally:
        restore()

    handler = pin_codex._GithubRedirects()

    def redirect(target: str):
        return lambda: handler.redirect_request(None, None, 302, 'Found', {}, target)

    for name, target, fragment in [
            ('non_https', 'http://release-assets.githubusercontent.com/x', 'non-HTTPS'),
            ('wrong_host', 'https://evil.example/x', 'refusing a redirect'),
            ('credentials', 'https://u:p@release-assets.githubusercontent.com/x',
             'refusing a redirect'),
            ('port', 'https://release-assets.githubusercontent.com:444/x',
             'refusing a redirect')]:
        expect_refusal(f'transport_redirect_refused/{name}', redirect(target), fragment)

    label = 'transport_redirect_approved'
    try:
        handler.redirect_request(None, None, 302, 'Found', {}, CDN_URL)
    except pin_codex._Follow as follow:
        if follow.url != CDN_URL:
            fail(label, follow.url)
        else:
            report(label)
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        fail(label, 'an approved CDN redirect was not handed back to the fetch loop')


# --- writer process: ownership, CI boundary, recovery ------------------------

def _run_writer(argv: list[str], work: Path, *, offline: bool = False,
                keep_ci: bool = False) -> tuple[int, str]:
    """One in-process `pin-codex.py` run with its own TMPDIR and stderr.

    `offline` also trips any network or subprocess attempt, which is how the cases
    prove a refusal happened before either. `keep_ci` leaves GitHub Actions set
    for the case that exercises the writer's CI refusal; every other case is a
    maintainer-host run, so the CI variable the test runner inherits must not
    leak into it (CI runs this suite with GITHUB_ACTIONS=true).
    """
    saved_tmp = os.environ.get('TMPDIR')
    os.environ['TMPDIR'] = str(work)
    saved_ci = None if keep_ci else os.environ.pop('GITHUB_ACTIONS', None)
    Path(work).mkdir(parents=True, exist_ok=True)
    captured = io.StringIO()
    saved_err = sys.stderr
    sys.stderr = captured
    try:
        if offline:
            with guard():
                status = pin_codex.main(argv)
        else:
            status = pin_codex.main(argv)
    except SystemExit as exit_:
        status = int(exit_.code or 0)
    finally:
        sys.stderr = saved_err
        if saved_tmp is None:
            os.environ.pop('TMPDIR', None)
        else:
            os.environ['TMPDIR'] = saved_tmp
        if saved_ci is not None:
            os.environ['GITHUB_ACTIONS'] = saved_ci
    return status, captured.getvalue()


def _checked_in_cosign(directory: Path) -> Path:
    """The fixture verifier, executable, reporting the reviewed version."""
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    cosign = directory / 'cosign'
    shutil.copyfile(COSIGN_STUB, cosign)
    cosign.chmod(0o755)
    return cosign


def case_writer_process(work: Path) -> None:
    fixture = Fixture(work / 'fixture-writer')

    # --- the staging directory belongs to the caller, not to this tool ------
    label = 'stage_dir_refuses_non_empty'
    repo = FixtureRepo(work / 'repo-own')
    staged, _calls = _stage(repo, work / 'stage-own', fixture, Verifier())
    sentinel = work / 'stage-sentinel' / 'unrelated.txt'
    sentinel.parent.mkdir(parents=True, exist_ok=True)
    sentinel.parent.chmod(0o700)
    sentinel.write_text('do not delete me\n')
    before = sentinel.read_bytes()
    try:
        pin_codex.write_stage(sentinel.parent, staged)
    except REFUSALS as error:
        if 'not empty' not in str(error):
            fail(label, error)
        elif sentinel.read_bytes() != before:
            fail(label, 'the sentinel was modified')
        else:
            report(label)
    else:
        fail(label, 'a non-empty staging directory was erased and reused')

    label = 'stage_dir_refuses_a_shared_directory'
    # Validation and writing are the same operation on the same inode; a directory
    # another user can reach is not a staging area, so it is refused before any
    # file is created.
    shared = work / 'stage-shared'
    shared.mkdir(parents=True, exist_ok=True)
    shared.chmod(0o755)
    try:
        pin_codex.write_stage(shared, staged)
    except REFUSALS as error:
        if 'private to this user' not in str(error):
            fail(label, error)
        elif list(shared.iterdir()):
            fail(label, f'a refused directory received files: {list(shared.iterdir())}')
        else:
            report(label)
    else:
        fail(label, 'a 0755 staging directory was written to')

    label = 'stage_dir_refuses_symlink'
    target = work / 'stage-symlink-target'
    target.mkdir(parents=True, exist_ok=True)
    target.chmod(0o700)
    link = work / 'stage-symlink'
    link.symlink_to(target, target_is_directory=True)
    expect_refusal(label, lambda: pin_codex.write_stage(link, staged), 'symlink')
    if list(target.iterdir()):
        fail(label + '/target_untouched', f'the symlink target received files: '
                                          f'{list(target.iterdir())}')
    else:
        report(label + '/target_untouched')

    label = 'stage_dir_swapped_for_a_symlink_never_writes_the_victim'
    # The reviewer's race, made deterministic: the checked, empty directory is
    # moved away and replaced by a symlink to a directory holding an unrelated
    # victim file, between the emptiness check and the first write. The writes are
    # anchored to the descriptor of the directory that was checked, so the victim
    # must be untouched -- and the swap must still be reported.
    victim = work / 'stage-victim'
    victim.mkdir(parents=True, exist_ok=True)
    (victim / 'pin.json').write_text('victim bytes\n')
    (victim / 'evidence').mkdir(exist_ok=True)
    victim_before = {path.name: path.read_bytes() for path in victim.rglob('*')
                     if path.is_file()}
    swapped = work / 'stage-swap'
    swapped.mkdir(parents=True, exist_ok=True)
    swapped.chmod(0o700)
    moved = work / 'stage-swap-moved'
    real_write = pin_codex._write_stage_file
    armed = [True]

    def swap_then_write(directory_fd, name, raw):
        if armed[0]:
            armed[0] = False
            swapped.rename(moved)
            swapped.symlink_to(victim, target_is_directory=True)
        return real_write(directory_fd, name, raw)

    pin_codex._write_stage_file = swap_then_write
    try:
        try:
            pin_codex.write_stage(swapped, staged)
        except REFUSALS as error:
            failure = error
        except Exception as error:  # noqa: BLE001
            failure = error
        else:
            failure = None
    finally:
        pin_codex._write_stage_file = real_write
    victim_after = {path.name: path.read_bytes() for path in victim.rglob('*')
                    if path.is_file()}
    if failure is None:
        fail(label, 'a staging directory swapped for a symlink was accepted')
    elif victim_after != victim_before:
        fail(label, f'the victim directory was overwritten: {sorted(victim_after)}')
    elif not any(name in str(failure) for name in ('replaced', 'symlink')):
        fail(label, failure)
    elif not (moved / 'pin.json').is_file():
        fail(label, f'the writes did not stay anchored to the checked directory: '
                    f'{sorted(path.name for path in moved.rglob("*")) if moved.exists() else "gone"}')
    else:
        report(label)

    label = 'stage_dir_created_when_absent'
    fresh = work / 'stage-fresh' / 'nested'
    try:
        pin_codex.write_stage(fresh, staged)
    except Exception as error:  # noqa: BLE001
        fail(label, error)
    else:
        written = sorted(str(path.relative_to(fresh)) for path in fresh.rglob('*')
                         if path.is_file())
        expected = sorted(['pin.json', 'Dockerfile', 'evidence/release.json',
                           'evidence/codex-package_SHA256SUMS',
                           'evidence/codex-package-x86_64-unknown-linux-musl'
                           '.tar.gz.sigstore',
                           'evidence/codex-package-aarch64-unknown-linux-musl'
                           '.tar.gz.sigstore'])
        if written != expected:
            fail(label, written)
        else:
            report(label)

    label = 'stage_dir_via_main_keeps_the_callers_files'
    repo = FixtureRepo(work / 'repo-own-main')
    before_repo = repo.snapshot()
    blocked = work / 'stage-main' / 'keep.txt'
    blocked.parent.mkdir(parents=True, exist_ok=True)
    blocked.write_text('mine\n')
    cosign = _checked_in_cosign(work / 'own-cosign')
    saved_fetch = pin_codex.https_fetch
    saved_verify = pin_codex.cosign_blob_verifier
    pin_codex.https_fetch = fake_fetch(served_urls(fixture), [])
    pin_codex.cosign_blob_verifier = lambda _cosign: Verifier()
    try:
        status, stderr = _run_writer(
            ['--version', RELEASE, '--repo-root', str(repo.root),
             '--cosign', str(cosign), '--stage-dir', str(blocked.parent)],
            work / 'tmp-main')
    finally:
        pin_codex.https_fetch = saved_fetch
        pin_codex.cosign_blob_verifier = saved_verify
    if status != 1:
        fail(label, f'exit {status}: {stderr}')
    elif blocked.read_text() != 'mine\n':
        fail(label, 'the staged run erased an unrelated file')
    elif repo.snapshot() != before_repo:
        fail(label, 'a refused staging changed the repository')
    else:
        report(label)

    # --- the CI boundary is before every write mode ------------------------
    label = 'publish_stage_refuses_ci'
    repo = FixtureRepo(work / 'repo-publish-ci')
    staged, _calls = _stage(repo, work / 'stage-publish-ci', fixture, Verifier())
    stage_dir = work / 'publish-ci'
    pin_codex.write_stage(stage_dir, staged)
    before_repo = repo.snapshot()
    saved = os.environ.get('GITHUB_ACTIONS')
    os.environ['GITHUB_ACTIONS'] = 'true'
    try:
        # `offline` also proves the refusal precedes any subprocess or socket.
        status, stderr = _run_writer(
            ['--publish-stage', str(stage_dir), '--repo-root', str(repo.root)],
            work / 'tmp-publish-ci', offline=True, keep_ci=True)
    finally:
        if saved is None:
            os.environ.pop('GITHUB_ACTIONS', None)
        else:
            os.environ['GITHUB_ACTIONS'] = saved
    if status != 1:
        fail(label, f'exit {status}: {stderr}')
    elif 'GitHub Actions' not in stderr:
        fail(label, stderr)
    elif repo.snapshot() != before_repo:
        fail(label, 'a refused CI publication changed the repository')
    else:
        report(label)

    label = 'probe_cosign_is_read_only'
    repo = FixtureRepo(work / 'repo-probe')
    before_repo = repo.snapshot()
    status, stderr = _run_writer(
        ['--probe-cosign', str(_checked_in_cosign(work / 'probe-cosign')),
         '--repo-root', str(repo.root)], work / 'tmp-probe')
    if status != 0:
        fail(label, f'exit {status}: {stderr}')
    elif repo.snapshot() != before_repo:
        fail(label, 'the probe wrote to the repository')
    else:
        report(label)

    # --- the published path is absolute, whatever the caller typed ---------
    label = 'publish_stage_prints_an_absolute_proposal_path'
    repo = FixtureRepo(work / 'repo-publish-abs')
    staged, _calls = _stage(repo, work / 'stage-publish-abs', fixture, Verifier())
    pin_codex.write_stage(work / 'publish-abs', staged)
    before_repo = repo.snapshot()
    saved_cwd = os.getcwd()
    os.chdir(work)
    try:
        # A relative argument is exactly what a caller writes by hand; the path it
        # is told to recover from must still resolve from anywhere.
        status, stderr = _run_writer(
            ['--publish-stage', 'publish-abs', '--repo-root', str(repo.root)],
            work / 'tmp-publish-abs')
    finally:
        os.chdir(saved_cwd)
    printed = re.findall(r'publishing the staged proposal in (.+)', stderr)
    if status != 0:
        fail(label, f'exit {status}: {stderr}')
    elif not printed:
        fail(label, f'no proposal path was printed: {stderr}')
    elif not printed[-1].startswith('/') or not Path(printed[-1]).is_dir():
        fail(label, f'the printed proposal path is not an absolute directory: '
                    f'{printed[-1]!r}')
    elif repo.snapshot() == before_repo:
        fail(label, 'the publication reported success and changed nothing')
    else:
        report(label)

    label = 'publish_stage_refuses_a_swapped_proposal_directory'
    # The published bytes are read through descriptors opened from the directory
    # itself, so replacing it with a symlink to a different (plausible) proposal
    # cannot publish that one.
    repo = FixtureRepo(work / 'repo-publish-swap')
    staged, _calls = _stage(repo, work / 'stage-publish-swap', fixture, Verifier())
    swapped = work / 'publish-swap'
    pin_codex.write_stage(swapped, staged)
    victim = work / 'publish-swap-victim'
    shutil.copytree(swapped, victim)
    (victim / 'pin.json').write_text('a different proposal\n')
    victim_before = {str(path.relative_to(victim)): path.read_bytes()
                     for path in victim.rglob('*') if path.is_file()}
    before_repo = repo.snapshot()
    moved = work / 'publish-swap-moved'
    swapped.rename(moved)
    swapped.symlink_to(victim, target_is_directory=True)
    status, stderr = _run_writer(
        ['--publish-stage', str(swapped), '--repo-root', str(repo.root)],
        work / 'tmp-publish-swap')
    victim_after = {str(path.relative_to(victim)): path.read_bytes()
                    for path in victim.rglob('*') if path.is_file()}
    if status != 1:
        fail(label, f'exit {status}: {stderr}')
    elif repo.snapshot() != before_repo:
        fail(label, 'a proposal reached through a symlink was published')
    elif victim_after != victim_before:
        fail(label, 'the proposal behind the symlink was modified')
    elif 'symlink' not in stderr:
        fail(label, stderr)
    else:
        report(label)

    # --- a status-3 publication keeps this invocation's scratch -----------
    label = 'standalone_recovery_retains_owner_scratch'
    bumped = Fixture(work / 'fixture-bumped', release=NEXT_RELEASE)
    repo = FixtureRepo(work / 'repo-recovery')
    repo.install(fixture)
    publisher = pin_codex.publisher_module(repo.root)
    real_copyfile = publisher.shutil.copyfile
    saved_restore = publisher.Publisher.restore
    armed = [True]

    def copyfile(source, destination, **kwargs):
        destination = Path(destination)
        if armed[0] and destination.name == 'release.json' and 'staged' in Path(source).parts:
            armed[0] = False
            destination.write_text('half-written\n')
            raise OSError('injected write failure')
        return real_copyfile(source, destination, **kwargs)

    def refuse_restore(_self):
        raise OSError('injected restore failure')

    saved_fetch = pin_codex.https_fetch
    saved_verify = pin_codex.cosign_blob_verifier
    cosign = _checked_in_cosign(work / 'recovery-cosign')
    scratch = work / 'tmp-recovery'
    pin_codex.https_fetch = fake_fetch(
        served_urls(bumped, release=NEXT_RELEASE), [])
    pin_codex.cosign_blob_verifier = lambda _cosign: Verifier()
    publisher.shutil.copyfile = copyfile
    publisher.Publisher.restore = refuse_restore
    try:
        status, stderr = _run_writer(
            ['--version', NEXT_RELEASE, '--repo-root', str(repo.root),
             '--cosign', str(cosign)], scratch)
    finally:
        pin_codex.https_fetch = saved_fetch
        pin_codex.cosign_blob_verifier = saved_verify
        publisher.shutil.copyfile = real_copyfile
        publisher.Publisher.restore = saved_restore
    retained = re.findall(r'owner scratch retained at (.+)', stderr)
    proposals = re.findall(r'staged proposal retained at (.+)', stderr)
    backups = re.findall(r'recovery material retained at (.+)', stderr)
    if status != 3:
        fail(label, f'exit {status}: {stderr}')
    elif not retained or not backups or not proposals:
        fail(label, f'the retained directories were not all printed: {stderr}')
    else:
        owner = Path(retained[-1].strip())
        kept = Path(backups[-1].strip())
        proposal = Path(proposals[-1].strip())
        missing = [name for name in ('pin.json', 'Dockerfile', 'evidence/release.json')
                   if not (proposal / name).is_file() or not (owner / 'publish' / name)
                   .is_file()]
        numbered = sorted(path.name for path in (kept / 'backup').iterdir()
                          if path.name != '.complete')
        if missing:
            fail(label, f'the retained owner scratch lost {missing}: {owner}')
        elif not (kept / 'backup' / '.complete').is_file():
            fail(label, f'the retained publisher backup is incomplete: {kept}')
        elif numbered != ['0', '1', '2', '3', '4', '5']:
            fail(label, f'the retained publisher backup lost numbered copies: {numbered}')
        else:
            report(label)
        # This case owns what it retained; nothing outside the case needs it.
        shutil.rmtree(owner, ignore_errors=True)
        shutil.rmtree(kept, ignore_errors=True)

    # --- a real SIGINT reaches os._exit(3): both absolute paths are printed --
    label = 'standalone_signal_recovery_prints_both_absolute_paths'
    # The publisher leaves through `os._exit(3)` from its signal handler, so the
    # owner's own post-return reporting never runs. This case sends a real SIGINT
    # from inside publication with a refusing restore and requires BOTH absolute
    # directories -- the publisher's work directory and the owner scratch -- on
    # stderr, in a real subprocess (an in-process run would end the table).
    repos = FixtureRepo(work / 'repo-signal')
    repos.install(fixture)
    # The fixture loader the writer imports in a child process is the checked-in
    # adapter beside its sidecar, exactly as the owner suite builds it.
    shutil.copyfile(ADAPTER_FIXTURE, repos.pin_directory / 'codex_pin.py')
    (repos.pin_directory / 'codex-pin-adapter.json').write_text(
        json.dumps({'module': str(CODEX_DIR / 'codex_pin.py')}) + '\n')
    assets = work / 'signal-assets'
    (work / 'signal-native.sh').write_text('printf "codex-cli 0.159.4\\n"\n')
    write_installer_fixture(assets, NEXT_RELEASE, TARGETS['linux/amd64'],
                            work / 'signal-native.sh', None, None)
    signal_tmp = work / 'tmp-signal'
    signal_tmp.mkdir(parents=True, exist_ok=True)
    environment = dict(os.environ)
    environment.pop('GITHUB_ACTIONS', None)
    environment.update({
        'FIXTURE_CODEX_WRITER': str(REPO_ROOT / 'script' / 'dev' / 'pin-codex.py'),
        'FIXTURE_CODEX_ASSETS': str(assets / 'assets'),
        'FIXTURE_CODEX_INJECT': 'signal',
        'FIXTURE_CODEX_INJECT_AT':
            'codex-package-aarch64-unknown-linux-musl.tar.gz.sigstore',
        'FIXTURE_CODEX_REFUSE_RESTORE': '1',
        'TMPDIR': str(signal_tmp),
    })
    finished = subprocess.run(
        [sys.executable, str(REPO_ROOT / 'script' / 'test' / 'fixtures'
                            / 'agent-versions' / 'codex-writer.py'),
         '--version', NEXT_RELEASE, '--repo-root', str(repos.root),
         '--cosign', str(_checked_in_cosign(work / 'signal-cosign'))],
        capture_output=True, text=True, env=environment)
    owners = re.findall(r'owner scratch retained at (/\S+)', finished.stderr)
    works = re.findall(r'recovery material retained at (/\S+)', finished.stderr)
    if finished.returncode != 3:
        fail(label, f'exit {finished.returncode}: {finished.stderr}')
    elif not owners or not works:
        fail(label, 'both absolute directories were not printed by the signal path: '
                    + finished.stderr.strip())
    elif not Path(owners[-1]).is_dir() or not Path(works[-1]).is_dir():
        fail(label, f'a printed directory is gone: {owners[-1]} {works[-1]}')
    elif not (Path(owners[-1]) / 'publish' / 'pin.json').is_file():
        fail(label, f'the owner scratch no longer holds the staged proposal: '
                    f'{owners[-1]}')
    elif not (Path(works[-1]) / 'backup' / '.complete').is_file():
        fail(label, f'the retained publisher directory has no complete backup: '
                    f'{works[-1]}')
    else:
        report(label)
        shutil.rmtree(owners[-1], ignore_errors=True)
        shutil.rmtree(works[-1], ignore_errors=True)

    # --- the fixture programs are real, runnable files ---------------------
    label = 'fixture_adapter_runs_on_its_own'
    source = work / 'adapter-source'
    (work / 'adapter-native.sh').write_text('printf "codex-cli 0.159.3\\n"\n')
    write_installer_fixture(source, RELEASE, TARGETS['linux/amd64'],
                            work / 'adapter-native.sh', None, None)
    finished = subprocess.run(
        [sys.executable, str(source / 'source' / 'codex_pin.py'), 'select',
         '--pin-dir', str(source / 'source'), '--release', RELEASE,
         '--target', TARGETS['linux/amd64']],
        capture_output=True, text=True)
    fields = finished.stdout.strip().splitlines()
    if finished.returncode != 0:
        fail(label, finished.stderr.strip())
    elif len(fields) != 4:
        fail(label, f'{len(fields)} fields: {finished.stdout!r}')
    else:
        report(label)

    label = 'fixture_cosign_stub_runs_on_its_own'
    stub = _checked_in_cosign(work / 'cosign-standalone')
    finished = subprocess.run([str(stub), 'version', '--json'],
                              capture_output=True, text=True)
    if finished.returncode != 0 or 'v2.4.1' not in finished.stdout:
        fail(label, finished.stderr.strip() or finished.stdout)
    else:
        report(label)


# --- the shared checksum parser ---------------------------------------------

def case_checksum_parser(work: Path) -> None:
    """One grammar, two callers: the loader and the writer must agree."""
    label = 'checksum_parser_accepts_a_canonical_manifest'
    manifest = b'a' * 64 + b'  codex-package-x86_64-unknown-linux-musl.tar.gz\n'
    rows = codex_pin.parse_checksums(manifest, 'committed checksums')
    if [(row.digest, row.name) for row in rows] != [
            ('a' * 64, 'codex-package-x86_64-unknown-linux-musl.tar.gz')]:
        fail(label, rows)
    else:
        report(label)

    for name, manifest, fragment in [
            ('duplicate', (b'a' * 64 + b'  same.tar.gz\n') * 2, 'repeat the entry'),
            ('unreadable', b'not a checksum line\n', 'unreadable line'),
            ('not_utf8', b'\xff\xfe\n', 'not UTF-8')]:
        label = f'checksum_parser_refuses_{name}'
        expect_refusal(label,
                       lambda manifest=manifest: codex_pin.parse_checksums(
                           manifest, 'committed checksums'),
                       fragment)
        # The writer path must refuse the same bytes with the same reason: this
        # is the regression lock for two parsers that could drift apart.
        writer_label = f'writer_checksums_refuse_{name}'
        expect_refusal(writer_label,
                       lambda manifest=manifest: pin_codex._require_checksums_cover(
                           codex_pin, manifest, {}),
                       fragment)


# --- the transport against a real socket ------------------------------------

def case_transport_sockets(work: Path) -> None:
    """The same transport over loopback sockets, with no network.

    The injected-response cases above cannot prove two things: that urllib
    propagates this module's approved redirect out of its own error chain, and
    that a real socket honours the clamped budget. Both are proved here against a
    loopback server: scheme/host validation is unit-tested above and is the one
    thing patched out, because a loopback origin is not `https://github.com`.
    """
    directory = work / 'transport-sockets'
    directory.mkdir(parents=True, exist_ok=True)
    output = directory / 'out.bin'

    class Handler(http.server.BaseHTTPRequestHandler):
        protocol_version = 'HTTP/1.1'

        def log_message(self, *_args):  # keep the case output clean
            return

        def do_GET(self):  # noqa: N802 - http.server's spelling
            if self.path.startswith('/payload'):
                self.send_response(302)
                self.send_header('Location', f'{BASE}/asset?sig=REVIEW-SIGNED-SECRET')
                self.send_header('Content-Length', '0')
                self.end_headers()
                return
            if self.path.startswith('/asset'):
                body = b'live-payload'
                self.send_response(200)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            if self.path.startswith('/drip-headers'):
                # A status line, then header BYTES that never end the header block.
                # Every recv() returns well inside any inactivity timeout, so no
                # socket timeout can end this: only a whole-request alarm can.
                try:
                    self.wfile.write(b'HTTP/1.1 200 OK\r\n')
                    self.wfile.flush()
                    for _ in range(200):
                        self.wfile.write(b'X-Drip: payload')
                        self.wfile.flush()
                        time.sleep(0.02)
                except OSError:
                    return
                return
            if self.path.startswith('/drip-chunk'):
                # The same trick one layer deeper: `read1` parses the chunk-size
                # line inside the call that would otherwise be one socket read, and
                # that line never terminates.
                try:
                    self.send_response(200)
                    self.send_header('Transfer-Encoding', 'chunked')
                    self.end_headers()
                    for _ in range(200):
                        self.wfile.write(b'1')
                        self.wfile.flush()
                        time.sleep(0.02)
                except OSError:
                    return
                return
            if self.path.startswith('/drip'):
                self.send_response(200)
                self.send_header('Content-Length', '10000')
                self.end_headers()
                try:
                    for _ in range(10000):
                        self.wfile.write(b'x')
                        self.wfile.flush()
                        time.sleep(0.02)
                except OSError:
                    # The client hanging up mid-body is the case under test.
                    return
                return
            self.send_response(404)
            self.send_header('Content-Length', '0')
            self.end_headers()

    class QuietServer(http.server.ThreadingHTTPServer):
        daemon_threads = True

        def handle_error(self, request, client_address):
            # A client that hangs up mid-body is the case under test, not a fault.
            return

    server = QuietServer(('127.0.0.1', 0), Handler)
    global BASE
    BASE = f'http://127.0.0.1:{server.server_port}'
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    saved_origin = pin_codex._require_origin
    saved_redirects = pin_codex._GithubRedirects

    class PermissiveRedirect(pin_codex._GithubRedirects):
        """The production handler with only the https/host rules removed."""

        def redirect_request(self, req, fp, code, msg, headers, newurl):
            try:
                fp.close()
            except (AttributeError, OSError):
                pass
            raise pin_codex._Follow(newurl)

    pin_codex._require_origin = lambda url: urllib.parse.urlsplit(url)
    try:
        label = 'transport_socket_follows_approved_redirect'
        pin_codex._GithubRedirects = PermissiveRedirect
        try:
            pin_codex.https_fetch(f'{BASE}/payload', output, 1024)
        except Exception as error:  # noqa: BLE001
            fail(label, error)
        else:
            if output.read_bytes() != b'live-payload':
                fail(label, output.read_bytes())
            else:
                report(label)

        label = 'transport_socket_stalled_drip_is_bounded'
        pin_codex._GithubRedirects = saved_redirects
        saved = pin_codex.FETCH_TIMEOUT_SECONDS
        pin_codex.FETCH_TIMEOUT_SECONDS = 0.3
        started = time.monotonic()
        try:
            pin_codex.https_fetch(f'{BASE}/drip', output, 1 << 20)
        except REFUSALS as error:
            elapsed = time.monotonic() - started
            if 'deadline expired' not in str(error):
                fail(label, error)
            elif elapsed > 3:
                fail(label, f'a dripping socket held the request for {elapsed:.2f}s '
                            'on a 0.3s budget')
            else:
                report(label)
        except Exception as error:  # noqa: BLE001
            fail(label, error)
        else:
            fail(label, 'a dripping socket was accepted')
        finally:
            pin_codex.FETCH_TIMEOUT_SECONDS = saved

        # The reviewer's two probes, as cases: a 0.1 s budget must end a request
        # whose peer drips header bytes, and one that drips chunk-size framing.
        # Neither can be ended by an inactivity timeout, because every recv()
        # returns; both used to hold the request ~0.7 s (and indefinitely if the
        # peer keeps dripping).
        for label, path, budget, ceiling in (
                ('transport_socket_dripping_headers_is_bounded',
                 '/drip-headers', 0.1, 0.6),
                ('transport_socket_dripping_chunk_framing_is_bounded',
                 '/drip-chunk', 0.1, 0.6)):
            pin_codex.FETCH_TIMEOUT_SECONDS = budget
            started = time.monotonic()
            try:
                pin_codex.https_fetch(f'{BASE}{path}', output, 1 << 20)
            except REFUSALS as error:
                elapsed = time.monotonic() - started
                if 'deadline expired' not in str(error):
                    fail(label, error)
                elif elapsed > ceiling:
                    fail(label, f'the request held for {elapsed:.2f}s on a '
                                f'{budget:g}s budget')
                else:
                    report(label)
            except Exception as error:  # noqa: BLE001
                fail(label, error)
            else:
                fail(label, 'a dripping peer was accepted')
            finally:
                pin_codex.FETCH_TIMEOUT_SECONDS = saved
    finally:
        pin_codex._require_origin = saved_origin
        pin_codex._GithubRedirects = saved_redirects
        server.shutdown()
        server.server_close()


# --- real signatures (opt-in) ------------------------------------------------

def case_real_signatures(work: Path, archive_dir: Path, cosign: Path) -> None:
    """The real reviewed verifier, both architectures, positives and negatives.

    This is the opt-in evidence mode, never part of the hermetic aggregate: it
    needs the retained upstream payloads, the Committed bundles and network access
    to Sigstore. Two passes on purpose -- every positive first, then every
    negative -- because the swapped-bundle control needs BOTH architectures
    loaded, and an in-loop control silently skipped one direction.
    """
    label = 'real_signatures/committed_pin'
    try:
        pin = codex_pin.load_pin(CODEX_DIR)
    except codex_pin.Refusal as error:
        fail(label, error)
        return
    report(label)

    label = 'real_signatures/cosign_version'
    try:
        # The production prerequisite, not a printout: a verifier that is not
        # exactly the reviewed one makes every result below meaningless.
        pin_codex.require_cosign(Path(cosign), REPO_ROOT)
    except REFUSALS as error:
        fail(label, error)
        return
    report(f'{label} (v2.4.1)')
    print(f'  cosign: {cosign}')

    verify = pin_codex.cosign_blob_verifier(Path(cosign))
    identity = pin.authentication.identity
    issuer = pin.authentication.issuer
    payloads: dict[str, tuple[Path, Path]] = {}

    # --- pass 1: every retained payload verifies --------------------------
    for platform, platform_pin in pin.platforms.items():
        asset = platform_pin.asset
        source = Path(archive_dir) / asset
        label = f'real_signatures/retained_payload/{platform.value}'
        if not source.is_file():
            fail(label, f'missing {source}')
            continue
        target = work / asset
        shutil.copyfile(source, target)
        if sha256(target.read_bytes()) != platform_pin.digest.value:
            fail(label, 'the retained payload does not match the committed digest')
            continue
        report(label)
        bundle = work / codex_pin.bundle_name(platform_pin.target)
        shutil.copyfile(CODEX_DIR / 'evidence' / codex_pin.bundle_name(platform_pin.target),
                        bundle)
        payloads[platform.value] = (target, bundle)
        label = f'real_signatures/positive/{platform.value}'
        try:
            verify(target, bundle, identity, issuer, work / f'real-{platform.name.lower()}')
        except REFUSALS as error:
            fail(label, error)
        else:
            report(label)

    # --- pass 2: every negative, for both architectures -------------------
    for platform, platform_pin in pin.platforms.items():
        if platform.value not in payloads:
            continue
        target, bundle = payloads[platform.value]
        stem = platform.name.lower()

        label = f'real_signatures/negative_changed_payload/{platform.value}'
        changed = work / f'changed-{stem}' / platform_pin.asset
        changed.parent.mkdir(parents=True, exist_ok=True)
        raw = bytearray(target.read_bytes())
        raw[len(raw) // 2] ^= 0xFF
        changed.write_bytes(bytes(raw))
        try:
            verify(changed, bundle, identity, issuer, work / f'real-changed-{stem}')
        except REFUSALS:
            report(label)
        else:
            fail(label, 'a changed payload byte verified')

        label = f'real_signatures/negative_changed_signature/{platform.value}'
        document = json.loads(bundle.read_bytes())
        decoded = bytearray(base64.b64decode(document['base64Signature'], validate=True))
        decoded[4] ^= 0xFF
        document['base64Signature'] = base64.b64encode(bytes(decoded)).decode()
        forged = work / f'forged-{stem}-{platform_pin.target.value}.sigstore'
        forged.write_bytes(json.dumps(document).encode())
        try:
            verify(target, forged, identity, issuer, work / f'real-forged-{stem}')
        except REFUSALS:
            report(label)
        else:
            fail(label, 'a changed decoded signature byte verified')

        label = f'real_signatures/negative_wrong_identity/{platform.value}'
        try:
            verify(target, bundle, identity.replace('rust-release.yml', 'other.yml'),
                   issuer, work / f'real-ident-{stem}')
        except REFUSALS:
            report(label)
        else:
            fail(label, 'a wrong workflow identity verified')

        label = f'real_signatures/negative_wrong_issuer/{platform.value}'
        try:
            verify(target, bundle, identity, 'https://accounts.google.com',
                   work / f'real-issuer-{stem}')
        except REFUSALS:
            report(label)
        else:
            fail(label, 'a wrong issuer verified')

        # The other architecture's bundle against this payload: it is a real,
        # trusted signature -- for different bytes. Both directions run, because
        # both payloads are loaded by the first pass.
        other = codex_pin.Platform.ARM64 if platform is codex_pin.Platform.AMD64 \
            else codex_pin.Platform.AMD64
        label = f'real_signatures/negative_other_arch_bundle/{platform.value}'
        if other.value not in payloads:
            fail(label, f'the {other.value} payload was not retained')
        else:
            other_bundle = work / f'other-{stem}-bundle'
            shutil.copyfile(CODEX_DIR / 'evidence' /
                            codex_pin.bundle_name(pin.platforms[other].target),
                            other_bundle)
            try:
                verify(target, other_bundle, identity, issuer, work / f'real-other-{stem}')
            except REFUSALS:
                report(label)
            else:
                fail(label, 'the other architecture bundle verified this payload')

        # A staged proposal whose evidence hash was recomputed for the changed
        # signature must still fail at cryptographic verification, not at a file
        # hash: the signature is the authority, not the file digest.
        label = f'real_signatures/negative_staged_signature/{platform.value}'
        staged = FixtureRepo(work / f'real-repo-{stem}')
        shutil.copytree(CODEX_DIR, staged.pin_directory, dirs_exist_ok=True)
        forged_target = staged.pin_directory / 'evidence' / codex_pin.bundle_name(
            platform_pin.target)
        forged_target.write_bytes(forged.read_bytes())
        staged_pin = json.loads((staged.pin_directory / 'pin.json').read_text())
        staged_pin['platforms'][platform.value]['bundle']['sha256'] = sha256(
            forged.read_bytes())
        metadata = json.loads((staged.pin_directory / 'evidence' /
                               'release.json').read_text())
        for entry in metadata['assets']:
            if entry['name'] == codex_pin.bundle_name(platform_pin.target):
                entry['digest'] = f'sha256:{sha256(forged.read_bytes())}'
        (staged.pin_directory / 'evidence' / 'release.json').write_text(
            json.dumps(metadata, indent=2) + '\n')
        staged_pin['metadata']['sha256'] = sha256(
            (staged.pin_directory / 'evidence' / 'release.json').read_bytes())
        (staged.pin_directory / 'pin.json').write_text(
            json.dumps(staged_pin, indent=2) + '\n')
        try:
            staged_module = pin_codex.codex_pin(staged.root)
            staged_module.load_pin(staged.pin_directory)
        except Exception as error:  # noqa: BLE001
            fail(label, f'the forged staged proposal does not load: {error}')
            continue
        try:
            verify(target, forged_target,
                   staged_pin['authentication']['certificate_identity'],
                   staged_pin['authentication']['certificate_oidc_issuer'],
                   work / f'real-staged-{stem}')
        except REFUSALS:
            report(label)
        else:
            fail(label, 'a recomputed-evidence forged signature verified')

    print(f'  real signatures: {len(payloads)}/{len(pin.platforms)} committed payloads '
          'verified with the reviewed verifier; every negative control was refused')


# --- driver ------------------------------------------------------------------

def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument('--real-signatures', action='store_true')
    parser.add_argument('--archive-dir')
    parser.add_argument('--cosign')
    # The shell suites need a valid synthetic closure on disk; generating it here
    # keeps one fixture model instead of a second parallel harness.
    parser.add_argument('--write-fixture', metavar='DIR')
    parser.add_argument('--write-installer-fixture', metavar='DIR')
    parser.add_argument('--release', default=RELEASE)
    parser.add_argument('--target', default=TARGETS['linux/amd64'])
    parser.add_argument('--native-script')
    parser.add_argument('--package-version')
    parser.add_argument('--package-target')
    arguments = parser.parse_args(argv)
    if arguments.write_installer_fixture is not None:
        if not arguments.native_script:
            print('codex-pins: --write-installer-fixture needs --native-script',
                  file=sys.stderr)
            return 2
        if arguments.target not in TARGETS.values():
            print(f'codex-pins: unknown target {arguments.target}', file=sys.stderr)
            return 2
        write_installer_fixture(Path(arguments.write_installer_fixture),
                                arguments.release, arguments.target,
                                Path(arguments.native_script),
                                arguments.package_version, arguments.package_target)
        print(f'codex-pins: wrote an installer fixture for {arguments.release} '
              f'{arguments.target}')
        return 0
    if arguments.write_fixture is not None:
        try:
            release = codex_pin.require_release(arguments.release)
        except codex_pin.Refusal as error:
            print(f'codex-pins: {error}', file=sys.stderr)
            return 2
        Fixture(Path(arguments.write_fixture), release=release)
        print(f'codex-pins: wrote a synthetic {release} closure in '
              f'{arguments.write_fixture}')
        return 0
    if arguments.real_signatures and (not arguments.archive_dir or not arguments.cosign):
        print('codex-pins: --real-signatures needs --archive-dir and --cosign',
              file=sys.stderr)
        return 2
    work = Path(tempfile.mkdtemp(prefix='codex-pins.')).resolve()
    try:
        for case in (case_loader, case_schema, case_urls, case_evidence,
                     case_corroboration, case_preflight, case_staging,
                     case_verifier, case_transport, case_transport_sockets,
                     case_writer_process, case_checksum_parser):
            name = work / case.__name__
            name.mkdir(parents=True, exist_ok=True)
            case(name)
        if arguments.real_signatures:
            directory = work / 'real-signatures'
            directory.mkdir(parents=True, exist_ok=True)
            case_real_signatures(directory, Path(arguments.archive_dir),
                                 Path(arguments.cosign))
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if failures:
        print(f'codex-pins: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('codex-pins: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
