#!/usr/bin/env python3
"""Failing and expiring gpg wrappers for the frozen reader rows.

Two rules cannot be reached with a real key on a real clock: a signature that
verifies but whose exit status says otherwise, and one that has expired. The mode
comes from the directory name — `gpg-fail` or `gpg-expire` — and the real gpg's
absolute path is substituted at copy time by `driver.install_tools`.

Until #245 a third mode recorded every invocation so the same row could be
compared against the Python reader's transcript. That parity surface died with
the oracle: the rows below now assert only that this reader refuses.
"""
from __future__ import annotations

import os
import subprocess
import sys
import time

REAL_GPG = '@REAL_GPG@'


def mode() -> str:
    return os.path.basename(os.path.dirname(os.path.abspath(sys.argv[0])))


def run(argv: list[str]) -> subprocess.CompletedProcess:
    # The delegated child needs the real gpg's directory on PATH: gpg starts
    # gpg-agent and gpgconf from beside itself, while the verifier's own PATH names
    # only this wrapper's directory.
    env = dict(os.environ)
    env['PATH'] = os.path.dirname(REAL_GPG) + ':' + env.get('PATH', '/usr/bin:/bin')
    return subprocess.run([REAL_GPG, *argv], env=env, capture_output=True, text=True)


def main() -> int:
    argv = sys.argv[1:]
    selected = mode()
    if selected == 'gpg-expire' and '--verify' in argv:
        # An absolute epoch, not a duration: gpg reads "+2d" as epoch 2 and then
        # refuses because the signature lies in the future, which would prove
        # nothing about expiry. Two days ahead puts the signature past its own
        # one-day expiry.
        faked = str(int(time.time()) + 2 * 86400)
        index = argv.index('--verify')
        argv = [*argv[:index], '--faked-system-time', faked, *argv[index:]]
    result = run(argv)
    if selected == 'gpg-fail' and '--verify' in argv:
        # The real verdict's stdout is kept, so a VALIDSIG line reaches the caller
        # and only the exit status disagrees: the case a status-only check misses.
        # Import and listing delegate normally, so the failure is the signature
        # verdict and not the keyring.
        sys.stdout.write(result.stdout)
        sys.stderr.write('injected failure\n')
        return 1
    sys.stdout.write(result.stdout)
    sys.stderr.write(result.stderr)
    return result.returncode


if __name__ == '__main__':
    sys.exit(main())
