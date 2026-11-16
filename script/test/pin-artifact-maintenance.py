#!/usr/bin/env python3
"""Black-box evidence for the pin maintenance writer (#96, #179).

`script/dev/pin-artifact.sh` is the only writer of a pin, and the guarantees it
claims are the ones a successful run cannot show: a failing second architecture
changes nothing, a symlinked destination is refused, CI is refused, an inexact
version is a usage error, a backup that cannot be taken touches nothing, pin-time
verification uses its own keyring, and `verified_on` is the UTC date rather than
the maintainer host's.

Every case drives the real program through the wrapper. The upstream bytes come
from a fixture directory (`FIXTURE_PIN_UPSTREAM`) and the Node release key is a
fixture key this file generates, so the detached signature, the archive decode and
the whole publication are real work — nothing is monkeypatched, because nothing can
be: the writer is a binary. The archive and manifest are built here, which is why a
case can make one architecture fail while the other resolves.

The publication transaction's own fault paths (a partial write, a signal during
publication, an incomplete restore, a recovery record that cannot be written) need
injected failures inside the process, so they are Go tests in
`internal/artifactwriter/publish_test.go`, where the same cases keep their names.

usage: python3 script/test/pin-artifact-maintenance.py
"""
from __future__ import annotations

import datetime
import hashlib
import io
import json
import lzma
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
TOOL = ROOT / 'script' / 'dev' / 'pin-artifact.sh'
ARTIFACTS = ROOT / 'images' / 'artifacts'
VERSION = '9.9.9'
NPM_VERSION = '10.9.9'
PLATFORMS = {'linux/amd64': 'linux-x64', 'linux/arm64': 'linux-arm64'}

failures: list[str] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: str) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


# --- fixture ----------------------------------------------------------------


class Fixture:
    """A throwaway repository root and the upstream bytes a refresh resolves."""

    def __init__(self, work: Path) -> None:
        self.work = work
        self.root = work / 'repo'
        self.upstream = work / 'upstream'
        self.artifacts = self.root / 'images' / 'artifacts'
        self.artifacts.mkdir(parents=True)
        (self.root / 'images' / 'Dockerfile').parent.mkdir(parents=True, exist_ok=True)
        for path in ARTIFACTS.iterdir():
            if path.is_file():
                shutil.copyfile(path, self.artifacts / path.name)
        shutil.copyfile(ROOT / 'images' / 'Dockerfile', self.root / 'images' / 'Dockerfile')
        self.original = {path: path.read_bytes() for path in self.destinations()}
        self.signer = work / 'signer'
        self.signer.mkdir(mode=0o700)
        self.fingerprint = self._generate_key(self.signer)
        self.key = self.artifacts / 'node-release-key.asc'
        self.committed_key = self.key.read_bytes()
        self.key.write_text(self._export_key(self.signer, self.fingerprint))
        self.serve()
        self.original = {path: path.read_bytes() for path in self.destinations()}

    # A destination is anything a refresh may rewrite: both pins, the label and the
    # release key.
    def destinations(self) -> list[Path]:
        return [self.artifacts / 'node.json', self.root / 'images' / 'Dockerfile',
                self.artifacts / 'node-release-key.asc']

    def unchanged(self) -> list[str]:
        return [str(path) for path, raw in self.original.items()
                if path.read_bytes() != raw]

    def _generate_key(self, home: Path) -> str:
        environment = dict(os.environ, GNUPGHOME=str(home), LC_ALL='C')
        generated = subprocess.run(
            ['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
             '--quick-generate-key',
             f'pin fixture <fixture-{home.name}@example.invalid>', 'ed25519', 'sign', '0'],
            capture_output=True, text=True, check=False, env=environment)
        if generated.returncode != 0:
            raise SystemExit(f'cannot generate the fixture key: {generated.stderr.strip()}')
        listed = subprocess.run(['gpg', '--batch', '--with-colons', '--list-keys'],
                                capture_output=True, text=True, check=False,
                                env=environment).stdout
        return next(line.split(':')[9] for line in listed.splitlines()
                    if line.startswith('fpr:')).upper()

    def _export_key(self, home: Path, fingerprint: str) -> str:
        environment = dict(os.environ, GNUPGHOME=str(home), LC_ALL='C')
        return subprocess.run(['gpg', '--batch', '--armor', '--export', fingerprint],
                              capture_output=True, text=True, check=False,
                              env=environment).stdout

    def fixture_path(self, url: str) -> Path:
        rest = url.split('://', 1)[-1]
        return self.upstream / rest

    def archive(self, platform: str) -> bytes:
        token = PLATFORMS[platform]
        filename = f'node-v{VERSION}-{token}.tar.xz'
        source = json.dumps({'version': NPM_VERSION}).encode()
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode='w:xz') as tar:
            member = tarfile.TarInfo(
                f'node-v{VERSION}-{token}/lib/node_modules/npm/package.json')
            member.size = len(source)
            member.mode = 0o644
            tar.addfile(member, io.BytesIO(source))
        path = self.fixture_path(f'https://nodejs.org/dist/v{VERSION}/{filename}')
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(buffer.getvalue())
        return buffer.getvalue()

    def serve(self) -> None:
        """Write both archives, the signed manifest and the detached signature."""
        lines = []
        for platform in PLATFORMS:
            filename = f'node-v{VERSION}-{PLATFORMS[platform]}.tar.xz'
            lines.append(f'{sha256(self.archive(platform))}  {filename}')
        manifest = self.fixture_path(f'https://nodejs.org/dist/v{VERSION}/SHASUMS256.txt')
        manifest.parent.mkdir(parents=True, exist_ok=True)
        manifest.write_text('\n'.join(lines) + '\n')
        self.sign(manifest)

    def sign(self, manifest: Path, home: Path | None = None) -> None:
        environment = dict(os.environ, GNUPGHOME=str(home or self.signer), LC_ALL='C')
        signed = subprocess.run(
            ['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
             '--armor', '--yes', '--detach-sign', '--output', str(manifest) + '.sig',
             str(manifest)],
            capture_output=True, text=True, check=False, env=environment)
        if signed.returncode != 0:
            raise SystemExit(f'cannot sign the fixture manifest: {signed.stderr.strip()}')

    def run(self, *arguments: str, environment: dict | None = None,
            zone: str | None = None) -> subprocess.CompletedProcess:
        env = dict(os.environ)
        env.pop('GITHUB_ACTIONS', None)
        for name in ('FIXTURE_PIN_UPSTREAM', 'GNUPGHOME', 'TZ'):
            env.pop(name, None)
        env['FIXTURE_PIN_UPSTREAM'] = str(self.upstream)
        env.update(environment or {})
        if zone is not None:
            env['TZ'] = zone
        return subprocess.run(
            ['bash', str(TOOL), '--root', str(self.root), *arguments],
            cwd=ROOT, capture_output=True, text=True, check=False, env=env)

    def refresh(self, version: str = VERSION,
                environment: dict | None = None,
                zone: str | None = None) -> subprocess.CompletedProcess:
        return self.run('--artifact', 'node', '--version', version,
                        environment=environment, zone=zone)

    def pin(self) -> dict:
        return json.loads((self.artifacts / 'node.json').read_text())


def scratch(work: Path, name: str) -> Path:
    directory = work / name
    directory.mkdir()
    return directory


# --- cases ------------------------------------------------------------------


def case_refresh_publishes(work: Path) -> None:
    fixture = Fixture(scratch(work, 'refresh'))
    result = fixture.refresh()
    if result.returncode != 0:
        fail('a clean refresh publishes and exits 0', f'{result.returncode}: {result.stderr}')
        return
    if 'published node 9.9.9' not in result.stdout:
        fail('a clean refresh says what it published', result.stdout.strip())
        return
    value = fixture.pin()
    if value['version'] != VERSION or value.get('bundled') != {'npm': NPM_VERSION}:
        fail('the refreshed pin records the resolved version and bundled npm', str(value))
        return
    label = f'LABEL org.agent-vm.version.node="{VERSION}"'
    if label not in (fixture.root / 'images' / 'Dockerfile').read_text():
        fail('the refreshed label follows the pin', 'the label is stale')
        return
    if sha256(fixture.key.read_bytes()) != value['authentication']['key']['sha256']:
        fail('the pin binds the key it was verified with', 'the key digest disagrees')
        return
    published = {path: path.read_bytes() for path in fixture.destinations()}
    again = fixture.refresh()
    if again.returncode != 0 or 'unchanged (node 9.9.9)' not in again.stdout:
        fail('a second identical refresh publishes nothing',
             f'{again.returncode}: {again.stdout.strip()} {again.stderr.strip()}')
        return
    rewritten = [str(path) for path in published if path.read_bytes() != published[path]]
    if rewritten:
        fail('a second identical refresh leaves the bytes identical', str(rewritten))
        return
    report('a clean refresh publishes the pin, the label and the bundled npm; a second is a no-op')


def case_second_architecture(work: Path) -> None:
    fixture = Fixture(scratch(work, 'second'))
    missing = fixture.fixture_path(
        f'https://nodejs.org/dist/v{VERSION}/node-v{VERSION}-linux-arm64.tar.xz')
    missing.unlink()
    result = fixture.refresh()
    if result.returncode != 1:
        fail('a failed second architecture exits 1', f'{result.returncode}: {result.stderr}')
        return
    if 'cannot fetch' not in result.stderr:
        fail('a failed second architecture reports the failure', result.stderr.strip())
        return
    if fixture.unchanged():
        fail('a failed second architecture leaves the committed bytes identical',
             str(fixture.unchanged()))
        return
    report('a second-architecture failure publishes nothing')


def case_manifest_disagrees(work: Path) -> None:
    fixture = Fixture(scratch(work, 'disagrees'))
    archive = fixture.fixture_path(
        f'https://nodejs.org/dist/v{VERSION}/node-v{VERSION}-linux-arm64.tar.xz')
    archive.write_bytes(archive.read_bytes() + b'drift')
    result = fixture.refresh()
    if result.returncode != 1 or 'the signed manifest disagrees' not in result.stderr:
        fail('an archive the signed manifest does not name is refused',
             f'{result.returncode}: {result.stderr.strip()}')
        return
    if fixture.unchanged():
        fail('a manifest disagreement publishes nothing', str(fixture.unchanged()))
        return
    report('an archive whose digest the signed manifest does not carry is refused')


def case_foreign_signature(work: Path) -> None:
    fixture = Fixture(scratch(work, 'foreign'))
    foreign = scratch(work, 'foreign-signer')
    foreign.chmod(0o700)
    fingerprint = fixture._generate_key(foreign)
    fixture.sign(fixture.fixture_path(
        f'https://nodejs.org/dist/v{VERSION}/SHASUMS256.txt'), home=foreign)
    result = fixture.refresh()
    if result.returncode != 1:
        fail('a manifest signed by another key exits 1',
             f'{result.returncode}: {result.stderr}')
        return
    if fingerprint in result.stderr:
        # The refusal may quote the signer, which is the useful diagnostic.
        pass
    if 'does not verify' not in result.stderr and 'signed by' not in result.stderr:
        fail('a manifest signed by another key is refused', result.stderr.strip())
        return
    if fixture.unchanged():
        fail('a foreign signature publishes nothing', str(fixture.unchanged()))
        return
    report('a manifest signed by a key the pin does not name is refused')


def case_symlinked_destination(work: Path) -> None:
    fixture = Fixture(scratch(work, 'symlink'))
    elsewhere = fixture.work / 'elsewhere'
    elsewhere.mkdir()
    real = fixture.artifacts / 'node.json'
    moved = elsewhere / 'node.json'
    shutil.move(str(real), str(moved))
    real.symlink_to(moved)
    result = fixture.refresh()
    if result.returncode != 1 or 'symlink' not in result.stderr:
        fail('a symlinked destination is refused', f'{result.returncode}: {result.stderr}')
        return
    if moved.read_bytes() != fixture.original[real]:
        fail('a symlinked destination is not written through', moved.read_text()[:40])
        return
    real.unlink()
    shutil.move(str(moved), str(real))

    parent = fixture.artifacts
    target = fixture.work / 'artifacts-target'
    shutil.move(str(parent), str(target))
    parent.symlink_to(target)
    result = fixture.refresh()
    if result.returncode != 1 or 'symlink' not in result.stderr:
        fail('a symlinked destination parent is refused',
             f'{result.returncode}: {result.stderr}')
        return
    if '9.9.9' in (target / 'node.json').read_text():
        fail('a symlinked parent is not written through', 'the staged bytes landed')
        return
    parent.unlink()
    shutil.move(str(target), str(parent))
    report('a symlinked destination and a symlinked destination parent are refused')


def case_ci_and_version(work: Path) -> None:
    fixture = Fixture(scratch(work, 'ci'))
    result = fixture.refresh(environment={'GITHUB_ACTIONS': 'true'})
    if result.returncode != 1 or 'refusing GitHub Actions' not in result.stderr:
        fail('GITHUB_ACTIONS=true is refused', f'{result.returncode}: {result.stderr}')
        return
    if fixture.unchanged():
        fail('a refused CI run writes nothing', str(fixture.unchanged()))
        return
    result = fixture.refresh(version='9.9')
    if result.returncode != 2 or 'X.Y.Z' not in result.stderr:
        fail('an inexact version is a usage error', f'{result.returncode}: {result.stderr}')
        return
    report('CI and a non-X.Y.Z version are refused before anything is staged')


def case_unrestorable_publication(work: Path) -> None:
    """A publication that cannot be undone reports status 3 and where the way back is.

    The pin file itself is read-only, so the first copy fails *and* the restore of
    that same file fails. That is the one case where the writer must not claim the
    sources are unchanged: it has to retain its backups and say where they are.
    """
    fixture = Fixture(scratch(work, 'unrestorable'))
    pin = fixture.artifacts / 'node.json'
    pin.chmod(0o400)
    try:
        result = fixture.refresh()
    finally:
        pin.chmod(0o644)
    if result.returncode != 3:
        fail('an incomplete recovery exits 3', f'{result.returncode}: {result.stderr}')
        return
    if 'restore incomplete' not in result.stderr or 'STOP: recover' not in result.stderr:
        fail('an incomplete recovery says where the material is', result.stderr.strip())
        return
    matches = re.findall(r'(/[^\s:;]+/pin-artifact\.[A-Za-z0-9_]+)', result.stderr)
    retained = [Path(match) for match in matches if Path(match).is_dir()]
    if not retained:
        fail('an incomplete recovery retains its work directory', result.stderr.strip())
        return
    record = retained[-1] / 'recovery.json'
    if not record.is_file():
        fail('an incomplete recovery retains recovery.json', str(retained[-1]))
        return
    value = json.loads(record.read_text())
    if value['phase'] != 'publication':
        fail('the recovery record names the phase', value['phase'])
        return
    for path in retained:
        shutil.rmtree(path, ignore_errors=True)
    report('an incomplete recovery exits 3 and retains the work directory')


def case_check(work: Path) -> None:
    fixture = Fixture(scratch(work, 'check'))
    # The committed pins bind the committed key; the fixture refresh replaced it.
    fixture.key.write_bytes(fixture.committed_key)
    committed = subprocess.run(['bash', str(TOOL), '--check'], cwd=ROOT,
                               capture_output=True, text=True, check=False,
                               env={key: value for key, value in os.environ.items()
                                    if key != 'GITHUB_ACTIONS'})
    if committed.returncode != 0:
        fail('--check accepts the repository pins', committed.stderr.strip())
        return
    value = fixture.pin()
    value['platforms']['linux/arm64']['sha256'] = '0' * 64
    value['install']['payload'] = ['../escape']
    (fixture.artifacts / 'node.json').write_text(json.dumps(value, indent=2) + '\n')
    result = fixture.run('--check')
    if result.returncode != 1 or 'FAIL' not in result.stderr:
        fail('--check refuses a mutated pin', f'{result.returncode}: {result.stderr}')
        return
    (fixture.artifacts / 'node.json').write_bytes(fixture.original[fixture.artifacts / 'node.json'])
    result = fixture.run('--check')
    if result.returncode != 0:
        fail('--check accepts the fixture pins', result.stderr.strip())
        return
    report('--check accepts the committed pins and refuses a mutated one')


def case_gpg_isolation(work: Path) -> None:
    """Pin-time verification must use the keyring the writer constructs.

    The writer builds a private GNUPGHOME; if it is never passed to gpg, the host's
    ambient keyring takes the import and decides the check (#96 review). The ambient
    home here is a decoy: after a successful verification it must hold nothing.
    """
    fixture = Fixture(scratch(work, 'isolation'))
    ambient = fixture.work / 'ambient-gnupg'
    ambient.mkdir(mode=0o700)
    result = fixture.refresh(environment={'GNUPGHOME': str(ambient)})
    if result.returncode != 0:
        fail('the isolated verification succeeds', f'{result.returncode}: {result.stderr}')
        return
    if fixture.pin()['authentication']['key']['fingerprint'] != fixture.fingerprint:
        fail('the isolated verification reports the pinned fingerprint',
             fixture.pin()['authentication']['key']['fingerprint'])
        return
    if any(ambient.iterdir()):
        fail('the ambient keyring is untouched', str([path.name for path in ambient.iterdir()]))
        return
    report('pin-time verification uses its own keyring and leaves the ambient one alone')


# The writer is the only site that stamps `verified_on`. The two extreme zones
# differ from UTC in opposite directions (Kiritimati is UTC+14, Etc/GMT+12 is
# UTC-12), so at least one of the two always has a local date other than the UTC
# date. That is what makes the pair time-of-day independent instead of a coin flip.
CLOCK_ZONES = ('Pacific/Kiritimati', 'Etc/GMT+12')


def utc_date() -> datetime.date:
    return datetime.datetime.now(datetime.timezone.utc).date()


def case_writer_clock(work: Path) -> None:
    """`verified_on` is the UTC date, never the maintainer host's local date.

    A pin is written on one host and read by reviewers and builders in every zone,
    so the stamp has to be one instant. The stamp is read back from a published pin
    in each zone: the assertion is about the real process, not about a date function.
    """
    label = 'clock_writer_stamps_the_utc_date'
    for attempt in range(2):
        today = utc_date()
        differing = False
        stale = None
        for zone in CLOCK_ZONES:
            fixture = Fixture(scratch(work, f'clock-{attempt}-{zone.replace("/", "-")}'))
            local = subprocess.run(
                ['python3', '-c',
                 'import datetime, os, time; time.tzset(); '
                 'print(datetime.date.today().isoformat())'],
                capture_output=True, text=True, check=False,
                env=dict(os.environ, TZ=zone)).stdout.strip()
            if local == today.isoformat():
                continue
            differing = True
            result = fixture.refresh(zone=zone)
            if result.returncode != 0:
                stale = f'under TZ={zone} the refresh failed: {result.stderr.strip()}'
                break
            stamped = fixture.pin()['authentication']['verified_on']
            if stamped != today.isoformat():
                stale = (f'under TZ={zone}, where the local date is {local}, the pin is '
                         f'stamped {stamped}, not the UTC date {today}')
                break
        if stale is not None:
            fail(label, stale)
            return
        if utc_date() != today:
            continue
        if not differing:
            fail(label, 'no candidate zone differs from the UTC date')
            return
        report(label)
        return
    fail(label, 'the UTC date rolled over twice while this case ran')


def main() -> int:
    if shutil.which('gpg') is None:
        print('pin-artifact-maintenance: gpg is required to sign the fixture manifest',
              file=sys.stderr)
        return 1
    work = Path(tempfile.mkdtemp(prefix='pin-artifact-maintenance.')).resolve()
    try:
        for case in (case_refresh_publishes, case_second_architecture,
                     case_manifest_disagrees, case_foreign_signature,
                     case_symlinked_destination, case_ci_and_version,
                     case_unrestorable_publication, case_check, case_gpg_isolation,
                     case_writer_clock):
            case(work)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if failures:
        print(f'pin-artifact-maintenance: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('pin-artifact-maintenance: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
