#!/usr/bin/env python3
"""The committed shape of one third-party release-artifact pin (#96).

A pin names an exact upstream release, one archive per supported architecture
with its reviewed SHA-256, the archive's own top-level prefix, what to publish
and which authentication evidence binds the bytes. Node and the GitHub CLI are
the two artifacts, and the shape is one shape on purpose: the installer, the
maintenance writer and the offline suite all read the same record, so a pin the
gate accepts is a pin those three can act on.

The loader is strict rather than lenient. Every mapping has an exact key set, so
an unknown field, a missing platform, an upper-case digest or a `latest` version
is a refusal instead of a default. Nothing here proves a release is safe: it
proves which bytes were selected and reviewed.
"""
from __future__ import annotations

import hashlib
import json
import re
import urllib.parse
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

SCHEMA = 1
# The two architectures the base image builds for, keyed by the platform string
# the pins and the gate use, valued by the Docker `TARGETARCH` token.
PLATFORMS = {'linux/amd64': 'amd64', 'linux/arm64': 'arm64'}
ARCH_PLATFORMS = {arch: platform for platform, arch in PLATFORMS.items()}
ARCHIVES = {'.tar.gz': 'tar.gz', '.tar.xz': 'tar.xz'}
# `X.Y.Z` only: a pre-release, a build suffix or a range is a different
# selection with a different review, so the maintenance command refuses one.
VERSION = re.compile(r'(0|[1-9][0-9]{0,5})[.](0|[1-9][0-9]{0,5})[.](0|[1-9][0-9]{0,5})')
DIGEST = re.compile(r'[0-9a-f]{64}')
FINGERPRINT = re.compile(r'[0-9A-F]{40}|[0-9A-F]{64}')
SHA256SUM_LINE = re.compile(r'([0-9a-f]{64}) [ *]([^/\n]+)')

# What each artifact publishes. Node installs the whole reviewed archive because
# the bundled npm/npx and `lib/node_modules/npm` are one tree; gh publishes the
# binary and its man pages, and deliberately not the archive's LICENSE, which
# would land on /usr/local/LICENSE next to Node's.
NODE_PAYLOAD = ('bin', 'include', 'lib', 'share', 'CHANGELOG.md', 'LICENSE', 'README.md')
GH_PAYLOAD = ('bin/gh', 'share/man')
# `/usr/bin/node` is the pinned interpreter path of the whole repository: the
# pnpm launcher, its verifiers, the dsh helper and several suites hardcode it.
NODE_LINKS = ({'path': '/usr/bin/node', 'target': '/usr/local/bin/node'},)
NODE_KEY = 'node-release-key.asc'
NODE_KEY_FINGERPRINT = '5BE8A3F6C8A5C01D106C0AD820B1A390B168D356'
# The npm command Node's archive ships, relative to the install destination. The
# pin records the version reviewed in that exact file, so the runtime evidence
# has an expectation and a refresh that changes the bundled npm is visible.
NPM_PACKAGE = 'lib/node_modules/npm/package.json'
# cli/cli signs its release assets with its deployment workflow's Sigstore
# identity; GitHub CLI is also the only verifier that can read those bundles,
# which is why the pin-time check is authoritative and the build check is a
# binding of committed bundle to committed hash.
GH_POLICY = {
    'repository': 'cli/cli',
    'signer_workflow': 'cli/cli/.github/workflows/deployment.yml',
    'cert_oidc_issuer': 'https://token.actions.githubusercontent.com',
    'deny_self_hosted_runners': True,
}


class Refusal(Exception):
    """A pin, archive or evidence file this repository will not act on."""


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


@dataclass(frozen=True)
class Upstream:
    project: str
    release: str


@dataclass(frozen=True)
class Install:
    destination: str
    payload: tuple[str, ...]
    links: tuple[tuple[str, str], ...]

    def link_pairs(self) -> tuple[tuple[str, str], ...]:
        return self.links


def platform_token(name: str, platform: str) -> str:
    """The architecture token each project spells its own way: Node says
    `linux-x64`, the GitHub CLI says `linux_amd64`."""
    arch = PLATFORMS[platform]
    if name == 'node':
        return 'linux-x64' if arch == 'amd64' else 'linux-arm64'
    return f'linux_{arch}'


def expected_filename(name: str, version: str, platform: str) -> str:
    """The one asset name a pin for this release and platform may name."""
    token = platform_token(name, platform)
    if name == 'node':
        return f'node-v{version}-{token}.tar.xz'
    return f'gh_{version}_{token}.tar.gz'


def canonical_url(name: str, version: str, platform: str) -> str:
    """The upstream URL, *computed* rather than matched by prefix. A pin has to
    equal this exactly, so `..` segments (which GitHub resolves into another
    repository), a lookalike host, an extra path segment and a query string all
    stop being able to name something other than the reviewed release asset."""
    filename = expected_filename(name, version, platform)
    if name == 'node':
        return f'https://nodejs.org/dist/v{version}/{filename}'
    return f'https://github.com/cli/cli/releases/download/v{version}/{filename}'


@dataclass(frozen=True)
class Archive:
    platform: str
    url: str
    archive: str
    member_prefix: str
    digest: str
    bundle: str | None
    bundle_digest: str | None

    @property
    def filename(self) -> str:
        return self.url.rsplit('/', 1)[-1]


@dataclass(frozen=True)
class OpenPgp:
    manifest_url: str
    manifest_digest: str
    signature_url: str
    signature_digest: str
    key_path: str
    key_fingerprint: str
    key_digest: str
    verified_on: str
    reason: str


@dataclass(frozen=True)
class Sigstore:
    checksums_url: str
    checksums_digest: str
    policy: dict
    verified_on: str
    reason: str
    evidence: tuple[str, ...]


@dataclass(frozen=True)
class Pin:
    directory: Path
    name: str
    version: str
    kind: str
    upstream: Upstream
    install: Install
    archives: dict[str, Archive]
    authentication: OpenPgp | Sigstore
    bundled: dict[str, str] | None = None

    def archive(self, platform: str) -> Archive:
        try:
            return self.archives[platform]
        except KeyError:
            raise Refusal(f'{self.name}: the pin has no {platform} archive') from None

    def reason(self) -> str:
        return self.authentication.reason


# --- strict reading ---------------------------------------------------------

def _members(value: object, keys: tuple[str, ...], context: str) -> dict:
    if not isinstance(value, dict):
        raise Refusal(f'{context}: expected an object')
    missing = [key for key in keys if key not in value]
    unknown = [key for key in value if key not in keys]
    if missing:
        raise Refusal(f'{context}: missing {", ".join(missing)}')
    if unknown:
        raise Refusal(f'{context}: unknown {", ".join(sorted(unknown))}')
    return value


def _text(value: object, context: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise Refusal(f'{context}: expected a non-empty string')
    return value


def _digest(value: object, context: str) -> str:
    text = _text(value, context)
    if not DIGEST.fullmatch(text):
        raise Refusal(f'{context}: expected a lower-case SHA-256, got {text!r}')
    return text


def _relative(value: object, context: str) -> str:
    text = _text(value, context)
    if text.startswith('/') or '..' in text.split('/') or '\\' in text:
        raise Refusal(f'{context}: expected an artifacts-relative path, got {text!r}')
    return text


def _absolute(value: object, context: str) -> str:
    text = _text(value, context)
    if not text.startswith('/') or '..' in text.split('/'):
        raise Refusal(f'{context}: expected an absolute path, got {text!r}')
    return text


def _version(value: object, context: str) -> str:
    text = _text(value, context)
    if not VERSION.fullmatch(text):
        raise Refusal(f'{context}: expected an exact X.Y.Z version, got {text!r}')
    return text


def _source_url(value: object, context: str) -> str:
    """A fetchable URL. `file:` exists for the offline suite, which drives this
    loader; which origins a *committed* pin may name is the gate's decision, and
    it requires the upstream https releases."""
    text = _text(value, context)
    if not text.startswith(('https://', 'file://')):
        raise Refusal(f'{context}: expected an https (or fixture file) URL, got {text!r}')
    return text


def _date(value: object, context: str) -> str:
    text = _text(value, context)
    if not re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}', text):
        raise Refusal(f'{context}: expected YYYY-MM-DD, got {text!r}')
    return text


def archive_suffix(filename: str) -> str:
    for suffix, archive in ARCHIVES.items():
        if filename.endswith(suffix):
            return archive
    raise Refusal(f'{filename}: not a supported release archive ({sorted(ARCHIVES)})')


def member_prefix(filename: str) -> str:
    for suffix in ARCHIVES:
        if filename.endswith(suffix):
            return filename[: -len(suffix)] + '/'
    raise Refusal(f'{filename}: not a supported release archive ({sorted(ARCHIVES)})')


def check_source_url(name: str, url: str, version: str, platform: str) -> None:
    """The pin's URL must be the computed one, not merely start like it."""
    context = f'{name}: {platform}'
    expected = canonical_url(name, version, platform)
    if url == expected:
        return
    filename = expected_filename(name, version, platform)
    if url.startswith('file:'):
        # The offline suite's transport. It is held to the same asset name and
        # refused any traversal, query or fragment, so the fixture cannot
        # express a URL a committed pin could not.
        parsed = urllib.parse.urlparse(url)
        if parsed.query or parsed.fragment or parsed.netloc not in ('', 'localhost'):
            raise Refusal(f'{context}: {url} is not a plain file url')
        parts = PurePosixPath(parsed.path).parts
        if any(part in ('', '.', '..') for part in parts) or not parts \
                or parts[-1] != filename:
            raise Refusal(f'{context}: {url} must name {filename} in a plain directory')
        return
    raise Refusal(f'{context}: the url must be exactly {expected}, got {url}')


def _archive(platform: str, value: object, name: str, version: str) -> Archive:
    context = f'{name}: {platform}'
    keys = ('url', 'archive', 'member_prefix', 'sha256')
    if name == 'gh':
        keys = keys + ('bundle',)
    members = _members(value, keys, context)
    url = _source_url(members['url'], context + ' url')
    check_source_url(name, url, version, platform)
    filename = expected_filename(name, version, platform)
    suffix = archive_suffix(filename)
    if members['archive'] != suffix:
        raise Refusal(f'{context}: archive must be {suffix} for {filename}')
    if members['member_prefix'] != member_prefix(filename):
        raise Refusal(
            f'{context}: member_prefix must be {member_prefix(filename)} for {filename}')
    bundle = bundle_digest = None
    if name == 'gh':
        bundle_members = _members(members['bundle'], ('path', 'sha256'), context + ' bundle')
        bundle = _relative(bundle_members['path'], context + ' bundle path')
        expected = bundle_name(name, version, platform)
        if bundle != expected:
            raise Refusal(f'{context}: bundle must be {expected}, got {bundle}')
        bundle_digest = _digest(bundle_members['sha256'], context + ' bundle sha256')
    return Archive(platform, url, suffix, members['member_prefix'],
                   _digest(members['sha256'], context + ' sha256'), bundle, bundle_digest)


def bundle_name(name: str, version: str, platform: str) -> str:
    return f'{name}-{version}-{platform.replace("/", "-")}.bundle.jsonl'


def _authentication(name: str, value: object) -> OpenPgp | Sigstore:
    if name == 'node':
        members = _members(
            value,
            ('method', 'manifest', 'signature', 'key', 'verified_on', 'reason'),
            'node: authentication')
        if members['method'] != 'openpgp-signed-manifest':
            raise Refusal('node: authentication must be an openpgp-signed-manifest')
        manifest = _members(members['manifest'], ('url', 'sha256'), 'node: manifest')
        signature = _members(members['signature'], ('url', 'sha256'), 'node: signature')
        key = _members(members['key'], ('path', 'fingerprint', 'sha256'), 'node: key')
        fingerprint = _text(key['fingerprint'], 'node: key fingerprint').replace(' ', '')
        if not FINGERPRINT.fullmatch(fingerprint):
            raise Refusal(f'node: key fingerprint must be 40 or 64 upper-case hex, got {fingerprint}')
        return OpenPgp(
            manifest_url=_source_url(manifest['url'], 'node: manifest url'),
            manifest_digest=_digest(manifest['sha256'], 'node: manifest sha256'),
            signature_url=_source_url(signature['url'], 'node: signature url'),
            signature_digest=_digest(signature['sha256'], 'node: signature sha256'),
            key_path=_relative(key['path'], 'node: key path'),
            key_fingerprint=fingerprint,
            key_digest=_digest(key['sha256'], 'node: key sha256'),
            verified_on=_date(members['verified_on'], 'node: verified_on'),
            reason=_text(members['reason'], 'node: reason'))
    members = _members(
        value,
        ('method', 'checksums', 'policy', 'verified_on', 'reason', 'evidence'),
        'gh: authentication')
    if members['method'] != 'sigstore-attestation':
        raise Refusal('gh: authentication must be a sigstore-attestation')
    checksums = _members(members['checksums'], ('url', 'sha256'), 'gh: checksums')
    policy = _members(members['policy'],
                      ('repository', 'signer_workflow', 'cert_oidc_issuer',
                       'deny_self_hosted_runners'),
                      'gh: policy')
    if {key: policy[key] for key in policy} != GH_POLICY:
        raise Refusal(f'gh: policy must be the reviewed {GH_POLICY}')
    evidence = members['evidence']
    if not isinstance(evidence, list) or not evidence:
        raise Refusal('gh: evidence must be a non-empty list of URLs')
    urls = tuple(_source_url(item, 'gh: evidence') for item in evidence)
    return Sigstore(
        checksums_url=_source_url(checksums['url'], 'gh: checksums url'),
        checksums_digest=_digest(checksums['sha256'], 'gh: checksums sha256'),
        policy=dict(policy),
        verified_on=_date(members['verified_on'], 'gh: verified_on'),
        reason=_text(members['reason'], 'gh: reason'),
        evidence=urls)


def parse(text: str, directory: Path, name: str) -> Pin:
    """Parse and fully validate one committed pin named by its file name."""
    try:
        value = json.loads(text)
    except json.JSONDecodeError as error:
        raise Refusal(f'{name}: not JSON: {error}') from None
    keys = ['schema', 'name', 'kind', 'version', 'upstream', 'install', 'platforms']
    if name == 'node':
        keys.append('bundled')
    keys.append('authentication')
    members = _members(value, tuple(keys), f'{name}: pin')
    if members['schema'] != SCHEMA:
        raise Refusal(f'{name}: unsupported schema {members["schema"]!r}')
    if members['name'] != name:
        raise Refusal(f'{name}: the pin names artifact {members["name"]!r}')
    if members['kind'] != 'release-archive':
        raise Refusal(f'{name}: kind must be release-archive')
    version = _version(members['version'], f'{name}: version')
    upstream = _members(members['upstream'], ('project', 'release'), f'{name}: upstream')
    release = _text(upstream['release'], f'{name}: release')
    if release != f'v{version}':
        raise Refusal(f'{name}: upstream release must be v{version}, got {release!r}')
    install = _members(members['install'], ('destination', 'payload', 'links'),
                       f'{name}: install')
    destination = _absolute(install['destination'], f'{name}: destination')
    if destination != '/usr/local':
        raise Refusal(f'{name}: destination must be /usr/local, got {destination}')
    payload = install['payload']
    if not isinstance(payload, list) or not payload:
        raise Refusal(f'{name}: payload must be a non-empty list')
    payload_paths = tuple(_relative(item, f'{name}: payload entry') for item in payload)
    expected_payload = NODE_PAYLOAD if name == 'node' else GH_PAYLOAD
    if payload_paths != expected_payload:
        raise Refusal(f'{name}: payload must be {list(expected_payload)}, got {list(payload_paths)}')
    links = install['links']
    if not isinstance(links, list):
        raise Refusal(f'{name}: links must be a list')
    link_pairs = tuple(
        (_absolute(_members(item, ('path', 'target'), f'{name}: link')['path'],
                   f'{name}: link path'),
         _absolute(item['target'], f'{name}: link target'))
        for item in links)
    expected_links = tuple((link['path'], link['target']) for link in NODE_LINKS) \
        if name == 'node' else ()
    if link_pairs != expected_links:
        raise Refusal(
            f'{name}: links must be {list(expected_links)}, got {list(link_pairs)}')
    platforms = _members(members['platforms'], tuple(PLATFORMS), f'{name}: platforms')
    archives = {platform: _archive(platform, platforms[platform], name, version)
                for platform in PLATFORMS}
    bundled = None
    if name == 'node':
        bundled_members = _members(members['bundled'], ('npm',), 'node: bundled')
        bundled = {'npm': _version(bundled_members['npm'], 'node: bundled npm')}
    return Pin(
        directory=directory,
        name=name,
        version=version,
        kind='release-archive',
        upstream=Upstream(_text(upstream['project'], f'{name}: project'), release),
        install=Install(destination, payload_paths, link_pairs),
        archives=archives,
        authentication=_authentication(name, members['authentication']),
        bundled=bundled)


def load(path: Path) -> Pin:
    if path.suffix != '.json':
        raise Refusal(f'{path}: a pin is a .json file')
    return parse(path.read_text(), path.parent, path.stem)


def signed_digest(manifest: bytes, filename: str) -> str:
    """The digest a signed checksums manifest lists for one file.

    Both the maintenance tool and the installer refuse on a missing line: an
    archive the reviewed manifest does not name has no reviewed identity.
    """
    for line in manifest.decode('utf-8', 'replace').splitlines():
        match = SHA256SUM_LINE.fullmatch(line.strip())
        if match and match.group(2) == filename:
            return match.group(1)
    raise Refusal(f'the signed manifest carries no line for {filename}')


def evidence_path(pin: Pin, relative: str) -> Path:
    """A committed evidence file, refused if it is not a plain file inside the pin's
    directory: a symlink could redirect the hash to other bytes."""
    path = pin.directory / relative
    if path.is_symlink() or not path.is_file():
        raise Refusal(f'{pin.name}: missing or symlinked evidence file {relative}')
    return path


def check_evidence(pin: Pin, relative: str, digest: str) -> Path:
    path = evidence_path(pin, relative)
    actual = sha256(path.read_bytes())
    if actual != digest:
        raise Refusal(f'{pin.name}: {relative} is {actual}, the pin says {digest}')
    return path
