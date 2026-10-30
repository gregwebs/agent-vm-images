#!/usr/bin/env python3
"""Bounded host orchestration for the deterministic owning-policy matrix.

The matrix proves the *base* pnpm policy, not a fixture manager: one container
runs the real ``/usr/local/bin/pnpm`` launcher and the shared hard runner
against a loopback registry in the same network namespace, so ``--network none``
is enough and no external registry or credential is involved. Every scenario
starts cold (unique HOME/XDG/store) and repeats with ``AGENT_INSTALL_SOFT_FAIL``
unset, 1 and 0, because a soft value that changed an outcome would be a policy
regression rather than a build convenience.

An expected child failure is evidence, not a suite failure: the fixture reports
the child status and the terminal pnpm record, and this driver compares them
against the independently authored expectations below. A generic transport
failure can never satisfy a policy expectation because the code must match
exactly and the registry request log must show the request arrived.
"""

from __future__ import annotations

import hashlib
import json
import os
import signal
import subprocess
import sys
import time
import uuid
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
# Evidence lives under the build root. AGENT_VM_EVIDENCE_ROOT exists only so the
# fake-Docker self-tests can redirect it: a directory under target/ci-logs is a
# claim about a real run, and a fake run must not be able to make that claim.
EVIDENCE_ROOT = Path(os.environ.get('AGENT_VM_EVIDENCE_ROOT') or ROOT / 'target/ci-logs')
FIXTURE = '/source/script/test/fixtures/standard-node-policy.py'
USAGE = ('usage: standard-node-policy.sh --self-test\n'
         '       standard-node-policy.sh BASE_IMAGE [--platform linux/ARCH] '
         '[--consumer dsh|pi|bridge|copilot|all]')
SOFT_VALUES = ('unset', '1', '0')
# One scenario runs a bounded lock construction and one 600 s-bounded manager
# install; the whole matrix is bounded by the shell watchdog as well.
COMMAND_TIMEOUT_SECONDS = 700
OUTPUT_LIMIT_BYTES = 64 * 1024
VOLUME_FREE_MIN_KIB = 524288
PLATFORMS = {'linux/amd64': 'amd64', 'linux/arm64': 'arm64'}


def contract_context(platform):
    """Composed helper directory for the platform, through the shared adapter."""
    adapter = ROOT / 'script/build/recipe-contract.sh'
    result = subprocess.run(['bash', str(adapter), '--platform', platform],
                            capture_output=True, text=True)
    if result.returncode:
        raise Failure('cannot build the ' + platform + ' recipe-contract helper: '
                      + (result.stderr.strip() or result.stdout.strip() or 'no diagnostic'))
    context = Path(result.stdout.strip())
    if not (context / 'out/recipe-contract').is_file():
        raise Failure('the ' + platform + ' recipe-contract build published no binary under ' + str(context))
    return context
MIGRATED = ('dsh', 'pi', 'bridge')
UNMIGRATED = ('copilot',)
# The Pi/bridge legs do not run the fake-registry matrix (that matrix proves the
# base policy, which those projects inherit unchanged). They prove the *owning
# preparation boundary* of each project on its real four inputs: an equal request
# starts no manager at all, every bypass/override is refused before Node, and a
# changed or refreshed request is hard when the authorized manager route cannot
# complete. The fixture's recorder shim is the only manager route in those cases,
# so "the shim was never called" is an observable fact, not the driver's claim.
PREPARE_INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
PREPARE_SOURCE = {'pi': 'images/tools/pi', 'bridge': 'images/tools/pi/bridge'}
PREPARE_COMMITTED = {'pi': 'images/tools/pi', 'bridge': 'images/tools/pi/bridge'}
# Expectations are authored here, not in the fixture: the fixture only reports
# what happened. `managerCalls` False is the zero-manager guarantee; `reason` is
# required diagnostic text for a hard failure.
PREPARE_CASES = {
    'equal': {'arguments': [], 'managerCalls': False, 'status': 0, 'reason': None},
    'equal-exact-selection': {'arguments': ['exact'], 'managerCalls': False, 'status': 0,
                              'reason': None},
    'refresh-equal': {'arguments': ['--refresh-lock'], 'managerCalls': True, 'status': 1,
                      'reason': None},
    'changed': {'arguments': ['0.0.1'], 'managerCalls': True, 'status': 1, 'reason': None},
    'manager-mismatch': {'arguments': ['0.0.1'], 'managerCalls': True, 'status': 1,
                         'reason': '11.11.0'},
    'node-options': {'arguments': [], 'managerCalls': False, 'status': 1,
                     'reason': 'NODE_OPTIONS'},
    'node-path': {'arguments': [], 'managerCalls': False, 'status': 1, 'reason': 'NODE_PATH'},
    'bun-options': {'arguments': [], 'managerCalls': False, 'status': 1,
                    'reason': 'BUN_OPTIONS'},
    'npmrc': {'arguments': [], 'managerCalls': False, 'status': 1, 'reason': '.npmrc'},
    'registry-env': {'arguments': [], 'managerCalls': False, 'status': 1,
                     'reason': 'PNPM_CONFIG_REGISTRY'},
}


@dataclass(frozen=True)
class Scenario:
    name: str
    code: str | None
    reason: str | None


# Expected pnpm terminal diagnostics come from the frozen-install oracle in
# script/test/opencode-pnpm-policy.py, which captures what stock 11.11.0 emits.
SCENARIOS = (
    Scenario('eligible', None, None),
    Scenario('too-young', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION',
             'within the minimumReleaseAge cutoff'),
    Scenario('missing-time', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION',
             'could not be checked against minimumReleaseAge'),
    Scenario('invalid-time', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION',
             'publish timestamp is not a valid date'),
    Scenario('partial-time', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION',
             'could not be checked against minimumReleaseAge'),
    Scenario('trust-downgrade', 'ERR_PNPM_TRUST_DOWNGRADE', 'High-risk trust downgrade'),
    Scenario('corrupt-tarball', 'ERR_PNPM_TARBALL_INTEGRITY', None),
    Scenario('manifest-mismatch', 'ERR_PNPM_OUTDATED_LOCKFILE', None),
    Scenario('config-mismatch', 'ERR_PNPM_LOCKFILE_CONFIG_MISMATCH', None),
)
SCENARIO_BY_NAME = {scenario.name: scenario for scenario in SCENARIOS}


class Failure(Exception):
    def __init__(self, message, status=1):
        super().__init__(message)
        self.status = status


def arguments(args):
    """Return (image, platform, consumers). Usage problems raise ValueError."""
    if not args or args[0].startswith('-'):
        raise ValueError(USAGE)
    image = args[0]
    platform = None
    consumer = None
    rest = args[1:]
    while rest:
        flag = rest.pop(0)
        if flag not in ('--platform', '--consumer'):
            raise ValueError(f'unknown option: {flag}\n{USAGE}')
        if not rest or rest[0].startswith('--'):
            raise ValueError(f'missing value for {flag}\n{USAGE}')
        value = rest.pop(0)
        if flag == '--platform':
            if platform is not None:
                raise ValueError(f'duplicate option: {flag}\n{USAGE}')
            platform = value
        else:
            if consumer is not None:
                raise ValueError(f'duplicate option: {flag}\n{USAGE}')
            consumer = value
    if platform is not None and platform not in PLATFORMS:
        raise ValueError(f'unsupported platform: {platform}\n{USAGE}')
    if consumer is None:
        consumer = 'all'
    if consumer in UNMIGRATED:
        raise ValueError(f'consumer not migrated to pnpm yet: {consumer}')
    if consumer == 'all':
        # The default set stays dsh: it is the fake-registry matrix, and widening
        # the default would silently change what a plain invocation proves.
        consumers = ['dsh']
    elif consumer in MIGRATED:
        consumers = [consumer]
    else:
        raise ValueError(f'unknown consumer: {consumer}\n{USAGE}')
    return image, platform, consumer, consumers


def validate(scenario, soft, record):
    """Compare one reported scenario run against the authored expectation."""
    if not isinstance(record, dict):
        return ['scenario record is not an object']
    problems = []
    terminal = record.get('terminal') if isinstance(record.get('terminal'), dict) else {}

    def expect(condition, message):
        if not condition:
            problems.append(message)

    expect(record.get('scenario') == scenario.name,
           f'reported scenario {record.get("scenario")} is not {scenario.name}')
    expect(record.get('soft') == soft, f'reported soft {record.get("soft")} is not {soft}')
    expect(record.get('softObserved') == soft,
           f'the envelope saw soft {record.get("softObserved")}, not {soft}')
    expect(record.get('lockUnchanged') is True, 'the locked graph changed during the cold install')
    expect(record.get('scriptSentinel') is False, 'a package script ran during the install')
    expect(record.get('npmFallback') is False, 'npm was invoked as a fallback')
    expect(record.get('receiptUnchanged') is True, 'the stale transport receipt was modified')
    expect(record.get('networkError') is False, 'a transport failure appeared in the manager output')
    expect(isinstance(record.get('registryRequests'), int) and record['registryRequests'] > 0,
           'the fixture registry served no request')
    if scenario.code is None:
        expect(record.get('expected') == 'success', 'fixture did not expect a success')
        expect(record.get('envelopeStatus') == 0,
               f'envelope status {record.get("envelopeStatus")} is not 0')
        expect(record.get('childStatus') == 0, f'child status {record.get("childStatus")} is not 0')
        expect(record.get('status') == 'installed',
               f'run-install status {record.get("status")} is not installed')
        expect(record.get('nodeModules') is True, 'the installed payload is missing')
    else:
        expect(record.get('expected') == 'failure', 'fixture did not expect a failure')
        expect(record.get('childStatus') != 0,
               'an expected policy failure succeeded')
        expect(record.get('envelopeStatus') not in (0, None),
               f'envelope status {record.get("envelopeStatus")} is not a hard failure')
        expect(record.get('status') == 'pending',
               f'run-install status {record.get("status")} is not pending')
        expect(record.get('nodeModules') is False, 'a partial node_modules was left behind')
        expect(terminal.get('code') == scenario.code,
               f'terminal code {terminal.get("code")} is not {scenario.code}')
        if scenario.reason:
            expect(scenario.reason in (terminal.get('message') or ''),
                   f'terminal message {terminal.get("message")!r} lacks {scenario.reason!r}')
    if scenario.name == 'corrupt-tarball':
        expect(record.get('tarballServed') is True,
               'the registry never served the locked tarball bytes')
    return problems


class Capture:
    """One bounded child whose output goes to files, never inherited pipes.

    A fixture server or grandchild that keeps the pipe open would otherwise hold
    the wrapper read forever; real files also keep the retained evidence inside
    OUTPUT_LIMIT_BYTES instead of buffering in this process.
    """

    def __init__(self, logs: Path, limit=OUTPUT_LIMIT_BYTES):
        self.logs = logs
        self.limit = limit

    def run(self, index, args, timeout=None):
        # Resolved at call time so the suite bound is one named constant.
        timeout = COMMAND_TIMEOUT_SECONDS if timeout is None else timeout
        stdout_path = self.logs / f'{index:03d}.out'
        stderr_path = self.logs / f'{index:03d}.err'
        with open(stdout_path, 'wb') as stdout, open(stderr_path, 'wb') as stderr:
            process = subprocess.Popen(args, stdout=stdout, stderr=stderr)
            deadline = time.monotonic() + timeout
            while True:
                try:
                    process.wait(timeout=0.2)
                except subprocess.TimeoutExpired:
                    if stdout_path.stat().st_size + stderr_path.stat().st_size > self.limit:
                        process.kill()
                        process.wait()
                        raise Failure(f'{args}: output exceeded the {self.limit} byte bound')
                    if time.monotonic() > deadline:
                        process.kill()
                        process.wait()
                        raise Failure(f'command timeout: {args!r}', 124)
                else:
                    break
        for path in (stdout_path, stderr_path):
            if path.stat().st_size > self.limit:
                body = path.read_bytes()[:self.limit]
                path.write_bytes(body + b'\n[evidence truncated]\n')
        return subprocess.CompletedProcess(
            args, process.returncode,
            stdout_path.read_bytes().decode('utf-8', 'replace'),
            stderr_path.read_bytes().decode('utf-8', 'replace'))


class Invocation:
    def __init__(self, image, requested, consumer='dsh'):
        self.image = image
        self.requested = requested
        self.consumer = consumer
        self.token = 'pnpm-policy-' + uuid.uuid4().hex
        self.logs = EVIDENCE_ROOT / self.token
        self.logs.mkdir(parents=True)
        self.capture = Capture(self.logs)
        self.containers = []
        self.volume = None
        self.index = 0
        self.platform = None
        print('standard-node-policy: evidence ' + str(self.logs), flush=True)

    def run(self, args, timeout=None):
        self.index += 1
        return self.capture.run(self.index, args, timeout)

    def docker(self, *args, timeout=None):
        result = self.run(['docker', *args], timeout)
        if result.returncode:
            raise Failure(f'docker {args}: status {result.returncode}: {result.stderr.strip()}',
                          result.returncode if result.returncode > 0 else 1)
        return result.stdout

    def docker_quiet(self, *args):
        """Removal paths tolerate a missing object; failures still surface."""
        return self.run(['docker', *args])

    def json_line(self, output, what):
        records = []
        for line in output.splitlines():
            try:
                value = json.loads(line)
            except ValueError:
                continue
            if isinstance(value, dict):
                records.append(value)
        if len(records) != 1:
            raise Failure(f'{what}: expected one JSON record, got {len(records)}')
        return records[0]

    def create(self, name, *args):
        # Register before create: a failed client can still leave daemon objects,
        # and cleanup must never touch names this invocation did not register.
        self.containers.append(name)
        self.docker('create', '--name', name, *args)
        return name

    def execute(self):
        if self.consumer in ('pi', 'bridge'):
            return self.execute_prepare_policy()
        source = self.run(['git', '-C', str(ROOT), 'rev-parse', 'HEAD'])
        if source.returncode:
            raise Failure('cannot record the source revision')
        source = source.stdout.strip()
        inputs = [ROOT / 'images/pnpm/launcher.sh', ROOT / 'images/pnpm/pin.json',
                  ROOT / 'images/recipe-contract/run-pnpm.sh',
                  ROOT / 'images/recipe-contract/run-install.sh',
                  ROOT / 'images/recipe-contract/pnpmenv/environment.go',
                  ROOT / 'script/test/fixtures/standard-node-policy.py',
                  ROOT / 'script/test/fixtures/pnpm/registry.py',
                  ROOT / 'script/test/fixtures/pnpm/packages.json',
                  ROOT / 'script/test/fixtures/pnpm/scenarios.py',
                  ROOT / 'script/test/fixtures/pnpm/make-packages.py',
                  ROOT / 'images/tools/dsh/pnpm-workspace.yaml']
        (self.logs / 'source.json').write_text(json.dumps({'source': source, 'inputs': {
            str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in inputs}}, indent=2))
        context = self.docker('context', 'show').strip()
        driver = self.docker('buildx', 'inspect', context)
        if not any(line.split() == ['Driver:', 'docker'] for line in driver.splitlines()):
            raise Failure('daemon-backed docker driver required')
        daemon = json.loads(self.docker('info', '--format', '{{json .}}'))
        image = json.loads(self.docker('image', 'inspect', self.image))[0]
        self.docker('history', '--no-trunc', self.image)
        platform = self.requested or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
            daemon['Architecture'], daemon['Architecture'])
        if platform not in PLATFORMS:
            raise Failure(f'unsupported native platform: {platform}')
        if daemon['OSType'] != 'linux' or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
                daemon['Architecture'], daemon['Architecture']) != platform:
            raise Failure('daemon platform mismatch/emulation')
        if image['Os'] != 'linux' or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
                image['Architecture'], image['Architecture']) != platform:
            raise Failure('base-image platform mismatch/emulation')
        self.platform = platform
        print(f'platform: daemon={platform} image={image["Id"]}; the client OS is not a native test',
              flush=True)

        self.volume = self.token + '-fixtures'
        self.docker('volume', 'create', self.volume)
        # The owning env guard and the hard runner are the composed helper's, not a
        # host source copy: the fixture execs `/contract/recipe-contract`.
        self.contract = contract_context(platform)
        name = self.create(self.token + '-policy', '--platform', platform, '--network', 'none',
                           '--cap-drop', 'ALL',
                           '--mount', f'type=bind,src={ROOT},dst=/source,readonly',
                           # The fake-registry matrix reads the composed helper from
                           # `/contract`; only the preparation cases run under the
                           # authorized fixture root and mount it as that root's
                           # child. Each path is owned by its fixture and asserted
                           # by the self-test.
                           '--mount', f'type=bind,src={self.contract / "out"},dst=/contract,readonly',
                           '--mount', f'type=volume,src={self.volume},dst=/work',
                           self.image, 'sleep', 'infinity')
        self.docker('start', name)
        free = self.docker('exec', name, 'df', '-Pk', '/work')
        rows = [line.split() for line in free.splitlines()[1:] if line.strip()]
        if len(rows) != 1 or len(rows[0]) < 4 or int(rows[0][3]) < VOLUME_FREE_MIN_KIB:
            raise Failure(f'capacity: need {VOLUME_FREE_MIN_KIB} KiB free on the fixture volume: {free}')
        self.docker('exec', name, 'mkdir', '-p', '/work/fixtures')
        ready = self.json_line(self.docker('exec', name, 'python3', '-B', FIXTURE, 'ready'),
                              'readiness')
        if ready.get('manager') != '11.11.0' or ready.get('readonly') is not True:
            raise Failure('container readiness/identity evidence invalid: ' + json.dumps(ready))
        print('authenticated base launcher, read-only sources, unset soft mode', flush=True)

        observed = {}
        for scenario in SCENARIOS:
            for soft in SOFT_VALUES:
                environment = ['--env', 'AGENT_VM_FIXTURE_ROOT=/work/fixtures']
                if soft != 'unset':
                    environment += ['--env', f'AGENT_INSTALL_SOFT_FAIL={soft}']
                output = self.docker('exec', *environment, name, 'python3', '-B', FIXTURE,
                                     'run', scenario.name, soft)
                record = self.json_line(output, f'{scenario.name}/{soft}')
                key = (scenario.name, soft)
                if key in observed:
                    raise Failure(f'duplicate scenario report: {scenario.name}/{soft}')
                observed[key] = record
                problems = validate(scenario, soft, record)
                if problems:
                    raise Failure(f'{scenario.name}/{soft}: ' + '; '.join(problems))
                print(f'{scenario.name}/{soft}: ' + json.dumps(
                    {'childStatus': record['childStatus'], 'terminal': record['terminal']['code'],
                     'status': record['status']}), flush=True)
        missing = [(scenario.name, soft) for scenario in SCENARIOS for soft in SOFT_VALUES
                   if (scenario.name, soft) not in observed]
        if missing:
            raise Failure('missing scenario reports: ' + ', '.join(f'{n}/{s}' for n, s in missing))
        for scenario in SCENARIOS:
            outcomes = {(observed[(scenario.name, soft)]['childStatus'] != 0,
                         (observed[(scenario.name, soft)]['terminal'] or {}).get('code'))
                        for soft in SOFT_VALUES}
            if len(outcomes) != 1:
                raise Failure(f'soft mode changed the outcome of {scenario.name}: {outcomes}')
        print(f'migrated consumers exercised: {", ".join(MIGRATED)}; '
              f'{len(SCENARIOS)} scenarios x {len(SOFT_VALUES)} soft values', flush=True)

    def prepare_inputs(self):
        """The consumer's committed four inputs, as one unit."""
        directory = ROOT / PREPARE_COMMITTED[self.consumer]
        missing = [name for name in PREPARE_INPUTS if not (directory / name).is_file()]
        if missing:
            raise Failure(f'the committed {self.consumer} four inputs are incomplete: '
                          + ', '.join(missing))
        return directory

    def execute_prepare_policy(self):
        """Real guards, real driver, offline: one case per authored expectation."""
        inputs = self.prepare_inputs()
        selection = json.loads((inputs / 'package.json').read_text())[
            'dependencies'][{'pi': '@earendil-works/pi-coding-agent',
                             'bridge': 'pi-claude-bridge'}[self.consumer]]
        source = self.run(['git', '-C', str(ROOT), 'rev-parse', 'HEAD'])
        if source.returncode:
            raise Failure('cannot record the source revision')
        source = source.stdout.strip()
        recorded = [*(inputs / name for name in PREPARE_INPUTS),
                    ROOT / f'{PREPARE_SOURCE[self.consumer]}/prepare-lock.sh',
                    ROOT / 'images/tools/pi/prepare-project.py',
                    ROOT / 'images/tools/pnpm-project.py',
                    ROOT / 'script/test/standard-node-layout.py',
                    ROOT / 'script/test/fixtures/standard-node-policy.py']
        (self.logs / 'source.json').write_text(json.dumps({'source': source, 'selection': selection,
                                                           'consumer': self.consumer,
                                                           'inputs': {
            str(item.relative_to(ROOT)) if ROOT in item.parents else str(item):
                hashlib.sha256(item.read_bytes()).hexdigest()
            for item in recorded}}, indent=2))
        context = self.docker('context', 'show').strip()
        driver = self.docker('buildx', 'inspect', context)
        if not any(line.split() == ['Driver:', 'docker'] for line in driver.splitlines()):
            raise Failure('daemon-backed docker driver required')
        daemon = json.loads(self.docker('info', '--format', '{{json .}}'))
        image = json.loads(self.docker('image', 'inspect', self.image))[0]
        self.docker('history', '--no-trunc', self.image)
        platform = self.requested or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
            daemon['Architecture'], daemon['Architecture'])
        if platform not in PLATFORMS:
            raise Failure(f'unsupported native platform: {platform}')
        if daemon['OSType'] != 'linux' or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
                daemon['Architecture'], daemon['Architecture']) != platform:
            raise Failure('daemon platform mismatch/emulation')
        if image['Os'] != 'linux' or 'linux/' + {'aarch64': 'arm64', 'x86_64': 'amd64'}.get(
                image['Architecture'], image['Architecture']) != platform:
            raise Failure('base-image platform mismatch/emulation')
        self.platform = platform
        self.volume = self.token + '-fixtures'
        self.docker('volume', 'create', self.volume)
        self.contract = contract_context(platform)
        name = self.create(self.token + '-policy', '--platform', platform, '--network', 'none',
                           '--cap-drop', 'ALL',
                           '--mount', f'type=bind,src={ROOT},dst=/source,readonly',
                           # The owning filesystem module authorizes the contract
                           # directory as a child of the fixture root, so it is
                           # mounted there (exactly like the dsh harness) rather
                           # than at an unrelated absolute path.
                           '--mount', f'type=bind,src={self.contract / "out"},dst=/work/fixtures/contract,readonly',
                           '--mount', f'type=volume,src={self.volume},dst=/work',
                           self.image, 'sleep', 'infinity')
        self.docker('start', name)
        self.docker('exec', name, 'mkdir', '-p', '/work/fixtures')
        ready = self.json_line(self.docker('exec', name, 'python3', '-B', FIXTURE, 'ready'),
                              'readiness')
        if ready.get('manager') != '11.11.0' or ready.get('readonly') is not True:
            raise Failure('container readiness/identity evidence invalid: ' + json.dumps(ready))
        self.docker('exec', name, 'mkdir', '-p', f'/work/{self.consumer}-inputs')
        for input_name in PREPARE_INPUTS:
            self.docker('cp', str(inputs / input_name),
                        f'{name}:/work/{self.consumer}-inputs/{input_name}')
        output = self.docker('exec', '--env', 'AGENT_VM_FIXTURE_ROOT=/work/fixtures', name,
                             'python3', '-B', FIXTURE, 'prepare-policy', self.consumer, selection)
        records = {}
        for line in output.splitlines():
            try:
                record = json.loads(line)
            except ValueError:
                continue
            if not isinstance(record, dict) or 'case' not in record:
                continue
            if record['case'] in records:
                raise Failure('duplicate preparation-policy record: ' + record['case'])
            records[record['case']] = record
        missing = sorted(set(PREPARE_CASES) - set(records))
        if missing:
            raise Failure('missing preparation-policy records: ' + ', '.join(missing))
        extra = sorted(set(records) - set(PREPARE_CASES))
        if extra:
            raise Failure('unreviewed preparation-policy records: ' + ', '.join(extra))
        for case in sorted(PREPARE_CASES):
            expectation = PREPARE_CASES[case]
            record = records[case]
            problems = []
            if record.get('consumer') != self.consumer:
                problems.append(f'recorded consumer {record.get("consumer")} is not '
                                f'{self.consumer}')
            if record.get('selection') != selection:
                problems.append(f'recorded selection {record.get("selection")} is not '
                                f'{selection}')
            if bool(record.get('managerCalls')) is not expectation['managerCalls']:
                problems.append(f'manager calls {record.get("managerCalls")} are not '
                                f'{expectation["managerCalls"]}')
            if record.get('status') != expectation['status']:
                problems.append(f'status {record.get("status")} is not {expectation["status"]}')
            if record.get('inputsUnchanged') is not True:
                problems.append('the four reviewed inputs changed during preparation')
            if record.get('nodeModules') is not False:
                problems.append('a partial node_modules survived')
            if record.get('statusRecord') is not False:
                problems.append('preparation wrote a health record')
            if expectation['reason'] and expectation['reason'] not in (record.get('diagnostic')
                                                                       or ''):
                problems.append(f'diagnostic {record.get("diagnostic")!r} lacks '
                                f'{expectation["reason"]!r}')
            if problems:
                raise Failure(f'{self.consumer}/{case}: ' + '; '.join(problems))
            print(f'{self.consumer}/{case}: ' + json.dumps(
                {'status': record['status'], 'managerCalls': record['managerCalls']}), flush=True)
        print(f'{self.consumer}: preparation-policy matrix PASS (real guards and driver; the '
              'recorder shim is the only manager route)', flush=True)

    def cleanup(self):
        errors = []
        for name in list(reversed(self.containers)):
            try:
                found = self.docker_quiet('ps', '-aq', '--filter', 'name=^/' + name + '$')
                if found.returncode == 0 and found.stdout.strip():
                    removed = self.docker_quiet('rm', '-f', name)
                    if removed.returncode:
                        raise Failure(f'docker rm -f {name}: status {removed.returncode}',
                                      removed.returncode)
                self.containers.remove(name)
            except Failure as error:
                errors.append(error)
        if self.volume:
            result = self.docker_quiet('volume', 'rm', self.volume)
            if result.returncode:
                errors.append(Failure(f'docker volume rm {self.volume}: status {result.returncode}',
                                      result.returncode))
        if errors:
            raise Failure('owned cleanup failed: ' + '; '.join(str(error) for error in errors),
                          errors[0].status)


def main(args):
    try:
        image, requested, consumer, consumers = arguments(args)
    except ValueError as error:
        print(f'standard-node-policy: {error}', file=sys.stderr)
        return 2
    if consumer == 'all':
        print('migrated consumers: ' + ', '.join(consumers), flush=True)
    if consumers[0] not in MIGRATED:
        print(f'standard-node-policy: unsupported consumer set {consumers}', file=sys.stderr)
        return 2
    invocation = Invocation(image, requested, consumers[0])
    status = 0

    def interrupted(number, _frame):
        raise Failure('signal ' + str(number), 128 + number)

    for sig in [signal.SIGINT, signal.SIGTERM, signal.SIGHUP]:
        signal.signal(sig, interrupted)
    try:
        invocation.execute()
    except (Failure, OSError, ValueError, KeyError, IndexError) as error:
        print('standard-node-policy: ' + str(error), file=sys.stderr)
        status = error.status if isinstance(error, Failure) else 1
    finally:
        try:
            invocation.cleanup()
        except Failure as error:
            print(str(error), file=sys.stderr)
            if not status:
                status = error.status
    if not status:
        if invocation.consumer == 'dsh':
            print('standard-node-policy: dsh owning policy matrix PASS')
        else:
            print(f'standard-node-policy: {invocation.consumer} preparation-policy matrix PASS')
    return status


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
