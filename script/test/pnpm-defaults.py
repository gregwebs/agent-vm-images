#!/usr/bin/env python3
"""Command-seam probes; substituted exec targets exist only in owned scratch."""
import json
import os
import hashlib
from pathlib import Path
import subprocess
import shutil
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
DEFAULTS = {
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '1440',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'true',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'false',
    'PNPM_CONFIG_TRUST_POLICY': 'no-downgrade',
    'PNPM_CONFIG_IGNORE_SCRIPTS': 'true',
}


def hermetic() -> None:
    with tempfile.TemporaryDirectory(prefix='pnpm-launcher-') as directory:
        work = Path(directory)
        probe = work / 'probe.py'
        shutil.copyfile(ROOT / 'script/test/fixtures/pnpm/exec-probe.py', probe)
        text = (ROOT / 'images/pnpm/launcher.sh').read_text()
        text = text.replace('/usr/bin/node', sys.executable)
        text = text.replace('/opt/agent-vm/pnpm/bin/pnpm.mjs', str(probe) + ' pnpm')
        text = text.replace('/opt/agent-vm/pnpm/bin/pnpx.mjs', str(probe) + ' dlx')
        launcher = work / 'pnpm'
        launcher.write_text(text)
        launcher.chmod(0o755)
        for alias in ('pnx', 'pnpx'):
            (work / alias).symlink_to('pnpm')
        env = {k: v for k, v in os.environ.items() if not k.startswith('PNPM_CONFIG_')}
        env.update(HOME=str(work), XDG_CONFIG_HOME=str(work / 'config'))
        for name in ('pnpm', 'pnx', 'pnpx'):
            for values in ({}, dict.fromkeys(DEFAULTS, ''), {
                    'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0',
                    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'false',
                    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true',
                    'PNPM_CONFIG_TRUST_POLICY': 'off',
                    'PNPM_CONFIG_IGNORE_SCRIPTS': 'false'}):
                child = subprocess.Popen([str(work / name), '', 'space value', '--flag'],
                                         env=env | values | {'TEST_EXIT': '17'},
                                         stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                         stderr=subprocess.PIPE, text=True)
                out, err = child.communicate('stdin-token\n', timeout=10)
                assert child.returncode == 17, (out, err)
                observed = json.loads(out)
                assert observed['pid'] == child.pid, 'launcher must exec, not supervise'
                assert observed['argv'] == [('pnpm' if name == 'pnpm' else 'dlx'), '', 'space value', '--flag']
                assert observed['stdin'] == 'stdin-token\n'
                assert err == 'stderr-token\n'
                for key, default in DEFAULTS.items():
                    assert observed['env'][key] == (values.get(key) or default), key
                for key in ('HOME', 'XDG_CONFIG_HOME', 'PATH'):
                    assert observed['env'][key] == env[key], key
        # The launcher only owns PNPM_CONFIG_*; an invocation under any other
        # name must refuse rather than silently exec the pnpm entrypoint with
        # the image defaults attached to an unexpected command.
        unsupported = work / 'pnpm-other'
        shutil.copyfile(launcher, unsupported)
        unsupported.chmod(0o755)
        result = subprocess.run([str(unsupported)], env=env, capture_output=True, text=True, timeout=10)
        assert result.returncode == 1 and 'unsupported invocation name' in result.stderr, result
        mutations = []
        for key, value in DEFAULTS.items():
            mutations.append(('remove default ' + key, text.replace(': "${' + key + ':=' + value + '}"\n', '')))
            lines = text.splitlines(keepends=True)
            mutations.append(('remove export ' + key, ''.join(
                ((' '.join(word for word in line.split() if word != key) + '\n') if len(line.split()) > 2 else '') if line.startswith('export ') and key in line.split() else line for line in lines)))
        for key, value in [('PNPM_CONFIG_MINIMUM_RELEASE_AGE', '1440'), ('PNPM_CONFIG_IGNORE_SCRIPTS', 'true')]:
            mutations.append(('clobber caller ' + key, text.replace(': "${' + key + ':=' + value + '}"', key + '=' + value)))
        for label, mutation in mutations:
            launcher.write_text(mutation)
            values = {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0', 'PNPM_CONFIG_IGNORE_SCRIPTS': 'false'} if label.startswith('clobber') else {}
            result = subprocess.run([str(launcher)], env=env | values, input='', capture_output=True, text=True, timeout=10)
            assert result.returncode == 0, (label, result.stderr)
            observed = json.loads(result.stdout)['env']
            assert any(observed.get(key) != values.get(key, default) for key, default in DEFAULTS.items()), label
        launcher.write_text(text.replace(str(probe) + ' dlx', str(probe) + ' pnpm'))
        observed = json.loads(subprocess.check_output([str(work / 'pnx')], env=env, text=True))
        assert observed['argv'] != ['dlx'], 'alias-dispatch mutation must be caught'
        launcher.write_text(text)
        signal_probe = work / 'termination_probe.py'
        shutil.copyfile(ROOT / 'script/test/fixtures/pnpm/termination-probe.py', signal_probe)
        launcher.write_text(text.replace(str(probe) + ' pnpm', str(signal_probe)))
        result = subprocess.run([str(launcher)], env=env, timeout=10)
        assert result.returncode == -15, result.returncode
    print('pnpm launcher hermetic controls passed')


def artifact(archive: Path) -> None:
    sys.path.insert(0, str(ROOT / 'images/pnpm'))
    from bootstrap import read_pin, authenticate, extract
    pin = read_pin(ROOT / 'images/pnpm/pin.json')
    authenticate(archive, pin)
    with tempfile.TemporaryDirectory(prefix='pnpm-artifact-') as directory:
        work = Path(directory)
        package = extract(archive, work, pin)
        sys.path.insert(0, str(ROOT / 'script/test/fixtures/pnpm'))
        from precedence import Install, assert_defaults_win, write_conflicting_project
        from registry import Registry
        commands = work / 'commands'
        commands.mkdir()
        node = shutil.which('node')
        assert node
        text = (ROOT / 'images/pnpm/launcher.sh').read_text().replace('/usr/bin/node', node)
        text = text.replace('/opt/agent-vm/pnpm', str(package))
        launcher = commands / 'pnpm'
        launcher.write_text(text)
        launcher.chmod(0o755)
        for alias in ('pnx', 'pnpx'):
            (commands / alias).symlink_to('pnpm')
        home = work / 'home'
        home.mkdir()
        project = work / 'project'
        project.mkdir()
        env = {k: v for k, v in os.environ.items() if not k.startswith('PNPM_CONFIG_') and not k.startswith('XDG_')}
        env.update(HOME=str(home), PATH=str(commands) + ':' + env['PATH'])

        def run(args: list[str], overrides: dict[str, str] | None = None) -> str:
            result = subprocess.run([str(launcher), *args], cwd=project, env=env | (overrides or {}),
                                    capture_output=True, text=True, timeout=30)
            assert result.returncode == 0, (args, result.stdout, result.stderr)
            return result.stdout.strip()

        assert run(['--version']) == '11.11.0'
        alternatives = ['0', 'false', 'true', 'off', 'false']
        for (key, default), alternative in zip(DEFAULTS.items(), alternatives):
            config = key.removeprefix('PNPM_CONFIG_').lower().replace('_', '-')
            assert run(['config', 'get', config]) == default, config
            assert run(['config', 'get', config], {key: ''}) == default, config
            assert run(['config', 'get', config], {key: alternative}) == alternative, config
            assert run(['--config.' + config + '=' + alternative, 'config', 'get', config]) == alternative, config
        # npm-style environment keys (NPM_CONFIG_*/npm_config_*) are outside the
        # documented interface and must not relax the five image defaults.
        relaxed = {'minimum-release-age': '0', 'minimum-release-age-strict': 'false',
                   'minimum-release-age-ignore-missing-time': 'true', 'trust-policy': 'off',
                   'ignore-scripts': 'false'}
        for key, default in DEFAULTS.items():
            config = key.removeprefix('PNPM_CONFIG_').lower().replace('_', '-')
            upper = 'NPM_CONFIG_' + config.upper().replace('-', '_')
            lower = 'npm_config_' + config.replace('-', '_')
            assert run(['config', 'get', config], {upper: relaxed[config], lower: relaxed[config]}) == default, config
        yaml = 'minimumReleaseAge: 10\nminimumReleaseAgeStrict: false\nminimumReleaseAgeIgnoreMissingTime: true\ntrustPolicy: off\nignoreScripts: false\n'
        (project / 'pnpm-workspace.yaml').write_text(yaml)
        (project / '.npmrc').write_text('minimum-release-age=10\nignore-scripts=false\n')
        globalconfig = home / '.config/pnpm/config.yaml'
        globalconfig.parent.mkdir(parents=True)
        globalconfig.write_text(yaml.replace('10', '120'))
        for key, default in DEFAULTS.items():
            config = key.removeprefix('PNPM_CONFIG_').lower().replace('_', '-')
            assert run(['config', 'get', config]) == default, config
        assert run(['config', 'get', 'minimum-release-age'], {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '9999'}) == '9999'
        assert run(['--config.minimum-release-age=7', 'config', 'get', 'minimum-release-age'], {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '9999'}) == '7'

        # Config queries are not installation proof: run the conflicting direct
        # keys through real installs against a loopback registry.
        with Registry() as registry:
            env['PNPM_CONFIG_REGISTRY'] = registry.url
            counter = 0

            def install(args: list[str], overrides: dict[str, str]) -> Install:
                nonlocal counter
                counter += 1
                scenario = work / ('install-' + str(counter))
                scenario.mkdir()
                (scenario / 'package.json').write_text('{"name":"project","version":"1.0.0"}')
                write_conflicting_project(scenario)
                scenario_home = scenario / 'home'
                scenario_home.mkdir()
                child = {**env, 'HOME': str(scenario_home), 'PNPM_CONFIG_FETCH_RETRIES': '0',
                         'MARKERS': str(scenario / 'markers'), **overrides}
                result = subprocess.run([str(launcher), *args], cwd=scenario, env=child,
                                        capture_output=True, text=True, timeout=60)
                return Install(scenario, result.returncode, result.stdout + result.stderr)

            assert_defaults_win(install)
        print('artifact SHA-512=' + pin['sha512'])
        print('pristine bundle SHA-256=' + hashlib.sha256((package / 'dist/pnpm.mjs').read_bytes()).hexdigest())
        print('Node=' + subprocess.check_output([node, '--version'], text=True).strip())
    print('pristine artifact/config/install precedence probes passed (not lifecycle/native acceptance)')


if __name__ == '__main__':
    if sys.argv[1:] == ['--hermetic']:
        hermetic()
    elif len(sys.argv) == 3 and sys.argv[1] == '--artifact':
        artifact(Path(sys.argv[2]))
    else:
        raise SystemExit('usage: pnpm-defaults.sh --hermetic | --artifact PATH')
