#!/usr/bin/env python3
"""Owning Pi/bridge actions for the focused native harness, in production shape.

The Dockerfile installs Pi and the bridge into the *fixed* production paths, so
the only honest focused proof is the same shape: this fixture creates
`/opt/agent-vm/pi`, `/opt/agent-vm/pi-packages`, `/opt/agent-vm/pi-extensions`,
`/usr/local/bin/pi`, `/opt/agent-vm/seed.d/...` and `/opt/agent-vm/install-status`
inside a disposable container and runs the image's own installer, verifier and
build-only helpers there. Nothing is published anywhere, and the host driver
records that this is a diagnostic container on a locally-built base, never the
proposed-SHA product.

Why the production tuple matters here:

* the static warning wrapper hardcodes the mandatory extension path, so a
  fixture-only tree cannot run it without rewriting reviewed bytes;
* `verify-pi.sh`'s single absence decision authorizes one closed tuple, and a
  mixed production/fixture tuple is refused by design;
* `standard-node-layout.py --consumer pi|bridge` reads the production prefixes
  from an extracted root, so the extraction has to be that root.

Actions are one per owning phase, each a separate bounded `docker exec`, so the
host driver can put its own deadline and evidence record around them instead of
one unaccountable shell script.
"""
import hashlib
import json
import os
from pathlib import Path
import posixpath
import re
import shutil
import stat
import subprocess
import sys
import tempfile

SOURCE = Path('/source/images/tools/pi')
SOURCE_DIR = Path('/source')
CONTRACT = Path('/tmp/recipe-contract')
PI = Path('/opt/agent-vm/pi')
BRIDGE = Path('/opt/agent-vm/pi-packages')
EXTENSIONS = Path('/opt/agent-vm/pi-extensions')
WRAPPER = Path('/usr/local/bin/pi')
SEED = Path('/opt/agent-vm/seed.d/20-pi-claude-bridge')
STATUS = Path('/opt/agent-vm/install-status')
WORK = Path('/tmp/pi-verify')
GATE = Path('/tmp/pi-gate')
CODEX = Path('/agent-vm-state/codex')
HARNESS = Path('/opt/pi-harness')
ARCHIVES = HARNESS / 'archives'
CLAUDE = Path('/opt/agent/.local/bin/claude')
PI_STATE = Path('/agent-vm-state/pi')
CONFIG = PI_STATE / 'agent/claude-bridge.json'
INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
PAIRS = ((12345, 23456), (0, 23456), (12345, 0), (12345, 12345))
POLICY = ('--config.minimum-release-age=1440', '--config.minimum-release-age-strict=true',
          '--config.minimum-release-age-ignore-missing-time=false',
          '--config.trust-policy=no-downgrade', '--ignore-scripts', '--ignore-pnpmfile',
          '--pm-on-fail=error', '--config.update-notifier=false',
          '--registry=https://registry.npmjs.org/', '--package-import-method=copy')
POLICY_ENV = {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '1440',
              'PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT': 'true',
              'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'false',
              'PNPM_CONFIG_TRUST_POLICY': 'no-downgrade',
              'PNPM_CONFIG_IGNORE_SCRIPTS': 'true'}
# The file the wrapper hands Pi as `--extension` for the image-owned bridge
# (`pi.sh`'s BRIDGE_EXTENSION default), relative to the resolved bridge package.
# Authored from the reviewed payload rather than read from its manifest: 0.8.0 is
# TypeScript source that Pi transpiles on load, so its manifest carries no
# `main`, `module`, `exports`, `types` or `bin` at all. A manifest-derived entry
# would refuse the payload the image runs.
BRIDGE_ENTRY = 'src/index.ts'
SYNTHETIC_NOTE = 'agent-vm focused harness: LABELED SYNTHETIC claude, never real health'
# The registered model catalog the investigation measured. The driver, not this
# fixture, owns the expectation; this is only a probe body.
PROBE = '''export default function (pi) {
  pi.on("session_start", (_event, ctx) => {
    const p = ctx.modelRegistry.getProvider("claude-bridge");
    const n = p && typeof p.getModels === "function" ? p.getModels().length
            : p && Array.isArray(p.models) ? p.models.length : 0;
    ctx.ui.notify(p ? `AGENT-VM-BRIDGE-REGISTERED models=${n}` : "AGENT-VM-BRIDGE-MISSING",
                  "warning");
  });
}
'''


def invocation_record():
    try:
        return json.loads((HARNESS / 'invocation.json').read_text())
    except (OSError, ValueError):
        return {}


def environment():
    env = dict(os.environ, **POLICY_ENV)
    # The selection travels as the owning hook's own variables, exactly as the
    # standard image passes its build args: an installer that disagrees with the
    # requested exact version fails instead of installing the committed default.
    record = invocation_record()
    env['AGENT_VM_VERSION_PI'] = record.get('pi', os.environ.get('AGENT_VM_VERSION_PI', ''))
    env['AGENT_VM_VERSION_PI_CLAUDE_BRIDGE'] = record.get(
        'bridge', os.environ.get('AGENT_VM_VERSION_PI_CLAUDE_BRIDGE', ''))
    env.update(AGENT_VM_CONTRACT_DIR=str(CONTRACT), AGENT_VM_PI_SOURCE_DIR=str(SOURCE),
               AGENT_VM_PI_SHARED='/source/images/tools/pnpm-project.py',
               AGENT_VM_PI_BYTES_CHECK=str(SOURCE / 'check-installed-bytes.mjs'),
               AGENT_VM_PI_TARGET_PLATFORM=env['AGENT_VM_HARNESS_PLATFORM'],
               AGENT_VM_PI_WORK_DIR=str(WORK), AGENT_VM_PI_PREFIX=str(PI),
               AGENT_VM_PI_WRAPPER=str(WRAPPER), AGENT_VM_PI_PACKAGES_PREFIX=str(BRIDGE),
               AGENT_VM_PI_EXTENSION_DIR=str(EXTENSIONS), AGENT_VM_PI_SEED_HOOK=str(SEED),
               AGENT_VM_PI_GATE_WORK_DIR=str(GATE), AGENT_VM_PI_CODEX_STATE_DIR=str(CODEX),
               AGENT_VM_PI_ENTRY=str(PI / 'node_modules/.bin/pi'),
               AGENT_VM_INSTALL_STATUS_DIR=str(STATUS), PNPM_SOURCE_DIR='/source/images/pnpm',
               PYTHONDONTWRITEBYTECODE='1')
    env.pop('AGENT_INSTALL_SOFT_FAIL', None)
    return env


def run(argv, seconds=170, env=None, cwd=None):
    """One bounded child; files instead of pipes so a grandchild cannot hold us."""
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        result = subprocess.run(argv, stdout=out, stderr=err, env=env or environment(),
                                timeout=seconds, cwd=cwd)
        out.seek(0)
        err.seek(0)
        stdout, stderr = out.read(65537), err.read(65537)
    if result.returncode or len(stdout) > 65536 or len(stderr) > 65536:
        raise RuntimeError(f'{argv}: status {result.returncode}: {stderr.decode(errors="replace")}')
    return stdout.decode()


def run_status(argv, seconds=170, env=None, cwd=None):
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        result = subprocess.run(argv, stdout=out, stderr=err, env=env or environment(),
                                timeout=seconds, cwd=cwd)
        out.seek(0)
        err.seek(0)
        return (result.returncode, out.read(65536).decode(errors='replace'),
                err.read(65536).decode(errors='replace'))


def home(uid, gid):
    return Path('/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}')


def base_identity():
    run(['python3', '/source/images/pnpm/verify.py'], seconds=120)
    return hashlib.sha256(Path('/opt/agent-vm/pnpm/dist/pnpm.mjs').read_bytes()).hexdigest()


def download(url, destination, seconds=300):
    """One classified download with a fresh private receipt of our own.

    `download.sh` refuses to run without a receipt destination, and the receipt
    is this invocation's evidence: a transport failure is reported with its
    classified curl code instead of being retried with less policy.
    """
    receipt = HARNESS / ('receipt-' + hashlib.sha256(url.encode()).hexdigest()[:16])
    env = dict(os.environ, AGENT_VM_TRANSPORT_RECEIPT=str(receipt))
    status, _, stderr = run_status(['sh', str(CONTRACT / 'download.sh'), url, str(destination)],
                                   seconds=seconds, env=env)
    return status, stderr


def sha512_integrity(path):
    digest = hashlib.sha512(Path(path).read_bytes()).digest()
    import base64
    return 'sha512-' + base64.b64encode(digest).decode()


def initialize(token, source, pi_version, bridge_version):
    """Create the production tuple and the per-architecture invocation state."""
    WORK.mkdir(parents=True, mode=0o700)
    HARNESS.mkdir(parents=True, mode=0o700)
    for directory in (STATUS, PI, BRIDGE, EXTENSIONS):
        directory.mkdir(parents=True, exist_ok=True)
    SEED.parent.mkdir(parents=True, exist_ok=True)
    WRAPPER.parent.mkdir(parents=True, exist_ok=True)
    # The Dockerfile's explicit runtime COPYs: the warning wrapper is a regular
    # file (never a link), the extensions and the seed hook are image-owned and
    # executable. No byte is edited here.
    shutil.copyfile(SOURCE_DIR / 'images/tools/pi/pi.sh', WRAPPER)
    WRAPPER.chmod(0o755)
    for name in ('guest-credential-warning.js', 'agent-vm-codex.js'):
        shutil.copyfile(SOURCE_DIR / 'images/tools/pi/extensions' / name, EXTENSIONS / name)
        (EXTENSIONS / name).chmod(0o644)
    shutil.copyfile(SOURCE / 'seed-claude-bridge-config.sh', SEED)
    SEED.chmod(0o755)
    for uid, gid in PAIRS:
        destination = home(uid, gid)
        if uid:
            run(['setpriv', '--reuid', str(uid), '--regid', str(gid), '--clear-groups',
                 'mkdir', '-m', '700', str(destination)])
    record = {'token': token, 'source': source, 'pi': pi_version, 'bridge': bridge_version,
              'base': base_identity(),
              'launcher': hashlib.sha256(Path('/usr/local/bin/pnpm').read_bytes()).hexdigest(),
              'pin': hashlib.sha256(Path('/opt/agent-vm/pnpm/pin.json').read_bytes()).hexdigest(),
              'platform': os.environ['AGENT_VM_HARNESS_PLATFORM']}
    (HARNESS / 'invocation.json').write_text(json.dumps(record, sort_keys=True) + '\n')
    # The numeric-identity probes are unprivileged: the record they bind to must
    # be readable without granting the setup process any capability.
    (HARNESS / 'invocation.json').chmod(0o644)
    HARNESS.chmod(0o755)
    print(json.dumps({'initialize': token, 'base': record['base'], 'pin': record['pin'],
                      'launcher': record['launcher']}, sort_keys=True))


def root_sri(version):
    """The selected root archive's registry SRI, captured BEFORE preparation.

    Bounded metadata query with the five owning defaults and the npmjs registry;
    no manager install, no lock. It is the independent anchor the driver later
    compares the prepared root record against.
    """
    env = environment()
    env.update(HOME=str(HARNESS / 'view-home'), XDG_CACHE_HOME=str(HARNESS / 'view-home/cache'),
               TMPDIR=str(HARNESS / 'view-home/tmp'))
    for key in ('HOME', 'XDG_CACHE_HOME', 'TMPDIR'):
        Path(env[key]).mkdir(parents=True, exist_ok=True)
    # The metadata verb has its own accepted spellings; `--ignore-pnpmfile` and
    # the install-style `--registry` are not accepted by `pnpm view` in 11.11.0,
    # so the semantic settings use the `--config.*` forms and an unsupported flag
    # is a hard failure, never a retry with less policy.
    argv = ['timeout', '--kill-after=5', '60', '/usr/local/bin/pnpm', 'view',
            '@earendil-works/pi-coding-agent@' + version, 'dist.integrity', 'dist.tarball', '--json',
            '--pm-on-fail=error', '--config.update-notifier=false',
            '--config.ignore-pnpmfile=true', '--registry=https://registry.npmjs.org/']
    status, stdout, stderr = run_status(argv, seconds=75, env=env, cwd=str(HARNESS))
    if status:
        raise RuntimeError(f'root-sri metadata query exited {status}: {stderr.strip()}')
    record = json.loads(stdout)
    integrity = record.get('dist.integrity')
    tarball = record.get('dist.tarball')
    if not isinstance(integrity, str) or not integrity.startswith('sha512-'):
        raise RuntimeError('root-sri: no sha512 dist.integrity for ' + version)
    if not isinstance(tarball, str) or not tarball.startswith('https://'):
        raise RuntimeError('root-sri: no https dist.tarball for ' + version)
    print(json.dumps({'rootSri': {'version': version, 'integrity': integrity,
                                  'tarball': tarball}}, sort_keys=True))


def own_inputs(prefix):
    """docker cp preserves the uploading uid; recreate the four owned inputs."""
    for name in INPUTS:
        original = prefix / name
        if original.is_symlink() or not original.is_file():
            raise RuntimeError(f'{original} is not an uploaded regular input')
        temporary = prefix / (name + '.owning')
        temporary.write_bytes(original.read_bytes())
        temporary.replace(original)


def visible(argv, seconds):
    """Run an owning phase and keep its report: the driver checks what it said."""
    status, stdout, stderr = run_status(argv, seconds=seconds)
    sys.stdout.write(stdout)
    sys.stderr.write(stderr)
    if status:
        raise RuntimeError(f'{argv[1]} exited {status}: {stderr.strip()[-600:]}')
    return stdout


def install_pi(version):
    own_inputs(PI)
    visible(['sh', str(CONTRACT / 'run-install.sh'), 'pi', str(STATUS / 'pi'),
             'sh', str(SOURCE / 'install-pi.sh')], 900)


def install_bridge(version):
    own_inputs(BRIDGE)
    visible(['sh', str(CONTRACT / 'run-install.sh'), 'pi-claude-bridge',
             str(STATUS / 'pi-claude-bridge'), 'sh', str(SOURCE / 'install-pi-packages.sh')], 600)


def graph():
    """The prepared lock, through the syntax-only adapter, as JSON.

    Syntax only: the driver authors its own identity/SRI/edge/optional/peer
    comparison and must never inherit the production converter's opinion.
    """
    destination = HARNESS / 'lock.json'
    with open(destination, 'wb') as out:
        result = subprocess.run([str(CONTRACT / 'recipe-contract'), 'pnpm-yaml',
                                 str(PI / 'pnpm-lock.yaml')], stdout=out, stderr=subprocess.PIPE,
                                env=environment(), timeout=120)
    if result.returncode:
        raise RuntimeError('pnpm-yaml exited %d: %s'
                           % (result.returncode, result.stderr.decode(errors='replace')[:400]))
    value = json.loads(destination.read_text())
    print(json.dumps({'graph': {'lockfileVersion': value.get('lockfileVersion'),
                                'packages': len(value.get('packages') or {}),
                                'snapshots': len(value.get('snapshots') or {}),
                                'path': str(destination)}}, sort_keys=True))


def archives(plan_path):
    """Download, SRI-verify and extract the archives the driver names itself.

    The plan comes from the driver's own parse of the prepared lock plus its
    authored family table, never from the production inventory helper, so this
    is a second independent derivation of which authenticated bytes are needed.
    Family members land in PARENT/<name>/<version> and providers in the
    checker's evidence layout; the checker re-hashes every retained tarball.
    """
    plan = json.loads(Path(plan_path).read_text())
    ARCHIVES.mkdir(parents=True, exist_ok=True)
    for entry in plan['archives']:
        name, version = entry['name'], entry['version']
        label = f'{name}@{version}'
        tarball = HARNESS / ('archive-' + re.sub(r'[^A-Za-z0-9._-]', '_', label) + '.tgz')
        status, stderr = download(entry['tarball'], tarball)
        if status:
            raise RuntimeError(f'could not download {label}: {stderr.strip()}')
        actual = sha512_integrity(tarball)
        if actual != entry['integrity']:
            raise RuntimeError(f'{label}: archive SRI {actual} is not the lock SRI '
                               f'{entry["integrity"]}')
        if entry['kind'] == 'member':
            root = ARCHIVES.joinpath(*name.split('/'), version)
            if root.exists():
                shutil.rmtree(root)
            root.parent.mkdir(parents=True, exist_ok=True)
            run(['python3', str(SOURCE / 'archive.py'), 'extract', str(tarball), str(root)],
                seconds=200)
        else:
            evidence = ARCHIVES / 'evidence'
            target = evidence.joinpath(*name.split('/'), version)
            target.mkdir(parents=True, exist_ok=True)
            digest = entry['integrity'].split('-', 1)[1]
            import base64
            filename = base64.b64decode(digest).hex() + '.tgz'
            shutil.copyfile(tarball, target / filename)
        print(json.dumps({'archive': label, 'kind': entry['kind'], 'integrity': actual},
                         sort_keys=True))


def bytes_proof(sibling):
    """The installer's own authenticated byte checker, invoked by the harness."""
    member = ARCHIVES.joinpath('@earendil-works', sibling)
    versions = [item.name for item in member.iterdir() if item.is_dir()]
    if len(versions) != 1:
        raise RuntimeError(f'{member}: expected one extracted {sibling} version, found {versions}')
    env = environment()
    env['AGENT_VM_CONTRACT_DIR'] = str(CONTRACT)
    output = run(['node', str(SOURCE / 'check-installed-bytes.mjs'), str(PI), sibling,
                  str(member / versions[0])], seconds=300, env=env)
    print(output, end='')


def runtime():
    """genai/protobufjs from Pi's own context, plus the legacy chord/esbuild proof."""
    env = environment()
    output = run(['node', str(SOURCE / 'check-runtime.mjs'), str(PI)], seconds=300, env=env)
    line = [row for row in output.splitlines() if row.startswith('pi runtime: ')]
    if len(line) != 1:
        raise RuntimeError('check-runtime emitted no single runtime record')
    print(json.dumps({'runtime': json.loads(line[0][len('pi runtime: '):])}, sort_keys=True))


def verify():
    env = environment()
    status, stdout, stderr = run_status(['sh', str(SOURCE / 'verify-pi.sh')], seconds=420, env=env)
    record = {'status': status,
              'pi': (STATUS / 'pi').read_text().strip() if (STATUS / 'pi').is_file() else None,
              'bridge': ((STATUS / 'pi-claude-bridge').read_text().strip()
                         if (STATUS / 'pi-claude-bridge').is_file() else None)}
    sys.stdout.write(stdout)
    sys.stderr.write(stderr)
    print(json.dumps({'verify': record}, sort_keys=True))


def loader_candidates(requester, relative):
    """Every `node_modules` copy the loader tries for `relative`, nearest first.

    A bare specifier is resolved from the requiring file's own directory upward:
    each `node_modules` on the way is tried before the next. The requester is
    therefore part of the answer, and `<bridge>/node_modules/<sdk>` is tried before
    `<prefix>/node_modules/<sdk>` -- so naming the expected path is an assumption
    about the tree, not an observation of what loaded.
    """
    return [ancestor / 'node_modules' / relative for ancestor in (requester, *requester.parents)]


def loader_resolution(requester, relative):
    """The copy the loader would load, and every other copy that exists at all.

    `competing` includes the copies *nearer* than the resolved one (the nested
    `node_modules` inside the requiring package, which win) and the ones above it
    (which a different requester would reach). Either is a second copy of the same
    identity in one tree, so the driver treats a non-empty list as a failure.
    """
    directory = None
    competing = []
    for candidate in loader_candidates(requester, relative):
        present = candidate.exists() or candidate.is_symlink()
        if directory is None:
            if present:
                directory = candidate
            continue
        if present:
            competing.append(str(candidate))
    return directory, sorted(competing)


def extension_entries(manifest):
    """The entry files a Pi extension package declares for Pi's own discovery."""
    pi = manifest.get('pi')
    declared = pi.get('extensions') if isinstance(pi, dict) else None
    if not isinstance(declared, list):
        return []
    return [posixpath.normpath(item) for item in declared if isinstance(item, str)]


def module_entry(manifest):
    """The file Node's resolver imports for the package's own name.

    `exports` wins over `main`/`module` when a package declares it, and a package
    that declares none of the three cannot be imported at all -- that is a real
    refusal, not a modelling choice.
    """
    exported = manifest.get('exports')
    if isinstance(exported, str):
        return exported
    root = exported.get('.') if isinstance(exported, dict) else None
    if isinstance(root, str):
        return root
    if isinstance(root, dict):
        for condition in ('default', 'import', 'require'):
            if isinstance(root.get(condition), str):
                return root[condition]
    for key in ('main', 'module'):
        if isinstance(manifest.get(key), str):
            return manifest[key]
    return None


def entry_for(label, manifest):
    """The package-relative entry file the image loads for a resolved package.

    The two loaders are not the same question. Pi's extension loader is handed an
    explicit path, so the bridge's entry is the registered one and its manifest
    only has to agree (`pi.extensions` is Pi's discovery answer; an entry that
    disagrees means the registered and the discoverable file are different
    payloads). The bridge's own `import` of the SDK goes through Node's resolver,
    which reads the SDK manifest.
    """
    if label == 'bridge':
        declared = extension_entries(manifest)
        if declared and BRIDGE_ENTRY not in declared:
            raise RuntimeError(f'the bridge manifest declares the extension entries {declared} but '
                               f'the image registers {BRIDGE_ENTRY}')
        return BRIDGE_ENTRY
    entry = module_entry(manifest)
    if entry is None:
        raise RuntimeError('the sdk manifest names no entry file')
    return entry


def loaded_entry(directory, relative):
    """The absolute entry path, and whether it stays inside the resolved package."""
    root = directory.resolve()
    entry = (root / relative).resolve()
    return entry, root == entry.parent or root in entry.parents


def resolution(bridge_version):
    """Where the bridge and its SDK actually resolve, and what else could be loaded.

    A registration count is a fact about a provider, not about which copy of the
    bridge produced it, so the report answers the loader's question from the
    requiring directory: the prefix for the bridge, and the resolved bridge itself
    for the SDK (which is what jiti gives the bridge's own `import`). The driver
    compares the answer with the expected path and refuses any second copy.
    """
    bridge_directory, bridge_competing = loader_resolution(BRIDGE, 'pi-claude-bridge')
    if bridge_directory is None:
        raise RuntimeError(f'the loader finds no bridge from {BRIDGE}')
    sdk_directory, sdk_competing = loader_resolution(bridge_directory, '@anthropic-ai/claude-agent-sdk')
    report = {}
    for label, relative, requester, directory, competing, expected_version in (
            ('bridge', 'pi-claude-bridge', BRIDGE, bridge_directory, bridge_competing,
             bridge_version),
            ('sdk', '@anthropic-ai/claude-agent-sdk', bridge_directory, sdk_directory,
             sdk_competing, None)):
        if directory is None:
            raise RuntimeError(f'the loader finds no {label} from {requester}')
        if directory.is_symlink() or not directory.is_dir():
            raise RuntimeError(f'{directory} is not the installed {label} directory')
        manifest = json.loads((directory / 'package.json').read_text())
        if manifest.get('name') != relative:
            raise RuntimeError(f'{directory} declares the identity {manifest.get("name")!r}')
        if expected_version is not None and manifest.get('version') != expected_version:
            raise RuntimeError(f'{directory} is version {manifest.get("version")!r}, '
                               f'the selection asked for {expected_version!r}')
        entry_path, inside = loaded_entry(directory, entry_for(label, manifest))
        report[label] = {'path': str(directory), 'expected': str(BRIDGE / 'node_modules' / relative),
                         'resolvedFrom': str(requester), 'version': manifest.get('version'),
                         'entry': str(entry_path), 'entryRegular': entry_path.is_file(),
                         'entryInside': inside, 'competing': competing}
    print(json.dumps({'resolution': report}, sort_keys=True))


def resolution_self_test():
    """The loader model, against trees the container never has.

    The three shapes that matter cannot be built here without a real install: the
    reviewed hoisted tree, the same tree with a *nested* SDK inside the bridge (which
    the bridge's own `import` reaches first), and a copy above the prefix. Each is a
    temporary directory tree, so this runs on the host without Docker, Node or a
    package manager, and it fails if the model ever goes back to naming the expected
    path instead of resolving from the requiring directory.
    """
    sdk = '@anthropic-ai/claude-agent-sdk'
    with tempfile.TemporaryDirectory() as staging:
        prefix = Path(staging) / 'pi-packages'
        bridge = prefix / 'node_modules' / 'pi-claude-bridge'
        bridge.mkdir(parents=True)
        (bridge / 'package.json').write_text(json.dumps({'name': 'pi-claude-bridge',
                                                         'version': '1.0.0'}))
        top = prefix / 'node_modules' / sdk
        nested = bridge / 'node_modules' / sdk
        above = Path(staging) / 'node_modules' / sdk
        for label, present, expected, competing in (
                ('hoisted', (top,), top, []),
                # A nested copy wins, and the expected top-level copy is what it wins
                # against -- the report has to say so rather than name the expectation.
                ('nested', (top, nested), nested, [str(top)]),
                ('above', (top, above), top, [str(above)]),
                ('nested-first', (nested, top, above), nested, [str(top), str(above)])):
            for directory in (top, nested, above):
                shutil.rmtree(directory, ignore_errors=True)
            for directory in present:
                directory.mkdir(parents=True)
                (directory / 'package.json').write_text(json.dumps({'name': sdk,
                                                                    'version': '1.0.0'}))
            observed, others = loader_resolution(bridge, sdk)
            assert observed == expected, (label, observed, expected)
            assert others == sorted(competing), (label, others, competing)
        entry_cases(prefix, bridge)
    print(json.dumps({'resolutionSelfTest': 'hoisted, nested, above-prefix and nested-first; '
                                             'bridge and sdk entry models'},
                     sort_keys=True))


def entry_cases(prefix, bridge):
    """The entry model, on the two payload shapes the image actually loads.

    The reviewed bridge is TypeScript source: no `main`, `module` or `exports`, and
    a manifest that names its entry only through `pi.extensions`. A model that
    demanded a manifest `main` refused exactly that payload while the image ran it,
    so the no-entry-field shape is a case here and not an assumption. The SDK is
    the opposite: Node's resolver has to have an entry, and prefers `exports`.
    """
    sdk = '@anthropic-ai/claude-agent-sdk'
    source = bridge / BRIDGE_ENTRY
    source.parent.mkdir(parents=True, exist_ok=True)
    source.write_text('export default function () {}\n')
    for label, manifest in (
            # 0.8.0 minus Pi's discovery field: still loadable, because the path is
            # registered rather than discovered.
            ('undeclared', {'name': 'pi-claude-bridge', 'version': '1.0.0'}),
            ('declared', {'name': 'pi-claude-bridge', 'version': '1.0.0',
                          'pi': {'extensions': ['./src/index.ts']}})):
        assert entry_for('bridge', manifest) == BRIDGE_ENTRY, label
        entry, inside = loaded_entry(bridge, BRIDGE_ENTRY)
        assert (entry, inside) == (source.resolve(), True), (label, entry, inside)
    for label, manifest in (
            ('disagreeing', {'name': 'pi-claude-bridge', 'pi': {'extensions': ['./dist/index.js']}}),
            ('disagreeing-amongst-several',
             {'name': 'pi-claude-bridge',
              'pi': {'extensions': ['./a.ts', './dist/index.js']}})):
        try:
            entry_for('bridge', manifest)
        except RuntimeError:
            continue
        raise AssertionError(f'the bridge model accepted {label} entry declarations')
    assert module_entry({'exports': './sdk.mjs'}) == './sdk.mjs'
    assert module_entry({'exports': {'.': {'default': './sdk.mjs'}}, 'main': 'ignored.mjs'}) \
        == './sdk.mjs'
    assert module_entry({'exports': {'.': './a.mjs'}, 'main': 'b.mjs'}) == './a.mjs'
    assert module_entry({'main': 'sdk.mjs'}) == 'sdk.mjs'
    assert module_entry({'module': 'sdk.mjs'}) == 'sdk.mjs'
    assert module_entry({'name': sdk}) is None
    try:
        entry_for('sdk', {'name': sdk})
    except RuntimeError:
        pass
    else:
        raise AssertionError('the sdk model accepted a manifest with no entry')
    escaping = {'main': '../../outside.mjs'}
    (prefix.parent / 'outside.mjs').write_text('export default 1\n')
    assert loaded_entry(prefix / 'node_modules' / sdk, module_entry(escaping))[1] is False


def registration():
    """One canonical bridge registration through the shipped warning wrapper."""
    probe = HARNESS / 'probe.js'
    probe.write_text(PROBE)
    out, err = HARNESS / 'registration.out', HARNESS / 'registration.err'
    env = environment()
    env.update(PI_TELEMETRY='0')
    status, _, _ = run_status(['sh', str(CONTRACT / 'run-report.sh'), '150', str(out), str(err),
                               str(WRAPPER), '-e', str(probe), '--mode', 'rpc', '--no-session',
                               '--no-approve'], seconds=200, env=env)
    text = out.read_text(errors='replace') if out.is_file() else ''
    registered = re.findall(r'AGENT-VM-BRIDGE-REGISTERED models=([0-9]+)', text)
    missing = text.count('AGENT-VM-BRIDGE-MISSING')
    print(json.dumps({'registration': {'status': status, 'registered': registered,
                                       'missing': missing,
                                       'malformed': text.count('AGENT-VM-BRIDGE-REGISTERED models=') - len(registered)}},
                     sort_keys=True))


def seed():
    """Explicit seed-hook invocation; the hook is never a Docker CMD here."""
    records = []

    def call():
        status, _, stderr = run_status(['sh', str(SEED)], seconds=60)
        if status:
            raise RuntimeError(f'the seed hook exited {status}: {stderr.strip()}')

    def snapshot():
        return CONFIG.read_bytes() if CONFIG.is_file() else None

    def record(case, **extra):
        body = {'case': case, **extra}
        records.append(body)
        print(json.dumps({'seed': body}, sort_keys=True))

    # 1. No image Claude: nothing may be written, and the exit stays 0.
    shutil.rmtree(CLAUDE.parent, ignore_errors=True)
    shutil.rmtree(PI_STATE, ignore_errors=True)
    call()
    record('absent-binary', wrote=CONFIG.exists())

    # 2. A labeled synthetic executable (never real Claude health) and a fresh state.
    CLAUDE.parent.mkdir(parents=True, exist_ok=True)
    CLAUDE.write_text(f'#!/bin/sh\n# {SYNTHETIC_NOTE}\nexit 0\n')
    CLAUDE.chmod(0o755)
    (CLAUDE.parent / 'HARNESS-SYNTHETIC-CLAUDE').write_text(SYNTHETIC_NOTE + '\n')
    PI_STATE.mkdir(parents=True)
    call()
    written = json.loads(snapshot() or b'{}')
    record('fresh', wrote=snapshot() is not None,
           pathToClaudeCodeExecutable=((written.get('provider') or {}).get('pathToClaudeCodeExecutable')))
    first = snapshot()

    # 3. Idempotent: a second boot adds nothing and rewrites no byte.
    call()
    record('idempotent', identical=snapshot() == first)

    # 4. A user-set path and unrelated siblings survive untouched.
    merged = {'provider': {'pathToClaudeCodeExecutable': '/opt/user/claude', 'extra': True},
              'startupNoticeShown': True, 'other': {'keep': [1, 2, 3]}}
    CONFIG.write_text(json.dumps(merged, indent=2) + '\n')
    before = snapshot()
    call()
    record('preserved', identical=snapshot() == before)

    # 5. A file that does not parse is left byte-identical rather than clobbered.
    for case, body in [('invalid-json', b'{not json'), ('scalar', b'"value"\n'),
                       ('array', b'[]\n'), ('provider-shape', b'{"provider":"nope"}\n')]:
        CONFIG.write_bytes(body)
        call()
        record(case, identical=snapshot() == body)

    # 6. The original mode is carried across the atomic rename.
    CONFIG.write_text(json.dumps({'provider': {}}, indent=2) + '\n')
    CONFIG.chmod(0o640)
    call()
    record('mode', mode=oct(CONFIG.stat().st_mode & 0o777),
           pathToClaudeCodeExecutable=((json.loads(snapshot()).get('provider') or {})
                                       .get('pathToClaudeCodeExecutable')))

    # 7. No provisioned Pi state: the hook is a no-op that still exits 0.
    shutil.rmtree(PI_STATE, ignore_errors=True)
    call()
    record('absent-state', wrote=PI_STATE.exists())

    print(json.dumps({'seedRecords': records}, sort_keys=True))


# --- real-layout mutants ----------------------------------------------------
# Each mutant mutates the *installed* tree or the retained authenticated
# extraction, requires the checker's exact diagnostic, and then repairs and
# requires a positive. No second install and no normalisation pass happens in
# between: a repair always restores the exact bytes/mode/link text recorded
# before the mutation.
def checker(sibling, seconds=300):
    member = ARCHIVES.joinpath('@earendil-works', sibling)
    versions = [item.name for item in member.iterdir() if item.is_dir()]
    if len(versions) != 1:
        raise RuntimeError(f'{member}: expected one extracted {sibling} version')
    env = environment()
    return run_status(['node', str(SOURCE / 'check-installed-bytes.mjs'), str(PI), sibling,
                       str(member / versions[0])], seconds=seconds, env=env)


def installed_package(name):
    """The canonical installed directory of one family member, through a link.

    The project's own `node_modules` holds only its direct dependency; a family
    member is a dependency *link* beside the root package inside the root's
    virtual-store entry, which is the layout the byte checker resolves through
    real edges. Deriving it here is what makes the mutants real-layout mutants
    rather than edits to a guessed `.pnpm/name@version` path.
    """
    root = Path(os.path.realpath(PI / 'node_modules/@earendil-works/pi-coding-agent'))
    if PI not in root.parents:
        raise RuntimeError('the installed root package escapes the payload')
    # <entry>/node_modules holds the package directory and one link per locked
    # dependency, so the scope directory is `root.parent` and the container is
    # its parent.
    sibling = root.parents[1] / name
    if not sibling.exists() and not sibling.is_symlink():
        raise RuntimeError(f'{sibling} is not an installed family dependency link')
    target = Path(os.path.realpath(sibling))
    if PI not in target.parents:
        raise RuntimeError(f'{sibling} escapes the payload')
    return target


def mutations():
    """(name, apply, expected diagnostic code, repair) in a fixed order."""
    name = '@earendil-works/pi-agent-core'
    installed = installed_package(name)
    payload = next(item for item in sorted(installed.rglob('*.js')) if item.is_file())
    archive_member = ARCHIVES.joinpath(*name.split('/'), PI_VERSION_CACHE[0])
    archive_file = archive_member / payload.relative_to(installed)
    tui = installed_package('@earendil-works/pi-tui')
    core_link = installed / 'node_modules/.bin/pi-ai'

    def installed_byte_edit():
        original = payload.read_bytes()
        payload.write_bytes(original[:-1] + bytes([original[-1] ^ 0xFF]))

        def repair():
            payload.write_bytes(original)
        return repair

    def installed_missing():
        original = payload.read_bytes()
        payload.unlink()

        def repair():
            payload.write_bytes(original)
        return repair

    def installed_extra():
        extra = installed / 'node_modules' / 'unreviewed-shadow'
        extra.mkdir(parents=True)

        def repair():
            shutil.rmtree(extra)
        return repair

    def installed_shadow():
        shadow = payload.parent / 'node_modules'
        shadow.mkdir()

        def repair():
            shutil.rmtree(shadow)
        return repair

    def extraction_byte_edit():
        original = archive_file.read_bytes()
        archive_file.write_bytes(original[:-1] + bytes([original[-1] ^ 0xFF]))

        def repair():
            archive_file.write_bytes(original)
        return repair

    def extraction_missing():
        original = archive_file.read_bytes()
        archive_file.unlink()

        def repair():
            archive_file.write_bytes(original)
        return repair

    def archive_owns_generated():
        """An archive that already carries a generated `.bin` path stops review."""
        generated = archive_member / 'node_modules/.bin'
        generated.mkdir(parents=True)
        (generated / 'esbuild').write_bytes(b'#!/bin/sh\n')

        def repair():
            shutil.rmtree(archive_member / 'node_modules')
        return repair

    def bin_link_retarget():
        target = tui / 'node_modules/.bin/marked'
        if not target.is_symlink():
            raise RuntimeError(f'{target} is not the generated bin link')
        original = os.readlink(target)
        target.unlink()
        target.symlink_to('../package.json')

        def repair():
            target.unlink()
            target.symlink_to(original)
        return repair

    def bin_link_missing():
        target = tui / 'node_modules/.bin/marked'
        original = os.readlink(target)
        target.unlink()

        def repair():
            target.symlink_to(original)
        return repair

    def bin_target_mode():
        target = Path(os.path.realpath(core_link)) if core_link.is_symlink() else None
        if target is None or not target.is_file():
            raise RuntimeError(f'{core_link} does not resolve to a bin target')
        original = target.stat().st_mode & 0o777
        target.chmod(0o644)

        def repair():
            target.chmod(original)
        return repair

    def bin_target_bytes():
        target = Path(os.path.realpath(core_link))
        original = target.read_bytes()
        target.write_bytes(original[:-1] + bytes([original[-1] ^ 0xFF]))

        def repair():
            target.write_bytes(original)
        return repair

    def dependency_link_escape():
        link = installed.parents[1] / '@earendil-works/pi-ai'
        if not link.is_symlink():
            raise RuntimeError(f'{link} is not a dependency link')
        original = os.readlink(link)
        link.unlink()
        link.symlink_to('/')

        def repair():
            link.unlink()
            link.symlink_to(original)
        return repair

    def dependency_link_text():
        link = installed.parents[1] / '@earendil-works/pi-ai'
        original = os.readlink(link)
        resolved = os.path.realpath(link)
        link.unlink()
        # Same target, non-canonical text: an absolute path instead of pnpm's
        # relative one. Only the exact readlink bytes may describe the edge.
        link.symlink_to(resolved)

        def repair():
            link.unlink()
            link.symlink_to(original)
        return repair

    return [
        ('installed-byte-edit', installed_byte_edit, 'byte-mismatch'),
        ('installed-missing-file', installed_missing, 'missing-path'),
        ('installed-extra-file', installed_extra, 'unexpected-path'),
        ('installed-shadow-node-modules', installed_shadow, 'unexpected-path'),
        ('extraction-byte-edit', extraction_byte_edit, 'byte-mismatch'),
        ('extraction-missing-file', extraction_missing, 'unexpected-path'),
        ('archive-owns-generated-path', archive_owns_generated, 'archive-owns-generated-path'),
        ('bin-link-retarget', bin_link_retarget, 'link-mismatch'),
        ('bin-link-missing', bin_link_missing, 'missing-generated'),
        ('bin-target-mode-0644', bin_target_mode, 'mode-mismatch'),
        ('bin-target-bytes', bin_target_bytes, 'byte-mismatch'),
        ('dependency-link-escape', dependency_link_escape, 'link-escape'),
        ('dependency-link-noncanonical', dependency_link_text, 'dependency-link'),
    ]


PI_VERSION_CACHE = ['0.0.0']


def mutants(sibling, version):
    PI_VERSION_CACHE[0] = version
    records = []
    for name, factory, code in mutations():
        repair = factory()
        try:
            status, _, stderr = checker(sibling)
            diagnostic = [line for line in stderr.splitlines() if line.strip()]
            actual = diagnostic[0] if diagnostic else ''
        finally:
            repair()
        if status == 0:
            raise RuntimeError(f'mutant {name} was accepted by the byte checker')
        match = re.match(r'check-installed-bytes\.mjs: ([a-z-]+): ', actual)
        repaired, _, repaired_err = checker(sibling)
        record = {'mutant': name, 'status': status,
                  'code': match.group(1) if match else None,
                  'diagnostic': actual[:300], 'expect': code,
                  'repairedStatus': repaired}
        records.append(record)
        print(json.dumps({'mutant': record}, sort_keys=True))
        if repaired:
            raise RuntimeError(f'the repaired tree still fails: {repaired_err.strip()}')
    print(json.dumps({'mutants': [record['mutant'] for record in records],
                      'count': len(records)}, sort_keys=True))


def selected_archive(version, integrity, tarball):
    """The selected archive itself: SRI-verified before extraction, then read.

    The pre-run registry SRI comes from the driver, so this cannot accept a
    different archive than the one the driver recorded: the bytes are hashed
    here again and compared with the captured SRI before the shrinkwrap or the
    manifest is read as data.
    """
    tarball_path = HARNESS / f'selected-{version}.tgz'
    status, stderr = download(tarball, tarball_path)
    if status:
        raise RuntimeError(f'could not download the selected archive: {stderr.strip()}')
    actual = sha512_integrity(tarball_path)
    if actual != integrity:
        raise RuntimeError(f'the selected archive hashes to {actual}, not the captured {integrity}')
    root = HARNESS / 'selected' / version
    if root.exists():
        shutil.rmtree(root)
    root.parent.mkdir(parents=True, exist_ok=True)
    run(['python3', str(SOURCE / 'archive.py'), 'extract', str(tarball_path), str(root)],
        seconds=200)
    # archive.py strips the archive's single `package/` root by contract.
    shrinkwrap_file = root / 'npm-shrinkwrap.json'
    shrinkwrap = shrinkwrap_file.is_file() and not shrinkwrap_file.is_symlink()
    manifest = (root / 'package.json').is_file() and not (root / 'package.json').is_symlink()
    if not manifest:
        raise RuntimeError('the selected archive carries no package.json')
    recorded = json.loads((root / 'package.json').read_text())
    if recorded.get('version') != version:
        raise RuntimeError(f'the selected archive declares {recorded.get("version")}, not {version}')
    print(json.dumps({'selectedArchive': {'version': version, 'integrity': actual,
                                          'shrinkwrap': shrinkwrap, 'manifest': True,
                                          'root': str(root)}}, sort_keys=True))


def read(path):
    """One bounded regular file as evidence; never a link, never a directory."""
    item = Path(path)
    info = item.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_size > 262144:
        raise RuntimeError(f'{item} is not a small regular evidence file')
    body = item.read_bytes()
    print(json.dumps({'read': {'path': str(item),
                               'sha256': hashlib.sha256(body).hexdigest(),
                               'mode': oct(info.st_mode & 0o777),
                               'text': body.decode('utf-8', 'replace')}}, sort_keys=True))


def break_bridge():
    """A real bridge payload failure: its TypeScript entry point disappears."""
    source = BRIDGE / 'node_modules/pi-claude-bridge/src'
    if not source.is_dir() or source.is_symlink():
        raise RuntimeError(f'{source} is not the installed bridge source directory')
    source.rename(BRIDGE / 'node_modules/pi-claude-bridge/src.broken')
    print(json.dumps({'breakBridge': 'moved'}, sort_keys=True))


def repair_bridge():
    broken = BRIDGE / 'node_modules/pi-claude-bridge/src.broken'
    if not broken.is_dir():
        raise RuntimeError('there is no broken bridge source to restore')
    broken.rename(BRIDGE / 'node_modules/pi-claude-bridge/src')
    print(json.dumps({'repairBridge': 'restored'}, sort_keys=True))


def probe(token, source, pi_version, bridge_version, uid, gid):
    assert (os.getuid(), os.getgid()) == (uid, gid)
    status = dict(line.split(':', 1)
                  for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
    assert all(int(status[key].strip(), 16) == 0 for key in
               ('CapEff', 'CapPrm', 'CapAmb', 'CapInh', 'CapBnd'))
    assert status['NoNewPrivs'].strip() == '1'
    assert {entry.name for entry in Path('/sys/class/net').iterdir()} == {'lo'}
    invocation = json.loads((HARNESS / 'invocation.json').read_text())
    assert (token, source) == (invocation['token'], invocation['source'])
    assert os.environ['HOME'] == str(home(uid, gid))
    env = environment()
    scratch = home(uid, gid) / 'runtime'
    scratch.mkdir()
    try:
        for name in ('config', 'cache', 'data', 'state', 'tmp'):
            (scratch / name).mkdir()
        env.update(XDG_CONFIG_HOME=str(scratch / 'config'), XDG_CACHE_HOME=str(scratch / 'cache'),
                   XDG_DATA_HOME=str(scratch / 'data'), XDG_STATE_HOME=str(scratch / 'state'),
                   TMPDIR=str(scratch / 'tmp'), PI_TELEMETRY='0')
        if not stat.S_ISREG(WRAPPER.lstat().st_mode):
            raise RuntimeError(f'{WRAPPER} is not a regular file')
        version = run([str(WRAPPER), '--version'], seconds=90, env=env).strip()
        run([str(CONTRACT / 'recipe-contract'), 'tool-access', '--tree', str(PI)], env=env)
        run([str(CONTRACT / 'recipe-contract'), 'tool-access', str(WRAPPER),
             str(PI / 'node_modules/.bin/pi')], env=env)
        assert base_identity() == invocation['base']
    finally:
        shutil.rmtree(scratch)
    print(json.dumps({'piProbe': token, 'source': source, 'pi': pi_version,
                      'bridge': bridge_version, 'pair': f'{uid}:{gid}', 'caps': 'zero',
                      'offline': True, 'wrapper': 'regular-file', 'version': version},
                     sort_keys=True))


def clean():
    assert (STATUS / 'pi').read_text() == 'installed\n'
    assert (STATUS / 'pi-claude-bridge').read_text() == 'installed\n'
    for prefix in (PI, BRIDGE):
        assert not (prefix / 'package-lock.json').exists(), prefix
        assert all((prefix / name).is_file() for name in INPUTS), prefix
    # The installer's authorized scratch parent must be empty again, and no
    # transient archive or store may survive outside the harness's own root.
    assert not [item for item in WORK.iterdir()], list(WORK.iterdir())
    print(json.dumps({'clean': {'work': 'empty', 'records': 2}}, sort_keys=True))


def main():
    action, *args = sys.argv[1:]
    if action == 'initialize':
        initialize(*args)
    elif action == 'root-sri':
        root_sri(*args)
    elif action == 'install-pi':
        install_pi(*args)
    elif action == 'install-bridge':
        install_bridge(*args)
    elif action == 'graph':
        graph()
    elif action == 'archives':
        archives(*args)
    elif action == 'bytes':
        bytes_proof(*args)
    elif action == 'runtime':
        runtime()
    elif action == 'verify':
        verify()
    elif action == 'selected-archive':
        selected_archive(*args)
    elif action == 'read':
        read(*args)
    elif action == 'break-bridge':
        break_bridge()
    elif action == 'repair-bridge':
        repair_bridge()
    elif action == 'registration':
        registration()
    elif action == 'resolution':
        resolution(*args)
    elif action == 'resolution-self-test':
        resolution_self_test()
    elif action == 'seed':
        seed()
    elif action == 'mutants':
        mutants(*args)
    elif action == 'probe':
        probe(*args[:4], int(args[4]), int(args[5]))
    elif action == 'clean':
        clean()
    else:
        raise ValueError('unknown pi action: ' + action)


if __name__ == '__main__':
    main()
