"""Bounded curl transport with no operator configuration or credentials."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import os
import hashlib
import json
import release_trace
import subprocess
from pathlib import Path


def public_env() -> dict[str, str]:
    # Deliberate allowlist: no proxies, netrc, curlrc, TLS overrides or tokens.
    return {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'HOME': '/nonexistent',
            'CURL_HOME': '/nonexistent', 'XDG_CONFIG_HOME': '/nonexistent'}


def transfer(argv: list[str], destination: Path, limit: int) -> None:
    """Bound bytes even with chunked/no-length bodies; discard partial outputs."""
    if limit <= 0:
        raise ValueError('positive HTTP byte limit required')
    command = ['curl', '-q', '--netrc-file', '/dev/null', '--max-filesize', str(limit)] + argv
    proc = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                            env=public_env())
    created = False
    try:
        assert proc.stdout is not None
        with destination.open('xb') as stream:
            created = True
            size = 0
            digest = hashlib.sha256()
            while chunk := proc.stdout.read(65536):
                size += len(chunk)
                if size > limit:
                    raise ValueError('HTTP response exceeds byte limit')
                stream.write(chunk)
                digest.update(chunk)
        status = proc.wait(timeout=10)
        # Public transfer evidence records actual received bytes, not a verdict stub.
        if '--config' not in argv:
            release_trace.emit(command, status, json.dumps({'received_bytes': size,
                'sha256': 'sha256:' + digest.hexdigest()}).encode())
        if status:
            raise ValueError(f'HTTP transport failed (curl exit {status})')
    except BaseException:
        if proc.poll() is None:
            proc.kill()
        proc.wait()
        if created:
            destination.unlink(missing_ok=True)
        raise
    finally:
        if proc.stdout is not None:
            proc.stdout.close()
