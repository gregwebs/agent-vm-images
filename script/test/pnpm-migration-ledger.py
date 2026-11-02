#!/usr/bin/env python3
"""Black-box tests for the migration ledger's object-absence failures.

`script/test/pnpm-migration-ledger.mjs --check` regenerates the ledger from the
baseline commit and from the pinned historical pnpm source (`c33ab9b`) and
compares it with the committed JSON. Both reads go through one `git()` helper
that asserts a zero status, so an object the checkout does not have must end the
run with a nonzero status and a diagnostic. A gate that skipped instead would
report "matches its generator" for a record it never read -- the exact silent
pass the plan's provenance transition names (AC-L1 step 1: "Verify it with
`git cat-file -e`, failing loudly").

Every case runs against a scratch clone, so the committed repository and its
history are never written. Case `shallow-checkout` is the real shape: a CI
checkout without `fetch-depth: 0`. Case `historical-object-absent` keeps the
baseline object and removes only the historical source, so it proves the
`c33ab9b` read is the one that refuses.
"""
from __future__ import annotations

import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GENERATOR = 'script/test/pnpm-migration-ledger.mjs'
BASELINE = '6c51509'
HISTORICAL = 'c33ab9b61e631ef383cc2e45475e1a32e42da875'
ABSENT = '0' * 40


class Failed(Exception):
    pass


def run(command, cwd):
    return subprocess.run(command, cwd=cwd, capture_output=True, text=True)


def git(command, cwd):
    result = run(['git', *command], cwd)
    if result.returncode != 0:
        raise Failed(f'git {" ".join(command)} in {cwd} failed: {result.stderr.strip()}')
    return result.stdout


def clone(destination, *options):
    source = 'file://' + str(ROOT) if '--depth=1' in options else str(ROOT)
    git(['clone', '--quiet', '--no-tags', *options, source, str(destination)], ROOT)
    # The gate must judge the working tree's generator, not the last commit's.
    shutil.copyfile(ROOT / GENERATOR, destination / GENERATOR)
    return destination


def check(repository):
    return run(['node', GENERATOR, '--check'], repository)


def expect_loud_failure(label, repository, absent_object):
    result = check(repository)
    if result.returncode == 0:
        raise Failed(f'{label}: --check exited 0 without the object it reads; the gate skipped')
    if 'matches its generator' in result.stdout:
        raise Failed(f'{label}: --check reported a match it cannot have read')
    diagnostic = result.stderr.strip().splitlines()
    if not diagnostic or not diagnostic[0].startswith('pnpm-migration-ledger: '):
        raise Failed(f'{label}: no ledger diagnostic, stderr={result.stderr!r}')
    if 'git rev-parse' not in diagnostic[0] or 'failed' not in diagnostic[0]:
        raise Failed(f'{label}: diagnostic does not name the failed read: {diagnostic[0]!r}')
    if absent_object not in diagnostic[0]:
        raise Failed(f'{label}: diagnostic names another object: {diagnostic[0]!r}')
    print(f'{label}: exit {result.returncode}; {diagnostic[0]}')


def shallow_checkout(work):
    repository = clone(work / 'shallow', '--depth=1')
    expect_loud_failure('shallow-checkout', repository, BASELINE)
    # The clone really is shallow: neither object the gate reads is present.
    for revision in (BASELINE, HISTORICAL):
        present = run(['git', 'cat-file', '-e', f'{revision}^{{commit}}'], repository)
        if present.returncode == 0:
            raise Failed(f'shallow-checkout: {revision} is present, so the case proves nothing')


def historical_object_absent(work):
    repository = clone(work / 'full', '--shared')
    baseline = run(['git', 'cat-file', '-e', f'{BASELINE}^{{commit}}'], repository)
    if baseline.returncode != 0:
        raise Failed(f'full: baseline {BASELINE} is present in the committed history and must clone')
    control = check(repository)
    if control.returncode != 0 or 'matches its generator' not in control.stdout:
        raise Failed(f'full: the untouched clone must be green, exit {control.returncode}: {control.stderr.strip()}')
    print('full: the untouched clone matches its generator')
    generator = repository / GENERATOR
    source = generator.read_text()
    if source.count(f'"{HISTORICAL}"') != 1:
        raise Failed(f'full: {GENERATOR} does not pin {HISTORICAL} once')
    generator.write_text(source.replace(f'"{HISTORICAL}"', f'"{ABSENT}"'))
    expect_loud_failure('historical-object-absent', repository, ABSENT)
    return repository


def main():
    if len(sys.argv) != 1:
        print('usage: python3 script/test/pnpm-migration-ledger.py', file=sys.stderr)
        return 2
    with tempfile.TemporaryDirectory(prefix='pnpm-migration-ledger-test.') as directory:
        work = Path(directory).resolve()
        try:
            shallow_checkout(work)
            historical_object_absent(work)
        except Failed as error:
            print(f'FAIL: {error}', file=sys.stderr)
            return 1
    print('pnpm-migration-ledger: object-absence failures are loud')
    return 0


if __name__ == '__main__':
    sys.exit(main())
