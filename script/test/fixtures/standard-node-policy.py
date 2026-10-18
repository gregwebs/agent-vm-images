#!/usr/bin/env python3
"""In-container half of the owning-policy matrix.

One scenario builds a real frozen lock over a deterministic loopback registry,
plants exactly one fault in the *registry* (young, missing or malformed
publication time, downgraded provenance, corrupted tarball bytes) or in the
*project* (manifest or workspace drift), then installs cold through the real
base launcher and the shared hard runner. Verification is what proves the
policy, so nothing here may relax the owning invocation: only lock construction
runs with relaxed eligibility, and that call never installs a tree.

Why the fixture must not soften anything:

* Script hooks may never run. Every fixture archive carries a postinstall hook
  that appends to ``$MARKERS``; success and failure must both leave it absent.
* A generic transport failure must not masquerade as a policy verdict. The
  registry request log proves the request reached this server, and the host
  half compares the terminal NDJSON code exactly.
* The registry override is only legitimate for an invocation-owned fixture
  project. ``/opt/agent-vm/dsh`` and every other fixed path are refused here, so
  this seam can never reach a production prefix.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import urllib.request
from dataclasses import dataclass
from pathlib import Path

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path('/source/script/test/fixtures/pnpm')))

SOURCE = Path('/source')
CONTRACT = SOURCE / 'images/recipe-contract'
FIXTURES = SOURCE / 'script/test/fixtures/pnpm'
LAUNCHER = Path('/usr/local/bin/pnpm')
COMMITTED_WORKSPACE = SOURCE / 'images/tools/dsh/pnpm-workspace.yaml'

USAGE = ('usage: standard-node-policy.py ready | '
         'run SCENARIO SOFT | install PROJECT')

# The five image defaults are set explicitly in the owning subprocess env *and*
# passed as CLI flags for locked resolution, exactly as install-dsh.sh does.
POLICY_ENV = {
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '1440',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'true',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'false',
    'PNPM_CONFIG_TRUST_POLICY': 'no-downgrade',
    'PNPM_CONFIG_IGNORE_SCRIPTS': 'true',
}
OWNING = [
    '--config.minimum-release-age=1440',
    '--config.minimum-release-age-strict=true',
    '--config.minimum-release-age-ignore-missing-time=false',
    '--config.trust-policy=no-downgrade',
    '--ignore-scripts',
    '--ignore-pnpmfile',
    '--pm-on-fail=error',
    '--config.update-notifier=false',
    '--package-import-method=copy',
]
# A fresh lock is constructed for every scenario; none of these settings reach
# the cold install that decides the outcome.
SETUP_ENV = {
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0',
    'PNPM_CONFIG_TRUST_POLICY': 'off',
    'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true',
}
SETUP_TIMEOUT_SECONDS = 180
# run-pnpm.sh owns the 600 s manager bound and 5 s kill escalation.
INSTALL_TIMEOUT_SECONDS = 640
TERMINAL_CODE = re.compile(r'^ERR_[A-Z0-9_]+$')


@dataclass(frozen=True)
class Scenario:
    name: str
    package: str
    version: str
    mutation: str = ''
    expected: str = 'failure'


SCENARIOS = {
    scenario.name: scenario for scenario in [
        Scenario('eligible', 'fixture-hooks', '1.0.0', expected='success'),
        Scenario('too-young', 'fixture-package', '1.1.0'),
        Scenario('missing-time', 'fixture-missing', '1.0.0'),
        Scenario('invalid-time', 'fixture-package', '1.0.0', 'invalid-time'),
        Scenario('partial-time', 'fixture-partial-time', '1.1.0'),
        Scenario('trust-downgrade', 'fixture-trust', '1.1.0'),
        Scenario('corrupt-tarball', 'fixture-corrupt', '1.0.0'),
        Scenario('manifest-mismatch', 'fixture-package', '1.0.0', 'manifest'),
        Scenario('config-mismatch', 'fixture-package', '1.0.0', 'workspace'),
    ]}


class Refused(Exception):
    """An unauthorized fixture path or registry; never softened."""


def environment(**values):
    """Caller package-manager configuration never reaches a manager process."""
    env = {key: value for key, value in os.environ.items()
           if not key.lower().startswith(('npm_config_', 'pnpm_config_', 'corepack_', 'pnpm_home'))}
    env.pop('AGENT_VM_TRANSPORT_RECEIPT', None)
    env.update(values)
    env['CI'] = 'true'
    return env


def run(args, *, env=None, cwd=None, timeout=SETUP_TIMEOUT_SECONDS, log=None):
    """Bounded child whose output goes to files, never to this process's pipes."""
    stdout = open(log + '.out', 'wb') if log else subprocess.DEVNULL
    stderr = open(log + '.err', 'wb') if log else subprocess.DEVNULL
    try:
        return subprocess.run(args, cwd=cwd, env=env, stdout=stdout, stderr=stderr,
                              timeout=timeout, check=False)
    finally:
        if log:
            stdout.close()
            stderr.close()


def bounded_read(path, limit=8192):
    try:
        return Path(path).read_bytes()[-limit:].decode('utf-8', 'replace')
    except OSError:
        return ''


def authorized_root():
    raw = os.environ.get('AGENT_VM_FIXTURE_ROOT')
    if not raw:
        raise Refused('AGENT_VM_FIXTURE_ROOT is required')
    root = Path(raw)
    if not root.is_absolute():
        raise Refused('fixture root must be absolute')
    resolved = root.resolve(strict=True)
    if not resolved.is_dir() or resolved == Path('/'):
        raise Refused('fixture root must be an existing non-root directory')
    if resolved == Path('/opt') or Path('/opt') in resolved.parents:
        raise Refused('production /opt is not an authorized fixture root')
    return resolved


def authorized_project(root, name, soft):
    if not re.fullmatch(r'[a-z0-9-]+', name) or soft not in ('unset', '1', '0'):
        raise Refused('invalid scenario token')
    project = root / f'{name}-{soft}'
    if project.parent.resolve(strict=True) != root:
        raise Refused('project must be a direct child of the fixture root')
    if Path('/opt') in project.resolve().parents or str(project.resolve()).startswith('/opt/'):
        raise Refused('production prefix is not an authorized fixture project')
    if project.exists():
        raise Refused(f'{project}: fixture project already exists')
    return project


def authorized_registry(project, url):
    """The registry override exists only for a loopback server we started."""
    from urllib.parse import urlsplit
    parts = urlsplit(url)
    if parts.scheme != 'http' or parts.hostname != '127.0.0.1' or not parts.port:
        raise Refused(f'{url}: fixture registry must be owned loopback HTTP')
    resolved = str(project.resolve())
    if resolved.startswith('/opt/') or not resolved.startswith('/'):
        raise Refused(f'{resolved}: production prefixes cannot use a fixture registry')
    for key in os.environ:
        if key.lower() in ('npm_config_registry', 'pnpm_config_registry'):
            raise Refused(f'ambient registry override: {key}')
    return url


def workspace_bytes():
    """Committed policy settings, with fixture-scoped overrides substituted.

    The committed override selectors name dsh packages that cannot exist in a
    two-package fixture graph, so only that block is replaced; every policy
    setting is required to appear verbatim in the committed workspace first, so
    the fixture cannot silently test a different policy.
    """
    committed = COMMITTED_WORKSPACE.read_text()
    head, separator, _ = committed.partition('\noverrides:\n')
    required = [
        'packages: [.]', 'autoInstallPeers: true', 'strictPeerDependencies: false',
        'nodeLinker: isolated', 'hoist: true', 'enableGlobalVirtualStore: false',
        'pmOnFail: error', 'sideEffectsCache: false', 'allowBuilds: {}',
        'extendNodePath: false', "trustPolicyExclude: ['chokidar@4.0.3']",
    ]
    if not separator:
        raise Refused('committed workspace has no override block')
    for line in required:
        if line not in head.splitlines():
            raise Refused(f'committed workspace no longer states {line!r}')
    return head + '\n' + 'overrides:\n  fixture-trust: 1.1.0\n'


def terminal(output_path):
    """Terminal structured pnpm record: last ERR_* line, never a retry warning."""
    codes = {}
    try:
        lines = Path(output_path).read_bytes().splitlines()
    except OSError:
        return {'code': None, 'message': ''}
    for line in lines[-400:]:
        try:
            record = json.loads(line)
        except ValueError:
            continue
        if not isinstance(record, dict):
            continue
        nested = record.get('err') if isinstance(record.get('err'), dict) else {}
        code = record.get('code') or nested.get('code')
        if not isinstance(code, str) or not TERMINAL_CODE.match(code):
            continue
        message = ' '.join(part for part in (record.get('message'), nested.get('message'))
                           if isinstance(part, str))
        codes[code] = message
    if not codes:
        return {'code': None, 'message': ''}
    if len(codes) > 1:
        raise Refused('ambiguous terminal code: ' + ', '.join(sorted(codes)))
    code, message = next(iter(codes.items()))
    return {'code': code, 'message': message}


def ready():
    launcher = os.lstat(LAUNCHER)
    import stat as stat_module
    if not stat_module.S_ISREG(launcher.st_mode):
        raise Refused('/usr/local/bin/pnpm is not a regular launcher file')
    digest = hashlib.sha256(LAUNCHER.read_bytes()).hexdigest()
    expected = hashlib.sha256((SOURCE / 'images/pnpm/launcher.sh').read_bytes()).hexdigest()
    if digest != expected:
        raise Refused('image launcher bytes differ from the reviewed source')
    source = subprocess.run(['sh', '-c', 'mkdir /source/agent-vm-policy-probe'],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=30)
    if source.returncode == 0:
        raise Refused('/source is writable; the fixture sources must be read-only')
    version = subprocess.run([str(LAUNCHER), '--version', '--pm-on-fail=error',
                              '--config.update-notifier=false'],
                             capture_output=True, text=True, timeout=60)
    if version.returncode or version.stdout.strip() != '11.11.0':
        raise Refused('base manager is not authenticated 11.11.0')
    if os.environ.get('AGENT_INSTALL_SOFT_FAIL') is not None:
        raise Refused('soft mode must be unset in the harness environment')
    for key in ('NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS'):
        if os.environ.get(key):
            raise Refused(f'unsupported runtime override: {key}')
    print(json.dumps({'ready': 'policy', 'arch': os.uname().machine, 'launcher': digest,
                      'manager': version.stdout.strip(), 'readonly': True}))
    return 0


def install(project_path):
    """run-install.sh body: the owning invocation and its success record only."""
    project = Path(project_path)
    root = authorized_root()
    if root not in project.resolve().parents:
        raise Refused(f'{project}: outside the fixture root')
    registry = authorized_registry(project, os.environ['POLICY_REGISTRY'])
    status = Path(os.environ['POLICY_STATUS'])
    store = os.environ['POLICY_STORE']
    env = environment(**POLICY_ENV)
    # The owning pre-Node guard runs before the manager can start.
    guard = run(['node', str(CONTRACT / 'check-pnpm-environment.mjs'), str(project)], env=env)
    if guard.returncode:
        raise Refused('owning environment guard rejected the fixture environment')
    # Record the soft value the envelope actually reached us with: the host
    # compares it, so an unset/inherited value cannot masquerade as a matrix leg.
    (project / 'soft-seen').write_text(os.environ.get('AGENT_INSTALL_SOFT_FAIL', 'unset') + '\n')
    result = Path(os.environ['POLICY_RESULT'])
    process = run(['sh', str(CONTRACT / 'run-pnpm.sh'), str(result), '--', str(LAUNCHER),
                   'install', '--prod', '--frozen-lockfile', *OWNING,
                   '--registry=' + registry, '--store-dir', store],
                  cwd=project, env=env, timeout=INSTALL_TIMEOUT_SECONDS)
    if process.returncode:
        # The owning hook removes what it partially wrote before failing; the
        # runner must not be able to leave a half-installed tree behind.
        shutil.rmtree(project / 'node_modules', ignore_errors=True)
        return 1
    # Verify the payload the project declared, not a hard-coded name: a scenario
    # that was expected to fail must be able to report an accidental success.
    for name, version in json.loads((project / 'package.json').read_text())['dependencies'].items():
        manifest = project / 'node_modules' / name / 'package.json'
        if not manifest.is_file():
            raise Refused(f'{name}: installed payload is missing')
        if json.loads(manifest.read_text()).get('version') != version:
            raise Refused(f'{name}: installed version is not {version}')
    if not (project / 'node_modules' / '.modules.yaml').is_file():
        raise Refused('pnpm install metadata is missing from the payload')
    status.write_text('installed\n')
    return 0


def scenario(name, soft):
    root = authorized_root()
    project = authorized_project(root, name, soft)
    definition = SCENARIOS[name]
    project.mkdir(parents=True)
    setup_home = project / 'setup-home'
    cold_home = project / 'cold-home'
    for home in (setup_home, cold_home):
        home.mkdir()
    (project / 'package.json').write_text(json.dumps({
        'name': 'policy-fixture', 'version': '1.0.0',
        'dependencies': {definition.package: definition.version}}))
    (project / 'pnpm-workspace.yaml').write_text(workspace_bytes())
    markers = project / 'markers'
    receipt = project / 'receipt'
    receipt.write_text('stale-envelope\n')
    status = project / 'run-status/dsh'
    result = project / 'install.ndjson'
    shims = project / 'shims'
    shims.mkdir()
    for manager in ('npm', 'npx'):
        shim = shims / manager
        shim.write_text(f'#!/bin/sh\nprintf "%s\\n" {manager} >>"{markers}"\nexit 97\n')
        shim.chmod(0o755)

    from registry import Registry
    with Registry() as registry:
        with urllib.request.urlopen(registry.url + '/fixture-package', timeout=10) as response:
            if response.status != 200:
                raise Refused('fixture registry is not serving documents')
        authorized_registry(project, registry.url)
        store_setup = str(project / 'setup-store')
        env = environment(HOME=str(setup_home), XDG_CONFIG_HOME=str(setup_home / 'config'),
                          XDG_CACHE_HOME=str(setup_home / 'cache'),
                          XDG_DATA_HOME=str(setup_home / 'data'),
                          XDG_STATE_HOME=str(setup_home / 'state'),
                          MARKERS=str(markers), **SETUP_ENV)
        setup = run([str(LAUNCHER), 'install', '--lockfile-only', '--no-frozen-lockfile',
                     '--ignore-scripts', '--ignore-pnpmfile', '--pm-on-fail=error',
                     '--config.update-notifier=false', '--registry=' + registry.url,
                     '--store-dir', store_setup],
                    cwd=project, env=env, log=str(project / 'setup'))
        if setup.returncode:
            raise Refused(f'lock construction failed: {bounded_read(str(project / "setup.err"))}')
        lock = project / 'pnpm-lock.yaml'
        if not lock.is_file():
            raise Refused('lock construction wrote no lock')
        digest = hashlib.sha256(lock.read_bytes()).hexdigest()
        if definition.mutation == 'manifest':
            manifest = json.loads((project / 'package.json').read_text())
            manifest['dependencies'][definition.package] = '1.1.0'
            (project / 'package.json').write_text(json.dumps(manifest))
        elif definition.mutation == 'workspace':
            (project / 'pnpm-workspace.yaml').write_text(
                workspace_bytes() + '  fixture-package: 1.1.0\n')
        elif definition.mutation == 'invalid-time':
            document = json.loads(registry.documents[definition.package])
            document['time'][definition.version] = 'not-a-timestamp'
            registry.documents[definition.package] = json.dumps(document).encode()

        soft_value = None if soft == 'unset' else soft
        envelope = {'AGENT_VM_FIXTURE_ROOT': str(root),
                    'POLICY_REGISTRY': registry.url,
                    'POLICY_STATUS': str(status),
                    'POLICY_RESULT': str(result),
                    'POLICY_STORE': str(project / 'cold-store'),
                    'HOME': str(cold_home),
                    'XDG_CONFIG_HOME': str(cold_home / 'config'),
                    'XDG_CACHE_HOME': str(cold_home / 'cache'),
                    'XDG_DATA_HOME': str(cold_home / 'data'),
                    'XDG_STATE_HOME': str(cold_home / 'state'),
                    'TMPDIR': str(project / 'tmp'),
                    'MARKERS': str(markers),
                    'PATH': str(shims) + ':' + os.environ['PATH'],
                    'AGENT_VM_TRANSPORT_RECEIPT': str(receipt)}
        child_env = environment(**envelope)
        if soft_value is not None:
            child_env['AGENT_INSTALL_SOFT_FAIL'] = soft_value
        (project / 'tmp').mkdir()
        wrapped = run(['sh', str(CONTRACT / 'run-install.sh'), 'dsh', str(status),
                       sys.executable, '-B', str(Path(__file__).resolve()),
                       'install', str(project)],
                      env=child_env, cwd=project, log=str(project / 'envelope'))
        requests = list(registry.requests)
        observed = terminal(str(result))
        output = bounded_read(str(result), 262144) + bounded_read(str(project / 'envelope.err'))

    marker_lines = markers.read_text().split() if markers.exists() else []
    soft_seen = project / 'soft-seen'
    record = {
        'scenario': name,
        'soft': soft,
        'softObserved': soft_seen.read_text().strip() if soft_seen.is_file() else None,
        'expected': definition.expected,
        'envelopeStatus': wrapped.returncode,
        'childStatus': 0 if wrapped.returncode == 0 else 1,
        'terminal': observed,
        'lockUnchanged': hashlib.sha256(lock.read_bytes()).hexdigest() == digest,
        # The fixture hooks append their package name; the shims append npm/npx.
        'scriptSentinel': any(line not in ('npm', 'npx') for line in marker_lines),
        'npmFallback': any(line in ('npm', 'npx') for line in marker_lines),
        'receiptUnchanged': receipt.read_text() == 'stale-envelope\n',
        'status': status.read_text().strip() if status.is_file() else None,
        'nodeModules': (project / 'node_modules').exists(),
        'registryRequests': len(requests),
        'tarballServed': any(path.endswith('.tgz') for path in requests),
        'networkError': 'ERR_PNPM_META_FETCH_FAIL' in output or 'ERR_PNPM_FETCH_' in output,
    }
    print(json.dumps(record, sort_keys=True))
    shutil.rmtree(project, ignore_errors=True)
    return 0


def main(argv):
    try:
        if argv[:1] == ['ready'] and len(argv) == 1:
            return ready()
        if argv[:1] == ['run'] and len(argv) == 3:
            return scenario(argv[1], argv[2])
        if argv[:1] == ['install'] and len(argv) == 2:
            return install(argv[1])
        print(USAGE, file=sys.stderr)
        return 2
    except (Refused, OSError, KeyError, ValueError) as error:
        print(f'standard-node-policy fixture: {error}', file=sys.stderr)
        return 1


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
