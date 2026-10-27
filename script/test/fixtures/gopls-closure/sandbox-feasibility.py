#!/usr/bin/env python3
"""Feasibility gate: can the unchanged fixture boundary run the pinned Go?

The gopls hermetic suites run the *real* ``go`` command inside the OS boundary in
``fixtures/rust-toolchain/sandbox.py``. That boundary was built for a gpg child
tree, so before any suite depends on it this probe proves the parts a Go child
needs, without touching the model or the stager:

* the pinned compiler builds and *inspects* a Linux amd64 and arm64 binary
  (``go version -m -json``); neither binary is executed on this host,
* a loopback GOPROXY serves one synthetic module that the real ``go`` fetches,
* a native ``go`` child cannot reach an off-host endpoint: on macOS the refusal
  is the operating system's own network-outbound denial (part of the boundary);
  on Linux the removed network namespace makes the refusal indistinguishable from
  a host with no route, so that leg is supplementary and the executable-denial
  controls below are the Linux proof,
* the fail-closed paths still refuse: an unavailable sandbox exits nonzero before
  anything runs, a direct exec of an allowed binary copied into writable scratch
  is denied, and an off-host Python socket is refused by the audit hook.

Run it with the absolute root of the same Go the layer pins:

    python3 script/test/fixtures/gopls-closure/sandbox-feasibility.py \
        --go-root /absolute/go1.27.3

A failure here is a stop-and-report gate. The proxy recorder below is *detection*
for the loopback case; the network boundary is the OS sandbox, and no weaker
substitute (a logging proxy, a scrubbed environment) is accepted in its place.
"""
from __future__ import annotations

import argparse
import http.server
import io
import json
import os
import subprocess
import sys
import tempfile
import threading
import zipfile
from pathlib import Path

sys.dont_write_bytecode = True
FIXTURES = Path(__file__).resolve().parent
REPOSITORY_ROOT = Path(__file__).resolve().parents[4]
sys.path.insert(0, str(FIXTURES.parent / 'rust-toolchain'))
# The test-side scratch remover lives beside this probe; Go's caches are
# read-only, so a plain rmtree would leave the tree behind on every run.
from scratch import remove_tree  # noqa: E402

PINNED_GO = 'go1.27.3'
OFF_HOST = ('192.0.2.1', 9)  # TEST-NET-1: no service, must never be reached
DENIAL_MARKERS = ('operation not permitted', 'permission denied')
LINUX_DENIAL_MARKERS = DENIAL_MARKERS + ('network is unreachable',)
PROBE_MODULE = 'example.test/probe'
PROBE_VERSION = 'v1.0.0'

Failures: list[str] = []


def check(name: str, condition: bool, detail: str = '') -> None:
    if condition:
        print(f'  ok   {name}')
        return
    Failures.append(name)
    print(f'  FAIL {name} {detail}')


def go_root(arguments: argparse.Namespace) -> Path:
    root = Path(arguments.go_root)
    if not root.is_absolute() or not (root / 'bin' / 'go').is_file():
        print(f'sandbox-feasibility: --go-root must be an absolute Go root with bin/go: {root}',
              file=sys.stderr)
        raise SystemExit(2)
    return root


def executables(root: Path) -> list[str]:
    """The pinned go plus every one of its own tools, resolved.

    A Go build starts ``compile``, ``link``, ``asm`` and friends from GOTOOLDIR by
    absolute path. Omitting them makes the boundary refuse a *correct* build, which
    would hide a real problem behind a false pass.
    """
    allowed = [os.path.realpath(str(root / 'bin' / 'go')), os.path.realpath(sys.executable)]
    for tool_directory in sorted((root / 'pkg' / 'tool').glob('*')):
        if not tool_directory.is_dir():
            continue
        for tool in sorted(tool_directory.iterdir()):
            if tool.is_file() and os.access(tool, os.X_OK):
                allowed.append(os.path.realpath(str(tool)))
    return sorted(set(allowed))


def child_environment(work: Path, root: Path, **overrides: str) -> dict[str, str]:
    env = {
        'PATH': f'{root}/bin:/usr/bin:/bin',
        'HOME': str(work / 'home'),
        'GOPATH': str(work / 'gopath'),
        'GOMODCACHE': str(work / 'gopath' / 'pkg' / 'mod'),
        'GOCACHE': str(work / 'gocache'),
        'TMPDIR': str(work / 'tmp'),
        'GOENV': 'off',
        'GOWORK': 'off',
        'GOFLAGS': '',
        'GOTOOLCHAIN': 'local',
        'CGO_ENABLED': '0',
        'GONOSUMDB': '',
        'GONOPROXY': '',
        'GOPRIVATE': '',
        'GOINSECURE': '',
        'GOAUTH': 'off',
        'GOVCS': '*:off',
        'GOSUMDB': 'off',
    }
    env.update(overrides)
    for directory in ('home', 'gopath', 'gocache', 'tmp'):
        (work / directory).mkdir(parents=True, exist_ok=True)
    return env


def run_go(root: Path, argv: list[str], env: dict[str, str], cwd: Path,
           timeout: int = 300) -> subprocess.CompletedProcess:
    return subprocess.run([str(root / 'bin' / 'go'), *argv], cwd=str(cwd), env=env,
                          capture_output=True, text=True, timeout=timeout)


# --- a minimal GOPROXY --------------------------------------------------------
def synthetic_module() -> dict[str, bytes]:
    go_mod = f'module {PROBE_MODULE}\n\ngo 1.26.0\n'.encode()
    source = b'package probe\n\nfunc Name() string { return "probe" }\n'
    buffer = io.BytesIO()
    with zipfile.ZipFile(buffer, 'w') as archive:
        for name, raw in (('go.mod', go_mod), ('probe.go', source)):
            archive.writestr(f'{PROBE_MODULE}@{PROBE_VERSION}/{name}', raw)
    info = json.dumps({'Version': PROBE_VERSION,
                       'Time': '2024-01-01T00:00:00Z'}).encode()
    prefix = f'/{PROBE_MODULE}/@v/{PROBE_VERSION}'
    return {prefix + '.info': info, prefix + '.mod': go_mod, prefix + '.zip': buffer.getvalue()}


class Proxy:
    """A loopback GOPROXY that records request paths."""

    def __init__(self, files: dict[str, bytes]) -> None:
        self.requests: list[str] = []
        recorder = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self) -> None:  # noqa: N802 - http.server protocol name
                recorder.requests.append(self.path)
                raw = files.get(self.path)
                if raw is None:
                    self.send_error(404)
                    return
                self.send_response(200)
                self.send_header('Content-Length', str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def log_message(self, *_arguments) -> None:
                pass

        self._server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.port = self._server.server_address[1]
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    def url(self) -> str:
        return f'http://127.0.0.1:{self.port}'

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()
        self._thread.join(timeout=10)


# --- cases --------------------------------------------------------------------
def test_compile_and_inspect(root: Path, work: Path) -> None:
    print('compile: Linux amd64 and arm64 are built and inspected, never executed')
    module = work / 'tiny'
    module.mkdir(parents=True, exist_ok=True)
    (module / 'go.mod').write_text('module example.test/tiny\n\ngo 1.26.0\n')
    (module / 'main.go').write_text(
        'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("tiny") }\n')
    for arch in ('amd64', 'arm64'):
        env = child_environment(work / f'env-{arch}', root, GOOS='linux', GOARCH=arch,
                                GOPROXY='off')
        binary = work / f'tiny-{arch}'
        built = run_go(root, ['build', '-mod=readonly', '-trimpath', '-o', str(binary), '.'], env,
                       module)
        check(f'linux/{arch} builds', built.returncode == 0, built.stderr[-800:])
        if built.returncode != 0:
            continue
        inspected = run_go(root, ['version', '-m', '-json', str(binary)], env, module)
        check(f'linux/{arch} is inspected', inspected.returncode == 0, inspected.stderr[-400:])
        if inspected.returncode != 0:
            continue
        document = json.loads(inspected.stdout)
        settings = {item['Key']: item['Value'] for item in document.get('Settings', [])}
        check(f'linux/{arch} build info pairs the pinned compiler and target',
              (document.get('GoVersion') or '').removeprefix('go') == PINNED_GO.removeprefix('go')
              and settings.get('GOOS') == 'linux' and settings.get('GOARCH') == arch,
              f'({document.get("GoVersion")}, {settings.get("GOOS")}/{settings.get("GOARCH")})')
        check(f'linux/{arch} binary is an ELF object, not a host program',
              binary.read_bytes()[:4] == b'\x7fELF')


def test_loopback_fetch(root: Path, work: Path) -> None:
    print('fetch: the real go downloads a module from a loopback GOPROXY')
    proxy = Proxy(synthetic_module())
    try:
        module = work / 'fetch'
        module.mkdir(parents=True, exist_ok=True)
        (module / 'go.mod').write_text('module example.test/fetch\n\ngo 1.26.0\n')
        env = child_environment(work / 'fetch-env', root, GOPROXY=proxy.url(),
                                GOSUMDB='off', GOFLAGS='-mod=mod')
        fetched = run_go(root, ['mod', 'download', '-json',
                                f'{PROBE_MODULE}@{PROBE_VERSION}'], env, module)
        check('a loopback module download succeeds', fetched.returncode == 0,
              fetched.stderr[-800:])
        check('the proxy served the module .mod and .zip',
              f'/{PROBE_MODULE}/@v/{PROBE_VERSION}.mod' in proxy.requests
              and f'/{PROBE_MODULE}/@v/{PROBE_VERSION}.zip' in proxy.requests,
              f'({proxy.requests})')
        check('no other origin was requested',
              all(path.startswith(f'/{PROBE_MODULE}/') for path in proxy.requests),
              f'({proxy.requests})')
    finally:
        proxy.close()


def off_host_download(root: Path, work: Path) -> subprocess.CompletedProcess:
    module = work / 'off-host'
    module.mkdir(parents=True, exist_ok=True)
    (module / 'go.mod').write_text('module example.test/offhost\n\ngo 1.26.0\n')
    env = child_environment(work / 'off-host-env', root, GOPROXY=f'http://{OFF_HOST[0]}:{OFF_HOST[1]}',
                            GOSUMDB='off', GOFLAGS='-mod=mod')
    return run_go(root, ['mod', 'download', '-json', 'example.test/isolation@v1.0.0'], env, module,
                  timeout=120)


def test_off_host_denial(root: Path, work: Path) -> None:
    print('boundary: a native go child cannot reach an off-host endpoint')
    try:
        attempted = off_host_download(root, work)
    except subprocess.TimeoutExpired:
        check('the off-host attempt is denied by the operating system', False,
              '(timeout is inconclusive: the boundary did not refuse the connection)')
        return
    output = (attempted.stdout + attempted.stderr).lower()
    check('the off-host attempt does not succeed', attempted.returncode != 0,
          attempted.stdout[-400:])
    if sys.platform.startswith('linux'):
        # The removed network namespace is the Linux network boundary, but its
        # refusal (``network is unreachable``) is indistinguishable from a host
        # with no route to TEST-NET. This leg only records that the download did
        # not succeed; the executable-denial controls below are the Linux proof.
        check('the off-host download is refused (supplementary on Linux)',
              any(marker in output for marker in LINUX_DENIAL_MARKERS),
              attempted.stderr[-800:])
    else:
        # On macOS the sandbox profile's own network-outbound denial is the
        # operating system refusing the connection, not routing or DNS.
        check('the refusal is an operating-system denial',
              any(marker in output for marker in DENIAL_MARKERS), attempted.stderr[-800:])
    destination = work / 'off-host-env' / 'gopath' / 'pkg' / 'mod'
    published = [path for path in destination.rglob('isolation*')] if destination.exists() else []
    check('the refused download published nothing', not published, f'({published})')


def test_fail_closed(root: Path, work: Path, allowed: list[str]) -> None:
    print('controls: the boundary refuses rather than degrading')
    probe = Path(__file__).resolve()
    environment = {key: value for key, value in os.environ.items()
                   if key not in ('FIXTURE_SANDBOXED', 'FIXTURE_EXEC_RESTRICTED')}
    unavailable = dict(environment, FIXTURE_SANDBOX_UNAVAILABLE='1')
    result = subprocess.run([sys.executable, str(probe), '--go-root', str(root),
                             '--only-fail-closed-check'], env=unavailable, capture_output=True,
                            text=True, timeout=300)
    check('a host without the isolation prerequisite exits nonzero',
          result.returncode != 0 and 'ISOLATION-UNAVAILABLE' in result.stderr,
          f'(status {result.returncode}) {result.stderr[-300:]}')

    import sandbox

    check('a direct exec of an allowed binary copied into writable scratch is denied',
          sandbox.exec_of_copy_denied(allowed) == 'denied')

    import guards

    guards_installed = getattr(guards.install, 'installed', False)
    if not guards_installed:
        guards.install(allowed)
        guards.install.installed = True  # type: ignore[attr-defined]
    import socket

    def refused() -> bool:
        try:
            socket.create_connection(OFF_HOST, timeout=5)
        except RuntimeError:
            return True
        except OSError:
            return False
        return False

    check('an off-host Python socket is refused by the audit hook', refused())

    def wrong_tool() -> bool:
        try:
            subprocess.run(['/usr/bin/true'], capture_output=True)
        except RuntimeError:
            return True
        except OSError:
            return False
        return False

    check('an unregistered program cannot run', wrong_tool())


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--go-root', required=True)
    parser.add_argument('--only-fail-closed-check', action='store_true',
                        help='internal: the missing-prerequisite control')
    arguments = parser.parse_args()
    root = go_root(arguments)

    import sandbox

    # The child must be confined from its first line, so the boundary goes in
    # before the guard and before any Go process: a native grandchild is not
    # covered by the Python audit hook.
    allowed = executables(root)
    # Created *after* the re-exec, like the suites do: a directory made before it is
    # owned by the process `exec` replaces, so no code in this one can remove it and
    # every run would leak it. The temp directories are already in the writable
    # allowlist below, so nothing about the boundary changes.
    try:
        sandbox.reexec_into_sandbox(
            allowed, writable=[tempfile.gettempdir(), '/tmp', '/private/tmp'],
            readable=[str(REPOSITORY_ROOT), str(root)], loopback=True)
    except sandbox.SandboxUnavailable as error:
        print(f'ISOLATION-UNAVAILABLE: {error}', file=sys.stderr)
        return 3
    if arguments.only_fail_closed_check:
        # Reached only when the prerequisite check above was skipped, which the
        # control must never do: a green result here would mean the fail-closed
        # path is bypassable.
        print('sandbox-feasibility: the missing-prerequisite control was not refused',
              file=sys.stderr)
        return 1

    scratch = Path(tempfile.mkdtemp(prefix='gopls-feasibility-'))
    print(f'platform: {sys.platform}; pinned compiler: {PINNED_GO}')
    check('the probe runs inside the OS boundary',
          os.environ.get(sandbox.SANDBOXED_ENV) == '1')
    version = run_go(root, ['env', 'GOVERSION'],
                     child_environment(scratch / 'version-env', root, GOPROXY='off'),
                     scratch)
    check('the selected Go is the pinned compiler',
          version.returncode == 0 and version.stdout.strip() == PINNED_GO,
          f'({version.stdout.strip() or version.stderr[-200:]})')

    try:
        test_compile_and_inspect(root, scratch)
        test_loopback_fetch(root, scratch)
        test_off_host_denial(root, scratch)
        test_fail_closed(root, scratch, allowed)
    finally:
        remove_tree(scratch)

    print()
    if Failures:
        print(f'FAILED {len(Failures)} case(s): {", ".join(Failures)}')
        return 1
    print('sandbox-feasibility: real Go runs inside the unchanged fixture boundary')
    return 0


if __name__ == '__main__':
    sys.exit(main())
