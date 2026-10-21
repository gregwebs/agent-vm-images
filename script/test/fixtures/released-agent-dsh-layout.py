#!/usr/bin/env python3
"""Assert the installed dsh layout: reviewed manifest, no private manager, direct link.

This is a lightweight identity check, not the production dsh parsing or runtime
certification. Fixed guest paths and the ordered dependency assertion are
deliberate: the probe runs from a rendered string with no checkout to consult.
"""
from __future__ import annotations

import json
import os
import sys

dsh, pnpm = sys.argv[1:]
prefix = '/opt/agent-vm/dsh'
manifest = json.load(open(prefix + '/package.json'))
assert manifest['name'] == 'agent-vm-dsh-layer', manifest['name']
assert manifest['private'] is True
assert manifest['packageManager'] == 'pnpm@' + pnpm, manifest['packageManager']
assert list(manifest['dependencies']) == ['@deepseek-ai/dsh'], manifest['dependencies']
assert manifest['dependencies']['@deepseek-ai/dsh'] == dsh
assert 'devDependencies' not in manifest
assert not os.path.lexists(prefix + '/node_modules/pnpm')
assert not os.path.lexists(prefix + '/node_modules/.bin/pnpm')
link = '/usr/local/bin/dsh'
target = prefix + '/node_modules/@deepseek-ai/dsh/lib/bin.js'
assert os.path.islink(link), link
assert os.readlink(link) == target, os.readlink(link)
assert os.path.samefile(link, target)
assert os.access(target, os.X_OK)
