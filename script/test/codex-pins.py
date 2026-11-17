#!/usr/bin/env python3
"""Black-box evidence for the committed Codex payload closure (#181).

Every case drives a real program. The reader cases run
`recipe-contract codex-pin check|select|preflight …` (`internal/codexpin` +
`internal/cli`), the writer cases run `script/dev/pin-codex.sh`
(`avm-pin codex`), and the closures they are pointed at are synthesized here:
real tar/gzip archives, real package metadata, real ELF headers, real evidence
digests. Nothing reaches the network, nothing runs Docker, no fixture binary is
executed and no committed evidence is touched.

Three seams are used, and no others:

* a mutated or freshly built closure, for everything the reader decides from
  committed bytes;
* `--fixture-upstream DIR`, which serves `https://host/path` from `DIR/host/path`,
  so the writer's whole request set is answered from a directory this table wrote
  (and, with one file removed, its refusals name the exact URL it asked for);
* an executable fixture cosign per case, which reports `{"gitVersion":"v2.4.1"}`,
  either accepts `verify-blob` or refuses it on demand, and can record the argv
  and environment it was handed. The reviewed version is a closed constant in the
  writer, so a fixture verifier is the only way to reach the "cosign refused"
  refusal without the host tool, and the child is the only observer of the
  environment production constructs for it.

The pin date is UTC on both sides, so the clock case runs the real writer as a
process under `TZ=Pacific/Kiritimati` and `TZ=Etc/GMT+12`: a host's local date only
exists inside a real zone, and it is exactly a local-date comparison that the UTC
pin date replaced. One of the two zones always has a local date other than the UTC
date, so the case is not time-of-day dependent.

There is no monkeypatching and no import of a production module: the programs
are binaries, so a case that cannot be expressed this way does not exist here.
The cases that used to patch production internals live in Go instead, and each
group has exactly one owner:

* `internal/codexpin/pin_test.go` — the pin schema and its evidence re-hashing,
  the bundle shape, the ELF check, the archive member policy, the raw-block
  bounds, PAX size overrides, the decoded-stream budget, gzip truncation and the
  physical-header budget;
* `internal/codewriter/{render,stagedir,writer}_test.go` — the rendered pin's
  byte shape, the staging directory rules, the CI refusal and the same-release
  identity comparisons;
* `internal/publication/publish_test.go` — the publication fault cases: a partial
  write, a signal, a refused restore and an unwritable recovery record;
* `script/test/image-sources/tests/image_sources.rs` — the independent
  re-derivation of the pin schema and of every URL and digest mutation.

`--real-signatures` is gone. It drove the Python verifier against retained
payloads and the network; the Go reader never verifies a signature (it checks
committed bytes only), so the mode has no black-box equivalent and no owner.

`--write-fixture DIR --release TAG` writes a committed-shape closure (pin plus
its four evidence files) that `script/test/codex-installer.sh` installs as its
fixture pin directory. `--write-installer-fixture DIR --release TAG --target T
--native-script FILE` writes the fixture the two installer suites consume:
`source/` (the closure they mount as the pin directory), `assets/` (the six
upstream documents, by their canonical asset names), `archive.tgz` for the
selected target, and `upstream/` laid out exactly as `--fixture-upstream`
serves it. The entrypoint of that fixture package is the reviewed ELF header
followed by the given script body: the header is what the production preflight
reads, and the body is what a suite that runs the payload executes.
"""
from __future__ import annotations

import argparse
import base64
import datetime
import gzip
import hashlib
import io
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path
from typing import Callable, Mapping

REPO_ROOT = Path(__file__).resolve().parents[2]
CODEX_DIR = REPO_ROOT / 'images' / 'tools' / 'codex'
WRITER = REPO_ROOT / 'script' / 'dev' / 'pin-codex.sh'
CONTRACT_BUILD = REPO_ROOT / 'script' / 'build' / 'recipe-contract.sh'

RELEASE = 'rust-v0.159.3'
VERSION = '0.159.3'
NEXT_RELEASE = 'rust-v0.159.4'
TARGETS = {'linux/amd64': 'x86_64-unknown-linux-musl',
           'linux/arm64': 'aarch64-unknown-linux-musl'}
MACHINES = {'x86_64-unknown-linux-musl': 62, 'aarch64-unknown-linux-musl': 183}
ISSUER = 'https://token.actions.githubusercontent.com'
WORKFLOW = 'https://github.com/openai/codex/.github/workflows/rust-release.yml'
VERIFIER_VERSION = 'v2.4.1'
CHECKSUMS_ASSET = 'codex-package_SHA256SUMS'

# The two extreme zones differ from UTC in opposite directions, so between them
# every hour of the UTC day has one host whose local date differs from the UTC date.
CLOCK_ZONES = ('Pacific/Kiritimati', 'Etc/GMT+12')

# The reader's reviewed exact-release grammar (`internal/codexpin`'s
# releaseVersionPattern), mirrored here because the fixture builder may not import
# a production module. A fixture release that the reader would refuse cannot be
# written, so the mirror is asserted by every `check` case rather than trusted.
FIXTURE_RELEASE_RE = re.compile(
    r'^rust-v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
    r'(-alpha(\.(0|[1-9][0-9]*)){0,2}|-beta(\.(0|[1-9][0-9]*))?)?$')

failures: list[str] = []
cases: list[str] = []
work: list[Path] = []
_reader: Path | None = None


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: object) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def run_case(label: str, function) -> None:
    try:
        function()
    except AssertionError as error:
        fail(label, str(error))
        return
    except Exception as error:  # noqa: BLE001 - every case reports, none aborts
        fail(label, f'{type(error).__name__}: {error}')
        return
    cases.append(label)
    report(label)


def utc_today() -> datetime.date:
    return datetime.datetime.now(datetime.timezone.utc).date()


def scratch(label: str) -> Path:
    directory = Path(tempfile.mkdtemp(prefix=f'codex-pins.{label}.',
                                      dir=os.environ.get('TMPDIR')))
    work.append(directory)
    return directory


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


# --- the real programs -------------------------------------------------------

def reader_binary() -> Path:
    """The reader, compiled once per run.

    `script/build/recipe-contract.sh --native` prints the composed directory it
    built (its build log goes to stderr), which is the same helper the image
    receives through a named build context.
    """
    global _reader
    if _reader is None:
        built = subprocess.run(['bash', str(CONTRACT_BUILD), '--native'],
                               capture_output=True, text=True, check=False)
        require(built.returncode == 0,
                f'cannot build recipe-contract: {built.stderr.strip()}')
        context = built.stdout.strip().splitlines()[-1]
        _reader = Path(context) / 'out' / 'recipe-contract'
        require(_reader.is_file(), f'the composed context holds no reader: {_reader}')
    return _reader


def run_reader(arguments: list[str], *, env: dict | None = None,
               timeout: int = 600) -> subprocess.CompletedProcess:
    return subprocess.run([str(reader_binary()), 'codex-pin', *arguments],
                          capture_output=True, text=True, check=False, env=env,
                          timeout=timeout)


READER_PREFIX = 'codex_pin: '
WRITER_PREFIX = 'pin-codex: '


def refuses(arguments: list[str], needles=(), *, prefix: str = READER_PREFIX,
            run=None) -> str:
    """The message of a refused invocation, which exits 1 and names its reason."""
    finished = (run or run_reader)(arguments)
    require(finished.returncode == 1,
            f'expected a refusal, got {finished.returncode}: '
            f'{finished.stdout.strip()} {finished.stderr.strip()}')
    require(finished.stderr.startswith(prefix),
            f'the refusal is not prefixed with {prefix!r}: {finished.stderr!r}')
    text = finished.stderr[len(prefix):].strip()
    for needle in needles:
        require(needle in text, f'the refusal did not mention {needle!r}: {text}')
    return text


def accepts(arguments: list[str], *, run=None) -> str:
    finished = (run or run_reader)(arguments)
    require(finished.returncode == 0,
            f'unexpected refusal ({finished.returncode}): {finished.stderr.strip()}')
    return finished.stdout


def usage_refused(arguments: list[str], needle: str, *, run=None) -> None:
    """A malformed invocation: status 2, the same prefix, and its own reason."""
    finished = (run or run_reader)(arguments)
    require(finished.returncode == 2,
            f'expected a usage error, got {finished.returncode}: '
            f'{finished.stdout.strip()} {finished.stderr.strip()}')
    require(finished.stderr.startswith(READER_PREFIX),
            f'the usage error is not prefixed with {READER_PREFIX!r}: '
            f'{finished.stderr!r}')
    text = finished.stderr[len(READER_PREFIX):].strip()
    require(needle in text, f'the usage error did not mention {needle!r}: {text}')


def writer_env(overrides: Mapping[str, str | None] | None = None) -> dict:
    """The environment a maintainer host would have, minus the CI marker.

    Every case here stands in for a maintainer host, and the aggregate normally
    runs on a runner, so an inherited `GITHUB_ACTIONS` would turn each writer case
    into the CI refusal. That refusal has its own case, which sets the marker
    explicitly.
    """
    environment = {key: value for key, value in os.environ.items()
                   if key != 'GITHUB_ACTIONS'}
    for name, value in (overrides or {}).items():
        if value is None:
            environment.pop(name, None)
        else:
            environment[name] = value
    return environment


def run_writer(arguments: list[str], *, root: Path,
               env: Mapping[str, str | None] | None = None,
               timeout: int = 1800) -> subprocess.CompletedProcess:
    """The real maintainer wrapper: it compiles `avm-pin` and execs `avm-pin codex`."""
    return subprocess.run(['bash', str(WRITER), '--root', str(root), *arguments],
                          capture_output=True, text=True, check=False,
                          env=writer_env(env), timeout=timeout)


def write_cosign(directory: Path, *, version: str = VERIFIER_VERSION,
                 verify: int = 0, record: Path | None = None) -> Path:
    """The signature seam: an executable that is the reviewed verifier, or refuses.

    It is a fixture program, never a production flag: `--probe-cosign` reports
    whatever the file it is given reports, and `verify-blob` is how the writer
    learns whether the committed bundle verifies. `verify` is the status it exits
    with instead of 0, which is the whole "cosign refused" case.

    `record` is where one line of `argv` and one line of environment names are
    appended per run: the writer constructs that child's environment, so the child
    is the only place a case can observe it.
    """
    path = Path(directory) / 'cosign'
    path.parent.mkdir(parents=True, exist_ok=True)
    script = ('#!/bin/sh\n'
              'if [ "${1:-}" = version ]; then\n'
              "  printf '{\"gitVersion\":\"%s\"}\\n' '" + version + "'\n"
              '  exit 0\n'
              'fi\n')
    if record is not None:
        script += ("{\n"
                   "  printf 'argv'\n"
                   '  for argument in "$@"; do printf \' %s\' "$argument"; done\n'
                   "  printf '\\n'\n"
                   "  printf 'env'\n"
                   "  env | sed 's/=.*//' | sort | while read -r name; do\n"
                   "    printf ' %s' \"$name\"\n"
                   "  done\n"
                   "  printf '\\n'\n"
                   f"}} >> {str(Path(record))!r}\n")
    script += f'exit {verify}\n'
    path.write_text(script)
    path.chmod(0o755)
    require(path.is_file(), f'the fixture cosign was not written: {path}')
    return path


# The environment entries a POSIX shell adds for itself, which say nothing about
# what the parent passed on.
SHELL_LOCALS = frozenset({'PWD', 'OLDPWD', 'SHLVL', '_'})


def read_cosign_record(path: Path) -> list[dict[str, list[str]]]:
    """The recorded argv and environment of every verifier run, in order."""
    lines = [line for line in Path(path).read_text().splitlines() if line.strip()]
    records = []
    while lines:
        argv, names = lines.pop(0), lines.pop(0)
        require(argv.startswith('argv'), f'the verifier record is not argv: {argv!r}')
        require(names.startswith('env'), f'the verifier record is not env: {names!r}')
        records.append({'argv': argv[len('argv'):].split(),
                        'env': names[len('env'):].split()})
    return records


# --- synthetic fixtures ------------------------------------------------------

def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def api_url(release: str) -> str:
    """The exact-tag release document's canonical URL."""
    return f'https://api.github.com/repos/openai/codex/releases/tags/{release}'


def asset_url(release: str, asset: str) -> str:
    return f'https://github.com/openai/codex/releases/download/{release}/{asset}'


def asset_name(target: str) -> str:
    return f'codex-package-{target}.tar.gz'


def bundle_name(target: str) -> str:
    return f'{asset_name(target)}.sigstore'


def identity(release: str) -> str:
    return f'{WORKFLOW}@refs/tags/{release}'


def require_fixture_release(release: str):
    require(isinstance(release, str) and release != '',
            'a fixture release must be a non-empty string')
    require(FIXTURE_RELEASE_RE.match(release) is not None,
            f'the fixture release is not an exact Codex release: {release!r}')
    return release


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


def make_metadata(digests: Mapping[str, str], *, release: str = RELEASE,
                  download_count: int = 1, extra_assets: tuple = ()) -> bytes:
    assets = [{'name': name, 'digest': f'sha256:{digest}',
               'download_count': download_count}
              for name, digest in digests.items()]
    assets += list(extra_assets)
    return json.dumps({'tag_name': release, 'name': release, 'assets': assets},
                      indent=2).encode() + b'\n'


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
    require(len(encoded) <= 100, f'a tar member name is too long: {name}')
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


def fixture_payload(machine: int, body: bytes = b'') -> bytes:
    """An entrypoint the reviewed preflight accepts.

    The reader's ELF check reads the first twenty bytes of `bin/codex` and nothing
    else, so those twenty bytes are the whole header it needs; a newline follows
    them and the optional body is what a suite that runs the payload executes.
    """
    return elf_header(machine)[:20] + b'\n' + body


def default_members(*, version: str = VERSION,
                    target: str = TARGETS['linux/amd64'],
                    package: dict | None = None,
                    machine: int | None = None,
                    elf: bytes | None = None) -> list[tuple[str, bytes, bytes, int]]:
    """(name, data, typeflag, mode) in tar order: the reviewed package layout."""
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


def build_archive(path: Path, members=None, **kwargs) -> Path:
    entries = default_members(**kwargs) if members is None else members
    raw = b''
    for name, data, typeflag, mode in entries:
        raw += tar_member(name, data, typeflag, mode)
    raw += b'\x00' * 1024
    Path(path).write_bytes(gzip_bytes(raw))
    return Path(path)


class Fixture:
    """A complete synthetic closure: pin.json plus its four evidence files.

    Every digest in the pin and in the release metadata is derived from the bytes
    this object holds, so the closure is internally consistent by construction and
    a case that changes one payload gets a closure that commits the changed
    payload -- the digest is then never the reason a case refuses.
    """

    def __init__(self, directory: Path, *, release: str = RELEASE,
                 archives: Mapping[str, bytes] | None = None) -> None:
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self.release = require_fixture_release(release)
        self.version = self.release[len('rust-v'):]
        self.archives = dict(archives or self._default_archives())
        self.checksums = make_checksums(
            {asset_name(target): sha256(self.archives[platform])
             for platform, target in TARGETS.items()},
            extra=(('decoy', '1' * 64),))
        self.bundles = {platform: make_bundle(sha256(raw))
                        for platform, raw in self.archives.items()}
        self.metadata = make_metadata(self.release_digests(), release=self.release)
        self.pin = self._pin()
        self.write()

    def _default_archives(self) -> dict[str, bytes]:
        """Byte-deterministic per platform, so a repeat is genuinely a repeat.

        `tarfile.open(mode='w:gz')` stamps the current time into the gzip header,
        which would make every fixture a different identity and hide exactly the
        repeat/unchanged behaviour the writer cases assert.
        """
        archives = {}
        for platform, target in TARGETS.items():
            raw = b''
            for name, data, typeflag, mode in default_members(
                    version=self.version, target=target):
                raw += tar_member(name, data, typeflag, mode)
            raw += b'\x00' * 1024
            archives[platform] = gzip_bytes(raw)
        return archives

    def release_digests(self) -> dict[str, str]:
        """What the exact-tag release document must declare for these bytes."""
        digests = {CHECKSUMS_ASSET: sha256(self.checksums)}
        for platform, target in TARGETS.items():
            digests[asset_name(target)] = sha256(self.archives[platform])
            digests[bundle_name(target)] = sha256(self.bundles[platform])
        return digests

    def _evidence(self, url: str, path: str, raw: bytes) -> dict:
        return {'url': url, 'path': path, 'sha256': sha256(raw)}

    def _pin(self) -> dict:
        platforms = {}
        for platform, target in TARGETS.items():
            asset = asset_name(target)
            platforms[platform] = {
                'target': target,
                'asset': asset,
                'url': asset_url(self.release, asset),
                'sha256': sha256(self.archives[platform]),
                'bundle': self._evidence(asset_url(self.release, bundle_name(target)),
                                         f'evidence/{bundle_name(target)}',
                                         self.bundles[platform]),
            }
        return {
            'schema': 1, 'name': 'codex', 'kind': 'codex-standalone-package',
            'release': self.release, 'version': self.version,
            'package': {'layout_version': 1, 'variant': 'codex',
                        'entrypoint': 'bin/codex'},
            'metadata': self._evidence(api_url(self.release),
                                       'evidence/release.json', self.metadata),
            'checksums': self._evidence(asset_url(self.release, CHECKSUMS_ASSET),
                                        f'evidence/{CHECKSUMS_ASSET}', self.checksums),
            'platforms': platforms,
            'authentication': {
                'method': 'sigstore-blob-pin-time',
                'verifier': {'name': 'cosign', 'version': VERIFIER_VERSION},
                'trust_root': 'sigstore-production-tuf',
                'certificate_identity': identity(self.release),
                'certificate_oidc_issuer': ISSUER,
                'verified_on': '2026-01-02',
            },
        }

    def write(self, pin: dict | None = None) -> None:
        """Write the closure, in the exact shape the writer renders."""
        pin = self.pin if pin is None else pin
        (self.directory / 'evidence').mkdir(parents=True, exist_ok=True)
        (self.directory / 'pin.json').write_text(json.dumps(pin, indent=2) + '\n')
        (self.directory / 'evidence' / 'release.json').write_bytes(self.metadata)
        (self.directory / 'evidence' / CHECKSUMS_ASSET).write_bytes(self.checksums)
        for platform, target in TARGETS.items():
            (self.directory / 'evidence' / bundle_name(target)).write_bytes(
                self.bundles[platform])

    def served(self) -> dict[str, bytes]:
        """Every canonical URL this closure's release answers, and its bytes."""
        served = {api_url(self.release): self.metadata,
                  asset_url(self.release, CHECKSUMS_ASSET): self.checksums}
        for platform, target in TARGETS.items():
            asset = asset_name(target)
            served[asset_url(self.release, asset)] = self.archives[platform]
            served[asset_url(self.release, bundle_name(target))] = self.bundles[platform]
        return served

    def write_upstream(self, directory: Path) -> Path:
        """The `--fixture-upstream` tree: `<host>/<path>` for every canonical URL."""
        directory = Path(directory)
        for url, raw in self.served().items():
            path = upstream_path(directory, url)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(raw)
        return directory

    def archive(self, directory: Path, platform: str = 'linux/amd64') -> Path:
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True)
        path = directory / asset_name(TARGETS[platform])
        path.write_bytes(self.archives[platform])
        return path


def upstream_path(directory: Path, url: str) -> Path:
    """Where `--fixture-upstream DIR` serves `url` from."""
    parts = url.split('://', 1)
    require(len(parts) == 2, f'not an absolute URL: {url}')
    return Path(directory) / parts[1]


def write_installer_fixture(directory: Path, release: str, target: str,
                            native_script: Path, package_version: str | None,
                            package_target: str | None) -> None:
    """The fixture the two installer suites consume.

    The package layout is the real one -- four explicit top-level names, real
    package metadata, an entrypoint whose first twenty bytes are the reviewed ELF
    header -- and every pin/evidence digest is derived from these exact bytes. The
    body after the ELF header is the given script, so a suite that runs the payload
    gets a recording command instead of a foreign-architecture binary.
    """
    release = require_fixture_release(release)
    version = release[len('rust-v'):]
    target = target if target in TARGETS.values() else None
    require(target is not None, f'unknown target: {target}')
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    body = Path(native_script).read_bytes()
    archives: dict[str, bytes] = {}
    for platform, platform_target in TARGETS.items():
        document = package_document(package_version or version,
                                    package_target or platform_target)
        payload = fixture_payload(MACHINES[platform_target], body)
        members = [
            ('bin', b'', tarfile.DIRTYPE, 0o755),
            ('bin/codex', payload, tarfile.REGTYPE, 0o755),
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
    # Both payloads are exported under their canonical asset names, so a suite can
    # answer the writer's exact URLs from a directory without re-deriving anything.
    # `archive.tgz` stays the selected platform for the vendored suite.
    assets = directory / 'assets'
    assets.mkdir(parents=True, exist_ok=True)
    for platform, name in TARGETS.items():
        asset = asset_name(name)
        (assets / asset).write_bytes(archives[platform])
        (assets / bundle_name(name)).write_bytes(fixture.bundles[platform])
    (assets / 'release.json').write_bytes(fixture.metadata)
    (assets / CHECKSUMS_ASSET).write_bytes(fixture.checksums)
    (directory / 'archive.tgz').write_bytes(archives[selected])
    fixture.write_upstream(directory / 'upstream')


DOCKERFILE_TEMPLATE = (
    'FROM scratch\n'
    'ARG AGENT_VERSION_CODEX={release}\n'
    'LABEL org.agent-vm.version.codex="${{AGENT_VERSION_CODEX}}"\n'
    'ARG AGENT_VERSION_OPENCODE=v2.0.29\n'
    'ARG AGENT_VERSION_CLAUDE=2.1.286\n'
    'ARG AGENT_VERSION_COPILOT=1.0.90\n'
)


class Repository:
    """The repository the writer publishes into: the pin directory and the Dockerfile.

    Those are the only two things a Codex transaction reads from the tree, so the
    fixture repository carries exactly them and a case can never pass by accident
    on a file the real repository happens to have.
    """

    def __init__(self, root: Path, *, release: str = RELEASE,
                 dockerfile: str | None = None) -> None:
        self.root = Path(root)
        shutil.rmtree(self.root, ignore_errors=True)
        self.pin_directory = self.root / 'images' / 'tools' / 'codex'
        self.pin_directory.mkdir(parents=True)
        (self.root / 'images' / 'standard').mkdir(parents=True)
        self.dockerfile = self.root / 'images' / 'standard' / 'Dockerfile'
        self.dockerfile.write_text(dockerfile
                                   if dockerfile is not None
                                   else DOCKERFILE_TEMPLATE.format(release=release))

    def install(self, fixture: Fixture) -> 'Repository':
        for relative in ['pin.json', f'evidence/release.json',
                         f'evidence/{CHECKSUMS_ASSET}'] + [
                             f'evidence/{bundle_name(target)}' for target in TARGETS.values()]:
            source = fixture.directory / relative
            destination = self.pin_directory / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(source.read_bytes())
        return self

    def committed(self) -> dict[str, bytes]:
        """The six proposal destinations as the repository holds them."""
        names = ['images/tools/codex/pin.json',
                 'images/tools/codex/evidence/release.json',
                 f'images/tools/codex/evidence/{CHECKSUMS_ASSET}'] + [
                     f'images/tools/codex/evidence/{bundle_name(target)}'
                     for target in TARGETS.values()] + ['images/standard/Dockerfile']
        return {name: (self.root / name).read_bytes() for name in names}

    def snapshot(self) -> dict[str, bytes]:
        return {str(path.relative_to(self.root)): path.read_bytes()
                for path in sorted(self.root.rglob('*'))
                if path.is_file()}

    def stage_directory(self, path: Path) -> Path:
        """An absent staging directory: the writer creates it 0700 itself."""
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        return path


# --- the committed closure ---------------------------------------------------

def committed_cases() -> None:
    """The real committed pin, read and checked by the real programs."""
    document = json.loads((CODEX_DIR / 'pin.json').read_text())
    release = document['release']

    def reader_check() -> None:
        printed = accepts(['check', '--pin-dir', str(CODEX_DIR), '--release', release])
        lines = [line for line in printed.splitlines() if line.strip()]
        require(len(lines) == len(TARGETS) + 2, f'check printed {len(lines)} lines: {lines}')
        require(lines[0] == f'codex {release}: offline committed-input check '
                            '(committed bytes and policy only; not a signature check)',
                f'check did not report the offline verification: {lines[0]}')
        for line in lines[1:-1]:
            require(line.startswith('  linux/'), f'unexpected platform line: {line}')
        require(lines[-1] == '  evidence: 2 files plus 2 structurally consistent bundles',
                f'check did not report the evidence set: {lines[-1]}')

    def select() -> None:
        for platform, target in TARGETS.items():
            printed = accepts(['select', '--pin-dir', str(CODEX_DIR),
                               '--release', release, '--target', target])
            pinned = document['platforms'][platform]
            require(printed == (f'{document["version"]}\n{pinned["asset"]}\n'
                                f'{pinned["url"]}\n{pinned["sha256"]}\n'),
                    f'select {target} printed {printed!r}')

    def writer_check() -> None:
        finished = run_writer(['--check'], root=REPO_ROOT)
        require(finished.returncode == 0,
                f'the writer refused the committed tree ({finished.returncode}): '
                f'{finished.stderr.strip()}')
        require('offline committed-input check' in finished.stdout,
                f'--check did not report the offline verification: {finished.stdout}')
        require(f'ARG AGENT_VERSION_CODEX={release}' in finished.stdout,
                f'--check did not report the single ARG slot: {finished.stdout}')

    for label, function in (('committed: the reader verifies the closure offline',
                             reader_check),
                            ('committed: select prints four fields per target', select),
                            ('committed: the writer --check accepts the tree',
                             writer_check)):
        run_case(label, function)


# --- reader: the program contract --------------------------------------------

def reader_cases() -> None:
    fixture = Fixture(scratch('reader') / 'source')

    def check_transcript() -> None:
        printed = accepts(['check', '--pin-dir', str(fixture.directory),
                           '--release', RELEASE])
        require(printed.startswith(f'codex {RELEASE}: offline committed-input check'),
                f'check printed {printed!r}')
        require(printed.endswith('structurally consistent bundles\n'),
                f'check printed {printed!r}')

    def select_four_fields() -> None:
        for platform, target in TARGETS.items():
            printed = accepts(['select', '--pin-dir', str(fixture.directory),
                               '--release', RELEASE, '--target', target])
            asset = asset_name(target)
            expected = (f'{VERSION}\n{asset}\n{asset_url(RELEASE, asset)}\n'
                        f'{sha256(fixture.archives[platform])}\n')
            require(printed == expected,
                    f'select {target} printed {printed!r}, want {expected!r}')

    def unpinned_release() -> None:
        refuses(['check', '--pin-dir', str(fixture.directory),
                 '--release', NEXT_RELEASE],
                ('not the pinned Codex release', NEXT_RELEASE))

    def missing_closure() -> None:
        refuses(['check', '--pin-dir', str(fixture.directory / 'absent'),
                 '--release', RELEASE],
                ('is missing at', 'pin.json'))

    def release_grammar() -> None:
        for candidate, needle in (
                ('rust-v0.159', 'not an exact Codex version'),
                ('latest', 'must be rust-v<version>'),
                ('0.159.3', 'must be rust-v<version>'),
                ('rust-v01.2.3', 'not an exact Codex version')):
            refuses(['check', '--pin-dir', str(fixture.directory),
                     '--release', candidate], (needle,))

    def unknown_target() -> None:
        refuses(['select', '--pin-dir', str(fixture.directory),
                 '--release', RELEASE, '--target', 'x86_64-unknown-linux-gnu'],
                ('must be one of', 'unknown-linux-musl'))

    def usage() -> None:
        pinned = ['--pin-dir', str(fixture.directory), '--release', RELEASE]
        target = ['--target', TARGETS['linux/amd64']]
        for arguments, needle in (
                (['version', *pinned], 'unknown verb version'),
                (['check', '--release', RELEASE], 'check needs --pin-dir'),
                (['check', *pinned, *target], 'check does not take --target'),
                (['select', *pinned], 'select needs --target'),
                (['select', *pinned, *target, '--archive', '/nonexistent'],
                 'select does not take --archive'),
                (['preflight', *pinned, *target], 'preflight needs --archive')):
            usage_refused(arguments, needle)

    for label, function in (
            ('reader: check prints the offline transcript', check_transcript),
            ('reader: select prints four exact fields per target', select_four_fields),
            ('reader: an exact but unpinned release is refused', unpinned_release),
            ('reader: a closure with no pin.json is refused', missing_closure),
            ('reader: a released tag that is not exact is refused', release_grammar),
            ('reader: an unmaintained target is refused', unknown_target),
            ('reader: a malformed invocation exits 2 with the reader prefix', usage)):
        run_case(label, function)


# --- reader: preflight on a real archive -------------------------------------

def preflight_cases() -> None:
    fixture = Fixture(scratch('preflight') / 'source')
    target = TARGETS['linux/amd64']

    def selected_preflight(arguments: list[str], *needles: str) -> str:
        return refuses(['preflight', '--pin-dir', str(fixture.directory),
                        '--release', RELEASE, '--target', target, *arguments], needles)

    def accepts_the_package() -> None:
        archive = fixture.archive(scratch('preflight-accept'), 'linux/amd64')
        printed = accepts(['preflight', '--pin-dir', str(fixture.directory),
                           '--release', RELEASE, '--target', target,
                           '--archive', str(archive)])
        require(printed == (f'codex {VERSION} linux/amd64: committed identity, package '
                            'metadata and ELF machine verified before extraction\n'),
                f'preflight printed {printed!r}')

    def not_the_committed_bytes() -> None:
        directory = scratch('preflight-substituted')
        archive = directory / asset_name(target)
        archive.write_bytes(fixture.archives['linux/amd64'] + b'extra')
        selected_preflight(['--archive', str(archive)],
                           'does not match the committed')

    def missing_archive() -> None:
        selected_preflight(['--archive', str(scratch('preflight-missing') / 'none.tgz')],
                           'no downloaded archive at')

    def other_machine() -> None:
        """A closed closure whose archive is built for the other machine.

        The digest is committed, the package metadata agrees with itself, and the
        only thing left to refuse is the ELF machine: this is the check that no
        amount of corroboration can substitute for.
        """
        directory = scratch('preflight-machine')
        archive = build_archive(directory / asset_name(target),
                                machine=MACHINES['aarch64-unknown-linux-musl'])
        local = Fixture(directory / 'source',
                        archives={**fixture.archives,
                                  'linux/amd64': archive.read_bytes()})
        refuses(['preflight', '--pin-dir', str(local.directory), '--release', RELEASE,
                 '--target', target, '--archive', str(archive)],
                ('ELF machine 183', 'x86_64-unknown-linux-musl'))

    for label, function in (
            ('preflight: accepts the reviewed package', accepts_the_package),
            ('preflight: refuses an archive that is not the committed bytes',
             not_the_committed_bytes),
            ('preflight: refuses a missing archive', missing_archive),
            ('preflight: refuses the other machine\'s ELF', other_machine)):
        run_case(label, function)


# --- writer: the staging and publication contract ----------------------------

def writer_cases() -> None:
    base = scratch('writer')
    template = Fixture(base / 'template')
    upstream = template.write_upstream(base / 'upstream')
    cosign = write_cosign(base / 'cosign')

    def repo(label: str) -> Repository:
        return Repository(base / label / 'repo')

    def tmp(label: str) -> Path:
        directory = base / label / 'tmp'
        directory.mkdir(parents=True, exist_ok=True)
        return directory

    def check_accepts_the_closure() -> None:
        repository = repo('check-ok').install(template)
        finished = run_writer(['--check'], root=repository.root)
        require(finished.returncode == 0,
                f'--check refused the closure ({finished.returncode}): '
                f'{finished.stderr.strip()}')
        require(f'codex {RELEASE}: offline committed-input check' in finished.stdout,
                f'--check did not print the reader transcript: {finished.stdout}')
        require(f'ARG AGENT_VERSION_CODEX={RELEASE}' in finished.stdout,
                f'--check did not print the ARG slot: {finished.stdout}')

    def check_refuses(description: str, dockerfile: str, needle: str) -> None:
        repository = Repository(base / f'check-{description}' / 'repo',
                                dockerfile=dockerfile).install(template)
        refuses(['--check'], (needle,), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root))

    def probe_accepts() -> None:
        repository = repo('probe-ok').install(template)
        before = repository.committed()
        finished = run_writer(['--probe-cosign', str(cosign)], root=repository.root)
        require(finished.returncode == 0,
                f'--probe-cosign refused the reviewed verifier: {finished.stderr}')
        require(finished.stdout == f'cosign {VERIFIER_VERSION}: {cosign}\n',
                f'--probe-cosign printed {finished.stdout!r}')
        require(repository.committed() == before, 'the probe wrote to the repository')

    def probe_refuses_a_version() -> None:
        other = write_cosign(base / 'cosign-other', version='v2.2.0')
        repository = repo('probe-version').install(template)
        refuses(['--probe-cosign', str(other)],
                ('reviewed verifier is v2.4.1',), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root))

    def probe_refuses_a_non_executable() -> None:
        plain = base / 'cosign-plain'
        plain.write_text('not executable\n')
        repository = repo('probe-plain').install(template)
        refuses(['--probe-cosign', str(plain)], ('is not an executable file',),
                prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root))

    def ci_refusal() -> None:
        repository = repo('ci')
        before = repository.snapshot()
        # The fixture upstream holds the whole release, so a fetch before the
        # refusal would have succeeded and the case would prove nothing. The
        # served tree is emptied to make a fetch impossible instead: the refusal
        # must still be the CI one.
        empty = base / 'no-upstream'
        empty.mkdir(parents=True, exist_ok=True)
        refuses(['--version', RELEASE, '--cosign', str(cosign),
                 '--fixture-upstream', str(empty)], ('refusing GitHub Actions',),
                prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'GITHUB_ACTIONS': 'true'}))
        require(repository.snapshot() == before,
                'a refused CI run changed the repository')

    def publish_stage_ci_refusal() -> None:
        repository = repo('publish-ci')
        stage, _printed = stage_proposal(repository, 'publish-ci')
        before = repository.snapshot()
        refuses(['--publish-stage', str(stage)], ('refusing GitHub Actions',),
                prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'GITHUB_ACTIONS': 'true'}))
        require(repository.snapshot() == before,
                'a refused CI publication changed the repository')

    def stage_proposal(repository: Repository, label: str, *,
                       release: str = RELEASE,
                       served: Path | None = None) -> tuple[Path, str]:
        """Stage one proposal through the real writer, from the served release."""
        stage = repository.stage_directory(base / label / 'stage')
        finished = run_writer(['--version', release, '--stage-dir', str(stage),
                               '--cosign', str(cosign),
                               '--fixture-upstream', str(served or upstream)],
                              root=repository.root, env={'TMPDIR': str(tmp(label))})
        require(finished.returncode == 0,
                f'staging {release} failed ({finished.returncode}): '
                f'{finished.stdout.strip()} {finished.stderr.strip()}')
        return stage, finished.stdout

    def six_file_proposal() -> None:
        repository = repo('stage-ok')
        before = repository.snapshot()
        stage, printed = stage_proposal(repository, 'stage-ok')
        require(f'pin-codex: staged {RELEASE} in {stage}\n' in printed,
                f'the writer did not print the frozen staged line: {printed!r}')
        written = sorted(str(path.relative_to(stage)) for path in stage.rglob('*')
                         if path.is_file())
        expected = sorted(['pin.json', 'Dockerfile', 'evidence/release.json',
                           f'evidence/{CHECKSUMS_ASSET}']
                          + [f'evidence/{bundle_name(target)}'
                             for target in TARGETS.values()])
        require(written == expected, f'the proposal is {written}')
        # The proposal is a closure the reader accepts, from its own directory.
        accepts(['check', '--pin-dir', str(stage), '--release', RELEASE])
        require(repository.snapshot() == before,
                'a staging run wrote into the repository')
        # The served tree holds exactly the six canonical documents, so a staging
        # run that succeeded read nothing else: the request set is the whole tree.
        documents = [path for path in upstream.rglob('*') if path.is_file()]
        require(len(documents) == 6,
                f'the served tree is not the six documents: '
                f'{sorted(str(path.relative_to(upstream)) for path in documents)}')

    def keeps_the_callers_files() -> None:
        repository = repo('stage-owned')
        stage = repository.stage_directory(base / 'stage-owned' / 'stage')
        stage.mkdir(parents=True)
        stage.chmod(0o700)
        sentinel = stage / 'mine.txt'
        sentinel.write_text('mine\n')
        before = repository.snapshot()
        refuses(['--version', RELEASE, '--stage-dir', str(stage),
                 '--cosign', str(cosign), '--fixture-upstream', str(upstream)],
                ('is not empty',), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'TMPDIR': str(tmp('stage-owned'))}))
        require(sentinel.read_text() == 'mine\n', 'the refused run erased a caller file')
        require(repository.snapshot() == before,
                'a refused staging run changed the repository')

    def unchanged_repeat() -> None:
        """A byte-identical re-stage of the committed release prints unchanged."""
        repository = repo('stage-repeat').install(template)
        stage = repository.stage_directory(base / 'stage-repeat' / 'stage')
        finished = run_writer(['--version', RELEASE, '--stage-dir', str(stage),
                               '--cosign', str(cosign),
                               '--fixture-upstream', str(upstream)],
                              root=repository.root,
                              env={'TMPDIR': str(tmp('stage-repeat'))})
        require(finished.returncode == 0,
                f'the repeat refused ({finished.returncode}): {finished.stderr.strip()}')
        require(f'pin-codex: unchanged ({RELEASE}); the staged proposal is '
                f'byte-identical to the committed files ({stage})\n'
                in finished.stdout,
                f'the repeat did not print the frozen unchanged line: {finished.stdout!r}')

    def publishes_the_proposal() -> None:
        repository = repo('publish')
        stage, _printed = stage_proposal(repository, 'publish')
        proposal = {str(path.relative_to(stage)): path.read_bytes()
                    for path in stage.rglob('*') if path.is_file()}
        before = repository.snapshot()
        finished = run_writer(['--publish-stage', str(stage)], root=repository.root)
        require(finished.returncode == 0,
                f'--publish-stage refused ({finished.returncode}): {finished.stderr}')
        require(f'publishing the staged proposal in {stage}' in finished.stderr,
                f'the publisher did not name the proposal: {finished.stderr}')
        require(f'owner scratch: {stage}' in finished.stderr,
                f'the publisher did not name the owner scratch: {finished.stderr}')
        published = repository.committed()
        require(published['images/tools/codex/pin.json'] == proposal['pin.json'],
                'the published pin is not the staged pin')
        require(published['images/standard/Dockerfile'] == proposal['Dockerfile'],
                'the published Dockerfile is not the staged Dockerfile')
        for target in TARGETS.values():
            relative = f'evidence/{bundle_name(target)}'
            require(published[f'images/tools/codex/{relative}'] == proposal[relative],
                    f'the published {relative} is not the staged one')
        require(repository.snapshot() != before, 'the publication changed nothing')
        accepts(['check', '--pin-dir', str(repository.pin_directory),
                 '--release', RELEASE])

    def publish_refuses_a_foreign_arg() -> None:
        repository = repo('publish-arg')
        stage, _printed = stage_proposal(repository, 'publish-arg')
        dockerfile = stage / 'Dockerfile'
        text = dockerfile.read_text()
        marker = 'ARG AGENT_VERSION_CLAUDE=2.1.286'
        require(text.count(marker) == 1,
                f'the staged Dockerfile does not carry one Claude ARG: {text!r}')
        dockerfile.write_text(text.replace(marker, 'ARG AGENT_VERSION_CLAUDE=9.9.9'))
        before = repository.snapshot()
        refuses(['--publish-stage', str(stage)],
                ('changes more than the single-slot version defaults',),
                prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root))
        require(repository.snapshot() == before,
                'a refused proposal changed the repository')

    def same_release_rewrite() -> None:
        repository = repo('rewrite').install(template)
        directory = scratch('rewrite-archive')
        rewritten = build_archive(
            directory / asset_name(TARGETS['linux/amd64']),
            members=default_members() + [
                ('codex-resources/voice', b'', tarfile.DIRTYPE, 0o755),
                ('codex-resources/voice/note.txt', b'rewritten\n', tarfile.REGTYPE,
                 0o644)])
        local = Fixture(scratch('rewrite-source') / 'source',
                        archives={**template.archives,
                                  'linux/amd64': rewritten.read_bytes()})
        served = local.write_upstream(scratch('rewrite-upstream'))
        before = repository.snapshot()
        refuses(['--version', RELEASE, '--stage-dir',
                 str(base / 'rewrite' / 'stage'), '--cosign', str(cosign),
                 '--fixture-upstream', str(served)],
                ('changed for the already-pinned',), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'TMPDIR': str(tmp('rewrite'))}))
        require(repository.snapshot() == before,
                'a refused same-release rewrite changed the repository')

    def refused_verifier() -> None:
        repository = repo('refused')
        refusing = write_cosign(base / 'cosign-refusing', verify=1)
        before = repository.snapshot()
        refuses(['--version', RELEASE, '--stage-dir',
                 str(base / 'refused' / 'stage'), '--cosign', str(refusing),
                 '--fixture-upstream', str(upstream)],
                ('cosign refused the committed bundle',), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'TMPDIR': str(tmp('refused'))}))
        require(repository.snapshot() == before,
                'a refusing verifier changed the repository')

    def verifier_contract() -> None:
        """The reviewed argv, and the environment the writer constructs for it.

        The child is the only observer of both: production builds that
        environment rather than inheriting it, which is what keeps an ambient
        `SIGSTORE_*`, a token or a proxy from changing what "verified" means.
        """
        record = base / 'cosign-record'
        recording = write_cosign(base / 'cosign-recording', record=record)
        repository = repo('verifier')
        stage = repository.stage_directory(base / 'verifier' / 'stage')
        finished = run_writer(['--version', RELEASE, '--stage-dir', str(stage),
                               '--cosign', str(recording),
                               '--fixture-upstream', str(upstream)],
                              root=repository.root,
                              env={'TMPDIR': str(tmp('verifier'))})
        require(finished.returncode == 0,
                f'the recording verifier was refused: {finished.stderr.strip()}')
        runs = read_cosign_record(record)
        require(len(runs) == len(TARGETS), f'the verifier ran {len(runs)} times')
        for run in runs:
            arguments = run['argv']
            require(arguments[:1] == ['verify-blob'],
                    f'the verifier was not asked to verify a blob: {arguments}')
            require(arguments[1] == '--bundle', f'no bundle: {arguments}')
            require(arguments[3] == '--certificate-identity', f'no identity: {arguments}')
            require(arguments[5] == '--certificate-oidc-issuer', f'no issuer: {arguments}')
            require(arguments[4] == identity(RELEASE),
                    f'the identity is not the reviewed one: {arguments[4]}')
            require(arguments[6] == ISSUER,
                    f'the issuer is not the reviewed one: {arguments[6]}')
            require(arguments[2].endswith('.sigstore'),
                    f'the bundle is not a .sigstore file: {arguments[2]}')
            require(arguments[7].endswith('.tar.gz'),
                    f'the payload is not the package archive: {arguments[7]}')
            # `PWD`, `OLDPWD`, `SHLVL` and `_` are the verifying shell's own, not
            # inherited configuration: everything else has to be the four entries
            # production constructed, so no ambient trust setting survives.
            inherited = [name for name in run['env'] if name not in SHELL_LOCALS]
            require(inherited == ['HOME', 'LC_ALL', 'PATH', 'XDG_CACHE_HOME'],
                    'the verifier inherited more than the constructed environment: '
                    f'{run["env"]}')

    def stamps_the_utc_date() -> None:
        """The pin date is the UTC date, whatever the host's local date is.

        Kiritimati is UTC+14 and Etc/GMT+12 is UTC-12, so between them every hour
        of the UTC day is covered by a host whose local date differs: a writer that
        stamped its own local calendar is caught here in whichever zone differs.
        Each run is one process in one real zone, which is the only place a local
        date exists at all.
        """
        for zone in CLOCK_ZONES:
            name = f'clock-{zone.replace("/", "-")}'
            for attempt in (1, 2):
                repository = repo(name)
                before = utc_today()
                stage = repository.stage_directory(base / name / 'stage')
                finished = run_writer(['--version', RELEASE, '--stage-dir',
                                       str(stage), '--cosign', str(cosign),
                                       '--fixture-upstream', str(upstream)],
                                      root=repository.root,
                                      env={'TZ': zone, 'TMPDIR': str(tmp(name))})
                require(finished.returncode == 0,
                        f'staging under TZ={zone} failed: {finished.stderr.strip()}')
                stamped = json.loads((stage / 'pin.json').read_text())
                stamped = stamped['authentication']['verified_on']
                if utc_today() != before and attempt == 1:
                    continue
                require(stamped == before.isoformat(),
                        f'under TZ={zone} the writer stamped {stamped}, not the UTC '
                        f'date {before}')
                # What it stamped has to verify from this closure's own directory.
                accepts(['check', '--pin-dir', str(stage), '--release', RELEASE])
                break
            else:
                raise AssertionError('the UTC date rolled over twice while this case ran')

    def usage() -> None:
        repository = repo('usage').install(template)
        cases = (
            (['--check', '--version', RELEASE], '--check takes no other argument'),
            (['--check', '--cosign', str(cosign)], '--check takes no other argument'),
            (['--probe-cosign', str(cosign), '--version', RELEASE],
             '--probe-cosign takes only --root'),
            (['--publish-stage', str(base / 'usage' / 'stage'), '--version', RELEASE],
             '--publish-stage takes only --root'),
            ([], '--version is required for a write run'),
        )
        for arguments, needle in cases:
            finished = run_writer(arguments, root=repository.root)
            require(finished.returncode == 2,
                    f'{arguments} exited {finished.returncode}, want 2: '
                    f'{finished.stderr.strip()}')
            require(finished.stderr.startswith(WRITER_PREFIX),
                    f'{arguments} is not prefixed with {WRITER_PREFIX!r}: '
                    f'{finished.stderr!r}')
            require(needle in finished.stderr,
                    f'{arguments} did not mention {needle!r}: {finished.stderr}')

    def inexact_release() -> None:
        repository = repo('release').install(template)
        refuses(['--version', 'rust-v0.159', '--stage-dir',
                 str(base / 'release' / 'stage'), '--cosign', str(cosign),
                 '--fixture-upstream', str(upstream)],
                ('not an exact Codex version',), prefix=WRITER_PREFIX,
                run=lambda arguments: run_writer(arguments, root=repository.root,
                                                 env={'TMPDIR': str(tmp('release'))}))

    def transport_request_set() -> list[tuple[str, Callable[[], None]]]:
        """Each canonical document, missing in turn, is named by the refusal.

        The writer's whole upstream contract is six exact URLs. Serving five and
        removing the sixth is the black-box form of "this is the request set": a
        request the writer makes only for a document it names, and a refusal that
        names the URL it asked for.
        """

        def missing(url: str, label: str) -> Callable[[], None]:
            def case() -> None:
                removed = scratch(f'transport-{label.replace(" ", "-")[:24]}-'
                                  f'{sha256(url.encode())[:8]}')
                served = template.write_upstream(removed / 'upstream')
                upstream_path(served, url).unlink()
                (removed / 'tmp').mkdir(parents=True, exist_ok=True)
                repository = Repository(removed / 'repo', release=RELEASE)
                finished = run_writer(['--version', RELEASE, '--stage-dir',
                                       str(removed / 'stage'), '--cosign', str(cosign),
                                       '--fixture-upstream', str(served)],
                                      root=repository.root,
                                      env={'TMPDIR': str(removed / 'tmp')})
                require(finished.returncode == 1,
                        f'a missing {label} exited {finished.returncode}: '
                        f'{finished.stdout.strip()} {finished.stderr.strip()}')
                require(finished.stderr.startswith(WRITER_PREFIX),
                        f'the refusal is not prefixed: {finished.stderr!r}')
                require(f'cannot fetch {url}' in finished.stderr,
                        f'the refusal did not name {url}: {finished.stderr}')
                require(not (removed / 'stage').exists(),
                        'a refused staging run left a staging directory')
            return case

        documents = [(api_url(RELEASE), 'the exact-tag release document')]
        documents.append((asset_url(RELEASE, CHECKSUMS_ASSET), CHECKSUMS_ASSET))
        for target in TARGETS.values():
            documents.append((asset_url(RELEASE, asset_name(target)), asset_name(target)))
            documents.append((asset_url(RELEASE, bundle_name(target)), bundle_name(target)))
        return [(f'writer: a missing upstream document is refused by its URL '
                 f'({label})', missing(url, label)) for url, label in documents]

    for label, function in (
            ('writer: --check accepts the committed closure',
             check_accepts_the_closure),
            ('writer: --check refuses a Dockerfile with no Codex ARG', lambda: check_refuses(
                'no-slot', 'FROM scratch\nLABEL org.agent-vm.version.codex="x"\n',
                'exactly one `ARG AGENT_VERSION_CODEX=')),
            ('writer: --check refuses a second Codex ARG', lambda: check_refuses(
                'two-slots',
                DOCKERFILE_TEMPLATE.format(release=RELEASE)
                + f'ARG AGENT_VERSION_CODEX={RELEASE}\n',
                'exactly one `ARG AGENT_VERSION_CODEX=')),
            ('writer: --check refuses a Dockerfile whose label does not read the ARG',
             lambda: check_refuses(
                 'no-label',
                 DOCKERFILE_TEMPLATE.format(release=RELEASE).replace(
                     'LABEL org.agent-vm.version.codex="${AGENT_VERSION_CODEX}"',
                     'LABEL org.agent-vm.version.codex="static"'),
                 'must reference ${AGENT_VERSION_CODEX}')),
            ('writer: the CI refusal precedes the fetch', ci_refusal),
            ('writer: the CI refusal precedes publishing', publish_stage_ci_refusal),
            ('writer: --probe-cosign accepts the reviewed verifier', probe_accepts),
            ('writer: --probe-cosign refuses another version', probe_refuses_a_version),
            ('writer: --probe-cosign refuses a non-executable',
             probe_refuses_a_non_executable),
            ('writer: --stage-dir writes the six-file proposal', six_file_proposal),
            ('writer: --stage-dir keeps the caller\'s files',
             keeps_the_callers_files),
            ('writer: --stage-dir prints unchanged for a byte-identical repeat',
             unchanged_repeat),
            ('writer: --publish-stage publishes the proposal', publishes_the_proposal),
            ('writer: the verifier gets the reviewed argv and environment',
             verifier_contract),
            ('writer: --publish-stage refuses an unrelated Dockerfile edit',
             publish_refuses_a_foreign_arg),
            ('writer: a same-release rewrite is refused', same_release_rewrite),
            ('writer: a refusing verifier stages nothing', refused_verifier),
            ('writer: the pin date is the UTC date in either extreme zone',
             stamps_the_utc_date),
            ('writer: a malformed invocation exits 2 with the writer prefix', usage),
            ('writer: a released tag that is not exact is refused', inexact_release)):
        run_case(label, function)
    for label, function in transport_request_set():
        run_case(label, function)


# --- driver ------------------------------------------------------------------

def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser()
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
        try:
            write_installer_fixture(Path(arguments.write_installer_fixture),
                                    arguments.release, arguments.target,
                                    Path(arguments.native_script),
                                    arguments.package_version,
                                    arguments.package_target)
        except AssertionError as error:
            print(f'codex-pins: {error}', file=sys.stderr)
            return 2
        print(f'codex-pins: wrote an installer fixture for {arguments.release} '
              f'{arguments.target}')
        return 0
    if arguments.write_fixture is not None:
        try:
            Fixture(Path(arguments.write_fixture), release=arguments.release)
        except AssertionError as error:
            print(f'codex-pins: {error}', file=sys.stderr)
            return 2
        print(f'codex-pins: wrote a synthetic {arguments.release} closure in '
              f'{arguments.write_fixture}')
        return 0
    run = Path(tempfile.mkdtemp(prefix='codex-pins.')).resolve()
    work.append(run)
    try:
        committed_cases()
        reader_cases()
        preflight_cases()
        writer_cases()
    finally:
        for directory in work:
            shutil.rmtree(directory, ignore_errors=True)
    if failures:
        print(f'codex-pins: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print(f'codex-pins: {len(cases)} case(s) passed')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
