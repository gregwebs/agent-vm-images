#!/usr/bin/env python3
"""Black-box evidence for the agy owner writer (#131, #182).

Every case drives the real program: `script/dev/pin-agy.sh` compiles
`./cmd/avm-pin` into the checkout's `target/` and execs `avm-pin agy "$@"`, which is
the whole trust decision for an artifact with no upstream signature. The compiled
binary is built once per run and then invoked as one process per case against a
fixture repository (a Dockerfile and, when a case needs one, a committed closure)
and a fixture upstream directory. Nothing here imports a production module, loads
`script/dev/pin-agy.py`, monkeypatches a writer or sets a `FIXTURE_AGY_*` seam.

Three seams are used, and no others:

* `--fixture-upstream DIR`, which serves `https://host/path` from `DIR/host/path`,
  so the writer's whole request set is answered from a directory this table wrote,
  and a refused case can leave a document out to prove nothing else was asked for;
* a fixture repository the writer publishes into, so a refusal is checked against
  every file and directory that existed before it;
* a real process under `TZ=Pacific/Kiritimati` and `TZ=Etc/GMT+12` for the pin
  date, because a host's local date only exists inside a real zone.

The bytes are synthesized here: real tar/gzip archives, real ELF headers, real
evidence digests. `--write-fixture DIR [--version X.Y.Z]` publishes one release
with the real writer and copies what it produced, so a suite that checks the
result in holds the bytes production publishes rather than a hand-written pin.

The cases that cannot be expressed this way live in Go instead, and each group has
exactly one owner:

* `internal/agywriter/render_test.go` — the rendered pin's byte shape (the
  payload's member/path spelling, the empty links array, the hash-only block and
  the reason text) and the post-render validation of a proposal whose evidence
  digest disagrees with the evidence bytes beside it;
* `internal/agywriter/writer_test.go` — the GitHub Actions refusal before any
  fetch (a counting fetcher), and the owner scratch retained and printed (status 3)
  or removed again (status 1) around a publication;
* `internal/publication/publish_test.go` — the publication fault, signal and
  restore cases: a partial write, a signal, and a refused restore.

Those four cases (`case_module_binding`, the transport-origin half of
`case_ci_and_origins`, `case_proposal_only_tamper` and `case_publication_fault` /
`case_recovery`) patched a Python module or injected an environment variable into a
function, and the writer is a binary now. The CI half of `case_ci_and_origins` is
still here: it is a real run with `GITHUB_ACTIONS=true`.
"""
from __future__ import annotations

import argparse
import datetime
import gzip
import hashlib
import io
import json
import os
import shutil
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path
from typing import Mapping

ROOT = Path(__file__).resolve().parents[2]
WRITER = ROOT / 'script' / 'dev' / 'pin-agy.sh'
REPOSITORY = 'google-antigravity/antigravity-cli'
MANIFEST_ORIGIN = ('https://antigravity-cli-auto-updater-974169037036.us-central1.run.app'
                   '/manifests/')
PIN_DIRECTORY = 'images/tools/agy'
PIN_FILE = f'{PIN_DIRECTORY}/pin.json'
DOCKERFILE = 'images/standard/Dockerfile'
VERSION = '1.4.11'
OLDER = '1.4.10'
NEWER = '1.4.12'
MACHINES = {'linux/amd64': (62, 'x64'), 'linux/arm64': (183, 'arm64')}
ARCH = {'linux/amd64': 'amd64', 'linux/arm64': 'arm64'}
# The five files one publication touches.
DESTINATIONS = [PIN_FILE,
                f'{PIN_DIRECTORY}/evidence/release.json',
                f'{PIN_DIRECTORY}/evidence/manifest-linux-amd64.json',
                f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json',
                DOCKERFILE]
# The two extreme zones differ from UTC in opposite directions, so between them
# every hour of the UTC day has one host whose local date differs from the UTC date.
CLOCK_ZONES = (('Pacific/Kiritimati', datetime.timedelta(hours=14)),
               ('Etc/GMT+12', datetime.timedelta(hours=-12)))

failures: list[str] = []
cases: list[str] = []
scratch_directories: list[Path] = []
_binary: Path | None = None


def report(label: str) -> None:
    print(f'ok: {label}')


def fail(label: str, detail: object) -> None:
    failures.append(f'{label}: {detail}')
    print(f'FAIL: {label}: {detail}', file=sys.stderr)


def run_case(label: str, function) -> None:
    try:
        function()
    except AssertionError as error:
        fail(label, str(error))
        return
    except Exception as error:  # noqa: BLE001 - every case reports, none aborts
        fail(label, f'{type(error).__name__}: {error}')
        return
    cases.append(label)


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def scratch(prefix: str) -> Path:
    directory = Path(tempfile.mkdtemp(prefix=prefix)).resolve()
    scratch_directories.append(directory)
    return directory


def clean_scratch() -> None:
    for directory in scratch_directories:
        shutil.rmtree(directory, ignore_errors=True)
    scratch_directories.clear()


# --- the real program --------------------------------------------------------

def writer_binary() -> Path:
    """The compiled writer, built once for the whole run.

    This is exactly what `script/dev/pin-agy.sh` builds (`go build -o
    target/avm-pin-bin/avm-pin ./cmd/avm-pin`, then `exec avm-pin agy`). Building it
    once here keeps the suite to one compile and never leaves a binary in the
    checkout; `case_maintainer_entry_point` still runs the wrapper itself.
    """
    global _binary
    if _binary is None:
        directory = scratch('pin-agy-binary.')
        path = directory / 'avm-pin'
        built = subprocess.run(['go', 'build', '-o', str(path), './cmd/avm-pin'],
                               cwd=str(ROOT), capture_output=True, text=True, check=False)
        require(built.returncode == 0, f'cannot build avm-pin: {built.stderr.strip()}')
        require(path.is_file(), f'the build wrote no binary: {path}')
        _binary = path
    return _binary


def writer_env(overrides: Mapping[str, str | None] | None = None) -> dict:
    """The environment a maintainer host would have, minus the CI marker.

    Every case here stands in for a maintainer host, and the aggregate normally runs
    on a runner, so an inherited `GITHUB_ACTIONS` would turn each writer case into the
    CI refusal. That refusal has its own case, which sets the marker explicitly.
    """
    environment = {key: value for key, value in os.environ.items()
                   if key not in ('GITHUB_ACTIONS', 'PYTHONPATH')}
    for name, value in (overrides or {}).items():
        if value is None:
            environment.pop(name, None)
        else:
            environment[name] = value
    return environment


def run_writer(repository: 'Repository', *arguments: str,
               upstream: Path | None = None,
               env: Mapping[str, str | None] | None = None,
               tmp: Path | None = None) -> subprocess.CompletedProcess:
    """One `avm-pin agy` process, with --root absolute and cwd outside both trees."""
    argv = [str(writer_binary()), 'agy', '--root', str(repository.path), *arguments]
    if upstream is not None:
        argv += ['--fixture-upstream', str(upstream)]
    overrides = {'TMPDIR': str(tmp if tmp is not None else repository.temporary())}
    overrides.update(env or {})
    return subprocess.run(argv, capture_output=True, text=True, check=False,
                          env=writer_env(overrides),
                          cwd=str(Path(tempfile.gettempdir())))


# --- fixture bytes -----------------------------------------------------------

def elf_header(machine: int) -> bytes:
    """An ELF64 little-endian header: the 20 bytes every reader here looks at."""
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


def release_api_url(version: str) -> str:
    return f'https://api.github.com/repos/{REPOSITORY}/releases/tags/{version}'


def asset_url(version: str, filename: str) -> str:
    return f'https://github.com/{REPOSITORY}/releases/download/{version}/{filename}'


def manifest_url(platform: str) -> str:
    return f'{MANIFEST_ORIGIN}linux_{ARCH[platform]}.json'


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha512(raw: bytes) -> str:
    return hashlib.sha512(raw).hexdigest()


def encode(document: dict) -> bytes:
    return (json.dumps(document, indent=2) + '\n').encode()


class Upstream:
    """One served release: both manifests, the GitHub release and both archives."""

    def __init__(self, version: str = VERSION, *, machines=None, bodies=None,
                 download_count: int = 4102) -> None:
        self.version = version
        self.download_count = download_count
        self.machines = dict(machines or {platform: machine
                                         for platform, (machine, _) in MACHINES.items()})
        self.bodies = dict(bodies or {platform: archive(self.machines[platform])
                                     for platform in MACHINES})
        self.manifest_versions = {platform: version for platform in MACHINES}
        self.manifest_sha512 = {platform: sha512(self.bodies[platform])
                                for platform in MACHINES}
        self.manifest_urls = {
            platform: f'https://storage.googleapis.com/antigravity-public/{version}/'
                      f'{asset_name(platform)}' for platform in MACHINES}
        self.release_digests = {asset_name(platform): 'sha256:' + sha256(raw)
                                for platform, raw in self.bodies.items()}
        self.release_tag = version
        self.draft = False
        self.drop_assets: set[str] = set()

    def documents(self) -> dict[str, bytes]:
        """The five served files, by their canonical asset (and file) names."""
        documents: dict[str, bytes] = {}
        for platform in MACHINES:
            documents[f'linux_{ARCH[platform]}.json'] = encode({
                'version': self.manifest_versions[platform],
                'url': self.manifest_urls[platform],
                'sha512': self.manifest_sha512[platform],
            })
            documents[asset_name(platform)] = self.bodies[platform]
        documents['release.json'] = encode({
            'tag_name': self.release_tag,
            'draft': self.draft,
            'immutable': False,
            'assets': [
                {'name': name, 'size': len(self.bodies[platform_of(name)]),
                 'digest': digest, 'download_count': self.download_count,
                 'uploader': {'login': 'fixture'}}
                for name, digest in self.release_digests.items()
                if name not in self.drop_assets],
        })
        return documents

    def served(self) -> dict[str, bytes]:
        """Every canonical URL this release answers, and its bytes."""
        documents = self.documents()
        served = {release_api_url(self.version): documents['release.json']}
        for platform in MACHINES:
            served[manifest_url(platform)] = documents[f'linux_{ARCH[platform]}.json']
            served[asset_url(self.version, asset_name(platform))] = \
                documents[asset_name(platform)]
        return served

    def write_served(self, directory: Path) -> Path:
        """The `--fixture-upstream` tree: `<host>/<path>` for every canonical URL."""
        directory = Path(directory)
        for url, raw in self.served().items():
            path = served_path(directory, url)
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(raw)
        return directory

    def write_assets(self, directory: Path) -> Path:
        """The same bytes by their asset names, for another suite's fixture."""
        directory = Path(directory)
        directory.mkdir(parents=True, exist_ok=True)
        for name, raw in self.documents().items():
            (directory / name).write_bytes(raw)
        return directory


def platform_of(name: str) -> str:
    for platform in MACHINES:
        if asset_name(platform) == name:
            return platform
    raise KeyError(name)


def served_path(directory: Path, url: str) -> Path:
    """Where `--fixture-upstream DIR` serves `url` from."""
    parts = url.split('://', 1)
    require(len(parts) == 2, f'not an absolute URL: {url}')
    return Path(directory) / parts[1]


# --- fixture repository ------------------------------------------------------

def dockerfile(arg: str = VERSION) -> str:
    return f'''ARG BASE_IMAGE=agent-vm-base:local
FROM ${{BASE_IMAGE}}
ARG AGENT_VERSION_AGY={arg}

LABEL org.agent-vm.version.agy="${{AGENT_VERSION_AGY}}"

RUN sh /tmp/install-agy.sh
'''


class Repository:
    """The repository a publication writes into: the Dockerfile and the pin directory.

    Those are the only two things an agy transaction reads from the tree, so the
    fixture carries exactly them: the writer is a compiled program that takes its
    bytes from `--fixture-upstream`, not a module copied out of this checkout and
    imported here.
    """

    def __init__(self, directory: Path, *, arg: str = VERSION) -> None:
        self.path = Path(directory)
        (self.path / DOCKERFILE).parent.mkdir(parents=True, exist_ok=True)
        (self.path / DOCKERFILE).write_text(dockerfile(arg))

    def read(self, relative: str) -> bytes:
        return (self.path / relative).read_bytes()

    def write(self, relative: str, raw: bytes) -> None:
        path = self.path / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)

    def install(self, closure: dict[str, bytes]) -> 'Repository':
        """Fold a golden publication's destinations into this repository."""
        for name in DESTINATIONS:
            if name in closure:
                self.write(name, closure[name])
        return self

    def tree(self) -> dict[str, bytes]:
        """Every source byte a publication may change."""
        found: dict[str, bytes] = {}
        for name in DESTINATIONS:
            path = self.path / name
            if path.is_file():
                found[name] = path.read_bytes()
        return found

    def state(self) -> tuple[dict[str, bytes], list[str]]:
        return directory_state(self.path)

    def closure(self) -> Path:
        return self.path / PIN_DIRECTORY

    def temporary(self) -> Path:
        """This case's TMPDIR, so a run's scratch directory is inside the case."""
        directory = self.path.parent / 'tmp'
        directory.mkdir(parents=True, exist_ok=True)
        return directory


def directory_state(directory: Path) -> tuple[dict[str, bytes], list[str]]:
    """Every file and directory below directory: what "no partial state" means."""
    files: dict[str, bytes] = {}
    directories: list[str] = []
    for path in sorted(Path(directory).rglob('*')):
        relative = str(path.relative_to(directory))
        if path.is_dir():
            directories.append(relative)
        else:
            files[relative] = path.read_bytes()
    return files, directories


# --- assertions --------------------------------------------------------------

def expect_refusal(label: str, result: subprocess.CompletedProcess, needle: str,
                   repository: Repository, before: tuple[dict[str, bytes], list[str]],
                   *, status: int = 1) -> bool:
    """A refusal is its status, its needle, and an unchanged tree."""
    if result.returncode != status:
        fail(label, f'expected status {status}, got {result.returncode}: '
                    f'{result.stdout.strip()} {result.stderr.strip()}')
        return False
    text = result.stdout + result.stderr
    if needle not in text:
        fail(label, f'the refusal does not mention {needle!r}: {result.stderr.strip()}')
        return False
    after = repository.state()
    if after != before:
        added = sorted(set(after[0]) - set(before[0]))
        removed = sorted(set(before[0]) - set(after[0]))
        changed = sorted(name for name in after[0]
                         if name in before[0] and after[0][name] != before[0][name])
        fail(label, f'the refusal changed the sources: added {added}, removed '
                    f'{removed}, changed {changed}, directories {after[1]}')
        return False
    report(label)
    return True


def published(repository: Repository, upstream: Upstream, case: Path,
              version: str = VERSION) -> dict[str, bytes]:
    """Publish one release into the fixture: a real run, then the tree it produced."""
    served = upstream.write_served(case / 'upstream')
    result = run_writer(repository, '--version', version, upstream=served)
    require(result.returncode == 0,
            f'cannot publish agy {version}: {result.stdout.strip()} '
            f'{result.stderr.strip()}')
    return repository.tree()


# --- cases -------------------------------------------------------------------

def case_publish_transcript(work: Path) -> None:
    """A first adoption, held to the frozen transcript and the five destinations.

    Maintainers and the other suites read these lines: the published summary names
    the destinations in sorted order, the second line names the file that must then
    be updated, and the owner scratch is named absolutely and removed by a run that
    published.
    """
    case = work / 'transcript'
    repository = Repository(case / 'repo')
    served = Upstream().write_served(case / 'upstream')
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    require(result.returncode == 0, f'the first adoption refused: {result.stderr.strip()}')
    lines = result.stdout.splitlines()
    require(lines[0] == (f'pin-agy: published agy {VERSION}: '
                         + ', '.join(sorted(DESTINATIONS))),
            f'the published line is {lines[0]!r}')
    require(any('pin-agy: now update script/test/fixtures/installer-egress/selections.tsv'
                in line for line in lines[1:]),
            f'the transcript never names the selections file: {lines}')
    require(len(lines) == 2, f'the published run printed {lines}')
    announced = [line for line in result.stderr.splitlines() if 'owner scratch: ' in line]
    require(len(announced) == 1,
            f'the run announced {len(announced)} scratch directories: {result.stderr!r}')
    directory = Path(announced[0].split('owner scratch: ', 1)[1].strip())
    require(directory.is_absolute(), f'the scratch is not absolute: {directory}')
    require(not directory.exists(), f'a published run kept its scratch: {directory}')
    require('retained' not in result.stderr, f'a published run retained material: {result.stderr}')
    require(sorted(repository.tree()) == sorted(DESTINATIONS),
            f'the publication wrote {sorted(repository.tree())}')
    require(repository.state() != before, 'the publication wrote nothing')
    require(json.loads(repository.read(PIN_FILE))['version'] == VERSION,
            'the pin does not name the published release')
    require(f'ARG AGENT_VERSION_AGY={VERSION}' in repository.read(DOCKERFILE).decode(),
            'the Dockerfile ARG was not written')
    report('a first adoption publishes the five files with the frozen transcript')


def case_maintainer_entry_point(work: Path) -> None:
    """`script/dev/pin-agy.sh --root DIR …` is the path CONTRIBUTING names.

    Every other case calls the binary the wrapper builds, so this is the one place
    the wrapper's own three lines (compile, `cd` to the checkout, `exec avm-pin agy`)
    are exercised.
    """
    case = work / 'wrapper'
    repository = Repository(case / 'repo')
    served = Upstream().write_served(case / 'upstream')
    finished = subprocess.run(['bash', str(WRITER), '--root', str(repository.path),
                               '--version', VERSION, '--fixture-upstream', str(served)],
                              capture_output=True, text=True, check=False,
                              env=writer_env({'TMPDIR': str(repository.temporary())}),
                              cwd=str(Path(tempfile.gettempdir())))
    require(finished.returncode == 0,
            f'the wrapper refused: {finished.stdout.strip()} {finished.stderr.strip()}')
    require(f'pin-agy: published agy {VERSION}:' in finished.stdout,
            f'the wrapper did not run the writer: {finished.stdout!r}')
    require(sorted(repository.tree()) == sorted(DESTINATIONS),
            f'the wrapper published {sorted(repository.tree())}')
    report('the maintainer wrapper compiles and runs `avm-pin agy`')


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
        repository = Repository(case / 'repo')
        upstream = Upstream()
        mutate(upstream)
        served = upstream.write_served(case / 'upstream')
        before = repository.state()
        result = run_writer(repository, '--version', VERSION, upstream=served)
        if needle is None:
            if result.returncode != 0:
                fail(label, result.stderr.strip())
                continue
            if sorted(repository.tree()) != sorted(DESTINATIONS):
                fail(label, f'published {sorted(repository.tree())}')
                continue
            if f'ARG AGENT_VERSION_AGY={VERSION}' not in repository.read(DOCKERFILE).decode():
                fail(label, 'the Dockerfile ARG was not written')
                continue
            report(label)
            continue
        expect_refusal(label, result, needle, repository, before)


def case_arm64_checked_against_its_own_manifest(work: Path) -> None:
    """A platform is only ever checked against its own manifest: swapping the
    amd64 sha512 into the arm64 manifest must not corroborate the arm64 archive."""
    case = work / 'cross-manifest'
    repository = Repository(case / 'repo')
    upstream = Upstream()
    other = Upstream()
    upstream.manifest_sha512['linux/arm64'] = other.manifest_sha512['linux/amd64']
    require(upstream.manifest_urls['linux/amd64'] != upstream.manifest_urls['linux/arm64'],
            'the fixture urls are equal, so the row proves nothing')
    served = upstream.write_served(case / 'upstream')
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    expect_refusal('a platform is checked against its own manifest only', result,
                   'manifest records sha512', repository, before)


def case_wrong_elf_arch(work: Path) -> None:
    """Consistent hashes on both channels, and the archive is the other machine."""
    case = work / 'wrong-elf'
    repository = Repository(case / 'repo')
    served = Upstream(machines={'linux/amd64': 183,
                                'linux/arm64': 183}).write_served(case / 'upstream')
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    expect_refusal('a wrong-architecture binary under consistent hashes is refused',
                   result, 'ELF machine 183', repository, before)


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
        repository = Repository(case / 'repo')
        bodies = {platform: archive(machine, mode=mode)
                  for platform, (machine, _) in MACHINES.items()}
        served = Upstream(bodies=bodies).write_served(case / 'upstream')
        before = repository.state()
        result = run_writer(repository, '--version', VERSION, upstream=served)
        expect_refusal(f'a {mode:04o} member is refused at pin time ({label})', result,
                       f'is mode {mode:04o}', repository, before)


def case_committed_second_platform(work: Path) -> None:
    """The committed corroboration is checked for the platform not being built.

    A loop narrowed to the first platform would let half a release drift, so each
    row tampers the arm64 record (and re-hashes it into the pin, or the row would
    only prove the evidence hash), then asks for a bump. The served tree has no
    archive at all, so the expected refusal can only be the corroboration: a payload
    fetch would report its own, different failure.
    """
    golden = work / 'second-platform-golden'
    golden_repository = Repository(golden / 'repo')
    committed = published(golden_repository, Upstream(VERSION), golden)

    def case_repository(directory: Path) -> Repository:
        return Repository(directory).install(committed)

    def retarget_pin(repository: Repository, mutate) -> None:
        """Re-hash the tampered evidence into the pin, so only the content check
        can refuse."""
        document = json.loads(repository.read(PIN_FILE))
        mutate(document)
        repository.write(PIN_FILE, encode(document))

    def rehash_manifest(repository: Repository, platform: str) -> None:
        relative = f'{PIN_DIRECTORY}/evidence/manifest-linux-{ARCH[platform]}.json'
        retarget_pin(repository, lambda pin: pin['authentication']['manifests'][platform]
                     .update({'sha256': sha256(repository.read(relative))}))

    def rehash_release(repository: Repository) -> None:
        relative = f'{PIN_DIRECTORY}/evidence/release.json'
        retarget_pin(repository, lambda pin: pin['authentication']['release']
                     .update({'sha256': sha256(repository.read(relative))}))

    def rewrite(repository: Repository, relative: str, mutate) -> None:
        document = json.loads(repository.read(relative))
        mutate(document)
        repository.write(relative, encode(document))

    def strip_archives(served: Path) -> None:
        for platform in MACHINES:
            served_path(served, asset_url(VERSION, asset_name(platform))).unlink()

    case = work / 'second-platform-manifest'
    repository = case_repository(case / 'repo')
    rewrite(repository, f'{PIN_DIRECTORY}/evidence/manifest-linux-arm64.json',
            lambda document: document.update({'sha512': '0' * 128}))
    rehash_manifest(repository, 'linux/arm64')
    before = repository.state()
    served = Upstream(VERSION).write_served(case / 'upstream')
    strip_archives(served)
    result = run_writer(repository, '--version', VERSION, upstream=served)
    if expect_refusal('an arm64 manifest sha512 that disagrees with the pin is refused',
                      result, 'the pin says', repository, before):
        require('cannot fetch' not in result.stdout + result.stderr,
                'the corroboration was checked before any payload fetch')

    case = work / 'second-platform-release'
    repository = case_repository(case / 'repo')
    arm64 = asset_name('linux/arm64')

    def tamper(document: dict) -> None:
        for asset in document['assets']:
            if asset['name'] == arm64:
                asset['digest'] = 'sha256:' + '0' * 64

    rewrite(repository, f'{PIN_DIRECTORY}/evidence/release.json', tamper)
    rehash_release(repository)
    before = repository.state()
    served = Upstream(VERSION).write_served(case / 'upstream')
    strip_archives(served)
    result = run_writer(repository, '--version', VERSION, upstream=served)
    if expect_refusal('an arm64 release digest that disagrees with the pin is refused',
                      result, 'the pin says', repository, before):
        require('cannot fetch' not in result.stdout + result.stderr,
                'the corroboration was checked before any payload fetch')


def case_latest_only_and_rewrite(work: Path) -> None:
    """The committed state is consulted before any payload is downloaded."""
    golden = work / 'golden'
    golden_repository = Repository(golden / 'repo', arg=VERSION)
    committed = published(golden_repository, Upstream(VERSION), golden)

    case = work / 'older-manifest'
    repository = Repository(case / 'repo', arg=VERSION).install(committed)
    served = Upstream(OLDER).write_served(case / 'upstream')
    # Before the run, never after it: a tree read after the writer returned already
    # contains whatever the writer wrote, so comparing it with itself would pass
    # even when the refusal damaged the sources.
    before = repository.state()
    result = run_writer(repository, '--version', OLDER, upstream=served)
    expect_refusal('a manifest older than the pin is refused', result,
                   f'older than the pinned {VERSION}', repository, before)

    case = work / 'same-release-rewrite'
    repository = Repository(case / 'repo', arg=VERSION).install(committed)
    rewritten = Upstream(VERSION)
    rewritten.bodies['linux/amd64'] = archive(62, elf_header(62) + b'# rewritten\n')
    rewritten.manifest_sha512['linux/amd64'] = sha512(rewritten.bodies['linux/amd64'])
    rewritten.release_digests[asset_name('linux/amd64')] = (
        'sha256:' + sha256(rewritten.bodies['linux/amd64']))
    served = rewritten.write_served(case / 'upstream')
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    expect_refusal('a same-release upstream rewrite is refused', result,
                   'same-release upstream rewrite', repository, before)


def case_repeat_is_byte_identical(work: Path) -> None:
    """A second run of the same release keeps the committed evidence bytes, so a
    daily download-count change is not a diff and the date does not move."""
    case = work / 'repeat'
    repository = Repository(case / 'repo')
    served = Upstream(VERSION, download_count=4111).write_served(case / 'upstream')
    result = run_writer(repository, '--version', VERSION, upstream=served)
    require(result.returncode == 0, f'the first adoption refused: {result.stderr.strip()}')
    before = repository.state()

    # Same release, new download count on every asset: the security projection is
    # unchanged, so every committed byte must survive.
    Upstream(VERSION, download_count=9999).write_served(case / 'upstream')
    result = run_writer(repository, '--version', VERSION, upstream=case / 'upstream')
    require(result.returncode == 0, f'a repeat run refused: {result.stderr.strip()}')
    require('unchanged' in result.stdout,
            f'a repeat run did not report unchanged: {result.stdout.strip()}')
    require(repository.state() == before, 'a repeat run changed the tree')
    report('a repeat run is byte-identical and reports unchanged')

    # One platform's manifest is served in another encoding but parses to the same
    # document: every committed evidence byte must still be kept verbatim.
    reformatted = Upstream(VERSION, download_count=9999)
    served = reformatted.write_served(case / 'upstream')
    document = json.loads((served_path(served, manifest_url('linux/amd64'))).read_bytes())
    served_path(served, manifest_url('linux/amd64')).write_bytes(
        json.dumps(document, indent=4).encode() + b'\n')
    result = run_writer(repository, '--version', VERSION, upstream=served)
    require(result.returncode == 0,
            f'a reformatted manifest was refused: {result.stderr.strip()}')
    require(repository.state() == before,
            'a reformatted manifest changed the committed evidence')
    require('unchanged' in result.stdout,
            f'a reformatted manifest is not a change: {result.stdout.strip()}')
    report('a reformatted manifest keeps every committed evidence byte')


def case_clock(work: Path) -> None:
    """`verified_on` is the UTC date, and a repeat keeps the committed date.

    Kiritimati is UTC+14 and Etc/GMT+12 is UTC-12, so at least one of them always
    has a local date other than the UTC date. Running both is what makes a writer
    that stamps the host's local date fail at every hour instead of at some of them.
    """
    utc_now = datetime.datetime.now(datetime.timezone.utc)
    differing = [zone for zone, offset in CLOCK_ZONES
                 if (utc_now + offset).date() != utc_now.date()]
    require(differing != [], f'neither zone has a local date other than the UTC date: '
                             f'{[zone for zone, _ in CLOCK_ZONES]}')

    for zone, _offset in CLOCK_ZONES:
        slug = f'clock-{zone.replace("/", "-")}'
        case = work / slug
        repository = Repository(case / 'repo')
        served = Upstream().write_served(case / 'upstream')
        before = datetime.datetime.now(datetime.timezone.utc).date()
        result = run_writer(repository, '--version', VERSION, upstream=served,
                            env={'TZ': zone})
        after = datetime.datetime.now(datetime.timezone.utc).date()
        require(result.returncode == 0,
                f'a writer under TZ={zone} refused: {result.stderr.strip()}')
        stamped = json.loads(repository.read(PIN_FILE))['authentication']['verified_on']
        # A run that straddles UTC midnight legitimately stamps either date.
        require(stamped in (before.isoformat(), after.isoformat()),
                f'a writer under TZ={zone} stamped {stamped}, not the UTC date {after}')
        report(f'a writer under TZ={zone} stamps the UTC date {after}')

    # A same-release repeat keeps the committed `verified_on` instead of moving it to
    # today's UTC date, so a UTC stamp cannot perturb a byte-identical re-pin.
    case = work / 'clock-repeat'
    repository = Repository(case / 'repo')
    served = Upstream(VERSION, download_count=4111).write_served(case / 'upstream')
    result = run_writer(repository, '--version', VERSION, upstream=served,
                        env={'TZ': differing[0]})
    require(result.returncode == 0, f'the repeat fixture refused: {result.stderr.strip()}')
    raw = repository.read(PIN_FILE)
    published_on = json.loads(raw)['authentication']['verified_on']
    field = f'"verified_on": "{published_on}"'.encode()
    require(raw.count(field) == 1,
            f'the committed pin carries {raw.count(field)} verified_on fields')
    pinned_on = '2026-01-02'
    repository.write(PIN_FILE, raw.replace(field, f'"verified_on": "{pinned_on}"'.encode()))
    before = repository.state()
    Upstream(VERSION, download_count=9999).write_served(case / 'upstream')
    result = run_writer(repository, '--version', VERSION, upstream=case / 'upstream',
                        env={'TZ': differing[0]})
    require(result.returncode == 0,
            f'a dated same-release repeat refused: {result.stderr.strip()}')
    require('unchanged' in result.stdout,
            f'a dated same-release repeat is not a change: {result.stdout.strip()}')
    require(repository.state() == before, 'a dated same-release repeat changed the tree')
    report(f'a same-release repeat under TZ={differing[0]} keeps verified_on {pinned_on}')


def case_adoption_and_dockerfile(work: Path) -> None:
    """A partial old state, and the three Dockerfile shapes.

    Every row snapshots the sources BEFORE the writer runs, so a write made just
    before the refusal is a failure rather than the comparison's own baseline.
    """
    case = work / 'partial'
    repository = Repository(case / 'repo')
    repository.write(f'{PIN_DIRECTORY}/evidence/release.json', b'{}\n')
    served = Upstream().write_served(case / 'upstream')
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    expect_refusal('a partial committed directory is refused', result,
                   'with no pin.json', repository, before)

    for label, text, needle in (
            ('no ARG', dockerfile().replace('ARG AGENT_VERSION_AGY=', 'ARG OTHER='),
             'exactly one `ARG AGENT_VERSION_AGY='),
            ('two ARGs', dockerfile() + '\nARG AGENT_VERSION_AGY=0.0.1\n',
             'exactly one `ARG AGENT_VERSION_AGY='),
            ('no label', dockerfile().replace(
                'LABEL org.agent-vm.version.agy="${AGENT_VERSION_AGY}"',
                'LABEL org.agent-vm.version.other="x"'),
             'LABEL org.agent-vm.version.agy')):
        slug = label.replace(' ', '-')
        case = work / f'dockerfile-{slug}'
        repository = Repository(case / 'repo')
        (repository.path / DOCKERFILE).write_text(text)
        served = Upstream().write_served(case / 'upstream')
        before = repository.state()
        result = run_writer(repository, '--version', VERSION, upstream=served)
        expect_refusal(f'the Dockerfile with {label} is refused', result, needle,
                       repository, before)


def case_ci(work: Path) -> None:
    """A write run refuses CI before its first fetch, and `--check` still reads.

    The write row is served an empty tree, so a fetch before the refusal would fail
    with its own, different message; the served directory is also compared, because
    a refusal that wrote into it did not happen before the request.
    """
    case = work / 'ci'
    repository = Repository(case / 'repo')
    published(repository, Upstream(), case)
    before = repository.state()

    empty = case / 'empty-upstream'
    empty.mkdir()
    served_before = (dict(), [])
    result = run_writer(repository, '--version', NEWER, upstream=empty,
                        env={'GITHUB_ACTIONS': 'true'})
    if expect_refusal('a write run refuses GitHub Actions', result,
                      'refusing GitHub Actions', repository, before):
        require('cannot fetch' not in result.stdout + result.stderr,
                'a fetch was attempted before the CI refusal')
        require(directory_state(empty) == served_before,
                'a refused CI run wrote into the served tree')

    # --check is the offline reader: CI may run it, and the marker changes nothing.
    checked = run_writer(repository, '--check', env={'GITHUB_ACTIONS': 'true'})
    require(checked.returncode == 0,
            f'--check refused the committed tree in CI ({checked.returncode}): '
            f'{checked.stderr.strip()}')
    require('offline committed-input check' in checked.stdout,
            f'--check did not report the offline check in CI: {checked.stdout!r}')
    require('refusing GitHub Actions' not in checked.stderr,
            'the CI refusal blocked the read-only check')
    report('--check works in CI')


def case_clean_process(work: Path) -> None:
    """`--check` in a bare process: no PYTHONPATH, no repository on the path.

    The writer is a binary, so there is nothing to bind. What this still holds is
    that the check reads only the committed bytes: a mutated evidence byte is a
    refusal, not a warning.
    """
    case = work / 'clean'
    repository = Repository(case / 'repo')
    published(repository, Upstream(), case)
    result = run_writer(repository, '--check')
    require(result.returncode == 0,
            f'--check refused a clean process ({result.returncode}): {result.stderr.strip()}')
    require('offline committed-input check' in result.stdout and
            'not a fresh corroboration' in result.stdout,
            f'--check did not name its own limits: {result.stdout!r}')
    report('--check passes in a bare process with no PYTHONPATH')

    evidence = repository.path / f'{PIN_DIRECTORY}/evidence/manifest-linux-amd64.json'
    original = evidence.read_bytes()
    evidence.write_bytes(original.replace(b'"version": "1.4.11"', b'"version": "1.4.10"'))
    try:
        result = run_writer(repository, '--check')
        if result.returncode == 0:
            fail('--check refuses a mutated evidence byte', 'it passed')
        elif 'the pin says' not in (result.stdout + result.stderr):
            fail('--check refuses a mutated evidence byte',
                 (result.stdout + result.stderr).strip())
        else:
            report('--check refuses a mutated evidence byte')
    finally:
        evidence.write_bytes(original)


def case_check(work: Path) -> None:
    """`--check` on the committed tree, and the ARG it must catch."""
    case = work / 'check'
    repository = Repository(case / 'repo')
    published(repository, Upstream(), case)
    result = run_writer(repository, '--check')
    require(result.returncode == 0,
            f'a committed tree was refused ({result.returncode}): {result.stderr.strip()}')
    require('offline committed-input check' in result.stdout,
            f'--check did not report the offline check: {result.stdout!r}')
    require('not a fresh corroboration' in result.stdout,
            f'--check did not name its own limit: {result.stdout!r}')
    report('--check passes on the committed tree and names its own limits')

    dockerfile = repository.read(DOCKERFILE).decode()
    repository.write(DOCKERFILE, dockerfile.replace(f'ARG AGENT_VERSION_AGY={VERSION}',
                                                    f'ARG AGENT_VERSION_AGY={OLDER}').encode())
    result = run_writer(repository, '--check')
    if result.returncode == 0:
        fail('--check refuses an ARG that disagrees with the pin', 'it passed')
    elif result.stdout.strip():
        fail('a refused check reported the closure', result.stdout.strip())
    elif f'ARG AGENT_VERSION_AGY={VERSION}' not in result.stderr:
        fail('--check refuses an ARG that disagrees with the pin', result.stderr.strip())
    else:
        report('--check refuses an ARG that disagrees with the pin')


def case_transport_failure(work: Path) -> None:
    """A failed manifest fetch publishes nothing, and names the URL it asked for."""
    case = work / 'transport'
    repository = Repository(case / 'repo')
    served = Upstream().write_served(case / 'upstream')
    missing = manifest_url('linux/arm64')
    served_path(served, missing).unlink()
    before = repository.state()
    result = run_writer(repository, '--version', VERSION, upstream=served)
    expect_refusal('a failed manifest fetch publishes nothing', result,
                   f'cannot fetch {missing}', repository, before)


# --- driver ------------------------------------------------------------------

def fixture_main(argv: list[str]) -> int:
    """`--write-fixture DIR [--version X.Y.Z]`: no cases, just fixture bytes."""
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--write-fixture')
    parser.add_argument('--version', default=VERSION)
    try:
        arguments = parser.parse_args(argv)
    except SystemExit:
        print('usage: pin-agy-maintenance.py [--write-fixture DIR [--version X.Y.Z]]',
              file=sys.stderr)
        return 2
    if not arguments.write_fixture:
        print('usage: pin-agy-maintenance.py [--write-fixture DIR [--version X.Y.Z]]',
              file=sys.stderr)
        return 2
    try:
        return write_fixture(Path(arguments.write_fixture), arguments.version)
    except AssertionError as error:
        print(f'pin-agy fixture: {error}', file=sys.stderr)
        return 1


def write_fixture(directory: Path, version: str) -> int:
    """Publish one release through the real writer into a directory a suite checks in.

    `DIR/source/` holds the committed closure at the same relative path production
    uses (`images/tools/agy/pin.json` plus its three evidence files) and `DIR/assets/`
    the served upstream documents by their canonical asset and file names. Both come
    from a real writer run, so a suite that checks this closure in is holding the
    bytes production publishes rather than a hand-written lookalike.
    """
    directory = directory.resolve()
    if (directory / 'source').exists() or (directory / 'assets').exists():
        print(f'pin-agy fixture: {directory} already holds a fixture', file=sys.stderr)
        return 1
    work = scratch('pin-agy-fixture.')
    try:
        repository = Repository(work / 'repo', arg=version)
        upstream = Upstream(version)
        served = upstream.write_served(work / 'upstream')
        result = run_writer(repository, '--version', version, upstream=served)
        if result.returncode != 0:
            print(f'pin-agy fixture: the writer refused {version}: '
                  f'{result.stderr.strip()}', file=sys.stderr)
            return 1
        shutil.copytree(repository.closure(), directory / 'source' / PIN_DIRECTORY)
        upstream.write_assets(directory / 'assets')
    finally:
        clean_scratch()
    print(f'pin-agy fixture: agy {version} written to {directory}')
    return 0


CASES = [
    ('a first adoption publishes the five files with the frozen transcript',
     case_publish_transcript),
    ('the maintainer wrapper runs `avm-pin agy`', case_maintainer_entry_point),
    ('the decision table', case_decision_table),
    ('a platform is checked against its own manifest', case_arm64_checked_against_its_own_manifest),
    ('a wrong-architecture binary is refused', case_wrong_elf_arch),
    ('the archive member modes', case_member_mode),
    ('the committed second platform is corroborated', case_committed_second_platform),
    ('latest-only and the same-release rewrite', case_latest_only_and_rewrite),
    ('a repeat run is byte-identical', case_repeat_is_byte_identical),
    ('the pin date is the UTC date', case_clock),
    ('adoption and the Dockerfile shapes', case_adoption_and_dockerfile),
    ('the CI refusal precedes the fetch', case_ci),
    ('--check in a bare process', case_clean_process),
    ('--check on the committed tree', case_check),
    ('a failed fetch publishes nothing', case_transport_failure),
]


def main(argv: list[str]) -> int:
    if argv:
        return fixture_main(argv)
    work = scratch('pin-agy-maintenance.')
    try:
        for label, function in CASES:
            run_case(label, lambda case=function: case(work))
    finally:
        clean_scratch()
    if failures:
        print(f'pin-agy-maintenance: {len(failures)} case(s) failed', file=sys.stderr)
        return 1
    print(f'pin-agy-maintenance: {len(cases)} case(s) passed')
    print('pin-agy-maintenance: all cases passed')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
