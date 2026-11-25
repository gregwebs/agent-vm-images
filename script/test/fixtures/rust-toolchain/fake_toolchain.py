#!/usr/bin/env python3
"""Standalone rustup/cargo/rustc/rustfmt emulator for the frozen install rows.

Why it is a program and not a Python seam: the rows run the shipped Go binary,
which cannot be given a fake runner, so this is a copy of one tool name (rustup,
cargo, rustc or rustfmt) that answers from a case file and records what it was
asked. Until #245 a differential ran both implementations against it and compared
the two call logs; only this program is left, so the log is evidence a reader can
inspect rather than something a row compares.

Rules, all of them fail-closed:

* ``argv[0]`` must be ``$CARGO_HOME/bin/<name>``. A tool from anywhere else is not
  the install the caller named, so it is refused rather than answered.
* The answers come from ``$RUSTUP_HOME.case.json`` — a *sibling* of the home, so
  the case never writes into the home it is describing. The case, not this
  emulator, decides the components, the targets, the toolchains and the failures;
  `verify-installed` only reads, so nothing here writes ``settings.toml`` or the
  toolchain tree.
* Every call appends ``{argv, env}`` to ``$RUSTUP_HOME/calls.jsonl``, so what the
  binary asked for, and with what environment, is on disk after a row.
* Anything outside the table exits 97 with ``fixture emulator: unrecognized
  command``, so a command the shipped checks grew cannot pass by accident.

Both halves are here: the read commands `verify-installed` reaches, and the four
install verbs `install` reaches (`set …`, `toolchain install`, `default`). The
install verbs update the same settings and toolchain tree a real rustup would, so
the post-install verification runs against the state the transcript produced.
"""
from __future__ import annotations

import json
import os
import sys

UNRECOGNIZED = 97


def refuse(argv: list[str]) -> int:
    print(f'fixture emulator: unrecognized command {argv!r}', file=sys.stderr)
    return UNRECOGNIZED


def case_for(rustup_home: str) -> dict:
    path = rustup_home + '.case.json'
    with open(path, encoding='utf-8') as handle:
        return json.load(handle)


def settings_path(rustup_home: str) -> str:
    return rustup_home + '/settings.toml'


def update_settings(rustup_home: str, **updates: str) -> None:
    path = settings_path(rustup_home)
    lines = open(path, encoding='utf-8').read().splitlines() if os.path.exists(path) else []
    seen = set()
    result = []
    for line in lines:
        key = line.split(' = ', 1)[0]
        if key in updates:
            result.append(f'{key} = "{updates[key]}"')
            seen.add(key)
        else:
            result.append(line)
    for key, value in updates.items():
        if key not in seen:
            result.insert(0, f'{key} = "{value}"')
    with open(path, 'w', encoding='utf-8') as handle:
        handle.write('\n'.join(result) + '\n')


def create_toolchain(rustup_home: str, channel: str, host: str, musl: str) -> None:
    root = os.path.join(rustup_home, 'toolchains', f'{channel}-{host}')
    for target in (host, musl):
        lib = os.path.join(root, 'lib', 'rustlib', target, 'lib')
        os.makedirs(lib, exist_ok=True)
        with open(os.path.join(lib, 'libstd-fixture.rlib'), 'wb') as handle:
            handle.write(b'rlib')


def record(rustup_home: str, argv: list[str]) -> None:
    # One line per call: append-only, so a row's log is the order the binary ran
    # in.
    with open(rustup_home + '/calls.jsonl', 'a', encoding='utf-8') as handle:
        handle.write(json.dumps({'argv': argv, 'env': dict(os.environ),
                                 'cwd': os.getcwd()},
                                sort_keys=True) + '\n')


def main() -> int:
    argv = sys.argv[1:]
    name = os.path.basename(sys.argv[0])
    cargo_home = os.environ.get('CARGO_HOME', '')
    rustup_home = os.environ.get('RUSTUP_HOME', '')
    if not cargo_home or not rustup_home:
        return refuse([name, *argv])
    if os.path.realpath(sys.argv[0]) != os.path.realpath(
            os.path.join(cargo_home, 'bin', name)):
        return refuse([name, *argv])
    case = case_for(rustup_home)
    record(rustup_home, [name, *argv])
    joined = ' '.join([name, *argv])
    for needle in case.get('fail_when', []):
        if needle in joined:
            # The source reader's emulator answer for an injected failure.
            print('injected failure', file=sys.stderr)
            return 1
    if name == 'rustup':
        if argv == ['set', 'auto-self-update', 'disable']:
            update_settings(rustup_home, auto_self_update='disable')
            return 0
        if argv == ['set', 'auto-install', 'disable']:
            update_settings(rustup_home, auto_install='disable')
            return 0
        if len(argv) >= 2 and argv[:2] == ['toolchain', 'install']:
            channel = argv[2]
            target = argv[argv.index('--target') + 1] if '--target' in argv else ''
            host = case['rustc_verbose'].split('host: ', 1)[1].splitlines()[0]
            create_toolchain(rustup_home, channel, host, target)
            return 0
        if len(argv) == 2 and argv[0] == 'default':
            host = case['rustc_verbose'].split('host: ', 1)[1].splitlines()[0]
            update_settings(rustup_home, default_toolchain=f'{argv[1]}-{host}')
            return 0
        if argv == ['--version']:
            print(f"rustup {case['rustup_version']} (fixture)")
            return 0
        if argv == ['toolchain', 'list']:
            print('\n'.join(case['toolchains']))
            return 0
        if argv == ['component', 'list', '--installed']:
            print('\n'.join(case['components']))
            return 0
        if argv == ['target', 'list', '--installed']:
            print('\n'.join(case['targets']))
            return 0
    elif name == 'cargo':
        if argv == ['--version']:
            print(case['cargo_version'])
            return 0
        if argv == ['clippy', '--version']:
            print(case['clippy_version'])
            return 0
    elif name == 'rustfmt':
        if argv == ['--version']:
            print(case['rustfmt_version'])
            return 0
    elif name == 'rustc':
        if argv == ['-vV']:
            print(case['rustc_verbose'], end='')
            return 0
    return refuse([name, *argv])


if __name__ == '__main__':
    sys.exit(main())
