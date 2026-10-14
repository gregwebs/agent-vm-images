#!/usr/bin/env python3
"""Real pinned commands against an isolated registry; failures never become skips."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
from urllib.request import urlopen

sys.path.insert(0, str(Path(__file__).parent / 'fixtures/pnpm'))
FIXTURES = Path(__file__).parent / 'fixtures/pnpm'
from registry import Registry


def main() -> None:
    assert subprocess.check_output(['pnpm', '--version'], text=True).strip() == '11.11.0'
    launcher = Path('/usr/local/bin/pnpm')
    assert launcher.is_file() and not launcher.is_symlink()
    assert launcher.read_bytes() == Path('/source/images/pnpm/launcher.sh').read_bytes()
    for alias in ('pnx', 'pnpx'):
        assert os.readlink('/usr/local/bin/' + alias) == 'pnpm'
    with tempfile.TemporaryDirectory(prefix='pnpm-runtime-') as directory, Registry() as registry:
        work = Path(directory)
        # A registry startup/transport failure is not policy evidence.
        with urlopen(registry.url + '/fixture-package', timeout=5) as response:
            assert response.status == 200
        index = 0

        def scenario() -> tuple[Path, dict[str, str]]:
            nonlocal index
            index += 1
            cwd = work / str(index)
            cwd.mkdir()
            (cwd / 'package.json').write_text('{"name":"test-project","version":"1.0.0"}')
            home = (Path(os.environ['HOME']) / str(index)) if os.environ.get('PNPM_TEST_OVERMOUNT') else cwd / 'home'
            home.mkdir()
            env = {k: v for k, v in os.environ.items() if not k.startswith('PNPM_CONFIG_') and not k.startswith('XDG_')}
            env.update(HOME=str(home), PNPM_CONFIG_REGISTRY=registry.url,
                       MARKERS=str(cwd / 'markers'), CI='true', PNPM_CONFIG_FETCH_RETRIES='0')
            return cwd, env

        def run(args: list[str], cwd: Path, env: dict[str, str], error: str | None = None,
                command: str = 'pnpm') -> str:
            result = subprocess.run([command, *args], cwd=cwd, env=env, capture_output=True,
                                    text=True, timeout=90)
            output = result.stdout + result.stderr
            if error:
                assert result.returncode != 0 and error in output, (args, result.returncode, output)
                print(f'rejected {args}: status={result.returncode}, evidence={error}\\n{output}', flush=True)
            else:
                assert result.returncode == 0, (args, result.returncode, output)
            return output

        cwd, env = scenario()
        for key, value in [('minimum-release-age', '1440'), ('minimum-release-age-strict', 'true'),
                           ('minimum-release-age-ignore-missing-time', 'false'),
                           ('trust-policy', 'no-downgrade'), ('ignore-scripts', 'true')]:
            assert run(['config', 'get', key], cwd, env).strip() == value
        # npm-style env keys are not an override interface; they must not relax
        # the five image defaults that the launcher exports.
        default_by_config = {'minimum-release-age': '1440', 'minimum-release-age-strict': 'true',
                             'minimum-release-age-ignore-missing-time': 'false',
                             'trust-policy': 'no-downgrade', 'ignore-scripts': 'true'}
        relaxed = {'minimum-release-age': '0', 'minimum-release-age-strict': 'false',
                   'minimum-release-age-ignore-missing-time': 'true', 'trust-policy': 'off',
                   'ignore-scripts': 'false'}
        for config, relaxing in relaxed.items():
            upper = 'NPM_CONFIG_' + config.upper().replace('-', '_')
            lower = 'npm_config_' + config.replace('-', '_')
            assert run(['config', 'get', config], cwd, env | {upper: relaxing, lower: relaxing}).strip() == \
                default_by_config[config], config
        print('npm-style env keys cannot relax the five image defaults', flush=True)
        run(['add', 'fixture-package@*'], cwd, env)
        assert json.loads((cwd / 'node_modules/fixture-package/package.json').read_text())['version'] == '1.0.0'
        assert not (cwd / 'markers').exists()
        for name, version, error in [('fixture-package', '1.1.0', 'ERR_PNPM_NO_MATURE_MATCHING_VERSION'),
                                     ('fixture-missing', '1.0.0', 'ERR_PNPM_MISSING_TIME'),
                                     ('fixture-trust', '1.1.0', 'ERR_PNPM_TRUST_DOWNGRADE'),
                                     ('fixture-corrupt', '1.0.0', 'ERR_PNPM_TARBALL_INTEGRITY')]:
            cwd, env = scenario()
            run(['add', name + '@' + version], cwd, env, error)
            assert not (cwd / 'markers').exists()
        for command, args in [('pnpm', ['dlx', 'fixture-hooks@1.0.0']),
                               ('pnx', ['fixture-hooks@1.0.0']), ('pnpx', ['fixture-hooks@1.0.0'])]:
            cwd, env = scenario()
            output = run(args + ['', 'space value', '--bin-flag'], cwd, env, command=command)
            assert 'BIN:fixture-hooks:1.0.0' in output
            assert '"ignoreScripts":"true"' in output
            observation = next(json.loads(line) for line in output.splitlines() if line.startswith('{'))
            assert observation['argv'] == ['', 'space value', '--bin-flag']
            for key, value in [('MINIMUM_RELEASE_AGE', '1440'), ('MINIMUM_RELEASE_AGE_STRICT', 'true'),
                               ('MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME', 'false'),
                               ('TRUST_POLICY', 'no-downgrade'), ('IGNORE_SCRIPTS', 'true')]:
                assert observation['policy']['PNPM_CONFIG_' + key] == value
            assert not (cwd / 'markers').exists()
            print(f'{command}: bin executed, cold entry/dependency hooks absent')
        cwd, env = scenario()
        env['PNPM_HOME'] = str(cwd / 'global')
        Path(env['PNPM_HOME']).mkdir()
        (Path(env['PNPM_HOME']) / 'bin').mkdir()
        env['PATH'] = str(Path(env['PNPM_HOME']) / 'bin') + ':' + env['PATH']
        globalbin = run(['bin', '-g'], cwd, env).strip()
        assert globalbin == str(Path(env['PNPM_HOME']) / 'bin'), globalbin
        run(['add', '-g', 'fixture-hooks@1.0.0'], cwd, env)
        assert not (cwd / 'markers').exists()
        output = subprocess.check_output([str(Path(globalbin) / 'fixture-hooks')], env=env, text=True)
        assert 'BIN:fixture-hooks:1.0.0' in output
        assert all(path.stat().st_uid == os.getuid() for path in work.rglob('*') if not path.is_symlink())
        print('global bin executed; writable native state owned by invoking UID')
        defaults = [('minimum-release-age', '1440'), ('minimum-release-age-strict', 'true'),
                    ('minimum-release-age-ignore-missing-time', 'false'),
                    ('trust-policy', 'no-downgrade'), ('ignore-scripts', 'true')]

        def effective(cwd: Path, env: dict[str, str]) -> None:
            for key, value in defaults:
                assert run(['config', 'get', key], cwd, env).strip() == value

        # Opt-out setup and cold policy verification are deliberately separate invocations.
        for name, error, bypass in [('fixture-package', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION', 'trustLockfile: true'),
                                    ('fixture-trust', 'ERR_PNPM_TRUST_DOWNGRADE', 'trustPolicyExclude: [fixture-trust]')]:
            cwd, env = scenario()
            run(['add', name + '@1.1.0', '--lockfile-only'], cwd,
                env | {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0', 'PNPM_CONFIG_TRUST_POLICY': 'off'})
            lock = (cwd / 'pnpm-lock.yaml').read_bytes()
            cold = cwd / 'cold-home'
            cold.mkdir()
            env['HOME'] = str(cold)
            run(['install', '--frozen-lockfile'], cwd, env, error)
            assert (cwd / 'pnpm-lock.yaml').read_bytes() == lock
            assert not (cwd / 'markers').exists()
            import shutil
            if (cwd / 'node_modules').exists():
                shutil.rmtree(cwd / 'node_modules')
            bypass_home = cwd / 'bypass-home'
            bypass_home.mkdir()
            env['HOME'] = str(bypass_home)
            (cwd / 'pnpm-workspace.yaml').write_text(bypass + '\n')
            effective(cwd, env)
            run(['install', '--frozen-lockfile'], cwd, env)
        for name, setting in [('fixture-package@1.1.0', 'minimumReleaseAgeExclude: [fixture-package]'),
                               ('fixture-trust@1.1.0', 'trustPolicyExclude: [fixture-trust]')]:
            cwd, env = scenario()
            (cwd / 'pnpm-workspace.yaml').write_text(setting + '\n')
            effective(cwd, env)
            run(['add', name], cwd, env)
            dlx_cwd, dlx_env = scenario()
            (dlx_cwd / 'pnpm-workspace.yaml').write_text(setting + '\n')
            effective(dlx_cwd, dlx_env)
            assert 'BIN:' in run(['dlx', name], dlx_cwd, dlx_env)
        cwd, env = scenario()
        run(['add', 'fixture-partial-time@1.1.0'], cwd, env, 'ERR_PNPM_TRUST_CHECK_FAIL')
        cwd, env = scenario()
        run(['add', '@private/missing@1.0.0'], cwd, env, 'ERR_PNPM_MISSING_TIME')
        cwd, env = scenario()
        run(['add', '@private/missing@1.0.0'], cwd,
            env | {'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true'}, 'ERR_PNPM_MISSING_TIME')
        for overrides in [
                {'PNPM_CONFIG_MINIMUM_RELEASE_AGE_EXCLUDE': '["@private/*"]',
                 'PNPM_CONFIG_TRUST_POLICY_EXCLUDE': '["@private/*"]'},
                {'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true',
                 'PNPM_CONFIG_TRUST_POLICY': 'off'}]:
            cwd, env = scenario()
            run(['add', '@private/missing@1.0.0'], cwd, env | overrides)

        # Cold state per approval/control: a previously built cache is not suppression evidence.
        for kind in ('local', 'global', 'dlx', 'pnx', 'pnpx'):
            for control in ('default', 'allow-only', 'false-only', 'approved', 'cli'):
                cwd, env = scenario()
                approved = control in ('allow-only', 'approved', 'cli')
                if control in ('false-only', 'approved'):
                    env['PNPM_CONFIG_IGNORE_SCRIPTS'] = 'false'
                flags = ['--config.ignore-scripts=false'] if control == 'cli' else []
                if kind == 'local':
                    if approved:
                        (cwd / 'pnpm-workspace.yaml').write_text('allowBuilds:\n  fixture-hooks: true\n  fixture-dependency: true\n')
                    args, command = ['add', 'fixture-hooks@1.0.0'], 'pnpm'
                elif kind == 'global':
                    env['PNPM_HOME'] = str(cwd / 'global')
                    (Path(env['PNPM_HOME']) / 'bin').mkdir(parents=True)
                    env['PATH'] = str(Path(env['PNPM_HOME']) / 'bin') + ':' + env['PATH']
                    args, command = ['add', '-g', 'fixture-hooks@1.0.0'], 'pnpm'
                    if approved:
                        flags += ['--allow-build=fixture-hooks', '--allow-build=fixture-dependency']
                else:
                    args = (['dlx'] if kind == 'dlx' else [])
                    if approved:
                        args += ['--allow-build=fixture-dependency']
                    args += ['fixture-hooks@1.0.0']
                    command = 'pnpm' if kind == 'dlx' else kind
                if control == 'cli' and kind in ('dlx', 'pnx', 'pnpx'):
                    env['PNPM_TEST_NESTED'] = '1'
                output = run(flags + args, cwd, env, error='ERR_PNPM_IGNORED_BUILDS' if control == 'false-only' and kind == 'local' else None, command=command)
                markers = (cwd / 'markers').read_text().splitlines() if (cwd / 'markers').exists() else []
                if control in ('approved', 'cli'):
                    assert set(markers) == {'fixture-hooks', 'fixture-dependency'}, (kind, control, output, markers)
                elif control in ('default', 'allow-only'):
                    assert not markers, (kind, control, markers)
                else:
                    assert 'fixture-dependency' not in markers, (kind, control, markers)
                if kind in ('dlx', 'pnx', 'pnpx') and control != 'false-only':
                    assert 'BIN:fixture-hooks:1.0.0' in output
                    if control == 'cli':
                        assert '"ignoreScripts":"true"' in output
                        assert 'NESTED:true' in output
                print(f'{kind} lifecycle {control}: {markers}', flush=True)
        for optin in (False, True):
            cwd, env = scenario()
            (cwd / 'hook.js').write_bytes((FIXTURES / 'project-hook.js').read_bytes())
            (cwd / 'package.json').write_text(json.dumps({'name': 'project', 'version': '1.0.0',
                'scripts': {'postinstall': 'node hook.js'}}))
            run(['install'], cwd, env | ({'PNPM_CONFIG_IGNORE_SCRIPTS': 'false'} if optin else {}))
            assert (cwd / 'markers').exists() == optin
        cwd, env = scenario()
        env['XDG_CONFIG_HOME'] = str(cwd / 'xdg')
        Path(env['XDG_CONFIG_HOME']).mkdir()
        (cwd / 'pnpm-workspace.yaml').write_text('minimumReleaseAge: 10\nignoreScripts: false\n')
        effective(cwd, env)
        assert run(['config', 'get', 'minimum-release-age'], cwd, env | {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '9999'}).strip() == '9999'
        assert run(['--config.minimum-release-age=7', 'config', 'get', 'minimum-release-age'], cwd, env).strip() == '7'
        output = run(['dlx', 'fixture-hooks@1.0.0'], cwd, env)
        assert '"home":' + json.dumps(env['HOME'], separators=(',', ':')) in output
        assert '"xdg":' + json.dumps(env['XDG_CONFIG_HOME'], separators=(',', ':')) in output
        assert not (cwd / 'markers').exists()

        # Config queries are not installation proof: conflicting direct keys are
        # decided by real installs, and only env/CLI overrides succeed.
        from precedence import Install, assert_defaults_win, write_conflicting_project

        def install(args: list[str], overrides: dict[str, str]) -> Install:
            project, project_env = scenario()
            write_conflicting_project(project)
            result = subprocess.run(['pnpm', *args], cwd=project, env=project_env | overrides,
                                    capture_output=True, text=True, timeout=90)
            return Install(project, result.returncode, result.stdout + result.stderr)

        assert_defaults_win(install)
        print('installation-time precedence: conflicting project keys lose, env/CLI win', flush=True)
        cwd, env = scenario()
        result = subprocess.run(['pnpm', 'add', 'fixture-package@1.0.0', '--store-dir=/dev/null/agent-vm-unwritable'],
                                cwd=cwd, env=env, capture_output=True, text=True, timeout=30)
        assert result.returncode != 0, result.stdout
        assert 'EACCES' in result.stdout + result.stderr or 'ENOENT' in result.stdout + result.stderr or 'EROFS' in result.stdout + result.stderr or 'ENOTDIR' in result.stdout + result.stderr

        # The read-only bind is established independently before pinning the
        # store there. pnpm's recursive mkdir reports ENOENT (not EROFS) on this
        # daemon, so assert a hard nonzero without naming the errno.
        readonly = Path('/source')
        options = next((fields[5] for fields in (line.split() for line in
                        Path('/proc/self/mountinfo').read_text().splitlines())
                        if len(fields) > 5 and fields[4] == str(readonly)), None)
        assert options is not None and 'ro' in options.split(','), options
        probe = readonly / 'agent-vm-readonly-probe'
        try:
            probe.mkdir()
        except OSError:
            pass
        else:
            probe.rmdir()
            raise AssertionError('expected ' + str(readonly) + ' to reject new entries')
        store = readonly / 'agent-vm-readonly-store'
        result = subprocess.run(['pnpm', 'add', 'fixture-package@1.0.0', '--store-dir=' + str(store)],
                                cwd=cwd, env=env, capture_output=True, text=True, timeout=60)
        combined = result.stdout + result.stderr
        assert result.returncode != 0, combined
        assert str(store) in combined, combined
        assert any(code in combined for code in ('EROFS', 'ENOENT', 'EACCES', 'ENOTDIR')), combined
        assert not store.exists() and not (cwd / 'node_modules').exists() and not (cwd / 'markers').exists()
        print('read-only store path: hard nonzero, no fallback, no hooks', flush=True)

        # Native npm scripts and npx already-local bins, without public network access.
        cwd, env = scenario()
        import shutil
        npm, npx = Path(shutil.which('npm')).resolve(), Path(shutil.which('npx')).resolve()
        assert str(npm).startswith('/usr/lib/node_modules/npm/') and str(npx).startswith('/usr/lib/node_modules/npm/')
        before = (npm.read_bytes(), npx.read_bytes())
        run(['install', '--registry=' + registry.url, 'fixture-hooks@1.0.0'], cwd, env, command='npm')
        assert {'fixture-hooks', 'fixture-dependency'} <= set((cwd / 'markers').read_text().splitlines())
        assert 'BIN:fixture-hooks:1.0.0' in run(['--no-install', 'fixture-hooks'], cwd, env, command='npx')
        assert before == (npm.read_bytes(), npx.read_bytes())

        assert all(path.stat().st_uid == os.getuid() for path in work.rglob('*') if not path.is_symlink())
        if os.environ.get('PNPM_TEST_OVERMOUNT'):
            assert all(path.stat().st_uid == os.getuid() for path in Path(os.environ['HOME']).rglob('*') if not path.is_symlink())
        if Path('/usr/local/bin/dsh').exists():
            cwd, env = scenario()
            assert run(['plugin', '--profile', 'pnpm-fixture', 'config', 'get', 'ignore-scripts'], cwd, env, command='dsh').splitlines()[0] == 'true'
            run(['plugin', '--profile', 'pnpm-fixture', 'add', 'fixture-package@1.1.0'], cwd, env,
                'ERR_PNPM_NO_MATURE_MATCHING_VERSION', command='dsh')
    print('pnpm runtime matrix passed')


if __name__ == '__main__':
    main()
