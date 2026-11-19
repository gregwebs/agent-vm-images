#!/usr/bin/env python3
"""Hermetic authenticated archive probes using real hashlib digests."""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tarfile
import tempfile

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parent))
from contract_context import native_contract_directory  # noqa: E402  (path first)

ROOT = Path(__file__).resolve().parents[2]


def main() -> None:
    # The bootstrap is a recipe-contract subcommand, so the hermetic probe needs
    # the composed helper exactly as a build would mount it.
    contract = native_contract_directory(ROOT) / 'recipe-contract'
    assert contract.is_file(), contract
    with tempfile.TemporaryDirectory(prefix='install-pnpm-test-') as directory:
        work = Path(directory)
        source = work / 'source'
        shutil.copytree(ROOT / 'images/pnpm', source)
        archive = work / 'package.tgz'
        absent_archive = work / 'absent-package.tgz'
        baseline = json.loads((source / 'pin.json').read_text())
        files = {'package/package.json': b'{"name":"pnpm","version":"11.11.0"}',
                 'package/bin/pnpm.mjs': b'upstream pnpm entry',
                 'package/bin/pnpx.mjs': b'upstream alias entry',
                 'package/dist/pnpm.mjs': b'pristine fixture bundle',
                 'package/LICENSE': b'MIT License'}
        cases = ['valid', 'corrupt-archive', 'bad-bundle', 'version', 'missing-entry',
                 'missing-license', 'unsafe-path', 'unsafe-link', 'bad-url', 'bad-sri',
                 'bad-version', 'download-timeout', 'conflict', 'escape-opt', 'escape-bin']
        for case in cases:
            members = files.copy()
            if case == 'version':
                members['package/package.json'] = b'{"name":"pnpm","version":"1.0.0"}'
            if case == 'missing-entry':
                del members['package/bin/pnpx.mjs']
            if case == 'missing-license':
                del members['package/LICENSE']
            if case == 'unsafe-path':
                members['package/../../outside'] = b'bad'
            with tarfile.open(archive, 'w:gz') as tar:
                for name, content in members.items():
                    member = tarfile.TarInfo(name)
                    member.size = len(content)
                    member.mode = 0o644
                    tar.addfile(member, io.BytesIO(content))
                if case == 'unsafe-link':
                    link = tarfile.TarInfo('package/link')
                    link.type = tarfile.SYMTYPE
                    link.linkname = '/tmp'
                    tar.addfile(link)
            pin = baseline.copy()
            digest = hashlib.sha512(archive.read_bytes()).digest()
            pin.update(sha512=digest.hex(), integrity='sha512-' + base64.b64encode(digest).decode(),
                       bundleSha256=hashlib.sha256(files['package/dist/pnpm.mjs']).hexdigest())
            if case == 'corrupt-archive':
                content = bytearray(archive.read_bytes())
                content[-1] ^= 1
                archive.write_bytes(content)
            if case == 'bad-bundle':
                pin['bundleSha256'] = '0' * 64
            if case == 'bad-url':
                pin['url'] = 'https://example.com/pnpm.tgz'
            if case == 'bad-sri':
                pin['integrity'] = 'sha512-bad'
            if case == 'bad-version':
                pin['version'] = 'latest'
            (source / 'pin.json').write_text(json.dumps(pin))
            destination = work / case
            destination.mkdir()
            commands = destination / 'usr/local/bin'
            outside = work / (case + '-outside')
            outside.mkdir()
            if case == 'escape-opt':
                (destination / 'opt').symlink_to(outside)
            if case == 'escape-bin':
                (destination / 'usr/local').mkdir(parents=True)
                commands.symlink_to(outside)
            if case == 'conflict':
                commands.mkdir(parents=True)
                (commands / 'pnpm').write_text('existing')
            # `--fixture-archive` is the bootstrap's hermetic stand-in for the
            # download: the bytes it would have fetched are the ones named here.
            # The transport failure has no replacement seam of its own, so the
            # download-timeout case names an archive that is not there: an
            # unreadable fixture archive is a refusal like any other, and it must
            # leave no destination behind either.
            fixture_archive = absent_archive if case == 'download-timeout' else archive
            env = os.environ | {'PNPM_SOURCE_DIR': str(source),
                                'AGENT_VM_FIXTURE_ROOT': str(work),
                                'PNPM_FIXTURE_DESTINATION': str(destination),
                                'AGENT_INSTALL_SOFT_FAIL': '1',
                                'RECIPE_CONTRACT_HELPER': str(contract)}
            result = subprocess.run(['bash', str(ROOT / 'images/install-pnpm.sh'),
                                     '--fixture-archive', str(fixture_archive)],
                                    env=env, capture_output=True, text=True, timeout=15)
            if case == 'valid':
                assert result.returncode == 0, result.stderr
                prefix = destination / 'opt/agent-vm/pnpm'
                assert (prefix / 'dist/pnpm.mjs').read_bytes() == files['package/dist/pnpm.mjs']
                assert (commands / 'pnpm').read_bytes() == (source / 'launcher.sh').read_bytes()
                assert (commands / 'pnpm').stat().st_mode & 0o777 == 0o755
                assert (prefix / 'dist/pnpm.mjs').stat().st_mode & 0o777 == 0o644
                for alias in ('pnx', 'pnpx'):
                    assert os.readlink(commands / alias) == 'pnpm'
            else:
                assert result.returncode != 0, case
                if case != 'conflict':
                    assert not (commands / 'pnpm').exists(), case
                assert not (destination / 'opt/agent-vm/pnpm').exists(), case
            assert not list(outside.iterdir()), case
            print(case + ': passed')
    print('pnpm bootstrap controls passed')


if __name__ == '__main__':
    main()
