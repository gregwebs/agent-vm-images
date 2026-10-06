#!/usr/bin/env python3
"""Strict OCI graph and standard-release file contracts. No registry/build client."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import gzip
import hashlib
import json
import os
import re
import shutil
import sys
import tarfile
import tempfile
import release_trace
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO, Literal

Arch = Literal['amd64', 'arm64']
MANIFEST = 'application/vnd.oci.image.manifest.v1+json'
INDEX = 'application/vnd.oci.image.index.v1+json'
CONFIG = 'application/vnd.oci.image.config.v1+json'
LAYERS = {'application/vnd.oci.image.layer.v1.tar', 'application/vnd.oci.image.layer.v1.tar+gzip'}
COMPRESSED_CAP = 4 * 1024 ** 3
UNCOMPRESSED_CAP = 8 * 1024 ** 3
TAR_CAP = COMPRESSED_CAP + 64 * 1024 ** 2
JSON_CAP = 16 * 1024 ** 2
SOURCE = 'https://github.com/gregwebs/agent-vm-images'
SELECTIONS = {'codex', 'opencode', 'claude', 'copilot', 'dsh', 'pnpm', 'pi', 'pi-claude-bridge'}


@dataclass(frozen=True)
class ImageVersion:
    value: str


@dataclass(frozen=True)
class Sha256:
    value: str


def parse_arch(value: str) -> Arch:
    if value not in ('amd64', 'arm64'):
        raise ValueError('architecture must be amd64 or arm64')
    return value


def parse_version(text: str) -> ImageVersion:
    if not isinstance(text, str) or not re.fullmatch(r'(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)', text):
        raise ValueError('invalid stable image version')
    return ImageVersion(text)


def parse_sha256(text: str) -> Sha256:
    if not isinstance(text, str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', text):
        raise ValueError('invalid SHA-256 identity')
    return Sha256(text)


def mapping(value: object, context: str) -> dict[str, object]:
    if not isinstance(value, dict):
        raise ValueError(context + ': JSON object required')
    return value


def sequence(value: object, context: str) -> list[object]:
    if not isinstance(value, list):
        raise ValueError(context + ': JSON array required')
    return value


def object_keys(value: object, keys: set[str]) -> dict:
    if not isinstance(value, dict) or set(value) != keys:
        raise ValueError(f'expected exact fields {sorted(keys)}')
    return value


def positive(value: object) -> int:
    if type(value) is not int or value <= 0:
        raise ValueError('expected positive integer')
    return value


def no_duplicates(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError('duplicate JSON key')
        result[key] = value
    return result


def read_json(path: Path) -> dict:
    if not path.is_file() or path.stat().st_size > JSON_CAP or path.stat().st_size == 0:
        raise ValueError(f'invalid/oversized JSON file: {path}')
    result = json.loads(path.read_bytes(), object_pairs_hook=no_duplicates)
    if not isinstance(result, dict):
        raise ValueError('JSON object required')
    return result


def hash_stream(stream: BinaryIO, limit: int) -> tuple[Sha256, int]:
    digest, size = hashlib.sha256(), 0
    while True:
        chunk = stream.read(1024 * 1024)
        if not chunk:
            break
        size += len(chunk)
        if size > limit:
            raise ValueError('content exceeds committed resource cap')
        digest.update(chunk)
    return Sha256('sha256:' + digest.hexdigest()), size


def hash_file(path: Path, limit: int = TAR_CAP) -> tuple[Sha256, int]:
    if path.is_symlink() or not path.is_file():
        raise ValueError(f'required regular file missing: {path}')
    with path.open('rb') as stream:
        return hash_stream(stream, limit)


def atomic_json(path: Path, value: object) -> None:
    fd, temporary = tempfile.mkstemp(prefix='.validated-', dir=path.parent)
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(value, stream, indent=2)
            stream.write('\n')
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


@dataclass(frozen=True)
class Descriptor:
    digest: Sha256
    size: int
    media_type: str

    @classmethod
    def parse(cls, value: dict, *, oci: bool = False) -> Descriptor:
        if not isinstance(value, dict):
            raise ValueError('descriptor object required')
        if not oci:
            object_keys(value, {'digest', 'size', 'media_type'})
        elif not {'digest', 'size', 'mediaType'} <= set(value):
            raise ValueError('incomplete OCI descriptor')
        return cls(parse_sha256(value['digest']), positive(value['size']),
                   value['mediaType' if oci else 'media_type'])

    def json(self) -> dict:
        return {'digest': self.digest.value, 'size': self.size, 'media_type': self.media_type}

    def blob(self, layout: Path) -> Path:
        return layout / 'blobs/sha256' / self.digest.value[7:]

    def verify(self, layout: Path) -> None:
        digest, size = hash_file(self.blob(layout), COMPRESSED_CAP)
        if (digest, size) != (self.digest, self.size):
            raise ValueError(f'blob digest/size mismatch: {self.digest.value}')


@dataclass(frozen=True)
class ImageGraph:
    arch: Arch
    manifest: Descriptor
    config: Descriptor
    layers: tuple[Descriptor, ...]
    diff_ids: tuple[Sha256, ...]
    uncompressed_bytes: int

    def json(self) -> dict:
        return {'os': 'linux', 'architecture': self.arch, 'manifest': self.manifest.json(),
                'config': self.config.json(), 'layers': [x.json() for x in self.layers],
                'diff_ids': [x.value for x in self.diff_ids],
                'uncompressed_bytes': self.uncompressed_bytes}

    @classmethod
    def parse(cls, value: dict) -> ImageGraph:
        object_keys(value, {'os', 'architecture', 'manifest', 'config', 'layers', 'diff_ids',
                            'uncompressed_bytes'})
        if value['os'] != 'linux':
            raise ValueError('Linux graph required')
        graph = cls(parse_arch(value['architecture']), Descriptor.parse(value['manifest']),
                    Descriptor.parse(value['config']), tuple(Descriptor.parse(x) for x in sequence(value['layers'], 'graph layers')),
                    tuple(parse_sha256(x) for x in sequence(value['diff_ids'], 'graph diff IDs')), positive(value['uncompressed_bytes']))
        graph.validate()
        return graph

    def validate(self) -> None:
        if self.manifest.media_type != MANIFEST or self.config.media_type != CONFIG:
            raise ValueError('unsupported manifest/config media type')
        if not self.layers or len(self.layers) != len(self.diff_ids):
            raise ValueError('ordered layer/diff_id length mismatch')
        if self.uncompressed_bytes > UNCOMPRESSED_CAP:
            raise ValueError('uncompressed graph exceeds cap')
        descriptors = (self.manifest, self.config) + self.layers
        metadata = {}
        for descriptor in descriptors:
            previous = metadata.setdefault(descriptor.digest, descriptor)
            if previous != descriptor:
                raise ValueError('conflicting descriptor metadata for repeated digest')
        if sum(x.size for x in set(descriptors)) > COMPRESSED_CAP:
            raise ValueError('compressed graph exceeds cap')
        if any(layer.media_type not in LAYERS for layer in self.layers):
            raise ValueError('unsupported layer media type')


def inventory_layout(path: Path, arch: Arch, *, require_platform: bool = False) -> ImageGraph:
    path = path.resolve()
    if read_json(path / 'oci-layout') != {'imageLayoutVersion': '1.0.0'}:
        raise ValueError('unsupported OCI layout version')
    index = read_json(path / 'index.json')
    if index.get('schemaVersion') != 2 or index.get('mediaType', INDEX) != INDEX:
        raise ValueError('unsupported layout index')
    roots = index.get('manifests')
    if not isinstance(roots, list) or len(roots) != 1:
        raise ValueError('exactly one runnable root image required')
    root = roots[0]
    manifest = Descriptor.parse(root, oci=True)
    if manifest.media_type != MANIFEST:
        raise ValueError('root must describe a runnable OCI image manifest')
    manifest.verify(path)
    raw = read_json(manifest.blob(path))
    if raw.get('schemaVersion') != 2 or raw.get('mediaType') != MANIFEST:
        raise ValueError('unsupported child manifest')
    config = Descriptor.parse(raw['config'], oci=True)
    if config.media_type != CONFIG:
        raise ValueError('unsupported config')
    config.verify(path)
    cfg = read_json(config.blob(path))
    if cfg.get('os') != 'linux' or cfg.get('architecture') != arch:
        raise ValueError('config/expected architecture mismatch')
    expected_platform = {'os': 'linux', 'architecture': arch}
    if ('platform' in root and root['platform'] != expected_platform) or (require_platform and 'platform' not in root):
        raise ValueError('root/config platform disagreement or missing archive platform')
    if require_platform and not mapping(root.get('annotations', {}), 'root annotations').get('org.opencontainers.image.ref.name'):
        raise ValueError('archive root ref-name annotation required')
    rootfs = mapping(cfg['rootfs'], 'config rootfs')
    if rootfs.get('type') != 'layers' or not isinstance(rootfs.get('diff_ids'), list):
        raise ValueError('invalid config rootfs')
    layers = tuple(Descriptor.parse(x, oci=True) for x in sequence(raw['layers'], 'manifest layers'))
    diff_ids = tuple(parse_sha256(x) for x in rootfs['diff_ids'])
    if not layers or len(layers) != len(diff_ids):
        raise ValueError('ordered layer/diff_id count mismatch')
    uncompressed = 0
    for layer, diff_id in zip(layers, diff_ids):
        if layer.media_type not in LAYERS:
            raise ValueError('unsupported layer media type')
        layer.verify(path)
        with layer.blob(path).open('rb') as stream:
            if layer.media_type.endswith('+gzip'):
                with gzip.GzipFile(fileobj=stream) as decoded:
                    digest, size = hash_stream(decoded, UNCOMPRESSED_CAP - uncompressed)
            else:
                digest, size = hash_stream(stream, UNCOMPRESSED_CAP - uncompressed)
        if digest != diff_id:
            raise ValueError('ordered rootfs diff_id mismatch')
        uncompressed += size
    graph = ImageGraph(arch, manifest, config, layers, diff_ids, uncompressed)
    graph.validate()
    allowed = {'oci-layout', 'index.json'} | {
        'blobs/sha256/' + x.digest.value[7:] for x in (manifest, config) + layers}
    for member in path.rglob('*'):
        name = member.relative_to(path).as_posix()
        if member.is_symlink() or (not member.is_file() and not member.is_dir()) or (member.is_file() and name not in allowed):
            raise ValueError(f'unreferenced/unsafe layout member: {name}')
        # Buildx leaves an empty destination-side ingestion directory after
        # success. It is not shipped in the tar and must contain no residue.
        if member.is_dir() and name not in ('blobs', 'blobs/sha256'):
            if name != 'ingest' or any(member.iterdir()):
                raise ValueError('unexpected layout directory or incomplete ingest')
    return graph


def normalize_root(path: Path, graph: ImageGraph) -> None:
    index = read_json(path / 'index.json')
    root = index['manifests'][0]
    root['platform'] = {'os': 'linux', 'architecture': graph.arch}
    mapping(root.setdefault('annotations', {}), 'root annotations')['org.opencontainers.image.ref.name'] = 'standard'
    atomic_json(path / 'index.json', index)


def extract_archive(path: Path, target: Path) -> None:
    seen, total = set(), 0
    with tarfile.open(path, mode='r|') as archive:
        for member in archive:
            name = member.name.rstrip('/') if member.isdir() else member.name
            if name in seen:
                raise ValueError('duplicate archive member')
            seen.add(name)
            if member.isdir() and name in ('blobs', 'blobs/sha256'):
                (target / name).mkdir(exist_ok=True)
                continue
            allowed = name in ('oci-layout', 'index.json') or re.fullmatch(r'blobs/sha256/[0-9a-f]{64}', name)
            if not allowed or not member.isreg() or member.size <= 0:
                raise ValueError(f'unsafe archive member: {member.name}')
            total += member.size
            if total > COMPRESSED_CAP + JSON_CAP:
                raise ValueError('archive content exceeds resource cap')
            if member.size > os.statvfs(target).f_bavail * os.statvfs(target).f_frsize:
                raise ValueError('insufficient extraction capacity')
            destination = target / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            stream = archive.extractfile(member)
            if stream is None:
                raise ValueError('unreadable archive member')
            with destination.open('xb') as output:
                shutil.copyfileobj(stream, output, 1024 * 1024)
            if destination.stat().st_size != member.size:
                raise ValueError('truncated archive member')


def verify_archive(path: Path, expected: ImageGraph) -> ImageGraph:
    if path.stat().st_size > TAR_CAP:
        raise ValueError('archive framing cap exceeded')
    with tempfile.TemporaryDirectory(prefix='oci-check-', dir=path.parent) as directory:
        layout = Path(directory)
        extract_archive(path, layout)
        actual = inventory_layout(layout, expected.arch, require_platform=True)
        compare_platforms(actual, expected)
        return actual


def check_msb_graph(value: object, graph: ImageGraph) -> None:
    image = mapping(value, 'msb inspection')
    config = mapping(image.get('config'), 'msb config')
    layers = [mapping(x, 'msb layer') for x in sequence(image.get('layers'), 'msb layers')]
    if (image.get('digest') != graph.manifest.digest.value or config.get('digest') != graph.config.digest.value
            or image.get('os') != 'linux' or image.get('architecture') != graph.arch
            or [x.get('blob_digest') for x in layers] != [x.digest.value for x in graph.layers]
            or [x.get('diff_id') for x in layers] != [x.value for x in graph.diff_ids]
            or [x.get('media_type') for x in layers] != [x.media_type for x in graph.layers]):
        raise ValueError('msb ordered graph identities mismatch')


def compare_platforms(registry: ImageGraph, archive: ImageGraph) -> None:
    if registry != archive:
        raise ValueError('platform manifest/config/ordered compressed layers/diff_ids differ')
    release_trace.emit(['compare-platforms'], 0, json.dumps({'registry': registry.json(), 'archive': archive.json()}).encode())


@dataclass(frozen=True)
class ArchivePart:
    name: str
    sha256: Sha256
    size: int

    @classmethod
    def parse(cls, value: dict) -> ArchivePart:
        object_keys(value, {'name', 'sha256', 'size'})
        name = value['name']
        if not isinstance(name, str) or not re.fullmatch(r'[a-zA-Z0-9][a-zA-Z0-9._-]*', name):
            raise ValueError('unsafe asset filename')
        return cls(name, parse_sha256(value['sha256']), positive(value['size']))

    def json(self) -> dict:
        return {'name': self.name, 'sha256': self.sha256.value, 'size': self.size}

    def verify(self, directory: Path) -> None:
        if hash_file(directory / self.name) != (self.sha256, self.size):
            raise ValueError(f'asset checksum/size mismatch: {self.name}')


@dataclass(frozen=True)
class PlatformInventory:
    version: ImageVersion
    source_sha: str
    run_id: str
    run_attempt: str
    invocation_url: str
    graph: ImageGraph
    archive: ArchivePart
    parts: tuple[ArchivePart, ...]
    sbom: ArchivePart
    selections: tuple[tuple[str, str], ...]
    docker_reported_id: Sha256
    docker_driver: str
    docker_version: str

    @classmethod
    def parse(cls, value: dict) -> PlatformInventory:
        object_keys(value, {'schema_version', 'product', 'version', 'source_sha', 'source_url',
                            'build_run_id', 'build_run_attempt', 'invocation_url', 'graph', 'archive',
                            'parts', 'sbom', 'selections', 'docker_reported_id', 'docker_driver', 'docker_version'})
        if value['schema_version'] != 1 or value['product'] != 'standard' or value['source_url'] != SOURCE:
            raise ValueError('unsupported release product/schema/source')
        if not isinstance(value['source_sha'], str) or not re.fullmatch(r'[0-9a-f]{40}', value['source_sha']):
            raise ValueError('full source SHA required')
        run, attempt = value['build_run_id'], value['build_run_attempt']
        if not all(isinstance(x, str) and re.fullmatch(r'[1-9][0-9]*', x) for x in (run, attempt)):
            raise ValueError('invalid run/attempt binding')
        invocation = f'{SOURCE}/actions/runs/{run}/attempts/{attempt}'
        if value['invocation_url'] != invocation:
            raise ValueError('invocation URL/run/attempt mismatch')
        selections = object_keys(value['selections'], SELECTIONS)
        if any(not isinstance(x, str) or not x or len(x) > 128 for x in selections.values()):
            raise ValueError('missing standard selection')
        graph = ImageGraph.parse(value['graph'])
        version = parse_version(value['version'])
        archive = ArchivePart.parse(value['archive'])
        basename = f'agent-vm-standard-v{version.value}-linux-{graph.arch}'
        if archive.name != basename + '.oci.tar' or archive.size > TAR_CAP:
            raise ValueError('wrong logical archive filename/size')
        parts = tuple(ArchivePart.parse(x) for x in sequence(value['parts'], 'archive parts'))
        if parts:
            if sum(x.size for x in parts) != archive.size:
                raise ValueError('archive part lengths do not sum to tar length')
            for number, part in enumerate(parts):
                if (part.name != archive.name + f'.part-{number:03d}' or part.size > 1000000000
                        or (number < len(parts) - 1 and part.size != parts[0].size)):
                    raise ValueError('invalid archive part order/size/name')
        elif archive.size >= 2000000000:
            raise ValueError('large archive must be split')
        sbom = ArchivePart.parse(value['sbom'])
        if sbom.name != basename + '.spdx.json' or sbom.size > JSON_CAP:
            raise ValueError('wrong/oversized SBOM asset')
        for field in ('docker_driver', 'docker_version'):
            if not isinstance(value[field], str) or not value[field]:
                raise ValueError('Docker audit metadata required')
        return cls(version, value['source_sha'], run, attempt, invocation, graph, archive, parts,
                   sbom, tuple(sorted(selections.items())), parse_sha256(value['docker_reported_id']),
                   value['docker_driver'], value['docker_version'])

    def json(self) -> dict:
        return {'schema_version': 1, 'product': 'standard', 'version': self.version.value,
                'source_sha': self.source_sha, 'source_url': SOURCE, 'build_run_id': self.run_id,
                'build_run_attempt': self.run_attempt, 'invocation_url': self.invocation_url,
                'graph': self.graph.json(), 'archive': self.archive.json(),
                'parts': [x.json() for x in self.parts], 'sbom': self.sbom.json(),
                'selections': dict(self.selections), 'docker_reported_id': self.docker_reported_id.value,
                'docker_driver': self.docker_driver, 'docker_version': self.docker_version}


@dataclass(frozen=True)
class ReleaseMetadata:
    version: ImageVersion
    source_sha: str
    run_id: str
    run_attempt: str
    index_digest: Sha256
    platforms: tuple[PlatformInventory, ...]

    @classmethod
    def parse(cls, value: dict) -> ReleaseMetadata:
        object_keys(value, {'schema_version', 'product', 'version', 'source_sha', 'source_url',
                            'release_tag', 'build_run_id', 'build_run_attempt', 'invocation_url',
                            'index_digest', 'platforms'})
        platforms = tuple(PlatformInventory.parse(x) for x in sequence(value['platforms'], 'release platforms'))
        if len(platforms) != 2 or {x.graph.arch for x in platforms} != {'amd64', 'arm64'}:
            raise ValueError('release requires exactly amd64 and arm64 standard platforms')
        version = parse_version(value['version'])
        first = platforms[0]
        if (value['schema_version'] != 1 or value['product'] != 'standard'
                or value['source_url'] != SOURCE or value['release_tag'] != 'v' + version.value
                or value['invocation_url'] != first.invocation_url):
            raise ValueError('release product/schema/source/tag/invocation mismatch')
        for platform in platforms:
            if (platform.version != version or platform.source_sha != value['source_sha']
                    or platform.run_id != value['build_run_id']
                    or platform.run_attempt != value['build_run_attempt']):
                raise ValueError('foreign source/version/run/attempt platform inventory')
        return cls(version, value['source_sha'], value['build_run_id'], value['build_run_attempt'],
                   parse_sha256(value['index_digest']), platforms)

    def platform(self, arch: Arch) -> PlatformInventory:
        return next(x for x in self.platforms if x.graph.arch == arch)

    def json(self) -> dict:
        return {'schema_version': 1, 'product': 'standard', 'version': self.version.value,
                'source_sha': self.source_sha, 'source_url': SOURCE, 'release_tag': 'v' + self.version.value,
                'build_run_id': self.run_id, 'build_run_attempt': self.run_attempt,
                'invocation_url': f'{SOURCE}/actions/runs/{self.run_id}/attempts/{self.run_attempt}',
                'index_digest': self.index_digest.value, 'platforms': [x.json() for x in self.platforms]}


def parse_release(path: Path) -> ReleaseMetadata:
    return ReleaseMetadata.parse(read_json(path))


def check_index(path: Path, platforms: tuple[PlatformInventory, ...]) -> Sha256:
    index = read_json(path)
    if index.get('schemaVersion') != 2 or index.get('mediaType') not in (
            INDEX, 'application/vnd.docker.distribution.manifest.list.v2+json'):
        raise ValueError('unsupported registry index')
    roots = index.get('manifests')
    if not isinstance(roots, list) or len(roots) != 2:
        raise ValueError('exactly two runnable platform descriptors required')
    expected = {x.graph.arch: x.graph.manifest for x in platforms}
    seen = set()
    for root in roots:
        root = mapping(root, 'index descriptor')
        arch = parse_arch(mapping(root.get('platform'), 'index platform').get('architecture'))
        if root['platform'] != {'os': 'linux', 'architecture': arch} or arch in seen:
            raise ValueError('duplicate/unsupported runnable platform')
        seen.add(arch)
        if Descriptor.parse(root, oci=True) != expected[arch]:
            raise ValueError('index child descriptor mismatch')
    return hash_file(path, JSON_CAP)[0]


def assemble_archive(platform: PlatformInventory, assets: Path, output: Path) -> None:
    if output.exists():
        raise ValueError('archive output must be unused')
    platform.sbom.verify(assets)
    files = platform.parts or (platform.archive,)
    for part in files:
        part.verify(assets)
    if platform.archive.size > os.statvfs(output.parent).f_bavail * os.statvfs(output.parent).f_frsize:
        raise ValueError('insufficient archive assembly capacity')
    fd, temporary = tempfile.mkstemp(prefix='.assemble-', dir=output.parent)
    try:
        with os.fdopen(fd, 'wb') as stream:
            for part in files:
                with (assets / part.name).open('rb') as source:
                    shutil.copyfileobj(source, stream, 1024 * 1024)
        temp = Path(temporary)
        if hash_file(temp) != (platform.archive.sha256, platform.archive.size):
            raise ValueError('full reconstructed tar checksum mismatch')
        verify_archive(temp, platform.graph)
        release_trace.emit(['assemble-archive', str(output)], 0, json.dumps({'graph': platform.graph.json(),
            'archive': platform.archive.json(), 'parts': [x.json() for x in files]}).encode())
        os.rename(temp, output)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    cli = sub.add_parser('inventory')
    cli.add_argument('--layout', type=Path, required=True)
    cli.add_argument('--arch', required=True)
    cli.add_argument('--output', type=Path, required=True)
    cli.add_argument('--normalize-root', action='store_true')
    cli = sub.add_parser('verify-archive')
    cli.add_argument('--archive', type=Path, required=True)
    cli.add_argument('--graph', type=Path, required=True)
    cli = sub.add_parser('compare')
    cli.add_argument('--registry', type=Path, required=True)
    cli.add_argument('--archive', type=Path, required=True)
    cli = sub.add_parser('assemble-release')
    for name in ('version', 'source-sha', 'run-id', 'run-attempt'):
        cli.add_argument('--' + name, required=True)
    cli.add_argument('--index-raw', type=Path, required=True)
    cli.add_argument('--platform-json', type=Path, action='append', required=True)
    cli.add_argument('--output', type=Path, required=True)
    cli = sub.add_parser('check-release')
    cli.add_argument('--release', type=Path, required=True)
    cli = sub.add_parser('assemble-archive')
    cli.add_argument('--release', type=Path, required=True)
    cli.add_argument('--arch', required=True)
    cli.add_argument('--assets-dir', type=Path, required=True)
    cli.add_argument('--output', type=Path, required=True)
    cli = sub.add_parser('check-evidence')
    cli.add_argument('--release', type=Path, required=True)
    cli.add_argument('--evidence-dir', type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == 'check-evidence':
            from evidence import check_evidence
            check_evidence(args.release, args.evidence_dir)
        elif args.command == 'inventory':
            graph = inventory_layout(args.layout, parse_arch(args.arch))
            if args.normalize_root:
                normalize_root(args.layout, graph)
            atomic_json(args.output, graph.json())
        elif args.command == 'verify-archive':
            verify_archive(args.archive, ImageGraph.parse(read_json(args.graph)))
        elif args.command == 'compare':
            compare_platforms(ImageGraph.parse(read_json(args.registry)), ImageGraph.parse(read_json(args.archive)))
        elif args.command == 'check-release':
            print(json.dumps(parse_release(args.release).json()))
        elif args.command == 'assemble-release':
            platforms = tuple(PlatformInventory.parse(read_json(x)) for x in args.platform_json)
            release = ReleaseMetadata(parse_version(args.version), args.source_sha, args.run_id,
                                      args.run_attempt, check_index(args.index_raw, platforms), platforms)
            release = ReleaseMetadata.parse(release.json())
            atomic_json(args.output, release.json())
        elif args.command == 'assemble-archive':
            assemble_archive(parse_release(args.release).platform(parse_arch(args.arch)), args.assets_dir, args.output)
    except (ValueError, OSError, KeyError, TypeError, EOFError, tarfile.TarError) as error:
        print(f'content: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
