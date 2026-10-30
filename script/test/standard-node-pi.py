#!/usr/bin/env python3
"""Focused Pi (and its Claude bridge) orchestration for the native harness.

`standard-node-consumers.sh BASE --consumer pi [--version EXACT]` runs the real
image-owned install of both projects cold, then proves the things that a lock
conversion cannot: the authenticated sibling byte proof, the derived generated
bin inventory, the runtime probes, the warning wrapper, the one canonical
bridge registration, the seed hook, the two independent health records, the
offline numeric-identity runtime and a set of real-layout mutants. It shares the
primitive accounting contract (`standard-node-consumers.py`) with the dsh and
primitives harnesses instead of re-inventing bounded commands.

Three rules decide the shape of this file:

* **Every expectation is authored here**, not read back from the artifact under
  test. The family tables, dispatch lists, generated-bin inventories, model
  count and mutant diagnostics below are independent vectors; the installed
  tree, the production converter and the production graph helper never supply
  an expectation.
* **The changed selection is compared against the authenticated archive.** For
  a Pi selection whose tarball carries `npm-shrinkwrap.json`, the harness
  captures the registry SRI *before* preparation, downloads and SRI-verifies
  that archive itself, then compares the emitted graph's identity/occurrence/
  SRI/edge/optional/peer facts against the shrinkwrap with its own comparator.
  A canned lock or a fixture graph can never satisfy it.
* **A fake success fails.** The installer's success line is not acceptance: the
  harness re-runs the byte checker, requires the exact family count in its
  output, re-derives the archive list from its own parse, and fails on a
  missing or contradictory record.
"""
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tempfile

spec = importlib.util.spec_from_file_location(
    'consumers', Path(__file__).with_name('standard-node-consumers.py'))
base = importlib.util.module_from_spec(spec)
spec.loader.exec_module(base)
ROOT = base.ROOT
INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
BODY = '/source/script/test/fixtures/standard-node-pi.py'
# The installed payload plus a private archive root, measured on the real cold
# runs and checked against measured free space before any PASS is printed.
PI_BUDGET_KIB = 3145728
RESERVE_KIB = 524288
PI_PREFIX = '/opt/agent-vm/pi'
BRIDGE_PREFIX = '/opt/agent-vm/pi-packages'
# The bridge's loadable entry as the image registers it: `pi.sh` hands Pi
# `--extension <this path>` and a jiti-loaded TypeScript payload has no `main` to
# resolve instead (install-pi-packages.sh requires the same file).
BRIDGE_ENTRY = BRIDGE_PREFIX + '/node_modules/pi-claude-bridge/src/index.ts'
WRAPPER = '/usr/local/bin/pi'
EXACT = re.compile(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)'
                   r'(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
                   r'(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?'
                   r'(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?')

# --- authored expectations --------------------------------------------------
# The reachable Pi family per *exact selected version*, its direct root edges and
# every package-root generated `.bin` command. Written from the authenticated
# archive manifests during review; never derived from an installed tree.
PI_FAMILY = {
    '1.4.0': {'members': ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp',
                          'pi-telemetry', 'pi-tui'),
              'direct': ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-tui'),
              'bins': {'pi-agent-core': ('pi-ai',), 'pi-ai': ('anthropic-ai-sdk',),
                       'pi-codemode': (), 'pi-mcp': (), 'pi-telemetry': (),
                       'pi-tui': ('marked',)}},
    '0.87.0': {'members': ('chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui'),
               'direct': ('chord', 'pi-agent-core', 'pi-ai', 'pi-tui'),
               'bins': {'chord': ('esbuild',), 'pi-agent-core': ('pi-ai', 'yaml'),
                        'pi-ai': ('anthropic-ai-sdk',), 'pi-telemetry': (),
                        'pi-tui': ('marked',)}},
}
PI_ROOT = '@earendil-works/pi-coding-agent'
PI_ROOT_BIN = 'dist/bundle/cli.js'
# The reviewed per-selection subcommand lists the installer publishes as data.
DISPATCH = {'1.4.0': ('auth', 'config', 'install', 'list', 'mcp', 'remove', 'uninstall', 'update'),
            '0.87.0': ('auth', 'config', 'install', 'list', 'remove', 'uninstall', 'update')}
# `pi --help` on 1.4.0 advertises no chord/esbuild transform dependency; the
# historical selection ships chord and its native esbuild package.
MODERN_ABSENT = ('@earendil-works/chord', 'esbuild')
# The bridge's registered catalog per *measured (Pi, bridge) pair*, measured rather
# than inherited. The planning investigation saw 12 models; the first real cold
# install of the reviewed 0.8.0 payload beside Pi 1.4.0 (focused harness,
# 2026-10-28, linux/arm64) registered 14. The same bridge payload beside the
# authenticated legacy Pi 0.87.0 graph registered 11: the bridge enumerates the
# providers the selected Pi SDK ships, and 0.87.0 ships an older, smaller catalog.
# Both numbers are measurements of a verified graph (the byte proof and the
# independent shrinkwrap comparison pass in the same run), recorded here so a
# *change* still fails: an unknown pair or a different count is a stop for review,
# and the generic verifier keeps accepting one canonical positive N. A count alone
# is not the proof -- `resolution_leg` pins the resolved bridge and SDK directories
# -- so keying this table by the Pi version only would let a bridge bump inherit a
# number nobody measured.
BRIDGE_MODELS = {('1.4.0', '0.8.0'): 14, ('0.87.0', '0.8.0'): 11}
# The three reviewed representation normalisations of the authenticated 0.87.0
# shrinkwrap -> pnpm import conversion (ADR 0011, pending operator acceptance).
# They are named explicitly so any *new* delta fails instead of averaging in.
SHRINKWRAP_NORMALISATIONS = {
    'mergedOccurrences': {'agent-base@9.0.0': 2},
    'addedEdges': {('openai@6.40.0', 'ws')},
    'peerDeclarations': {('debug@4.4.3', 'supports-color')},
}
SEED_CASES = ('absent-binary', 'fresh', 'idempotent', 'preserved', 'invalid-json', 'scalar',
              'array', 'provider-shape', 'mode', 'absent-state')


def arguments(args):
    if not args or args[0].startswith('-'):
        raise ValueError('usage: standard-node-consumers.sh BASE_IMAGE --consumer pi '
                         '[--version EXACT] [--bridge-version EXACT] [--platform linux/ARCH]')
    image = args[0]
    flags = {}
    rest = args[1:]
    while rest:
        flag, *rest = rest
        if flag not in ('--consumer', '--version', '--bridge-version', '--platform') \
                or flag in flags or not rest:
            raise ValueError('unknown/duplicate/missing option: ' + flag)
        value, *rest = rest
        flags[flag] = value
    if flags.get('--consumer') != 'pi':
        raise ValueError('expected --consumer pi')
    for name, label in (('--version', 'pi version'), ('--bridge-version', 'bridge version')):
        if name in flags and not EXACT.fullmatch(flags[name]):
            raise ValueError('expected canonical exact ' + label)
    platform = flags.get('--platform')
    if platform not in (None, 'linux/amd64', 'linux/arm64'):
        raise ValueError('expected native linux/amd64 or linux/arm64')
    return image, platform, flags.get('--version'), flags.get('--bridge-version')


def inputs_for(consumer):
    """The consumer's committed four reviewed inputs, as one unit."""
    directory = {'pi': ROOT / 'images/tools/pi',
                 'bridge': ROOT / 'images/tools/pi/bridge'}[consumer]
    missing = [name for name in INPUTS if not (directory / name).is_file()]
    if missing:
        raise base.Failure(f'the committed {consumer} four inputs are incomplete: '
                           + ', '.join(missing))
    return directory


# --- independent shrinkwrap comparison --------------------------------------
def npm_identity(location, packages):
    """`name@version` of one prefixed npm location (compare.py's definition)."""
    record = packages[location]
    version = record.get('version')
    if not isinstance(version, str) or not version:
        raise base.Failure(f'{location}: the authenticated record has no version')
    return f'{location.split("node_modules/")[-1]}@{version}'


def strip_context(key):
    return key.split('(', 1)[0]


def npm_edges(packages, location):
    record = packages[location]
    edges = {}
    for name, specifier in {**record.get('dependencies', {}),
                            **record.get('optionalDependencies', {})}.items():
        edges[name] = {'kind': 'optional' if name in record.get('optionalDependencies', {})
                       else 'mandatory', 'target': npm_resolve(packages, location, name)}
    return edges


def npm_resolve(packages, location, name):
    """Node's own ancestor node_modules lookup, over the prefixed locations."""
    parts = location.split('/') if location else []
    for index in range(len(parts), -1, -1):
        if index and parts[index - 1] == 'node_modules':
            continue
        prefix = '/'.join(parts[:index])
        candidate = (prefix + '/' if prefix else '') + 'node_modules/' + name
        if candidate in packages:
            return candidate
    return None


def contexts_of(key):
    """The peer-context entries of one snapshot key, as `name@version` text."""
    entries, depth, start = [], 0, None
    for index, character in enumerate(key):
        if character == '(':
            if depth == 0:
                start = index + 1
            depth += 1
        elif character == ')':
            depth -= 1
            if depth == 0:
                entries.append(key[start:index])
    return entries


def justified_peer_instances(prefixed):
    """Every peer instance the authenticated shrinkwrap declares and resolves.

    A peer context entry is not free text: it names the instance a package was
    resolved against. Whichever instance that is, some authenticated record has to
    declare the package as a peer and this graph has to resolve it there.
    """
    instances = {}
    for location, record in prefixed.items():
        names = set(record.get('peerDependencies') or {}) | set(record.get('peerDependenciesMeta') or {})
        for name in names:
            target = npm_resolve(prefixed, location, name)
            if target is None:
                continue
            identity = npm_identity(target, prefixed)
            if identity is not None:
                instances.setdefault(name, set()).add(identity)
    return instances


def snapshot_target(snapshots, name, reference):
    if isinstance(reference, dict):
        reference = reference.get('version')
    if not isinstance(reference, str):
        raise base.Failure(f'unreadable snapshot reference {name}={reference!r}')
    for candidate in (f'{name}@{reference}', reference):
        if candidate in snapshots:
            return candidate
    raise base.Failure(f'the prepared lock references a missing snapshot {name}@{reference}')


def compare_shrinkwrap(shrinkwrap, lock, root_integrity):
    """The prepared graph against the authenticated shrinkwrap, independently.

    Returns the observed normalisations; any unreviewed delta raises. Peer
    *contexts* are representation (they choose an instance, not an edge), so an
    edge is compared by its resolved archive identity across every context of
    one identity, while optionality, SRI and declaration text stay exact. A
    context is still compared as a fact: ADR 0003 keeps it as an opaque whole key,
    never reconstructed, but every entry must name an instance of a package the
    authenticated shrinkwrap declares as a peer and resolves for it, so retargeting
    an instance at an unrelated installed package is a change, not representation.
    """
    if shrinkwrap.get('lockfileVersion') != 3:
        raise base.Failure('the authenticated shrinkwrap is not lockfileVersion 3')
    records = shrinkwrap.get('packages') or {}
    root_record = records.get('')
    if not isinstance(root_record, dict) or root_record.get('name') != PI_ROOT:
        raise base.Failure('the authenticated shrinkwrap root is not ' + PI_ROOT)
    # The shrinkwrap belongs to the Pi package, so its root record *is* the
    # installed root archive; every other location lives under it.
    prefixed = {f'node_modules/{PI_ROOT}' if location == ''
                else f'node_modules/{PI_ROOT}/' + location: record
                for location, record in records.items()}
    packages = lock.get('packages') or {}
    snapshots = lock.get('snapshots') or {}
    importer = ((lock.get('importers') or {}).get('.') or {}).get('dependencies') or {}
    selected = importer.get(PI_ROOT) or {}
    version = root_record.get('version')
    stated = (selected.get('version') or selected.get('specifier') or '').split('(', 1)[0]
    if stated and stated != version:
        raise base.Failure('the prepared lock does not select the authenticated root version')
    root_key = f'{PI_ROOT}@{version}'
    root_package = packages.get(root_key)
    if not isinstance(root_package, dict):
        raise base.Failure('the prepared lock carries no root archive record ' + root_key)
    actual_root = (root_package.get('resolution') or {}).get('integrity')
    if actual_root != root_integrity:
        raise base.Failure(f'the prepared root SRI {actual_root!r} is not the pre-run '
                           f'authenticated SRI {root_integrity!r}')

    # Identity multiset: npm physical locations versus pnpm snapshot records.
    npm_counts, npm_records = {}, {}
    for location in prefixed:
        identity = npm_identity(location, prefixed)
        npm_counts[identity] = npm_counts.get(identity, 0) + 1
        npm_records[identity] = prefixed[location]
    pnpm_counts = {}
    for key in snapshots:
        identity = strip_context(key)
        pnpm_counts[identity] = pnpm_counts.get(identity, 0) + 1
    identities = set(npm_counts) | set(pnpm_counts) | {root_key}
    added = sorted(identities - set(npm_counts))
    dropped = sorted(set(npm_counts) - set(pnpm_counts) - {root_key})
    if added or dropped:
        raise base.Failure(f'shrinkwrap identities changed: added {added[:5]}, dropped {dropped[:5]}')
    merged = {}
    for identity in sorted(npm_counts):
        if identity == root_key:
            continue
        expected = npm_counts[identity]
        found = pnpm_counts.get(identity, 0)
        if found == expected:
            continue
        allowance = SHRINKWRAP_NORMALISATIONS['mergedOccurrences'].get(identity)
        if allowance == expected and found == 1:
            merged[identity] = expected
            continue
        raise base.Failure(f'{identity}: {found} prepared snapshots for {expected} authenticated '
                           'occurrences, which is not a reviewed normalisation')

    # SRI: every authenticated integrity is preserved byte-for-byte; the five
    # historically missing sibling SRIs must exist and be sha512 (the harness
    # verifies their archive bytes itself when it downloads them).
    missing_integrity = []
    for identity, record in sorted(npm_records.items()):
        if identity == root_key:
            continue
        archive = packages.get(identity)
        if not isinstance(archive, dict):
            raise base.Failure('the prepared lock carries no archive record for ' + identity)
        integrity = record.get('integrity')
        actual = (archive.get('resolution') or {}).get('integrity')
        if integrity is None:
            if not (isinstance(actual, str) and actual.startswith('sha512-')):
                raise base.Failure(f'{identity}: the completed SRI is not sha512: {actual!r}')
            missing_integrity.append(identity)
            continue
        if actual != integrity:
            raise base.Failure(f'{identity}: prepared SRI {actual!r} is not the authenticated '
                               f'{integrity!r}')
    expected_missing = sorted(f'{PI_ROOT.rsplit("/", 1)[0]}/{member}@{version}'
                              for member in PI_FAMILY.get(version, {'members': ()})['members'])
    if sorted(missing_integrity) != expected_missing:
        raise base.Failure('the authenticated shrinkwrap omits SRI for an unreviewed set: '
                           + ', '.join(missing_integrity))

    # Edges and optionality, per identity, resolved identities on both sides.
    # The root is compared like every other identity: its own snapshot(s) carry
    # the same named edges the authenticated root record does.
    normalised_edges = set()
    for identity in sorted(npm_records):
        old = npm_edges(prefixed, next(location for location in prefixed
                                      if location and npm_identity(location, prefixed) == identity))
        new = {}
        for key in snapshots:
            if strip_context(key) != identity:
                continue
            for name, reference in {**snapshots[key].get('dependencies', {}),
                                    **snapshots[key].get('optionalDependencies', {})}.items():
                target = snapshot_target(snapshots, name, reference)
                kind = 'optional' if name in snapshots[key].get('optionalDependencies', {}) else 'mandatory'
                previous = new.get(name)
                if previous is not None and previous['target'] != strip_context(target):
                    raise base.Failure(f'{identity}: {name} resolves to two identities in different '
                                       'contexts; review the peer contexts')
                new[name] = {'kind': kind, 'target': strip_context(target)}
        for name in sorted(set(old) | set(new)):
            before, after = old.get(name), new.get(name)
            before_identity = (npm_identity(before['target'], prefixed)
                               if before and before['target'] else None)
            after_identity = after['target'] if after else None
            if before is None:
                if (identity, name) in SHRINKWRAP_NORMALISATIONS['addedEdges']:
                    normalised_edges.add((identity, name))
                    continue
                raise base.Failure(f'{identity}: prepared lock adds the edge {name}')
            if after is None:
                raise base.Failure(f'{identity}: prepared lock drops the edge {name}')
            if before_identity != after_identity:
                raise base.Failure(f'{identity}: {name} resolves to {after_identity}, the '
                                   f'authenticated graph resolves {before_identity}')
            if before['kind'] != after['kind']:
                raise base.Failure(f'{identity}: {name} changed from {before["kind"]} to '
                                   f'{after["kind"]}')

    # Effective peer contexts. The entry names an instance; the authenticated
    # graph has to justify it. Comparing edges by stripped identity alone leaves
    # this text unconstrained, which is how a retargeted instance used to pass.
    instances = justified_peer_instances(prefixed)
    for key in snapshots:
        for entry in contexts_of(key):
            name, _, version = entry.rpartition('@')
            if not name:
                raise base.Failure(f'{key}: the peer context entry {entry!r} names no package')
            if f'{name}@{version}' in instances.get(name, set()):
                continue
            raise base.Failure(f'{key}: the peer context names {entry}, which no authenticated '
                               'record declares as a peer that resolves to it')

    # Peer declarations are data, but a completion is still reported.
    normalised_peers = set()
    for identity, record in sorted(npm_records.items()):
        declared = set(record.get('peerDependencies') or {})
        prepared = set()
        for key in snapshots:
            if strip_context(key) == identity:
                prepared |= set((packages.get(identity) or {}).get('peerDependencies') or {})
        for name in sorted(prepared - declared):
            if (identity, name) in SHRINKWRAP_NORMALISATIONS['peerDeclarations']:
                normalised_peers.add((identity, name))
                continue
            raise base.Failure(f'{identity}: prepared lock adds the peer declaration {name}')
    return {'mergedOccurrences': sorted(merged), 'addedEdges': sorted(normalised_edges),
            'peerDeclarations': sorted(normalised_peers)}


def tarball_for(identity, resolution):
    """The archive URL, from the lock when it carries one, else the registry rule.

    pnpm v9 omits `tarball` for the default registry, so the npmjs URL template
    is derived here rather than borrowed from the production graph helper. A
    wrong derivation cannot pass: the download itself has to produce bytes that
    hash to the lock's SRI.
    """
    declared = resolution.get('tarball')
    if isinstance(declared, str):
        return declared
    name, _, version = identity.rpartition('@')
    return f'https://registry.npmjs.org/{name}/-/{name.rsplit("/", 1)[-1]}-{version}.tgz'


def family_from_lock(lock, version):
    """The selected family read from the prepared lock, with authored checks."""
    shape = PI_FAMILY.get(version)
    if shape is None:
        raise base.Failure(f'no reviewed Pi family shape covers {version}; add one from the '
                           'authenticated archives before certifying that selection')
    packages = lock.get('packages') or {}
    snapshots = lock.get('snapshots') or {}
    importer = ((lock.get('importers') or {}).get('.') or {}).get('dependencies') or {}
    root = importer.get(PI_ROOT) or {}
    root_version = root.get('version') or ''
    if root_version.split('(', 1)[0] != version:
        raise base.Failure(f'the prepared lock selects {root_version}, not {version}')
    members, providers = {}, {}
    for base_name in shape['members']:
        identity = f'@earendil-works/{base_name}@{version}'
        archive = packages.get(identity)
        if not isinstance(archive, dict):
            raise base.Failure('the prepared lock is missing the family member ' + identity)
        integrity = (archive.get('resolution') or {}).get('integrity')
        if not (isinstance(integrity, str) and integrity.startswith('sha512-')):
            raise base.Failure(f'{identity}: no sha512 SRI in the prepared lock')
        members[base_name] = {'name': f'@earendil-works/{base_name}', 'version': version,
                              'integrity': integrity,
                              'tarball': tarball_for(identity, archive.get('resolution') or {})}
        keys = [key for key in snapshots if strip_context(key) == identity]
        if not keys:
            raise base.Failure(f'{identity}: the prepared lock has no snapshot')
        for key in keys:
            for name, reference in {**snapshots[key].get('dependencies', {}),
                                    **snapshots[key].get('optionalDependencies', {})}.items():
                target = strip_context(snapshot_target(snapshots, name, reference))
                if target.startswith(PI_ROOT + '@'):
                    continue
                if name.startswith('@earendil-works/'):
                    continue
                archive = packages.get(target)
                if not isinstance(archive, dict):
                    raise base.Failure(f'{identity} -> {name}: no archive record for {target}')
                integrity = (archive.get('resolution') or {}).get('integrity')
                if not (isinstance(integrity, str) and integrity.startswith('sha512-')):
                    raise base.Failure(f'{target}: no sha512 SRI for a family provider')
                providers[target] = {'name': target.rsplit('@', 1)[0],
                                     'version': target.rsplit('@', 1)[1],
                                     'integrity': integrity,
                                     'tarball': tarball_for(target,
                                                            archive.get('resolution') or {})}
    root_keys = [key for key in snapshots if strip_context(key) == f'{PI_ROOT}@{version}']
    if not root_keys:
        raise base.Failure(f'{PI_ROOT}@{version}: the prepared lock has no root snapshot')
    declared = set()
    for key in root_keys:
        for name in snapshots[key].get('dependencies', {}):
            if name.startswith('@earendil-works/'):
                declared.add(name.rsplit('/', 1)[1])
    if declared != set(shape['direct']):
        raise base.Failure(f'the prepared lock direct family edges {sorted(declared)} are not the '
                           f'reviewed {sorted(shape["direct"])}')
    return {'root': {'name': PI_ROOT, 'version': version,
                     'integrity': (packages.get(f'{PI_ROOT}@{version}', {})
                                   .get('resolution') or {}).get('integrity')},
            'members': members, 'providers': providers, 'shape': shape}


class Invocation(base.Invocation):
    def __init__(self, image, requested, pi_version, bridge_version):
        super().__init__(image, requested)
        self.pi_version, self.bridge_version = pi_version, bridge_version
        self.source = ''
        self.deadline = 190
        self.contract = None
        self.archive_plan = None
        self.selection = {}

    def run(self, args):
        previous = base.COMMAND_TIMEOUT_SECONDS
        base.COMMAND_TIMEOUT_SECONDS = self.deadline
        try:
            return super().run(args)
        finally:
            base.COMMAND_TIMEOUT_SECONDS = previous

    def check_budget(self):
        retained = self.retained_kib()
        if retained > PI_BUDGET_KIB:
            raise base.Failure(f'pi install budget exceeded: {retained} > {PI_BUDGET_KIB} KiB')
        return retained

    def evidence_allowance_bytes(self):
        return max(PI_BUDGET_KIB * 1024 - (self.rw_kib + self.fixture_kib) * 1024
                   - self.evidence_allocated_bytes() - base.EVIDENCE_BLOCK_BYTES, 0)

    def mounts(self):
        return ['--mount', f'type=bind,src={ROOT},dst=/source,readonly',
                '--mount', f'type=bind,src={self.contract / "out"},dst=/tmp/recipe-contract,readonly']

    def measure(self, main):
        """No fixture volume: the disposable container's own layer is the budget."""
        return self.check_budget()

    def action(self, main, action, *args, seconds=300):
        self.deadline = seconds + 15
        try:
            return self.docker('exec', '-e', 'AGENT_VM_HARNESS_PLATFORM=' + self.platform,
                               main, 'timeout', '--kill-after=5', str(seconds),
                               'python3', '-B', BODY, action, *args)
        finally:
            self.deadline = 190

    def evidence(self, name, payload):
        (self.logs / name).write_text(json.dumps(payload, indent=2, sort_keys=True) + '\n')
        return payload

    def provenance(self, main):
        """Source, image, launcher, manager archive and selected root SRI, pre-run."""
        self.source = self.command('git', '-C', str(ROOT), 'rev-parse', 'HEAD').strip()
        files = [*(self.pi_inputs / name for name in INPUTS),
                 *(self.bridge_inputs / name for name in INPUTS),
                 *(ROOT / 'images/pnpm').rglob('*'),
                 *(ROOT / 'images/tools/pi').glob('*'),
                 *(ROOT / 'images/tools/pi/bridge').glob('*'),
                 *(ROOT / 'images/tools/pi/extensions').glob('*'),
                 *(ROOT / 'images/tools/pi/dispatch').glob('*'),
                 ROOT / 'images/tools/pnpm-project.py', ROOT / 'images/tools/pnpm-host-manager.py',
                 *(ROOT / 'images/recipe-contract').rglob('*'),
                 *(ROOT / 'script/test').glob('standard-node-*.py'),
                 ROOT / 'script/test/fixtures/standard-node-pi.py']
        hashes = {}
        for file in files:
            if file.is_file() and '__pycache__' not in file.parts:
                hashes[str(file.relative_to(ROOT)) if ROOT in file.parents else str(file)] = \
                    base.hashlib.sha256(file.read_bytes()).hexdigest()
        self.evidence('source.json', {'source': self.source, 'pi': self.pi_version,
                                    'bridge': self.bridge_version,
                                    'inputs': {consumer: {'path': str(directory),
                                                          'files': {name: base.hashlib.sha256(
                                                              (directory / name).read_bytes()).hexdigest()
                                                                    for name in INPUTS}}
                                               for consumer, directory in
                                               (('pi', self.pi_inputs), ('bridge', self.bridge_inputs))},
                                    'files': hashes,
                                    'dirty': self.command('git', '-C', str(ROOT), 'status', '--short')})
        context = self.docker('context', 'show').strip()
        driver = self.docker('buildx', 'inspect', context)
        if not any(line.split() == ['Driver:', 'docker'] for line in driver.splitlines()):
            raise base.Failure('daemon-backed docker driver required')
        daemon = json.loads(self.docker('info', '--format', '{{json .}}'))
        image = json.loads(self.docker('image', 'inspect', self.image))[0]
        self.docker('history', '--no-trunc', self.image)
        (self.logs / 'image.json').write_text(json.dumps(
            {'Id': image['Id'], 'Os': image['Os'], 'Architecture': image['Architecture'],
             'Labels': image['Config'].get('Labels'), 'Created': image.get('Created'),
             'repoDigests': image.get('RepoDigests')}, indent=2, sort_keys=True) + '\n')
        self.platform = self.requested or 'linux/' + base.arch(daemon['Architecture'])
        if daemon['OSType'] != 'linux' or 'linux/' + base.arch(daemon['Architecture']) != self.platform:
            raise base.Failure('daemon platform mismatch/emulation')
        if image['Os'] != 'linux' or 'linux/' + base.arch(image['Architecture']) != self.platform:
            raise base.Failure('base-image platform mismatch/emulation')
        if not image.get('RepoDigests'):
            print('provenance: the base carries no repository digest; this is a locally-built '
                  'image, so the run is diagnostic and never a product certification', flush=True)

    def execute(self):
        self.pi_inputs, self.bridge_inputs = inputs_for('pi'), inputs_for('bridge')
        self.pi_version = self.pi_version or json.loads(
            (self.pi_inputs / 'package.json').read_text())['dependencies'][PI_ROOT]
        self.bridge_version = self.bridge_version or json.loads(
            (self.bridge_inputs / 'package.json').read_text()
        )['dependencies']['pi-claude-bridge']
        for name in (self.pi_version, self.bridge_version):
            if not EXACT.fullmatch(name):
                raise base.Failure('invalid input selection: ' + name)
        self.provenance(None)
        print(f'pi: predeclared install budget {PI_BUDGET_KIB} KiB + reserve {RESERVE_KIB} KiB',
              flush=True)
        self.select_contract()
        self.capacity(PI_BUDGET_KIB + RESERVE_KIB)
        main = self.create(self.token + '-probe', '--platform', self.platform, '--network', 'bridge',
                           '--cap-drop', 'ALL', '--cap-add', 'SETUID', '--cap-add', 'SETGID',
                           '--cap-add', 'SETPCAP',
                           '--tmpfs', '/root:rw,mode=0770,uid=0,gid=23456',
                           '--tmpfs', '/runtime-homes:rw,mode=1777', *self.mounts(),
                           self.image, 'sleep', 'infinity')
        self.docker('start', main)
        initialize = self.json_line(self.action(main, 'initialize', self.token, self.source,
                                                self.pi_version, self.bridge_version), 'initialize')
        if initialize.get('initialize') != self.token or not initialize.get('base'):
            raise base.Failure('container readiness/identity evidence invalid')
        self.selection = self.evidence('base.json', initialize)
        # Before any preparation: the selected root archive's registry SRI.
        self.root = self.json_line(self.action(main, 'root-sri', self.pi_version), 'rootSri')['rootSri']
        self.evidence('selected-root.json', self.root)
        if self.root.get('version') != self.pi_version:
            raise base.Failure('the captured root SRI is for another selection')
        self.selected = self.json_line(
            self.action(main, 'selected-archive', self.pi_version, self.root['integrity'],
                        self.root['tarball']), 'selectedArchive')['selectedArchive']
        self.evidence('selected-archive.json', self.selected)
        if self.pi_version in PI_FAMILY and 'chord' in PI_FAMILY[self.pi_version]['members'] \
                and not self.selected.get('shrinkwrap'):
            raise base.Failure('the selected archive carries no npm-shrinkwrap.json, so the '
                               'authenticated import route cannot be the one exercised')
        for name in INPUTS:
            self.docker('cp', str(self.pi_inputs / name), main + ':' + PI_PREFIX + '/' + name)
        install = self.action(main, 'install-pi', self.pi_version, seconds=900)
        if f'installed pi {self.pi_version} from the reviewed inputs' not in install:
            raise base.Failure('install-pi.sh did not report the expected installed selection')
        self.action(main, 'graph', seconds=120)
        lock = self.fetch(main, '/opt/pi-harness/lock.json', 'lock.json')
        family = family_from_lock(lock, self.pi_version)
        self.evidence('family.json', family)
        if family['root']['integrity'] != self.root['integrity']:
            raise base.Failure('the prepared root archive SRI is not the pre-run authenticated SRI')
        if self.selected.get('shrinkwrap'):
            shrinkwrap = self.fetch(main, f'/opt/pi-harness/selected/{self.pi_version}'
                                          '/npm-shrinkwrap.json', 'shrinkwrap.json')
            normalisations = compare_shrinkwrap(shrinkwrap, lock, self.root['integrity'])
            self.evidence('shrinkwrap-delta.json', normalisations)
            print('pi: authenticated shrinkwrap comparison PASS ' + json.dumps(normalisations), flush=True)
        plan = {'archives': [dict(entry, kind='member') for entry in family['members'].values()]
                + [dict(entry, kind='provider') for entry in family['providers'].values()]}
        self.archive_plan = self.evidence('archive-plan.json', plan)
        local = self.logs / 'archive-plan.upload.json'
        local.write_text(json.dumps(plan))
        self.docker('cp', str(local), main + ':/opt/pi-harness/archive-plan.json')
        for line in self.action(main, 'archives', '/opt/pi-harness/archive-plan.json',
                                seconds=900).splitlines():
            record = self.json_line(line, 'archive')
            if record['integrity'] != next(entry['integrity'] for entry in plan['archives']
                                           if f'{entry["name"]}@{entry["version"]}' == record['archive']):
                raise base.Failure('a downloaded archive does not match its lock SRI: ' + record['archive'])
        anchor = 'pi-agent-core'
        proven = self.action(main, 'bytes', anchor, seconds=420)
        self.evidence('bytes.txt', {'output': proven})
        expected = f'{len(family["members"])}/{len(family["members"])} Pi sibling archives and ' \
                   'installed bytes verified'
        if expected not in proven:
            raise base.Failure('the authenticated byte proof did not report ' + expected)
        for name in INPUTS:
            self.docker('cp', str(self.bridge_inputs / name), main + ':' + BRIDGE_PREFIX + '/' + name)
        bridge = self.action(main, 'install-bridge', self.bridge_version, seconds=600)
        if f'installed pi-claude-bridge {self.bridge_version} from the reviewed inputs' not in bridge:
            raise base.Failure('install-pi-packages.sh did not report the expected installed bridge')
        runtime = self.json_line(self.action(main, 'runtime', seconds=420), 'runtime')['runtime']
        self.evidence('runtime.json', runtime)
        shape = sorted(runtime['shape'])
        if shape != sorted(PI_FAMILY[self.pi_version]['members']):
            raise base.Failure(f'check-runtime reports the family {shape}, the reviewed shape is '
                               f'{sorted(PI_FAMILY[self.pi_version]["members"])}')
        if 'chord' in PI_FAMILY[self.pi_version]['members']:
            if not isinstance(runtime.get('transform'), str) or ':' in runtime['transform']:
                raise base.Failure('the legacy chord-context esbuild transform kept a TypeScript '
                                   'annotation: ' + repr(runtime.get('transform')))
            if runtime.get('nativePackage') not in ('@esbuild/linux-arm64', '@esbuild/linux-x64'):
                raise base.Failure('the transform did not run under a platform native package: '
                                   + repr(runtime.get('nativePackage')))
        else:
            for absent in MODERN_ABSENT:
                if absent in runtime.get('dependencies', []) or absent in runtime['shape']:
                    raise base.Failure(f'{absent} is installed in a selection that must not ship it')
            if 'transform' in runtime or 'native' in runtime:
                raise base.Failure('a modern selection ran the legacy transform probe')
        if not runtime.get('genai') or not runtime.get('protobuf'):
            raise base.Failure('the genai/protobufjs probes reported no result')
        verified = self.json_line(self.action(main, 'verify', seconds=600), 'verify')['verify']
        self.evidence('verify.json', verified)
        if verified['status'] != 0:
            raise base.Failure('verify-pi.sh failed; see the verification log')
        if (verified.get('pi'), verified.get('bridge')) != ('installed', 'installed'):
            raise base.Failure('the verifier did not publish both independent health records: '
                               + json.dumps(verified))
        registration = self.json_line(self.action(main, 'registration', seconds=300),
                                      'registration')['registration']
        self.evidence('registration.json', registration)
        if registration['status'] != 0:
            raise base.Failure('the bridge registration probe exited ' + str(registration['status']))
        if len(registration['registered']) != 1 or registration['missing'] or registration['malformed']:
            raise base.Failure('registration is not one unambiguous positive: ' + json.dumps(registration))
        pair = (self.pi_version, self.bridge_version)
        expected_models = BRIDGE_MODELS.get(pair)
        if expected_models is None:
            raise base.Failure(f'there is no measured bridge catalog for Pi {self.pi_version} with '
                               'bridge ' + self.bridge_version + '; the measured pairs are '
                               + json.dumps(['Pi ' + pi + ' with bridge ' + bridge
                                             for pi, bridge in BRIDGE_MODELS]))
        if int(registration['registered'][0]) != expected_models:
            raise base.Failure(f'the bridge registered {registration["registered"][0]} models; the '
                               f'measured catalog for Pi {self.pi_version} with bridge '
                               f'{self.bridge_version} is {expected_models} '
                               '-- stop for review instead of accepting a different number')
        self.resolution_leg(main)
        dispatch = self.read(main, PI_PREFIX + '/agent-vm-subcommands')
        self.evidence('dispatch.json', dispatch)
        published = tuple(dispatch['text'].split('\n'))[:len(DISPATCH[self.pi_version])]
        if published != DISPATCH[self.pi_version]:
            raise base.Failure(f'the published subcommand list {published} is not the reviewed '
                               f'{DISPATCH[self.pi_version]}')
        seeds = []
        for line in self.action(main, 'seed', seconds=300).splitlines():
            record = json.loads(line)
            if 'seed' in record:
                seeds.append(record['seed'])
        self.evidence('seed.json', seeds)
        expected_seed = {
            'absent-binary': {'wrote': False}, 'fresh': {'wrote': True},
            'idempotent': {'identical': True}, 'preserved': {'identical': True},
            'invalid-json': {'identical': True}, 'scalar': {'identical': True},
            'array': {'identical': True}, 'provider-shape': {'identical': True},
            'mode': {'mode': '0o640'}, 'absent-state': {'wrote': False}}
        if [record['case'] for record in seeds] != list(SEED_CASES):
            raise base.Failure('the seed cases are not the reviewed set: '
                               + json.dumps([record['case'] for record in seeds]))
        for record in seeds:
            for key, value in expected_seed[record['case']].items():
                if record.get(key) != value:
                    raise base.Failure(f'seed {record["case"]}: {key}={record.get(key)!r}, '
                                       f'reviewed {value!r}')
        if seeds[1].get('pathToClaudeCodeExecutable') != '/opt/agent/.local/bin/claude':
            raise base.Failure('the seed wrote another executable path')
        mutants = []
        for line in self.action(main, 'mutants', anchor, self.pi_version, seconds=600).splitlines():
            record = json.loads(line)
            if 'mutant' in record:
                mutants.append(record['mutant'])
        self.evidence('mutants.json', mutants)
        if not mutants:
            raise base.Failure('no layout mutant was reported')
        for record in mutants:
            if record['code'] != record['expect']:
                raise base.Failure(f'{record["mutant"]}: diagnostic {record["code"]} is not the '
                                   f'expected {record["expect"]}: {record["diagnostic"]}')
            if record['repairedStatus'] != 0:
                raise base.Failure(f'{record["mutant"]}: the repaired tree did not pass')
        self.records_leg(main)
        self.identity_leg(main)
        print('provenance: diagnostic disposable container; the installed payload is not published '
              'anywhere and this is not product certification', flush=True)

    def read(self, main, path):
        return self.json_line(self.action(main, 'read', path, seconds=120), 'read')['read']

    def resolution_leg(self, main):
        """The bridge and its SDK resolve where the loader looks, and nowhere else.

        A model count is a fact about a provider, never about which copy of the
        payload supplied it. The fixture answers the loader's own question from the
        requiring directory -- the prefix for the bridge, the resolved bridge for the
        SDK -- because a nested
        `pi-claude-bridge/node_modules/@anthropic-ai/claude-agent-sdk` is tried before
        the prefix's copy and would be what the bridge actually imported. The
        expected path is therefore compared against the answer, and every other copy
        the loader could reach (nested or above) is a failure. The entry is the one the
        wrapper registers: a payload with no `main` is loadable TypeScript here, so
        requiring a manifest entry field would refuse the image's own payload.
        """
        report = self.json_line(self.action(main, 'resolution', self.bridge_version, seconds=120),
                                'resolution')['resolution']
        self.evidence('resolution.json', report)
        expected = {'bridge': BRIDGE_PREFIX + '/node_modules/pi-claude-bridge',
                    'sdk': BRIDGE_PREFIX + '/node_modules/@anthropic-ai/claude-agent-sdk'}
        for label, path in expected.items():
            record = report.get(label) or {}
            if record.get('expected') != path:
                raise base.Failure(f'the {label} report does not name {path} as the expected copy')
            if record.get('path') != path:
                raise base.Failure(f'the loader resolves the {label} to {record.get("path")!r}, the '
                                   f'reviewed payload is {path}')
            if not record.get('entryRegular') or not record.get('entryInside'):
                raise base.Failure(f'the {label} entry {record.get("entry")!r} is not a regular '
                                   'file inside the resolved package')
            if record.get('competing'):
                raise base.Failure(f'the {label} has another copy the loader could load: '
                                   + json.dumps(record['competing']))
        bridge_record, sdk_record = report.get('bridge') or {}, report.get('sdk') or {}
        # Only the bridge's entry is pinned: it is the path the image registers, so a
        # payload that moved it would leave the wrapper loading nothing. The SDK's own
        # entry belongs to its SRI-authenticated archive and Node's resolver reads it.
        if bridge_record.get('entry') != BRIDGE_ENTRY:
            raise base.Failure(f'the bridge entry is {bridge_record.get("entry")!r}, the image '
                               f'registers {BRIDGE_ENTRY}')
        if sdk_record.get('resolvedFrom') != bridge_record.get('path'):
            raise base.Failure('the SDK was resolved from '
                               f'{sdk_record.get("resolvedFrom")!r}, not from the resolved bridge '
                               f'{bridge_record.get("path")!r}, so the report cannot say what the '
                               'bridge imports')
        if bridge_record.get('version') != self.bridge_version:
            raise base.Failure('the resolved bridge is version '
                               + str((report.get('bridge') or {}).get('version'))
                               + ', the selection asked for ' + self.bridge_version)

    def fetch(self, main, container_path, name):
        destination = self.scratch / name
        self.docker('cp', main + ':' + container_path, str(destination))
        return json.loads(destination.read_text())

    def records_leg(self, main):
        """The two health records stay independent under a real bridge failure."""
        self.action(main, 'break-bridge', seconds=120)
        try:
            for slot in ('pi', 'pi-claude-bridge'):
                self.docker('exec', main, 'sh', '-c', f'printf "pending\\n" > /opt/agent-vm/install-status/{slot}')
            failed = self.json_line(self.action(main, 'verify', seconds=600), 'verify')['verify']
            if failed['status'] == 0:
                raise base.Failure('the verifier accepted a bridge whose source is missing')
            if failed.get('pi') != 'installed':
                raise base.Failure('a bridge failure cost the independently installed Pi record: '
                                   + json.dumps(failed))
            if failed.get('bridge') == 'installed':
                raise base.Failure('a failed bridge gate still published its own record')
            self.evidence('verify-bridge-failure.json', failed)
        finally:
            self.action(main, 'repair-bridge', seconds=120)
        for slot in ('pi', 'pi-claude-bridge'):
            self.docker('exec', main, 'sh', '-c', f'printf "pending\\n" > /opt/agent-vm/install-status/{slot}')
        repaired = self.json_line(self.action(main, 'verify', seconds=600), 'verify')['verify']
        self.evidence('verify-repaired.json', repaired)
        if (repaired['status'], repaired.get('pi'), repaired.get('bridge')) != (0, 'installed', 'installed'):
            raise base.Failure('the repaired bridge did not restore both records')

    def identity_leg(self, main):
        """Extracted layout oracle, then offline capability-zero numeric pairs."""
        image_root = Path(tempfile.mkdtemp(prefix='pi-image-root-'))
        try:
            for directory in ('opt/agent-vm', 'usr/local/bin'):
                (image_root / directory).mkdir(parents=True)
            self.docker('cp', main + ':' + PI_PREFIX, str(image_root / 'opt/agent-vm/pi'))
            self.docker('cp', main + ':' + BRIDGE_PREFIX, str(image_root / 'opt/agent-vm/pi-packages'))
            self.docker('cp', main + ':' + WRAPPER, str(image_root / 'usr/local/bin/pi'))
            self.command('python3', str(ROOT / 'script/test/standard-node-layout.py'),
                         '--consumer', 'pi', '--root', str(image_root),
                         '--platform', self.platform, '--expect-version', self.pi_version)
            self.command('python3', str(ROOT / 'script/test/standard-node-layout.py'),
                         '--consumer', 'bridge', '--root', str(image_root),
                         '--platform', self.platform, '--expect-version', self.bridge_version)
        finally:
            shutil.rmtree(image_root, ignore_errors=True)
        info = json.loads(self.docker('inspect', '--size', main))[0]
        self.setup_rw += int(info['SizeRw'])
        self.rw_kib = (self.setup_rw + 1023) // 1024
        self.capacity(RESERVE_KIB)
        self.docker('network', 'disconnect', 'bridge', main)
        if json.loads(self.docker('inspect', '--format', '{{json .NetworkSettings.Networks}}', main)):
            raise base.Failure('network attachments remain before offline runtime')
        seen = set()
        for uid, gid in base.PAIRS:
            home = '/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}'
            output = self.docker('exec', '-e', 'AGENT_VM_HARNESS_PLATFORM=' + self.platform,
                                 '-e', 'HOME=' + home, main, 'timeout', '--kill-after=5',
                                 '300', 'setpriv', '--bounding-set=-all', '--inh-caps=-all',
                                 '--ambient-caps=-all', '--reuid', str(uid), '--regid', str(gid),
                                 '--clear-groups', '--no-new-privs',
                                 'python3', '-B', BODY, 'probe', self.token, self.source,
                                 self.pi_version, self.bridge_version, str(uid), str(gid))
            record = self.json_line(output, 'piProbe')
            if record.get('piProbe') != self.token or record.get('source') != self.source:
                raise base.Failure('stale/duplicate/invalid Pi runtime evidence')
            if (record.get('pi'), record.get('bridge')) != (self.pi_version, self.bridge_version):
                raise base.Failure('runtime versions do not bind to the invocation selection')
            if record.get('pair') in seen:
                raise base.Failure('duplicate numeric pair evidence')
            if (record.get('caps'), record.get('offline'), record.get('wrapper')) != \
                    ('zero', True, 'regular-file'):
                raise base.Failure('the offline capability-zero/wrapper evidence is invalid')
            seen.add(record['pair'])
            print(output, end='', flush=True)
        self.action(main, 'clean', seconds=120)
        retained = self.check_budget()
        print(f'pi: retained {retained}/{PI_BUDGET_KIB} KiB (container layer + evidence)', flush=True)

    def capacity(self, minimum):
        name = self.create(self.token + '-capacity', '--platform', self.platform,
                           '--network', 'none', '--cap-drop', 'ALL', self.image,
                           'df', '-Pk', '/')
        output = self.docker('start', '-a', name)
        if self.docker('wait', name).strip() != '0':
            raise base.Failure('capacity container failed')
        info = json.loads(self.docker('inspect', '--size', name))[0]
        self.setup_rw += int(info['SizeRw'])
        self.remove(name)
        rows = [line.split() for line in output.splitlines()[1:] if line.strip()]
        if len(rows) != 1 or len(rows[0]) < 6 or int(rows[0][3]) < minimum:
            raise base.Failure(f'capacity: need {minimum} KiB available on the daemon writable '
                               f'layer: {rows}')
        print(f'capacity: daemon writable layer >= {minimum} KiB', flush=True)

    def select_contract(self):
        self.contract = base.contract_context(self.platform)
        binary = self.contract / 'out/recipe-contract'
        version = base.subprocess.run(['go', 'version', str(binary)], capture_output=True, text=True)
        if version.returncode:
            raise base.Failure('cannot read the helper compiler: ' + version.stderr.strip())
        record = {'context': str(self.contract),
                  'sha256': base.hashlib.sha256(binary.read_bytes()).hexdigest(),
                  'compiler': version.stdout.strip()}
        (self.logs / 'helper.json').write_text(json.dumps(record, indent=2) + '\n')
        print('contract: ' + record['compiler'] + ' ' + record['sha256'], flush=True)


def main(args):
    try:
        image, platform, pi_version, bridge_version = arguments(args)
        invocation = Invocation(image, platform, pi_version, bridge_version)
    except (ValueError, OSError) as error:
        print('standard-node-consumers: ' + str(error), file=sys.stderr)
        return 2
    status = 0

    def interrupted(number, _frame):
        raise base.Failure('signal ' + str(number), 128 + number)

    for sig in (base.signal.SIGINT, base.signal.SIGTERM, base.signal.SIGHUP):
        base.signal.signal(sig, interrupted)
    try:
        invocation.execute()
    except (base.Failure, OSError, ValueError, KeyError, IndexError) as error:
        print('standard-node-consumers: ' + str(error), file=sys.stderr)
        status = error.status if isinstance(error, base.Failure) else 1
    finally:
        try:
            invocation.cleanup()
        except base.Failure as error:
            print(str(error), file=sys.stderr)
            status = status or error.status
    if not status:
        print('standard-node-consumers: pi PASS (native focused diagnostic)')
    return status


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
