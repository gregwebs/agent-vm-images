#!/usr/bin/env python3
"""TEST-ONLY loader adapter: the production `codex_pin.py` with a fixture entrypoint.

The suites hand the vendored installer and the owner writer a *synthetic* pinned
closure whose `bin/codex` is a recording shell script, not an ELF. The production
loader refuses that at `require_elf`, so the fixture pin directory carries a file
named `codex_pin.py` (the name production code imports and executes) that is this
adapter: it loads the real module, re-exports its public surface, and overrides
exactly one seam -- the entrypoint inspector -- to accept the fixture marker and
delegate everything else to the real `require_elf`.

Everything else stays production code: the schema, URL derivation, evidence
hashing, the corroborating SHA256SUMS/metadata checks, the member policy, the
package metadata checks, the ELF inspection for every non-entrypoint member and
every budget.

Configuration: a JSON sidecar beside this file (`codex-pin-adapter.json`), so
nothing is passed through the environment of the process under test:

  {"module": "/abs/path/to/images/tools/codex/codex_pin.py"}

The entrypoint marker is a fixture constant shared with the archive builder
(`script/test/codex-pins.py` imports `MARKER`), so it is not configuration.

Runnable on its own: `python3 codex_pin_adapter.py select --pin-dir DIR
--release TAG --target TARGET` is the production `select`.
"""
from __future__ import annotations

import importlib.util
import json
import sys
from pathlib import Path

SIDECAR = 'codex-pin-adapter.json'

# The fixture `bin/codex` is a recording shell script, not an ELF. The production
# ELF inspector refuses that, so this adapter accepts exactly a shell-script
# header (`#!/bin/sh`) and delegates every other member and every other check to
# the real `require_elf` -- which still inspects the header length, the machine
# two fixture packages disagree on, and the executability policy.
SHEBANG = b'#!/bin/sh\n'


def _load(path: str):
    spec = importlib.util.spec_from_file_location('codex_pin_real', path)
    if spec is None or spec.loader is None:
        raise SystemExit(f'{path} is not an importable module')
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which a
    # bare module_from_spec does not populate (Python 3.12 raises there).
    sys.modules['codex_pin_real'] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop('codex_pin_real', None)
        raise
    return module


_HERE = Path(__file__).resolve().parent
_sidecar = _HERE / SIDECAR
if not _sidecar.is_file():
    raise SystemExit(f'{_sidecar} must name the production codex_pin.py')
_real_path = json.loads(_sidecar.read_text(encoding='utf-8')).get('module')
if not isinstance(_real_path, str) or not _real_path:
    raise SystemExit(f'{_sidecar} has no "module" path')
MODULE = _load(_real_path)

# The writer imports this file as a module and reads the production constants
# (`VERIFIER_VERSION`, the budgets) and helpers off it, so re-export the real
# module's public surface before overriding the one seam.
for _name, _value in vars(MODULE).items():
    if not _name.startswith('_'):
        globals()[_name] = _value


def fixture_script_inspector(header, target):
    if header.startswith(SHEBANG):
        return
    MODULE.require_elf(header, target)


# `stage_codex` (the owner writer) calls `preflight_archive` without an
# `inspect_binary` argument, so the fixture module -- never production -- makes
# the fixture inspector the default here.
_real_preflight = MODULE.preflight_archive


def preflight(pin, selected, archive, *, inspect_binary=fixture_script_inspector):
    return _real_preflight(pin, selected, archive, inspect_binary=inspect_binary)


MODULE.preflight_archive = preflight
preflight_archive = preflight

# Importable too: the owner writer loads this file as a module, and only the
# command-line invocation runs the CLI.
if __name__ == '__main__':
    sys.exit(MODULE.main(sys.argv[1:], inspect_binary=fixture_script_inspector))
