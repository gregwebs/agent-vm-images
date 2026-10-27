#!/usr/bin/env python3
"""Test-owned ``python3`` for the gopls-closure maintenance suite.

The shell writer resolves ``python3`` from its PATH and runs the real stager file.
This launcher is what that ``python3`` runs: it confines itself in the same OS
boundary the pins suite uses (the stager is the part that contacts upstream and
adopts bytes), installs the audit hook, and imports the *real* updater module,
supplying only the fixture transport origin and executable paths.

Faults are applied to the proxy's byte store by the suite, never to the code under
test. ``FIXTURE_MUTATE_DURING_STAGE`` exists so a control can prove the writer
refuses a destination that changes while a proposal is being staged.
"""
from __future__ import annotations

import os
import sys
import tempfile
import time
from pathlib import Path

sys.dont_write_bytecode = True
HERE = Path(__file__).resolve().parent
FIXTURES = Path(os.environ.get('FIXTURE_FIXTURES', str(HERE)))
TREE = Path(os.environ.get('FIXTURE_TREE', str(FIXTURES.parents[2])))


def confined(sandbox, allowed: list[str]) -> bool:
    """Prove the boundary from inside it: a copied allowed binary must not execute."""
    try:
        return sandbox.exec_of_copy_denied(allowed) == 'denied'
    except Exception:  # noqa: BLE001 - any failure is a missing proof
        return False


def main(argv: list[str]) -> int:
    go_root = Path(os.environ['FIXTURE_GO_ROOT'])
    sys.path.insert(0, str(TREE / 'examples' / 'layers' / 'go-dev'))
    sys.path.insert(0, str(TREE / 'script' / 'dev'))
    sys.path.insert(0, str(FIXTURES))
    # The OS boundary itself lives with the Rust fixtures, which #102 owns and
    # this suite imports unchanged.
    sys.path.insert(0, str(FIXTURES.parent / 'rust-toolchain'))

    import sandbox

    from executables import go_executables

    allowed = go_executables(go_root)
    if not confined(sandbox, allowed):
        if os.environ.get(sandbox.SANDBOXED_ENV) == '1':
            # A forged marker is not a boundary: refuse before
            # reexec_into_sandbox can short-circuit on it and apply only the
            # Linux direct-exec rule.
            print('ISOLATION-UNAVAILABLE: the boundary could not be demonstrated '
                  '(forged marker)', file=sys.stderr)
            return 3
        try:
            sandbox.reexec_into_sandbox(
                allowed,
                writable=[str(TREE), tempfile.gettempdir(), '/tmp', '/private/tmp'],
                readable=[str(FIXTURES), str(TREE), str(go_root)], loopback=True)
        except sandbox.SandboxUnavailable as error:
            print(f'ISOLATION-UNAVAILABLE: {error}', file=sys.stderr)
            return 3
    if not confined(sandbox, allowed):
        # An inherited marker is not a boundary: without a demonstrated one this
        # child refuses rather than resolving modules unconfined.
        print('ISOLATION-UNAVAILABLE: the boundary could not be demonstrated',
              file=sys.stderr)
        return 3
    import guards

    guards.install(allowed)

    import gopls_closure as model  # noqa: F401 - imported after confinement

    updater = _load('gopls_closure_update', TREE / 'script' / 'dev'
                    / 'gopls-closure-update.py')
    if os.environ.get('FIXTURE_SLOW_STAGE'):
        time.sleep(float(os.environ['FIXTURE_SLOW_STAGE']))
    mutate = os.environ.get('FIXTURE_MUTATE_DURING_STAGE')
    if mutate:
        with open(mutate, 'ab') as handle:
            handle.write(b'\n')
    if not argv:
        return 2
    if argv[0].endswith('gopls-closure-update.py'):
        return updater.main(argv[1:], environment(argv[1:]))
    print(f'launcher: unexpected module {argv[0]}', file=sys.stderr)
    return 2


def requested_go(argv: list[str]) -> str:
    """The --go the writer asked for, so a mismatch control can reach Go itself."""
    for index, argument in enumerate(argv):
        if argument == '--go' and index + 1 < len(argv):
            return argv[index + 1]
        if argument.startswith('--go='):
            return argument.split('=', 1)[1]
    return os.environ['FIXTURE_GO_VERSION']


def environment(argv: list[str]):
    updater = sys.modules['gopls_closure_update']
    return updater.MaintenanceEnvironment(
        proxy=os.environ['FIXTURE_PROXY_URL'], sumdb='off',
        toolchain=f'go{requested_go(argv)}',
        host_go=Path(os.environ['FIXTURE_GO_ROOT']) / 'bin' / 'go',
        transport={},
        in_github_actions=os.environ.get('GITHUB_ACTIONS') == 'true')


def _load(name: str, path: Path):
    import importlib.util

    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    previous = sys.dont_write_bytecode
    sys.dont_write_bytecode = True
    try:
        spec.loader.exec_module(module)
    finally:
        sys.dont_write_bytecode = previous
    return module


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
