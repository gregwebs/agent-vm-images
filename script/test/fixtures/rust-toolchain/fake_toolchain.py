#!/usr/bin/env python3
"""Standalone rustup/cargo/rustc/rustfmt emulator for the Go/Python differential.

Why it is a program and not a Python seam: the differential runs the shipped Go
binary, which cannot be given a fake runner. Both implementations must therefore
execute the *same* child and be asked the same questions, so this is a copy of one
tool name (rustup, cargo, rustc or rustfmt) that answers from a case file and
records what it was asked.

Rules, all of them fail-closed:

* ``argv[0]`` must be ``$CARGO_HOME/bin/<name>``. A tool from anywhere else is not
  the install the caller named, so it is refused rather than answered.
* The answers come from ``$RUSTUP_HOME.case.json`` — a *sibling* of the home, so
  the case never writes into the home it is describing. The case, not this
  emulator, decides the components, the targets, the toolchains and the failures;
  `verify-installed` only reads, so nothing here writes ``settings.toml`` or the
  toolchain tree.
* Every call appends ``{argv, env}`` to ``$RUSTUP_HOME/calls.jsonl``, which is how
  the differential compares what each implementation asked and what environment it
  asked with.
* Anything outside the table exits 97 with ``fixture emulator: unrecognized
  command``, so a command the shipped checks grew cannot pass by accident.

Only the read commands are here: they are the ones `verify-installed` reaches. The
install rows (`rustup toolchain install`, `default`, `set …`) belong to #231's
differential, which is the first caller that has them.
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


def record(rustup_home: str, argv: list[str]) -> None:
    # One line per call: the differential compares the two logs of one row, so the
    # writer must not reorder or rewrite them.
    with open(rustup_home + '/calls.jsonl', 'a', encoding='utf-8') as handle:
        handle.write(json.dumps({'argv': argv, 'env': dict(os.environ)},
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
