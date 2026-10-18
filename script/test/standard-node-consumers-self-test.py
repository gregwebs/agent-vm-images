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
DSH_BODY = '/source/script/test/fixtures/standard-node-dsh.py'
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
        elif DSH_BODY in args:
            command = args[args.index(DSH_BODY) + 1:]
            action = command[0]
            if action == 'initialize':
                state['binding'] = command[1:]
            if action == 'cordis-negative':
                # Plumbing only: the real negative needs a native container.
                assert command[1] == '/invocation/cordis-negative', command
                output = json.dumps({'cordisNegative': 'exact-peer rejection',
                                     'diagnostic': 'peer @deepseek-ai/cordis wanted 4.0.2, got 4.0.5'})
                failure = case == 'cordis-negative'
            if action == 'probe':
                _, token, source, selection, uid, gid = command
                assert state['binding'] == [token, source, selection]
                report = {'dshProbe': token, 'source': source, 'selection': selection,
                          'pair': uid + ':' + gid, 'caps': 'zero', 'offline': True}
                if case == 'stale': report['dshProbe'] = 'stale'
                if case == 'wrong-selection': report['selection'] = '9.9.9'
                if case == 'wrong-source': report['source'] = 'stale'
                if case == 'duplicate': report['pair'] = '12345:23456'
                details = {'dshRuntime': token, 'source': source, 'selection': selection, 'pair': uid + ':' + gid,
                           'runtime': {'fixtureOnly': True}, 'web': {'http': {'status': 200}, 'cleanup': {'execTmpRemoved': True, 'survivors': 0}},
                           'independent': {'subprocess': {'stdout': 'independent-dsh\n', 'exitCode': 7, 'disposed': True}}}
                if case == 'stale-details': details['source'] = 'stale'
                if case == 'dirty-temp': details['web']['cleanup']['execTmpRemoved'] = False
                output = '' if case == 'missing' else json.dumps(details) + '\n' + json.dumps(report)
            failure = case == action or (case == 'web' and action == 'probe')
            if case == 'signal' and action == 'prepare': signal_parent = True
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
    if args and args[0].endswith('/standard-node-layout.py'):
        state_path = Path(os.environ['AVM_FAKE_STATE'])
        state = json.loads(state_path.read_text())
        state['commands'].append(['layout', *args[1:]])
        state_path.write_text(json.dumps(state))
        assert '--platform' in args and '--expect-version' in args
        return 1 if os.environ.get('AVM_FAKE_CASE') == 'layout' else 0
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


def dsh_action_tests(work):
    spec = importlib.util.spec_from_file_location('dsh_actions', ROOT / 'script/test/fixtures/standard-node-dsh.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    prefix = work / 'owning-actions/prefix'
    prefix.mkdir(parents=True)
    command = work / 'owning-actions/bin/dsh'
    command.parent.mkdir()
    inputs = ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256']
    for name in inputs: (prefix / name).write_text(name)
    calls = []
    with patch.object(module, 'PREFIX', prefix), patch.object(module, 'BIN', command), \
            patch.object(module, 'run', lambda argv, **options: calls.append((argv, options))):
        for action in [['prepare', '0.1.5-rc.1'], ['install'], ['verify']]:
            with patch.object(sys, 'argv', ['fixture', *action]): module.main()
    assert [Path(argv[1]).name for argv, _ in calls] == ['prepare-lock.sh', 'run-install.sh', 'verify-dsh.sh']
    assert calls[0] == (['sh', str(module.SOURCE / 'prepare-lock.sh'), str(prefix), '0.1.5-rc.1', ''], {'seconds': 625})
    assert calls[1] == (['sh', '/fixtures/contract/run-install.sh', 'dsh', '/fixtures/install-status/dsh',
                         'sh', str(module.SOURCE / 'install-dsh.sh')], {'seconds': 625})
    assert os.readlink(command) == str(prefix / 'node_modules/@deepseek-ai/dsh/lib/bin.js')
    assert [(prefix / name).read_text() for name in inputs] == inputs
    print('standard-node-consumers dsh self-test: owning actions/direct link PASS')


def dsh_tests(work, environment):
    dsh_action_tests(work)
    cases = ['positive', 'driver', 'daemon-platform', 'image-platform', 'capacity',
             'create', 'copy', 'initialize', 'prepare', 'install', 'verify', 'clean',
             'disconnect', 'attachments', 'layout', 'web', 'stale', 'wrong-source',
             'wrong-selection', 'duplicate', 'missing', 'stale-details', 'dirty-temp', 'remove', 'volume-remove', 'signal']
    for case in cases:
        state_path = work / ('dsh-' + case + '.json')
        state_path.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        env = dict(environment, AVM_FAKE_CASE=case, AVM_FAKE_STATE=str(state_path))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['bash', str(ENTRY), 'base:fixture', '--consumer', 'dsh',
                                     '--platform', 'linux/arm64'], env=env, stdout=output, stderr=output, timeout=45)
            output.seek(0)
            text = output.read().decode()
        assert (result.returncode == 0) == (case == 'positive'), (case, result.returncode, text)
        state = json.loads(state_path.read_text())
        if case not in ['remove', 'volume-remove']:
            assert not state['containers'] and not state['volume'], (case, state)
        if case == 'positive':
            commands = state['commands']
            actions = [args[args.index(DSH_BODY) + 1] for args in commands if DSH_BODY in args]
            assert actions == ['initialize', 'prepare', 'install', 'verify', 'clean'] + ['probe'] * 4, actions
            create = next(args for args in commands if args[0] == 'create' and DSH_BODY not in args and args[args.index('--name') + 1].endswith('-probe'))
            assert '/runtime-homes:rw,mode=1777' in create and '/root:rw,mode=0770,uid=0,gid=23456' in create
            assert create[create.index('--cap-drop') + 1] == 'ALL'
            copies = [args for args in commands if args[0] == 'cp']
            assert [Path(args[1]).name for args in copies[:4]] == ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256']
            assert len(copies) == 6 and copies[-1][1].endswith('/usr/local/bin/dsh')
            layout = next(args for args in commands if args[0] == 'layout')
            assert layout[layout.index('--expect-version') + 1] == '0.1.5-rc.2'
            probes = [args for args in commands if DSH_BODY in args and 'probe' in args]
            assert [(args[args.index('--reuid') + 1], args[args.index('--regid') + 1]) for args in probes] == [('12345','23456'),('0','23456'),('12345','0'),('12345','12345')]
            for args in probes:
                assert all(flag in args for flag in ['--bounding-set=-all','--inh-caps=-all','--ambient-caps=-all','--clear-groups','--no-new-privs'])
            clean = next(index for index, args in enumerate(commands) if DSH_BODY in args and 'clean' in args)
            disconnect = next(index for index, args in enumerate(commands) if args[:2] == ['network','disconnect'])
            oracle = commands.index(layout)
            first_probe = commands.index(probes[0])
            assert clean < disconnect < oracle < first_probe
            for args in commands:
                if DSH_BODY in args:
                    action = args[args.index(DSH_BODY) + 1]
                    seconds = '630' if action in ['prepare','install'] else '180'
                    assert seconds in args and '--kill-after=5' in args
        print('standard-node-consumers dsh self-test: ' + case + ' PASS')
    staged_root = (work / 'staged-fixture').resolve()
    staged = staged_root / 'inputs'
    staged.mkdir(parents=True)
    (staged_root / 'pnpm').mkdir()
    import shutil
    shutil.copyfile(ROOT / 'images/pnpm/pin.json', staged_root / 'pnpm/pin.json')
    for name in ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256']:
        shutil.copyfile(ROOT / 'images/tools/dsh' / name, staged / name)
    for name, flags in [('changed', ['--version', '0.1.5-rc.1']),
                        ('cordis-negative', ['--cordis-negative']),
                        ('cordis-negative-failure', ['--cordis-negative']),
                        ('inputs', ['--version', '0.1.5-rc.1', '--inputs', str(staged)])]:
        state_path = work / ('dsh-' + name + '.json')
        state_path.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        case = 'cordis-negative' if name == 'cordis-negative-failure' else 'positive'
        env = dict(environment, AVM_FAKE_CASE=case, AVM_FAKE_STATE=str(state_path), AGENT_VM_FIXTURE_ROOT=str(staged_root))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['bash', str(ENTRY), 'base:fixture', '--consumer', 'dsh', *flags],
                                    env=env, stdout=output, stderr=output, timeout=90)
            output.seek(0)
            text = output.read().decode()
        assert (result.returncode == 0) == (name != 'cordis-negative-failure'), (name, text)
        state = json.loads(state_path.read_text())
        if name.startswith('cordis-negative'):
            # The negative leg runs before the committed install, so a failure in it
            # cannot be mistaken for a healthy run, and the overrides-removed copy is
            # uploaded from this invocation's scratch.
            actions = [args[args.index(DSH_BODY) + 1] for args in state['commands'] if DSH_BODY in args]
            assert actions[:2] == ['initialize', 'cordis-negative'], actions
            uploads = [args for args in state['commands'] if args[0] == 'cp' and args[-1].endswith(':/invocation/cordis-negative')]
            assert len(uploads) == 1 and 'cordis-negative' in uploads[0][1], uploads
            if name == 'cordis-negative':
                assert 'cordisNegative' in text, text[-3000:]
            else:
                # A failing negative leg blocks the repair/readiness steps entirely:
                # a recorded failure cannot be mistaken for acceptance.
                assert 'dsh PASS' not in text
                assert not any('install' in args for args in state['commands'] if DSH_BODY in args)
            assert not state['containers'] and not state['volume']
            print('standard-node-consumers dsh self-test: ' + name + ' PASS')
            continue
        assert state['binding'][2] == '0.1.5-rc.1'
        layout = next(args for args in state['commands'] if args[0] == 'layout')
        assert layout[layout.index('--expect-version') + 1] == '0.1.5-rc.1'
        if name == 'inputs':
            assert all(Path(args[1]).parent == staged for args in state['commands'] if args[0] == 'cp' and not ':' in args[1])
        assert not state['containers'] and not state['volume']
        print('standard-node-consumers dsh self-test: ' + name + ' PASS')
    for flags in [['--version','latest'], ['--version','12'], ['--version','0.1.5-rc.1','--version','0.1.5-rc.2'], ['--inputs','/tmp']]:
        result = subprocess.run(['bash', str(ENTRY), 'base:fixture', '--consumer','dsh', *flags],
                                env=environment, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        assert result.returncode == 2, flags


def main():
    with tempfile.TemporaryDirectory(prefix='standard-node-self-test-') as directory:
        work = Path(directory)
        tools = work / 'bin'
        tools.mkdir()
        for executable, mode in [('docker', '--fake-docker'), ('python3', '--fake-python')]:
            file = tools / executable
            file.write_text(f'#!{sys.executable}\nimport runpy,sys\nsys.argv=[{str(Path(__file__).resolve())!r},{mode!r},*sys.argv[1:]]\nrunpy.run_path(sys.argv[0],run_name="__main__")\n')
            file.chmod(0o755)
        environment = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
                           AGENT_VM_EVIDENCE_ROOT=str(work / 'evidence-subprocess'))
        for variable in ['NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS']:
            environment.pop(variable, None)
        spec = importlib.util.spec_from_file_location('primitive_host', ROOT / 'script/test/standard-node-consumers.py')
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        # Fake-Docker runs must not leave artifact-shaped evidence in the build
        # log directory: redirect it and prove the real directory stayed untouched.
        evidence_root = work / 'evidence'
        evidence_root.mkdir()
        module.EVIDENCE_ROOT = evidence_root
        real_evidence = module.ROOT / 'target/ci-logs'
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
        dsh_tests(work, environment)
        for args in [[], ['base', '--bad'], ['base', '--consumer', 'primitives', '--extra'],
                     ['base', '--consumer', 'primitives', '--platform', 'linux/riscv64']]:
            result = subprocess.run(['bash', str(ENTRY), *args], env=environment, capture_output=True)
            assert result.returncode == 2
        for consumer in ['pi', 'copilot']:
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
        # The redirect is proven positively: a child's evidence appears under the
        # redirected root, not the build-log default. A real harness may legitimately
        # add its own evidence while this self-test runs, so only this run's tokens
        # are checked for leakage into target/ci-logs.
        own_tokens = set()

        def note_tokens(state):
            for args in state['commands']:
                if args[0] != 'create' or '--name' not in args:
                    continue
                name = args[args.index('--name') + 1]
                for suffix in ('-capacity', '-setup', '-probe'):
                    if name.endswith(suffix):
                        own_tokens.add(name[:-len(suffix)])

        for path in work.glob('*.json'):
            note_tokens(json.loads(path.read_text()))
        redirected = {path.name for path in (work / 'evidence-subprocess').glob('pnpm-primitives-*')}
        assert own_tokens, 'the self-test recorded no invocation token'
        assert redirected, 'no child evidence reached the redirected evidence root'
        own_tokens |= redirected
        leaked = sorted(path.name for path in real_evidence.glob('pnpm-primitives-*')
                        if path.name in own_tokens) if real_evidence.is_dir() else []
        assert not leaked, ('the fake suite wrote evidence into target/ci-logs', leaked)
    print('standard-node-consumers: plumbing self-test PASS (no native acceptance claim)')


if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-docker':
        sys.exit(fake_docker(sys.argv[2:]))
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-python':
        sys.exit(fake_python(sys.argv[2:]))
    main()
