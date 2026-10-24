#!/usr/bin/env python3
"""Black-box tests for the Chrome example's pinned-input maintenance updater (#41).

The updater resolves in a real parent image and verifies real Sigstore
provenance. Both are replaced here: the fixture tools assert the exact interface
(identity flags, argv, empty `GH_CONFIG_DIR`, no credential) instead of imitating
it, and one synthetic tarball digest is shared by the fake Docker, the fake curl
and the fake gh, so a lock published here is exactly the lock the build's
`check` accepts.

Every case runs the updater in a fresh empty working directory and asserts the
caller's directory stays empty, that only the five publication destinations
changed (plan B3/B6), and that no fake container is left running.
"""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
FIXTURE = ROOT / 'script/test/fixtures/chrome-mcp-upgrade/tools.py'
EXAMPLE = 'examples/layers/chrome-devtools'
INPUTS = EXAMPLE + '/chrome-devtools-mcp'
BUNDLE = EXAMPLE + '/chrome-devtools-mcp.sigstore.json'
UPDATER = 'script/dev/upgrade-chrome-devtools-mcp.sh'
SOURCES = [UPDATER, 'script/build/npm-pin.sh', 'script/build/transactional-publish.sh',
           'script/build/chrome-mcp-provenance.sh']
# Publication order; the index is also the backup name (plan B3).
DESTINATIONS = [INPUTS + '/package.json', INPUTS + '/pnpm-lock.yaml',
                INPUTS + '/pnpm-workspace.yaml', INPUTS + '/pnpm-inputs.sha256', BUNDLE]
NEXT = '9.9.9'
REGISTRY = 'https://registry.npmjs.org'


def fixture():
    spec = importlib.util.spec_from_file_location('chrome_mcp_upgrade_tools', FIXTURE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


TOOLS = fixture()


def pins_for(feeds):
    """The pin file the build's `check_pins` accepts, over `feeds`' three names."""
    return ''.join(hashlib.sha256(feeds[name]).hexdigest() + '  ' + name + '\n' for name in sorted(feeds))


def assert_lock_template():
    """The fixture's lock template is the committed lock modulo version+integrity."""
    committed = (ROOT / INPUTS / 'pnpm-lock.yaml').read_text()
    selected = json.loads((ROOT / INPUTS / 'package.json').read_text())['dependencies']['chrome-devtools-mcp']
    integrity = re.search(r'integrity: sha512-([A-Za-z0-9+/=]+)', committed).group(1)
    assert TOOLS.lock_text(selected, integrity_value=integrity) == committed, \
        'the fixture lock template no longer matches the committed lock'


class Case:
    """One disposable copy of the maintenance sources plus the fake host tools."""

    def __init__(self, root, name, state='existing'):
        self.root = root / name
        self.root.mkdir()
        self.repo = self.root / 'repo'
        for source in SOURCES:
            destination = self.repo / source
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / source, destination)
        shutil.copytree(ROOT / EXAMPLE, self.repo / EXAMPLE,
                        ignore=shutil.ignore_patterns('node_modules', '__pycache__'))
        self.inputs = self.repo / INPUTS
        self.manifest = json.loads((self.inputs / 'package.json').read_text())
        self.workspace = (self.inputs / 'pnpm-workspace.yaml').read_bytes()
        self.selected = self.manifest['dependencies']['chrome-devtools-mcp']
        self.write_committed_state(state)
        self.targets = [self.repo / name for name in DESTINATIONS]
        self.files = self.snapshot()
        self.before = self.bytes()
        self.tools = self.root / 'bin'
        self.tools.mkdir()
        native = self.tools / 'tools.py'
        native.write_text('#!' + sys.executable + '\n' + FIXTURE.read_text().split('\n', 1)[1])
        native.chmod(0o755)
        for tool in ['docker', 'gh', 'curl', 'cp']:
            (self.tools / tool).symlink_to('tools.py')
        self.log = self.root / 'events.jsonl'
        self.state_file = self.root / 'docker.json'
        self.env = {key: value for key, value in os.environ.items()
                    if key not in TOOLS.TOKEN_VARS + ['GITHUB_ACTIONS', 'BASH_ENV', 'GH_CONFIG_DIR',
                                                      'GH_PROMPT_DISABLED', 'GH_NO_UPDATE_NOTIFIER']}
        self.env.update(PATH=str(self.tools) + os.pathsep + os.environ['PATH'], TMPDIR=str(self.root),
                        AVM_CHROME_ROOT=str(self.repo), AVM_CHROME_LOG=str(self.log),
                        AVM_CHROME_STATE=str(self.state_file),
                        AVM_CHROME_CONTAINERS=str(self.root / 'containers'),
                        AVM_CHROME_WORK_ROOT=str(self.root), AVM_CHROME_VERSION=NEXT,
                        AVM_CHROME_TARGETS=json.dumps([str(target) for target in self.targets]))
        self.runs = 0
        self.output = ''

    def write_committed_state(self, state):
        """Synthetic pinned inputs: the real manifest and workspace, fixture lock."""
        lock = self.inputs / 'pnpm-lock.yaml'
        pins = self.inputs / 'pnpm-inputs.sha256'
        bundle = self.repo / BUNDLE
        if state == 'existing':
            lock.write_text(TOOLS.lock_text(self.selected))
            pins.write_text(self.computed_pins())
            bundle.write_text(TOOLS.bundle_text())
            return
        assert state == 'first', state
        for path in [lock, pins, bundle]:
            path.unlink()

    def feeds(self, version):
        """The three pinned input files the updater must publish for `version`."""
        manifest = dict(self.manifest, dependencies={'chrome-devtools-mcp': version})
        return {'package.json': (json.dumps(manifest, indent=2) + '\n').encode(),
                'pnpm-lock.yaml': TOOLS.lock_text(version).encode(),
                'pnpm-workspace.yaml': self.workspace}

    def computed_pins(self):
        return pins_for(self.feeds(self.selected))

    # --- running -----------------------------------------------------------

    def command(self, args):
        return ['/bin/bash', str(self.repo / UPDATER), *args]

    def prepare(self, settings, extra):
        self.log.write_text('')
        self.state_file.write_text(json.dumps({'containers': {}}))
        env = dict(self.env, **(extra or {}))
        for key, value in settings.items():
            env['AVM_CHROME_' + key.upper()] = str(value)
        self.runs += 1
        cwd = self.root / ('cwd%d' % self.runs)
        cwd.mkdir()
        return env, cwd

    def settle(self, cwd):
        left = list(cwd.iterdir())
        assert not left, 'the updater wrote into the caller working directory: %s' % left
        assert 'Traceback' not in self.output, 'a fixture tool failed instead of the updater'

    def run(self, args=(), extra=None, **settings):
        env, cwd = self.prepare(settings, extra)
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(self.command(args), cwd=cwd, env=env, stdout=output,
                                    stderr=output, timeout=240)
            output.seek(0)
            self.output = output.read().decode()
        self.status = result.returncode
        self.settle(cwd)
        return self.status

    def signalled(self, args, trigger, number, extra=None, **settings):
        """Deliver `number` once the fixture reports `trigger`."""
        env, cwd = self.prepare(settings, extra)
        with tempfile.TemporaryFile() as output:
            process = subprocess.Popen(self.command(args), cwd=cwd, env=env, stdout=output,
                                       stderr=output)
            deadline = time.time() + 120
            while not trigger(self.events):
                if time.time() > deadline or process.poll() is not None:
                    process.kill()
                    raise AssertionError('the run never reached the signal point')
                time.sleep(0.05)
            process.send_signal(number)
            self.status = process.wait(timeout=240)
            output.seek(0)
            self.output = output.read().decode()
        self.settle(cwd)
        return self.status

    def seam(self, args, state, **run_settings):
        """Signal once the updater's own variables hold `state` (the DEBUG seam)."""
        witness = self.root / 'seam'
        seam = self.root / 'bash-env'
        condition = ' && '.join('[ "${%s-}" = %s ]' % (name, value) for name, value in state.items())
        seam.write_text('trap \'if ' + condition + ' && [ "${SEAM_FIRED-}" != yes ]; then '
                        'SEAM_FIRED=yes; printf "SEAM work=%s status=%s publishing=%s published=%s '
                        'decided=%s\\n" "$work" "${status-}" "${publishing-}" "${published-}" '
                        '"${decided-}" >> ' + str(witness) + '; kill -TERM $$; fi\' DEBUG\n')
        status = self.signalled(args, lambda events: witness.exists(), signal.SIGTERM,
                                extra={'BASH_ENV': str(seam)}, **run_settings)
        self.seam_state = witness.read_text().strip()
        return status

    # --- fixture observations ---------------------------------------------

    @property
    def events(self):
        try:
            return [json.loads(line) for line in self.log.read_text().splitlines() if line.strip()]
        except FileNotFoundError:
            return []

    def containers(self):
        return json.loads(self.state_file.read_text())['containers'] if self.state_file.exists() else {}

    def snapshot(self):
        return {str(path.relative_to(self.repo)): path.read_bytes() for path in self.repo.rglob('*')
                if path.is_file() and not path.is_symlink()}

    def bytes(self):
        return [target.read_bytes() if target.exists() else None for target in self.targets]

    def changed(self):
        after = self.snapshot()
        return {name for name in set(after) | set(self.files) if after.get(name) != self.files.get(name)}

    def remove(self, *names):
        for name in names:
            (self.repo / name).unlink()
        self.files = self.snapshot()
        self.before = self.bytes()

    def backup_started(self):
        return lambda events: any(event['kind'] == 'cp' and 'publish-backup/' in event['value'][1]
                                  for event in events)

    def publication(self, index):
        return lambda events: any(event['kind'] == 'cp' and event['value'][1] == str(self.targets[index])
                                  for event in events)

    def resolve_started(self):
        return lambda events: any(event['kind'] == 'docker' and event['value'][:1] == ['start']
                                  for event in events)

    def docker_calls(self, *verbs):
        return [event['value'] for event in self.events
                if event['kind'] == 'docker' and event['value'][:len(verbs)] == list(verbs)]

    # --- expectations ------------------------------------------------------

    def published_only(self):
        assert self.changed() <= set(DESTINATIONS), 'unexpected change: %s' % sorted(self.changed())

    def unchanged(self):
        assert self.changed() == set(), 'sources changed: %s\n%s' % (sorted(self.changed()), self.output)

    def assert_published(self, version):
        feeds = self.feeds(version)
        assert (self.repo / DESTINATIONS[0]).read_bytes() == feeds['package.json'], self.output
        assert (self.repo / DESTINATIONS[1]).read_bytes() == feeds['pnpm-lock.yaml'], self.output
        assert (self.repo / DESTINATIONS[2]).read_bytes() == feeds['pnpm-workspace.yaml'], self.output
        assert (self.repo / DESTINATIONS[3]).read_text() == pins_for(feeds), self.output
        assert (self.repo / DESTINATIONS[4]).read_text() == TOOLS.bundle_text(), self.output
        self.published_only()

    def check(self, directory=None):
        return subprocess.run(['/bin/sh', str(self.repo / EXAMPLE / 'chrome-devtools-mcp.sh'), 'check',
                               str(directory or self.inputs)],
                              cwd=str(self.root), env=dict(self.env), capture_output=True, text=True)

    def no_work(self):
        left = [path for path in self.root.glob('chrome-mcp-upgrade.*') if path.is_dir()]
        assert not left, 'the recovery directory was not removed: %s' % left

    def retained(self):
        phrase = 'recovery material retained at '
        lines = [line for line in self.output.splitlines() if phrase in line]
        assert lines, 'no recovery material was reported:\n' + self.output
        path = Path(lines[0].split(phrase, 1)[1].strip())
        assert path.is_dir(), 'retained path is not a directory: %s' % path
        assert path.parent == self.root, 'retained path is not the invocation work dir: %s' % path
        return path

    def clean(self):
        live = [cid for cid, entry in self.containers().items() if entry['live']]
        assert not live, 'a fake container is still running: %s' % live

    def restores(self):
        return [int(Path(event['value'][0]).name) for event in self.events
                if event['kind'] == 'cp' and 'publish-backup/' in event['value'][0]]


def retained_backups(case, path):
    assert (path / 'publish-backup' / '.complete').is_file(), 'the backup set is not marked complete'
    for index, original in enumerate(case.before):
        if original is None:
            assert not (path / 'publish-backup' / str(index)).exists(), \
                'a backup was taken of an originally absent file'
            continue
        assert (path / 'publish-backup' / str(index)).read_bytes() == original, 'backup %d differs' % index


# --- cases -----------------------------------------------------------------

def report(identifier, description):
    print('chrome-mcp upgrade: ' + identifier + ' ' + description + ' PASS')


def t1(root):
    case = Case(root, 't1')
    assert case.run([NEXT]) == 0, case.output
    case.assert_published(NEXT)
    checked = case.check()
    assert checked.returncode == 0 and checked.stdout.strip() == NEXT, checked
    staged = [event['value'][0] for event in case.events if event['kind'] == 'cp']
    assert any(source.endswith('/stage/pnpm-inputs.sha256') for source in staged), staged
    case.no_work()
    case.clean()
    report('T1', 'publishes the resolved proposal, the staged pins and removes the work dir')


def t2(root):
    case = Case(root, 't2', state='first')
    assert case.run(['1.0.1'], version='1.0.1', lock='same') == 0, case.output
    case.assert_published('1.0.1')
    assert case.check().stdout.strip() == '1.0.1'
    case.no_work()
    case.clean()
    report('T2', 'first adoption creates lock, pins and bundle')


def t3(root):
    case = Case(root, 't3')
    assert case.run([]) == 0, case.output
    discovery = [event['value'][-1] for event in case.events if event['kind'] == 'curl']
    assert REGISTRY + '/chrome-devtools-mcp' in discovery, discovery
    case.assert_published(NEXT)
    case.no_work()
    case.clean()
    report('T3', 'no argument follows dist-tags.latest')


def t4(root):
    case = Case(root, 't4')
    assert case.run(['1.0.1'], version='1.0.1', lock='same') == 0, case.output
    assert 'unchanged (re-resolved and re-verified; nothing published)' in case.output, case.output
    case.unchanged()
    assert not [event for event in case.events
                if event['kind'] == 'cp' and 'publish-backup/' in event['value'][1]], \
        'a backup was taken for an unchanged selection'
    case.no_work()
    case.clean()
    report('T4', 'unchanged selection publishes nothing and takes no backup')


def t5(root):
    case = Case(root, 't5')
    case.remove(INPUTS + '/pnpm-lock.yaml')
    assert case.run([NEXT]) == 1, case.output
    assert 'partial pinned state; restore the committed files first' in case.output, case.output
    case.unchanged()
    assert not case.docker_calls('create'), 'a partial pinned state reached the container step'
    case.no_work()
    case.clean()
    report('T5', 'partial pinned state refused')


def t6(root):
    case = Case(root, 't6')
    assert case.run(['^1.0.1']) == 1, case.output
    assert 'VERSION must be one canonical exact version' in case.output, case.output
    case.unchanged()
    # Only the architecture probe precedes version validation (plan steps 3 and 5).
    assert not [value for value in case.docker_calls() if value[:1] != ['version']], case.events
    case.no_work()
    case.clean()
    report('T6', 'version range refused before any container work')


def resolve_failure(root, identifier, description, mode, detail):
    case = Case(root, identifier)
    assert case.run([NEXT], start=mode) == 1, case.output
    assert 'resolve failed' in case.output and detail in case.output, case.output
    case.unchanged()
    assert case.docker_calls('rm', '-f'), 'the resolve container was not removed'
    case.no_work()
    case.clean()
    report(identifier.upper(), description)


def t9(root):
    case = Case(root, 't9')
    assert case.run([NEXT], lock='two-packages') == 1, case.output
    assert 'closure shape changed; review required' in case.output, case.output
    case.unchanged()
    case.no_work()
    case.clean()
    report('T9', 'two-package closure refused')


def t10(root):
    case = Case(root, 't10')
    assert case.run([NEXT], tarball='bad') == 1, case.output
    assert 'does not match the lock' in case.output, case.output
    case.unchanged()
    case.no_work()
    case.clean()
    report('T10', 'tarball that is not the locked one refused')


def attestations_case(root, identifier, description, mode, message):
    case = Case(root, identifier)
    assert case.run([NEXT], attestations=mode) == 1, case.output
    assert message in case.output, case.output
    case.unchanged()
    case.no_work()
    case.clean()
    report('T11', description)


def gh_case(root, identifier, description, mode, message):
    case = Case(root, identifier)
    assert case.run([NEXT], gh=mode) == 1, case.output
    assert message in case.output, case.output
    assert 'hash-only exception' not in case.output, 'a hash-only fallback was offered: ' + case.output
    assert [event for event in case.events if event['kind'] == 'gh-verified'], \
        'the fake gh never checked the argv it was given'
    case.unchanged()
    case.no_work()
    case.clean()
    report('T12', description)


def t13(root):
    case = Case(root, 't13')
    assert case.run([NEXT], extra={'GITHUB_ACTIONS': 'true'}) == 1, case.output
    assert 'refusing GitHub Actions' in case.output, case.output
    case.unchanged()
    assert not case.docker_calls(), 'CI was refused only after a docker call'
    case.no_work()
    case.clean()
    report('T13', 'GitHub Actions refused without any docker call')


def t14(root):
    for index in [0, 2, 4]:
        case = Case(root, 't14-%d' % index)
        assert case.run([NEXT], publish=str(index)) == 1, case.output
        assert 'restoring attempted files inclusively' in case.output, case.output
        case.unchanged()
        assert case.restores() == list(range(index, -1, -1)), case.restores()
        case.no_work()
        case.clean()
        report('T14', 'publication failure at destination %d restores every destination' % index)
    case = Case(root, 't14-first', state='first')
    assert case.run(['1.0.1'], version='1.0.1', lock='same', publish='3') == 1, case.output
    case.unchanged()
    for index in [1, 3, 4]:
        assert not case.targets[index].exists(), 'a first-adoption destination survived the rollback'
    case.no_work()
    case.clean()
    report('T14', 'publication failure at destination 3 of a first adoption restores every destination')


def t15(root):
    case = Case(root, 't15')
    assert case.run([NEXT], publish='2', rollback='1') == 3, case.output
    retained = case.retained()
    retained_backups(case, retained)
    assert case.targets[1].read_bytes() != case.before[1], 'the injected restore failure was ignored'
    for index in [0, 2, 3, 4]:
        assert case.targets[index].read_bytes() == case.before[index], index
    case.published_only()
    case.clean()
    report('T15', 'incomplete recovery retains the backups at status 3')


def s1(root):
    case = Case(root, 's1')
    assert case.signalled([NEXT], case.backup_started(), signal.SIGTERM, sleep_backup='0') == 3, case.output
    assert 'interrupted while taking backups; sources unchanged' in case.output, case.output
    case.unchanged()
    retained = case.retained()
    assert not (retained / 'publish-backup' / '0').exists(), 'a partial backup appeared under its final name'
    assert (retained / 'publish-backup' / '.tmp0').is_file(), 'the interrupted backup was not left partial'
    assert not case.restores(), 'an incomplete backup was restored over a source'
    assert (retained / 'stage' / 'package.json').is_file(), 'the proposal was not retained'
    case.clean()
    report('S1', 'signal during the backup phase leaves sources unchanged')


def s2(root):
    for index in [1, 4]:
        case = Case(root, 's2-%d' % index)
        assert case.signalled([NEXT], case.publication(index), signal.SIGTERM,
                              sleep_publish=str(index)) == 143, case.output
        assert 'publication interrupted; sources restored' in case.output, case.output
        case.unchanged()
        retained_backups(case, case.retained())
        case.clean()
        report('S2', 'signal during publication of destination %d restores sources' % index)
    case = Case(root, 's2-first', state='first')
    assert case.signalled(['1.0.1'], case.publication(4), signal.SIGTERM, version='1.0.1',
                          lock='same', sleep_publish='4') == 143, case.output
    case.unchanged()
    for index in [1, 3, 4]:
        assert not case.targets[index].exists(), 'an originally absent file survived the restore'
    retained_backups(case, case.retained())
    case.clean()
    report('S2', 'signal during publication of destination 4 of a first adoption restores sources')


def s2_signal(root, identifier, label, description, number, expected):
    case = Case(root, identifier)
    assert case.signalled([NEXT], case.publication(2), number, sleep_publish='2') == expected, case.output
    case.unchanged()
    retained_backups(case, case.retained())
    case.clean()
    report(label, description)


def s3(root):
    case = Case(root, 's3')
    status = case.seam([NEXT], {'published': 1, 'publishing': 1, 'decided': 0})
    assert status == 3, case.output
    assert 'publication result undecided when the signal arrived' in case.output, case.output
    assert 'publishing=1 published=1 decided=0' in case.seam_state, case.seam_state
    assert json.loads(case.targets[0].read_text())['dependencies']['chrome-devtools-mcp'] == NEXT
    case.published_only()
    retained_backups(case, Path(case.seam_state.split('work=', 1)[1].split(' ', 1)[0]))
    case.clean()
    report('S3', 'signal after a successful return reports status 3 and keeps new files')


def s4(root):
    case = Case(root, 's4')
    status = case.seam([NEXT], {'published': 1, 'publishing': 1, 'decided': 0}, publish='2', rollback='1')
    assert status == 3, case.output
    assert 'status=3' in case.seam_state, case.seam_state
    retained = Path(case.seam_state.split('work=', 1)[1].split(' ', 1)[0])
    retained_backups(case, retained)
    # The signal path re-attempts the whole inclusive restore, not just the
    # positions publish_transactional already rolled back (0..2).
    assert {3, 4} <= set(case.restores()), case.restores()
    assert case.targets[1].read_bytes() != case.before[1], 'the injected restore failure was ignored'
    assert case.targets[0].read_bytes() == case.before[0]
    case.published_only()
    case.clean()
    report('S4', 'signal over an incomplete rollback re-attempts the whole restore')


def s5(root):
    case = Case(root, 's5')
    status = case.seam([NEXT], {'decided': 1, 'publishing': 0})
    assert status == 3, case.output
    assert 'publication result undecided when the signal arrived' in case.output, case.output
    assert 'publishing=0' in case.seam_state and 'decided=1' in case.seam_state, case.seam_state
    case.published_only()
    retained_backups(case, Path(case.seam_state.split('work=', 1)[1].split(' ', 1)[0]))
    case.clean()
    report('S5', 'signal after the decision keeps the recovery material at status 3')


def s6(root):
    case = Case(root, 's6')
    assert case.signalled([NEXT], case.resolve_started(), signal.SIGTERM, start='sleep') == 143, case.output
    case.unchanged()
    assert case.docker_calls('rm', '-f'), 'the resolve container was not removed'
    case.no_work()
    case.clean()
    report('S6', 'signal during resolve removes the container and the work dir')


def main():
    assert_lock_template()
    with tempfile.TemporaryDirectory(prefix='chrome-mcp-upgrade-test-') as directory:
        root = Path(directory).resolve()
        for run in [t1, t2, t3, t4, t5, t6, t9, t10, t13, t14, t15, s1, s2, s3, s4, s5, s6]:
            run(root)
        resolve_failure(root, 't7', 'a resolve that exits 1 leaves sources unchanged and removes the container',
                        'exit1', '(status 1, container 1)')
        resolve_failure(root, 't8', 'a resolve that is killed leaves sources unchanged and removes the container',
                        'exit124', '(status 124, container 124)')
        attestations_case(root, 't11-zero', 'a zero-SLSA attestation response is refused', 'zero',
                          'expected exactly one SLSA provenance attestation for chrome-devtools-mcp@%s, found 0' % NEXT)
        attestations_case(root, 't11-two', 'two SLSA attestations are refused', 'two', 'found 2')
        attestations_case(root, 't11-none', 'a missing attestation is refused with no hash-only fallback',
                          'none', 'no SLSA provenance for chrome-devtools-mcp@%s' % NEXT)
        gh_case(root, 't12-fail', 'a failing gh verification is refused without any fallback', 'fail',
                'provenance does not verify under the trusted identity')
        gh_case(root, 't12-subject', 'a verified statement for another artifact is refused', 'wrong-subject',
                'verified statement does not name the locked tarball')
        s2_signal(root, 's2-int', 'S2i', 'INT during publication restores sources', signal.SIGINT, 130)
        s2_signal(root, 's2-hup', 'S2h', 'HUP during publication restores sources', signal.SIGHUP, 129)
    print('chrome-mcp upgrade black-box tests passed (fixture Docker/registry, not runtime acceptance)')


if __name__ == '__main__':
    main()
