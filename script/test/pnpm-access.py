#!/usr/bin/env python3
"""Fixed launcher's embedded exec targets need audits beyond its /bin/sh shebang."""
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

# Importing the fixture module must not leave a __pycache__ in the source tree.
sys.dont_write_bytecode = True

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / 'script/test/fixtures/pnpm'
spec = importlib.util.spec_from_file_location('verifier_fixture', FIXTURES / 'verifier-fixture.py')
assert spec and spec.loader
fixture = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fixture)


def main() -> None:
    with tempfile.TemporaryDirectory(prefix='pnpm-access-', dir='/tmp') as directory:
        # Resolve so the lexical fixture paths do not pass through a symlinked
        # /tmp (macOS): a lexical /tmp ancestor would be audited as a writable
        # waypoint and mask the mutation the control intends to exercise.
        work = Path(directory).resolve()
        destination = work / 'image'
        destination.mkdir()
        version = '11.11.0'
        source = fixture.stage(destination, version)
        commands = destination / 'usr/local/bin'
        node = destination / 'usr/bin/node'
        env = os.environ | {'PNPM_SOURCE_DIR': str(source), 'PNPM_FIXTURE_DESTINATION': str(destination),
                            'PNPM_FIXTURE_VERSION': version, 'AGENT_VM_FIXTURE_ROOT': str(work),
                            'PYTHONDONTWRITEBYTECODE': '1',
                            'PATH': str(commands) + ':' + os.environ['PATH']}

        def check(ok: bool) -> None:
            result = subprocess.run(['python3', str(source / 'verify.py')], env=env,
                                    capture_output=True, text=True, timeout=10)
            assert (result.returncode == 0) == ok, result.stderr

        check(True)
        for relative, mode in [('usr/bin/node', 0o700), ('usr/bin', 0o700),
                               ('opt/agent-vm/pnpm/bin/pnpm.mjs', 0o700),
                               ('opt/agent-vm/pnpm/bin/pnpx.mjs', 0o700),
                               ('opt/agent-vm/pnpm/bin', 0o700), ('usr/local/bin', 0o700)]:
            target = destination / relative
            original = target.stat().st_mode & 0o777
            target.chmod(mode)
            check(False)
            target.chmod(original)
        alias = commands / 'pnx'
        alias.unlink()
        alias.symlink_to('other')
        check(False)
        alias.unlink()
        alias.symlink_to('pnpm')
        bundle = destination / 'opt/agent-vm/pnpm/dist/pnpm.mjs'
        dist = bundle.parent
        for target in (bundle, dist):
            original = target.stat().st_mode & 0o777
            for mode in (0o666, 0o000):
                target.chmod(mode)
                check(False)
            target.chmod(original)
        before = bundle.read_bytes()
        bundle.write_bytes(before + b'corrupt')
        check(False)
        bundle.write_bytes(before)
        # A linked distribution entry can hide its whole subtree from the walk,
        # so no unauthenticated link may appear inside the prefix at all.
        relocated = destination / 'relocated-dist'
        dist.rename(relocated)
        dist.symlink_to(relocated, target_is_directory=True)
        (relocated / 'pnpm.mjs').chmod(0o666)
        check(False)
        dist.unlink()
        relocated.rename(dist)
        (dist / 'pnpm.mjs').chmod(0o644)
        # A link whose resolved parent is writable lets the checked target be
        # replaced later; the physical ancestry, not just the link's own mode, matters.
        writable_parent = destination / 'writable-parent'
        writable_parent.mkdir()
        writable_parent.chmod(0o777)
        relocated_bundle = writable_parent / 'pnpm.mjs'
        relocated_bundle.write_bytes(bundle.read_bytes())
        relocated_bundle.chmod(0o644)
        bundle.unlink()
        bundle.symlink_to(relocated_bundle)
        check(False)
        bundle.unlink()
        relocated_bundle.rename(bundle)
        bundle.chmod(0o644)
        # The fixed exec targets are outside the prefix, so only the resolved
        # ancestry audit can catch a writable physical parent there.
        node_parent = destination / 'node-parent'
        node_parent.mkdir()
        node_parent.chmod(0o777)
        real_node = node_parent / 'node'
        real_node.write_bytes(node.read_bytes())
        real_node.chmod(0o755)
        node.unlink()
        node.symlink_to(real_node)
        check(False)
        node.unlink()
        real_node.rename(node)
        node.chmod(0o755)
        # realpath collapses intermediate hops, so a writable waypoint between
        # the fixed target and its safe final file must still be audited.
        resolved_node = destination / 'usr/bin/real-node'
        node.rename(resolved_node)
        writable_hop = destination / 'writable-hop'
        writable_hop.mkdir()
        writable_hop.chmod(0o777)
        hop = writable_hop / 'node'
        hop.symlink_to(resolved_node)
        node.symlink_to(hop)
        check(False)
        node.unlink()
        hop.unlink()
        writable_hop.rmdir()
        resolved_node.rename(node)
        node.chmod(0o755)
        shadow = work / 'shadow'
        shadow.mkdir()
        raw = shadow / 'pnpm'
        shutil.copyfile(FIXTURES / 'shadow-pnpm.sh', raw)
        raw.chmod(0o755)
        env['PATH'] = str(shadow) + ':' + env['PATH']
        env['PNPM_SHADOW_VERSION'] = version
        check(False)
    print('pnpm fixed-target/access/corruption/shadow controls passed')


if __name__ == '__main__':
    main()
