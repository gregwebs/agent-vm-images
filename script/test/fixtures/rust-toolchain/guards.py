#!/usr/bin/env python3
"""Deny-by-default network and process guard for fixture children.

Why a guard at all: the fixture seams (a byte store for transport, a recorder for
processes) only cover code that uses them. A direct `urllib` call, a `curl`
fallback or an unregistered subprocess in the module under test would reach the
maintainer's network or processes without any test noticing, so a green suite
would not establish the no-fallback property it claims.

Installed *before* the modules under test are imported, and allowlisted by
resolved absolute executable: matching a basename would let any file named `gpg`
run, which is not an allowlist. Everything else — an off-host socket, an
unsolicited executable, `os.system`, a nested interpreter — is refused.

This hook is the exec authority for fixture children on Linux. The OS layer in
``sandbox.py`` provides the network namespace and filesystem view there, and a
direct-exec deny list, but not an executable allowlist (an allowed loader can
start another program without a kernel ``exec`` of it). On macOS ``sandbox-exec``
enforces the executable allowlist as well. Neither layer replaces this one: only
this hook sees a Python child's *intent* to start a program.

Loopback is the one network exception: the transport's whole-request deadline is
proved against a real trickling peer on 127.0.0.1, and a listener the test itself
opened is not the maintainer's network. Every non-loopback address is refused
before a packet exists, which is what the guard controls assert.
"""
from __future__ import annotations

import os
import sys
from typing import Sequence

_PROCESS_EVENTS = ('subprocess.Popen', 'os.exec', 'os.posix_spawn', 'os.system', 'os.spawn')
_LOOPBACK_HOSTS = frozenset(('127.0.0.1', '::1', '::ffff:127.0.0.1', 'localhost'))
_ADDRESS_EVENTS = ('socket.connect', 'socket.bind', 'socket.sendto', 'socket.sendmsg')


def resolve(executable: str) -> str:
    return os.path.realpath(executable)


def install(allowed: Sequence[str]) -> None:
    allowed_paths = {resolve(path) for path in allowed}

    def identities(event: str, arguments: tuple) -> tuple[object, object]:
        """(executable, argv) for one process audit event.

        `subprocess.Popen` reports the *program that will run* first and its
        argument vector second. Authorizing the argument vector instead -- as if
        `executable=` did not exist -- lets `Popen([gpg, ...], executable=...)`
        run anything at all, so the executable identity is the one checked, and
        argv[0] must agree with it when it is a path of its own.
        """
        if event == 'subprocess.Popen':
            return arguments[0], arguments[1]
        if event == 'os.system':
            # A shell string has no executable identity to authorize.
            return arguments[0], None
        if event == 'os.exec':
            return arguments[0], arguments[1]
        if event == 'os.posix_spawn':
            return arguments[0], arguments[1]
        # os.spawn*: (mode, path, argv, env)
        return arguments[1], arguments[2]

    def authorized(identity: object) -> bool:
        if isinstance(identity, (list, tuple)):
            identity = identity[0] if identity else None
        if not isinstance(identity, (str, bytes, os.PathLike)):
            return False
        try:
            return resolve(os.fsdecode(identity)) in allowed_paths
        except (OSError, ValueError):
            return False

    def loopback(event: str, arguments: tuple) -> bool:
        if event == 'socket.__new__':
            return True
        if event == 'socket.getaddrinfo':
            host = arguments[0] if arguments else None
        elif event in _ADDRESS_EVENTS:
            address = arguments[1] if len(arguments) > 1 else None
            host = address[0] if isinstance(address, (list, tuple)) and address else None
        else:
            return False
        if not isinstance(host, (str, bytes)):
            return False
        return os.fsdecode(host) in _LOOPBACK_HOSTS

    def hook(event: str, arguments: tuple) -> None:
        if event.startswith('socket.'):
            if loopback(event, arguments):
                return
            raise RuntimeError(f'off-host network access in a fixture child: {event}')
        if not event.startswith(_PROCESS_EVENTS):
            return
        executable, argv = identities(event, arguments)
        if authorized(executable) and (argv is None or authorized(argv)):
            return
        raise RuntimeError(f'unregistered process in a fixture child: {event} '
                           f'{executable!r} argv={argv!r}')

    sys.addaudithook(hook)
