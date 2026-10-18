#!/usr/bin/env python3
"""Scripted Docker/artifact plumbing tests; never a policy or native proof.

The native matrix's value is that real pnpm decides it. This file therefore only
checks what can be checked without a daemon: CLI/usage/subset handling, the
per-scenario expectation table and aggregation, that a wrong or missing terminal
record is rejected, timeout/signal propagation, owned cleanup, and that the
fixture registry seam refuses a production prefix.

Every record the fake daemon prints is derived from the driver's own expectation
table, so a branch that stopped checking a field would show up as a self-test
failure rather than as a green native run.
"""

from __future__ import annotations

import contextlib
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
ENTRY = ROOT / 'script/test/standard-node-policy.sh'
DRIVER = ROOT / 'script/test/standard-node-policy.py'
FIXTURE = ROOT / 'script/test/fixtures/standard-node-policy.py'


def load(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    # dataclasses resolve their defining module through sys.modules.
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


DRIVER_MODULE = load(DRIVER, 'policy_driver')
# The fake matrix must never write artifact-shaped evidence: it is redirected to
# the test's own temp directory in main().
REAL_EVIDENCE = DRIVER_MODULE.ROOT / 'target/ci-logs'
FIXTURE_MODULE = load(FIXTURE, 'policy_fixture')


def scenario_record(name, soft, case='positive'):
    """A record the driver must accept, before `case` perturbs one field."""
    scenario = DRIVER_MODULE.SCENARIO_BY_NAME[name]
    failure = scenario.code is not None
    record = {
        'scenario': name,
        'soft': soft,
        'softObserved': soft,
        'expected': 'failure' if failure else 'success',
        'envelopeStatus': 1 if failure else 0,
        'childStatus': 1 if failure else 0,
        'terminal': {'code': scenario.code or '', 'message': scenario.reason or ''},
        'lockUnchanged': True,
        'scriptSentinel': False,
        'receiptUnchanged': True,
        'npmFallback': False,
        'status': 'pending' if failure else 'installed',
        'nodeModules': not failure,
        'registryRequests': 4,
        'tarballServed': True,
        'networkError': False,
    }
    if case == 'wrong-code':
        record['terminal']['code'] = 'ERR_PNPM_SUDDEN_DEATH'
    elif case == 'no-code':
        record['terminal']['code'] = None
    elif case == 'network':
        record['terminal']['code'] = 'ERR_PNPM_META_FETCH_FAIL'
        record['networkError'] = True
    elif case == 'installed':
        record['status'] = 'installed'
    elif case == 'partial':
        record['nodeModules'] = True
    elif case == 'soft-changed':
        record['softObserved'] = 'unset' if soft != 'unset' else '1'
    return record


def fake_docker(args):
    state_path = Path(os.environ['AVM_FAKE_STATE'])
    state = json.loads(state_path.read_text())
    state['commands'].append(args)
    case = os.environ.get('AVM_FAKE_CASE', 'positive')
    output = ''
    failure = False
    signal_parent = False
    key = args[0]
    if key == 'context':
        output = 'native-linux-daemon\n'
    elif key == 'buildx':
        output = 'Driver: docker-container\n' if case == 'driver' else 'Driver: docker\n'
    elif key == 'info':
        output = json.dumps({'OSType': 'linux', 'Architecture': 'aarch64'})
    elif args[:2] == ['image', 'inspect']:
        architecture = 'amd64' if case == 'image-platform' else 'arm64'
        output = json.dumps([{'Os': 'linux', 'Architecture': architecture, 'Id': 'sha256:fake'}])
    elif key == 'history':
        output = 'fixture history\n'
    elif args[:2] == ['volume', 'create']:
        state['volume'] = args[-1]
        output = args[-1] + '\n'
    elif key == 'create':
        name = args[args.index('--name') + 1]
        state['containers'].append(name)
        failure = case == 'create'
        output = name + '\n'
    elif key == 'start':
        output = state['containers'][-1] + '\n'
        failure = case == 'start'
    elif key == 'exec':
        command = args[args.index('exec') + 1:]
        while command and command[0].startswith('--'):
            command = command[2:]
        command = command[1:]  # container name
        if command[:2] == ['df', '-Pk']:
            free = 100 if case == 'capacity' else 4000000
            output = f'Filesystem 1024-blocks Used Available Capacity Mounted on\noverlay 8000000 1000 {free} 1% /work\n'
        elif command[:2] == ['mkdir', '-p']:
            output = ''
        elif command[0] == 'python3' and command[-1] == 'ready':
            output = json.dumps({'ready': 'policy', 'arch': 'aarch64', 'launcher': 'a' * 64,
                                 'manager': '11.11.0' if case != 'manager' else '12.9.1',
                                 'readonly': True})
            failure = case == 'readiness'
        elif command[0] == 'python3' and 'run' in command:
            name, soft = command[command.index('run') + 1:]
            assert 'AGENT_VM_FIXTURE_ROOT=/work/fixtures' in args, args
            soft_args = [value for value in args if value.startswith('AGENT_INSTALL_SOFT_FAIL=')]
            assert soft_args == ([] if soft == 'unset' else ['AGENT_INSTALL_SOFT_FAIL=' + soft]), args
            state.setdefault('runs', []).append(f'{name}/{soft}')
            if case == 'skip' and name == 'too-young' and soft == '1':
                output = ''
            else:
                record_case = case if case in ('wrong-code', 'no-code', 'network', 'installed',
                                               'partial', 'soft-changed') else 'positive'
                output = json.dumps(scenario_record(name, soft, record_case)) + '\n'
            if case == 'duplicate':
                output = output + output
            if case == 'timeout':
                time.sleep(30)
            if case == 'signal':
                signal_parent = True
        else:
            raise AssertionError('unhandled fake docker exec: ' + repr(args))
    elif key == 'ps':
        name = args[-1].removeprefix('name=^/').removesuffix('$')
        output = name + '\n' if name in state['containers'] else ''
    elif key == 'rm':
        name = args[-1]
        failure = case == 'remove'
        if not failure:
            assert name in state['containers'], 'attempted unrelated container cleanup'
            state['containers'].remove(name)
    elif args[:2] == ['volume', 'rm']:
        assert args[-1] == state['volume'], 'attempted unrelated volume cleanup'
        failure = case == 'volume-remove'
        if not failure:
            state['volume'] = None
    else:
        raise AssertionError('unhandled fake docker command: ' + repr(args))
    state_path.write_text(json.dumps(state))
    if signal_parent:
        os.kill(os.getppid(), signal.SIGTERM)
    if failure:
        print('scripted failure: ' + case, file=sys.stderr)
        return 19
    print(output, end='')
    return 0


def fake_python(args):
    if args and args[0].endswith('/host-watchdog.py'):
        assert args[1] == '3600', 'missing overall 3600s watchdog'
        if os.environ.get('AVM_FAKE_CASE') == 'watchdog-timeout':
            return 124
    os.execv(sys.executable, [sys.executable, *args])


def check_expectation_branches():
    """Every accepted record must be rejectable by perturbing one field."""
    mutations = {
        'expected': 'success',
        'lockUnchanged': False,
        'scriptSentinel': True,
        'receiptUnchanged': False,
        'npmFallback': True,
        'networkError': True,
        'registryRequests': 0,
        'softObserved': 'unset',
        'status': 'installed',
        'nodeModules': True,
        'terminal': {'code': 'ERR_PNPM_SUDDEN_DEATH', 'message': ''},
        'envelopeStatus': 0,
        'childStatus': 0,
    }
    scenario = DRIVER_MODULE.SCENARIO_BY_NAME['too-young']
    good = scenario_record('too-young', '1')
    assert DRIVER_MODULE.validate(scenario, '1', good) == [], DRIVER_MODULE.validate(scenario, '1', good)
    for field, value in mutations.items():
        broken = dict(good)
        broken[field] = value
        assert DRIVER_MODULE.validate(scenario, '1', broken), f'{field} mutation was accepted'
    success = DRIVER_MODULE.SCENARIO_BY_NAME['eligible']
    good = scenario_record('eligible', 'unset')
    assert DRIVER_MODULE.validate(success, 'unset', good) == []
    for field, value in [('childStatus', 1), ('status', 'pending'), ('nodeModules', False),
                         ('expected', 'failure'), ('envelopeStatus', 1)]:
        broken = dict(good)
        broken[field] = value
        assert DRIVER_MODULE.validate(success, 'unset', broken), f'{field} mutation was accepted'
    corrupt = DRIVER_MODULE.SCENARIO_BY_NAME['corrupt-tarball']
    good = scenario_record('corrupt-tarball', '0')
    assert DRIVER_MODULE.validate(corrupt, '0', good) == [], DRIVER_MODULE.validate(corrupt, '0', good)
    broken = dict(good, tarballServed=False)
    assert DRIVER_MODULE.validate(corrupt, '0', broken), 'missing tarball proof was accepted'
    assert DRIVER_MODULE.validate(scenario, '1', {'scenario': 'too-young'}), 'incomplete record accepted'
    print('standard-node-policy self-test: expectation-table branches PASS')


def check_fixture_authorization():
    """The registry override must be impossible for a production prefix."""
    refused = FIXTURE_MODULE.Refused
    cases = []
    for root in (None, '', 'relative/root', '/', '/opt', '/opt/agent-vm/dsh'):
        cases.append(({'AGENT_VM_FIXTURE_ROOT': root} if root is not None else {}, 'fixture root'))
    for environment, label in cases:
        saved = os.environ.get('AGENT_VM_FIXTURE_ROOT')
        try:
            if 'AGENT_VM_FIXTURE_ROOT' in environment:
                if environment['AGENT_VM_FIXTURE_ROOT'] is None:
                    os.environ.pop('AGENT_VM_FIXTURE_ROOT', None)
                else:
                    os.environ['AGENT_VM_FIXTURE_ROOT'] = environment['AGENT_VM_FIXTURE_ROOT']
            else:
                os.environ.pop('AGENT_VM_FIXTURE_ROOT', None)
            try:
                FIXTURE_MODULE.authorized_root()
            except (refused, OSError):
                continue
            raise AssertionError(f'{label} was authorized')
        finally:
            if saved is None:
                os.environ.pop('AGENT_VM_FIXTURE_ROOT', None)
            else:
                os.environ['AGENT_VM_FIXTURE_ROOT'] = saved
    with tempfile.TemporaryDirectory(prefix='policy-root-') as directory:
        root = Path(directory).resolve()
        os.environ['AGENT_VM_FIXTURE_ROOT'] = str(root)
        try:
            assert FIXTURE_MODULE.authorized_root() == root
            project = FIXTURE_MODULE.authorized_project(root, 'eligible', 'unset')
            assert project.parent == root
            for name, soft in [('../escape', 'unset'), ('eligible', 'two')]:
                try:
                    FIXTURE_MODULE.authorized_project(root, name, soft)
                except refused:
                    continue
                raise AssertionError(f'{name}/{soft} was authorized')
            assert FIXTURE_MODULE.authorized_registry(project, 'http://127.0.0.1:12345') \
                == 'http://127.0.0.1:12345'
            for url in ('https://registry.npmjs.org/', 'http://example.com:80', 'http://127.0.0.1'):
                try:
                    FIXTURE_MODULE.authorized_registry(project, url)
                except refused:
                    continue
                raise AssertionError(f'{url} was authorized as a fixture registry')
        finally:
            os.environ.pop('AGENT_VM_FIXTURE_ROOT', None)
    print('standard-node-policy self-test: fixture-registry production-prefix denial PASS')


def main():
    check_expectation_branches()
    check_fixture_authorization()
    before_evidence = sorted(REAL_EVIDENCE.glob('pnpm-policy-*')) if REAL_EVIDENCE.is_dir() else []
    with tempfile.TemporaryDirectory(prefix='standard-node-policy-self-test-') as directory:
        work = Path(directory)
        DRIVER_MODULE.EVIDENCE_ROOT = work / 'evidence'
        DRIVER_MODULE.EVIDENCE_ROOT.mkdir()
        tools = work / 'bin'
        tools.mkdir()
        for executable, mode in [('docker', '--fake-docker'), ('python3', '--fake-python')]:
            file = tools / executable
            file.write_text(f'#!{sys.executable}\nimport runpy,sys\n'
                            f'sys.argv=[{str(Path(__file__).resolve())!r},{mode!r},*sys.argv[1:]]\n'
                            f'runpy.run_path(sys.argv[0],run_name="__main__")\n')
            file.chmod(0o755)
        environment = dict(os.environ, PATH=str(tools) + os.pathsep + os.environ['PATH'],
                           AGENT_VM_EVIDENCE_ROOT=str(work / 'evidence-subprocess'))
        for variable in ['NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS', 'AGENT_INSTALL_SOFT_FAIL']:
            environment.pop(variable, None)

        def state_file(label):
            path = work / (label + '.json')
            path.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None}))
            return path

        def entry(label, args, case='positive', timeout=120):
            state = state_file(label)
            result = subprocess.run(['bash', str(ENTRY), *args],
                                    env=dict(environment, AVM_FAKE_CASE=case, AVM_FAKE_STATE=str(state)),
                                    capture_output=True, text=True, timeout=timeout)
            return result, json.loads(state.read_text())

        # Usage problems must be decided before any daemon call.
        for args in ([], ['--bad', 'x'], ['base', '--consumer'], ['base', '--extra', 'y'],
                     ['base', '--consumer', 'dsh', '--consumer', 'dsh'],
                     ['base', '--platform', 'linux/riscv64'],
                     ['base', '--platform', 'linux/arm64', '--platform', 'linux/arm64']):
            result, state = entry('usage', args)
            assert result.returncode == 2, (args, result.returncode, result.stderr)
            assert state['commands'] == [], (args, state['commands'])
        for consumer in ('pi', 'bridge', 'copilot', 'unknown'):
            result, state = entry('unmigrated', ['base', '--consumer', consumer])
            assert result.returncode == 2, (consumer, result.returncode)
            assert 'not migrated' in result.stderr or 'unknown consumer' in result.stderr
            assert state['commands'] == []
        print('standard-node-policy self-test: CLI parsing/subset PASS')

        # Positive: default consumer runs the migrated set only, every scenario
        # under every soft value, and removes its container and volume.
        result, state = entry('positive', ['base:fixture', '--platform', 'linux/arm64'])
        assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
        assert 'migrated consumers: dsh' in result.stdout, result.stdout
        assert 'standard-node-policy: dsh owning policy matrix PASS' in result.stdout, result.stdout
        expected_runs = {f'{scenario.name}/{soft}' for scenario in DRIVER_MODULE.SCENARIOS
                         for soft in DRIVER_MODULE.SOFT_VALUES}
        assert set(state['runs']) == expected_runs, sorted(set(state['runs']) ^ expected_runs)
        assert len(state['runs']) == len(expected_runs), 'a scenario ran twice'
        assert not state['containers'] and state['volume'] is None, state
        creates = [args for args in state['commands'] if args[0] == 'create']
        assert len(creates) == 1, creates
        assert '--network' in creates[0] and 'none' in creates[0]
        assert 'readonly' in ' '.join(creates[0])
        assert not any(args[0] == 'run' for args in state['commands']), 'host network used'
        print('standard-node-policy self-test: positive matrix + owned cleanup PASS')

        # Result branches: a wrong code, a missing code, a transport failure and
        # a changed soft observation must each fail the suite.
        for case in ('wrong-code', 'no-code', 'network', 'installed', 'partial', 'soft-changed',
                     'skip', 'duplicate'):
            result, state = entry(case, ['base:fixture', '--platform', 'linux/arm64'], case=case)
            assert result.returncode == 1, (case, result.returncode, result.stderr)
            assert 'owning policy matrix PASS' not in result.stdout, (case, result.stdout)
            assert not state['containers'] and state['volume'] is None, (case, state)
        print('standard-node-policy self-test: result-branch rejection PASS')

        # Driver/daemon/image/readiness/capacity failures stay hard.
        for case in ('driver', 'image-platform', 'readiness', 'capacity', 'manager', 'create',
                     'start', 'remove', 'volume-remove'):
            result, state = entry(case, ['base:fixture', '--platform', 'linux/arm64'], case=case)
            assert result.returncode != 0, (case, result.returncode, result.stderr)
            assert 'owning policy matrix PASS' not in result.stdout, (case, result.stdout)
            if case not in ('remove', 'volume-remove'):
                assert not state['containers'] and state['volume'] is None, (case, state)
        result, state = entry('daemon-platform', ['base:fixture', '--platform', 'linux/amd64'])
        assert result.returncode == 1 and 'mismatch' in result.stderr, result.stderr
        result, state = entry('watchdog-timeout', ['base:fixture', '--platform', 'linux/arm64'],
                              case='watchdog-timeout')
        assert result.returncode == 124, (result.returncode, result.stderr)
        result, state = entry('signal', ['base:fixture', '--platform', 'linux/arm64'], case='signal')
        assert result.returncode == 143, (result.returncode, result.stderr)
        assert 'signal' in result.stderr, result.stderr
        assert not state['containers'] and state['volume'] is None, state
        print('standard-node-policy self-test: orchestration failures PASS')

        # A manager timeout must be a hard failure, not missing evidence success.
        state_path = state_file('timeout')
        environment_timeout = dict(environment, AVM_FAKE_CASE='timeout', AVM_FAKE_STATE=str(state_path))
        saved = DRIVER_MODULE.COMMAND_TIMEOUT_SECONDS
        with patch.object(DRIVER_MODULE, 'COMMAND_TIMEOUT_SECONDS', 1.0), \
                patch.dict(os.environ, environment_timeout), \
                contextlib.redirect_stdout(io.StringIO()) as output, \
                contextlib.redirect_stderr(io.StringIO()) as errors:
            try:
                status = DRIVER_MODULE.main(['base:fixture', '--platform', 'linux/arm64'])
            finally:
                for sig, handler in [(signal.SIGINT, signal.default_int_handler),
                                     (signal.SIGTERM, signal.SIG_DFL), (signal.SIGHUP, signal.SIG_DFL)]:
                    signal.signal(sig, handler)
                DRIVER_MODULE.COMMAND_TIMEOUT_SECONDS = saved
        assert status == 124, (status, output.getvalue(), errors.getvalue())
        assert 'owning policy matrix PASS' not in output.getvalue()
        state = json.loads(state_path.read_text())
        assert not state['containers'] and state['volume'] is None, state
        print('standard-node-policy self-test: command timeout PASS')

        sentinel = work / 'preloaded'
        preload = work / 'preload.cjs'
        preload.write_text(f'require("fs").writeFileSync({str(sentinel)!r}, "unsafe")')
        for variable, value in [('NODE_OPTIONS', '--require=' + str(preload)),
                                ('NODE_PATH', str(work)),
                                ('BUN_OPTIONS', '--preload=' + str(preload))]:
            result = subprocess.run(['bash', str(ENTRY), 'base', '--consumer', 'dsh'],
                                    env=dict(environment, **{variable: value}), capture_output=True)
            assert result.returncode == 1 and not sentinel.exists(), variable
            assert b'runtime overrides' in result.stderr
        print('standard-node-policy self-test: pre-Node guard PASS')

    after_evidence = sorted(REAL_EVIDENCE.glob('pnpm-policy-*')) if REAL_EVIDENCE.is_dir() else []
    assert after_evidence == before_evidence, 'the fake matrix wrote evidence into target/ci-logs'
    print('standard-node-policy: plumbing self-test PASS (no policy or native claim)')


if __name__ == '__main__':
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-docker':
        sys.exit(fake_docker(sys.argv[2:]))
    if len(sys.argv) > 1 and sys.argv[1] == '--fake-python':
        sys.exit(fake_python(sys.argv[2:]))
    main()
