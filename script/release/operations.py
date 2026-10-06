#!/usr/bin/env python3
"""Build, package and distribute one standard graph. Consumption cannot build."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import hashlib
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from dataclasses import dataclass

import attestations
import content as c
import preflight
import public_http
import release_trace

ROOT = Path(__file__).resolve().parents[2]
IMAGE = 'ghcr.io/gregwebs/agent-vm-standard'


@dataclass(frozen=True)
class PackageRequest:
    layout: Path
    docker_ref: str
    archive: Path
    version: str
    arch: c.Arch
    out: Path


@dataclass(frozen=True)
class DownloadRequest:
    version: str
    arch: c.Arch
    out: Path


@dataclass(frozen=True)
class PlatformPublishRequest:
    layout: Path
    metadata: Path
    image: str
    tag: str
    arch: c.Arch


@dataclass(frozen=True)
class PrepareRequest:
    version: str
    source_sha: str
    run_id: str
    run_attempt: str
    platform_dir: tuple[Path, ...]
    out: Path


@dataclass(frozen=True)
class PublishRequest:
    state: Path


def docker_endpoint(value: object) -> str:
    contexts = c.sequence(value, 'Docker context inspection')
    if len(contexts) != 1:
        raise ValueError('Docker context inspection: exactly one context required')
    context = c.mapping(contexts[0], 'Docker context')
    endpoints = c.mapping(context.get('Endpoints'), 'Docker context endpoints')
    docker = c.mapping(endpoints.get('docker'), 'Docker context docker endpoint')
    host = docker.get('Host')
    if not isinstance(host, str) or not host.startswith('unix://'):
        raise ValueError('Docker context: explicit local Unix endpoint required')
    return host


def buildx_plugin(value: object) -> Path:
    plugins = [c.mapping(x, 'Docker CLI plugin') for x in c.sequence(value, 'Docker CLI plugins')]
    matches = [x for x in plugins if x.get('Name') == 'buildx']
    if len(matches) != 1:
        raise ValueError('Docker CLI plugins: exactly one named buildx plugin required')
    path = matches[0].get('Path')
    if not isinstance(path, str) or not path:
        raise ValueError('Docker buildx plugin: nonempty executable path required')
    executable = Path(path).resolve(strict=True)
    if not executable.is_file():
        raise ValueError('Docker buildx plugin: regular executable required')
    return executable


def run(argv: list[str], *, output: Path | None = None, timeout: int = 1800,
        env: dict[str, str] | None = None, stdin: bytes | None = None) -> bytes:
    start = time.monotonic()
    if output is None:
        result = subprocess.run(argv, check=False, capture_output=True, timeout=timeout, env=env, input=stdin)
        release_trace.emit(argv, result.returncode, result.stdout, result.stderr)
        result.check_returncode()
        return result.stdout
    with output.open('wb') as stream:
        stream.write((json.dumps({'argv': argv, 'started_at': int(time.time())}) + '\n').encode())
        stream.flush()
        proc = subprocess.run(argv, stdout=stream, stderr=subprocess.STDOUT, timeout=timeout,
                              env=env, input=stdin, check=False)
        stream.write((json.dumps({'status': proc.returncode, 'seconds': time.monotonic() - start}) + '\n').encode())
    if proc.returncode:
        raise subprocess.CalledProcessError(proc.returncode, argv)
    return b''


def native(arch: c.Arch) -> None:
    actual = {'x86_64': 'amd64', 'aarch64': 'arm64', 'arm64': 'arm64'}.get(platform.machine())
    if actual != arch:
        raise ValueError('requested platform is not the native host architecture')


def source_sha() -> str:
    sha = run(['git', '-C', str(ROOT), 'rev-parse', 'HEAD']).decode().strip()
    if len(sha) != 40 or run(['git', '-C', str(ROOT), 'status', '--porcelain']).strip():
        raise ValueError('release requires clean committed sources')
    return sha


def committed_version(version: str) -> c.ImageVersion:
    value = c.parse_version(version)
    if (ROOT / 'images/standard/version').read_bytes() != (version + '\n').encode():
        raise ValueError('version must equal committed image version file')
    return value


def selections() -> dict[str, str]:
    import re
    dockerfile = (ROOT / 'images/standard/Dockerfile').read_text()
    selected = {}
    for name in ('codex', 'opencode', 'claude', 'copilot'):
        matches = re.findall(r'^ARG AGENT_VERSION_' + name.upper() + r'=(.+)$', dockerfile, re.M)
        if len(matches) != 1:
            raise ValueError('invalid committed single-slot selection')
        selected[name] = matches[0]
    for name, path, dependency in (
        ('dsh', 'dsh', '@deepseek-ai/dsh'), ('pnpm', 'dsh', 'pnpm'),
        ('pi', 'pi', '@earendil-works/pi-coding-agent'),
        ('pi-claude-bridge', 'pi/bridge', 'pi-claude-bridge')):
        selected[name] = c.read_json(ROOT / f'images/tools/{path}/package.json')['dependencies'][dependency]
    return selected


def trusted_run(*, sha: str) -> tuple[str, str, str]:
    run_id, attempt = os.environ['GITHUB_RUN_ID'], os.environ['GITHUB_RUN_ATTEMPT']
    if (os.environ.get('GITHUB_REPOSITORY') != preflight.REPO
            or os.environ.get('GITHUB_REF') != 'refs/heads/main'
            or os.environ.get('GITHUB_SHA') != sha
            or not run_id.isdecimal() or not attempt.isdecimal()
            or int(run_id) < 1 or int(attempt) < 1):
        raise ValueError('exact trusted main workflow run required')
    return run_id, attempt, f'{c.SOURCE}/actions/runs/{run_id}/attempts/{attempt}'


def asset(path: Path) -> c.ArchivePart:
    digest, size = c.hash_file(path)
    return c.ArchivePart(path.name, digest, c.positive(size))


def stage_file(source: Path, destination: Path) -> None:
    if destination.exists():
        raise ValueError('refusing existing staged asset')
    try:
        os.link(source, destination)
    except OSError:
        if source.stat().st_size > shutil.disk_usage(destination.parent).free - 5 * 1024 ** 3:
            raise ValueError('insufficient capacity for output filesystem copy')
        shutil.copyfile(source, destination)


def split_archive(archive: Path, out: Path, *, limit: int = 2000000000,
                  part_size: int = 1000000000) -> tuple[c.ArchivePart, ...]:
    if archive.stat().st_size < limit:
        stage_file(archive, out / archive.name)
        return ()
    split = 'gsplit' if shutil.which('gsplit') else 'split'
    if b'GNU coreutils' not in run([split, '--version']):
        raise ValueError('GNU split required; use gnubin PATH or gsplit on macOS')
    run([split, '-b', str(part_size), '-d', '-a', '3', str(archive), str(out / (archive.name + '.part-'))])
    count = (archive.stat().st_size + part_size - 1) // part_size
    return tuple(asset(out / (archive.name + f'.part-{number:03d}')) for number in range(count))


def checksums(out: Path, names: list[str], *, filename: str = 'SHA256SUMS') -> None:
    if len(names) != len(set(names)) or len(names) > 1024:
        raise ValueError('duplicate/oversized checksum subject list')
    lines = [f'{c.hash_file(out / name)[0].value[7:]}  {name}\n' for name in names]
    (out / filename).write_text(''.join(lines))


def docker_matches(layout: Path, graph: c.ImageGraph, ref: str) -> dict:
    inspected = json.loads(run(['docker', 'image', 'inspect', ref]))
    if not isinstance(inspected, list) or len(inspected) != 1:
        raise ValueError('expected one Docker-loaded image')
    image = c.mapping(inspected[0], 'Docker image')
    c.mapping(image.get('RootFS'), 'Docker rootfs')
    c.mapping(image.get('Config'), 'Docker config')
    if image['Os'] != 'linux' or image['Architecture'] != graph.arch:
        raise ValueError('Docker/layout platform mismatch')
    if image['RootFS']['Layers'] != [x.value for x in graph.diff_ids]:
        raise ValueError('Docker/layout ordered diff_ids mismatch')
    cfg = c.mapping(c.read_json(graph.config.blob(layout)).get('config', {}), 'OCI config')
    defaults = {'Env': [], 'Entrypoint': [], 'Cmd': [], 'WorkingDir': '', 'User': '',
                'Labels': {}, 'ExposedPorts': {}, 'Volumes': {}, 'StopSignal': ''}
    for key, default in defaults.items():
        if (cfg.get(key) or default) != (image['Config'].get(key) or default):
            raise ValueError('Docker/layout runtime config differs: ' + key)
    c.parse_sha256(image['Id'])
    return image


def package_standard(args: PackageRequest) -> None:
    arch = c.parse_arch(args.arch)
    native(arch)
    version = committed_version(args.version)
    sha = source_sha()
    run_id, attempt, invocation = trusted_run(sha=sha)
    graph = c.inventory_layout(args.layout, arch, require_platform=True)
    c.verify_archive(args.archive, graph)
    image = docker_matches(args.layout, graph, args.docker_ref)
    labels = c.mapping(image['Config'].get('Labels') or {}, 'Docker labels')
    expected = selections()
    if any(labels.get('org.agent-vm.version.' + name) != value for name, value in expected.items()):
        raise ValueError('loaded graph does not have committed standard selections')
    for name, value in (('version', version.value), ('revision', sha), ('source', c.SOURCE)):
        if labels.get('org.opencontainers.image.' + name) != value:
            raise ValueError('loaded graph OCI source/version label mismatch')
    args.out.mkdir()
    # This wrapper has no skip-certification interface; artifact health is never
    # inferred from labels. Full source audits run in the build owner as well.
    run(['bash', str(ROOT / 'script/test/standard-image.sh'), '--artifact', args.docker_ref,
         '--platform', 'linux/' + arch], output=args.out / 'package-audit.log', timeout=5400)
    basename = f'agent-vm-standard-v{version.value}-linux-{arch}'
    sbom = args.out / (basename + '.spdx.json')
    run(['syft', 'oci-dir:' + str(args.layout), '-o', 'spdx-json=' + str(sbom)], timeout=1800)
    if sbom.stat().st_size > c.JSON_CAP:
        raise ValueError('SBOM exceeds attestation input bound')
    sbom_json = c.read_json(sbom)
    if not sbom_json.get('spdxVersion') or not sbom_json.get('packages'):
        raise ValueError('invalid/empty SPDX SBOM')
    canonical_tar = args.archive.parent / (basename + '.oci.tar')
    stage_file(args.archive, canonical_tar)
    logical = asset(canonical_tar)
    parts = split_archive(canonical_tar, args.out)
    driver = json.loads(run(['docker', 'info', '--format', '{{json .DriverStatus}}']))
    docker_version = run(['docker', 'version', '--format', '{{.Server.Version}}']).decode().strip()
    inventory = c.PlatformInventory(version, sha, run_id, attempt, invocation, graph, logical,
                                    parts, asset(sbom), tuple(sorted(expected.items())),
                                    c.parse_sha256(image['Id']), json.dumps(driver), docker_version)
    inventory = c.PlatformInventory.parse(inventory.json())
    c.atomic_json(args.out / 'platform.json', inventory.json())
    checksums(args.out, ['platform.json', sbom.name] + [x.name for x in parts or (logical,)])


def authfile() -> Path:
    config = Path(os.environ['DOCKER_CONFIG']).resolve()
    path = config / 'config.json'
    if not path.is_file() or path.is_symlink():
        raise ValueError('owned post-audit Docker login authfile required')
    return path


def authenticated_refs(version: str) -> dict:
    with tempfile.TemporaryDirectory(prefix='release-auth-') as directory:
        client = preflight.Curl(Path(directory))
        preflight.validate_repository(client, os.environ['GITHUB_TOKEN'])
        exists, _ = preflight.owner_inventory(client, os.environ['GHCR_PACKAGE_INVENTORY_TOKEN'])
        return preflight.check_refs(client, workflow_token=os.environ['GITHUB_TOKEN'],
                                    actor=os.environ['GITHUB_ACTOR'], version=version, package_exists=exists)


def publish_standard(args: PlatformPublishRequest) -> None:
    inventory = c.PlatformInventory.parse(c.read_json(args.metadata))
    run_id, attempt, _ = trusted_run(sha=inventory.source_sha)
    if (inventory.run_id, inventory.run_attempt) != (run_id, attempt):
        raise ValueError('foreign native inventory')
    arch = c.parse_arch(args.arch)
    if args.image != IMAGE or args.tag != f'v{inventory.version.value}-{arch}' or arch != inventory.graph.arch:
        raise ValueError('only exact standard platform version ref may be published')
    c.compare_platforms(c.inventory_layout(args.layout, arch, require_platform=True), inventory.graph)
    refs = authenticated_refs(inventory.version.value)
    allowed = {preflight.Outcome.REF_ABSENT, preflight.Outcome.PACKAGE_NOT_CREATED}
    if refs[args.tag] not in allowed or refs[f'v{inventory.version.value}'] not in allowed:
        raise ValueError('platform/index ref occupied or inaccessible before native push')
    if any(x not in allowed | {preflight.Outcome.REF_PRESENT} for x in refs.values()):
        raise ValueError('unknown registry authority before native push')
    with tempfile.TemporaryDirectory(prefix='skopeo-release-') as directory:
        run(['skopeo', '--tmpdir', directory, 'copy', '--authfile', str(authfile()), '--preserve-digests',
             'oci:' + str(args.layout) + ':standard', 'docker://' + IMAGE + ':' + args.tag], timeout=1800)
        raw = run(['skopeo', 'inspect', '--raw', '--authfile', str(authfile()),
                   'docker://' + IMAGE + '@' + inventory.graph.manifest.digest.value])
        if raw != inventory.graph.manifest.blob(args.layout).read_bytes():
            raise ValueError('registry changed canonical platform manifest')
        tagged = run(['skopeo', 'inspect', '--raw', '--authfile', str(authfile()), 'docker://' + IMAGE + ':' + args.tag])
        if tagged != raw:
            raise ValueError('platform version tag does not equal just-pushed digest')


def verify_platform_handoff(directory: Path, *, sha: str, version: str,
                            run_id: str, attempt: str) -> c.PlatformInventory:
    metadata = directory / 'platform.json'
    invocation = f'{c.SOURCE}/actions/runs/{run_id}/attempts/{attempt}'
    # Verify the inventory subject before trusting any of its contents.
    candidates = [directory / f'platform-{arch}-files.sigstore.json' for arch in ('amd64', 'arm64')]
    bundles = [x for x in candidates if x.is_file()]
    if len(bundles) != 1:
        raise ValueError('exactly one named native inventory provenance bundle required')
    attestations.verify(metadata, bundles[0], source_sha=sha, invocation=invocation)
    inventory = c.PlatformInventory.parse(c.read_json(metadata))
    if (inventory.source_sha, inventory.version.value, inventory.run_id, inventory.run_attempt) != (sha, version, run_id, attempt):
        raise ValueError('signed inventory belongs to another source/version/run/attempt')
    arch = inventory.graph.arch
    if bundles[0].name != f'platform-{arch}-files.sigstore.json':
        raise ValueError('inventory/bundle architecture disagreement')
    subject = 'oci://' + IMAGE + '@' + inventory.graph.manifest.digest.value
    attestations.verify(subject, directory / f'platform-{arch}-image.sigstore.json', source_sha=sha,
                        digest=inventory.graph.manifest.digest, invocation=invocation)
    attestations.verify(subject, directory / f'platform-{arch}-sbom.sigstore.json', source_sha=sha,
                        digest=inventory.graph.manifest.digest, predicate=attestations.SPDX, invocation=invocation,
                        spdx_file=directory / inventory.sbom.name)
    for payload in (inventory.parts or (inventory.archive,)) + (inventory.sbom,):
        payload.verify(directory)
        attestations.verify(directory / payload.name, bundles[0], source_sha=sha, invocation=invocation)
    return inventory


def release_absence(version: str) -> None:
    with tempfile.TemporaryDirectory(prefix='release-authority-') as directory:
        preflight.require_release_absent(preflight.Curl(Path(directory)), token=os.environ['GITHUB_TOKEN'], version=version)


def prepare_release(args: PrepareRequest) -> None:
    committed_version(args.version)
    sha = source_sha()
    run_id, attempt, _ = trusted_run(sha=sha)
    if (args.source_sha, args.run_id, args.run_attempt) != (sha, run_id, attempt) or len(args.platform_dir) != 2:
        raise ValueError('assembly source/run/platform handoff mismatch')
    inventories = tuple(verify_platform_handoff(x, sha=sha, version=args.version, run_id=run_id, attempt=attempt)
                        for x in args.platform_dir)
    if {x.graph.arch for x in inventories} != {'amd64', 'arm64'}:
        raise ValueError('assembly requires both native standard platforms')
    args.out.mkdir()
    # Capacity from authenticated signed sizes, before extracting/reassembling.
    from capacity import check_space
    needed = sum(x.archive.size * 2 + sum(d.size for d in set((x.graph.manifest, x.graph.config) + x.graph.layers)) * 2
                 for x in inventories)
    check_space(args.out, needed)
    for directory, inventory in zip(args.platform_dir, inventories):
        with tempfile.TemporaryDirectory(prefix='assembly-content-', dir=args.out) as temp:
            c.assemble_archive(inventory, directory, Path(temp) / 'verified.tar')
        for payload in (inventory.parts or (inventory.archive,)) + (inventory.sbom,):
            stage_file(directory / payload.name, args.out / payload.name)
        arch = inventory.graph.arch
        stage_file(directory / 'platform.json', args.out / f'platform-{arch}.json')
        for kind in ('files', 'image', 'sbom'):
            name = f'platform-{arch}-{kind}.sigstore.json'
            stage_file(directory / name, args.out / name)
        raw = run(['skopeo', 'inspect', '--raw', '--authfile', str(authfile()),
                   'docker://' + IMAGE + ':v' + args.version + '-' + arch])
        if c.Sha256('sha256:' + hashlib.sha256(raw).hexdigest()) != inventory.graph.manifest.digest:
            raise ValueError('remote child ref does not match this verified native inventory')
    refs = authenticated_refs(args.version)
    if refs['v' + args.version] != preflight.Outcome.REF_ABSENT:
        raise ValueError('assembly version index occupied/inaccessible')
    for inventory in inventories:
        if refs['v' + args.version + '-' + inventory.graph.arch] != preflight.Outcome.REF_PRESENT:
            raise ValueError('assembly current-run platform ref missing')
    # Same checker as initial preflight, using fresh job-owned authority, at the
    # first-write boundary. Never check absence against our own later draft.
    release_absence(args.version)
    run(['docker', 'buildx', 'imagetools', 'create', '--tag', IMAGE + ':v' + args.version] +
        [IMAGE + '@' + x.graph.manifest.digest.value for x in inventories])
    raw_index = args.out / 'index-raw.json'
    raw_index.write_bytes(run(['skopeo', 'inspect', '--raw', '--authfile', str(authfile()),
                              'docker://' + IMAGE + ':v' + args.version]))
    digest = c.check_index(raw_index, inventories)
    metadata = c.ReleaseMetadata(c.parse_version(args.version), sha, run_id, attempt, digest, inventories)
    metadata = c.ReleaseMetadata.parse(metadata.json())
    c.atomic_json(args.out / 'release.json', metadata.json())
    payloads = ['release.json']
    for inventory in inventories:
        payloads += [x.name for x in (inventory.parts or (inventory.archive,)) + (inventory.sbom,)]
        payloads += [f'platform-{inventory.graph.arch}.json']
    checksums(args.out, payloads)
    # Recheck before draft creation too: no unrelated draft can be adopted.
    release_absence(args.version)
    draft = json.loads(run(['gh', 'api', '--method', 'POST', f'repos/{preflight.REPO}/releases',
                           '--input', '-'], stdin=json.dumps({'tag_name': 'v' + args.version,
                           'target_commitish': sha, 'name': 'Standard image v' + args.version,
                           'draft': True, 'prerelease': True, 'make_latest': 'false',
                           'body': 'NOT validated/default-ready. Native acceptance and maintainer promotion pending.'}).encode()))
    release_id = c.positive(draft['id'])
    bundles = [f'platform-{arch}-{kind}.sigstore.json' for arch in ('amd64', 'arm64') for kind in ('files', 'image', 'sbom')]
    files = payloads + ['SHA256SUMS'] + bundles + ['release-index.sigstore.json', 'release-metadata.sigstore.json']
    c.atomic_json(args.out / 'state.json', {'release_id': release_id, 'version': args.version, 'source_sha': sha,
                                          'release_subject': c.hash_file(args.out / 'release.json')[0].value,
                                          'index_digest': digest.value, 'files': files})
    checksums(args.out, ['release.json', 'SHA256SUMS'], filename='metadata-subjects.sha256')
    c.atomic_json(args.out / 'result.json', {'index_digest': digest.value, 'out': str(args.out.resolve())})


def anonymous_registry(release: c.ReleaseMetadata, scratch: Path) -> None:
    isolated = public_http.public_env()
    for key in ('GITHUB_TOKEN', 'GH_TOKEN', 'GHCR_PACKAGE_INVENTORY_TOKEN', 'PUBLISH_TOKEN', 'REGISTRY_AUTH_FILE'):
        isolated.pop(key, None)
    auth = scratch / 'anonymous-auth'
    auth.mkdir()
    (auth / 'config.json').write_text('{"auths":{}}\n')
    isolated['DOCKER_CONFIG'] = str(auth)
    isolated['REGISTRY_AUTH_FILE'] = str(auth / 'config.json')
    temporary = scratch / 'skopeo-tmp'
    temporary.mkdir()
    tag_raw = run(['skopeo', 'inspect', '--raw', '--no-creds', '--authfile', str(auth / 'config.json'),
                   'docker://' + IMAGE + ':v' + release.version.value], env=isolated)
    index = scratch / 'public-index.json'
    index.write_bytes(tag_raw)
    if c.check_index(index, release.platforms) != release.index_digest:
        raise ValueError('anonymous version index changed')
    digest_raw = run(['skopeo', 'inspect', '--raw', '--no-creds', '--authfile', str(auth / 'config.json'),
                      'docker://' + IMAGE + '@' + release.index_digest.value], env=isolated)
    if digest_raw != tag_raw:
        raise ValueError('anonymous tag/index digest disagreement')
    for inventory in release.platforms:
        platform_tag = run(['skopeo', 'inspect', '--raw', '--no-creds', '--authfile', str(auth / 'config.json'),
                            'docker://' + IMAGE + ':v' + release.version.value + '-' + inventory.graph.arch], env=isolated)
        if c.Sha256('sha256:' + hashlib.sha256(platform_tag).hexdigest()) != inventory.graph.manifest.digest:
            raise ValueError('anonymous platform version tag changed: ' + inventory.graph.arch)
        layout = scratch / ('registry-' + inventory.graph.arch)
        run(['skopeo', '--tmpdir', str(temporary), '--override-os', 'linux', '--override-arch', inventory.graph.arch,
             'copy', '--src-no-creds', '--authfile', str(auth / 'config.json'), '--preserve-digests',
             'docker://' + IMAGE + '@' + inventory.graph.manifest.digest.value, 'oci:' + str(layout) + ':standard'],
            env=isolated, timeout=1800)
        c.compare_platforms(c.inventory_layout(layout, inventory.graph.arch), inventory.graph)


def publish_release(args: PublishRequest) -> None:
    state = c.read_json(args.state)
    out = args.state.parent
    release = c.parse_release(out / 'release.json')
    run_id, attempt, _ = trusted_run(sha=release.source_sha)
    if (release.run_id, release.run_attempt) != (run_id, attempt):
        raise ValueError('publication continuation belongs to another run/attempt')
    if (state['version'] != release.version.value or state['source_sha'] != release.source_sha
            or state['release_subject'] != c.hash_file(out / 'release.json')[0].value
            or state['index_digest'] != release.index_digest.value):
        raise ValueError('owned continuation state/metadata mismatch')
    draft = json.loads(run(['gh', 'api', f'repos/{preflight.REPO}/releases/{c.positive(state["release_id"])}']))
    if (draft['draft'] is not True or draft['tag_name'] != 'v' + release.version.value
            or draft['target_commitish'] != release.source_sha or draft.get('assets')):
        raise ValueError('recorded draft changed or already contains assets')
    invocation = release.platforms[0].invocation_url
    for name in ('release.json', 'SHA256SUMS'):
        attestations.verify(out / name, out / 'release-metadata.sigstore.json', source_sha=release.source_sha, invocation=invocation)
    attestations.verify('oci://' + IMAGE + '@' + release.index_digest.value, out / 'release-index.sigstore.json',
                        source_sha=release.source_sha, digest=release.index_digest, invocation=invocation)
    for name in state['files']:
        c.ArchivePart.parse(asset(out / name).json())
        run(['gh', 'release', 'upload', 'v' + release.version.value, str(out / name), '--repo', preflight.REPO], timeout=1800)
    with tempfile.TemporaryDirectory(prefix='anonymous-publication-', dir=out) as temp:
        from capacity import check_space
        check_space(Path(temp), sum(x.archive.size + c.COMPRESSED_CAP for x in release.platforms))
        anonymous_registry(release, Path(temp))
    run(['gh', 'release', 'edit', 'v' + release.version.value, '--draft=false', '--prerelease', '--latest=false',
         '--repo', preflight.REPO])


def public_download(name: str, *, version: str, out: Path, limit: int = c.JSON_CAP) -> None:
    c.ArchivePart.parse({'name': name, 'sha256': 'sha256:' + '0' * 64, 'size': 1})
    public_http.transfer(['--fail', '--silent', '--show-error', '--location', '--proto', '=https',
        '--proto-redir', '=https', '--connect-timeout', '15', '--max-time', '1800',
        c.SOURCE + '/releases/download/v' + version + '/' + name], out / name, limit)


def authenticate_metadata(out: Path, version: str) -> c.ReleaseMetadata:
    attestations.verify(out / 'release.json', out / 'release-metadata.sigstore.json', source_sha=None)
    release = c.parse_release(out / 'release.json')
    if release.version.value != version:
        raise ValueError('downloaded release is not requested version')
    attestations.verify(out / 'release.json', out / 'release-metadata.sigstore.json', source_sha=release.source_sha,
                        invocation=release.platforms[0].invocation_url)
    return release


def download_release(args: DownloadRequest) -> None:
    version, arch = c.parse_version(args.version), c.parse_arch(args.arch)
    args.out.mkdir()
    for name in ('release.json', 'release-metadata.sigstore.json'):
        public_download(name, version=version.value, out=args.out)
    release = authenticate_metadata(args.out, version.value)
    inventory = release.platform(arch)
    from capacity import check_space
    check_space(args.out, inventory.archive.size * 3 + c.COMPRESSED_CAP * 2)
    names = [x.name for x in (inventory.parts or (inventory.archive,)) + (inventory.sbom,)]
    names += ['SHA256SUMS', f'platform-{arch}.json', 'release-index.sigstore.json']
    names += [f'platform-{arch}-{kind}.sigstore.json' for kind in ('files', 'image', 'sbom')]
    sizes = {x.name: x.size for x in (inventory.parts or (inventory.archive,)) + (inventory.sbom,)}
    for name in names:
        public_download(name, version=version.value, out=args.out, limit=sizes.get(name, c.JSON_CAP))
    attestations.verify(args.out / 'SHA256SUMS', args.out / 'release-metadata.sigstore.json', source_sha=release.source_sha,
                        invocation=inventory.invocation_url)
    for payload in (inventory.parts or (inventory.archive,)) + (inventory.sbom,):
        payload.verify(args.out)


def main() -> int:
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    cli = sub.add_parser('package')
    for key in ('layout', 'archive', 'out'):
        cli.add_argument('--' + key, type=Path, required=True)
    for key in ('docker-ref', 'version', 'arch'):
        cli.add_argument('--' + key, required=True)
    cli = sub.add_parser('publish-platform')
    for key in ('layout', 'metadata'):
        cli.add_argument('--' + key, type=Path, required=True)
    for key in ('image', 'tag', 'arch'):
        cli.add_argument('--' + key, required=True)
    cli = sub.add_parser('prepare')
    for key in ('version', 'source-sha', 'run-id', 'run-attempt'):
        cli.add_argument('--' + key, required=True)
    cli.add_argument('--platform-dir', action='append', type=Path, required=True)
    cli.add_argument('--out', type=Path, required=True)
    cli = sub.add_parser('publish')
    cli.add_argument('--state', type=Path, required=True)
    cli = sub.add_parser('download')
    cli.add_argument('--version', required=True)
    cli.add_argument('--arch', required=True)
    cli.add_argument('--out', type=Path, required=True)
    args = parser.parse_args()
    try:
        if args.command == 'package':
            package_standard(PackageRequest(args.layout, args.docker_ref, args.archive,
                                           c.parse_version(args.version).value, c.parse_arch(args.arch), args.out))
        elif args.command == 'publish-platform':
            publish_standard(PlatformPublishRequest(args.layout, args.metadata, args.image, args.tag, c.parse_arch(args.arch)))
        elif args.command == 'prepare':
            prepare_release(PrepareRequest(c.parse_version(args.version).value, args.source_sha, args.run_id,
                                           args.run_attempt, tuple(args.platform_dir), args.out))
        elif args.command == 'publish':
            publish_release(PublishRequest(args.state))
        else:
            download_release(DownloadRequest(c.parse_version(args.version).value, c.parse_arch(args.arch), args.out))

    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(f'release {args.command}: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
