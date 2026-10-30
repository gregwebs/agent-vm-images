#!/usr/bin/env python3
"""Black-box dsh-only upgrade workflow, alongside unchanged Pi/bridge tests."""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]


def composed_contract():
    """The composed helper directory: the outer context's, else a native build."""
    exported = os.environ.get('RECIPE_CONTRACT_CONTEXT')
    if exported and (Path(exported) / 'out/recipe-contract').is_file():
        return Path(exported)
    result = subprocess.run(['bash', str(ROOT / 'script/build/recipe-contract.sh'), '--native'],
                            capture_output=True, text=True)
    if result.returncode:
        raise SystemExit('cannot build the native recipe-contract helper: '
                         + (result.stderr.strip() or 'no diagnostic'))
    return Path(result.stdout.strip())
INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
OVERRIDES = {'@deepseek-ai/dsh>@deepseek-ai/cordis': '4.0.2',
             '@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-loader': '1.0.3',
             '@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer': '1.1.4',
             '@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-include': '1.0.7'}


def pins(prefix):
    (prefix / INPUTS[-1]).write_text(''.join(hashlib.sha256((prefix / name).read_bytes()).hexdigest() + '  ' + name + '\n' for name in INPUTS[:3]))


class Case:
    """One disposable copy of the dsh sources plus the fake manager/Docker tools.

    `fast_gate` substitutes a trivial consumer harness for the real focused one.
    It is used only for cases whose subject is derivation/publication ordering
    after the gate: the real harness ordering proof is asserted by the `default`
    success case and the two gate-failure cases, so repeating it in every one of
    the twenty publish/rollback positions would cost ~25 s each for no coverage.
    """

    def __init__(self, root, name, fast_gate=False):
        self.root = root / name
        self.root.mkdir()
        for source in ['images/tools/dsh', 'images/pnpm', 'script/build']:
            shutil.copytree(ROOT / source, self.root / source, ignore=shutil.ignore_patterns('__pycache__'))
        # The dsh sources are adapters over the shared project module: a scratch
        # checkout has to carry both, exactly as the Docker mount topology does.
        for shared in ('pnpm-project.py', 'pnpm-host-manager.py'):
            shutil.copyfile(ROOT / 'images/tools' / shared, self.root / 'images/tools' / shared)
        for source in ['script/test/standard-node-consumers.sh', 'script/test/standard-node-consumers.py',
                       'script/test/standard-node-dsh.py', 'script/test/host-watchdog.py']:
            destination = self.root / source
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / source, destination)
        if fast_gate:
            harness = self.root / 'script/test/standard-node-consumers.sh'
            harness.write_text('#!/usr/bin/env bash\n'
                               '# Publication-only fixture: see the fast_gate note in script/test/dsh-upgrade.py.\n'
                               'set -euo pipefail\n'
                               'printf \'fast-gate-consumers: %s\\n\' "$*"\n')
        (self.root / 'images/standard').mkdir()
        shutil.copyfile(ROOT / 'images/standard/Dockerfile', self.root / 'images/standard/Dockerfile')
        (self.root / 'pnpm').mkdir()
        shutil.copyfile(ROOT / 'images/pnpm/pin.json', self.root / 'pnpm/pin.json')
        self.prefix = self.root / 'images/tools/dsh'
        seed = ROOT / 'script/test/fixtures/pnpm-lock/derivation'
        manifest = json.loads((seed / 'package.json').read_text())
        workspace = json.loads((seed / 'pnpm-workspace.yaml').read_text())
        workspace['overrides'] = OVERRIDES
        graph = json.loads((seed / 'pnpm-lock.yaml').read_text())
        graph['overrides'] = OVERRIDES
        graph['snapshots']['declarations@1.0.0']['dependencies'] = {
            '@deepseek-ai/cordis': '4.0.2', '@deepseek-ai/cordis-plugin-loader': '1.0.3(@deepseek-ai/cordis@4.0.2)',
            '@deepseek-ai/cordis-plugin-timer': '1.1.4', '@deepseek-ai/cordis-plugin-include': '1.0.7'}
        for filename, document in [('package.json', manifest), ('pnpm-workspace.yaml', workspace), ('pnpm-lock.yaml', graph)]:
            (self.prefix / filename).write_text(json.dumps(document, indent=2) + '\n')
        pins(self.prefix)
        self.graph = self.root / 'graph.json'
        self.graph.write_text(json.dumps(graph))
        self.tools = self.root / 'bin'
        self.tools.mkdir()
        script = (ROOT / 'script/test/fixtures/dsh/upgrade-tools.py').read_text()
        native = self.tools / 'tools.py'
        native.write_text('#!' + sys.executable + '\n' + script.split('\n', 1)[1])
        native.chmod(0o755)
        for tool in ['npm', 'pnpm-fixture', 'cp', 'docker', 'python3', 'git', 'go']:
            (self.tools / tool).symlink_to('tools.py')
        # The authorized manager seam intentionally refuses symlink executables.
        (self.tools / 'pnpm-fixture').unlink()
        shutil.copyfile(native, self.tools / 'pnpm-fixture')
        (self.tools / 'pnpm-fixture').chmod(0o755)
        # A developer upgrade authorizes its composed helper directory before the
        # first graph check, so this scratch checkout carries a real one. Building it
        # here, before the fake `go` below enters PATH, keeps the helper authentic.
        self.native_contract = self.root / 'native-contract'
        shutil.copytree(composed_contract() / 'out', self.native_contract)
        # The focused consumer harness mounts a composed helper directory and execs
        # `recipe-contract` from it. A fixture stands in for the Linux cross-build,
        # so these cases neither compile Go nor mount the source-only module.
        self.contract = self.root / 'contexts' / 'linux-arm64'
        (self.contract / 'out').mkdir(parents=True)
        (self.contract / 'out/recipe-contract').write_bytes(b'fixture helper')
        (self.contract / 'out/run-install.sh').write_text('fixture run-install\n')
        self.targets = [self.prefix / name for name in INPUTS] + [self.root / 'images/standard/Dockerfile']
        self.before = [target.read_bytes() for target in self.targets]
        self.log = self.root / 'events.jsonl'
        self.state = self.root / 'docker.json'
        self.env = {key: value for key, value in os.environ.items() if not key.lower().startswith(('npm_config_', 'pnpm_config_', 'corepack_'))
                    and key not in ['NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS', 'NODE_EXTRA_CA_CERTS', 'AGENT_VM_DSH_PNPM_PACKAGE', 'AGENT_VM_FIXTURE_BASE_PNPM']}
        self.env.update(PATH=str(self.tools) + os.pathsep + os.environ['PATH'], TMPDIR=str(self.root),
                        AGENT_VM_FIXTURE_ROOT=str(self.root), AGENT_VM_FIXTURE_PNPM=str(self.tools / 'pnpm-fixture'),
                        AGENT_VM_CONTRACT_DIR=str(self.native_contract),
                        RECIPE_CONTRACT_CONTEXT=str(self.contract),
                        DSH_TEST_REPO=str(ROOT), DSH_TEST_GRAPH=str(self.graph), DSH_TEST_LOG=str(self.log),
                        DSH_TEST_TARGETS=json.dumps(list(map(str, self.targets))), AVM_FAKE_STATE=str(self.state))

    def run(self, args=(), mode='', gate='positive', publish='', rollback=''):
        self.log.write_text('')
        self.state.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        env = dict(self.env, DSH_TEST_MODE=mode, AVM_FAKE_CASE=gate, DSH_TEST_PUBLISH=str(publish), DSH_TEST_ROLLBACK=str(rollback))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['/bin/bash', str(self.prefix / 'upgrade-dsh.sh'), *args], env=env, stdout=output, stderr=output, timeout=70)
            output.seek(0)
            self.output = output.read().decode()
        self.events = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.status = result.returncode
        return self.status

    def run_signalled(self, index):
        """Start the upgrade, then deliver SIGTERM between two publication copies."""
        self.log.write_text('')
        self.state.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        env = dict(self.env, DSH_TEST_MODE='', AVM_FAKE_CASE='positive', DSH_TEST_PUBLISH='',
                   DSH_TEST_ROLLBACK='', DSH_TEST_SLEEP_PUBLISH=str(index))
        with tempfile.TemporaryFile() as output:
            process = subprocess.Popen(['/bin/bash', str(self.prefix / 'upgrade-dsh.sh'), 'next'], env=env,
                                       stdout=output, stderr=output)
            deadline = time.time() + 90
            while time.time() < deadline:
                try:
                    events = [json.loads(line) for line in self.log.read_text().splitlines()]
                except FileNotFoundError:
                    events = []
                if any(event['kind'] == 'cp' and event['value'][1] == str(self.targets[index]) for event in events):
                    break
                time.sleep(0.05)
            else:
                process.kill()
                raise AssertionError('publication never started')
            process.terminate()
            self.status = process.wait(timeout=90)
            output.seek(0)
            self.output = output.read().decode()
        self.events = [json.loads(line) for line in self.log.read_text().splitlines()]
        return self.status

    def run_backup_signalled(self, index):
        """Start the upgrade, then deliver SIGTERM while backup `index` is written.

        The fake ``cp`` writes a partial prefix at the destination it was given and
        never completes, exactly as a killed ``cp -p`` leaves it. Nothing has been
        published at that point, so the only safe outcome is unchanged sources.
        """
        self.log.write_text('')
        self.state.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        env = dict(self.env, DSH_TEST_MODE='', AVM_FAKE_CASE='positive', DSH_TEST_PUBLISH='',
                   DSH_TEST_ROLLBACK='', DSH_TEST_SLEEP_PUBLISH='', DSH_TEST_SLEEP_BACKUP=str(index))
        with tempfile.TemporaryFile() as output:
            process = subprocess.Popen(['/bin/bash', str(self.prefix / 'upgrade-dsh.sh'), 'next'], env=env,
                                       stdout=output, stderr=output)
            deadline = time.time() + 90
            while time.time() < deadline:
                try:
                    events = [json.loads(line) for line in self.log.read_text().splitlines()]
                except FileNotFoundError:
                    events = []
                if any(event['kind'] == 'cp' and '/publish-backup/' in event['value'][1] for event in events):
                    break
                time.sleep(0.05)
            else:
                process.kill()
                raise AssertionError('the backup phase never started')
            process.terminate()
            self.status = process.wait(timeout=90)
            output.seek(0)
            self.output = output.read().decode()
        self.events = [json.loads(line) for line in self.log.read_text().splitlines()]
        return self.status

    def run_window_signalled(self, publish='', rollback=''):
        """Signal after the five copies return but before the recovery decision.

        A DEBUG seam fires exactly in that window: no wall-clock delay can name it
        reliably, and the state there - sources published or partly restored,
        complete backups, ``keep_recovery`` still false - is what the EXIT trap
        would otherwise delete.
        """
        self.log.write_text('')
        self.state.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        witness = self.root / 'window'
        witness.unlink(missing_ok=True)
        seam = self.root / 'bash-env'
        seam.write_text(
            'trap \'if [ "${published-}" = 1 ] && [ "${keep_recovery-}" = false ] '
            '&& [ "${WINDOW_FIRED-}" != yes ]; then WINDOW_FIRED=yes; '
            f'printf "WINDOW work=%s publishing=%s status=%s\\n" "$work" "$publishing" "${{status-}}" >> {witness}; '
            'kill -TERM $$; fi\' DEBUG\n')
        env = dict(self.env, DSH_TEST_MODE='', AVM_FAKE_CASE='positive', DSH_TEST_PUBLISH=str(publish),
                   DSH_TEST_ROLLBACK=str(rollback), DSH_TEST_SLEEP_PUBLISH='', BASH_ENV=str(seam))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['/bin/bash', str(self.prefix / 'upgrade-dsh.sh'), 'next'], env=env,
                                    stdout=output, stderr=output, timeout=90)
            output.seek(0)
            self.output = output.read().decode()
        self.status = result.returncode
        self.events = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.window = witness.read_text().strip() if witness.exists() else ''
        return self.status

    def run_decided_signalled(self):
        """Signal after the recovery decision but before the script exits.

        The DEBUG seam fires exactly on the instruction that follows the decision
        (``decided`` is 1, ``publishing`` already cleared), so the outcome is known
        but nothing has been reported yet. That window must not exit with the
        signal status, and it must not drop ``$work``.
        """
        self.log.write_text('')
        self.state.write_text(json.dumps({'commands': [], 'containers': [], 'volume': None, 'network': None}))
        witness = self.root / 'decided'
        witness.unlink(missing_ok=True)
        seam = self.root / 'bash-env'
        seam.write_text(
            'trap \'if [ "${decided-}" = 1 ] && [ "${publishing-}" = 0 ] '
            '&& [ "${DECIDED_FIRED-}" != yes ]; then DECIDED_FIRED=yes; '
            f'printf "DECIDED work=%s status=%s publishing=%s\\n" "$work" "${{status-}}" "${{publishing-}}" >> {witness}; '
            'kill -TERM $$; fi\' DEBUG\n')
        env = dict(self.env, DSH_TEST_MODE='', AVM_FAKE_CASE='positive', DSH_TEST_PUBLISH='',
                   DSH_TEST_ROLLBACK='', DSH_TEST_SLEEP_PUBLISH='', BASH_ENV=str(seam))
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['/bin/bash', str(self.prefix / 'upgrade-dsh.sh'), 'next'], env=env,
                                    stdout=output, stderr=output, timeout=90)
            output.seek(0)
            self.output = output.read().decode()
        self.status = result.returncode
        self.events = [json.loads(line) for line in self.log.read_text().splitlines()]
        self.window = witness.read_text().strip() if witness.exists() else ''
        return self.status

    def unchanged(self):
        assert [target.read_bytes() for target in self.targets] == self.before, self.output

    def clean(self):
        state = json.loads(self.state.read_text())
        assert not state['containers'] and not state['volume'], state


def main():
    with tempfile.TemporaryDirectory(prefix='dsh-upgrade-test-') as directory:
        root = Path(directory).resolve()
        for name, args in [('default', []), ('exact', ['0.1.5-rc.1']), ('tag', ['next']), ('base', ['--pnpm','11.11.0'])]:
            # `default` proves the real focused gate runs before publication; the
            # other three are argv/selection vectors that do not spend 25 s each
            # re-running the same harness.
            case = Case(root, name, fast_gate=name != 'default')
            assert case.run(args) == 0, case.output
            selected = '0.1.5-rc.1' if name in ['exact','tag'] else '0.1.5-rc.2'
            assert json.loads(case.targets[0].read_text())['dependencies']['@deepseek-ai/dsh'] == selected
            assert '${AGENT_VERSION_DSH:-' + selected + '}' in case.targets[-1].read_text()
            assert 'org.agent-vm.version.pnpm=' not in case.targets[-1].read_text()
            assert case.targets[-1].read_bytes() == case.before[-1].replace(b'${AGENT_VERSION_DSH:-0.1.5-rc.2}', ('${AGENT_VERSION_DSH:-' + selected + '}').encode())
            assert (case.root / 'images/pnpm/pin.json').read_bytes() == (ROOT / 'images/pnpm/pin.json').read_bytes()
            events = case.events
            managers = [event['value'] for event in events if event['kind'] == 'pnpm']
            assert len(managers) == 4, managers  # two version checks, tentative, final ordinary prepare
            assert all(event['value'][0] == 'view' for event in events if event['kind'] == 'npm')
            publish = next(i for i,e in enumerate(events) if e['kind'] == 'cp' and e['value'][1] in list(map(str,case.targets)))
            if name == 'default':
                # The real focused harness consumed the staged inputs first.
                gate = next(i for i,e in enumerate(events) if e['kind'] == 'layout')
                assert gate < publish
            state = json.loads(case.state.read_text())
            uploads = [event for event in state['commands'] if event[0] == 'cp'][:4]
            assert all('/final/' in event[1] and '/tentative/' not in event[1] for event in uploads)
            case.clean()
            # Pi/bridge inputs and base are never publication destinations.
            for source in ['images/tools/pi/package.json', 'images/tools/pi/bridge/package.json']:
                assert str(case.root / source) not in [event['value'][-1] for event in events if event['kind'] == 'cp']
            print('dsh upgrade: ' + name + ' PASS')
        for name, args, expected, mode in [
            ('pnpm12', ['--pnpm','12.0.0'], 1, ''), ('pnpm-tag', ['--pnpm','latest'], 1, ''),
            ('duplicate', ['--pnpm','11.11.0','--pnpm','11.11.0'], 2, ''),
            ('missing', ['--cordis-overrides'], 2, ''), ('unknown', ['--bad'], 2, ''),
            ('unpublished', ['9.9.9'], 1, ''), ('query-failure', [], 1, 'metadata-failure'),
            ('manager12', [], 1, 'manager12'), ('peer-conflict', [], 1, 'conflict'),
            ('ambiguous', [], 1, 'ambiguous'), ('integrity', [], 1, 'integrity'),
            ('sandbox', [], 1, 'sandbox'), ('effective-peer', [], 1, 'effective-peer'),
            ('map-change', [], 1, 'changed-map'), ('backup', [], 1, 'backup-failure')]:
            case = Case(root, name)
            assert case.run(args, mode) == expected, (name, case.status, case.output)
            case.unchanged()
            if name in ['pnpm12','pnpm-tag','duplicate','missing','unknown']:
                assert not any(e['kind'] in ['npm','pnpm','docker'] for e in case.events)
            if name == 'ambiguous': assert 'ambiguous family selection' in case.output and 'pass --cordis-overrides after review' in case.output
            if name == 'unpublished': assert 'not a published version' in case.output
            if name == 'query-failure': assert 'could not query' in case.output and 'not a published' not in case.output
            case.clean()
            print('dsh upgrade: ' + name + ' PASS')
        # A no-op re-pin: a root range selects a newer timer beside the reviewed
        # 1.1.4, both satisfy every declaration, and the reviewed value is one of
        # the survivors, so the unchanged map keeps it and publishes normally.
        case = Case(root, 'repin', fast_gate=True)
        assert case.run([], 'repin') == 0, case.output
        assert 'ambiguous family selection' not in case.output
        timer = '@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer'
        assert json.loads(case.targets[2].read_text())['overrides'][timer] == '1.1.4', case.output
        print('dsh upgrade: reviewed no-op re-pin PASS')
        # The ambiguity stop advertises `--cordis-overrides`; that route must really
        # decide the graph instead of re-entering the stop. Two timer selections
        # survive (1.1.5 and 1.1.6) and the reviewed map names 1.1.6: the upgrade
        # publishes that value. A reviewed value that is not a candidate fails and
        # names the candidates instead of picking one.
        for name, reviewed, expected in [('reviewed-ambiguity', '1.1.6', 0),
                                         ('reviewed-not-a-candidate', '1.1.7', 1)]:
            case = Case(root, name, fast_gate=True)
            proposal = case.root / 'reviewed.json'
            proposal.write_text(json.dumps(dict(OVERRIDES, **{timer: reviewed})))
            assert case.run(['--cordis-overrides', str(proposal)], 'ambiguous') == expected, (name, case.status, case.output)
            if expected:
                assert 'candidates: 1.1.5, 1.1.6' in case.output, case.output
                assert 'reviewed value 1.1.7 is not one of the candidates' in case.output, case.output
                assert not any(e['kind'] == 'docker' for e in case.events)
                case.unchanged()
            else:
                assert 'ambiguous family selection' not in case.output, case.output
                # The reviewed value reached the committed workspace map. The changed
                # map is re-serialized by the Go encoder (D17: v3 style, not the
                # retired npm formatter), so the value is read through the bounded
                # parser rather than by matching a line.
                published = json.loads(subprocess.check_output(
                    [str(case.native_contract / 'recipe-contract'), 'pnpm-yaml', str(case.targets[2])], text=True))
                assert published['overrides'][timer] == '1.1.6', published['overrides']
                assert '==> pinned @deepseek-ai/dsh@0.1.5-rc.2' in case.output, case.output
            case.clean()
        print('dsh upgrade: reviewed map decides an ambiguous family PASS')
        for name, gate in [('runtime-failure','web'), ('oracle-failure','layout')]:
            case = Case(root, name)
            assert case.run(gate=gate) != 0, case.output
            case.unchanged(); case.clean()
            assert not any(e['kind'] == 'cp' and e['value'][1] in list(map(str,case.targets)) for e in case.events)
        case = Case(root, 'proposal', fast_gate=True)
        proposal = case.root / 'proposal.json'
        assert case.run(['--propose-cordis-overrides', str(proposal)], 'changed-map') == 0, case.output
        expected = dict(OVERRIDES, **{'@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer': '1.1.5'})
        assert json.loads(proposal.read_text()) == expected
        case.unchanged()
        assert not any(e['kind'] == 'docker' for e in case.events)
        assert case.run(['--cordis-overrides', str(proposal)], 'changed-map') == 0, case.output
        for name, document in [('stale', OVERRIDES), ('extra', dict(expected, unexpected='1.0.0')),
                               ('missing-map', {}), ('duplicate-map', '{"x":"1.0.0","x":"1.0.0"}'), ('range', dict(expected, **{'@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer': '^1.1.5'}))]:
            case = Case(root, name, fast_gate=True)
            proposal = case.root / 'reviewed.json'
            proposal.write_text(document if isinstance(document, str) else json.dumps(document))
            assert case.run(['--cordis-overrides', str(proposal)], 'changed-map') == 1, case.output
            case.unchanged()
        # A signal during the five-file publication restores every destination and
        # keeps the complete backup set: no half-published sources, no deleted
        # recovery. The signal lands after copies 0 and 1 and mid-copy 2.
        case = Case(root, 'publish-signal', fast_gate=True)
        assert case.run_signalled(2) == 143, (case.status, case.output)
        case.unchanged()
        assert 'publication interrupted; sources restored from the retained backups at ' in case.output
        assert '==> pinned ' not in case.output
        retained = Path(next(line.split('retained backups at ', 1)[1]
                             for line in case.output.splitlines()
                             if line.startswith('error: publication interrupted; sources restored')))
        assert retained.is_absolute() and retained.is_dir()
        for index, original in enumerate(case.before):
            assert (retained / f'publish-backup/{index}').read_bytes() == original
        assert (retained / 'final/pnpm-workspace.yaml').is_file()
        case.clean()
        print('dsh upgrade: mid-publication signal restores sources and keeps backups PASS')
        # A signal while a backup is still being written must not restore from it: a
        # killed `cp -p` leaves an incomplete file under a temporary name only, so
        # every source stays untouched while the recovery material is retained.
        case = Case(root, 'backup-signal', fast_gate=True)
        assert case.run_backup_signalled(0) == 3, (case.status, case.output)
        assert 'publication interrupted while taking backups; sources unchanged' in case.output, case.output
        case.unchanged()
        assert '==> pinned ' not in case.output
        assert not any(e['kind'] == 'cp' and '/publish-backup/' in e['value'][0] for e in case.events), \
            'an incomplete backup was restored over a source'
        retained = Path(next(line.split('recovery material retained at ', 1)[1]
                             for line in case.output.splitlines()
                             if line.startswith('error: publication interrupted while taking backups')))
        assert retained.is_absolute() and retained.is_dir()
        assert (retained / 'final/pnpm-workspace.yaml').is_file()
        assert not (retained / 'publish-backup/0').exists(), 'an incomplete backup appeared under its final name'
        case.clean()
        print('dsh upgrade: signal during the backup phase leaves sources unchanged PASS')
        # A signal in the status-3 window - every copy attempted, recovery decision
        # pending - must keep the complete backups at status 3, not delete them.
        case = Case(root, 'publish-window-3', fast_gate=True)
        assert case.run_window_signalled(publish=2, rollback=1) == 3, (case.status, case.output)
        assert 'publication result undecided when the signal arrived' in case.output, case.output
        assert 'status=3' in case.window, case.window
        assert '==> pinned ' not in case.output
        retained = Path(case.window.split('work=', 1)[1].split(' ', 1)[0])
        assert retained.is_dir(), case.window
        assert (retained / 'final/pnpm-workspace.yaml').is_file()
        for index, original in enumerate(case.before):
            assert (retained / f'publish-backup/{index}').read_bytes() == original
        # The sources really were restored: only the destination whose restore was
        # injected to fail keeps the published bytes.
        assert case.targets[0].read_bytes() == case.before[0], 'restored source 0 kept published bytes'
        assert case.targets[2].read_bytes() == case.before[2], 'restored source 2 kept published bytes'
        assert case.targets[3].read_bytes() == case.before[3] and case.targets[4].read_bytes() == case.before[4]
        assert case.targets[1].read_bytes() != case.before[1], 'the injected restore failure was ignored'
        case.clean()
        print('dsh upgrade: signal in the status-3 recovery window retains backups PASS')
        # A signal in the status-0 window leaves the completed publication in
        # place but still refuses to claim it: status 3 plus the retained recovery.
        case = Case(root, 'publish-window-0', fast_gate=True)
        assert case.run_window_signalled() == 3, (case.status, case.output)
        assert 'publication result undecided when the signal arrived' in case.output, case.output
        assert 'status=0' in case.window and 'publishing=1' in case.window, case.window
        assert '==> pinned ' not in case.output
        retained = Path(case.window.split('work=', 1)[1].split(' ', 1)[0])
        assert retained.is_dir(), case.window
        assert json.loads(case.targets[0].read_text())['dependencies']['@deepseek-ai/dsh'] == '0.1.5-rc.1'
        assert '${AGENT_VERSION_DSH:-0.1.5-rc.1}' in case.targets[-1].read_text()
        case.clean()
        print('dsh upgrade: signal in the status-0 recovery window reports status 3 PASS')
        # A signal after the decision but before the exit must not report the signal
        # status nor delete $work: the outcome is undecided for the caller, so the
        # script exits 3 with the recovery material retained.
        case = Case(root, 'decided-signal', fast_gate=True)
        assert case.run_decided_signalled() == 3, (case.status, case.output)
        assert 'publication result undecided when the signal arrived' in case.output, case.output
        assert 'status=0' in case.window and 'publishing=0' in case.window, case.window
        assert '==> pinned ' not in case.output
        retained = Path(case.window.split('work=', 1)[1].split(' ', 1)[0])
        assert retained.is_dir(), case.window
        assert (retained / 'final/pnpm-workspace.yaml').is_file()
        for index in range(5):
            assert (retained / f'publish-backup/{index}').is_file(), index
        assert json.loads(case.targets[0].read_text())['dependencies']['@deepseek-ai/dsh'] == '0.1.5-rc.1'
        case.clean()
        print('dsh upgrade: signal after the recovery decision exits 3 and keeps recovery PASS')
        case = Case(root, 'final-drift', fast_gate=True)
        proposal = case.root / 'reviewed.json'
        proposal.write_text(json.dumps(expected))
        assert case.run(['--cordis-overrides', str(proposal)], 'final-drift') == 1, case.output
        assert 'requirement drift' in case.output
        case.unchanged()
        # Each forward failure includes the partially written destination. Every
        # restore position is then independently made to fail; later restores continue.
        for index in range(5):
            for rollback in [''] + list(range(index + 1)):
                case = Case(root, f'publish-{index}-restore-{rollback}', fast_gate=True)
                expected_status = 1 if rollback == '' else 3
                assert case.run(['next'], publish=index, rollback=rollback) == expected_status, case.output
                restores = [int(Path(e['value'][0]).name) for e in case.events if e['kind'] == 'cp' and '/publish-backup/' in e['value'][0]]
                assert restores == list(range(index, -1, -1)), restores
                if expected_status == 1:
                    case.unchanged()
                else:
                    assert '==> pinned ' not in case.output
                    recovery = Path(next(line.split('retained at ',1)[1] for line in case.output.splitlines() if line.startswith('error: recovery material retained at ')))
                    assert recovery.is_absolute() and recovery.is_dir()
                    assert (recovery / 'final/pnpm-workspace.yaml').is_file()
                    for i, original in enumerate(case.before): assert (recovery / f'publish-backup/{i}').read_bytes() == original
                    reused = subprocess.run(['/bin/bash', '-c', '. "$1"; publish_transactional "$2" "$3" "$4"', '_',
                                             str(ROOT / 'script/build/transactional-publish.sh'), str(recovery / 'publish-backup'),
                                             str(recovery / 'final/package.json'), str(case.targets[0])],
                                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
                    assert reused.returncode == 1, 'reused recovery directory was accepted'
                case.clean()
            print(f'dsh upgrade: publication index {index}, inclusive rollback matrix PASS')
    print('dsh upgrade-script black-box tests passed (fake managers/Docker, not runtime acceptance)')


if __name__ == '__main__':
    main()
