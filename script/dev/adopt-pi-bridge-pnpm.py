#!/usr/bin/env python3
"""Maintainer-only first conversion of the Pi or bridge npm lock to pnpm inputs.

This is deliberately a thin shell around the owning preparation driver. The
conversion has two properties that must not be reachable from an ordinary build:
it is the *first* input set for a project (so there is no pinned predecessor to
compare against) and it reads npm lock data that the source tree no longer has
once the migration lands. Both live inside
`images/tools/pi/prepare-project.py adopt`, which reads the fixed baseline commit
through Git instead of the working tree.

Usage:
    script/dev/adopt-pi-bridge-pnpm.py --consumer pi|bridge --proposal DIR \
        [--baseline COMMIT] [--keep-payload DIR]

The proposal is written outside the source checkout, under the invocation-owned
root named by AGENT_VM_SCRATCH_ROOT (or AGENT_VM_FIXTURE_ROOT in a fixture). An
unreviewed proposal is not an install: nothing here publishes into
`images/tools/pi`, and a build never calls this file.
"""
import argparse
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
DRIVER = ROOT / 'images/tools/pi/prepare-project.py'


def main():
    parser = argparse.ArgumentParser(prog='adopt-pi-bridge-pnpm.py')
    parser.add_argument('--consumer', required=True, choices=('pi', 'bridge'))
    parser.add_argument('--proposal', required=True)
    parser.add_argument('--baseline')
    parser.add_argument('--keep-payload')
    options = parser.parse_args()
    if not (os.environ.get('AGENT_VM_SCRATCH_ROOT')
            or os.environ.get('AGENT_VM_FIXTURE_ROOT')):
        print('adopt-pi-bridge-pnpm: set AGENT_VM_SCRATCH_ROOT to the owning root '
              'that will hold the proposal', file=sys.stderr)
        return 2
    argv = [sys.executable, str(DRIVER), '--consumer', options.consumer,
            'adopt', options.proposal]
    if options.baseline:
        argv += ['--baseline', options.baseline]
    if options.keep_payload:
        argv += ['--keep-payload', options.keep_payload]
    return subprocess.run(argv).returncode


if __name__ == '__main__':
    sys.exit(main())
