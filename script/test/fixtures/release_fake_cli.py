#!/usr/bin/env python3
"""Deterministic external clients for release-interface tests, NEVER real acceptance.

Installed with an absolute Python shebang and a private config filename. The fake
signer returns frozen verified statements; production binding policies still run.
OCI bytes/hashes, archives, clean git checkout, isolation and output files are real.
"""
import sys
sys.dont_write_bytecode = True
import gzip
import hashlib
import json
import os
import re
import shutil
import subprocess
import tarfile
from pathlib import Path

CONFIG = Path('__FIXTURE_CONFIG__')
config = json.loads(CONFIG.read_text())
args = sys.argv[1:]
tool = Path(sys.argv[0]).name
state_path = Path(config['state']) / 'remote.json'
state = json.loads(state_path.read_text()) if state_path.exists() else {}
with Path(config['calls']).open('a') as log:
    log.write(json.dumps({'tool': tool, 'argv': args,
        'env': {key: os.environ.get(key) for key in ('HOME', 'PATH', 'DOCKER_CONFIG', 'DOCKER_HOST',
            'REGISTRY_AUTH_FILE', 'MSB_HOME', 'MSB_CONFIG_PATH')},
        'credential_variables': [key for key in ('GITHUB_TOKEN', 'GH_TOKEN', 'PUBLISH_TOKEN',
            'GHCR_PACKAGE_INVENTORY_TOKEN') if os.environ.get(key)]}) + '\n')
# Discover the owned verifier root without exposing a production hook.
home = Path(os.environ.get('HOME', '/nonexistent'))
if home.name == 'home' and home.parent.name.startswith('verify-'):
    state['verify_root'] = str(home.parent)


def save():
    state_path.write_text(json.dumps(state))


def fail(message='fake client failure'):
    save()
    print(message, file=sys.stderr)
    raise SystemExit(17)


def option(flag, default=None):
    return args[args.index(flag) + 1] if flag in args else default


def emit(value):
    sys.stdout.buffer.write(value if isinstance(value, bytes) else json.dumps(value).encode())


def graph(layout, arch=None):
    index = json.loads((layout / 'index.json').read_text())
    root = index['manifests'][0]
    def read(desc):
        data = (layout / 'blobs/sha256' / desc['digest'][7:]).read_bytes()
        if len(data) != desc['size'] or hashlib.sha256(data).hexdigest() != desc['digest'][7:]:
            raise ValueError('Digest did not match, expected ' + desc['digest'])
        return data
    manifest = json.loads(read(root))
    cfg = json.loads(read(manifest['config']))
    if arch is not None and cfg['architecture'] != arch:
        raise ValueError('wrong host architecture')
    for layer, diff_id in zip(manifest['layers'], cfg['rootfs']['diff_ids']):
        data = read(layer)
        raw = gzip.decompress(data) if layer['mediaType'].endswith('+gzip') else data
        if 'sha256:' + hashlib.sha256(raw).hexdigest() != diff_id:
            raise ValueError('diff_id mismatch')
    return {'digest': root['digest'], 'config': {'digest': manifest['config']['digest']},
        'os': cfg['os'], 'architecture': cfg['architecture'], 'layers': [
            {'blob_digest': layer['digest'], 'diff_id': diff_id, 'media_type': layer['mediaType']}
            for layer, diff_id in zip(manifest['layers'], cfg['rootfs']['diff_ids'])]}


def registry_path(ref):
    endpoint = ref.split('/')[0]
    name = state.get('registries', {}).get(endpoint)
    return Path(config['state']) / ('registry-' + name) if name else None


def source_layout(ref):
    local = registry_path(ref)
    if local is not None:
        return local
    arch = option('--override-arch', config['arch'])
    if '@sha256:' in ref:
        digest = ref.rsplit('@', 1)[1]
        for candidate in config['layouts'].values():
            if graph(Path(candidate))['digest'] == digest:
                return Path(candidate)
    elif ref.endswith('-amd64') or ref.endswith('-arm64'):
        arch = ref.rsplit('-', 1)[1]
    return Path(config['layouts'][arch])


if tool == 'docker':
    if args[:2] == ['context', 'inspect']:
        emit(config.get('context', [{'Endpoints': {'docker': {'Host': 'unix:///fixture-only.sock'}}}]))
    elif args[:2] == ['context', 'show']:
        print('fixture-builder')
    elif args[0] == 'info':
        if 'Plugins' in option('--format', ''):
            emit(config.get('plugins', [{'Name': 'not-buildx', 'Path': config['bin'] + '/docker'},
                                       {'Name': 'buildx', 'Path': config['bin'] + '/buildx'}]))
        else:
            emit(config.get('daemon', {'DriverStatus': [['driver-type', 'io.containerd.snapshotter.v1']],
                'DockerRootDir': '/fixture/docker', 'Containerd': {'Address': '/run/containerd/containerd.sock'}, 'ID': 'fixture'}))
    elif args[:2] == ['buildx', 'inspect']:
        print('Driver: docker')
    elif args[:3] == ['buildx', 'imagetools', 'create']:
        if config.get('failure') == 'index': fail('injected index creation failure')
        state['index_created'] = True
    elif args[:2] == ['network', 'create']:
        print(args[-1])
    elif args[0] == 'create':
        name = option('--name')
        private = '-private' in name
        state.setdefault('registries', {})['127.0.0.1:' + ('5002' if private else '5001')] = name
        print(name)
    elif args[0] == 'port':
        print('127.0.0.1:' + ('5002' if '-private' in args[1] else '5001'))
    elif args[0] == 'cp':
        source, destination = args[1:]
        if ':' in source:
            container, remote = source.split(':', 1)
            digest = remote.split('/')[-2]
            shutil.copyfile(Path(config['state']) / ('registry-' + container) / 'blobs/sha256' / digest, destination)
        elif '/var/lib/registry/' in destination:
            container, remote = destination.split(':', 1)
            digest = remote.split('/')[-2]
            shutil.copyfile(source, Path(config['state']) / ('registry-' + container) / 'blobs/sha256' / digest)
        # htpasswd folder copy is test-only setup, not a registry implementation.
    elif args[0] == 'run':
        if '/probe/0' in args[-1]:
            print('1\nfixture 999999999 1 100000000 1% /probe\n' * 3, end='')
        else:
            print('fake Docker guest output; not a real container')
    elif 'build' in args:
        fail('verifier/publisher attempted forbidden build')
    elif args[:2] == ['buildx', 'version'] or args[0] == 'version':
        print('fake Docker/buildx interface version')
    elif args[0] not in ('start', 'stop', 'rm', 'pull', 'image', 'network'):
        fail('unsupported fake Docker operation')

elif tool == 'skopeo':
    if '--version' in args:
        print('fake skopeo interface version')
    elif 'inspect' in args:
        ref = args[-1].removeprefix('docker://')
        local = registry_path(ref)
        if local is not None and '-private' in str(local) and '--authfile' not in args:
            fail('UNAUTHORIZED')
        if ref.endswith(':absent'): fail('MANIFEST_UNKNOWN')
        if ref.endswith('-' + config['arch']) and config.get('failure') == 'platform-tag-deleted':
            fail('MANIFEST_UNKNOWN platform tag')
        if ref.endswith('-' + config['arch']) and config.get('failure') == 'platform-tag-changed':
            other = 'amd64' if config['arch'] == 'arm64' else 'arm64'
            layout = Path(config['layouts'][other])
            emit((layout / 'blobs/sha256' / graph(layout)['digest'][7:]).read_bytes())
        elif ref.endswith(':v0.1.0') or ref.endswith('@' + config['index_digest']):
            if config.get('mode') == 'prepare' and not state.get('index_created'): fail('MANIFEST_UNKNOWN index')
            raw = Path(config['index']).read_bytes()
            if config.get('failure') == 'index-changed':
                changed = json.loads(raw)
                changed['manifests'][0]['digest'] = 'sha256:' + '0' * 64
                raw = json.dumps(changed).encode()
            emit(raw)
        else:
            layout = source_layout(ref)
            emit((layout / 'blobs/sha256' / graph(layout)['digest'][7:]).read_bytes())
    elif 'copy' in args:
        source, destination = args[-2:]
        if '--preserve-digests' not in args: fail('missing preserve-digests')
        layout = Path(source[4:].rsplit(':', 1)[0]) if source.startswith('oci:') else source_layout(source.removeprefix('docker://'))
        try:
            graph(layout)
        except (ValueError, OSError) as error:
            fail(str(error))
        if destination.startswith('oci:'):
            if source.startswith('docker://') and '--src-no-creds' not in args: fail('missing anonymous source flag')
            target = Path(destination[4:].rsplit(':', 1)[0])
        else:
            target = registry_path(destination.removeprefix('docker://'))
        if target is None: fail('unknown fixture registry')
        if target.exists(): shutil.rmtree(target)
        shutil.copytree(layout, target)
    else:
        fail('unsupported fake skopeo operation')

elif tool == 'gh':
    if args == ['--version']:
        print('fake gh verifier interface version')
    elif args[:2] == ['attestation', 'verify']:
        bundle = json.loads(Path(option('--bundle')).read_text())
        if option('--source-digest') not in (None, config['source_sha']): fail('source digest disagreement')
        required = ('--deny-self-hosted-runners', '--source-ref', '--predicate-type', '--format', '--signer-workflow')
        if any(flag not in args for flag in required): fail('missing verifier trust policy')
        subject = args[2]
        digest = subject.rsplit('@sha256:', 1)[1] if subject.startswith('oci://') else hashlib.sha256(Path(subject).read_bytes()).hexdigest()
        predicate = option('--predicate-type')
        matches = [entry for entry in bundle['fake_verified'] if entry['verificationResult']['statement']['predicateType'] == predicate
            and any(x['digest']['sha256'] == digest for x in entry['verificationResult']['statement']['subject'])]
        if not matches: fail('signed subject mismatch')
        if config.get('failure') == 'signature' and predicate.startswith('https://spdx.dev/Document'):
            matches[0]['verificationResult']['signature']['certificate']['extensions']['runInvocationURI'] += '9'
        if config.get('failure') == 'publish-signature' and Path(subject).name == 'SHA256SUMS':
            matches[0]['verificationResult']['statement']['predicate']['runDetails']['metadata']['invocationId'] += '9'
        emit(matches)
    elif args[0] == 'api':
        endpoint = next(x for x in args if x.startswith('repos/'))
        if '--method' in args:
            payload = json.load(sys.stdin)
            if config.get('failure') == 'draft': fail('injected draft creation failure')
            state['draft'] = {'id': 264, 'draft': True, 'tag_name': payload['tag_name'],
                'target_commitish': payload['target_commitish'], 'assets': []}
            emit(state['draft'])
        else:
            emit(state.get('draft', {'id': 264, 'draft': True, 'tag_name': 'v0.1.0',
                'target_commitish': config['source_sha'], 'assets': []}))
    elif args[:2] == ['release', 'upload']:
        if config.get('failure') == 'upload': fail('injected upload failure')
        state.setdefault('uploads', []).append(args[3])
    elif args[:2] == ['release', 'edit']:
        state['published'] = True
    else:
        fail('unsupported fake gh operation')

elif tool == 'curl':
    url = args[-1]
    if '--config' in args:
        text = Path(option('--config')).read_text()
        url = re.search(r'^url = "([^"]+)"', text, re.M).group(1)
    # The real-curl hermetic HTTP suite is still run by fixture controls.
    if url.startswith('http://127.0.0.1:') and not any(':' + port + '/' in url for port in ('5001', '5002')):
        raise SystemExit(subprocess.call(['/usr/bin/curl', *args]))
    status, body, headers = 200, b'', {}
    if '/releases/download/' in url:
        name = url.rsplit('/', 1)[-1]
        if name.endswith('.missing-control'): status = 404
        else:
            asset = Path(config['assets']) / name
            if asset.is_file(): body = asset.read_bytes()
            else: status = 404
    elif url.endswith('/v2/'):
        status = 401 if ':5002/' in url else 200
    elif ':5002/v2/' in url:
        password = option('--user').split(':', 1)[1]
        status = 401 if password == 'wrong' else 404 if url.endswith('/absent') else 200
        body = json.dumps({'errors': [{'code': 'MANIFEST_UNKNOWN'}]}).encode() if status == 404 else b'{}'
    elif url.endswith('/user'):
        body = b'{"login":"gregwebs"}'; headers['x-oauth-scopes'] = 'read:packages'
    elif '/packages?' in url:
        body = b'[{"name":"agent-vm-standard","package_type":"container","visibility":"public"}]'
    elif '/packages/container/' in url:
        body = b'{"name":"agent-vm-standard","package_type":"container","visibility":"public"}'
    elif url.startswith('https://ghcr.io/token'):
        body = b'{"token":"nonsecret-fixture"}'
    elif '/manifests/' in url:
        ref = url.rsplit('/', 1)[1]
        if ref == 'v0.1.0' and not state.get('index_created'):
            status = 404; body = b'{"errors":[{"code":"MANIFEST_UNKNOWN"}]}'
        else:
            arch = ref.rsplit('-', 1)[-1]
            layout = Path(config['layouts'][arch]); body = (layout / 'blobs/sha256' / graph(layout)['digest'][7:]).read_bytes()
            headers['docker-content-digest'] = 'sha256:' + hashlib.sha256(body).hexdigest()
    elif '/releases?' in url:
        body = b'[]'
    elif '/releases/tags/' in url or '/git/ref/tags/' in url:
        status = 404; body = b'{"message":"Not Found"}'
    else:
        body = b'{"full_name":"gregwebs/agent-vm-images"}'
    if '--dump-header' in args:
        Path(option('--dump-header')).write_text('HTTP/1.1 ' + str(status) + '\n' +
            ''.join(key + ': ' + value + '\n' for key, value in headers.items()) + '\n')
    if '--fail' in args and status >= 400: fail('HTTP ' + str(status))
    output = option('--output')
    if output and output != '/dev/null': Path(output).write_bytes(body)
    elif not output: emit(body)
    if '--write-out' in args: print(status, end='')

elif tool == 'msb':
    if args == ['--version']: print('fake msb interface version')
    elif args[0] in ('doctor', 'ping', 'stop'): print('fake runtime response, not a native VM')
    elif args[:2] == ['image', 'load'] or args[:2] == ['image', 'pull']:
        home = Path(os.environ['MSB_HOME']); images_path = home / 'fake-images.json'
        images = json.loads(images_path.read_text()) if images_path.exists() else {}
        ref = option('--tag') if args[1] == 'load' else args[-1]
        if config.get('failure') == 'import' and ref == 'av264-standard:archive': fail('injected late archive import failure')
        try:
            if args[1] == 'load':
                layout = home / 'fake-load-layout'
                layout.mkdir(exist_ok=True)
                with tarfile.open(option('--input')) as archive:
                    for member in archive:
                        if member.isdir(): continue
                        if not member.isreg() or not (member.name in ('index.json', 'oci-layout') or re.fullmatch(r'blobs/sha256/[0-9a-f]{64}', member.name)):
                            raise ValueError('unsafe fixture archive')
                        target = layout / member.name; target.parent.mkdir(parents=True, exist_ok=True)
                        target.write_bytes(archive.extractfile(member).read())
            else: layout = source_layout(ref)
            images[ref] = graph(layout, config['arch'])
        except (ValueError, OSError, tarfile.TarError, KeyError) as error: fail(str(error))
        images_path.write_text(json.dumps(images))
    elif args[:2] == ['image', 'inspect']:
        path = Path(os.environ['MSB_HOME']) / 'fake-images.json'
        images = json.loads(path.read_text()) if path.exists() else {}
        if args[-1] not in images: fail('image not registered')
        emit(images[args[-1]])
    elif args[0] == 'create':
        state['boots'] = state.get('boots', 0) + 1
    elif args[0] == 'exec':
        print('FAKE guest probe output; interface sequencing only, no actual guest boot')
    elif args[0] == 'remove':
        state['removes'] = state.get('removes', 0) + 1
        if config.get('failure') == 'evidence-write' and state['removes'] == 7:
            (Path(state['verify_root']) / ('verification-' + config['arch'] + '.json')).mkdir()
    else: fail('unsupported fake msb operation')

elif tool == 'bash':
    if Path(args[0]).name == 'standard-image.sh':
        if '--artifact' not in args: fail('source audit/build requested in verifier')
        print('FAKE Docker standard artifact oracle output; not image-health acceptance')
    else: raise SystemExit(subprocess.call(['/bin/bash', *args]))
elif tool == 'htpasswd':
    print('fixture:fake-test-only-hash')
elif tool in ('otool', 'ldd'):
    print('fake embedded runtime linkage fixture')
elif tool in ('buildx', 'syft'):
    print('fake tool interface version')
else:
    fail('unknown fake executable ' + tool)
save()
