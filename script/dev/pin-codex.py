#!/usr/bin/env python3
"""Maintainer-host refresh of the one committed Codex payload closure (#97).

This is the only writer of `images/tools/codex/pin.json`, the four files under
`images/tools/codex/evidence/` and the `ARG AGENT_VERSION_CODEX` default in
`images/standard/Dockerfile`. Ordinary builds only verify those bytes: the
vendored installer never resolves a release, never downloads a checksum, never
picks a version and never contacts Sigstore.

One run resolves the exact release, downloads both maintained Linux packages
with the original release metadata, the original `codex-package_SHA256SUMS` and
both original legacy `.sigstore` bundles, verifies **both** payload/bundle pairs
with the reviewed host verifier under the exact signer policy, preflights both
archives through the production package/ELF checks, and only then publishes the
six files as one transaction. A partial upstream failure leaves every committed
byte identical. The tool never commits, never runs under GitHub Actions and
never adopts a newer version by itself.

Status: 0 published or unchanged, 1 failed with the sources unchanged or fully
restored, 2 usage, 3 undecided or incomplete recovery (the retained directories
are printed), 128+N after a signal whose rollback succeeded.
"""
from __future__ import annotations

import argparse
import datetime
import errno
import hashlib
import importlib.util
import json
import os
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Mapping, Protocol

ROOT = Path(__file__).resolve().parents[2]

PIN_DIRECTORY = 'images/tools/codex'
EVIDENCE_NAME = 'evidence'
EVIDENCE_DIRECTORY = f'{PIN_DIRECTORY}/{EVIDENCE_NAME}'
DOCKERFILE = 'images/standard/Dockerfile'
PIN_FILE = f'{PIN_DIRECTORY}/pin.json'
METADATA_FILE = f'{EVIDENCE_DIRECTORY}/release.json'
CHECKSUMS_FILE = f'{EVIDENCE_DIRECTORY}/codex-package_SHA256SUMS'
CODEX_ARG = 'AGENT_VERSION_CODEX'
CODEX_LABEL = 'org.agent-vm.version.codex'

# One 300-second budget per request. A release payload is ~150 MB today; the
# deadline is what keeps a slow or truncated response from holding a maintainer
# terminal open, and the ceiling is what keeps a rewritten upstream asset from
# filling the host's disk.
FETCH_TIMEOUT_SECONDS = 300
VERIFIER_TIMEOUT_SECONDS = 300
READ_CHUNK_BYTES = 1 << 16
# A release asset redirect chain is one hop today (github.com -> the signed CDN
# host). The bound exists so a server cannot hold the budget open with hops.
MAX_REDIRECTS = 5
# The only origins this writer ever asks. Release assets redirect to a signed
# CDN; that redirect is followed because GitHub issues it, and nowhere else.
ORIGINS = frozenset({'api.github.com', 'github.com'})
REDIRECT_ORIGINS = frozenset({'release-assets.githubusercontent.com',
                              'objects.githubusercontent.com'})
# The single-slot ARG defaults this owner does not own. They are masked before
# and after the staged Dockerfile so the Codex transaction cannot carry somebody
# else's edit into a commit.
UNOWNED_ARGS = ('AGENT_VERSION_OPENCODE', 'AGENT_VERSION_CLAUDE', 'AGENT_VERSION_COPILOT')


class Refusal(Exception):
    """A prerequisite, an upstream response or a staged proposal is unacceptable."""


# --- module loading ----------------------------------------------------------
#
# The selection model and the transactional publisher are loaded from the
# repository under maintenance rather than from this file's own directory, so a
# fixture repository exercises the real modules and production has exactly one
# copy of each.

_modules: dict[Path, object] = {}


def _load(path: Path, name: str) -> object:
    path = Path(path).resolve()
    cached = _modules.get(path)
    if cached is not None:
        return cached
    if not path.is_file():
        raise Refusal(f'missing {path}')
    module_name = f'{name}_{abs(hash(str(path)))}'
    spec = importlib.util.spec_from_file_location(module_name, path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    # Dataclasses resolve their defining module through sys.modules, which a
    # bare module_from_spec does not populate (Python 3.12 raises there).
    sys.modules[module_name] = module
    try:
        spec.loader.exec_module(module)
    except BaseException:
        sys.modules.pop(module_name, None)
        raise
    _modules[path] = module
    return module


def codex_pin(repo_root: Path):
    return _load(Path(repo_root) / PIN_DIRECTORY / 'codex_pin.py', 'codex_pin')


def publisher_module(repo_root: Path):
    return _load(Path(repo_root) / 'script' / 'dev' / 'pin-artifact.py', 'pin_artifact')


# --- injected seams ----------------------------------------------------------

class Fetcher(Protocol):
    def __call__(self, url: str, output: Path, limit: int) -> None: ...


class BlobVerifier(Protocol):
    def __call__(self, archive: Path, bundle: Path, identity: str, issuer: str,
                 work: Path) -> None: ...


@dataclass(frozen=True)
class StagedCodex:
    """The publication bytes, keyed by repository-relative destination."""

    files: Mapping[str, bytes]
    release: str


# --- transport ---------------------------------------------------------------

class _Deadline:
    """One absolute per-request budget, shared by every hop and every read."""

    def __init__(self, seconds: float) -> None:
        self.seconds = seconds
        self._end = time.monotonic() + seconds

    def remaining(self) -> float:
        left = self._end - time.monotonic()
        if left <= 0:
            raise Refusal(f'the request deadline expired after {self.seconds:g}s')
        return left


class _RequestWatchdog:
    """One alarm for the whole request: connect, headers, redirects and reads.

    Re-clamping the socket timeout bounds a read that eventually returns; it does
    not bound a read that never returns at all, and `HTTPResponse.read1` parses
    status lines, header lines and chunk framing *inside* the call it would
    otherwise bound. A peer that keeps the pipe busy a byte at a time, below the
    inactivity timeout but never completing a line, therefore holds the whole
    request open. Only an independent timer can end that, and one `SIGALRM` armed
    for the whole of `https_fetch` covers every one of those steps.

    The caller's own handler and its remaining timer are saved and restored, so
    this does not steal an alarm it did not install; if the handler or the timer
    cannot be installed the fetch fails closed instead of running unbounded.
    """

    def __init__(self, seconds: float) -> None:
        self.seconds = seconds
        self._handler = None
        self._timer: tuple[float, float] | None = None

    def _expired(self, _signum, _frame):
        raise Refusal(f'the request deadline expired after {self.seconds:g}s')

    def __enter__(self) -> '_RequestWatchdog':
        self._handler = signal.getsignal(signal.SIGALRM)
        try:
            signal.signal(signal.SIGALRM, self._expired)
            self._timer = signal.setitimer(signal.ITIMER_REAL, self.seconds)
        except (OSError, ValueError) as error:
            # Nothing is left armed: `setitimer` either never ran or the handler
            # install failed first, and a half-installed watchdog is exactly the
            # unbounded state this exists to prevent.
            try:
                signal.setitimer(signal.ITIMER_REAL, 0)
            except (OSError, ValueError):
                pass
            try:
                if self._handler is not None:
                    signal.signal(signal.SIGALRM, self._handler)
            except (OSError, ValueError):
                self._handler = None
            raise Refusal('the whole-request deadline cannot be enforced in this '
                          f'process ({error}); refusing an unbounded fetch') from None
        return self

    def __exit__(self, *_exception) -> None:
        # Disarm first, then hand the handler back, then let the caller's own
        # alarm continue where it would have been: a timer restored under the
        # wrong handler would fire into this module.
        try:
            signal.setitimer(signal.ITIMER_REAL, 0)
        finally:
            if self._handler is not None:
                try:
                    signal.signal(signal.SIGALRM, self._handler)
                except (OSError, ValueError):
                    pass
            if self._timer is not None and (self._timer[0] > 0 or self._timer[1] > 0):
                try:
                    signal.setitimer(signal.ITIMER_REAL, *self._timer)
                except (OSError, ValueError):
                    pass
        return None


class _Follow(Exception):
    """An approved redirect target that the fetch loop opens itself."""

    def __init__(self, url: str) -> None:
        super().__init__(url)
        self.url = url


class _GithubRedirects(urllib.request.HTTPRedirectHandler):
    """Approve a GitHub release-asset redirect, and only to a GitHub asset host.

    Approval leaves the hop to `https_fetch` (`_Follow`) instead of handing the
    new request back to urllib: urllib reuses the FIRST hop's socket timeout for
    every later hop, so a chain of slow hops could outlive the request deadline.
    Following here means each hop is opened under whatever budget is left.
    """

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        target = urllib.parse.urlsplit(newurl)
        if target.scheme != 'https':
            raise Refusal(f'refusing a non-HTTPS redirect to {newurl}')
        if target.hostname not in REDIRECT_ORIGINS:
            raise Refusal(f'refusing a redirect to {target.hostname}')
        if target.username or target.password or target.port not in (None, 443):
            raise Refusal(f'refusing a redirect to {newurl}')
        # The redirect response itself is never streamed, so its body is closed
        # here rather than left to urllib (which would do it after this call).
        try:
            fp.close()
        except (AttributeError, OSError):
            pass
        raise _Follow(newurl)


def _require_origin(url: str) -> urllib.parse.SplitResult:
    target = urllib.parse.urlsplit(url)
    if target.scheme != 'https':
        raise Refusal(f'{url} is not an HTTPS URL')
    if target.hostname not in ORIGINS:
        raise Refusal(f'{url} is not on a GitHub origin')
    if target.username or target.password or target.port not in (None, 443):
        raise Refusal(f'{url} carries credentials or a non-default port')
    if target.query or target.fragment:
        raise Refusal(f'{url} carries a query or fragment')
    return target


def _bound_transport(response, seconds: float) -> None:
    """Clamp the live socket timeout to the budget that is actually left.

    `HTTPResponse.read(n)` loops over the socket until it has `n` bytes, so the
    inactivity timeout set when the connection was opened does not bound a read
    at all: a peer dripping one byte just under that timeout can hold one read
    far past the deadline. The timeout is re-clamped to the remaining budget
    before every read and the loop reads at most one socket chunk. That bounds
    the read itself; it cannot bound a read that never returns, which is what
    `_RequestWatchdog` is for (`read1` also parses chunk framing inside the call).
    """
    try:
        sock = response.fp.raw._sock
    except (AttributeError, ValueError):
        return
    try:
        sock.settimeout(max(seconds, 0.001))
    except OSError:
        pass


def _read_chunk(response, size: int) -> bytes:
    """One socket-sized read; `read` would block until `size` bytes arrived."""
    read1 = getattr(response, 'read1', None)
    if read1 is not None:
        return read1(size)
    return response.read(size)


def https_fetch(url: str, output: Path, limit: int) -> None:
    """Stream one exact URL to `output` under a total deadline and a byte ceiling.

    The budget is enforced twice over: the independent watchdog can interrupt a
    step that never returns (connect, status line, headers, chunk framing), and
    the per-read deadline check turns the ordinary slow-body case into the same
    refusal before the alarm has to fire. Both are absolute across the redirect
    chain, and the deadline is checked again at EOF -- a response that ends after
    the budget is spent is a refusal, not a small file.
    """
    with _RequestWatchdog(FETCH_TIMEOUT_SECONDS):
        _fetch_under_deadline(url, output, limit)


def _fetch_under_deadline(url: str, output: Path, limit: int) -> None:
    deadline = _Deadline(FETCH_TIMEOUT_SECONDS)
    target = _require_origin(url)
    headers = {}
    if target.hostname == 'api.github.com':
        headers['Accept'] = 'application/vnd.github+json'
    opener = urllib.request.build_opener(_GithubRedirects())
    response = None
    for _hop in range(MAX_REDIRECTS + 1):
        request = urllib.request.Request(url, headers=headers)
        try:
            response = opener.open(request, timeout=deadline.remaining())
            break
        except _Follow as follow:
            url = follow.url
            continue
        except urllib.error.HTTPError as error:
            raise Refusal(f'cannot fetch {url}: HTTP {error.code}') from None
        except (urllib.error.URLError, OSError) as error:
            raise Refusal(f'cannot fetch {url}: {error}') from None
    if response is None:
        raise Refusal(f'{url} redirected more than {MAX_REDIRECTS} times')
    received = 0
    try:
        with response, open(output, 'wb') as handle:
            while True:
                _bound_transport(response, deadline.remaining())
                chunk = _read_chunk(response, min(READ_CHUNK_BYTES, limit + 1 - received))
                if not chunk:
                    break
                received += len(chunk)
                if received > limit:
                    raise Refusal(f'{url} is larger than the {limit} byte ceiling')
                handle.write(chunk)
            deadline.remaining()
    except Refusal:
        raise
    except TimeoutError:
        raise Refusal(f'the request deadline expired while reading {url}') from None
    except (urllib.error.URLError, OSError) as error:
        raise Refusal(f'cannot read {url}: {error}') from None


# --- verifier ----------------------------------------------------------------

def _run_bounded(argv: list[str], environment: dict, work: Path,
                 label: str) -> subprocess.CompletedProcess:
    """Run a host tool with a process-group kill on timeout.

    `start_new_session` puts the child in its own process group, so a hung
    verifier and anything it spawned are signalled together; a plain
    `subprocess.run(timeout=...)` would leave grandchildren behind.
    """
    process = subprocess.Popen(argv, cwd=str(work), env=environment,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, start_new_session=True)
    try:
        stdout, stderr = process.communicate(timeout=VERIFIER_TIMEOUT_SECONDS)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(os.getpgid(process.pid), 9)
        except OSError:
            pass
        process.communicate()
        raise Refusal(f'{label}: the verifier did not finish within '
                      f'{VERIFIER_TIMEOUT_SECONDS}s') from None
    return subprocess.CompletedProcess(argv, process.returncode, stdout, stderr)


def cosign_environment(work: Path) -> dict:
    """The complete environment a pin-time cosign run may see.

    Nothing is inherited except the executable search path: an ambient
    `SIGSTORE_*`/`COSIGN_*` override, a token, a keyring pointer or proxy
    configuration could silently change what "verified" means, so this
    constructed mapping is the whole environment. The private HOME and XDG cache
    are what keep the TUF trust root genuinely invocation-private.
    """
    home = work / 'home'
    cache = work / 'cache'
    for directory in (home, cache):
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    return {'PATH': os.environ.get('PATH', '/usr/bin:/bin'),
            'HOME': str(home),
            'XDG_CACHE_HOME': str(cache),
            'LC_ALL': 'C'}


def cosign_blob_verifier(cosign: Path) -> BlobVerifier:
    """The exact reviewed command line, with every default security check on.

    No `--insecure-ignore-tlog`, no `--insecure-ignore-sct`, no identity regexp,
    no bare public key and no rewritten bundle: certificate chain, SCT and Rekor
    verification stay at their defaults, and the signer is pinned to the exact
    workflow and issuer the schema records.
    """
    def verify(archive: Path, bundle: Path, identity: str, issuer: str,
               work: Path) -> None:
        environment = cosign_environment(Path(work))
        argv = [str(cosign), 'verify-blob', '--bundle', str(bundle),
                '--certificate-identity', identity,
                '--certificate-oidc-issuer', issuer,
                str(archive)]
        result = _run_bounded(argv, environment, Path(work), archive.name)
        if result.returncode != 0:
            detail = (result.stderr or result.stdout).strip()
            raise Refusal(f'{archive.name}: cosign refused the committed bundle '
                          f'({result.returncode}): {detail}')
    return verify


def require_cosign(path: Path, repo_root: Path) -> Path:
    """The reviewed host verifier, at exactly the reviewed version.

    cosign is a maintainer prerequisite like gpg: not an image input, not
    downloaded by this tool and not replaced by a different version. A failed or
    unparseable version report is a refusal, never a reason to continue with a
    different trust story.
    """
    path = Path(path)
    if not path.is_absolute():
        raise Refusal(f'--cosign must be an absolute path, got {path}')
    if not os.access(path, os.X_OK):
        raise Refusal(f'{path} is not an executable file')
    environment = {'PATH': os.environ.get('PATH', '/usr/bin:/bin'), 'LC_ALL': 'C'}
    result = _run_bounded([str(path), 'version', '--json'], environment, Path('/'),
                          'cosign version')
    if result.returncode != 0:
        raise Refusal(f'{path} version --json failed ({result.returncode})')
    try:
        reported = json.loads(result.stdout)['gitVersion']
    except (ValueError, KeyError, TypeError):
        raise Refusal(f'{path} version --json did not report a gitVersion') from None
    reviewed = codex_pin(repo_root).VERIFIER_VERSION
    if reported != reviewed:
        raise Refusal(f'{path} is {reported}, but the reviewed verifier is {reviewed}')
    return path


# --- staged proposal names ---------------------------------------------------

def stage_names(repo_root: Path) -> list[str]:
    """The fixed stage layout: the pin directory shape plus one Dockerfile."""
    module = codex_pin(repo_root)
    names = ['pin.json', 'evidence/release.json', 'evidence/codex-package_SHA256SUMS']
    names += [f'evidence/{module.bundle_name(target)}' for target in module.Target]
    names.append('Dockerfile')
    return names


def destination_for(stage_name: str) -> str:
    if stage_name == 'pin.json':
        return PIN_FILE
    if stage_name == 'Dockerfile':
        return DOCKERFILE
    return f'{PIN_DIRECTORY}/{stage_name}'


def stage_name_for(destination: str) -> str:
    if destination == DOCKERFILE:
        return 'Dockerfile'
    if destination == PIN_FILE:
        return 'pin.json'
    prefix = f'{PIN_DIRECTORY}/'
    if destination.startswith(prefix):
        return destination[len(prefix):]
    raise Refusal(f'{destination} is not part of the Codex closure')


# --- proposal rendering ------------------------------------------------------

def render_codex_arg(dockerfile: str, release: str) -> str:
    """The reviewed `ARG AGENT_VERSION_CODEX` default, on exactly one line."""
    lines = dockerfile.split('\n')
    matches = [index for index, line in enumerate(lines)
               if line.startswith(f'ARG {CODEX_ARG}=')]
    if len(matches) != 1:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {CODEX_ARG}=`, '
                      f'found {len(matches)}')
    lines[matches[0]] = f'ARG {CODEX_ARG}={release}'
    return '\n'.join(lines)


def require_single_codex_slot(dockerfile: str, release: str) -> None:
    args = [line for line in dockerfile.split('\n') if line.startswith(f'ARG {CODEX_ARG}=')]
    if args != [f'ARG {CODEX_ARG}={release}']:
        raise Refusal(f'the Dockerfile must declare exactly one `ARG {CODEX_ARG}={release}`')
    if f'LABEL {CODEX_LABEL}="${{{CODEX_ARG}}}"' not in dockerfile.split('\n'):
        raise Refusal(f'the Dockerfile label `{CODEX_LABEL}` must reference ${{{CODEX_ARG}}}')


def _unowned_view(dockerfile: str) -> list[str]:
    """The Dockerfile with every single-slot ARG default masked.

    Two recipes that differ only in those defaults are the same file for the
    purpose of this transaction, so a Codex proposal cannot smuggle an unrelated
    Dockerfile edit through the Codex publication.
    """
    names = (CODEX_ARG, *UNOWNED_ARGS)
    view = []
    for line in dockerfile.split('\n'):
        for name in names:
            if line.startswith(f'ARG {name}='):
                view.append(f'ARG {name}=<masked>')
                break
        else:
            view.append(line)
    return view


def _security_projection(module, metadata: bytes) -> tuple:
    """The part of the release metadata a substitution would have to change.

    Download counts, body text, ids and timestamps change on every fetch of the
    same tag; the tag and the asset names with their digests are the security
    projection. Ignoring the mutable part is what makes a repeat run of the same
    pin print `unchanged` instead of a daily date-only diff.
    """
    document = module.strict_json(metadata, 'release metadata')
    if not isinstance(document, dict):
        raise Refusal('release metadata is not a JSON object')
    assets = document.get('assets')
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
    projection.sort()
    return (document.get('tag_name'), tuple(projection))


def _require_metadata_covers(module, metadata: bytes, expected: Mapping[str, str]) -> None:
    """Every committed identity must be exactly what the exact-tag API reports."""
    document = module.strict_json(metadata, 'release metadata')
    if not isinstance(document, dict) or not isinstance(document.get('assets'), list):
        raise Refusal('release metadata has no assets array')
    digests: dict[str, object] = {}
    for entry in document['assets']:
        if not isinstance(entry, dict) or not isinstance(entry.get('name'), str):
            raise Refusal('release metadata has an unreadable asset')
        if entry['name'] in digests:
            raise Refusal(f'release metadata repeats the asset {entry["name"]!r}')
        digests[entry['name']] = entry.get('digest')
    for name, wanted in expected.items():
        found = digests.get(name)
        if found is None:
            raise Refusal(f'release metadata does not list the required asset {name!r}')
        if found != f'sha256:{wanted}':
            raise Refusal(f'release metadata digests {name} as {found!r}, but this run '
                          f'downloaded {wanted}')


def _require_checksums_cover(module, checksums: bytes,
                             expected: Mapping[str, str]) -> None:
    """The original checksum manifest must corroborate exactly these digests.

    The grammar and its duplicate rejection live in the loader
    (`parse_checksums`), which is also what parses the committed evidence, so the
    two security parsers cannot drift; only the corroboration below is this
    writer's own.
    """
    declared = {line.name: line.digest
                for line in module.parse_checksums(checksums,
                                                   module.CHECKSUMS_ASSET)}
    for name, wanted in expected.items():
        found = declared.get(name)
        if found is None:
            raise Refusal(f'{module.CHECKSUMS_ASSET} has no line for {name!r}')
        if found != wanted:
            raise Refusal(f'{module.CHECKSUMS_ASSET} disagrees with the downloaded '
                          f'{name}: {found} vs {wanted}')


def _sha256(path: Path) -> str:
    hasher = hashlib.sha256()
    with open(path, 'rb') as handle:
        while True:
            chunk = handle.read(READ_CHUNK_BYTES)
            if not chunk:
                break
            hasher.update(chunk)
    return hasher.hexdigest()


def _digest_of(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def _render_pin(module, release: str, version: str, metadata: bytes, checksums: bytes,
                fetched: Mapping[str, tuple[Path, str]], verified_on: datetime.date) -> str:
    pin = {
        'schema': module.SCHEMA,
        'name': module.NAME,
        'kind': module.KIND,
        'release': release,
        'version': version,
        'package': {'layout_version': module.PACKAGE_LAYOUT_VERSION,
                    'variant': module.PACKAGE_VARIANT,
                    'entrypoint': module.PACKAGE_ENTRYPOINT},
        'metadata': {'url': module.api_url(release),
                     'path': module.METADATA_PATH,
                     'sha256': _digest_of(metadata)},
        'checksums': {'url': module.asset_url(release, module.CHECKSUMS_ASSET),
                      'path': module.CHECKSUMS_PATH,
                      'sha256': _digest_of(checksums)},
        'platforms': {},
        'authentication': {
            'method': module.AUTHENTICATION_METHOD,
            'verifier': {'name': module.VERIFIER_NAME,
                         'version': module.VERIFIER_VERSION},
            'trust_root': module.TRUST_ROOT,
            'certificate_identity': module.identity(release),
            'certificate_oidc_issuer': module.ISSUER,
            'verified_on': verified_on.isoformat(),
        },
    }
    for platform in module.Platform:
        target = module.Target[platform.name]
        archive_digest = fetched[platform.value][1]
        bundle_digest = fetched[f'bundle:{platform.value}'][1]
        pin['platforms'][platform.value] = {
            'target': target.value,
            'asset': module.asset_name(target),
            'url': module.asset_url(release, module.asset_name(target)),
            'sha256': archive_digest,
            'bundle': {'url': module.asset_url(release, module.bundle_name(target)),
                       'path': module.bundle_path(target),
                       'sha256': bundle_digest},
        }
    return json.dumps(pin, indent=2) + '\n'


def _require_same_release(module, committed, release: str, fetched: Mapping[str, tuple],
                          expected: Mapping[str, str], metadata: bytes) -> None:
    """A same-release repeat must be identical, or it is an upstream rewrite."""
    for platform in module.Platform:
        platform_pin = committed.platforms[platform]
        target = module.Target[platform.name]
        archive_digest = fetched[platform.value][1]
        bundle_digest = fetched[f'bundle:{platform.value}'][1]
        if archive_digest != platform_pin.digest.value:
            raise Refusal(
                f'{module.asset_name(target)} changed for the already-pinned {release}: '
                f'committed {platform_pin.digest.value}, upstream {archive_digest}. '
                'A same-release payload rewrite needs a separate reviewed adoption.')
        if bundle_digest != platform_pin.bundle.digest.value:
            raise Refusal(
                f'{module.bundle_name(target)} changed for the already-pinned {release}. '
                'A same-release evidence rewrite needs a separate reviewed adoption.')
    if expected[module.CHECKSUMS_ASSET] != committed.checksums.digest.value:
        raise Refusal(f'{module.CHECKSUMS_ASSET} changed for the already-pinned '
                      f'{release}; refusing to re-pin it.')
    committed_metadata = (Path(committed.directory) / committed.metadata.path).read_bytes()
    if _security_projection(module, metadata) != \
            _security_projection(module, committed_metadata):
        raise Refusal(f'the security projection of the {release} release metadata '
                      'changed; a same-tag identity change needs a separate reviewed '
                      'adoption.')


def _committed_state(module, repo_root: Path):
    """The validated committed closure, or None on a clean first adoption.

    A half-populated directory is refused rather than repaired: this writer
    publishes a reviewed closure, it does not make a broken one look better.
    """
    directory = Path(repo_root) / PIN_DIRECTORY
    names = stage_names(repo_root)[:-1]
    present = [name for name in names
               if (directory / name).exists() or (directory / name).is_symlink()]
    if not present:
        return None
    if not (directory / 'pin.json').is_file():
        raise Refusal(f'{directory} holds Codex evidence with no pin.json; '
                      'refusing a partial old state')
    return module.load_pin(directory)


def _absolute(path: Path) -> Path:
    """An absolute path for every message and every recorded location."""
    return Path(os.path.abspath(path))


def _open_stage_directory(directory: Path, *, create: bool) -> tuple[int, list[str]]:
    """Open the staging directory itself, following no symlink to it.

    Validation and writing must be the *same* operation on the *same* inode, so
    the directory is opened `O_DIRECTORY|O_NOFOLLOW` from its parent and every
    later create happens relative to that descriptor. A path that is swapped for
    a symlink (or for another directory) after the emptiness check can therefore
    no longer receive a write: the descriptor is bound to the inode that was
    checked, and `_require_same_directory` reports the swap afterwards so the
    caller never publishes something it did not inspect.

    The directory must be a real directory this invocation owns and no other user
    can reach (0700): a shared `$TMPDIR` entry is somebody else's staging area.
    """
    directory = _absolute(directory)
    if not directory.name:
        raise Refusal(f'{directory} is not a staging directory')
    if create:
        try:
            os.makedirs(directory, mode=0o700, exist_ok=True)
        except OSError as error:
            raise Refusal(f'cannot create {directory}: {error}') from None
    try:
        parent = os.open(directory.parent, os.O_RDONLY | os.O_DIRECTORY)
    except OSError as error:
        raise Refusal(f'cannot open {directory.parent}: {error}') from None
    try:
        try:
            handle = os.open(directory.name,
                             os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                             dir_fd=parent)
        except OSError as error:
            if error.errno in (errno.ELOOP, errno.EMLINK, errno.ENOTDIR):
                # Linux reports ELOOP for a symlink that `O_NOFOLLOW` refused;
                # macOS may report ENOTDIR once `O_DIRECTORY` is also set. Both
                # are the same thing here: the path is not a real directory.
                raise Refusal(f'{directory} is not a real directory (a symlink or a '
                              'non-directory is refused as a staging directory): '
                              f'{error.strerror}') from None
            raise Refusal(f'cannot open {directory}: {error}') from None
    finally:
        os.close(parent)
    try:
        info = os.fstat(handle)
        if info.st_uid != os.geteuid():
            raise Refusal(f'{directory} is owned by uid {info.st_uid}, not by this '
                          f'invocation (uid {os.geteuid()})')
        mode = stat.S_IMODE(info.st_mode)
        if mode & 0o077:
            raise Refusal(f'{directory} is mode {mode:04o}; a staging directory must '
                          'be private to this user (0700)')
        return handle, sorted(os.listdir(handle))
    except BaseException:
        os.close(handle)
        raise


def _require_same_directory(directory: Path, handle: int) -> None:
    """The pathname still names the inode every write or read went to."""
    opened = os.fstat(handle)
    try:
        current = os.stat(directory, follow_symlinks=False)
    except OSError as error:
        raise Refusal(f'{directory} disappeared while it was used: {error}') from None
    if (current.st_dev, current.st_ino) != (opened.st_dev, opened.st_ino):
        raise Refusal(f'{directory} was replaced while it was in use; the operation '
                      'stayed anchored to the directory that was checked, so the new '
                      'path was neither read nor written')


def _write_stage_file(directory_fd: int, name: str, raw: bytes) -> None:
    """Create exactly one proposal file, exclusively and without following links."""
    try:
        handle = os.open(name, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW,
                         0o600, dir_fd=directory_fd)
    except OSError as error:
        raise Refusal(f'cannot create {name} in the staging directory: {error}') from None
    try:
        view = memoryview(raw)
        while view:
            view = view[os.write(handle, view):]
    finally:
        os.close(handle)


def _read_stage_file(directory_fd: int, name: str, label: str) -> bytes:
    handle = os.open(name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory_fd)
    try:
        if not stat.S_ISREG(os.fstat(handle).st_mode):
            raise Refusal(f'{label} is not a regular file')
        chunks = []
        while True:
            chunk = os.read(handle, READ_CHUNK_BYTES)
            if not chunk:
                return b''.join(chunks)
            chunks.append(chunk)
    finally:
        os.close(handle)


def _require_owned_directory(directory: Path) -> Path:
    """An absent or empty, private, non-symlink directory this invocation may fill.

    Only a directory this invocation created, or one the caller deliberately
    handed over empty, is written to. Recursively erasing a caller's directory
    would delete unrelated files with no backup and no record of what was lost.
    """
    directory = _absolute(directory)
    handle, entries = _open_stage_directory(directory, create=True)
    try:
        if entries:
            raise Refusal(f'{directory} is not empty ({entries}); stage into an '
                          'absent or empty directory')
    finally:
        os.close(handle)
    return directory


def _write_stage(directory: Path, staged: StagedCodex) -> None:
    directory = _require_owned_directory(directory)
    handle, _entries = _open_stage_directory(directory, create=False)
    evidence = None
    try:
        # The child proposal directory is created through the verified descriptor
        # and never reused: a pre-planted `evidence` (a symlink, say) would
        # otherwise redirect the four evidence writes.
        try:
            os.mkdir(EVIDENCE_NAME, 0o700, dir_fd=handle)
        except OSError as error:
            raise Refusal(f'cannot create {directory / EVIDENCE_NAME} as a new '
                          f'directory: {error}') from None
        try:
            evidence = os.open(EVIDENCE_NAME,
                               os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                               dir_fd=handle)
        except OSError as error:
            raise Refusal(f'cannot open {directory / EVIDENCE_NAME}: {error}') from None
        for destination, raw in staged.files.items():
            name = stage_name_for(destination)
            head, _, rest = name.partition('/')
            if head == EVIDENCE_NAME and rest:
                _write_stage_file(evidence, rest, raw)
            else:
                _write_stage_file(handle, name, raw)
        _require_same_directory(directory, handle)
    finally:
        if evidence is not None:
            os.close(evidence)
        os.close(handle)


def _read_stage(directory: Path, names: list[str]) -> dict[str, bytes]:
    """Read the fixed proposal through verified descriptors, never via the path.

    The descriptor is what makes the read safe: a directory swapped for a symlink
    or for another directory between the check and the read cannot supply
    different bytes, and `_require_same_directory` still reports the swap.
    """
    directory = _absolute(directory)
    handle, _entries = _open_stage_directory(directory, create=False)
    evidence = None
    try:
        try:
            evidence = os.open(EVIDENCE_NAME,
                               os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                               dir_fd=handle)
        except OSError:
            evidence = None
        read: dict[str, bytes] = {}
        missing: list[str] = []
        for name in names:
            head, _, rest = name.partition('/')
            directory_fd = evidence if head == EVIDENCE_NAME else handle
            entry = rest if head == EVIDENCE_NAME else head
            if directory_fd is None:
                missing.append(name)
                continue
            try:
                read[name] = _read_stage_file(directory_fd, entry, f'{directory / name}')
            except FileNotFoundError:
                missing.append(name)
            except OSError as error:
                raise Refusal(f'cannot read {directory / name}: {error}') from None
        if missing:
            raise Refusal(f'the staged proposal is incomplete: {missing}')
        unexpected = sorted(name for name in os.listdir(evidence)
                            if f'{EVIDENCE_NAME}/{name}' not in names)
        if unexpected:
            raise Refusal(f'the staged evidence set has unexpected entries: {unexpected}')
        _require_same_directory(directory, handle)
        return read
    finally:
        if evidence is not None:
            os.close(evidence)
        os.close(handle)


# --- staging -----------------------------------------------------------------

def stage_codex(release: str, repo_root: Path, work: Path, *,
                fetch: Fetcher, verify: BlobVerifier) -> StagedCodex:
    """Resolve, authenticate, preflight and render one release. Writes only in `work`."""
    repo_root = Path(repo_root)
    work = Path(work)
    module = codex_pin(repo_root)
    release = module.require_release(release)
    version = module.release_version(release)
    downloads = work / 'downloads'
    downloads.mkdir(parents=True, exist_ok=True)
    committed = _committed_state(module, repo_root)
    same_release = committed is not None and committed.release == release

    # 1. Every upstream byte, bounded and in private scratch. Nothing below this
    #    point re-fetches: a failed step is a failed proposal, not a retry.
    metadata_path = downloads / 'release.json'
    checksums_path = downloads / module.CHECKSUMS_ASSET
    fetch(module.api_url(release), metadata_path, module.MAX_METADATA_BYTES)
    fetch(module.asset_url(release, module.CHECKSUMS_ASSET), checksums_path,
          module.MAX_CHECKSUMS_BYTES)
    fetched: dict[str, tuple[Path, str]] = {}
    for platform in module.Platform:
        target = module.Target[platform.name]
        asset = module.asset_name(target)
        archive = downloads / asset
        bundle = downloads / module.bundle_name(target)
        fetch(module.asset_url(release, asset), archive, module.MAX_COMPRESSED_BYTES)
        fetch(module.asset_url(release, module.bundle_name(target)), bundle,
              module.MAX_BUNDLE_BYTES)
        fetched[platform.value] = (archive, _sha256(archive))
        fetched[f'bundle:{platform.value}'] = (bundle, _sha256(bundle))

    metadata = metadata_path.read_bytes()
    checksums = checksums_path.read_bytes()
    expected = {module.CHECKSUMS_ASSET: _sha256(checksums_path)}
    for platform in module.Platform:
        target = module.Target[platform.name]
        expected[module.asset_name(target)] = fetched[platform.value][1]
        expected[module.bundle_name(target)] = fetched[f'bundle:{platform.value}'][1]
    _require_metadata_covers(module, metadata, expected)
    _require_checksums_cover(module, checksums,
                             {module.asset_name(module.Target[p.name]): fetched[p.value][1]
                              for p in module.Platform})

    # 2. The exact signer policy, on both payloads, before anything is rendered.
    for platform in module.Platform:
        archive = fetched[platform.value][0]
        bundle = fetched[f'bundle:{platform.value}'][0]
        verify(archive, bundle, module.identity(release), module.ISSUER,
               work / 'verify' / platform.name.lower())

    # 3. A same-release repeat keeps the committed raw metadata and the original
    #    verification date when every identity agrees, so the proposal is
    #    byte-identical; any changed identity is refused above, never re-pinned.
    if same_release:
        _require_same_release(module, committed, release, fetched, expected, metadata)
        metadata = (repo_root / METADATA_FILE).read_bytes()
        checksums = (repo_root / CHECKSUMS_FILE).read_bytes()
        verified_on = committed.authentication.verified_on
    else:
        verified_on = datetime.date.today()

    staged: dict[str, bytes] = {
        PIN_FILE: _render_pin(module, release, version, metadata, checksums,
                              fetched, verified_on).encode(),
        METADATA_FILE: metadata,
        CHECKSUMS_FILE: checksums,
    }
    for platform in module.Platform:
        target = module.Target[platform.name]
        staged[f'{EVIDENCE_DIRECTORY}/{module.bundle_name(target)}'] = \
            fetched[f'bundle:{platform.value}'][0].read_bytes()
    staged[DOCKERFILE] = render_codex_arg(
        (repo_root / DOCKERFILE).read_text(), release).encode()

    # 4. Validate the proposal through the production loader, then preflight both
    #    real archives against it. `preflight` reads the committed version and
    #    platform from the pin, so the closure is checked first and the archives
    #    are checked against it; publishing waits for both.
    proposal = StagedCodex(files=staged, release=release)
    scratch = work / 'proposal'
    _write_stage(scratch, proposal)
    loaded = module.load_pin(scratch)
    for platform in module.Platform:
        selected = module.select(loaded, release, module.Target[platform.name])
        module.preflight_archive(loaded, selected, fetched[platform.value][0])
    return proposal


def write_stage(stage_dir: Path, staged: StagedCodex) -> None:
    """Write the fixed stage layout the owner bumper edits and publishes."""
    _write_stage(Path(stage_dir), staged)


def unchanged(repo_root: Path, staged: StagedCodex) -> bool:
    """True when every publication byte already equals the committed byte."""
    for destination, raw in staged.files.items():
        path = Path(repo_root) / destination
        if not path.is_file() or path.read_bytes() != raw:
            return False
    return True


# --- commands ----------------------------------------------------------------

def check_committed(repo_root: Path) -> int:
    """Offline check of the committed inputs, the policy and the single ARG.

    No network, no payload archive, no cosign and no writes. This is not a fresh
    signature check, and it says so.
    """
    repo_root = Path(repo_root)
    module = codex_pin(repo_root)
    pin = module.load_pin(repo_root / PIN_DIRECTORY)
    dockerfile = (repo_root / DOCKERFILE).read_text()
    require_single_codex_slot(dockerfile, pin.release)
    print(f'codex {pin.release}: offline committed-input check '
          '(committed bytes and policy only; not a signature check)')
    for platform in module.Platform:
        platform_pin = pin.platforms[platform]
        print(f'  {platform.value} {platform_pin.target.value} '
              f'{platform_pin.digest.value}')
    print(f'  evidence: {len(module.EVIDENCE_FILES)} files plus '
          f'{len(pin.platforms)} structurally consistent bundles')
    print(f'  {DOCKERFILE}: ARG {CODEX_ARG}={pin.release}, '
          f'LABEL {CODEX_LABEL} references it')
    return 0


def _decode_stage_file(raw: bytes, name: str) -> str:
    """The staged text must be UTF-8, or the proposal is refused, not a traceback."""
    try:
        return raw.decode()
    except UnicodeDecodeError as error:
        raise Refusal(f'the staged {name} is not valid UTF-8 ({error})') from None


def publish_stage(repo_root: Path, stage_dir: Path, *,
                  owner_scratch: Path | None = None) -> int:
    """Publish the fixed Codex closure set as one transaction.

    The proposal is read through descriptors opened from the directory itself
    (`_read_stage`), never from paths that could change under the transaction, and
    the loader validates exactly the bytes that are then published.

    A status-3 publication (restore incomplete) keeps this invocation's scratch --
    for the bumper that is the caller's stage directory, for `--version` it is the
    owner work directory -- and says where it is. That report cannot wait for this
    function to return: a signal-driven rollback exits from inside the publisher,
    so `owner_scratch` is handed over and printed *before* publication starts.
    """
    repo_root = Path(repo_root)
    stage_dir = _absolute(stage_dir)
    module = codex_pin(repo_root)
    names = stage_names(repo_root)
    staged = _read_stage(stage_dir, names)
    print(f'pin-codex: publishing the staged proposal in {stage_dir}', file=sys.stderr)
    # The production loader runs over a private copy of exactly the bytes read
    # above, so a directory swapped under the path between the check and the
    # publication cannot substitute a different proposal.
    verify_directory = Path(tempfile.mkdtemp(prefix='pin-codex.verify.',
                                            dir=os.environ.get('TMPDIR')))
    try:
        for name, raw in staged.items():
            path = verify_directory / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(raw)
        loaded = module.load_pin(verify_directory)
    finally:
        shutil.rmtree(verify_directory, ignore_errors=True)
    staged_dockerfile = _decode_stage_file(staged['Dockerfile'], 'Dockerfile')
    require_single_codex_slot(staged_dockerfile, loaded.release)
    committed_dockerfile = (repo_root / DOCKERFILE).read_text()
    if _unowned_view(staged_dockerfile) != _unowned_view(committed_dockerfile):
        raise Refusal(f'the staged {DOCKERFILE} changes more than the single-slot '
                      'version defaults')
    files = {destination_for(name): raw for name, raw in staged.items()}
    owner = None
    if owner_scratch is not None:
        owner = str(_absolute(owner_scratch))
        announce('owner scratch:', Path(owner))
    status = publisher_module(repo_root).publish(files, root=repo_root, owner=owner)
    if status == 3:
        # The proposal the retained backups belong to is part of the way back.
        retain('staged proposal retained at', stage_dir)
    return status


def announce(what: str, directory: Path) -> None:
    """Say where a directory is before a publication that may exit from a signal.

    The publisher can `os._exit(3)` from a signal handler, so anything this module
    would print after it returns is lost; the line is therefore flushed first and
    the same absolute path is handed to the publisher, whose handler repeats it.
    """
    print(f'pin-codex: {what} {_absolute(directory)}', file=sys.stderr)
    sys.stderr.flush()


def retain(what: str, directory: Path) -> None:
    """Print, and keep, a directory a status-3 recovery depends on."""
    directory = _absolute(directory)
    print(f'pin-codex: {what} {directory}', file=sys.stderr)
    print(f'STOP: recover {directory} together with the publisher directory above '
          'before further edits/builds/commits', file=sys.stderr)


def probe_cosign(path: Path, repo_root: Path) -> int:
    """Report the exact reviewed verifier version, without any write or fetch.

    The owner bumper needs this before it resolves a single upstream tag: an
    incompatible verifier must stop the run before discovery, not after it.
    """
    verifier = require_cosign(Path(path), Path(repo_root))
    reviewed = codex_pin(repo_root).VERIFIER_VERSION
    print(f'cosign {reviewed}: {verifier}')
    return 0


def _cosign_path(argument: str | None, repo_root: Path) -> Path:
    if argument is not None:
        return require_cosign(Path(argument), repo_root)
    found = shutil.which('cosign')
    if found is None:
        raise Refusal('the reviewed cosign is required for a write run; install v2.4.1 '
                      'or pass --cosign /absolute/path')
    return require_cosign(Path(found), repo_root)


def _usage(message: str) -> None:
    print(f'pin-codex: {message}', file=sys.stderr)
    sys.exit(2)


def refuse_ci() -> None:
    if os.environ.get('GITHUB_ACTIONS') == 'true':
        raise Refusal('refusing GitHub Actions: pins are maintainer-host maintenance only')


USAGE = """usage: python3 script/dev/pin-codex.py --version rust-v<VERSION> [--cosign /abs/cosign] [--repo-root DIR]
       python3 script/dev/pin-codex.py --check [--repo-root DIR]
       python3 script/dev/pin-codex.py --probe-cosign /abs/cosign [--repo-root DIR]
       python3 script/dev/pin-codex.py --version TAG --stage-dir DIR [--cosign /abs/cosign] [--repo-root DIR]
       python3 script/dev/pin-codex.py --publish-stage DIR [--repo-root DIR]

--version TAG       resolve that exact openai/codex release for linux/amd64 and
                    linux/arm64, verify both legacy .sigstore bundles under the
                    committed signer policy, preflight both archives and publish
                    the pin, its four evidence files and the
                    `ARG AGENT_VERSION_CODEX` default as one transaction.
--cosign FILE       the reviewed cosign (v2.4.1) to verify with. Required in
                    every write mode; never downloaded or substituted.
--probe-cosign FILE check that FILE is exactly the reviewed verifier and print
                    it; offline, writes nothing, so a caller can gate an upstream
                    lookup on the tool it will actually use.
--stage-dir DIR     write the fixed six-file proposal into DIR (absent or empty)
                    instead of publishing it (the owner bumper's integration
                    point).
--publish-stage DIR publish that proposal as one transaction.
--check             validate the committed evidence, its hashes and the single
                    Codex ARG/label; offline, writes nothing, needs no payload
                    archive and does not verify signatures again.

Status: 0 published or unchanged, 1 failed with sources unchanged or restored,
2 usage, 3 incomplete recovery (the retained directories are printed).
"""


WRITE_MODES = ('--version', '--stage-dir', '--publish-stage')


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument('--version')
    parser.add_argument('--cosign')
    parser.add_argument('--probe-cosign')
    parser.add_argument('--repo-root')
    parser.add_argument('--stage-dir')
    parser.add_argument('--publish-stage')
    parser.add_argument('--check', action='store_true')
    parser.add_argument('--help', action='store_true')
    arguments = parser.parse_args(argv)
    if arguments.help:
        print(USAGE, end='')
        return 0
    # The loader declares its own Refusal, so the writer's top-level handler must
    # accept both: a corrupt committed input is a refusal whether the loader or
    # this file noticed it first.
    refusals: tuple = (Refusal,)
    keep_work = False
    work: Path | None = None
    try:
        repo_root = Path(arguments.repo_root or ROOT)
        refusals = (Refusal, codex_pin(repo_root).Refusal)
        if arguments.check:
            if arguments.version or arguments.cosign or arguments.stage_dir or \
                    arguments.publish_stage is not None or arguments.probe_cosign:
                _usage('--check takes no other argument')
            return check_committed(repo_root)
        if arguments.probe_cosign is not None:
            if arguments.version or arguments.cosign or arguments.stage_dir or \
                    arguments.publish_stage is not None:
                _usage('--probe-cosign takes only --repo-root')
            return probe_cosign(Path(arguments.probe_cosign), repo_root)
        # Every mode that can write refuses CI *before* it does anything: the
        # staged-proposal path publishes as much as the `--version` path does.
        refuse_ci()
        if arguments.publish_stage is not None:
            if arguments.version or arguments.cosign or arguments.stage_dir:
                _usage('--publish-stage takes only --repo-root')
            # For the bumper, the stage directory IS the owner scratch, so the
            # path is resolved and handed to the publisher as the caller's own.
            stage_dir = _absolute(Path(arguments.publish_stage))
            return publish_stage(repo_root, stage_dir, owner_scratch=stage_dir)
        if not arguments.version:
            _usage('--version is required for a write run')
        cosign = _cosign_path(arguments.cosign, repo_root)
        release = codex_pin(repo_root).require_release(arguments.version)
        work = Path(tempfile.mkdtemp(prefix='pin-codex.work.', dir=os.environ.get('TMPDIR')))
        try:
            staged = stage_codex(release, repo_root, work, fetch=https_fetch,
                                 verify=cosign_blob_verifier(cosign))
            if arguments.stage_dir is not None:
                stage_dir = _absolute(Path(arguments.stage_dir))
                write_stage(stage_dir, staged)
                if unchanged(repo_root, staged):
                    print(f'pin-codex: unchanged ({release}); the staged proposal is '
                          f'byte-identical to the committed files ({stage_dir})')
                else:
                    print(f'pin-codex: staged {release} in {stage_dir}')
                return 0
            if unchanged(repo_root, staged):
                print(f'pin-codex: unchanged ({release})')
                return 0
            stage_dir = work / 'publish'
            write_stage(stage_dir, staged)
            # A signal-driven rollback exits from inside the publisher, so the
            # owner scratch is named before publication starts, not after it.
            announce('owner scratch:', work)
            status = publish_stage(repo_root, stage_dir, owner_scratch=work)
            if status == 3:
                # The transaction is undecided: this invocation's scratch (the
                # staged six-file proposal) is part of the way back, so it is
                # retained and printed rather than cleaned up.
                keep_work = True
                retain('owner scratch retained at', work)
            return status
        finally:
            if not keep_work:
                shutil.rmtree(work, ignore_errors=True)
    except Exception as error:  # noqa: BLE001 - the single top-level handler
        if isinstance(error, refusals):
            print(f'pin-codex: {error}', file=sys.stderr)
            return 1
        raise


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
