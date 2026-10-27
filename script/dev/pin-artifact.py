#!/usr/bin/env python3
"""Maintainer-host refresh of one third-party release-artifact pin (#96).

This is the only writer of `images/artifacts/<name>.json`, of the GitHub CLI
attestation bundles beside it and of the `org.agent-vm.version.<name>` labels in
`images/Dockerfile`. Builds only verify those bytes: no ordinary build resolves a
URL, downloads a checksum or picks a version.

Every run resolves the requested exact version on the live upstream, hashes both
supported architectures, verifies every piece of authentication evidence and only
then publishes the pin, the evidence files and the label as one transaction. A
partial upstream failure leaves the committed files byte-identical; the tool
never commits, never runs in CI and never adopts a newer version by itself.

Status: 0 published or unchanged, 1 failed with the sources unchanged or fully
restored, 2 usage, 3 undecided or incomplete recovery (the work directory is
retained and printed), 128+N after a signal whose rollback succeeded.
"""
from __future__ import annotations

import argparse
import datetime
import json
import os
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'images' / 'artifacts'))

import artifact_pin  # noqa: E402  (the pin model this tool writes)
from artifact_pin import Refusal  # noqa: E402

ARTIFACTS = ROOT / 'images' / 'artifacts'
DOCKERFILE = ROOT / 'images' / 'Dockerfile'
LABEL_PREFIX = 'org.agent-vm.version.'
DOWNLOAD_TIMEOUT_SECONDS = 300
# Node amd64 is 31,029,996 B today, so 32 MiB leaves 3% headroom; 64 MiB keeps a
# legitimate refresh from failing and still refuses an absurd payload.
MAX_BYTES = 64 << 20


# --- upstream resolution ----------------------------------------------------

def fetch(url: str) -> bytes:
    try:
        with urllib.request.urlopen(url, timeout=DOWNLOAD_TIMEOUT_SECONDS) as response:
            raw = response.read(MAX_BYTES + 1)
    except urllib.error.HTTPError as error:
        raise Refusal(f'cannot fetch {url}: HTTP {error.code}') from None
    except (urllib.error.URLError, OSError) as error:
        raise Refusal(f'cannot fetch {url}: {error}') from None
    if len(raw) > MAX_BYTES:
        raise Refusal(f'{url} is larger than the {MAX_BYTES} byte ceiling')
    return raw


def node_urls(version: str, platform: str) -> dict:
    token = artifact_pin.platform_token('node', platform)
    filename = f'node-v{version}-{token}.tar.xz'
    return {'url': f'https://nodejs.org/dist/v{version}/{filename}',
            'archive': 'tar.xz',
            'member_prefix': artifact_pin.member_prefix(filename)}


def gh_urls(version: str, platform: str) -> dict:
    token = artifact_pin.platform_token('gh', platform)
    filename = f'gh_{version}_{token}.tar.gz'
    return {'url': f'https://github.com/cli/cli/releases/download/v{version}/{filename}',
            'archive': 'tar.gz',
            'member_prefix': artifact_pin.member_prefix(filename)}


URLS = {'node': node_urls, 'gh': gh_urls}


def run(argv: list[str], cwd: Path, check: bool = True,
        environment: dict | None = None) -> subprocess.CompletedProcess:
    """`environment` is the *complete* environment when it is given: a gpg call
    must not inherit the host's keyring pointers (see `gpg_environment`)."""
    result = subprocess.run(argv, cwd=str(cwd), capture_output=True, text=True, check=False,
                            env=environment)
    if check and result.returncode != 0:
        raise Refusal(f'{" ".join(argv)} failed ({result.returncode}): '
                      f'{(result.stderr or result.stdout).strip()}')
    return result


def gpg_environment(home: Path) -> dict:
    """The private keyring *and* the rest of the environment the check may see.

    A constructed `GNUPGHOME` that is never passed to `gpg` leaves the host's
    ambient keyring in charge of pin-time verification, so every gpg call gets
    this and nothing else.
    """
    return {'GNUPGHOME': str(home), 'LC_ALL': 'C',
            'PATH': os.environ.get('PATH', '/usr/bin:/bin')}


def verify_signed_manifest(version: str, key: Path, work: Path) -> dict:
    """The reviewed key must sign this version's manifest, and the fingerprint in
    the key file is what the pin records."""
    manifest_url = f'https://nodejs.org/dist/v{version}/SHASUMS256.txt'
    signature_url = manifest_url + '.sig'
    manifest = fetch(manifest_url)
    signature = fetch(signature_url)
    (work / 'SHASUMS256.txt').write_bytes(manifest)
    (work / 'SHASUMS256.txt.sig').write_bytes(signature)
    home = work / 'gnupg'
    home.mkdir(mode=0o700, exist_ok=True)
    environment = gpg_environment(home)
    run(['gpg', '--batch', '--quiet', '--import', str(key)], work, environment=environment)
    listed = run(['gpg', '--batch', '--with-colons', '--import-options', 'show-only',
                  '--import', str(key)], work, environment=environment).stdout
    primaries = []
    for line in listed.splitlines():
        if line.startswith('pub:'):
            primaries.append(None)
        elif line.startswith('fpr:') and primaries and primaries[-1] is None:
            primaries[-1] = line.split(':')[9].upper()
    if len(primaries) != 1 or primaries[0] is None:
        raise Refusal(f'{key} must carry exactly one primary key, found {primaries}')
    status = run(['gpg', '--batch', '--status-fd=1', '--verify',
                  str(work / 'SHASUMS256.txt.sig'), str(work / 'SHASUMS256.txt')], work,
                 check=False, environment=environment)
    if status.returncode != 0:
        raise Refusal(f'{signature_url} does not verify: {status.stdout.strip()}')
    signers = {field.upper() for line in status.stdout.splitlines() if ' VALIDSIG ' in line
               for field in (line.split()[2], line.split()[-1]) if len(field) in (40, 64)}
    if signers != {primaries[0]}:
        raise Refusal(f'the manifest is signed by {sorted(signers)}, not by {primaries[0]}')
    return {'manifest': manifest, 'signature': signature,
            'manifest_digest': artifact_pin.sha256(manifest),
            'signature_digest': artifact_pin.sha256(signature),
            'key_digest': artifact_pin.sha256(key.read_bytes()),
            'fingerprint': primaries[0]}


def gh_bundle(version: str, archive: Path, work: Path) -> bytes:
    """Download the Sigstore bundle for one archive and verify it with the policy
    the pin will record. `--bundle` needs no token; the download is the only step
    that uses the maintainer's `gh` credentials."""
    run(['gh', 'attestation', 'download', str(archive), '--repo',
         artifact_pin.GH_POLICY['repository']], work)
    digest = artifact_pin.sha256(archive.read_bytes())
    bundle = work / f'sha256:{digest}.jsonl'
    if not bundle.is_file():
        raise Refusal(f'gh attestation download produced no bundle for {archive.name}')
    policy = artifact_pin.GH_POLICY
    result = run(['gh', 'attestation', 'verify', str(archive), '--bundle', str(bundle),
                  '--repo', policy['repository'],
                  '--signer-workflow', policy['signer_workflow'],
                  '--cert-oidc-issuer', policy['cert_oidc_issuer'],
                  '--deny-self-hosted-runners'], work, check=False)
    if result.returncode != 0:
        raise Refusal(f'the {archive.name} attestation does not verify: '
                      f'{(result.stderr or result.stdout).strip()}')
    return bundle.read_bytes()


def bundled_npm(archive: Path, platform: str) -> str:
    """The npm version this archive ships, read from the reviewed bytes.

    A release can change the bundled npm without changing the major, and the
    pin is what the runtime suite compares against, so the value is recorded
    instead of being whatever the image happens to report.
    """
    prefix = artifact_pin.member_prefix(archive.name)
    member = f'{prefix}{artifact_pin.NPM_PACKAGE}'
    with tarfile.open(archive, 'r:xz') as tar:
        try:
            source = tar.extractfile(member)
        except KeyError:
            source = None
        if source is None:
            raise Refusal(f'{archive.name} carries no {artifact_pin.NPM_PACKAGE}')
        try:
            return str(json.loads(source.read())['version'])
        except (ValueError, KeyError) as error:
            raise Refusal(f'{archive.name}: unreadable npm version: {error}') from None


def resolve(name: str, version: str, key: Path | None,
            work: Path) -> tuple[dict, dict, dict | None, dict | None]:
    """Everything a pin needs, resolved and verified; nothing is written here."""
    signed = verify_signed_manifest(version, key, work) if name == 'node' else None
    platforms: dict[str, dict] = {}
    evidence: dict[str, bytes] = {}
    bundled: dict[str, str] | None = {} if name == 'node' else None
    for platform in artifact_pin.PLATFORMS:
        spec = URLS[name](version, platform)
        archive = work / spec['url'].rsplit('/', 1)[-1]
        archive.write_bytes(fetch(spec['url']))
        digest = artifact_pin.sha256(archive.read_bytes())
        entry = {**spec, 'sha256': digest}
        if signed is not None:
            manifest = work / 'SHASUMS256.txt'
            if artifact_pin.signed_digest(manifest.read_bytes(), archive.name) != digest:
                raise Refusal(f'the signed manifest disagrees with {archive.name}')
            found = bundled_npm(archive, platform)
            if bundled.get('npm') not in (None, found):
                raise Refusal(f'the {platform} archive ships npm {found}, another supported '
                              f'architecture ships npm {bundled["npm"]}')
            bundled['npm'] = found
        else:
            checksums = fetch(f'https://github.com/cli/cli/releases/download/v{version}/'
                              f'gh_{version}_checksums.txt').decode()
            if artifact_pin.signed_digest(checksums.encode(), archive.name) != digest:
                raise Refusal(f'the upstream checksums disagree with {archive.name}')
            if 'checksums_digest' not in evidence:
                evidence['checksums'] = fetch(
                    f'https://github.com/cli/cli/releases/download/v{version}/'
                    f'gh_{version}_checksums.txt')
            bundle = gh_bundle(version, archive, work)
            bundle_name = artifact_pin.bundle_name(name, version, platform)
            evidence[bundle_name] = bundle
            entry['bundle'] = {'path': bundle_name,
                               'sha256': artifact_pin.sha256(bundle)}
        platforms[platform] = entry
    return platforms, evidence, signed, bundled


def render(name: str, version: str, platforms: dict, evidence: dict,
           signed: dict | None, bundled: dict | None = None) -> dict:
    today = datetime.date.today().isoformat()
    if name == 'node':
        assert signed is not None
        authentication = {
            'method': 'openpgp-signed-manifest',
            'manifest': {'url': f'https://nodejs.org/dist/v{version}/SHASUMS256.txt',
                         'sha256': signed['manifest_digest']},
            'signature': {'url': f'https://nodejs.org/dist/v{version}/SHASUMS256.txt.sig',
                          'sha256': signed['signature_digest']},
            'key': {'path': artifact_pin.NODE_KEY,
                    'fingerprint': signed['fingerprint'],
                    'sha256': signed['key_digest']},
            'verified_on': today,
            'reason': NODE_REASON,
        }
    else:
        authentication = {
            'method': 'sigstore-attestation',
            'checksums': {
                'url': f'https://github.com/cli/cli/releases/download/v{version}/'
                       f'gh_{version}_checksums.txt',
                'sha256': artifact_pin.sha256(evidence['checksums'])},
            'policy': dict(artifact_pin.GH_POLICY),
            'verified_on': today,
            'evidence': GH_EVIDENCE(version),
            'reason': GH_REASON,
        }
    pin = {
        'schema': artifact_pin.SCHEMA,
        'name': name,
        'kind': 'release-archive',
        'version': version,
        'upstream': {'project': ('https://nodejs.org/dist' if name == 'node'
                                 else 'https://github.com/cli/cli'),
                     'release': f'v{version}'},
        'install': {
            'destination': '/usr/local',
            'payload': list(artifact_pin.NODE_PAYLOAD if name == 'node'
                            else artifact_pin.GH_PAYLOAD),
            'links': [dict(link) for link in artifact_pin.NODE_LINKS]
            if name == 'node' else [],
        },
        'platforms': platforms,
    }
    if name == 'node':
        assert bundled is not None and 'npm' in bundled
        pin['bundled'] = {'npm': bundled['npm']}
    pin['authentication'] = authentication
    return pin


NODE_REASON = (
    'The signed manifest is the selection authority: the build verifies the detached '
    'signature over SHASUMS256.txt against the committed release key, requires the pinned '
    'issuer fingerprint, and refuses unless the signed line for the archive equals the '
    'committed digest before the archive is fetched. The npm, npx and corepack commands '
    'are the ones bundled in this reviewed archive; there is no separate npm pin.')
GH_REASON = (
    'cli/cli publishes no detached OpenPGP signature or standalone Sigstore bundle; its '
    'SLSA provenance is readable only by `gh attestation verify`, and the GitHub CLI is the '
    'artifact being installed, so the authoritative verification is the pin-time one '
    'recorded here and the build\'s check binds the committed bundle to the committed '
    'archive with the installed verifier. One statement covers every release asset, so the '
    'two committed bundle files carry the same bytes; each platform entry still hashes its '
    'own copy. The committed checksums file is the pin-time cross-check that the arch line '
    'equals the committed digest, and its bytes are committed so an upstream rewrite is '
    'visible; ordinary builds do not fetch it, because the bundle proves more.')


def GH_EVIDENCE(version: str) -> list[str]:
    return [f'https://github.com/cli/cli/releases/tag/v{version}',
            'https://github.com/cli/cli/attestations',
            'https://github.com/cli/cli/blob/trunk/.github/workflows/deployment.yml']


def render_label(dockerfile: str, name: str, version: str) -> str:
    key = f'{LABEL_PREFIX}{name}'
    lines = dockerfile.split('\n')
    matches = [index for index, line in enumerate(lines)
               if line.startswith(f'LABEL {key}=')]
    if len(matches) != 1:
        raise Refusal(f'{DOCKERFILE} must declare exactly one `LABEL {key}=`, found '
                      f'{len(matches)}')
    index = matches[0]
    lines[index] = f'LABEL {key}="{version}"'
    return '\n'.join(lines)


# --- publication ------------------------------------------------------------

class Recovery(Exception):
    """Rollback was not complete: the work directory is the only way back."""


class Publisher:
    """One transaction over files that may not exist yet.

    Three explicit phases - staging, a *complete* backup, publication - because
    the order decides what a failure may undo. Staging and a failed backup touch
    no destination at all; only publication can, so only publication is restored,
    and only as far as it actually got (the destination being written counts).
    The `.complete` marker is what makes that safe: a restore never guesses from a
    half-written backup directory.

    The conventions are the shell publisher's: an invocation-owned backup
    directory, a numbered backup per destination, a `.complete` marker, restore
    in reverse order, and a retained work directory plus `recovery.json` when a
    restore fails so nothing is guessed afterwards.
    """

    def __init__(self, root: Path = ROOT) -> None:
        # `root` is the repository the destinations are relative to. It is a
        # parameter, not the module constant, because the Codex owner writer
        # publishes its own six-file closure through this same transaction; the
        # default keeps every Node/gh call site unchanged.
        self.root = Path(root)
        # Absolute at creation, before any publication or recovery failure: a
        # retained work directory is printed during recovery, and a relative path
        # resolved from a different cwd would point nowhere.
        self.work = Path(os.path.abspath(
            tempfile.mkdtemp(prefix='pin-artifact.', dir=os.environ.get('TMPDIR'))))
        self.backup = self.work / 'backup'
        self.backup.mkdir()
        self.staged = self.work / 'staged'
        self.staged.mkdir()
        self.destinations: list[Path] = []
        self.existed: list[bool] = []
        self.phase = 'staging'
        # Pre-rendered recovery lines. The signal handler ends the process with
        # `os._exit(3)`, which skips every buffer flush and every line the caller
        # would print after `publish` returns; these are built here, at setup, and
        # written with `os.write`, which is what a handler may safely do.
        self.work_line = (f'pin-artifact: recovery material retained at {self.work}\n'
                          .encode())
        self.owner_line = b''
        self.owner_note = ''
        # Destinations publication may have written: set *before* each copy, so
        # the file being written is included in a rollback.
        self.touched = 0

    def report_owner(self, owner: str | None) -> None:
        """Record the caller's own scratch directory in the signal-safe form.

        The publisher cannot discover it: for the Codex owner writer it is the
        owner's work or stage directory, which only the caller knows, and the
        handler that exits the process runs before the caller can say anything.
        """
        if not owner:
            return
        self.owner_note = f'pin-artifact: owner scratch retained at {owner}'
        self.owner_line = (self.owner_note + '\n').encode()

    def stage(self, relative: str, raw: bytes) -> None:
        path = self.staged / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)

    def back_up(self) -> None:
        self.phase = 'backup'
        self.existed = []
        for index, destination in enumerate(self.destinations):
            exists = destination.exists() or destination.is_symlink()
            self.existed.append(exists)
            if not exists:
                continue
            temporary = self.backup / f'.tmp{index}'
            shutil.copyfile(destination, temporary)
            shutil.copystat(destination, temporary)
            temporary.rename(self.backup / str(index))
        (self.backup / '.complete').write_text('')

    def backup_complete(self) -> bool:
        marker = (self.backup / '.complete').is_file()
        return marker and len(self.existed) == len(self.destinations)

    def publish(self) -> None:
        self.phase = 'publication'
        for index, destination in enumerate(self.destinations):
            staged = self.staged / destination.relative_to(self.root)
            destination.parent.mkdir(parents=True, exist_ok=True)
            self.touched = index + 1
            shutil.copyfile(staged, destination)

    def restore(self) -> None:
        """Reverse order over what publication could have touched.

        Nothing to do before publication, and nothing to trust without a complete
        backup: restoring from a partial backup directory would replace good bytes
        with half a file.
        """
        if self.touched == 0:
            return
        if not self.backup_complete():
            raise OSError(f'the backup in {self.backup} is incomplete')
        for index in reversed(range(self.touched)):
            destination = self.destinations[index]
            if self.existed[index]:
                shutil.copyfile(self.backup / str(index), destination)
                shutil.copystat(self.backup / str(index), destination)
            elif destination.exists() or destination.is_symlink():
                destination.unlink()

    def recovery_record(self, error: str) -> None:
        record = [{'destination': str(destination.relative_to(self.root)),
                   'existed': self.existed[index] if index < len(self.existed) else None,
                   'backup': str(self.backup / str(index))
                   if index < len(self.existed) and self.existed[index] else None}
                  for index, destination in enumerate(self.destinations)]
        (self.work / 'recovery.json').write_text(
            json.dumps({'error': error, 'phase': self.phase, 'destinations': record},
                       indent=2) + '\n')

    def record_recovery_best_effort(self, error: str) -> None:
        """Write the recovery record without letting its failure escape.

        A rollback that failed and then cannot even describe itself is still a
        status-3 outcome: the retained `backup/` directory is the way back, and
        `recovery.json` is a convenience. Catching only `OSError` would let a
        serialization or encoding failure replace the status with a traceback and
        status 1, so *every* exception is caught here and reported instead.
        """
        try:
            self.recovery_record(error)
        except Exception as record_error:  # noqa: BLE001 - the contract is "never escape"
            print(f'pin-artifact: could not write the recovery record in {self.work} '
                  f'({record_error}); the backups listed below are still authoritative',
                  file=sys.stderr)


def symlinked(destination: Path, root: Path = ROOT) -> Path | None:
    """A destination reached through a symlink is a redirection, not a layout.

    Rejecting only a symlinked *leaf* let the publisher change files through a
    symlinked parent directory, so the leaf and every ancestor between the root
    and it are checked with `lstat` semantics.
    """
    if destination.is_symlink():
        return destination
    try:
        relative = destination.relative_to(root)
    except ValueError:
        return root
    current = root
    for part in relative.parts[:-1]:
        current = current / part
        if current.is_symlink():
            return current
        if current.exists() and not current.is_dir():
            return current
    return None


def publish(staged: dict[str, bytes], *, root: Path = ROOT,
            owner: str | None = None) -> int:
    """Publish `staged` as one transaction; report the caller's own scratch too.

    `owner` is the caller's scratch directory (absolute), if it has one. It is
    reported together with this transaction's work directory *from the signal
    handler as well*, because that handler leaves through `os._exit(3)`: a caller
    that only printed its own path after `publish` returned would never say it.
    """
    publisher = Publisher(root)
    publisher.report_owner(owner)
    destinations = [root / relative for relative in staged]
    refused = sorted({str(found) for found in
                      (symlinked(destination, root) for destination in destinations)
                      if found is not None})
    if refused:
        print(f'pin-artifact: refusing a path reached through a symlink: {refused}',
              file=sys.stderr)
        shutil.rmtree(publisher.work, ignore_errors=True)
        return 1
    publisher.destinations = destinations

    def interrupted(signum, _frame):
        # Async-signal-safe only: no print, no logging, no allocation of note. The
        # two paths that make recovery possible were rendered before the handlers
        # were installed.
        if publisher.phase == 'publication' and publisher.touched:
            try:
                publisher.restore()
            except OSError as error:
                publisher.record_recovery_best_effort(str(error))
                os.write(2, b'pin-artifact: interrupted and restore incomplete\n')
                os.write(2, publisher.owner_line)
                os.write(2, publisher.work_line)
                os.write(2, b'STOP: recover both directories above before further '
                            b'edits/builds/commits\n')
                os._exit(3)
            os.write(2, b'pin-artifact: interrupted; sources restored\n')
        else:
            shutil.rmtree(publisher.work, ignore_errors=True)
            os.write(2, b'pin-artifact: interrupted before publication; sources '
                        b'unchanged\n')
        os._exit(128 + signum)

    for signum in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(signum, interrupted)
    try:
        for relative, raw in staged.items():
            publisher.stage(relative, raw)
        publisher.back_up()
        publisher.publish()
    except OSError as error:
        if publisher.touched == 0:
            print(f'pin-artifact: {publisher.phase} failed ({error}); no destination was '
                  f'touched, sources unchanged', file=sys.stderr)
            shutil.rmtree(publisher.work, ignore_errors=True)
            return 1
        print(f'pin-artifact: publication failed ({error}); restoring', file=sys.stderr)
        try:
            publisher.restore()
        except OSError as restore_error:
            publisher.record_recovery_best_effort(str(restore_error))
            print(f'pin-artifact: restore incomplete ({restore_error}); recovery material '
                  f'retained at {publisher.work}', file=sys.stderr)
            if publisher.owner_note:
                print(publisher.owner_note, file=sys.stderr)
            print(f'STOP: recover {publisher.work}'
                  + (f' together with {owner}' if owner else '')
                  + ' before further edits/builds/commits', file=sys.stderr)
            return 3
        shutil.rmtree(publisher.work, ignore_errors=True)
        return 1
    shutil.rmtree(publisher.work, ignore_errors=True)
    return 0


# --- commands ---------------------------------------------------------------

def refuse_ci() -> None:
    if os.environ.get('GITHUB_ACTIONS') == 'true':
        raise Refusal('refusing GitHub Actions: pins are maintainer-host maintenance only')


def check() -> int:
    """Re-verify the committed pins offline: schema, evidence digests and labels."""
    problems = []
    for name in ('node', 'gh'):
        try:
            pin = artifact_pin.load(ARTIFACTS / f'{name}.json')
            if pin.name != name:
                problems.append(f'{name}: the pin names {pin.name}')
            if isinstance(pin.authentication, artifact_pin.OpenPgp):
                artifact_pin.check_evidence(pin, pin.authentication.key_path,
                                            pin.authentication.key_digest)
            for platform, archive in pin.archives.items():
                if archive.bundle:
                    artifact_pin.check_evidence(pin, archive.bundle, archive.bundle_digest)
                print(f'{name} {pin.version} {platform}: {archive.filename} {archive.digest}')
            expected = f'LABEL {LABEL_PREFIX}{name}="{pin.version}"'
            if expected not in DOCKERFILE.read_text():
                problems.append(f'images/Dockerfile does not declare {expected}')
        except Refusal as error:
            problems.append(str(error))
    if problems:
        for problem in problems:
            print(f'FAIL: {problem}', file=sys.stderr)
        return 1
    print('pin-artifact: the committed pins, evidence files and labels agree')
    return 0


def refresh(name: str, version: str, key: Path | None) -> int:
    refuse_ci()
    if not artifact_pin.VERSION.fullmatch(version):
        _usage(f'--version must be an exact X.Y.Z version, got {version}')
    if name == 'node' and key is None:
        key = ARTIFACTS / artifact_pin.NODE_KEY
        if not key.is_file():
            raise Refusal(f'no committed node release key at {key}')
    work = Path(tempfile.mkdtemp(prefix='pin-artifact.work.', dir=os.environ.get('TMPDIR')))
    try:
        platforms, evidence, signed, bundled = resolve(name, version, key, work)
        pin = render(name, version, platforms, evidence, signed, bundled)
        text = json.dumps(pin, indent=2) + '\n'
        artifact_pin.parse(text, ARTIFACTS, name)
        dockerfile = render_label(DOCKERFILE.read_text(), name, version)
        staged = {f'images/artifacts/{name}.json': text.encode(),
                  'images/Dockerfile': dockerfile.encode()}
        if signed is not None:
            staged[f'images/artifacts/{artifact_pin.NODE_KEY}'] = key.read_bytes()
        for entry in evidence:
            if entry.endswith('.bundle.jsonl'):
                staged[f'images/artifacts/{entry}'] = evidence[entry]
        committed = {relative: (ROOT / relative).read_bytes()
                     for relative in staged if (ROOT / relative).is_file()}
        if committed == staged:
            print(f'pin-artifact: unchanged ({name} {version})')
            return 0
        for relative in sorted(staged):
            if committed.get(relative) != staged[relative]:
                print(f'pin-artifact: would publish {relative}', file=sys.stderr)
        status = publish(staged, root=ROOT)
        if status == 0:
            print(f'pin-artifact: published {name} {version}: '
                  + ', '.join(sorted(staged)))
            print('pin-artifact: now run: bash script/build/base-inputs.sh --write')
        return status
    finally:
        shutil.rmtree(work, ignore_errors=True)


def _usage(message: str) -> None:
    print(f'pin-artifact: {message}', file=sys.stderr)
    sys.exit(2)


USAGE = """usage: python3 script/dev/pin-artifact.py --artifact {node,gh} --version X.Y.Z [--key FILE]
       python3 script/dev/pin-artifact.py --check

--artifact/--version  resolve that exact upstream release for linux/amd64 and
                      linux/arm64, verify every piece of evidence and publish
                      images/artifacts/<name>.json, its evidence files and the
                      org.agent-vm.version.<name> label as one transaction.
--key FILE            the Node release key to verify against (default: the
                      committed images/artifacts/node-release-key.asc).
--check               re-verify the committed pins, their evidence digests and
                      the Dockerfile labels; offline, writes nothing.
"""


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--artifact')
    parser.add_argument('--version')
    parser.add_argument('--key')
    parser.add_argument('--check', action='store_true')
    parser.add_argument('--help', action='store_true')
    arguments = parser.parse_args(argv)
    if arguments.help:
        print(USAGE, end='')
        return 0
    try:
        if arguments.check:
            if arguments.artifact or arguments.version or arguments.key:
                _usage('--check takes no other argument')
            return check()
        if not arguments.artifact or not arguments.version:
            _usage('both --artifact and --version are required')
        if arguments.artifact not in URLS:
            _usage(f'--artifact must be one of {sorted(URLS)}')
        return refresh(arguments.artifact, arguments.version,
                       Path(arguments.key) if arguments.key else None)
    except Refusal as error:
        print(f'pin-artifact: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
