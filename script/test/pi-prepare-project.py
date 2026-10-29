#!/usr/bin/env python3
"""Preparation-driver sequencing table.

`images/tools/pi/prepare-project.py` is the one process that may prepare a Pi or
bridge lock, so its value is the order it enforces, not the commands it happens to
run. This suite proves that order with a recorded fake manager and a recorded fake
contract:

* an equal, unrefreshed selection starts no manager and writes nothing into the
  prefix;
* a refused candidate comparison pins nothing, installs nothing and publishes
  nothing, whether the refusal comes from the graph verb or from a failed frozen
  install;
* a successful change publishes exactly four inputs, and only after the pin, the
  frozen install and the recheck;
* the maintainer adoption reads the fixed baseline from Git rather than the
  working tree, removes the import-only npm lock before the candidate is
  validated, and writes an external proposal;
* a manifest without the manager pin, an unauthorized proposal destination and a
  refused import all fail before publication.

The fake contract is not a graph validator; the real Go helper owns that and its
own suites cover it. What is under test here is that the driver cannot skip it.
"""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True


def alive_pid(pid):
    """True while `pid` exists and is not a zombie we cannot see the parent of."""
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True

ROOT = Path(__file__).resolve().parents[2]
DRIVER = ROOT / 'images/tools/pi/prepare-project.py'
BASELINE = 'bc1b2df'
PI_ROOT = '@earendil-works/pi-coding-agent'
LEGACY = ('chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui')
# Any syntactically complete sha512 SRI will do for a record whose bytes are never
# fetched; the fixture's real SRIs are hashed from the tarballs it writes.
PLACEHOLDER_SRI = 'sha512-' + base64.b64encode(b'x' * 64).decode()

FAKE_CONTRACT = r'''#!/usr/bin/env python3
import hashlib, json, os, sys
from pathlib import Path
args = sys.argv[1:]
state = Path(__file__).resolve().parent.parent / 'state'
with (state / 'calls.jsonl').open('a') as handle:
    handle.write(json.dumps(args) + '\n')
verb = args[0] if args else ''
if verb == 'pnpm-environment':
    sys.exit(0)
if verb == 'pnpm-json':
    # Syntax-only canonical JSON: the real adapter rejects duplicate keys, depth
    # and size abuse before any consumer sees the document.
    print(json.dumps(json.loads(Path(args[1]).read_text()), sort_keys=True))
    sys.exit(0)
if verb == 'pnpm-lock':
    action, kind, prefix = args[1], args[2], Path(args[3])
    if action == 'pin':
        body = ''.join('{}  {}\n'.format(
            hashlib.sha256((prefix / name).read_bytes()).hexdigest(), name)
            for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'))
        (prefix / 'pnpm-inputs.sha256').write_text(body)
        sys.exit(0)
    if action == 'check':
        if (state / 'refuse-check').exists():
            print('check refused', file=sys.stderr)
            sys.exit(1)
        (state / 'checks').write_text(
            ((state / 'checks').read_text() if (state / 'checks').exists() else '') + 'check\n')
        sys.exit(0)
if verb == 'pnpm-lock-candidate':
    mode = args[1]
    (state / ('authority-' + mode)).write_text(
        'yes' if '--shrinkwrap-authority' in args else 'no')
    (state / ('refresh-' + mode)).write_text('yes' if '--refresh-lock' in args else 'no')
    scratch = Path(args[args.index('--scratch') + 1])
    names = sorted(item.name for item in (scratch / 'candidate').iterdir())
    (state / 'candidate-inventory.json').write_text(json.dumps(names))
    # The candidate carries exactly the three unpinned files: an import-only npm
    # lock or a pin file left behind is a sequencing failure, not a graph one.
    if names != ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml']:
        print('candidate inventory: ' + ','.join(names), file=sys.stderr)
        sys.exit(1)
    if (state / 'refuse-candidate').exists():
        print('candidate refused', file=sys.stderr)
        sys.exit(1)
    if mode == 'graph':
        if '--before' not in args or '--requested-version' not in args:
            print('graph requires a reviewed predecessor', file=sys.stderr)
            sys.exit(1)
        (state / 'before').write_text(args[args.index('--before') + 1])
        (state / 'version').write_text(args[args.index('--requested-version') + 1])
    else:
        if '--before' in args or '--requested-version' in args:
            print('adoption has no predecessor', file=sys.stderr)
            sys.exit(1)
    (state / 'candidate-order').write_text(
        ((state / 'candidate-order').read_text()
         if (state / 'candidate-order').exists() else '') + 'candidate\n')
    digests = {name: hashlib.sha256((scratch / 'candidate' / name).read_bytes()).hexdigest()
               for name in names}
    # The projected root record: the archive ledger's own SRI for the requested
    # selection, unless the case substitutes a second tarball for the same version
    # the way an equivocating registry would.
    graph = {'snapshots': {}}
    candidate_manifest = json.loads((scratch / 'candidate' / 'package.json').read_text())
    for root, wanted in (candidate_manifest.get('dependencies') or {}).items():
        ledger = json.loads((state / 'archives.json').read_text())
        integrity = (ledger['metadata'].get(root + '@' + wanted) or {}).get('dist.integrity', '')
        if (state / 'equivocate-root').exists():
            integrity = (state / 'equivocate-root').read_text().strip()
        if not (state / 'drop-root').exists():
            graph['packages'] = [{'id': {'name': root, 'version': wanted},
                                  'integrity': integrity}]
    out = {'inputState': 'candidate', 'inputDigests': digests, 'graph': graph}
    if mode == 'graph':
        out['selectionDelta'] = {'mode': 'change'}
    print(json.dumps(out))
    sys.exit(0)
print('unknown verb ' + verb, file=sys.stderr)
sys.exit(2)
'''

FAKE_RUN_PNPM = '''#!/bin/sh
# The real envelope owns the 600s/5s budget; the fake keeps the argv contract.
result=$1
shift 2
if ! "$@" >"$result" 2>&1; then
    exit 1
fi
'''

FAKE_PNPM = r'''#!/bin/sh
log=$(dirname "$0")/state/manager.jsonl
printf '%s\n' "$*" >>"$log"
case " $* " in
    *" --version "*)
        printf '11.11.0\n'
        exit 0
        ;;
esac
state=$(dirname "$0")/state
case "$1" in
    view)
        if [ -e "$state/refuse-metadata" ]; then
            printf 'metadata refused\n' >&2
            exit 1
        fi
        exec "$(dirname "$0")/fixture-view" "$2"
        ;;
    import)
        # `pnpm import` consumes the npm-shaped project lock the driver synthesized
        # from the authenticated shrinkwrap. A run in which the importer saw no such
        # lock would prove nothing about the synthesis step, so the fake refuses it
        # and keeps the document it was actually handed.
        if [ ! -f package-lock.json ]; then
            printf 'import without a synthesized package-lock.json\n' >&2
            exit 3
        fi
        cp package-lock.json "$state/import-input.json"
        printf '%s\n' '["pnpm", "import"]' >>"$state/calls.jsonl"
        printf 'lockfileVersion: 9.0\n' >pnpm-lock.yaml
        exit 0
        ;;
    install)
        case " $* " in
            *" --lockfile-only "*)
                if [ -e "$state/hang-install" ]; then
                    # A manager that keeps working past an interruption, with a
                    # grandchild: the process group is the only thing that ends both.
                    printf '%s\n' "$$" >"$state/child.pid"
                    sleep 300 &
                    printf '%s\n' "$!" >"$state/grandchild.pid"
                    wait
                    exit 0
                fi
                # Record whether a reviewed lock was seeded, so the caller can tell
                # incremental maintenance from a fresh resolution.
                if [ -f pnpm-lock.yaml ]; then
                    printf 'seeded\n' >>"$state/seed.log"
                    cp pnpm-lock.yaml "$state/seed.txt"
                else
                    printf 'fresh\n' >>"$state/seed.log"
                fi
                printf 'lockfileVersion: 9.0\n' >pnpm-lock.yaml
                exit 0
                ;;
            *" --frozen-lockfile "*)
                if [ -e "$(dirname "$0")/state/refuse-install" ]; then
                    printf 'frozen install refused\n' >&2
                    exit 1
                fi
                mkdir -p node_modules
                printf 'payload\n' >node_modules/marker
                exit 0
                ;;
        esac
        ;;
esac
printf 'fake pnpm: unexpected argv: %s\n' "$*" >&2
exit 2
'''


# The metadata answer for one exact selection, from the fixture's own archive
# ledger. The registry is never contacted: the archive the ledger names is the one
# `download.sh` will hand back, so the driver's SRI check is a real rehash.
FAKE_VIEW = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
state = Path(__file__).resolve().parent / 'state'
ledger = json.loads((state / 'archives.json').read_text())
record = ledger['metadata'].get(sys.argv[1])
if record is None:
    print('no such selection: ' + sys.argv[1], file=sys.stderr)
    sys.exit(1)
print(json.dumps(record))
'''

# The classified contract is a POSIX sh helper, so the fixture is one too: a
# fixture `download.sh` may be handed to `sh` and writes the receipt the real
# contract writes. A transport refusal proves preparation has no soft path.
FAKE_DOWNLOAD = r'''#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd -P)
if [ -e "$root/state/refuse-download" ]; then
    printf 'transport download 7\n' >"${AGENT_VM_TRANSPORT_RECEIPT:-$root/state/download.receipt}"
    exit 75
fi
source=$(awk -v url="$1" '$1 == url { print $2 }' "$root/state/download-map.txt")
[ -n "$source" ] || { echo "no such tarball: $1" >&2; exit 1; }
cp "$source" "$2"
'''

FAKE_NODE = r'''#!/usr/bin/env python3
import json, sys
from pathlib import Path
state = Path(__file__).resolve().parent / 'state'
argv = sys.argv[1:]
# Node's argv begins with the script path; the verb follows it.
verb = argv[1] if len(argv) > 1 else ''
args = argv[1:]
with (state / 'node.jsonl').open('a') as handle:
    handle.write(json.dumps(argv) + '\n')
with (state / 'calls.jsonl').open('a') as handle:
    handle.write(json.dumps(['node', verb]) + '\n')
if verb == 'synthesize':
    captured = {}
    for index, word in enumerate(args):
        if word in ('--manifest', '--shrinkwrap', '--root', '--siblings'):
            captured[word[2:]] = json.loads(Path(args[index + 1]).read_text())
    (state / 'synthesize-inputs.json').write_text(json.dumps(captured, sort_keys=True))
    print(json.dumps({'name': 'pi-selected-import', 'lockfileVersion': 3,
                      'requires': True, 'packages': {}}))
    sys.stderr.write('completed SRIs: []\n')
    sys.exit(0)
if verb == 'validate':
    (state / 'validate-argv.json').write_text(json.dumps(args))
    # The review is a required input of the delta comparison: the driver must name
    # the reviewed normalisations for THIS selection, and the file must be a list.
    if '--reviewed' not in args:
        print('fake node: validate without --reviewed', file=sys.stderr)
        sys.exit(2)
    reviewed = json.loads(Path(args[args.index('--reviewed') + 1]).read_text())
    if not isinstance(reviewed, list):
        print('fake node: --reviewed is not a list', file=sys.stderr)
        sys.exit(2)
    (state / 'reviewed.json').write_text(json.dumps(reviewed))
    if (state / 'refuse-delta').exists():
        print(json.dumps({'accepted': False, 'rejected': [{'code': 'sri-changed',
                                                           'detail': 'fixture'}],
                          'reported': [], 'counts': {}}))
        sys.exit(1)
    print(json.dumps({'accepted': True, 'rejected': [], 'reported': [], 'counts': {},
                      'supplements': []}))
    sys.exit(0)
print('fake node: unexpected verb ' + verb, file=sys.stderr)
sys.exit(2)
'''

class DriverTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix='pi-prepare-')).resolve()
        self.contract = self.root / 'recipe-contract'
        self.contract.mkdir()
        self.write(self.contract / 'recipe-contract', FAKE_CONTRACT, 0o755)
        self.write(self.contract / 'run-pnpm.sh', FAKE_RUN_PNPM, 0o755)
        (self.root / 'pnpm').mkdir()
        self.write(self.root / 'pnpm/pin.json', '{}\n', 0o644)
        self.manager = self.root / 'fake-pnpm'
        self.write(self.manager, FAKE_PNPM, 0o755)
        self.write(self.root / 'fixture-view', FAKE_VIEW, 0o755)
        self.write(self.contract / 'download.sh', FAKE_DOWNLOAD, 0o755)
        self.state = self.root / 'state'
        self.state.mkdir()
        node = self.root / 'fake-node'
        self.write(node, '#!' + sys.executable + '\n' + FAKE_NODE.split('\n', 1)[1], 0o755)
        self.ledger = {'metadata': {}, 'tarballs': {}}
        self.env = patch.dict(os.environ, {
            'AGENT_VM_FIXTURE_ROOT': str(self.root),
            'AGENT_VM_CONTRACT_DIR': str(self.contract),
            'AGENT_VM_FIXTURE_PNPM': str(self.manager),
            'AGENT_VM_FIXTURE_NODE': str(node),
        })
        self.env.start()
        self.prefix = self.root / 'prefix'
        self.make_prefix(self.prefix, '1.4.0')
        # The bridge project is its own single-root prefix with its own selection.
        self.bridge_prefix = self.root / 'bridge-prefix'
        self.make_prefix(self.bridge_prefix, '0.8.0', root='pi-claude-bridge')
        # The ordinary changed selection: a published archive with no dependency
        # shrinkwrap, so the range route stays the one under test elsewhere.
        self.archive(PI_ROOT, '1.4.1')

    def tearDown(self):
        self.env.stop()
        shutil.rmtree(self.root, ignore_errors=True)

    def write(self, path, body, mode):
        path.write_text(body)
        os.chmod(path, mode)

    # --- authenticated-archive fixtures -------------------------------------

    def write_tarball(self, path, entries):
        """Write one gzip tar; `entries` maps member name to bytes."""
        path.parent.mkdir(parents=True, exist_ok=True)
        with tarfile.open(path, 'w:gz') as tar:
            for member, body in entries.items():
                info = tarfile.TarInfo(member)
                info.size = len(body)
                info.mode = 0o644
                tar.addfile(info, io.BytesIO(body))

    def ledger_written(self):
        (self.state / 'archives.json').write_text(json.dumps(self.ledger, indent=2))
        (self.state / 'download-map.txt').write_text(
            ''.join(f'{url} {path}\n' for url, path in self.ledger['tarballs'].items()))

    def register(self, name, version, path, integrity=None):
        """Record one archive's registry facts the way the registry would."""
        base = name.rsplit('/', 1)[-1]
        url = f'https://registry.npmjs.org/{name}/-/{base}-{version}.tgz'
        if integrity is None:
            integrity = 'sha512-' + base64.b64encode(
                hashlib.sha512(path.read_bytes()).digest()).decode()
        self.ledger['metadata'][f'{name}@{version}'] = {
            'name': name, 'version': version,
            'dist.integrity': integrity, 'dist.tarball': url}
        self.ledger['tarballs'][url] = str(path)
        self.ledger_written()
        return {'name': name, 'version': version, 'integrity': integrity, 'url': url}

    def archive(self, name, version, shrinkwrap=None):
        """One package archive, with or without a dependency shrinkwrap."""
        base = name.rsplit('/', 1)[-1]
        entries = {'package/package.json': (json.dumps(
            {'name': name, 'version': version}, indent=2) + '\n').encode()}
        if shrinkwrap is not None:
            entries['package/npm-shrinkwrap.json'] = json.dumps(shrinkwrap, indent=2).encode()
        path = self.state / 'archives' / f'{base}-{version}.tgz'
        self.write_tarball(path, entries)
        return self.register(name, version, path)

    def siblings(self, version):
        for base in LEGACY:
            self.archive(f'@earendil-works/{base}', version)

    def pi_shrinkwrap(self, version, omitted=None, sibling_versions=None, extra=None):
        """A legacy-shaped Pi shrinkwrap: root plus the five same-version siblings.

        `omitted` names the sibling records whose `integrity` is absent (the
        published 0.87.0 shape omits all five). `extra` adds one more record, so
        the tests can present a non-legacy missing SRI.
        """
        omitted = set(LEGACY if omitted is None else omitted)
        sibling_versions = sibling_versions or {}
        packages = {'': {'name': PI_ROOT, 'version': version,
                         'license': 'MIT',
                         'dependencies': {f'@earendil-works/{base}': f'^{version}'
                                          for base in LEGACY if base != 'pi-telemetry'}}}
        for base in LEGACY:
            pinned = sibling_versions.get(base, version)
            record = {'version': pinned,
                      'resolved': (f'https://registry.npmjs.org/@earendil-works/{base}'
                                   f'/-/{base}-{pinned}.tgz')}
            if base not in omitted:
                record['integrity'] = PLACEHOLDER_SRI
            packages[f'node_modules/@earendil-works/{base}'] = record
        packages.update(extra or {})
        return {'name': PI_ROOT, 'version': version, 'lockfileVersion': 3,
                'requires': True, 'packages': packages}

    def request_shrinkwrapped(self, version, shrinkwrap):
        """Register the selected archive plus its siblings and previews."""
        if shrinkwrap is not None:
            self.siblings(version)
        return self.archive(PI_ROOT, version, shrinkwrap)

    def make_prefix(self, prefix, version, root='@earendil-works/pi-coding-agent'):
        prefix.mkdir(mode=0o700)
        manifest = {
            'name': 'agent-vm-guest-pi',
            'version': '0.0.0',
            'private': True,
            'description': 'fixture',
            'license': 'MIT',
            'packageManager': 'pnpm@11.11.0',
            'dependencies': {root: version},
        }
        self.write(prefix / 'package.json', json.dumps(manifest, indent=2) + '\n', 0o644)
        self.write(prefix / 'pnpm-lock.yaml', 'lockfileVersion: 9.0\n', 0o644)
        self.write(prefix / 'pnpm-workspace.yaml', 'packages: [.]\n', 0o644)
        self.write(prefix / 'pnpm-inputs.sha256', '', 0o644)

    def image(self, prefix):
        return {name: (prefix / name).read_bytes() for name in
                ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml',
                 'pnpm-inputs.sha256')}

    def calls(self):
        """Contract invocations, without the environment guard that precedes them."""
        return [call for call in (json.loads(line) for line in
                                  (self.state / 'calls.jsonl').read_text().splitlines() if line)
                if call[0] != 'pnpm-environment']

    def invocation_order(self):
        """The contract, converter and manager calls as one recorded sequence."""
        names = []
        for call in self.calls():
            if call[0] == 'node':
                names.append('node-' + call[1])
            elif call[0] == 'pnpm':
                names.append('pnpm-' + call[1])
            elif call[0] != 'pnpm-json':
                names.append(call[0])
        return names

    def assert_order(self, expected):
        self.assertEqual(self.invocation_order(), expected)

    def manager_calls(self):
        if not (self.state / 'manager.jsonl').exists():
            return []
        return [line for line in
                (self.state / 'manager.jsonl').read_text().splitlines() if line]

    def run_driver(self, *argv):
        return subprocess.run([sys.executable, str(DRIVER), *argv],
                              capture_output=True, text=True)

    # --- the ordinary route -------------------------------------------------

    def test_equal_selection_starts_no_manager_and_writes_nothing(self):
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), {'mode': 'equal', 'consumer': 'pi',
                                                     'version': '1.4.0', 'managerCalls': 0})
        self.assertEqual(self.manager_calls(), [])
        self.assertEqual(self.calls(), [['pnpm-lock', 'check', 'pi', str(self.prefix)]])
        self.assertEqual(self.image(self.prefix), before)

    def test_equal_with_an_explicit_version_is_the_same_no_op(self):
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.manager_calls(), [])

    def test_change_publishes_only_after_pin_and_frozen_install(self):
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['mode'], 'change')
        calls = self.manager_calls()
        kinds = ['version' if '--version' in line else
                 'resolve' if '--lockfile-only' in line else
                 'frozen' if '--frozen-lockfile' in line else 'other' for line in calls]
        self.assertEqual([kind for kind in kinds if kind != 'other'],
                         ['version', 'resolve', 'frozen'])
        verbs = [call[0:2] for call in self.calls()]
        self.assertIn(['pnpm-lock', 'pin'], verbs)
        self.assertLess(verbs.index(['pnpm-lock', 'pin']), verbs.index(['pnpm-lock', 'check']),
                        'a pin must be written before the ordinary check re-reads it')
        # The candidate is compared with the reviewed prefix as its predecessor.
        self.assertEqual((self.state / 'before').read_text(), str(self.prefix))
        self.assertEqual((self.state / 'version').read_text(), '1.4.1')
        # Publication writes the staged bytes, including the new pins, and no
        # installed payload.
        published = json.loads((self.prefix / 'package.json').read_text())
        self.assertEqual(published['dependencies']['@earendil-works/pi-coding-agent'], '1.4.1')
        digests = (self.prefix / 'pnpm-inputs.sha256').read_text()
        for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'):
            expected = hashlib.sha256((self.prefix / name).read_bytes()).hexdigest()
            self.assertIn(f'{expected}  {name}\n', digests)
        self.assertFalse((self.prefix / 'node_modules').exists())

    def test_refused_candidate_pins_and_publishes_nothing(self):
        (self.state / 'refuse-candidate').write_text('1')
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1)
        self.assertIn('candidate refused', result.stderr)
        self.assertEqual(self.image(self.prefix), before)
        self.assertNotIn(['pnpm-lock', 'pin'],
                         [call[0:2] for call in self.calls()])
        self.assertNotIn('frozen', [line for line in self.manager_calls()
                                    if '--frozen-lockfile' in line])

    def test_failed_frozen_install_publishes_nothing(self):
        (self.state / 'refuse-install').write_text('1')
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.image(self.prefix), before)

    def test_an_equivocating_registry_cannot_replace_the_authenticated_root(self):
        """The range route must pin the archive whose SRI was verified.

        The selected archive is authenticated as A and carries no shrinkwrap. A
        registry that answers B -- another tarball for the same exact version, with
        a different SRI and, say, its own dependency shrinkwrap -- would have B
        pinned without the shrinkwrap comparator ever seeing it. Identity and
        version are equal in that case, so only the SRI can tell them apart.
        """
        before = self.image(self.prefix)
        other = 'sha512-' + base64.b64encode(hashlib.sha512(b'second tarball').digest()).decode()
        (self.state / 'equivocate-root').write_text(other)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn('not the authenticated', result.stderr)
        self.assertIn(other, result.stderr)
        self.assertEqual(self.image(self.prefix), before)
        self.assertNotIn(['pnpm-lock', 'pin'], [call[0:2] for call in self.calls()])
        self.assertNotIn('frozen', [line for line in self.manager_calls()
                                    if '--frozen-lockfile' in line])

    def test_the_resolved_root_must_be_present_exactly_once(self):
        before = self.image(self.prefix)
        # No package record at all: the driver cannot confirm the SRI, so it must
        # refuse rather than treat an absent projection as agreement.
        (self.state / 'drop-root').write_text('1')
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn('not exactly one', result.stderr)
        self.assertEqual(self.image(self.prefix), before)

    def test_manifest_without_the_manager_pin_is_refused_before_any_manager(self):
        manifest = json.loads((self.prefix / 'package.json').read_text())
        del manifest['packageManager']
        (self.prefix / 'package.json').write_text(json.dumps(manifest, indent=2) + '\n')
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1)
        self.assertIn('pnpm@11.11.0', result.stderr)
        self.assertEqual(self.manager_calls(), [])

    def test_unexact_version_is_refused_before_any_manager(self):
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), 'latest')
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.manager_calls(), [])

    # --- the maintainer adoption route --------------------------------------

    def test_adoption_writes_the_proposal_and_drops_the_import_lock(self):
        proposal = self.root / 'proposal'
        result = self.run_driver('--consumer', 'pi', 'adopt', str(proposal))
        self.assertEqual(result.returncode, 0, result.stderr)
        summary = json.loads(result.stdout)
        self.assertEqual(summary['mode'], 'adopt')
        self.assertEqual(summary['version'], '1.4.0')
        # The baseline is the fixed commit, not the working tree.
        resolved = subprocess.run(['git', '-C', str(ROOT), 'rev-parse',
                                   BASELINE + '^{commit}'], capture_output=True, text=True)
        self.assertEqual(summary['baseline'], resolved.stdout.strip())
        for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml',
                     'pnpm-inputs.sha256'):
            self.assertTrue((proposal / name).is_file(), name)
            self.assertEqual(summary['digests'][name],
                             hashlib.sha256((proposal / name).read_bytes()).hexdigest())
        self.assertFalse((proposal / 'package-lock.json').exists())
        # No predecessor on the adoption verb, and the candidate inventory was the
        # three unpinned files (the fake contract refused anything else).
        # One comparison, one pin, the ordinary pinned check, and a recheck after
        # the frozen install; there is never a `graph` call on the adoption verb.
        self.assert_order(['pnpm-import', 'pnpm-lock-candidate', 'pnpm-lock', 'pnpm-lock',
                           'pnpm-lock'])
        self.assertEqual([call[0:2] for call in self.calls()],
                         [['pnpm', 'import'], ['pnpm-lock-candidate', 'adoption-graph'],
                          ['pnpm-lock', 'pin'], ['pnpm-lock', 'check'],
                          ['pnpm-lock', 'check']])
        manifest = json.loads((proposal / 'package.json').read_text())
        self.assertEqual(manifest['packageManager'], 'pnpm@11.11.0')
        self.assertEqual(manifest['dependencies'],
                         {'@earendil-works/pi-coding-agent': '1.4.0'})
        self.assertIn("trustPolicyExclude: ['undici-types@6.21.0']",
                      (proposal / 'pnpm-workspace.yaml').read_text())
        self.assertFalse((proposal / 'node_modules').exists())

    def test_adoption_never_deletes_a_directory_it_did_not_create(self):
        """The staging holder is allocated, not claimed.

        A fixed `<authorized-root>/<consumer>-proposal-stage` had to be removed
        before it could be used, so anything already at that path was deleted
        recursively without any proof this invocation created it. The holder is now
        unique per invocation, and a pre-existing path with the same old name is
        untouched evidence rather than scratch.
        """
        squatter = self.root / 'pi-proposal-stage'
        squatter.mkdir(mode=0o700)
        sentinel = squatter / 'not-ours.txt'
        sentinel.write_text('somebody else\n')
        proposal = self.root / 'proposal'
        result = self.run_driver('--consumer', 'pi', 'adopt', str(proposal))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(sentinel.is_file(), 'the pre-existing directory was deleted')
        self.assertEqual(sentinel.read_text(), 'somebody else\n')
        # Nothing of ours is left behind either: the unique holder is removed.
        leftovers = [entry.name for entry in self.root.iterdir()
                     if entry.name.startswith('pi-proposal-stage.')]
        self.assertEqual(leftovers, [], 'the proposal holder survived publication')
        for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml',
                     'pnpm-inputs.sha256'):
            self.assertTrue((proposal / name).is_file(), name)

    def test_adoption_of_the_bridge_uses_its_own_workspace(self):
        proposal = self.root / 'bridge-proposal'
        result = self.run_driver('--consumer', 'bridge', 'adopt', str(proposal))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['version'], '0.8.0')
        workspace = (proposal / 'pnpm-workspace.yaml').read_text()
        self.assertIn('nodeLinker: hoisted', workspace)
        self.assertIn("ignoredOptionalDependencies: ['@anthropic-ai/claude-agent-sdk-*']",
                      workspace)

    def test_adoption_refuses_a_proposal_outside_the_authorized_root(self):
        outside = Path(tempfile.mkdtemp(prefix='pi-proposal-')).resolve()
        self.addCleanup(shutil.rmtree, outside, True)
        result = self.run_driver('--consumer', 'pi', 'adopt', str(outside / 'proposal'))
        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.manager_calls(), [])
        self.assertFalse((outside / 'proposal').exists())

    def test_adoption_refuses_an_unknown_baseline(self):
        result = self.run_driver('--consumer', 'pi', 'adopt',
                                 str(self.root / 'proposal'), '--baseline', 'not-a-commit')
        self.assertEqual(result.returncode, 1)
        self.assertIn('baseline', result.stderr)
        self.assertEqual(self.manager_calls(), [])

    def test_refused_import_publishes_nothing(self):
        (self.state / 'refuse-install').write_text('1')
        proposal = self.root / 'proposal'
        result = self.run_driver('--consumer', 'pi', 'adopt', str(proposal))
        self.assertEqual(result.returncode, 1)
        self.assertFalse((proposal / 'package.json').exists())


    # --- the authenticated selected-archive route ---------------------------

    def selection_evidence(self, result):
        return json.loads(result.stdout)['evidence']

    def test_shrinkwrap_route_is_chosen_by_archive_content(self):
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = self.selection_evidence(result)
        self.assertEqual(evidence['route'], 'shrinkwrap')
        self.assertTrue(evidence['selectedArchive']['shrinkwrap'])
        # The authenticated archive is the import input, never a range resolve.
        manager = [line for line in self.manager_calls() if '--lockfile-only' not in line]
        self.assertTrue(any(' view ' in ' ' + line + ' ' for line in manager)
                        or any('view @earendil-works/pi-coding-agent@0.87.0' in line
                               for line in manager), self.manager_calls())
        self.assertFalse([line for line in self.manager_calls() if '--lockfile-only' in line])
        self.assertTrue([line for line in self.manager_calls() if ' import ' in ' ' + line + ' '])
        # Synthesis precedes import, and the delta comparison follows the candidate
        # comparison -- never the other way round.
        self.assert_order(['node-synthesize', 'pnpm-import', 'pnpm-lock-candidate',
                           'node-validate', 'pnpm-lock', 'pnpm-lock', 'pnpm-lock'])
        # The conversion completed exactly the five missing sibling SRIs, each from
        # its own authenticated metadata origin.
        receipts = evidence['supplementalSris']
        self.assertEqual(sorted(receipt['name'] for receipt in receipts),
                         sorted(f'@earendil-works/{base}' for base in LEGACY))
        for receipt in receipts:
            self.assertTrue(receipt['integrity'].startswith('sha512-'), receipt)
            self.assertIn('dist.integrity', receipt['origin'])
        self.assertEqual(evidence['shrinkwrapDelta']['accepted'], True)
        # The published prefix carries the requested selection and no npm lock.
        published = json.loads((self.prefix / 'package.json').read_text())
        self.assertEqual(published['dependencies'][PI_ROOT], '0.87.0')
        self.assertFalse((self.prefix / 'package-lock.json').exists())
        self.assertFalse((self.prefix / 'npm-shrinkwrap.json').exists())

    def reviewed_normalisations(self):
        return json.loads((self.state / 'reviewed.json').read_text())

    def test_the_reviewed_normalisations_are_the_ones_for_this_selection(self):
        """A reported normalisation is accepted only when this selection's review names it.

        The converter refuses any normalisation the caller did not list, so the file
        the driver passes is the review. A selection with a committed record must get
        exactly that record, and a selection without one must get an empty list --
        never a record borrowed from another version.
        """
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        record = json.loads((ROOT / 'images/tools/pi/reviewed-normalisations.json').read_text())
        self.assertEqual(self.reviewed_normalisations(),
                         record['selections'][PI_ROOT + '@0.87.0']['normalisations'])

    def test_a_selection_without_a_review_gets_no_normalisations(self):
        shrinkwrap = self.pi_shrinkwrap('1.4.1')
        self.request_shrinkwrapped('1.4.1', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.reviewed_normalisations(), [],
                         'a version nobody reviewed must not inherit 0.87.0 review')

    def test_an_unreviewed_normalisation_stops_before_pinning(self):
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        (self.state / 'refuse-delta').write_text('1')
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 1)
        self.assertIn('not the authenticated shrinkwrap graph', result.stderr)
        self.assertEqual(self.image(self.prefix), before)
        self.assertNotIn(['pnpm-lock', 'pin'], [call[0:2] for call in self.calls()])

    def test_route_follows_content_not_the_version_number(self):
        """A 1.4.x archive that ships a shrinkwrap takes the import route."""
        shrinkwrap = self.pi_shrinkwrap('1.4.1')
        self.request_shrinkwrapped('1.4.1', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.selection_evidence(result)['route'], 'shrinkwrap')
        self.assertTrue([line for line in self.manager_calls() if ' import ' in ' ' + line + ' '])

    def test_an_explicit_refresh_reaches_the_candidate_comparator(self):
        """`--refresh-lock` is the caller's, and the comparator needs it.

        The comparator's equal-selection rule is byte equality, so an explicit
        developer refresh that rewrites lock bytes for the same version is only
        accepted when the request says it is a refresh. A driver that drops the flag
        turns that legitimate refresh into `equal selection requires identical bytes`.
        """
        self.request_shrinkwrapped('0.87.0', self.pi_shrinkwrap('0.87.0'))
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0',
                                '--refresh-lock')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['mode'], 'refresh')
        self.assertEqual((self.state / 'refresh-graph').read_text(), 'yes')

    def test_an_equal_unrefreshed_request_still_carries_no_refresh(self):
        self.request_shrinkwrapped('0.87.0', self.pi_shrinkwrap('0.87.0'))
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.state / 'refresh-graph').read_text(), 'no')

    def test_a_bridge_refresh_is_seeded_with_the_reviewed_lock(self):
        """A bridge refresh updates the reviewed graph; a changed build does not.

        The plan's bridge refresh row is `new manifest + workspace + old pnpm lock`:
        incremental maintenance must start from the reviewed lock instead of
        resolving the whole graph again. An ordinary changed build keeps the fresh
        resolution, so the seed cannot become an undocumented second route.
        """
        reviewed = (self.bridge_prefix / 'pnpm-lock.yaml').read_bytes()
        result = self.run_driver('--consumer', 'bridge', 'prepare', str(self.bridge_prefix),
                                '0.8.1', '--refresh-lock')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.state / 'seed.log').read_text(), 'seeded\n')
        self.assertEqual((self.state / 'seed.txt').read_bytes(), reviewed)
        self.assertEqual((self.state / 'refresh-graph').read_text(), 'yes')

        # A changed build of the next version is not a refresh: fresh resolution.
        (self.state / 'seed.log').unlink()
        changed = self.run_driver('--consumer', 'bridge', 'prepare', str(self.bridge_prefix),
                                  '0.8.2')
        self.assertEqual(changed.returncode, 0, changed.stderr)
        self.assertEqual((self.state / 'seed.log').read_text(), 'fresh\n')
        self.assertEqual((self.state / 'refresh-graph').read_text(), 'no')

    def test_an_interrupted_preparation_stops_its_children(self):
        """A TERM must not outlive the manager it interrupted.

        Children run in their own session, so the signal this driver receives never
        reaches them and no orphan reaper owns them. An interrupted wait used to
        raise straight out of the child while the scratch it was using was deleted,
        leaving the manager and its grandchild running. Both are named here by the
        fixture, and both must be gone before the driver's exit is accepted.
        """
        (self.state / 'hang-install').write_text('1')
        process = subprocess.Popen(
            [sys.executable, str(DRIVER), '--consumer', 'bridge', 'prepare',
             str(self.bridge_prefix), '0.8.2', '--refresh-lock'],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            pids = self.wait_for_children()
            process.terminate()
            _, errors = process.communicate(timeout=60)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait()
        self.assertEqual(process.returncode, 143, errors)
        self.assert_children_gone(pids)

    def wait_for_children(self):
        child = self.state / 'child.pid'
        grandchild = self.state / 'grandchild.pid'
        deadline = time.time() + 60
        while time.time() < deadline:
            if child.exists() and grandchild.exists():
                pids = [int(child.read_text()), int(grandchild.read_text())]
                if all(pid > 0 for pid in pids):
                    return pids
            time.sleep(0.05)
        self.fail('the hanging manager never started')

    def assert_children_gone(self, pids):
        deadline = time.time() + 15
        alive = list(pids)
        while time.time() < deadline:
            alive = [pid for pid in alive if alive_pid(pid)]
            if not alive:
                return
            time.sleep(0.05)
        self.fail(f'children survived the interruption: {alive}')

    def test_range_route_still_used_without_a_shrinkwrap(self):
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = self.selection_evidence(result)
        self.assertEqual(evidence['route'], 'resolve')
        self.assertFalse(evidence['selectedArchive']['shrinkwrap'])
        self.assertTrue([line for line in self.manager_calls() if '--lockfile-only' in line])
        self.assertFalse([line for line in self.manager_calls() if ' import ' in ' ' + line + ' '])
        self.assertEqual([call[0:2] for call in self.calls()],
                         [['pnpm-lock-candidate', 'graph'], ['pnpm-lock', 'pin'],
                          ['pnpm-lock', 'check'], ['pnpm-lock', 'check']])

    def test_missing_sri_completion_is_limited_to_the_exact_legacy_five(self):
        cases = {
            'four-siblings': self.pi_shrinkwrap(
                '0.87.0', omitted=('chord', 'pi-agent-core', 'pi-ai', 'pi-tui')),
            'non-legacy': self.pi_shrinkwrap('0.87.0', extra={
                'node_modules/ws': {'version': '8.21.0', 'resolved':
                                    'https://registry.npmjs.org/ws/-/ws-8.21.0.tgz'}}),
            'sixth-sibling': self.pi_shrinkwrap('0.87.0', extra={
                'node_modules/@earendil-works/pi-codemode': {'version': '0.87.0', 'resolved':
                    'https://registry.npmjs.org/@earendil-works/pi-codemode/-/pi-codemode-0.87.0.tgz'}}),
        }
        for label, shrinkwrap in cases.items():
            with self.subTest(label):
                shutil.rmtree(self.root / 'state/archives', ignore_errors=True)
                self.ledger = {'metadata': {}, 'tarballs': {}}
                self.make_prefix(self.prefix, '1.4.0') if not self.prefix.is_dir() else None
                self.request_shrinkwrapped('0.87.0', shrinkwrap)
                before = self.image(self.prefix)
                result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
                self.assertEqual(result.returncode, 1, label)
                self.assertIn('only the exact legacy five siblings', result.stderr)
                self.assertEqual([call[0] for call in self.calls() if call[0] == 'node'], [])
                self.assertEqual(self.image(self.prefix), before)

    def test_a_drifted_sibling_version_is_rejected(self):
        shrinkwrap = self.pi_shrinkwrap('0.87.0', sibling_versions={'pi-ai': '0.87.1'})
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 1)
        self.assertIn('is 0.87.1, not the selected 0.87.0', result.stderr)
        self.assertEqual([call[0] for call in self.calls() if call[0] == 'node'], [])
        self.assertNotIn(['pnpm-lock', 'pin'], [call[0:2] for call in self.calls()])

    def test_import_only_lock_is_absent_before_candidate_validation(self):
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        # The fake contract is the validator and refuses any other inventory, so a
        # recorded three-file candidate *is* the proof that the synthesized npm
        # lock, its shrinkwrap and any pin file were gone before validation.
        inventory = json.loads((self.state / 'candidate-inventory.json').read_text())
        self.assertEqual(inventory, ['package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml'])
        # The synthesized lock was really supplied to the importer and to the
        # pre-import expectation, which is what makes its absence meaningful. The
        # importer refuses to run without it and keeps what it was handed, so this
        # document is the synthesizer's own output -- not the archive's shrinkwrap
        # and not a file the archive happened to carry.
        inputs = json.loads((self.state / 'synthesize-inputs.json').read_text())
        self.assertEqual(inputs['root']['version'], '0.87.0')
        self.assertEqual(len(inputs['siblings']), len(LEGACY))
        imported = json.loads((self.state / 'import-input.json').read_text())
        self.assertEqual(imported, {'name': 'pi-selected-import', 'lockfileVersion': 3,
                                    'requires': True, 'packages': {}})
        # The two processes are one order: the converter synthesizes, the manager
        # imports that document, and only then is the candidate compared and the
        # delta validated.
        self.assert_order(['node-synthesize', 'pnpm-import', 'pnpm-lock-candidate',
                           'node-validate', 'pnpm-lock', 'pnpm-lock', 'pnpm-lock'])

    def test_comparison_authority_switches_only_for_the_shrinkwrap_route(self):
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.state / 'authority-graph').read_text(), 'yes')

    def test_ordinary_route_keeps_the_retained_freeze_and_adoption_stays_unreachable(self):
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.state / 'authority-graph').read_text(), 'no')
        proposal = self.root / 'proposal'
        adopted = self.run_driver('--consumer', 'pi', 'adopt', str(proposal))
        self.assertEqual(adopted.returncode, 0, adopted.stderr)
        self.assertEqual((self.state / 'authority-adoption-graph').read_text(), 'no')

    def poisoned_archive(self, label):
        """One archive the extractor refuses, written under its own name."""
        path = self.state / 'archives' / f'poisoned-{label}.tgz'
        if label == 'symlink':
            with tarfile.open(path, 'w:gz') as tar:
                body = b'{"name":"x","version":"1.0.0"}'
                info = tarfile.TarInfo('package/package.json')
                info.size = len(body)
                info.mode = 0o644
                tar.addfile(info, io.BytesIO(body))
                link = tarfile.TarInfo('package/escape')
                link.type = tarfile.SYMTYPE
                link.linkname = '../../../etc/passwd'
                tar.addfile(link)
        else:
            self.write_tarball(path, {'package/package.json': b'{"name":"x","version":"1.0.0"}',
                                      'package/../../escape': b'x'})
        path.parent.mkdir(parents=True, exist_ok=True)
        return path

    def test_poisoned_archive_is_rejected(self):
        # Each refusal names its own reason: a traversal and a link escape share
        # the driver's one "the selected archive was refused" wrapper, so the code
        # is the only part that can tell them apart.
        for label, code in (('traversal', 'unsafe-path'), ('symlink', 'link-escape')):
            with self.subTest(label):
                path = self.poisoned_archive(label)
                self.ledger = {'metadata': {}, 'tarballs': {}}
                self.register(PI_ROOT, '0.87.0', path)
                before = self.image(self.prefix)
                result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
                self.assertEqual(result.returncode, 1, label)
                self.assertIn(f'the selected archive was refused: {code}:', result.stderr)
                self.assertEqual([call[0] for call in self.calls() if call[0] == 'node'], [])
                self.assertEqual(self.image(self.prefix), before)

    def test_integrity_is_proved_before_any_extraction(self):
        """A refused SRI must be the reason, never a refusal the extractor would give.

        The substituted tarball is *also* one the extractor refuses (a traversal
        member). If extraction ran first, the diagnostic would be `unsafe-path`;
        the driver must rehash the download and refuse it before the archive's
        member list is looked at, so the diagnostic names the integrity.
        """
        path = self.poisoned_archive('traversal')
        other = self.state / 'archives' / 'authentic-elsewhere.tgz'
        self.write_tarball(other, {'package/package.json': b'{"name":"x","version":"1.0.0"}'})
        self.ledger = {'metadata': {}, 'tarballs': {}}
        self.register(PI_ROOT, '0.87.0', path,
                      integrity='sha512-' + base64.b64encode(
                          hashlib.sha512(other.read_bytes()).digest()).decode())
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 1)
        self.assertIn('the selected archive was refused: integrity-mismatch:', result.stderr)
        self.assertNotIn('unsafe-path', result.stderr)
        self.assertEqual([call[0] for call in self.calls() if call[0] == 'node'], [])
        self.assertEqual(self.image(self.prefix), before)

    def test_archive_that_does_not_match_its_registry_sri_is_rejected(self):
        path = self.state / 'archives' / 'substituted.tgz'
        self.write_tarball(path, {'package/package.json': b'{"name":"x","version":"1.0.0"}'})
        self.ledger = {'metadata': {}, 'tarballs': {}}
        other = self.state / 'archives' / 'other.tgz'
        self.write_tarball(other, {'package/package.json': b'{"name":"x","version":"9.9.9"}'})
        self.register(PI_ROOT, '0.87.0', path,
                      integrity='sha512-' + base64.b64encode(
                          hashlib.sha512(other.read_bytes()).digest()).decode())
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 1)
        self.assertIn('integrity-mismatch', result.stderr)
        self.assertEqual([call[0] for call in self.calls() if call[0] == 'node'], [])

    def test_refused_shrinkwrap_delta_stops_before_pinning(self):
        (self.state / 'refuse-delta').write_text('1')
        shrinkwrap = self.pi_shrinkwrap('0.87.0')
        self.request_shrinkwrapped('0.87.0', shrinkwrap)
        before = self.image(self.prefix)
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '0.87.0')
        self.assertEqual(result.returncode, 1)
        self.assertIn('the imported graph is not the authenticated shrinkwrap graph: sri-changed',
                      result.stderr)
        self.assertEqual(self.image(self.prefix), before)
        self.assertNotIn(['pnpm-lock', 'pin'], [call[0:2] for call in self.calls()])
        self.assertFalse([line for line in self.manager_calls() if '--frozen-lockfile' in line])

    def test_a_failed_archive_download_has_no_soft_route(self):
        (self.state / 'refuse-download').write_text('1')
        result = self.run_driver('--consumer', 'pi', 'prepare', str(self.prefix), '1.4.1')
        self.assertEqual(result.returncode, 1)
        self.assertIn('preparation has no degraded or range-resolve route', result.stderr)
        self.assertFalse([line for line in self.manager_calls() if '--lockfile-only' in line])
        self.assertFalse([line for line in self.manager_calls() if '--frozen-lockfile' in line])

if __name__ == '__main__':
    unittest.main(verbosity=2)
