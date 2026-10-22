#!/usr/bin/env python3
"""Hermetic metadata/manager/Docker/publication fakes for dsh maintenance."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def log(kind, value):
    with open(os.environ['DSH_TEST_LOG'], 'a') as handle:
        handle.write(json.dumps({'kind': kind, 'value': value}) + '\n')


def manager(args):
    log('pnpm', {'args': args, 'cwd': os.getcwd(), 'env': {key: value for key, value in os.environ.items() if key.startswith(('PNPM_CONFIG_', 'XDG_')) or key in ('HOME', 'TMPDIR')}})
    if '--version' in args:
        print('12.0.0' if os.environ.get('DSH_TEST_MODE') == 'manager12' else '11.11.0')
        return 0
    assert args[:3] == ['install', '--lockfile-only', '--no-frozen-lockfile'], args
    required = ['--config.minimum-release-age=1440', '--config.minimum-release-age-strict=true',
                '--config.minimum-release-age-ignore-missing-time=false', '--config.trust-policy=no-downgrade',
                '--ignore-scripts', '--ignore-pnpmfile', '--pm-on-fail=error', '--config.update-notifier=false',
                '--registry=https://registry.npmjs.org/', '--package-import-method=copy', '--store-dir']
    assert all(flag in args for flag in required), args
    assert os.environ['PNPM_CONFIG_MINIMUM_RELEASE_AGE'] == '1440'
    assert os.environ['PNPM_CONFIG_TRUST_POLICY'] == 'no-downgrade'
    graph = json.loads(Path(os.environ['DSH_TEST_GRAPH']).read_text())
    version = json.loads(Path('package.json').read_text())['dependencies']['@deepseek-ai/dsh']
    old = graph['importers']['.']['dependencies']['@deepseek-ai/dsh']['specifier']
    graph['importers']['.']['dependencies']['@deepseek-ai/dsh'] = {'specifier': version, 'version': version}
    for field in ['packages', 'snapshots']:
        graph[field]['@deepseek-ai/dsh@' + version] = graph[field].pop('@deepseek-ai/dsh@' + old)
    workspace = json.loads(subprocess.check_output(['node', str(Path(os.environ['DSH_TEST_REPO']) / 'script/test/fixtures/dsh/read-workspace.mjs')], text=True))
    tentative = 'overrides' not in workspace
    if tentative:
        assert not Path('pnpm-lock.yaml').exists(), 'tentative graph seeded with old overrides'
        graph.pop('overrides', None)
    else:
        graph['overrides'] = workspace['overrides']
    mode = os.environ.get('DSH_TEST_MODE')
    if mode in ['changed-map', 'final-drift']:
        for field in ['packages', 'snapshots']:
            graph[field]['@deepseek-ai/cordis-plugin-timer@1.1.5'] = graph[field].pop('@deepseek-ai/cordis-plugin-timer@1.1.4')
        graph['snapshots']['@deepseek-ai/dsh@' + version]['dependencies']['@deepseek-ai/cordis-plugin-timer'] = '1.1.5'
        graph['snapshots']['declarations@1.0.0']['dependencies']['@deepseek-ai/cordis-plugin-timer'] = '1.1.5'
        if mode == 'final-drift' and not tentative:
            graph['packages']['declarations@1.0.0']['peerDependencies']['@deepseek-ai/cordis-plugin-timer'] = '^1.1.5'
    if mode == 'conflict':
        graph['packages']['declarations@1.0.0']['peerDependencies']['@deepseek-ai/cordis'] = '4.0.3'
    if mode == 'repin':
        # A root range selects a newer timer beside the 1.1.4 the reviewed map
        # pins. Both satisfy every declaration, so only the reviewed value can
        # decide: this is the legitimate no-op re-pin of an unchanged map.
        family = '@deepseek-ai/cordis-plugin-timer'
        graph['packages'][family + '@1.1.5'] = graph['packages'][family + '@1.1.4']
        graph['snapshots'][family + '@1.1.5'] = {}
        graph['snapshots']['@deepseek-ai/dsh@' + version]['dependencies'][family] = '1.1.5'
    if mode == 'ambiguous':
        # Two selections that satisfy every declaration while the reviewed 1.1.4 is
        # not one of them: no graph fact and no reviewed value can decide, so
        # derivation must stop instead of choosing a version.
        family = '@deepseek-ai/cordis-plugin-timer'
        for field in ['packages', 'snapshots']:
            graph[field][family + '@1.1.5'] = graph[field].pop(family + '@1.1.4')
        graph['packages'][family + '@1.1.6'] = graph['packages'][family + '@1.1.5']
        graph['snapshots'][family + '@1.1.6'] = {}
        for snapshot in graph['snapshots'].values():
            if snapshot.get('dependencies', {}).get(family) == '1.1.4':
                snapshot['dependencies'][family] = '1.1.5'
        graph['snapshots']['@deepseek-ai/dsh@' + version]['dependencies'][family] = '1.1.6'
    if not tentative:
        if mode == 'integrity': del graph['packages']['leaf@1.0.0']['resolution']['integrity']
        if mode == 'sandbox':
            del graph['packages']['@deepseek-ai/dsh-sandbox-local@1.0.0']
            del graph['snapshots']['@deepseek-ai/dsh-sandbox-local@1.0.0']
            del graph['snapshots']['@deepseek-ai/dsh@' + version]['dependencies']['@deepseek-ai/dsh-sandbox-local']
        if mode == 'effective-peer':
            graph['snapshots']['declarations@1.0.0']['dependencies']['@deepseek-ai/cordis'] = '4.0.3'
    Path('pnpm-lock.yaml').write_text(json.dumps(graph, indent=2) + '\n')
    return 0


def main():
    name = Path(sys.argv[0]).name
    args = sys.argv[1:]
    if name == 'pnpm-fixture': return manager(args)
    if name == 'npm':
        log('npm', args)
        assert args[0] == 'view', 'dsh npm must be view-only'
        assert os.environ['npm_config_registry'] == 'https://registry.npmjs.org/'
        assert json.loads(Path('package.json').read_text()) == {'private': True}
        assert Path(os.environ['npm_config_userconfig']).read_bytes() == b''
        assert Path(os.environ['npm_config_globalconfig']).read_bytes() == b''
        if os.environ.get('DSH_TEST_MODE') == 'metadata-failure':
            print('fixture registry unavailable', file=sys.stderr)
            return 1
        if args[2:5] == ['dist-tags', 'versions', '--json']:
            print(json.dumps({'dist-tags': {'latest': '0.1.5-rc.2', 'next': '0.1.5-rc.1'}, 'versions': ['0.1.5-rc.1', '0.1.5-rc.2']}))
        else:
            assert args[2:] == ['version', 'dist', 'peerDependencies', '--json'], args
            print(json.dumps({'version': args[1].rsplit('@', 1)[1], 'dist': {'integrity': 'sha512-fixture'}, 'peerDependencies': {}}))
        return 0
    if name == 'cp':
        source, destination = args[-2:]
        log('cp', [source, destination])
        targets = json.loads(os.environ['DSH_TEST_TARGETS'])
        rollback = '/publish-backup/' in source
        if destination in targets:
            index = targets.index(destination)
            if rollback and str(index) == os.environ.get('DSH_TEST_ROLLBACK'):
                return 9
            if not rollback and str(index) == os.environ.get('DSH_TEST_PUBLISH'):
                Path(destination).write_text('partial')
                return 8
        if '/publish-backup/' in destination and os.environ.get('DSH_TEST_MODE') == 'backup-failure': return 8
        backup_index = os.environ.get('DSH_TEST_SLEEP_BACKUP', '')
        if backup_index and '/publish-backup/' in destination and Path(destination).name.endswith(backup_index):
            # Emulate a `cp -p` killed mid-write: the destination it was given holds
            # only a partial prefix and the copy never completes. With an atomic
            # backup the incomplete bytes sit under the temporary name, so no
            # restore can read them.
            Path(destination).write_bytes(Path(source).read_bytes()[:8])
            time.sleep(float(os.environ.get('DSH_TEST_SLEEP_SECONDS', '2')))
            return 8
        sleep_index = os.environ.get('DSH_TEST_SLEEP_PUBLISH', '')
        if not rollback and sleep_index and destination in targets and sleep_index == str(index):
            # Widen the publication window so the caller can deliver a real signal
            # between two of the five copies.
            time.sleep(float(os.environ.get('DSH_TEST_SLEEP_SECONDS', '2')))
        return subprocess.call(['/bin/cp', *args])
    if name == 'go':
        # The consumer harness records the mounted helper's compiler; the fixture
        # context is not a real Go binary, so `go version BINARY` is fixture data.
        log('go', args)
        if args[:1] != ['version'] or len(args) != 2:
            raise ValueError('the fixture harness must not build: ' + repr(args))
        print(args[-1] + ': go1.27.3')
        return 0
    if name == 'git':
        if 'status' in args: return 0
        if 'rev-parse' in args:
            print('f' * 40)
            return 0
        return 1
    spec = importlib.util.spec_from_file_location('consumer_fake', Path(os.environ['DSH_TEST_REPO']) / 'script/test/standard-node-consumers-self-test.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    if name == 'docker':
        log('docker', args)
        return module.fake_docker(args)
    if name == 'python3':
        if args and args[0].endswith('/standard-node-layout.py'):
            log('layout', args)
        return module.fake_python(args)
    raise ValueError('unexpected fake tool ' + name)


if __name__ == '__main__':
    sys.exit(main())
