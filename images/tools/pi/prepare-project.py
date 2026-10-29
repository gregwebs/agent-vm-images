#!/usr/bin/env python3
"""The one owning preparation process for the Pi and Pi-bridge pnpm projects.

Both migrated projects share one routine: guard the environment, mint a private
`pnpm-project-*` candidate stage, get the manager argv from the shared
capabilities, generate a lock under the reviewed workspace, ask the Go helper to
validate the candidate against its reviewed predecessor (or, for a first
adoption, against nothing), pin only after the comparison is accepted, run a
cold frozen validation, and publish exactly four inputs.

Nothing here parses YAML, decides graph policy or classifies manager transport:
those are the Go helper's and the shell installers' jobs. This module owns only
*sequencing*, so no caller can reorder "compare, then pin" or skip the frozen
validation by invoking a lower-level verb.

Two entry points exist:

  prepare TARGET VERSION [--refresh-lock]
      A project that already has its four reviewed inputs. `pnpm-lock-candidate
      graph` compares the candidate with that prefix's pinned graph, so the
      reviewed predecessor is ordinary input rather than a caller argument.

      A changed Pi selection has two possible sources and the *authenticated
      archive decides which*: the exact tarball is fetched through the classified
      download contract, its SHA-512 is checked against the registry's own SRI
      before extraction, and then either

        * the archive ships `npm-shrinkwrap.json` -- its upstream graph is the
          authority. The only missing SRIs completed are the five legacy
          siblings', each from exact metadata and a verified archive; the
          shrinkwrap is synthesized into one npm-shaped project lock, imported by
          the pinned pnpm under the reviewed workspace, and the imported graph is
          compared with that expectation and with the prefix under the
          shrinkwrap-authority comparator. There is no range-resolve fallback,
          and no canned lock for a particular version lives here; or
        * the archive ships none -- there is nothing to convert, so the ordinary
          all-hashed range resolution under the same workspace and policy runs.

  adopt PROPOSAL [--baseline REF]
      The maintainer-only first conversion of an npm-locked project. There is no
      pinned pnpm predecessor, so it uses the no-predecessor `adoption-graph`
      verb, imports the fixed baseline npm lock read-only from Git, and writes an
      external proposal; it never mutates the source checkout and is not
      reachable from the ordinary preparation path.

The candidate stage is a fixed shape: `mkdtemp` of `pnpm-project-*` under the
platform's canonical system temporary directory, holding a 0700 `candidate`
child with exactly three files, plus the inherited directory descriptor that
gives `recipe-contract` its own authority. Logs, private state and the store live
in siblings, never inside `candidate`.
"""
import argparse
from dataclasses import dataclass
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time

sys.dont_write_bytecode = True

SHARED = Path(__file__).resolve().parents[1] / 'pnpm-project.py'
# The two build-only helpers of this route, mounted beside this driver. Neither
# is ever COPYed into runtime: the converter turns an authenticated archive into
# pnpm import input, and the extractor owns bounded safe extraction and the SRI
# rehash. Both are resolved from this file's own directory, never from PATH or a
# caller-selected path.
CONVERTER = Path(__file__).resolve().with_name('import-shrinkwrap.mjs')
EXTRACTOR = Path(__file__).resolve().with_name('archive.py')
REVIEWED_NORMALISATIONS = Path(__file__).resolve().with_name('reviewed-normalisations.json')
# The one Node the base owns; `/usr/bin/node` is the image's declared toolchain
# alias of that binary (the base manager distribution uses the same path).
NODE = Path('/usr/bin/node')

PIN_FILE = 'pnpm-inputs.sha256'
CANDIDATE_INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml')
# The candidate stage is a *system* temporary directory: recipe-contract checks
# that canonical spelling itself, so the driver must mint exactly that shape.
SYSTEM_TEMP = {'darwin': Path('/private/tmp'), 'linux': Path('/tmp')}
CANDIDATE_TOKEN = 'pnpm-project-'
EXACT_VERSION = re.compile(
    r'^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
    r'(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
    r'(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
    r'(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$')
# The baseline a maintainer conversion reads. A commit, not a tag or a branch, so
# the same npm inputs stay readable after the source locks are deleted.
BASELINE = 'bc1b2df'

# Closed per-consumer facts. The manifest is built from the baseline manifest
# plus these values; the workspace text is the reviewed exact map the Go policy
# already enforces, kept verbatim so generation cannot drift into a convenient
# local variant.
PROJECTS = {
    'pi': {
        'root': '@earendil-works/pi-coding-agent',
        'description': ('Pinned Pi coding agent for the agent-vm guest image. The version '
                        'below is the pin; the base pnpm installs the reviewed frozen graph '
                        'with scripts disabled and asserts `pi --version` matches it.'),
        'workspace': (
            'packages: [.]\n'
            'autoInstallPeers: true\n'
            'strictPeerDependencies: false\n'
            'nodeLinker: isolated\n'
            'hoist: false\n'
            'enableGlobalVirtualStore: false\n'
            'pmOnFail: error\n'
            'sideEffectsCache: false\n'
            'allowBuilds: {}\n'
            'preferSymlinkedExecutables: true\n'
            'extendNodePath: false\n'
            "trustPolicyExclude: ['undici-types@6.21.0']\n"),
    },
    'bridge': {
        'root': 'pi-claude-bridge',
        'description': ('Pinned Pi extension packages for the agent-vm guest image. The base '
                        'pnpm installs the reviewed frozen graph with scripts disabled into '
                        '/opt/agent-vm/pi-packages; the wrapper loads pi-claude-bridge from it '
                        'as a second --extension. The reviewed workspace omits only the SDK '
                        'platform packages, because Pi supplies the SDK JavaScript itself.'),
        'workspace': (
            'packages: [.]\n'
            'autoInstallPeers: false\n'
            'strictPeerDependencies: false\n'
            'nodeLinker: hoisted\n'
            'enableGlobalVirtualStore: false\n'
            'pmOnFail: error\n'
            'sideEffectsCache: false\n'
            'allowBuilds: {}\n'
            'extendNodePath: false\n'
            "ignoredOptionalDependencies: ['@anthropic-ai/claude-agent-sdk-*']\n"),
    },
}

# The five age/trust/script defaults, named once. They are exported into every
# manager child *and* repeated on the command line, so neither an inherited
# environment nor a future default can change what was reviewed.
POLICY_ENVIRONMENT = {
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '1440',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'true',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'false',
    'PNPM_CONFIG_TRUST_POLICY': 'no-downgrade',
    'PNPM_CONFIG_IGNORE_SCRIPTS': 'true',
}
POLICY_FLAGS = ('--config.minimum-release-age=1440',
                '--config.minimum-release-age-strict=true',
                '--config.minimum-release-age-ignore-missing-time=false',
                '--config.trust-policy=no-downgrade')
REGISTRY_FLAGS = ('--ignore-pnpmfile', '--pm-on-fail=error',
                  '--config.update-notifier=false',
                  '--registry=https://registry.npmjs.org')
# `pnpm import` rejects the install-style spellings of these settings, so the
# accepted `--config.*` forms are used for it and the install template for real
# trees. Both carry the same five reviewed policy values; this is not an
# exemption.
IMPORT_FLAGS = (*POLICY_FLAGS, '--config.ignore-scripts=true',
                '--config.registry=https://registry.npmjs.org',
                '--config.ignore-pnpmfile=true', '--pm-on-fail=error',
                '--config.update-notifier=false')
INSTALL_FLAGS = (*POLICY_FLAGS, '--ignore-scripts', *REGISTRY_FLAGS)
VERSION_TIMEOUT = 20
METADATA_TIMEOUT = 60
RUN_PNPM_TIMEOUT = 660
DOWNLOAD_TIMEOUT = 660
CONVERT_TIMEOUT = 120

# The authenticated selected-archive route. `LEGACY_SIBLINGS` is the closed set
# of same-version family packages whose SRI the published Pi 0.87.0 shrinkwrap
# omits; nothing outside it may be completed, and nothing inside it may be
# completed from anywhere but exact registry metadata for the selected version.
PI_SCOPE = '@earendil-works'
LEGACY_SIBLINGS = ('chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui')
REGISTRY_TARBALL = 'https://registry.npmjs.org/{name}/-/{base}-{version}.tgz'
# `pnpm view` in 11.11.0 rejects the install-style spellings of these settings,
# so the metadata query uses the `--config.*` forms and the registry argument;
# the five owning PNPM_CONFIG_* defaults still travel in the environment.
METADATA_FLAGS = ('--pm-on-fail=error', '--config.update-notifier=false',
                  '--config.ignore-pnpmfile=true',
                  '--registry=https://registry.npmjs.org/')
ROUTE_RESOLVE = 'resolve'
ROUTE_SHRINKWRAP = 'shrinkwrap'


def load_shared():
    name = 'pnpm_project'
    module = sys.modules.get(name)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(name, SHARED)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


def load_extractor():
    """The bounded archive helper, loaded as a module (never a shell-out).

    `archive.py` owns both halves this route needs: the whole-archive preflight
    before any member is written, and the SHA-512 rehash of the retained tarball.
    Importing it keeps the extraction policy in exactly one place.
    """
    name = 'pi_archive'
    module = sys.modules.get(name)
    if module is not None:
        return module
    spec = importlib.util.spec_from_file_location(name, EXTRACTOR)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


project = load_shared()


class PreparationError(Exception):
    """A handled preparation failure: status 1, diagnostic on stderr."""


class Interrupted(BaseException):
    """A signal that must reach the caller with its own status, not as status 1."""

    def __init__(self, signum):
        super().__init__('interrupted by signal ' + str(signum))
        self.signum = signum


@dataclass(frozen=True)
class CandidateStage:
    """The private scratch and the descriptor that authenticates it.

    Minted only by `mint_stage` in this process. The descriptor is inherited by
    the Go helper through `pass_fds`, so the stage's identity is decided by the
    kernel instead of by the spelling of a path this module also chose.
    """
    scratch: Path
    directory_fd: int
    consumer: str

    @property
    def candidate(self):
        return self.scratch / 'candidate'

    @property
    def inputs(self):
        return tuple(self.candidate / name for name in CANDIDATE_INPUTS)


def system_temp():
    try:
        return SYSTEM_TEMP[platform.system().lower()]
    except KeyError:
        raise PreparationError('no canonical system temporary directory for this platform')


def exact_version(text):
    if '\n' in text or not EXACT_VERSION.match(text):
        raise PreparationError('not an exact npm version: ' + repr(text))
    return text


def sha256(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def write_private(path, body, mode=0o600):
    with open(path, 'xb') as handle:
        handle.write(body)
    os.chmod(path, mode)


def mint_stage(consumer):
    """Create the candidate scratch and register its descriptor.

    `mkdtemp` already gives 0700; the `candidate` child is 0700 as well and only
    its three inputs are readable, because the Go helper refuses any other mode
    or entry.
    """
    scratch = Path(tempfile.mkdtemp(prefix=CANDIDATE_TOKEN, dir=str(system_temp()))).resolve()
    if os.stat(scratch).st_uid != os.getuid():
        raise PreparationError('stage must be invocation-owned')
    os.chmod(scratch, 0o700)
    (scratch / 'candidate').mkdir(mode=0o700)
    descriptor = os.open(scratch, os.O_RDONLY | getattr(os, 'O_DIRECTORY', 0))
    if descriptor <= 2:
        os.close(descriptor)
        raise PreparationError('stage descriptor must be above the stdio range')
    return CandidateStage(scratch, descriptor, consumer)


def bootstrap_stage(consumer):
    """Create the invocation's work parent for a first import.

    A first import has no reviewed inputs, so it is authorized by the bootstrap
    stage rather than by a mutation capability. The stage is the consumer's
    private child of the *consumer* scratch parent (`/tmp/pi-*`), which the
    shared authorization recognises as production, or of a fixture's own
    authorized root. It is deliberately distinct from the candidate scratch: the
    Go helper requires that one to be a fixed-shape `pnpm-project-*` system
    temporary directory, while this one carries the invocation's logs, store and
    staged project.
    """
    token = project.CONSUMERS[consumer].scratch_token
    fixture = os.environ.get('AGENT_VM_FIXTURE_ROOT') or os.environ.get('AGENT_VM_SCRATCH_ROOT')
    parent = project.authorized_root() if fixture else project.CONSUMERS[consumer].scratch_parent
    stage = Path(tempfile.mkdtemp(prefix=token, dir=str(parent))).resolve()
    os.chmod(stage, 0o700)
    return stage


def discard_stage(stage, keep=False):
    """Remove the stage unless the caller still needs the evidence it holds."""
    if keep:
        return
    try:
        os.close(stage.directory_fd)
    except OSError:
        pass
    shutil.rmtree(stage.scratch)


def child_environment(scratch):
    """The closed environment every manager and helper child receives.

    The caller's HOME, XDG directories, TMPDIR and any inherited PNPM_* setting
    are replaced, never merged: an ambient store or config file would be state
    this invocation neither owns nor cleans.
    """
    environment = {'PATH': '/usr/local/bin:/usr/bin:/bin', 'LANG': 'C.UTF-8',
                   'LC_ALL': 'C.UTF-8', **POLICY_ENVIRONMENT}
    for name, key in (('home', 'HOME'), ('config', 'XDG_CONFIG_HOME'),
                      ('cache', 'XDG_CACHE_HOME'), ('data', 'XDG_DATA_HOME'),
                      ('state', 'XDG_STATE_HOME'), ('tmp', 'TMPDIR')):
        directory = Path(scratch) / name
        directory.mkdir(mode=0o700, exist_ok=True)
        environment[key] = str(directory)
    return environment


def stop_child(process):
    """TERM the child's whole session, escalate to KILL, then reap it.

    The child runs in its own session so no manager can leave a surviving
    grandchild holding the store — which also means it does not receive the signal
    this process received, and no orphan reaper will stop it: the only thing that
    ends it is this function. Both the deadline and an interrupted wait therefore
    use it, and further signals are ignored while it runs so a second TERM cannot
    abandon the escalation halfway and leave the child holding a scratch that is
    about to be deleted.
    """
    previous = {}
    for item in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        previous[item] = signal.signal(item, signal.SIG_IGN)
    try:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            return process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        return process.wait()
    finally:
        for item, handler in previous.items():
            signal.signal(item, handler)


def run(argv, *, cwd, environment, timeout, label, scratch, pass_fds=()):
    """One bounded child: TERM at the deadline, KILL five seconds later.

    The child runs in its own session so a killed manager cannot leave a
    surviving grandchild holding the store. Both captures are invocation-owned.
    An interrupted wait is the same situation as an expired one: the child is in
    its own session, so raising out of `wait` without stopping it would leave a
    manager running against a scratch this invocation then deletes, so every
    exceptional exit stops and reaps the child before it propagates.
    """
    logs = Path(scratch) / 'logs'
    logs.mkdir(mode=0o700, exist_ok=True)
    out_path, err_path = logs / (label + '.out'), logs / (label + '.err')
    started = time.time()
    with out_path.open('wb') as out, err_path.open('wb') as err:
        process = subprocess.Popen([str(word) for word in argv], cwd=str(cwd),
                                   env=environment, stdout=out, stderr=err,
                                   start_new_session=True, pass_fds=tuple(pass_fds))
        try:
            status = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            stop_child(process)
            raise PreparationError(f'{label}: child did not exit within {timeout}s')
        except BaseException:
            stop_child(process)
            raise
    outcome = {'label': label, 'argv': [str(word) for word in argv], 'cwd': str(cwd),
               'seconds': round(time.time() - started, 3), 'status': status,
               'stdout': str(out_path), 'stderr': str(err_path)}
    with (logs / 'commands.jsonl').open('a') as handle:
        handle.write(json.dumps(outcome) + '\n')
    return outcome


def contract_call(contract, argv, *, cwd, environment, scratch, label, timeout=300,
                  pass_fds=()):
    """Run one Go helper invocation and return its stdout text."""
    outcome = run([Path(contract) / 'recipe-contract', *argv], cwd=cwd,
                  environment=environment, timeout=timeout, label=label,
                  scratch=scratch, pass_fds=pass_fds)
    if outcome['status'] != 0:
        detail = (Path(outcome['stderr']).read_text(errors='replace').strip()
                  or Path(outcome['stdout']).read_text(errors='replace').strip())
        raise PreparationError(f'{label}: helper failed: {detail}')
    return Path(outcome['stdout']).read_text()


def manager_version(manager, environment, scratch):
    outcome = run([*manager, '--version', '--pm-on-fail=error',
                   '--config.update-notifier=false', '--ignore-pnpmfile',
                   '--registry=https://registry.npmjs.org'],
                  cwd=scratch, environment=environment, timeout=VERSION_TIMEOUT,
                  label='manager-version', scratch=scratch)
    if outcome['status'] != 0:
        raise PreparationError('manager version check failed')
    version = Path(outcome['stdout']).read_text().strip()
    if version != '11.11.0':
        raise PreparationError('manager is not the reviewed 11.11.0 launcher: ' + version)
    return version


def run_pnpm(contract, argv, *, cwd, environment, scratch, label):
    """Run one resolving/install/import call inside the reviewed hard envelope."""
    receipt = Path(scratch) / 'logs' / (label + '.ndjson')
    receipt.parent.mkdir(mode=0o700, exist_ok=True)
    outcome = run(['sh', Path(contract) / 'run-pnpm.sh', receipt, '--', *argv], cwd=cwd,
                  environment=environment, timeout=RUN_PNPM_TIMEOUT, label=label,
                  scratch=scratch)
    if outcome['status'] != 0:
        if outcome['status'] in (130, 143, 129):
            raise Interrupted(outcome['status'] - 128)
        raise PreparationError(label + ': pnpm refused the call; no transport classification')
    return outcome


def import_argv(manager, store):
    return [*manager, 'import', *IMPORT_FLAGS, '--config.package-import-method=copy',
            '--config.store-dir=' + str(store)]


def install_argv(manager, store, *, frozen, lockfile_only=False):
    argv = [*manager, 'install', '--prod']
    if frozen:
        argv.append('--frozen-lockfile')
    if lockfile_only:
        argv = [*manager, 'install', '--lockfile-only', '--no-frozen-lockfile']
    return [*argv, *INSTALL_FLAGS, '--package-import-method=copy',
            '--store-dir', str(store)]


# --- the authenticated selected-archive route -------------------------------
#
# A changed Pi selection is not equivalent to a fresh range resolution: an
# archive that publishes `npm-shrinkwrap.json` states its own transitive graph,
# and pnpm does not read a dependency shrinkwrap. The selection's own bytes
# therefore decide the route. Neither branch is reachable for an equal,
# unrefreshed request, and the shrinkwrap branch never falls back to a range
# resolve.

def node_binary(capability):
    """The one Node this route may execute.

    Production is the reviewed absolute binary (never a PATH lookup). A fixture
    may name its own executable for hermetic driver tests, exactly as it may name
    a fake manager; the file must be a fixture-owned regular executable, and the
    production route refuses the variable outright.
    """
    if capability.production:
        return str(NODE)
    fake = os.environ.get('AGENT_VM_FIXTURE_NODE')
    if not fake:
        return str(NODE)
    executable = project.absolute(fake)
    project.no_redirect(executable, capability.root)
    project.regular(executable)
    if not os.access(executable, os.X_OK):
        raise PreparationError('fixture node must be executable')
    return str(executable)


def view(manager, environment, scratch, selector, label):
    """One bounded exact metadata query under the owning policy defaults."""
    argv = [*manager, 'view', selector, 'name', 'version', 'dist.integrity',
            'dist.tarball', '--json', *METADATA_FLAGS]
    outcome = run(argv, cwd=scratch, environment=environment, timeout=METADATA_TIMEOUT,
                  label='metadata-' + label, scratch=scratch)
    if outcome['status'] != 0:
        raise PreparationError(f'metadata-{label}: pnpm view failed for {selector}')
    try:
        record = json.loads(Path(outcome['stdout']).read_text())
    except ValueError as error:
        raise PreparationError(f'metadata-{label}: unreadable response: {error}')
    if not isinstance(record, dict):
        raise PreparationError(f'metadata-{label}: response is not an object')
    return record


def archive_facts(record, name, version, label):
    """Validate one registry answer: exact identity, sha512 SRI, exact npmjs tarball."""
    if record.get('name') != name or record.get('version') != version:
        raise PreparationError(f'{label}: registry answered {record.get("name")}'
                               f'@{record.get("version")}, not {name}@{version}')
    integrity = record.get('dist.integrity')
    if not isinstance(integrity, str) or not integrity.startswith('sha512-'):
        raise PreparationError(f'{label}: no sha512 dist.integrity for {name}@{version}')
    expected = REGISTRY_TARBALL.format(name=name, base=name.rsplit('/', 1)[-1], version=version)
    tarball = record.get('dist.tarball')
    if tarball != expected:
        raise PreparationError(f'{label}: {tarball!r} is not the exact npmjs tarball {expected}')
    return integrity, tarball


def download(contract, url, destination, environment, scratch, label):
    """Fetch one archive through the classified contract; no degraded route.

    Preparation is a precondition, not an optional install step: a transport
    failure here is as hard as a checksum failure, because the alternative is
    converting a selection from bytes nobody authenticated.
    """
    logs = Path(scratch) / 'logs'
    logs.mkdir(mode=0o700, exist_ok=True)
    child = dict(environment)
    child['AGENT_VM_TRANSPORT_RECEIPT'] = str(logs / (label + '.receipt'))
    outcome = run(['sh', Path(contract) / 'download.sh', url, destination], cwd=scratch,
                  environment=child, timeout=DOWNLOAD_TIMEOUT, label=label, scratch=scratch)
    if outcome['status'] != 0:
        raise PreparationError(f'{label}: the selected archive could not be downloaded; '
                               'preparation has no degraded or range-resolve route')


def selected_archive(contract, manager, environment, work, root, version):
    """Authenticate the selected Pi archive and decide the conversion route.

    The tarball is verified against the registry's own SRI *before* extraction,
    and the extraction itself is bounded and unsafe-input rejecting. The route is
    then whatever the authenticated bytes say: an archive carrying
    `npm-shrinkwrap.json` has an upstream graph to honour, one without it has
    nothing to convert.
    """
    archiver = load_extractor()
    integrity, tarball = archive_facts(view(manager, environment, work, f'{root}@{version}',
                                            'selected'), root, version, 'selected')
    directory = work / 'selected'
    directory.mkdir(mode=0o700)
    tarball_path = directory / f'{root.rsplit("/", 1)[-1]}-{version}.tgz'
    download(contract, tarball, tarball_path, environment, work, 'selected-download')
    try:
        archiver.verify_integrity(tarball_path, integrity)
        extracted = directory / 'package'
        archiver.extract(tarball_path, extracted)
    except archiver.ArchiveError as error:
        raise PreparationError('the selected archive was refused: ' + str(error))
    manifest_path = extracted / 'package.json'
    if manifest_path.is_symlink() or not manifest_path.is_file():
        raise PreparationError('the selected archive has no regular package.json')
    try:
        manifest = json.loads(manifest_path.read_bytes())
    except ValueError as error:
        raise PreparationError('the selected archive manifest is unreadable: ' + str(error))
    if not isinstance(manifest, dict) or manifest.get('name') != root \
            or manifest.get('version') != version:
        raise PreparationError('the selected archive manifest is not ' + f'{root}@{version}')
    shrinkwrap = extracted / 'npm-shrinkwrap.json'
    present = os.path.lexists(shrinkwrap)
    if present and (shrinkwrap.is_symlink() or not shrinkwrap.is_file()):
        raise PreparationError('npm-shrinkwrap.json is not a regular file in the archive')
    return {'name': root, 'version': version, 'integrity': integrity, 'tarball': tarball,
            'manifest': manifest_path, 'shrinkwrap': shrinkwrap if present else None,
            'route': ROUTE_SHRINKWRAP if present else ROUTE_RESOLVE}


def canonical_json(contract, source, destination, environment, scratch, label):
    """Read one authenticated JSON document through the bounded syntax adapter.

    `recipe-contract pnpm-json` owns duplicate-key rejection, depth, size and
    encoding bounds, so a manifest or shrinkwrap with two `packages` keys can
    never be read as one by this driver.
    """
    text = contract_call(contract, ['pnpm-json', str(source)], cwd=scratch,
                         environment=environment, scratch=scratch, label=label, timeout=120)
    write_private(destination, text.encode(), 0o600)
    try:
        return json.loads(text)
    except ValueError as error:
        raise PreparationError(f'{label}: canonical JSON is unreadable: {error}')


def supplemental_receipts(contract, manager, environment, work, shrinkwrap, version):
    """Complete exactly the legacy five missing SRIs, from verified archives.

    Nothing else may be completed: a missing SRI on any other package, on a
    sibling at another version, or on a subset of the five (a four-sibling graph)
    is an upstream change that needs review, not a hash to invent. Each receipt
    exists only after its own archive was downloaded and rehashed, so the SRI
    admitted to the conversion is one this invocation proved.
    """
    archiver = load_extractor()
    packages = shrinkwrap.get('packages') if isinstance(shrinkwrap, dict) else None
    if not isinstance(packages, dict):
        raise PreparationError('the selected shrinkwrap has no packages object')
    missing = []
    for location, record in packages.items():
        if location == '':
            continue
        if not isinstance(record, dict):
            raise PreparationError('shrinkwrap ' + str(location) + ' is not an object')
        if 'integrity' in record:
            continue
        name = str(location).split('node_modules/')[-1]
        missing.append((str(location), name, record.get('version')))
    if not missing:
        return []
    expected = {f'{PI_SCOPE}/{base}' for base in LEGACY_SIBLINGS}
    names = [name for _, name, _ in missing]
    if len(set(names)) != len(names) or set(names) != expected:
        raise PreparationError('the selected shrinkwrap omits integrity for '
                               + ', '.join(sorted(set(names)))
                               + '; only the exact legacy five siblings at the selected '
                                 'version may be completed')
    receipts = []
    for location, name, record_version in missing:
        if record_version != version:
            raise PreparationError(f'{location} is {record_version}, not the selected {version}')
        base = name.rsplit('/', 1)[-1]
        label = 'sibling-' + base
        integrity, tarball = archive_facts(view(manager, environment, work, f'{name}@{version}',
                                                label), name, version, label)
        directory = work / 'siblings'
        directory.mkdir(mode=0o700, exist_ok=True)
        tarball_path = directory / f'{base}-{version}.tgz'
        download(contract, tarball, tarball_path, environment, work, label + '-download')
        try:
            archiver.verify_integrity(tarball_path, integrity)
            archiver.extract(tarball_path, directory / f'{base}-extracted')
        except archiver.ArchiveError as error:
            raise PreparationError(f'{label}: the sibling archive was refused: {error}')
        receipts.append({'name': name, 'version': version, 'integrity': integrity,
                         'resolved': tarball,
                         'origin': f'pnpm view {name}@{version} dist.integrity',
                         'archive': 'sha512 verified before extraction'})
    return receipts


def converter(capability, stage, environment, argv, label):
    """Run the build-only conversion helper and return its two captured streams."""
    outcome = run([node_binary(capability), str(CONVERTER), *argv], cwd=stage.scratch,
                  environment=environment, timeout=CONVERT_TIMEOUT, label=label,
                  scratch=stage.scratch)
    return outcome, Path(outcome['stdout']).read_text(), Path(outcome['stderr']).read_text()


def shrinkwrap_candidate(contract, capability, manager, store, workspace, work, manifest_bytes,
                         selected, stage, environment):
    """Synthesize the import input, import it, and return the expected graph.

    The synthesized npm-shaped lock is written *only* into the staging project,
    copied to read-only evidence, and removed again before the candidate is
    validated: the candidate itself may never carry an npm lock.
    """
    resolve = stage.scratch / 'resolve'
    resolve.mkdir(mode=0o700)
    inputs = stage.scratch / 'inputs'
    inputs.mkdir(mode=0o700)
    evidence = stage.scratch / 'evidence'
    evidence.mkdir(mode=0o700)
    manifest = canonical_json(contract, selected['manifest'], inputs / 'archive-manifest.json',
                              environment, stage.scratch, 'archive-manifest')
    shrinkwrap = canonical_json(contract, selected['shrinkwrap'],
                               inputs / 'authenticated-shrinkwrap.json', environment,
                               stage.scratch, 'archive-shrinkwrap')
    receipts = supplemental_receipts(contract, manager, environment, work, shrinkwrap,
                                     manifest.get('version'))
    root_resolution = {'name': manifest.get('name'), 'version': manifest.get('version'),
                       'integrity': selected['integrity'], 'resolved': selected['tarball']}
    write_private(inputs / 'root-archive.json',
                  (json.dumps(root_resolution, indent=2) + '\n').encode(), 0o600)
    write_private(inputs / 'sibling-archives.json',
                  (json.dumps(receipts, indent=2) + '\n').encode(), 0o600)
    outcomes, stdout, stderr = converter(capability, stage, environment, [
        'synthesize',
        '--manifest', str(inputs / 'archive-manifest.json'),
        '--shrinkwrap', str(inputs / 'authenticated-shrinkwrap.json'),
        '--root', str(inputs / 'root-archive.json'),
        '--siblings', str(inputs / 'sibling-archives.json')], 'shrinkwrap-synthesize')
    if outcomes['status'] != 0:
        raise PreparationError('shrinkwrap-synthesize: the authenticated shrinkwrap was refused: '
                               + stderr.strip()[-600:])
    write_private(evidence / 'synthesized-package-lock.json', stdout.encode(), 0o644)
    write_private(evidence / 'completed-sris.txt', stderr.encode(), 0o644)
    write_private(resolve / 'package-lock.json', stdout.encode(), 0o600)
    write_private(resolve / 'package.json', manifest_bytes, 0o644)
    write_private(resolve / 'pnpm-workspace.yaml', workspace, 0o644)
    run_pnpm(contract, import_argv(manager, store), cwd=resolve, environment=environment,
             scratch=stage.scratch, label='import')
    # Import input only: kept as evidence, then removed before candidate capture.
    shutil.copyfile(resolve / 'package-lock.json', evidence / 'import-input-package-lock.json')
    (resolve / 'package-lock.json').unlink()
    for name in ('npm-shrinkwrap.json', '.npmrc', PIN_FILE):
        if os.path.lexists(resolve / name):
            raise PreparationError('pnpm import left an unexpected ' + name)
    if (resolve / 'node_modules').exists():
        raise PreparationError('pnpm import created node_modules')
    stage_candidate(stage, {name: resolve / name for name in CANDIDATE_INPUTS})
    return inputs, evidence, receipts


def import_fidelity(capability, stage, environment, report, inputs, evidence, reviewed):
    """Compare the imported graph with the pre-import expected graph.

    The expected graph is the synthesized lock itself: root archive with the
    authenticated SRI, every nonroot occurrence under the selected Pi, every
    retained SRI byte-for-byte. The comparator reports representation
    normalisations separately and rejects anything else; a rejection here stops
    preparation instead of writing pins or a fallback lock.
    """
    graph = stage.scratch / 'evidence' / 'imported-graph.json'
    write_private(graph, (json.dumps(report['graph']) + '\n').encode(), 0o600)
    allowed = inputs / 'sibling-archives.json'
    review = inputs / 'reviewed-normalisations.json'
    write_private(review, (json.dumps(reviewed, indent=2) + '\n').encode(), 0o600)
    outcome, stdout, stderr = converter(capability, stage, environment, [
        'validate', '--expect', str(evidence / 'synthesized-package-lock.json'),
        '--graph', str(graph), '--reviewed', str(review),
        '--allowed-missing', str(allowed)], 'shrinkwrap-delta')
    write_private(evidence / 'shrinkwrap-delta.json', stdout.encode(), 0o644)
    try:
        delta = json.loads(stdout)
    except ValueError:
        raise PreparationError('shrinkwrap-delta: the comparator returned no result: '
                               + stderr.strip()[-400:])
    if outcome['status'] != 0 or not delta.get('accepted'):
        codes = sorted({entry.get('code', 'unknown') for entry in delta.get('rejected') or []})
        raise PreparationError('shrinkwrap-delta: the imported graph is not the authenticated '
                               'shrinkwrap graph: ' + ', '.join(codes or [stderr.strip()[-200:]]))
    return delta


def bind_resolved_root(report, selected):
    """Require the resolved graph's root to be the archive that was authenticated.

    The no-shrinkwrap route authenticates the selected archive's SRI and then
    resolves ranges against the registry, so the two can disagree: a registry
    answering a second tarball for the same exact version (equivocation) would have
    that second archive's SRI pinned here. If that archive also carried
    `npm-shrinkwrap.json`, its graph would be pinned without ever passing the
    shrinkwrap comparator, which is the fidelity this route exists to keep. Root
    identity alone is not enough for the same reason.
    """
    graph = report.get('graph') or {}
    records = [record for record in graph.get('packages') or []
               if isinstance(record, dict)
               and (record.get('id') or {}).get('name') == selected['name']
               and (record.get('id') or {}).get('version') == selected['version']]
    if len(records) != 1:
        raise PreparationError('the resolved graph has '
                               f'{len(records)} records for {selected["name"]}'
                               f'@{selected["version"]}, not exactly one')
    integrity = records[0].get('integrity')
    if integrity != selected['integrity']:
        raise PreparationError('the resolved root archive '
                               f'{selected["name"]}@{selected["version"]} carries {integrity}, '
                               f"not the authenticated {selected['integrity']}")


def reviewed_normalisations(version):
    """The representation normalisations an operator reviewed for this selection.

    The converter reports every normalisation it finds and refuses the ones it was
    not told to expect, so the driver must supply a review for exactly this
    selection. An empty list is the honest answer for a version nobody has
    reviewed: pnpm output that is not byte-fidelity then stops preparation for
    review instead of entering the pinned graph.
    """
    record = read_json(REVIEWED_NORMALISATIONS)
    if record.get('schema') != 1:
        raise PreparationError(f'{REVIEWED_NORMALISATIONS}: unsupported schema')
    selections = record.get('selections')
    if not isinstance(selections, dict):
        raise PreparationError(f'{REVIEWED_NORMALISATIONS}: no selections object')
    entry = selections.get(f"{PROJECTS['pi']['root']}@{version}") or {}
    normalisations = entry.get('normalisations', [])
    if not isinstance(normalisations, list):
        raise PreparationError(f'{REVIEWED_NORMALISATIONS}: {version} normalisations is not a list')
    for item in normalisations:
        if not isinstance(item, dict) or not isinstance(item.get('code'), str) \
                or not isinstance(item.get('detail'), dict):
            raise PreparationError(f'{REVIEWED_NORMALISATIONS}: a {version} normalisation is malformed')
    return normalisations


def read_json(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError) as error:
        raise PreparationError(f'{path}: unreadable JSON: {error}')


def baseline_inputs(consumer, reference):
    """The fixed baseline npm inputs, read read-only from Git.

    The working tree is never consulted for these: after activation the source
    npm lock is gone, and a caller must not be able to substitute a convenient
    local lock for the reviewed baseline.
    """
    root = project.checkout_root()
    sources = {'pi': 'images/tools/pi', 'bridge': 'images/tools/pi/bridge'}[consumer]
    resolved = subprocess.run(['git', '-C', str(root), 'rev-parse', '--verify',
                               reference + '^{commit}'], capture_output=True, text=True)
    if resolved.returncode != 0:
        raise PreparationError('baseline is not a commit in this checkout: ' + reference)
    commit = resolved.stdout.strip()
    files = {}
    for name in ('package.json', 'package-lock.json'):
        blob = subprocess.run(['git', '-C', str(root), 'show',
                               f'{commit}:{sources}/{name}'], capture_output=True)
        if blob.returncode != 0:
            raise PreparationError(f'{commit}:{sources}/{name} is not readable')
        files[name] = blob.stdout
    return commit, files


def final_manifest(consumer, baseline_manifest, version):
    """The reviewed pnpm manifest: baseline identity, one exact dependency, manager pin."""
    facts = PROJECTS[consumer]
    entry = json.loads(baseline_manifest)
    dependencies = entry.get('dependencies') or {}
    if list(dependencies) != [facts['root']]:
        raise PreparationError('baseline manifest must declare exactly ' + facts['root'])
    if dependencies[facts['root']] != version:
        raise PreparationError('baseline manifest selects '
                               f"{dependencies[facts['root']]}, not the requested {version}")
    manifest = {
        'name': entry.get('name'),
        'version': entry.get('version'),
        'private': True,
        'description': facts['description'],
        'license': entry.get('license'),
        'packageManager': 'pnpm@11.11.0',
        'dependencies': {facts['root']: version},
    }
    for key in ('name', 'version', 'license'):
        if not isinstance(manifest[key], str) or not manifest[key]:
            raise PreparationError('baseline manifest has no usable ' + key)
    return json.dumps(manifest, indent=2) + '\n'


def stage_candidate(stage, sources):
    """Copy the three generated files into the candidate with reviewed modes.

    `package-lock.json` and every pin file are import-only: the caller removes
    them first, and the Go helper would refuse the stage if they were present.
    """
    for name in CANDIDATE_INPUTS:
        write_private(stage.candidate / name, Path(sources[name]).read_bytes(), 0o644)


def candidate_report(contract, consumer, stage, environment, *, verb, before=None, version=None,
                     shrinkwrap_authority=False, refresh=False):
    """Ask the Go helper to validate the candidate and return its report.

    `shrinkwrap_authority` is only ever set by the authenticated selected-archive
    route: there the archive's own shrinkwrap, compared separately, is the
    authority for the graph, so the reviewed predecessor's frozen snapshot edges
    are not substituted for it. The helper's archives, manifest, workspace and
    manager rules are identical in both modes.

    `refresh` is the caller's own `--refresh-lock`. The comparator's equal-selection
    rule is byte equality, so an explicit developer refresh that legitimately
    rewrites lock bytes for the same version must be named here; without it the
    comparator refuses the rewrite as an equal selection whose bytes differ.
    """
    argv = ['pnpm-lock-candidate', verb, consumer, '--scratch', str(stage.scratch),
            '--scratch-fd', str(stage.directory_fd)]
    if before is not None:
        argv += ['--before', str(before), '--requested-version', version]
    if shrinkwrap_authority:
        argv.append('--shrinkwrap-authority')
    if refresh:
        argv.append('--refresh-lock')
    text = contract_call(contract, argv, cwd=stage.candidate, environment=environment,
                         scratch=stage.scratch, label=verb,
                         pass_fds=(stage.directory_fd,))
    try:
        report = json.loads(text)
    except ValueError as error:
        raise PreparationError(f'{verb}: helper returned malformed JSON: {error}')
    for key in ('inputState', 'inputDigests', 'graph'):
        if key not in report:
            raise PreparationError(f'{verb}: report has no {key}')
    if report['inputState'] != 'candidate':
        raise PreparationError(f'{verb}: unexpected input state ' + str(report['inputState']))
    for name in CANDIDATE_INPUTS:
        if report['inputDigests'].get(name) != sha256(stage.candidate / name):
            raise PreparationError(f'{verb}: {name} changed after candidate validation')
    return report


def frozen_validation(contract, directory, manager, store, environment, scratch):
    """Prove the staged lock installs cold and hand back the installed payload.

    A frozen install is the only check that exercises the lock the way the image
    will; the payload stays inside the invocation's scratch until the caller
    either discards it or moves it to a diagnostic destination.
    """
    run_pnpm(contract, install_argv(manager, store, frozen=True), cwd=directory,
             environment=environment, scratch=scratch, label='frozen-install')
    nodes = directory / 'node_modules'
    if not nodes.is_dir():
        raise PreparationError('frozen install produced no node_modules')
    return nodes


def write_pins(contract, consumer, directory, environment, scratch):
    """Pins are written only after the candidate comparison is accepted."""
    contract_call(contract, ['pnpm-lock', 'pin', consumer, str(directory)], cwd=directory,
                  environment=environment, scratch=scratch, label='pin')
    contract_call(contract, ['pnpm-lock', 'check', consumer, str(directory)], cwd=directory,
                  environment=environment, scratch=scratch, label='check')


def publish_four(directory, destination, consumer):
    """Publish the four reviewed inputs as one all-or-nothing transaction."""
    project.publish(directory, destination, consumer=consumer)


def prepare(options):
    """Refresh or convert a project that already has its reviewed four inputs."""
    consumer = options.consumer
    prefix = Path(options.target)
    facts = PROJECTS[consumer]
    capability = project.authorize_mutation(prefix, consumer=consumer)
    project.guard_environment([prefix], consumer=consumer)
    contract = project.authorize_contract([str(prefix)], consumer=consumer)
    manifest = read_json(prefix / 'package.json')
    if manifest.get('packageManager') != 'pnpm@11.11.0':
        raise PreparationError('committed manifest does not pin pnpm@11.11.0')
    committed = (manifest.get('dependencies') or {}).get(facts['root'])
    if not isinstance(committed, str) or not committed:
        raise PreparationError('committed manifest selects no ' + facts['root'])
    version = exact_version(options.version or committed)
    if version == committed and not options.refresh_lock:
        # An equal, unrefreshed request starts no manager at all and writes nothing
        # into the prefix: it re-reads the committed bytes and the reviewed graph
        # through the ordinary pinned path, logging into an invocation scratch.
        scratch = Path(tempfile.mkdtemp(prefix=consumer + '-check-', dir=str(system_temp())))
        try:
            contract_call(contract, ['pnpm-lock', 'check', consumer, str(prefix)], cwd=prefix,
                          environment=os.environ.copy(), scratch=scratch, label='noop-check')
        finally:
            shutil.rmtree(scratch, ignore_errors=True)
        emit({'mode': 'equal', 'consumer': consumer, 'version': version, 'managerCalls': 0})
        return 0

    work = project.owned_scratch(capability)
    environment = child_environment(work)
    store = work / 'store'
    published = False
    evidence = {}
    try:
        manager = project.manager_argv(capability)
        manager_version(manager, environment, work)
        # The route is decided by the authenticated selection's own content. Only
        # Pi publishes a dependency shrinkwrap, so the bridge keeps its ordinary
        # path and never pays for a download it would not use.
        selected = None
        route = ROUTE_RESOLVE
        if consumer == 'pi':
            selected = selected_archive(contract, manager, environment, work,
                                        facts['root'], version)
            route = selected['route']
            evidence['selectedArchive'] = {
                'name': selected['name'], 'version': selected['version'],
                'integrity': selected['integrity'], 'tarball': selected['tarball'],
                'shrinkwrap': selected['shrinkwrap'] is not None}
        evidence['route'] = route
        evidence['routeReason'] = (
            'the authenticated selected archive carries npm-shrinkwrap.json, so its '
            'upstream graph is converted by pnpm import'
            if route == ROUTE_SHRINKWRAP else
            'the selected selection carries no dependency shrinkwrap: it is resolved '
            'under the reviewed workspace and policy as an ordinary all-hashed graph'
            if consumer == 'pi' else
            'the bridge publishes no dependency shrinkwrap; its manifest is resolved '
            'under the reviewed workspace and policy')
        stage = mint_stage(consumer)
        keep = False
        try:
            changed = dict(manifest)
            changed['dependencies'] = dict(manifest['dependencies'])
            changed['dependencies'][facts['root']] = version
            manifest_bytes = (json.dumps(changed, indent=2) + '\n').encode()
            workspace_bytes = (prefix / 'pnpm-workspace.yaml').read_bytes()
            if route == ROUTE_SHRINKWRAP:
                inputs, evidence_root, receipts = shrinkwrap_candidate(
                    contract, capability, manager, store, workspace_bytes, work,
                    manifest_bytes, selected, stage, environment)
                evidence['supplementalSris'] = receipts
                report = candidate_report(contract, consumer, stage, environment, verb='graph',
                                          before=prefix, version=version,
                                          shrinkwrap_authority=True,
                                          refresh=options.refresh_lock)
                delta = import_fidelity(capability, stage, environment, report, inputs,
                                        evidence_root, reviewed_normalisations(version))
                evidence['shrinkwrapDelta'] = {
                    'accepted': delta.get('accepted'), 'counts': delta.get('counts'),
                    'reported': delta.get('reported'), 'supplements': delta.get('supplements')}
            else:
                resolve = stage.scratch / 'resolve'
                resolve.mkdir(mode=0o700)
                write_private(resolve / 'package.json', manifest_bytes, 0o644)
                write_private(resolve / 'pnpm-workspace.yaml', workspace_bytes, 0o644)
                if consumer == 'bridge' and options.refresh_lock:
                    # A bridge refresh is incremental maintenance: the reviewed lock
                    # is the seed, so pnpm updates the existing graph instead of
                    # resolving it from nothing. An ordinary changed build keeps the
                    # fresh resolution the reviewed route describes.
                    seeded = prefix / 'pnpm-lock.yaml'
                    if seeded.is_symlink() or not seeded.is_file():
                        raise PreparationError(f'{seeded} is not a regular reviewed lock to seed')
                    write_private(resolve / 'pnpm-lock.yaml', seeded.read_bytes(), 0o644)
                run_pnpm(contract, install_argv(manager, store, frozen=False, lockfile_only=True),
                         cwd=resolve, environment=environment, scratch=stage.scratch,
                         label='resolve')
                if (resolve / 'node_modules').exists():
                    raise PreparationError('lockfile-only resolution created node_modules')
                stage_candidate(stage, {name: resolve / name for name in CANDIDATE_INPUTS})
                report = candidate_report(contract, consumer, stage, environment, verb='graph',
                                          before=prefix, version=version,
                                          refresh=options.refresh_lock)
                if selected is not None:
                    bind_resolved_root(report, selected)
            staged = work / 'prefix'
            staged.mkdir(mode=0o700)
            for name in CANDIDATE_INPUTS:
                shutil.copyfile(stage.candidate / name, staged / name)
            write_pins(contract, consumer, staged, environment, stage.scratch)
            nodes = frozen_validation(contract, staged, manager, store, environment,
                                      stage.scratch)
            if options.keep_payload:
                keep_payload(nodes, Path(options.keep_payload),
                             project.authorize_mutation(prefix, consumer=consumer).root)
            else:
                shutil.rmtree(nodes)
            contract_call(contract, ['pnpm-lock', 'check', consumer, str(staged)], cwd=staged,
                          environment=environment, scratch=stage.scratch, label='recheck')
            publish_four(staged, prefix, consumer)
            published = True
        except project.PublicationFailure as error:
            keep = error.status == 3
            raise
        except BaseException:
            keep = True
            raise
        finally:
            discard_stage(stage, keep=keep)
    finally:
        try:
            project.cleanup_scratch(work)
        except (OSError, ValueError):
            if published:
                raise
    emit({'mode': 'refresh' if options.refresh_lock else 'change', 'consumer': consumer,
          'version': version, 'evidence': evidence, 'report': report_summary(report)})
    return 0


def report_summary(report):
    """The bounded build record of a candidate report.

    The helper's full graph is deliberately not echoed: a changed Pi graph is
    ~150 identities and its edge lists would swamp the installer's own stdout.
    The counts and the compared digests are what a build record needs; the full
    report and every delta list stay in the invocation's captured helper output
    (`logs/graph.stdout`), which survives for the failed run that needs it.
    """
    delta = report.get('selectionDelta') or {}
    return {
        'inputState': report.get('inputState'),
        'inputDigests': report.get('inputDigests'),
        'selectionDelta': {key: (len(value) if isinstance(value, list) else value)
                           for key, value in delta.items()},
    }


def keep_payload(nodes, destination, root):
    """Move a diagnostic installed tree out of the scratch, below an owned root."""
    project.below(destination, root)
    if destination.exists():
        shutil.rmtree(destination)
    shutil.move(str(nodes), str(destination))
    print('prepare-project: installed payload kept at ' + str(destination), file=sys.stderr)


def adopt(options):
    """Convert the fixed baseline npm lock into an external four-input proposal."""
    consumer = options.consumer
    facts = PROJECTS[consumer]
    proposal = Path(options.proposal)
    root = project.authorized_root()
    project.below(proposal, root)
    if os.path.lexists(proposal) and not proposal.is_dir():
        raise PreparationError('proposal destination must be a directory')
    if not proposal.is_dir():
        proposal.mkdir(mode=0o700)
    baseline, files = baseline_inputs(consumer, options.baseline)
    baseline_manifest = json.loads(files['package.json'])
    dependencies = baseline_manifest.get('dependencies') or {}
    if list(dependencies) != [facts['root']]:
        raise PreparationError('baseline manifest must declare exactly ' + facts['root'])
    version = exact_version(dependencies[facts['root']])
    manifest = final_manifest(consumer, files['package.json'], version)

    work = bootstrap_stage(consumer)
    environment = child_environment(work)
    store = work / 'store'
    keep = False
    try:
        # A first import has no reviewed inputs, so the manager comes from the
        # bootstrap stage capability instead of a mutation capability.
        capability = project.authorize_stage(work, consumer=consumer)
        project.guard_environment([], consumer=consumer)
        contract = project.authorize_contract([], consumer=consumer)
        manager = project.stage_manager_argv(capability)
        manager_version(manager, environment, work)
        stage = mint_stage(consumer)
        try:
            resolve = work / 'resolve'
            resolve.mkdir(mode=0o700)
            write_private(resolve / 'package.json', manifest.encode(), 0o600)
            write_private(resolve / 'pnpm-workspace.yaml', facts['workspace'].encode(), 0o600)
            # The baseline npm lock is import input only: never committed, never
            # COPYed, never handed to npm.
            write_private(resolve / 'package-lock.json', files['package-lock.json'], 0o600)
            run_pnpm(contract, import_argv(manager, store), cwd=resolve,
                     environment=environment, scratch=work, label='import')
            evidence = work / 'evidence'
            evidence.mkdir(mode=0o700)
            shutil.copyfile(resolve / 'package-lock.json', evidence / 'package-lock.json')
            (resolve / 'package-lock.json').unlink()
            for name in ('npm-shrinkwrap.json', '.npmrc', PIN_FILE):
                if (resolve / name).exists():
                    raise PreparationError('import left an unexpected ' + name)
            stage_candidate(stage, {name: resolve / name for name in CANDIDATE_INPUTS})
            report = candidate_report(contract, consumer, stage, environment,
                                      verb='adoption-graph')
            staged = work / 'prefix'
            staged.mkdir(mode=0o700)
            for name in CANDIDATE_INPUTS:
                shutil.copyfile(stage.candidate / name, staged / name)
            write_pins(contract, consumer, staged, environment, work)
            nodes = frozen_validation(contract, staged, manager, store, environment, work)
            contract_call(contract, ['pnpm-lock', 'check', consumer, str(staged)],
                          cwd=staged, environment=environment, scratch=work, label='recheck')
            # A unique holder inside the authorized root, created atomically: a
            # fixed name would have to be proven ours before it could be removed,
            # and there is no proof for a path somebody else may already own.
            holder = Path(tempfile.mkdtemp(prefix=consumer + '-proposal-stage.',
                                           dir=str(project.authorized_root())))
            os.chmod(holder, 0o700)
            try:
                for name in (*CANDIDATE_INPUTS, PIN_FILE):
                    shutil.copyfile(staged / name, holder / name)
                publish_four(holder, proposal, consumer)
            finally:
                shutil.rmtree(holder, ignore_errors=True)
            if options.keep_payload:
                keep_payload(nodes, Path(options.keep_payload), project.authorized_root())
        except project.PublicationFailure as error:
            keep = error.status == 3
            raise
        except BaseException:
            keep = True
            raise
        finally:
            discard_stage(stage, keep=keep)
    finally:
        if not keep:
            shutil.rmtree(work, ignore_errors=True)
    emit({'mode': 'adopt', 'consumer': consumer, 'version': version, 'baseline': baseline,
          'baselineDigests': {name: hashlib.sha256(body).hexdigest()
                              for name, body in files.items()},
          'proposal': str(proposal),
          'digests': {name: sha256(proposal / name)
                      for name in (*CANDIDATE_INPUTS, PIN_FILE)},
          'candidateDigests': report['inputDigests']})
    return 0


def emit(payload):
    json.dump(payload, sys.stdout, indent=2, sort_keys=True)
    sys.stdout.write('\n')


def main():
    parser = argparse.ArgumentParser(prog='prepare-project.py')
    parser.add_argument('--consumer', required=True, choices=sorted(PROJECTS))
    sub = parser.add_subparsers(dest='verb', required=True)
    refresh = sub.add_parser('prepare')
    refresh.add_argument('target')
    refresh.add_argument('version', nargs='?', default='')
    refresh.add_argument('--refresh-lock', action='store_true')
    refresh.add_argument('--keep-payload')
    first = sub.add_parser('adopt')
    first.add_argument('proposal')
    first.add_argument('--baseline', default=BASELINE)
    first.add_argument('--keep-payload')
    options = parser.parse_args()
    handlers = {'prepare': prepare, 'adopt': adopt}
    try:
        return handlers[options.verb](options)
    except PreparationError as error:
        print('prepare-project: ' + str(error), file=sys.stderr)
        return 1
    except project.PublicationFailure as error:
        print('prepare-project: ' + str(error), file=sys.stderr)
        return error.status
    except Interrupted as error:
        return 128 + error.signum
    except (ValueError, OSError) as error:
        print('prepare-project: ' + str(error), file=sys.stderr)
        return 1


def entry():
    for item in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(item, lambda signum, frame: (_ for _ in ()).throw(Interrupted(signum)))
    sys.exit(main())


if __name__ == '__main__':
    entry()
