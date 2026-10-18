#!/usr/bin/env python3
"""Owning cold-install actions and read-only numeric-identity runtime actions."""
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys

ROOT = Path('/fixtures')
PREFIX = ROOT / 'image-root/opt/agent-vm/dsh'
BIN = ROOT / 'image-root/usr/local/bin/dsh'
SOURCE = Path('/source/images/tools/dsh')
PAIRS = ((12345, 23456), (0, 23456), (12345, 0), (12345, 12345))
INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
POLICY = ('--config.minimum-release-age=1440', '--config.minimum-release-age-strict=true',
          '--config.minimum-release-age-ignore-missing-time=false', '--config.trust-policy=no-downgrade',
          '--ignore-scripts', '--ignore-pnpmfile', '--pm-on-fail=error',
          '--config.update-notifier=false', '--registry=https://registry.npmjs.org/',
          '--package-import-method=copy')
# A graph without the reviewed overrides must fail loudly when it starts: these
# are the startup failures a mismatched Cordis/HMR instance produces.
FATAL = ('requires the Cordis HMR service', 'ERR_MODULE_NOT_FOUND', 'Cannot find module')


def environment():
    return dict(os.environ, AGENT_VM_FIXTURE_ROOT=str(ROOT), AGENT_VM_FIXTURE_BASE_PNPM='1',
                AGENT_VM_DSH_PREFIX=str(PREFIX), AGENT_VM_DSH_COMMAND=str(BIN),
                AGENT_VM_DSH_SOURCE_DIR=str(SOURCE), AGENT_VM_CONTRACT_DIR='/fixtures/contract',
                AGENT_VM_INSTALL_STATUS_DIR='/fixtures/install-status', PNPM_SOURCE_DIR='/source/images/pnpm',
                PATH=str(BIN.parent) + ':' + os.environ['PATH'], PYTHONDONTWRITEBYTECODE='1')


def run(argv, seconds=170, env=None, cwd=None):
    # Files, not inherited pipes: a service grandchild may outlive its leader.
    import tempfile
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        result = subprocess.run(argv, stdout=out, stderr=err, env=env or environment(), timeout=seconds, cwd=cwd)
        out.seek(0); err.seek(0)
        stdout, stderr = out.read(65537), err.read(65537)
    if result.returncode or len(stdout) > 65536 or len(stderr) > 65536:
        raise RuntimeError(f'{argv}: status {result.returncode}: {stderr.decode(errors="replace")}')
    return stdout.decode()


def run_status(argv, seconds=170, env=None, cwd=None, keep=None):
    """Run with captured output and return the status instead of raising.

    `keep` names a file that receives stdout verbatim. A peer report lists one
    diagnostic per unmet declaration and can exceed the bounded capture, while its
    numbers still have to be exact: the decision then reads the file, not a
    truncated prefix.
    """
    import tempfile
    with tempfile.TemporaryFile() as out, tempfile.TemporaryFile() as err:
        target = open(keep, 'wb') if keep else out
        try:
            result = subprocess.run(argv, stdout=target, stderr=err,
                                    env=env or environment(), timeout=seconds, cwd=cwd)
        finally:
            if keep:
                target.close()
        out.seek(0); err.seek(0)
        stdout = '' if keep else out.read(65537).decode(errors='replace')
        return result.returncode, stdout, err.read(65537).decode(errors='replace')


def home(uid, gid):
    return Path('/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}')


def base_identity():
    run(['python3', '/source/images/pnpm/verify.py'])
    return hashlib.sha256(Path('/opt/agent-vm/pnpm/dist/pnpm.mjs').read_bytes()).hexdigest()


def initialize(token, source, version):
    Path('/invocation').mkdir()
    PREFIX.mkdir(parents=True)
    BIN.parent.mkdir(parents=True)
    (ROOT / 'pnpm').mkdir()
    shutil.copyfile('/source/images/pnpm/pin.json', ROOT / 'pnpm/pin.json')
    for uid, gid in PAIRS:
        destination = home(uid, gid)
        if uid:
            run(['setpriv', '--reuid', str(uid), '--regid', str(gid), '--clear-groups',
                 'mkdir', '-m', '700', str(destination)])
    (ROOT / 'invocation.json').write_text(json.dumps({'token': token, 'source': source, 'version': version,
                                                    'base': base_identity()}))


def clean():
    assert (ROOT / 'install-status/dsh').read_text() == 'installed\n'
    assert (PREFIX / 'node_modules/.pnpm').is_dir()
    assert not (PREFIX / 'package-lock.json').exists()
    assert not list(ROOT.glob('dsh-*')), 'owning phase leaked transient state'
    assert set(item.name for item in ROOT.iterdir()) == {'image-root', 'pnpm', 'invocation.json', 'install-status', 'contract'}
    assert base_identity() == json.loads((ROOT / 'invocation.json').read_text())['base']


def cordis_negative(directory):
    """The ticket's real negative leg: drop the four overrides, re-resolve, fail.

    A project without the reviewed overrides is not a locked project, so the
    reviewed reader (`pnpm-lock.mjs`) cannot be the oracle here: derivation is the
    owning reader for that shape, and its exact-requirement rules are what refuse
    to publish. A derivation that *accepts* the removed-override graph means the
    graph is already peer-consistent, so the overrides are only load-bearing at
    runtime and the leg installs it and requires the startup failure instead.
    """
    uploaded = Path(directory)
    assert all((uploaded / name).is_file() and not (uploaded / name).is_symlink() for name in INPUTS), uploaded
    # docker cp keeps the host UID and the probe container has no CAP_DAC_OVERRIDE,
    # so the disposable copy is recreated container-owned, like `prepare` does.
    prefix = ROOT / 'cordis-negative-work'
    prefix.mkdir()
    for name in INPUTS:
        (prefix / name).write_bytes((uploaded / name).read_bytes())
    # A re-resolution, not a reuse: keeping the committed lock would keep the
    # override-pinned selections and prove nothing about the overrides' absence.
    (prefix / 'pnpm-lock.yaml').unlink()
    status, output, errors = run_status(['sh', '/fixtures/contract/run-pnpm.sh',
                                         '/invocation/cordis-negative.ndjson', '--',
                                         '/usr/local/bin/pnpm', 'install', '--lockfile-only',
                                         '--no-frozen-lockfile', *POLICY,
                                         '--store-dir', '/tmp/cordis-negative-store'],
                                        seconds=200, cwd=str(prefix))
    assert status == 0, errors
    assert not (prefix / 'node_modules').exists(), 'lock-only resolution installed a tree'
    # Two different failures hide behind "derivation failed": a graph whose own
    # exact peers are unmet, and a graph whose peers are all satisfied but whose
    # global requirements conflict. Only the first is a peer rejection, so the
    # graph's own satisfaction is measured before the class is chosen.
    report = ROOT / 'cordis-negative-peers.json'
    status, _, errors = run_status(['node', '/fixtures/contract/pnpm-lock.mjs', 'peers', 'dsh',
                                    str(prefix)], seconds=90, keep=str(report))
    assert status == 0, errors
    peers = json.loads(report.read_text())
    families = ('@deepseek-ai/cordis', '@deepseek-ai/cordis-plugin-loader',
                '@deepseek-ai/cordis-plugin-timer', '@deepseek-ai/cordis-plugin-include')
    unmet = [row for row in peers['unsatisfied']
             if any(f'peer {family} wanted' in row for family in families)]
    derive = ['node', str(SOURCE / 'derive-cordis-overrides.mjs'), str(prefix)]
    status, output, errors = run_status(derive, seconds=90)
    numbers = {'exactPeers': peers['exact'], 'unsatisfiedPeers': len(unmet)}
    if status != 0 and unmet:
        # The proof in the diagnostic can exceed the capture bound; the record keeps
        # the class and a bounded prefix, and the full text stays in the container log.
        diagnostic = [line for line in errors.splitlines() if line.strip()][-1][:400]
        print(json.dumps(dict({'cordisNegative': 'exact-peer rejection', 'diagnostic': diagnostic}, **numbers),
                         sort_keys=True))
        report.unlink()
        shutil.rmtree(prefix, ignore_errors=True)
        return
    if status == 0:
        # The full derivation proof exceeds the bounded capture, and only the map is
        # compared here; the adapter always prints overrides first.
        match = re.match(r'^\{"overrides":(\{[^}]*\})', output)
        assert match, 'derivation output has no leading override map'
        derived = json.loads(match.group(1))
        reviewed = json.loads((uploaded / 'reviewed-overrides.json').read_text())
        if derived != reviewed:
            # A different exact map is a review stop, not silent acceptance.
            print(json.dumps({'cordisNegative': 'derivation requires review',
                              'diagnostic': 'derived map differs from the reviewed overrides'}, sort_keys=True))
            shutil.rmtree(prefix, ignore_errors=True)
            return
    # Peer-consistent without the overrides: either derivation succeeded, or it
    # stopped on a conflicting requirement rather than an unmet peer. The reviewed
    # overrides are then only load-bearing at runtime, so install the graph and
    # require the real startup failure instead of accepting a derivation
    # diagnostic as proof.
    negative_prefix = ROOT / 'cordis-negative-prefix'
    bin_dir = ROOT / 'cordis-negative-bin'
    runtime = Path('/root/cordis-negative-runtime')
    try:
        shutil.copytree(prefix, negative_prefix)
        status, output, errors = run_status(['/usr/local/bin/pnpm', 'install', '--prod', '--frozen-lockfile',
                                             *POLICY, '--store-dir', '/tmp/cordis-negative-store'],
                                            seconds=625, cwd=str(negative_prefix))
        assert status == 0, errors
        bin_dir.mkdir()
        (bin_dir / 'dsh').symlink_to(negative_prefix / 'node_modules/@deepseek-ai/dsh/lib/bin.js')
        runtime.mkdir(mode=0o700)
        for name in ('config', 'cache', 'data', 'state', 'tmp'):
            (runtime / name).mkdir()
        env = environment()
        env.update(HOME=str(runtime), TMPDIR=str(runtime / 'tmp'),
                   XDG_CONFIG_HOME=str(runtime / 'config'), XDG_CACHE_HOME=str(runtime / 'cache'),
                   XDG_DATA_HOME=str(runtime / 'data'), XDG_STATE_HOME=str(runtime / 'state'))
        status, _, errors = run_status(['python3', str(SOURCE / 'probe-dsh.py'),
                                        '--prefix', str(negative_prefix),
                                        '--command', str(bin_dir / 'dsh'), '--scratch-root', str(runtime)],
                                       seconds=90, env=env)
        assert status == 1, (status, errors)
        markers = [marker for marker in FATAL if marker in errors]
        assert markers, errors
        print(json.dumps(dict({'cordisNegative': 'runtime startup failure', 'diagnostic': markers[0]}, **numbers),
                         sort_keys=True))
    finally:
        for item in (negative_prefix, bin_dir, runtime, prefix):
            shutil.rmtree(item, ignore_errors=True)


def probe(token, source, version, uid, gid):
    assert (os.getuid(), os.getgid()) == (uid, gid)
    status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
    assert all(int(status[key].strip(), 16) == 0 for key in ('CapEff', 'CapPrm', 'CapAmb', 'CapInh', 'CapBnd'))
    assert status['NoNewPrivs'].strip() == '1'
    assert {entry.name for entry in Path('/sys/class/net').iterdir()} == {'lo'}
    invocation = json.loads((ROOT / 'invocation.json').read_text())
    assert (token, source, version) == (invocation['token'], invocation['source'], invocation['version'])
    assert os.environ['HOME'] == str(home(uid, gid))
    env = environment()
    scratch = home(uid, gid) / 'runtime'
    scratch.mkdir()
    try:
        for name in ('config', 'cache', 'data', 'state', 'tmp'):
            (scratch / name).mkdir()
        env.update(XDG_CONFIG_HOME=str(scratch / 'config'), XDG_CACHE_HOME=str(scratch / 'cache'),
                   XDG_DATA_HOME=str(scratch / 'data'), XDG_STATE_HOME=str(scratch / 'state'), TMPDIR=str(scratch / 'tmp'))
        assert run([str(BIN), '--version'], env=env).strip() == version
        assert base_identity() == invocation['base']
        run(['python3', '/fixtures/contract/check-tool-access.py', '--tree', str(PREFIX)], env=env)
        run(['python3', '/fixtures/contract/check-tool-access.py', str(BIN), 'pnpm'], env=env)
        runtime = json.loads(run(['node', str(SOURCE / 'check-runtime.mjs'), str(PREFIX), '--runtime'], seconds=60, env=env))
        web = json.loads(run(['python3', str(SOURCE / 'probe-dsh.py'), '--prefix', str(PREFIX), '--command', str(BIN),
             '--scratch-root', str(scratch)], seconds=75, env=env))
        report = json.loads(run(['node', '/source/script/test/fixtures/dsh/oracle.mjs', str(PREFIX), version], seconds=30, env=env))
        assert report['subprocess'] == {'stdout': 'independent-dsh\n', 'exitCode': 7, 'disposed': True}
        assert base_identity() == invocation['base']
    finally:
        shutil.rmtree(scratch)
    print(json.dumps({'dshRuntime': token, 'source': source, 'selection': version, 'pair': f'{uid}:{gid}',
                      'runtime': runtime, 'web': web, 'independent': report}, sort_keys=True))
    print(json.dumps({'dshProbe': token, 'source': source, 'selection': version, 'pair': f'{uid}:{gid}',
                      'caps': 'zero', 'offline': True}))


def main():
    action, *args = sys.argv[1:]
    if action == 'initialize':
        initialize(*args)
    elif action == 'prepare':
        # docker cp preserves the host UID on these uploaded files. Recreate
        # only the four owned fixture inputs instead of granting FOWNER/CHOWN.
        for name in INPUTS:
            original = PREFIX / name
            assert original.is_file() and not original.is_symlink()
            temporary = PREFIX / (name + '.new')
            temporary.write_bytes(original.read_bytes())
            temporary.replace(original)
        run(['sh', str(SOURCE / 'prepare-lock.sh'), str(PREFIX), args[0], ''], seconds=625)
    elif action == 'install':
        run(['sh', '/fixtures/contract/run-install.sh', 'dsh', '/fixtures/install-status/dsh',
             'sh', str(SOURCE / 'install-dsh.sh')], seconds=625)
        BIN.symlink_to(PREFIX / 'node_modules/@deepseek-ai/dsh/lib/bin.js')
    elif action == 'verify':
        run(['sh', str(SOURCE / 'verify-dsh.sh')])
    elif action == 'clean':
        clean()
    elif action == 'probe':
        probe(*args[:3], int(args[3]), int(args[4]))
    elif action == 'cordis-negative':
        cordis_negative(*args)
    else:
        raise ValueError('unknown dsh action')


if __name__ == '__main__':
    main()
