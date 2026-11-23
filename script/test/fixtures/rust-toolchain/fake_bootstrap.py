#!/usr/bin/env python3
"""The fixture bootstrap the Go install row executes.

The differential runs the shipped Go binary, so its bootstrap call cannot be
intercepted in-process. ``--fixture-bootstrap-exec`` names this program; the Go
side then runs ``[fake-bootstrap, <verified rustup-init>, <argv...>]``. This
program validates the bootstrap the caller is about to run, writes the rustup
settings a real ``rustup-init`` would write, and links the installed tools into
``CARGO_HOME/bin`` so the subsequent rustup/cargo/rustc/rustfmt calls reach the
allowlisted emulator.

Fail closed: an unexpected argv, a bootstrap outside the row's work area, a mode
other than 0755 or a digest that is not the pin's bootstrap exits 97.
"""
from __future__ import annotations

import hashlib
import json
import os
import sys

UNRECOGNIZED = 97
# The install environment is scrubbed, so the tool root is this program's own
# location, not an inherited variable.
TOOLBOX = os.path.dirname(os.path.dirname(os.path.realpath(sys.argv[0])))
HOST_ARG = '--default-host'


def refuse(message: str) -> int:
    print(f'fake-bootstrap: {message}', file=sys.stderr)
    return UNRECOGNIZED


def digest(path: str) -> str:
    with open(path, 'rb') as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def main() -> int:
    argv = sys.argv[1:]
    if len(argv) < 2:
        return refuse(f'unexpected argv {argv!r}')
    bootstrap, arguments = argv[0], argv[1:]
    rustup_home = os.environ.get('RUSTUP_HOME', '')
    cargo_home = os.environ.get('CARGO_HOME', '')
    tmpdir = os.environ.get('TMPDIR', '')
    if not rustup_home or not cargo_home:
        return refuse('RUSTUP_HOME and CARGO_HOME are required')
    if HOST_ARG not in arguments:
        return refuse(f'unexpected argv {argv!r}')
    host = arguments[arguments.index(HOST_ARG) + 1]
    expected_argv = ['-y', '--no-modify-path', HOST_ARG, host, '--default-toolchain',
                     'none', '--profile', 'minimal']
    if arguments != expected_argv:
        return refuse(f'unexpected bootstrap argv {arguments!r}')
    del tmpdir
    scratch = os.path.dirname(os.path.dirname(bootstrap))
    if not os.path.basename(scratch).startswith('rust-prepare-'):
        return refuse(f'bootstrap {bootstrap!r} is not in an invocation scratch directory')
    if os.stat(bootstrap).st_mode & 0o777 != 0o755:
        return refuse(f'bootstrap {bootstrap!r} is not mode 0755')
    with open(os.path.join(TOOLBOX, 'bootstrap', 'digests.json'),
              encoding='utf-8') as handle:
        expected = json.load(handle)[host]
    if digest(bootstrap) != expected:
        return refuse(f'bootstrap {bootstrap!r} is not the pinned digest')
    os.makedirs(rustup_home, exist_ok=True)
    settings = {
        'version': '12',
        'default_host_triple': host,
        'default_toolchain': 'none',
        'profile': 'minimal',
        'auto_self_update': 'disable',
        'auto_install': 'disable',
    }
    with open(os.path.join(rustup_home, 'settings.toml'), 'w', encoding='utf-8') as handle:
        for key, value in settings.items():
            handle.write(f'{key} = "{value}"\n')
        handle.write('\n[overrides]\n')
    bindir = os.path.join(cargo_home, 'bin')
    os.makedirs(bindir, exist_ok=True)
    tools = os.path.join(TOOLBOX, 'cargo', 'bin')
    for name in ('rustup', 'cargo', 'rustc', 'rustfmt'):
        link = os.path.join(bindir, name)
        if os.path.lexists(link):
            os.unlink(link)
        os.symlink(os.path.join(tools, name), link)
    return 0


if __name__ == '__main__':
    sys.exit(main())
