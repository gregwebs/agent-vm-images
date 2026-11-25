#!/usr/bin/env python3
"""Fixture harness for the hermetic Rust toolchain suites.

The two suites are black-box since #245: they build the layer's helper and the
`avm-pin` writer, drive them as child processes over a synthetic signed
distribution, and assert on status, streams and the bytes left on disk. Nothing
here imports the code under test, so this module holds only what a suite needs to
*arrange* a row: the signed fixture itself, a copy of a pin directory, a tree
digest, the two builds, the emulated toolchain farm, the gpg wrappers and a
loopback byte store for the install rows.

`fixture.py` builds the distribution; this module builds the programs.
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import urllib.parse
from dataclasses import dataclass
from pathlib import Path

FIXTURES = Path(__file__).resolve().parent
REPOSITORY_ROOT = Path(__file__).resolve().parents[4]

sys.path.insert(0, str(FIXTURES))
import fixture  # noqa: E402

TOOLS = 'tools'
# The fixture row that stages the pin this suite reads. The date is fixed and in
# the past: a row that re-stages does not acquire a new review date.
STAGED_ON = '2026-01-02'
STAGED_BY = 'fixture-reviewer'


def sha_of_bytes(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _gpg_path() -> str:
    found = shutil.which('gpg')
    assert found is not None, 'gpg is a prerequisite'
    return os.path.realpath(found)


def host_os() -> str:
    return 'darwin' if sys.platform == 'darwin' else 'linux'


def build_helper(binary_directory: Path, fixture_seams: bool) -> str:
    """One build of the layer's helper, through the reviewed host wrapper."""
    binary_directory.mkdir(parents=True, exist_ok=True)
    arguments = ['bash', str(REPOSITORY_ROOT / 'script' / 'build' / 'example-helper.sh'),
                 'rust-dev', '--root', str(REPOSITORY_ROOT), '--os', host_os()]
    if fixture_seams:
        arguments.append('--fixture')
    completed = subprocess.run(arguments, capture_output=True, text=True)
    if completed.returncode != 0:
        raise SystemExit(f'cannot build the layer helper: {completed.stderr.strip()}')
    return completed.stdout.strip()


def build_writer(directory: Path) -> Path:
    """The fixture build of the pin writer: the only one that reads FIXTURE_DIST."""
    directory.mkdir(parents=True, exist_ok=True)
    binary = directory / 'avm-pin'
    build = subprocess.run(['go', 'build', '-trimpath', '-buildvcs=false',
                            '-tags', 'example_fixture', '-o', str(binary),
                            './cmd/avm-pin'],
                           cwd=str(REPOSITORY_ROOT), capture_output=True, text=True,
                           env=dict(os.environ, GOFLAGS='-mod=vendor', GOPROXY='off'))
    if build.returncode != 0:
        raise SystemExit(f'cannot build the fixture avm-pin: {build.stderr[-2000:]}')
    return binary


@dataclass
class Fixture:
    """A saved distribution and the pin directory staged from it."""

    dist: fixture.Dist
    pinned: Path
    root: Path


def build(root: Path, writer: Path) -> Fixture:
    """Stage the fixture pin with the shipped writer, not with a fixture of it.

    The suite's input has to come from the program under test: a Python writer
    would be a second implementation of every rule the rows below assert.
    """
    root.mkdir(parents=True, exist_ok=True)
    upstream = root / 'upstream-fixture'
    dist = fixture.build(upstream)
    fixture.save(upstream, dist)
    dockerfile = root / 'Dockerfile'
    dockerfile.write_text(f'ARG RUST_TOOLCHAIN={fixture.CHANNEL}\nFROM fixture\n')
    out = root / 'pinned'
    completed = subprocess.run(
        [str(writer), 'rust-toolchain', 'stage',
         '--channel', fixture.CHANNEL, '--rustup', fixture.RUSTUP_VERSION,
         '--key', str(dist.key), '--current', str(root / 'absent'),
         '--out', str(out), '--dockerfile', str(dockerfile),
         '--verify-bootstrap-exception', STAGED_ON, '--reviewer', STAGED_BY],
        env=dict(os.environ, FIXTURE_DIST=str(upstream)),
        cwd=str(root), capture_output=True, text=True, timeout=600)
    if completed.returncode != 0:
        raise SystemExit('cannot stage the fixture pin: '
                         + completed.stdout[-1500:] + completed.stderr[-1500:])
    return Fixture(dist=dist, pinned=out / 'rust-toolchain', root=root)


def load_fixture(destination: Path, pinned: Path) -> Fixture:
    """A Fixture over a saved distribution (e.g. a second upstream build)."""
    return Fixture(dist=fixture.load(destination), pinned=pinned, root=destination)


def copy_pin(source: Path, destination: Path) -> Path:
    shutil.rmtree(destination, ignore_errors=True)
    shutil.copytree(source, destination)
    return destination


def digest_tree(directory: Path) -> dict[str, str]:
    return {path.relative_to(directory).as_posix(): sha_of_bytes(path.read_bytes())
            for path in sorted(directory.rglob('*')) if path.is_file()}


def run_go(binary: str | Path, argv, env: dict[str, str], timeout: float = 300.0,
           cwd: str | None = None):
    """Run one built program with exactly env."""
    completed = subprocess.run([str(binary), *argv], env=dict(env), cwd=cwd,
                               capture_output=True, text=True, timeout=timeout)
    return completed.returncode, completed.stdout, completed.stderr


def install_tools(directory: Path, real_gpg: str, python: str,
                  files: dict[str, bytes]) -> dict[str, str]:
    """Build the emulated toolchain, the fake bootstrap and the gpg wrappers.

    They are fixture *programs*, not seams in the code under test: the helper runs
    them through the environment it builds, so a wrapper is reached only through
    PATH and a cargo home, exactly as a real toolchain is. `files` is the
    distribution the fake bootstrap holds the installed bootstrap's digest to.
    """
    tools = directory / TOOLS
    cargo = tools / 'cargo'
    absent = tools / 'cargo-absent'
    (cargo / 'bin').mkdir(parents=True, exist_ok=True)
    (absent / 'bin').mkdir(parents=True, exist_ok=True)
    emulator = (FIXTURES / 'fake_toolchain.py').read_text()
    for name in ('rustup', 'cargo', 'rustc', 'rustfmt'):
        path = cargo / 'bin' / name
        path.write_text(f'#!{python} -I\n{emulator}')
        path.chmod(0o755)
        if name == 'rustup':
            continue
        # A cargo home with no rustup at all: the row where the tool the checks
        # start with is absent.
        copy = absent / 'bin' / name
        copy.write_text(f'#!{python} -I\n{emulator}')
        copy.chmod(0o755)
    bootstrap_directory = tools / 'bootstrap'
    bootstrap_directory.mkdir(parents=True, exist_ok=True)
    bootstrap = bootstrap_directory / 'fake-bootstrap'
    bootstrap.write_text(f'#!{python} -I\n' + (FIXTURES / 'fake_bootstrap.py').read_text())
    bootstrap.chmod(0o755)
    digests = {}
    for arch, host in fixture.HOSTS.items():
        url = (f'https://static.rust-lang.org/rustup/archive/{fixture.RUSTUP_VERSION}/'
               f'{host}/rustup-init')
        digests[host] = sha_of_bytes(files[url])
    (bootstrap_directory / 'digests.json').write_text(json.dumps(digests))
    wrapper_source = (FIXTURES / 'fake_gpg.py').read_text()
    for mode in ('gpg-record', 'gpg-fail', 'gpg-expire'):
        mode_directory = tools / mode
        mode_directory.mkdir(parents=True, exist_ok=True)
        path = mode_directory / 'gpg'
        path.write_text(f'#!{python} -I\n'
                        + wrapper_source.replace('@REAL_GPG@', real_gpg))
        path.chmod(0o755)
    return {'cargo': str(cargo), 'cargo_absent': str(absent),
            'bootstrap': str(bootstrap)}


def remove_tools(directory: Path, prefix: str) -> None:
    """Remove the fixture tool farm, refusing anything that is not ours."""
    resolved = Path(directory).resolve()
    temporary = Path(tempfile.gettempdir()).resolve()
    if temporary not in resolved.parents or not resolved.name.startswith(prefix):
        raise AssertionError(f'refusing to remove {resolved}: not a {prefix}* under '
                             f'{temporary}')
    shutil.rmtree(resolved)


class LoopbackDistribution:
    """A listener that serves the fixture byte store to the shipped HTTP client.

    The binary gets only an exact-URL rewrite onto this origin; the listener is
    here. It sets `Content-Length` so the client's truncation check is exercised,
    and records every path it was asked for, which is how a row counts fetches.
    """

    def __init__(self, files: dict[str, bytes]) -> None:
        import http.server
        import socketserver

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

        class Server(http.server.ThreadingHTTPServer):
            # BaseHTTPServer.server_bind calls socket.getfqdn, which is an
            # off-host resolver call. Nothing here needs a name: the origin is
            # always 127.0.0.1.
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
