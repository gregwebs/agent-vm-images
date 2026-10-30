#!/usr/bin/env python3
"""Maintainer-host refresh of the one committed agy selection (#131).

This is the only writer of `images/tools/agy/pin.json`, the three files under
`images/tools/agy/evidence/` and the `ARG AGENT_VERSION_AGY` default in
`images/standard/Dockerfile`. Ordinary builds only verify those bytes: they fetch
the one computed release URL, re-check the committed evidence against the pin and
accept only the committed digests.

Google publishes no signature or attestation for these assets, so this tool is
the whole of the trust decision and it is deliberately narrow: a version is
pinnable only when the downloaded bytes equal **both** GitHub's asset digest and
the sha512 in Google's updater manifest, and only when that manifest names the
same release the run was asked for. The manifest names only the latest release,
so an older version cannot be re-pinned and a same-release upstream rewrite is
refused rather than silently adopted. Everything is then published as one
transaction; a partial upstream failure leaves every committed byte identical.

Status: 0 published or unchanged, 1 failed with the sources unchanged or fully
restored, 2 usage, 3 undecided or incomplete recovery (the retained directories
are printed), 128+N after a signal whose rollback succeeded.
"""
from __future__ import annotations

import argparse
import datetime
import hashlib
import importlib.util
import json
import os
import shutil
import sys
import tarfile
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping

# Loading the shared model must not leave `__pycache__` in the checkout: these are
# base inputs whose identity `bash script/build/base-inputs.sh --check` compares.
sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parents[2]

PIN_DIRECTORY = 'images/tools/agy'
PIN_FILE = f'{PIN_DIRECTORY}/pin.json'
DOCKERFILE = 'images/standard/Dockerfile'
AGY_ARG = 'AGENT_VERSION_AGY'
AGY_LABEL = 'org.agent-vm.version.agy'
# Every origin this writer asks: the GitHub API, the GitHub release host and
# Google's updater-manifest host. The redirect policy is the shared owner
# transport's, not a per-origin one: it approves a redirect only to GitHub's own
# asset hosts, and it does so for any approved origin - including the manifest
# host, which therefore is not prevented from redirecting.
ORIGINS = frozenset({
    'api.github.com',
    'github.com',
    'antigravity-cli-auto-updater-974169037036.us-central1.run.app',
})
MAX_JSON_BYTES = 1 << 20

AGY_REASON = (
    'Google publishes no signature or attestation for these assets and the GitHub '
    'release is mutable and uploaded by an individual account. The pin is '
    'hash-only: at pin time the downloaded bytes had to equal both GitHub\'s asset '
    'digest and the sha512 in Google\'s updater manifest, which names only the '
    'latest release. The build re-checks the committed evidence against the pin '
    'and accepts only the committed digests. See ADR 0012.')


class Refusal(Exception):
    """A prerequisite, an upstream response or a staged proposal is unacceptable."""


# --- module loading ----------------------------------------------------------
#
# The selection model, the installer and the transactional publisher are loaded
# from the repository under maintenance rather than from this file's directory, so
# a fixture repository exercises the real modules and production has exactly one
# copy of each.
#
# The model and the installer are registered under their *plain* names because
# `install_artifact` does `import artifact_pin` / `from artifact_pin import
# Refusal`: an alias would leave the installer with a missing module, or with a
# second `Refusal` class that no `except` here would catch.

_models: dict[str, object] = {}
# Set by `bind_models`; the module-level `https_fetch` is the seam the hermetic
# suite replaces, exactly as `pin-codex.py` exposes its own transport.
_transport: object | None = None


def _exec_module(name: str, path: Path) -> object:
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which a bare
    # module_from_spec does not populate (Python 3.12 raises there).
    sys.modules[name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(name, None)
        raise
    return module


def _bind(path: Path, name: str) -> object:
    """Bind one module under its plain name, refusing to rebind a different one."""
    path = path.resolve()
    existing = sys.modules.get(name)
    if existing is not None:
        source = getattr(existing, '__file__', None)
        if source is None or Path(source).resolve() != path:
            raise Refusal(f'{name} is already bound to {source}, not {path}; '
                          'refusing to rebind the shared model')
        return existing
    if not path.is_file():
        raise Refusal(f'missing {path}')
    return _exec_module(name, path)


def _load(path: Path, name: str) -> object:
    """Load a module nothing imports by name, through a per-path alias."""
    path = Path(path).resolve()
    cached = _models.get(str(path))
    if cached is not None:
        return cached
    if not path.is_file():
        raise Refusal(f'missing {path}')
    module = _exec_module(f'{name}_{abs(hash(str(path)))}', path)
    _models[str(path)] = module
    return module


def bind_models(repo_root: Path) -> tuple[object, object, object, object]:
    """Bind the model once, then everything that imports it by name.

    Returns (artifact_pin, install_artifact, publisher, transport). Call this
    before any other load: `install_artifact` and `pin-artifact.py` both import
    `artifact_pin`, and whichever runs first decides which object they get.
    """
    repo_root = Path(repo_root)
    artifacts = repo_root / 'images' / 'artifacts'
    model = _bind(artifacts / 'artifact_pin.py', 'artifact_pin')
    installer = _bind(artifacts / 'install_artifact.py', 'install_artifact')
    publisher = _load(repo_root / 'script' / 'dev' / 'pin-artifact.py', 'pin_artifact')
    transport = _load(repo_root / 'script' / 'dev' / 'pin-codex.py', 'pin_codex')
    # One model, one Refusal: a second copy would make the top-level handler miss
    # a refusal the installer raised.
    if installer.artifact_pin is not model or publisher.artifact_pin is not model:
        raise Refusal('the shared model was loaded twice; refusing a split binding')
    if installer.Refusal is not model.Refusal:
        raise Refusal('install_artifact has a different Refusal class than the model')
    global _transport
    _transport = transport
    return model, installer, publisher, transport


# --- injected seams ----------------------------------------------------------

Fetcher = Callable[[str, Path, int], None]


def https_fetch(url: str, output: Path, limit: int) -> None:
    """The one network primitive: the owner transport with this owner's origins.

    A module-level function so the hermetic suite can replace exactly the network
    and nothing else, the way `pin-codex.py` is tested.
    """
    if _transport is None:
        raise Refusal('the transport is not bound; call bind_models first')
    _transport.https_fetch(url, output, limit, origins=ORIGINS)


@dataclass(frozen=True)
class Manifest:
    """One parsed, validated updater manifest."""
    version: str
    url: str
    sha512: str


@dataclass(frozen=True)
class Corroborated:
    """One platform after both channels agreed with the bytes."""
    platform: str
    archive: Path
    sha256: str
    sha512: str


@dataclass(frozen=True)
class StagedAgy:
    """The publication bytes, keyed by repository-relative destination."""
    files: Mapping[str, bytes]
    version: str


# --- reading upstream --------------------------------------------------------

def _stream_digests(path: Path) -> tuple[str, str]:
    """sha256 and sha512 of one file, read once."""
    both = (hashlib.sha256(), hashlib.sha512())
    with path.open('rb') as handle:
        while True:
            chunk = handle.read(1 << 16)
            if not chunk:
                break
            for hasher in both:
                hasher.update(chunk)
    return both[0].hexdigest(), both[1].hexdigest()


def _parse_manifest(model, raw: bytes, platform: str) -> Manifest:
    context = f'agy {platform} manifest'
    members = model.parse_manifest(raw, context)
    version = members['version']
    if not model.VERSION.fullmatch(version):
        raise Refusal(f'{context}: expected an exact X.Y.Z version, got {version!r}')
    return Manifest(version=version, url=members['url'], sha512=members['sha512'])


def _read_json(path: Path, what: str) -> bytes:
    raw = path.read_bytes()
    if len(raw) > MAX_JSON_BYTES:
        raise Refusal(f'{what} is larger than the {MAX_JSON_BYTES} byte ceiling')
    return raw


def _fetch_manifests(model, version: str, work: Path, fetch: Fetcher) -> dict[str, Manifest]:
    """Both updater manifests, compared on their `version` fields only.

    Each architecture keeps its own `url` and `sha512`: they necessarily differ, and
    a platform's archive is only ever checked against its own manifest.
    """
    manifests: dict[str, Manifest] = {}
    for platform in model.PLATFORMS:
        output = work / f'manifest-linux-{model.PLATFORMS[platform]}.json'
        fetch(model.manifest_url(platform), output, MAX_JSON_BYTES)
        manifests[platform] = _parse_manifest(model, _read_json(output, platform), platform)
    versions = {platform: manifest.version for platform, manifest in manifests.items()}
    for platform, found in versions.items():
        if found != version:
            other = ', '.join(f'{name} {value}' for name, value in versions.items())
            raise Refusal(f'the updater manifests name {other}; only the release they '
                          f'currently name can be pinned (asked for {version})')
    return manifests


def _fetch_release(model, version: str, work: Path, fetch: Fetcher) -> bytes:
    output = work / 'release.json'
    fetch(model.AGY_RELEASE_API + version, output, MAX_JSON_BYTES)
    return _read_json(output, 'release metadata')


def _corroborate(model, version: str, platform: str, work: Path,
                 manifests: Mapping[str, Manifest], digests: Mapping[str, str],
                 fetch: Fetcher) -> Corroborated:
    """Download one archive and require both channels to name its bytes."""
    filename = model.expected_filename('agy', version, platform)
    archive = work / filename
    fetch(model.canonical_url('agy', version, platform), archive,
          model.MAX_ARCHIVE_BYTES['agy'])
    sha256, sha512 = _stream_digests(archive)
    expected = digests.get(filename)
    if expected is None:
        raise Refusal(f'GitHub does not list {filename} for {version}')
    if expected != f'sha256:{sha256}':
        raise Refusal(f'GitHub records {expected} for {filename}, but the downloaded '
                      f'bytes are sha256:{sha256}')
    if manifests[platform].sha512 != sha512:
        raise Refusal(f'Google\'s {platform} manifest records sha512 '
                      f'{manifests[platform].sha512} for {filename}, but the '
                      f'downloaded bytes are {sha512}')
    return Corroborated(platform=platform, archive=archive, sha256=sha256, sha512=sha512)


def _check_archive(model, installer, pin, corroborated: Corroborated) -> None:
    """Preflight the reviewed archive against the proposal pin.

    The same checks the build runs, from the same code: preflight (members, links,
    setuid), the exact member set and decoded ceiling, and the ELF machine read
    straight out of the archive instead of extracting 234 MB to look at 20 bytes.
    """
    platform = corroborated.platform
    machine = model.ELF_MACHINES['agy'][platform]
    prefix = model.expected_member_prefix('agy', corroborated.archive.name)
    with tarfile.open(corroborated.archive, 'r:gz') as tar:
        members = installer.preflight(tar, prefix)
        installer.check_members(pin, members)
        named = {installer.canonical_member(member): member for member in members}
        for member, published in pin.install.payload:
            found = named.get(member)
            if found is None:
                raise Refusal(f'the archive carries no {member} to publish')
            source = tar.extractfile(found)
            if source is None:
                raise Refusal(f'the archive member is unreadable: {member}')
            installer.check_elf(pin.name, published, source.read(installer.ELF_HEADER_BYTES),
                                machine, platform)


# --- rendering ---------------------------------------------------------------

def _release_projection(model, raw: bytes) -> tuple:
    """The part of the GitHub release a substitution would have to change.

    Download counts, ids and timestamps change on every fetch of the same tag; the
    tag and the asset names with their digests are the security projection.
    """
    value = model.strict_json(raw, 'release metadata')
    if not isinstance(value, dict):
        raise Refusal('release metadata is not a JSON object')
    assets = value.get('assets')
    if not isinstance(assets, list):
        raise Refusal('release metadata has no assets array')
    projection = []
    seen = set()
    for entry in assets:
        if not isinstance(entry, dict):
            raise Refusal('release metadata has an asset that is not an object')
        name = entry.get('name')
        if not isinstance(name, str) or not name:
            raise Refusal('release metadata has an asset with no name')
        if name in seen:
            raise Refusal(f'release metadata repeats the asset {name!r}')
        seen.add(name)
        projection.append((name, entry.get('digest')))
    return (value.get('tag_name'), tuple(sorted(projection)))


def render_pin(model, version: str, release: bytes,
               archives: Mapping[str, Corroborated], manifest_bytes: Mapping[str, bytes],
               verified_on: str) -> str:
    pin = {
        'schema': model.SCHEMA,
        'name': 'agy',
        'kind': 'release-archive',
        'version': version,
        'upstream': {'project': f'https://github.com/{model.AGY_REPOSITORY}',
                     'release': model.release_tag('agy', version)},
        'install': {'destination': '/usr/local',
                    'payload': [{'member': member, 'path': path}
                                for member, path in model.AGY_PAYLOAD],
                    'links': []},
        'platforms': {
            platform: {
                'url': model.canonical_url('agy', version, platform),
                'archive': 'tar.gz',
                'member_prefix': model.expected_member_prefix(
                    'agy', model.expected_filename('agy', version, platform)),
                'sha256': archives[platform].sha256,
                'sha512': archives[platform].sha512,
            } for platform in model.PLATFORMS
        },
        'authentication': {
            'method': 'hash-only',
            'release': {'url': model.AGY_RELEASE_API + version,
                        'path': model.AGY_EVIDENCE['release'],
                        'sha256': model.sha256(release)},
            'manifests': {
                platform: {'url': model.manifest_url(platform),
                           'path': model.AGY_EVIDENCE[platform],
                           'sha256': model.sha256(manifest_bytes[platform])}
                for platform in model.PLATFORMS
            },
            'verified_on': verified_on,
            'reason': AGY_REASON,
        },
    }
    return json.dumps(pin, indent=2) + '\n'


LABEL_LINE = f'LABEL {AGY_LABEL}="${{{AGY_ARG}}}"'


def require_single_agy_slot(dockerfile: str, version: str) -> None:
    """Exactly one agy ARG at this version, and a label that reads it."""
    args = [line for line in dockerfile.split('\n') if line.startswith(f'ARG {AGY_ARG}=')]
    if args != [f'ARG {AGY_ARG}={version}']:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {AGY_ARG}={version}`, '
                      f'found {args}')
    if LABEL_LINE not in dockerfile.split('\n'):
        raise Refusal(f'the Dockerfile must declare `{LABEL_LINE}`')


def render_agy_arg(dockerfile: str, version: str) -> str:
    """The reviewed `ARG AGENT_VERSION_AGY` default, on exactly one line."""
    lines = dockerfile.split('\n')
    matches = [index for index, line in enumerate(lines)
               if line.startswith(f'ARG {AGY_ARG}=')]
    if len(matches) != 1:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {AGY_ARG}={version}`, '
                      f'found {len(matches)}')
    lines[matches[0]] = f'ARG {AGY_ARG}={version}'
    rendered = '\n'.join(lines)
    require_single_agy_slot(rendered, version)
    return rendered


# --- staging -----------------------------------------------------------------

def _committed(model, repo_root: Path):
    """The validated committed pin, or None on a clean first adoption.

    A half-populated directory is refused rather than repaired: this writer
    publishes a reviewed selection, it does not make a broken one look better.
    """
    directory = Path(repo_root) / PIN_DIRECTORY
    if not (directory / 'pin.json').is_file():
        leftovers = sorted(path.name for path in directory.iterdir()) if directory.is_dir() else []
        if leftovers:
            raise Refusal(f'{directory} holds {leftovers} with no pin.json; refusing '
                          'a partial old state')
        return None
    pin = model.load(directory / 'pin.json')
    model.check_corroboration(pin)
    return pin


def _version_key(version: str) -> tuple[int, ...]:
    return tuple(int(part) for part in version.split('.'))


def _destination(relative: str) -> str:
    return relative if relative == DOCKERFILE else f'{PIN_DIRECTORY}/{relative}'


def _stage_files(model, version: str, release: bytes,
                 archives: Mapping[str, Corroborated], dockerfile: str,
                 verified_on: str, manifest_bytes: Mapping[str, bytes]) -> dict[str, bytes]:
    files = {
        _destination('pin.json'): render_pin(model, version, release, archives,
                                             manifest_bytes, verified_on).encode(),
        _destination(model.AGY_EVIDENCE['release']): release,
        _destination(DOCKERFILE): dockerfile.encode(),
    }
    for platform in model.PLATFORMS:
        files[_destination(model.AGY_EVIDENCE[platform])] = manifest_bytes[platform]
    return files


def _write_proposal(files: Mapping[str, bytes], directory: Path) -> None:
    for relative, raw in files.items():
        path = directory / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)


def stage_agy(version: str, repo_root: Path, work: Path, *,
              fetch: Fetcher) -> StagedAgy:
    """Resolve, corroborate, preflight and render one release. Writes only in `work`."""
    repo_root = Path(repo_root)
    work = Path(work)
    model, installer, _publisher, _transport_module = bind_models(repo_root)
    if not model.VERSION.fullmatch(version):
        raise Refusal(f'--version must be an exact X.Y.Z version, got {version}')

    # 1. Both manifests, compared on their version fields only. This is the
    #    latest-only gate: the manifest names one release, so that is the release
    #    a run may pin.
    manifests = _fetch_manifests(model, version, work, fetch)
    manifest_bytes = {platform: _read_json(
        work / f'manifest-linux-{model.PLATFORMS[platform]}.json', platform)
        for platform in model.PLATFORMS}

    # 2. The committed state, before a payload is downloaded: a manifest that names
    #    a release OLDER than the pin is a refusal, not a downgrade. A newer one is
    #    the normal forward bump and continues.
    committed = _committed(model, repo_root)
    if committed is not None and _version_key(committed.version) > _version_key(version):
        raise Refusal(f'the updater manifest names {version}, which is older than the '
                      f'pinned {committed.version}')

    # 3. The exact release, and its per-asset digests.
    release = _fetch_release(model, version, work, fetch)
    digests = model.release_asset_digests(
        model.strict_json(release, 'release metadata'), 'agy', version)

    # 4. The payload, accepted only when both channels name these bytes.
    archives = {platform: _corroborate(model, version, platform, work, manifests,
                                       digests, fetch)
                for platform in model.PLATFORMS}

    same_release = committed is not None and committed.version == version
    verified_on = datetime.date.today().isoformat()
    if same_release:
        for platform in model.PLATFORMS:
            archive = archives[platform]
            pinned = committed.archives[platform]
            if (archive.sha256, archive.sha512) != (pinned.digest, pinned.sha512):
                raise Refusal(f'{archive.archive.name} changed for the already-pinned '
                              f'{version}; a same-release upstream rewrite needs a '
                              'separate reviewed adoption')
        # Download counts change daily; keeping the committed bytes when the
        # security projection is unchanged is what makes a repeat run print
        # `unchanged` instead of a churn-only diff.
        committed_release = repo_root / PIN_DIRECTORY / model.AGY_EVIDENCE['release']
        committed_raw = committed_release.read_bytes()
        if _release_projection(model, release) == _release_projection(model, committed_raw):
            release = committed_raw
        for platform in model.PLATFORMS:
            committed_manifest = (repo_root / PIN_DIRECTORY
                                  / model.AGY_EVIDENCE[platform]).read_bytes()
            if _parse_manifest(model, committed_manifest, platform) == manifests[platform]:
                manifest_bytes[platform] = committed_manifest
        verified_on = committed.authentication.verified_on

    # 5. The proposal bytes, with the ARG rewritten and the label required.
    dockerfile = render_agy_arg((repo_root / DOCKERFILE).read_text(), version)
    files = _stage_files(model, version, release, archives, dockerfile,
                         verified_on, manifest_bytes)

    # 6. Validate exactly the bytes that are then published: the production loader
    #    over a private copy, its corroboration check, the digests from step 4, and
    #    the archive's own shape read from the downloaded file.
    proposal = work / 'proposal'
    _write_proposal(files, proposal)
    loaded = model.load(proposal / PIN_FILE)
    model.check_corroboration(loaded)
    if loaded.version != version:
        raise Refusal(f'the proposal pin selects {loaded.version}, not {version}')
    for platform, archive in loaded.archives.items():
        corroborated = archives[platform]
        if (archive.digest, archive.sha512) != (corroborated.sha256, corroborated.sha512):
            raise Refusal(f'the proposal pin records a different identity for {platform} '
                          'than the bytes this run corroborated')
        _check_archive(model, installer, loaded, corroborated)
    return StagedAgy(files=files, version=version)


def unchanged(repo_root: Path, staged: StagedAgy) -> bool:
    """True when every publication byte already equals the committed byte."""
    for destination, raw in staged.files.items():
        path = Path(repo_root) / destination
        if not path.is_file() or path.read_bytes() != raw:
            return False
    return True


# --- commands ----------------------------------------------------------------

def check_committed(repo_root: Path) -> int:
    """Offline check of the committed pin, its evidence and the single ARG."""
    repo_root = Path(repo_root)
    model, _installer, _publisher, _transport_module = bind_models(repo_root)
    if not (repo_root / PIN_FILE).is_file():
        raise Refusal(f'{repo_root / PIN_FILE} is missing: there is no committed '
                      'agy selection to check')
    pin = model.load(repo_root / PIN_FILE)
    model.check_corroboration(pin)
    dockerfile = (repo_root / DOCKERFILE).read_text()
    # The committed ARG must already be the pin's version: `--check` reports, it
    # does not prepare a rewrite.
    require_single_agy_slot(dockerfile, pin.version)
    print(f'agy {pin.version}: offline committed-input check '
          '(committed bytes only; not a fresh corroboration)')
    for platform, archive in pin.archives.items():
        print(f'  {platform} {archive.filename} {archive.digest}')
    print(f'  evidence: {len(model.AGY_EVIDENCE)} files, all agreeing with the pin')
    print(f'  {DOCKERFILE}: ARG {AGY_ARG}={pin.version}, LABEL {AGY_LABEL} references it')
    return 0


def announce(what: str, directory: Path) -> None:
    """Say where a directory is before a publication that may exit from a signal.

    The publisher can `os._exit(3)` from a signal handler, so anything printed
    after it returns is lost; the line is therefore flushed first and the same
    absolute path is handed to the publisher, whose handler repeats it.
    """
    print(f'pin-agy: {what} {Path(os.path.abspath(directory))}', file=sys.stderr)
    sys.stderr.flush()


def retain(what: str, directory: Path) -> None:
    """Print, and keep, a directory a status-3 recovery depends on."""
    directory = Path(os.path.abspath(directory))
    print(f'pin-agy: {what} {directory}', file=sys.stderr)
    print(f'STOP: recover {directory} together with the publisher directory above '
          'before further edits/builds/commits', file=sys.stderr)


def refuse_ci() -> None:
    if os.environ.get('GITHUB_ACTIONS') == 'true':
        raise Refusal('refusing GitHub Actions: pins are maintainer-host maintenance only')


def _usage(message: str) -> None:
    print(f'pin-agy: {message}', file=sys.stderr)
    sys.exit(2)


USAGE = """usage: python3 script/dev/pin-agy.py --version X.Y.Z [--repo-root DIR]
       python3 script/dev/pin-agy.py --check [--repo-root DIR]

--version X.Y.Z  require Google's per-architecture updater manifests to name that
                 release, require the downloaded bytes for both architectures to
                 equal both GitHub's asset digests and that platform's manifest
                 sha512, preflight both archives, then publish the pin, its three
                 evidence files and the `ARG AGENT_VERSION_AGY` default as one
                 transaction. Only the release the manifest currently names can
                 be pinned.
--check          validate the committed pin, its evidence and the single agy
                 ARG/label; offline, writes nothing, is not a fresh corroboration.

Status: 0 published or unchanged, 1 failed with sources unchanged or restored,
2 usage, 3 incomplete recovery (the retained directories are printed).
"""


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--version')
    parser.add_argument('--repo-root')
    parser.add_argument('--check', action='store_true')
    parser.add_argument('--help', action='store_true')
    arguments = parser.parse_args(argv)
    if arguments.help:
        print(USAGE, end='')
        return 0
    keep_work = False
    work: Path | None = None
    # The loader, the installer and the transport each declare their own Refusal;
    # a corrupt input or a refused connection is a refusal whichever module
    # noticed it first, so every class this process can load is in the tuple.
    refusals: list = [Refusal]
    try:
        repo_root = Path(arguments.repo_root or ROOT)
        model, _installer, publisher, transport = bind_models(repo_root)
        refusals.extend([model.Refusal, transport.Refusal])
        if arguments.check:
            if arguments.version:
                _usage('--check takes only --repo-root')
            return check_committed(repo_root)
        refuse_ci()
        if not arguments.version:
            _usage('--version is required for a write run')
        version = arguments.version
        work = Path(tempfile.mkdtemp(prefix='pin-agy.work.', dir=os.environ.get('TMPDIR')))
        staged = stage_agy(version, repo_root, work, fetch=https_fetch)
        if unchanged(repo_root, staged):
            print(f'pin-agy: unchanged (agy {version})')
            return 0
        # A signal-driven rollback exits from inside the publisher, so the owner
        # scratch is named before publication starts, not after it.
        announce('owner scratch:', work)
        status = publisher.publish(dict(staged.files), root=repo_root, owner=str(work))
        if status == 3:
            keep_work = True
            retain('owner scratch retained at', work)
            return status
        if status == 0:
            print(f'pin-agy: published agy {version}: '
                  + ', '.join(sorted(staged.files)))
            print('pin-agy: now update script/test/fixtures/installer-egress/selections.tsv '
                  f'to agy {version}, then run: bash script/test/contracts.sh')
        return status
    except Exception as error:  # noqa: BLE001 - the single top-level handler
        if isinstance(error, tuple(refusals)):
            print(f'pin-agy: {error}', file=sys.stderr)
            return 1
        raise
    finally:
        if not keep_work and work is not None:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
