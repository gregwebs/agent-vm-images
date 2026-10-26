#!/usr/bin/env python3
"""Fixture adapters for the hermetic Rust toolchain suites.

Two seams exist in the production module: transport (HTTPS vs this byte store)
and command execution (real processes vs this recorder). Everything else —
strict decoding, canonical-origin derivation, signature verification with real
gpg, archive preflight, install ordering and the installed-state checks — runs as
shipped. There is deliberately no "skip verification" switch to reach for.
"""
from __future__ import annotations

import hashlib
import os
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Sequence

REPOSITORY_ROOT = Path(__file__).resolve().parents[4]
# Appended, not prepended: a test that imports the modules from its own temporary
# tree must win the lookup.
for _path in (Path(__file__).resolve().parent,
              REPOSITORY_ROOT / 'script' / 'dev',
              REPOSITORY_ROOT / 'examples' / 'layers' / 'rust-dev'):
    if str(_path) not in sys.path:
        sys.path.append(str(_path))

import fixture  # noqa: E402
import rust_toolchain as model  # noqa: E402
import importlib.util  # noqa: E402


def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    # Registered before execution: dataclasses resolves string annotations through
    # ``sys.modules`` (Python 3.13), and an unregistered module makes that lookup
    # fail rather than the module under test.
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def load_mutant(name: str, replacements: Sequence[tuple[str, str]], root: Path):
    """A scratch copy of the shipped module with the named edits applied.

    Mutants of the *production* code are how the suite proves its oracles
    discriminate; nothing here replaces a check.
    """
    source = (REPOSITORY_ROOT / 'examples' / 'layers' / 'rust-dev'
              / 'rust_toolchain.py').read_text()
    for old, new in replacements:
        if old not in source:
            raise AssertionError(f'mutant {name}: pattern not found: {old[:60]!r}')
        source = source.replace(old, new, 1)
    root.mkdir(parents=True, exist_ok=True)
    path = root / f'{name}.py'
    path.write_text(source)
    module = _load(f'rust_toolchain_{name.replace("-", "_")}', path)
    register_layouts(module)
    return module


# Bootstrap paths the verified layouts of this process own. Dispatch is by this
# identity, never by the program's basename: `/unregistered/rustup-init` with the
# documented argv must be refused, which a suffix test cannot do.
REGISTERED_BOOTSTRAPS: set[str] = set()


def register_layouts(module) -> None:
    """Record the bootstrap of every layout ``module.prepare`` hands back.

    The shipped CLI prepares and installs internally, so a suite that drives the
    CLI registers at that one seam; a suite that calls ``prepare`` itself gets the
    same registration because this wraps it either way.
    """
    original = module.prepare
    if getattr(original, 'registers_layouts', False):
        return

    def prepare(*arguments, **keywords):
        layout = original(*arguments, **keywords)
        REGISTERED_BOOTSTRAPS.add(os.path.realpath(str(layout.bootstrap)))
        return layout

    prepare.registers_layouts = True  # type: ignore[attr-defined]
    module.prepare = prepare


updater = _load('rust_toolchain_update', REPOSITORY_ROOT / 'script' / 'dev'
                / 'rust-toolchain-update.py')


class FixtureTransport:
    def __init__(self, files: dict[str, bytes]) -> None:
        self.files = dict(files)
        self.fetches: list[str] = []
        self.faults: dict[str, Callable[[bytes], bytes]] = {}
        self.failures: dict[str, str] = {}

    def replace(self, url: str, raw: bytes) -> None:
        self.files[url] = raw

    def fault(self, url: str, mutate: Callable[[bytes], bytes]) -> None:
        self.faults[url] = mutate

    def fail(self, url: str, message: str) -> None:
        self.failures[url] = message

    def fetch(self, url: model.CanonicalUrl, destination: Path, max_bytes: int) -> None:
        self.fetches.append(url.value)
        if url.value in self.failures:
            raise model.ToolchainError(f'fetch {url.value}: {self.failures[url.value]}')
        if url.value not in self.files:
            raise model.ToolchainError(f'fetch {url.value}: refused endpoint')
        raw = self.files[url.value]
        if url.value in self.faults:
            raw = self.faults[url.value](raw)
        if len(raw) > max_bytes:
            raise model.ToolchainError(f'fetch {url.value}: response exceeds {max_bytes} bytes')
        destination.write_bytes(raw)


class UnrecognizedCommand(model.ToolchainError):
    """A command the recorder does not model.

    Fail closed: inventing success for an unknown argv is how an added live
    downloader, a retry against the real dist server or a stray interpreter gets
    through a green suite.
    """


# Banned executables/tokens: a call containing any of these is not something an
# install may ever do, whatever a mutant adds.
FORBIDDEN_CALL_TOKENS = ('curl', 'wget', 'apt-get', 'apt ', 'npm', 'npx', 'sh.rustup.rs')


@dataclass
class RecordingRunner:
    """Records every process invocation and emulates only rustup/bootstrap I/O."""

    arch: model.Architecture
    channel: str
    rustup_version: str
    calls: list[tuple[list[str], dict[str, str], str | None]] = field(default_factory=list)
    fail_when: Callable[[Sequence[str]], bool] | None = None
    omit_components: tuple[str, ...] = ()
    extra_toolchains: tuple[str, ...] = ()
    settings_override: dict[str, str] = field(default_factory=dict)
    # The real gpg this process trusts, resolved once by the fixture.
    gpg: str = field(default_factory=lambda: _gpg_path())

    def run(self, argv: Sequence[str], env: dict[str, str],
            cwd: Path | None = None) -> subprocess.CompletedProcess:
        """Emulate exactly the documented rustup/toolchain calls, or refuse.

        Dispatch is on the executable *and* the complete argument vector. An
        argv-tail match invented success for any program invoked with a recognized
        tail (`/unregistered/program clippy --version` returned a fabricated
        clippy), which is not a fixture the module under test could be trusted
        against. The tools must also come from the install homes the caller named,
        so a stray binary elsewhere is not "rustup".
        """
        argv = [str(item) for item in argv]
        self.calls.append((argv, dict(env), None if cwd is None else str(cwd)))
        executable, arguments = argv[0], argv[1:]
        name = os.path.basename(executable)
        if os.path.realpath(executable) == self.gpg:
            # Real gpg is delegated by its resolved identity, not by its name: a
            # foreign program called `gpg` is not the trusted tool.
            return subprocess.run(argv, env=env, capture_output=True, text=True)
        if self.fail_when is not None and self.fail_when(argv):
            return subprocess.CompletedProcess(argv, 1, '', 'injected failure')
        homes = model.InstallHomes(Path(env['RUSTUP_HOME']), Path(env['CARGO_HOME']))
        if name == 'rustup-init':
            expected = ['-y', '--no-modify-path', '--default-host', self.arch.host.value,
                        '--default-toolchain', 'none', '--profile', 'minimal']
            if (os.path.realpath(executable) not in REGISTERED_BOOTSTRAPS
                    or arguments != expected):
                raise UnrecognizedCommand(
                    f'fixture runner: {executable!r} is not the bootstrap of a verified '
                    f'layout this process prepared, with {arguments!r}')
            (homes.cargo / 'bin').mkdir(parents=True, exist_ok=True)
            (homes.cargo / 'bin' / 'rustup').write_text('#!/bin/sh\n')
            self._write_settings(homes, default_toolchain=None)
            return subprocess.CompletedProcess(argv, 0, '', '')
        if executable != str(homes.cargo / 'bin' / name):
            raise UnrecognizedCommand(
                f'fixture runner: {executable!r} is not a tool from the install homes')
        if name == 'rustup':
            if arguments == ['--version']:
                return subprocess.CompletedProcess(
                    argv, 0, f'rustup {self.rustup_version} (fixture)\n', '')
            if arguments == ['set', 'auto-self-update', 'disable']:
                self._update_settings(homes, auto_self_update='disable')
                return subprocess.CompletedProcess(argv, 0, '', '')
            if arguments == ['set', 'auto-install', 'disable']:
                self._update_settings(homes, auto_install='disable')
                return subprocess.CompletedProcess(argv, 0, '', '')
            install = [self.channel, '--profile', 'minimal', '--component', 'clippy,rustfmt',
                       '--target', self.arch.musl.value, '--no-self-update']
            if arguments == ['toolchain', 'install'] + install:
                self._create_toolchain(homes)
                return subprocess.CompletedProcess(argv, 0, '', '')
            if arguments == ['default', self.channel]:
                self._update_settings(
                    homes, default_toolchain=f'{self.channel}-{self.arch.host.value}')
                return subprocess.CompletedProcess(argv, 0, '', '')
            if arguments == ['toolchain', 'list']:
                lines = [f'{self.channel}-{self.arch.host.value} (active, default)']
                lines.extend(self.extra_toolchains)
                return subprocess.CompletedProcess(argv, 0, '\n'.join(lines) + '\n', '')
            if arguments == ['component', 'list', '--installed']:
                names = [f'{fixture.STEMS[package]}-{target.value}'
                         for package, target in model.selected_pairs(self.arch)
                         if package not in self.omit_components]
                return subprocess.CompletedProcess(argv, 0, '\n'.join(names) + '\n', '')
            if arguments == ['target', 'list', '--installed']:
                targets = [self.arch.host.value, self.arch.musl.value]
                if 'musl' in self.omit_components:
                    targets = [self.arch.host.value]
                return subprocess.CompletedProcess(argv, 0, '\n'.join(targets) + '\n', '')
        elif name == 'cargo':
            if arguments == ['--version']:
                return subprocess.CompletedProcess(argv, 0, 'cargo 1.0.0 (fixture)\n', '')
            if arguments == ['clippy', '--version']:
                return subprocess.CompletedProcess(argv, 0, 'clippy 0.1.0 (fixture)\n', '')
        elif name == 'rustfmt':
            if arguments == ['--version']:
                return subprocess.CompletedProcess(argv, 0, 'rustfmt 1.0.0 (fixture)\n', '')
        elif name == 'rustc':
            if arguments == ['-vV']:
                text = (f'rustc 1.0.0\nbinary: rustc\nrelease: {self.channel}\n'
                        f'host: {self.arch.host.value}\n')
                return subprocess.CompletedProcess(argv, 0, text, '')
        raise UnrecognizedCommand(f'fixture runner: unrecognized command {argv!r}')

    def _settings_path(self, homes: model.InstallHomes) -> Path:
        return homes.rustup / 'settings.toml'

    def _write_settings(self, homes: model.InstallHomes, default_toolchain: str | None) -> None:
        homes.rustup.mkdir(parents=True, exist_ok=True)
        values = {'version': '12', 'default_host_triple': self.arch.host.value,
                  'default_toolchain': default_toolchain or 'none', 'profile': 'minimal',
                  'auto_self_update': 'disable', 'auto_install': 'disable'}
        values.update(self.settings_override)
        body = ''.join(f'{key} = "{value}"\n' for key, value in values.items())
        self._settings_path(homes).write_text(body + '\n[overrides]\n')

    def _update_settings(self, homes: model.InstallHomes, **updates: str) -> None:
        if not self._settings_path(homes).exists():
            self._write_settings(homes, default_toolchain=None)
        lines = self._settings_path(homes).read_text().splitlines()
        result = []
        for line in lines:
            key = line.split(' = ', 1)[0]
            if key in updates:
                result.append(f'{key} = "{updates[key]}"')
            else:
                result.append(line)
        self._settings_path(homes).write_text('\n'.join(result) + '\n')

    def _create_toolchain(self, homes: model.InstallHomes) -> None:
        root = homes.rustup / 'toolchains' / f'{self.channel}-{self.arch.host.value}'
        for target in (self.arch.host, self.arch.musl):
            lib = root / 'lib' / 'rustlib' / target.value / 'lib'
            lib.mkdir(parents=True, exist_ok=True)
            (lib / 'libstd-fixture.rlib').write_bytes(b'rlib')

    def executed(self) -> list[str]:
        return [' '.join(argv) for argv, _, _ in self.calls]

    def executed_substrings(self) -> str:
        return '\n'.join(self.executed())

    def foreign_calls(self) -> list[str]:
        return [line for line in self.executed()
                if any(token in line for token in FORBIDDEN_CALL_TOKENS)]


@dataclass
class Fixture:
    dist: fixture.Dist
    trust: model.TrustPolicy
    pinned: Path
    transport: FixtureTransport
    root: Path

    def verifier(self, work: Path) -> model.SignatureVerifier:
        return model.SignatureVerifier(model.GpgProgram(_gpg_path()), self.dist.key, work,
                                       model.ProcessRunner(), self.trust)

    def dependencies(self, runner: model.Runner | None = None) -> model.Dependencies:
        return model.Dependencies(transport=self.transport,
                                  runner=runner or RecordingRunner(model.Architecture.ARM64,
                                                                   fixture.CHANNEL,
                                                                   fixture.RUSTUP_VERSION),
                                  gpg=model.GpgProgram(_gpg_path()), trust=self.trust)


def _gpg_path() -> str:
    import shutil
    found = shutil.which('gpg')
    assert found is not None, 'gpg is a prerequisite'
    return os.path.realpath(found)


def build(root: Path) -> Fixture:
    root.mkdir(parents=True, exist_ok=True)
    fixture_destination = root / 'upstream-fixture'
    dist = fixture.build(fixture_destination)
    fixture.save(fixture_destination, dist)
    trust = model.TrustPolicy(primary=model.Fingerprint(dist.fingerprint),
                              signers=(model.Fingerprint(dist.fingerprint),))
    transport = FixtureTransport(dist.files)
    dependencies = model.Dependencies(transport=transport,
                                      runner=RecordingRunner(model.Architecture.ARM64,
                                                             fixture.CHANNEL,
                                                             fixture.RUSTUP_VERSION),
                                      gpg=model.GpgProgram(_gpg_path()), trust=trust)
    dockerfile = root / 'Dockerfile'
    dockerfile.write_text(f'ARG RUST_TOOLCHAIN={fixture.CHANNEL}\nFROM fixture\n')
    out = root / 'pinned'
    updater.stage(updater.StageRequest(
        channel=model.Channel(fixture.CHANNEL),
        rustup=model.Semver(fixture.RUSTUP_VERSION),
        key=dist.key,
        current=root / 'absent',
        out=out,
        reviewed_on=model.IsoDate('2026-01-02'),
        reviewer='fixture-reviewer'), dependencies, dockerfile=dockerfile)
    return Fixture(dist=dist, trust=trust, pinned=out / 'rust-toolchain',
                   transport=transport, root=root)


def load_fixture(destination: Path, pinned: Path) -> Fixture:
    """A Fixture over a saved distribution (e.g. a second upstream build)."""
    dist = fixture.load(destination)
    trust = model.TrustPolicy(primary=model.Fingerprint(dist.fingerprint),
                              signers=(model.Fingerprint(dist.fingerprint),))
    return Fixture(dist=dist, trust=trust, pinned=pinned,
                   transport=FixtureTransport(dist.files), root=destination)


def copy_pin(source: Path, destination: Path) -> Path:
    import shutil
    shutil.rmtree(destination, ignore_errors=True)
    shutil.copytree(source, destination)
    return destination


def digest_tree(directory: Path) -> dict[str, str]:
    return {path.relative_to(directory).as_posix(): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted(directory.rglob('*')) if path.is_file()}
