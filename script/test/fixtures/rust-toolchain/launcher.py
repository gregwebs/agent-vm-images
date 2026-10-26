#!/usr/bin/env python3
"""Test-owned launcher for the maintenance suite.

The writer resolves ``python3`` from its PATH and invokes the real stager file.
This launcher is what that ``python3`` runs: it imports the *real* updater and
example modules and supplies only the fixture transport/runner dependencies, so
no transaction or verification logic is replaced. Faults are applied to the byte
store, never to the code under test.
"""
from __future__ import annotations

import os
import shutil
import sys
import tempfile
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
# The suite copies this launcher into a test-owned bin directory first on the
# writer's PATH; the fixture modules stay in the checkout.
FIXTURES = Path(os.environ.get('FIXTURE_FIXTURES', str(HERE)))
REPOSITORY = Path(os.environ.get('FIXTURE_REPOSITORY', str(FIXTURES.parents[2])))
TREE = Path(os.environ.get('FIXTURE_TREE', str(REPOSITORY)))
# The temporary repository tree's own copies come first: the shipped modules then
# resolve their sibling paths (the Dockerfile, the data directory) inside the tree.
sys.path.insert(0, str(TREE / 'examples' / 'layers' / 'rust-dev'))
sys.path.insert(0, str(TREE / 'script' / 'dev'))
sys.path.insert(0, str(FIXTURES))

# The OS boundary goes in before anything else: a child tree must start confined,
# and the boundary has to cover native grandchildren the audit hook cannot see.
# A host that cannot provide it is a prerequisite failure that exits *before* any
# code under test is imported or run -- a printed diagnostic and a green child
# would let every maintenance case execute unsandboxed.
import sandbox  # noqa: E402

GPG = shutil.which('gpg') or '/usr/bin/gpg'
EXECUTABLES = sandbox.gpg_executables(GPG)
try:
    sandbox.reexec_into_sandbox(
        EXECUTABLES,
        writable=[str(TREE), str(Path(os.environ.get('FIXTURE_DIST', str(TREE))).parent),
                  tempfile.gettempdir(), '/tmp', '/private/tmp'],
        # The modules this child imports live in the checkout, which is not under
        # the scratch tree: the Linux prefix has to expose them read-only.
        readable=[str(FIXTURES), str(REPOSITORY)])
except sandbox.SandboxUnavailable as error:
    print(f'ISOLATION-UNAVAILABLE: {error}', file=sys.stderr)
    sys.exit(3)

# The guard goes in first: the modules under test are what it must constrain, so
# it may not be installed after importing them.
import guards  # noqa: E402

guards.install(EXECUTABLES)

import driver  # noqa: E402
import fixture  # noqa: E402
import rust_toolchain as model  # noqa: E402

assert model.__file__.startswith(str(TREE)), model.__file__
# Bootstrap dispatch is bound to the layout this process verified, so register at
# the one seam every prepared layout passes through.
driver.register_layouts(model)

# The tree's own copy of the stager, so its sibling paths (the Dockerfile it
# rewrites) resolve inside the temporary repository.
updater = driver._load('rust_toolchain_update', TREE / 'script' / 'dev'
                       / 'rust-toolchain-update.py')


def dependencies() -> model.Dependencies:
    destination = Path(os.environ['FIXTURE_DIST'])
    dist = fixture.load(destination)
    trust = model.TrustPolicy(primary=model.Fingerprint(dist.fingerprint),
                              signers=(model.Fingerprint(dist.fingerprint),))
    transport = driver.FixtureTransport(dist.files)
    apply_fault(transport, dist)
    runner = driver.RecordingRunner(model.Architecture.AMD64, fixture.CHANNEL,
                                    fixture.RUSTUP_VERSION)
    return model.Dependencies(transport=transport, runner=runner,
                              gpg=model.GpgProgram(driver._gpg_path()), trust=trust)


def apply_fault(transport, dist) -> None:
    fault = os.environ.get('FIXTURE_FAULT', '')
    if fault == 'bootstrap-arm64':
        transport.fail(f'https://static.rust-lang.org/rustup/archive/{fixture.RUSTUP_VERSION}'
                       '/aarch64-unknown-linux-gnu/rustup-init', 'fixture endpoint refused')
    elif fault == 'manifest-signature':
        transport.replace(f'https://static.rust-lang.org/dist/channel-rust-'
                          f'{fixture.CHANNEL}.toml.asc', b'not a signature')
    elif fault == 'archive-digest':
        for url in sorted(transport.files):
            if url.endswith('.tar.xz'):
                transport.fault(url, lambda raw: raw[:-1] + b'\x00')
                break
    elif fault == 'last-signature':
        signatures = sorted(url for url in transport.files if url.endswith('.tar.xz.asc'))
        transport.replace(signatures[-1], b'not a signature')
    elif fault == 'bootstrap-bytes':
        import hashlib
        url = (f'https://static.rust-lang.org/rustup/archive/{fixture.RUSTUP_VERSION}'
               '/aarch64-unknown-linux-gnu/rustup-init')
        replacement = dist.files[url] + b'\x00'
        transport.replace(url, replacement)
        transport.replace(url + '.sha256',
                          f'{hashlib.sha256(replacement).hexdigest()} *./rustup-init\n'.encode())
    elif fault == 'slow-manifest':
        original = transport.fetch

        def slow(url, destination, max_bytes):
            if url.value.endswith('.toml'):
                time.sleep(60)  # the suite sends SIGTERM while this blocks
            original(url, destination, max_bytes)

        transport.fetch = slow


def guard_self_test() -> int:
    """Prove this child's guard: every forbidden operation must be refused."""
    import socket
    import subprocess

    def attempt(action) -> str:
        try:
            action()
        except RuntimeError:
            return 'denied'
        except OSError:
            return 'allowed'
        return 'allowed'

    outcomes = {
        'socket': attempt(lambda: socket.create_connection(('192.0.2.1', 9), timeout=0.1)),
        'downloader': attempt(lambda: subprocess.run(
            [shutil.which('curl') or '/usr/bin/curl', '--version'], capture_output=True)),
        'nested': attempt(lambda: subprocess.run([sys.executable, '-c', 'pass'],
                                                 capture_output=True)),
        'identity': attempt(lambda: subprocess.run(
            [shutil.which('gpg') or '/usr/bin/gpg', 'MARKER'], executable='/bin/echo',
            capture_output=True)),
    }
    for name, outcome in outcomes.items():
        print(f'{name}={outcome}')
    return 0 if set(outcomes.values()) == {'denied'} else 1


def main(argv: list[str]) -> int:
    if argv[:1] == ['--guard-self-test']:
        return guard_self_test()
    if not argv:
        return 2
    if argv[0].endswith('rust-toolchain-update.py'):
        return updater.main(argv[1:], dependencies())
    if argv[0].endswith('rust_toolchain.py'):
        return model.main(argv[1:], dependencies())
    print(f'launcher: unexpected module {argv[0]}', file=sys.stderr)
    return 2


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
