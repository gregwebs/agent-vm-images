#!/usr/bin/env python3
"""Maintainer-host refresh of the one committed Claude native payload closure (#98).

This is the only writer of `images/tools/claude/pin.json`, the two files under
`images/tools/claude/evidence/` and the `ARG AGENT_VERSION_CLAUDE` default in
`images/standard/Dockerfile`. It never writes the release key, the hooks or
`claude_pin.py`: the key is committed by hand so a key change is always a
reviewed diff. Ordinary builds only verify those bytes: they fetch the one
computed executable URL, compare it with the committed pin and accept only the
committed digests.

One run fetches the signed manifest, its detached signature and the published
release key, requires the published key to be byte-identical to the committed
one, requires the committed key's single primary to be the reviewed signer,
verifies the signature over the manifest, downloads both maintained Linux
executables and requires each to match the signed manifest and to be an ELF for
its machine, then publishes the pin, both evidence files and the ARG default as
one transaction with an auditable payload delta. A partial upstream failure
leaves every committed byte identical. The tool never commits, never runs under
GitHub Actions and never adopts a newer version by itself.

Every *initial* request goes to `https://downloads.claude.ai` through the shared
owner transport. That transport follows a redirect only to
`release-assets.githubusercontent.com` or `objects.githubusercontent.com` and
refuses every other target; the two asset hosts are tolerated because the
transport is shared with the Codex and agy writers and a redirect cannot change
what is accepted here -- the manifest and signature only count when the committed
key verifies them, the key only when it is byte-identical to the committed one,
and an executable only when its SHA-256 and size equal the signed manifest.
`downloads.claude.ai` does not redirect at all today, and the build's egress gate
denies every redirect outright (docs/adr/0013-claude-native-payload-pin.md).

Status: 0 published or unchanged, 1 failed with the sources unchanged or fully
restored, 2 usage, 3 undecided or incomplete recovery (the retained directories
are printed), 128+N after a signal whose rollback succeeded.
"""
from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import shutil
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Callable, Mapping

# Loading the shared model must not leave `__pycache__` in the checkout: these are
# base inputs whose identity `bash script/build/base-inputs.sh --check` compares.
sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parents[2]

PIN_DIRECTORY = 'images/tools/claude'
PIN_FILE = f'{PIN_DIRECTORY}/pin.json'
DOCKERFILE = 'images/standard/Dockerfile'
CLAUDE_ARG, CLAUDE_LABEL = 'AGENT_VERSION_CLAUDE', 'org.agent-vm.version.claude'
# Initial requests only. The shared transport may follow a redirect from here to
# release-assets.githubusercontent.com or objects.githubusercontent.com (its
# REDIRECT_ORIGINS); every byte is still accepted only by the committed key, the
# signature or the signed manifest digest, so the serving host cannot change what
# is adopted. The build's egress denies redirects outright (ADR 0013).
ORIGINS = frozenset({'downloads.claude.ai'})
READ_CHUNK_BYTES = 1 << 20

CLAUDE_REASON = (
    'The manifest is the selection authority: the build verifies the detached '
    'OpenPGP signature over the committed manifest.json with the committed release '
    'key, requires the pinned primary fingerprint, and refuses unless the signed '
    'checksum and size for the platform equal the committed ones before the '
    'executable is fetched. The executable is placed in the native layout and '
    '`claude install` runs only with no network, so the native installer\'s own '
    'downloads never happen. See ADR 0013.')

LABEL_LINE = f'LABEL {CLAUDE_LABEL}="${{{CLAUDE_ARG}}}"'


class Refusal(Exception):
    """A prerequisite, an upstream response or a staged proposal is unacceptable."""


# --- module loading ----------------------------------------------------------
#
# The selection model, the shared publisher and the shared transport are loaded
# from the repository under maintenance rather than from this file's directory, so
# a fixture repository exercises the real modules and production has exactly one
# copy of each. The model is bound under its *plain* name because the hooks and
# `claude_pin.py`'s own CLI refer to it that way, and two copies would mean two
# `Refusal` classes.

_models: dict[str, object] = {}
_model: object | None = None
_publisher: object | None = None
# Set by `bind_models`; the module-level `https_fetch` is the seam the hermetic
# suite replaces, exactly as `pin-agy.py` and `pin-codex.py` expose theirs.
_transport: object | None = None


def _exec_module(name: str, path: Path) -> object:
    spec = importlib.util.spec_from_file_location(name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules (Python 3.12
    # raises otherwise).
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


def bind_models(repo_root: Path) -> tuple[object, object, object]:
    """Bind (claude_pin, publisher, transport) from `repo_root`."""
    repo_root = Path(repo_root)
    model = _bind(repo_root / PIN_DIRECTORY / 'claude_pin.py', 'claude_pin')
    publisher = _load(repo_root / 'script' / 'dev' / 'pin-artifact.py', 'pin_artifact')
    transport = _load(repo_root / 'script' / 'dev' / 'pin-codex.py', 'pin_codex')
    global _model, _publisher, _transport
    _model, _publisher, _transport = model, publisher, transport
    return model, publisher, transport


# --- injected seam ------------------------------------------------------------

Fetcher = Callable[[str, Path, int], None]


def https_fetch(url: str, output: Path, limit: int) -> None:
    """The one network primitive: the shared owner transport with this owner's origins.

    A module-level function so the hermetic suite can replace exactly the network
    and nothing else.
    """
    if _transport is None:
        raise Refusal('the transport is not bound; call bind_models first')
    _transport.https_fetch(url, output, limit, origins=ORIGINS)


@dataclass(frozen=True)
class Fetched:
    """The three committed evidence candidates, as upstream served them."""
    manifest: bytes
    signature: bytes
    published_key: bytes


@dataclass(frozen=True)
class Downloaded:
    token: str
    path: Path
    sha256: str
    size: int


@dataclass(frozen=True)
class StagedClaude:
    """The publication bytes, keyed by repository-relative destination."""
    files: Mapping[str, bytes]
    version: str
    delta: str


# --- reading upstream ---------------------------------------------------------

def _read_bounded(path: Path, limit: int, what: str) -> bytes:
    raw = Path(path).read_bytes()
    if len(raw) > limit:
        raise Refusal(f'{what} is larger than the {limit} byte ceiling')
    return raw


def _stream_digest(path: Path) -> tuple[str, int]:
    hasher = hashlib.sha256()
    total = 0
    with Path(path).open('rb') as handle:
        while True:
            chunk = handle.read(READ_CHUNK_BYTES)
            if not chunk:
                break
            total += len(chunk)
            hasher.update(chunk)
    return hasher.hexdigest(), total


def _fetch_evidence(model, version: str, work: Path, fetch: Fetcher) -> Fetched:
    targets = {
        'manifest': (model.manifest_url(version), 'manifest.json'),
        'signature': (model.signature_url(version), 'manifest.json.sig'),
        'published_key': (model.KEY_URL, 'claude-code.asc'),
    }
    raw = {}
    for field, (url, name) in targets.items():
        output = work / name
        fetch(url, output, model.MAX_EVIDENCE_BYTES)
        raw[field] = _read_bounded(output, model.MAX_EVIDENCE_BYTES, url)
    return Fetched(**raw)


def _download_executables(model, version: str, work: Path, fetch: Fetcher,
                          manifest) -> dict[str, Downloaded]:
    """Both maintained executables, each accepted only as the manifest's bytes."""
    downloaded: dict[str, Downloaded] = {}
    for token in model.TOKENS:
        url = model.canonical_executable_url(version, token)
        path = work / f'claude-{token}'
        fetch(url, path, model.MAX_EXECUTABLE_BYTES)
        sha256, size = _stream_digest(path)
        signed = manifest.executables.get(token)
        if signed is None:
            raise Refusal(f'the signed manifest has no {token} platform')
        if (sha256, size) != signed:
            raise Refusal(f'the downloaded {token} executable is {sha256} / '
                          f'{size} bytes, but the signed manifest records '
                          f'{signed[0]} / {signed[1]} bytes')
        model.check_elf(path, token)
        downloaded[token] = Downloaded(token=token, path=path, sha256=sha256, size=size)
    return downloaded


# --- rendering ---------------------------------------------------------------

def render_pin(model, version: str, manifest: bytes, signature: bytes,
               key_bytes: bytes, executables: Mapping[str, Downloaded],
               verified_on: str) -> str:
    document = {
        'schema': model.SCHEMA,
        'name': model.NAME,
        'kind': model.KIND,
        'version': version,
        'manifest': {'url': model.manifest_url(version),
                     'path': model.MANIFEST_PATH,
                     'sha256': _sha256(manifest)},
        'signature': {'url': model.signature_url(version),
                      'path': model.SIGNATURE_PATH,
                      'sha256': _sha256(signature)},
        'key': {'url': model.KEY_URL,
                'path': model.KEY_PATH,
                'fingerprint': model.REVIEWED_SIGNER_FINGERPRINT,
                'sha256': _sha256(key_bytes)},
        'platforms': {
            platform: {'manifest_platform': token,
                       'url': model.canonical_executable_url(version, token),
                       'sha256': executables[token].sha256,
                       'size': executables[token].size}
            for platform, token in model.PLATFORMS.items()
        },
        'authentication': {'method': model.AUTHENTICATION_METHOD,
                           'verified_on': verified_on,
                           'reason': CLAUDE_REASON},
    }
    return json.dumps(document, indent=2) + '\n'


def _sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def require_single_claude_slot(dockerfile: str, version: str) -> None:
    """Exactly one Claude ARG at this version, and a label that reads it."""
    args = [line for line in dockerfile.split('\n') if line.startswith(f'ARG {CLAUDE_ARG}=')]
    if args != [f'ARG {CLAUDE_ARG}={version}']:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {CLAUDE_ARG}='
                      f'{version}`, found {args}')
    if LABEL_LINE not in dockerfile.split('\n'):
        raise Refusal(f'the Dockerfile must declare `{LABEL_LINE}`')


def render_claude_arg(dockerfile: str, version: str) -> str:
    """The reviewed `ARG AGENT_VERSION_CLAUDE` default, on exactly one line."""
    lines = dockerfile.split('\n')
    matches = [index for index, line in enumerate(lines)
               if line.startswith(f'ARG {CLAUDE_ARG}=')]
    if len(matches) != 1:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {CLAUDE_ARG}='
                      f'{version}`, found {len(matches)}')
    lines[matches[0]] = f'ARG {CLAUDE_ARG}={version}'
    rendered = '\n'.join(lines)
    require_single_claude_slot(rendered, version)
    return rendered


def _transition(old: object, new: object) -> str:
    return f'{new}' if old == new else f'{old} -> {new}'


def payload_delta(model, committed, proposed) -> str:
    """What the review is being asked to accept, field by field, before it lands."""
    new = proposed.pin
    old = committed.pin if committed is not None else None
    lines = [f'claude payload delta: {old.version if old else "(none)"} -> {new.version}']
    lines.append(f'  manifest     sha256 '
                 f'{_transition(old.manifest.sha256 if old else "(none)", new.manifest.sha256)}')
    lines.append(f'               commit '
                 f'{_transition(committed.manifest.commit if committed else "(none)", proposed.manifest.commit)}'
                 f'   buildDate '
                 f'{_transition(committed.manifest.build_date if committed else "(none)", proposed.manifest.build_date)}')
    lines.append(f'  signature    primary '
                 f'{_transition(committed.signature.primary_fingerprint if committed else "(none)", proposed.signature.primary_fingerprint)}'
                 f'   created '
                 f'{_transition(committed.signature.created if committed else "(none)", proposed.signature.created)}')
    for platform, token in model.PLATFORMS.items():
        before = old.executables[token] if old is not None else None
        after = new.executables[token]
        left = f'{before.sha256} {before.size} B' if before else '(none)'
        lines.append(f'  {platform:12} {left} -> {after.sha256} {after.size} B')
    return '\n'.join(lines) + '\n'


# --- staging -----------------------------------------------------------------

def _committed(model, directory: Path):
    """The validated committed closure, or None on a clean first adoption.

    A half-populated closure is refused rather than repaired: this writer
    publishes a reviewed selection, it does not make a broken one look better.
    Only the closure's own files count -- the directory also holds the hooks and
    the vendor tree.
    """
    if not (directory / 'pin.json').is_file():
        if (directory / 'evidence').exists():
            leftovers = sorted(path.name for path in (directory / 'evidence').iterdir())
            raise Refusal(f'{directory}/evidence holds {leftovers} with no pin.json; '
                          'refusing a partial old state')
        return None
    pin = model.load(directory)
    return model.verify(pin, pin.version)


def _write_proposal(files: Mapping[str, bytes], key: bytes, model, directory: Path) -> None:
    for relative, raw in files.items():
        path = directory / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(raw)
    pin_directory = directory / PIN_DIRECTORY
    (pin_directory / model.KEY_PATH).write_bytes(key)
    shutil.copyfile(model.__file__, pin_directory / 'claude_pin.py')


def stage_claude(version: str, repo_root: Path, work: Path, *,
                 fetch: Fetcher) -> StagedClaude:
    """Resolve, verify, preflight and render one release. Writes only in `work`."""
    repo_root = Path(repo_root)
    work = Path(work)
    model, _publisher_module, _transport_module = bind_models(repo_root)
    version = model.require_version(version, '--version')
    directory = repo_root / PIN_DIRECTORY

    # 1. The committed key is the trust anchor and is never written by this tool.
    key_path = directory / model.KEY_PATH
    if not key_path.is_file():
        raise Refusal(f'{key_path} is missing: the release key is committed by hand '
                      'before the first adoption')
    key_bytes = key_path.read_bytes()
    with model.gpg_home() as environment:
        primaries = model.key_primaries(key_path, environment)
    if primaries != [model.REVIEWED_SIGNER_FINGERPRINT]:
        raise Refusal(f'the committed release key must hold exactly the reviewed '
                      f'primary {model.REVIEWED_SIGNER_FINGERPRINT}, found {primaries}')

    # 2. The evidence, from the computed URLs only.
    fetched = _fetch_evidence(model, version, work, fetch)
    if fetched.published_key != key_bytes:
        raise Refusal(f'the published release key changed; review it and replace '
                      f'{model.KEY_PATH} by hand')

    # 3. The signature over the manifest, under the reviewed signer, and the signed
    #    manifest's own selection.
    model.verify_signature(key_bytes, model.REVIEWED_SIGNER_FINGERPRINT,
                           fetched.manifest, fetched.signature)
    manifest = model.parse_manifest(fetched.manifest)
    if manifest.version != version:
        raise Refusal(f'the signed manifest names {manifest.version}, not the '
                      f'requested {version}')

    # 4. The committed state, then the payload.
    committed = _committed(model, directory)
    downloaded = _download_executables(model, version, work, fetch, manifest)

    # 5. A same-release upstream rewrite is a separate reviewed adoption, never a
    #    silent re-pin. An identical repeat keeps the committed `verified_on` so
    #    the run prints `unchanged` and touches nothing. The stamp is the UTC date
    #    from the reader's own definition, so a maintainer east of UTC cannot write
    #    a date that every UTC-or-west builder then refuses as future.
    same_release = committed is not None and committed.pin.version == version
    verified_on = model.utc_today().isoformat()
    if same_release:
        if fetched.manifest != model.evidence_bytes(
                committed.pin, committed.pin.manifest.path,
                committed.pin.manifest.sha256):
            raise Refusal(f'the signed manifest changed for the already-pinned '
                          f'{version}; a same-release upstream rewrite needs a '
                          'separate reviewed adoption')
        if fetched.signature != model.evidence_bytes(
                committed.pin, committed.pin.signature.path,
                committed.pin.signature.sha256):
            raise Refusal(f'the detached signature changed for the already-pinned '
                          f'{version}; a same-release upstream rewrite needs a '
                          'separate reviewed adoption')
        for platform, token in model.PLATFORMS.items():
            pinned = committed.pin.executables[token]
            if (downloaded[token].sha256, downloaded[token].size) != (pinned.sha256,
                                                                      pinned.size):
                raise Refusal(f'the {platform} executable changed for the '
                              f'already-pinned {version}; a same-release upstream '
                              'rewrite needs a separate reviewed adoption')
        verified_on = committed.pin.verified_on.isoformat()

    # 6. The proposal bytes, with the ARG rewritten and the label required.
    dockerfile = render_claude_arg((repo_root / DOCKERFILE).read_text(), version)
    pin_raw = render_pin(model, version, fetched.manifest, fetched.signature,
                         key_bytes, downloaded, verified_on)
    files = {
        PIN_FILE: pin_raw.encode(),
        f'{PIN_DIRECTORY}/{model.MANIFEST_PATH}': fetched.manifest,
        f'{PIN_DIRECTORY}/{model.SIGNATURE_PATH}': fetched.signature,
        DOCKERFILE: dockerfile.encode(),
    }

    # 7. Validate exactly the bytes that are then published: the production loader
    #    over a private copy, the signer policy, and the identities this run
    #    downloaded.
    proposal = work / 'proposal'
    _write_proposal(files, key_bytes, model, proposal)
    loaded = model.load(proposal / PIN_DIRECTORY)
    verified = model.verify(loaded, version)
    model.check_policy(loaded)
    for platform, token in model.PLATFORMS.items():
        pinned = verified.pin.executables[token]
        if (pinned.sha256, pinned.size) != (downloaded[token].sha256,
                                            downloaded[token].size):
            raise Refusal(f'the proposal pin records a different identity for '
                          f'{platform} than the bytes this run downloaded')
    return StagedClaude(files=files, version=version,
                        delta=payload_delta(model, committed, verified))


def unchanged(repo_root: Path, staged: StagedClaude) -> bool:
    """True when every publication byte already equals the committed byte."""
    for destination, raw in staged.files.items():
        path = Path(repo_root) / destination
        if not path.is_file() or path.read_bytes() != raw:
            return False
    return True


# --- commands ----------------------------------------------------------------

def check_committed(repo_root: Path) -> int:
    """Offline check of the committed closure, its signature and the single ARG."""
    repo_root = Path(repo_root)
    model, _publisher_module, _transport_module = bind_models(repo_root)
    directory = repo_root / PIN_DIRECTORY
    if not (directory / 'pin.json').is_file():
        raise Refusal(f'{directory / "pin.json"} is missing: there is no committed '
                      'Claude selection to check')
    pin = model.load(directory)
    verified = model.verify(pin, pin.version)
    model.check_policy(pin)
    require_single_claude_slot((repo_root / DOCKERFILE).read_text(), pin.version)
    print(f'claude {pin.version}: offline committed-input check '
          '(signature verified offline; not a fresh fetch)')
    for platform, token in model.PLATFORMS.items():
        pinned = verified.pin.executables[token]
        print(f'  {platform} {token} {pinned.sha256} {pinned.size}')
    print(f'  signer {verified.signature.primary_fingerprint} '
          f'(signature created {verified.signature.created}), manifest commit '
          f'{verified.manifest.commit}')
    print(f'  {DOCKERFILE}: ARG {CLAUDE_ARG}={pin.version}, LABEL {CLAUDE_LABEL} '
          'references it')
    return 0


def announce(what: str, directory: Path) -> None:
    """Say where a directory is before a publication that may exit from a signal."""
    print(f'pin-claude: {what} {Path(os.path.abspath(directory))}', file=sys.stderr)
    sys.stderr.flush()


def retain(what: str, directory: Path) -> None:
    directory = Path(os.path.abspath(directory))
    print(f'pin-claude: {what} {directory}', file=sys.stderr)
    print(f'STOP: recover {directory} together with the publisher directory above '
          'before further edits/builds/commits', file=sys.stderr)


def refuse_ci() -> None:
    if os.environ.get('GITHUB_ACTIONS') == 'true':
        raise Refusal('refusing GitHub Actions: pins are maintainer-host maintenance only')


def _usage(message: str) -> None:
    print(f'pin-claude: {message}', file=sys.stderr)
    sys.exit(2)


USAGE = """usage: python3 script/dev/pin-claude.py --version X.Y.Z [--repo-root DIR]
       python3 script/dev/pin-claude.py --check [--repo-root DIR]

--version X.Y.Z  fetch the signed manifest, its detached signature and the
                 published key from downloads.claude.ai, require the published key
                 to equal the committed one and the committed key's primary to be
                 the reviewed signer, verify the signature, download both Linux
                 executables, require each to match the signed manifest and to be
                 an ELF for its machine, then publish the pin, both evidence files
                 and the `ARG AGENT_VERSION_CLAUDE` default as one transaction.
                 Prints the payload delta; a same-release rewrite is refused.
--check          validate the committed pin, its evidence, the signature, the
                 reviewed signer and the single ARG/label; offline, writes nothing.

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
    # The model, the publisher and the transport each declare their own Refusal;
    # a corrupt input or a refused connection is a refusal whichever module
    # noticed it first, so every class this process can load is in the tuple.
    refusals: list = [Refusal]
    try:
        repo_root = Path(arguments.repo_root or ROOT)
        model, publisher, transport = bind_models(repo_root)
        refusals.extend([model.Refusal, transport.Refusal])
        if arguments.check:
            if arguments.version:
                _usage('--check takes only --repo-root')
            return check_committed(repo_root)
        refuse_ci()
        if not arguments.version:
            _usage('--version is required for a write run')
        version = arguments.version
        work = Path(tempfile.mkdtemp(prefix='pin-claude.work.',
                                    dir=os.environ.get('TMPDIR')))
        staged = stage_claude(version, repo_root, work, fetch=https_fetch)
        if unchanged(repo_root, staged):
            print(f'pin-claude: unchanged (claude {version})')
            return 0
        print(staged.delta, end='')
        # A signal-driven rollback exits from inside the publisher, so the owner
        # scratch is named before publication starts, not after it.
        announce('owner scratch:', work)
        status = publisher.publish(dict(staged.files), root=repo_root, owner=str(work))
        if status == 3:
            keep_work = True
            retain('owner scratch retained at', work)
            return status
        if status == 0:
            print(f'pin-claude: published claude {version}: '
                  + ', '.join(sorted(staged.files)))
            print('pin-claude: now update script/test/fixtures/installer-egress/'
                  f'selections.tsv to claude {version}, then run: '
                  'bash script/test/contracts.sh')
        return status
    except Exception as error:  # noqa: BLE001 - the single top-level handler
        if isinstance(error, tuple(refusals)):
            print(f'pin-claude: {error}', file=sys.stderr)
            return 1
        raise
    finally:
        if not keep_work and work is not None:
            shutil.rmtree(work, ignore_errors=True)


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
