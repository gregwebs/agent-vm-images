#!/usr/bin/env python3
"""Wiring controls for the signed-source Go setup in verify-release-boot.yml.

A recorded release is verified from its own signed source, which may predate the
Go helper module. The workflow must therefore set up Go only when that source
carries `images/recipe-contract/go.mod`, after the source is authenticated and
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
    conditions = re.findall(r"^\s+if: \$\{\{ hashFiles\('([^']+)'\) != '' \}\}$", block, re.M)
    assert len(conditions) == 1, f'{what}: expected one hashFiles condition, got {conditions}'
    return conditions[0]

signed_checkout = only('ref: ${{ steps.release.outputs.source_sha }}', 'signed-source checkout')
exact_checkout = only('Require exact signed source checkout', 'exact-checkout check')
setup = only('actions/setup-go@', 'setup-go')
assertion = only('Assert the signed source', 'toolchain assertion')
boot = only('Native amd64 boot verification', 'boot verification')
assert signed_checkout < exact_checkout < setup < assertion < boot, (
    'order must be: authenticated checkout, exact-SHA check, setup-go, toolchain '
    f'assertion, boot verification; got {signed_checkout, exact_checkout, setup, assertion, boot}')

setup_block = steps[setup]
assert '40f1582b2485089dde7abd97c1529aa768e1baff' in setup_block, 'setup-go must keep the reviewed commit pin'
assert 'go-version-file: source/images/recipe-contract/go.mod' in setup_block, setup_block
assert 'cache: false' in setup_block, setup_block
assert re.search(r'^\s+timeout-minutes: \d+$', setup_block, re.M), 'setup-go needs a step bound like its neighbours'
assert 'path: source' in steps[signed_checkout], steps[signed_checkout]

assertion_block = steps[assertion]
want = "bash source/script/ci/assert-go-toolchain.sh source/images/recipe-contract"
assert want in assertion_block, assertion_block
assert re.search(r'^\s+timeout-minutes: \d+$', assertion_block, re.M), 'the assertion step needs a step bound'

glob = condition_glob(setup_block, 'setup-go')
assert glob == 'source/images/recipe-contract/go.mod', glob
assert condition_glob(assertion_block, 'toolchain assertion') == glob, 'setup and assertion must test the same path'

# Never the harness checkout, never a current-main pin.
assert 'harness/images/recipe-contract' not in text, 'the harness module must not be a fallback'
for line in lines:
    if 'go-version-file:' in line:
        assert line.strip() == 'go-version-file: source/images/recipe-contract/go.mod', line
assert 'source/script/release/verify-release.sh' in steps[boot], 'the boot step must run the signed source own verifier'
for index, block in enumerate(steps):
    if index in (setup, assertion):
        continue
    assert 'go test' not in block and 'go build' not in block and 'setup-go' not in block, (
        f'step {index} needs Go but is not conditioned on the signed source: {block}')

# Both signed-source shapes, evaluated against the glob the workflow names.
workspace = Path(sys.argv[2])

def condition_runs() -> bool:
    return bool(list(workspace.glob(glob)))

harness_module = workspace / 'harness/images/recipe-contract/go.mod'
harness_module.parent.mkdir(parents=True, exist_ok=True)
harness_module.write_text('module harness\n')
python_only = workspace / 'source/script/release/verify-release.sh'
python_only.parent.mkdir(parents=True, exist_ok=True)
python_only.write_text('#!/bin/sh\n')
assert not condition_runs(), 'an old Python-only signed source must skip Go setup even when the harness has a module'
module = workspace / 'source/images/recipe-contract/go.mod'
module.parent.mkdir(parents=True, exist_ok=True)
module.write_text('module signed\n')
assert condition_runs(), 'a new Go signed source must run Go setup'
print('ci boot signed-source Go setup controls passed')
