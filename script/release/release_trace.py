"""Scoped evidence sink for captured command and validated content outputs."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

from contextvars import ContextVar
from typing import Callable
from pathlib import Path
import json
import subprocess
import time

Sink = Callable[[list[str], int, bytes, bytes], None]
sink: ContextVar[Sink | None] = ContextVar('release_evidence_sink', default=None)


def emit(argv: list[str], status: int, stdout: bytes, stderr: bytes = b'') -> None:
    callback = sink.get()
    if callback is not None:
        callback(argv, status, stdout, stderr)


def capture(argv: list[str], *, path: Path, env: dict[str, str], timeout: int,
            rejection: bool = False, rejection_stderr: bool = False) -> bytes:
    """One captured-command status/evidence policy, separate from streaming builds."""
    started = time.monotonic()
    proc = subprocess.run(argv, env=env, capture_output=True, timeout=timeout)
    path.write_bytes((json.dumps({'argv': argv, 'status': proc.returncode,
        'duration_seconds': time.monotonic() - started}) + '\n').encode() + proc.stdout + proc.stderr)
    if (proc.returncode == 0) == rejection:
        raise ValueError(f'unexpected status {proc.returncode}: {argv}; evidence {path}')
    return proc.stdout + proc.stderr if rejection and rejection_stderr else proc.stdout
