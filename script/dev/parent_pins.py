#!/usr/bin/env python3
"""Resolve, authenticate and stage the maintained parent-pin inventory (#95).

Builds never run this. It is the maintenance side of `images/parent-pins.json`:
`refresh-parent-pins.sh` first asks `destinations` for a network-free publication
plan, then `stage`, which resolves the selected parents against the live
registries, authenticates the released-standard metadata with the same signer
policy the release path uses, and writes complete candidate copies plus a
human-readable proposal. Nothing here writes the checkout.

Two deliberate differences from `script/release/`:

* Official-image parents carry only per-platform digest and provenance
  annotations. Their `verification` is a reviewed, digest-bound hash-only
  exception: Docker Content Trust, the channel that signed Docker Official
  Images, is retired and no supported replacement signing scheme is published
  for these images, so the reviewed choice is the registry digest plus the
  recorded source revision.
* The released standard image must be authenticated against the metadata bytes
  it was published with. That metadata predates the current selection set, so
  this module authenticates the original bytes and then validates only the
  fields a parent pin binds. It never calls the production release parser, which
  correctly requires today's selections.
"""
from __future__ import annotations

import sys

sys.dont_write_bytecode = True

import argparse
import datetime
import difflib
import hashlib
import json
import os
import re
import shutil
import subprocess
import urllib.parse
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

ROOT = Path(__file__).resolve().parents[2]
RELEASE_MODULES = ROOT / 'script/release'
if str(RELEASE_MODULES) not in sys.path:
    sys.path.insert(0, str(RELEASE_MODULES))

import attestations
import content as release_content
import operations

INVENTORY = 'images/parent-pins.json'
DEBIAN_SOURCES = 'images/apt-policy/debian.sources'
# The one owner of the recorded legacy apt instructions. A gap is an exact
# exception for a reviewed RUN, never a marker or a pattern: changing it needs a
# human to re-review the instruction and update the record.
APT_GAP_OWNER = 'https://github.com/gregwebs/agent-vm-images/issues/96'
KEYRINGS = 'images/apt-policy/keyrings.sha256'
POLICY_FILES = (DEBIAN_SOURCES, KEYRINGS)

DEBIAN_PIN = 'debian'
GO_PIN = 'golang-builder'
RELEASE_PIN = 'standard-release'
PINS = (DEBIAN_PIN, GO_PIN, RELEASE_PIN)

# The maintained candidates. Selecting a different Debian release or Go
# toolchain is a reviewed source change of this file and the coordinated
# recipes, never a silent effect of running the refresher.
DEBIAN_TAG = 'debian:13-slim'
DEBIAN_SOURCE = 'https://github.com/debuerreotype/docker-debian-artifacts.git'
GO_SOURCE = 'https://github.com/docker-library/golang.git'
STANDARD_REPOSITORY = 'ghcr.io/gregwebs/agent-vm-standard'
STANDARD_VERSION = '0.1.3'
# The released index the four example layers address today. Bootstrap must
# authenticate exactly this digest; falling back to a tag would silently
# retarget every layer.
STANDARD_BOOTSTRAP_DIGEST = 'sha256:04701db70ef6c2c75078ca39a85ce4b4c15b04cacfea47836c2e5d113c4d42de'
LOCAL_BASE = 'agent-vm-base:local'
LAYERS_README = 'examples/layers/README.md'
# The committed repository base identity (#95): a manifest of the images/ sources
# the base is built from, and the copied extension's generated default. Both are
# derived from the staged recipes, so a refresh that moves a local parent input
# must republish them or the base and its consumers disagree.
BASE_INPUTS = 'script/build/base-inputs.sh'
BASE_MANIFEST = 'images/base-inputs.sha256'
EXTENSION = 'examples/base-extension/Dockerfile'
LOCAL_PINS = (DEBIAN_PIN, GO_PIN)

# Reviewed Debian repository policy for the pinned parent. A future parent
# carrying a different URI, suite or key path is a policy change, not a refresh.
DEBIAN_SIGNED_BY = '/usr/share/keyrings/debian-archive-keyring.pgp'
DEBIAN_OBJECTS = (('http://deb.debian.org/debian', ('trixie', 'trixie-updates')),
                  ('http://deb.debian.org/debian-security', ('trixie-security',)))
DEBIAN_COMPONENTS = ('main',)

INDEX_MEDIA_TYPES = ('application/vnd.oci.image.index.v1+json',
                     'application/vnd.docker.distribution.manifest.list.v2+json')
MANIFEST_MEDIA_TYPES = ('application/vnd.oci.image.manifest.v1+json',
                        'application/vnd.docker.distribution.manifest.v2+json')
OFFICIAL_PLATFORMS = ('linux/amd64', 'linux/arm64')
RELEASE_PLATFORMS = ('linux/amd64', 'linux/arm64')

HASH_ONLY_EVIDENCE = (
    'https://docs.docker.com/engine/security/trust/',
    'https://www.docker.com/blog/docker-content-trust-retirement-and-migration-guidance/',
    'https://docs.docker.com/docker-hub/image-library/trusted-content/',
    'https://www.docker.com/blog/retiring-docker-content-trust/',
)
HASH_ONLY_REASSESS = 'Stop refresh if a supported verifiable signing scheme is published for this image'

# Per-image, digest-bound exception rationale. It lives here rather than being
# derived from the registry so a new digest cannot silently inherit a waiver;
# the reviewer re-dates `reviewed_on` before a refresh is merged.
HASH_ONLY_REASONS = {
    DEBIAN_PIN: ('Docker Official Image for Debian. Docker Content Trust, the scheme that signed '
                 'official images, is retired and its DOI certificates expire; Docker points base '
                 'image consumers at signed Docker Hardened Images and no supported replacement '
                 'signing scheme is published for this image. The reviewed choice is the registry '
                 'digest plus the recorded source revision and creation time, re-checked on every '
                 'refresh.'),
    GO_PIN: ('Docker Official Image for Go, used only as the build-only helper compiler. Same '
             'retired Docker Content Trust channel as the Debian parent and no published '
             'replacement signing scheme for this image; the reviewed choice is the registry '
             'digest plus the recorded source revision, which the build additionally cross-checks '
             'against go.mod.'),
}

SHA256 = re.compile(r'sha256:([0-9a-f]{64})')
REVISION = re.compile(r'[0-9a-f]{40}')
STAMP = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}')
IMAGE_NAME = re.compile(r'(?P<repository>[a-z0-9][a-z0-9._/-]*):(?P<tag>[A-Za-z0-9][A-Za-z0-9._-]*)')
GO_TAG = re.compile(r'golang:(?P<version>[0-9]+\.[0-9]+\.[0-9]+)-trixie')
GO_FRAGMENT = re.compile(r'(?P<revision>[0-9a-f]{40}):(?P<major_minor>[0-9]+\.[0-9]+)/trixie')
CREATED = re.compile(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z')
WORD = re.compile(r'[A-Za-z0-9][A-Za-z0-9._-]*')
FROM_LINE = re.compile(r'^FROM(?:\s+(?P<flags>(?:--[a-z-]+=\S+\s+)*))?(?P<image>\S+)'
                       r'(?:\s+AS\s+(?P<stage>\S+))?\s*$', re.IGNORECASE)
ARG_LINE = re.compile(r'^ARG\s+(?P<declarations>.+)$', re.IGNORECASE)
ARG_REFERENCE = re.compile(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}')


class Refusal(ValueError):
    """A refusal the caller reports as `parent-pins: <reason>` with status 1."""


# --- strict JSON ------------------------------------------------------------

def no_duplicates(pairs: list[tuple[str, object]]) -> dict:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise Refusal(f'duplicate JSON key: {key}')
        result[key] = value
    return result


def strict_json(raw: bytes, context: str) -> object:
    """Parse raw bytes rejecting duplicate object keys at every level.

    `json.loads` keeps the last duplicate, so a hand-edited inventory or a
    hostile registry response could carry two competing digests for one key. The
    pairs hook runs before any collapse, so the source text is rejected instead
    of a rebuilt map.
    """
    try:
        return json.loads(raw, object_pairs_hook=no_duplicates)
    except Refusal:
        raise
    except (ValueError, UnicodeDecodeError) as error:
        raise Refusal(f'{context}: invalid JSON: {error}') from error


def sha256_file(path: Path) -> 'Digest':
    digest = hashlib.sha256()
    with path.open('rb') as stream:
        while chunk := stream.read(1 << 20):
            digest.update(chunk)
    return Digest('sha256:' + digest.hexdigest())


# --- validated types --------------------------------------------------------

@dataclass(frozen=True)
class Digest:
    value: str

    @classmethod
    def parse(cls, value: object) -> 'Digest':
        if not isinstance(value, str) or not SHA256.fullmatch(value):
            raise Refusal('invalid SHA-256 digest')
        return cls(value)

    @classmethod
    def of(cls, raw: bytes) -> 'Digest':
        return cls('sha256:' + hashlib.sha256(raw).hexdigest())


@dataclass(frozen=True)
class SourceRevision:
    value: str

    @classmethod
    def parse(cls, value: object) -> 'SourceRevision':
        if not isinstance(value, str) or not REVISION.fullmatch(value):
            raise Refusal('invalid 40-hex source revision')
        return cls(value)


@dataclass(frozen=True)
class Timestamp:
    value: str

    @classmethod
    def parse(cls, value: object) -> 'Timestamp':
        if not isinstance(value, str) or not CREATED.fullmatch(value):
            raise Refusal('invalid RFC3339 creation timestamp')
        return cls(value)


@dataclass(frozen=True)
class Date:
    value: str

    @classmethod
    def parse(cls, value: object) -> 'Date':
        if not isinstance(value, str) or not STAMP.fullmatch(value):
            raise Refusal('invalid YYYY-MM-DD date')
        return cls(value)


@dataclass(frozen=True)
class Repository:
    value: str

    @classmethod
    def parse(cls, value: object) -> 'Repository':
        if not isinstance(value, str) or not re.fullmatch(r'[a-z0-9][a-z0-9._/-]*', value):
            raise Refusal('invalid image repository')
        return cls(value)


@dataclass(frozen=True)
class PinnedReference:
    """A digest reference plus the tag it was selected by (official images only)."""

    repository: Repository
    tag: str | None
    digest: Digest

    @classmethod
    def parse(cls, value: object, *, require_tag: bool) -> 'PinnedReference':
        if not isinstance(value, str):
            raise Refusal('invalid pinned reference')
        name, separator, raw_digest = value.partition('@')
        if not separator:
            raise Refusal('a pinned reference must carry a digest')
        match = IMAGE_NAME.fullmatch(name)
        if match:
            if not require_tag:
                raise Refusal('this pin must reference a digest without a tag')
            return cls(Repository.parse(match.group('repository')), match.group('tag'),
                       Digest.parse(raw_digest))
        if require_tag:
            raise Refusal('an official-image reference requires an explicit tag')
        return cls(Repository.parse(name), None, Digest.parse(raw_digest))

    def text(self) -> str:
        tag = '' if self.tag is None else ':' + self.tag
        return f'{self.repository.value}{tag}@{self.digest.value}'


@dataclass(frozen=True)
class OfficialPlatform:
    manifest: Digest
    revision: SourceRevision
    source: str
    created: Timestamp

    def json(self) -> dict:
        return {'manifest': self.manifest.value, 'revision': self.revision.value,
                'source': self.source, 'created': self.created.value}


@dataclass(frozen=True)
class ReleasePlatform:
    manifest: Digest

    def json(self) -> dict:
        return {'manifest': self.manifest.value}


@dataclass(frozen=True)
class HashOnly:
    reviewed_on: Date
    reason: str
    evidence: tuple[str, ...]
    reassess: str
    method = 'hash-only'

    def json(self) -> dict:
        return {'method': self.method, 'reviewed_on': self.reviewed_on.value, 'reason': self.reason,
                'evidence': list(self.evidence), 'reassess': self.reassess}


@dataclass(frozen=True)
class GithubAttestation:
    repository: str
    signer_workflow: str
    source_ref: str
    source_sha: SourceRevision
    release_json_sha256: Digest
    invocation_url: str
    method = 'github-attestation'

    def json(self) -> dict:
        return {'method': self.method, 'repository': self.repository,
                'signer_workflow': self.signer_workflow, 'source_ref': self.source_ref,
                'source_sha': self.source_sha.value,
                'release_json_sha256': self.release_json_sha256.value,
                'invocation_url': self.invocation_url}


@dataclass(frozen=True)
class OfficialPin:
    reference: PinnedReference
    expected_source: str
    platforms: dict[str, OfficialPlatform]
    verification: HashOnly
    resolved_on: Date
    kind = 'docker-official-image'

    def json(self) -> dict:
        return {'kind': self.kind, 'reference': self.reference.text(),
                'expected_source': self.expected_source,
                'platforms': {name: self.platforms[name].json() for name in OFFICIAL_PLATFORMS},
                'verification': self.verification.json(), 'resolved_on': self.resolved_on.value}


@dataclass(frozen=True)
class ReleasePin:
    reference: PinnedReference
    version: str
    platforms: dict[str, ReleasePlatform]
    verification: GithubAttestation
    resolved_on: Date
    kind = 'repository-release'

    def json(self) -> dict:
        return {'kind': self.kind, 'reference': self.reference.text(), 'version': self.version,
                'platforms': {name: self.platforms[name].json() for name in RELEASE_PLATFORMS},
                'verification': self.verification.json(), 'resolved_on': self.resolved_on.value}


@dataclass(frozen=True)
class ParentSpec:
    stage: str | None
    binding: str
    pin: str | None


@dataclass(frozen=True)
class AptOpenGap:
    """One reviewed legacy apt source installation, recorded verbatim."""

    recipe: str
    instruction: str
    owner: str

    def json(self) -> dict:
        return {'recipe': self.recipe, 'instruction': self.instruction, 'owner': self.owner}


@dataclass(frozen=True)
class ParentInventory:
    pins: dict[str, 'OfficialPin | ReleasePin']
    recipes: dict[str, tuple[ParentSpec, ...]]
    apt_open_gaps: tuple[AptOpenGap, ...] = ()


@dataclass(frozen=True)
class FromInstruction:
    raw_image: str
    resolved_image: str
    stage: str | None
    platform: str | None


@dataclass(frozen=True)
class Destination:
    relative_path: PurePosixPath
    staged_path: Path


@dataclass(frozen=True)
class OfficialPolicy:
    name: str
    tag: str
    expected_source: str
    revision_fragment: bool

    def verification(self, reviewed_on: Date) -> HashOnly:
        return HashOnly(reviewed_on, HASH_ONLY_REASONS[self.name], HASH_ONLY_EVIDENCE,
                        HASH_ONLY_REASSESS)


@dataclass(frozen=True)
class Request:
    pins: tuple[str, ...]
    bootstrap: bool
    go_tag: str | None
    version: str | None

    def label(self) -> str:
        if self.bootstrap:
            return 'bootstrap'
        return 'refresh ' + ', '.join(self.pins)


def _members(value: object, context: str) -> dict:
    if not isinstance(value, dict):
        raise Refusal(f'{context}: JSON object required')
    return value


def _sequence(value: object, context: str) -> list:
    if not isinstance(value, list):
        raise Refusal(f'{context}: JSON array required')
    return value


def _exact(value: object, keys: set[str], context: str) -> dict:
    members = _members(value, context)
    if set(members) != keys:
        raise Refusal(f'{context}: expected exactly {sorted(keys)}')
    return members


# --- Dockerfile FROM parsing ------------------------------------------------

def logical_instructions(text: str) -> list[str]:
    """Join continuations and drop comment/blank lines, preserving order."""
    instructions, current = [], ''
    for line in text.splitlines():
        stripped = line.strip()
        if not current and (not stripped or stripped.startswith('#')):
            continue
        if line.rstrip().endswith('\\'):
            current += line.rstrip()[:-1]
            continue
        current += line
        if current.strip():
            instructions.append(current.strip())
        current = ''
    if current.strip():
        instructions.append(current.strip())
    return instructions


def physical_instructions(text: str) -> list[str]:
    """Instruction blocks with their own bytes: continuations kept, no final LF.

    An apt gap is an exact exception, so the record has to be compared with the
    recipe's physical text. `logical_instructions` deliberately normalizes
    whitespace and cannot express that distinction.
    """
    instructions, current = [], []
    for line in text.split('\n'):
        stripped = line.strip()
        if not current and (not stripped or stripped.startswith('#')):
            continue
        current.append(line)
        if not line.rstrip().endswith('\\'):
            instructions.append('\n'.join(current))
            current = []
    if current:
        instructions.append('\n'.join(current))
    return instructions


def global_args(instructions: list[str]) -> dict[str, str]:
    """Global ARG defaults, i.e. those declared before the first FROM.

    A stage-local `ARG NAME` redeclaration carries no default and must not be
    mistaken for the global default a `${NAME}` FROM expands to.
    """
    defaults: dict[str, str] = {}
    for instruction in instructions:
        if instruction.upper().startswith('FROM '):
            break
        match = ARG_LINE.match(instruction)
        if not match:
            continue
        for declaration in match.group('declarations').split():
            name, separator, default = declaration.partition('=')
            if separator:
                defaults[name] = default
    return defaults


def parse_froms(text: str) -> list[FromInstruction]:
    instructions = logical_instructions(text)
    defaults = global_args(instructions)
    result: list[FromInstruction] = []
    for instruction in instructions:
        match = FROM_LINE.match(instruction)
        if not match:
            continue
        platform = None
        for flag in (match.group('flags') or '').split():
            name, _, value = flag.partition('=')
            if name.lower() == '--platform':
                platform = value
        raw = match.group('image')
        result.append(FromInstruction(raw, resolve_image(raw, defaults), match.group('stage'),
                                      platform))
    return result


def resolve_image(raw: str, defaults: dict[str, str]) -> str:
    if '$' not in raw:
        return raw
    match = ARG_REFERENCE.fullmatch(raw)
    if match is None:
        raise Refusal(f'unresolved or nested FROM expansion: {raw}')
    name = match.group(1)
    if name not in defaults:
        raise Refusal(f'FROM references an undeclared or defaultless ARG: {name}')
    return defaults[name]


# --- inventory parsing ------------------------------------------------------

def parse_verification(value: object) -> 'HashOnly | GithubAttestation':
    members = _members(value, 'verification')
    method = members.get('method')
    if method == 'hash-only':
        _exact(value, {'method', 'reviewed_on', 'reason', 'evidence', 'reassess'}, 'verification')
        evidence = _sequence(members['evidence'], 'verification evidence')
        if not evidence or any(not isinstance(item, str) or not item for item in evidence):
            raise Refusal('verification evidence must list URLs')
        reason, reassess = members['reason'], members['reassess']
        if not isinstance(reason, str) or not reason.strip():
            raise Refusal('a hash-only exception requires a reason')
        if not isinstance(reassess, str) or not reassess.strip():
            raise Refusal('a hash-only exception requires a reassessment condition')
        return HashOnly(Date.parse(members['reviewed_on']), reason, tuple(evidence), reassess)
    if method == 'github-attestation':
        _exact(value, {'method', 'repository', 'signer_workflow', 'source_ref', 'source_sha',
                       'release_json_sha256', 'invocation_url'}, 'verification')
        for key in ('repository', 'signer_workflow', 'invocation_url'):
            if not isinstance(members[key], str) or not members[key]:
                raise Refusal(f'verification {key} must be nonempty')
        # Only this repository's release workflow signs a parent release. The
        # identity comes from the release modules, not from the inventory, so a
        # hand-edited repository plus its own composed signer cannot pass.
        if members['repository'] != attestations.REPO:
            raise Refusal(f'verification repository must be {attestations.REPO}')
        if members['signer_workflow'] != attestations.WORKFLOW:
            raise Refusal(f'verification signer workflow must be {attestations.WORKFLOW}')
        if members['source_ref'] != 'refs/heads/main':
            raise Refusal('verification source_ref must be refs/heads/main')
        return GithubAttestation(members['repository'], members['signer_workflow'],
                                 members['source_ref'], SourceRevision.parse(members['source_sha']),
                                 Digest.parse(members['release_json_sha256']),
                                 members['invocation_url'])
    raise Refusal('unknown verification method')


def parse_pin(name: str, value: object) -> 'OfficialPin | ReleasePin':
    members = _members(value, f'pin {name}')
    kind = members.get('kind')
    if kind == 'docker-official-image':
        _exact(value, {'kind', 'reference', 'expected_source', 'platforms', 'verification',
                       'resolved_on'}, f'pin {name}')
        reference = PinnedReference.parse(members['reference'], require_tag=True)
        expected = members['expected_source']
        if not isinstance(expected, str) or not expected:
            raise Refusal(f'pin {name}: expected_source must be a URL')
        verification = parse_verification(members['verification'])
        if not isinstance(verification, HashOnly):
            raise Refusal(f'pin {name}: an official image carries a reviewed hash-only exception')
        platforms = _members(members['platforms'], f'pin {name} platforms')
        if set(platforms) != set(OFFICIAL_PLATFORMS):
            raise Refusal(f'pin {name}: exactly the amd64 and arm64 platforms are supported')
        parsed = {}
        for platform in OFFICIAL_PLATFORMS:
            record = _exact(platforms[platform], {'manifest', 'revision', 'source', 'created'},
                            f'pin {name} {platform}')
            if not isinstance(record['source'], str) or not record['source']:
                raise Refusal(f'pin {name} {platform}: source is required')
            parsed[platform] = OfficialPlatform(Digest.parse(record['manifest']),
                                              SourceRevision.parse(record['revision']),
                                              record['source'], Timestamp.parse(record['created']))
        return OfficialPin(reference, expected, parsed, verification,
                           Date.parse(members['resolved_on']))
    if kind == 'repository-release':
        _exact(value, {'kind', 'reference', 'version', 'platforms', 'verification', 'resolved_on'},
               f'pin {name}')
        reference = PinnedReference.parse(members['reference'], require_tag=False)
        version = members['version']
        if not isinstance(version, str):
            raise Refusal(f'pin {name}: invalid release version')
        parse_release_version(version, f'pin {name}')
        verification = parse_verification(members['verification'])
        if not isinstance(verification, GithubAttestation):
            raise Refusal(f'pin {name}: a release pin requires GitHub attestation provenance')
        platforms = _members(members['platforms'], f'pin {name} platforms')
        if set(platforms) != set(RELEASE_PLATFORMS):
            raise Refusal(f'pin {name}: exactly the amd64 and arm64 platforms are supported')
        parsed = {platform: ReleasePlatform(Digest.parse(
            _exact(platforms[platform], {'manifest'}, f'pin {name} {platform}')['manifest']))
            for platform in RELEASE_PLATFORMS}
        return ReleasePin(reference, version, parsed, verification,
                          Date.parse(members['resolved_on']))
    raise Refusal(f'pin {name}: unknown pin kind')


def parse_recipes(value: object) -> dict[str, tuple[ParentSpec, ...]]:
    recipes = {}
    for path, specs in _members(value, 'recipes').items():
        if not isinstance(path, str) or not re.fullmatch(r'[A-Za-z0-9._/-]+', path):
            raise Refusal('recipe path must be checkout-relative')
        entries, stages = [], set()
        for spec in _sequence(specs, f'recipe {path}'):
            members = _members(spec, f'recipe {path} spec')
            if set(members) not in ({'stage', 'pin'}, {'stage', 'local_base'}):
                raise Refusal(f'recipe {path}: expected stage plus pin or local_base')
            stage = members['stage']
            if stage is not None and (not isinstance(stage, str) or not WORD.fullmatch(stage)):
                raise Refusal(f'recipe {path}: invalid stage name')
            if stage in stages:
                raise Refusal(f'recipe {path}: duplicate stage {stage}')
            stages.add(stage)
            binding = members.get('pin') or members.get('local_base')
            if not isinstance(binding, str) or not binding:
                raise Refusal(f'recipe {path}: empty binding')
            entries.append(ParentSpec(stage, 'pin' if 'pin' in members else 'local_base', binding))
        if not entries:
            raise Refusal(f'recipe {path}: at least one parent is required')
        recipes[path] = tuple(entries)
    return recipes


def parse_apt_open_gaps(value: object, recipes: dict[str, tuple[ParentSpec, ...]]) \
        -> tuple[AptOpenGap, ...]:
    gaps, seen = [], set()
    for item in _sequence(value, 'apt_open_gaps'):
        members = _exact(item, {'recipe', 'instruction', 'owner'}, 'apt_open_gaps entry')
        recipe, instruction = members['recipe'], members['instruction']
        if not isinstance(recipe, str) or recipe not in recipes:
            raise Refusal(f'apt gap recipe is not a recorded recipe: {recipe!r}')
        if members['owner'] != APT_GAP_OWNER:
            raise Refusal(f'apt gap in {recipe}: owner must be {APT_GAP_OWNER}')
        if not isinstance(instruction, str) or not instruction.startswith('RUN ') \
                or '\r' in instruction or instruction.endswith('\n'):
            raise Refusal(f'apt gap in {recipe}: an exact RUN instruction is required')
        lines = instruction.split('\n')
        if any(not line.strip() for line in lines) \
                or any(not line.rstrip().endswith('\\') for line in lines[:-1]) \
                or lines[-1].rstrip().endswith('\\'):
            raise Refusal(f'apt gap in {recipe}: instruction boundaries are malformed')
        if (recipe, instruction) in seen:
            raise Refusal(f'apt gap in {recipe}: duplicate recorded instruction')
        seen.add((recipe, instruction))
        gaps.append(AptOpenGap(recipe, instruction, members['owner']))
    return tuple(gaps)


def parse_inventory(text: str) -> ParentInventory:
    members = _exact(strict_json(text.encode(), INVENTORY),
                     {'schema', 'pins', 'recipes', 'apt_open_gaps'}, INVENTORY)
    if members['schema'] != 1:
        raise Refusal('unsupported parent inventory schema')
    pins = {}
    for name, value in _members(members['pins'], 'pins').items():
        if name not in PINS:
            raise Refusal(f'unknown pin: {name}')
        pins[name] = parse_pin(name, value)
    if set(pins) != set(PINS):
        raise Refusal(f'pin inventory is incomplete: {sorted(set(PINS) - set(pins))}')
    recipes = parse_recipes(members['recipes'])
    return ParentInventory(pins, recipes, parse_apt_open_gaps(members['apt_open_gaps'], recipes))


def inventory_json(pins: dict, recipes: dict[str, tuple[ParentSpec, ...]],
                   apt_open_gaps: tuple[AptOpenGap, ...] = ()) -> dict:
    return {'schema': 1,
            'pins': {name: pins[name].json() for name in PINS},
            'recipes': {path: [{'stage': spec.stage,
                                ('pin' if spec.binding == 'pin' else 'local_base'): spec.pin}
                               for spec in recipes[path]]
                        for path in sorted(recipes)},
            'apt_open_gaps': [gap.json() for gap in apt_open_gaps]}


def inventory_text(pins: dict, recipes: dict[str, tuple[ParentSpec, ...]],
                   apt_open_gaps: tuple[AptOpenGap, ...] = ()) -> bytes:
    return (json.dumps(inventory_json(pins, recipes, apt_open_gaps), indent=2) + '\n').encode()


# --- recipe binding rules ---------------------------------------------------

def walk_recipes(root: Path) -> dict[str, str]:
    recipes = {}
    for base in ('images', 'examples'):
        for path in sorted((root / base).rglob('Dockerfile')):
            recipes[path.relative_to(root).as_posix()] = path.read_text()
    return recipes


REPOSITORY_PINS = ((DEBIAN_PIN, 'debian'), (GO_PIN, 'golang'), (RELEASE_PIN, STANDARD_REPOSITORY))


def image_repository(image: str) -> str:
    """The repository identity of an image reference, ignoring tag and digest."""
    name = image.split('@', 1)[0]
    head, separator, tail = name.rpartition(':')
    return head if separator and '/' not in tail else name


def pin_for_image(image: str) -> str | None:
    """The pin a resolved FROM image addresses, compared by repository identity."""
    repository = image_repository(image)
    for name, expected in REPOSITORY_PINS:
        if repository == expected:
            return name
    return None


def local_base_for(path: str) -> str:
    return 'extension' if path.startswith('examples/base-extension/') else 'standard'


def derive_recipes(root: Path) -> dict[str, tuple[ParentSpec, ...]]:
    """First-adoption recipe records, derived from the recipes themselves."""
    recipes = {}
    for path, text in walk_recipes(root).items():
        specs = []
        for instruction in parse_froms(text):
            name = pin_for_image(instruction.resolved_image)
            if name is not None:
                specs.append(ParentSpec(instruction.stage, 'pin', name))
            elif instruction.resolved_image == LOCAL_BASE:
                specs.append(ParentSpec(instruction.stage, 'local_base', local_base_for(path)))
            else:
                raise Refusal(f'{path}: unresolvable parent {instruction.resolved_image}')
        if not specs:
            raise Refusal(f'{path}: no parent FROM instruction')
        recipes[path] = tuple(specs)
    return recipes


def check_recipe_parents(inventory: ParentInventory, recipes: dict[str, str]) -> None:
    """Exact recipe set, FROM order/count/stage and literal pinned-reference equality.

    A tag-only parent, a same-digest reference at another repository, a new FROM
    or an unlisted recipe all fail here: the inventory is the single record of
    what each maintained recipe builds on.
    """
    if set(recipes) != set(inventory.recipes):
        raise Refusal(f'recipe inventory mismatch: missing {sorted(set(inventory.recipes) - set(recipes))}, '
                      f'unlisted {sorted(set(recipes) - set(inventory.recipes))}')
    for path in sorted(recipes):
        froms = parse_froms(recipes[path])
        specs = inventory.recipes[path]
        if len(froms) != len(specs):
            raise Refusal(f'{path}: {len(froms)} FROM instructions but {len(specs)} inventory entries')
        for instruction, spec in zip(froms, specs):
            if instruction.stage != spec.stage:
                raise Refusal(f'{path}: FROM stage {instruction.stage} does not match inventory '
                              f'stage {spec.stage}')
            if spec.binding == 'pin':
                pin = inventory.pins[spec.pin]
                if instruction.resolved_image != pin.reference.text():
                    raise Refusal(f'{path}: FROM {instruction.resolved_image} is not the pinned '
                                  f'{spec.pin} reference {pin.reference.text()}')
                if '$' not in instruction.raw_image and instruction.raw_image != pin.reference.text():
                    raise Refusal(f'{path}: a literal FROM must stay inside its pinned repository')
            elif instruction.resolved_image != LOCAL_BASE:
                raise Refusal(f'{path}: local base must resolve to {LOCAL_BASE}, not '
                              f'{instruction.resolved_image}')
            elif spec.pin != local_base_for(path):
                raise Refusal(f'{path}: local base {spec.pin} does not describe this recipe')
            elif pin_for_image(instruction.resolved_image) is not None:
                raise Refusal(f'{path}: a local base must not address a pinned parent')


def check_apt_open_gaps(inventory: ParentInventory, recipes: dict[str, str]) -> None:
    """Every recorded gap must still be exactly one instruction of its recipe.

    The image-source gate owns which instructions need an entry; the stager owns
    preservation, so a refresh can never drop, move or silently rewrite the
    reviewed legacy instructions it is not allowed to regenerate.
    """
    for gap in inventory.apt_open_gaps:
        if gap.recipe not in recipes:
            raise Refusal(f'apt gap recipe is not a maintained recipe: {gap.recipe}')
        found = physical_instructions(recipes[gap.recipe]).count(gap.instruction)
        if found != 1:
            raise Refusal(f'apt gap changed/closed: review or remove exact instruction entry for '
                          f'{gap.recipe} (owner #96)')


def staged_inventory(text: str, recipes: dict[str, str]) -> ParentInventory:
    inventory = parse_inventory(text)
    check_recipe_parents(inventory, recipes)
    check_apt_open_gaps(inventory, recipes)
    return inventory


# --- live registry resolution ----------------------------------------------

def docker_json(reference: str) -> tuple[bytes, object]:
    argv = ['docker', 'buildx', 'imagetools', 'inspect', '--raw', reference]
    proc = subprocess.run(argv, check=False, capture_output=True, timeout=900)
    if proc.returncode:
        detail = proc.stderr.decode(errors='replace').strip()
        raise Refusal(f'registry resolution failed for {reference} ({proc.returncode}): {detail}')
    if not proc.stdout:
        raise Refusal(f'registry returned no bytes for {reference}')
    return proc.stdout, strict_json(proc.stdout, f'registry index {reference}')


def official_platform_key(platform: object, context: str) -> str | None:
    record = _members(platform, context)
    if record.get('os') != 'linux':
        return None
    architecture = record.get('architecture')
    if architecture == 'amd64':
        if 'variant' in record:
            raise Refusal(f'{context}: amd64 must not declare a variant')
        return 'linux/amd64'
    if architecture == 'arm64':
        if record.get('variant') != 'v8':
            raise Refusal(f'{context}: official arm64 must declare variant v8')
        return 'linux/arm64'
    return None


def released_platform_key(platform: object, context: str) -> str | None:
    record = _members(platform, context)
    if record.get('os') != 'linux' or record.get('architecture') not in ('amd64', 'arm64'):
        return None
    if 'variant' in record:
        raise Refusal(f'{context}: a released platform must not declare a variant')
    return 'linux/' + record['architecture']


def index_children(raw: bytes, parsed: object, context: str, keyer) -> dict[str, dict]:
    """The runnable children of an index, keyed by platform.

    Attestation descriptors (`unknown/unknown`) are unsigned metadata, not
    runnable platforms, so they are ignored rather than trusted. A duplicate
    target child is refused because the selection would otherwise be ambiguous.
    """
    members = _members(parsed, context)
    if members.get('schemaVersion') != 2:
        raise Refusal(f'{context}: unsupported index schema')
    if members.get('mediaType') not in INDEX_MEDIA_TYPES:
        raise Refusal(f'{context}: unsupported index media type')
    children: dict[str, dict] = {}
    for descriptor in _sequence(members.get('manifests'), f'{context} manifests'):
        entry = _members(descriptor, f'{context} descriptor')
        if entry.get('mediaType') not in MANIFEST_MEDIA_TYPES:
            raise Refusal(f'{context}: unsupported child media type')
        key = keyer(entry.get('platform'), context)
        if key is None:
            continue
        if key in children:
            raise Refusal(f'{context}: duplicate {key} child')
        Digest.parse(entry.get('digest'))
        children[key] = entry
    return children


def require_platforms(children: dict[str, dict], platforms: tuple[str, ...], context: str) -> None:
    if set(children) != set(platforms):
        raise Refusal(f'{context}: exactly {", ".join(platforms)} children are required')


def fetch_child(repository: str, descriptor: dict, context: str) -> dict:
    digest = Digest.parse(descriptor.get('digest'))
    raw, parsed = docker_json(f'{repository}@{digest.value}')
    if Digest.of(raw) != digest:
        raise Refusal(f'{context}: child bytes do not hash to {digest.value}')
    members = _members(parsed, f'{context} child')
    if members.get('schemaVersion') != 2 or members.get('mediaType') not in MANIFEST_MEDIA_TYPES:
        raise Refusal(f'{context}: unsupported child manifest')
    return members


def annotation(descriptor: dict, child: dict, key: str, context: str) -> str:
    values = []
    for source in (descriptor.get('annotations'), child.get('annotations')):
        if not isinstance(source, dict):
            continue
        value = source.get(key)
        if value is None:
            continue
        if not isinstance(value, str) or not value:
            raise Refusal(f'{context}: {key} annotation must be a nonempty string')
        values.append(value)
    if not values:
        raise Refusal(f'{context}: missing {key} annotation')
    if len(set(values)) > 1:
        raise Refusal(f'{context}: conflicting {key} annotations')
    return values[0]


def check_source(raw: str, policy: OfficialPolicy, revision: str) -> None:
    """Repository equality against the reviewed source, not a URL prefix.

    Only two shapes pass: the reviewed repository URL (whose `.git` suffix may be
    omitted) and, for docker-library images, that URL plus the
    `#<revision>:<major.minor>/<suite>` build fragment. Everything else — a
    lookalike host or path, `.git.evil`, a query, userinfo, a port, encoded
    separators or an extra fragment — is refused.
    """
    if not isinstance(raw, str) or '%' in raw or '\\' in raw:
        raise Refusal(f'{policy.name}: encoded or escaped source URL: {raw!r}')
    parsed = urllib.parse.urlsplit(raw)
    expected = urllib.parse.urlsplit(policy.expected_source)
    if parsed.scheme != 'https' or parsed.netloc != expected.netloc:
        raise Refusal(f'{policy.name}: source is not the reviewed repository: {raw}')
    if parsed.query or parsed.username or parsed.password:
        raise Refusal(f'{policy.name}: source carries query, userinfo or parameters: {raw}')
    names = {expected.path,
             expected.path[:-4] if expected.path.endswith('.git') else expected.path}
    if parsed.path not in names:
        raise Refusal(f'{policy.name}: source is not the reviewed repository: {raw}')
    if not parsed.fragment:
        return
    if not policy.revision_fragment:
        raise Refusal(f'{policy.name}: unexpected source fragment: {raw}')
    match = GO_FRAGMENT.fullmatch(parsed.fragment)
    if match is None:
        raise Refusal(f'{policy.name}: unexpected source fragment: {raw}')
    if match.group('revision') != revision:
        raise Refusal(f'{policy.name}: source fragment revision differs from the image revision')
    tag = GO_TAG.fullmatch(policy.tag)
    if tag is None:
        raise Refusal(f'{policy.name}: a Go tag must be golang:<version>-trixie')
    major, minor = tag.group('version').split('.')[:2]
    if match.group('major_minor') != f'{major}.{minor}':
        raise Refusal(f'{policy.name}: source fragment directory differs from the selected tag')


def resolve_official(policy: OfficialPolicy, resolved_on: Date, *,
                     required_digest: Digest | None = None) -> tuple[OfficialPin, str]:
    """Resolve one official image, verifying digest bytes and provenance."""
    repository, _, tag = policy.tag.partition(':')
    raw, parsed = docker_json(policy.tag)
    index_digest = Digest.of(raw)
    note = 'resolved by tag'
    if required_digest is not None and required_digest != index_digest:
        if policy.name != GO_PIN:
            raise Refusal(f'{policy.name}: tag {policy.tag} resolves to {index_digest.value}, not the '
                          f'committed digest {required_digest.value}')
        # The committed pin is the reviewed selection: resolve it by digest and
        # report the tag drift instead of bumping the toolchain silently.
        raw, parsed = docker_json(f'{repository}@{required_digest.value}')
        index_digest = Digest.of(raw)
        if index_digest != required_digest:
            raise Refusal(f'{policy.name}: committed digest {required_digest.value} no longer resolves')
        note = (f'tag {policy.tag} now resolves to a different index; the pin retains the committed '
                f'digest {required_digest.value} (a Go toolchain update belongs to its coordinated change)')
    elif required_digest is not None:
        note = 'tag and committed digest agree'
    children = index_children(raw, parsed, f'{policy.name} index', official_platform_key)
    require_platforms(children, OFFICIAL_PLATFORMS, f'{policy.name} index')
    platforms = {}
    for platform in OFFICIAL_PLATFORMS:
        descriptor = children[platform]
        child = fetch_child(repository, descriptor, f'{policy.name} {platform}')
        source = annotation(descriptor, child, 'org.opencontainers.image.source',
                            f'{policy.name} {platform}')
        revision = annotation(descriptor, child, 'org.opencontainers.image.revision',
                              f'{policy.name} {platform}')
        created = annotation(descriptor, child, 'org.opencontainers.image.created',
                             f'{policy.name} {platform}')
        check_source(source, policy, revision)
        platforms[platform] = OfficialPlatform(Digest.parse(descriptor['digest']),
                                              SourceRevision.parse(revision), source,
                                              Timestamp.parse(created))
    reference = PinnedReference(Repository.parse(repository), tag, index_digest)
    return OfficialPin(reference, policy.expected_source, platforms,
                       policy.verification(resolved_on), resolved_on), note


# --- released-standard authentication --------------------------------------

def external(context: str, action) -> None:
    """Run one external verification, reporting its own stderr as a refusal.

    `attestations.verify` and `public_download` already bound and trace their
    commands; the maintenance CLI must not turn their failure into a traceback,
    and it must not swallow the reason either.
    """
    try:
        action()
    except subprocess.CalledProcessError as error:
        detail = error.stderr
        if isinstance(detail, bytes):
            detail = detail.decode(errors='replace')
        raise Refusal(f'{context} failed ({error.returncode}): '
                      f'{(detail or "").strip() or error}') from error
    except (ValueError, OSError) as error:
        raise Refusal(f'{context} failed: {error}') from error


def download_release_assets(version: str, scratch: Path) -> None:
    scratch.mkdir(parents=True, exist_ok=False)
    for name in ('release.json', 'release-metadata.sigstore.json', 'release-index.sigstore.json'):
        external(f'download of {name}', lambda name=name: operations.public_download(
            name, version=version, out=scratch))


def bound_release_platform(value: object, context: str) -> dict:
    """Only the fields a parent pin binds, from a historical release inventory.

    Deliberately not `content.PlatformInventory.parse`: v0.1.3 predates today's
    selection set, so the production reader would reject bytes that were
    correctly signed. Extra fields are ignored; every bound field is checked.
    """
    members = _members(value, context)
    if members.get('product') != 'standard':
        raise Refusal(f'{context}: not a standard product inventory')
    graph = _members(members.get('graph'), f'{context} graph')
    if graph.get('os') != 'linux' or graph.get('architecture') not in ('amd64', 'arm64'):
        raise Refusal(f'{context}: unsupported platform')
    manifest = _members(graph.get('manifest'), f'{context} manifest')
    invocation = members.get('invocation_url')
    if not isinstance(invocation, str) or not invocation:
        raise Refusal(f'{context}: invocation URL is required')
    selections = _members(members.get('selections'), f'{context} selections')
    if not selections or any(not isinstance(item, str) or not item for item in selections.values()):
        raise Refusal(f'{context}: empty selection record')
    return {'architecture': graph['architecture'], 'manifest': Digest.parse(manifest.get('digest')),
            'invocation_url': invocation}


def resolve_release(version: str, scratch: Path, *, expected_digest: Digest | None,
                    repository: str, resolved_on: Date) -> tuple[ReleasePin, str]:
    """Authenticate a published standard release and bind it as a parent."""
    download_release_assets(version, scratch)
    release_json = scratch / 'release.json'
    metadata_bundle = scratch / 'release-metadata.sigstore.json'
    index_bundle = scratch / 'release-index.sigstore.json'
    # 1. Authenticate the original bytes before reading a single field from them.
    external('release metadata authentication', lambda: attestations.verify(
        release_json, metadata_bundle, source_sha=None))
    release = _members(strict_json(release_json.read_bytes(), 'release metadata'),
                       'release metadata')
    if release.get('schema_version') != 1 or release.get('product') != 'standard':
        raise Refusal('release metadata is not a schema-1 standard release')
    if release.get('version') != version:
        raise Refusal(f'release metadata is version {release.get("version")!r}, not {version!r}')
    source_sha = SourceRevision.parse(release.get('source_sha'))
    index_digest = Digest.parse(release.get('index_digest'))
    if expected_digest is not None and index_digest != expected_digest:
        raise Refusal(f'authenticated release index {index_digest.value} is not the expected '
                      f'{expected_digest.value}')
    run_id, attempt = release.get('build_run_id'), release.get('build_run_attempt')
    if not all(isinstance(item, str) and re.fullmatch(r'[1-9][0-9]*', item)
               for item in (run_id, attempt)):
        raise Refusal('release metadata carries no build run/attempt')
    invocation = f'{release_content.SOURCE}/actions/runs/{run_id}/attempts/{attempt}'
    if release.get('invocation_url') != invocation:
        raise Refusal('release invocation URL does not match its run/attempt')
    if release.get('source_url') != release_content.SOURCE:
        raise Refusal('release metadata source URL is not this repository')
    if release.get('release_tag') != 'v' + version:
        raise Refusal('release metadata tag does not match its version')
    platforms = _sequence(release.get('platforms'), 'release platforms')
    if len(platforms) != 2:
        raise Refusal('release metadata must describe exactly two platforms')
    records = {}
    for value in platforms:
        record = bound_release_platform(value, 'release platform')
        key = 'linux/' + record['architecture']
        if key in records:
            raise Refusal(f'release metadata repeats the {key} platform')
        if record['invocation_url'] != invocation:
            raise Refusal('release platform invocation URL disagrees with the release')
        records[key] = record
    require_platforms(records, RELEASE_PLATFORMS, 'release metadata')
    # 2. Bind the authenticated bytes to the source SHA and the invocation.
    external('release metadata source binding', lambda: attestations.verify(
        release_json, metadata_bundle, source_sha=source_sha.value, invocation=invocation))
    external('release index authentication', lambda: attestations.verify(
        f'oci://{repository}@{index_digest.value}', index_bundle,
        source_sha=source_sha.value, digest=release_content.Sha256(index_digest.value),
        invocation=invocation))
    # 3. The registry must serve exactly the authenticated index and children.
    raw, parsed = docker_json(f'{repository}@{index_digest.value}')
    if Digest.of(raw) != index_digest:
        raise Refusal('registry index bytes do not hash to the authenticated digest')
    children = index_children(raw, parsed, 'released standard index', released_platform_key)
    require_platforms(children, RELEASE_PLATFORMS, 'released standard index')
    for platform in RELEASE_PLATFORMS:
        descriptor = children[platform]
        fetch_child(repository, descriptor, f'released standard {platform}')
        served = Digest.parse(descriptor['digest'])
        if served != records[platform]['manifest']:
            raise Refusal(f'{platform}: registry child {served.value} is not the authenticated '
                          f'{records[platform]["manifest"].value}')
    pin = ReleasePin(PinnedReference(Repository.parse(repository), None, index_digest), version,
                     {platform: ReleasePlatform(records[platform]['manifest'])
                      for platform in RELEASE_PLATFORMS},
                     GithubAttestation(attestations.REPO, attestations.WORKFLOW, 'refs/heads/main',
                                       source_sha, sha256_file(release_json), invocation),
                     resolved_on)
    return pin, f'authenticated v{version} at {invocation}'


# --- Debian apt policy extraction -------------------------------------------

def debian_stanzas(text: str) -> list[dict[str, tuple[str, ...]]]:
    """Deb822 stanzas with comment lines dropped and multi-value fields split."""
    stanzas, current = [], {}
    for line in text.splitlines():
        if line.startswith('#'):
            continue
        if not line.strip():
            if current:
                stanzas.append(current)
                current = {}
            continue
        if line[0] in ' \t':
            raise Refusal('debian.sources carries continuation lines')
        name, separator, value = line.partition(':')
        if not separator:
            raise Refusal(f'debian.sources has a non-field line: {line!r}')
        if name in current:
            raise Refusal(f'debian.sources repeats the {name} field')
        current[name] = tuple(value.split())
    if current:
        stanzas.append(current)
    return stanzas


def check_debian_policy(sources: bytes) -> None:
    """Exactly the reviewed Debian repository objects, and nothing else."""
    text = sources.decode('utf-8')
    stanzas = debian_stanzas(text)
    if len(stanzas) != len(DEBIAN_OBJECTS):
        raise Refusal(f'debian.sources must declare exactly {len(DEBIAN_OBJECTS)} stanzas, '
                      f'not {len(stanzas)}')
    for stanza, (uri, suites) in zip(stanzas, DEBIAN_OBJECTS):
        unknown = set(stanza) - {'Types', 'URIs', 'Suites', 'Components', 'Signed-By'}
        if unknown:
            raise Refusal(f'debian.sources carries an unreviewed field: {sorted(unknown)}')
        if stanza.get('Types', ('deb',)) != ('deb',):
            raise Refusal('a debian.sources stanza must be deb only')
        if stanza.get('URIs') != (uri,) or stanza.get('Suites') != suites:
            raise Refusal(f'debian.sources URI/suite policy changed: {stanza}')
        if stanza.get('Components') != DEBIAN_COMPONENTS:
            raise Refusal('debian.sources Components policy changed')
        if stanza.get('Signed-By') != (DEBIAN_SIGNED_BY,):
            raise Refusal(f'debian.sources Signed-By policy changed: {stanza.get("Signed-By")}')


@dataclass(frozen=True)
class AptTree:
    sources: bytes
    keyrings: dict[str, bytes]


def inside_copy(root: Path, relative: str) -> Path:
    """The copied-tree path for `relative`, refusing a link anywhere on the way.

    `docker cp` copies a symlink as a link, so a component below the roots (not
    only the root itself) can still point at a host path. The caller gets a
    resolved path that is proved to stay inside the copy.
    """
    path = root
    for component in PurePosixPath(relative).parts:
        path = path / component
        if path.is_symlink():
            raise Refusal(f'parent apt tree component is a symlink: {relative}')
    resolved = path.resolve()
    if not resolved.is_relative_to(root.resolve()):
        raise Refusal(f'parent apt tree path leaves the copy: {relative}')
    return resolved


def read_apt_tree(apt: Path, keyrings: Path) -> AptTree:
    # `docker cp` copies a symlink as a link, so a parent that ships /etc/apt or
    # /usr/share/keyrings as a link could redirect these reads at host files.
    if apt.is_symlink() or keyrings.is_symlink():
        raise Refusal('parent apt tree is a symlink; refusing to read it')
    sources = inside_copy(apt, 'sources.list.d/debian.sources')
    if not sources.is_file():
        raise Refusal('parent has no regular /etc/apt/sources.list.d/debian.sources')
    if inside_copy(apt, 'trusted.gpg').exists():
        raise Refusal('parent ships /etc/apt/trusted.gpg; the reviewed policy has none')
    signed = inside_copy(keyrings, PurePosixPath(DEBIAN_SIGNED_BY).name)
    if not signed.is_file():
        raise Refusal(f'parent is missing the reviewed Signed-By key {DEBIAN_SIGNED_BY}')
    files = {'usr/share/keyrings/' + signed.name: signed.read_bytes()}
    trusted = inside_copy(apt, 'trusted.gpg.d')
    if trusted.is_dir():
        for entry in sorted(trusted.iterdir()):
            keyring = inside_copy(apt, 'trusted.gpg.d/' + entry.name)
            if not keyring.is_file():
                raise Refusal(f'unreviewed apt keyring entry: {entry.name}')
            files['etc/apt/trusted.gpg.d/' + entry.name] = keyring.read_bytes()
    return AptTree(sources.read_bytes(), files)


def keyrings_text(keyrings: dict[str, bytes]) -> bytes:
    return ''.join(f'{hashlib.sha256(keyrings[path]).hexdigest()}  {path}\n'
                   for path in sorted(keyrings)).encode()


def extract_apt_policy(pin: OfficialPin, scratch: Path) -> dict[str, bytes]:
    """Copy the verified parents' apt policy without starting a container."""
    scratch.mkdir(parents=True, exist_ok=False)
    trees = {}
    for platform in OFFICIAL_PLATFORMS:
        arch = platform.split('/')[1]
        reference = f'{pin.reference.repository.value}@{pin.platforms[platform].manifest.value}'
        pull = subprocess.run(['docker', 'pull', '--platform', platform, reference], check=False,
                              capture_output=True, timeout=1800)
        if pull.returncode:
            detail = pull.stderr.decode(errors='replace').strip()
            raise Refusal(f'cannot pull {reference}: {detail}')
        created = subprocess.run(['docker', 'create', '--platform', platform, reference],
                                 check=False, capture_output=True, text=True, timeout=600)
        if created.returncode or not created.stdout.strip():
            raise Refusal(f'cannot create {reference}: {created.stderr.strip()}')
        container = created.stdout.strip()
        try:
            target = scratch / arch
            target.mkdir()
            for source, destination in ((f'{container}:/etc/apt', target / 'apt'),
                                        (f'{container}:/usr/share/keyrings', target / 'keyrings')):
                copied = subprocess.run(['docker', 'cp', source, str(destination)], check=False,
                                        capture_output=True, text=True, timeout=600)
                if copied.returncode:
                    raise Refusal(f'cannot extract {source}: {copied.stderr.strip()}')
            # Only the container this invocation created is removed.
            trees[arch] = read_apt_tree(target / 'apt', target / 'keyrings')
        finally:
            subprocess.run(['docker', 'rm', '-f', container], check=False, capture_output=True,
                           timeout=300)
    check_debian_policy(trees['amd64'].sources)
    if trees['amd64'].sources != trees['arm64'].sources:
        raise Refusal('amd64 and arm64 parent apt sources differ')
    if trees['amd64'].keyrings != trees['arm64'].keyrings:
        raise Refusal('amd64 and arm64 parent keyrings differ')
    return {DEBIAN_SOURCES: trees['amd64'].sources,
            KEYRINGS: keyrings_text(trees['amd64'].keyrings)}


# --- selection --------------------------------------------------------------

def parse_release_version(text: str, context: str) -> str:
    try:
        release_content.parse_version(text)
    except ValueError as error:
        raise Refusal(f'{context}: release version must be exactly X.Y.Z') from error
    return text


def go_mod_toolchain(root: Path) -> str:
    """The selected Go patch release, without the `go` prefix go.mod writes."""
    for line in (root / 'images/recipe-contract/go.mod').read_text().splitlines():
        if line.startswith('toolchain go'):
            return line.split()[1].strip().removeprefix('go')
    raise Refusal('go.mod declares no toolchain directive')


def go_committed_reference(root: Path) -> str:
    for instruction in parse_froms((root / 'images/Dockerfile').read_text()):
        if instruction.resolved_image.startswith('golang:'):
            return instruction.resolved_image
    raise Refusal('the base Dockerfile has no golang builder stage')


def go_tag_default(root: Path) -> str:
    return go_committed_reference(root).split('@', 1)[0]


def parse_selection(pin: str | None, selection: str | None, bootstrap: bool,
                    root: Path) -> Request:
    if bootstrap:
        if pin is not None or selection is not None:
            raise Refusal('--bootstrap resolves every pin and takes no selection')
        return Request(PINS, True, go_tag_default(root), STANDARD_VERSION)
    if pin not in PINS:
        raise Refusal(f'unknown pin {pin!r}; expected one of {", ".join(PINS)}')
    if pin == DEBIAN_PIN:
        if selection is not None:
            raise Refusal('debian takes no selection; the reviewed candidate is ' + DEBIAN_TAG)
        return Request((DEBIAN_PIN,), False, None, None)
    if pin == GO_PIN:
        tag = selection or go_tag_default(root)
        match = GO_TAG.fullmatch(tag)
        if match is None:
            raise Refusal('golang-builder takes golang:<version>-trixie')
        toolchain = go_mod_toolchain(root)
        if match.group('version') != toolchain:
            raise Refusal(f'Go selection {tag} differs from the go.mod toolchain {toolchain}; '
                          'a toolchain change is coordinated with it')
        return Request((GO_PIN,), False, tag, None)
    if selection is None:
        raise Refusal('standard-release requires an explicit X.Y.Z version')
    parse_release_version(selection, 'standard-release')
    return Request((RELEASE_PIN,), False, None, selection)


# --- admission, destinations and staging -----------------------------------

def base_input_roots(root: Path) -> tuple[str, ...]:
    """The canonical images/ root names, read from the generator that owns them."""
    result = subprocess.run(['/bin/bash', str(root / BASE_INPUTS), 'inputs'], cwd=str(root),
                            capture_output=True, text=True, check=False)
    if result.returncode != 0:
        raise Refusal(f'{BASE_INPUTS} inputs failed ({result.returncode}): '
                      f'{result.stderr.strip() or result.stdout.strip()}')
    roots = tuple(line for line in result.stdout.splitlines() if line)
    if not roots:
        raise Refusal(f'{BASE_INPUTS} reported no base input roots')
    return roots


def git_command(root: Path, arguments: list[str], environment: dict | None = None,
                stdin: bytes | None = None) -> str:
    result = subprocess.run(['git', '-C', str(root), *arguments], input=stdin, env=environment,
                            capture_output=True, check=False)
    if result.returncode != 0:
        raise Refusal(f'git {arguments[0]} failed ({result.returncode}): '
                      + result.stderr.decode(errors='replace').strip())
    return result.stdout.decode()


def candidate_index(root: Path, staged: dict[str, bytes], tracked: list[str],
                    index: Path) -> dict:
    """An index that also lists the paths this proposal publishes.

    The manifest names the *tracked* inputs, and a refresh can publish a newly
    tracked one before anybody can `git add` it (the first apt extraction). Git's
    own answer is an index that is not the real one: a copy of the committed
    index plus a placeholder entry for each published path, so the generator sees
    the same selection the following commit will produce. The real index and the
    worktree are untouched.
    """
    git_dir = Path(git_command(root, ['rev-parse', '--absolute-git-dir']).strip())
    environment = dict(os.environ, GIT_INDEX_FILE=str(index))
    if (git_dir / 'index').exists():
        shutil.copyfile(git_dir / 'index', index)
    else:
        git_command(root, ['read-tree', 'HEAD'], environment)
    published = [path for path in staged if path.startswith('images/')]
    extra = [path for path in published if path not in tracked]
    if extra:
        # The generator reads mode and stage from the index and hashes the tree,
        # so a placeholder object is enough and no blob has to be written.
        blob = git_command(root, ['hash-object', '--stdin'], stdin=b'').strip()
        for path in extra:
            git_command(root, ['update-index', '--add', '--cacheinfo', f'100644,{blob},{path}'],
                        environment)
    return environment


def base_input_identity(root: Path, staged: dict[str, bytes], out: Path) -> dict[str, bytes]:
    """Render the base manifest and the extension default for this proposal.

    The generator hashes a tree, never the working copy: an unrelated local edit
    in an input this selection did not stage must not reach the identity, and the
    reviewed manifest must describe exactly the bytes the proposal publishes.
    """
    tree = out.parent / (out.name + '.base-inputs')
    rendered = out.parent / (out.name + '.base-inputs.out')
    index = out.parent / (out.name + '.base-inputs.index')
    for scratch in (tree, rendered, index):
        if scratch.exists():
            raise Refusal(f'refusing an existing base-input scratch path: {scratch}')
    pathspecs = [f'images/{name}' for name in base_input_roots(root)]
    tracked = [entry for entry in
               git_command(root, ['ls-files', '-z', '--', *pathspecs]).split('\0') if entry]
    if not tracked:
        raise Refusal('the tracked base input set is empty')
    environment = candidate_index(root, staged, tracked, index)
    tree.mkdir(parents=True)
    for path in tracked:
        relative = PurePosixPath(path).relative_to('images')
        destination = tree.joinpath(*relative.parts)
        destination.parent.mkdir(parents=True, exist_ok=True)
        # The preflight proved every tracked input clean, so the working copy is
        # the committed bytes; this export makes that explicit.
        destination.write_bytes((root / path).read_bytes())
    for path, raw in staged.items():
        if not path.startswith('images/'):
            continue
        relative = PurePosixPath(path).relative_to('images')
        destination = tree.joinpath(*relative.parts)
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(raw)
    external('rendering the repository base identity', lambda: subprocess.run(
        ['/bin/bash', str(root / BASE_INPUTS), '--stage', str(tree), str(rendered)],
        cwd=str(root), env=environment, capture_output=True, text=True, check=True))
    return {
        BASE_MANIFEST: (rendered / 'base-inputs.sha256').read_bytes(),
        EXTENSION: (rendered / 'extension.Dockerfile').read_bytes(),
    }


def committed_inventory(root: Path) -> ParentInventory | None:
    path = root / INVENTORY
    return parse_inventory(path.read_text()) if path.exists() else None


def admission_state(root: Path) -> str:
    """`bootstrap`, `existing`, or a refusal for any partial adoption."""
    inventory = (root / INVENTORY).exists()
    policy = [name for name in POLICY_FILES if (root / name).exists()]
    if not inventory and not policy:
        return 'bootstrap'
    if inventory and len(policy) == len(POLICY_FILES):
        return 'existing'
    raise Refusal('partial adoption; restore or recover before refreshing')


def require_state(request: Request, root: Path) -> 'ParentInventory | None':
    """Admission is a single state machine: one bootstrap, then refreshes."""
    state = admission_state(root)
    if request.bootstrap:
        if state != 'bootstrap':
            raise Refusal('--bootstrap is only valid before the inventory is adopted')
        return None
    if state != 'existing':
        raise Refusal('there is no inventory to refresh; run --bootstrap first')
    return committed_inventory(root)


def destination_paths(request: Request, root: Path,
                      inventory: ParentInventory | None) -> list[str]:
    """The complete ordered destination list for this selection, without network."""
    paths = {INVENTORY}
    recipes = inventory.recipes if inventory is not None else derive_recipes(root)
    for path, specs in recipes.items():
        if any(spec.binding == 'pin' and spec.pin in request.pins for spec in specs):
            paths.add(path)
    if RELEASE_PIN in request.pins:
        paths.add(LAYERS_README)
    if DEBIAN_PIN in request.pins:
        paths |= set(POLICY_FILES)
    if any(pin in request.pins for pin in LOCAL_PINS):
        # Only the local parents change base inputs; a release refresh touches the
        # example layers, whose sources are not part of the base manifest.
        paths |= {BASE_MANIFEST, EXTENSION}
    return sorted(paths)


def merge_pins(existing: dict | None, resolved: dict) -> dict:
    """Keep committed review data unless the resolved identity actually changed.

    A hash-only exception is reviewed data, not resolver output. An unchanged
    digest keeps it verbatim, including `resolved_on`. A changed digest is a new
    selection to review, not a new review: the rationale, its evidence, the
    reassessment condition and the previous `reviewed_on` all stay exactly as
    the reviewer left them, so the now-stale date stays visible until a human
    bumps it. Only `resolved_on` advances.
    """
    pins = dict(existing or {})
    for name, pin in resolved.items():
        previous = pins.get(name)
        if isinstance(pin, OfficialPin) and isinstance(previous, OfficialPin):
            resolved_on = previous.resolved_on
            if pin.reference.digest != previous.reference.digest:
                resolved_on = pin.resolved_on
            pin = OfficialPin(pin.reference, pin.expected_source, pin.platforms,
                              previous.verification, resolved_on)
        if isinstance(pin, ReleasePin) and isinstance(previous, ReleasePin) \
                and pin.reference.digest == previous.reference.digest \
                and pin.version == previous.version:
            pin = ReleasePin(pin.reference, pin.version, pin.platforms, previous.verification,
                             previous.resolved_on)
        pins[name] = pin
    return pins


def rewrite_from_reference(text: str, old: str, new: str, path: str) -> str:
    """Replace the image token (or its global ARG default) of the FROM addressing `old`."""
    for instruction in parse_froms(text):
        if instruction.resolved_image != old:
            continue
        if '$' in instruction.raw_image:
            match = ARG_REFERENCE.fullmatch(instruction.raw_image)
            if match is None:
                raise Refusal(f'{path}: unsupported FROM expansion {instruction.raw_image}')
            name = match.group(1)
            defaults = global_args(logical_instructions(text))
            return replace_line(text, f'ARG {name}={defaults[name]}', f'ARG {name}={new}', path)
        return replace_token(text, instruction.raw_image, new, path)
    raise Refusal(f'{path}: no FROM instruction addresses {old}')


def replace_line(text: str, old_line: str, new_line: str, path: str) -> str:
    lines = text.splitlines(keepends=True)
    hits = [number for number, line in enumerate(lines) if line.strip() == old_line]
    if len(hits) != 1:
        raise Refusal(f'{path}: expected exactly one {old_line!r}, found {len(hits)}')
    lines[hits[0]] = new_line + ('\n' if lines[hits[0]].endswith('\n') else '')
    return ''.join(lines)


def replace_token(text: str, token: str, replacement: str, path: str) -> str:
    count = text.count(token)
    if count != 1:
        raise Refusal(f'{path}: expected one occurrence of {token}, found {count}')
    return text.replace(token, replacement)


def current_reference(text: str, name: str) -> str:
    for instruction in parse_froms(text):
        if pin_for_image(instruction.resolved_image) == name:
            return instruction.resolved_image
    raise Refusal(f'no FROM instruction addresses the {name} parent')


def rewrite_recipes(inventory: ParentInventory | None, pins: dict, request: Request,
                    root: Path) -> dict[str, bytes]:
    """Candidate recipe bytes for every recipe this selection touches."""
    staged = {}
    recipes = inventory.recipes if inventory is not None else derive_recipes(root)
    for path, specs in recipes.items():
        bound = [spec.pin for spec in specs if spec.binding == 'pin' and spec.pin in request.pins]
        if not bound:
            continue
        text = (root / path).read_text()
        for name in bound:
            old = inventory.pins[name].reference.text() if inventory is not None \
                else current_reference(text, name)
            new = pins[name].reference.text()
            if old == new:
                continue
            if name == RELEASE_PIN:
                # Digest comments beside the ARG must move with the pin.
                if text.count(old) < 1:
                    raise Refusal(f'{path}: cannot locate the pinned reference {old}')
                text = text.replace(old, new)
            else:
                text = rewrite_from_reference(text, old, new, path)
        staged[path] = text.encode()
    return staged


def stage(request: Request, root: Path, out: Path) -> list[Destination]:
    if out.exists():
        raise Refusal(f'refusing an existing staging directory: {out}')
    inventory = require_state(request, root)
    today = Date(datetime.datetime.now(datetime.timezone.utc).date().isoformat())
    resolved: dict = {}
    notes: list[str] = []
    if DEBIAN_PIN in request.pins:
        pin, note = resolve_official(OfficialPolicy(DEBIAN_PIN, DEBIAN_TAG, DEBIAN_SOURCE, False),
                                     today)
        resolved[DEBIAN_PIN] = pin
        notes.append(f'{DEBIAN_PIN}: {note}')
    if GO_PIN in request.pins:
        required = None
        if request.bootstrap:
            required = Digest.parse(go_committed_reference(root).split('@', 1)[1])
        pin, note = resolve_official(OfficialPolicy(GO_PIN, request.go_tag, GO_SOURCE, True), today,
                                     required_digest=required)
        resolved[GO_PIN] = pin
        notes.append(f'{GO_PIN}: {note}')
    if RELEASE_PIN in request.pins:
        expected = None
        if request.bootstrap:
            expected = Digest.parse(STANDARD_BOOTSTRAP_DIGEST)
        elif isinstance(inventory.pins[RELEASE_PIN], ReleasePin) \
                and inventory.pins[RELEASE_PIN].version == request.version:
            expected = inventory.pins[RELEASE_PIN].reference.digest
        scratch = out.parent / (out.name + '.release')
        if scratch.exists():
            raise Refusal(f'refusing an existing release scratch directory: {scratch}')
        pin, note = resolve_release(request.version, scratch, expected_digest=expected,
                                    repository=STANDARD_REPOSITORY, resolved_on=today)
        resolved[RELEASE_PIN] = pin
        notes.append(f'{RELEASE_PIN}: {note}')
    pins = merge_pins(inventory.pins if inventory else None, resolved)
    recipes = inventory.recipes if inventory is not None else derive_recipes(root)
    # Bootstrap records no exception: a legacy instruction needs a human to add
    # the exact entry, and the image-source gate refuses to commit without it.
    gaps = inventory.apt_open_gaps if inventory is not None else ()
    staged: dict[str, bytes] = {INVENTORY: inventory_text(pins, recipes, gaps)}
    staged.update(rewrite_recipes(inventory, pins, request, root))
    if RELEASE_PIN in request.pins:
        text = (root / LAYERS_README).read_text()
        old = inventory.pins[RELEASE_PIN].reference.text() if inventory is not None \
            else f'{STANDARD_REPOSITORY}@{STANDARD_BOOTSTRAP_DIGEST}'
        new = pins[RELEASE_PIN].reference.text()
        if text.count(old) < 1 and old != new:
            raise Refusal(f'{LAYERS_README}: no reference to {old}')
        staged[LAYERS_README] = text.replace(old, new).encode()
    if DEBIAN_PIN in request.pins:
        apt_scratch = out.parent / (out.name + '.apt')
        if apt_scratch.exists():
            raise Refusal(f'refusing an existing apt scratch directory: {apt_scratch}')
        staged.update(extract_apt_policy(resolved[DEBIAN_PIN], apt_scratch))
    if any(pin in request.pins for pin in LOCAL_PINS):
        staged.update(base_input_identity(root, staged, out))
    # Overlay the staged bytes on the recipes that exist on disk, not on the
    # recipes the inventory lists: otherwise an unlisted new recipe could never
    # be seen here and a refresh would leave it at the old digest.
    candidates = walk_recipes(root)
    for path, raw in staged.items():
        if path in candidates:
            candidates[path] = raw.decode()
    staged_inventory(staged[INVENTORY].decode(), candidates)
    out.mkdir(parents=True)
    destinations = []
    for relative in sorted(staged):
        target = out / 'staged' / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(staged[relative])
        destinations.append(Destination(PurePosixPath(relative), target))
    (out / 'destinations.json').write_text(
        json.dumps([destination.relative_path.as_posix() for destination in destinations]) + '\n')
    (out / 'summary.txt').write_text(summary(request, root, inventory, pins, notes,
                                            [destination.relative_path.as_posix()
                                             for destination in destinations], staged))
    return destinations


# --- proposal summary -------------------------------------------------------

def summary(request: Request, root: Path, inventory: ParentInventory | None, pins: dict,
            notes: list[str], destinations: list[str], staged: dict[str, bytes]) -> str:
    lines = [f'parent-pins: proposal ({request.label()})']
    for name in PINS:
        pin = pins[name]
        if inventory is not None:
            lines.append(f'before {name}: {inventory.pins[name].reference.text()}')
        lines.append(f'after  {name}: {pin.reference.text()}')
        if isinstance(pin, OfficialPin):
            lines.append(f'  expected source: {pin.expected_source}')
            for platform in OFFICIAL_PLATFORMS:
                record = pin.platforms[platform]
                lines.append(f'  {platform}: {record.manifest.value} revision {record.revision.value} '
                             f'created {record.created.value}')
                lines.append(f'  {platform} source: {record.source}')
            verification = pin.verification
            lines.append(f'  verification {verification.method} reviewed_on '
                         f'{verification.reviewed_on.value} resolved_on {pin.resolved_on.value}')
            lines.append(f'  reason: {verification.reason}')
            lines.append('  evidence: ' + ' '.join(verification.evidence))
            lines.append(f'  reassess: {verification.reassess}')
        else:
            verification = pin.verification
            lines.append(f'  version {pin.version} resolved_on {pin.resolved_on.value}')
            for platform in RELEASE_PLATFORMS:
                lines.append(f'  {platform}: {pin.platforms[platform].manifest.value}')
            lines.append(f'  verification {verification.method} repository {verification.repository}')
            lines.append(f'  signer workflow {verification.signer_workflow} '
                         f'source ref {verification.source_ref}')
            lines.append(f'  source sha {verification.source_sha.value} '
                         f'release.json {verification.release_json_sha256.value}')
            lines.append(f'  invocation {verification.invocation_url}')
    if request.bootstrap:
        lines.append('proposal: every reviewed hash-only exception above is proposed for review; '
                     'set reviewed_on to the actual review date before merging')
    for name in request.pins:
        if inventory is not None and inventory.pins[name].reference.digest != pins[name].reference.digest:
            if isinstance(pins[name], OfficialPin):
                lines.append(f'hash-only exception requires renewed review: {name} '
                             f'{pins[name].reference.digest.value} (reviewed_on '
                             f'{pins[name].verification.reviewed_on.value}; set it to the review date)')
    lines += notes
    gaps = inventory.apt_open_gaps if inventory is not None else ()
    if gaps:
        lines.append(f'apt open gaps preserved exactly ({len(gaps)} instructions, owner #96)')
        lines += [f'  {gap.recipe}: ' + gap.instruction.split('\n', 1)[0] for gap in gaps]
    elif request.bootstrap:
        lines.append('apt open gaps: bootstrap records none; a legacy apt source or key '
                     'installation must be added by hand for review (#96)')
    if BASE_MANIFEST in staged:
        identity = 'sha256:' + hashlib.sha256(staged[BASE_MANIFEST]).hexdigest()
        committed = root / BASE_MANIFEST
        state = 'unchanged' if committed.exists() \
            and committed.read_bytes() == staged[BASE_MANIFEST] else 'changed'
        lines.append(f'base identity {identity} ({state}; {EXTENSION} default follows it)')
    for relative in POLICY_FILES:
        if relative not in staged:
            continue
        previous = (root / relative).read_bytes() if (root / relative).exists() else b''
        current = staged[relative]
        if not previous:
            lines.append(f'apt policy {relative}: new file')
        elif previous == current:
            lines.append(f'apt policy {relative}: unchanged')
        else:
            lines.append(f'apt policy {relative}: changed')
            lines += ['  ' + line for line in difflib.unified_diff(
                previous.decode().splitlines(), current.decode().splitlines(),
                'committed/' + relative, 'proposed/' + relative, lineterm='', n=3)]
    lines.append('destinations:')
    lines += ['  ' + path for path in destinations]
    return '\n'.join(lines) + '\n'


# --- CLI --------------------------------------------------------------------

def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    sub = parser.add_subparsers(dest='command', required=True)
    for name in ('stage', 'destinations'):
        cli = sub.add_parser(name)
        cli.add_argument('pin', nargs='?')
        cli.add_argument('selection', nargs='?')
        cli.add_argument('--root', required=True, type=Path)
        if name == 'stage':
            cli.add_argument('--out', required=True, type=Path)
            cli.add_argument('--bootstrap', action='store_true')
        else:
            cli.add_argument('--bootstrap', action='store_true')
    args = parser.parse_args(argv)
    root = args.root.resolve()
    try:
        request = parse_selection(args.pin, args.selection, args.bootstrap, root)
        if args.command == 'destinations':
            for path in destination_paths(request, root, require_state(request, root)):
                print(path)
            return 0
        if getattr(args, 'out', None) is None:
            raise Refusal('stage requires --out')
        stage(request, root, args.out)
        return 0
    except Refusal as error:
        print(f'parent-pins: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main())
