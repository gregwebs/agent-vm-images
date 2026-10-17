#!/usr/bin/env python3
"""Scripted Docker/watchdog plumbing tests; never substitutes for native probes."""
import contextlib
import errno
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
ENTRY = ROOT / 'script/test/standard-node-consumers.sh'
BODY = '/source/script/test/fixtures/standard-node-consumers.sh'
HASH = 'a' * 64


def fake_docker(args):
    state_path = Path(os.environ['AVM_FAKE_STATE'])
    state = json.loads(state_path.read_text())
    state['commands'].append(args)
    case = os.environ.get('AVM_FAKE_CASE', 'positive')
    output = ''
    failure = False
    status = 19
    label = case
    signal_parent = False
    key = args[0]
    if key == 'context':
        output = 'native-linux-daemon\n'
    elif key == 'buildx':
        output = 'Driver: docker-container\n' if case == 'driver' else 'Driver: docker\n'
    elif key == 'info':
        output = json.dumps({'OSType': 'linux', 'Architecture': 'x86_64' if case == 'daemon-platform' else 'aarch64'})
    elif args[:2] == ['image', 'inspect']:
        output = json.dumps([{'Os': 'linux', 'Architecture': 'amd64' if case == 'image-platform' else 'arm64', 'Id': 'sha256:fake', 'Created': 'fixture', 'Config': {'Labels': {}}}])
    elif key == 'history':
        output = 'fixture history\n'
    elif args[:2] == ['volume', 'create']:
        state['volume'] = args[-1]
        output = args[-1]
    elif key == 'create':
        name = args[args.index('--name') + 1]
        state['containers'].append(name)
        state['token'] = name.removesuffix('-setup').removesuffix('-probe').removesuffix('-capacity')
        failure = case == 'create'
        output = name + '\n'
    elif key == 'start':
        if args[-1].endswith('-capacity'):
            reserve = 100 if case == 'capacity' else 2000000
            output = f'Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 4000000 1000 {reserve} 1% /\nvolume 4000000 1000 {reserve} 1% /fixtures\n'
            failure = case == 'capacity-start'
        elif '-a' in args:
            output = json.dumps({'device': 'wrong' if case == 'device-type' else 'character:1:3', 'verified': True})
            failure = case == 'device-setup'
        else:
            failure = case == 'start'
    elif key == 'wait':
        output = '1\n' if case == 'wait' else '0\n'
    elif key == 'inspect' and '--size' in args:
        output = json.dumps([{'SizeRw': 4096}])
    elif key == 'inspect':
        output = json.dumps({'remaining': {}} if case == 'attachments' else {})
    elif key == 'cp':
        failure = case == 'copy'
    elif args[:2] == ['network', 'create']:
        state['network'] = args[-1]
        output = args[-1]
    elif args[:2] == ['network', 'disconnect']:
        failure = case == 'disconnect'
    elif key == 'exec':
        if 'df' in args:
            used = 999999 if case == 'home-budget' else 4
            output = f'Filesystem 1024-blocks Used Available Capacity Mounted on\ntmpfs 2000000 {used} 1000000 1% /root\ntmpfs 2000000 4 1000000 1% /runtime-homes\n'
        elif 'du' in args:
            assert '/root' not in args and '/runtime-homes' not in args, 'private HOMEs cannot be inventoried by restricted root'
            output = ('999999' if case == 'budget' else '24') + '\t/fixtures\n4\t/invocation\n'
        elif BODY in args:
            command = args[args.index(BODY) + 1:]
            action = command[0]
            if action == 'ready':
                output = json.dumps({'ready': state['token'], 'arch': 'x86_64' if case == 'container-platform' else 'aarch64', 'launcher': HASH})
                failure = case == 'readiness'
                if case == 'signal':
                    signal_parent = True
            elif action == 'mutate':
                stage = command[1]
                output = json.dumps({'mutation': stage, 'verified': True})
                if case in ['acl-create', 'acl-readback'] and stage == 'acl-negative':
                    failure = True
                if case == 'acl-remove' and stage == 'acl-repaired':
                    failure = True
                if case == 'device-remove' and stage == 'device-repaired':
                    failure = True
            elif action == 'probe':
                _, token, stage, uid, gid, launcher = command
                output = json.dumps({'probe': 'old-token' if case == 'stale' else token, 'stage': stage,
                                     'pair': uid + ':' + gid, 'caps': 'zero', 'offline': True, 'launcher': launcher})
                if case == 'missing':
                    output = 'standard-node-consumers: primitives PASS\n'
                filler = {'late-output': 131072, 'cumulative-log': 49152}.get(case, 0)
                if filler:
                    output = 'filler ' + 'X' * filler + '\n' + output
                failure = case in ['exec', 'exec-and-cleanup']
    elif key == 'ps':
        name = args[-1].removeprefix('name=^/').removesuffix('$')
        output = name if name in state['containers'] else ''
    elif key == 'rm':
        name = args[-1]
        failure = (case == 'remove' and not name.endswith('-capacity')) or (
            case == 'exec-and-cleanup' and name.endswith('-probe'))
        if case == 'exec-and-cleanup' and name.endswith('-probe'):
            # A distinct cleanup status exposes implementations that replace the
            # primary child status with the cleanup status.
            status = 23
            label = 'cleanup'
        if not failure:
            assert name in state['containers'], 'attempted unrelated container cleanup'
            state['containers'].remove(name)
    elif args[:2] == ['volume', 'rm']:
        assert args[-1] == state['volume'], 'attempted unrelated volume cleanup'
        failure = case == 'volume-remove'
        if not failure:
            state['volume'] = None
    elif args[:2] == ['network', 'rm']:
        assert args[-1] == state['network'], 'attempted unrelated network cleanup'
        failure = case == 'network-remove'
        if not failure:
            state['network'] = None
    else:
        raise AssertionError('unhandled fake docker command: ' + repr(args))
    state_path.write_text(json.dumps(state))
    if signal_parent:
        os.kill(os.getppid(), signal.SIGTERM)
    if failure:
        print('scripted failure: ' + label, file=sys.stderr)
        return status
    print(output)
    return 0


def fake_python(args):
    if args and args[0].endswith('/host-watchdog.py'):
        assert args[1] == '1800', 'missing overall 1800s watchdog'
        if os.environ.get('AVM_FAKE_CASE') == 'watchdog-timeout':
            return 124
    os.execv(sys.executable, [sys.executable, *args])


class FullEvidence:
    """Evidence path whose writes fail like an exhausted filesystem."""

    def __init__(self):
        self.writes = 0

    def __truediv__(self, name):
        return self

    def write_bytes(self, body):
        self.writes += 1
        raise OSError(errno.ENOSPC, 'No space left on device')

    def rglob(self, pattern):
        return []


def main():
    with tempfile.TemporaryDirectory(prefix='standard-node-self-test-') as directory:
        work = Path(directory)
        tools = work / 'bin'
        tools.mkdir()
        for executable, mode in [('docker', '--fake-docker'), ('python3', '--fake-python')]:
            file = tools / executable
            file.write_text(f'#!{sys.executable}\nimport runpy,sys\nsys.argv=[{str(Path(__file__).resolve())!r},{mode!r},*sys.argv[1:]]\nrunpy.run_path(sys.argv[0],run_name="__main__")\n')
            file.chmod(0o755)
        environment = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'])
        for variable in ['NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS']:
            environment.pop(variable, None)
        spec = importlib.util.spec_from_file_location('primitive_host', ROOT / 'script/test/standard-node-consumers.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        live_invocation = module.Invocation

        def bare_invocation(**attributes):
            """Invocation stand-in that avoids evidence-directory side effects."""
            invocation = module.Invocation.__new__(module.Invocation)
            invocation.logs = Path('unused-evidence')
            invocation.containers = []
            invocation.volume = None
            invocation.network = None
            invocation.index = 0
            invocation.setup_rw = 0
            invocation.rw_kib = 0
            invocation.fixture_kib = 0
            invocation.recording_failures = []
            for name, value in attributes.items():
                setattr(invocation, name, value)
            return invocation

        def in_process(case, budget=None, on_create=None):
            """Run main() against scripted docker; budget stays a module constant."""
            state_path = work / ('in-process-' + case + '.json')
            state_path.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
            run_environment = dict(environment, AVM_FAKE_CASE=case, AVM_FAKE_STATE=str(state_path))
            created = []

            def create(*args):
                invocation = live_invocation(*args)
                if on_create is not None:
                    on_create(invocation)
                created.append(invocation)
                return invocation

            output = io.StringIO()
            saved = {sig: signal.getsignal(sig) for sig in [signal.SIGINT, signal.SIGTERM, signal.SIGHUP]}
            try:
                with patch.dict(os.environ, run_environment), patch.object(module, 'Invocation', create), \
                        contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                    if budget is None:
                        status = module.main(['base:fixture', '--consumer', 'primitives', '--platform', 'linux/arm64'])
                    else:
                        with patch.object(module, 'PRIMITIVE_SETUP_BUDGET_KIB', budget):
                            status = module.main(['base:fixture', '--consumer', 'primitives', '--platform', 'linux/arm64'])
            finally:
                for sig, handler in saved.items():
                    signal.signal(sig, handler)
            return status, output.getvalue(), json.loads(state_path.read_text()), created[0]

        cases = ['positive', 'driver', 'daemon-platform', 'image-platform', 'container-platform',
                 'capacity', 'capacity-start', 'budget', 'home-budget', 'device-setup', 'device-type', 'acl-create', 'acl-readback',
                 'acl-remove', 'device-remove', 'create', 'copy', 'start', 'readiness', 'exec',
                 'wait', 'disconnect', 'attachments', 'remove', 'volume-remove', 'network-remove',
                 'stale', 'missing', 'watchdog-timeout', 'signal', 'exec-and-cleanup']
        for case in cases:
            state_path = work / (case + '.json')
            state_path.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
            env = dict(environment, AVM_FAKE_CASE=case, AVM_FAKE_STATE=str(state_path))
            result = subprocess.run(['bash', str(ENTRY), 'base:fixture', '--consumer', 'primitives', '--platform', 'linux/arm64'],
                                    env=env, capture_output=True, text=True, timeout=40)
            child_failures = ['capacity-start', 'device-setup', 'acl-create', 'acl-readback', 'acl-remove', 'device-remove',
                              'create', 'copy', 'start', 'readiness', 'exec', 'disconnect', 'remove',
                              'volume-remove', 'network-remove', 'exec-and-cleanup']
            expected = 0 if case == 'positive' else 124 if case == 'watchdog-timeout' else 143 if case == 'signal' else 19 if case in child_failures else 1
            assert result.returncode == expected, (case, result.returncode, result.stdout, result.stderr)
            marker = 'standard-node-consumers: primitives PASS'
            assert (marker in result.stdout) == (case == 'positive'), (case, result.stdout)
            state = json.loads(state_path.read_text())
            if case == 'positive':
                assert not state['containers'] and not state['volume'] and not state['network']
                creates = [args for args in state['commands'] if args[0] == 'create']
                assert len(creates) == 4
                assert all('--name' in args for args in creates)
                assert not any(args[0] == 'run' for args in state['commands'])
                main_create = next(args for args in creates if args[args.index('--name') + 1].endswith('-probe'))
                assert '/root:rw,mode=0770,uid=0,gid=23456' in main_create
                assert '/runtime-homes:rw,mode=1777' in main_create
                assert 'MKNOD' not in main_create and 'CHOWN' not in main_create
                assert main_create[main_create.index('--cap-drop') + 1] == 'ALL'
                assert sum('readonly' in arg for arg in main_create) == 2
                probes = [args for args in state['commands'] if 'probe' in args and '--bounding-set=-all' in args]
                assert len(probes) == 28
                assert all('--no-new-privs' in args and '--clear-groups' in args for args in probes)
            if case not in ['remove', 'volume-remove', 'network-remove', 'exec-and-cleanup']:
                assert not state['containers'] and not state['volume'] and not state['network'], (case, state)
            if case == 'exec-and-cleanup':
                # A real probe fails first; cleanup then fails with a different
                # status. Replacing the primary status with 23 must fail here.
                probes = [args for args in state['commands'] if args[0] == 'exec' and 'probe' in args]
                assert len(probes) == 1, (case, len(probes))
                assert state['containers'] and state['containers'][0].endswith('-probe'), state
                assert state['volume'] is None and state['network'] is None, state
                assert 'scripted failure: exec-and-cleanup' in result.stderr
                assert 'scripted failure: cleanup' in result.stderr and 'owned cleanup failed' in result.stderr
                assert 'status 19' in result.stderr and 'status 23' in result.stderr, result.stderr
            print('standard-node-consumers self-test: ' + case + ' PASS')
        for args in [[], ['base', '--bad'], ['base', '--consumer', 'primitives', '--extra'],
                     ['base', '--consumer', 'primitives', '--platform', 'linux/riscv64']]:
            result = subprocess.run(['bash', str(ENTRY), *args], env=environment, capture_output=True)
            assert result.returncode == 2
        for consumer in ['dsh', 'pi', 'copilot']:
            result = subprocess.run(['bash', str(ENTRY), 'base', '--consumer', consumer], env=environment, capture_output=True)
            assert result.returncode == 2 and b'consumer not available in this slice' in result.stderr
        sentinel = work / 'preloaded'
        preload = work / 'preload.cjs'
        preload.write_text(f'require("fs").writeFileSync({str(sentinel)!r}, "unsafe")')
        for variable, value in [('NODE_OPTIONS', '--require=' + str(preload)), ('NODE_PATH', str(work)), ('BUN_OPTIONS', '--preload=' + str(preload))]:
            result = subprocess.run(['bash', str(ENTRY), 'base', '--consumer', 'primitives'],
                                    env=dict(environment, **{variable: value}), capture_output=True)
            assert result.returncode == 1 and not sentinel.exists()
            assert b'runtime overrides' in result.stderr
        # S1: an unwritable evidence filesystem must not abort owned cleanup or
        # replace a real child status, yet must still fail when nothing else did.
        invocation = bare_invocation(logs=FullEvidence(), containers=['owned-container'],
                                     network='owned-network', volume='owned-volume')
        commands = []

        def cleanup_run(args, **kwargs):
            commands.append(list(args))
            found = 'owned-container\n' if args[1] == 'ps' else ''
            return subprocess.CompletedProcess(args, 0, found, ''), False

        with patch.object(invocation, 'run', cleanup_run):
            try:
                invocation.cleanup()
            except module.Failure as error:
                assert error.status == module.RECORDING_STATUS, error.status
                assert 'evidence' in str(error)
            else:
                raise AssertionError('cleanup swallowed an evidence-recording failure')
        assert [args[1:3] for args in commands] == [['ps', '-aq'], ['rm', '-f'], ['network', 'rm'], ['volume', 'rm']], commands
        assert invocation.containers == [] and len(invocation.recording_failures) == 4

        invocation = bare_invocation(logs=FullEvidence())
        with patch.object(invocation, 'run', lambda args: (subprocess.CompletedProcess(args, 19, '', 'scripted child failure\n'), False)), \
                contextlib.redirect_stderr(io.StringIO()) as gap:
            try:
                invocation.command('docker', 'exec', 'probe')
            except module.Failure as error:
                assert error.status == 19 and 'status 19' in str(error), error.status
            else:
                raise AssertionError('child failure was swallowed')
        assert 'evidence' in gap.getvalue()

        invocation = bare_invocation(logs=FullEvidence())
        with patch.object(invocation, 'run', lambda args: (subprocess.CompletedProcess(args, 0, 'ok\n', ''), False)):
            try:
                invocation.command('docker', 'ps')
            except module.Failure as error:
                assert error.status == module.RECORDING_STATUS, error.status
            else:
                raise AssertionError('unrecorded success was accepted')

        # The budget preflight inventories the same evidence filesystem, so its
        # OSError must also become a recording failure, never escape cleanup.
        def unreadable_evidence():
            raise OSError(errno.EIO, 'Input/output error')

        inventory_commands = []

        def inventory_run(args, **kwargs):
            inventory_commands.append(list(args))
            found = 'owned-container\n' if args[1] == 'ps' else ''
            return subprocess.CompletedProcess(args, 0, found, ''), False

        invocation = bare_invocation(logs=FullEvidence(), containers=['owned-container'],
                                     network='owned-network', volume='owned-volume')
        with patch.object(invocation, 'run', inventory_run), \
                patch.object(invocation, 'evidence_allowance_bytes', unreadable_evidence):
            try:
                invocation.cleanup()
            except module.Failure as error:
                assert error.status == module.RECORDING_STATUS, error.status
                assert 'evidence' in str(error)
            else:
                raise AssertionError('budget-inventory OSError escaped cleanup')
        assert [args[1:3] for args in inventory_commands] == \
            [['ps', '-aq'], ['rm', '-f'], ['network', 'rm'], ['volume', 'rm']], inventory_commands
        assert invocation.containers == []

        invocation = bare_invocation(logs=FullEvidence())
        with patch.object(invocation, 'run', lambda args: (subprocess.CompletedProcess(args, 19, '', 'scripted child failure\n'), False)), \
                patch.object(invocation, 'evidence_allowance_bytes', unreadable_evidence), \
                contextlib.redirect_stderr(io.StringIO()) as gap:
            try:
                invocation.command('docker', 'exec', 'probe')
            except module.Failure as error:
                assert error.status == 19 and 'status 19' in str(error), error.status
            else:
                raise AssertionError('child failure was swallowed by budget inventory failure')
        assert 'evidence' in gap.getvalue()

        invocation = bare_invocation(logs=FullEvidence())
        with patch.object(invocation, 'run', lambda args: (subprocess.CompletedProcess(args, 0, 'ok\n', ''), False)), \
                patch.object(invocation, 'evidence_allowance_bytes', unreadable_evidence):
            try:
                invocation.command('docker', 'ps')
            except module.Failure as error:
                assert error.status == module.RECORDING_STATUS, error.status
            else:
                raise AssertionError('unrecorded success was accepted after inventory failure')

        def stub_main(child_status):
            invocation = bare_invocation(logs=FullEvidence(), containers=['owned-container'],
                                         network='owned-network', volume='owned-volume')

            def execute():
                if child_status:
                    raise module.Failure('primary child failure', child_status)

            invocation.execute = execute
            output = io.StringIO()
            with patch.object(module, 'Invocation', lambda *args: invocation), \
                    patch.object(invocation, 'run', cleanup_run), \
                    contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                status = module.main(['base:fixture', '--consumer', 'primitives', '--platform', 'linux/arm64'])
            return status, output.getvalue(), invocation

        status, output, invocation = stub_main(19)
        assert status == 19, (status, output)
        assert 'primary child failure' in output and 'evidence' in output
        assert invocation.containers == []
        status, output, invocation = stub_main(None)
        assert status == module.RECORDING_STATUS, (status, output)
        assert 'standard-node-consumers: primitives PASS' not in output

        # S2: capture is bounded while reading and the shared budget is rechecked
        # after every command, including late probes, before PASS is possible.
        result, overrun = bare_invocation().run([sys.executable, '-c',
            'import sys; sys.stdout.write("o" * 1000000); sys.stderr.write("e" * 1000000)'])
        assert overrun and result.returncode < 0, result.returncode
        assert len(result.stdout) + len(result.stderr) <= module.COMMAND_OUTPUT_LIMIT_KIB * 1024
        result, overrun = bare_invocation().run([sys.executable, '-c',
            'import sys; print("out"); print("err", file=sys.stderr)'])
        assert not overrun and result.stdout == 'out\n' and result.stderr == 'err\n'

        # A descendant holding the output pipes must not extend a command past
        # its deadline; a prompt descendant still drains and succeeds.
        held = ('import subprocess, sys; subprocess.Popen([sys.executable, "-c", '
                '"import time; time.sleep(10)"]); print("leader done")')
        prompt = ('import subprocess, sys; subprocess.Popen([sys.executable, "-c", '
                  '"import time; time.sleep(0.1)"]); print("leader done")')
        with patch.object(module, 'COMMAND_TIMEOUT_SECONDS', 1.0):
            started = time.monotonic()
            try:
                bare_invocation().run([sys.executable, '-c', held])
                raise AssertionError('held output pipe was accepted')
            except module.Failure as error:
                assert error.status == 124 and 'drain' in str(error), (error.status, str(error))
            assert time.monotonic() - started < 2.5, 'drain was not bounded by the command deadline'
        result, overrun = bare_invocation().run([sys.executable, '-c', prompt])
        assert not overrun and result.returncode == 0 and result.stdout.strip() == 'leader done'

        # Reader failures must fail the command instead of silently truncating
        # evidence into a success.
        class BrokenPipe:
            def read1(self, size):
                raise OSError(5, 'Input/output error')

        capture = module.Capture(None, 1024)
        capture._drain('stdout', BrokenPipe())
        assert isinstance(capture.error, OSError)

        class ReaderFailure(module.Capture):
            def _drain(self, name, pipe):
                self.error = OSError(5, 'Input/output error')

        with patch.object(module, 'Capture', ReaderFailure):
            try:
                bare_invocation().run([sys.executable, '-c', 'print("out")'])
                raise AssertionError('reader failure was swallowed')
            except module.Failure as error:
                assert error.status == 1 and 'capture failed' in str(error), (error.status, str(error))

        status, output, state, invocation = in_process('late-output')
        assert status == 1 and 'capture bound' in output, (status, output)
        assert 'standard-node-consumers: primitives PASS' not in output
        probes = [args for args in state['commands'] if args[0] == 'exec' and 'probe' in args]
        assert len(probes) == 1, len(probes)
        assert not state['containers'] and not state['volume'] and not state['network'], state
        largest = max(file.stat().st_size for file in invocation.logs.rglob('*') if file.is_file())
        assert largest <= module.COMMAND_OUTPUT_LIMIT_KIB * 1024 + 1024, largest

        status, output, state, _ = in_process('cumulative-log', budget=1024)
        assert status == 1 and 'setup budget overrun' in output, (status, output)
        assert 'standard-node-consumers: primitives PASS' not in output
        probes = [args for args in state['commands'] if args[0] == 'exec' and 'probe' in args]
        assert 1 <= len(probes) < 28, len(probes)
        assert not state['containers'] and not state['volume'] and not state['network'], state

        # Recording itself must respect the remaining allowance: cap a log body
        # that does not fit and keep the retained allocation under the budget.
        with tempfile.TemporaryDirectory(prefix='evidence-budget-') as evidence:
            invocation = bare_invocation(logs=Path(evidence))
            invocation.rw_kib = module.PRIMITIVE_SETUP_BUDGET_KIB - 8
            failure = invocation.record(['docker', 'probe'],
                                        subprocess.CompletedProcess(['docker', 'probe'], 0, 'X' * 20000, ''), False)
            assert failure is not None and failure.status == 1 and 'overrun' in str(failure)
            assert invocation.evidence_allocated_bytes() <= 8 * 1024
            assert module.EVIDENCE_TRUNCATED in next(Path(evidence).rglob('*.log')).read_bytes()

        # Cleanup with an exhausted allowance still removes every owned resource,
        # retains nothing further, and fails instead of printing PASS.
        with tempfile.TemporaryDirectory(prefix='evidence-exhausted-') as evidence:
            invocation = bare_invocation(logs=Path(evidence), containers=['owned-container'],
                                         network='owned-network', volume='owned-volume')
            invocation.rw_kib = module.PRIMITIVE_SETUP_BUDGET_KIB
            commands = []

            def exhausted_run(args, **kwargs):
                commands.append(list(args))
                found = 'owned-container\n' if args[1] == 'ps' else ''
                return subprocess.CompletedProcess(args, 0, found, ''), False

            with patch.object(invocation, 'run', exhausted_run):
                try:
                    invocation.cleanup()
                except module.Failure as error:
                    assert 'overrun' in str(error) and 'owned cleanup failed' in str(error)
                else:
                    raise AssertionError('exhausted allowance accepted cleanup')
            assert [args[1:3] for args in commands] == [['ps', '-aq'], ['rm', '-f'], ['network', 'rm'], ['volume', 'rm']], commands
            assert invocation.containers == []
            assert not list(Path(evidence).rglob('*.log'))
            assert invocation.retained_kib() <= module.PRIMITIVE_SETUP_BUDGET_KIB

        # End to end: budget set to the pre-cleanup retention must not be
        # overshot by cleanup logs, must not print PASS, and must still remove
        # owned resources; a small headroom repairs the positive.
        def watch_cleanup(invocation):
            original = invocation.cleanup

            def cleanup():
                invocation.pre_cleanup_kib = invocation.retained_kib()
                invocation.pre_cleanup_logs = len(list(invocation.logs.rglob('*.log')))
                return original()

            invocation.cleanup = cleanup

        status, output, state, relaxed = in_process('positive', budget=1024, on_create=watch_cleanup)
        assert status == 0 and 'standard-node-consumers: primitives PASS' in output, (status, output)
        assert not state['containers'] and not state['volume'] and not state['network'], state
        assert relaxed.retained_kib() <= 1024
        tight_budget = relaxed.pre_cleanup_kib + 1
        status, output, state, tight = in_process('positive', budget=tight_budget, on_create=watch_cleanup)
        assert status != 0 and 'standard-node-consumers: primitives PASS' not in output, (status, output)
        assert 'owned cleanup failed' in output and 'setup budget overrun' in output, output
        assert not state['containers'] and not state['volume'] and not state['network'], state
        assert tight.retained_kib() <= tight_budget, (tight.retained_kib(), tight_budget)
        assert len(list(tight.logs.rglob('*.log'))) == tight.pre_cleanup_logs

        # A cumulative overrun only visible in the final state must still block
        # PASS, even when cleanup itself succeeded.
        invocation = bare_invocation(logs=FullEvidence(), rw_kib=module.PRIMITIVE_SETUP_BUDGET_KIB + 1)
        invocation.execute = lambda: None
        output = io.StringIO()
        with patch.object(module, 'Invocation', lambda *args: invocation), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
            status = module.main(['base:fixture', '--consumer', 'primitives', '--platform', 'linux/arm64'])
        assert status == 1 and 'standard-node-consumers: primitives PASS' not in output.getvalue(), (status, output.getvalue())
        assert 'setup budget overrun' in output.getvalue()

        # A host-command timeout must be a hard failure, not missing evidence success.
        with patch.object(module, 'COMMAND_TIMEOUT_SECONDS', 0.2):
            try:
                bare_invocation().run(['sleep', '30'])
                raise AssertionError('timeout swallowed')
            except module.Failure as error:
                assert error.status == 124 and 'timeout' in str(error)
    print('standard-node-consumers: plumbing self-test PASS (no native acceptance claim)')


if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-docker':
        sys.exit(fake_docker(sys.argv[2:]))
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-python':
        sys.exit(fake_python(sys.argv[2:]))
    main()
