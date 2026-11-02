#!/usr/bin/env python3
"""Fixture closures for the Claude native payload pin (#98).

One throwaway OpenPGP key, ELF-shaped executables and a consistent signed closure
(pin, manifest, detached signature, key, and a copy of the real `claude_pin.py`),
so the offline suites exercise the production loader over bytes they control. The
fixture never touches the real committed evidence and never reaches the network.

The fixture's gpg homes are deliberately separate from `claude_pin.gpg_home`: a
short transient 0700 home under `/tmp` for key generation, export, signing and
two-key assembly, removed by the invocation that created it. A defect in the
module under test must not be able to mask a fixture defect, and vice versa.
Every gpg exit status is checked.

CLI:

    claude_fixture.py keygen OUT [--subkey]
    claude_fixture.py elf OUT --machine 62|183 [--class 2] [--endian 1] [--size N]
    claude_fixture.py closure OUT --signer DIR --version V \\
        --amd64 FILE --arm64 FILE [--key FILE] [--fingerprint FPR] [--signature FILE]
    claude_fixture.py two-primaries OUT A B

`keygen` writes OUT/key.asc (armored public), OUT/secret.asc (armored secret) and
OUT/fingerprint (primary); with `--subkey` the primary is cert-only and the
signing subkey's fingerprint goes to OUT/signing-subkey. `closure` writes
OUT/pin.json, OUT/evidence/manifest.json, OUT/evidence/manifest.json.sig, the key
file and OUT/claude_pin.py. The same behaviour is importable:
`make_signer`, `sign`, `make_closure`, `two_primaries`.
"""
from __future__ import annotations

import argparse
import contextlib
import datetime
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

ROOT = Path(__file__).resolve().parents[4]
REAL_PIN_MODULE = ROOT / 'images' / 'tools' / 'claude' / 'claude_pin.py'
FIXTURE_UID = 'fixture claude signer <fixture@example.invalid>'
FIXTURE_REASON = ('Fixture closure: the manifest is the selection authority and '
                  'the detached signature is verified with the committed key.')
# Same rule as the module under test, deliberately a separate copy: gpg-agent's
# socket path limit makes a deep TMPDIR-derived home fail the key import.
GPG_HOME_ROOT = Path('/tmp')
GPG_HOME_PREFIX = 'claude-fixture-gpg.'
MAX_GPG_HOME_CHARS = 64
ELF_HEADER_BYTES = 20


@dataclass(frozen=True)
class Signer:
    directory: Path
    fingerprint: str
    signing_fingerprint: str
    key: Path
    secret: Path


@contextlib.contextmanager
def short_gnupghome() -> Iterator[dict]:
    try:
        home = Path(tempfile.mkdtemp(prefix=GPG_HOME_PREFIX, dir=GPG_HOME_ROOT))
    except OSError as error:
        raise SystemExit(f'claude_fixture: cannot create a gpg home under '
                         f'{GPG_HOME_ROOT}: {error}') from None
    environment = {'GNUPGHOME': str(home), 'LC_ALL': 'C',
                   'PATH': os.environ.get('PATH', '/usr/bin:/bin')}
    try:
        if len(str(home)) > MAX_GPG_HOME_CHARS:
            raise SystemExit(f'claude_fixture: the gpg home {home} is longer than '
                             f'{MAX_GPG_HOME_CHARS} characters')
        yield environment
    finally:
        _kill_agent(environment)
        shutil.rmtree(home, ignore_errors=True)


def _kill_agent(environment: dict) -> None:
    try:
        subprocess.run(['gpgconf', '--kill', 'all'], env=environment,
                       capture_output=True, text=True, check=False)
    except OSError:
        pass


def _gpg(argv: list[str], environment: dict, what: str) -> subprocess.CompletedProcess:
    try:
        completed = subprocess.run(['gpg', *argv], env=environment,
                                   capture_output=True, text=True, check=False)
    except OSError as error:
        raise SystemExit(f'claude_fixture: cannot run gpg for {what}: {error}') from None
    if completed.returncode != 0:
        raise SystemExit(f'claude_fixture: gpg {what} exited '
                         f'{completed.returncode}: {completed.stderr.strip()}')
    return completed


def _primaries(environment: dict) -> list[str]:
    listed = _gpg(['--batch', '--with-colons', '--list-keys'], environment,
                  'list keys').stdout
    primaries: list[str] = []
    pending = False
    for line in listed.splitlines():
        fields = line.split(':')
        if fields[0] == 'pub':
            pending = True
        elif fields[0] == 'fpr' and pending:
            primaries.append(fields[9].upper())
            pending = False
    return primaries


def _subkeys(environment: dict) -> list[str]:
    listed = _gpg(['--batch', '--with-colons', '--list-keys'], environment,
                  'list keys').stdout
    subkeys: list[str] = []
    pending = False
    for line in listed.splitlines():
        fields = line.split(':')
        if fields[0] in ('sub', 'ssb'):
            pending = True
        elif fields[0] == 'fpr' and pending:
            subkeys.append(fields[9].upper())
            pending = False
    return subkeys


def make_signer(out: Path, *, subkey: bool = False) -> Signer:
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    with short_gnupghome() as environment:
        usage = 'cert' if subkey else 'default'
        _gpg(['--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
              '--quick-generate-key', FIXTURE_UID, 'ed25519', usage, '0'],
             environment, 'generate the fixture key')
        primaries = _primaries(environment)
        if len(primaries) != 1:
            raise SystemExit(f'claude_fixture: {len(primaries)} primary keys after keygen')
        fingerprint = primaries[0]
        signing = fingerprint
        if subkey:
            _gpg(['--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
                  '--quick-add-key', fingerprint, 'ed25519', 'sign', '0'],
                 environment, 'add the signing subkey')
            subkeys = _subkeys(environment)
            if len(subkeys) != 1:
                raise SystemExit(f'claude_fixture: {len(subkeys)} subkeys after add-key')
            signing = subkeys[0]
        public = _gpg(['--batch', '--armor', '--export', fingerprint], environment,
                      'export the public key').stdout
        secret = _gpg(['--batch', '--armor', '--export-secret-keys', fingerprint],
                      environment, 'export the secret key').stdout
    key = out / 'key.asc'
    key.write_text(public)
    sealed = out / 'secret.asc'
    sealed.write_text(secret)
    (out / 'fingerprint').write_text(fingerprint + '\n')
    if subkey:
        (out / 'signing-subkey').write_text(signing + '\n')
    return Signer(directory=out, fingerprint=fingerprint, signing_fingerprint=signing,
                  key=key, secret=sealed)


def read_signer(directory: Path, *, subkey: bool = False) -> Signer:
    directory = Path(directory)
    fingerprint = (directory / 'fingerprint').read_text().strip()
    signing = fingerprint
    if subkey:
        signing = (directory / 'signing-subkey').read_text().strip()
    return Signer(directory=directory, fingerprint=fingerprint,
                  signing_fingerprint=signing, key=directory / 'key.asc',
                  secret=directory / 'secret.asc')


def sign(signer: Signer, data: Path, *, faked_time: int | None = None) -> bytes:
    """One detached OpenPGP signature over the bytes of `data`.

    `faked_time` moves the signature packet's creation time, which is how a suite
    produces a *different* but valid signature over the same bytes (EdDSA is
    deterministic, so re-signing at the same time would reproduce the bytes).
    """
    data = Path(data)
    with short_gnupghome() as environment:
        _gpg(['--batch', '--quiet', '--import', str(signer.secret)], environment,
             'import the fixture secret key')
        signature = Path(environment['GNUPGHOME']) / 'data.sig'
        extra = [f'--faked-system-time={int(faked_time)}!'] if faked_time else []
        _gpg(['--batch', '--yes', '--pinentry-mode', 'loopback', '--passphrase', '',
              *extra, '--detach-sign', '--output', str(signature), str(data)],
             environment, 'sign the fixture manifest')
        return signature.read_bytes()


def elf(path: Path, machine: int, *, elf_class: int = 2, endian: int = 1,
        size: int = 4096, marker: bytes = b'') -> Path:
    path = Path(path)
    order = 'little' if endian == 1 else 'big'
    header = bytearray(b'\x7fELF')
    # e_ident is 16 bytes: magic, class, data, version, osabi, abiversion, pad.
    header.extend(bytes([elf_class, endian, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0]))
    header.extend((2).to_bytes(2, order))
    header.extend(int(machine).to_bytes(2, order))
    header.extend(b'\x00' * (ELF_HEADER_BYTES - len(header)))
    path.parent.mkdir(parents=True, exist_ok=True)
    body = bytes(header) + marker
    path.write_bytes(body + b'\x00' * max(size - len(body), 0))
    return path


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def manifest_bytes(version: str, executables: dict[str, Path]) -> bytes:
    """The signed manifest's real shape, including platforms this pin ignores."""
    platforms = {}
    for token, path in executables.items():
        raw = Path(path).read_bytes()
        platforms[token] = {'binary': 'claude', 'checksum': _sha256(raw),
                            'size': len(raw)}
    document = {
        'version': version,
        'manifestSignatureEnforcement': 'flag',
        'commit': '0' * 40,
        'modsCommit': '1' * 40,
        'buildDate': '2026-09-30T15:55:40Z',
        'platforms': platforms,
        'sdkCompat': {'harnessSchema': 1, 'testedWrapperVersions': ['0.3.285']},
    }
    return json.dumps(document, indent=2).encode() + b'\n'


def make_closure(out: Path, *, signer: Signer, version: str, amd64: Path,
                 arm64: Path, key: Path | None = None,
                 fingerprint: str | None = None,
                 signature: Path | None = None,
                 reason: str = FIXTURE_REASON) -> Path:
    """Write one consistent closure into `out` and return it."""
    out = Path(out)
    out.mkdir(parents=True, exist_ok=True)
    evidence = out / 'evidence'
    evidence.mkdir(exist_ok=True)
    key_bytes = Path(key or signer.key).read_bytes()
    manifest = manifest_bytes(version, {'linux-x64': amd64, 'linux-arm64': arm64,
                                        'linux-x64-musl': amd64,
                                        'darwin-arm64': arm64})
    (evidence / 'manifest.json').write_bytes(manifest)
    # A caller may supply a signature made over *other* bytes: that is how the
    # suites build a closure whose pin and evidence digests are internally
    # consistent while the signature is stale.
    if signature is not None:
        detached = Path(signature).read_bytes()
    else:
        detached = sign(signer, evidence / 'manifest.json')
    (evidence / 'manifest.json.sig').write_bytes(detached)
    (out / 'claude-code-release-key.asc').write_bytes(key_bytes)
    pinned_amd64 = Path(amd64).read_bytes()
    pinned_arm64 = Path(arm64).read_bytes()
    base = 'https://downloads.claude.ai/claude-code-releases'
    document = {
        'schema': 1,
        'name': 'claude',
        'kind': 'claude-native-release',
        'version': version,
        'manifest': {'url': f'{base}/{version}/manifest.json',
                     'path': 'evidence/manifest.json',
                     'sha256': _sha256(manifest)},
        'signature': {'url': f'{base}/{version}/manifest.json.sig',
                      'path': 'evidence/manifest.json.sig',
                      'sha256': _sha256(detached)},
        'key': {'url': 'https://downloads.claude.ai/keys/claude-code.asc',
                'path': 'claude-code-release-key.asc',
                'fingerprint': (fingerprint or signer.fingerprint).upper(),
                'sha256': _sha256(key_bytes)},
        'platforms': {
            'linux/amd64': {'manifest_platform': 'linux-x64',
                            'url': f'{base}/{version}/linux-x64/claude',
                            'sha256': _sha256(pinned_amd64),
                            'size': len(pinned_amd64)},
            'linux/arm64': {'manifest_platform': 'linux-arm64',
                            'url': f'{base}/{version}/linux-arm64/claude',
                            'sha256': _sha256(pinned_arm64),
                            'size': len(pinned_arm64)},
        },
        'authentication': {'method': 'openpgp-signed-manifest',
                           # The fixture stamps what the writer stamps (the UTC
                           # date), or a maintainer east of UTC would build
                           # closures the reader refuses as future.
                           'verified_on': datetime.datetime.now(
                               datetime.timezone.utc).date().isoformat(),
                           'reason': reason},
    }
    (out / 'pin.json').write_text(json.dumps(document, indent=2) + '\n')
    shutil.copyfile(REAL_PIN_MODULE, out / 'claude_pin.py')
    return out


def two_primaries(out: Path, first: Path, second: Path) -> Path:
    out = Path(out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_bytes(Path(first).read_bytes() + Path(second).read_bytes())
    return out


# --- CLI ---------------------------------------------------------------------

def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(prog='claude_fixture.py')
    verbs = parser.add_subparsers(dest='verb', required=True)
    keygen = verbs.add_parser('keygen')
    keygen.add_argument('out')
    keygen.add_argument('--subkey', action='store_true')
    shape = verbs.add_parser('elf')
    shape.add_argument('out')
    shape.add_argument('--machine', type=int, required=True)
    shape.add_argument('--class', dest='elf_class', type=int, default=2)
    shape.add_argument('--endian', type=int, default=1)
    shape.add_argument('--size', type=int, default=4096)
    closure = verbs.add_parser('closure')
    closure.add_argument('out')
    closure.add_argument('--signer', required=True)
    closure.add_argument('--version', required=True)
    closure.add_argument('--amd64', required=True)
    closure.add_argument('--arm64', required=True)
    closure.add_argument('--key')
    closure.add_argument('--fingerprint')
    closure.add_argument('--signature')
    closure.add_argument('--subkey', action='store_true')
    both = verbs.add_parser('two-primaries')
    both.add_argument('out')
    both.add_argument('first')
    both.add_argument('second')
    arguments = parser.parse_args(argv)
    try:
        if arguments.verb == 'keygen':
            signer = make_signer(Path(arguments.out), subkey=arguments.subkey)
            print(signer.fingerprint)
            return 0
        if arguments.verb == 'elf':
            elf(Path(arguments.out), arguments.machine,
                elf_class=arguments.elf_class, endian=arguments.endian,
                size=arguments.size)
            return 0
        if arguments.verb == 'closure':
            signer = read_signer(Path(arguments.signer), subkey=arguments.subkey)
            make_closure(Path(arguments.out), signer=signer, version=arguments.version,
                         amd64=Path(arguments.amd64), arm64=Path(arguments.arm64),
                         key=Path(arguments.key) if arguments.key else None,
                         fingerprint=arguments.fingerprint,
                         signature=Path(arguments.signature)
                         if arguments.signature else None)
            return 0
        if arguments.verb == 'two-primaries':
            two_primaries(Path(arguments.out), Path(arguments.first),
                          Path(arguments.second))
            return 0
        return 2
    except SystemExit as error:
        print(f'claude_fixture: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
