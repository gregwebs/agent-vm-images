#!/usr/bin/env python3
"""Cold frozen policy controls through the actual launcher and hard-only runner."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).parent / 'fixtures/pnpm'))
from registry import Registry

assert subprocess.check_output(['/usr/local/bin/pnpm', '--version']).strip() == b'11.11.0'
assert Path('/usr/local/bin/pnpm').read_bytes() == Path('/source/images/pnpm/launcher.sh').read_bytes()
with tempfile.TemporaryDirectory(prefix='opencode-policy-') as private, Registry() as registry:
    root = Path(private)
    # Codes and reasons are what stock pnpm 11.11.0 actually emits, captured by
    # running this suite against the base launcher inside the base image. Two
    # fixtures collapse: missing and partial publish-time data both surface under
    # the age code with the same message, so they exercise one engine path rather
    # than two. The invalid-timestamp and partial-time cases stay covered because
    # their messages differ.
    for index, (name, version, mutation, code, reason) in enumerate([
            ('fixture-package', '1.1.0', '', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION', 'within the minimumReleaseAge cutoff'),
            ('fixture-missing', '1.0.0', '', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION', 'could not be checked against minimumReleaseAge'),
            ('fixture-package', '1.0.0', 'invalid-time', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION', 'publish timestamp is not a valid date'),
            ('fixture-trust', '1.1.0', '', 'ERR_PNPM_TRUST_DOWNGRADE', 'High-risk trust downgrade'),
            ('fixture-partial-time', '1.1.0', '', 'ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION', 'could not be checked against minimumReleaseAge'),
            ('fixture-corrupt', '1.0.0', '', 'ERR_PNPM_TARBALL_INTEGRITY', ''),
            ('fixture-package', '1.0.0', 'manifest', 'ERR_PNPM_OUTDATED_LOCKFILE', ''),
            ('fixture-package', '1.0.0', 'config', 'ERR_PNPM_LOCKFILE_CONFIG_MISMATCH', '')]):
        project = root / str(index)
        project.mkdir()
        (project / 'package.json').write_text(json.dumps({'name': 'policy-fixture', 'version': '1.0.0', 'dependencies': {name: version}}))
        (project / 'pnpm-workspace.yaml').write_text('autoInstallPeers: false\nallowBuilds: {}\n')
        env = {key: value for key, value in os.environ.items()
               if not key.lower().startswith(('npm_config_', 'pnpm_config_', 'xdg_'))}
        setup = project / 'setup-home'
        setup.mkdir()
        env.update(HOME=str(setup), CI='true', MARKERS=str(project / 'markers'))
        common = ['--ignore-scripts', '--ignore-pnpmfile', '--pm-on-fail=error', '--registry=' + registry.url, '--fetch-retries=0']
        # Only lock construction is opted out. None of these settings survive
        # into cold verification, and no production lock is generated here.
        result = subprocess.run(['/usr/local/bin/pnpm', 'install', '--lockfile-only', '--no-frozen-lockfile', *common],
            cwd=project, env=env | {'PNPM_CONFIG_MINIMUM_RELEASE_AGE': '0', 'PNPM_CONFIG_TRUST_POLICY': 'off',
                                  'PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME': 'true'}, capture_output=True, timeout=90)
        assert result.returncode == 0, result.stdout + result.stderr
        lock = project / 'pnpm-lock.yaml'
        digest = hashlib.sha256(lock.read_bytes()).digest()
        if mutation == 'manifest':
            manifest = json.loads((project / 'package.json').read_bytes())
            manifest['dependencies'][name] = '1.1.0'
            (project / 'package.json').write_text(json.dumps(manifest))
        if mutation == 'config':
            (project / 'pnpm-workspace.yaml').write_text('autoInstallPeers: false\nallowBuilds: {}\noverrides:\n  fixture-package: 1.1.0\n')
        original_document = registry.documents[name]
        if mutation == 'invalid-time':
            document = json.loads(original_document)
            document['time'][version] = 'not-a-timestamp'
            registry.documents[name] = json.dumps(document).encode()
        cold = project / 'cold-home'
        cold.mkdir()
        env['HOME'] = str(cold)
        receipt = project / 'receipt'
        env['AGENT_VM_TRANSPORT_RECEIPT'] = str(receipt)
        result = subprocess.run(['sh', '/source/images/recipe-contract/run-pnpm.sh', str(project / 'result'), '--',
                                 '/usr/local/bin/pnpm', 'install', '--frozen-lockfile', '--prod', '--no-optional',
                                 '--store-dir', str(project / 'cold-store'), *common], cwd=project, env=env,
                                capture_output=True, timeout=620)
        output = result.stdout + result.stderr + (project / 'result').read_bytes()
        text = output.decode(errors='replace')
        assert result.returncode != 0, text
        assert code in text, text
        if reason:
            assert reason in text, text
        assert hashlib.sha256(lock.read_bytes()).digest() == digest
        assert not receipt.exists() and not (project / 'markers').exists()
        registry.documents[name] = original_document
        print(f'{name}/{mutation or version}: {code} rejected; unchanged lock, no hooks/receipt')
