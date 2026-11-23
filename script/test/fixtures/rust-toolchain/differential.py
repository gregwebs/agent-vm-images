#!/usr/bin/env python3
"""The differential oracle: Python's loader and Go's reader over the same bytes.

Python is the oracle, never a fallback. Every row runs both implementations and
the suite fails on any disagreement; a row that is only compared in Python, or a
suite that silently compares fewer rows than it lists, is the failure this module
exists to prevent. The comparison is the plan's:

* exit statuses are equal;
* on 0 both stdouts are the channel and Go's stderr is empty;
* on 1 both stderrs start with ``rust_toolchain: `` and contain every fragment the
  row names, and Go's stdout is empty;
* on 2 both stderrs print a usage line;
* the gpg transcript (the recording wrapper's argv, environment and keyring mode)
  is identical once the invocation's scratch path is normalized;
* the per-row TMPDIR is empty afterwards, and the pin directory's digest tree did
  not change.

A Python-side crash is the pre-ruled P1 divergence: the row may carry
``python_crashes=True`` only when Python's stderr really holds a traceback, and the
status must still agree (1).
"""
from __future__ import annotations

import contextlib
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import traceback
from pathlib import Path
import dataclasses
import hashlib

TOOLS = 'tools'
HELPERS = 'helpers'


def _sha256(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def digest_tree(directory: Path) -> dict[str, str]:
    """Every file under directory, by relative path and digest."""
    result: dict[str, str] = {}
    for path in sorted(directory.rglob('*')):
        if path.is_file() and not path.is_symlink():
            result[str(path.relative_to(directory))] = _sha256(
                path.read_bytes().decode('utf-8', 'replace'))
    return result


def install_tools(directory: Path, real_gpg: str, python: str) -> dict[str, list[str]]:
    """Build the fixture tool farm and the gpg wrappers.

    Everything the Go child may execute has to exist *before* the sandbox's exec
    allowlist is fixed, which is why this runs in the suite's first pass. Each
    program is a copy of a fixture script whose first line names the real
    interpreter, so the sandbox only ever sees an allowlisted literal.
    """
    tools = directory / TOOLS
    cargo = tools / 'cargo'
    absent = tools / 'cargo-absent'
    (cargo / 'bin').mkdir(parents=True, exist_ok=True)
    (absent / 'bin').mkdir(parents=True, exist_ok=True)
    emulator = (Path(__file__).resolve().parent / 'fake_toolchain.py').read_text()
    wrappers: list[str] = []
    programs: list[str] = []
    for name in ('rustup', 'cargo', 'rustc', 'rustfmt'):
        path = cargo / 'bin' / name
        path.write_text(f'#!{python} -I\n{emulator}')
        path.chmod(0o755)
        programs.append(str(path))
        if name == 'rustup':
            continue
        # A cargo home with no rustup at all: the P1 row where the tool the
        # checks start first is absent. A copy made later could not be executed
        # inside the boundary, which is the boundary's own control.
        copy = absent / 'bin' / name
        copy.write_text(f'#!{python} -I\n{emulator}')
        copy.chmod(0o755)
        programs.append(str(copy))
    bootstrap_directory = tools / 'bootstrap'
    bootstrap_directory.mkdir(parents=True, exist_ok=True)
    bootstrap = bootstrap_directory / 'fake-bootstrap'
    bootstrap.write_text(f'#!{python} -I\n'
                         + (Path(__file__).resolve().parent / 'fake_bootstrap.py').read_text())
    bootstrap.chmod(0o755)
    programs.append(str(bootstrap))
    wrapper_source = (Path(__file__).resolve().parent / 'fake_gpg.py').read_text()
    for mode in ('gpg-record', 'gpg-fail', 'gpg-expire'):
        mode_directory = tools / mode
        mode_directory.mkdir(parents=True, exist_ok=True)
        path = mode_directory / 'gpg'
        path.write_text(f'#!{python} -I\n'
                        + wrapper_source.replace('@REAL_GPG@', real_gpg))
        path.chmod(0o755)
        wrappers.append(str(path))
    return {'programs': programs, 'wrappers': wrappers,
            'cargo': str(cargo), 'cargo_absent': str(absent),
            'bootstrap': str(bootstrap)}


def run_python(argv, dependencies, tmpdir: Path, cwd: Path | None = None):
    """Run the shipped module in this process with the fixture dependencies.

    In-process is the only way to give Python the fixture trust: the CLI has no
    trust seam, and adding one to shipping code for a suite would be a second
    authority for the trust root. ``tempfile.tempdir`` is reset because
    ``mkdtemp`` caches the first TMPDIR it saw, and every row gets a fresh one.
    """
    import rust_toolchain as model  # imported by the caller already; same object

    previous_tmpdir = os.environ.get('TMPDIR')
    previous_cwd = os.getcwd()
    os.environ['TMPDIR'] = str(tmpdir)
    tempfile.tempdir = None
    stdout, stderr = io.StringIO(), io.StringIO()
    status = 0
    try:
        if cwd is not None:
            os.chdir(cwd)
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try:
                status = model.main(list(argv), dependencies)
            except SystemExit as exit_request:  # argparse's status 2, and --help
                code = exit_request.code
                status = 0 if code is None else int(code)
            except Exception:  # noqa: BLE001 - the source reader's own P1 behaviour
                # An uncaught exception in the shipped module is what the CLI
                # does: a traceback on stderr and status 1. Capturing it here is
                # what makes the pre-ruled P1 rows comparable at all.
                status = 1
                traceback.print_exc(file=stderr)
    finally:
        os.chdir(previous_cwd)
        tempfile.tempdir = None
        if previous_tmpdir is None:
            os.environ.pop('TMPDIR', None)
        else:
            os.environ['TMPDIR'] = previous_tmpdir
    return status, stdout.getvalue(), stderr.getvalue()


def run_go(binary: str, argv, env: dict[str, str], timeout: float = 300.0):
    """Run the shipped binary with exactly env."""
    completed = subprocess.run([binary, *argv], env=dict(env), capture_output=True,
                               text=True, timeout=timeout)
    return completed.returncode, completed.stdout, completed.stderr


def _normalize(value):
    """Replace this invocation's scratch path, and nothing else.

    The scratch directory name is random per invocation (each implementation makes
    its own 0700 `rust-verify-*`), so it is the one difference the comparison
    tolerates: in an argument and in every environment value that carries it.
    """
    if not isinstance(value, str) or '/rust-verify-' not in value:
        return value
    head, _, tail = value.partition('/rust-verify-')
    remainder = tail.split('/', 1)
    return head + '<SCRATCH>' + ('/' + remainder[1] if len(remainder) > 1 else '')


def transcript(row_directory: Path) -> list[dict]:
    """The recording wrapper's log for one row, normalized."""
    path = row_directory / 'gpg-transcript.jsonl'
    if not path.exists():
        return []
    entries = []
    for line in path.read_text().splitlines():
        entry = json.loads(line)
        entries.append({'mode': entry['mode'],
                        'argv': [_normalize(argument) for argument in entry['argv']],
                        'env': {key: _normalize(value)
                                for key, value in entry['env'].items()},
                        'keyring_mode': entry['keyring_mode']})
    return entries


class Comparison:
    """The verdict of one row, and what it compared."""

    def __init__(self, row: str) -> None:
        self.row = row
        self.problems: list[str] = []
        self.compared = True
        self.note = ''

    def require(self, condition: bool, message: str) -> None:
        if not condition:
            self.problems.append(f'{self.row}: {message}')

    @property
    def ok(self) -> bool:
        return not self.problems


def compare(row: str, python, go, fragments=(), channel: str = '',
            python_crashes: bool = False, python_transcript=(),
            go_transcript=(), python_tmpdir: Path | None = None,
            go_tmpdir: Path | None = None) -> Comparison:
    """Apply the plan's comparison key to one row's two results."""
    comparison = Comparison(row)
    py_status, py_out, py_err = python
    go_status, go_out, go_err = go
    comparison.require(py_status == go_status,
                       f'statuses differ: python {py_status} {py_err!r}; '
                       f'go {go_status} {go_err!r}')
    if py_status == 0:
        comparison.require(py_out == f'{channel}\n',
                           f'python stdout is {py_out!r}, not the channel')
        comparison.require(go_out == f'{channel}\n',
                           f'go stdout is {go_out!r}, not the channel')
        comparison.require(go_err == '', f'go stderr is {go_err!r}, not empty')
    elif py_status == 1:
        if python_crashes:
            comparison.require('Traceback' in py_err,
                               f'the row claims a Python crash but stderr is {py_err!r}')
        else:
            comparison.require(py_err.startswith('rust_toolchain: '),
                               f'python stderr does not carry the prefix: {py_err!r}')
        comparison.require(go_err.startswith('rust_toolchain: '),
                           f'go stderr does not carry the prefix: {go_err!r}')
        comparison.require(go_out == '', f'go stdout is {go_out!r}, not empty')
        for fragment in fragments:
            if not python_crashes:
                # A P1 row's Python text is a traceback, which is the pre-ruled
                # divergence: the fragment is the Go reader's diagnostic.
                comparison.require(fragment in py_err,
                                   f'python refusal lacks {fragment!r}: {py_err!r}')
            comparison.require(fragment in go_err,
                               f'go refusal lacks {fragment!r}: {go_err!r}')
    elif py_status == 2:
        comparison.require('usage' in py_err, f'python printed no usage: {py_err!r}')
        comparison.require('usage' in go_err, f'go printed no usage: {go_err!r}')
    if python_transcript or go_transcript:
        comparison.require(list(python_transcript) == list(go_transcript),
                           'the gpg transcripts differ:\n'
                           f'  python {python_transcript}\n  go       {go_transcript}')
    for label, directory in (('python', python_tmpdir), ('go', go_tmpdir)):
        if directory is None:
            continue
        left = sorted(entry.name for entry in Path(directory).iterdir())
        comparison.require(not left, f'{label} left {left} in TMPDIR')
    return comparison


def python_case(row: str, two_primaries: bool = False):
    """A row description that a Python-only control can reuse."""
    return dataclasses.make_dataclass('Case', [('row', str), ('two', bool)])(
        row, two_primaries)


def require_empty_tmpdir(path: Path) -> None:
    left = sorted(entry.name for entry in Path(path).iterdir())
    if left:
        raise AssertionError(f'TMPDIR was not empty: {left}')


def remove_tools(directory: Path, prefix: str) -> None:
    """Remove the fixture tool farm, refusing anything that is not ours.

    The suite creates it under the system temp directory in the first pass and
    removes it at the end of the second; a value outside the temp directory or
    without the prefix is refused rather than removed.
    """
    resolved = Path(directory).resolve()
    temporary = Path(tempfile.gettempdir()).resolve()
    if temporary not in resolved.parents or not resolved.name.startswith(prefix):
        raise AssertionError(f'refusing to remove {resolved}: not a {prefix}* under '
                             f'{temporary}')
    shutil.rmtree(resolved)


class LoopbackDistribution:
    """The harness-side listener for the Go install rows.

    The binary gets only an exact-URL rewrite onto this origin; the listener is
    here. It serves the same `dist.files` byte store the Python transport reads,
    sets `Content-Length` so the shipped client's truncation check is exercised,
    and records every path it was asked for. A path the pin does not name would
    be a 404, and the binary refuses it before it ever gets here.
    """

    def __init__(self, files: dict[str, bytes]) -> None:
        import http.server
        import urllib.parse

        parsed = {}
        for url, raw in files.items():
            parsed[urllib.parse.urlsplit(url).path] = raw
        self.files = parsed
        self.requests: list[str] = []
        outer = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - http.server's name
                outer.requests.append(self.path)
                raw = outer.files.get(self.path)
                if raw is None:
                    self.send_response(404)
                    self.send_header('Content-Length', '0')
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *arguments) -> None:
                pass

        import socketserver

        class Server(http.server.ThreadingHTTPServer):
            # BaseHTTPServer.server_bind calls socket.getfqdn, which is an
            # off-host resolver call and is refused inside the fixture boundary.
            # Nothing here needs a name: the origin is always 127.0.0.1.
            def server_bind(self):
                socketserver.TCPServer.server_bind(self)
                self.server_name, self.server_port = self.server_address[:2]

        self.server = Server(('127.0.0.1', 0), Handler)
        self.origin = f'http://127.0.0.1:{self.server.server_address[1]}'
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *arguments) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join()

    def paths(self) -> list[str]:
        return list(self.requests)


def interpreter() -> str:
    return os.path.realpath(sys.executable)
