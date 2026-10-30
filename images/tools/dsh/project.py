#!/usr/bin/env python3
"""dsh entry point for the shared pnpm project module.

The filesystem/manager/publication implementation lives in `../pnpm-project.py`,
because dsh, Pi and the bridge share one security boundary and two copies of it
would drift. This adapter keeps dsh's CLI, its imported Python surface and its
module identity: install-dsh.sh, prepare-lock.sh, upgrade-dsh.sh, verify-dsh.sh
and probe-dsh.py keep naming `project.py` beside them, and the Docker mount of
`tools/dsh` stays sufficient together with one mount of the shared file.

dsh is the default consumer here, so `--consumer` is not accepted: the shared CLI
(and only it) requires the flag. The shared module is registered in sys.modules
before it is executed, so its capability dataclasses keep a single identity; a
second instance would make `isinstance` checks against this module's names lie.
"""
import importlib.util
from pathlib import Path
import sys

sys.dont_write_bytecode = True

SHARED = Path(__file__).resolve().parents[1] / 'pnpm-project.py'


def load():
    name = 'pnpm_project'
    module = sys.modules.get(name)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(name, SHARED)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


project = load()
# Re-export the shared surface unchanged, so `project.authorize_mutation`,
# `project.PublicationFailure` and `project.PRODUCTION` are the shared objects.
globals().update({name: value for name, value in vars(project).items()
                  if not name.startswith('__')})


if __name__ == '__main__':
    project.entry('dsh')
