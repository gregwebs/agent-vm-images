#!/usr/bin/env python3
"""Offline evidence for the pinned release-artifact installer (#96).

Every case here runs `images/install-artifact.sh` - the installer a build runs -
against a synthesized release tree that is served from `file://` or from a local
HTTP server. The fixture has its own OpenPGP key, so the Node signature chain,
the digest comparison, the archival preflight and the published result are the
real code paths; nothing reaches the network and no committed evidence is
touched.

What this suite cannot prove is that the upstream bytes behind the committed
pins are still the reviewed ones: that needs a network and is the maintenance
tool's job (`script/dev/pin-artifact.py`). The negative cases are the point here:
changed bytes, a different architecture, a wrong version, a bad signature, a
substituted key, a deleted authentication block, a 404, a refused connection, a
hostile archive member, and that a failure never falls back to apt, npm, a
re-download or an extraction.
"""
from __future__ import annotations

import http.server
import io
import json
import os
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import threading
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
INSTALLER = ROOT / 'images' / 'install-artifact.sh'
ARTIFACTS = ROOT / 'images' / 'artifacts'
sys.path.insert(0, str(ARTIFACTS))

import artifact_pin  # noqa: E402  (the model the installer and the gate share)

VERSION = '9.9.9'
# The npm version the fixture Node archive ships. The pin records it and the
# installer refuses an archive whose bundled npm differs, so this is a value the
# cases can change to prove the check is live.
NPM_VERSION = '9.8.7'
NODE_PREFIX = {'linux/amd64': f'node-v{VERSION}-linux-x64/', 'linux/arm64': f'node-v{VERSION}-linux-arm64/'}
GH_PREFIX = {'linux/amd64': f'gh_{VERSION}_linux_amd64/', 'linux/arm64': f'gh_{VERSION}_linux_arm64/'}
ARCH = {'linux/amd64': 'amd64', 'linux/arm64': 'arm64'}
GH_POLICY = dict(artifact_pin.GH_POLICY)

# The image's own PATH tools, plus recorders for the fallbacks the installer must
# never reach: any invocation of one of these is a failure, not a warning.
FALLBACK_TOOLS = ('apt-get', 'apt', 'npm', 'npx', 'curl', 'wget')

failures: list[str] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: str) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def sha256(raw: bytes) -> str:
    return artifact_pin.sha256(raw)


# --- fixture synthesis ------------------------------------------------------

def run(argv: list[str], **kwargs) -> subprocess.CompletedProcess:
    return subprocess.run(argv, capture_output=True, text=True, check=False, **kwargs)


def make_signer(work: Path) -> tuple[str, Path]:
    """A throwaway OpenPGP key: the fixture signs its own manifest, and the pin
    records the fingerprint the installer has to require."""
    home = work / 'signer'
    home.mkdir(mode=0o700)
    environment = dict(os.environ, GNUPGHOME=str(home), LC_ALL='C')
    result = run(['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback',
                  '--passphrase', '', '--quick-generate-key',
                  'fixture signer <fixture@example.invalid>', 'ed25519', 'sign', '0'],
                 env=environment)
    if result.returncode != 0:
        raise SystemExit(f'artifact-pins: cannot generate the fixture key: {result.stderr}')
    listed = run(['gpg', '--batch', '--with-colons', '--list-keys'], env=environment).stdout
    fingerprint = next(line.split(':')[9] for line in listed.splitlines()
                       if line.startswith('fpr:'))
    armor = run(['gpg', '--batch', '--armor', '--export', fingerprint], env=environment).stdout
    key = work / 'node-release-key.asc'
    key.write_text(armor)
    return fingerprint, key


def sign(home: Path, path: Path) -> Path:
    environment = dict(os.environ, GNUPGHOME=str(home), LC_ALL='C')
    result = run(['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
                  '--armor', '--yes', '--detach-sign', '--output', str(path) + '.sig', str(path)],
                 env=environment)
    if result.returncode != 0:
        raise SystemExit(f'artifact-pins: cannot sign the fixture manifest: {result.stderr}')
    return Path(str(path) + '.sig')


def add(tar: tarfile.TarFile, name: str, body: bytes, mode: int = 0o644) -> None:
    info = tarfile.TarInfo(name)
    info.size = len(body)
    info.mode = mode
    tar.addfile(info, io.BytesIO(body))


def add_dir(tar: tarfile.TarFile, name: str) -> None:
    info = tarfile.TarInfo(name)
    info.type = tarfile.DIRTYPE
    info.mode = 0o755
    tar.addfile(info)


def add_link(tar: tarfile.TarFile, name: str, target: str) -> None:
    info = tarfile.TarInfo(name)
    info.type = tarfile.SYMTYPE
    info.linkname = target
    info.mode = 0o777
    tar.addfile(info)


def add_hardlink(tar: tarfile.TarFile, name: str, target: str) -> None:
    info = tarfile.TarInfo(name)
    info.type = tarfile.LNKTYPE
    info.linkname = target
    info.mode = 0o755
    tar.addfile(info)


def npm_package(version: str) -> bytes:
    return json.dumps({'name': 'npm', 'version': version}).encode() + b'\n'


def node_archive(prefix: str, report: Path, compression: str,
                 npm_version: str = NPM_VERSION) -> bytes:
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode=f'w:{compression}', format=tarfile.GNU_FORMAT) as tar:
        add_dir(tar, prefix)
        add_dir(tar, prefix + 'bin/')
        add(tar, prefix + 'bin/node', f'#!/bin/sh\necho "v{VERSION}"\n'.encode(), 0o755)
        add_link(tar, prefix + 'bin/npm', '../lib/node_modules/npm/bin/npm-cli.js')
        add_link(tar, prefix + 'bin/npx', '../lib/node_modules/npm/bin/npx-cli.js')
        add_link(tar, prefix + 'bin/corepack', '../lib/node_modules/corepack/dist/corepack.js')
        add_dir(tar, prefix + 'lib/node_modules/npm/bin/')
        add(tar, prefix + 'lib/node_modules/npm/package.json', npm_package(npm_version))
        add(tar, prefix + 'lib/node_modules/npm/bin/npm-cli.js',
            f'#!/usr/bin/env node\nconsole.log("{VERSION}")\n'.encode(), 0o755)
        add(tar, prefix + 'lib/node_modules/npm/bin/npx-cli.js',
            f'#!/usr/bin/env node\nconsole.log("{VERSION}")\n'.encode(), 0o755)
        add_dir(tar, prefix + 'lib/node_modules/corepack/dist/')
        add(tar, prefix + 'lib/node_modules/corepack/dist/corepack.js', b'// corepack\n')
        add_dir(tar, prefix + 'include/node/')
        add(tar, prefix + 'include/node/node.h', b'/* fixture */\n')
        add_dir(tar, prefix + 'share/man/man1/')
        add(tar, prefix + 'share/man/man1/node.1', b'.TH NODE 1\n')
        for name in ('CHANGELOG.md', 'LICENSE', 'README.md'):
            add(tar, prefix + name, f'{name} fixture\n'.encode())
    return buffer.getvalue()


def gh_archive(prefix: str, report: Path) -> bytes:
    """The fixture `gh` records how the installer invoked it, so the case can
    assert the pinned policy was passed and that failure fails closed."""
    body = f'''#!/bin/sh
echo "$@" >> "{report}"
exit "${{FIXTURE_GH_STATUS:-0}}"
'''
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as tar:
        add_dir(tar, prefix)
        add_dir(tar, prefix + 'bin/')
        add(tar, prefix + 'bin/gh', body.encode(), 0o755)
        add_dir(tar, prefix + 'share/man/man1/')
        add(tar, prefix + 'share/man/man1/gh.1', b'.TH GH 1\n')
        add(tar, prefix + 'LICENSE', b'fixture license\n')
    return buffer.getvalue()


def hostile_archive(prefix: str, kind: str) -> bytes:
    buffer = io.BytesIO()
    # The reviewer's symlink chain: two links that each resolve lexically inside
    # the prefix, and a third member reached *through* the second one.
    if kind in ('symlink-chain', 'canonical-duplicate'):
        with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.PAX_FORMAT) as tar:
            add_dir(tar, prefix)
            add_dir(tar, prefix + 'bin/')
            add(tar, prefix + 'bin/gh', b'#!/bin/sh\nexit 0\n', 0o755)
            add_dir(tar, prefix + 'share/man/man1/')
            add(tar, prefix + 'share/man/man1/gh.1', b'.TH GH 1\n')
            if kind == 'symlink-chain':
                add_link(tar, prefix + 'bin/a', '..')
                add_link(tar, prefix + 'bin/b', 'a/../..')
                add_link(tar, prefix + 'bin/b/escaped', '.')
            else:
                # A second spelling of a path the archive already carries.
                add(tar, prefix + 'bin/./gh', b'#!/bin/sh\nexit 0\n', 0o755)
        return buffer.getvalue()
    if kind == 'hardlink-chain':
        # A two-hop hard link: h2 names h1, and h1 is itself a link.
        with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as tar:
            add_dir(tar, prefix)
            add_dir(tar, prefix + 'bin/')
            add(tar, prefix + 'bin/gh', b'#!/bin/sh\nexit 0\n', 0o755)
            add_hardlink(tar, prefix + 'bin/h1', prefix + 'bin/gh')
            add_hardlink(tar, prefix + 'bin/h2', prefix + 'bin/h1')
            add_dir(tar, prefix + 'share/man/man1/')
            add(tar, prefix + 'share/man/man1/gh.1', b'.TH GH 1\n')
        return buffer.getvalue()
    with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as tar:
        add_dir(tar, prefix)
        add_dir(tar, prefix + 'bin/')
        add(tar, prefix + 'bin/gh', b'#!/bin/sh\nexit 0\n',
            0o4755 if kind == 'setuid' else 0o755)
        add_dir(tar, prefix + 'share/man/man1/')
        add(tar, prefix + 'share/man/man1/gh.1', b'.TH GH 1\n')
        if kind == 'absolute':
            add(tar, '/tmp/fixture-escape', b'escape\n')
        elif kind == 'dotdot':
            add(tar, prefix + '../escape', b'escape\n')
        elif kind == 'symlink':
            add_link(tar, prefix + 'bin/escape', '../../../etc/passwd')
        elif kind == 'device':
            info = tarfile.TarInfo(prefix + 'dev/null')
            info.type = tarfile.CHRTYPE
            info.devmajor, info.devminor = 1, 3
            tar.addfile(info)
        elif kind == 'duplicate':
            add(tar, prefix + 'bin/gh', b'#!/bin/sh\nexit 0\n', 0o755)
        elif kind in ('setuid',):
            pass
        elif kind == 'file-directory-conflict':
            add(tar, prefix + 'bin/gh/inside', b'conflict\n')
        elif kind == 'outside-prefix':
            add(tar, 'other-root/bin/gh', b'#!/bin/sh\nexit 0\n', 0o755)
        else:
            raise SystemExit(f'artifact-pins: unknown hostile member {kind}')
    return buffer.getvalue()


class Fixture:
    """The served release tree plus the two pins that name it.

    The layout mirrors the upstream one - `dist/v<version>/<file>` for Node and
    `<file>` for the GitHub CLI release - because the pin's URL has to carry the
    version and the exact file name the member prefix derives from.
    """

    def __init__(self, work: Path) -> None:
        self.work = work
        self.artifacts = work / 'artifacts'
        self.artifacts.mkdir(parents=True)
        self.releases = work / 'releases'
        self.dist = self.releases / f'v{VERSION}'
        self.dist.mkdir(parents=True)
        self.gh_log = work / 'gh-invocations.log'
        self.fingerprint, key = make_signer(work)
        self.key = self.artifacts / 'node-release-key.asc'
        shutil.copyfile(key, self.key)
        self.node_bytes = {platform: node_archive(prefix, self.gh_log, 'xz')
                           for platform, prefix in NODE_PREFIX.items()}
        self.gh_bytes = {platform: gh_archive(prefix, self.gh_log)
                         for platform, prefix in GH_PREFIX.items()}
        for platform, raw in self.node_bytes.items():
            self.node_path(platform).write_bytes(raw)
        for platform, raw in self.gh_bytes.items():
            self.gh_path(platform).write_bytes(raw)
        self.node_manifest = self.dist / 'SHASUMS256.txt'
        self.node_manifest.write_text(''.join(
            f'{sha256(raw)}  {node_name(platform)}\n'
            for platform, raw in self.node_bytes.items()))
        self.node_signature = sign(work / 'signer', self.node_manifest)
        self.gh_checksums = self.dist / f'gh_{VERSION}_checksums.txt'
        self.gh_checksums.write_text(''.join(
            f'{sha256(raw)}  {gh_name(platform)}\n'
            for platform, raw in self.gh_bytes.items()))
        self.bundle = {platform: (self.artifacts
                                  / artifact_pin.bundle_name('gh', VERSION, platform))
                       for platform in GH_PREFIX}
        for path in self.bundle.values():
            path.write_text('{"fixture":"attestation bundle"}\n')
        self.write_pins()

    def node_path(self, platform: str) -> Path:
        return self.dist / node_name(platform)

    def gh_path(self, platform: str) -> Path:
        return self.dist / gh_name(platform)

    def url(self, path: Path) -> str:
        return path.as_uri()

    def bundle_url(self, platform: str) -> str:
        return self.bundle[platform].as_uri()

    def write_pins(self) -> None:
        self.write_node()
        self.write_gh()

    def restore(self) -> None:
        """Back to the reviewed fixture tree: the served bytes and both pins, so
        one case cannot leak into the next."""
        for platform, raw in self.node_bytes.items():
            self.node_path(platform).write_bytes(raw)
        for platform, raw in self.gh_bytes.items():
            self.gh_path(platform).write_bytes(raw)
        self.write_pins()

    def write_node(self, tamper=None) -> None:
        pin = {
            'schema': 1, 'name': 'node', 'kind': 'release-archive', 'version': VERSION,
            'upstream': {'project': 'https://nodejs.org/dist', 'release': f'v{VERSION}'},
            'install': {
                'destination': '/usr/local',
                'payload': list(artifact_pin.NODE_PAYLOAD),
                'links': [dict(link) for link in artifact_pin.NODE_LINKS],
            },
            'bundled': {'npm': NPM_VERSION},
            'platforms': {
                platform: {
                    'url': self.url(self.node_path(platform)),
                    'archive': 'tar.xz',
                    'member_prefix': NODE_PREFIX[platform],
                    'sha256': sha256(raw),
                } for platform, raw in self.node_bytes.items()
            },
            'authentication': {
                'method': 'openpgp-signed-manifest',
                'manifest': {'url': self.url(self.node_manifest),
                             'sha256': sha256(self.node_manifest.read_bytes())},
                'signature': {'url': self.url(self.node_signature),
                              'sha256': sha256(self.node_signature.read_bytes())},
                'key': {'path': 'node-release-key.asc', 'fingerprint': self.fingerprint,
                        'sha256': sha256(self.key.read_bytes())},
                'verified_on': '2026-01-01',
                'reason': 'fixture',
            },
        }
        if tamper:
            tamper(pin)
        (self.artifacts / 'node.json').write_text(json.dumps(pin, indent=2) + '\n')

    def write_gh(self, tamper=None) -> None:
        pin = {
            'schema': 1, 'name': 'gh', 'kind': 'release-archive', 'version': VERSION,
            'upstream': {'project': 'https://github.com/cli/cli', 'release': f'v{VERSION}'},
            'install': {'destination': '/usr/local', 'payload': list(artifact_pin.GH_PAYLOAD),
                        'links': []},
            'platforms': {
                platform: {
                    'url': self.url(self.gh_path(platform)),
                    'archive': 'tar.gz',
                    'member_prefix': GH_PREFIX[platform],
                    'sha256': sha256(raw),
                    'bundle': {'path': artifact_pin.bundle_name('gh', VERSION, platform),
                               'sha256': sha256(self.bundle[platform].read_bytes())},
                } for platform, raw in self.gh_bytes.items()
            },
            'authentication': {
                'method': 'sigstore-attestation',
                'checksums': {
                    'url': 'https://github.com/cli/cli/releases/download/'
                           f'v{VERSION}/gh_{VERSION}_checksums.txt',
                    'sha256': sha256(self.gh_checksums.read_bytes())},
                'policy': dict(GH_POLICY),
                'verified_on': '2026-01-01',
                'reason': 'fixture',
                'evidence': ['https://github.com/cli/cli/attestations'],
            },
        }
        if tamper:
            tamper(pin)
        (self.artifacts / 'gh.json').write_text(json.dumps(pin, indent=2) + '\n')


def node_name(platform: str) -> str:
    return f'node-v{VERSION}-{artifact_pin.platform_token("node", platform)}.tar.xz'


def gh_name(platform: str) -> str:
    return f'gh_{VERSION}_{artifact_pin.platform_token("gh", platform)}.tar.gz'


# --- running the installer --------------------------------------------------

def install(fixture: Fixture, name: str, arch: str, root: Path,
            environment: dict | None = None, events: Path | None = None,
            images: Path | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ, ARTIFACT_SOURCE_DIR=str(images or ROOT / 'images'))
    env.pop('FIXTURE_GH_STATUS', None)
    env.update(environment or {})
    # Each run starts with an empty verifier log, so a case can tell whether the
    # installed verifier ran at all.
    fixture.gh_log.unlink(missing_ok=True)
    argv = ['bash', str(INSTALLER), name, str(fixture.artifacts / f'{name}.json'), arch,
            '--root', str(root)]
    if events is not None:
        argv.extend(['--events', str(events)])
    return run(argv, env=env)


def expect_refusal(label: str, result: subprocess.CompletedProcess, needle: str,
                   root: Path, fixture: Fixture) -> None:
    if result.returncode == 0:
        fail(label, f'installed instead of refusing: {result.stderr.strip()}')
        return
    if needle not in (result.stderr + result.stdout):
        fail(label, f'refusal does not mention {needle!r}: {result.stderr.strip()}')
        return
    leftovers = [path for path in (root / 'usr').rglob('*')] if (root / 'usr').exists() else []
    if leftovers:
        fail(label, f'nothing may be published on a refusal, found {leftovers[:4]}')
        return
    if fixture.gh_log.exists():
        fail(label, 'the installed verifier must not run when the pin refuses first')
        return
    report(label)


def graph(root: Path) -> list[str]:
    return sorted(str(path.relative_to(root)) for path in root.rglob('*'))


def case_happy_path(work: Path, fixture: Fixture) -> None:
    root = work / 'happy'
    root.mkdir()
    for name, arch in (('node', 'amd64'), ('node', 'arm64'), ('gh', 'amd64'), ('gh', 'arm64')):
        result = install(fixture, name, arch, root if arch == 'amd64' else work / f'happy-{arch}')
        if result.returncode != 0:
            fail(f'{name} {arch} installs', result.stderr.strip())
            return
    if (root / 'usr/local/bin/node').stat().st_mode & 0o111 == 0:
        fail('the node binary is executable', oct((root / 'usr/local/bin/node').stat().st_mode))
        return
    link = root / 'usr/bin/node'
    if not link.is_symlink() or os.readlink(link) != '/usr/local/bin/node':
        fail('/usr/bin/node', f'expected an absolute link, found {link.is_symlink()}')
        return
    if not (root / 'usr/local/lib/node_modules/npm/bin/npm-cli.js').is_file():
        fail('the bundled npm is published', 'lib/node_modules/npm is missing')
        return
    if not (root / 'usr/local/share/man/man1/node.1').is_file():
        fail('the node man page is published', 'share/man/man1/node.1 is missing')
        return
    if (root / 'usr/local/LICENSE').read_bytes() != b'LICENSE fixture\n':
        fail('the node payload is the whole archive', 'LICENSE differs')
        return
    invocations = fixture.gh_log.read_text().splitlines()
    if len(invocations) != 1 or 'attestation verify' not in invocations[0]:
        fail('the installed verifier runs once', str(invocations))
        return
    for flag in ('--bundle', '--repo cli/cli',
                 '--signer-workflow cli/cli/.github/workflows/deployment.yml',
                 '--cert-oidc-issuer https://token.actions.githubusercontent.com',
                 '--deny-self-hosted-runners'):
        if flag not in invocations[0]:
            fail('the verifier receives the pinned policy', f'{flag} missing from {invocations[0]}')
            return
    report('both artifacts install from the committed evidence and publish the declared payload')


def case_changed_bytes(work: Path, fixture: Fixture) -> None:
    """The signature chain passes - the manifest is the reviewed one - and the
    archive the origin serves is not the bytes the signed line names."""
    original = fixture.node_path('linux/amd64').read_bytes()
    fixture.node_path('linux/amd64').write_bytes(original[:-16] + b'changed bytes!!\n')
    root = work / 'changed-bytes'
    root.mkdir()
    expect_refusal('changed bytes are refused before extraction',
                   install(fixture, 'node', 'amd64', root), 'the pin says', root, fixture)
    fixture.restore()


def case_wrong_architecture(work: Path, fixture: Fixture) -> None:
    """The arm64 payload served under the amd64 name, with a manifest the reviewed
    key signs over its digest: every hash agrees, so only the archive's own
    top-level prefix gives the substitution away."""
    arm = fixture.node_bytes['linux/arm64']
    fixture.node_path('linux/amd64').write_bytes(arm)
    fixture.node_manifest.write_text(''.join(
        f'{sha256(arm) if platform == "linux/amd64" else sha256(raw)}  {node_name(platform)}\n'
        for platform, raw in fixture.node_bytes.items()))
    signature = sign(work / 'signer', fixture.node_manifest)

    def tamper(pin):
        pin['platforms']['linux/amd64']['sha256'] = sha256(arm)
        pin['authentication']['manifest']['sha256'] = sha256(
            fixture.node_manifest.read_bytes())
        pin['authentication']['signature']['sha256'] = sha256(signature.read_bytes())

    fixture.write_node(tamper)
    root = work / 'wrong-arch'
    root.mkdir()
    expect_refusal('a consistent hash over the wrong architecture is refused',
                   install(fixture, 'node', 'amd64', root), 'outside', root, fixture)
    fixture.node_manifest.write_text(''.join(
        f'{sha256(raw)}  {node_name(platform)}\n'
        for platform, raw in fixture.node_bytes.items()))
    sign(work / 'signer', fixture.node_manifest)
    fixture.restore()


def case_pin_refusals(work: Path, fixture: Fixture) -> None:
    cases = {
        'wrong version in the url': (
            lambda pin: pin['upstream'].update({'release': 'v1.0.0'}),
            'upstream release'),
        'unknown field': (
            lambda pin: pin.update({'latest': True}),
            'unknown'),
        'deleted authentication': (
            lambda pin: pin.pop('authentication'),
            'missing authentication'),
        'downgraded authentication': (
            lambda pin: pin['authentication'].update({'method': 'hash-only'}),
            'openpgp-signed-manifest'),
        'missing platform': (
            lambda pin: pin['platforms'].pop('linux/arm64'),
            'linux/arm64'),
        'payload escaping the destination': (
            lambda pin: pin['install'].update({'payload': ['../escape']}),
            'payload'),
    }
    for index, (label, (tamper, needle)) in enumerate(cases.items()):
        fixture.write_node(tamper)
        root = work / f'pin-{index}'
        root.mkdir()
        expect_refusal(label, install(fixture, 'node', 'amd64', root), needle, root, fixture)
    fixture.write_pins()
    # The URL has to name the exact release and file the prefix derives from; a
    # version that disagrees with the pin is refused before anything is fetched.
    fixture.write_node(lambda pin: pin['platforms']['linux/amd64'].update(
        {'url': fixture.url(fixture.node_path('linux/arm64')).replace(VERSION, '9.9.8')}))
    root = work / 'pin-version'
    root.mkdir()
    expect_refusal('a url that does not name the pinned release is refused',
                   install(fixture, 'node', 'amd64', root), 'must name', root, fixture)
    fixture.write_pins()


def case_bad_signature(work: Path, fixture: Fixture) -> None:
    signature = fixture.node_signature.read_bytes()
    # Inside the base64 body: an edited armour trailer can still verify.
    index = signature.index(b'\n\n') + 24
    replacement = b'A' if signature[index:index + 1] != b'A' else b'B'
    tampered = signature[:index] + replacement + signature[index + 1:]

    def tamper(pin):
        pin['authentication']['signature']['sha256'] = sha256(tampered)

    fixture.node_signature.write_bytes(tampered)
    fixture.write_node(tamper)
    root = work / 'bad-signature'
    root.mkdir()
    expect_refusal('a tampered signature is refused',
                   install(fixture, 'node', 'amd64', root), 'does not verify', root, fixture)
    fixture.node_signature.write_bytes(signature)
    fixture.write_pins()


def case_attacker_key(work: Path, fixture: Fixture) -> None:
    # An attacker-signed manifest with a consistent manifest/signature hash and
    # the attacker's own key: only the recorded fingerprint separates them.
    attacker_home = work / 'attacker'
    attacker_home.mkdir(mode=0o700)
    environment = dict(os.environ, GNUPGHOME=str(attacker_home), LC_ALL='C')
    run(['gpg', '--batch', '--quiet', '--pinentry-mode', 'loopback', '--passphrase', '',
         '--quick-generate-key', 'attacker <attacker@example.invalid>', 'ed25519', 'sign', '0'],
        env=environment)
    listed = run(['gpg', '--batch', '--with-colons', '--list-keys'], env=environment).stdout
    attacker_fingerprint = next(line.split(':')[9] for line in listed.splitlines()
                                if line.startswith('fpr:'))
    attacker_copy = work / 'attacker-manifest.txt'
    attacker_copy.write_bytes(fixture.node_manifest.read_bytes())
    attacker_signature = sign(attacker_home, attacker_copy)
    committed_key = (fixture.artifacts / 'node-release-key.asc').read_bytes()
    attacker_key = run(['gpg', '--batch', '--armor', '--export', attacker_fingerprint],
                       env=environment).stdout.encode()
    (fixture.artifacts / 'node-release-key.asc').write_bytes(attacker_key)
    committed_signature = fixture.node_signature.read_bytes()
    fixture.node_signature.write_bytes(attacker_signature.read_bytes())

    def tamper(pin):
        pin['authentication']['signature']['sha256'] = sha256(
            fixture.node_signature.read_bytes())
        pin['authentication']['key']['sha256'] = sha256(attacker_key)

    fixture.write_node(tamper)
    root = work / 'attacker-key'
    root.mkdir()
    expect_refusal('a substituted key with a consistent hash is refused by fingerprint',
                   install(fixture, 'node', 'amd64', root), 'not the pinned', root, fixture)
    (fixture.artifacts / 'node-release-key.asc').write_bytes(committed_key)
    fixture.node_signature.write_bytes(committed_signature)
    fixture.write_pins()


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    """A 404 is the expected answer in this suite, so the access log is noise."""

    def log_message(self, *args) -> None:  # noqa: D401 - signature inherited
        pass


def server(directory: Path) -> tuple[http.server.ThreadingHTTPServer, str]:
    def handler(*args, **kwargs) -> QuietHandler:
        return QuietHandler(*args, directory=str(directory), **kwargs)

    httpd = http.server.ThreadingHTTPServer(('127.0.0.1', 0), handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    return httpd, f'http://127.0.0.1:{httpd.server_address[1]}'


def case_missing_asset(work: Path, fixture: Fixture) -> None:
    """A pin whose asset is absent: the install refuses before anything is
    published. The absent file is what a `file:` origin can express."""
    fixture.write_node(lambda pin: pin['platforms']['linux/amd64'].update(
        {'url': (fixture.dist / 'absent' / node_name('linux/amd64')).as_uri()}))
    root = work / 'missing'
    root.mkdir()
    expect_refusal('a missing asset is refused',
                   install(fixture, 'node', 'amd64', root), 'cannot fetch', root, fixture)
    fixture.write_pins()


def case_transport(work: Path, fixture: Fixture) -> None:
    """The two HTTP failure modes, through the fetch primitive every download in
    the installer uses. A committed pin may only name `https` (the gate enforces
    the upstream origins), so a local origin cannot appear in one; the transport
    is what this case pins down."""
    import install_artifact  # the installer's own module, not a copy
    served = work / 'served'
    served.mkdir()
    httpd, base = server(served)
    try:
        try:
            install_artifact.fetch(f'{base}/absent.tar.gz', 1024)
        except install_artifact.Refusal as error:
            if 'HTTP 404' not in str(error):
                fail('a 404 is refused', str(error))
            else:
                report('a missing asset (HTTP 404) is refused')
        else:
            fail('a 404 is refused', 'the fetch succeeded')
    finally:
        httpd.shutdown()
        httpd.server_close()
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', 0))
        port = probe.getsockname()[1]
    try:
        install_artifact.fetch(f'http://127.0.0.1:{port}/node.tar.xz', 1024)
    except install_artifact.Refusal as error:
        if 'cannot fetch' not in str(error):
            fail('a refused connection is refused', str(error))
        else:
            report('an unreachable endpoint (connection refused) is refused')
    else:
        fail('a refused connection is refused', 'the fetch succeeded')


def case_hostile_members(work: Path, fixture: Fixture) -> None:
    expectations = {
        'absolute': 'unusable member name',
        'dotdot': 'escapes its prefix',
        'symlink': 'links outside the archive',
        'symlink-chain': 'write through the link',
        'hardlink-chain': 'which is not a file',
        'canonical-duplicate': 'not a canonical path',
        'file-directory-conflict': 'under the file',
        'device': 'not a file, directory or link',
        'duplicate': 'repeats a member',
        'setuid': 'setuid',
        'outside-prefix': 'outside',
    }
    for index, (kind, needle) in enumerate(expectations.items()):
        prefix = GH_PREFIX['linux/amd64']
        raw = hostile_archive(prefix, kind)
        fixture.gh_path('linux/amd64').write_bytes(raw)
        fixture.write_gh(lambda pin: pin['platforms']['linux/amd64'].update({'sha256': sha256(raw)}))
        root = work / f'hostile-{index}'
        root.mkdir()
        expect_refusal(f'hostile archive member: {kind}',
                       install(fixture, 'gh', 'amd64', root), needle, root, fixture)
    fixture.gh_path('linux/amd64').write_bytes(fixture.gh_bytes['linux/amd64'])
    fixture.write_pins()


def case_attestation(work: Path, fixture: Fixture) -> None:
    root = work / 'attestation-fail'
    root.mkdir()
    result = install(fixture, 'gh', 'amd64', root,
                     environment={'FIXTURE_GH_STATUS': '1'})
    if result.returncode == 0:
        fail('a failing attestation fails the install', 'exit 0')
        return
    if 'does not verify the archive' not in result.stderr:
        fail('a failing attestation fails closed', result.stderr.strip())
        return
    report('a failing attestation bundle fails the install closed')

    bundle = fixture.bundle['linux/amd64']
    original = bundle.read_bytes()
    bundle.write_bytes(original + b'{"tampered":true}\n')
    root = work / 'attestation-bundle'
    root.mkdir()
    expect_refusal('a changed committed bundle is refused before extraction',
                   install(fixture, 'gh', 'amd64', root), 'the pin says', root, fixture)
    bundle.write_bytes(original)


def case_no_fallback(work: Path, fixture: Fixture) -> None:
    """A mismatch must not reach apt, npm, a package tool or a re-download, and the
    archive must never be extracted."""
    tools = work / 'fallback-tools'
    tools.mkdir()
    marker = work / 'fallback-used'
    for tool in FALLBACK_TOOLS:
        script = tools / tool
        script.write_text(f'#!/bin/sh\necho {tool} >> "{marker}"\nexit 1\n')
        script.chmod(0o755)
    environment = {'PATH': f'{tools}{os.pathsep}{os.environ.get("PATH", "")}'}
    original = fixture.node_path('linux/amd64').read_bytes()
    fixture.node_path('linux/amd64').write_bytes(original[:-16] + b'changed bytes!!\n')
    root = work / 'no-fallback'
    root.mkdir()
    result = install(fixture, 'node', 'amd64', root, environment=environment)
    expect_refusal('a mismatch installs nothing', result, 'the pin says', root, fixture)
    if marker.exists():
        fail('no fallback tool is invoked', marker.read_text())
        return
    report('a failure reaches no apt/npm/curl fallback and never starts extraction')
    fixture.restore()


def case_url_mutations(work: Path, fixture: Fixture) -> None:
    """The pin's URL is *computed*, not matched by prefix or substring.

    Every row here used to pass both validators while naming bytes from
    somewhere else: `..` segments GitHub resolves into another repository, a
    lookalike host, an extra path segment, a query string and a different
    repository entirely.
    """
    node_name_amd64 = node_name('linux/amd64')
    gh_name_amd64 = gh_name('linux/amd64')
    rows = [
        ('node', 'linux/amd64', 'traversal',
         f'https://nodejs.org/dist/v{VERSION}/../../../attacker/repo/dist/v{VERSION}/'
         + node_name_amd64, 'must be exactly'),
        ('node', 'linux/amd64', 'lookalike host',
         f'https://nodejs.org.evil/dist/v{VERSION}/' + node_name_amd64, 'must be exactly'),
        ('node', 'linux/amd64', 'extra path segment',
         f'https://nodejs.org/dist/v{VERSION}/extra/' + node_name_amd64, 'must be exactly'),
        ('node', 'linux/amd64', 'query string',
         f'https://nodejs.org/dist/v{VERSION}/' + node_name_amd64 + '?raw=1', 'must be exactly'),
        ('node', 'linux/amd64', 'another repository',
         f'https://github.com/cli/cli/releases/download/v{VERSION}/' + node_name_amd64,
         'must be exactly'),
        ('node', 'linux/amd64', 'file transport traversal',
         (fixture.dist / 'sub' / '..' / node_name_amd64).as_uri(), 'in a plain directory'),
        ('gh', 'linux/amd64', 'traversal',
         'https://github.com/cli/cli/releases/download/../../attacker/repo/releases/'
         f'download/v{VERSION}/' + gh_name_amd64, 'must be exactly'),
        ('gh', 'linux/amd64', 'lookalike host',
         f'https://github.com.evil/cli/cli/releases/download/v{VERSION}/' + gh_name_amd64,
         'must be exactly'),
        ('gh', 'linux/amd64', 'extra path segment',
         f'https://github.com/cli/cli/releases/download/v{VERSION}/extra/' + gh_name_amd64,
         'must be exactly'),
        ('gh', 'linux/amd64', 'query string',
         f'https://github.com/cli/cli/releases/download/v{VERSION}/' + gh_name_amd64 + '?raw=1',
         'must be exactly'),
        ('gh', 'linux/amd64', 'another repository',
         f'https://github.com/attacker/repo/releases/download/v{VERSION}/' + gh_name_amd64,
         'must be exactly'),
    ]
    for index, (name, platform, label, url, needle) in enumerate(rows):
        def tamper(pin, url=url, platform=platform):
            pin['platforms'][platform]['url'] = url
        (fixture.write_node if name == 'node' else fixture.write_gh)(tamper)
        root = work / f'url-{index}'
        root.mkdir()
        expect_refusal(f'{name}: {label} in the pin url is refused',
                       install(fixture, name, 'amd64', root), needle, root, fixture)
    fixture.write_pins()


def case_destination_ancestor(work: Path, fixture: Fixture) -> None:
    """`--root` is where publication lands, so a symlinked ancestor inside it is
    a redirection, not a layout: `root/usr/local` pointing elsewhere used to make
    the installer write outside the root it was given."""
    root = work / 'ancestor'
    elsewhere = work / 'ancestor-target'
    (root / 'usr').mkdir(parents=True)
    elsewhere.mkdir()
    (root / 'usr/local').symlink_to(elsewhere)
    result = install(fixture, 'node', 'amd64', root)
    if result.returncode == 0:
        fail('a symlinked destination ancestor is refused', 'the install succeeded')
        return
    if 'symlinked directory' not in result.stderr:
        fail('a symlinked destination ancestor is refused', result.stderr.strip())
        return
    written = sorted(str(path.relative_to(elsewhere)) for path in elsewhere.iterdir())
    if written:
        fail('nothing is written through the link', str(written))
        return
    report('publication refuses a symlinked destination ancestor')


def case_partial_publication(work: Path, fixture: Fixture) -> None:
    """A conflict at the *second* published path used to leave the first one
    installed, so the precheck has to happen before any write."""
    root = work / 'partial'
    (root / 'usr/local/share/man/man1').mkdir(parents=True)
    (root / 'usr/local/share/man/man1/gh.1').write_text('another input owns this\n')
    result = install(fixture, 'gh', 'amd64', root)
    if result.returncode == 0:
        fail('a publication conflict is refused', 'the install succeeded')
        return
    if 'refusing to replace an installed path' not in result.stderr:
        fail('a publication conflict is refused', result.stderr.strip())
        return
    bin_dir = root / 'usr/local/bin'
    leftovers = sorted(path.name for path in bin_dir.glob('*')) if bin_dir.exists() else []
    if leftovers:
        fail('a refused publication leaves nothing published', str(leftovers))
        return
    report('a later publication conflict publishes nothing')


def case_attestation_before_publication(work: Path, fixture: Fixture) -> None:
    """The verifier is the artifact, so it runs from the staged tree: a failed
    attestation must not leave a published `gh` behind."""
    root = work / 'attestation-staged'
    root.mkdir()
    result = install(fixture, 'gh', 'amd64', root, environment={'FIXTURE_GH_STATUS': '1'})
    if result.returncode == 0:
        fail('a failed attestation publishes nothing', 'exit 0')
        return
    leftovers = sorted(str(path.relative_to(root)) for path in root.rglob('*')
                       if path.is_file() or path.is_symlink())
    if leftovers:
        fail('a failed attestation publishes nothing', str(leftovers))
        return
    report('the attestation is verified from staging, before publication')


def case_bundled_npm(work: Path, fixture: Fixture) -> None:
    """The pin records the npm version the reviewed archive ships, so an archive
    that carries a different npm is refused instead of silently relabelled."""
    raw = node_archive(NODE_PREFIX['linux/amd64'], fixture.gh_log, 'xz', npm_version='9.8.6')
    fixture.node_path('linux/amd64').write_bytes(raw)
    fixture.node_manifest.write_text(''.join(
        f'{sha256(raw) if platform == "linux/amd64" else sha256(served)}  {node_name(platform)}\n'
        for platform, served in fixture.node_bytes.items()))
    signature = sign(work / 'signer', fixture.node_manifest)

    def tamper(pin):
        pin['platforms']['linux/amd64']['sha256'] = sha256(raw)
        pin['authentication']['manifest']['sha256'] = sha256(
            fixture.node_manifest.read_bytes())
        pin['authentication']['signature']['sha256'] = sha256(signature.read_bytes())

    fixture.write_node(tamper)
    root = work / 'bundled'
    root.mkdir()
    expect_refusal('an archive whose bundled npm differs from the pin is refused',
                   install(fixture, 'node', 'amd64', root), 'the pin records', root, fixture)
    fixture.node_manifest.write_text(''.join(
        f'{sha256(served)}  {node_name(platform)}\n'
        for platform, served in fixture.node_bytes.items()))
    sign(work / 'signer', fixture.node_manifest)
    fixture.restore()


# --- agy: the hash-only artifact ---------------------------------------------

AGY_MACHINES = {'linux/amd64': 62, 'linux/arm64': 183}
AGY_RELEASE_JSON = 'evidence/release.json'


def elf_header(machine: int, elf_class: int = 2) -> bytes:
    """The first 64 bytes of an ELF binary for one machine, which is all the
    installer reads before publishing."""
    header = bytearray(64)
    header[0:4] = b'\x7fELF'
    header[4] = elf_class
    header[5] = 1  # little-endian
    header[6] = 1  # EV_CURRENT
    header[18:20] = machine.to_bytes(2, 'little')
    return bytes(header)


def agy_archive(body: bytes = None) -> bytes:
    """The reviewed shape: one prefixless executable named after the project."""
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as tar:
        add(tar, 'antigravity', body if body is not None else elf_header(62), 0o755)
    return buffer.getvalue()


def agy_hostile(kind: str) -> bytes:
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as tar:
        if kind == 'extra-member':
            add(tar, 'antigravity', elf_header(62), 0o755)
            add(tar, 'antigravity-cli', elf_header(62), 0o755)
        elif kind == 'non-canonical':
            add(tar, './antigravity', elf_header(62), 0o755)
        elif kind == 'prefixed':
            add(tar, 'agy/antigravity', elf_header(62), 0o755)
        elif kind == 'symlink':
            add_link(tar, 'antigravity', '../../etc/passwd')
        elif kind == 'setuid':
            add(tar, 'antigravity', elf_header(62), 0o4755)
        elif kind == 'not-executable':
            add(tar, 'antigravity', elf_header(62), 0o644)
        elif kind.startswith('mode-'):
            # A permission mode the review did not select. The digest covers the
            # mode too, so the fixture is internally consistent: only the
            # member-mode check separates `-rwxrwxrwx /usr/local/bin/agy` from the
            # reviewed 0755.
            add(tar, 'antigravity', elf_header(62), int(kind.split('-', 1)[1], 8))
        else:
            raise SystemExit(f'artifact-pins: unknown agy archive {kind}')
    return buffer.getvalue()


class AgyFixture:
    """The committed corroboration layout of the hash-only artifact.

    `pin.json` and `evidence/` are the two records a build re-checks against each
    other: GitHub's release digests and Google's per-architecture updater
    manifests. The served archives are `file:` fixtures, but the evidence URLs and
    the pin's archive URLs are the computed upstream ones.
    """

    def __init__(self, work: Path) -> None:
        self.work = work
        self.directory = work / 'agy'
        self.evidence = self.directory / 'evidence'
        self.evidence.mkdir(parents=True)
        self.releases = work / 'agy-releases'
        self.releases.mkdir()
        self.bytes = {platform: agy_archive(elf_header(machine))
                      for platform, machine in AGY_MACHINES.items()}
        self.original = dict(self.bytes)
        for platform, raw in self.bytes.items():
            self.path(platform).write_bytes(raw)
        self.write_evidence()
        self.write_pin()

    def filename(self, platform: str) -> str:
        return artifact_pin.expected_filename('agy', VERSION, platform)

    def path(self, platform: str) -> Path:
        return self.releases / self.filename(platform)

    def url(self, platform: str) -> str:
        return self.path(platform).as_uri()

    def manifest_name(self, platform: str) -> str:
        return artifact_pin.AGY_EVIDENCE[platform]

    def write_evidence(self) -> None:
        assets = [{'name': self.filename(platform),
                   'size': len(raw),
                   'digest': 'sha256:' + sha256(raw),
                   'uploader': {'login': 'fixture'},
                   'download_count': 4102}
                  for platform, raw in self.bytes.items()]
        release = {'tag_name': VERSION, 'draft': False, 'prerelease': False,
                   'immutable': False, 'assets': assets}
        (self.directory / AGY_RELEASE_JSON).write_text(json.dumps(release, indent=2) + '\n')
        for platform, raw in self.bytes.items():
            manifest = {'version': VERSION,
                        'url': f'https://storage.googleapis.com/fixture/{self.filename(platform)}',
                        'sha512': artifact_pin.sha512(raw)}
            (self.directory / self.manifest_name(platform)).write_text(
                json.dumps(manifest, indent=2) + '\n')

    def write_pin(self, tamper=None) -> None:
        pin = {
            'schema': 1, 'name': 'agy', 'kind': 'release-archive', 'version': VERSION,
            'upstream': {'project': f'https://github.com/{artifact_pin.AGY_REPOSITORY}',
                         'release': artifact_pin.release_tag('agy', VERSION)},
            'install': {'destination': '/usr/local',
                        'payload': [{'member': member, 'path': path}
                                    for member, path in artifact_pin.AGY_PAYLOAD],
                        'links': []},
            'platforms': {
                platform: {
                    'url': self.url(platform),
                    'archive': 'tar.gz',
                    'member_prefix': '',
                    'sha256': sha256(raw),
                    'sha512': artifact_pin.sha512(raw),
                } for platform, raw in self.bytes.items()
            },
            'authentication': {
                'method': 'hash-only',
                'release': {'url': artifact_pin.AGY_RELEASE_API + VERSION,
                            'path': AGY_RELEASE_JSON,
                            'sha256': sha256((self.directory / AGY_RELEASE_JSON).read_bytes())},
                'manifests': {
                    platform: {'url': artifact_pin.manifest_url(platform),
                               'path': self.manifest_name(platform),
                               'sha256': sha256(
                                   (self.directory / self.manifest_name(platform)).read_bytes())}
                    for platform in AGY_MACHINES
                },
                'verified_on': '2026-10-30',
                'reason': 'fixture',
            },
        }
        if tamper:
            tamper(pin)
        (self.directory / 'pin.json').write_text(json.dumps(pin, indent=2) + '\n')

    def restore(self) -> None:
        self.bytes = dict(self.original)
        for platform, raw in self.bytes.items():
            self.path(platform).write_bytes(raw)
        self.write_evidence()
        self.write_pin()


def install_agy(fixture: AgyFixture, root: Path, arch: str = 'amd64', *,
                pin: Path | None = None, events: Path | None = None,
                environment: dict | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ, ARTIFACT_SOURCE_DIR=str(ROOT / 'images'))
    env.update(environment or {})
    argv = ['bash', str(INSTALLER), 'agy', str(pin or fixture.directory / 'pin.json'), arch,
            '--root', str(root)]
    if events is not None:
        argv.extend(['--events', str(events)])
    return run(argv, env=env)


def events_of(log: Path) -> list[str]:
    return [line.split(' ', 1)[0] for line in log.read_text().splitlines() if line.strip()]


def expect_agy_refusal(label: str, result: subprocess.CompletedProcess, needle: str,
                       root: Path) -> None:
    """A refusal, its message, and — the rule this suite holds every row to —
    nothing published: a check placed after publication cannot pass."""
    if result.returncode == 0:
        fail(label, f'installed instead of refusing: {result.stderr.strip()}')
        return
    if needle not in (result.stderr + result.stdout):
        fail(label, f'refusal does not mention {needle!r}: {result.stderr.strip()}')
        return
    published = root / 'usr/local/bin/agy'
    if published.exists() or (root / 'usr').exists() and any((root / 'usr').rglob('*')):
        fail(label, f'nothing may be published on a refusal, found {published}')
        return
    report(label)


def case_agy_install(work: Path, fixture: AgyFixture) -> None:
    for arch, platform in (('amd64', 'linux/amd64'), ('arm64', 'linux/arm64')):
        root = work / f'agy-happy-{arch}'
        root.mkdir()
        log = work / f'agy-happy-{arch}.log'
        result = install_agy(fixture, root, arch, events=log)
        if result.returncode != 0:
            fail(f'agy {arch} installs', result.stderr.strip())
            return
        published = root / 'usr/local/bin/agy'
        if not published.is_file():
            fail(f'agy {arch} publishes /usr/local/bin/agy', graph(root))
            return
        if published.read_bytes() != elf_header(AGY_MACHINES[platform]):
            fail(f'agy {arch} publishes the reviewed bytes', 'the published bytes differ')
            return
        if published.stat().st_mode & 0o777 != 0o755:
            fail(f'agy {arch} publishes mode 0755', oct(published.stat().st_mode))
            return
        if (root / 'usr/local/bin/antigravity').exists():
            fail('agy publishes no antigravity alias', graph(root))
            return
        if graph(root) != ['usr', 'usr/local', 'usr/local/bin', 'usr/local/bin/agy']:
            fail(f'agy {arch} publishes exactly one path', graph(root))
            return
        entries = [tuple(line.split(' ', 1)) for line in log.read_text().splitlines()
                   if line.strip()]
        kinds = [kind for kind, _ in entries]
        if kinds != ['verify', 'fetch', 'extract', 'publish']:
            fail('agy: corroboration, then one fetch, then extraction, then publication',
                 str(entries))
            return
        if entries[0][1] != 'corroboration':
            fail('agy verifies the committed corroboration first', str(entries[0]))
            return
    report('agy installs from committed corroboration and publishes one alias-free binary')


def case_agy_committed_corroboration(work: Path, fixture: AgyFixture) -> None:
    """The committed records and the pin must still agree. Each row is refused
    before any fetch, so a substituted archive is never even requested."""
    cases = {
        'tampered release evidence': (
            lambda: (fixture.directory / AGY_RELEASE_JSON)
            .write_text((fixture.directory / AGY_RELEASE_JSON).read_text().replace('4102', '4103')),
            'release.json'),
        'release names another tag': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: value.update({'tag_name': '9.9.8'})),
            'the pin says'),
        'release is a draft': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: value.update({'draft': True})),
            'not a published release'),
        'release digest disagrees with the pin': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: value['assets'][0].update(
                                      {'digest': 'sha256:' + '0' * 64})),
            'the pin says sha256:'),
        'release omits the asset': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: value.update({'assets': value['assets'][1:]})),
            'carries no'),
        'release repeats an asset name': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: value.update(
                                      {'assets': value['assets'] + value['assets'][:1]})),
            'twice'),
        'manifest names another version': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/amd64'),
                                  lambda value: value.update({'version': '9.9.8'})),
            'the pin says'),
        'manifest sha512 disagrees with the pin': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/amd64'),
                                  lambda value: value.update({'sha512': '0' * 128})),
            'the manifest sha512 is'),
        'manifest carries an extra key': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/amd64'),
                                  lambda value: value.update({'signature': 'none'})),
            'unknown'),
        'manifest has a duplicate key': (
            lambda: (fixture.directory / fixture.manifest_name('linux/amd64')).write_text(
                '{"version": "9.9.9", "version": "9.9.9", "url": "https://x/", '
                '"sha512": "' + artifact_pin.sha512(
                    fixture.bytes['linux/amd64']) + '"}\n'),
            'duplicate'),
        'manifest url is not https': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/amd64'),
                                  lambda value: value.update({'url': 'file:///tmp/x'})),
            'https URL'),
        # The corroboration is per platform, and the build checks the platform it
        # was *not* asked for too: an amd64 build still refuses an arm64 record
        # that disagrees with the pin, so a loop narrowed to one platform fails
        # here instead of silently pinning half a release.
        'arm64 release digest disagrees with the pin': (
            lambda: _rewrite_json(fixture.directory / AGY_RELEASE_JSON,
                                  lambda value: _asset(value, fixture, 'linux/arm64')
                                  .update({'digest': 'sha256:' + '0' * 64})),
            'the pin says sha256:'),
        'arm64 manifest sha512 disagrees with the pin': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/arm64'),
                                  lambda value: value.update({'sha512': '0' * 128})),
            'the manifest sha512 is'),
        'arm64 manifest names another version': (
            lambda: _rewrite_json(fixture.directory / fixture.manifest_name('linux/arm64'),
                                  lambda value: value.update({'version': '9.9.8'})),
            'the pin says'),
    }
    for index, (label, (mutate, needle)) in enumerate(cases.items()):
        fixture.restore()
        mutate()
        # The pin records the evidence digests, so a hand-edited evidence file is
        # first re-hashed into the pin for the "changed evidence" row: the point
        # is that the *content* cannot disagree, not only the hash.
        if label != 'tampered release evidence':
            fixture.write_pin()
        root = work / f'agy-evidence-{index}'
        root.mkdir()
        log = work / f'agy-evidence-{index}.log'
        result = install_agy(fixture, root, events=log)
        expect_agy_refusal(label, result, needle, root)
        if log.exists() and 'fetch' in events_of(log):
            fail(label, f'the archive must not be fetched: {log.read_text().strip()}')
    fixture.restore()


def _rewrite_json(path: Path, mutate) -> None:
    value = json.loads(path.read_text())
    mutate(value)
    path.write_text(json.dumps(value, indent=2) + '\n')


def _asset(release: dict, fixture: AgyFixture, platform: str) -> dict:
    """One platform's asset record, addressed by name rather than position so a
    row cannot silently tamper the other architecture."""
    name = fixture.filename(platform)
    return next(asset for asset in release['assets'] if asset['name'] == name)


def case_agy_archive_bytes(work: Path, fixture: AgyFixture) -> None:
    """The bytes, the second hash and the archive's own shape, each with the
    committed corroboration kept consistent so only one check can refuse."""
    changed = agy_archive(elf_header(62) + b'# a different build\n')
    fixture.path('linux/amd64').write_bytes(changed)
    root = work / 'agy-sha256'
    root.mkdir()
    log = work / 'agy-sha256.log'
    expect_agy_refusal('changed bytes are refused before extraction',
                       install_agy(fixture, root, events=log), 'the pin says', root)
    if 'extract' in events_of(log):
        fail('a mismatch never starts extraction', str(events_of(log)))
    fixture.restore()

    # Both corroboration records name a sha512 the served bytes do not have, and
    # they agree with each other: only the post-fetch hash check can refuse, so
    # this row proves the check runs after the fetch and before extraction.
    zeros = '0' * 128
    _rewrite_json(fixture.directory / fixture.manifest_name('linux/amd64'),
                  lambda value: value.update({'sha512': zeros}))
    fixture.write_pin(lambda pin: pin['platforms']['linux/amd64'].update({'sha512': zeros}))
    root = work / 'agy-sha512'
    root.mkdir()
    log = work / 'agy-sha512.log'
    expect_agy_refusal('a corroborated sha512 is checked after the fetch',
                       install_agy(fixture, root, events=log), 'corroborated sha512', root)
    entries = events_of(log)
    if 'fetch' not in entries or 'extract' in entries:
        fail('the sha512 is checked after the fetch and before extraction', str(entries))
    fixture.restore()

    for index, (kind, needle) in enumerate({
            'extra-member': 'the archive carries',
            'non-canonical': 'not a canonical path',
            'prefixed': 'the archive carries',
            'symlink': 'links outside the archive',
            'setuid': 'setuid',
            'not-executable': 'is mode 0644',
            # The reviewed mode is exactly 0755. A mode the review did not select
            # is refused rather than repaired: the image publishes
            # `/usr/local/bin/agy` for every guest UID, so `-rwxrwxrwx` would let
            # one guest replace the binary another runs. Group/world-write and
            # setuid are the dangerous ones, and 0700/0750 are refused too because
            # the rule is one exact mode, not a set of acceptable ones.
            'mode-777': 'is mode 0777',
            'mode-775': 'is mode 0775',
            'mode-750': 'is mode 0750',
            'mode-700': 'is mode 0700'}.items()):
        raw = agy_hostile(kind)
        fixture.bytes['linux/amd64'] = raw
        fixture.path('linux/amd64').write_bytes(raw)
        fixture.write_evidence()
        fixture.write_pin()
        root = work / f'agy-shape-{index}'
        root.mkdir()
        expect_agy_refusal(f'agy archive shape: {kind}', install_agy(fixture, root), needle, root)
    fixture.restore()

    # A consistent-hash wrong-architecture upload: both channels and the archive
    # digest agree, so only the ELF header separates the two binaries.
    wrong = agy_archive(elf_header(183))
    fixture.bytes['linux/amd64'] = wrong
    fixture.path('linux/amd64').write_bytes(wrong)
    fixture.write_evidence()
    fixture.write_pin()
    root = work / 'agy-wrong-elf'
    root.mkdir()
    expect_agy_refusal('a consistent-hash wrong-architecture binary is refused',
                       install_agy(fixture, root), 'ELF machine 183', root)
    fixture.restore()

    not_elf = agy_archive(b'not an ELF header at all\n')
    fixture.bytes['linux/amd64'] = not_elf
    fixture.path('linux/amd64').write_bytes(not_elf)
    fixture.write_evidence()
    fixture.write_pin()
    root = work / 'agy-not-elf'
    root.mkdir()
    expect_agy_refusal('a published member that is not ELF is refused',
                       install_agy(fixture, root), 'is not an ELF binary', root)
    fixture.restore()

    # A 32-bit ELF for the right machine: `e_machine` cannot tell it apart from the
    # reviewed binary, so only the class/endianness header check can.
    thirty_two_bit = agy_archive(elf_header(62, elf_class=1))
    fixture.bytes['linux/amd64'] = thirty_two_bit
    fixture.path('linux/amd64').write_bytes(thirty_two_bit)
    fixture.write_evidence()
    fixture.write_pin()
    root = work / 'agy-elf-class'
    root.mkdir()
    expect_agy_refusal('a 32-bit ELF for the right machine is refused',
                       install_agy(fixture, root), 'not a 64-bit little-endian ELF', root)
    fixture.restore()


def case_agy_ceiling(work: Path, fixture: AgyFixture) -> None:
    """The 64 MiB ceiling that suits Node would refuse agy's real amd64 archive,
    so the ceiling is per artifact — and the decoded member has its own."""
    if artifact_pin.MAX_ARCHIVE_BYTES['agy'] <= 67_990_723:
        fail('the agy archive ceiling admits the real amd64 archive',
             str(artifact_pin.MAX_ARCHIVE_BYTES['agy']))
        return
    if artifact_pin.MAX_ARCHIVE_BYTES['node'] != 64 << 20 \
            or artifact_pin.MAX_ARCHIVE_BYTES['gh'] != 64 << 20:
        fail('the node and gh ceilings are unchanged', str(artifact_pin.MAX_ARCHIVE_BYTES))
        return
    report('the per-artifact archive ceilings leave node and gh unchanged')

    import install_artifact
    previous = artifact_pin.MAX_MEMBER_BYTES['agy']
    artifact_pin.MAX_MEMBER_BYTES['agy'] = len(elf_header(62)) - 1
    try:
        root = work / 'agy-member-ceiling'
        root.mkdir()
        status = install_artifact.main(
            ['agy', str(fixture.directory / 'pin.json'), 'amd64', '--root', str(root)])
    finally:
        artifact_pin.MAX_MEMBER_BYTES['agy'] = previous
    if status == 0 or (root / 'usr/local/bin/agy').exists():
        fail('a member past the decoded ceiling is refused before extraction',
             f'status {status}')
        return
    report('a member past the decoded ceiling is refused before extraction')


def case_agy_publish_conflict(work: Path, fixture: AgyFixture) -> None:
    """An installed `agy` is another input's path: the install publishes nothing
    and leaves the existing bytes in place."""
    root = work / 'agy-conflict'
    (root / 'usr/local/bin').mkdir(parents=True)
    existing = root / 'usr/local/bin/agy'
    existing.write_bytes(b'#!/bin/sh\necho 1.0.0\n')
    result = install_agy(fixture, root)
    if result.returncode == 0:
        fail('an installed agy is not replaced', 'the install succeeded')
        return
    if 'refusing to replace an installed path' not in result.stderr:
        fail('an installed agy is not replaced', result.stderr.strip())
        return
    if existing.read_bytes() != b'#!/bin/sh\necho 1.0.0\n':
        fail('an installed agy is not replaced', 'the existing bytes changed')
        return
    report('an installed agy is left untouched and nothing else is published')


def case_agy_pin_refusals(work: Path, fixture: AgyFixture) -> None:
    """The pin itself: the method is bound to the name, the payload is the
    reviewed rename, and the URLs are computed rather than matched."""
    created = artifact_pin.canonical_url('agy', VERSION, 'linux/amd64')
    asset = artifact_pin.expected_filename('agy', VERSION, 'linux/amd64')
    rows = [
        ('downgraded-to-openpgp',
         lambda pin: pin['authentication'].update({'method': 'openpgp-signed-manifest'}),
         'must be hash-only'),
        ('payload-member',
         lambda pin: pin['install']['payload'][0].update({'member': 'agy'}), 'payload'),
        ('payload-path',
         lambda pin: pin['install']['payload'][0].update({'path': 'bin/antigravity'}),
         'payload'),
        ('payload-string',
         lambda pin: pin['install'].update({'payload': ['antigravity']}), 'payload'),
        ('v-prefixed-tag',
         lambda pin: pin['upstream'].update({'release': f'v{VERSION}'}), 'upstream release'),
        ('v-prefixed-url',
         lambda pin: pin['platforms']['linux/amd64'].update(
             {'url': created.replace(f'/download/{VERSION}/', f'/download/v{VERSION}/')}),
         'must be exactly'),
        ('musl-asset',
         lambda pin: pin['platforms']['linux/amd64'].update(
             {'url': created.replace('linux_x64', 'linux_musl')}), 'must be exactly'),
        ('another-repository',
         lambda pin: pin['platforms']['linux/amd64'].update(
             {'url': created.replace(artifact_pin.AGY_REPOSITORY, 'attacker/repo')}),
         'must be exactly'),
        ('query-string',
         lambda pin: pin['platforms']['linux/amd64'].update({'url': created + '?raw=1'}),
         'must be exactly'),
        ('missing-sha512',
         lambda pin: pin['platforms']['linux/amd64'].pop('sha512'), 'missing sha512'),
        ('upper-case-sha512',
         lambda pin: pin['platforms']['linux/amd64'].update(
             {'sha512': pin['platforms']['linux/amd64']['sha512'].upper()}), 'SHA-512'),
        ('member-prefix',
         lambda pin: pin['platforms']['linux/amd64'].update({'member_prefix': 'agy/'}),
         'member_prefix'),
        ('release-evidence-url',
         lambda pin: pin['authentication']['release'].update(
             {'url': 'https://api.github.com/repos/attacker/repo/releases/tags/' + VERSION}),
         'the url must be exactly'),
        ('manifest-evidence-url',
         lambda pin: pin['authentication']['manifests']['linux/amd64'].update(
             {'url': pin['authentication']['manifests']['linux/arm64']['url']}),
         'the url must be exactly'),
        ('evidence-path',
         lambda pin: pin['authentication']['manifests']['linux/amd64'].update(
             {'path': 'evidence/other.json'}), 'the path must be'),
        ('evidence-missing-volume',
         lambda pin: pin['authentication'].pop('manifests'), 'missing manifests'),
        ('links',
         lambda pin: pin['install'].update(
             {'links': [{'path': '/usr/local/bin/antigravity',
                         'target': '/usr/local/bin/agy'}]}), 'links'),
    ]
    for index, (label, tamper, needle) in enumerate(rows):
        fixture.restore()
        fixture.write_pin(tamper)
        root = work / f'agy-pin-{index}'
        root.mkdir()
        expect_agy_refusal(f'agy pin refusal: {label}', install_agy(fixture, root), needle, root)
    fixture.restore()
    report(f'agy: {len(rows)} pin mutations are refused, naming the field')
    if asset != 'agy_cli_linux_x64.tar.gz':
        fail('the agy asset name', asset)


def case_agy_named_by_directory(work: Path, fixture: AgyFixture) -> None:
    """`images/tools/<name>/pin.json` names its artifact by its directory, which is
    how the committed agy pin gets its name."""
    tree = work / 'committed' / 'agy'
    tree.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(fixture.directory, tree)
    if artifact_pin.load(tree / 'pin.json').name != 'agy':
        fail('a pin.json names its artifact by its directory', 'the name differs')
        return
    report('a pin.json names its artifact by its directory')


def case_agy_duplicate_keys(work: Path, fixture: AgyFixture) -> None:
    """A pin is read exactly once and exactly one way.

    `json.loads` keeps the last of two identical keys, so a pin could carry one
    version for a reviewer's reader and another for the installer — and the Rust
    gate already refuses duplicates, so the two would disagree about the pin.
    """
    fixture.restore()
    text = (fixture.directory / 'pin.json').read_text()
    amd64 = sha256(fixture.bytes['linux/amd64'])
    zeros = '0' * 64
    rows = [
        ('top-level version',
         f'"version": "{VERSION}",',
         f'"version": "0.0.1", "version": "{VERSION}",'),
        ('top-level authentication method',
         '"method": "hash-only",',
         '"method": "hash-only", "method": "openpgp-signed-manifest",'),
        ('nested platform sha256',
         f'"sha256": "{amd64}",',
         f'"sha256": "{zeros}", "sha256": "{amd64}",'),
    ]
    for index, (label, needle, replacement) in enumerate(rows):
        if needle not in text:
            fail(f'a duplicate-key row still applies: {label}', 'the pin changed shape')
            return
        (fixture.directory / 'pin.json').write_text(text.replace(needle, replacement, 1))
        root = work / f'agy-duplicate-{index}'
        root.mkdir()
        log = work / f'agy-duplicate-{index}.log'
        expect_agy_refusal(f'agy pin refusal: duplicate {label}',
                           install_agy(fixture, root, events=log), 'duplicate JSON key', root)
        if log.exists() and 'fetch' in events_of(log):
            fail(f'duplicate {label}', 'a refused pin must not fetch anything')
    fixture.restore()
    report('agy: a pin with a duplicate key is refused before any fetch')


def case_unknown_artifact_name(work: Path, fixture: AgyFixture) -> None:
    """An unknown `images/tools/<x>/pin.json` is refused, not read as `gh`.

    The loader names a pin after its directory, so any new artifact's directory
    reaches the parser. Before this refusal an unknown name fell through to the
    GitHub CLI defaults and then died on a missing per-artifact ceiling instead of
    naming what was wrong.
    """
    tree = work / 'unknown' / 'other'
    tree.parent.mkdir(parents=True, exist_ok=True)
    shutil.copytree(fixture.directory, tree)
    root = work / 'agy-unknown-name'
    root.mkdir()
    result = install_agy(fixture, root, pin=tree / 'pin.json')
    if result.returncode == 0:
        fail('an unknown artifact name is refused', 'it installed')
        return
    if 'unknown artifact' not in result.stderr or 'other' not in result.stderr:
        fail('an unknown artifact name is refused', result.stderr.strip())
        return
    report('a pin whose directory names no reviewed artifact is refused')


def case_agy_wrong_artifact(work: Path, fixture: AgyFixture) -> None:
    """A pin may only be installed as the artifact it names, however the caller
    spells the path."""
    installed = ROOT / 'images/tools/agy/pin.json'
    if not installed.is_file():
        fail('the committed agy pin exists', f'{installed} is missing')
        return
    result = run(['bash', str(INSTALLER), 'gh', str(installed), 'amd64',
                  '--root', str(work / 'agy-wrong-artifact')],
                 env=dict(os.environ, ARTIFACT_SOURCE_DIR=str(ROOT / 'images')))
    if result.returncode == 0 or 'pins agy, not gh' not in result.stderr:
        fail('the committed agy pin cannot be installed as gh', result.stderr.strip())
        return
    report('the committed agy pin cannot be installed as another artifact')




def ordering_problems(log: Path, archive_name: str,
                      expect_extraction: bool = True) -> list[str]:
    """What the events log says the installer did, checked as an ordering."""
    entries = [tuple(line.split(' ', 1)) for line in log.read_text().splitlines() if line.strip()]
    archive_fetches = [index for index, (kind, detail) in enumerate(entries)
                       if kind == 'fetch' and detail.endswith('/' + archive_name)]
    extracts = [index for index, (kind, _) in enumerate(entries) if kind == 'extract']
    problems = []
    if len(archive_fetches) != 1:
        problems.append(f'the archive was fetched {len(archive_fetches)} times')
    if extracts and not archive_fetches:
        problems.append('the archive was extracted without being fetched')
    elif extracts and archive_fetches and extracts[0] < archive_fetches[0]:
        problems.append('extraction started before the archive was fetched')
    if extracts and not expect_extraction:
        problems.append('extraction started although the archive did not match the pin')
    if expect_extraction and not extracts:
        problems.append('the matching archive was never extracted')
    return problems


def case_ordering(work: Path, fixture: Fixture) -> None:
    """`--events` records what the installer did, so "extraction never started"
    and "no re-download" are observed instead of assumed. The two regressions a
    reviewer introduced into the real installer are replayed against a copied
    tree: both keep every other assertion green and change only the log."""
    node_name_amd64 = node_name('linux/amd64')
    log = work / 'ordering-success.log'
    root = work / 'ordering-success'
    root.mkdir()
    result = install(fixture, 'node', 'amd64', root, events=log)
    if result.returncode != 0:
        fail('the ordering case installs', result.stderr.strip())
        return
    problems = ordering_problems(log, node_name_amd64)
    if problems:
        fail('a successful install fetches once and extracts after the hash', '; '.join(problems))
        return
    report('a successful install fetches the archive once and extracts after hashing')

    original = fixture.node_path('linux/amd64').read_bytes()
    # A valid archive whose bytes are not the pinned ones: the corruption must not
    # be what the installer notices, or the ordering would never be exercised.
    fixture.node_path('linux/amd64').write_bytes(
        node_archive(NODE_PREFIX['linux/amd64'], fixture.gh_log, 'xz', npm_version='1.0.0'))
    try:
        log = work / 'ordering-refusal.log'
        root = work / 'ordering-refusal'
        root.mkdir()
        result = install(fixture, 'node', 'amd64', root, events=log)
        if result.returncode == 0 or 'the pin says' not in result.stderr:
            fail('a mismatch refuses before extraction', result.stderr.strip())
            return
        entries = [line.split(' ', 1)[0] for line in log.read_text().splitlines() if line.strip()]
        if 'extract' in entries:
            fail('a mismatch never starts extraction', str(entries))
            return
        problems = ordering_problems(log, node_name_amd64, expect_extraction=False)
        if problems:
            fail('a mismatch fetches the archive exactly once', '; '.join(problems))
            return
        report('a mismatch fetches the archive once and never starts extraction')

        digest_check = """        if artifact_pin.sha256(payload) != archive.digest:
            raise Refusal(f'{archive.filename} is {artifact_pin.sha256(payload)}, '
                          f'the pin says {archive.digest}')
        if archive.sha512 is not None and artifact_pin.sha512(payload) != archive.sha512:
            raise Refusal(f'{archive.filename} does not match the corroborated sha512')
"""
        extraction = """        archive_file = scratch / archive.filename
        archive_file.write_bytes(payload)
        with tarfile.open(archive_file, 'r:' + ('xz' if archive.archive == 'tar.xz' else 'gz')) as tar:
            members = preflight(tar, archive.member_prefix)
            check_members(pin, members)
            extract(members, tar, staging, archive.member_prefix)
        check_executable(pin, platform, staging)
"""
        regressions = {
            'extraction before the digest check': (
                digest_check + extraction, extraction + digest_check),
            'a second fetch after the mismatch': (
                "        payload = fetch(archive.url, artifact_pin.MAX_ARCHIVE_BYTES[pin.name])\n",
                "        payload = fetch(archive.url, artifact_pin.MAX_ARCHIVE_BYTES[pin.name])\n"
                "        payload = fetch(archive.url, artifact_pin.MAX_ARCHIVE_BYTES[pin.name])\n"),
        }
        for label, (from_text, to_text) in regressions.items():
            tree = work / 'mutated-images'
            shutil.rmtree(tree, ignore_errors=True)
            shutil.copytree(ROOT / 'images', tree)
            source = (tree / 'artifacts/install_artifact.py').read_text()
            if from_text not in source:
                fail(f'the {label} regression still applies', 'the installer changed shape')
                return
            (tree / 'artifacts/install_artifact.py').write_text(
                source.replace(from_text, to_text, 1))
            log = work / 'ordering-mutant.log'
            root = work / 'ordering-mutant'
            shutil.rmtree(root, ignore_errors=True)
            root.mkdir()
            mutated_result = install(fixture, 'node', 'amd64', root, events=log, images=tree)
            if mutated_result.returncode == 0:
                fail(f'the {label} regression is caught', 'the mutated installer installed')
                return
            if 'the pin says' not in mutated_result.stderr:
                fail(f'the {label} regression is caught',
                     f'the refusal changed: {mutated_result.stderr.strip()}')
                return
            if not ordering_problems(log, node_name_amd64, expect_extraction=False):
                fail(f'the {label} regression is caught',
                     'the ordering check accepted the mutated installer')
                return
    finally:
        fixture.node_path('linux/amd64').write_bytes(original)
    report('both replayed ordering regressions are caught by the event log')



def main() -> int:
    if shutil.which('gpg') is None:
        print('artifact-pins: gpg is required to sign the fixture manifest', file=sys.stderr)
        return 1
    work = Path(tempfile.mkdtemp(prefix='artifact-pins.')).resolve()
    try:
        fixture = Fixture(work)
        agy = AgyFixture(work)
        case_happy_path(work, fixture)
        case_changed_bytes(work, fixture)
        case_wrong_architecture(work, fixture)
        case_pin_refusals(work, fixture)
        case_bad_signature(work, fixture)
        case_attacker_key(work, fixture)
        case_missing_asset(work, fixture)
        case_transport(work, fixture)
        case_hostile_members(work, fixture)
        case_attestation(work, fixture)
        case_no_fallback(work, fixture)
        case_url_mutations(work, fixture)
        case_destination_ancestor(work, fixture)
        case_partial_publication(work, fixture)
        case_attestation_before_publication(work, fixture)
        case_bundled_npm(work, fixture)
        case_ordering(work, fixture)
        case_agy_install(work, agy)
        case_agy_committed_corroboration(work, agy)
        case_agy_archive_bytes(work, agy)
        case_agy_ceiling(work, agy)
        case_agy_publish_conflict(work, agy)
        case_agy_pin_refusals(work, agy)
        case_agy_named_by_directory(work, agy)
        case_agy_duplicate_keys(work, agy)
        case_unknown_artifact_name(work, agy)
        case_agy_wrong_artifact(work, agy)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if failures:
        print(f'artifact-pins: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('artifact-pins: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
