#!/usr/bin/env python3
"""OCI/content mutations with independently computed bytes and SHA-256 values."""
from __future__ import annotations

import copy
import gzip
import hashlib
import importlib.util
import io
import json
import subprocess
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'script/release'))
spec = importlib.util.spec_from_file_location('content', ROOT / 'script/release/content.py')
content = importlib.util.module_from_spec(spec)
sys.modules['content'] = content
spec.loader.exec_module(content)


def encoded(value):
    return json.dumps(value, separators=(',', ':')).encode()


def digest(data):
    return 'sha256:' + hashlib.sha256(data).hexdigest()


def blob(layout, data, media):
    (layout / 'blobs/sha256').mkdir(parents=True, exist_ok=True)
    (layout / 'blobs/sha256' / digest(data)[7:]).write_bytes(data)
    return {'mediaType': media, 'size': len(data), 'digest': digest(data)}


def fixture(layout, arch='arm64', repeated=False, root_platform=True):
    layout.mkdir()
    # The repeated gzip is an actual empty tar stream, not a duplicate-file tar.
    raw_layers = [b'first rootfs stream', b'\0' * 1024]
    if repeated:
        raw_layers += [b'\0' * 1024, b'\0' * 1024]
    layers = [blob(layout, gzip.compress(x, mtime=0), 'application/vnd.oci.image.layer.v1.tar+gzip')
              for x in raw_layers]
    config = {'os': 'linux', 'architecture': arch,
              'rootfs': {'type': 'layers', 'diff_ids': [digest(x) for x in raw_layers]},
              'config': {'User': '12345:23456', 'Env': ['HOME=/tmp']}}
    config_desc = blob(layout, encoded(config), content.CONFIG)
    manifest = {'schemaVersion': 2, 'mediaType': content.MANIFEST,
                'config': config_desc, 'layers': layers}
    root = blob(layout, encoded(manifest), content.MANIFEST)
    root['annotations'] = {'org.opencontainers.image.ref.name': 'standard'}
    if root_platform:
        root['platform'] = {'os': 'linux', 'architecture': arch}
    (layout / 'index.json').write_bytes(encoded({'schemaVersion': 2, 'mediaType': content.INDEX, 'manifests': [root]}))
    (layout / 'oci-layout').write_bytes(encoded({'imageLayoutVersion': '1.0.0'}))
    return config, manifest, root


def tar_layout(layout, output, extra=None):
    with tarfile.open(output, 'w', format=tarfile.USTAR_FORMAT) as archive:
        for path in sorted(layout.rglob('*')):
            if path.is_file():
                data = path.read_bytes()
                header = tarfile.TarInfo(path.relative_to(layout).as_posix())
                header.size = len(data)
                archive.addfile(header, io.BytesIO(data))
        if extra:
            header, data = extra
            archive.addfile(header, io.BytesIO(data))


class ContentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.layout = self.root / 'layout'
        self.config, self.manifest, self.descriptor = fixture(self.layout)
        self.graph = content.inventory_layout(self.layout, 'arm64')
        self.archive = self.root / 'image.tar'
        tar_layout(self.layout, self.archive)

    def rewrite_manifest(self, manifest):
        root = blob(self.layout, encoded(manifest), content.MANIFEST)
        root.update({'platform': {'os': 'linux', 'architecture': 'arm64'},
                     'annotations': {'org.opencontainers.image.ref.name': 'standard'}})
        index = {'schemaVersion': 2, 'mediaType': content.INDEX, 'manifests': [root]}
        (self.layout / 'index.json').write_bytes(encoded(index))
        # Retain only referenced blobs to isolate this mutation's actual oracle.
        allowed = {root['digest'], manifest['config']['digest']} | {x['digest'] for x in manifest['layers']}
        for path in (self.layout / 'blobs/sha256').iterdir():
            if 'sha256:' + path.name not in allowed:
                path.unlink()

    def test_nested_wrong_types_are_classified(self):
        for field, value in (('rootfs', []), ('rootfs', None)):
            with self.subTest(field=field, value=value):
                config = copy.deepcopy(self.config)
                config[field] = value
                manifest = copy.deepcopy(self.manifest)
                manifest['config'] = blob(self.layout, encoded(config), content.CONFIG)
                self.rewrite_manifest(manifest)
                with self.assertRaises(ValueError):
                    content.inventory_layout(self.layout, 'arm64')
        index = content.read_json(self.layout / 'index.json')
        index['manifests'][0]['annotations'] = []
        (self.layout / 'index.json').write_bytes(encoded(index))
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64', require_platform=True)

    def test_exact_roundtrip_and_wrapper_identity(self):
        actual = content.verify_archive(self.archive, self.graph)
        content.compare_platforms(self.graph, actual)
        self.assertEqual(self.graph.manifest.digest.value, digest(encoded(self.manifest)))
        self.assertNotEqual(digest(self.archive.read_bytes()), self.graph.manifest.digest.value)
        self.assertNotEqual(digest((self.layout / 'index.json').read_bytes()), self.graph.manifest.digest.value)

    def test_repeated_empty_layers_are_ordered_not_a_set(self):
        layout = self.root / 'repeated'
        fixture(layout, repeated=True)
        graph = content.inventory_layout(layout, 'arm64')
        self.assertEqual(len(graph.layers), 4)
        self.assertEqual(graph.layers[1], graph.layers[2])
        self.assertEqual(graph.diff_ids[1], graph.diff_ids[3])
        archive = self.root / 'repeated.tar'
        tar_layout(layout, archive)
        content.verify_archive(archive, graph)
        mutated = graph.json()
        mutated['diff_ids'][2] = self.graph.diff_ids[0].value
        with self.assertRaises(ValueError):
            content.compare_platforms(graph, content.ImageGraph.parse(mutated))

    def test_registry_root_missing_platform_allowed_archive_requires_it(self):
        layout = self.root / 'registry'
        fixture(layout, root_platform=False)
        actual = content.inventory_layout(layout, 'arm64')
        content.compare_platforms(self.graph, actual)
        archive = self.root / 'missing-platform.tar'
        tar_layout(layout, archive)
        with self.assertRaises(ValueError):
            content.verify_archive(archive, self.graph)
        content.normalize_root(layout, actual)
        tar_layout(layout, archive)
        content.verify_archive(archive, self.graph)

    def test_flip_every_blob(self):
        for path in (self.layout / 'blobs/sha256').iterdir():
            with self.subTest(blob=path.name):
                original = path.read_bytes()
                path.write_bytes(bytes([original[0] ^ 1]) + original[1:])
                with self.assertRaises(ValueError):
                    content.inventory_layout(self.layout, 'arm64')
                path.write_bytes(original)

    def test_manifest_descriptor_size_media_order_diffid_mutations(self):
        for mutation in ('size', 'media', 'order', 'diff_id'):
            with self.subTest(mutation=mutation):
                manifest = copy.deepcopy(self.manifest)
                if mutation == 'size':
                    manifest['layers'][0]['size'] += 1
                elif mutation == 'media':
                    manifest['layers'][0]['mediaType'] = 'application/zstd'
                elif mutation == 'order':
                    manifest['layers'].reverse()
                else:
                    config = copy.deepcopy(self.config)
                    config['rootfs']['diff_ids'][0] = digest(b'wrong')
                    manifest['config'] = blob(self.layout, encoded(config), content.CONFIG)
                self.rewrite_manifest(manifest)
                with self.assertRaises(ValueError):
                    content.inventory_layout(self.layout, 'arm64')
                # Restore the generated baseline independently.
                import shutil
                shutil.rmtree(self.layout)
                self.config, self.manifest, self.descriptor = fixture(self.layout)

    def test_recompressed_same_rootfs_fails_strict_identity(self):
        manifest = copy.deepcopy(self.manifest)
        manifest['layers'][0] = blob(self.layout, gzip.compress(b'first rootfs stream', mtime=1),
                                     'application/vnd.oci.image.layer.v1.tar+gzip')
        self.rewrite_manifest(manifest)
        actual = content.inventory_layout(self.layout, 'arm64')
        self.assertEqual(self.graph.diff_ids, actual.diff_ids)
        with self.assertRaises(ValueError):
            content.compare_platforms(self.graph, actual)

    def test_architecture_mismatch_both_directions(self):
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'amd64')
        layout = self.root / 'amd64'
        fixture(layout, arch='amd64')
        with self.assertRaises(ValueError):
            content.inventory_layout(layout, 'arm64')

    def test_duplicate_or_extra_runnable_root(self):
        index = content.read_json(self.layout / 'index.json')
        index['manifests'].append(copy.deepcopy(index['manifests'][0]))
        (self.layout / 'index.json').write_bytes(encoded(index))
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64')

    def test_missing_blob_unreferenced_blob_and_incomplete_ingest(self):
        path = self.graph.layers[0].blob(self.layout)
        saved = path.read_bytes()
        path.unlink()
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64')
        path.write_bytes(saved)
        extra = self.layout / 'blobs/sha256' / ('f' * 64)
        extra.write_bytes(b'unreferenced')
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64')
        extra.unlink()
        (self.layout / 'ingest').mkdir()
        content.inventory_layout(self.layout, 'arm64')
        (self.layout / 'ingest/pending').write_bytes(b'incomplete')
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64')

    def test_unsafe_archive_members(self):
        for name, kind in (('../escape', tarfile.REGTYPE), ('/absolute', tarfile.REGTYPE),
                           ('blobs/sha256/link', tarfile.SYMTYPE),
                           ('index.json', tarfile.REGTYPE), ('manifest.json', tarfile.REGTYPE),
                           ('._oci-layout', tarfile.REGTYPE)):
            with self.subTest(name=name):
                header = tarfile.TarInfo(name)
                header.type = kind
                header.size = 1 if kind == tarfile.REGTYPE else 0
                header.linkname = '/escape'
                tar_layout(self.layout, self.archive, (header, b'x'))
                with self.assertRaises(ValueError):
                    content.verify_archive(self.archive, self.graph)
        self.assertFalse((self.root / 'escape').exists())

    def test_truncated_tar(self):
        data = self.archive.read_bytes()
        self.archive.write_bytes(data[:100])
        with self.assertRaises((ValueError, tarfile.TarError)):
            content.verify_archive(self.archive, self.graph)

    def test_invalid_version_and_digest_values(self):
        for value in ('latest', 'v0.1.0', '01.1.0', '0.1.0\n', '-1.0.0', '../x',
                      '0.1.0;echo pwned', '0.1.0-rc.1', '0.1.0+build', ' 0.1.0'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                content.parse_version(value)
        for value in ('sha512:' + '0' * 64, 'sha256:' + 'A' * 64, 'sha256:../x'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                content.parse_sha256(value)

    def test_cli_failure_preserves_validated_output(self):
        output = self.root / 'graph.json'
        output.write_text('prior validated output')
        proc = subprocess.run([sys.executable, str(ROOT / 'script/release/content.py'), 'inventory',
                               '--layout', str(self.layout), '--arch', 'wrong', '--output', str(output)],
                              capture_output=True)
        self.assertEqual(proc.returncode, 1)
        self.assertEqual(output.read_text(), 'prior validated output')

    def test_duplicate_json_keys(self):
        (self.layout / 'index.json').write_text('{"schemaVersion":2,"schemaVersion":1}')
        with self.assertRaises(ValueError):
            content.inventory_layout(self.layout, 'arm64')

    def inventory_for(self, arch, *, run='1', attempt='1', source='a' * 40, archive_size=1024,
                      parts=()):
        layout = self.root / ('layout-' + arch + '-' + str(len(list(self.root.glob('layout-*')))))
        fixture(layout, arch=arch)
        graph = content.inventory_layout(layout, arch)
        basename = f'agent-vm-standard-v0.1.0-linux-{arch}'
        return {'schema_version': 1, 'product': 'standard', 'version': '0.1.0',
                'source_sha': source, 'source_url': content.SOURCE, 'build_run_id': run,
                'build_run_attempt': attempt,
                'invocation_url': content.SOURCE + f'/actions/runs/{run}/attempts/{attempt}',
                'graph': graph.json(),
                'archive': {'name': basename + '.oci.tar', 'sha256': 'sha256:' + 'b' * 64,
                            'size': archive_size},
                'parts': [{'name': basename + f'.oci.tar.part-{i:03d}',
                           'sha256': 'sha256:' + f'{i:064x}', 'size': size}
                          for i, size in enumerate(parts)],
                'sbom': {'name': basename + '.spdx.json', 'sha256': 'sha256:' + 'c' * 64, 'size': 10},
                'selections': {name: '1' for name in sorted(content.SELECTIONS)},
                'docker_reported_id': 'sha256:' + 'd' * 64, 'docker_driver': '[]', 'docker_version': '1'}

    def release_metadata(self, platforms):
        first = platforms[0]
        return {'schema_version': 1, 'product': 'standard', 'version': '0.1.0',
                'source_sha': first['source_sha'], 'source_url': content.SOURCE,
                'release_tag': 'v0.1.0', 'build_run_id': first['build_run_id'],
                'build_run_attempt': first['build_run_attempt'],
                'invocation_url': first['invocation_url'], 'index_digest': 'sha256:' + 'e' * 64,
                'platforms': platforms}

    def test_archive_part_boundaries(self):
        limit = 2_000_000_000
        self.assertIsNotNone(content.PlatformInventory.parse(
            self.inventory_for('arm64', archive_size=limit - 1)))
        with self.assertRaises(ValueError):
            content.PlatformInventory.parse(self.inventory_for('arm64', archive_size=limit))
        self.assertIsNotNone(content.PlatformInventory.parse(
            self.inventory_for('arm64', archive_size=limit, parts=[10 ** 9, 10 ** 9])))
        # A final short part is valid framing; a wrong sum/order/oversized is not.
        self.assertIsNotNone(content.PlatformInventory.parse(
            self.inventory_for('arm64', archive_size=limit + 1, parts=[10 ** 9, 10 ** 9, 1])))
        bad = ((limit + 1, [10 ** 9, 10 ** 9, 2]), (limit, [10 ** 9, 5 * 10 ** 8, 5 * 10 ** 8]),
               (limit, [10 ** 9 + 1, 10 ** 9 - 1]), (limit + 1, [10 ** 9, 1, 10 ** 9]),
               (limit - 1, [limit - 1]))
        for size, parts in bad:
            with self.subTest(parts=parts), self.assertRaises(ValueError):
                content.PlatformInventory.parse(self.inventory_for('arm64', archive_size=size, parts=parts))

    def test_assemble_release_cli_rejects_foreign_run_and_missing_arch(self):
        arm, amd = self.inventory_for('arm64'), self.inventory_for('amd64')
        index = self.root / 'index.json'
        manifests = [{'mediaType': content.MANIFEST, 'size': content.ImageGraph.parse(arm['graph']).manifest.size,
                      'digest': content.ImageGraph.parse(arm['graph']).manifest.digest.value,
                      'platform': {'os': 'linux', 'architecture': 'arm64'}},
                     {'mediaType': content.MANIFEST, 'size': content.ImageGraph.parse(amd['graph']).manifest.size,
                      'digest': content.ImageGraph.parse(amd['graph']).manifest.digest.value,
                      'platform': {'os': 'linux', 'architecture': 'amd64'}}]
        index.write_bytes(encoded({'schemaVersion': 2, 'mediaType': content.INDEX, 'manifests': manifests}))
        paths = []
        for name, inventory in (('arm.json', arm), ('amd.json', amd)):
            path = self.root / name
            path.write_bytes(encoded(inventory))
            paths.append(path)
        output = self.root / 'release.json'

        def invoke():
            return subprocess.run([sys.executable, str(ROOT / 'script/release/content.py'), 'assemble-release',
                '--version', '0.1.0', '--source-sha', 'a' * 40, '--run-id', '1', '--run-attempt', '1',
                '--index-raw', str(index), '--platform-json', str(paths[0]),
                '--platform-json', str(paths[1]), '--output', str(output)], capture_output=True)

        self.assertEqual(invoke().returncode, 0)
        output.write_text('prior validated output')
        foreign = self.root / 'foreign.json'
        foreign.write_bytes(encoded(self.inventory_for('amd64', attempt='2')))
        paths[1] = foreign
        result = invoke()
        self.assertEqual(result.returncode, 1)
        self.assertEqual(output.read_text(), 'prior validated output')
        output.unlink()
        with self.assertRaises(ValueError):
            content.ReleaseMetadata.parse(self.release_metadata([arm, self.inventory_for('amd64', attempt='2')]))
        with self.assertRaises(ValueError):
            content.ReleaseMetadata.parse(self.release_metadata([amd, self.inventory_for('amd64')]))
        wrong = self.release_metadata([arm, amd])
        wrong['product'] = 'base'
        with self.assertRaises(ValueError):
            content.ReleaseMetadata.parse(wrong)


if __name__ == '__main__':
    unittest.main()
