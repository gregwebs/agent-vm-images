#!/usr/bin/env python3
"""Wiring controls for the signed-source Go setup in verify-release-boot.yml.

A recorded release is verified from its own signed source, which may predate the
Go helper module. The workflow must therefore set up Go only when that source
carries a root `go.mod`, after the source is authenticated and
checked out at the recorded SHA, and it must never fall back to the harness
checkout's module or to a current-main pin. Both source shapes are exercised as
filesystem fixtures against the condition the workflow actually names.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

workflow = Path(sys.argv[1])
lines = workflow.read_text().splitlines()
text = '\n'.join(lines)

def step_blocks(source_lines):
    """Split a job's steps on their six-space list markers."""
    blocks, current = [], None
    for line in source_lines:
        if re.match(r'^      - ', line):
            if current is not None:
                blocks.append('\n'.join(current))
            current = [line]
        elif current is not None:
            current.append(line)
    if current is not None:
        blocks.append('\n'.join(current))
    return blocks

steps = step_blocks(lines)
assert steps, 'no steps found'

def only(marker, what):
    found = [index for index, block in enumerate(steps) if marker in block]
    assert len(found) == 1, f'{what}: expected one step containing {marker!r}, got {len(found)}'
    return found[0]

def condition_glob(block, what):
    """The globs a step's `if:` tests with hashFiles, in order."""
    conditions = re.findall(r"hashFiles\('([^']+)'\)", block)
    assert conditions, f'{what}: no hashFiles condition'
    return conditions

signed_checkout = only('ref: ${{ steps.release.outputs.source_sha }}', 'signed-source checkout')
exact_checkout = only('Require exact signed source checkout', 'exact-checkout check')
setup = [index for index, block in enumerate(steps) if 'actions/setup-go@' in block]
assert len(setup) == 2, f'setup-go: expected one step per signed-source module shape, got {len(setup)}'
assertion = [index for index, block in enumerate(steps)
             if 'assert-go-toolchain.sh' in block and 'run:' in block]
assert len(assertion) == 2, f'toolchain assertion: expected two, got {len(assertion)}'
boot = only('Native amd64 boot verification', 'boot verification')
assert signed_checkout < exact_checkout < min(setup) and max(setup) < min(assertion) \
    and max(assertion) < boot, (
        'order must be: authenticated checkout, exact-SHA check, setup-go, toolchain '
        f'assertion, boot verification; got {signed_checkout, exact_checkout, setup, assertion, boot}')

# One (setup, assertion) pair per signed-source shape, both conditioned on the same
# glob: the root module of ADR 0017, and the nested images/recipe-contract module
# of the releases in between. A shape with no module runs neither.
SHAPES = {
    'root': ('source/go.mod', 'source'),
    'nested': ('source/images/recipe-contract/go.mod', 'source/images/recipe-contract'),
}
root_glob = SHAPES['root'][0]
nested_glob = SHAPES['nested'][0]
for index, block in zip(setup, assertion):
    assert '40f1582b2485089dde7abd97c1529aa768e1baff' in steps[index], \
        'setup-go must keep the reviewed commit pin'
    assert 'cache: false' in steps[index], steps[index]
    assert re.search(r'^\s+timeout-minutes: \d+$', steps[index], re.M), \
        'setup-go needs a step bound like its neighbours'
    assert re.search(r'^\s+timeout-minutes: \d+$', steps[block], re.M), \
        'the assertion step needs a step bound'
assert condition_glob(steps[setup[0]], 'setup-go root') == [root_glob], steps[setup[0]]
# The nested shape must only fire when no root module exists, or a root-module
# release would be set up twice.
assert condition_glob(steps[setup[1]], 'setup-go nested') == [root_glob, nested_glob], steps[setup[1]]
assert "hashFiles('source/go.mod') == ''" in steps[setup[1]], steps[setup[1]]
assert 'go-version-file: ' + root_glob in steps[setup[0]], steps[setup[0]]
assert 'go-version-file: ' + nested_glob in steps[setup[1]], steps[setup[1]]
assert condition_glob(steps[assertion[0]], 'toolchain assertion root') == [root_glob]
assert 'assert-go-toolchain.sh source' in steps[assertion[0]]
assert 'assert-go-toolchain.sh source/images/recipe-contract' in steps[assertion[1]]
assert 'path: source' in steps[signed_checkout], steps[signed_checkout]

# Never the harness checkout, never a current-main pin.
assert 'harness/go.mod' not in text, 'the harness module must not be a fallback'
assert 'harness/images/recipe-contract' not in text, 'the harness module must not be a fallback'
expected_globs = {'go-version-file: ' + glob for glob in [root_glob, nested_glob]}
for line in lines:
    if 'go-version-file:' in line:
        assert line.strip() in expected_globs, line
assert 'source/script/release/verify-release.sh' in steps[boot], 'the boot step must run the signed source own verifier'
for index, block in enumerate(steps):
    if index in (*setup, *assertion):
        continue
    assert 'go test' not in block and 'go build' not in block and 'setup-go' not in block, (
        f'step {index} needs Go but is not conditioned on the signed source: {block}')

# Every signed-source shape, evaluated against the globs the workflow names.
workspace = Path(sys.argv[2])

def condition_runs(condition_glob_value: str) -> bool:
    return bool(list(workspace.glob(condition_glob_value)))


def make(*parts: str, content: str = 'module fixture\n') -> None:
    path = workspace.joinpath(*parts)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content)


# The harness checkout has a root module of its own; it must never be the fallback.
make('harness', 'go.mod', content='module harness\n')
make('source', 'script', 'release', 'verify-release.sh', content='#!/bin/sh\n')

# 1. A signed source that predates the Go helper: neither shape selects Go.
assert not condition_runs(root_glob), 'a Python-only signed source must skip Go setup'
assert not condition_runs(nested_glob), 'a Python-only signed source must skip Go setup'

# 2. The nested module of the releases between #75 and this restructure: Go is
#    required, selected by the nested module, and the root shape stays silent.
make('source', 'images', 'recipe-contract', 'go.mod', content='module signed\n')
assert not condition_runs(root_glob), 'a nested-module signed source has no root module'
assert condition_runs(nested_glob), 'a nested-module signed source must run Go setup'

# 3. The root module from ADR 0017 onwards: the root shape selects Go and the
#    nested condition is false because it requires the absence of a root module.
make('source', 'go.mod', content='module signed-root\n')
(workspace / 'source' / 'images' / 'recipe-contract' / 'go.mod').unlink()
assert condition_runs(root_glob), 'a root-module signed source must run Go setup'
assert not condition_runs(nested_glob + '#absent'), 'the nested shape must not win'
print('ci boot signed-source Go setup controls passed')
