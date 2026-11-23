#!/usr/bin/env python3
"""Recording, failing and expiring gpg wrappers for the differential.

The verifier's gpg transcript is a parity surface: two implementations that both
accept a manifest but ask gpg different questions are not interchangeable. So both
children exec the *same* real gpg through this wrapper, and the wrapper records
what it was asked.

The mode comes from the directory name — `gpg-record`, `gpg-fail` or `gpg-expire`
— and the real gpg's absolute path is substituted at copy time by
`differential.install_tools`, which is also what puts the wrapper inside the exec
allowlist before the sandbox is entered.

Where the transcript goes: the gpg child environment is exactly the five variables
the source reader builds (PATH, GNUPGHOME, HOME, LC_ALL, TMPDIR), so there is no
variable to name a log file with. Both readers freeze the same layout — the keyring
is ``$TMPDIR/rust-verify-*/gnupg`` — and the suite requires the row's TMPDIR to be
*empty* afterwards, so the transcript is written two levels above the gpg child's
TMPDIR: at the row directory, never inside TMPDIR. Nothing else about the layout
is assumed.
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import time

REAL_GPG = '@REAL_GPG@'


def mode() -> str:
    return os.path.basename(os.path.dirname(os.path.abspath(sys.argv[0])))


def transcript_path() -> str:
    scratch = os.environ.get('TMPDIR', '')
    if not scratch:
        return os.path.join(tempfile.gettempdir(), 'gpg-transcript.jsonl')
    return os.path.join(os.path.dirname(os.path.dirname(scratch)),
                        'gpg-transcript.jsonl')


def record(argv: list[str]) -> None:
    gnupghome = os.environ.get('GNUPGHOME', '')
    entry = {'mode': mode(), 'argv': argv, 'env': dict(os.environ),
             'keyring_mode': None}
    try:
        entry['keyring_mode'] = oct(os.stat(gnupghome).st_mode & 0o777)
    except OSError:
        pass
    with open(transcript_path(), 'a', encoding='utf-8') as handle:
        handle.write(json.dumps(entry, sort_keys=True) + '\n')


def run(argv: list[str]) -> subprocess.CompletedProcess:
    # The recorded environment is the verifier's own, unchanged. The delegated
    # child, though, needs the real gpg's directory on PATH: gpg starts
    # gpg-agent and gpgconf from beside itself, and the verifier's PATH names only
    # this wrapper's directory (which is what the two implementations must agree
    # on). Prepending it here keeps the transcript the verifier's environment and
    # stops the wrapper from hiding gpg's own helpers from it.
    env = dict(os.environ)
    env['PATH'] = os.path.dirname(REAL_GPG) + ':' + env.get('PATH', '/usr/bin:/bin')
    return subprocess.run([REAL_GPG, *argv], env=env, capture_output=True, text=True)


def main() -> int:
    argv = sys.argv[1:]
    record(argv)
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
