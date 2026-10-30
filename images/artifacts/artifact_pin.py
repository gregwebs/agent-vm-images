#!/usr/bin/env python3
"""The committed shape of one third-party release-artifact pin (#96, #131).

A pin names an exact upstream release, one archive per supported architecture
with its reviewed SHA-256, the archive's own top-level prefix, what to publish
and which authentication evidence binds the bytes. There are three artifacts -
Node, the GitHub CLI and the Google Antigravity CLI (`agy`) - and the shape is
one shape on purpose: the installer, the maintenance writer and the offline suite
all read the same record, so a pin the gate accepts is a pin those three can act
on.

`agy` is the one artifact with no upstream signature or attestation, so its
authentication is `hash-only`: the pin-time tool required the downloaded bytes to
equal both GitHub's asset digest and Google's updater-manifest sha512, committed
both upstream records under the pin's directory as evidence, and the build
re-checks that the committed records still agree with the pin before it fetches
anything. Its pin lives in `images/tools/agy/` rather than `images/artifacts/`
because it is an agent selection, not a base input (ADR 0012).

The loader is strict rather than lenient. Every mapping has an exact key set, so
an unknown field, a missing platform, an upper-case digest or a `latest` version
is a refusal instead of a default; the document is decoded once, refusing a
duplicate key, and its name has to be one of the three above. Nothing here proves
a release is safe: it proves which bytes were selected and reviewed.
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
SHA512 = re.compile(r'[0-9a-f]{128}')
FINGERPRINT = re.compile(r'[0-9A-F]{40}|[0-9A-F]{64}')
SHA256SUM_LINE = re.compile(r'([0-9a-f]{64}) [ *]([^/\n]+)')

# What each artifact publishes. Node installs the whole reviewed archive because
# the bundled npm/npx and `lib/node_modules/npm` are one tree; gh publishes the
# binary and its man pages, and deliberately not the archive's LICENSE, which
# would land on /usr/local/LICENSE next to Node's.
NODE_PAYLOAD = ('bin', 'include', 'lib', 'share', 'CHANGELOG.md', 'LICENSE', 'README.md')
GH_PAYLOAD = ('bin/gh', 'share/man')
# agy's archive is prefixless and carries one binary named after the project, so
# the payload is (archive member, published path) rather than a bare path: the
# image publishes `/usr/local/bin/agy` and never an `antigravity` alias.
AGY_PAYLOAD = (('antigravity', 'bin/agy'),)
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


AGY_REPOSITORY = 'google-antigravity/antigravity-cli'
# Google's updater manifest is the second corroboration channel. Only its
# `version` and per-architecture `sha512` are acted on; the GCS URL it carries is
# never a download source.
AGY_MANIFEST_ORIGIN = (
    'https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/')
AGY_RELEASE_API = f'https://api.github.com/repos/{AGY_REPOSITORY}/releases/tags/'
AGY_EVIDENCE = {
    'release': 'evidence/release.json',
    'linux/amd64': 'evidence/manifest-linux-amd64.json',
    'linux/arm64': 'evidence/manifest-linux-arm64.json',
}
# The three artifacts the shared model acts on. `load` names a pin after its
# directory, so this list is also what makes an unknown `images/tools/<x>/pin.json`
# a refusal instead of a fall-through to the GitHub CLI defaults.
ARTIFACT_NAMES = ('node', 'gh', 'agy')
# A hash-only artifact's archive shape is checked as well as its digest: an
# archive that carries a second member, or an ELF for another machine under a
# consistent hash, is not the reviewed binary.
EXACT_MEMBERS = {'agy': ('antigravity',)}
# The digest covers every archive byte, member mode included, so this is not a
# hole in the hash: a hash only proves *which* archive was selected. Both channels
# can agree on an archive whose member is mode 0777 — which would publish a
# world-writable /usr/local/bin/agy that every guest UID runs — so the reviewed
# mode is part of the artifact's shape and is checked like the member name. An
# EXACT_MEMBERS artifact therefore carries exactly this mode: `extract` publishes
# the member's own bits, so refusing anything else here is what keeps the published
# file 0755.
EXACT_MEMBER_MODES = {'agy': 0o755}
ELF_MACHINES = {'agy': {'linux/amd64': 62, 'linux/arm64': 183}}
# Ceilings live here rather than in the installer so the pin-time writer shares
# them. 64 MiB is absurdly generous for Node (amd64 is 31,029,996 B) and absurd
# for anything else. agy amd64 1.4.11 is 67,990,723 B compressed and decodes to
# 233,943,440 B, so it needs its own numbers; 128 MiB/512 MiB leave about 2x
# headroom and still refuse an absurd payload.
MAX_ARCHIVE_BYTES = {'node': 64 << 20, 'gh': 64 << 20, 'agy': 128 << 20}
MAX_MEMBER_BYTES = {'agy': 512 << 20}


class Refusal(Exception):
    """A pin, archive or evidence file this repository will not act on."""


def sha256(raw: bytes) -> str:
    return hashlib.sha256(raw).hexdigest()


def sha512(raw: bytes) -> str:
    return hashlib.sha512(raw).hexdigest()


@dataclass(frozen=True)
class Upstream:
    project: str
    release: str


@dataclass(frozen=True)
class Install:
    destination: str
    # (archive member, published path) pairs. Node and gh publish the reviewed
    # archive at the same relative path; agy renames its one member.
    payload: tuple[tuple[str, str], ...]
    links: tuple[tuple[str, str], ...]

    def link_pairs(self) -> tuple[tuple[str, str], ...]:
        return self.links


def platform_token(name: str, platform: str) -> str:
    """The architecture token each project spells its own way: Node says
    `linux-x64`, the GitHub CLI says `linux_amd64`, agy says `x64`."""
    arch = PLATFORMS[platform]
    if name == 'node':
        return 'linux-x64' if arch == 'amd64' else 'linux-arm64'
    if name == 'agy':
        return 'x64' if arch == 'amd64' else 'arm64'
    return f'linux_{arch}'


def expected_filename(name: str, version: str, platform: str) -> str:
    """The one asset name a pin for this release and platform may name."""
    token = platform_token(name, platform)
    if name == 'node':
        return f'node-v{version}-{token}.tar.xz'
    if name == 'agy':
        return f'agy_cli_linux_{token}.tar.gz'
    return f'gh_{version}_{token}.tar.gz'


def release_tag(name: str, version: str) -> str:
    """The upstream tag a release is published under: Node and the GitHub CLI
    prefix it with `v`, agy's tag is the bare version."""
    return version if name == 'agy' else f'v{version}'


def canonical_url(name: str, version: str, platform: str) -> str:
    """The upstream URL, *computed* rather than matched by prefix. A pin has to
    equal this exactly, so `..` segments (which GitHub resolves into another
    repository), a lookalike host, an extra path segment and a query string all
    stop being able to name something other than the reviewed release asset."""
    filename = expected_filename(name, version, platform)
    if name == 'node':
        return f'https://nodejs.org/dist/v{version}/{filename}'
    tag = release_tag(name, version)
    if name == 'agy':
        return f'https://github.com/{AGY_REPOSITORY}/releases/download/{tag}/{filename}'
    return f'https://github.com/cli/cli/releases/download/{tag}/{filename}'


@dataclass(frozen=True)
class Archive:
    platform: str
    url: str
    archive: str
    member_prefix: str
    digest: str
    bundle: str | None
    bundle_digest: str | None
    sha512: str | None = None

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
class Evidence:
    """One committed corroboration file: where it came from, where it is committed
    and the digest that binds the committed bytes."""
    url: str
    path: str
    digest: str


@dataclass(frozen=True)
class HashOnly:
    """No upstream signature exists, so the selection rests on two records that
    had to agree at pin time and are re-checked against each other at build time."""
    release: Evidence
    manifests: dict[str, Evidence]
    verified_on: str
    reason: str


@dataclass(frozen=True)
class Pin:
    directory: Path
    name: str
    version: str
    kind: str
    upstream: Upstream
    install: Install
    archives: dict[str, Archive]
    authentication: OpenPgp | Sigstore | HashOnly
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


def _sha512(value: object, context: str) -> str:
    text = _text(value, context)
    if not SHA512.fullmatch(text):
        raise Refusal(f'{context}: expected a lower-case SHA-512, got {text!r}')
    return text


def _https(value: object, context: str) -> str:
    text = _text(value, context)
    if not text.startswith('https://'):
        raise Refusal(f'{context}: expected an https URL, got {text!r}')
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


def expected_member_prefix(name: str, filename: str) -> str:
    """Node and gh archives carry one top-level directory named after the file;
    agy's archive carries its binary at the root."""
    return '' if name == 'agy' else member_prefix(filename)


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
    if name == 'agy':
        keys = keys + ('sha512',)
    members = _members(value, keys, context)
    url = _source_url(members['url'], context + ' url')
    check_source_url(name, url, version, platform)
    filename = expected_filename(name, version, platform)
    suffix = archive_suffix(filename)
    if members['archive'] != suffix:
        raise Refusal(f'{context}: archive must be {suffix} for {filename}')
    prefix = expected_member_prefix(name, filename)
    if members['member_prefix'] != prefix:
        raise Refusal(f'{context}: member_prefix must be {prefix} for {filename}')
    bundle = bundle_digest = sha512 = None
    if name == 'gh':
        bundle_members = _members(members['bundle'], ('path', 'sha256'), context + ' bundle')
        bundle = _relative(bundle_members['path'], context + ' bundle path')
        expected = bundle_name(name, version, platform)
        if bundle != expected:
            raise Refusal(f'{context}: bundle must be {expected}, got {bundle}')
        bundle_digest = _digest(bundle_members['sha256'], context + ' bundle sha256')
    if name == 'agy':
        sha512 = _sha512(members['sha512'], context + ' sha512')
    return Archive(platform=platform, url=url, archive=suffix,
                   member_prefix=members['member_prefix'],
                   digest=_digest(members['sha256'], context + ' sha256'),
                   bundle=bundle, bundle_digest=bundle_digest, sha512=sha512)


def bundle_name(name: str, version: str, platform: str) -> str:
    return f'{name}-{version}-{platform.replace("/", "-")}.bundle.jsonl'


def manifest_url(platform: str) -> str:
    """Google's updater manifest for one architecture. Its GCS `url` field is
    never a download source; only the version and sha512 are acted on."""
    return f'{AGY_MANIFEST_ORIGIN}linux_{PLATFORMS[platform]}.json'


def _committed_evidence(value: object, context: str, url: str, path: str) -> Evidence:
    """One corroboration file named by the pin. A build never requests these, so
    there is no fixture `file:` exception: the recorded origin has to be the
    upstream one, and the committed path is fixed."""
    members = _members(value, ('url', 'path', 'sha256'), context)
    recorded_url = _text(members['url'], context + ' url')
    if recorded_url != url:
        raise Refusal(f'{context}: the url must be exactly {url}, got {recorded_url}')
    recorded_path = _relative(members['path'], context + ' path')
    if recorded_path != path:
        raise Refusal(f'{context}: the path must be {path}, got {recorded_path}')
    return Evidence(recorded_url, recorded_path,
                    _digest(members['sha256'], context + ' sha256'))


def _authentication(name: str, version: str, value: object) -> OpenPgp | Sigstore | HashOnly:
    if name == 'agy':
        members = _members(
            value, ('method', 'release', 'manifests', 'verified_on', 'reason'),
            'agy: authentication')
        if members['method'] != 'hash-only':
            raise Refusal('agy: authentication must be hash-only')
        manifests_value = _members(members['manifests'], tuple(PLATFORMS), 'agy: manifests')
        return HashOnly(
            release=_committed_evidence(members['release'], 'agy: release evidence',
                                        AGY_RELEASE_API + version, AGY_EVIDENCE['release']),
            manifests={platform: _committed_evidence(
                manifests_value[platform], f'agy: {platform} manifest',
                manifest_url(platform), AGY_EVIDENCE[platform])
                for platform in PLATFORMS},
            verified_on=_date(members['verified_on'], 'agy: verified_on'),
            reason=_text(members['reason'], 'agy: reason'))
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
    if name not in ARTIFACT_NAMES:
        raise Refusal(f'{name}: unknown artifact; the reviewed pins are '
                      f'{list(ARTIFACT_NAMES)}')
    # The pin itself goes through the duplicate-rejecting decoder, not `json.loads`:
    # that one silently keeps the last of two identical keys, so a pin could say one
    # version to a reviewer's reader and another to this installer.
    value = strict_json(text.encode(), f'{name}: pin')
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
    tag = release_tag(name, version)
    if release != tag:
        raise Refusal(f'{name}: upstream release must be {tag}, got {release!r}')
    install = _members(members['install'], ('destination', 'payload', 'links'),
                       f'{name}: install')
    destination = _absolute(install['destination'], f'{name}: destination')
    if destination != '/usr/local':
        raise Refusal(f'{name}: destination must be /usr/local, got {destination}')
    payload = install['payload']
    if not isinstance(payload, list) or not payload:
        raise Refusal(f'{name}: payload must be a non-empty list')
    if name == 'agy':
        payload_pairs = tuple(
            (_relative(_members(item, ('member', 'path'), f'{name}: payload entry')['member'],
                       f'{name}: payload member'),
             _relative(item['path'], f'{name}: payload path'))
            for item in payload)
        if payload_pairs != AGY_PAYLOAD:
            raise Refusal(f'{name}: payload must publish {list(AGY_PAYLOAD)}, '
                          f'got {list(payload_pairs)}')
    else:
        payload_paths = tuple(_relative(item, f'{name}: payload entry') for item in payload)
        expected_payload = NODE_PAYLOAD if name == 'node' else GH_PAYLOAD
        if payload_paths != expected_payload:
            raise Refusal(
                f'{name}: payload must be {list(expected_payload)}, got {list(payload_paths)}')
        payload_pairs = tuple((path, path) for path in payload_paths)
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
        install=Install(destination, payload_pairs, link_pairs),
        archives=archives,
        authentication=_authentication(name, version, members['authentication']),
        bundled=bundled)


def load(path: Path) -> Pin:
    if path.suffix != '.json':
        raise Refusal(f'{path}: a pin is a .json file')
    # images/artifacts/<name>.json (a base artifact) or images/tools/<name>/pin.json
    # (an agent's own pin): the location names the artifact, never the content.
    name = path.parent.name if path.name == 'pin.json' else path.stem
    return parse(path.read_text(), path.parent, name)


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


# --- corroboration ----------------------------------------------------------

def _no_number(text: str) -> None:
    """A float or a `NaN`/`Infinity` constant: no reviewed field is a number of
    either kind, and `1e400` is how a value slips past an integer comparison."""
    raise Refusal(f'{text} is not a JSON number this build acts on')


def _unique_pairs(pairs: list) -> dict:
    members: dict = {}
    for key, value in pairs:
        if key in members:
            raise Refusal(f'duplicate JSON key {key!r}')
        members[key] = value
    return members


def strict_json(raw: bytes, what: str) -> object:
    """One complete JSON document with no duplicate keys and no trailing input.

    `json.loads` keeps the last of two identical keys, so a document could carry
    one value for a reader that takes the first and another for us. The ~15 lines
    are duplicated from the Codex pin reader on purpose: that is a standard-image
    tool, and a base input cannot import one.
    """
    try:
        text = raw.decode('utf-8')
    except UnicodeDecodeError:
        raise Refusal(f'{what} is not UTF-8') from None
    decoder = json.JSONDecoder(object_pairs_hook=_unique_pairs, parse_float=_no_number,
                               parse_constant=_no_number)
    try:
        value, end = decoder.raw_decode(text)
    except ValueError as error:
        raise Refusal(f'{what} is not JSON: {error}') from None
    if text[end:].strip():
        raise Refusal(f'{what} has trailing input after the JSON document')
    return value


def parse_manifest(raw: bytes, context: str) -> dict:
    """One Google updater manifest: exactly the three fields the corroboration
    acts on. The `url` is only required to be https because it is never a
    download source."""
    members = _members(strict_json(raw, context), ('version', 'url', 'sha512'), context)
    _text(members['version'], context + ' version')
    _sha512(members['sha512'], context + ' sha512')
    _https(members['url'], context + ' url')
    return members


def release_asset_digests(release: object, name: str, version: str) -> dict[str, str]:
    """A GitHub release's asset name -> `sha256:<hex>` map, refusing an ambiguous
    release. Shared by the build-time corroboration check and the pin-time writer
    so the two read the same record the same way."""
    context = f'{name}: {AGY_EVIDENCE["release"]}'
    if not isinstance(release, dict):
        raise Refusal(f'{context}: expected the GitHub release object')
    if release.get('tag_name') != version:
        raise Refusal(f'{context}: the release is {release.get("tag_name")!r}, '
                      f'the pin says {version!r}')
    if release.get('draft') is not False:
        raise Refusal(f'{context}: the release is not a published release')
    assets = release.get('assets')
    if not isinstance(assets, list):
        raise Refusal(f'{context}: expected an assets list')
    digests: dict[str, str] = {}
    for asset in assets:
        if not isinstance(asset, dict):
            raise Refusal(f'{context}: expected an asset object')
        asset_name = asset.get('name')
        digest = asset.get('digest')
        if not isinstance(asset_name, str) or not isinstance(digest, str):
            raise Refusal(f'{context}: an asset carries no name or digest')
        if asset_name in digests:
            raise Refusal(f'{context}: the release names {asset_name!r} twice')
        digests[asset_name] = digest
    return digests


def check_corroboration(pin: Pin) -> None:
    """Require the committed corroboration files to still agree with the pin.

    agy publishes no signature, so two records from its one publisher — the GitHub
    release asset's digest and Google's manifest sha512 — were compared at pin time
    and committed. They corroborate the bytes across two *channels*; neither is an
    independent identity, and neither is an authentication. Offline, this re-checks
    that each committed file hashes to the pin *and* that the files still say what
    the pin says: the GitHub release's digest and Google's manifest sha512 both have
    to name the pinned bytes. A hand-edited evidence file, or a pin asking for a
    different release than its evidence describes, is a refusal before any fetch.
    """
    authentication = pin.authentication
    if not isinstance(authentication, HashOnly):
        raise Refusal(f'{pin.name}: only a hash-only pin carries corroboration evidence')
    release_path = check_evidence(pin, authentication.release.path,
                                 authentication.release.digest)
    digests = release_asset_digests(
        strict_json(release_path.read_bytes(), f'{pin.name}: {authentication.release.path}'),
        pin.name, pin.version)
    for platform, archive in pin.archives.items():
        filename = expected_filename(pin.name, pin.version, platform)
        found = digests.get(filename)
        if found is None:
            raise Refusal(f'{pin.name}: GitHub\'s committed release carries no {filename} '
                          f'for {platform}')
        if found != f'sha256:{archive.digest}':
            raise Refusal(f'{pin.name}: GitHub records {found} for {filename}, '
                          f'the pin says sha256:{archive.digest}')
    for platform in PLATFORMS:
        evidence = authentication.manifests[platform]
        path = check_evidence(pin, evidence.path, evidence.digest)
        context = f'{pin.name}: {evidence.path}'
        document = parse_manifest(path.read_bytes(), context)
        if document['version'] != pin.version:
            raise Refusal(f'{context}: the manifest names {document["version"]}, '
                          f'the pin says {pin.version}')
        archive = pin.archives[platform]
        if document['sha512'] != archive.sha512:
            raise Refusal(f'{context}: the manifest sha512 is {document["sha512"]}, '
                          f'the pin says {archive.sha512}')
