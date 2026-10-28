#!/usr/bin/env python3
"""dsh entry point for the shared host manager acquisition.

The implementation moved to `../pnpm-host-manager.py` when Pi and the bridge
joined dsh on one manager boundary. This adapter keeps `host-manager.py PREFIX`
as dsh's developer-upgrade argv, with dsh as the fixed consumer.
"""
import importlib.util
from pathlib import Path
import sys

sys.dont_write_bytecode = True

SHARED = Path(__file__).resolve().parents[1] / 'pnpm-host-manager.py'


def load():
    name = 'pnpm_host_manager'
    module = sys.modules.get(name)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(name, SHARED)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


if __name__ == '__main__':
    load().entry('dsh')
