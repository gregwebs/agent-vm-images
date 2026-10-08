#!/usr/bin/env python3
"""Deterministic authority and allocation controls, without Docker or network."""
from __future__ import annotations

import gzip
import shutil
import tarfile
from dataclasses import replace
import hashlib
import importlib.util
import json
import os
import platform
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'script/release'))


def module(name: str):
    spec = importlib.util.spec_from_file_location(name, ROOT / 'script/release' / (name + '.py'))
    loaded = importlib.util.module_from_spec(spec)
    sys.modules[name] = loaded
    spec.loader.exec_module(loaded)
    return loaded


content = module('content')
preflight = module('preflight')
capacity = module('capacity')
attestations = module('attestations')
operations = module('operations')
build = module('build')
verify = module('verify')


def reply(status: int, body: object, headers: dict | None = None):
    return preflight.Response(status, headers or {}, json.dumps(body).encode())


class FakeClient:
    def __init__(self, responses):
        self.responses = responses
        self.requests = []

    def get(self, url, **kwargs):
        self.requests.append((url, kwargs))
        if not self.responses:
            raise AssertionError('unexpected HTTP request')
        return self.responses.pop(0)


class AuthorityTests(unittest.TestCase):
    def test_manifest_outcomes(self):
        cases = ((401, 'UNAUTHORIZED', 'UNAUTHORIZED'),
                 (403, 'DENIED', 'UNAUTHORIZED'),
                 (404, 'MANIFEST_UNKNOWN', 'REF_ABSENT'),
                 (404, 'NAME_UNKNOWN', 'UNKNOWN'),
                 (503, 'UNAVAILABLE', 'TRANSIENT_FAILURE'),
                 (429, 'TOOMANYREQUESTS', 'TRANSIENT_FAILURE'))
        for status, code, outcome in cases:
            with self.subTest(status=status, code=code):
                response = reply(status, {'errors': [{'code': code}]})
                self.assertEqual(preflight.classify_manifest(response, package_exists=True).value, outcome)
        self.assertEqual(preflight.classify_manifest(
            reply(404, {'errors': [{'code': 'MANIFEST_UNKNOWN'}]}), package_exists=False).value, 'UNKNOWN')

    def test_private_occupied_ref(self):
        body = {'schemaVersion': 2, 'mediaType': 'application/vnd.oci.image.manifest.v1+json'}
        response = reply(200, body)
        response = preflight.Response(200, {'docker-content-digest': 'sha256:' +
                                           hashlib.sha256(response.body).hexdigest()}, response.body)
        self.assertEqual(preflight.classify_manifest(response, package_exists=True).value, 'REF_PRESENT')
        bad = preflight.Response(200, {'docker-content-digest': 'sha256:' + '0' * 64}, response.body)
        self.assertEqual(preflight.classify_manifest(bad, package_exists=True).value, 'UNKNOWN')

    def test_malformed_registry_reply(self):
        for body in ({}, {'errors': []}, {'errors': [{'message': 'missing code'}]}):
            with self.subTest(body=body), self.assertRaises(ValueError):
                preflight.classify_manifest(reply(404, body), package_exists=True)

    def inventory_client(self, records, *, status=200, scopes='read:packages', login='gregwebs'):
        return FakeClient([reply(status, {'login': login}, {'x-oauth-scopes': scopes})] + records)

    def test_owner_authorized_absence(self):
        client = self.inventory_client([reply(200, []), reply(404, {'message': 'Package not found.'})])
        exists, digest = preflight.owner_inventory(client, 'fixture-read-token')
        self.assertFalse(exists)
        self.assertEqual(len(digest), 64)
        self.assertEqual(len(client.requests), 3)
        self.assertTrue(all(x[1]['authorization'] == 'Bearer fixture-read-token' for x in client.requests))

    def test_absence_requires_the_authoritative_package_message(self):
        # GitHub's container-package endpoint returns "Package not found."; a
        # 404 carrying the releases/git-ref wording (or any other message) is
        # not authoritative package absence and must fail closed.
        for message in ('Not Found', 'Package not found', 'forbidden', ''):
            with self.subTest(message=message), self.assertRaises(ValueError):
                preflight.owner_inventory(
                    self.inventory_client([reply(200, []), reply(404, {'message': message})]),
                    'fixture-read-token')

    def test_package_only_on_second_page(self):
        record = {'name': 'agent-vm-standard', 'package_type': 'container', 'visibility': 'private'}
        other = {'name': 'unrelated-private-package', 'package_type': 'container', 'visibility': 'private'}
        link = '<https://api.github.com/users/gregwebs/packages?package_type=container&page=2>; rel="next"'
        client = self.inventory_client([reply(200, [other], {'link': link}),
                                        reply(200, [record]), reply(200, record)])
        exists, digest = preflight.owner_inventory(client, 'fixture-read-token')
        self.assertTrue(exists)
        redacted = {'target': 'agent-vm-standard', 'present': True, 'visibility': 'private'}
        self.assertEqual(digest, hashlib.sha256(json.dumps(redacted, sort_keys=True).encode()).hexdigest())
        self.assertEqual(len(client.requests), 4)

    def test_denied_wrong_owner_or_excess_permissions(self):
        for kwargs in ({'status': 403}, {'login': 'other'}, {'scopes': ''},
                       {'scopes': 'read:packages, write:packages'}, {'scopes': 'repo, read:packages'}):
            with self.subTest(kwargs=kwargs), self.assertRaises(ValueError):
                preflight.owner_inventory(self.inventory_client([], **kwargs), 'fixture-read-token')

    def test_concealed_existing_package_metadata_fails(self):
        record = {'name': 'agent-vm-standard', 'package_type': 'container', 'visibility': 'private'}
        client = self.inventory_client([reply(200, [record]), reply(404, {'message': 'Package not found.'})])
        with self.assertRaises(ValueError):
            preflight.owner_inventory(client, 'fixture-read-token')

    def test_pagination_cannot_exfiltrate_credentials(self):
        for url in ('http://api.github.com/users/gregwebs/packages?page=2',
                    'https://attacker.test/users/gregwebs/packages?page=2',
                    'https://api.github.com/user?page=2'):
            with self.subTest(url=url), self.assertRaises(ValueError):
                preflight.next_page(f'<{url}>; rel="next"')

    def test_first_package_denied_bootstrap_only_after_owner_absence(self):
        denied = reply(403, {'errors': [{'code': 'DENIED'}]})
        client = FakeClient([denied])
        results = preflight.check_refs(client, workflow_token='fixture-workflow-token', actor='fixture',
                                       version='0.1.0', package_exists=False)
        self.assertEqual({x.value for x in results.values()}, {'PACKAGE_NOT_CREATED'})
        with self.assertRaises(ValueError):
            preflight.check_refs(FakeClient([denied]), workflow_token='fixture-workflow-token',
                                 actor='fixture', version='0.1.0', package_exists=True)
        with self.assertRaises(ValueError):
            preflight.check_refs(FakeClient([reply(401, {'errors': [{'code': 'UNAUTHORIZED'}]})]),
                                 workflow_token='fixture-workflow-token', actor='fixture',
                                 version='0.1.0', package_exists=False)
    def test_first_package_manifest_unknown_bootstraps_only_without_package(self):
        def refs(code, *, package_exists):
            # check_refs does one bearer-token exchange then one manifest GET
            # per version ref.
            client = FakeClient([reply(200, {'token': 'fixture-bearer'})] +
                                [reply(404, {'errors': [{'code': code}]})] * 3)
            return preflight.check_refs(client, workflow_token='fixture-workflow-token',
                                        actor='fixture', version='0.1.0',
                                        package_exists=package_exists)
        self.assertEqual({x.value for x in refs('MANIFEST_UNKNOWN', package_exists=False).values()},
                         {'PACKAGE_NOT_CREATED'})
        self.assertEqual({x.value for x in refs('MANIFEST_UNKNOWN', package_exists=True).values()},
                         {'REF_ABSENT'})
        # NAME_UNKNOWN is registry concealment, never bootstrap authority.
        self.assertEqual({x.value for x in refs('NAME_UNKNOWN', package_exists=False).values()},
                         {'UNKNOWN'})


class DraftAuthorityTests(unittest.TestCase):
    def client(self, records=None, *, page2=None, published=None, tag=None, status=200):
        responses = [reply(200, {'full_name': preflight.REPO})]
        headers = {} if page2 is None else {'link':
            '<https://api.github.com/repos/gregwebs/agent-vm-images/releases?per_page=100&page=2>; rel="next"'}
        responses.append(reply(status, records or [], headers))
        if page2 is not None:
            responses.append(reply(200, page2))
        responses += [reply(404, {'message': 'Not Found'}) if published is None else reply(200, published),
                      reply(404, {'message': 'Not Found'}) if tag is None else reply(200, tag)]
        return FakeClient(responses)

    def check(self, client):
        return preflight.release_tag_absence(client, token='fixture-workflow-token',
                                            version='0.1.0').value

    def test_complete_absence_is_get_only(self):
        client = self.client()
        self.assertEqual(self.check(client), 'ABSENT')
        self.assertEqual(len(client.requests), 4)
        self.assertIn('/releases?per_page=100&page=1', client.requests[1][0])

    def test_draft_without_tag_or_platform_refs(self):
        draft = {'id': 1, 'tag_name': 'v0.1.0', 'draft': True}
        self.assertEqual(self.check(self.client([draft])), 'OCCUPIED_BY_DRAFT')
        self.assertEqual(self.check(self.client(page2=[draft])), 'OCCUPIED_BY_DRAFT')

    def test_published_and_tag_only_collisions(self):
        release = {'id': 2, 'tag_name': 'v0.1.0', 'draft': False}
        self.assertEqual(self.check(self.client([release], published=release)), 'OCCUPIED_BY_PUBLISHED')
        tag = {'ref': 'refs/tags/v0.1.0', 'object': {'type': 'tag', 'sha': 'a' * 40}}
        self.assertEqual(self.check(self.client(tag=tag)), 'OCCUPIED_BY_TAG')
        self.assertEqual(self.check(self.client(published=release)), 'UNAUTHORIZED_OR_CONCEALED')
        self.assertEqual(self.check(self.client([release])), 'UNAUTHORIZED_OR_CONCEALED')

    def test_denied_concealed_malformed_incomplete_pages(self):
        for status in (401, 403, 404):
            self.assertEqual(self.check(self.client(status=status)), 'UNAUTHORIZED_OR_CONCEALED')
        client = self.client(page2=[{'tag_name': 'v0.1.0'}])
        self.assertEqual(self.check(client), 'UNAUTHORIZED_OR_CONCEALED')
        client = self.client()
        client.responses[1] = reply(200, [], {'link': '<https://evil.test/>; rel="next"'})
        self.assertEqual(self.check(client), 'UNAUTHORIZED_OR_CONCEALED')

    def test_intervening_draft_is_freshly_rechecked(self):
        self.assertEqual(self.check(self.client()), 'ABSENT')
        draft = {'id': 3, 'tag_name': 'v0.1.0', 'draft': True}
        self.assertEqual(self.check(self.client([draft])), 'OCCUPIED_BY_DRAFT')


class CapacityTests(unittest.TestCase):
    def fs(self, host='native', filesystem='disk', free=100 * capacity.GIB):
        return capacity.Filesystem(host, filesystem, '/owned', free)

    def check(self, scratch, daemon, phase='build', **kwargs):
        return capacity.check(phase, scratch, daemon, uncompressed=8 * capacity.GIB,
                              compressed=4 * capacity.GIB, build=25 * capacity.GIB, **kwargs)

    def test_shared_filesystem_sums_allocations_and_one_reserve(self):
        result = self.check(self.fs(), (self.fs(), self.fs()))
        self.assertEqual(len(result['filesystems']), 1)
        self.assertEqual(result['filesystems'][0]['required_bytes'], 38 * capacity.GIB)
        with self.assertRaises(ValueError):
            self.check(self.fs(free=37 * capacity.GIB), (self.fs(free=37 * capacity.GIB),))

    def test_device_numbers_from_distinct_hosts_not_equal(self):
        result = self.check(self.fs(host='client'), (self.fs(host='daemon'),))
        self.assertEqual(len(result['filesystems']), 2)

    def test_boundary_and_remaining_allocation(self):
        result = self.check(self.fs(free=13 * capacity.GIB), (self.fs(host='daemon', free=30 * capacity.GIB),))
        self.assertEqual(len(result['filesystems']), 2)
        with self.assertRaises(ValueError):
            self.check(self.fs(free=13 * capacity.GIB - 1), (self.fs(host='daemon'),))
        result = self.check(self.fs(), (self.fs(),), phase='stage', tar_bytes=4 * capacity.GIB + 10240)
        self.assertEqual(result['filesystems'][0]['required_bytes'], 9 * capacity.GIB + 10240)

    def test_tar_above_graph_cap_and_framing_limit(self):
        self.check(self.fs(), (self.fs(),), phase='load', tar_bytes=4 * capacity.GIB + 10240)
        with self.assertRaises(ValueError):
            self.check(self.fs(), (self.fs(),), phase='load',
                       tar_bytes=4 * capacity.GIB + capacity.TAR_OVERHEAD + 1)

    def test_unknown_daemon_measurements_and_invalid_free_fail(self):
        with self.assertRaises(ValueError):
            self.check(self.fs(), ())
        for value in (-1, True, '100'):
            with self.subTest(value=value), self.assertRaises(ValueError):
                capacity.Filesystem.parse({'host': 'native', 'filesystem': 'disk',
                                           'path': '/owned', 'free_bytes': value})


DOCKER_FAKE = """#!/usr/bin/env python3
import os, sys
with open(os.environ['LOG'], 'a') as f:
    f.write('docker\\t' + '\\t'.join(sys.argv[1:]) + '\\n')
sys.exit(0)
"""

SKOPEO_FAKE = """#!/usr/bin/env python3
import os, sys
argv = sys.argv[1:]
with open(os.environ['LOG'], 'a') as f:
    f.write('skopeo\\t' + '\\t'.join(argv) + '\\n')
if 'inspect' in argv and '--raw' in argv:
    sys.stdout.buffer.write(open(os.environ['FAKE_MANIFEST'], 'rb').read())
    sys.exit(0)
if 'copy' in argv:
    sys.exit(0)
sys.exit(99)
"""

CURL_FAKE = """#!/usr/bin/env python3
import json, os, sys
argv = sys.argv[1:]
with open(os.environ['LOG'], 'a') as f:
    f.write('curl\\t' + '\\t'.join(argv) + '\\n')
mapping = json.load(open(os.environ['FAKE_CURL_MAP']))

def value(flag):
    return argv[argv.index(flag) + 1] if flag in argv else None

def emit(url, output, header, configured):
    entry = mapping.get(url)
    if entry is None:
        sys.exit(3)
    if header:
        with open(header, 'w') as f:
            f.write('HTTP/1.1 %d\\r\\n' % entry['status'])
            for name, text in entry.get('headers', {}).items():
                f.write('%s: %s\\r\\n' % (name, text))
            f.write('\\r\\n')
    if output:
        with open(output, 'wb') as f:
            f.write(entry.get('body', '').encode())
    if not output:
        sys.stdout.write(entry.get('body', ''))
    if configured:
        sys.exit(0)
    sys.exit(22 if entry['status'] >= 400 else 0)

if '--config' in argv:
    url = None
    for line in open(value('--config')):
        line = line.strip()
        if line.startswith('url = '):
            url = line[len('url = '):].strip().strip('"')
    emit(url, value('--output'), value('--dump-header'), True)
else:
    emit(argv[-1], value('--output'), None, False)
"""


def make_layout(layout: Path, arch: str = 'arm64'):
    layout.mkdir()
    (layout / 'blobs/sha256').mkdir(parents=True)

    def blob(data: bytes, media: str) -> dict:
        digest = 'sha256:' + hashlib.sha256(data).hexdigest()
        (layout / 'blobs/sha256' / digest[7:]).write_bytes(data)
        return {'mediaType': media, 'size': len(data), 'digest': digest}

    raw = [b'rootfs-one']
    layers = [blob(gzip.compress(x, mtime=0),
                   'application/vnd.oci.image.layer.v1.tar+gzip') for x in raw]
    config = {'os': 'linux', 'architecture': arch,
              'rootfs': {'type': 'layers',
                         'diff_ids': ['sha256:' + hashlib.sha256(x).hexdigest() for x in raw]},
              'config': {'User': '12345:23456', 'Env': ['HOME=/tmp']}}
    config_desc = blob(json.dumps(config, separators=(',', ':')).encode(), content.CONFIG)
    manifest = {'schemaVersion': 2, 'mediaType': content.MANIFEST, 'config': config_desc, 'layers': layers}
    root = blob(json.dumps(manifest, separators=(',', ':')).encode(), content.MANIFEST)
    root['platform'] = {'os': 'linux', 'architecture': arch}
    root['annotations'] = {'org.opencontainers.image.ref.name': 'standard'}
    (layout / 'index.json').write_text(json.dumps(
        {'schemaVersion': 2, 'mediaType': content.INDEX, 'manifests': [root]}, separators=(',', ':')))
    (layout / 'oci-layout').write_text('{"imageLayoutVersion":"1.0.0"}')
    return content.inventory_layout(layout, arch)


def inventory_json(graph, arch: str, run: str = '264', attempt: str = '1') -> dict:
    version = '0.1.0'
    basename = f'agent-vm-standard-v{version}-linux-{arch}'
    return {'schema_version': 1, 'product': 'standard', 'version': version,
            'source_sha': 'a' * 40, 'source_url': content.SOURCE,
            'build_run_id': run, 'build_run_attempt': attempt,
            'invocation_url': f'{content.SOURCE}/actions/runs/{run}/attempts/{attempt}',
            'graph': graph.json(),
            'archive': {'name': basename + '.oci.tar', 'sha256': 'sha256:' + 'b' * 64, 'size': 1024},
            'parts': [],
            'sbom': {'name': basename + '.spdx.json', 'sha256': 'sha256:' + 'c' * 64, 'size': 1024},
            'selections': {name: '1.0.0' for name in sorted(content.SELECTIONS)},
            'docker_reported_id': 'sha256:' + 'd' * 64, 'docker_driver': '[]', 'docker_version': '29.5.2'}


class FakeTools:
    def __init__(self, root: Path):
        self.root = root
        self.bin = root / 'bin'
        self.bin.mkdir()
        self.log = root / 'calls.log'
        self.log.write_text('')
        self.responses = root / 'responses.json'
        for name, body in (('docker', DOCKER_FAKE), ('skopeo', SKOPEO_FAKE), ('curl', CURL_FAKE)):
            path = self.bin / name
            for key, value in {'LOG': self.log, 'FAKE_CURL_MAP': self.responses}.items():
                body = body.replace("os.environ['" + key + "']", repr(str(value)))
            path.write_text(body)
            path.chmod(0o755)
        self.environ = {'PATH': str(self.bin) + os.pathsep + os.environ['PATH'], 'LOG': str(self.log),
                        'FAKE_CURL_MAP': str(self.responses),
                        'FAKE_MANIFEST': str(root / 'manifest.json')}

    def write_responses(self, mapping: dict) -> None:
        self.responses.write_text(json.dumps(mapping))

    def calls(self) -> list[list[str]]:
        return [line.split('\t') for line in self.log.read_text().splitlines() if line]


class SubprocessOperationTests(unittest.TestCase):
    """External tools are fake executables that log argv; no build is permitted."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.tools = FakeTools(self.root)
        self.layout = self.root / 'layout'
        self.graph = make_layout(self.layout, 'arm64')
        manifest = self.graph.manifest.blob(self.layout)
        (self.root / 'manifest.json').write_bytes(manifest.read_bytes())
        self.metadata = self.root / 'platform.json'
        self.metadata.write_text(json.dumps(inventory_json(self.graph, 'arm64')))
        self.authdir = self.root / 'docker-config'
        self.authdir.mkdir()
        (self.authdir / 'config.json').write_text('{"auths":{}}')
        self.env = dict(os.environ, **self.tools.environ, **{
            'GITHUB_REPOSITORY': preflight.REPO, 'GITHUB_REF': 'refs/heads/main',
            'GITHUB_SHA': 'a' * 40, 'GITHUB_RUN_ID': '264', 'GITHUB_RUN_ATTEMPT': '1',
            'GITHUB_ACTOR': 'fixture', 'GITHUB_TOKEN': 'fixture-token',
            'GHCR_PACKAGE_INVENTORY_TOKEN': 'fixture-inventory', 'DOCKER_CONFIG': str(self.authdir)})

    def absent(self) -> dict:
        return {'status': 404, 'headers': {},
                'body': json.dumps({'errors': [{'code': 'MANIFEST_UNKNOWN'}]})}

    def responses(self) -> dict:
        return {
            'https://api.github.com/repos/' + preflight.REPO: {'status': 200, 'body': json.dumps({'full_name': preflight.REPO})},
            'https://api.github.com/user': {'status': 200, 'headers': {'x-oauth-scopes': 'read:packages'},
                                            'body': json.dumps({'login': 'gregwebs'})},
            'https://api.github.com/users/gregwebs/packages?package_type=container&per_page=100':
                {'status': 200, 'headers': {}, 'body': '[]'},
            'https://api.github.com/users/gregwebs/packages/container/agent-vm-standard':
                {'status': 404, 'headers': {}, 'body': json.dumps({'message': 'Package not found.'})},
            'https://ghcr.io/token?service=ghcr.io&scope=repository:gregwebs/agent-vm-standard:pull,push':
                {'status': 200, 'headers': {}, 'body': json.dumps({'token': 'fixture'})},
            'https://ghcr.io/v2/gregwebs/agent-vm-standard/manifests/v0.1.0': self.absent(),
            'https://ghcr.io/v2/gregwebs/agent-vm-standard/manifests/v0.1.0-amd64': self.absent(),
            'https://ghcr.io/v2/gregwebs/agent-vm-standard/manifests/v0.1.0-arm64': self.absent()}

    def publish(self) -> subprocess.CompletedProcess:
        return subprocess.run(
            ['bash', str(ROOT / 'script/release/publish-standard.sh'), '--layout', str(self.layout),
             '--image', operations.IMAGE, '--tag', 'v0.1.0-arm64', '--arch', 'arm64',
             '--metadata', str(self.metadata)],
            env=self.env, capture_output=True, text=True)

    def test_publish_platform_preserves_digests_and_never_builds(self):
        self.tools.write_responses(self.responses())
        result = self.publish()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.tools.calls()
        self.assertFalse([c for c in calls if c[0] == 'docker'], calls)
        self.assertNotIn('build', ' '.join(sum(calls, [])))
        copies = [c for c in calls if c[0] == 'skopeo' and 'copy' in c]
        self.assertEqual(len(copies), 1)
        self.assertIn('--preserve-digests', copies[0])
        self.assertIn('docker://' + operations.IMAGE + ':v0.1.0-arm64', copies[0])
        self.assertEqual(copies[0][copies[0].index('--authfile') + 1],
                         str(Path(self.authdir, 'config.json').resolve()))
        self.assertEqual(len([c for c in calls if c[0] == 'skopeo' and 'inspect' in c and '--raw' in c]), 2)

    def test_publish_platform_refuses_occupied_ref(self):
        mapping = self.responses()
        body = (self.root / 'manifest.json').read_text()
        mapping['https://ghcr.io/v2/gregwebs/agent-vm-standard/manifests/v0.1.0'] = {
            'status': 200, 'headers': {'docker-content-digest': 'sha256:' + hashlib.sha256(body.encode()).hexdigest()},
            'body': body}
        self.tools.write_responses(mapping)
        result = self.publish()
        self.assertEqual(result.returncode, 1)
        self.assertFalse([c for c in self.tools.calls() if c[0] == 'skopeo' and 'copy' in c])

    def test_public_download_exact_url_and_missing_asset_fail(self):
        assets = self.root / 'assets'
        assets.mkdir()
        version, archive = '0.1.0', 'agent-vm-standard-v0.1.0-linux-arm64.oci.tar'
        url = content.SOURCE + '/releases/download/v' + version + '/' + archive
        self.tools.write_responses({url: {'status': 200, 'headers': {}, 'body': 'archive-bytes'}})
        with mock.patch.dict(os.environ, self.tools.environ):
            operations.public_download(archive, version=version, out=assets)
            self.assertEqual((assets / archive).read_text(), 'archive-bytes')
            self.assertEqual([c[-1] for c in self.tools.calls() if c[0] == 'curl'][-1], url)
            self.tools.write_responses({})
            missing = 'agent-vm-standard-v0.1.0-linux-arm64.spdx.json'
            with self.assertRaises(ValueError):
                operations.public_download(missing, version=version, out=assets)
            attempted = [c[-1] for c in self.tools.calls() if c[0] == 'curl']
            self.assertEqual(attempted[-1],
                             content.SOURCE + '/releases/download/v' + version + '/' + missing)
            self.assertEqual(len(set(attempted)), len(attempted))


class ReviewRegressionTests(unittest.TestCase):
    def test_clean_committed_entrypoint_needs_no_bytecode_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            shutil.copytree(ROOT / 'script/release', root / 'script/release', ignore=shutil.ignore_patterns('__pycache__'))
            (root / 'images/standard').mkdir(parents=True)
            (root / 'images/standard/version').write_text('0.1.0\n')
            for command in (['git', 'init', '-q'], ['git', 'add', '.'],
                ['git', '-c', 'user.name=Fixture', '-c', 'user.email=f@invalid', 'commit', '-qm', 'fixture']):
                subprocess.run(command, cwd=root, check=True, capture_output=True)
            sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=root).decode().strip()
            binary = Path(directory).parent / (root.name + '-bin'); binary.mkdir()
            docker = binary / 'docker'
            docker.write_text('#!/bin/sh\necho clean-source-gate-passed >&2\nexit 17\n')
            docker.chmod(0o755)
            env = dict(os.environ, PATH=str(binary) + ':' + os.environ['PATH'], GITHUB_REPOSITORY=preflight.REPO,
                       GITHUB_REF='refs/heads/main', GITHUB_SHA=sha, GITHUB_RUN_ID='264', GITHUB_RUN_ATTEMPT='1')
            env.pop('PYTHONDONTWRITEBYTECODE', None)
            for arguments in (['--help'], ['check-evidence', '--help'],
                ['check-evidence', '--release', str(root / 'missing.json'), '--evidence-dir', str(root)]):
                direct = subprocess.run([sys.executable, str(root / 'script/release/content.py'), *arguments],
                                        env=env, capture_output=True, text=True)
                self.assertIn(direct.returncode, (0, 1))
                self.assertNotIn('Traceback', direct.stderr)
                self.assertEqual(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root), b'')
                self.assertFalse(list(root.rglob('__pycache__')))
            result = subprocess.run(['bash', str(root / 'script/release/build-standard.sh'), '--arch',
                'arm64' if platform.machine() in ('arm64', 'aarch64') else 'amd64', '--version', '0.1.0',
                '--out', str(Path(directory).parent / (root.name + '-output')),
                '--scratch-root', str(Path(directory).parent / (root.name + '-scratch'))],
                env=env, capture_output=True, text=True)
            # The deliberate Docker failure occurs after source validation.
            self.assertNotIn('release requires clean committed sources', result.stderr)
            self.assertIn('docker', result.stderr)
            self.assertEqual(subprocess.check_output(['git', 'status', '--porcelain'], cwd=root), b'')
            self.assertFalse(list(root.rglob('__pycache__')))
            for suffix in ('-output', '-scratch', '-bin'):
                shutil.rmtree(Path(directory).parent / (root.name + suffix), ignore_errors=True)

    def test_native_invalid_job_token_blocks_before_inventory_or_bootstrap(self):
        with mock.patch.dict(os.environ, {'GITHUB_TOKEN': 'invalid'}), \
             mock.patch.object(preflight, 'validate_repository', side_effect=ValueError('UNAUTHORIZED')), \
             mock.patch.object(preflight, 'owner_inventory') as inventory:
            with self.assertRaises(ValueError):
                operations.authenticated_refs('0.1.0')
            inventory.assert_not_called()

    def release_fixture(self, root: Path):
        inventories, folders = [], []
        for arch in ('amd64', 'arm64'):
            folder = root / arch; folder.mkdir()
            graph = make_layout(folder / 'layout', arch)
            archive = folder / f'agent-vm-standard-v0.1.0-linux-{arch}.oci.tar'
            with tarfile.open(archive, 'w') as tar:
                for name in ('oci-layout', 'index.json', 'blobs'): tar.add(folder / 'layout' / name, arcname=name)
            sbom = folder / f'agent-vm-standard-v0.1.0-linux-{arch}.spdx.json'
            sbom.write_text('{"spdxVersion":"SPDX-2.3","packages":[{"name":"standard"}]}')
            inventory = replace(content.PlatformInventory.parse(inventory_json(graph, arch)),
                                archive=operations.asset(archive), sbom=operations.asset(sbom))
            content.atomic_json(folder / 'platform.json', inventory.json())
            for kind in ('files', 'image', 'sbom'): (folder / f'platform-{arch}-{kind}.sigstore.json').write_text('{}')
            inventories.append(inventory); folders.append(folder)
        release = content.ReleaseMetadata(content.parse_version('0.1.0'), 'a'*40, '264', '1',
                                          content.Sha256('sha256:'+'e'*64), tuple(inventories))
        return release, folders

    def test_initial_preflight_then_intervening_draft_blocks_assembly_cli(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); release, folders = self.release_fixture(root)
            def snapshot(draft=False):
                return FakeClient([reply(200, {'full_name': preflight.REPO}), reply(200,
                    [{'id': 264, 'tag_name': 'v0.1.0', 'draft': True}] if draft else []),
                    reply(404, {'message': 'Not Found'}), reply(404, {'message': 'Not Found'})])
            env = {'GITHUB_REPOSITORY': preflight.REPO, 'GITHUB_REF': 'refs/heads/main', 'GITHUB_SHA': 'a'*40,
                'GITHUB_RUN_ID': '264', 'GITHUB_RUN_ATTEMPT': '1', 'GITHUB_TOKEN': 'fixture',
                'GHCR_PACKAGE_INVENTORY_TOKEN': 'fixture', 'GITHUB_ACTOR': 'fixture', 'GITHUB_ACTIONS': 'true'}
            client = snapshot(); client.responses.insert(0, reply(200, {'full_name': preflight.REPO}))
            with mock.patch.dict(os.environ, env), mock.patch.object(preflight, 'Curl', return_value=client), \
                 mock.patch.object(preflight, 'owner_inventory', return_value=(True, 'sanitized-hash')), \
                 mock.patch.object(preflight, 'check_refs', return_value={'v0.1.0': preflight.Outcome.REF_ABSENT}), \
                 mock.patch.object(sys, 'argv', ['preflight', '--version', '0.1.0', '--source-sha', 'a'*40,
                    '--run-id', '264', '--run-attempt', '1', '--out', str(root / 'preflight')]):
                self.assertEqual(preflight.main(), 0)
            commands = []
            def command(argv, **kwargs):
                commands.append(argv)
                if argv[:3] != ['skopeo', 'inspect', '--raw']:
                    raise AssertionError('unexpected remote write')
                arch = argv[-1].rsplit('-', 1)[-1]
                graph = release.platform(arch).graph
                return graph.manifest.blob(root / arch / 'layout').read_bytes()
            refs = {'v0.1.0': preflight.Outcome.REF_ABSENT,
                    'v0.1.0-amd64': preflight.Outcome.REF_PRESENT, 'v0.1.0-arm64': preflight.Outcome.REF_PRESENT}
            with mock.patch.dict(os.environ, env), mock.patch.object(preflight, 'Curl', return_value=snapshot(True)), \
                 mock.patch.object(operations, 'source_sha', return_value='a'*40), \
                 mock.patch.object(operations, 'committed_version'), mock.patch.object(operations, 'authfile', return_value=root/'owned.json'), \
                 mock.patch.object(operations, 'verify_platform_handoff', side_effect=release.platforms), \
                 mock.patch.object(operations, 'authenticated_refs', return_value=refs), \
                 mock.patch.object(operations, 'run', side_effect=command), \
                 mock.patch.object(sys, 'argv', ['operations', 'prepare', '--version', '0.1.0', '--source-sha', 'a'*40,
                    '--run-id', '264', '--run-attempt', '1', '--platform-dir', str(folders[0]),
                    '--platform-dir', str(folders[1]), '--out', str(root / 'assembled')]):
                self.assertEqual(operations.main(), 1)
            self.assertEqual(len(commands), 2)
            self.assertFalse((root / 'assembled/result.json').exists())

    def test_publish_cli_no_build_and_upload_failure_stops_publication(self):
        for fail in (False, True):
            with tempfile.TemporaryDirectory() as directory:
                root = Path(directory); release, _ = self.release_fixture(root)
                content.atomic_json(root / 'release.json', release.json())
                (root / 'SHA256SUMS').write_text('fixture checksums')
                state = {'release_id': 264, 'version': '0.1.0', 'source_sha': 'a'*40,
                    'release_subject': content.hash_file(root / 'release.json')[0].value,
                    'index_digest': release.index_digest.value, 'files': ['release.json', 'SHA256SUMS']}
                content.atomic_json(root / 'state.json', state)
                calls = []
                def command(argv, **kwargs):
                    calls.append(argv)
                    if argv[:2] == ['gh', 'api']:
                        return json.dumps({'draft': True, 'tag_name': 'v0.1.0', 'target_commitish': 'a'*40, 'assets': []}).encode()
                    if fail and argv[:3] == ['gh', 'release', 'upload']: raise subprocess.CalledProcessError(17, argv)
                    return b''
                with mock.patch.object(operations, 'run', side_effect=command), \
                     mock.patch.object(operations, 'trusted_run', return_value=('264', '1', 'fixture')), mock.patch.object(attestations, 'verify'), \
                     mock.patch.object(operations, 'anonymous_registry'), \
                     mock.patch.object(sys, 'argv', ['operations', 'publish', '--state', str(root/'state.json')]):
                    self.assertEqual(operations.main(), int(fail))
                self.assertFalse(any('build' in argv for argv in calls))
                self.assertEqual(any(argv[:3] == ['gh', 'release', 'edit'] for argv in calls), not fail)

    def test_signed_handoff_inventory_archive_sbom_mutations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); layout = root / 'layout'; graph = make_layout(layout)
            archive = root / 'agent-vm-standard-v0.1.0-linux-arm64.oci.tar'
            with tarfile.open(archive, 'w') as tar:
                for name in ('oci-layout', 'index.json', 'blobs'): tar.add(layout / name, arcname=name)
            sbom = root / 'agent-vm-standard-v0.1.0-linux-arm64.spdx.json'
            spdx = {'spdxVersion': 'SPDX-2.3', 'packages': [{'name': 'standard'}]}
            sbom.write_text(json.dumps(spdx))
            inventory = replace(content.PlatformInventory.parse(inventory_json(graph, 'arm64')),
                                archive=operations.asset(archive), sbom=operations.asset(sbom))
            metadata = root / 'platform.json'; content.atomic_json(metadata, inventory.json())
            for kind in ('files', 'image', 'sbom'):
                (root / f'platform-arm64-{kind}.sigstore.json').write_text('{}')
            signed = {str(path): content.hash_file(path)[0].value[7:] for path in (metadata, archive, sbom)}
            invocation = inventory.invocation_url
            def gh(argv, **kwargs):
                subject = argv[3]
                predicate_type = argv[argv.index('--predicate-type')+1]
                statement = {'subject': [{'digest': {'sha256': signed.get(subject, graph.manifest.digest.value[7:])}}],
                    'predicateType': predicate_type, 'predicate': spdx if predicate_type == attestations.spdx_predicate_type(spdx) else
                    {'runDetails': {'metadata': {'invocationId': invocation}}}}
                value = [{'verificationResult': {'verifiedTimestamps': [], 'statement': statement,
                    'signature': {'certificate': {'runInvocationURI': invocation}}}}]
                return subprocess.CompletedProcess(argv, 0, json.dumps(value).encode(), b'')
            with mock.patch.object(attestations.subprocess, 'run', side_effect=gh):
                actual = operations.verify_platform_handoff(root, sha='a'*40, version='0.1.0', run_id='264', attempt='1')
                self.assertEqual(actual, inventory)
                for path in (metadata, archive, sbom):
                    original = path.read_bytes(); path.write_bytes(original + b'changed-after-signing')
                    with self.subTest(path=path.name), self.assertRaises(ValueError):
                        operations.verify_platform_handoff(root, sha='a'*40, version='0.1.0', run_id='264', attempt='1')
                    path.write_bytes(original)

    def test_msb_capacity_checks_actual_tmp_before_state_allocation(self):
        instance = verify.Verification.__new__(verify.Verification)
        with mock.patch.object(verify.capacity, 'check_space', side_effect=ValueError('insufficient /tmp')) as check, \
             mock.patch.object(verify.tempfile, 'mkdtemp') as allocation:
            with self.assertRaises(ValueError): instance.msb_environment('registry')
            self.assertEqual(check.call_args.args[0], Path('/tmp'))
            allocation.assert_not_called()

    def test_missing_asset_requires_successful_transport_and_exact_404(self):
        instance = verify.Verification.__new__(verify.Verification)
        for status in (b'404', b'403', b'429', b'500', b'503', b'000'):
            with mock.patch.object(instance, 'command', return_value=status) as command:
                if status == b'404': instance.missing_asset(version='0.1.0', name='archive.tar')
                else:
                    with self.assertRaises(ValueError): instance.missing_asset(version='0.1.0', name='archive.tar')
                self.assertEqual(command.call_args.args[0][1], '-q')
                self.assertNotIn('GITHUB_TOKEN', command.call_args.kwargs['env'])
        with mock.patch.object(instance, 'command', side_effect=ValueError('TLS failure')):
            with self.assertRaises(ValueError): instance.missing_asset(version='0.1.0', name='archive.tar')

    def test_verify_shell_signature_failure_has_no_build_or_success_record(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); binary = root / 'bin'; binary.mkdir()
            log = root / 'calls.log'
            for name in ('gh', 'docker', 'skopeo', 'msb'):
                tool = binary / name
                tool.write_text('#!/bin/sh\nprintf "%s\n" "' + name + ' $*" >> ' + str(log) + '\nexit 17\n')
                tool.chmod(0o755)
            (root / 'release.json').write_text('{}')
            arch = 'arm64' if __import__('platform').machine() in ('arm64', 'aarch64') else 'amd64'
            result = subprocess.run(['bash', str(ROOT / 'script/release/verify-release.sh'), '--version', '0.1.0',
                '--arch', arch, '--assets-dir', str(root), '--msb-bin', str(binary / 'msb'), '--boot'],
                env=dict(os.environ, PATH=str(binary)+':'+os.environ['PATH']), capture_output=True)
            self.assertEqual(result.returncode, 1)
            self.assertNotIn('build', log.read_text())
            self.assertTrue(log.read_text().startswith('gh attestation verify'))
            self.assertFalse(list(root.rglob('verification-*.json')))
            self.assertFalse(list(root.rglob('public-assets')))

    def test_verifier_does_not_accept_naked_success_evidence(self):
        instance = verify.Verification.__new__(verify.Verification)
        instance.logs = []; instance.checks = {}
        with self.assertRaises(ValueError):
            instance.check('metadata-signature', lambda: None)
        self.assertEqual(instance.checks, {})

    def test_cleanup_attempts_all_owned_resources_and_invalidates_success(self):
        spec = importlib.util.spec_from_file_location('fixture_controls', ROOT / 'script/test/release-transports.py')
        fixture = importlib.util.module_from_spec(spec); spec.loader.exec_module(fixture)
        with tempfile.TemporaryDirectory() as directory:
            instance = fixture.Controls.__new__(fixture.Controls)
            instance.scratch = Path(directory)
            instance.containers = ['owned-1', 'owned-2']; instance.images = ['owned:fixture']
            instance.network = 'owned-network'; instance.network_created = True
            instance.state_dirs = []
            (instance.scratch / 'result.json').write_text('{}')
            calls = []
            def command(argv):
                calls.append(argv)
                raise ValueError('fixture cleanup refusal')
            instance.command = command
            with self.assertRaises(ValueError): instance.cleanup()
            self.assertEqual(len(calls), 4)
            self.assertFalse((instance.scratch / 'result.json').exists())

    def test_build_interface_once_defaults_context_package_and_phase_failures(self):
        # Real CLI owner and packaging, real committed source gate, real tar/graph;
        # only host commands/capability/storage are deterministic stand-ins.
        import platform
        arch = 'arm64' if platform.machine() in ('arm64', 'aarch64') else 'amd64'
        with tempfile.TemporaryDirectory() as directory:
            base = Path(directory); repo = base / 'repo'; repo.mkdir()
            shutil.copytree(ROOT / 'images', repo / 'images')
            shutil.copytree(ROOT / 'script/release', repo / 'script/release', ignore=shutil.ignore_patterns('__pycache__'))
            # committed_version() compares against the checked-in file, so pin the
            # fixture's own version instead of tracking the repository's bump.
            (repo / 'images/standard/version').write_text('0.1.0\n')
            for argv in (['git', 'init', '-q'], ['git', 'add', '.'],
                ['git', '-c', 'user.name=Fixture', '-c', 'user.email=f@invalid', 'commit', '-qm', 'fixture']):
                subprocess.run(argv, cwd=repo, check=True, capture_output=True)
            sha = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo).decode().strip()
            selected = operations.selections()
            image = {'Id': 'sha256:' + 'd'*64, 'Config': {'Labels': {
                **{'org.agent-vm.version.' + name: value for name, value in selected.items()},
                'org.opencontainers.image.version': '0.1.0', 'org.opencontainers.image.revision': sha,
                'org.opencontainers.image.source': content.SOURCE}}}
            original = operations.run
            stats = {'scratch_host': 'fixture-client', 'daemon_filesystems': [{'host': 'fixture-daemon',
                'filesystem': 'disk', 'path': '/owned', 'free_bytes': 100 * capacity.GIB}]}
            phases = [(phase, False) for phase in (None, 'base', 'standard', 'archive', 'load', 'audit', 'sbom', 'cleanup')]
            # A rehearsal build must pass the same end-to-end path, including the
            # packaging step that records the run identity.
            for phase, rehearsal in phases + [(None, True)]:
                calls = []
                def command(argv, **kwargs):
                    calls.append(argv)
                    if argv[0] == 'git':
                        return original(argv, **kwargs)
                    if 'Dockerfile' in ' '.join(argv) and 'capability' not in ' '.join(argv):
                        stage = 'standard' if '/standard/Dockerfile' in ' '.join(argv) else 'base'
                    elif 'archive-oci.sh' in ' '.join(argv): stage = 'archive'
                    elif argv[:3] == ['docker', 'image', 'load']: stage = 'load'
                    elif argv[0] == 'syft': stage = 'sbom'
                    elif argv[:3] == ['docker', 'image', 'rm']: stage = 'cleanup'
                    elif any('/script/test/' in x and x.endswith('.sh') for x in argv): stage = 'audit'
                    else: stage = ''
                    if stage == phase:
                        raise subprocess.CalledProcessError(17, argv)
                    if kwargs.get('output') is not None:
                        kwargs['output'].write_text('fixture command output\n')
                    if argv[:3] == ['docker', 'context', 'show']: return b'fixture-builder\n'
                    if argv[:3] == ['docker', 'buildx', 'inspect']: return b'Driver: docker\n'
                    if stage == 'standard':
                        dest = next(x.split('dest=')[1].split(',')[0] for x in argv if x.startswith('type=oci,'))
                        make_layout(Path(dest), arch)
                    if stage == 'archive': return original(argv, **kwargs)
                    if stage == 'sbom':
                        Path(argv[-1].split('=', 1)[1]).write_text('{"spdxVersion":"SPDX-2.3","packages":[{"name":"standard"}]}')
                    if argv[:2] == ['docker', 'info']: return b'[]'
                    if argv[:2] == ['docker', 'version']: return b'fixture-docker'
                    return b''
                out = base / f'out-{phase}-{rehearsal}'; scratch = base / f'scratch-{phase}-{rehearsal}'
                env = {'GITHUB_REPOSITORY': preflight.REPO, 'GITHUB_SHA': sha,
                       'GITHUB_REF': operations.REHEARSAL_REF_PREFIX + 'fixture' if rehearsal else 'refs/heads/main',
                       'GITHUB_RUN_ID': '264', 'GITHUB_RUN_ATTEMPT': '1'}
                with mock.patch.dict(os.environ, env), mock.patch.object(operations, 'ROOT', repo), \
                     mock.patch.object(operations, 'run', side_effect=command), \
                     mock.patch.object(operations, 'docker_matches', return_value=image), \
                     mock.patch.object(build.capability, 'check'), \
                     mock.patch.object(build.storage, 'measure', return_value=stats), \
                     mock.patch.object(build.peaks.Sampler, 'start'), mock.patch.object(build.peaks.Sampler, 'stop'), \
                     mock.patch.object(sys, 'argv', ['build', '--arch', arch, '--version', '0.1.0']
                                        + (['--rehearsal'] if rehearsal else [])
                                        + ['--out', str(out), '--scratch-root', str(scratch)]):
                    status = build.main()
                self.assertEqual(status, 0 if phase is None else 1, (phase, rehearsal))
                if phase is not None:
                    self.assertFalse((out / 'result.json').exists(), (phase, rehearsal))
                else:
                    recipe_builds = [argv for argv in calls if '-f' in argv and 'build' in argv]
                    self.assertEqual(len(recipe_builds), 2)
                    self.assertEqual(sum('/standard/Dockerfile' in ' '.join(argv) for argv in recipe_builds), 1)
                    self.assertTrue(all(argv[-1] == str(repo / 'images') for argv in recipe_builds))
                    self.assertTrue(all('linux/' + arch in argv for argv in recipe_builds))
                    self.assertIn('BASE_IMAGE=', ' '.join(recipe_builds[1]))
                    self.assertTrue((out / 'platform.json').exists())
                    self.assertEqual(content.PlatformInventory.parse(content.read_json(out / 'platform.json')).selections,
                                     tuple(sorted(selected.items())))
                self.assertEqual(subprocess.check_output(['git', 'status', '--porcelain'], cwd=repo), b'')

    def test_preflight_sigterm_auth_scratch_is_outside_artifact_root(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifact = root / 'artifact'
            marker = root / 'private-path'
            code = """
import sys, time, os
from pathlib import Path
sys.path.insert(0, sys.argv[1])
import preflight
marker = Path(sys.argv[2])
def interrupted(self, url, **kwargs):
    secret = self.private / 'sentinel.curl'
    secret.write_text('Authorization: Bearer synthetic-secret')
    secret.chmod(0o600)
    marker.write_text(str(self.private))
    time.sleep(60)
preflight.Curl.get = interrupted
sys.argv = ['preflight', '--version', '0.1.0', '--source-sha', 'a'*40, '--run-id', '264', '--run-attempt', '1', '--out', sys.argv[3]]
raise SystemExit(preflight.main())
"""
            env = dict(os.environ, GITHUB_REPOSITORY=preflight.REPO, GITHUB_REF='refs/heads/main',
                GITHUB_SHA='a'*40, GITHUB_RUN_ID='264', GITHUB_RUN_ATTEMPT='1', GITHUB_TOKEN='synthetic',
                GHCR_PACKAGE_INVENTORY_TOKEN='synthetic', GITHUB_ACTOR='fixture')
            proc = subprocess.Popen([sys.executable, '-c', code, str(ROOT / 'script/release'), str(marker), str(artifact)], env=env)
            import time
            try:
                for _ in range(100):
                    if marker.exists():
                        break
                    time.sleep(0.02)
                self.assertTrue(marker.exists())
                proc.terminate(); proc.wait(timeout=10)
                private = Path(marker.read_text())
                self.assertFalse(private.is_relative_to(artifact))
                self.assertFalse(list(artifact.rglob('*')))
                self.assertIn('synthetic-secret', (private / 'sentinel.curl').read_text())
                shutil.rmtree(private)
            finally:
                if proc.poll() is None:
                    proc.kill(); proc.wait()

    def test_assembly_cli_blocks_foreign_run_and_attempt_before_remote_write(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            platform_dirs = []
            for arch in ('amd64', 'arm64'):
                folder = root / arch; folder.mkdir()
                graph = make_layout(folder / 'layout', arch)
                (folder / 'platform.json').write_text(json.dumps(inventory_json(graph, arch)))
                (folder / f'platform-{arch}-files.sigstore.json').write_text('{}')
                platform_dirs.append(folder)
            argv = ['operations', 'prepare', '--version', '0.1.0', '--source-sha', 'a'*40,
                    '--run-id', '264', '--run-attempt', '1', '--platform-dir', str(platform_dirs[0]),
                    '--platform-dir', str(platform_dirs[1]), '--out', str(root / 'out')]
            for field in ('build_run_id', 'build_run_attempt'):
                value = inventory_json(make_layout(root / ('mutation-' + field)), 'arm64')
                value[field] = '265' if field == 'build_run_id' else '2'
                value['invocation_url'] = f"{content.SOURCE}/actions/runs/{value['build_run_id']}/attempts/{value['build_run_attempt']}"
                (platform_dirs[1] / 'platform.json').write_text(json.dumps(value))
                with mock.patch.object(operations, 'source_sha', return_value='a'*40), \
                     mock.patch.object(operations, 'committed_version'), \
                     mock.patch.object(operations, 'trusted_run', return_value=('264', '1', 'invocation')), \
                     mock.patch.object(attestations, 'verify'), \
                     mock.patch.object(operations, 'run') as command, \
                     mock.patch.object(operations, 'verify_platform_handoff', wraps=operations.verify_platform_handoff) as handoff, \
                     mock.patch.object(sys, 'argv', argv):
                    # First inventory needs no payload to isolate foreign signed-record validation.
                    original = handoff._mock_wraps
                    handoff.side_effect = lambda path, **kw: (content.PlatformInventory.parse(content.read_json(path / 'platform.json'))
                        if path == platform_dirs[0] else original(path, **kw))
                    self.assertEqual(operations.main(), 1)
                    command.assert_not_called()
                    self.assertFalse((root / 'out').exists())

    def test_real_split_and_reassembly_missing_and_swapped_equal_parts(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            layout = root / 'layout'; graph = make_layout(layout)
            archive = root / 'agent-vm-standard-v0.1.0-linux-arm64.oci.tar'
            with tarfile.open(archive, 'w') as tar:
                for name in ('oci-layout', 'index.json', 'blobs'):
                    tar.add(layout / name, arcname=name)
            assets = root / 'assets'; assets.mkdir()
            parts = operations.split_archive(archive, assets, limit=1024, part_size=1024)
            self.assertGreater(len(parts), 2)
            sbom = assets / 'agent-vm-standard-v0.1.0-linux-arm64.spdx.json'; sbom.write_text('{"spdxVersion":"SPDX-2.3"}')
            inventory = content.PlatformInventory.parse(inventory_json(graph, 'arm64'))
            inventory = replace(inventory, archive=operations.asset(archive), parts=parts, sbom=operations.asset(sbom))
            output = root / 'assembled.tar'
            content.assemble_archive(inventory, assets, output)
            self.assertEqual(output.read_bytes(), archive.read_bytes())
            output.unlink()
            first = assets / parts[0].name; second = assets / parts[1].name
            a, b = first.read_bytes(), second.read_bytes()
            self.assertEqual(len(a), len(b))
            first.write_bytes(b); second.write_bytes(a)
            with self.assertRaises(ValueError):
                content.assemble_archive(inventory, assets, output)
            self.assertFalse(output.exists())
            first.write_bytes(a); second.unlink()
            with self.assertRaises(ValueError):
                content.assemble_archive(inventory, assets, output)
            self.assertFalse(output.exists())

class SignatureBindingTests(unittest.TestCase):
    """The verified statement must bind exact subject, predicate and invocation."""

    def digest(self, text: str) -> str:
        return 'sha256:' + hashlib.sha256(text.encode()).hexdigest()

    def verification(self, predicate, subjects, invocation=None):
        statement = {'predicateType': predicate,
                     'subject': [{'digest': {'sha256': s[7:]}} for s in subjects]}
        if predicate == attestations.SLSA and invocation is not None:
            statement['predicate'] = {'runDetails': {'metadata': {'invocationId': invocation}}}
        return [{'verificationResult': {
            'verifiedTimestamps': [{'type': 'transparency-log', 'uri': 'tlog', 'timestamp': 't'}],
            'statement': statement}}]

    def test_spdx_predicate_type_tracks_the_pinned_action(self):
        # actions/attest-sbom builds this from the SBOM's own spdxVersion, so a verifier
        # that hardcodes the bare URI silently matches nothing (run 37702678174).
        self.assertEqual(attestations.spdx_predicate_type({'spdxVersion': 'SPDX-2.3'}),
                         'https://spdx.dev/Document/v2.3')
        self.assertEqual(attestations.spdx_predicate_type({'spdxVersion': 'SPDX-3.0'}),
                         'https://spdx.dev/Document/v3.0')
        for sbom in ({}, {'spdxVersion': '2.3'}, {'spdxVersion': 'SPDX-'}, {'spdxVersion': 'other-2.3'}):
            with self.subTest(sbom=sbom), self.assertRaises(ValueError):
                attestations.spdx_predicate_type(sbom)

    def test_spdx_verification_requires_the_signed_document(self):
        # Fail closed: the bare URI is not a predicate type any attester produces, so it
        # must be impossible to request an SPDX check without the document that owns it.
        payload = Path(tempfile.mkstemp()[1])
        self.addCleanup(payload.unlink)
        payload.write_text('payload')
        with mock.patch.object(attestations.subprocess, 'run') as run, self.assertRaises(ValueError):
            attestations.verify(payload, Path('/nonexistent-bundle'), source_sha='a' * 40, predicate=attestations.SPDX)
        self.assertFalse(run.called)

    def test_real_gh_certificate_extension_shape(self):
        # Shape anchor captured from gh 2.97.0 `attestation verify --format json` against the
        # real 0.1.1 SBOM attestation (run 37702678174): every field is verbatim except the
        # 16MB SPDX predicate, which is reduced. Two release runs failed `assemble` because
        # this parse assumed a nested "extensions" key that gh does not emit.
        path = Path(__file__).parent / 'fixtures' / 'gh-attestation-verify-spdx.json'
        value = json.loads(path.read_text())
        certificate = value[0]['verificationResult']['signature']['certificate']
        self.assertNotIn('extensions', certificate)
        invocation = content.SOURCE + '/actions/runs/37702678174/attempts/1'
        self.assertEqual(certificate['runInvocationURI'], invocation)
        digest = content.Sha256('sha256:48d51425a678f8643360a09be274f3ef92735b7060a36af0c62ae3b32c294ade')
        spdx = value[0]['verificationResult']['statement']['predicate']
        predicate = attestations.spdx_predicate_type(spdx)
        attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx=spdx)
        certificate['runInvocationURI'] = invocation.replace('/attempts/1', '/attempts/2')
        with self.assertRaises(ValueError):
            attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx=spdx)

    def test_spdx_binds_certificate_run_and_signed_file(self):
        digest = content.Sha256(self.digest('image'))
        invocation = content.SOURCE + '/actions/runs/264/attempts/1'
        spdx = {'spdxVersion': 'SPDX-2.3', 'packages': [{'name': 'standard'}]}
        predicate = attestations.spdx_predicate_type(spdx)
        value = self.verification(predicate, [digest.value])
        verified = value[0]['verificationResult']
        verified['statement']['predicate'] = spdx
        verified['signature'] = {'certificate': {'runInvocationURI': invocation}}
        attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx=spdx)
        verified['signature']['certificate']['runInvocationURI'] = invocation.replace('/264/', '/265/')
        with self.assertRaises(ValueError):
            attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx=spdx)
        verified['signature']['certificate']['runInvocationURI'] = invocation
        with self.assertRaises(ValueError):
            attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx={'packages': []})
        verified['signature'] = []
        with self.assertRaises(ValueError):
            attestations.check_verified(value, digest, predicate=predicate, invocation=invocation, spdx=spdx)

    def test_matching_signature_accepts_and_mutations_reject(self):
        path = Path(tempfile.mkstemp()[1])
        self.addCleanup(path.unlink)
        path.write_text('signed payload')
        digest = content.Sha256(self.digest('signed payload'))
        invocation = content.SOURCE + '/actions/runs/264/attempts/1'
        good = self.verification(attestations.SLSA, [digest.value], invocation)
        attestations.check_verified(good, digest, predicate=attestations.SLSA, invocation=invocation)
        for value in (self.verification(attestations.SLSA, [digest.value], invocation + '9'),
                      self.verification(attestations.SLSA, [self.digest('other')], invocation),
                      self.verification(attestations.SPDX, [digest.value], invocation),
                      [], None):
            with self.subTest(value=value), self.assertRaises(ValueError):
                attestations.check_verified(value, digest, predicate=attestations.SLSA, invocation=invocation)


class ReleaseInterfaceTests(unittest.TestCase):
    """Real public wrappers/policies, fake external clients; never deployment proof."""

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.checkout = self.root / 'checkout'
        self.checkout.mkdir()
        for name in ('script', 'images'):
            shutil.copytree(ROOT / name, self.checkout / name, ignore=shutil.ignore_patterns('__pycache__'))
        # The release fixtures below are written for 0.1.0 and committed_version()
        # reads this copied file, so pin it instead of tracking the repository bump.
        (self.checkout / 'images/standard/version').write_text('0.1.0\n')
        # Scale resource caps only in this disposable, committed fixture source.
        # Real free-space gates still run; tiny OCI interface tests must not need
        # standard-size (~49 GiB) host headroom. Production caps are unchanged.
        caps = self.checkout / 'script/release/content.py'
        source = caps.read_text()
        for original, replacement in (('COMPRESSED_CAP = 4 * 1024 ** 3', 'COMPRESSED_CAP = 8 * 1024 ** 2'),
                                      ('UNCOMPRESSED_CAP = 8 * 1024 ** 3', 'UNCOMPRESSED_CAP = 16 * 1024 ** 2')):
            self.assertIn(original, source)
            source = source.replace(original, replacement)
        caps.write_text(source)
        for argv in (['init', '-q'], ['add', '.'], ['-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
                        'commit', '-qm', 'Frozen verifier source']):
            subprocess.run(['git', '-C', str(self.checkout), *argv], check=True, capture_output=True)
        self.sha = subprocess.check_output(['git', '-C', str(self.checkout), 'rev-parse', 'HEAD']).decode().strip()
        self.arch = {'arm64': 'arm64', 'aarch64': 'arm64', 'x86_64': 'amd64'}[platform.machine()]
        self.assets = self.root / 'assets'; self.assets.mkdir()
        self.binary = self.root / 'bin'; self.binary.mkdir()
        self.remote = self.root / 'remote'; self.remote.mkdir()
        self.calls_path = self.root / 'calls.jsonl'
        self.config_path = self.root / 'config.json'
        source = (ROOT / 'script/test/fixtures/release_fake_cli.py').read_text()
        source = source.replace('#!/usr/bin/env python3', '#!' + sys.executable).replace('__FIXTURE_CONFIG__', str(self.config_path))
        for name in ('docker', 'buildx', 'skopeo', 'gh', 'curl', 'msb', 'bash', 'htpasswd', 'otool', 'ldd'):
            tool = self.binary / name; tool.write_text(source); tool.chmod(0o755)
        self.firmware = self.root / 'firmware'; self.firmware.write_bytes(b'fake firmware; not a real runtime')
        inventories, layouts, folders = [], {}, []
        for arch in ('amd64', 'arm64'):
            folder = self.root / arch; folder.mkdir(); folders.append(folder)
            layout = folder / 'layout'; graph = make_layout(layout, arch); layouts[arch] = str(layout)
            archive = self.assets / f'agent-vm-standard-v0.1.0-linux-{arch}.oci.tar'
            with tarfile.open(archive, 'w') as tar:
                for name in ('oci-layout', 'index.json', 'blobs'): tar.add(layout / name, arcname=name)
            sbom = self.assets / f'agent-vm-standard-v0.1.0-linux-{arch}.spdx.json'
            sbom.write_text('{"spdxVersion":"SPDX-2.3","packages":[{"name":"standard"}]}')
            inventory = replace(content.PlatformInventory.parse(inventory_json(graph, arch)),
                source_sha=self.sha, selections=tuple(sorted(operations.selections().items())),
                archive=operations.asset(archive), sbom=operations.asset(sbom))
            inventories.append(inventory)
            metadata = self.assets / f'platform-{arch}.json'; content.atomic_json(metadata, inventory.json())
            self.bundle(f'platform-{arch}-files.sigstore.json', [metadata, archive, sbom])
            self.bundle(f'platform-{arch}-image.sigstore.json', [graph.manifest.digest])
            self.bundle(f'platform-{arch}-sbom.sigstore.json', [graph.manifest.digest], json.loads(sbom.read_text()))
            for path in (metadata, archive, sbom, *self.assets.glob(f'platform-{arch}-*.sigstore.json')):
                shutil.copyfile(path, folder / ('platform.json' if path == metadata else path.name))
        self.folders = folders
        self.index = self.root / 'index.json'
        content.atomic_json(self.index, {'schemaVersion': 2, 'mediaType': content.INDEX,
            'manifests': [dict(mediaType=x.graph.manifest.media_type, size=x.graph.manifest.size, digest=x.graph.manifest.digest.value, platform={'os': 'linux', 'architecture': x.graph.arch}) for x in inventories]})
        self.release = content.ReleaseMetadata(content.parse_version('0.1.0'), self.sha, '264', '1',
            content.hash_file(self.index)[0], tuple(inventories))
        content.atomic_json(self.assets / 'release.json', self.release.json())
        operations.checksums(self.assets, ['release.json'] + [x.name for i in inventories for x in (i.archive, i.sbom)])
        self.bundle('release-metadata.sigstore.json', [self.assets / 'release.json', self.assets / 'SHA256SUMS'])
        self.bundle('release-index.sigstore.json', [self.release.index_digest])
        content.atomic_json(self.assets / 'state.json', {'release_id': 264, 'version': '0.1.0', 'source_sha': self.sha,
            'release_subject': content.hash_file(self.assets / 'release.json')[0].value,
            'index_digest': self.release.index_digest.value, 'files': ['release.json', 'SHA256SUMS']})
        self.config = {'state': str(self.remote), 'calls': str(self.calls_path), 'bin': str(self.binary),
            'arch': self.arch, 'source_sha': self.sha, 'layouts': layouts, 'index': str(self.index),
            'index_digest': self.release.index_digest.value, 'assets': str(self.assets)}
        self.env = dict(os.environ, PATH=str(self.binary) + ':' + os.environ['PATH'], PYTHONDONTWRITEBYTECODE='1',
            GITHUB_REPOSITORY=preflight.REPO, GITHUB_REF='refs/heads/main', GITHUB_SHA=self.sha,
            GITHUB_RUN_ID='264', GITHUB_RUN_ATTEMPT='1', GITHUB_TOKEN='fixture-host-secret',
            GITHUB_ACTOR='fixture', GITHUB_ACTIONS='true',
            GH_TOKEN='fixture-host-secret', GHCR_PACKAGE_INVENTORY_TOKEN='fixture-owner-secret',
            MSB_LIBKRUNFW_PATH=str(self.firmware), MSB_LIBKRUN_VERSION='fake-runtime', MSB_LIBKRUN_EMBEDDED='1')
        # The real Darwin storage gate still sees an existing backing filesystem.
        self.env['RELEASE_VM_BACKING_PATH'] = str(self.root)
        auth = self.root / 'publisher-auth'; auth.mkdir()
        (auth / 'config.json').write_text('{"auths":{}}')
        self.env['DOCKER_CONFIG'] = str(auth)
        self.owned_states = set()
        self.addCleanup(self.remove_owned_states)

    def bundle(self, name, subjects, spdx=None):
        invocation = content.SOURCE + '/actions/runs/264/attempts/1'
        entries = []
        for subject in subjects:
            digest = subject if isinstance(subject, content.Sha256) else content.hash_file(subject)[0]
            entries.append({'verificationResult': {'verifiedTimestamps': [], 'statement': {
                'subject': [{'digest': {'sha256': digest.value[7:]}}],
                'predicateType': attestations.spdx_predicate_type(spdx) if spdx is not None else attestations.SLSA,
                'predicate': spdx if spdx is not None else {'runDetails': {'metadata': {'invocationId': invocation}}}},
                'signature': {'certificate': {'runInvocationURI': invocation}}}})
        content.atomic_json(self.assets / name, {'fake_verified': entries})

    def calls(self):
        return [json.loads(x) for x in self.calls_path.read_text().splitlines()] if self.calls_path.exists() else []

    def remember_owned_states(self):
        # Cumulative ledger: some tests reset self.calls_path, so reading only the
        # current calls file at cleanup would forget earlier owned states.
        for call in self.calls():
            home = call['env'].get('MSB_HOME')
            if not home:
                continue
            parent = Path(home).parent
            if parent.parent == Path('/tmp') and parent.name.startswith(('av264-', 'a264-')):
                self.owned_states.add(parent)

    def remove_owned_states(self):
        for parent in self.owned_states:
            shutil.rmtree(parent, ignore_errors=True)

    def invoke(self, mode, failure=None, env=None):
        self.config.update(mode=mode, failure=failure)
        content.atomic_json(self.config_path, self.config)
        if mode == 'verify':
            script, args = 'verify-release.sh', ['--version', '0.1.0', '--arch', self.arch, '--assets-dir', str(self.assets),
                '--msb-bin', str(self.binary / 'msb'), '--boot']
        elif mode == 'prepare':
            script, args = 'release-standard.sh', ['prepare', '--version', '0.1.0', '--source-sha', self.sha,
                '--run-id', '264', '--run-attempt', '1', '--platform-dir', str(self.folders[0]),
                '--platform-dir', str(self.folders[1]), '--out', str(self.root / 'assembled')]
        else:
            script, args = 'release-standard.sh', ['publish', '--state', str(self.assets / 'state.json')]
        result = subprocess.run(['/bin/bash', str(self.checkout / 'script/release' / script), *args],
            env=env or self.env, capture_output=True, text=True, timeout=180)
        self.remember_owned_states()
        return result

    def test_successful_verifier_interface_is_isolated_build_free_and_hash_bound(self):
        result = self.invoke('verify')
        self.assertEqual(result.returncode, 0, result.stderr)
        records = list(self.assets.rglob('verification-' + self.arch + '.json'))
        self.assertEqual(len(records), 1)
        record = content.read_json(records[0])
        self.assertEqual(set(record['checks']), {
            'metadata-signature', 'platform-signatures', 'public-assets', 'anonymous-index',
            'anonymous-platform', 'registry-archive-graph', 'docker-artifact',
            'msb-registry-inspect', 'msb-archive-inspect', 'msb-registry-boot', 'msb-archive-boot',
            'corrupt-archive', 'missing-asset', 'wrong-architecture', 'fixture-corrupt-blob',
            'fixture-corrupt-archive', 'fixture-wrong-architecture', 'runtime-doctor',
        })
        self.assertEqual(record['release_subject'], content.hash_file(self.assets / 'release.json')[0].value)
        self.assertEqual(record['platform'], self.release.platform(self.arch).json())
        self.assertEqual(record['index_digest'], self.release.index_digest.value)
        registry = content.read_json(records[0].parent / (self.arch + '-registry-state.log'))
        archive = content.read_json(records[0].parent / (self.arch + '-archive-state.log'))
        self.assertNotEqual(registry['MSB_HOME'], archive['MSB_HOME'])
        self.assertEqual(registry['initial_cache_entries'], [])
        self.assertFalse(registry['docker_on_path'])
        names = {x['name'] for x in record['logs']}
        self.assertGreater(len(names), 50)
        for log in record['logs']:
            data = (records[0].parent / log['name']).read_bytes()
            self.assertEqual(len(data), log['size'])
            self.assertEqual('sha256:' + hashlib.sha256(data).hexdigest(), log['sha256'])
        for check in record['checks'].values():
            self.assertEqual(check['status'], 0)
            self.assertTrue(check['logs']); self.assertTrue(set(check['logs']) <= names)
        calls = self.calls()
        self.assertFalse(any(arg in ('build', 'prune') for x in calls for arg in x['argv']))
        self.assertTrue(any(x['tool'] == 'bash' and '--artifact' in x['argv'] for x in calls))
        self.assertTrue(any(x['tool'] == 'docker' and x['argv'][0] == 'pull' for x in calls))
        self.assertTrue(any(x['tool'] == 'curl' and '/releases/download/' in x['argv'][-1] for x in calls))
        states = {x['env']['MSB_HOME'] for x in calls if x['env'].get('MSB_HOME')}
        self.assertGreaterEqual(len(states), 10)
        for call in calls:
            if call['env'].get('MSB_HOME'):
                self.assertFalse(call['credential_variables'])
                if Path(call['env']['MSB_HOME']).parent.name.startswith('av264-'):
                    self.assertIsNone(call['env']['DOCKER_CONFIG'])
                    self.assertIsNone(call['env']['DOCKER_HOST'])
                    self.assertNotIn(str(self.binary), call['env']['PATH'])
                else:
                    self.assertEqual(content.read_json(Path(call['env']['DOCKER_CONFIG']) / 'config.json')['auths'], {})
            if call['tool'] == 'curl':
                self.assertFalse(call['credential_variables'])
                if '/releases/download/' in call['argv'][-1]:
                    self.assertEqual(call['env']['HOME'], '/nonexistent')
                    self.assertEqual(call['argv'][:3], ['-q', '--netrc-file', '/dev/null'])
            if call['tool'] == 'skopeo' or (call['tool'] == 'docker' and call['argv'][0] == 'pull'):
                self.assertFalse(call['credential_variables'])
                auth = content.read_json(Path(call['env']['DOCKER_CONFIG']) / 'config.json')
                self.assertEqual(auth['auths'], {})
        self.assertFalse(subprocess.check_output(['git', '-C', str(self.checkout), 'status', '--porcelain']).strip())

    def test_verifier_late_failures_never_emit_success(self):
        for failure, expected in (('signature', 'invocation'), ('import', 'av264-standard:archive'), ('evidence-write', 'verification-')):
            with self.subTest(failure=failure):
                self.calls_path.unlink(missing_ok=True)
                (self.remote / 'remote.json').unlink(missing_ok=True)
                result = self.invoke('verify', failure)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn(expected, result.stderr)
                self.assertFalse([x for x in self.assets.rglob('verification-*.json') if x.is_file()])
                calls = self.calls()
                self.assertFalse(any('build' in x['argv'] for x in calls))
                self.assertNotIn('Traceback', result.stderr)
                self.assertNotIn('completed verification-', result.stdout)
                if failure == 'signature':
                    self.assertTrue(any(x['tool'] == 'msb' and x['argv'][0] == 'create' for x in calls))
                    self.assertFalse(any(x['tool'] == 'docker' and x['argv'][0] == 'pull' for x in calls))
                elif failure == 'import':
                    self.assertTrue(any(x['tool'] == 'docker' and x['argv'][0] == 'pull' for x in calls))
                    self.assertTrue(any(x['tool'] == 'msb' and 'av264-standard:archive' in x['argv'] for x in calls))
                else:
                    remote = content.read_json(self.remote / 'remote.json')
                    self.assertEqual(remote['removes'], 7)
                    self.assertTrue((Path(remote['verify_root']) / ('verification-' + self.arch + '.json')).is_dir())

    def test_publish_rejects_same_source_foreign_run_or_attempt_before_any_client(self):
        for run, attempt in (('265', '1'), ('264', '2')):
            with self.subTest(run=run, attempt=attempt):
                result = self.invoke('publish', env=dict(self.env, GITHUB_RUN_ID=run, GITHUB_RUN_ATTEMPT=attempt))
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('another run/attempt', result.stderr)
                self.assertEqual(self.calls(), [])

    def test_publish_signature_upload_and_index_controls_stop_publication(self):
        for failure in ('publish-signature', 'upload', 'index-changed', 'platform-tag-changed', 'platform-tag-deleted', None):
            with self.subTest(failure=failure):
                self.calls_path.unlink(missing_ok=True)
                (self.remote / 'remote.json').unlink(missing_ok=True)
                result = self.invoke('publish', failure)
                self.assertEqual(result.returncode, int(failure is not None), result.stderr)
                calls = self.calls()
                self.assertEqual(any(x['argv'][:2] == ['release', 'edit'] for x in calls), failure is None)
                if failure == 'publish-signature':
                    self.assertIn('invocation/SPDX mismatch', result.stderr)
                    self.assertFalse(any(x['argv'][:2] == ['release', 'upload'] for x in calls))
                elif failure == 'upload':
                    self.assertTrue(any(x['argv'][:2] == ['release', 'upload'] for x in calls))
                elif failure is None:
                    remote = content.read_json(self.remote / 'remote.json')
                    state = content.read_json(self.assets / 'state.json')
                    self.assertTrue(remote['published'])
                    self.assertEqual(len(remote['uploads']), len(state['files']))
                self.assertFalse(any('build' in x['argv'] for x in calls))

    def test_prepare_failures_stop_continuation_and_success_signs_and_publishes(self):
        for failure in ('index', 'draft'):
            with self.subTest(failure=failure):
                shutil.rmtree(self.root / 'assembled', ignore_errors=True)
                (self.remote / 'remote.json').unlink(missing_ok=True)
                self.calls_path.unlink(missing_ok=True)
                result = self.invoke('prepare', failure)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('imagetools' if failure == 'index' else 'POST', result.stderr)
                self.assertFalse((self.root / 'assembled/state.json').exists())
                self.assertFalse((self.root / 'assembled/result.json').exists())
                calls = self.calls()
                self.assertFalse(any(x['argv'][:2] == ['release', 'upload'] for x in calls))
                if failure == 'index':
                    self.assertFalse(any('--method' in x['argv'] for x in calls))
                else:
                    self.assertTrue(any(x['tool'] == 'docker' and 'imagetools' in x['argv'] for x in calls))
        shutil.rmtree(self.root / 'assembled', ignore_errors=True)
        self.calls_path.unlink(missing_ok=True)
        (self.remote / 'remote.json').unlink(missing_ok=True)
        result = self.invoke('prepare')
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        assembled = self.root / 'assembled'
        self.bundle('release-metadata.sigstore.json', [assembled / 'release.json', assembled / 'SHA256SUMS'])
        self.bundle('release-index.sigstore.json', [self.release.index_digest])
        for name in ('release-metadata.sigstore.json', 'release-index.sigstore.json'):
            shutil.copyfile(self.assets / name, assembled / name)
        self.config.update(mode='publish', failure=None)
        content.atomic_json(self.config_path, self.config)
        result = subprocess.run(['/bin/bash', str(self.checkout / 'script/release/release-standard.sh'),
            'publish', '--state', str(assembled / 'state.json')], env=self.env,
            capture_output=True, text=True, timeout=180)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        remote = content.read_json(self.remote / 'remote.json')
        self.assertTrue(remote['published'])
        self.assertEqual(len(remote['uploads']), len(content.read_json(assembled / 'state.json')['files']))
        calls = self.calls()
        self.assertFalse(any('build' in x['argv'] for x in calls))
        self.assertEqual(sum(x['tool'] == 'gh' and '--method' in x['argv'] for x in calls), 1)
        for arch in ('amd64', 'arm64'):
            self.assertTrue(any(x['tool'] == 'skopeo' and x['argv'][-1].endswith(':v0.1.0-' + arch) for x in calls))

    def test_platform_tag_replacement_and_deletion_block_final_verifier(self):
        for failure in ('platform-tag-changed', 'platform-tag-deleted'):
            with self.subTest(failure=failure):
                self.calls_path.unlink(missing_ok=True)
                (self.remote / 'remote.json').unlink(missing_ok=True)
                result = self.invoke('verify', failure)
                self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
                self.assertFalse([x for x in self.assets.rglob('verification-*.json') if x.is_file()])
                self.assertTrue(any(x['tool'] == 'skopeo' and x['argv'][-1].endswith(':v0.1.0-' + self.arch)
                                    for x in self.calls()))

    def test_nested_docker_json_and_missing_buildx_are_classified(self):
        for context in ([], [None], [{}], [{'Endpoints': []}], [{'Endpoints': {'docker': []}}],
                        [{'Endpoints': {'docker': {'Host': []}}}]):
            with self.subTest(context=context):
                self.config['context'] = context
                content.atomic_json(self.config_path, self.config)
                for script, args, prefix in (
                    ('script/release/storage.py', ['--scratch', str(self.root), '--output', str(self.root / 'storage.json')], 'storage:'),
                    ('script/test/release-transports.py', ['--arch', self.arch, '--scratch-root', str(self.root)], 'transport:'),
                ):
                    result = subprocess.run([sys.executable, str(self.checkout / script), *args],
                        env=self.env, capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 1, result.stderr)
                    self.assertIn(prefix, result.stderr)
                    self.assertIn('Docker context', result.stderr)
                    self.assertNotIn('Traceback', result.stderr)
                result = self.invoke('verify')
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('verify-release: Docker context', result.stderr)
                self.assertNotIn('Traceback', result.stderr)
        self.config.pop('context')
        self.config['plugins'] = [{'Name': 'unrelated', 'Path': str(self.binary / 'docker')}]
        result = self.invoke('verify')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('exactly one named buildx plugin required', result.stderr)
        self.assertNotIn('Traceback', result.stderr)
        self.config['plugins'] = []
        result = self.invoke('verify')
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn('exactly one named buildx plugin required', result.stderr)
        self.assertNotIn('Traceback', result.stderr)
        self.config.pop('plugins')
        for daemon in ({'DriverStatus': {}}, {'DriverStatus': [None]},
                       {'DriverStatus': [['driver-type', 'io.containerd.snapshotter.v1']], 'ID': [], 'DockerRootDir': '/fake'},
                       {'DriverStatus': [['driver-type', 'io.containerd.snapshotter.v1']], 'ID': 'fake',
                        'DockerRootDir': '/fake', 'Containerd': []}):
            with self.subTest(daemon=daemon):
                self.config['daemon'] = daemon
                content.atomic_json(self.config_path, self.config)
                result = subprocess.run([sys.executable, str(self.checkout / 'script/release/storage.py'),
                    '--scratch', str(self.root), '--output', str(self.root / 'storage.json')],
                    env=self.env, capture_output=True, text=True, timeout=30)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('storage: daemon', result.stderr)
                self.assertNotIn('Traceback', result.stderr)

    def test_changed_recorded_draft_stops_before_signing_or_upload(self):
        for change in ({'draft': False}, {'assets': [{'id': 1}]}, {'target_commitish': 'b' * 40}, {'tag_name': 'v0.2.0'}):
            with self.subTest(change=change):
                self.calls_path.unlink(missing_ok=True)
                content.atomic_json(self.remote / 'remote.json', {'draft': dict(id=264, draft=True, assets=[],
                    target_commitish=self.sha, tag_name='v0.1.0') | change})
                result = self.invoke('publish')
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertIn('recorded draft changed', result.stderr)
                self.assertEqual(len(self.calls()), 1)


class RehearsalRunTests(unittest.TestCase):
    def env(self, **overrides):
        value = {'GITHUB_REPOSITORY': preflight.REPO,
                 'GITHUB_REF': operations.REHEARSAL_REF_PREFIX + 'try',
                 'GITHUB_SHA': 'a' * 40, 'GITHUB_RUN_ID': '264', 'GITHUB_RUN_ATTEMPT': '1'}
        value.update(overrides)
        return value

    def test_only_a_rehearsal_ref_is_accepted(self):
        # `main`, unrelated branches and the bare prefix must all be refused, so
        # rehearsal can never validate a publication ref or vice versa.
        for ref in ('refs/heads/main', 'refs/heads/feature/x', 'refs/heads/test/release',
                    'refs/heads/test/release-standard', 'refs/heads/test/release-standard/'):
            with self.subTest(ref=ref), mock.patch.dict(os.environ, self.env(GITHUB_REF=ref)):
                with self.assertRaises(ValueError):
                    operations.rehearsal_run(sha='a' * 40)
        with mock.patch.dict(os.environ, self.env()):
            operations.rehearsal_run(sha='a' * 40)

    def test_rehearsal_requires_the_exact_run_identity(self):
        for overrides in ({'GITHUB_REPOSITORY': 'other/repo'}, {'GITHUB_SHA': 'b' * 40},
                          {'GITHUB_RUN_ID': '0'}, {'GITHUB_RUN_ATTEMPT': 'x'}):
            with self.subTest(**overrides), mock.patch.dict(os.environ, self.env(**overrides)):
                with self.assertRaises(ValueError):
                    operations.rehearsal_run(sha='a' * 40)


if __name__ == '__main__':
    unittest.main()
