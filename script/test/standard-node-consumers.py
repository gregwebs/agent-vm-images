#!/usr/bin/env python3
"""Bounded host orchestration for the primitive-only native probe."""
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
# Evidence lives under the build root. AGENT_VM_EVIDENCE_ROOT exists only so the
# fake-Docker self-tests can redirect it: a directory under target/ci-logs is a
# claim about a real run, and a fake run must not be able to make that claim.
EVIDENCE_ROOT = Path(os.environ.get('AGENT_VM_EVIDENCE_ROOT') or ROOT / 'target/ci-logs')
PRIMITIVE_MIN_FREE_KIB = 524288
PRIMITIVE_SETUP_BUDGET_KIB = 262144
# Real evidence lines are a few KiB. This ceiling is shared by both pipes of one
# command and enforced while reading, so a late probe cannot buffer unbounded
# output in memory or on the evidence filesystem.
COMMAND_OUTPUT_LIMIT_KIB = 64
COMMAND_READ_CHUNK_KIB = 4
COMMAND_TIMEOUT_SECONDS = 190
# Reserve one filesystem block so block rounding on a new log file cannot push
# retained allocation past the shared budget.
EVIDENCE_BLOCK_BYTES = 4096
EVIDENCE_TRUNCATED = b'\n[evidence truncated: shared allowance exhausted]\n'
# sysexits EX_IOERR: evidence could not be retained although no command failed.
RECORDING_STATUS = 74
PAIRS = [(12345, 23456), (0, 23456), (12345, 0), (12345, 12345)]
STAGES = ['positive', 'private-negative', 'private-repaired', 'acl-negative',
          'acl-repaired', 'device-negative', 'device-repaired']
BODY = '/source/script/test/fixtures/standard-node-consumers.sh'


class Failure(Exception):
    def __init__(self, message, status=1):
        super().__init__(message)
        self.status = status


class Capture:
    """Bounded concurrent capture of one child's stdout and stderr.

    Both pipes share a hard byte ceiling while they are read; overrun kills the
    child instead of letting the parent buffer the stream and measure it after
    the fact. Draining continues after the kill so the child cannot block on a
    full pipe.
    """

    def __init__(self, process, limit):
        self.process = process
        self.limit = limit
        self.size = 0
        self.overrun = False
        self.error = None
        self.lock = threading.Lock()
        self.buffers = {'stdout': [], 'stderr': []}

    def start(self):
        readers = [threading.Thread(target=self._drain, args=(name, getattr(self.process, name)), daemon=True)
                   for name in self.buffers]
        for reader in readers:
            reader.start()
        return readers

    def _drain(self, name, pipe):
        read = getattr(pipe, 'read1', None) or pipe.read
        try:
            while True:
                chunk = read(COMMAND_READ_CHUNK_KIB * 1024)
                if not chunk:
                    return
                with self.lock:
                    if self.overrun:
                        continue
                    room = self.limit - self.size
                    kept = chunk[:room]
                    self.buffers[name].append(kept)
                    self.size += len(kept)
                    if len(kept) < len(chunk):
                        self.overrun = True
                        self.process.kill()
        except Exception as error:
            # A failed reader must fail the command; it may not silently truncate
            # evidence into a successful result.
            self.error = error

    def text(self):
        return tuple(b''.join(self.buffers[name]).decode('utf-8', 'replace') for name in ['stdout', 'stderr'])


def arch(value):
    return {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64', 'amd64': 'amd64'}.get(value, value)


def arguments(args):
    if not args or args[0].startswith('-'):
        raise ValueError('usage: standard-node-consumers.sh BASE_IMAGE --consumer primitives [--platform linux/ARCH]')
    image = args[0]
    consumer = None
    platform = None
    rest = args[1:]
    while rest:
        flag, *rest = rest
        if flag not in ['--consumer', '--platform'] or not rest:
            raise ValueError('unknown/missing option: ' + flag)
        value, *rest = rest
        if flag == '--consumer' and consumer is None:
            consumer = value
        elif flag == '--platform' and platform is None:
            platform = value
        else:
            raise ValueError('duplicate option: ' + flag)
    if consumer != 'primitives':
        raise ValueError('consumer not available in this slice' if consumer in ['dsh', 'pi', 'copilot'] else 'expected --consumer primitives')
    if platform not in [None, 'linux/amd64', 'linux/arm64']:
        raise ValueError('expected native linux/amd64 or linux/arm64')
    return image, platform


def contract_context(platform):
    """Resolve through the shared adapter; it owns platform selection and reuse."""
    adapter = ROOT / 'script/build/recipe-contract.sh'
    result = subprocess.run(['bash', str(adapter), '--platform', platform],
                            capture_output=True, text=True)
    if result.returncode:
        raise Failure(f'cannot build the {platform} recipe-contract helper: '
                      + (result.stderr.strip() or result.stdout.strip() or 'no diagnostic'))
    context = Path(result.stdout.strip())
    if not (context / 'out/recipe-contract').is_file():
        raise Failure(f'the {platform} recipe-contract build published no binary under {context}')
    return context


class Invocation:
    def __init__(self, image, requested):
        self.image = image
        self.requested = requested
        self.token = 'pnpm-primitives-' + uuid.uuid4().hex
        self.logs = EVIDENCE_ROOT / self.token
        self.logs.mkdir(parents=True)
        self.scratch = self.logs / 'invocation'
        self.scratch.mkdir()
        (self.scratch / 'token').write_text(self.token + '\n')
        self.containers = []
        self.contract = None
        self.volume = None
        self.network = None
        self.index = 0
        self.setup_rw = 0
        self.rw_kib = 0
        self.fixture_kib = 0
        self.recording_failures = []
        print('standard-node-consumers: evidence ' + str(self.logs), flush=True)

    def command(self, *args, cleanup=False):
        """Run one bounded command and account for it.

        `cleanup` marks best-effort owned-resource work: neither a missing
        evidence file nor an exhausted budget may abort removal, so recording
        gaps are deferred to `cleanup` and the budget is only enforced for
        commands whose success is required for PASS.
        """
        self.index += 1
        result, overrun = self.run(list(args))
        recording = self.record(args, result, overrun)
        failure = self.command_failure(args, result, overrun)
        if failure is not None:
            if recording is not None:
                print(recording, file=sys.stderr)
            raise failure
        if recording is not None:
            if not cleanup:
                raise recording
            self.recording_failures.append(recording)
        if not cleanup:
            if result.stderr:
                print(result.stderr, end='', file=sys.stderr)
            self.check_budget()
        return result.stdout

    def run(self, args):
        """Run one child under one deadline covering both wait and output drain."""
        deadline = time.monotonic() + COMMAND_TIMEOUT_SECONDS
        process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        capture = Capture(process, COMMAND_OUTPUT_LIMIT_KIB * 1024)
        readers = capture.start()
        try:
            process.wait(timeout=max(deadline - time.monotonic(), 0))
        except subprocess.TimeoutExpired as error:
            process.kill()
            process.wait()
            raise Failure('command timeout: ' + repr(args), 124) from error
        if not self.drain(readers, deadline):
            # A descendant still holds the pipes. Waiting for it would let an
            # exiting leader outlive the command deadline; report the timeout.
            raise Failure('command timeout: output drain exceeded: ' + repr(args), 124)
        stdout, stderr = capture.text()
        if capture.error is not None:
            raise Failure(f'{args}: output capture failed: {capture.error!r}')
        return subprocess.CompletedProcess(args, process.returncode, stdout, stderr), capture.overrun

    def drain(self, readers, deadline):
        """Join readers by an absolute deadline; False means a pipe is still held."""
        for reader in readers:
            reader.join(max(deadline - time.monotonic(), 0))
            if reader.is_alive():
                return False
        return True

    def command_failure(self, args, result, overrun):
        """The command's own outcome outranks any evidence-recording gap."""
        if overrun:
            return Failure(f'{args}: output exceeded the {COMMAND_OUTPUT_LIMIT_KIB} KiB capture bound')
        if result.returncode:
            return Failure(f'{args}: status {result.returncode}: {result.stderr.strip()}',
                           result.returncode if result.returncode > 0 else 128 - result.returncode)
        return None

    def record(self, args, result, overrun):
        """Persist one command log within the shared retained allowance.

        Recording is a separate obligation from the child result: an ENOSPC or
        an exhausted allowance becomes a Failure instead of escaping `cleanup` or
        replacing a real probe failure. The allowance inventory, encoding and
        write all live inside the protected boundary because the budget check
        reads the same evidence filesystem that can fail. The body is capped to
        the remaining allowance, so writing evidence can never push retained
        allocation past PRIMITIVE_SETUP_BUDGET_KIB between budget checks -
        including the cleanup logs written after the last check.
        """
        try:
            body = (f'command={args!r}\nstatus={result.returncode}\n'
                    + ('output=overrun\n' if overrun else '') + result.stdout + result.stderr).encode('utf-8')
            allowance = self.evidence_allowance_bytes()
            truncated = len(body) > allowance
            if truncated:
                body = (body[:allowance - len(EVIDENCE_TRUNCATED)] + EVIDENCE_TRUNCATED
                        if allowance >= len(EVIDENCE_TRUNCATED) else b'')
            if body:
                (self.logs / f'{self.index:03d}.log').write_bytes(body)
        except OSError as error:
            return Failure(f'command evidence not retained: {error}', RECORDING_STATUS)
        if truncated:
            return Failure('setup budget overrun: evidence allowance exhausted while recording', 1)
        return None

    def evidence_allocated_bytes(self):
        return sum(max(file.stat().st_size, file.stat().st_blocks * 512)
                   for file in self.logs.rglob('*') if file.is_file())

    def evidence_kib(self):
        """Host allocation retained by this invocation's evidence directory."""
        return (self.evidence_allocated_bytes() + 1023) // 1024

    def evidence_allowance_bytes(self):
        """Bytes the next evidence write may retain without exceeding the budget.

        Container allocation is accounted as ceiling KiB, so this is
        conservative; one filesystem block is held back for block rounding.
        """
        container = (self.rw_kib + self.fixture_kib) * 1024
        remaining = PRIMITIVE_SETUP_BUDGET_KIB * 1024 - container - self.evidence_allocated_bytes()
        return max(remaining - EVIDENCE_BLOCK_BYTES, 0)

    def retained_kib(self):
        """Container setup/scratch plus fixture/tmpfs plus host evidence."""
        return self.rw_kib + self.fixture_kib + self.evidence_kib()

    def check_budget(self):
        """Fail before PASS once cumulative retained allocation exceeds the bound.

        Every non-cleanup command shares the same remaining budget, so late log
        or fixture growth cannot obtain a fresh allowance after setup.
        """
        retained = self.retained_kib()
        if retained > PRIMITIVE_SETUP_BUDGET_KIB:
            raise Failure(f'setup budget overrun: {retained} KiB > {PRIMITIVE_SETUP_BUDGET_KIB}')
        return retained

    def measure(self, main):
        """Refresh container-side allocation and recheck the shared budget.

        Fixtures/tmpfs are only cheap enough to re-read per stage (two commands);
        host evidence is re-measured after every command by `check_budget`.
        """
        du = self.docker('exec', main, 'du', '-sk', '/fixtures', '/invocation')
        fixtures = sum(int(line.split()[0]) for line in du.splitlines() if line.strip())
        # Filesystem counters include private numeric-user HOMEs without granting
        # the setup process DAC_OVERRIDE merely to enumerate their 0700 contents.
        homes = self.docker('exec', main, 'df', '-Pk', '/root', '/runtime-homes')
        rows = [line.split() for line in homes.splitlines()[1:] if line.strip()]
        if len(rows) != 2 or any(len(row) < 6 for row in rows):
            raise Failure('setup budget: expected both HOME tmpfs allocation rows')
        self.fixture_kib = fixtures + sum(int(row[2]) for row in rows)
        return self.check_budget()

    def docker(self, *args, **kwargs):
        return self.command('docker', *args, **kwargs)

    def mounts(self):
        return ['--mount', f'type=bind,src={ROOT},dst=/source,readonly',
                '--mount', f'type=bind,src={self.contract / "out"},dst=/contract,readonly',
                '--mount', f'type=volume,src={self.volume},dst=/fixtures']

    def select_contract(self):
        """Build (or reuse) the helper for the validated platform and record it.

        A suite that rebuilds the shared output itself would race every other
        suite, so reuse the exported context when it matches; otherwise build once
        per invocation.
        """
        self.contract = contract_context(self.platform)
        binary = self.contract / 'out/recipe-contract'
        version = subprocess.run(['go', 'version', str(binary)], capture_output=True, text=True)
        if version.returncode:
            raise Failure('cannot read the helper compiler: ' + version.stderr.strip())
        record = {'context': str(self.contract),
                  'sha256': hashlib.sha256(binary.read_bytes()).hexdigest(),
                  'compiler': version.stdout.strip()}
        (self.logs / 'helper.json').write_text(json.dumps(record, indent=2) + '\n')
        print('contract: ' + record['compiler'] + ' ' + record['sha256'], flush=True)

    def capacity(self, minimum):
        name = self.create(self.token + '-capacity', '--platform', self.platform,
                           '--network', 'none', '--cap-drop', 'ALL',
                           '--mount', f'type=volume,src={self.volume},dst=/fixtures',
                           self.image, 'df', '-Pk', '/', '/fixtures')
        output = self.docker('start', '-a', name)
        if self.docker('wait', name).strip() != '0':
            raise Failure('capacity container failed')
        info = json.loads(self.docker('inspect', '--size', name))[0]
        self.setup_rw += int(info['SizeRw'])
        self.remove(name)
        rows = [line.split() for line in output.splitlines()[1:] if line.strip()]
        if len(rows) != 2:
            raise Failure('capacity: expected writable-layer and fixture-volume df rows')
        for row in rows:
            if len(row) < 6 or int(row[3]) < minimum:
                raise Failure(f'capacity: need {minimum} KiB available on daemon writable/fixture filesystems: {row}')
        print(f'capacity: daemon writable-layer and owned fixture volume >= {minimum} KiB', flush=True)

    def create(self, name, *args):
        # Register the exact invocation name before create: even a failed client
        # can leave a daemon-side object, and cleanup must never touch other names.
        self.containers.append(name)
        self.docker('create', '--name', name, *args)
        return name

    def remove(self, name):
        self.docker('rm', '-f', name, cleanup=True)
        self.containers.remove(name)

    def json_line(self, output, key):
        records = []
        for line in output.splitlines():
            try:
                value = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(value, dict) and key in value:
                records.append(value)
        if len(records) != 1:
            raise Failure('missing/ambiguous evidence: ' + key)
        return records[0]

    def execute(self):
        source = self.command('git', '-C', str(ROOT), 'rev-parse', 'HEAD').strip()
        inputs = sorted([ROOT / 'images/Dockerfile', *(ROOT / 'images/pnpm').rglob('*')])
        (self.logs / 'source.json').write_text(json.dumps({'source': source, 'inputs': {
            str(file.relative_to(ROOT)): hashlib.sha256(file.read_bytes()).hexdigest()
            for file in inputs if file.is_file() and '__pycache__' not in file.parts}}, indent=2))
        context = self.docker('context', 'show').strip()
        driver = self.docker('buildx', 'inspect', context)
        # Exact tokens prevent docker-container from passing a prefix match.
        if not any(line.split() == ['Driver:', 'docker'] for line in driver.splitlines()):
            raise Failure('daemon-backed docker driver required')
        daemon = json.loads(self.docker('info', '--format', '{{json .}}'))
        image = json.loads(self.docker('image', 'inspect', self.image))[0]
        self.docker('history', '--no-trunc', self.image)
        platform = self.requested or 'linux/' + arch(daemon['Architecture'])
        if daemon['OSType'] != 'linux' or 'linux/' + arch(daemon['Architecture']) != platform:
            raise Failure('daemon platform mismatch/emulation')
        if image['Os'] != 'linux' or 'linux/' + arch(image['Architecture']) != platform:
            raise Failure('base-image platform mismatch/emulation')
        print(f'platform: daemon={platform} image={image["Id"]}; client OS is not a native-daemon test', flush=True)
        self.platform = platform
        self.select_contract()
        self.volume = self.token + '-fixtures'
        self.docker('volume', 'create', self.volume)
        self.capacity(PRIMITIVE_MIN_FREE_KIB + PRIMITIVE_SETUP_BUDGET_KIB)
        setup = self.create(self.token + '-setup', '--platform', platform, '--network', 'none',
                            '--cap-drop', 'ALL', '--cap-add', 'MKNOD', *self.mounts(), self.image,
                            'sh', BODY, 'setup-device')
        setup_output = self.docker('start', '-a', setup)
        if self.json_line(setup_output, 'device') != {'device': 'character:1:3', 'verified': True}:
            raise Failure('device setup/type evidence invalid')
        if self.docker('wait', setup).strip() != '0':
            raise Failure('device setup container failed')
        info = json.loads(self.docker('inspect', '--size', setup))[0]
        self.setup_rw += int(info['SizeRw'])
        self.remove(setup)
        self.network = self.token + '-network'
        self.docker('network', 'create', '--internal', self.network)
        main = self.create(self.token + '-probe', '--platform', platform, '--network', self.network,
                           '--cap-drop', 'ALL', '--cap-add', 'SETUID', '--cap-add', 'SETGID', '--cap-add', 'SETPCAP',
                           '--tmpfs', '/root:rw,mode=0770,uid=0,gid=23456',
                           '--tmpfs', '/runtime-homes:rw,mode=1777', *self.mounts(), self.image,
                           'sleep', 'infinity')
        self.docker('cp', str(self.scratch) + '/.', main + ':/invocation')
        self.docker('start', main)
        ready = self.json_line(self.docker('exec', main, 'timeout', '--kill-after=10', '180', 'sh', BODY, 'ready', self.token), 'ready')
        if ready.get('ready') != self.token or 'linux/' + arch(ready.get('arch')) != platform:
            raise Failure('running container platform/readiness mismatch')
        launcher = ready.get('launcher', '')
        if len(launcher) != 64 or any(ch not in '0123456789abcdef' for ch in launcher):
            raise Failure('missing launcher evidence')
        self.capacity(PRIMITIVE_MIN_FREE_KIB)
        main_info = json.loads(self.docker('inspect', '--size', main))[0]
        self.rw_kib = (self.setup_rw + int(main_info['SizeRw']) + 1023) // 1024
        retained = self.measure(main)
        print(f'setup budget: {retained}/{PRIMITIVE_SETUP_BUDGET_KIB} KiB', flush=True)
        self.docker('network', 'disconnect', self.network, main)
        attachments = json.loads(self.docker('inspect', '--format', '{{json .NetworkSettings.Networks}}', main))
        if attachments:
            raise Failure('network attachments remain before offline probes')
        for stage in STAGES:
            mutation_output = self.docker('exec', main, 'timeout', '--kill-after=10', '180', 'sh', BODY, 'mutate', stage)
            if self.json_line(mutation_output, 'mutation') != {'mutation': stage, 'verified': True}:
                raise Failure('mutation/readback/removal evidence invalid: ' + stage)
            self.measure(main)
            for uid, gid in PAIRS:
                home = '/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}'
                output = self.docker('exec', '-e', 'HOME=' + home, main, 'timeout', '--kill-after=10', '180',
                                     'setpriv', '--bounding-set=-all', '--inh-caps=-all', '--ambient-caps=-all',
                                     '--reuid', str(uid), '--regid', str(gid), '--clear-groups', '--no-new-privs',
                                     'sh', BODY, 'probe', self.token, stage, str(uid), str(gid), launcher)
                expected = {'probe': self.token, 'stage': stage, 'pair': f'{uid}:{gid}', 'caps': 'zero', 'offline': True, 'launcher': launcher}
                if self.json_line(output, 'probe') != expected:
                    raise Failure('stale/invalid probe evidence')
                print(output, end='', flush=True)
        print('provenance: retained source/image/history and authenticated pnpm bytes; no whole-base revision attestation. Local smoke is diagnostic; clean proposed-SHA native CI remains required.', flush=True)

    def cleanup(self):
        """Remove only invocation-owned resources, even when evidence is unwritable.

        Runs on every exit path. Command failures and recording gaps are
        aggregated so owned removal continues and the first real status survives;
        with no primary child failure a recording gap is still a failure.
        """
        errors = []
        for name in list(reversed(self.containers)):
            try:
                # Failed create may leave no object; inspect determines whether
                # removal is necessary without ever discovering unrelated names.
                found = self.docker('ps', '-aq', '--filter', 'name=^/' + name + '$', cleanup=True).strip()
                if found:
                    self.remove(name)
                else:
                    self.containers.remove(name)
            except Failure as error:
                errors.append(error)
        for kind, owned in [('network', self.network), ('volume', self.volume)]:
            if owned:
                try:
                    self.docker(kind, 'rm', owned, cleanup=True)
                except Failure as error:
                    errors.append(error)
        if errors or self.recording_failures:
            problems = errors + self.recording_failures
            raise Failure('owned cleanup failed: ' + '; '.join(str(error) for error in problems), problems[0].status)


def main(args):
    if any(args[index:index + 2] == ['--consumer', 'dsh'] for index in range(len(args))):
        import runpy
        return runpy.run_path(str(Path(__file__).with_name('standard-node-dsh.py')))['main'](args)
    try:
        image, requested = arguments(args)
    except ValueError as error:
        print(error, file=sys.stderr)
        return 2
    invocation = Invocation(image, requested)
    status = 0

    def interrupted(number, _frame):
        raise Failure('signal ' + str(number), 128 + number)

    for sig in [signal.SIGINT, signal.SIGTERM, signal.SIGHUP]:
        signal.signal(sig, interrupted)
    try:
        invocation.execute()
    except (Failure, OSError, ValueError, KeyError, IndexError) as error:
        print('standard-node-consumers: ' + str(error), file=sys.stderr)
        status = error.status if isinstance(error, Failure) else 1
    finally:
        try:
            invocation.cleanup()
        except Failure as error:
            print(str(error), file=sys.stderr)
            if not status:
                status = error.status
    if not status:
        # The final state, including anything cleanup retained, must still fit
        # the shared budget before any success marker is printed.
        try:
            invocation.check_budget()
        except Failure as error:
            print('standard-node-consumers: ' + str(error), file=sys.stderr)
            status = error.status
    if not status:
        print('standard-node-consumers: primitives PASS')
    return status


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
