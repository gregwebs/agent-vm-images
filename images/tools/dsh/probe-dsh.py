#!/usr/bin/env python3
"""Bounded credential-free web-readiness probe for the installed dsh.

A version report is not health, and neither is "a server answered on the port".
This driver owns the parts JavaScript cannot: monotonic deadlines, a private
process group, bounded concurrent capture, the token handshake and teardown that
proves nothing survived. The JavaScript probes stay in check-runtime.mjs.

Why the token URL is requested instead of the bare page: `dsh web` prints one
`dsh web: http://127.0.0.1:<port>/?token=<secret>` line. The token is
unguessable, so a frontend response that accepts it can only come from the child
this invocation started -- a competing server on the same port rejects it. On
Linux the listening socket must additionally appear in the child group's own
descriptors, because a valid-looking HTML page is not ownership proof either.

Timeout is never readiness: a child that is still starting, already exiting, or
blocked printing the Cordis HMR / sandbox failure must fail the gate.
"""
import argparse
import http.client
import http.cookies
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import socket
import subprocess
import sys
import threading
import tempfile
import time
from urllib.parse import urljoin, urlsplit

MARKERS = (b'<!doctype html', b'__dsh_boot__', b'<title>deepseek harness</title>')
URL_RE = re.compile(r'^dsh web: http://127\.0\.0\.1:(\d+)/\?token=([A-Za-z0-9._~-]{32,})$')
FATAL_MARKERS = ('requires the Cordis HMR service', 'ERR_MODULE_NOT_FOUND', 'Cannot find module')
MAX_CAPTURE = 64 * 1024
MAX_RESPONSE = 64 * 1024
# Bound for joining the two capture readers after the child group is gone.
DRAIN_SECONDS = 10
DEFAULT_READINESS_SECONDS = 60.0
DEFAULT_ESCALATION_SECONDS = 5.0
REQUEST_TIMEOUT_SECONDS = 1.0
POLL_SECONDS = 0.02
GROUP_SETTLE_SECONDS = 1.0
MAX_ATTEMPTS = 3
REDIRECTS = frozenset((301, 302, 303, 307, 308))
FIXTURE_ENV_PREFIX = 'DSH_WEB_FIXTURE_'


class Failure(Exception):
    def __init__(self, message, retryable=False):
        super().__init__(message)
        self.retryable = retryable


class UsageError(Exception):
    pass


class Interrupted(Exception):
    def __init__(self, status):
        super().__init__(f'signal {status}')
        self.status = status


def load_project():
    source = Path(__file__).resolve().with_name('project.py')
    spec = importlib.util.spec_from_file_location('dsh_project', source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def parse(argv):
    parser = argparse.ArgumentParser(prog='probe-dsh.py', add_help=False)
    parser.add_argument('--prefix', required=True)
    parser.add_argument('--command', required=True)
    parser.add_argument('--scratch-root', required=True)
    parser.add_argument('--readiness-seconds', type=float)
    parser.add_argument('--escalation-seconds', type=float)
    try:
        options = parser.parse_args(argv)
    except SystemExit:
        raise UsageError('usage: probe-dsh.py --prefix PREFIX --command COMMAND --scratch-root OWNED_ROOT '
                         '[--readiness-seconds N] [--escalation-seconds N]')
    for value in (options.readiness_seconds, options.escalation_seconds):
        if value is not None and not 0 < value <= DEFAULT_READINESS_SECONDS:
            raise UsageError('probe bounds must be positive and at most the production deadline')
    return options


def child_environment(work, production, exec_tmp):
    # An allowlist, not a scrub: a harness token, proxy or caller config must not
    # reach the web process, and PATH/LANG are all a real dsh needs from outside.
    home = work / 'home'
    environment = {key: os.environ[key] for key in ('PATH', 'LANG', 'LC_ALL', 'TZ') if key in os.environ}
    environment.update({
        'HOME': str(home),
        'DSH_HOME': str(home / 'dsh'),
        'XDG_CONFIG_HOME': str(work / 'config'),
        'XDG_CACHE_HOME': str(work / 'cache'),
        'XDG_DATA_HOME': str(work / 'data'),
        'XDG_STATE_HOME': str(work / 'state'),
        'TMPDIR': str(exec_tmp), 'TMP': str(exec_tmp), 'TEMP': str(exec_tmp),
    })
    if not production:
        # Fixture-only seam: the fake command is authorized under the fixture
        # root, and it needs its own case/script selection. Production never sees
        # these names.
        for key, value in os.environ.items():
            if key.startswith(FIXTURE_ENV_PREFIX):
                environment[key] = value
    return environment


class Capture:
    """Bounded, concurrent capture of both pipes; overflow is a hard failure."""

    def __init__(self, process):
        self._buffers = {'stdout': bytearray(), 'stderr': bytearray()}
        self._lock = threading.Lock()
        self.overflow = False
        self.error = None
        self._threads = [threading.Thread(target=self._pump, args=(process.stdout, 'stdout'), daemon=True),
                         threading.Thread(target=self._pump, args=(process.stderr, 'stderr'), daemon=True)]
        for thread in self._threads:
            thread.start()

    def _pump(self, stream, name):
        try:
            while True:
                # read1: a buffered read(n) would block until n bytes arrive,
                # so a single startup line would never be visible in time.
                chunk = stream.read1(4096)
                if not chunk:
                    return
                with self._lock:
                    room = MAX_CAPTURE - len(self._buffers['stdout']) - len(self._buffers['stderr'])
                    if room <= 0:
                        self.overflow = True
                        continue
                    self._buffers[name].extend(chunk[:room])
                    # A chunk that only partly fits is truncated output, so the bound
                    # is exceeded even though some bytes were kept.
                    if len(chunk) > room:
                        self.overflow = True
        except (OSError, ValueError) as error:
            # A stream that dies mid-read hides the child's own diagnostic, so the
            # probe must fail instead of reporting readiness from unobserved output.
            self.error = f'{name}: {error}'
            return

    def drain(self, timeout=DRAIN_SECONDS):
        """Wait for both readers so a late read error is still reported.

        Readers end when both pipes close, which normally happens as soon as the
        child is reaped; a surviving writer is caught by the ownership/cleanup
        checks instead of here. Joining first is what makes a delayed read error
        (one that lands after the readiness response) a failure rather than a
        success with unobserved output.
        """
        for thread in self._threads:
            thread.join(timeout)
        return self.error

    def text(self, name):
        with self._lock:
            return bytes(self._buffers[name]).decode('utf-8', 'replace')


class Response:
    def __init__(self, status, headers, body):
        self.status = status
        self.headers = headers
        self.body = body

    def header(self, name):
        for key, value in self.headers:
            if key.lower() == name.lower():
                return value
        return None

    def cookie_header(self):
        jar = {}
        for key, value in self.headers:
            if key.lower() != 'set-cookie':
                continue
            cookie = http.cookies.SimpleCookie()
            try:
                cookie.load(value)
            except http.cookies.CookieError:
                continue
            for name, morsel in cookie.items():
                jar[name] = morsel.value
        return '; '.join(f'{name}={value}' for name, value in jar.items()) or None


def reserve_port():
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(('127.0.0.1', 0))
        return listener.getsockname()[1]


def port_free(port):
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            listener.bind(('127.0.0.1', port))
        except OSError:
            return False
    return True


def request(url, cookie=None):
    parts = urlsplit(url)
    target = parts.path or '/'
    if parts.query:
        target += '?' + parts.query
    headers = {'Host': f'{parts.hostname}:{parts.port}', 'Connection': 'close', 'Accept': 'text/html'}
    if cookie:
        headers['Cookie'] = cookie
    connection = http.client.HTTPConnection(parts.hostname, parts.port, timeout=REQUEST_TIMEOUT_SECONDS)
    try:
        connection.request('GET', target, headers=headers)
        response = connection.getresponse()
        body = response.read(MAX_RESPONSE + 1)
        return Response(response.status, response.getheaders(), body)
    finally:
        connection.close()


def try_request(url, cookie=None):
    try:
        return request(url, cookie)
    except (http.client.HTTPException, OSError):
        return None


def parse_url(text, port):
    lines = text.splitlines()
    if text and not text.endswith('\n'):
        lines = lines[:-1]
    for line in lines:
        line = line.strip()
        if not line.startswith('dsh web:'):
            continue
        match = URL_RE.match(line)
        if not match:
            raise Failure(f'unexpected web url line: {line[:200]}')
        advertised = int(match.group(1))
        if advertised != port:
            raise Failure(f'web advertised port {advertised}, but this invocation reserved {port}')
        return f'http://127.0.0.1:{port}/?token={match.group(2)}'
    return None


def frontend(outcome, port):
    """Return the readiness facts, or None while the port is not up yet."""
    redirects = 0
    if outcome.status in REDIRECTS:
        location = outcome.header('Location')
        if not location:
            raise Failure('token request redirected without a Location header')
        target = urljoin(f'http://127.0.0.1:{port}/', location)
        parts = urlsplit(target)
        if parts.scheme != 'http' or parts.hostname != '127.0.0.1' or parts.port != port:
            raise Failure(f'redirect left the probed origin: {parts.scheme}://{parts.hostname}:{parts.port}')
        final = try_request(target, outcome.cookie_header())
        if final is None:
            return None
        redirects = 1
    elif outcome.status == 200:
        final = outcome
    else:
        raise Failure(f'token request returned HTTP {outcome.status}')
    if final.status != 200:
        raise Failure(f'final frontend response returned HTTP {final.status}')
    if len(final.body) > MAX_RESPONSE:
        raise Failure(f'frontend response exceeded {MAX_RESPONSE} bytes')
    lowered = final.body.lower()
    missing = [marker.decode() for marker in MARKERS if marker not in lowered]
    if missing:
        raise Failure('response is not the shipped dsh frontend (missing ' + ', '.join(missing) + ')')
    return {'status': final.status, 'redirects': redirects, 'bytes': len(final.body),
            'markers': [marker.decode() for marker in MARKERS]}


def await_ready(process, capture, port, deadline, readiness):
    url = None
    while True:
        stdout = capture.text('stdout')
        stderr = capture.text('stderr')
        if capture.overflow:
            raise Failure(f'startup output exceeded {MAX_CAPTURE} bytes')
        for marker in FATAL_MARKERS:
            for text in (stdout, stderr):
                if marker not in text:
                    continue
                line = next((item for item in text.splitlines() if marker in item), marker)
                raise Failure(f'fatal startup diagnostic: {line.strip()[:300]}')
        if url is None:
            url = parse_url(stdout, port)
        status = process.poll()
        if status is not None:
            # A crashed Node process ends with its version banner, so the last line
            # alone hides the real cause. Publish a bounded tail of both streams.
            tail = '\n'.join(item for item in (stderr.strip()[-6000:], stdout.strip()[-600:]) if item)
            tail = re.sub(r'(token=)[A-Za-z0-9._~-]+', r'\1<redacted>', tail)
            detail = tail or f'exit status {status}'
            raise Failure(f'web exited before readiness:\n{detail}',
                          retryable='EADDRINUSE' in stderr or 'address already in use' in stderr)
        if url is not None:
            outcome = try_request(url)
            if outcome is not None:
                facts = frontend(outcome, port)
                if facts is not None:
                    return url, facts
        if time.monotonic() >= deadline:
            raise Failure(f'web readiness deadline of {readiness} s elapsed')
        time.sleep(POLL_SECONDS)


def group_alive(pgid):
    try:
        os.killpg(pgid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        # No member of the group is ours any more; the id was reused.
        return False
    return True


def wait_group_empty(process, pgid, seconds):
    until = time.monotonic() + seconds
    while group_alive(pgid):
        # An exited-but-unreaped leader is itself a group member, so reap it
        # while waiting; otherwise the group can never look empty.
        process.poll()
        if time.monotonic() >= until:
            return False
        time.sleep(POLL_SECONDS)
    return True


def group_members(pgid):
    members = []
    if not os.path.isdir('/proc'):
        return members
    for entry in os.listdir('/proc'):
        if not entry.isdigit():
            continue
        try:
            with open(f'/proc/{entry}/stat', encoding='utf-8') as stat:
                data = stat.read()
        except OSError:
            continue
        fields = data[data.rfind(')') + 2:].split()
        if len(fields) > 2 and fields[2] == str(pgid):
            members.append(int(entry))
    return members


def terminate(process, escalation):
    pgid = process.pid
    signalled = []
    for name, sig in (('SIGTERM', signal.SIGTERM), ('SIGKILL', signal.SIGKILL)):
        if process.poll() is not None and not group_alive(pgid):
            break
        try:
            os.killpg(pgid, sig)
            signalled.append(name)
        except ProcessLookupError:
            pass
        except PermissionError:
            # Darwin can return EPERM for a group containing only the just-exited
            # leader. A live leader still makes this a hard teardown failure.
            if process.poll() is None:
                raise
        if wait_group_empty(process, pgid, escalation):
            break
    try:
        process.wait(timeout=max(escalation, GROUP_SETTLE_SECONDS))
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait(timeout=max(escalation, GROUP_SETTLE_SECONDS))
    if group_alive(pgid):
        raise Failure('process group still has members after the leader was reaped: ' + str(group_members(pgid)))
    return {'signalled': signalled, 'survivors': 0}


def listener_inodes(port, proc=Path('/proc')):
    found = set()
    for table in (proc / 'net/tcp', proc / 'net/tcp6'):
        try:
            with open(table, encoding='ascii') as handle:
                lines = handle.read().splitlines()
        except OSError:
            return None
        for line in lines[1:]:
            fields = line.split()
            if len(fields) < 10 or fields[3] != '0A':
                continue
            try:
                local_port = int(fields[1].rsplit(':', 1)[1], 16)
            except (IndexError, ValueError):
                continue
            if local_port == port:
                found.add(fields[9])
    return found


def descriptor_links(pid, proc=Path('/proc')):
    links = []
    directory = proc / f'{pid}/fd'
    try:
        entries = os.listdir(directory)
    except OSError:
        return links
    for entry in entries:
        try:
            links.append(os.readlink(os.path.join(directory, entry)))
        except OSError:
            continue
    return links


def ownership(port, pgid, *, production=False, proc=Path('/proc'), host=sys.platform):
    # procfs is the only way to tie the listening socket to the child group. A
    # Linux run or a production capability that cannot read it has no ownership
    # proof at all, so that is a hard failure; only an explicit non-Linux fixture
    # records a skip, and the token handshake remains the proof there.
    unproven = production or host.startswith('linux')

    def skip(reason):
        if unproven:
            raise Failure(reason + ': listener ownership cannot be proven')
        return {'listenerInode': None, 'reason': 'fixture host without procfs: ' + reason}

    if not (proc / 'self/fd').is_dir():
        return skip('no procfs')
    inodes = listener_inodes(port, proc)
    if inodes is None:
        return skip('kernel socket tables unreadable')
    if not inodes:
        raise Failure(f'no listener owns the reserved port {port}')
    for pid in group_members(pgid):
        for link in descriptor_links(pid, proc):
            if link.startswith('socket:[') and link[8:-1] in inodes:
                return {'listenerInode': True, 'pid': pid}
    raise Failure('the reserved-port listener is not owned by the probed process group')


def run(capability, command, work, readiness, escalation, exec_tmp):
    environment = child_environment(work, capability.production, exec_tmp)
    cwd = work / 'cwd'
    cwd.mkdir()
    attempts = 0
    while True:
        attempts += 1
        port = reserve_port()
        process = subprocess.Popen(
            [str(command), 'web', '--host', '127.0.0.1', '--port', str(port), '--no-open'],
            cwd=str(cwd), env=environment, stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        capture = Capture(process)
        failure = None
        facts = None
        proof = None
        try:
            try:
                _, facts = await_ready(process, capture, port, time.monotonic() + readiness, readiness)
                if process.poll() is not None:
                    raise Failure('web exited immediately after the readiness response')
                proof = ownership(port, process.pid, production=capability.production)
            except Failure as error:
                failure = error
        finally:
            cleanup = terminate(process, escalation)
        if failure is not None:
            if failure.retryable and attempts < MAX_ATTEMPTS:
                continue
            raise failure
        # The readers drain before the success report: a read error or an overflow
        # that lands after the readiness response is still a failed proof, and a
        # reader that stopped at the capture bound would silently truncate it.
        incomplete = capture.drain()
        if incomplete is not None:
            raise Failure(f'the child output could not be read completely ({incomplete})')
        if capture.overflow:
            raise Failure(f'child output exceeded {MAX_CAPTURE} bytes')
        if not port_free(port):
            raise Failure(f'reserved port {port} still has a listener after cleanup')
        return {'command': str(command), 'port': port, 'url': f'http://127.0.0.1:{port}/',
                'http': facts, 'ownership': proof, 'cleanup': {**cleanup, 'portReleased': True},
                'attempts': attempts}


def self_test():
    """Ownership-degradation unit case: unproven procfs must never pass silently."""
    with tempfile.TemporaryDirectory(prefix='probe-dsh-self-test-') as directory:
        proc = Path(directory)
        for label, options in [('production capability', {'production': True}),
                               ('linux host', {'host': 'linux'})]:
            try:
                ownership(4321, 1, proc=proc, **options)
            except Failure as error:
                assert 'ownership cannot be proven' in str(error), (label, error)
            else:
                raise AssertionError(f'{label}: an unproven listener ownership passed')
        skipped = ownership(4321, 1, proc=proc, host='darwin')
        assert skipped == {'listenerInode': None,
                           'reason': 'fixture host without procfs: no procfs'}, skipped
    print('probe-dsh: ownership degradation self-test PASS')


def main(argv):
    if argv == ['--self-test']:
        self_test()
        return 0
    try:
        options = parse(argv)
    except UsageError as error:
        print(f'probe-dsh.py: {error}', file=sys.stderr)
        return 2
    project = load_project()
    try:
        project.guard_environment([options.prefix])
        capability = project.authorize_runtime(options.prefix, options.command, options.scratch_root)
        readiness = options.readiness_seconds or DEFAULT_READINESS_SECONDS
        escalation = options.escalation_seconds or DEFAULT_ESCALATION_SECONDS
        overrides = options.readiness_seconds is not None or options.escalation_seconds is not None
        if capability.production and overrides:
            raise UsageError('production readiness and escalation bounds are fixed')
        work = project.owned_scratch(capability, options.scratch_root)
        try:
            # The product dlopens its native internals binding through TMPDIR.
            # HOME may be a noexec tmpfs; a noexec TMPDIR is unsupported by dsh
            # web. Use a private directory on the system writable layer instead.
            parent = Path('/tmp')
            override = os.environ.get('AGENT_VM_FIXTURE_EXEC_TMPDIR')
            if override:
                if capability.production:
                    raise UsageError('production exec temp parent is fixed')
                parent = project.absolute(override)
                project.no_redirect(parent)
                project.below(parent, capability.root)
                if not parent.is_dir() or parent.stat().st_uid != os.getuid():
                    raise ValueError('fixture exec temp parent must be caller-owned')
            with tempfile.TemporaryDirectory(prefix='dsh-probe-', dir=parent) as temporary:
                exec_tmp = Path(temporary)
                os.chmod(exec_tmp, 0o700)
                report = run(capability, capability.command, work, readiness, escalation, exec_tmp)
            if exec_tmp.exists():
                raise Failure('private exec TMPDIR survived cleanup')
            report['cleanup']['execTmpRemoved'] = True
        finally:
            project.cleanup_scratch(work, (capability.prefix,))
        print(json.dumps(report, sort_keys=True))
        return 0
    except UsageError as error:
        print(f'probe-dsh.py: {error}', file=sys.stderr)
        return 2
    except Interrupted as error:
        print('dsh-runtime: web: interrupted', file=sys.stderr)
        return error.status
    except Failure as error:
        print(f'dsh-runtime: web: {error}', file=sys.stderr)
        return 1
    except (ValueError, OSError) as error:
        print(f'dsh-runtime: web: {error}', file=sys.stderr)
        return 1


def _interrupt(status):
    def handler(_signum, _frame):
        raise Interrupted(status)
    return handler


if __name__ == '__main__':
    for _signum, _status in ((signal.SIGINT, 130), (signal.SIGTERM, 143), (signal.SIGHUP, 129)):
        signal.signal(_signum, _interrupt(_status))
    sys.exit(main(sys.argv[1:]))
