#!/usr/bin/env python3
"""Hermetic Docker/registry/provenance/publication fakes for the Chrome MCP updater.

The updater's contract is that the lock it publishes, the tarball it downloads
and the attestation statement it verifies all name one artifact. That is why
`tarball` and `lock_text` live here: the fixture's committed state and the fake
`curl` serve the same synthetic bytes, so a lock written by the fake Docker is
exactly the lock the build's `check` accepts, and the fake `gh` builds its
statement subject from the tarball the updater actually fetched.

The fakes assert the interface instead of imitating it: an argv the trusted
identity module does not name, a non-empty `GH_CONFIG_DIR`, a leaked token, or a
resolution context that already carries a lock all fail the fake. A fake that
fails this way raises, and the test refuses any run whose output holds a
traceback, so a broken fake can never pass for a broken updater.
"""
import base64
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
from pathlib import Path

# `jq -ce .bundle` reproduces this compact form byte for byte, so the committed
# bundle can be compared with the published one.
BUNDLE = {'mediaType': 'application/vnd.dev.sigstore.bundle.v0.3+json',
          'verificationMaterial': {'fixture': 'chrome-mcp-upgrade'},
          'dsseEnvelope': {'payload': 'fixture'}}
NEXT = '9.9.9'
# The committed lock's template, with the two values a re-resolution changes.
LOCK_TEMPLATE = """lockfileVersion: '9.0'

settings:
  autoInstallPeers: false
  excludeLinksFromLockfile: false

importers:

  .:
    dependencies:
      chrome-devtools-mcp:
        specifier: {version}
        version: {version}

packages:

  chrome-devtools-mcp@{version}:
    resolution: {{integrity: sha512-{integrity}}}
    engines: {{node: ^20.19.0 || ^22.12.0 || >=23}}
    hasBin: true

snapshots:

  chrome-devtools-mcp@{version}: {{}}
"""
TOKEN_VARS = ['GH_TOKEN', 'GITHUB_TOKEN', 'GH_ENTERPRISE_TOKEN', 'GITHUB_ENTERPRISE_TOKEN', 'GH_HOST']


def tarball(version, mode='ok'):
    if mode == 'bad':
        return b'fixture tarball chrome-devtools-mcp@' + version.encode() + b' (not the published bytes)\n'
    return b'fixture tarball chrome-devtools-mcp@' + version.encode() + b'\n'


def integrity(version):
    return base64.b64encode(hashlib.sha512(tarball(version)).digest()).decode()


def lock_text(version, integrity_value=None, extra=None):
    """A lock `check_closure` accepts; `extra` adds a second package to break it."""
    text = LOCK_TEMPLATE.format(version=version, integrity=integrity_value or integrity(version))
    if extra:
        text = text.replace('packages:\n\n', 'packages:\n\n  %s@1.0.0:\n    resolution: {integrity: sha512-Zml4dHVyZQ==}\n' % extra)
        text = text.replace('snapshots:\n\n', 'snapshots:\n\n  %s@1.0.0: {}\n' % extra)
    return text


def bundle_text():
    return json.dumps(BUNDLE, separators=(',', ':')) + '\n'


def attestations(mode):
    """The npm attestations document the trusted identity is verified against."""
    if mode == 'none':
        return None
    slsa = {'predicateType': 'https://slsa.dev/provenance/v1', 'bundle': BUNDLE}
    entries = {'one': [slsa],
               'two': [slsa, slsa],
               'zero': [{'predicateType': 'https://spdx.dev/Document', 'bundle': BUNDLE}]}[mode]
    return json.dumps({'attestations': entries})


def identity():
    """The trusted identity the updater must pass, read from its single source."""
    text = (Path(os.environ['AVM_CHROME_ROOT']) / 'script/build/chrome-mcp-provenance.sh').read_text()
    return dict(re.findall(r'^(CHROME_MCP_[A-Z_]+)=(.*)$', text, re.M))


def log(kind, value):
    with open(os.environ['AVM_CHROME_LOG'], 'a') as handle:
        handle.write(json.dumps({'kind': kind, 'value': value}) + '\n')


def state():
    path = Path(os.environ['AVM_CHROME_STATE'])
    if path.exists() and path.read_text().strip():
        return json.loads(path.read_text())
    return {'containers': {}}


def save(document):
    Path(os.environ['AVM_CHROME_STATE']).write_text(json.dumps(document))


def container(cid):
    return state()['containers'][cid]


def container_root(cid):
    return Path(container(cid)['root'])


def docker(args):
    log('docker', args)
    document = state()
    containers = document['containers']
    if args[:1] == ['version']:
        assert args == ['version', '-f', '{{.Server.Arch}}'], args
        print('arm64')
        return 0
    if args[:1] == ['create']:
        assert args[1] == '--platform' and args[2] in ('linux/arm64', 'linux/amd64'), args
        assert re.fullmatch(r'ghcr\.io/gregwebs/agent-vm-standard@sha256:[0-9a-f]{64}', args[3]), args
        assert args[4:] == ['sh', '/work/chrome-devtools-mcp.sh', 'resolve', '/work/project'], args
        cid = 'fixture%02d' % (len(containers) + 1)
        root = Path(os.environ['AVM_CHROME_CONTAINERS']) / cid
        root.mkdir(parents=True)
        containers[cid] = {'live': True, 'exit': None, 'root': str(root)}
        save(document)
        print(cid)
        return 0
    # Every later verb names the one container the updater created.
    cid = next(iter(containers), '')
    assert cid in containers, 'no container was created before ' + repr(args)
    if args[:1] == ['cp']:
        source, destination = args[1], args[2]
        root = container_root(cid)
        if ':' in source:
            # Only the resolved lock may leave the container.
            assert source == cid + ':/work/project/pnpm-lock.yaml', args
            inner = root / source.split(':', 1)[1].lstrip('/')
            assert inner.is_file(), 'the container never resolved a lock: ' + str(inner)
            Path(destination).write_bytes(inner.read_bytes())
            return 0
        assert destination.startswith(cid + ':'), args
        assert not (root / 'work').exists(), 'the container context is not fresh: ' + destination
        shutil.copytree(source, root / destination.split(':', 1)[1].lstrip('/'))
        staged = root / 'work/project'
        assert sorted(path.name for path in staged.iterdir()) == ['package.json', 'pnpm-workspace.yaml'], \
            'the container must resolve a lock-free proposal'
        return 0
    if args[:2] == ['start', '-a']:
        mode = os.environ.get('AVM_CHROME_START', 'ok')
        staged = container_root(cid) / 'work/project'
        if mode == 'sleep':
            print('fixture resolve is sleeping', flush=True)
            time.sleep(float(os.environ.get('AVM_CHROME_SLEEP_SECONDS', '2')))
        elif mode in ('exit1', 'exit124'):
            print('fixture resolve failed', flush=True)
            containers[cid]['exit'] = int(mode[len('exit'):])
            save(document)
            return containers[cid]['exit']
        else:
            selection = json.loads((staged / 'package.json').read_text())['dependencies']['chrome-devtools-mcp']
            lock = os.environ.get('AVM_CHROME_LOCK', 'next')
            if lock == 'same':
                text = lock_text(selection)
            elif lock == 'two-packages':
                text = lock_text(NEXT, extra='left-pad')
            else:
                text = lock_text(NEXT)
            (staged / 'pnpm-lock.yaml').write_text(text)
            print('chrome-devtools-mcp@%s: lock resolved' % selection, flush=True)
        containers[cid]['exit'] = 0
        save(document)
        return 0
    if args[:1] == ['inspect']:
        assert args[1:] == ['-f', '{{.State.ExitCode}}', cid], args
        recorded = container(cid)['exit']
        print(0 if recorded is None else recorded)
        return 0
    if args[:2] == ['rm', '-f']:
        containers[cid]['live'] = False
        save(document)
        return 0
    raise ValueError('unexpected fake docker argv: ' + repr(args))


def curl(args):
    log('curl', args)
    registry = identity()['CHROME_MCP_REGISTRY']
    assert args[:2] == ['-fsS', '--max-time'], args
    output = args[args.index('-o') + 1] if '-o' in args else None
    url = args[-1]
    if url == registry + '/chrome-devtools-mcp':
        print(json.dumps({'name': 'chrome-devtools-mcp', 'dist-tags': {'latest': NEXT, 'beta': '1.0.1'}}))
        return 0
    tarball_match = re.fullmatch(re.escape(registry) + r'/chrome-devtools-mcp/-/chrome-devtools-mcp-(.+)\.tgz', url)
    if tarball_match:
        assert output, args
        Path(output).write_bytes(tarball(tarball_match.group(1), os.environ.get('AVM_CHROME_TARBALL', 'ok')))
        return 0
    attestation_match = re.fullmatch(
        re.escape(registry) + r'/-/npm/v1/attestations/chrome-devtools-mcp@(.+)', url)
    assert attestation_match, url
    document = attestations(os.environ.get('AVM_CHROME_ATTESTATIONS', 'one'))
    if document is None:
        print('404 Not Found', file=sys.stderr)
        return 22
    print(document)
    return 0


def gh(args):
    log('gh', args)
    expected = identity()
    assert args[:2] == ['attestation', 'verify'], args
    tarball_path, rest = args[2], args[3:]
    bundle_path = rest[rest.index('--bundle') + 1]
    version = os.environ['AVM_CHROME_VERSION']
    assert rest == ['--bundle', bundle_path, '--digest-alg', 'sha512',
                    '--repo', expected['CHROME_MCP_TRUSTED_REPO'],
                    '--signer-workflow', expected['CHROME_MCP_TRUSTED_WORKFLOW'],
                    '--source-ref', 'refs/tags/chrome-devtools-mcp-v' + version,
                    '--cert-oidc-issuer', expected['CHROME_MCP_TRUSTED_ISSUER'],
                    '--deny-self-hosted-runners', '--format', 'json'], rest
    config = Path(os.environ['GH_CONFIG_DIR'])
    assert config.is_dir(), 'GH_CONFIG_DIR must be a directory'
    assert not list(config.iterdir()), 'GH_CONFIG_DIR must be empty: ' + str(list(config.iterdir()))
    assert Path(os.environ['AVM_CHROME_WORK_ROOT']).resolve() in config.resolve().parents, config
    leaked = [name for name in TOKEN_VARS if name in os.environ]
    assert not leaked, 'a credential leaked into the verification: ' + ' '.join(leaked)
    assert os.environ['GH_PROMPT_DISABLED'] == '1', os.environ['GH_CONFIG_DIR']
    assert os.environ['GH_NO_UPDATE_NOTIFIER'] == '1', os.environ['GH_CONFIG_DIR']
    assert Path(bundle_path).read_text() == bundle_text(), 'the verified bundle is not the fetched one'
    digest = hashlib.sha512(Path(tarball_path).read_bytes()).hexdigest()
    log('gh-verified', {'subject': digest})
    mode = os.environ.get('AVM_CHROME_GH', 'ok')
    if mode == 'fail':
        print('Error: verifying bundle: tlog entry has no matching log entry', file=sys.stderr)
        return 1
    if mode == 'wrong-subject':
        digest = hashlib.sha512(b'not the artifact').hexdigest()
    print(json.dumps([{'verificationResult': {'statement': {
        'predicateType': expected['CHROME_MCP_SLSA'],
        'subject': [{'name': 'pkg:npm/chrome-devtools-mcp@' + version, 'digest': {'sha512': digest}}]}}}]))
    return 0


def cp(args):
    operands = [argument for argument in args if not argument.startswith('-')]
    assert len(operands) == 2, args
    source, destination = operands
    log('cp', [source, destination])
    resolved = Path(destination) / Path(source).name if Path(destination).is_dir() else Path(destination)
    targets = json.loads(os.environ['AVM_CHROME_TARGETS'])
    index = str(targets.index(str(resolved))) if str(resolved) in targets else ''
    if '/publish-backup/' in source:
        if Path(source).name == os.environ.get('AVM_CHROME_ROLLBACK', ''):
            return 9
        return subprocess.call(['/bin/cp', *args])
    if resolved.parent.name == 'publish-backup':
        if resolved.name == '.tmp' + os.environ.get('AVM_CHROME_SLEEP_BACKUP', ''):
            # A `cp -p` killed mid-write leaves a partial prefix under the name it
            # was given and never completes; publish_transactional only gives a
            # backup its numeric name after `mv`, so no restore can read these
            # bytes.
            Path(resolved).write_bytes(Path(source).read_bytes()[:8])
            time.sleep(float(os.environ.get('AVM_CHROME_SLEEP_SECONDS', '2')))
            return 8
        return subprocess.call(['/bin/cp', *args])
    if index and index == os.environ.get('AVM_CHROME_SLEEP_PUBLISH', ''):
        # Widen the publication window so the caller can deliver a real signal
        # between two of the five copies.
        time.sleep(float(os.environ.get('AVM_CHROME_SLEEP_SECONDS', '2')))
    if index and index == os.environ.get('AVM_CHROME_PUBLISH', ''):
        Path(resolved).write_text('partial')
        return 8
    return subprocess.call(['/bin/cp', *args])


def main():
    name = Path(sys.argv[0]).name
    args = sys.argv[1:]
    if name == 'docker':
        return docker(args)
    if name == 'curl':
        return curl(args)
    if name == 'gh':
        return gh(args)
    if name == 'cp':
        return cp(args)
    raise ValueError('unexpected fake tool ' + name)


if __name__ == '__main__':
    sys.exit(main())
