#!/usr/bin/env python3
"""Getting into the fixture boundary, and telling a real entry from a forged marker.

``sandbox.reexec_into_sandbox`` runs twice per suite: the first pass re-execs into
the OS boundary (``sandbox-exec`` on macOS, ``bwrap`` on Linux) and the second
pass, already inside, applies what cannot be done at exec time -- Landlock's
direct-exec denial and the namespace-local loopback. The ``FIXTURE_SANDBOXED``
marker is therefore set *before* the boundary is complete, so on its own it proves
nothing: a child handed a forged marker carries one too, and marker, environment
and argv cannot tell the two apart.

The platforms cannot be treated alike, and this is the whole reason this module
exists:

* **macOS** applies the complete profile at exec, so a genuine second pass already
  has it. There a failing confinement proof under a set marker *is* the verdict:
  the marker was forged.
* **Linux** cannot. Landlock requires ``no_new_privs`` and is irreversible, so the
  second pass is this process's own work; until it runs, a genuine entry fails
  exactly the proof a forged marker fails. The supervisor therefore records its
  own network-namespace identity before it starts any child: ``bwrap
  --unshare-all`` puts a genuine entry in a different namespace, so an equal
  identity says the marker was forged outside and only a different one may go on
  to the second pass. That identity lives in the supervisor's scratch root rather
  than the environment, because the marker itself is in the environment.

Both suites' children (``script/test/gopls-closure-maintenance.py`` and
``launcher.py``) enter through :func:`enter`, so this ordering cannot drift
between them.
"""
from __future__ import annotations

import os
import sys
from pathlib import Path
from typing import Callable, Sequence

# The supervisor's own identity, written into the scratch root it also hands the
# child. A file rather than an environment value: the marker is inherited, so
# anything in the environment is what a forged child would claim for itself.
RECORDED_NETNS = 'supervisor-netns.txt'
NETNS = Path('/proc/self/ns/net')
FORGED_MARKER = 'the boundary could not be demonstrated (forged marker)'


class Refusal(RuntimeError):
    """The boundary could not be demonstrated; the caller exits 3 before importing
    anything under test."""


def identity(netns: Path = NETNS) -> str | None:
    """The network namespace behind a ``/proc/.../ns/net`` path, or ``None``.

    ``st_ino`` is the namespace's identity: two processes are in the same network
    namespace exactly when the inode numbers agree.
    """
    try:
        return str(os.stat(netns).st_ino)
    except OSError:
        return None


def record_supervisor(root: Path) -> None:
    """Record this process's network namespace for the children it is about to start."""
    (root / RECORDED_NETNS).write_text(f'{identity() or ""}\n')


def second_pass_witnessed(root: Path | None, *, platform: str = sys.platform,
                          netns: Path = NETNS) -> bool:
    """Whether an independent witness says this process is a genuine second pass.

    ``platform`` and ``netns`` are parameters, not conveniences: the control that
    proves both verdicts has to run on a host that is not Linux. On macOS the
    answer is always no -- there is nothing left for a second pass to apply, so a
    failed proof cannot be anything but a forged marker.
    """
    if not platform.startswith('linux'):
        return False
    recorded = _recorded(root)
    current = identity(netns)
    return recorded is not None and current is not None and recorded != current


def _recorded(root: Path | None) -> str | None:
    if root is None:
        return None
    try:
        text = (root / RECORDED_NETNS).read_text().strip()
    except OSError:
        return None
    return text or None


def enter(sandbox, prove: Callable[[], str], *, executables: Sequence[str],
          writable: Sequence[str], readable: Sequence[str], root: Path | None,
          witness: Callable[[Path | None], bool] = second_pass_witnessed) -> None:
    """Enter the boundary and prove it from inside, or raise :class:`Refusal`.

    ``prove`` returns the refusal detail for the boundary as it stands (empty when
    the proof holds); it is called again once entry has been attempted, because a
    first pass that could not re-exec is not a boundary either. ``root`` is the
    supervisor's scratch root, where its recorded namespace identity lives, and is
    ``None`` where the caller cannot know it -- which only ever refuses more.
    """
    refusal = prove()
    if not refusal:
        return
    if os.environ.get(sandbox.SANDBOXED_ENV) == '1' and not witness(root):
        # Nothing is applied yet on this path, so refusing here is the whole point:
        # a forged marker must not reach the code that would otherwise trust it.
        raise Refusal(f'{FORGED_MARKER}: {refusal}')
    _reexec(sandbox, executables, writable, readable)
    refusal = prove()
    if refusal:
        raise Refusal(f'the boundary could not be demonstrated: {refusal}')


def _reexec(sandbox, executables: Sequence[str], writable: Sequence[str],
            readable: Sequence[str]) -> None:
    try:
        sandbox.reexec_into_sandbox(executables, writable=list(writable),
                                    readable=list(readable), loopback=True)
    except sandbox.SandboxUnavailable as error:
        raise Refusal(str(error)) from None
