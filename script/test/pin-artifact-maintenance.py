#!/usr/bin/env python3
"""Fault-injection evidence for the pin maintenance tool (#96).

`script/dev/pin-artifact.py` is the only writer of a pin, and the guarantees it
claims are transactional ones: a second architecture that fails to resolve
changes nothing, a failed publication restores every destination it may have
written (including the one being written), an interrupted publication restores
or reports incomplete recovery with a retained work directory, a symlinked
destination is refused, CI is refused and an inexact version is refused.

None of that is visible from a successful run, so every case here drives the real
module against a throwaway repository root and a real fault: a `fetch` that fails
for one architecture, a read-only destination directory, an injected write
failure, a signal delivered during publication. Nothing reaches the network.

usage: python3 script/test/pin-artifact-maintenance.py
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
TOOL = ROOT / 'script' / 'dev' / 'pin-artifact.py'
ARTIFACTS = ROOT / 'images' / 'artifacts'

failures: list[str] = []

DRIVER = '''#!/usr/bin/env python3
"""One fault, one run of the real maintenance module."""
import importlib.util
import json
import os
import shutil
import signal
import sys
from pathlib import Path

repo = Path(sys.argv[1])
root = Path(sys.argv[2])
case = sys.argv[3]

spec = importlib.util.spec_from_file_location('pin_artifact', repo / 'script/dev/pin-artifact.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.ROOT = root
module.ARTIFACTS = root / 'images/artifacts'
module.DOCKERFILE = root / 'images/Dockerfile'

staged = {
    'images/artifacts/node.json': b'{"node":"staged"}\\n',
    'images/Dockerfile': b'# staged dockerfile\\n',
    'images/artifacts/gh.json': b'{"gh":"staged"}\\n',
}

real_copyfile = shutil.copyfile


def fake_signed(version, key, work):
    """A signed manifest over the fixture bytes, so `resolve` has something to
    compare against without a network or a real key."""
    lines = []
    for platform in module.artifact_pin.PLATFORMS:
        spec = module.URLS['node'](version, platform)
        raw = module.fetch(spec['url'])
        lines.append(f"{module.artifact_pin.sha256(raw)}  {spec['url'].rsplit('/', 1)[-1]}")
    (work / 'SHASUMS256.txt').write_text('\\n'.join(lines) + '\\n')
    return {'manifest': b'', 'signature': b'', 'manifest_digest': '0' * 64,
            'signature_digest': '0' * 64, 'key_digest': '0' * 64,
            'fingerprint': module.artifact_pin.NODE_KEY_FINGERPRINT}


def write_then_fail(name, kill=None):
    """Fail the publication of one destination, once: the backup copies and the
    restore it triggers must still work."""
    armed = [True]

    def copy(src, dst, **kwargs):
        if armed[0] and Path(dst).name == name and 'staged' in Path(src).parts:
            armed[0] = False
            Path(dst).write_text('half-written\\n')
            if kill is not None:
                os.kill(os.getpid(), kill)
            raise OSError('injected write failure')
        return real_copyfile(src, dst, **kwargs)
    return copy


if case == 'publication-partial-write':
    shutil.copyfile = write_then_fail('gh.json')
elif case == 'publication-signal':
    shutil.copyfile = write_then_fail('Dockerfile', kill=signal.SIGINT)
elif case == 'publication-incomplete-recovery':
    shutil.copyfile = write_then_fail('gh.json')

    def refuse_restore(self):
        raise OSError('injected restore failure')
    module.Publisher.restore = refuse_restore
elif case == 'second-architecture':
    real_fetch = module.fetch

    def failing_fetch(url):
        if 'arm64' in url:
            raise module.Refusal(f'cannot fetch {url}: injected failure')
        return b'fixture bytes for ' + url.encode()
    module.fetch = failing_fetch
    module.verify_signed_manifest = fake_signed
    module.bundled_npm = lambda archive, platform: '10.9.9'
elif case == 'refresh-publishes':
    module.fetch = lambda url: b'fixture bytes for ' + url.encode()
    module.verify_signed_manifest = fake_signed
    module.bundled_npm = lambda archive, platform: '10.9.9'
elif case == 'ci':
    module.fetch = lambda url: b'fixture bytes'
elif case == 'inexact-version':
    module.fetch = lambda url: b'fixture bytes'
elif case == 'check-mutated':
    pass
elif case == 'new-destination-rollback':
    created = root / 'images/artifacts/created.json'
    if created.exists():
        created.unlink()
    publisher = module.Publisher()
    publisher.destinations = [created]
    publisher.back_up()
    created.parent.mkdir(parents=True, exist_ok=True)
    created.write_text('published' + chr(10))
    publisher.touched = 1
    publisher.restore()
    print('removed' if not created.exists() else 'kept')
    sys.exit(0 if not created.exists() else 1)
elif case == 'gpg-isolation':
    fixture_dir = root / 'fixture'
    module.fetch = lambda url: (fixture_dir / url.rsplit('/', 1)[-1]).read_bytes()
    work = root / 'gnupg-work'
    work.mkdir(exist_ok=True)
    signed = module.verify_signed_manifest('9.9.9', fixture_dir / 'node-release-key.asc', work)
    print(json.dumps({'fingerprint': signed['fingerprint']}))
    sys.exit(0)
elif case in ('check-committed', 'partial-backup-restore'):
    pass
else:
    raise SystemExit(f'unknown case {case}')

if case == 'partial-backup-restore':
    publisher = module.Publisher()
    publisher.destinations = [root / 'images/artifacts/node.json',
                              root / 'images/Dockerfile']
    publisher.back_up()
    (publisher.backup / '.complete').unlink()
    publisher.touched = 1
    try:
        publisher.restore()
    except OSError as error:
        print(f'restore refused: {error}')
        sys.exit(0)
    print('restore did not refuse an incomplete backup')
    sys.exit(1)

if case.startswith('check-'):
    sys.exit(module.check())
if case in ('ci', 'inexact-version'):
    sys.exit(module.refresh('node', '9.9' if case == 'inexact-version' else '9.9.9',
                            root / 'images/artifacts/node-release-key.asc'))
if case in ('second-architecture', 'refresh-publishes'):
    sys.exit(module.refresh('node', '9.9.9',
                            root / 'images/artifacts/node-release-key.asc'))
sys.exit(module.publish(dict(staged)))
'''


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: str) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


class Repo:
    """A throwaway repository root: the committed pins, labels and their bytes."""

    def __init__(self, work: Path) -> None:
        self.root = work / 'repo'
        shutil.rmtree(self.root, ignore_errors=True)
        self.images = self.root / 'images'
        self.artifacts = self.images / 'artifacts'
        self.artifacts.mkdir(parents=True)
        for path in ARTIFACTS.iterdir():
            if path.is_file():
                shutil.copyfile(path, self.artifacts / path.name)
        shutil.copyfile(ROOT / 'images' / 'Dockerfile', self.images / 'Dockerfile')
        self.original = {path.relative_to(self.root): path.read_bytes()
                         for path in self.destinations()}

    def destinations(self) -> list[Path]:
        return [self.artifacts / 'node.json', self.images / 'Dockerfile',
                self.artifacts / 'gh.json']

    def unchanged(self) -> list[str]:
        return [str(relative) for relative, raw in self.original.items()
                if (self.root / relative).read_bytes() != raw]

    def bytes_of(self, path: Path) -> bytes:
        return self.original[path.relative_to(self.root)]

    def driver(self, work: Path, case: str, environment: dict | None = None,
               argv: list[str] | None = None) -> subprocess.CompletedProcess:
        script = work / 'driver.py'
        script.write_text(DRIVER)
        env = dict(os.environ)
        env.pop('GITHUB_ACTIONS', None)
        env.update(environment or {})
        return subprocess.run([sys.executable, str(script), str(ROOT), str(self.root),
                               case, *(argv or [])],
                              capture_output=True, text=True, check=False, env=env)


def case_second_architecture(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'second-architecture')
    if result.returncode != 1:
        fail('a failed second architecture exits 1', f'{result.returncode}: {result.stderr}')
        return
    if 'injected failure' not in result.stderr:
        fail('a failed second architecture reports the failure', result.stderr.strip())
        return
    changed = repo.unchanged()
    if changed:
        fail('a failed second architecture leaves the committed bytes identical', str(changed))
        return
    report('a second-architecture failure publishes nothing')


def case_publication_failures(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'publication-partial-write')
    if result.returncode != 1:
        fail('a failed publication exits 1', f'{result.returncode}: {result.stderr}')
        return
    # Inclusive rollback: the destination being written and the one already
    # written are both restored from the backup.
    changed = repo.unchanged()
    if changed:
        fail('a failed publication restores every touched destination', str(changed))
        return
    report('a failed publication restores the written and the half-written destination')


def case_signal(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'publication-signal')
    if result.returncode != 128 + 2:
        fail('a SIGINT during publication exits 130', f'{result.returncode}: {result.stderr}')
        return
    if 'sources restored' not in result.stderr:
        fail('a SIGINT during publication reports the restore', result.stderr.strip())
        return
    changed = repo.unchanged()
    if changed:
        fail('a SIGINT during publication restores the sources', str(changed))
        return
    report('a SIGINT during publication restores the sources and exits 130')


def case_incomplete_recovery(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'publication-incomplete-recovery')
    if result.returncode != 3:
        fail('an incomplete recovery exits 3', f'{result.returncode}: {result.stderr}')
        return
    if 'STOP: recover' not in result.stderr:
        fail('an incomplete recovery says where the material is', result.stderr.strip())
        return
    retained = [path for path in Path(tempfile.gettempdir()).glob('pin-artifact.*')
                if (path / 'recovery.json').is_file()]
    if not retained:
        fail('an incomplete recovery retains recovery.json', 'no work directory with one')
        return
    record = json.loads((retained[-1] / 'recovery.json').read_text())
    if record['phase'] != 'publication':
        fail('the recovery record names the phase', record['phase'])
        return
    for path in retained:
        shutil.rmtree(path, ignore_errors=True)
    report('an incomplete recovery exits 3 and retains the work directory')


def case_symlinked_destination(work: Path, repo: Repo) -> None:
    elsewhere = work / 'elsewhere'
    elsewhere.mkdir()
    real = repo.images / 'artifacts' / 'gh.json'
    moved = elsewhere / 'gh.json'
    shutil.move(str(real), str(moved))
    real.symlink_to(moved)
    result = repo.driver(work, 'publication-partial-write')
    if result.returncode != 1 or 'symlink' not in result.stderr:
        fail('a symlinked destination is refused', f'{result.returncode}: {result.stderr}')
        return
    if moved.read_bytes() != repo.bytes_of(real):
        fail('a symlinked destination is not written through', moved.read_text())
        return
    real.unlink()
    shutil.move(str(moved), str(real))

    parent = repo.images / 'artifacts'
    target = work / 'artifacts-target'
    shutil.move(str(parent), str(target))
    parent.symlink_to(target)
    result = repo.driver(work, 'publication-partial-write')
    if result.returncode != 1 or 'symlink' not in result.stderr:
        fail('a symlinked destination parent is refused', f'{result.returncode}: {result.stderr}')
        return
    if 'half-written' in (target / 'gh.json').read_text():
        fail('a symlinked parent is not written through', 'the staged bytes landed')
        return
    parent.unlink()
    shutil.move(str(target), str(parent))
    report('a symlinked destination and a symlinked destination parent are refused')


def case_ci_and_version(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'ci', environment={'GITHUB_ACTIONS': 'true'})
    if result.returncode != 1 or 'refusing GitHub Actions' not in result.stderr:
        fail('GITHUB_ACTIONS=true is refused', f'{result.returncode}: {result.stderr}')
        return
    if repo.unchanged():
        fail('a refused CI run writes nothing', str(repo.unchanged()))
        return
    result = repo.driver(work, 'inexact-version')
    if result.returncode != 2 or 'X.Y.Z' not in result.stderr:
        fail('an inexact version is a usage error', f'{result.returncode}: {result.stderr}')
        return
    report('CI and a non-X.Y.Z version are refused before anything is staged')


def case_check(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'check-committed')
    if result.returncode != 0:
        fail('--check accepts the committed pins', result.stderr.strip())
        return
    committed = subprocess.run([sys.executable, str(TOOL), '--check'], capture_output=True,
                               text=True, check=False)
    if committed.returncode != 0:
        fail('--check accepts the repository pins', committed.stderr.strip())
        return
    pin = repo.artifacts / 'node.json'
    value = json.loads(pin.read_text())
    value['platforms']['linux/arm64']['sha256'] = '0' * 64
    value['install']['payload'] = ['../escape']
    pin.write_text(json.dumps(value, indent=2) + '\n')
    result = repo.driver(work, 'check-mutated')
    if result.returncode != 1 or 'FAIL' not in result.stderr:
        fail('--check refuses a mutated pin', f'{result.returncode}: {result.stderr}')
        return
    pin.write_bytes(repo.bytes_of(pin))
    report('--check accepts the committed pins and refuses a mutated one')


def case_gpg_isolation(work: Path, repo: Repo) -> None:
    """Pin-time verification must use the keyring the tool constructs.

    The tool builds a private `GNUPGHOME`; if it is never passed to `gpg`, the
    host's ambient keyring takes the import and decides the check (#96 review).
    The ambient home here is a decoy: after a successful verification it must hold
    nothing.
    """
    signer = work / 'isolation-signer'
    signer.mkdir(mode=0o700, exist_ok=True)
    environment = dict(os.environ, GNUPGHOME=str(signer), LC_ALL='C')
    generated = subprocess.run(
        ['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
         '--quick-generate-key', 'isolation <isolation@example.invalid>', 'ed25519', 'sign', '0'],
        capture_output=True, text=True, check=False, env=environment)
    if generated.returncode != 0:
        fail('the isolation fixture key is generated', generated.stderr.strip())
        return
    listed = subprocess.run(['gpg', '--batch', '--with-colons', '--list-keys'],
                            capture_output=True, text=True, check=False, env=environment).stdout
    fingerprint = next(line.split(':')[9] for line in listed.splitlines()
                       if line.startswith('fpr:'))
    fixture = repo.root / 'fixture'
    fixture.mkdir()
    key = fixture / 'node-release-key.asc'
    key.write_text(subprocess.run(['gpg', '--batch', '--armor', '--export', fingerprint],
                                  capture_output=True, text=True, check=False,
                                  env=environment).stdout)
    manifest = fixture / 'SHASUMS256.txt'
    manifest.write_text('0' * 64 + '  node-v9.9.9-linux-x64.tar.xz\n')
    subprocess.run(['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase',
                    '', '--armor', '--yes', '--detach-sign', '--output',
                    str(manifest) + '.sig', str(manifest)],
                   capture_output=True, text=True, check=False, env=environment)
    ambient = work / 'ambient-gnupg'
    ambient.mkdir(mode=0o700, exist_ok=True)
    result = repo.driver(work, 'gpg-isolation', environment={'GNUPGHOME': str(ambient)})
    if result.returncode != 0:
        fail('the isolated verification succeeds', f'{result.returncode}: {result.stderr}')
        return
    if fingerprint not in result.stdout:
        fail('the isolated verification reports the pinned fingerprint', result.stdout)
        return
    if any(ambient.iterdir()):
        fail('the ambient keyring is untouched',
             str([path.name for path in ambient.iterdir()]))
        return
    report('pin-time verification uses its own keyring and leaves the ambient one alone')


def case_backup_failure(work: Path, repo: Repo) -> None:
    """A failure in the backup phase happens before publication, so nothing may be
    restored or reported as published. The old code indexed a half-filled backup
    list and raised IndexError during the restore."""
    target = repo.artifacts / 'gh.json'
    target.unlink()
    target.mkdir()
    result = repo.driver(work, 'publication-partial-write')
    if result.returncode != 1:
        fail('a failed backup exits 1', f'{result.returncode}: {result.stderr}')
        return
    if 'no destination was touched' not in result.stderr:
        fail('a failed backup reports that nothing was touched', result.stderr.strip())
        return
    if 'IndexError' in result.stderr or 'Traceback' in result.stderr:
        fail('a failed backup does not crash the restore', result.stderr.strip())
        return
    changed = [str(relative) for relative, raw in repo.original.items()
               if relative.name != 'gh.json' and (repo.root / relative).read_bytes() != raw]
    if changed:
        fail('a failed backup leaves the other destinations identical', str(changed))
        return
    report('a failed backup restores nothing and reports that nothing was touched')


def case_partial_backup_restore(work: Path, repo: Repo) -> None:
    """The `.complete` marker is consulted: a restore without a complete backup
    would put half-written bytes back over good ones."""
    result = repo.driver(work, 'partial-backup-restore')
    if result.returncode != 0 or 'restore refused' not in result.stdout:
        fail('a restore without a complete backup is refused',
             f'{result.returncode}: {result.stdout.strip()} {result.stderr.strip()}')
        return
    report('a restore without a complete backup is refused')


def case_new_destination_rollback(work: Path, repo: Repo) -> None:
    """A destination that did not exist before publication is removed again on
    rollback: the branch a refresh of a brand-new file takes."""
    result = repo.driver(work, 'new-destination-rollback')
    if result.returncode != 0 or 'removed' not in result.stdout:
        fail('a rollback removes a destination publication created',
             f'{result.returncode}: {result.stdout.strip()} {result.stderr.strip()}')
        return
    report('a rollback removes a destination publication created')


def case_refresh_publishes(work: Path, repo: Repo) -> None:
    result = repo.driver(work, 'refresh-publishes')
    # Exactly 0: 3 means rollback did not fully recover, so accepting it would
    # let a refresh that reports failed recovery on every run pass.
    if result.returncode != 0:
        fail('a clean refresh publishes and exits 0', f'{result.returncode}: {result.stderr}')
        return
    value = json.loads((repo.artifacts / 'node.json').read_text())
    if value['version'] != '9.9.9' or value['bundled'] != {'npm': '10.9.9'}:
        fail('the refreshed pin records the resolved version and bundled npm', str(value))
        return
    if 'LABEL org.agent-vm.version.node="9.9.9"' not in (repo.images / 'Dockerfile').read_text():
        fail('the refreshed label follows the pin', 'the label is stale')
        return
    for relative in (repo.artifacts / 'node.json', repo.images / 'Dockerfile'):
        shutil.copyfile(ROOT / relative.relative_to(repo.root), relative)
    report('a clean refresh publishes the pin, the label and the bundled npm')


def main() -> int:
    work = Path(tempfile.mkdtemp(prefix='pin-artifact-maintenance.')).resolve()
    try:
        case_refresh_publishes(work, Repo(work))
        for case in (case_second_architecture, case_publication_failures, case_signal,
                     case_incomplete_recovery, case_symlinked_destination,
                     case_ci_and_version, case_check, case_gpg_isolation,
                     case_backup_failure, case_partial_backup_restore,
                     case_new_destination_rollback):
            case(work, Repo(work))
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if failures:
        print(f'pin-artifact-maintenance: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('pin-artifact-maintenance: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
