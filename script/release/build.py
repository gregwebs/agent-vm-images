#!/usr/bin/env python3
"""Own the sole base/standard build and all mandatory native audits."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import json
import os
import sys
import tempfile
import subprocess
from pathlib import Path

import capability
import capacity
import content as c
import operations as op
import storage
import peaks


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--arch', required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--out', type=Path, required=True)
    parser.add_argument('--scratch-root', type=Path, required=True)
    parser.add_argument('--rehearsal', action='store_true',
                        help='validate a non-publishing rehearsal run instead of a publication run')
    args = parser.parse_args()
    scratch = None
    sampler = None
    result = 0
    try:
        os.umask(0o077)
        arch = c.parse_arch(args.arch)
        op.native(arch)
        op.committed_version(args.version)
        sha = op.source_sha()
        if args.rehearsal:
            op.rehearsal_run(sha=sha)
        else:
            op.trusted_run(sha=sha)
        args.out.mkdir()
        args.scratch_root.mkdir(parents=True, exist_ok=True)
        scratch = Path(tempfile.mkdtemp(prefix='standard-', dir=args.scratch_root)).resolve()
        (scratch / 'tmp').mkdir()
        print('owned scratch (retained on failure): ' + str(scratch), flush=True)
        builder = op.run(['docker', 'context', 'show']).decode().strip()
        inspected = op.run(['docker', 'buildx', 'inspect', builder]).decode()
        import re
        if not re.search(r'^Driver:\s+docker$', inspected, re.M):
            raise ValueError('current context daemon-backed docker builder required')
        prefix = 'avm-release-' + scratch.name.lower().replace('_', 'x')
        storage_file = scratch / 'storage.json'
        sampler = peaks.Sampler(scratch / 'peak-samples.json', [scratch],
                                daemon=lambda: storage.measure(scratch, scratch / 'peak-daemon.json'))
        sampler.start()

        def check(phase: str, *, tar: int | None = None) -> None:
            stats = storage.measure(scratch, storage_file)
            st, space = scratch.stat(), os.statvfs(scratch)
            host = capacity.Filesystem(stats['scratch_host'], str(st.st_dev), str(scratch), space.f_bavail * space.f_frsize)
            daemon = tuple(capacity.Filesystem.parse(x) for x in stats['daemon_filesystems'])
            result = capacity.check(phase, host, daemon, uncompressed=c.UNCOMPRESSED_CAP,
                                    compressed=c.COMPRESSED_CAP, build=25 * capacity.GIB, tar_bytes=tar)
            c.atomic_json(scratch / (phase + '-capacity.json'), result)
            if phase == 'build' and any(x.free_bytes < 25 * capacity.GIB for x in daemon):
                raise ValueError('below existing 25 GiB post-base source capacity floor')

        # Detect exporter/loader/local FROM capability before either heavy recipe.
        capability.check(builder, arch, scratch, prefix)
        check('initial')
        base, standard = prefix + '-base:local', prefix + '-standard:local'
        env = dict(os.environ, TMPDIR=str(scratch / 'tmp'))
        for key in ('GITHUB_TOKEN', 'GH_TOKEN', 'PUBLISH_TOKEN', 'GHCR_PACKAGE_INVENTORY_TOKEN'):
            env.pop(key, None)
        watchdog = ['python3', str(op.ROOT / 'script/test/host-watchdog.py')]
        op.run(watchdog + ['10800', 'docker', 'build', '--builder', builder, '--platform', 'linux/' + arch,
                          '-t', base, '-f', str(op.ROOT / 'images/Dockerfile'), str(op.ROOT / 'images')],
               output=scratch / 'base-build.log', env=env, timeout=10860)
        check('build')
        layout = scratch / 'layout'
        op.run(watchdog + ['10800', 'docker', 'buildx', 'build', '--builder', builder, '--platform', 'linux/' + arch,
                          '--provenance=false', '--sbom=false', '-t', standard, '--build-arg', 'BASE_IMAGE=' + base,
                          '--label', 'org.opencontainers.image.version=' + args.version,
                          '--label', 'org.opencontainers.image.revision=' + sha,
                          '--label', 'org.opencontainers.image.source=' + c.SOURCE,
                          '--output', f'type=oci,dest={layout},tar=false,compression=gzip',
                          '-f', str(op.ROOT / 'images/standard/Dockerfile'), str(op.ROOT / 'images')],
               output=scratch / 'standard-build.log', env=env, timeout=10860)
        graph = c.inventory_layout(layout, arch)
        c.normalize_root(layout, graph)
        c.atomic_json(scratch / 'graph.json', graph.json())
        check('load')
        archive_dir = scratch / 'archive'
        op.run(['bash', str(op.ROOT / 'script/release/archive-oci.sh'), '--layout', str(layout), '--arch', arch,
                '--out', str(archive_dir)], output=scratch / 'archive.log')
        archive = archive_dir / 'archive.oci.tar'
        check('load', tar=archive.stat().st_size)
        op.run(['docker', 'image', 'load', '--input', str(archive)], output=scratch / 'standard-load.log')
        op.docker_matches(layout, graph, standard)
        audits = [(['standard-image.sh', base, standard, '--platform', 'linux/' + arch], 'finished-standard'),
                  (['pi-runtime.sh', base, standard], 'pi-runtime'),
                  (['standard-image.sh', '--pi-audit-status', base, '--platform', 'linux/' + arch], 'pi-access'),
                  (['standard-certification.sh'], 'certification-controls'),
                  (['shipped-installer-network.sh', base, '--platform', 'linux/' + arch], 'strict-egress')]
        for argv, name in audits:
            op.run(watchdog + ['5400', 'bash', str(op.ROOT / 'script/test' / argv[0])] + argv[1:],
                   output=scratch / (name + '.log'), env=env, timeout=5460)
        check('stage', tar=archive.stat().st_size)
        op.package_standard(op.PackageRequest(layout=layout, docker_ref=standard, archive=archive,
                                            version=args.version, arch=arch, out=scratch / 'package'),
                            rehearsal=args.rehearsal)
        package = scratch / 'package'
        for path in package.iterdir():
            op.stage_file(path, args.out / path.name)
        sampler.stop()
        sampler = None
        evidence = args.out / 'evidence'
        evidence.mkdir()
        for path in scratch.iterdir():
            if path.is_file() and path.suffix in ('.log', '.json'):
                op.stage_file(path, evidence / path.name)
        op.run(['docker', 'image', 'rm', standard, base])
        c.atomic_json(args.out / 'result.json', {'layout': str(layout), 'scratch': str(scratch),
                                               'platform_digest': graph.manifest.digest.value,
                                               'sbom': str((args.out / op.c.PlatformInventory.parse(c.read_json(args.out / 'platform.json')).sbom.name).resolve())})
        # Layout is still needed by the post-audit publication step; do not
        # delete it or prune BuildKit/Docker storage here.
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(f'build-standard: {error}; retained scratch: {scratch}', file=sys.stderr)
        result = 1
    finally:
        if sampler is not None:
            try:
                sampler.stop()
            except ValueError as error:
                print(f'build peak sampling: {error}', file=sys.stderr)
                result = 1
    return result


if __name__ == '__main__':
    raise SystemExit(main())
