#!/usr/bin/env python3
"""OS-level isolation for fixture child trees.

The in-process audit guard in ``guards.py`` only covers Python code in the
process that installed it. A native child -- the real gpg, a nested interpreter,
anything a test-owned launcher spawns -- runs outside that hook, so a fixture
child tree needs a second, operating-system boundary. This module builds the
strongest one the host offers and never pretends when it cannot.

What each platform actually enforces, because they are not the same thing:

* **Both**: no off-host network. macOS denies it in the sandbox profile; Linux
  removes the network namespace (``--unshare-all``) and leaves only the child's
  own loopback, which it raises on request.
* **macOS**: ``sandbox-exec`` with ``(deny default)`` whose ``process-exec`` rule
  is a list of literals taken from the declared executables (plus the
  interpreter's own re-exec target for framework builds), and whose write rules
  name only the declared directories. Every ``exec`` is checked against that
  path identity, so a copy of an allowed binary in writable scratch is a
  different literal and does not start: this is an executable allowlist.
* **Linux**: ``bubblewrap`` with an empty root exposing only the declared runtime
  paths, readable inputs and executables, plus Landlock. This layer's guarantees
  are network isolation and the filesystem view; it is **not** an executable
  allowlist. Landlock denies the ``exec`` syscall for paths outside the
  allowlist, which refuses a *direct* exec of a copy in writable scratch, but an
  allowed ELF loader has to be on that list (the kernel execs it) and a loader
  can map and start another program without a kernel exec of that file. So on
  Linux the authority for which programs run is the in-process audit hook in
  ``guards.py``, installed in every fixture child before the modules under test
  are imported; the OS layer holds the network and filesystem line. The gap is
  stated in ``CONTRIBUTING.md`` and tracked as follow-up work, not claimed here.
* With neither tool available it raises ``SandboxUnavailable`` with the exact
  prerequisite. Callers must exit nonzero **before** importing or running
  anything under test: see ``launcher.py`` and ``sandbox-self-control.py``.
"""
from __future__ import annotations

import ctypes
import errno
import fcntl
import os
import shutil
import socket
import struct
import sys
import sysconfig
from typing import Iterable, Sequence

SANDBOXED_ENV = 'FIXTURE_SANDBOXED'
EXEC_RESTRICTED_ENV = 'FIXTURE_EXEC_RESTRICTED'
# A control may simulate the missing prerequisite, so the fail-closed path is
# itself proved. It can only ever make isolation *stricter* to pass: with it set
# the suite requires a nonzero launch, never a green run.
SANDBOX_UNAVAILABLE_ENV = 'FIXTURE_SANDBOX_UNAVAILABLE'

# /dev/null is the only device a child may open for writing; stdout and stderr
# are inherited descriptors and need no rule.
DEVICE_WRITES = ('/dev/null', '/dev/tty')

LANDLOCK_CREATE_RULESET = 444
LANDLOCK_ADD_RULE = 445
LANDLOCK_RESTRICT_SELF = 446
LANDLOCK_CREATE_RULESET_VERSION = 1
LANDLOCK_RULE_PATH_BENEATH = 1
LANDLOCK_ACCESS_FS_EXECUTE = 1

SIOCGIFFLAGS = 0x8913
SIOCSIFFLAGS = 0x8914
IFF_UP = 0x1


class SandboxUnavailable(RuntimeError):
    """The host cannot provide the required isolation boundary."""


# The helpers a real gpg starts from its own bin directory. `gpg --import` starts
# gpg-agent and speaks to it over a unix socket, so an allowlist that names only
# `gpg` would refuse the real tool the fixtures must still be able to call.
GPG_HELPERS = ('gpg-agent', 'gpgconf', 'gpg-connect-agent', 'dirmngr', 'pinentry')


def gpg_executables(gpg: str) -> list[str]:
    """The real gpg plus the helpers it starts, resolved and deduplicated."""
    real = os.path.realpath(gpg)
    result = [real]
    directory = os.path.dirname(real)
    for name in GPG_HELPERS:
        candidate = os.path.join(directory, name)
        if os.access(candidate, os.X_OK) and candidate not in result:
            result.append(candidate)
    return result


def tool() -> str | None:
    """The sandbox launcher this host provides, or ``None``.

    A caller that starts a nested confinement control (or reaches this module
    from inside another sandbox) has to allowlist the launcher itself, so this is
    part of the interface rather than a private detail.
    """
    if sys.platform == 'darwin':
        found = '/usr/bin/sandbox-exec'
        return found if os.access(found, os.X_OK) else None
    return shutil.which('bwrap')


def _interpreter_reexec() -> list[str]:
    """Paths a framework Python re-execs itself through on macOS.

    Homebrew's framework build runs ``Python.app/Contents/MacOS/Python`` through
    ``posix_spawn`` at startup; without that literal in the allowlist the child
    dies before it runs a line of its own code.
    """
    # Both the path the caller starts and the file it resolves to: the interpreter
    # is usually a symlink, and the Linux prefix has to expose both.
    targets = [sys.executable]
    real = os.path.realpath(sys.executable)
    if real not in targets:
        targets.append(real)
    marker = f'{os.sep}Versions{os.sep}'
    if sys.platform == 'darwin' and marker in real:
        prefix = real.split(marker)[0]
        version = real.split(marker)[1].split(os.sep)[0]
        app = os.path.join(prefix, 'Versions', version, 'Resources', 'Python.app',
                           'Contents', 'MacOS', 'Python')
        if os.access(app, os.X_OK):
            targets.append(app)
    return targets


def _sandbox_exec_profile(executables: Sequence[str], writable: Sequence[str],
                          loopback: bool) -> str:
    def literals(paths: Iterable[str]) -> str:
        return ' '.join(f'(literal "{path}")' for path in paths)

    def subpaths(paths: Iterable[str]) -> str:
        return ' '.join(f'(subpath "{path}")' for path in paths)

    rules = ['(version 1)', '(deny default)',
             # Reads stay open: the fixture child must load its interpreter, the
             # standard library, the shipped modules and the committed evidence.
             # The exec allowlist is still a path identity, so a copy of an
             # allowed binary in a writable directory is a different vnode and is
             # refused -- which is what the nested control proves.
             '(allow file-read*)', '(allow sysctl-read)', '(allow process-fork)',
             f'(allow process-exec {literals(executables)})',
             f'(allow file-write* {subpaths(writable)} {literals(DEVICE_WRITES)})',
             # gpg-agent is reached over a unix socket inside the keyring
             # directory. Those rules are path-scoped, so they cannot authorize a
             # TCP connection: the self-control proves that from inside.
             f'(allow network-bind {subpaths(writable)})',
             f'(allow network-outbound {subpaths(writable)})',
             '(allow signal (target self))']
    if loopback:
        # The request deadline is proved against a peer the test itself opens.
        # These three rules are scoped to the loopback interface, so they cannot
        # reach the maintainer's network -- the same control asserts that.
        rules.insert(-1, '(allow network-bind (local ip "localhost:*"))\n'
                         '(allow network-inbound (local ip "localhost:*"))\n'
                         '(allow network-outbound (remote ip "localhost:*"))')
    return '\n'.join(rules)


def _ancestors(destinations: Iterable[str]) -> list[str]:
    """The directories a bind destination needs, shallowest first.

    ``bwrap`` creates a bind target but not its parents, and the checkout can sit
    under an arbitrary prefix (``/home/runner/work/...``), so the prefix is built
    from each destination rather than assumed.
    """
    needed: list[str] = []
    for destination in destinations:
        chain = []
        parent = os.path.dirname(destination)
        while parent not in ('', os.sep):
            chain.append(parent)
            parent = os.path.dirname(parent)
        for entry in reversed(chain):
            if entry not in needed:
                needed.append(entry)
    return needed


def _linux_prefix(executables: Sequence[str], writable: Sequence[str],
                  readable: Sequence[str], tool_path: str) -> list[str]:
    """An empty root with only the declared paths and executables.

    Shared libraries are visible because the dynamic loader needs them. The
    network namespace (``--unshare-all``) is the network boundary and this bind
    set is the filesystem boundary. The Landlock rules added inside are a
    direct-``exec`` deny list on top, not an executable allowlist: see the module
    docstring for why an allowed loader defeats that claim on Linux.
    """
    stdlib = sysconfig.get_paths().get('stdlib', '')
    exposed: list[tuple[str, bool]] = []
    for path in (stdlib, '/usr/lib', '/usr/lib64', '/lib', '/lib64',
                 '/etc/ld.so.cache', '/etc/localtime', '/usr/share/locale'):
        if path and os.path.exists(path):
            exposed.append((path, False))
    exposed.extend((path, False) for path in executables)
    exposed.extend((path, False) for path in readable if os.path.exists(path))
    exposed.extend((path, True) for path in writable if os.path.isdir(path))
    destinations = [path for path, _ in exposed]
    argv = [tool_path, '--unshare-all', '--die-with-parent', '--new-session',
            '--tmpfs', '/', '--dev', '/dev', '--proc', '/proc', '--dir', '/tmp']
    for directory in _ancestors(destinations):
        argv += ['--dir', directory]
    for source, writable_bind in exposed:
        argv += ['--bind' if writable_bind else '--ro-bind', source, source]
    return argv


def prefix(executables: Sequence[str], writable: Sequence[str],
           readable: Sequence[str] = (), loopback: bool = False) -> list[str]:
    """Run a child with no off-host network; on macOS, also an exec allowlist."""
    if os.environ.get(SANDBOXED_ENV) == '1':
        return []
    if os.environ.get(SANDBOX_UNAVAILABLE_ENV) == '1':
        raise SandboxUnavailable(
            f'{SANDBOX_UNAVAILABLE_ENV} is set: this control requires the missing '
            'prerequisite path')
    resolved = sorted({os.path.realpath(path) for path in executables})
    interpreters = _interpreter_reexec()
    writable = [os.path.realpath(path) for path in writable]
    readable = [os.path.realpath(path) for path in readable]
    tool_path = tool()
    if sys.platform == 'darwin':
        if tool_path is None:
            raise SandboxUnavailable(
                'sandbox-exec is required for fixture-child isolation on macOS '
                'and was not found; the isolation control refuses rather than '
                'claiming an in-process guard is an OS boundary')
        allowed = resolved + [path for path in interpreters if path not in resolved]
        return [tool_path, '-p', _sandbox_exec_profile(allowed, writable, loopback)]
    if tool_path is None:
        raise SandboxUnavailable(
            'bubblewrap (bwrap) is required for fixture-child isolation on Linux '
            'and was not found; install it (e.g. apt-get install bubblewrap) or '
            'the isolation control refuses rather than claiming an in-process '
            'guard is an OS boundary')
    if sys.platform.startswith('linux'):
        _require_landlock()
    # Bind the declared paths as well as their resolved targets: the child is
    # started through `sys.executable`, which is a symlink on many hosts, and the
    # symlink itself has to exist inside the empty root.
    binds = sorted({*executables, *resolved, *interpreters})
    return _linux_prefix(binds, writable, readable, tool_path)


def _landlock_call(number: int, *arguments):
    libc = ctypes.CDLL(None, use_errno=True)
    libc.syscall.restype = ctypes.c_long
    ctypes.set_errno(0)
    result = libc.syscall(ctypes.c_long(number), *arguments)
    if result < 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))
    return result


def _landlock_abi() -> int:
    return _landlock_call(LANDLOCK_CREATE_RULESET, ctypes.c_void_p(None),
                          ctypes.c_size_t(0),
                          ctypes.c_uint32(LANDLOCK_CREATE_RULESET_VERSION))


def _require_landlock() -> None:
    try:
        if _landlock_abi() >= 1:
            return
    except OSError as error:
        raise SandboxUnavailable(
            'Landlock is required for the Linux direct-exec restriction and is '
            f'not available ({error}); without it a copy written into the '
            'writable scratch tree could simply be executed') from None
    raise SandboxUnavailable('Landlock reported no usable ABI version')


PT_INTERP = 3


def program_interpreters(path: str) -> list[str]:
    """The programs an executable starts for itself: its loader or its shebang.

    Landlock checks that ``exec`` too, so a rule naming only the program refuses
    every dynamically linked binary with EACCES. Naming the exact loader keeps the
    list to the paths that actually start, instead of opening a library
    directory -- at the cost of making the loader itself exec-able, which is why
    this layer cannot claim to be an executable allowlist on Linux.
    """
    try:
        with open(path, 'rb') as handle:
            head = handle.read(64)
            if head.startswith(b'#!'):
                fields = handle.readline().decode('utf-8', 'replace').split()
                names = fields[:1]
            elif head.startswith(b'\x7fELF'):
                names = _elf_interpreters(handle, head)
            else:
                return []
    except OSError:
        return []
    return [name for name in names if os.path.isabs(name)]


def _elf_interpreters(handle, head: bytes) -> list[str]:
    order = 'little' if head[5] == 1 else 'big'
    wide = head[4] == 2
    phoff = int.from_bytes(head[32:40] if wide else head[28:32], order)
    phentsize = int.from_bytes(head[54:56] if wide else head[42:44], order)
    phnum = int.from_bytes(head[56:58] if wide else head[44:46], order)
    try:
        handle.seek(phoff)
        table = handle.read(phentsize * phnum)
    except OSError:
        return []
    for index in range(phnum):
        entry = table[index * phentsize:(index + 1) * phentsize]
        if len(entry) < phentsize or int.from_bytes(entry[0:4], order) != PT_INTERP:
            continue
        offset = int.from_bytes(entry[8:16] if wide else entry[4:8], order)
        size = int.from_bytes(entry[32:40] if wide else entry[16:20], order)
        try:
            handle.seek(offset)
            raw = handle.read(size)
        except OSError:
            return []
        name = raw.split(b'\x00', 1)[0].decode('utf-8', 'replace')
        return [name, os.path.realpath(name)] if name else []
    return []


def _restrict_exec(executables: Sequence[str]) -> None:
    """Deny the ``exec`` syscall for every path outside the list, for this process
    and every child.

    Landlock is allow-list-only and inherited across ``exec``, so this is applied
    once inside the sandbox and before anything under test is imported or run.
    The dynamic loaders of the listed programs are part of the list because the
    kernel execs them as programs of their own.

    This is a direct-exec denial, not an executable allowlist: a listed loader can
    still map and start another file without a kernel exec of that file. On Linux
    the exec authority for fixture children is the audit hook in ``guards.py``;
    keep that split in mind before describing this function as an allowlist.
    """
    if os.environ.get(EXEC_RESTRICTED_ENV) == '1':
        return
    _require_landlock()
    allowed = list(executables)
    for path in executables:
        allowed.extend(program_interpreters(path))
    executables = list(dict.fromkeys(allowed))
    attribute = ctypes.c_char_p(struct.pack('=Q', LANDLOCK_ACCESS_FS_EXECUTE))
    ruleset = _landlock_call(LANDLOCK_CREATE_RULESET, attribute, ctypes.c_size_t(8),
                             ctypes.c_uint32(0))
    try:
        for path in executables:
            descriptor = os.open(path, os.O_PATH | os.O_CLOEXEC)
            try:
                rule = ctypes.c_char_p(struct.pack('=Qi', LANDLOCK_ACCESS_FS_EXECUTE,
                                                   descriptor))
                _landlock_call(LANDLOCK_ADD_RULE,
                               ctypes.c_int(ruleset),
                               ctypes.c_int(LANDLOCK_RULE_PATH_BENEATH),
                               rule, ctypes.c_uint32(0))
            finally:
                os.close(descriptor)
        libc = ctypes.CDLL(None, use_errno=True)
        # Landlock requires no_new_privs, which also makes the restriction
        # non-bypassable by a setuid child.
        if libc.prctl(38, 1, 0, 0, 0) != 0:
            code = ctypes.get_errno()
            raise SandboxUnavailable(f'cannot set no_new_privs: {os.strerror(code)}')
        _landlock_call(LANDLOCK_RESTRICT_SELF, ctypes.c_int(ruleset),
                       ctypes.c_uint32(0))
    finally:
        os.close(ruleset)
    os.environ[EXEC_RESTRICTED_ENV] = '1'


def _bring_up_loopback() -> None:
    """Raise ``lo`` inside the sandbox's own network namespace.

    ``--unshare-all`` leaves the fresh interface down. The namespace belongs to
    the child, so the interface is reachable only from the child: this is what the
    test-owned loopback deadline controls need and nothing more.
    """
    request = struct.pack('16sH', b'lo', IFF_UP) + b'\x00' * 22
    with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as probe:
        current = fcntl.ioctl(probe, SIOCGIFFLAGS, request)
        flags = struct.unpack('16sH', current[:18])[1]
        if not flags & IFF_UP:
            fcntl.ioctl(probe, SIOCSIFFLAGS, struct.pack('16sH', b'lo', flags | IFF_UP)
                        + b'\x00' * 22)


def reexec_into_sandbox(executables: Sequence[str], writable: Sequence[str],
                        readable: Sequence[str] = (), loopback: bool = False) -> None:
    """Re-exec this process inside the sandbox once, before any code under test runs.

    A child tree starts sandboxed from its first line, so nothing it does -- and
    nothing a native grandchild does -- can happen outside the boundary. On the
    second pass (already inside) this applies the Linux direct-exec restriction
    and, when asked, the namespace-local loopback.
    """
    resolved = sorted({os.path.realpath(path) for path in executables})
    if os.environ.get(SANDBOXED_ENV) == '1':
        if loopback and sys.platform.startswith('linux'):
            _bring_up_loopback()
        if sys.platform.startswith('linux'):
            _restrict_exec(resolved)
        return
    prefix_argv = prefix(executables, writable, readable, loopback)
    if not prefix_argv:
        return
    os.environ[SANDBOXED_ENV] = '1'
    os.execv(prefix_argv[0], prefix_argv + [sys.executable, *sys.argv])


def exec_of_copy_denied(executables: Sequence[str]) -> str:
    """Try to ``exec`` a copy of an allowed binary from a writable directory.

    A native grandchild needs no Python guard, so this is the check that the
    boundary is the operating system: starting the copy *directly* must be
    refused. It proves only that syscall: on Linux an allowed loader can still
    map and start the copy, which is why the audit hook is the exec authority
    there and this probe is not a claim that arbitrary execution is impossible.
    """
    import subprocess
    import tempfile

    allowed = sorted({os.path.realpath(path) for path in executables})
    source = next((path for path in allowed if os.path.isfile(path)), None)
    if source is None:
        return 'skipped'
    directory = tempfile.mkdtemp(dir=tempfile.gettempdir())
    copy = os.path.join(directory, 'copied-allowed-binary')
    shutil.copyfile(source, copy)
    os.chmod(copy, 0o755)
    try:
        subprocess.run([copy, '--version'], capture_output=True, timeout=60)
    except (PermissionError, FileNotFoundError):
        return 'denied'
    except OSError as error:
        return 'denied' if error.errno in (errno.EPERM, errno.EACCES) else 'allowed'
    finally:
        shutil.rmtree(directory, ignore_errors=True)
    return 'allowed'
