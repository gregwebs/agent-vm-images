#!/usr/bin/env python3
"""Hermetic evidence for the agy owner writer (#131).

`script/dev/pin-agy.py` is the whole trust decision for an artifact with no
upstream signature: the downloaded bytes must equal both GitHub's asset digest
and Google's updater-manifest sha512, only the manifest's current release may be
pinned, and the pin, its three evidence files and the Dockerfile ARG are
published as one transaction.

Every case here runs the real writer from a fixture repository whose modules are
copies of this checkout's, with exactly two things replaced: the transport (files
instead of the network, through `fixtures/agent-versions/agy-writer.py`) and, in
the recovery cases, one injected publication fault. The refusal cases additionally
assert that no source byte changed, so a check placed after publication cannot
pass.

`--write-fixture DIR [--version X.Y.Z]` writes a published closure and the bytes
that produced it into DIR for another suite to check in (`agent-versions.sh`
needs an older committed agy closure than the release it serves).
"""
from __future__ import annotations

import argparse
import gzip
import importlib.util
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
WRITER = ROOT / 'script' / 'dev' / 'pin-agy.py'
ADAPTER = ROOT / 'script' / 'test' / 'fixtures' / 'agent-versions' / 'agy-writer.py'
PIN_DIRECTORY = 'images/tools/agy'
DOCKERFILE = 'images/standard/Dockerfile'
VERSION = '1.4.11'
OLDER = '1.4.10'
NEWER = '1.4.12'
MACHINES = {'linux/amd64': (62, 'x64'), 'linux/arm64': (183, 'arm64')}
ARCH = {'linux/amd64': 'amd64', 'linux/arm64': 'arm64'}
# The five files one publication touches.
DESTINATIONS = [f'{PIN_DIRECTORY}/pin.json',
                f'{PIN_DIRECTORY}/evidence/release.json',
                f'{PIN_DIRECTORY}/evidence/manifest-linux-amd64.json',
                f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json',
                DOCKERFILE]

failures: list[str] = []


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: str) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


# --- fixture bytes -----------------------------------------------------------

def elf_header(machine: int) -> bytes:
    header = bytearray(64)
    header[0:4] = b'\x7fELF'
    header[4] = 2
    header[5] = 1
    header[6] = 1
    header[18:20] = machine.to_bytes(2, 'little')
    return bytes(header)


def archive(machine: int, body: bytes | None = None, mode: int = 0o755) -> bytes:
    """One prefixless executable member, byte-identical across calls: a gzip header
    carries a timestamp, which would make two otherwise identical fixtures differ."""
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w', format=tarfile.GNU_FORMAT) as tar:
        payload = elf_header(machine) if body is None else body
        info = tarfile.TarInfo('antigravity')
        info.size = len(payload)
        info.mode = mode
        tar.addfile(info, io.BytesIO(payload))
    return gzip.compress(buffer.getvalue(), mtime=0)


def asset_name(platform: str) -> str:
    return f'agy_cli_linux_{MACHINES[platform][1]}.tar.gz'


class Upstream:
    """One served release: the two manifests, the GitHub release and both assets."""

    def __init__(self, version: str = VERSION, *, machines=None, bodies=None,
                 download_count: int = 4102) -> None:
        self.version = version
        self.download_count = download_count
        self.machines = dict(machines or {platform: machine
                                         for platform, (machine, _) in MACHINES.items()})
        self.bodies = dict(bodies or {platform: archive(self.machines[platform])
                                     for platform in MACHINES})
        self.manifest_versions = {platform: version for platform in MACHINES}
        self.manifest_sha512 = {platform: _sha512(self.bodies[platform])
                                for platform in MACHINES}
        self.manifest_urls = {
            platform: f'https://storage.googleapis.com/antigravity-public/{version}/'
                      f'{asset_name(platform)}' for platform in MACHINES}
        self.release_digests = {asset_name(platform): 'sha256:' + _sha256(raw)
                                for platform, raw in self.bodies.items()}
        self.release_tag = version
        self.draft = False
        self.drop_assets: set[str] = set()

    def files(self) -> dict[str, bytes]:
        files: dict[str, bytes] = {}
        for platform in MACHINES:
            files[f'linux_{ARCH[platform]}.json'] = _json({
                'version': self.manifest_versions[platform],
                'url': self.manifest_urls[platform],
                'sha512': self.manifest_sha512[platform],
            })
            files[asset_name(platform)] = self.bodies[platform]
        files['release.json'] = _json({
            'tag_name': self.release_tag,
            'draft': self.draft,
            'immutable': False,
            'assets': [
                {'name': name, 'size': len(self.bodies[_platform(name)]),
                 'digest': digest, 'download_count': self.download_count,
                 'uploader': {'login': 'fixture'}}
                for name, digest in self.release_digests.items()
                if name not in self.drop_assets],
        })
        return files


def _platform(name: str) -> str:
    for platform in MACHINES:
        if asset_name(platform) == name:
            return platform
    raise KeyError(name)


def _json(value: dict) -> bytes:
    return (json.dumps(value, indent=2) + '\n').encode()


def _sha256(raw: bytes) -> str:
    import hashlib
    return hashlib.sha256(raw).hexdigest()


def _sha512(raw: bytes) -> str:
    import hashlib
    return hashlib.sha512(raw).hexdigest()


# --- fixture repository ------------------------------------------------------

def dockerfile(version: str = VERSION) -> str:
    return f'''ARG BASE_IMAGE=agent-vm-base:local
FROM ${{BASE_IMAGE}}
# agy
ARG AGENT_VERSION_AGY={version}

LABEL org.agent-vm.version.agy="${{AGENT_VERSION_AGY}}"

RUN sh /tmp/install-agy.sh
'''


class Repository:
    """A repository copy whose writer, installer and model are this checkout's."""

    def __init__(self, directory: Path, *, arg: str = VERSION) -> None:
        self.path = directory
        (directory / 'images' / 'artifacts').mkdir(parents=True, exist_ok=True)
        (directory / 'images' / 'standard').mkdir(parents=True, exist_ok=True)
        (directory / 'script' / 'dev').mkdir(parents=True, exist_ok=True)
        for name in ('artifact_pin.py', 'install_artifact.py'):
            shutil.copyfile(ROOT / 'images' / 'artifacts' / name,
                            directory / 'images' / 'artifacts' / name)
        shutil.copyfile(ROOT / 'images' / 'install-artifact.sh',
                        directory / 'images' / 'install-artifact.sh')
        for name in ('pin-agy.py', 'pin-artifact.py', 'pin-codex.py'):
            shutil.copyfile(ROOT / 'script' / 'dev' / name,
                            directory / 'script' / 'dev' / name)
        (directory / DOCKERFILE).write_text(dockerfile(arg))

    def read(self, relative: str) -> bytes:
        return (self.path / relative).read_bytes()

    def write(self, relative: str, raw: bytes) -> None:
        path = self.path / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)

    def tree(self) -> dict[str, bytes]:
        """Every source byte a publication may change."""
        found: dict[str, bytes] = {}
        for name in DESTINATIONS:
            path = self.path / name
            if path.is_file():
                found[name] = path.read_bytes()
        return found

    def closure(self) -> Path:
        return self.path / PIN_DIRECTORY


class Assets:
    """The served directory the fixture transport reads."""

    def __init__(self, directory: Path, upstream: Upstream) -> None:
        self.path = directory
        directory.mkdir(parents=True, exist_ok=True)
        self.write(upstream)

    def write(self, upstream: Upstream) -> None:
        for name, raw in upstream.files().items():
            (self.path / name).write_bytes(raw)

    def fetches(self) -> list[str]:
        log = self.path / 'fetches.log'
        return log.read_text().splitlines() if log.is_file() else []

    def start_fetch_log(self) -> None:
        """Record fetches for a row that must observe *none* of them.

        The adapter only creates the log when it records a request, so an absent log
        cannot be told apart from recording that was never switched on — which is
        how a CI row that downloaded everything used to pass.
        """
        (self.path / 'fetches.log').write_text('')

    def clear_log(self) -> None:
        (self.path / 'fetches.log').unlink(missing_ok=True)


def run_writer(repo: Repository, *arguments: str, assets: Assets | None = None,
               environment: dict | None = None, adapter: bool = True,
               tmp: Path | None = None) -> subprocess.CompletedProcess:
    script = ADAPTER if adapter else repo.path / 'script' / 'dev' / 'pin-agy.py'
    env = {key: value for key, value in os.environ.items()
           if key not in ('GITHUB_ACTIONS', 'PYTHONPATH')}
    env['FIXTURE_AGY_WRITER'] = str(repo.path / 'script' / 'dev' / 'pin-agy.py')
    if assets is not None:
        env['FIXTURE_AGY_ASSETS'] = str(assets.path)
    if tmp is not None:
        env['TMPDIR'] = str(tmp)
    env.update(environment or {})
    argv = [sys.executable, '-I', str(script), *arguments, '--repo-root', str(repo.path)]
    return subprocess.run(argv, capture_output=True, text=True, check=False, env=env,
                          cwd=str(Path(tempfile.gettempdir())))


def expect_refusal(label: str, result: subprocess.CompletedProcess, needle: str,
                   repo: Repository, before: dict[str, bytes]) -> None:
    if result.returncode == 0:
        fail(label, f'published instead of refusing: {result.stdout.strip()}')
        return
    if needle not in (result.stdout + result.stderr):
        fail(label, f'refusal does not mention {needle!r}: {result.stderr.strip()}')
        return
    after = repo.tree()
    if after != before:
        changed = sorted(set(after) ^ set(before)) or sorted(
            name for name in after if after[name] != before.get(name))
        fail(label, f'the refusal changed source bytes: {changed}')
        return
    report(label)


# --- cases -------------------------------------------------------------------

def case_decision_table(work: Path) -> None:
    """Every row of the pin tool's decision table, with its own fixture."""
    rows = [
        ('both channels agree', lambda up: None, None),
        ('manifests disagree', lambda up: up.manifest_versions.update(
            {'linux/arm64': NEWER}), 'name linux/amd64'),
        ('manifests agree but not with --version', lambda up: (
            up.manifest_versions.update({platform: OLDER for platform in MACHINES})),
         'only the release they'),
        ('GitHub digest disagrees', lambda up: up.release_digests.update(
            {asset_name('linux/amd64'): 'sha256:' + '0' * 64}),
         'but the downloaded bytes are'),
        ('manifest sha512 disagrees', lambda up: up.manifest_sha512.update(
            {'linux/arm64': '0' * 128}), 'manifest records sha512'),
        ('GitHub omits the asset', lambda up: up.drop_assets.add(
            asset_name('linux/arm64')), 'does not list'),
        ('release names another tag', lambda up: setattr(up, 'release_tag', OLDER),
         'the pin says'),
        ('release is a draft', lambda up: setattr(up, 'draft', True),
         'not a published release'),
    ]
    for index, (label, mutate, needle) in enumerate(rows):
        case = work / f'decision-{index}'
        repo = Repository(case / 'repo')
        upstream = Upstream()
        mutate(upstream)
        assets = Assets(case / 'assets', upstream)
        before = repo.tree()
        result = run_writer(repo, '--version', VERSION, assets=assets)
        if needle is None:
            if result.returncode != 0:
                fail(label, result.stderr.strip())
                continue
            published = repo.tree()
            if sorted(published) != sorted(DESTINATIONS):
                fail(label, f'published {sorted(published)}')
                continue
            if f'ARG AGENT_VERSION_AGY={VERSION}' not in repo.read(DOCKERFILE).decode():
                fail(label, 'the Dockerfile ARG was not written')
                continue
            report(label)
            continue
        expect_refusal(label, result, needle, repo, before)


def case_arm64_checked_against_its_own_manifest(work: Path) -> None:
    """A platform is only ever checked against its own manifest: swapping the
    amd64 sha512 into the arm64 manifest must not corroborate the arm64 archive."""
    case = work / 'cross-manifest'
    repo = Repository(case / 'repo')
    upstream = Upstream()
    other = Upstream()
    upstream.manifest_sha512['linux/arm64'] = other.manifest_sha512['linux/amd64']
    # Same version on both, distinct urls and sha512s: as upstream serves them.
    if upstream.manifest_urls['linux/amd64'] == upstream.manifest_urls['linux/arm64']:
        fail('per-architecture manifests differ', 'the fixture urls are equal')
        return
    assets = Assets(case / 'assets', upstream)
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets)
    expect_refusal('a platform is checked against its own manifest only', result,
                   'manifest records sha512', repo, before)


def case_wrong_elf_arch(work: Path) -> None:
    """Consistent hashes on both channels, and the archive is the other machine."""
    case = work / 'wrong-elf'
    repo = Repository(case / 'repo')
    upstream = Upstream(machines={'linux/amd64': 183, 'linux/arm64': 183})
    assets = Assets(case / 'assets', upstream)
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets)
    expect_refusal('a wrong-architecture binary under consistent hashes is refused',
                   result, 'ELF machine 183', repo, before)


def case_member_mode(work: Path) -> None:
    """The archive's own permission bits are part of the review.

    Both channels can corroborate an archive whose single member is mode 0777, so
    the digest chain says nothing about who may write `/usr/local/bin/agy`. The
    pin-time preflight is the shared one the build runs, so refusing here is the
    same rule the install applies.
    """
    for label, mode in (('world-writable', 0o777), ('group-writable', 0o775),
                        ('owner-only', 0o700)):
        case = work / f'member-mode-{mode:04o}'
        repo = Repository(case / 'repo')
        bodies = {platform: archive(machine, mode=mode)
                  for platform, (machine, _) in MACHINES.items()}
        assets = Assets(case / 'assets', Upstream(bodies=bodies))
        before = repo.tree()
        result = run_writer(repo, '--version', VERSION, assets=assets)
        expect_refusal(f'a {mode:04o} member is refused at pin time ({label})', result,
                       f'is mode {mode:04o}', repo, before)


def case_committed_second_platform(work: Path) -> None:
    """The committed corroboration is checked for the platform not being built.

    A loop narrowed to the first platform would let half a release drift, so each
    row tampers the arm64 record (and re-hashes it into the pin, or the row would
    only prove the evidence hash), then asks for a bump: the refusal must come
    from the corroboration of the committed closure, before any payload fetch.
    """
    golden = work / 'second-platform-golden'
    golden_repo = Repository(golden / 'repo')
    served = Upstream(VERSION)
    assets = Assets(golden / 'assets', served)
    published = run_writer(golden_repo, '--version', VERSION, assets=assets)
    if published.returncode != 0:
        fail('the second-platform fixture publishes', published.stderr.strip())
        return
    committed = golden_repo.tree()

    def case_repo(case: Path) -> Repository:
        repo = Repository(case / 'repo')
        for name in DESTINATIONS:
            if name in committed:
                repo.write(name, committed[name])
        return repo

    def retarget_pin(repo: Repository, mutate) -> None:
        """Re-hash the tampered evidence into the pin, so only the content check
        can refuse."""
        pin_path = f'{PIN_DIRECTORY}/pin.json'
        document = json.loads(repo.read(pin_path))
        mutate(document)
        repo.write(pin_path, _json(document))

    def rehash_manifest(repo: Repository, platform: str) -> None:
        relative = f'{PIN_DIRECTORY}/evidence/manifest-linux-{ARCH[platform]}.json'
        digest = _sha256(repo.read(relative))
        retarget_pin(repo, lambda pin: pin['authentication']['manifests'][platform]
                     .update({'sha256': digest}))

    def rehash_release(repo: Repository) -> None:
        relative = f'{PIN_DIRECTORY}/evidence/release.json'
        digest = _sha256(repo.read(relative))
        retarget_pin(repo, lambda pin: pin['authentication']['release']
                     .update({'sha256': digest}))

    def rewrite(repo: Repository, relative: str, mutate) -> None:
        document = json.loads(repo.read(relative))
        mutate(document)
        repo.write(relative, _json(document))

    case = work / 'second-platform-manifest'
    repo = case_repo(case)
    relative = f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json'
    rewrite(repo, relative, lambda document: document.update({'sha512': '0' * 128}))
    rehash_manifest(repo, 'linux/arm64')
    before = repo.tree()
    assets = Assets(case / 'assets', Upstream(VERSION))
    assets.clear_log()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'FIXTURE_AGY_RECORD_FETCHES': '1'})
    expect_refusal('an arm64 manifest sha512 that disagrees with the pin is refused',
                   result, 'the manifest sha512 is', repo, before)
    assert_no_payload_fetch('the arm64 manifest row', assets)

    case = work / 'second-platform-release'
    repo = case_repo(case)
    relative = f'{PIN_DIRECTORY}/evidence/release.json'
    arm64 = asset_name('linux/arm64')

    def tamper(document: dict) -> None:
        for asset in document['assets']:
            if asset['name'] == arm64:
                asset['digest'] = 'sha256:' + '0' * 64

    rewrite(repo, relative, tamper)
    rehash_release(repo)
    before = repo.tree()
    assets = Assets(case / 'assets', Upstream(VERSION))
    assets.clear_log()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'FIXTURE_AGY_RECORD_FETCHES': '1'})
    expect_refusal('an arm64 release digest that disagrees with the pin is refused',
                   result, f'for {arm64}', repo, before)
    assert_no_payload_fetch('the arm64 release row', assets)


def assert_no_payload_fetch(label: str, assets: Assets) -> None:
    fetched = [url for url in assets.fetches() if url.endswith('.tar.gz')]
    if fetched:
        fail(label, f'a refused corroboration must download no payload: {fetched}')
        return
    report(f'{label}: the corroboration refused before any payload fetch')


def case_proposal_only_tamper(work: Path) -> None:
    """The staged proposal is validated by itself, not only through its inputs.

    A `render_pin` that writes a wrong evidence hash produces a proposal whose pin
    parses and whose archive digests agree; only the step-6 `check_corroboration`
    over the proposal's own bytes refuses it.
    """
    case = work / 'proposal-tamper'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream(VERSION))
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'FIXTURE_AGY_TAMPER_EVIDENCE': 'release'})
    expect_refusal('a proposal whose evidence hash is wrong is refused', result,
                   'evidence/release.json is', repo, before)


def case_latest_only_and_rewrite(work: Path) -> None:
    """The committed state is consulted before any payload is downloaded."""
    golden = work / 'golden'
    golden_repo = Repository(golden / 'repo', arg=VERSION)
    golden_assets = Assets(golden / 'assets', Upstream(VERSION))
    published = run_writer(golden_repo, '--version', VERSION, assets=golden_assets)
    if published.returncode != 0:
        fail('the golden closure is published', published.stderr.strip())
        return
    golden_tree = golden_repo.tree()

    def committed(case: Path, version: str = VERSION) -> Repository:
        repo = Repository(case / 'repo', arg=version)
        for name in DESTINATIONS:
            if name in golden_tree:
                repo.write(name, golden_tree[name])
        return repo

    case = work / 'older-manifest'
    repo = committed(case)
    assets = Assets(case / 'assets', Upstream(OLDER))
    # Before the run, never after it: a tree read after the writer returned already
    # contains whatever the writer wrote, so comparing it with itself would pass
    # even when the refusal damaged the sources.
    before = repo.tree()
    result = run_writer(repo, '--version', OLDER, assets=assets)
    expect_refusal('a manifest older than the pin is refused', result,
                   f'older than the pinned {VERSION}', repo, before)

    case = work / 'same-release-rewrite'
    repo = committed(case)
    rewritten = Upstream(VERSION)
    rewritten.bodies['linux/amd64'] = archive(62, elf_header(62) + b'# rewritten\n')
    rewritten.manifest_sha512['linux/amd64'] = _sha512(rewritten.bodies['linux/amd64'])
    rewritten.release_digests[asset_name('linux/amd64')] = (
        'sha256:' + _sha256(rewritten.bodies['linux/amd64']))
    assets = Assets(case / 'assets', rewritten)
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets)
    expect_refusal('a same-release upstream rewrite is refused', result,
                   'same-release upstream rewrite', repo, before)


def case_repeat_is_byte_identical(work: Path) -> None:
    """A second run of the same release keeps the committed evidence bytes, so a
    daily download-count change is not a diff and the date does not move."""
    case = work / 'repeat'
    repo = Repository(case / 'repo')
    first = Upstream(VERSION, download_count=4111)
    assets = Assets(case / 'assets', first)
    result = run_writer(repo, '--version', VERSION, assets=assets)
    if result.returncode != 0:
        fail('the first adoption publishes', result.stderr.strip())
        return
    before = repo.tree()

    # Same release, new download count on every asset: the security projection is
    # unchanged, so every committed byte must survive.
    assets.write(Upstream(VERSION, download_count=9999))
    result = run_writer(repo, '--version', VERSION, assets=assets)
    if result.returncode != 0:
        fail('a repeat run succeeds', result.stderr.strip())
        return
    if 'unchanged' not in result.stdout:
        fail('a repeat run reports unchanged', result.stdout.strip())
        return
    if repo.tree() != before:
        fail('a repeat run keeps every committed byte', 'the tree changed')
        return
    report('a repeat run is byte-identical and reports unchanged')

    # One platform's manifest bytes change but parse to the same document: the
    # other platform's committed bytes must still be kept verbatim.
    changed = Upstream(VERSION, download_count=9999)
    files = changed.files()
    files['linux_amd64.json'] = json.dumps(
        json.loads(files['linux_amd64.json']), indent=4).encode() + b'\n'
    for name, raw in files.items():
        (assets.path / name).write_bytes(raw)
    result = run_writer(repo, '--version', VERSION, assets=assets)
    if result.returncode != 0:
        fail('a reformatted manifest is still the same document', result.stderr.strip())
        return
    if repo.read(f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json') != \
            before.get(f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json'):
        fail('an unchanged manifest keeps its committed bytes', 'arm64 evidence moved')
        return
    report('only the re-rendered manifest changes; the other keeps its bytes')


def case_adoption_and_dockerfile(work: Path) -> None:
    """A first adoption, a partial old state, and the three Dockerfile shapes.

    Every row snapshots the sources BEFORE the writer runs, so a write made just
    before the refusal is a failure rather than the comparison's own baseline.
    """
    case = work / 'partial'
    repo = Repository(case / 'repo')
    repo.write(f'{PIN_DIRECTORY}/evidence/release.json', b'{}\n')
    assets = Assets(case / 'assets', Upstream())
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets)
    expect_refusal('a partial committed directory is refused', result,
                   'with no pin.json', repo, before)

    for label, text, needle in (
            ('no ARG', dockerfile().replace('ARG AGENT_VERSION_AGY=', 'ARG OTHER='),
             'exactly one `ARG AGENT_VERSION_AGY='),
            ('two ARGs', dockerfile() + '\nARG AGENT_VERSION_AGY=0.0.1\n',
             'exactly one `ARG AGENT_VERSION_AGY='),
            ('no label', dockerfile().replace(
                'LABEL org.agent-vm.version.agy="${AGENT_VERSION_AGY}"',
                'LABEL org.agent-vm.version.other="x"'),
             'LABEL org.agent-vm.version.agy')):
        case = work / f'dockerfile-{label.replace(" ", "-")}'
        repo = Repository(case / 'repo')
        (repo.path / DOCKERFILE).write_text(text)
        assets = Assets(case / 'assets', Upstream())
        before = repo.tree()
        result = run_writer(repo, '--version', VERSION, assets=assets)
        expect_refusal(f'the Dockerfile with {label} is refused', result, needle, repo,
                       before)


def case_ci_and_origins(work: Path) -> None:
    """A write run refuses CI before its first fetch, and the transport's origin
    allowlist is this owner's.

    The write row turns fetch recording on and requires the empty log to survive: a
    refusal that is merely silent is not the same as one that happened before the
    first request.
    """
    case = work / 'ci'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    before = repo.tree()
    assets.start_fetch_log()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'GITHUB_ACTIONS': 'true',
                                     'FIXTURE_AGY_RECORD_FETCHES': '1'})
    expect_refusal('a write run refuses GitHub Actions', result, 'refusing GitHub Actions',
                   repo, before)
    if not (assets.path / 'fetches.log').is_file():
        fail('a CI refusal fetches nothing', 'the fetch log is gone, so nothing recorded')
    elif assets.fetches():
        fail('a CI refusal fetches nothing', str(assets.fetches()))
    else:
        report('a CI refusal fetches nothing')

    # --check stays usable in CI.
    assets.clear_log()
    result = run_writer(repo, '--check', assets=assets, environment={'GITHUB_ACTIONS': 'true'})
    if result.returncode == 0:
        fail('--check refuses a repository with no committed pin',
             result.stdout.strip())
    else:
        report('--check refuses a repository with no committed pin, in CI too')

    module = load_writer(repo)
    with Bindings():
        model, _installer, _publisher, transport = module.bind_models(repo.path)
    try:
        transport._require_origin('https://evil.example/x', module.ORIGINS)
    except Exception as error:  # noqa: BLE001 - the transport has its own Refusal
        if 'not on an approved origin' not in str(error):
            fail('an unapproved origin is refused', str(error))
        else:
            report('an origin outside the approved set is refused')
    else:
        fail('an unapproved origin is refused', 'the url was accepted')
    for host in ('https://api.github.com/x', 'https://github.com/x',
                 'https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/x'):
        try:
            transport._require_origin(host, module.ORIGINS)
        except Exception as error:  # noqa: BLE001
            fail('the approved origins are accepted', f'{host}: {error}')
            return
    report('every approved origin is accepted, and only those')


CLEAN_MODULES = ('artifact_pin', 'install_artifact')


class Bindings:
    """Keep the fixture's in-process module bindings from leaking between cases."""

    def __enter__(self) -> None:
        self.saved = {name: sys.modules.get(name) for name in CLEAN_MODULES}
        return self

    def __exit__(self, *_exception) -> None:
        for name, module in self.saved.items():
            if module is None:
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = module
        return None


def load_writer(repo: Repository):
    """The writer module of one fixture repository, loaded in-process."""
    path = repo.path / 'script' / 'dev' / 'pin-agy.py'
    spec = importlib.util.spec_from_file_location(f'pin_agy_{abs(hash(str(path)))}', path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def case_module_binding(work: Path) -> None:
    """One model, one Refusal: the installer and the publisher import the same
    object the writer validates with, and a foreign binding is refused."""
    case = work / 'binding'
    repo = Repository(case / 'repo')
    module = load_writer(repo)
    with Bindings():
        model, installer, publisher, transport = module.bind_models(repo.path)
    if installer.artifact_pin is not model or publisher.artifact_pin is not model:
        fail('one model module', 'the installer and publisher disagree')
        return
    if installer.Refusal is not model.Refusal:
        fail('one Refusal class', 'install_artifact has its own')
        return
    if model.__name__ != 'artifact_pin' or installer.__name__ != 'install_artifact':
        fail('the model is bound under its plain name',
             f'{model.__name__}, {installer.__name__}')
        return
    if 'origins' not in transport.https_fetch.__code__.co_varnames:
        fail('the transport takes an origin set', 'https_fetch has no origins keyword')
        return
    report('the shared model is bound once, under its plain name')

    # A pre-seeded foreign binding is refused rather than silently rebound.
    other = work / 'foreign'
    other.mkdir()
    (other / 'artifact_pin.py').write_text('class Refusal(Exception):\n    pass\n')
    spec = importlib.util.spec_from_file_location('artifact_pin_foreign',
                                                  other / 'artifact_pin.py')
    foreign = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(foreign)
    with Bindings():
        sys.modules['artifact_pin'] = foreign
        try:
            module.bind_models(repo.path)
        except module.Refusal as error:
            if 'refusing to rebind' not in str(error):
                fail('a foreign model binding is refused', str(error))
            else:
                report('a pre-seeded foreign model binding is refused')
        else:
            fail('a foreign model binding is refused', 'the binding was rebound')


def case_clean_process(work: Path) -> None:
    """`-I`, no PYTHONPATH, cwd outside both repositories: the writer must find and
    bind its own modules, and `--check` must see a mutated evidence byte."""
    case = work / 'clean'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    published = run_writer(repo, '--version', VERSION, assets=assets)
    if published.returncode != 0:
        fail('the clean-process fixture publishes', published.stderr.strip())
        return
    result = run_writer(repo, '--check', adapter=False)
    if result.returncode != 0:
        fail('--check passes in a clean process', result.stderr.strip())
        return
    report('--check passes in a clean process with no PYTHONPATH')

    evidence = repo.path / f'{PIN_DIRECTORY}/evidence/manifest-linux-amd64.json'
    original = evidence.read_bytes()
    evidence.write_bytes(original.replace(b'"version": "1.4.11"', b'"version": "1.4.10"'))
    try:
        result = run_writer(repo, '--check', adapter=False)
        if result.returncode == 0:
            fail('--check refuses a mutated evidence byte', 'it passed')
        elif 'the pin says' not in (result.stdout + result.stderr):
            fail('--check refuses a mutated evidence byte',
                 (result.stdout + result.stderr).strip())
        else:
            report('--check refuses a mutated evidence byte')
    finally:
        evidence.write_bytes(original)


def case_publication_fault(work: Path) -> None:
    """A fault on the last file is fully restored: status 1, every byte identical."""
    case = work / 'fault'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    first = run_writer(repo, '--version', VERSION, assets=assets)
    if first.returncode != 0:
        fail('the fault fixture publishes once', first.stderr.strip())
        return
    new = Upstream(NEWER)
    assets.write(new)
    before = repo.tree()
    result = run_writer(repo, '--version', NEWER, assets=assets,
                        environment={'FIXTURE_AGY_INJECT': 'partial',
                                     'FIXTURE_AGY_INJECT_AT': 'manifest-linux-arm64.json'})
    if result.returncode != 1:
        fail('a publication fault exits 1', f'{result.returncode}: {result.stderr.strip()}')
        return
    if 'restoring' not in result.stderr and 'restored' not in result.stderr:
        fail('a publication fault reports the restore', result.stderr.strip())
        return
    if repo.tree() != before:
        fail('a restored publication leaves every byte identical',
             str(sorted(set(repo.tree()) ^ set(before))))
        return
    report('a publication fault on the last file restores every byte (status 1)')

    # First adoption: the files that did not exist are removed again.
    case = work / 'fault-first-adoption'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'FIXTURE_AGY_INJECT': 'partial',
                                     'FIXTURE_AGY_INJECT_AT': 'manifest-linux-arm64.json'})
    if result.returncode != 1:
        fail('a first-adoption fault exits 1', f'{result.returncode}: {result.stderr.strip()}')
        return
    if repo.tree() != before:
        fail('a first-adoption restore removes what it created',
             str(sorted(repo.tree())))
        return
    leftovers = sorted(str(path.relative_to(repo.path)) for path in
                       (repo.path / PIN_DIRECTORY).rglob('*') if path.is_file())
    if leftovers:
        fail('a first-adoption restore removes every created file', str(leftovers))
        return
    report('a first-adoption fault leaves only the pre-existing Dockerfile')


def _retained_directories(stderr: str) -> tuple[Path, Path]:
    owner = None
    publisher = None
    for line in stderr.splitlines():
        if 'owner scratch:' in line:
            owner = Path(line.split('owner scratch:', 1)[1].strip())
        if 'recovery material retained at' in line:
            publisher = Path(line.split('retained at', 1)[1].strip())
    if owner is None or publisher is None:
        raise AssertionError(f'the two directories were not both printed: {stderr}')
    return owner, publisher


def case_recovery(work: Path) -> None:
    """The owner-level recovery contract: status 3 keeps both directories and the
    five proposed files, a signal with a complete restore exits 130, and a signal
    with an incomplete restore exits 3."""
    golden = work / 'recovery-golden'
    golden_repo = Repository(golden / 'repo')
    golden_assets = Assets(golden / 'assets', Upstream())
    first = run_writer(golden_repo, '--version', VERSION, assets=golden_assets)
    if first.returncode != 0:
        fail('the recovery golden closure publishes', first.stderr.strip())
        return
    # The proposal a faulted run stages is the new release's closure; capture it
    # from a healthy run of the same assets.
    proposal_golden = work / 'recovery-proposal'
    proposal_repo = Repository(proposal_golden / 'repo')
    for name in DESTINATIONS:
        if name in golden_repo.tree():
            proposal_repo.write(name, golden_repo.read(name))
    new = Upstream(NEWER)
    proposal_assets = Assets(proposal_golden / 'assets', new)
    healthy = run_writer(proposal_repo, '--version', NEWER, assets=proposal_assets)
    if healthy.returncode != 0:
        fail('the proposal golden publishes', healthy.stderr.strip())
        return
    expected = proposal_repo.tree()

    def faulted(label: str, injection: str, *, refuse_restore: bool,
                wanted_status: int) -> None:
        case = work / f'recovery-{label}'
        repo = Repository(case / 'repo')
        for name in DESTINATIONS:
            if name in golden_repo.tree():
                repo.write(name, golden_repo.read(name))
        assets = Assets(case / 'assets', new)
        tmp = case / 'tmp'
        tmp.mkdir()
        environment = {'FIXTURE_AGY_INJECT': injection,
                       'FIXTURE_AGY_INJECT_AT': 'manifest-linux-arm64.json'}
        if refuse_restore:
            environment['FIXTURE_AGY_REFUSE_RESTORE'] = '1'
        before = repo.tree()
        before_modes = {name: (repo.path / name).stat().st_mode & 0o777 for name in before}
        result = run_writer(repo, '--version', NEWER, assets=assets,
                            environment=environment, tmp=tmp)
        if result.returncode != wanted_status:
            fail(label, f'expected status {wanted_status}, got {result.returncode}: '
                        f'{result.stderr.strip()}')
            return
        if wanted_status == 130:
            if repo.tree() != before:
                fail(label, 'a restorable signal leaves every byte identical')
                return
            leftover = sorted(path.name for path in tmp.iterdir())
            if any(name.startswith('pin-artifact.work.') for name in leftover):
                fail(label, f'the publisher scratch is removed: {leftover}')
                return
            report(label)
            return
        # Status 3: both directories are retained and printed absolutely.
        owner, publisher = _retained_directories(result.stderr)
        if not owner.is_absolute() or not publisher.is_absolute():
            fail(label, f'the directories are printed absolutely: {owner}, {publisher}')
            return
        for relative in DESTINATIONS:
            retained = owner / 'proposal' / relative
            if not retained.is_file():
                fail(label, f'the owner scratch holds {relative}')
                return
            if retained.read_bytes() != expected[relative]:
                fail(label, f'{relative} in the owner scratch differs from what was staged')
                return
        backup_root = publisher / 'backup'
        if not (backup_root / '.complete').is_file():
            fail(label, 'the publisher backup set is complete')
            return
        # Order-independent: the backup set must be exactly the pre-publication
        # (bytes, mode) of every destination that existed, and nothing else.
        found = sorted(((backup_root / str(index)).read_bytes(),
                        (backup_root / str(index)).stat().st_mode & 0o777)
                       for index in range(len(DESTINATIONS))
                       if (backup_root / str(index)).is_file())
        wanted = sorted((before[name], before_modes[name]) for name in before)
        if found != wanted:
            fail(label, 'the numbered backups hold the pre-publication bytes and modes')
            return
        report(label)

    faulted('publication-fault', 'partial', refuse_restore=True, wanted_status=3)
    faulted('signal', 'signal', refuse_restore=False, wanted_status=130)
    faulted('signal-incomplete', 'signal', refuse_restore=True, wanted_status=3)


def case_check(work: Path) -> None:
    """`--check` on the committed tree, and the mutation it must catch."""
    case = work / 'check'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    result = run_writer(repo, '--version', VERSION, assets=assets)
    if result.returncode != 0:
        fail('the check fixture publishes', result.stderr.strip())
        return
    result = run_writer(repo, '--check')
    if result.returncode != 0 or 'offline committed-input check' not in result.stdout:
        fail('--check passes on the committed tree', result.stdout + result.stderr)
        return
    if 'not a fresh corroboration' not in result.stdout:
        fail('--check says it is not a fresh corroboration', result.stdout)
        return
    report('--check passes on the committed tree and names its own limits')

    dockerfile = repo.read(DOCKERFILE).decode()
    repo.write(DOCKERFILE, dockerfile.replace(f'ARG AGENT_VERSION_AGY={VERSION}',
                                              f'ARG AGENT_VERSION_AGY={OLDER}').encode())
    result = run_writer(repo, '--check')
    if result.returncode == 0:
        fail('--check refuses an ARG that disagrees with the pin', 'it passed')
    else:
        report('--check refuses an ARG that disagrees with the pin')


def case_transport_failure(work: Path) -> None:
    """A failed manifest fetch publishes nothing."""
    case = work / 'transport'
    repo = Repository(case / 'repo')
    assets = Assets(case / 'assets', Upstream())
    before = repo.tree()
    result = run_writer(repo, '--version', VERSION, assets=assets,
                        environment={'FIXTURE_AGY_FAIL_URL':
                                     'https://antigravity-cli-auto-updater-974169037036.'
                                     'us-central1.run.app/manifests/linux_arm64.json'})
    expect_refusal('a failed manifest fetch publishes nothing', result, 'HTTP 500',
                   repo, before)


def main() -> int:
    if len(sys.argv) > 1:
        return fixture_main(sys.argv[1:])
    work = Path(tempfile.mkdtemp(prefix='pin-agy-maintenance.')).resolve()
    try:
        case_decision_table(work)
        case_arm64_checked_against_its_own_manifest(work)
        case_wrong_elf_arch(work)
        case_member_mode(work)
        case_committed_second_platform(work)
        case_proposal_only_tamper(work)
        case_latest_only_and_rewrite(work)
        case_repeat_is_byte_identical(work)
        case_adoption_and_dockerfile(work)
        case_ci_and_origins(work)
        case_module_binding(work)
        case_clean_process(work)
        case_publication_fault(work)
        case_recovery(work)
        case_check(work)
        case_transport_failure(work)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    if failures:
        print(f'pin-agy-maintenance: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print('pin-agy-maintenance: all cases passed')
    return 0


def write_fixture(directory: Path, version: str) -> int:
    """Publish one release into a directory another suite can check in as fixtures.

    `DIR/source/` holds the committed closure (the pin and its three evidence files)
    and `DIR/assets/` the bytes a fixture transport serves for that release. Both
    come from a real writer run, so a suite that checks this closure in is holding
    the bytes production publishes rather than a hand-written lookalike.
    """
    directory = directory.resolve()
    if (directory / 'source').exists() or (directory / 'assets').exists():
        print(f'pin-agy fixture: {directory} already holds a fixture', file=sys.stderr)
        return 1
    work = Path(tempfile.mkdtemp(prefix='pin-agy-fixture.')).resolve()
    try:
        repo = Repository(work / 'repo', arg=version)
        assets = Assets(work / 'assets', Upstream(version))
        result = run_writer(repo, '--version', version, assets=assets)
        if result.returncode != 0:
            print(f'pin-agy fixture: the writer refused {version}: '
                  f'{result.stderr.strip()}', file=sys.stderr)
            return 1
        shutil.copytree(repo.closure(), directory / 'source' / PIN_DIRECTORY)
        (directory / 'assets').mkdir(parents=True)
        for name in Upstream(version).files():
            shutil.copyfile(assets.path / name, directory / 'assets' / name)
    finally:
        shutil.rmtree(work, ignore_errors=True)
    print(f'pin-agy fixture: agy {version} written to {directory}')
    return 0


def fixture_main(argv: list[str]) -> int:
    """`--write-fixture DIR [--version X.Y.Z]`: no cases, just fixture bytes."""
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--write-fixture')
    parser.add_argument('--version', default=VERSION)
    arguments = parser.parse_args(argv)
    if not arguments.write_fixture:
        print('usage: pin-agy-maintenance.py [--write-fixture DIR [--version X.Y.Z]]',
              file=sys.stderr)
        return 2
    return write_fixture(Path(arguments.write_fixture), arguments.version)


if __name__ == '__main__':
    sys.exit(main())
