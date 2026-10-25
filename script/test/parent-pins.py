#!/usr/bin/env python3
"""Black-box tests for the maintained parent-pin resolver and publisher (#95).

Every case runs the real `script/dev/parent_pins.py` and the real
`script/dev/refresh-parent-pins.sh` inside a disposable Git checkout. Only the
three external seams are replaced by `script/test/fixtures/parent-pins/tools.py`
(docker, gh, curl); the fixture bytes under `script/test/fixtures/parent-pins/`
are the actual registry indexes, child manifests, parent apt policy and v0.1.3
release metadata, so the parsing, source policy and historical release adapter
under test are the ones a maintainer runs. No case touches the network, the real
checkout, or a real container.
"""
from __future__ import annotations

import datetime
import hashlib
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / 'script/test/fixtures/parent-pins'
PUBLISHER = 'script/dev/refresh-parent-pins.sh'
STAGER = 'script/dev/parent_pins.py'
INVENTORY = 'images/parent-pins.json'
POLICY_FILES = ('images/apt-policy/debian.sources', 'images/apt-policy/keyrings.sha256')
LAYERS_README = 'examples/layers/README.md'
LAYER_RECIPES = ['examples/layers/chrome-devtools/Dockerfile',
                 'examples/layers/go-dev/Dockerfile',
                 'examples/layers/rust-dev/Dockerfile',
                 'examples/layers/wirenboard-cpp/Dockerfile']
BASE_DOCKERFILE = 'images/Dockerfile'
STANDARD_DOCKERFILE = 'images/standard/Dockerfile'
# The repository base identity is derived from the staged recipes, so every
# selection that moves a local parent republishes it; a release refresh does not.
BASE_MANIFEST = 'images/base-inputs.sha256'
EXTENSION = 'examples/base-extension/Dockerfile'
LOCAL_IDENTITY = [BASE_MANIFEST, EXTENSION]
BOOTSTRAP_DESTINATIONS = sorted([
    INVENTORY, *POLICY_FILES, LAYERS_README, BASE_DOCKERFILE, STANDARD_DOCKERFILE,
    *LOCAL_IDENTITY, *LAYER_RECIPES])
DEBIAN_DESTINATIONS = sorted([INVENTORY, *POLICY_FILES, BASE_DOCKERFILE, *LOCAL_IDENTITY])
RELEASE_DESTINATIONS = sorted([
    INVENTORY, LAYERS_README, *LAYER_RECIPES])
# A disposable checkout is assembled from exactly the tracked files the tooling
# reads; it is not a copy of the maintained tree, so a test can never write into it.
STAGED = {
    # `images/apt-policy/` is deliberately absent: bootstrap is the only refresh
    # that creates it, and T8c/T12 start from that state.
    'images': ['Dockerfile', 'standard/Dockerfile', '.dockerignore', 'install-artifact.sh',
               'install-build-ca.sh', 'install-pnpm.sh', 'install-zellij.sh',
               'verify-base-inputs.sh', 'base-inputs.sha256',
               'artifacts/artifact_pin.py', 'artifacts/install_artifact.py',
               'artifacts/read-pin-version.py', 'artifacts/node.json', 'artifacts/gh.json',
               'artifacts/node-release-key.asc',
               'artifacts/gh-2.103.2-linux-amd64.bundle.jsonl',
               'artifacts/gh-2.103.2-linux-arm64.bundle.jsonl',
               'pnpm/pin.json', 'pnpm/verify-pnpm.sh', 'recipe-contract/go.mod'],
    'examples': ['base-extension/Dockerfile', 'base-extension/check-base-identity.sh',
                 'layers/README.md',
                 'layers/chrome-devtools/Dockerfile', 'layers/go-dev/Dockerfile',
                 'layers/rust-dev/Dockerfile', 'layers/wirenboard-cpp/Dockerfile'],
    'script': ['dev/parent_pins.py', 'dev/refresh-parent-pins.sh',
               'build/base-inputs.sh', 'build/transactional-publish.sh'],
}
RELEASED = 'ghcr.io/gregwebs/agent-vm-standard'
INVOCATION = 'https://github.com/gregwebs/agent-vm-images/actions/runs/37775269427/attempts/1'
SOURCE_SHA = '087f8bad3de624a5dad38f1669e7dea96996371a'
RELEASE_FIXTURE = 'release/release.json'
DEBIAN_INDEX = 'registry/debian-index.json'
GO_ARM64_INDEX_ENTRY = 4


def sha256_bytes(raw):
    return hashlib.sha256(raw).hexdigest()


def digest(path):
    return 'sha256:' + sha256_bytes(Path(path).read_bytes())


def module():
    """Import the real resolver in-process for the pure parser cases."""
    spec = importlib.util.spec_from_file_location('parent_pins_under_test', ROOT / STAGER)
    loaded = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which a
    # bare module_from_spec does not populate.
    sys.modules['parent_pins_under_test'] = loaded
    spec.loader.exec_module(loaded)
    return loaded


def replace(path, old, new):
    text = Path(path).read_text()
    assert old in text, '%s no longer contains %r' % (path, old)
    Path(path).write_text(text.replace(old, new, 1))


def edit_json(path, transform):
    value = json.loads(Path(path).read_bytes())
    transform(value)
    Path(path).write_text(json.dumps(value, indent=1) + '\n')


class Case:
    """One disposable checkout with fake host tools and fixture registry state."""

    def __init__(self, root, name):
        self.root = root / name
        self.root.mkdir()
        self.repo = self.root / 'repo'
        self.cwd = self.root / 'cwd'
        self.cwd.mkdir()
        for directory, entries in STAGED.items():
            for entry in entries:
                destination = self.repo / directory / entry
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(ROOT / directory / entry, destination)
        shutil.copytree(ROOT / 'script/release', self.repo / 'script/release',
                        ignore=shutil.ignore_patterns('__pycache__'))
        # Every case starts from the pre-adoption recipe shape: the committed
        # tree already carries the pin, and adoption is what the cases exercise.
        base = self.repo / BASE_DOCKERFILE
        base.write_text(re.sub(r'FROM debian:13-slim@sha256:[0-9a-f]{64}',
                               'FROM debian:13-slim', base.read_text()))
        self.fixtures = self.root / 'fixtures'
        for name in ('registry', 'apt', 'release'):
            shutil.copytree(FIXTURES / name, self.fixtures / name)
        self.tools = self.root / 'bin'
        self.tools.mkdir()
        native = self.tools / 'tools.py'
        native.write_text('#!' + sys.executable + '\n'
                          + (FIXTURES / 'tools.py').read_text().split('\n', 1)[1])
        native.chmod(0o755)
        for tool in ('docker', 'gh', 'curl', 'cp'):
            (self.tools / tool).symlink_to('tools.py')
        self.specs = {
            'debian': {'tag': 'debian:13-slim', 'repository': 'debian', 'index': DEBIAN_INDEX,
                       'children': {'amd64': 'registry/debian-amd64.json',
                                    'arm64': 'registry/debian-arm64.json'},
                       'apt': {'amd64': 'apt', 'arm64': 'apt'}},
            'golang': {'tag': 'golang:1.27.3-trixie', 'repository': 'golang',
                       'index': 'registry/golang-index.json',
                       'children': {'amd64': 'registry/golang-amd64.json',
                                    'arm64': 'registry/golang-arm64.json'}},
            'standard': {'tag': RELEASED, 'repository': RELEASED,
                         'index': 'registry/standard-index.json',
                         'children': {'amd64': 'registry/standard-amd64.json',
                                      'arm64': 'registry/standard-arm64.json'}},
        }
        self.override = {}
        # The registry serves a digest reference from content-addressed storage
        # regardless of the tag, so keep the committed Go index available by its
        # own digest even when a case mutates the tag's bytes.
        self.go_committed = re.search(r'golang:[^ ]+@sha256:[0-9a-f]{64}',
                                      (self.repo / BASE_DOCKERFILE).read_text()).group(0)
        self.go_committed_digest = 'golang@sha256:' + self.go_committed.rsplit('@sha256:', 1)[1]
        self.go_pristine = self.root / 'registry-committed'
        self.go_pristine.mkdir()
        shutil.copyfile(self.fixtures / 'registry/golang-index.json',
                        self.go_pristine / 'golang-index.json')
        self.release_json = self.fixtures / RELEASE_FIXTURE
        self.attest = {
            'repository': 'gregwebs/agent-vm-images',
            'workflow': 'gregwebs/agent-vm-images/.github/workflows/release-standard.yml',
            'source_sha': SOURCE_SHA, 'invocation': INVOCATION,
            'release_json_sha256': sha256_bytes(self.release_json.read_bytes()),
            'index_digest': sha256_bytes(
                (self.fixtures / 'registry/standard-index.json').read_bytes())}
        self.failures = {}
        self.hooks = []
        self.write_state()
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'user.name', 'Fixture')
        self.git('config', 'core.fsmonitor', 'false')
        self.git('config', 'commit.gpgsign', 'false')
        self.git('add', '-A')
        self.git('commit', '-qm', 'baseline')
        self.before = self.snapshot()
        self.status = None
        self.output = ''

    # --- fixture state -----------------------------------------------------

    def fixture(self, relative):
        return self.fixtures / relative

    def write_state(self):
        registry, images = {}, dict(self.override)
        for spec in self.specs.values():
            index = self.fixture(spec['index'])
            registry[spec['tag']] = str(index)
            registry[spec['repository'] + '@' + digest(index)] = str(index)
            try:
                parsed = json.loads(index.read_bytes())
            except ValueError:
                parsed = {}
            for descriptor in parsed.get('manifests', []):
                platform = descriptor.get('platform') or {}
                architecture = platform.get('architecture')
                if platform.get('os') != 'linux' or architecture not in ('amd64', 'arm64'):
                    continue
                child = self.fixture(spec['children'][architecture])
                registry[spec['repository'] + '@' + descriptor['digest']] = str(child)
                if spec.get('apt'):
                    images[spec['repository'] + '@' + descriptor['digest']] = {
                        '/etc/apt': str(self.fixture(spec['apt'][architecture] + '/etc/apt')),
                        '/usr/share/keyrings': str(self.fixture(spec['apt'][architecture]
                                                              + '/usr/share/keyrings'))}
        registry.update(self.override)
        registry.setdefault(self.go_committed_digest,
                            str(self.go_pristine / 'golang-index.json'))
        (self.root / 'state.json').write_text(json.dumps({
            'registry': registry, 'images': images,
            'downloads': str(self.fixtures / 'release'), 'attest': self.attest,
            'failures': self.failures, 'hooks': self.hooks}, indent=1) + '\n')

    def mutate(self, relative, transform):
        transform(self.fixture(relative))
        self.write_state()
        return self.fixture(relative)

    # --- git ---------------------------------------------------------------

    def git(self, *arguments):
        return subprocess.run(['git', '-C', str(self.repo), *arguments], capture_output=True,
                              text=True, check=False)

    def commit(self, message='update'):
        self.git('add', '-A')
        self.git('commit', '-qm', message)
        self.before = self.snapshot()

    def render_base_inputs(self):
        """Regenerate the committed base identity, as the documented edit flow does.

        A hand edit of a listed input makes the manifest stale by definition; the
        maintainer's repair is `--write`, not a parent-pin refresh that happens to
        notice. T17 goes through it so its refreshes start from a green tree.
        """
        result = subprocess.run(['/bin/bash', 'script/build/base-inputs.sh', '--write'],
                                cwd=str(self.repo), env=self.environment(),
                                capture_output=True, text=True, check=False)
        assert result.returncode == 0, result.stdout + result.stderr

    # --- running -----------------------------------------------------------

    def environment(self, extra=None):
        env = {key: value for key, value in os.environ.items()
               if key not in ('GH_TOKEN', 'GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN',
                              'GITHUB_ENTERPRISE_TOKEN', 'GH_CONFIG_DIR', 'GITHUB_ACTIONS',
                              'BASH_ENV', 'GH_PROMPT_DISABLED', 'GH_NO_UPDATE_NOTIFIER')}
        env.update(PATH=str(self.tools) + os.pathsep + os.environ['PATH'],
                   TMPDIR=str(self.root), HOME=str(self.root))
        env.update(extra or {})
        return env

    def prepare_run(self):
        """Event numbering and the event log are per run, so hooks are exact."""
        (self.root / 'events.count').write_text('0')
        (self.root / 'events.jsonl').write_text('')

    def run(self, arguments, env=None, script=None):
        self.prepare_run()
        with tempfile.TemporaryFile() as output:
            result = subprocess.run(['/bin/bash', str(script or (self.repo / PUBLISHER)),
                                     *arguments],
                                    cwd=str(self.cwd), env=self.environment(env), stdout=output,
                                    stderr=output, timeout=900)
            output.seek(0)
            self.output = output.read().decode()
        self.status = result.returncode
        assert not list(self.cwd.iterdir()), 'the publisher wrote into the caller directory'
        return self.status

    # --- observations ------------------------------------------------------

    @property
    def events(self):
        try:
            return [json.loads(line) for line in
                    (self.root / 'events.jsonl').read_text().splitlines() if line.strip()]
        except FileNotFoundError:
            return []

    def containers(self):
        path = self.root / 'containers.json'
        return json.loads(path.read_text()) if path.exists() else {}

    def files(self):
        return {path.relative_to(self.repo).as_posix(): path.read_bytes()
                for path in self.repo.rglob('*')
                if path.is_file() and not path.is_symlink() and '.git' not in path.parts}

    def snapshot(self):
        self.before = self.files()
        return self.before

    def changed(self):
        after = self.files()
        return {name for name in set(after) | set(self.before)
                if after.get(name) != self.before.get(name)}

    def published_only(self, expected):
        assert self.changed() <= set(expected), 'unexpected change: %s\n%s' % (
            sorted(self.changed() - set(expected)), self.output)

    def unchanged(self):
        assert self.changed() == set(), 'sources changed: %s\n%s' % (
            sorted(self.changed()), self.output)

    def no_work(self, retained=False):
        leftover = [path for path in self.root.glob('refresh-parent-pins.*') if path.is_dir()]
        if retained:
            assert leftover, 'no recovery material was retained'
        else:
            assert not leftover, 'the work directory was not removed: %s' % leftover
        assert not (self.repo / '.git/parent-pins.lock').exists(), 'the serialization lock leaked'

    def retained(self):
        phrase = 'recovery material retained at '
        lines = [line for line in self.output.splitlines() if phrase in line]
        assert lines, 'no retention was reported:\n' + self.output
        path = Path(lines[0].split(phrase, 1)[1].strip())
        assert path.is_dir(), 'the retained path is not a directory: %s' % path
        assert path.parent.resolve() == self.root.resolve(), \
            'the retained path is not the invocation work dir'
        return path

    def recover_path(self):
        """The status-3 work directory, as the STOP instruction prints it."""
        phrase = 'STOP: recover '
        lines = [line for line in self.output.splitlines() if phrase in line]
        assert lines, 'no STOP instruction was printed:\n' + self.output
        path = Path(lines[0].split(phrase, 1)[1].split(' before', 1)[0])
        assert path.is_dir(), 'the recovered path is not a directory: %s' % path
        assert path.parent.resolve() == self.root.resolve(), \
            'the recovered path is not the invocation work dir'
        return path

    def flag(self, rule):
        """Inject a copy fault or a signal; see fixtures/parent-pins/tools.py."""
        (self.root / 'fail-copy').write_text(rule if isinstance(rule, str) else
                                            json.dumps(rule))

    def publish_release(self, version):
        """Publish a new released-standard version over the same children.

        New index bytes move the digest; the release metadata is edited and the
        fake attestation is re-bound to it, so this drives a *real* version
        refresh instead of a byte-level signature failure.
        """
        index = self.mutate('registry/standard-index.json', lambda path: edit_json(
            path, lambda value: value.update({'annotations': {'fixture': 'v' + version}})))
        reference = RELEASED + '@' + digest(index)
        self.tamper_marker(RELEASE_FIXTURE, lambda value: value.update(
            {'version': version, 'release_tag': 'v' + version,
             'index_digest': reference.split('@', 1)[1]}))
        self.attest['index_digest'] = sha256_bytes(index.read_bytes())
        self.write_state()
        return reference

    def live_containers(self):
        return [identifier for identifier, entry in self.containers().items() if entry['live']]

    def inventory(self):
        return json.loads((self.repo / INVENTORY).read_text())

    # --- scenarios ---------------------------------------------------------

    def bootstrap(self):
        assert self.run(['--bootstrap']) == 0, self.output
        self.published_only(BOOTSTRAP_DESTINATIONS)

    def adopt(self):
        self.bootstrap()
        self.commit('adopt parent pins')
        self.unchanged()

    def shift_debian(self):
        """A real digest change: one extra index annotation moves the digest."""
        self.mutate(DEBIAN_INDEX, lambda path: edit_json(
            path, lambda value: value.update({'annotations': {'fixture': 'shifted'}})))

    def tamper_marker(self, relative, transform):
        """Change pinned metadata and re-bind the fake attestation to the bytes.

        Re-binding is what separates an *authenticated* field disagreement from
        a byte-level signature failure: only then does the adapter's own field
        validation get exercised.
        """
        assert relative == RELEASE_FIXTURE, relative
        value = json.loads(self.release_json.read_bytes())
        transform(value)
        self.release_json.write_text(json.dumps(value, indent=1) + '\n')
        self.attest['release_json_sha256'] = sha256_bytes(self.release_json.read_bytes())
        self.write_state()


def report(identifier, description):
    print('parent-pins: ' + identifier + ' ' + description + ' PASS')


def refused(status, case, fragment, frozen=True):
    assert status == 1, 'expected a refusal:\n' + case.output
    assert fragment in case.output, 'expected %r in:\n%s' % (fragment, case.output)
    if frozen:
        case.unchanged()


# --- parser and policy cases (in-process, no host tools) --------------------

def t1(root):
    """Duplicate object keys are rejected before any map can collapse them."""
    source = module()
    case = Case(root, 't1')
    case.bootstrap()
    template = (case.repo / INVENTORY).read_text()
    assert source.parse_inventory(template) is not None
    duplicates = {
        'top level': ('"schema": 1,', '"schema": 1, "schema": 1,'),
        'pin': ('"debian": {', '"debian": {}, "debian": {'),
        'platform': ('"linux/amd64": {', '"linux/amd64": {}, "linux/amd64": {'),
        'nested verification': ('"method": "hash-only",',
                                '"method": "hash-only", "method": "hash-only",'),
        'recipe entry': ('"stage": "recipe-contract-bin",',
                         '"stage": "recipe-contract-bin", "stage": "recipe-contract-bin",'),
    }
    for label, (old, new) in duplicates.items():
        text = template.replace(old, new, 1)
        assert text != template, label
        try:
            source.parse_inventory(text)
        except source.Refusal as error:
            assert 'duplicate JSON key' in str(error), (label, error)
        else:
            raise AssertionError('accepted a duplicate %s key' % label)
    mutations = {
        'unknown pin kind': ('"docker-official-image"', '"docker-official-image-x"'),
        'tag-only official reference': ('debian:13-slim@sha256:', 'debian:13-slim'),
        'tagged release reference': (RELEASED + '@sha256:', RELEASED + ':0.1.3@sha256:'),
        'missing amd64 platform': ('"linux/amd64"', '"linux/amd32"'),
        'unknown key': ('"expected_source":', '"expected-source":'),
        'bad date': ('"resolved_on": "2', '"resolved_on": "x'),
        'wrong signer ref': ('"refs/heads/main"', '"refs/heads/dev"'),
        'blank reassess': ('"reassess": "Stop refresh if a supported verifiable signing scheme '
                           'is published for this image"', '"reassess": "   "'),
        'wrong method': ('"method": "hash-only"', '"method": "hash-ish"'),
    }
    for label, (old, new) in mutations.items():
        text = template.replace(old, new, 1)
        assert text != template, label
        try:
            source.parse_inventory(text)
        except source.Refusal:
            continue
        raise AssertionError('accepted an inventory mutation: ' + label)
    # The release signer is the release module's identity, not whatever pair the
    # inventory happens to name: a fork signing with its own release workflow is
    # internally consistent but is not the trusted signer.
    forked = template.replace('gregwebs/agent-vm-images', 'gregwebs/agent-vm-images-fork')
    assert forked != template, 'the fork mutation did not apply'
    try:
        source.parse_inventory(forked)
    except source.Refusal as error:
        assert 'repository' in str(error) or 'signer' in str(error), error
    else:
        raise AssertionError('accepted a fork signing with its own release workflow')
    report('T1', 'rejects duplicate keys and malformed inventory records')


def t2(root):
    """FROM parsing keeps raw and resolved forms and only global ARG defaults."""
    source = module()
    inventory = source.parse_inventory((ROOT / INVENTORY).read_text())
    froms = source.parse_froms((ROOT / BASE_DOCKERFILE).read_text())
    assert [each.stage for each in froms] == ['recipe-contract-bin', None], froms
    assert froms[0].platform == '$BUILDPLATFORM'
    # A documented refresh moves the pin and the FROM together, so bind the parsed
    # forms to the inventory instead of to the toolchain reviewed today.
    assert froms[0].raw_image == inventory.pins['golang-builder'].reference.text()
    assert froms[1].raw_image == froms[1].resolved_image, 'a literal FROM resolves to itself'
    assert froms[1].resolved_image == inventory.pins['debian'].reference.text()
    standard = source.parse_froms((ROOT / STANDARD_DOCKERFILE).read_text())
    assert standard[-1].raw_image == '${BASE_IMAGE}'
    assert standard[-1].resolved_image == 'agent-vm-base:local'
    extension = source.parse_froms((ROOT / 'examples/base-extension/Dockerfile').read_text())
    # Part C moved the extension's base into a documented ARG, so its raw form is
    # the expansion while the resolved form still names the local base.
    assert extension[0].raw_image == '${BASE_IMAGE}', extension[0]
    assert extension[0].resolved_image == 'agent-vm-base:local', extension[0]
    joined = source.parse_froms(
        'ARG BASE_IMAGE=agent-vm-base:local\n'
        'FROM --platform=$BUILDPLATFORM golang:1.27.3-trixie \\\n    AS builder\n'
        '# a comment between instructions\n'
        'FROM ${BASE_IMAGE}\n')
    assert [(each.stage, each.resolved_image) for each in joined] == [
        ('builder', 'golang:1.27.3-trixie'), (None, 'agent-vm-base:local')]
    stage_local = source.parse_froms('ARG BASE_IMAGE=agent-vm-base:local\nFROM ${BASE_IMAGE}\n'
                                     'ARG BASE_IMAGE=somewhere:else\n')
    assert stage_local[0].resolved_image == 'agent-vm-base:local'
    for text in ('FROM ${BASE_IMAGE}\n', 'FROM ${BASE_IMAGE:-agent-vm-base:local}\n',
                 'FROM pre${BASE_IMAGE}\n', 'ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n'):
        try:
            source.parse_froms(text)
        except source.Refusal:
            continue
        raise AssertionError('accepted an unresolved FROM: ' + text)
    report('T2', 'parses raw and resolved FROMs, --platform and global ARG defaults')


def t3(root):
    """Recipe binding rules: the adopted tree passes and mutations do not."""
    source = module()
    case = Case(root, 't3')
    case.bootstrap()
    recipes = {path.relative_to(case.repo).as_posix(): path.read_text()
               for path in sorted((case.repo / 'images').rglob('Dockerfile'))
               + sorted((case.repo / 'examples').rglob('Dockerfile'))}
    inventory = source.parse_inventory((case.repo / INVENTORY).read_text())
    source.check_recipe_parents(inventory, recipes)
    pin = inventory.pins['debian'].reference
    mutations = {
        'changed Debian hex': (BASE_DOCKERFILE, pin.text(),
                               pin.repository.value + ':' + pin.tag + '@sha256:' + '0' * 64),
        'tag-only parent': (BASE_DOCKERFILE, pin.text(),
                            pin.repository.value + ':' + pin.tag),
        'same digest at another repository': (BASE_DOCKERFILE, pin.text(),
                                              'debianx:' + pin.tag + '@' + pin.digest.value),
        'new FROM': (BASE_DOCKERFILE, 'CMD ["/bin/bash"]',
                     'FROM busybox:1\n\nCMD ["/bin/bash"]'),
        'local base replaced': ('examples/base-extension/Dockerfile', 'agent-vm-base:local',
                                'agent-vm-base:other'),
        'inconsistent Go declaration': (STANDARD_DOCKERFILE,
                                        inventory.pins['golang-builder'].reference.text(),
                                        'golang:1.27.3-trixie@sha256:' + '1' * 64),
    }
    for label, (path, old, new) in mutations.items():
        mutate = dict(recipes)
        mutate[path] = mutate[path].replace(old, new, 1)
        assert mutate[path] != recipes[path], label
        try:
            source.check_recipe_parents(inventory, mutate)
        except source.Refusal:
            continue
        raise AssertionError('accepted a recipe mutation: ' + label)
    for label, mutate in {
        'unlisted recipe': lambda value: value.__setitem__(
            'examples/layers/new/Dockerfile',
            'FROM ' + inventory.pins['standard-release'].reference.text() + '\n'),
        'missing recipe': lambda value: value.pop('examples/layers/go-dev/Dockerfile'),
    }.items():
        changed = dict(recipes)
        mutate(changed)
        try:
            source.check_recipe_parents(inventory, changed)
        except source.Refusal:
            continue
        raise AssertionError('accepted a recipe set mutation: ' + label)
    # A local-base record must describe the recipe it is listed for, not just any
    # value in the closed set.
    swapped = source.parse_inventory((case.repo / INVENTORY).read_text())
    extension = 'examples/base-extension/Dockerfile'
    specs = list(swapped.recipes[extension])
    specs[0] = source.ParentSpec(specs[0].stage, 'local_base', 'standard')
    changed = dict(swapped.recipes)
    changed[extension] = tuple(specs)
    try:
        source.check_recipe_parents(source.ParentInventory(swapped.pins, changed), recipes)
    except source.Refusal as error:
        assert 'does not describe this recipe' in str(error), error
    else:
        raise AssertionError('accepted a local base that does not describe its recipe')
    report('T3', 'binds every maintained recipe to the inventory and refuses drift')


def t3b(root):
    """A recipe that exists on disk but not in the inventory stops a refresh."""
    case = Case(root, 't3b')
    case.adopt()
    new = case.repo / 'examples/layers/new/Dockerfile'
    new.parent.mkdir(parents=True)
    new.write_text('FROM ' + case.inventory()['pins']['standard-release']['reference'] + '\n')
    case.snapshot()
    refused(case.run(['debian']), case, 'unlisted')
    assert 'examples/layers/new/Dockerfile' in case.output, case.output
    assert 'parent-pins: published' not in case.output, case.output
    case.no_work()
    report('T3b', 'refuses a recipe on disk that the inventory does not list')


def t4(root):
    """Debian repository policy accepts the reviewed baseline and nothing else."""
    source = module()
    policy = (FIXTURES / 'apt/etc/apt/sources.list.d/debian.sources').read_bytes()
    source.check_debian_policy(policy)
    assert policy.startswith(b'Types: deb\n# http://snapshot.debian.org/'), \
        'the snapshot comments are preserved source bytes'
    mutations = {
        'changed suite': (b'Suites: trixie trixie-updates', b'Suites: trixie'),
        'changed URI': (b'URIs: http://deb.debian.org/debian\n',
                        b'URIs: https://deb.debian.org/debian\n'),
        'changed key path': (b'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp',
                             b'Signed-By: /etc/apt/keyrings/debian.pgp'),
        'extra field': (b'Components: main',
                        b'Components: main\nTrusted: yes'),
        'extra stanza': (b'Components: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n',
                         b'Components: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.pgp\n\n'
                         b'URIs: http://example.invalid/debian\nSuites: trixie\nComponents: main\n'
                         b'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp\n'),
        'not deb only': (b'Types: deb', b'Types: deb deb-src'),
        'continuation line': (b'Components: main', b'Components:\n main'),
        'duplicate field': (b'Components: main', b'Components: main\nComponents: main'),
        'missing second Signed-By': (
            b'Suites: trixie-security\nComponents: main\n'
            b'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp\n',
            b'Suites: trixie-security\nComponents: main\n'),
    }
    for label, (old, new) in mutations.items():
        mutated = policy.replace(old, new, 1)
        assert mutated != policy, label
        try:
            source.check_debian_policy(mutated)
        except source.Refusal:
            continue
        raise AssertionError('accepted a Debian policy mutation: ' + label)
    # `docker cp` copies a symlink as a link, so a parent that ships /etc/apt or
    # /usr/share/keyrings as one must be refused before anything is read from it.
    link = root / 't4-apt-link'
    link.symlink_to(FIXTURES / 'apt/etc/apt')
    try:
        source.read_apt_tree(link, FIXTURES / 'apt/usr/share/keyrings')
    except source.Refusal as error:
        assert 'symlink' in str(error), error
    else:
        raise AssertionError('followed a symlinked parent apt tree')
    keyring_link = root / 't4-keyring-link'
    keyring_link.symlink_to(FIXTURES / 'apt/usr/share/keyrings')
    try:
        source.read_apt_tree(FIXTURES / 'apt/etc/apt', keyring_link)
    except source.Refusal as error:
        assert 'symlink' in str(error), error
    else:
        raise AssertionError('followed a symlinked parent keyring directory')
    # A link one component down reaches the host the same way: these two
    # directories are read through `iterdir`/`is_file`, which resolve an absolute
    # copied link against the host, not against the copy.
    for label, relative in (('sources', 'sources.list.d'), ('trusted', 'trusted.gpg.d')):
        linked = root / ('t4-linked-' + label)
        shutil.copytree(FIXTURES / 'apt', linked, symlinks=True)
        directory = linked / 'etc/apt' / relative
        directory.rename(linked / 'etc/apt' / (relative + '-real'))
        directory.symlink_to(FIXTURES / 'apt/etc/apt' / relative)
        try:
            source.read_apt_tree(linked / 'etc/apt', linked / 'usr/share/keyrings')
        except source.Refusal as error:
            assert 'symlink' in str(error), error
        else:
            raise AssertionError('followed a symlinked ' + relative)
    report('T4', 'validates the exact two-stanza Debian repository policy')


def t5(root):
    """Source identity is repository equality, not a URL prefix."""
    source = module()
    debian = source.OfficialPolicy('debian', 'debian:13-slim',
                                   'https://github.com/debuerreotype/docker-debian-artifacts.git',
                                   False)
    golang = source.OfficialPolicy('golang-builder', 'golang:1.27.3-trixie',
                                   'https://github.com/docker-library/golang.git', True)
    revision = '41d9b4da70324ae7b6461ac2cc401b06719ff57d'
    debian_revision = 'cee78df586385250331cacba8fd1140133c054e5'
    source.check_source('https://github.com/docker-library/golang.git', golang, revision)
    source.check_source('https://github.com/docker-library/golang', golang, revision)
    source.check_source('https://github.com/docker-library/golang.git#' + revision + ':1.27/trixie',
                        golang, revision)
    source.check_source('https://github.com/debuerreotype/docker-debian-artifacts.git', debian,
                        debian_revision)
    source.check_source('https://github.com/debuerreotype/docker-debian-artifacts', debian,
                        debian_revision)
    for bad in ('https://github.com/docker-library/golang.git.evil',
                'https://github.com/docker-library/golang-evil.git',
                'https://github.com.evil/docker-library/golang.git',
                'https://GitHub.com/docker-library/golang.git',
                'https://github.com:443/docker-library/golang.git',
                'https://user@github.com/docker-library/golang.git',
                'http://github.com/docker-library/golang.git',
                'https://github.com/docker-library/golang.git?ref=1.27',
                'https://github.com/docker-library/golang.git/',
                'https://github.com/docker-library/%67olang.git',
                'https://github.com/docker-library/golang.git#' + revision + ':1.26/trixie',
                'https://github.com/docker-library/golang.git#' + revision + ':1.27/trixie/extra',
                'https://github.com/docker-library/golang.git#' + '0' * 40 + ':1.27/trixie',
                'https://github.com/docker-library/golang.git#1.27/trixie'):
        try:
            source.check_source(bad, golang, revision)
        except source.Refusal:
            continue
        raise AssertionError('accepted a source lookalike: ' + bad)
    try:
        source.check_source('https://github.com/debuerreotype/docker-debian-artifacts.git#'
                            + debian_revision, debian, debian_revision)
    except source.Refusal:
        pass
    else:
        raise AssertionError('accepted a Debian revision fragment')
    report('T5', 'compares source URLs by repository identity and revision fragment')


# --- resolution cases (real CLI over the fake seams) -----------------------

def t6(root):
    """Bootstrap adopts all three pins, then every rerun reports unchanged."""
    case = Case(root, 't6')
    case.bootstrap()
    assert not case.live_containers(), 'an extraction container was left behind'
    inventory = case.inventory()
    assert inventory['schema'] == 1
    assert sorted(inventory['pins']) == ['debian', 'golang-builder', 'standard-release']
    debian = inventory['pins']['debian']
    assert debian['reference'] == 'debian:13-slim@' + digest(FIXTURES / 'registry/debian-index.json')
    assert sorted(debian['platforms']) == ['linux/amd64', 'linux/arm64']
    assert debian['platforms']['linux/amd64']['manifest'] == digest(
        FIXTURES / 'registry/debian-amd64.json')
    assert debian['platforms']['linux/arm64']['manifest'] == digest(
        FIXTURES / 'registry/debian-arm64.json')
    assert debian['platforms']['linux/arm64']['revision'] == \
        '9c4818e3bfd35139d3798b0420e9c46836658b2e'
    assert debian['expected_source'] == 'https://github.com/debuerreotype/docker-debian-artifacts.git'
    assert debian['verification']['method'] == 'hash-only'
    assert debian['verification']['reviewed_on'] == debian['resolved_on']
    assert len(debian['verification']['evidence']) == 4
    golang = inventory['pins']['golang-builder']
    assert golang['reference'] == 'golang:1.27.3-trixie@' + digest(
        FIXTURES / 'registry/golang-index.json')
    assert ':1.27/trixie' in golang['platforms']['linux/amd64']['source']
    release = inventory['pins']['standard-release']
    assert release['reference'] == RELEASED + '@' + digest(FIXTURES / 'registry/standard-index.json')
    assert release['version'] == '0.1.3'
    assert 'expected_source' not in release
    assert release['platforms']['linux/amd64'] == {
        'manifest': digest(FIXTURES / 'registry/standard-amd64.json')}
    verification = release['verification']
    assert verification['method'] == 'github-attestation'
    assert verification['signer_workflow'] == case.attest['workflow']
    assert verification['source_ref'] == 'refs/heads/main'
    assert verification['source_sha'] == SOURCE_SHA
    assert verification['invocation_url'] == INVOCATION
    assert verification['release_json_sha256'] == 'sha256:' + case.attest['release_json_sha256']
    assert inventory['recipes']['images/Dockerfile'] == [
        {'stage': 'recipe-contract-bin', 'pin': 'golang-builder'},
        {'stage': None, 'pin': 'debian'}]
    assert inventory['recipes']['images/standard/Dockerfile'][-1] == {
        'stage': None, 'local_base': 'standard'}
    assert inventory['recipes']['examples/base-extension/Dockerfile'] == [
        {'stage': None, 'local_base': 'extension'}]
    assert len(inventory['recipes']) == 7
    assert 'FROM debian:13-slim@' + digest(FIXTURES / 'registry/debian-index.json') \
        in (case.repo / BASE_DOCKERFILE).read_text()
    sources = (case.repo / POLICY_FILES[0]).read_text()
    assert sources.count('Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp') == 2
    assert sources.count('# http://snapshot.debian.org/') == 2, 'snapshot comments are preserved'
    keyrings = (case.repo / POLICY_FILES[1]).read_text().splitlines()
    assert keyrings == sorted(keyrings), 'keyrings.sha256 must be sorted by path'
    assert all(len(line.split('  ')[0]) == 64 for line in keyrings), keyrings
    assert any(line.endswith('  usr/share/keyrings/debian-archive-keyring.pgp')
               for line in keyrings)
    assert any('  etc/apt/trusted.gpg.d/' in line for line in keyrings)
    assert 'after  debian: debian:13-slim@sha256:' in case.output
    assert 'linux/arm64: ' + digest(FIXTURES / 'registry/debian-arm64.json') in case.output
    assert 'proposal' in case.output, 'bootstrap must mark the exceptions as proposals'
    assert case.inventory()['apt_open_gaps'] == [], \
        'bootstrap records no exemption; a human adds each exact #96 instruction'
    assert '+FROM debian:13-slim@' in case.output, 'the proposal must show the diff'
    assert 'authenticated v0.1.3 at ' + INVOCATION in case.output
    case.no_work()
    case.commit('adopt')
    case.unchanged()
    for arguments in (['debian'], ['golang-builder'], ['standard-release', '0.1.3']):
        assert case.run(arguments) == 0, case.output
        assert 'unchanged' in case.output, case.output
        case.unchanged()
        case.no_work()
    # The real v0.1.3 metadata shape the historical adapter must accept: eight
    # selections, no opencode-claude, arm64 without a variant.
    metadata = json.loads((FIXTURES / RELEASE_FIXTURE).read_bytes())
    assert len(metadata['platforms'][1]['selections']) == 8
    assert 'opencode-claude' not in metadata['platforms'][1]['selections']
    assert 'variant' not in metadata['platforms'][1]['graph']
    report('T6', 'bootstraps all pins, keeps review data and reports unchanged reruns')


def _conflicting_revision(case):
    child = case.fixture('registry/debian-amd64.json')
    edit_json(child, lambda value: value.setdefault('annotations', {}).update(
        {'org.opencontainers.image.revision': '0' * 40}))
    index = case.fixture(DEBIAN_INDEX)
    edit_json(index, lambda value: value['manifests'][0].update(
        {'digest': digest(child), 'size': child.stat().st_size}))


def _cross_arch_apt(case):
    arm = case.fixture('apt-arm64')
    shutil.copytree(case.fixture('apt'), arm)
    replace(arm / 'etc/apt/sources.list.d/debian.sources', 'Components: main', 'Components: contrib')
    case.specs['debian']['apt']['arm64'] = 'apt-arm64'


def _drop_arm64(path):
    edit_json(path, lambda value: value.update({'manifests': [
        descriptor for descriptor in value['manifests']
        if (descriptor.get('platform') or {}).get('architecture') != 'arm64']}))


def _duplicate_amd64(path):
    edit_json(path, lambda value: value['manifests'].insert(1, dict(value['manifests'][0])))


GOOD_FRAGMENT = ('https://github.com/docker-library/golang.git#41d9b4da70324ae7b6461ac2cc401b06719ff57d'
                 ':1.27/trixie')


SOURCE_KEY = 'org.opencontainers.image.source'


def _golang_source(case, source):
    """A self-consistent wrong source: the child and its descriptor agree.

    The signed digest must follow the child bytes, so this is the only shape in
    which a repository mismatch reaches the source policy instead of being
    refused earlier as a digest or annotation conflict.
    """
    child = case.fixture('registry/golang-arm64.json')
    edit_json(child, lambda value: value['annotations'].update({SOURCE_KEY: source}))
    index = case.fixture('registry/golang-index.json')

    def relabel(value):
        descriptor = value['manifests'][GO_ARM64_INDEX_ENTRY]
        descriptor['annotations'][SOURCE_KEY] = source
        descriptor['digest'] = digest(child)
        descriptor['size'] = child.stat().st_size

    edit_json(index, relabel)
    case.write_state()


# (label, expected refusal fragment, mutation, is a Go-index refresh)
RESOLUTION_MUTATIONS = [
    ('child bytes mismatch', 'child bytes do not hash to',
     lambda case: case.specs['debian']['children'].update(arm64='registry/debian-amd64.json'),
     False),
    ('wrong repository', 'not the reviewed repository',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace('docker-library', 'golang-evil')),
     True),
    ('dot-git-evil suffix', 'not the reviewed repository',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace('golang.git#', 'golang.git.evil#')),
     True),
    ('lookalike host', 'not the reviewed repository',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace('github.com', 'github.com.evil')),
     True),
    ('encoded separator', 'encoded or escaped source URL',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace('docker-library', 'docker%2dlibrary')),
     True),
    ('source query', 'source carries query, userinfo or parameters',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace('golang.git#', 'golang.git?x=1#')),
     True),
    ('missing revision', 'missing org.opencontainers.image.revision',
     lambda case: case.mutate('registry/debian-index.json', lambda path: edit_json(
         path, lambda value: value['manifests'][0]['annotations'].pop(
             'org.opencontainers.image.revision'))), False),
    ('missing source', 'missing org.opencontainers.image.source',
     lambda case: case.mutate('registry/debian-index.json', lambda path: edit_json(
         path, lambda value: value['manifests'][0]['annotations'].pop(
             'org.opencontainers.image.source'))), False),
    ('conflicting revision', 'conflicting org.opencontainers.image.revision',
     _conflicting_revision, False),
    ('duplicate platform', 'duplicate linux/amd64 child',
     lambda case: case.mutate('registry/debian-index.json', _duplicate_amd64), False),
    ('official arm64 without variant', 'must declare variant v8',
     lambda case: case.mutate('registry/golang-index.json', lambda path: edit_json(
         path, lambda value: value['manifests'][GO_ARM64_INDEX_ENTRY]['platform'].pop('variant'))),
     True),
    ('missing arm64 child', 'children are required',
     lambda case: case.mutate('registry/debian-index.json', _drop_arm64), False),
    ('unsupported index media type', 'unsupported index media type',
     lambda case: case.mutate('registry/golang-index.json', lambda path: edit_json(
         path, lambda value: value.update({'mediaType': 'application/vnd.example+json'}))),
     True),
    ('fragment directory mismatch', 'directory differs from the selected tag',
     lambda case: _golang_source(case, GOOD_FRAGMENT.replace(':1.27/', ':1.26/')), True),
    ('changed Debian suite', 'URI/suite policy changed',
     lambda case: case.mutate('apt/etc/apt/sources.list.d/debian.sources',
                              lambda path: replace(path, 'Suites: trixie trixie-updates',
                                                   'Suites: trixie')), False),
    ('changed Debian key path', 'Signed-By policy changed',
     lambda case: case.mutate('apt/etc/apt/sources.list.d/debian.sources',
                              lambda path: replace(
                                  path, 'Signed-By: /usr/share/keyrings/debian-archive-keyring.pgp',
                                  'Signed-By: /usr/share/keyrings/other.pgp')), False),
    ('trusted.gpg present', 'ships /etc/apt/trusted.gpg',
     lambda case: case.fixture('apt/etc/apt/trusted.gpg').write_bytes(b'legacy keyring\n'), False),
    ('cross-arch apt difference', 'apt sources differ', _cross_arch_apt, False),
    ('missing metadata signature', 'no matching attestations',
     lambda case: case.failures.update({'attestation': ['release-metadata.sigstore.json']}), False),
    ('missing index signature', 'no matching attestations',
     lambda case: case.failures.update({'attestation': ['release-index.sigstore.json']}), False),
    ('missing release asset', 'download of release.json failed',
     lambda case: case.failures.update({'downloads': ['release.json']}), False),
    ('tampered metadata bytes', 'does not match the signed subject',
     lambda case: replace(case.release_json, '"version": "0.1.3"', '"version": "0.1.4"'), False),
    ('metadata version mismatch', "is version '0.1.4', not '0.1.3'",
     lambda case: case.tamper_marker(RELEASE_FIXTURE,
                                     lambda value: value.update({'version': '0.1.4'})), False),
    ('metadata product mismatch', 'not a schema-1 standard release',
     lambda case: case.tamper_marker(RELEASE_FIXTURE,
                                     lambda value: value.update({'product': 'other'})), False),
    ('metadata index mismatch', 'is not the expected',
     lambda case: case.tamper_marker(RELEASE_FIXTURE, lambda value: value.update(
         {'index_digest': 'sha256:' + '0' * 64})), False),
    ('metadata source sha mismatch', 'wrong --source-digest',
     lambda case: case.tamper_marker(RELEASE_FIXTURE,
                                     lambda value: value.update({'source_sha': '1' * 40})), False),
    ('metadata invocation mismatch', 'invocation URL disagrees',
     lambda case: case.tamper_marker(RELEASE_FIXTURE, lambda value: value['platforms'][0].update(
         {'invocation_url': INVOCATION.replace('37775269427', '37775269428')})), False),
    ('metadata tag mismatch', 'tag does not match its version',
     lambda case: case.tamper_marker(RELEASE_FIXTURE,
                                     lambda value: value.update({'release_tag': 'v0.1.4'})), False),
    ('duplicate platform in metadata', 'repeats the linux/arm64 platform',
     lambda case: case.tamper_marker(RELEASE_FIXTURE, lambda value: value['platforms'][0]['graph']
                                     .update({'architecture': 'arm64'})), False),
    ('empty selection record', 'empty selection record',
     lambda case: case.tamper_marker(RELEASE_FIXTURE, lambda value: value['platforms'][0]
                                     .update({'selections': {}})), False),
    ('registry index bytes differ', 'bytes do not hash to',
     lambda case: case.override.update(
         {RELEASED + '@sha256:' + case.attest['index_digest']:
          str(case.fixture('registry/standard-amd64.json'))}), False),
]


def t7(root):  # noqa: C901 - one table drives every mutation
    """Every resolution mutation is refused with the plan unchanged.

    A mutation to the pinned Go index can only be observed through a *refresh*:
    bootstrap resolves the committed digest, so altered bytes are refused before
    any annotation is read. That content-addressed refusal is asserted here too.
    """
    refusals = 0
    for number, (label, fragment, mutate, refresh) in enumerate(RESOLUTION_MUTATIONS,
                                                               start=1):
        case = Case(root, 't7-%02d' % number)
        if refresh:
            case.adopt()
        mutate(case)
        case.write_state()
        status = case.run(['golang-builder'] if refresh else ['--bootstrap'])
        assert status == 1, 'expected a refusal for %s:\n%s' % (label, case.output)
        assert fragment in case.output, 'expected %r for %s in:\n%s' % (fragment, label,
                                                                        case.output)
        case.unchanged()
        case.no_work()
        assert not case.live_containers(), 'an extraction container survived ' + label
        refusals += 1
    case = Case(root, 't7-digest')
    case.mutate('registry/golang-index.json', lambda path: edit_json(
        path, lambda value: value.update({'annotations': {'fixture': 'republished'}})))
    case.write_state()
    assert case.run(['--bootstrap']) == 0, case.output
    assert 'the pin retains the committed digest' in case.output, case.output
    assert case.inventory()['pins']['golang-builder']['reference'] == case.go_committed
    refusals += 1
    report('T7', 'refuses all %d resolution mutations without changing sources' % refusals)


def t8(root):
    """Selection and adoption states are enforced exactly."""
    case = Case(root, 't8')
    refuse_go = case.run(['golang-builder', 'golang:1.26.4-trixie'])
    assert refuse_go == 1, case.output
    assert 'differs from the go.mod toolchain' in case.output, case.output
    case.unchanged()
    for arguments in (['latest'], ['debian', 'debian:13-slim'], ['standard-release'],
                      ['standard-release', '1.0'], ['golang-builder', 'golang:latest-trixie'],
                      ['--bootstrap', 'debian'], ['nonsense'], []):
        assert case.run(arguments) == 2, (arguments, case.output)
    assert case.run(['--bootstrap'], env={'GITHUB_ACTIONS': 'true'}) == 2
    assert 'GitHub Actions' in case.output and '#105' in case.output
    case.unchanged()
    case = Case(root, 't8b')
    (case.repo / INVENTORY).write_text('{}\n')
    case.snapshot()
    case.run(['--bootstrap'])
    refused(case.status, case, 'partial adoption')
    assert (case.repo / INVENTORY).read_text() == '{}\n'
    case = Case(root, 't8c')
    (case.repo / 'images/apt-policy').mkdir()
    (case.repo / POLICY_FILES[0]).write_text('Types: deb\n')
    case.snapshot()
    case.run(['--bootstrap'])
    refused(case.status, case, 'partial adoption')
    case = Case(root, 't8d')
    case.run(['debian'])
    refused(case.status, case, 'no inventory to refresh')
    case = Case(root, 't8e')
    case.adopt()
    case.run(['--bootstrap'])
    refused(case.status, case, 'only valid before the inventory is adopted')
    report('T8', 'refuses usage errors, CI execution and every partial adoption state')


# --- publication cases -----------------------------------------------------

def t9(root):
    """Dirty, staged and untracked inputs and destinations stop a refresh."""
    case = Case(root, 't9')
    case.adopt()

    def dirty(relative, text='# local edit\n'):
        with (case.repo / relative).open('a') as stream:
            stream.write(text)
        case.snapshot()

    def clean(relative):
        case.git('reset', '-q', 'HEAD', relative)
        case.git('checkout', '--', relative)
        case.snapshot()

    dirty(BASE_DOCKERFILE)
    refused(case.run(['debian']), case,
            'refusing dirty or untracked input/destination: ' + BASE_DOCKERFILE)
    clean(BASE_DOCKERFILE)
    dirty(BASE_DOCKERFILE, '# staged edit\n')
    case.git('add', BASE_DOCKERFILE)
    refused(case.run(['debian']), case,
            'refusing dirty or untracked input/destination: ' + BASE_DOCKERFILE)
    clean(BASE_DOCKERFILE)
    # A dirty dependency that is not itself a destination still blocks.
    dirty('images/pnpm/verify-pnpm.sh')
    refused(case.run(['debian']), case,
            'refusing dirty or untracked input/destination: images/pnpm/verify-pnpm.sh')
    case.git('add', 'images/pnpm/verify-pnpm.sh')
    refused(case.run(['debian']), case,
            'refusing dirty or untracked input/destination: images/pnpm/verify-pnpm.sh')
    clean('images/pnpm/verify-pnpm.sh')
    # A dirty extracted apt-policy file is a base-input dependency even for a
    # refresh that does not write it (a Go or a release refresh).
    dirty(POLICY_FILES[0])
    refused(case.run(['golang-builder']), case,
            'refusing dirty or untracked input/destination: ' + POLICY_FILES[0])
    clean(POLICY_FILES[0])
    # A destination that exists but is no longer tracked.
    case.git('rm', '-q', '--cached', INVENTORY)
    refused(case.run(['debian']), case,
            'refusing dirty or untracked input/destination: ' + INVENTORY)
    case.git('reset', '-q', 'HEAD', INVENTORY)
    case.snapshot()
    case.unchanged()
    case.no_work()
    report('T9', 'refuses worktree, index and untracked input/destination changes')


def t10(root):
    """Destination ancestors must be real directories inside the checkout."""
    case = Case(root, 't10')
    (case.repo / 'images/apt-policy').symlink_to(case.root / 'outside')
    (case.root / 'outside').mkdir()
    case.snapshot()
    refused(case.run(['--bootstrap']), case, 'symlinked destination ancestor: images/apt-policy')
    case.no_work()
    case = Case(root, 't10b')
    (case.repo / 'images/apt-policy').write_text('not a directory\n')
    case.snapshot()
    refused(case.run(['--bootstrap']), case, 'non-directory destination ancestor: images/apt-policy')
    case = Case(root, 't10c')
    case.adopt()
    (case.repo / 'examples/layers').rename(case.repo / 'examples/layers-real')
    (case.repo / 'examples/layers').symlink_to('layers-real')
    case.snapshot()
    refused(case.run(['standard-release', '0.1.3']), case,
            'symlinked destination ancestor: examples/layers')
    # A linked worktree keeps `.git` as a file, so a lock under $ROOT/.git would
    # report a held lock on the first legitimate refresh from one.
    case = Case(root, 't10d')
    case.adopt()
    worktree = case.root / 'worktree'
    added = case.git('worktree', 'add', '-q', '-b', 'pins-worktree', str(worktree))
    assert added.returncode == 0, added.stderr
    git_dir = subprocess.run(['git', '-C', str(worktree), 'rev-parse', '--absolute-git-dir'],
                             capture_output=True, text=True, check=True).stdout.strip()
    assert Path(git_dir) != worktree / '.git', 'a linked worktree keeps .git in the repository'
    assert case.run(['debian'], script=worktree / PUBLISHER) == 0, case.output
    assert 'unchanged' in case.output, case.output
    assert not (Path(git_dir) / 'parent-pins.lock').exists(), 'the worktree lock leaked'
    case.no_work()
    # Work allocation fails under an unwritable TMPDIR. The lock is taken after the
    # exit trap is installed, so a failed run must still release it.
    case = Case(root, 't10e')
    case.adopt()
    status = case.run(['debian'], env={'TMPDIR': str(case.root / 'absent-tmpdir')})
    assert status == 1, case.output
    assert 'mktemp' in case.output, case.output
    assert not (case.repo / '.git/parent-pins.lock').exists(), 'the lock leaked on a failed mktemp'
    assert case.run(['debian']) == 0, case.output
    case.no_work()
    report('T10', 'refuses symlinked and non-directory destination ancestors')


def t11(root):
    """An ancestor or checked input that changes mid-run stops publication.

    Each case needs a refresh that really publishes, otherwise the publisher
    stops at `unchanged` before the second preflight. A shifted Debian fixture
    provides that, and the change is made while resolution is still running.
    """
    case = Case(root, 't11')
    case.adopt()
    case.shift_debian()
    case.commit('shift the Debian fixture')
    case.hooks = [{'after_events': 3,
                   'rename': {'path': str(case.repo / 'images/apt-policy'),
                              'to': str(case.repo / 'images/apt-policy-real')}},
                  {'after_events': 3,
                   'symlink': {'path': str(case.repo / 'images/apt-policy'),
                               'target': 'apt-policy-real'}}]
    case.write_state()
    status = case.run(['debian'])
    refused(status, case, 'symlinked destination ancestor: images/apt-policy', frozen=False)
    for relative in (BASE_DOCKERFILE, INVENTORY):
        assert (case.repo / relative).read_bytes() == case.before[relative], \
            'publication touched ' + relative
    case.no_work()
    case = Case(root, 't11b')
    case.adopt()
    case.shift_debian()
    case.commit('shift the Debian fixture')
    case.hooks = [{'after_events': 3,
                   'write': {'path': str(case.repo / 'images/pnpm/pin.json'),
                             'text': '{"edited": true}\n'}}]
    case.write_state()
    status = case.run(['debian'])
    refused(status, case, 'refusing dirty or untracked input/destination: images/pnpm/pin.json',
            frozen=False)
    assert 'parent-pins: published' not in case.output, case.output
    for relative in (BASE_DOCKERFILE, INVENTORY):
        assert (case.repo / relative).read_bytes() == case.before[relative], \
            'publication touched ' + relative
    report('T11', 'refuses an ancestor or checked input that changes mid-run')


def t12(root):
    """A failed publication copy restores every destination inclusively."""
    case = Case(root, 't12')
    before = case.files()
    # Fail one copy after six have landed: everything already published must be
    # restored, and the invocation-created directory must remain (empty) so a
    # clean bootstrap can follow.
    (case.root / 'fail-copy').write_text('images/apt-policy/debian.sources')
    status = case.run(['--bootstrap'])
    assert status == 1, case.output
    assert 'failed to publish' in case.output, case.output
    after = case.files()
    restored = {name: after.get(name) for name in before}
    assert restored == before, 'a destination was not restored: %s' % sorted(
        set(before) ^ set(restored))
    assert not (case.repo / INVENTORY).exists(), 'an originally absent destination survived'
    policy = case.repo / 'images/apt-policy'
    assert policy.is_dir() and not any(policy.iterdir()), \
        'the invocation-created directory stays, empty'
    assert 'STOP: recover' not in case.output
    case.no_work()
    (case.root / 'fail-copy').unlink()
    assert case.run(['--bootstrap']) == 0, case.output
    case.published_only(BOOTSTRAP_DESTINATIONS)
    report('T12', 'restores every destination after a failed publication copy')


def t13(root):
    """A changed Debian digest refreshes visibly and keeps the reviewed exception."""
    case = Case(root, 't13')
    case.adopt()
    review = {'reviewed_on': '2026-01-01',
              'reason': 'reviewer-edited rationale for the committed digest',
              'reassess': 'reviewer-edited reassessment condition'}

    def review_pin(value):
        value['pins']['debian']['verification'].update(review)
        value['pins']['debian']['resolved_on'] = '2025-06-01'

    edit_json(case.repo / INVENTORY, review_pin)
    case.commit('record the reviewed Debian exception')
    before = case.inventory()
    old = before['pins']['debian']['reference']
    case.shift_debian()
    case.commit('shift the Debian fixture')
    assert case.run(['debian']) == 0, case.output
    # Read after the run: a case must not straddle midnight UTC.
    today = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
    case.published_only(DEBIAN_DESTINATIONS)
    pin = case.inventory()['pins']['debian']
    new = pin['reference']
    assert new != old
    assert new.endswith('@' + digest(case.fixture(DEBIAN_INDEX))), new
    # M1: a hash-only exception is reviewed data. A changed digest advances only
    # resolved_on; the rationale, its evidence, the reassessment condition and
    # the now-stale reviewed_on stay exactly as the reviewer left them.
    verification = pin['verification']
    assert verification['reviewed_on'] == review['reviewed_on'], verification
    assert verification['reason'] == review['reason'], verification
    assert verification['reassess'] == review['reassess'], verification
    assert verification['evidence'] == \
        before['pins']['debian']['verification']['evidence'], verification
    assert pin['resolved_on'] == today, pin['resolved_on']
    assert pin['resolved_on'] != before['pins']['debian']['resolved_on'], \
        'the resolution date must advance with the digest'
    assert pin['platforms']['linux/amd64']['manifest'] == digest(
        case.fixture('registry/debian-amd64.json'))
    # The other pins keep their own review data untouched.
    after = case.inventory()['pins']
    for name in ('golang-builder', 'standard-release'):
        assert after[name] == before['pins'][name], name + ' changed'
    # AC5: the proposal shows visible before/after identities and demands review.
    assert 'before debian: ' + old in case.output, case.output
    assert 'after  debian: ' + new in case.output, case.output
    assert 'hash-only exception requires renewed review: debian ' + new.split('@', 1)[1] \
        in case.output, case.output
    assert 'set it to the review date' in case.output, case.output
    assert '+FROM debian:13-slim@' + new.split('@', 1)[1] in case.output, \
        'the diff must show the new FROM'
    assert 'FROM ' + new in (case.repo / BASE_DOCKERFILE).read_text()
    # Only this pin's destinations moved.
    for relative in (STANDARD_DOCKERFILE, POLICY_FILES[0], POLICY_FILES[1], LAYERS_README):
        assert (case.repo / relative).read_bytes() == case.before[relative], relative
    case.no_work()
    case.commit('refresh the Debian pin')
    assert case.run(['debian']) == 0, case.output
    assert 'unchanged' in case.output, case.output
    case.unchanged()
    case.no_work()
    report('T13', 'refreshes a changed Debian digest and keeps the reviewed exception')


def t14(root):
    """A released-standard version refresh moves the recipes, comments and README."""
    case = Case(root, 't14')
    case.adopt()
    before = case.inventory()['pins']['standard-release']
    old = before['reference']
    new = case.publish_release('0.1.4')
    assert case.run(['standard-release', '0.1.4']) == 0, case.output
    # Read after the run: a case must not straddle midnight UTC.
    today = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
    case.published_only(RELEASE_DESTINATIONS)
    pin = case.inventory()['pins']['standard-release']
    assert pin['reference'] == new, pin['reference']
    assert pin['version'] == '0.1.4', pin['version']
    assert pin['platforms'] == before['platforms'], 'the child digests did not move'
    assert pin['resolved_on'] == today, pin['resolved_on']
    assert pin['verification']['release_json_sha256'] == \
        'sha256:' + case.attest['release_json_sha256'], pin['verification']
    assert pin['verification']['invocation_url'] == INVOCATION
    assert 'before standard-release: ' + old in case.output, case.output
    assert 'after  standard-release: ' + new in case.output, case.output
    assert 'authenticated v0.1.4 at ' + INVOCATION in case.output, case.output
    stale = old.split('@', 1)[1]
    for relative in LAYER_RECIPES:
        text = (case.repo / relative).read_text()
        assert new in text, relative + ' did not move'
        assert stale not in text, relative + ' still carries the old digest'
    # The digest appears in go-dev's ARG default and in its build-arg comment, so
    # a rewrite of one without the other would leave a stale comment behind.
    go_dev = (case.repo / LAYER_RECIPES[1]).read_text()
    assert go_dev.count(new) == 2, go_dev
    readme = (case.repo / LAYERS_README).read_text()
    assert new in readme and stale not in readme, 'the layers README is stale'
    for relative in (BASE_DOCKERFILE, STANDARD_DOCKERFILE, POLICY_FILES[0]):
        assert (case.repo / relative).read_bytes() == case.before[relative], relative
    case.no_work()
    case.commit('refresh the released parent')
    assert case.run(['standard-release', '0.1.4']) == 0, case.output
    assert 'unchanged' in case.output, case.output
    case.unchanged()
    report('T14', 'refreshes a released-standard version across every recipe and the README')


def t15(root):
    """A failed publication with a failed restore keeps documented recovery."""
    case = Case(root, 't15')
    before = case.files()
    case.flag({'needle': BASE_DOCKERFILE, 'phase': 'publish', 'action': 'fail',
               'truncate': True})
    assert case.run(['--bootstrap']) == 3, case.output
    assert 'rollback failed: ' + str(case.repo / BASE_DOCKERFILE) in case.output, case.output
    assert 'publication result undecided' in case.output, case.output
    work = case.recover_path()
    assert not (case.repo / '.git/parent-pins.lock').exists(), 'the lock leaked'
    recovery = json.loads((work / 'recovery.json').read_text())
    entries = recovery['destinations']
    assert recovery['work'] == str(work), recovery['work']
    assert [entry['path'] for entry in entries] == BOOTSTRAP_DESTINATIONS, entries
    assert recovery['created_directories'] == ['images/apt-policy'], recovery
    existing = [entry for entry in entries if entry['existed'] == 'yes']
    absent = [entry for entry in entries if entry['existed'] == 'no']
    assert absent and len(existing) + len(absent) == len(entries)
    assert sorted(entry['path'] for entry in absent) == sorted([INVENTORY, *POLICY_FILES])
    for entry in existing:
        assert (work / 'publish-backup' / entry['backup']).is_file(), entry
    # The tree is genuinely mixed: the failed copy truncated its destination.
    assert case.files()[BASE_DOCKERFILE] != before[BASE_DOCKERFILE], \
        'the failed copy left no partial file behind'
    # Recover by hand exactly as recovery.json documents: numbered backups in,
    # originally absent paths out. The created directory may stay.
    for entry in reversed(entries):
        target = case.repo / entry['path']
        if entry['existed'] == 'yes':
            shutil.copyfile(work / 'publish-backup' / entry['backup'], target)
        elif target.exists() or target.is_symlink():
            target.unlink()
    assert case.files() == before, 'manual recovery did not restore the tree'
    policy = case.repo / 'images/apt-policy'
    assert policy.is_dir() and not any(policy.iterdir()), 'the created directory stays, empty'
    (case.root / 'fail-copy').unlink()
    shutil.rmtree(work)
    case.no_work()
    # A clean rerun after recovery publishes normally.
    assert case.run(['--bootstrap']) == 0, case.output
    case.published_only(BOOTSTRAP_DESTINATIONS)
    case.no_work()
    report('T15', 'retains recovery.json and backups after a failed restore, then reruns')


def t15b(root):
    """A signal keeps recovery material: 3 during backup, 143 during a copy."""
    case = Case(root, 't15b-backup')
    before = case.files()
    case.flag({'needle': BASE_DOCKERFILE, 'phase': 'backup', 'action': 'TERM'})
    assert case.run(['--bootstrap']) == 3, case.output
    assert 'interrupted while taking backups; sources unchanged' in case.output, case.output
    assert 'STOP: recover ' in case.output, case.output
    case.retained()
    case.unchanged()
    case.no_work(retained=True)
    case = Case(root, 't15b-copy')
    before = case.files()
    case.flag({'needle': BASE_DOCKERFILE, 'phase': 'publish', 'action': 'TERM'})
    assert case.run(['--bootstrap']) == 143, case.output
    assert 'publication interrupted; sources restored' in case.output, case.output
    assert 'STOP: recover ' in case.output, case.output
    case.retained()
    case.unchanged()
    assert case.files() == before, 'the interrupted publication was not restored'
    case.no_work(retained=True)
    report('T15b', 'keeps recovery material and distinguishes 3 from 143 on a signal')


def t16(root):
    """The publisher refuses any staged set outside its own computed plan."""
    cases = [('absolute destination', ABSOLUTE_STAGER, 'non-checkout-relative destination'),
             ('traversal destination', TRAVERSAL_STAGER, 'non-checkout-relative destination'),
             ('empty plan', EMPTY_STAGER, 'no destinations'),
             ('unplanned staged path', EXTRA_STAGER, 'unplanned destination'),
             ('incomplete staged set', PARTIAL_STAGER, 'destination list is incomplete'),
             ('malformed staged set', MALFORMED_STAGER, 'malformed')]
    for number, (label, script, fragment) in enumerate(cases, start=1):
        case = Case(root, 't16-%02d' % number)
        (case.repo / STAGER).write_text(script)
        case.snapshot()
        status = case.run(['--bootstrap'])
        assert status == 1, (label, case.output)
        assert fragment in case.output, (label, fragment, case.output)
        case.unchanged()
        case.no_work()
    report('T16', 'refuses unplanned, absolute, traversal and empty destination sets')


GAP_INSTRUCTION = ('RUN echo "deb http://legacy.example stable main" \\\n'
                   '        > /etc/apt/sources.list.d/legacy.list \\\n'
                   ' && rm -rf /var/lib/apt/lists/*')


def t17(root):
    """The closed apt gap record stays empty and refuses to grow (#96)."""
    source = module()
    case = Case(root, 't17')
    case.adopt()
    assert case.inventory()['apt_open_gaps'] == [], \
        'bootstrap records no exemption; #96 removed both recorded instructions'
    base = case.repo / BASE_DOCKERFILE
    base.write_text(base.read_text().replace(
        'CMD ["/bin/bash"]', GAP_INSTRUCTION + '\n\nCMD ["/bin/bash"]'))
    case.render_base_inputs()
    case.commit('re-add a legacy third-party apt instruction')
    case.snapshot()

    # A refresh publishes the empty record and says so: the recipe itself is the
    # image-source gate's business, so a hand-added instruction cannot acquire an
    # exemption here.
    assert case.run(['debian']) == 0, case.output
    assert case.inventory()['apt_open_gaps'] == [], 'a refresh dropped the empty record'
    assert 'apt open gaps: none' in case.output, case.output
    case.unchanged()
    case.no_work()

    # Parse-level: an inventory that records a gap is refused in every shape, and
    # neither the owner nor a well-formed instruction buys an exemption.
    template = json.loads((case.repo / INVENTORY).read_text())
    for label, entry in {
        'exact instruction': {'recipe': BASE_DOCKERFILE, 'instruction': GAP_INSTRUCTION,
                              'owner': source.APT_GAP_OWNER},
        'wrong owner': {'recipe': BASE_DOCKERFILE, 'instruction': GAP_INSTRUCTION,
                        'owner': 'https://github.com/gregwebs/agent-vm-images/issues/95'},
        'unknown field': {'recipe': BASE_DOCKERFILE, 'instruction': GAP_INSTRUCTION,
                          'owner': source.APT_GAP_OWNER, 'reviewed': True},
        'unknown recipe': {'recipe': 'images/Other/Dockerfile', 'instruction': GAP_INSTRUCTION,
                           'owner': source.APT_GAP_OWNER},
    }.items():
        value = json.loads(json.dumps(template))
        value['apt_open_gaps'] = [entry]
        text = json.dumps(value, indent=2) + '\n'
        try:
            source.parse_inventory(text)
        except source.Refusal as error:
            assert 'apt_open_gaps is closed' in str(error), (label, error)
        else:
            raise AssertionError('accepted a recorded apt gap: ' + label)

    # The same record on disk stops a refresh before it publishes anything.
    value = json.loads(json.dumps(template))
    value['apt_open_gaps'] = [{'recipe': BASE_DOCKERFILE, 'instruction': GAP_INSTRUCTION,
                               'owner': source.APT_GAP_OWNER}]
    (case.repo / INVENTORY).write_text(json.dumps(value, indent=2) + '\n')
    case.commit('record a gap by hand')
    case.snapshot()
    assert case.run(['debian']) == 1, case.output
    assert 'apt_open_gaps is closed' in case.output, case.output
    case.unchanged()
    case.no_work()
    report('T17', 'keeps the closed apt gap record empty and refuses any recorded gap')


ABSOLUTE_STAGER = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
arguments = sys.argv[1:]
out = Path(arguments[arguments.index('--out') + 1]) if '--out' in arguments else None
if out is None:
    print('/etc/passwd')
    raise SystemExit(0)
target = out / 'staged/etc/passwd'
target.parent.mkdir(parents=True)
target.write_text('x')
(out / 'destinations.json').write_text(json.dumps(['/etc/passwd']))
(out / 'summary.txt').write_text('proposal\n')
'''
TRAVERSAL_STAGER = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
arguments = sys.argv[1:]
out = Path(arguments[arguments.index('--out') + 1]) if '--out' in arguments else None
if out is None:
    print('images/../../escape')
    raise SystemExit(0)
(out / 'staged').mkdir(parents=True)
(out / 'destinations.json').write_text(json.dumps(['images/../../escape']))
(out / 'summary.txt').write_text('proposal\n')
'''
EMPTY_STAGER = r'''#!/usr/bin/env python3
raise SystemExit(0)
'''
EXTRA_STAGER = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
arguments = sys.argv[1:]
out = Path(arguments[arguments.index('--out') + 1]) if '--out' in arguments else None
if out is None:
    print('images/parent-pins.json')
    raise SystemExit(0)
for relative in ('images/parent-pins.json', 'examples/layers/extra'):
    target = out / 'staged' / relative
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text('x\n')
(out / 'destinations.json').write_text(json.dumps(['images/parent-pins.json',
                                                   'examples/layers/extra']))
(out / 'summary.txt').write_text('proposal\n')
'''
def t18(root):
    """A local refresh co-publishes the base manifest and the extension default."""
    case = Case(root, 't18')
    case.adopt()
    manifest = (case.repo / BASE_MANIFEST).read_bytes()
    identity = 'sha256:' + sha256_bytes(manifest)
    extension = (case.repo / EXTENSION).read_text()
    assert 'ARG AGENT_VM_BASE_IDENTITY=' + identity in extension, \
        'the committed extension default is not the manifest identity'
    for line in manifest.decode().splitlines():
        entry, path = line.split('  ', 1)
        assert sha256_bytes((case.repo / 'images' / path).read_bytes()) == entry, line
    lines = manifest.decode().splitlines()
    assert [line.split('  ', 1)[1] for line in lines] == \
        sorted(line.split('  ', 1)[1] for line in lines), 'the manifest must be byte-ordered'
    assert not any(line.endswith('  parent-pins.json') for line in lines), \
        'the inventory is metadata the base does not consume'
    # A moved local parent changes the recipes, so the manifest and the default
    # have to move with them in the same transaction.
    case.shift_debian()
    case.commit('shift the Debian fixture')
    assert case.run(['debian']) == 0, case.output
    case.published_only(DEBIAN_DESTINATIONS)
    moved = (case.repo / BASE_MANIFEST).read_bytes()
    assert moved != manifest, 'the moved recipe did not change the manifest'
    moved_identity = 'sha256:' + sha256_bytes(moved)
    assert 'ARG AGENT_VM_BASE_IDENTITY=' + moved_identity in (case.repo / EXTENSION).read_text()
    entry = [line for line in moved.decode().splitlines() if line.endswith('  Dockerfile')][0]
    assert entry.split('  ', 1)[0] == sha256_bytes((case.repo / BASE_DOCKERFILE).read_bytes())
    assert moved_identity in case.output, 'the proposal must name the published identity'
    case.no_work()
    case.commit('refresh the local parent')
    assert case.run(['debian']) == 0, case.output
    assert 'unchanged' in case.output, case.output
    case.unchanged()

    # An untracked collision at either derived destination is refused rather than
    # overwritten; removing the index entry leaves the file in place.
    for name, relative in (('t18b', BASE_MANIFEST), ('t18c', EXTENSION)):
        collision = Case(root, name)
        collision.adopt()
        collision.git('rm', '-q', '--cached', relative)
        collision.snapshot()
        refused(collision.run(['debian']), collision,
                'refusing dirty or untracked input/destination: ' + relative)
        collision.no_work()

    # The default is published before the manifest (sorted destination order), so
    # a failed manifest copy must restore the already-written default too.
    failed = Case(root, 't18d')
    failed.adopt()
    failed.shift_debian()
    failed.commit('shift the Debian fixture')
    before = failed.files()
    # The needle names the staged source, so only the publication copy fails: the
    # restore copies read from the backup directory and must succeed.
    failed.flag({'needle': 'staged/' + BASE_MANIFEST, 'phase': 'publish', 'action': 'fail',
                 'truncate': True})
    assert failed.run(['debian']) == 1, failed.output
    assert 'restoring attempted files inclusively' in failed.output, failed.output
    after = failed.files()
    for relative in (BASE_MANIFEST, EXTENSION, BASE_DOCKERFILE):
        assert after[relative] == before[relative], 'not restored: ' + relative
    failed.no_work()
    (failed.root / 'fail-copy').unlink()
    assert failed.run(['debian']) == 0, failed.output
    failed.published_only(DEBIAN_DESTINATIONS)

    # A signal during the same publication restores both derived destinations too.
    interrupted = Case(root, 't18e')
    interrupted.adopt()
    interrupted.shift_debian()
    interrupted.commit('shift the Debian fixture')
    before = interrupted.files()
    interrupted.flag({'needle': 'staged/' + BASE_MANIFEST, 'phase': 'publish', 'action': 'TERM'})
    assert interrupted.run(['debian']) == 143, interrupted.output
    assert 'publication interrupted; sources restored' in interrupted.output, interrupted.output
    after = interrupted.files()
    for relative in (BASE_MANIFEST, EXTENSION, BASE_DOCKERFILE):
        assert after[relative] == before[relative], 'not restored: ' + relative
    interrupted.no_work(retained=True)
    report('T18', 'co-publishes the base manifest and the extension default')


PARTIAL_STAGER = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
arguments = sys.argv[1:]
out = Path(arguments[arguments.index('--out') + 1]) if '--out' in arguments else None
if out is None:
    print('images/parent-pins.json')
    print('examples/layers/README.md')
    raise SystemExit(0)
target = out / 'staged/images/parent-pins.json'
target.parent.mkdir(parents=True)
target.write_text('{}\n')
(out / 'destinations.json').write_text(json.dumps(['images/parent-pins.json']))
(out / 'summary.txt').write_text('proposal\n')
'''
MALFORMED_STAGER = r'''#!/usr/bin/env python3
import sys
from pathlib import Path
arguments = sys.argv[1:]
out = Path(arguments[arguments.index('--out') + 1]) if '--out' in arguments else None
if out is None:
    print('images/parent-pins.json')
    raise SystemExit(0)
out.mkdir(parents=True)
(out / 'destinations.json').write_text('{not json')
(out / 'summary.txt').write_text('proposal\n')
'''


def main():
    root = Path(tempfile.mkdtemp(prefix='parent-pins-tests.'))
    try:
        for test in (t1, t2, t3, t3b, t4, t5, t6, t7, t8, t9, t10, t11, t12, t13, t14, t15,
                     t15b, t16, t17, t18):
            test(root)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print('parent-pins: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
