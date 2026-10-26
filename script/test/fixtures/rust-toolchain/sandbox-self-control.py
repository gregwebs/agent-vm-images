#!/usr/bin/env python3
"""Self-control: fixture children really are confined by the OS, not only by Python.

Run as a child process by the hermetic suites, with no daemon and no network.
The first stage re-execs this program inside the OS sandbox (macOS
``sandbox-exec``; Linux ``bwrap`` plus Landlock) before any in-process guard
exists. A second program is a *native* grandchild that never imports the guard,
so a denial from it is the operating system's rather than Python's audit hook's.

What that proves per platform: the network attempts are denied by the OS on both;
the executable attempts are denied by an allowlist only on macOS. On Linux they
are denied because Landlock refuses the ``exec`` syscall for paths outside its
list, which is a direct-exec denial -- the in-process audit hook is the exec
authority there, not this layer (see ``sandbox.py``).

Attempts inside the sandbox (all must be denied):

* a direct socket to an off-host address;
* a direct ``exec`` of an executable that is not on the declared list;
* a nested interpreter that attempts the socket above;
* a nested interpreter that copies an *allowed* binary into a writable directory
  and ``exec``s the copy directly; bubblewrap alone cannot stop that, which is
  why the Linux boundary also carries Landlock. This is the direct call, not a
  loader-mediated start: a listed ELF loader can still map another program, and
  that gap is tracked as follow-up work rather than asserted away here;
* the test-owned loopback listener (must be *allowed*): the request-deadline
  controls open their own peer, and the boundary has to keep that possible
  without opening the maintainer's network.

And one attempt that must succeed: the real, allowlisted gpg.

If no sandbox tool is available this exits 3 with an ``ISOLATION-UNAVAILABLE``
line and no claim. Otherwise it prints one ``NAME=denied|allowed`` line per
attempt and exits nonzero if any attempt escaped.
"""
from __future__ import annotations

import errno
import os
import shutil
import socket
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
if HERE not in sys.path:
    sys.path.insert(0, HERE)

import sandbox  # noqa: E402

GPG = os.path.realpath(shutil.which('gpg') or '/usr/bin/gpg')

# Errnos that mean the operating system refused the operation. ECONNREFUSED is
# deliberately absent: reaching a closed port proves the network stack is usable.
DENIED_ERRNOS = {errno.EPERM, errno.EACCES, errno.ENETUNREACH, errno.EHOSTUNREACH,
                 errno.EINVAL, errno.ENETDOWN}

# The native grandchild has no guard of its own, so it classifies outcomes the
# same way this control does: only a refusal or a missing binary is a denial.
NESTED_PROGRAM = '''
import errno, os, shutil, socket, subprocess, sys, tempfile
DENIED = {errno.EPERM, errno.EACCES, errno.ENETUNREACH, errno.EHOSTUNREACH,
          errno.EINVAL, errno.ENETDOWN}
try:
    socket.create_connection(("192.0.2.1", 9), timeout=0.2)
    print("nested-network=allowed")
except PermissionError:
    print("nested-network=denied")
except OSError as error:
    print("nested-network=denied" if error.errno in DENIED else "nested-network=allowed")
try:
    subprocess.run(["/bin/echo", "nested"], capture_output=True, check=True)
    print("nested-exec=allowed")
except (PermissionError, FileNotFoundError):
    print("nested-exec=denied")
except OSError as error:
    print("nested-exec=denied" if error.errno in DENIED else "nested-exec=allowed")
directory = tempfile.mkdtemp(dir=tempfile.gettempdir())
copy = os.path.join(directory, "copied-allowed-binary")
shutil.copyfile(sys.argv[1], copy)
os.chmod(copy, 0o755)
try:
    subprocess.run([copy, "--version"], capture_output=True)
    print("nested-copy-exec=allowed")
except (PermissionError, FileNotFoundError):
    print("nested-copy-exec=denied")
except OSError as error:
    print("nested-copy-exec=denied" if error.errno in DENIED else "nested-copy-exec=allowed")
finally:
    shutil.rmtree(directory, ignore_errors=True)
'''

NESTED_ATTEMPTS = ('nested-network', 'nested-exec', 'nested-copy-exec')


def network_outcome() -> str:
    try:
        socket.create_connection(('192.0.2.1', 9), timeout=0.2)
    except PermissionError:
        return 'denied'
    except OSError as error:
        return 'denied' if error.errno in DENIED_ERRNOS else 'allowed'
    return 'allowed'


def executable_outcome() -> str:
    try:
        subprocess.run(['/bin/echo', 'control'], capture_output=True, check=True)
    except PermissionError:
        return 'denied'
    except FileNotFoundError:
        # The sandbox did not expose the binary at all, which is stronger.
        return 'denied'
    except OSError as error:
        return 'denied' if error.errno in DENIED_ERRNOS else 'allowed'
    return 'allowed'


def nested_outcome() -> dict[str, str]:
    result = subprocess.run([sys.executable, '-c', NESTED_PROGRAM, GPG],
                            capture_output=True, text=True, timeout=120)
    outcomes = {}
    for line in result.stdout.splitlines():
        name, _, value = line.partition('=')
        if name in NESTED_ATTEMPTS:
            outcomes[name] = value.strip()
    for name in NESTED_ATTEMPTS:
        outcomes.setdefault(name, 'allowed')
    return outcomes


def loopback_outcome() -> str:
    """The test-owned deadline peer must still work inside the boundary."""
    try:
        listener = socket.socket()
        listener.bind(('127.0.0.1', 0))
        listener.listen()
        port = listener.getsockname()[1]
        client = socket.create_connection(('127.0.0.1', port), timeout=2)
        accepted, _ = listener.accept()
        accepted.close()
        client.close()
        listener.close()
    except OSError as error:
        return f'denied-{error.errno}'
    return 'allowed'


def gpg_outcome() -> str:
    try:
        result = subprocess.run([GPG, '--version'], capture_output=True, timeout=120)
    except OSError:
        return 'denied'
    return 'allowed' if result.returncode == 0 else 'denied'


def main() -> int:
    # Called unconditionally: the first pass re-execs into the sandbox, the second
    # applies the Linux direct-exec restriction and raises the namespace-local
    # loopback.
    executables = sandbox.gpg_executables(GPG) + [sys.executable]
    try:
        sandbox.reexec_into_sandbox(
            executables,
            writable=[tempfile.gettempdir(), '/tmp', '/private/tmp'],
            readable=[HERE], loopback=True)
    except sandbox.SandboxUnavailable as error:
        print(f'ISOLATION-UNAVAILABLE: {error}')
        return 3
    if os.environ.get(sandbox.SANDBOXED_ENV) != '1':
        print('ISOLATION-UNAVAILABLE: sandbox re-exec returned unexpectedly')
        return 3

    outcomes = {'network': network_outcome(), 'executable': executable_outcome(),
                'gpg': gpg_outcome(), 'loopback': loopback_outcome()}
    outcomes.update(nested_outcome())
    names = ('network', 'executable', 'loopback', 'gpg') + NESTED_ATTEMPTS
    for name in names:
        print(f'{name}={outcomes[name]}')
    expected = {'network': 'denied', 'executable': 'denied', 'loopback': 'allowed',
                'gpg': 'allowed',
                **{name: 'denied' for name in NESTED_ATTEMPTS}}
    return 0 if outcomes == expected else 1


if __name__ == '__main__':
    sys.exit(main())
