#!/usr/bin/env python3
"""Self-test only: the real host watchdog with a group check that never settles.

`layer-build.sh --self-test` has to prove that the outer supervisor's grace
outlasts BOTH escalation windows of the watchdog an EGRESS run nests inside it.
Only a watchdog that always waits its whole grace and then reports the group
present consumes both, so this is a fault injection, not a second watchdog.

The real one is loaded from `host-watchdog.py` beside this file and its `main` is
run with the argv it was given: the seam replaces the group check and nothing
else. `layer-build.sh --self-test` asserts that the watchdog it configures is
exactly that sibling, so moving either file fails the test instead of quietly
exercising something else. This is a committed file rather than a heredoc because
the contracts gate refuses Python embedded in shell sources.

usage: python3 inner-watchdog-fault.py [HOST-WATCHDOG ARGUMENTS...]
"""
import importlib.util
import sys
import time
from pathlib import Path

REAL_WATCHDOG = Path(__file__).resolve().with_name('host-watchdog.py')


def load_watchdog(path: Path):
    spec = importlib.util.spec_from_file_location('host_watchdog_fixture', path)
    if spec is None or spec.loader is None:
        sys.stderr.write(f'inner-watchdog-fault: cannot load {path}\n')
        sys.exit(2)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def never_empty(process, grace):
    """The group check the real watchdog asks: always present, after the wait."""
    time.sleep(grace)
    process.poll()
    return False


def main(argv):
    watchdog = load_watchdog(REAL_WATCHDOG)
    watchdog._group_empty = never_empty
    return watchdog.main([str(REAL_WATCHDOG)] + argv[1:])


if __name__ == '__main__':
    sys.exit(main(sys.argv))
