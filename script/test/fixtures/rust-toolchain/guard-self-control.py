#!/usr/bin/env python3
"""Self-control: the fixture guard really refuses each forbidden operation.

Run as a child process by the maintenance suite, with no daemon and no network.
It installs the same guard a fixture child installs and then attempts (1) a
direct off-host socket, (2) an absolute downloader by path, (3) a nested
unregistered interpreter and (4) an allowlisted argv launched through an
*unregistered*
executable (`executable=` overrides argv[0], so authorizing the argv tail would
let that run). Each must be refused *before* doing anything, so this proves the
guard is installed before the modules under test and is not a basename match.

Prints one `NAME=denied|allowed` line per attempt and exits nonzero if any
attempt was allowed.
"""
from __future__ import annotations

import os
import shutil
import socket
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import guards  # noqa: E402

GPG = shutil.which('gpg') or '/usr/bin/gpg'


def attempt(action) -> str:
    try:
        action()
    except RuntimeError:
        return 'denied'
    except OSError:
        # Reached the operating system and failed there: not a guard denial.
        return 'allowed'
    return 'allowed'


def main() -> int:
    guards.install([GPG])
    downloader = shutil.which('curl') or '/usr/bin/curl'
    results = {
        'socket': attempt(lambda: socket.create_connection(('192.0.2.1', 9), timeout=0.1)),
        'downloader': attempt(lambda: subprocess.run([downloader, '--version'],
                                                     capture_output=True)),
        'nested': attempt(lambda: subprocess.run([sys.executable, '-c', 'pass'],
                                                 capture_output=True)),
        'identity': attempt(lambda: subprocess.run([GPG, 'MARKER'], executable='/bin/echo',
                                                   capture_output=True)),
        'allowed-gpg': attempt(lambda: subprocess.run([GPG, '--version'],
                                                      capture_output=True))
        if os.access(GPG, os.X_OK) else 'skipped',
    }
    for name, outcome in results.items():
        print(f'{name}={outcome}')
    expected = {'socket': 'denied', 'downloader': 'denied', 'nested': 'denied',
                'identity': 'denied', 'allowed-gpg': 'allowed'}
    if results.get('allowed-gpg') == 'skipped':
        expected.pop('allowed-gpg')
    return 0 if results == expected else 1


if __name__ == '__main__':
    sys.exit(main())
