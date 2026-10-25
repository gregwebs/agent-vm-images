#!/usr/bin/env python3
"""Black-box tests for the committed repository base identity (#95).

Each case assembles a disposable Git checkout that contains the *real*
`script/build/base-inputs.sh`, `images/verify-base-inputs.sh` and
`examples/base-extension/check-base-identity.sh`, plus a small images/ tree. The
selection, hashing, manifest grammar, publication transaction and both verifiers
are the ones a maintainer and a build run; only the surrounding tree is a fixture
and only the copy fault is simulated (a fake `cp` on PATH, the same technique the
parent-pin suite uses).
"""
from __future__ import annotations

import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
GENERATOR = 'script/build/base-inputs.sh'
VERIFIER = 'images/verify-base-inputs.sh'
EXTENSION_CHECK = 'examples/base-extension/check-base-identity.sh'
MANIFEST = 'images/base-inputs.sha256'
EXTENSION = 'examples/base-extension/Dockerfile'
IDENTITY_ARG = 'AGENT_VM_BASE_IDENTITY'
DRIFT = 'base-inputs: inputs/default drift; run: bash script/build/base-inputs.sh --write'
ROOTS = ['.dockerignore', 'Dockerfile', 'apt-policy', 'install-build-ca.sh', 'install-pnpm.sh',
         'install-zellij.sh', 'pnpm', 'recipe-contract', 'verify-base-inputs.sh']
PLACEHOLDER = 'sha256:' + '0' * 64
EMPTY_BLOB = 'e69de29bb2d1d6434b8b29ae775ad8c2e48c5391'
FAKE_CP = r'''#!/usr/bin/env python3
"""Fail or signal one `cp` chosen by a JSON rule, then run the real cp."""
import json
import os
import signal
import sys
from pathlib import Path

argv = sys.argv[1:]
rules = json.loads(Path(os.environ['FAKE_CP_RULE']).read_text())
if isinstance(rules, dict):
    rules = [rules]
for rule in rules:
    if not any(rule['needle'] in argument for argument in argv):
        continue
    # transactional-publish.sh writes a backup as `cp -p DEST BACKUP/.tmpN`, a
    # publication as `cp STAGED DEST` and a restore as `cp BACKUP/N DEST`, so the
    # two operands name the phase.
    backup = '/.tmp' in argv[-1]
    publication = any('/staged/' in argument for argument in argv[:-1])
    restore = '/backup/' in argv[-2] and '/.tmp' not in argv[-2]
    phase = rule.get('phase')
    if (phase == 'backup' and not backup) or (phase == 'publication' and not publication) \
            or (phase == 'restore' and not restore):
        continue
    if rule.get('once'):
        marker = Path(os.environ['FAKE_CP_RULE'] + '.' + rule['needle'].replace('/', '_'))
        if marker.exists():
            continue
        marker.write_text('1')
    action = rule.get('action', 'fail')
    if action == 'term':
        # The publisher's trap is deferred until this child returns, which is the
        # boundary between the copies the signal is meant to interrupt.
        os.kill(os.getppid(), signal.SIGTERM)
    elif action == 'fail':
        if rule.get('truncate'):
            source, destination = Path(argv[-2]), Path(argv[-1])
            destination.write_bytes(source.read_bytes()[:8])
        print('injected copy failure for ' + rule['needle'], file=sys.stderr)
        raise SystemExit(1)
    else:
        raise SystemExit('unknown copy action: ' + action)
    break
os.execv('/bin/cp', ['/bin/cp', *argv])
'''
FIXTURE_FILES = {
    'images/.dockerignore': '**/*.log\n**/node_modules\n',
    'images/Dockerfile': 'FROM debian:13-slim\nCMD ["/bin/bash"]\n',
    'images/apt-policy/debian.sources': 'Types: deb\nURIs: http://deb.debian.org/debian\n',
    'images/install-build-ca.sh': '#!/bin/sh\nset -eu\n',
    'images/install-pnpm.sh': '#!/bin/sh\nset -eu\n',
    'images/install-zellij.sh': '#!/bin/sh\nset -eu\n',
    'images/pnpm/pin.json': '{"version": "11.11.0"}\n',
    'images/pnpm/verify-pnpm.sh': '#!/bin/sh\nset -eu\n',
    'images/recipe-contract/go.mod': 'module example.invalid/contract\n\ngo 1.27.3\n',
}
EXTENSION_FIXTURE = ('ARG BASE_IMAGE=agent-vm-base:local\n'
                     'ARG ' + IDENTITY_ARG + '=' + PLACEHOLDER + '\n'
                     'FROM ${BASE_IMAGE}\n'
                     'ARG ' + IDENTITY_ARG + '\n'
                     'RUN --mount=type=bind,source=check-base-identity.sh,'
                     'target=/tmp/check-base-identity.sh,ro \\\n'
                     '    sh /tmp/check-base-identity.sh "$' + IDENTITY_ARG + '"\n'
                     'CMD ["true"]\n')


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def is_hex(value: str) -> bool:
    return len(value) == 64 and all(character in '0123456789abcdef' for character in value)


class Repo:
    """A disposable checkout with the real generator and both verifiers."""

    def __init__(self, root: Path, name: str):
        self.root = root
        self.repo = root / name
        self.bin = root / (name + '-bin')
        self.bin.mkdir(parents=True)
        self.repo.mkdir(parents=True)
        for relative in (GENERATOR, 'script/build/transactional-publish.sh', VERIFIER,
                         EXTENSION_CHECK):
            destination = self.repo / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / relative, destination)
        for relative, text in FIXTURE_FILES.items():
            destination = self.repo / relative
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_text(text)
        (self.repo / EXTENSION).write_text(EXTENSION_FIXTURE)
        (self.repo / '.gitignore').write_text('images/pnpm/*.tmp\n')
        self.git('init', '-q', '-b', 'main')
        self.git('config', 'user.email', 'fixture@example.invalid')
        self.git('config', 'user.name', 'Fixture')
        self.git('config', 'commit.gpgsign', 'false')
        self.git('config', 'core.fsmonitor', 'false')
        self.git('add', '-A')
        self.git('commit', '-qm', 'baseline')
        fake = self.bin / 'cp'
        fake.write_text('#!' + sys.executable + '\n' + FAKE_CP.split('\n', 1)[1])
        fake.chmod(0o755)
        self.rule = self.root / (name + '-cp-rule.json')

    # --- running -----------------------------------------------------------

    def git(self, *arguments):
        return subprocess.run(['git', '-C', str(self.repo), *arguments],
                              capture_output=True, text=True, check=False)

    def run(self, path: Path, shell: str, *arguments, extra_env=None):
        env = {key: value for key, value in os.environ.items()
               if key not in ('GIT_INDEX_FILE', 'FAKE_CP_RULE', 'AGENT_VM_BASE_STAMP')}
        env.update(TMPDIR=str(self.root), GIT_CEILING_DIRECTORIES=str(self.root))
        env.update(extra_env or {})
        if self.rule.exists():
            env['FAKE_CP_RULE'] = str(self.rule)
            env['PATH'] = str(self.bin) + os.pathsep + env['PATH']
        result = subprocess.run([shell, str(path), *arguments], cwd=str(self.repo), env=env,
                                capture_output=True, text=True, check=False, timeout=300)
        return result.returncode, result.stdout + result.stderr

    def generator(self, *arguments, extra_env=None):
        return self.run(self.repo / GENERATOR, '/bin/bash', *arguments, extra_env=extra_env)

    def verify(self, *arguments, extra_env=None):
        return self.run(self.repo / VERIFIER, '/bin/sh', *arguments, extra_env=extra_env)

    def extension_check(self, *arguments, extra_env=None):
        return self.run(self.repo / EXTENSION_CHECK, '/bin/sh', *arguments, extra_env=extra_env)

    def write(self):
        status, output = self.generator('--write')
        assert status == 0, output
        return output

    def check(self):
        return self.generator('--check')

    # --- observations ------------------------------------------------------

    def manifest(self) -> str:
        return (self.repo / MANIFEST).read_text()

    def entries(self):
        return [line.split('  ', 1) for line in self.manifest().splitlines()]

    def identity(self) -> str:
        return 'sha256:' + sha256((self.repo / MANIFEST).read_bytes())

    def default(self) -> str:
        for line in (self.repo / EXTENSION).read_text().splitlines():
            if line.startswith('ARG ' + IDENTITY_ARG + '='):
                return line.split('=', 1)[1]
        raise AssertionError('the fixture extension lost its default')

    def snapshot(self):
        return {path.relative_to(self.repo).as_posix(): path.read_bytes()
                for path in self.repo.rglob('*')
                if path.is_file() and not path.is_symlink() and '.git' not in path.parts}

    def add(self, relative: str, text: str):
        path = self.repo / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text)
        self.git('add', relative)

    def fault(self, rules):
        self.rule.write_text(json.dumps(rules))

    def clear_fault(self):
        for path in list(self.root.glob(self.rule.name + '*')):
            path.unlink()

    def retained_work(self, output: str) -> Path:
        phrase = 'recovery material retained at '
        lines = [line for line in output.splitlines() if phrase in line]
        assert lines, 'no retention was reported:\n' + output
        return Path(lines[0].split(phrase, 1)[1].strip())


def report(label: str, description: str):
    print('base-inputs: %s %s PASS' % (label, description))


def refusal(status: int, output: str, fragment: str):
    assert status == 1, 'expected status 1, got %d:\n%s' % (status, output)
    assert fragment in output, 'expected %r in:\n%s' % (fragment, output)


# --- cases ------------------------------------------------------------------

def b1(root):
    """The generator's selection, format and identity are the committed contract."""
    case = Repo(root, 'b1')
    status, output = case.generator('inputs')
    assert status == 0, output
    assert output.split() == ROOTS, output
    output = case.write()
    assert 'published' in output, output
    text = case.manifest()
    assert text.endswith('\n') and '\r' not in text, text
    lines = text.splitlines()
    paths = [line.split('  ', 1)[1] for line in lines]
    assert paths == sorted(paths), 'the manifest is not byte-ordered by path'
    assert len(paths) == len(set(paths)), 'a path is listed twice'
    for line in lines:
        entry, path = line.split('  ', 1)
        assert is_hex(entry), line
        assert path != 'base-inputs.sha256', 'the manifest lists itself'
        assert sha256((case.repo / 'images' / path).read_bytes()) == entry, line
    for name in ROOTS:
        assert any(name == path or path.startswith(name + '/') for path in paths), name
    assert case.identity() == case.default(), 'the extension default is not the identity'
    assert case.identity() in output, 'the published identity is not reported'
    assert case.check()[0] == 0, case.check()[1]
    before = case.snapshot()
    output = case.write()
    assert 'unchanged' in output, output
    assert case.snapshot() == before, 'an unchanged write touched the tree'
    report('B1', 'generates the ordered manifest and the extension default')


def b2(root):
    """Content edits, additions, deletions and renames are drift until repaired."""
    case = Repo(root, 'b2')
    case.write()
    original = case.manifest()

    (case.repo / 'images/install-pnpm.sh').write_text('#!/bin/sh\nset -eu\n# edited\n')
    status, output = case.check()
    refusal(status, output, DRIFT)
    assert 'changed: install-pnpm.sh' in output, output
    assert case.manifest() == original, 'a check must not rewrite the manifest'
    case.write()
    assert case.manifest() != original
    assert case.check()[0] == 0, case.check()[1]
    repaired = case.manifest()

    case.add('images/pnpm/extra.json', '{"extra": true}\n')
    status, output = case.check()
    refusal(status, output, DRIFT)
    assert 'new: pnpm/extra.json' in output, output
    case.write()
    assert case.manifest() != repaired
    assert '  pnpm/extra.json' in case.manifest()

    case.git('rm', '-q', '-f', 'images/pnpm/extra.json')
    status, output = case.check()
    refusal(status, output, DRIFT)
    assert 'missing: pnpm/extra.json' in output, output
    case.write()
    assert '  pnpm/extra.json' not in case.manifest()

    case.git('mv', 'images/pnpm/pin.json', 'images/pnpm/pinned.json')
    status, output = case.check()
    refusal(status, output, DRIFT)
    assert 'new: pnpm/pinned.json' in output, output
    assert 'missing: pnpm/pin.json' in output, output
    case.write()
    assert '  pnpm/pinned.json' in case.manifest()
    assert '  pnpm/pin.json' not in case.manifest()
    assert case.check()[0] == 0, case.check()[1]

    # An edit of the committed manifest is drift too, even when every hash is
    # still right: the format is part of what a build re-verifies.
    entries = case.entries()
    (case.repo / MANIFEST).write_text(''.join(
        '%s  %s\n' % (entry, path) for entry, path in reversed(entries)))
    status, output = case.check()
    refusal(status, output, DRIFT)
    case.write()
    assert case.manifest() == ''.join(
        '%s  %s\n' % (entry, path) for entry, path in entries)
    assert case.check()[0] == 0, case.check()[1]
    report('B2', 'reports edited, added, deleted and renamed inputs and repairs them')


def b3(root):
    """Mode flips are not identity; the exclusion is deliberate and stable."""
    case = Repo(root, 'b3')
    case.write()
    identity = case.identity()
    (case.repo / 'images/install-pnpm.sh').chmod(0o755)
    assert case.check()[0] == 0, 'a physical mode flip changed the identity'
    case.git('add', 'images/install-pnpm.sh')
    case.git('update-index', '--chmod=+x', 'images/install-pnpm.sh')
    assert case.check()[0] == 0, 'an index mode flip changed the identity'
    case.git('config', 'core.fileMode', 'false')
    (case.repo / 'images/install-zellij.sh').chmod(0o755)
    assert case.check()[0] == 0, 'core.fileMode=false changed the identity'
    assert case.identity() == identity
    modes = case.git('ls-files', '-s', 'images/install-pnpm.sh').stdout
    assert modes.startswith('100755'), modes
    report('B3', 'keeps physical and index mode flips out of the identity')


def b4(root):
    """Only tracked regular files under the roots are selectable."""
    case = Repo(root, 'b4')
    case.write()
    original = case.manifest()

    (case.repo / 'images/pnpm/untracked.json').write_text('{}\n')
    (case.repo / 'images/pnpm/ignored.tmp').write_text('scratch\n')
    assert case.check()[0] == 0, 'an untracked file reached the identity'
    case.write()
    assert case.manifest() == original
    (case.repo / 'images/pnpm/untracked.json').unlink()
    (case.repo / 'images/pnpm/ignored.tmp').unlink()

    link = case.repo / 'images/pnpm/link.json'
    link.symlink_to('pin.json')
    case.git('add', 'images/pnpm/link.json')
    status, output = case.generator('--write')
    refusal(status, output, 'tracked base input is not a regular file')
    case.git('rm', '-q', '--cached', 'images/pnpm/link.json')
    link.unlink()

    case.git('update-index', '--add', '--cacheinfo',
             '160000,%s,images/pnpm/submodule' % EMPTY_BLOB)
    status, output = case.generator('--write')
    refusal(status, output, 'tracked base input is not a regular file')
    case.git('rm', '-q', '--cached', 'images/pnpm/submodule')

    case.add('images/pnpm/bad name.json', '{}\n')
    status, output = case.generator('--write')
    refusal(status, output, 'unsupported character in base input path')
    case.git('rm', '-q', '-f', '--cached', 'images/pnpm/bad name.json')
    (case.repo / 'images/pnpm/bad name.json').unlink()

    elsewhere = case.root / 'elsewhere'
    elsewhere.mkdir()
    (elsewhere / 'pin.json').write_text('{"outside": true}\n')
    shutil.move(str(case.repo / 'images/pnpm'), str(case.root / 'moved-pnpm'))
    (case.repo / 'images/pnpm').symlink_to(elsewhere)
    status, output = case.generator('--write')
    refusal(status, output, 'symlinked base input component: pnpm')
    (case.repo / 'images/pnpm').unlink()
    shutil.move(str(case.root / 'moved-pnpm'), str(case.repo / 'images/pnpm'))
    assert case.check()[0] == 0, case.check()[1]
    assert case.manifest() == original
    report('B4', 'excludes untracked files and refuses symlinks, gitlinks and bad paths')


def b5(root):
    """The in-build verifier refuses anything but the committed format."""
    case = Repo(root, 'b5')
    case.write()
    stamp = case.root / 'stamp'
    never = case.root / 'never'
    status, output = case.verify('images', 'stamp', str(stamp))
    assert status == 0, output
    assert stamp.read_text() == case.identity() + '\n', stamp.read_text()
    status, output = case.verify('images', 'verify', str(stamp))
    assert status == 0, output
    assert case.identity() in output, output

    good = case.manifest()
    entries = case.entries()
    first, second = entries[0], entries[1]
    mutations = {
        'missing separator': good.replace('  ' + first[1], ' ' + first[1], 1),
        'uppercase hash': good.replace(first[0], first[0].upper(), 1),
        'short hash': good.replace(first[0], first[0][:60], 1),
        'out-of-order': good.replace(
            '%s  %s\n%s  %s\n' % (first[0], first[1], second[0], second[1]),
            '%s  %s\n%s  %s\n' % (second[0], second[1], first[0], first[1]), 1),
        'duplicate': good + '%s  %s\n' % (first[0], first[1]),
        'traversal': good.replace('  ' + first[1], '  ../' + first[1], 1),
        'absolute': good.replace('  ' + first[1], '  /' + first[1], 1),
        'self entry': good.replace('  ' + first[1], '  base-inputs.sha256', 1),
        'empty': '',
    }
    for label, text in mutations.items():
        assert text != good, label
        (case.repo / MANIFEST).write_text(text)
        status, output = case.verify('images', 'verify', str(stamp))
        assert status == 1, 'accepted a malformed manifest: %s\n%s' % (label, output)
        assert 'base-inputs:' in output, output
    (case.repo / MANIFEST).write_text(good)

    # A listed file that changed or disappeared fails before anything is stamped.
    (case.repo / 'images/pnpm/pin.json').write_text('{"version": "12.0.0"}\n')
    status, output = case.verify('images', 'stamp', str(never))
    refusal(status, output, DRIFT)
    assert not never.exists(), 'a failed check stamped the image'
    (case.repo / 'images/pnpm/pin.json').unlink()
    status, output = case.verify('images', 'stamp', str(never))
    refusal(status, output, 'missing or non-regular manifest entry: pnpm/pin.json')
    real = case.repo / 'images/pnpm/pin.json'
    real.symlink_to(case.repo / 'images/pnpm/verify-pnpm.sh')
    status, output = case.verify('images', 'stamp', str(never))
    refusal(status, output, 'symlinked manifest entry: pnpm/pin.json')
    real.unlink()
    real.write_text('{"version": "11.11.0"}\n')
    assert case.verify('images', 'verify', str(stamp))[0] == 0
    assert not never.exists()

    # A manifest whose last line carries no newline must still have that entry
    # checked. `sha256sum -c` follows a symlink, so a per-entry check that a
    # shell `read` skipped would leave the entry unexamined but the stamp clean.
    verifier = case.repo / 'images/verify-base-inputs.sh'
    lookalike = case.repo / 'images/verify-base-inputs.sh.copy'
    lookalike.write_bytes(verifier.read_bytes())
    verifier.unlink()
    verifier.symlink_to(lookalike.name)
    (case.repo / MANIFEST).write_text(good.rstrip('\n'))
    status, output = case.verify('images', 'stamp', str(never))
    refusal(status, output, 'symlinked manifest entry: verify-base-inputs.sh')
    assert not never.exists(), 'an unchecked final entry was stamped'
    verifier.unlink()
    lookalike.rename(verifier)
    (case.repo / MANIFEST).write_text(good)
    assert case.verify('images', 'verify', str(stamp))[0] == 0
    report('B5', 'verifies the manifest grammar, content and stamp inside the image')


def b6(root):
    """Missing, malformed and mismatched stamps are distinguishable refusals."""
    case = Repo(root, 'b6')
    case.write()
    identity = case.identity()
    status, output = case.verify('images', 'verify', str(case.root / 'absent'))
    refusal(status, output,
            'base-inputs: BASE_IMAGE has no repository base identity; '
            'rebuild the base from this checkout')
    wrong = case.root / 'wrong'
    wrong.write_text('sha256:' + '1' * 64 + '\n')
    status, output = case.verify('images', 'verify', str(wrong))
    refusal(status, output, 'base-inputs: BASE_IMAGE declares sha256:' + '1' * 64)
    assert 'this checkout expects ' + identity in output, output
    assert 'rebuild the base' in output, output
    for label, text in {'garbage': 'not-an-identity\n',
                        'two lines': identity + '\n' + identity + '\n',
                        'no newline': identity,
                        'short': 'sha256:' + 'a' * 63 + '\n',
                        'uppercase': 'sha256:' + 'A' * 64 + '\n'}.items():
        path = case.root / ('bad-' + label.replace(' ', '-'))
        path.write_text(text)
        status, output = case.verify('images', 'verify', str(path))
        assert status == 1, (label, output)
        assert 'declares a malformed repository base identity' in output, (label, output)

    # The extension check uses the same stamp contract and names the override.
    status, output = case.extension_check(identity, extra_env={'AGENT_VM_BASE_STAMP': str(wrong)})
    assert status == 1, output
    assert 'use --build-arg ' + IDENTITY_ARG + '=sha256:' + '1' * 64 in output, output
    status, output = case.extension_check(
        identity, extra_env={'AGENT_VM_BASE_STAMP': str(case.root / 'absent')})
    refusal(status, output, 'BASE_IMAGE has no repository base identity')
    status, output = case.extension_check('agent-vm-base:local')
    assert status == 1 and IDENTITY_ARG + ' must be sha256:' in output, output
    stamp = case.root / 'extension-stamp'
    case.verify('images', 'stamp', str(stamp))
    assert case.extension_check(identity, extra_env={'AGENT_VM_BASE_STAMP': str(stamp)})[0] == 0
    report('B6', 'refuses a missing, malformed or mismatched parent identity')


def b7(root):
    """Publication is one transaction over the manifest and the extension."""
    case = Repo(root, 'b7')
    case.write()
    (case.repo / 'images/install-pnpm.sh').write_text('#!/bin/sh\nset -eu\n# edited\n')
    before = case.snapshot()

    # A failed publication copy restores both destinations, including the default
    # that was already written.
    case.fault({'needle': 'staged/base-inputs.sha256', 'phase': 'publication',
                'action': 'fail', 'truncate': True})
    status, output = case.generator('--write')
    assert status == 1, output
    assert 'restoring attempted files inclusively' in output, output
    assert case.snapshot() == before, 'a failed publication was not restored'
    case.clear_fault()

    # A failed publication *and* a failed restore keep the material and say where.
    case.fault([
        {'needle': 'base-inputs.sha256', 'phase': 'publication', 'action': 'fail',
         'truncate': True},
        {'needle': 'base-inputs.sha256', 'phase': 'restore', 'action': 'fail'},
    ])
    status, output = case.generator('--write')
    assert status == 3, output
    assert 'rollback failed' in output, output
    work = case.retained_work(output)
    assert work.is_dir(), work
    assert 'STOP: recover ' + str(work) in output, output
    backup = work / 'backup'
    assert (backup / '0').exists() and (backup / '1').exists(), sorted(backup.iterdir())
    shutil.copyfile(backup / '0', case.repo / MANIFEST)
    shutil.copyfile(backup / '1', case.repo / EXTENSION)
    assert case.snapshot() == before, 'manual recovery did not restore the destination'
    case.clear_fault()

    # A signal during publication restores and reports 128+N.
    case.fault({'needle': 'staged/extension.Dockerfile', 'phase': 'publication',
                'action': 'term', 'once': True})
    status, output = case.generator('--write')
    assert status == 143, output
    assert 'publication interrupted; sources restored' in output, output
    assert case.snapshot() == before, 'the interrupted publication was not restored'
    case.clear_fault()

    # The retry publishes both together and is then idempotent.
    status, output = case.generator('--write')
    assert status == 0 and 'published' in output, output
    assert case.snapshot() != before
    assert case.check()[0] == 0, case.check()[1]
    report('B7', 'publishes and restores both destinations as one transaction')


def b8(root):
    """--stage renders from an overlay tree and never publishes."""
    case = Repo(root, 'b8')
    case.write()
    before = case.snapshot()
    tree = case.root / 'overlay'
    shutil.copytree(case.repo / 'images', tree)
    (tree / 'install-pnpm.sh').write_text('#!/bin/sh\nset -eu\n# overlay edition\n')
    (tree / 'pnpm/overlay.json').write_text('{"overlay": true}\n')
    out = case.root / 'rendered'
    status, output = case.generator('--stage', str(tree), str(out))
    assert status == 0, output
    assert (out / 'base-inputs.sha256').exists() and (out / 'extension.Dockerfile').exists()
    staged = (out / 'base-inputs.sha256').read_text()
    assert sha256((tree / 'install-pnpm.sh').read_bytes()) in staged
    assert '  pnpm/overlay.json' not in staged, 'an untracked overlay file entered the manifest'
    staged_identity = 'sha256:' + sha256((out / 'base-inputs.sha256').read_bytes())
    assert ('ARG ' + IDENTITY_ARG + '=' + staged_identity) in \
        (out / 'extension.Dockerfile').read_text()
    assert case.snapshot() == before, '--stage published something'
    status, output = case.generator('--stage', str(tree), str(out))
    refusal(status, output, 'refusing an existing output directory')
    (tree / 'pnpm/pin.json').unlink()
    status, output = case.generator('--stage', str(tree), str(case.root / 'rendered2'))
    refusal(status, output, 'missing or non-regular base input: pnpm/pin.json')
    report('B8', 'stages a manifest from an overlay tree without publishing')


def main() -> int:
    root = Path(tempfile.mkdtemp(prefix='base-inputs-tests.'))
    try:
        for case in (b1, b2, b3, b4, b5, b6, b7, b8):
            case(root)
    finally:
        shutil.rmtree(root, ignore_errors=True)
    print('base-inputs: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
