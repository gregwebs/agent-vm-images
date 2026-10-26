#!/usr/bin/env python3
"""Signed synthetic Rust distribution for the hermetic suites.

Upstream payloads are hundreds of megabytes and private to no one, so the tests
build a tiny but *validly signed* two-architecture distribution: real xz
archives with real rust-installer version-3 metadata, real detached OpenPGP
signatures from an invocation-owned key, and a manifest in the signed schema
``rust_toolchain.py`` validates. Nothing here is a production key or upstream
byte, and nothing here relaxes a production check: the tests drive the real
model through its documented transport/runner/dependency seams.
"""
from __future__ import annotations

import hashlib
import io
import os
import shutil
import subprocess
import tarfile
from dataclasses import dataclass, field
from pathlib import Path

CHANNEL = '9.9.9'
DATE = '2026-01-02'
RUSTUP_VERSION = '1.28.2'
RUST_VERSION = f'{CHANNEL} (000000000 2026-01-01)'
HOSTS = {'amd64': 'x86_64-unknown-linux-gnu', 'arm64': 'aarch64-unknown-linux-gnu'}
MUSL = {'amd64': 'x86_64-unknown-linux-musl', 'arm64': 'aarch64-unknown-linux-musl'}
# `rust-mingw` is in the minimal profile but is published only for Windows, so
# upstream lists it as a per-target extension that a Linux host never matches.
# A wildcard row would make rustup select it as a target-independent component.
WINDOWS = {'amd64': 'x86_64-pc-windows-gnu', 'arm64': 'aarch64-pc-windows-gnu'}
MACHINES = {'amd64': 62, 'arm64': 183}
STEMS = {'cargo': 'cargo', 'clippy-preview': 'clippy', 'rust-std': 'rust-std',
         'rustc': 'rustc', 'rustfmt-preview': 'rustfmt'}
EXECUTABLES = {'cargo': ('bin/cargo',), 'clippy-preview': ('bin/cargo-clippy', 'bin/clippy-driver'),
               'rustc': ('bin/rustc',), 'rustfmt-preview': ('bin/cargo-fmt', 'bin/rustfmt')}


def elf_header(machine: int) -> bytes:
    header = bytearray(b'\x7fELF')
    header.append(2)  # 64-bit
    header.append(1)  # little-endian
    header.append(1)
    header.extend(b'\x00' * 9)
    header.extend(b'\x02\x00')  # e_type = ET_EXEC
    header.extend(machine.to_bytes(2, 'little'))
    header.extend(b'\x00' * 4)
    return bytes(header)


def selection(arch: str) -> list[tuple[str, str]]:
    host, musl = HOSTS[arch], MUSL[arch]
    return [('cargo', host), ('clippy-preview', host), ('rust-std', host),
            ('rust-std', musl), ('rustc', host), ('rustfmt-preview', host)]


def archive_name(arch: str, package: str, target: str) -> str:
    return f'{STEMS[package]}-{CHANNEL}-{target}.tar.xz'


def component_name(package: str, target: str) -> str:
    return f'rust-std-{target}' if package == 'rust-std' else package


def payload_directives(arch: str, package: str, target: str,
                      nonce: bytes = b'') -> tuple[list[tuple[str, bytes]], list[str]]:
    component = component_name(package, target)
    if package == 'rust-std':
        rel = f'lib/rustlib/{target}/lib/libstd-00000000.rlib'
        return [(rel, b'rlib-fixture'), (f'lib/rustlib/{target}/lib/libcore-00000000.rlib', b'core')], \
            [f'file:{rel}', f'file:lib/rustlib/{target}/lib/libcore-00000000.rlib']
    payload = []
    directives = []
    for rel in EXECUTABLES[package]:
        payload.append((rel, elf_header(MACHINES[arch])))
        directives.append(f'file:{rel}')
    lib = f'lib/lib{STEMS[package]}-00000000.so'
    payload.append((lib, b'shared' + nonce))
    directives.append(f'file:{lib}')
    return payload, directives


def build_archive(arch: str, package: str, target: str,
                  mutate: bool = False, nonce: bytes = b'') -> bytes:
    prefix = archive_name(arch, package, target)[:-len('.tar.xz')]
    component = component_name(package, target)
    payload, directives = payload_directives(arch, package, target, nonce)
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:xz', format=tarfile.GNU_FORMAT) as archive:
        def add(name: str, data: bytes, mode: int = 0o644,
                kind: bytes = tarfile.REGTYPE) -> None:
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = mode
            info.type = kind
            archive.addfile(info, io.BytesIO(data))

        add(prefix, b'', 0o755, tarfile.DIRTYPE)
        add(f'{prefix}/rust-installer-version', b'3\n')
        add(f'{prefix}/components', (component + '\n').encode())
        add(f'{prefix}/{component}/manifest.in', ('\n'.join(directives) + '\n').encode())
        for rel, data in payload:
            add(f'{prefix}/{component}/{rel}', b'corrupt' if mutate and rel == payload[0][0] else data,
                0o755 if rel.startswith('bin/') else 0o644)
    return buffer.getvalue()


# The rust package's per-host tables. rustup 1.28.2 merges `extensions` into
# `components` (forcing `is_extension = true`) and searches that one vector, so
# the fixture carries the same shape as an upstream manifest: `components` holds
# the profile candidates (including rust-docs, which no profile we request
# selects) and `extensions` holds the explicitly requestable packages, the musl
# standard library and the Windows-only `rust-mingw` profile entry.
COMPONENTS = ('rustc', 'rust-std', 'cargo', 'rust-docs')
EXTRA_EXTENSIONS = ('rust-src',)
# Every package the merged tables name, whether or not a profile selects it.
# rustup's `validate()` walks all of them, and a `Package` needs a `version`.
REFERENCED_PACKAGES = ('cargo', 'clippy-preview', 'rust', 'rust-docs', 'rust-mingw',
                       'rust-src', 'rust-std', 'rustc', 'rustfmt-preview')
# rustup resolves an unselected reference just as strictly as a selected one, so
# the packages nothing downloads still need a target record. `available = false`
# is the upstream shape for a target that exists but is not published.
UNAVAILABLE_TARGETS = (('rust-docs', HOSTS), ('rust-mingw', WINDOWS))


def manifest_text(hashes: dict[str, str]) -> str:
    lines = ['manifest-version = "2"', f'date = "{DATE}"', '', '[profiles]',
             'minimal = ["rustc", "cargo", "rust-std", "rust-mingw"]', '',
             '[renames.clippy]', 'to = "clippy-preview"', '',
             '[renames.rustfmt]', 'to = "rustfmt-preview"', '']
    for package in REFERENCED_PACKAGES:
        lines += [f'[pkg.{package}]', f'version = "{RUST_VERSION}"', '']
    for arch in HOSTS:
        host = HOSTS[arch]
        gz, xz = hashes[(arch, 'rust', host)]
        lines += [f'[pkg.rust.target.{host}]', 'available = true',
                  f'url = "https://static.rust-lang.org/dist/{DATE}/rust-{CHANNEL}-{host}.tar.gz"',
                  f'hash = "{gz}"',
                  f'xz_url = "https://static.rust-lang.org/dist/{DATE}/rust-{CHANNEL}-{host}.tar.xz"',
                  f'xz_hash = "{xz}"', '']
        for component in COMPONENTS:
            lines += [f'[[pkg.rust.target.{host}.components]]', f'pkg = "{component}"',
                      f'target = "{host}"', '']
        for extension in EXTRA_EXTENSIONS:
            lines += [f'[[pkg.rust.target.{host}.extensions]]', f'pkg = "{extension}"',
                      'target = "*"', '']
        for extension, target in (('rust-std', MUSL[arch]), ('clippy-preview', host),
                                  ('rustfmt-preview', host), ('rust-mingw', WINDOWS[arch])):
            lines += [f'[[pkg.rust.target.{host}.extensions]]', f'pkg = "{extension}"',
                      f'target = "{target}"', '']
    for package, hosts in UNAVAILABLE_TARGETS:
        for arch in HOSTS:
            lines += [f'[pkg.{package}.target.{hosts[arch]}]', 'available = false', '']
    # `target = "*"` deserializes to rustup's target-independent component, whose
    # lookup requires the package to be a single wildcard-targeted package.
    lines += ['[pkg.rust-src.target."*"]', 'available = false', '']
    for arch in HOSTS:
        for package, target in selection(arch):
            gz, xz = hashes[(arch, package, target)]
            lines += [f'[pkg.{package}.target.{target}]', 'available = true',
                      f'url = "https://static.rust-lang.org/dist/{DATE}/'
                      f'{STEMS[package]}-{CHANNEL}-{target}.tar.gz"',
                      f'hash = "{gz}"',
                      f'xz_url = "https://static.rust-lang.org/dist/{DATE}/'
                      f'{STEMS[package]}-{CHANNEL}-{target}.tar.xz"',
                      f'xz_hash = "{xz}"', '']
    return '\n'.join(lines) + '\n'


def unresolved_references(text: str) -> list[str]:
    """Every problem rustup 1.28.2's `Manifest::validate()` would raise.

    `src/dist/manifest.rs` requires, for each component row of every package,
    both the referenced package and the record its `target` names; `"*"` means
    `get_target(None)`, which only a single wildcard-targeted package satisfies
    (`PackageTargets::from`). It also needs a `version` on every `Package` and an
    `available` flag on every target. This check is independent of the shipped
    module: it exists so a fixture the tests trust is one a real rustup accepts,
    not only one our own reader accepts.
    """
    import tomllib

    document = tomllib.loads(text)
    packages = document.get('pkg', {})
    problems: list[str] = []
    for name, package in sorted(packages.items()):
        if type(package.get('version')) is not str:
            problems.append(f'pkg.{name}: no version')
        targets = package.get('target', {})
        for target, record in sorted(targets.items()):
            if 'available' not in record:
                problems.append(f'pkg.{name}.target.{target}: no available flag')
            rows = list(record.get('components', [])) + list(record.get('extensions', []))
            for row in rows:
                referenced = row.get('pkg')
                wanted = row.get('target', '*')
                if referenced is None:
                    problems.append(f'pkg.{name}.target.{target}: a row without pkg')
                    continue
                other = packages.get(referenced)
                if other is None:
                    problems.append(f'pkg.{name}.target.{target}: no package {referenced}')
                    continue
                other_targets = other.get('target', {})
                if wanted == '*':
                    if len(other_targets) != 1 or '*' not in other_targets:
                        problems.append(f'pkg.{name}.target.{target}: {referenced} is not '
                                        'a single wildcard-targeted package')
                elif wanted not in other_targets:
                    problems.append(f'pkg.{name}.target.{target}: {referenced} has no '
                                    f'{wanted} target')
    return problems


@dataclass
class Dist:
    """A generated upstream distribution keyed by canonical URL."""

    root: Path
    files: dict[str, bytes] = field(default_factory=dict)
    key: Path = Path()
    fingerprint: str = ''
    gnupg: Path = Path()
    home: Path = Path()

    def add(self, url: str, raw: bytes) -> None:
        self.files[url] = raw

    def url_of(self, name: str) -> str:
        for url in self.files:
            if url.endswith('/' + name):
                return url
        raise KeyError(name)


def gpg_path() -> str:
    found = shutil.which('gpg')
    assert found is not None, 'gpg is a prerequisite for the Rust toolchain suites'
    return found


def gpg(args: list[str], env: dict[str, str]) -> str:
    return subprocess.run([gpg_path(), *args], env=env, check=True, capture_output=True,
                          text=True).stdout


def sign_env(dist: 'Dist') -> dict[str, str]:
    return {'GNUPGHOME': str(dist.gnupg), 'HOME': str(dist.home),
            'PATH': os.path.dirname(gpg_path()) + ':/usr/bin:/bin'}


def armored_sign(path: Path, env: dict[str, str]) -> bytes:
    return subprocess.run([gpg_path(), '--batch', '--no-options', '--armor', '--detach-sign',
                           '--output', '-', str(path)], env=env, check=True,
                          capture_output=True).stdout


def write(path: Path, raw: bytes) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(raw)
    return path


def build(destination: Path) -> Dist:
    """Generate the fixture distribution and an invocation-owned signing key."""
    shutil.rmtree(destination, ignore_errors=True)
    destination.mkdir(parents=True, mode=0o700)
    gnupg = destination / 'gnupg'
    gnupg.mkdir(mode=0o700)
    env = {'GNUPGHOME': str(gnupg), 'PATH': os.path.dirname(gpg_path()) + ':/usr/bin:/bin',
           'HOME': str(destination / 'home')}
    (destination / 'home').mkdir(exist_ok=True)
    gpg(['--batch', '--no-options', '--passphrase', '', '--quick-generate-key',
         'Rust fixture <fixture@example.test>', 'ed25519', 'sign', '0'], env)
    listing = gpg(['--batch', '--no-options', '--with-colons', '--fingerprint',
                   '--list-keys'], env)
    fingerprint = next(line.split(':')[9] for line in listing.splitlines()
                       if line.startswith('fpr:'))
    key_path = destination / 'fixture-key.asc'
    key_path.write_text(gpg(['--batch', '--no-options', '--armor', '--export'], env))
    dist = Dist(root=destination / 'dist', key=key_path, fingerprint=fingerprint,
                gnupg=gnupg, home=destination / 'home')
    return _generate(dist, destination, env)


def respawn(source: Dist, destination: Path, nonce: bytes = b'-second-upstream-build') -> Dist:
    """A later upstream build of the same channel under the same signing key.

    Payload bytes, the manifest and every archive signature differ; the bootstrap
    and the key are byte-identical, so a publisher sees genuine destination
    changes without a bootstrap-exception renewal.
    """
    destination.mkdir(parents=True, exist_ok=True)
    home = destination / 'home'
    home.mkdir(exist_ok=True)
    env = {'GNUPGHOME': str(source.gnupg), 'PATH': os.path.dirname(gpg_path()) + ':/usr/bin:/bin',
           'HOME': str(home)}
    dist = Dist(root=destination / 'dist', key=source.key,
                fingerprint=source.fingerprint, gnupg=source.gnupg, home=home)
    generated = _generate(dist, destination, env, nonce=nonce)
    for arch in HOSTS:
        url = (f'https://static.rust-lang.org/rustup/archive/{RUSTUP_VERSION}/'
               f'{HOSTS[arch]}/rustup-init')
        generated.add(url, source.files[url])
        generated.add(url + '.sha256', source.files[url + '.sha256'])
    return generated


def _generate(dist: Dist, destination: Path, env: dict[str, str],
              nonce: bytes = b'') -> Dist:
    hashes: dict[tuple[str, str, str], tuple[str, str]] = {}
    for arch in HOSTS:
        for package, target in selection(arch):
            raw = build_archive(arch, package, target, nonce=nonce)
            stem = STEMS[package]
            url = (f'https://static.rust-lang.org/dist/{DATE}/'
                   f'{stem}-{CHANNEL}-{target}.tar.xz')
            dist.add(url, raw)
            gz = hashlib.sha256(raw + b'.gz').hexdigest()
            hashes[(arch, package, target)] = (gz, hashlib.sha256(raw).hexdigest())
        host = HOSTS[arch]
        rust_raw = b'rust-payload' + nonce
        dist.add(f'https://static.rust-lang.org/dist/{DATE}/rust-{CHANNEL}-{host}.tar.xz', rust_raw)
        hashes[(arch, 'rust', host)] = (hashlib.sha256(rust_raw + b'.gz').hexdigest(),
                                       hashlib.sha256(rust_raw).hexdigest())
    manifest = manifest_text(hashes).encode()
    manifest_path = write(destination / 'upstream' / 'channel-rust-9.9.9.toml', manifest)
    dist.add(f'https://static.rust-lang.org/dist/channel-rust-{CHANNEL}.toml', manifest)
    dist.add(f'https://static.rust-lang.org/dist/channel-rust-{CHANNEL}.toml.sha256',
             (hashlib.sha256(manifest).hexdigest() + f'  channel-rust-{CHANNEL}.toml\n').encode())
    dist.add(f'https://static.rust-lang.org/dist/channel-rust-{CHANNEL}.toml.asc',
             armored_sign(manifest_path, env))
    for arch in HOSTS:
        bootstrap = elf_header(MACHINES[arch]) + b'bootstrap'
        url = (f'https://static.rust-lang.org/rustup/archive/{RUSTUP_VERSION}/'
               f'{HOSTS[arch]}/rustup-init')
        dist.add(url, bootstrap)
        dist.add(url + '.sha256',
                 f'{hashlib.sha256(bootstrap).hexdigest()} *./rustup-init\n'.encode())
        for package, target in selection(arch):
            archive = dist.files[dist.url_of(f'{STEMS[package]}-{CHANNEL}-{target}.tar.xz')]
            signed = write(destination / 'upstream' / f'{arch}-{package}-{target}.tar.xz', archive)
            dist.add(url_of_archive(package, target) + '.asc', armored_sign(signed, env))
    assert os.access(dist.key, os.R_OK)
    return dist


def save(destination: Path, dist: 'Dist') -> None:
    import base64
    import json
    payload = {'key': str(dist.key), 'fingerprint': dist.fingerprint,
               'gnupg': str(dist.gnupg), 'home': str(dist.home),
               'files': {url: base64.b64encode(raw).decode() for url, raw in dist.files.items()}}
    (destination / 'dist.json').write_text(json.dumps(payload))


def load(destination: Path) -> 'Dist':
    import base64
    import json
    payload = json.loads((destination / 'dist.json').read_text())
    return Dist(root=destination / 'dist', key=Path(payload['key']),
                fingerprint=payload['fingerprint'], gnupg=Path(payload['gnupg']),
                home=Path(payload['home']),
                files={url: base64.b64decode(raw) for url, raw in payload['files'].items()})


def url_of_archive(package: str, target: str) -> str:
    return (f'https://static.rust-lang.org/dist/{DATE}/'
            f'{STEMS[package]}-{CHANNEL}-{target}.tar.xz')
