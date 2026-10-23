#!/usr/bin/env python3
"""Native primitive smoke. All mutations are confined to the owned fixture volume."""
import errno
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import stat
import struct
import subprocess
import sys

FIXTURE = Path('/fixtures')
PAIRS = [(12345, 23456), (0, 23456), (12345, 0), (12345, 12345)]
CHECKER = '/contract/recipe-contract'
CAPS = ['CapEff', 'CapPrm', 'CapAmb', 'CapInh', 'CapBnd']
ACL_ENTRIES = [(1, 6, 0xffffffff), (2, 4, 12345), (4, 4, 0xffffffff),
               (16, 4, 0xffffffff), (32, 4, 0xffffffff)]
ACL = struct.pack('<I', 2) + b''.join(struct.pack('<HHI', *entry) for entry in ACL_ENTRIES)


def command(args, **kwargs):
    environment = dict(os.environ, PNPM_SOURCE_DIR='/source/images/pnpm', AGENT_VM_CONTRACT_DIR='/contract', PYTHONDONTWRITEBYTECODE='1')
    result = subprocess.run(args, capture_output=True, text=True, timeout=170, env=environment, **kwargs)
    if result.returncode:
        raise RuntimeError(f'{args}: status {result.returncode}: {result.stderr}')
    return result.stdout


def launcher_hash():
    return hashlib.sha256(Path('/usr/local/bin/pnpm').read_bytes()).hexdigest()


def home(uid, gid):
    return '/root' if uid == 0 else f'/runtime-homes/u{uid}g{gid}'


def drop(uid, gid):
    return ['setpriv', '--bounding-set=-all', '--inh-caps=-all', '--ambient-caps=-all',
            '--reuid', str(uid), '--regid', str(gid), '--clear-groups', '--no-new-privs']


def setup_device():
    for name in ['positive', 'private', 'acl', 'device']:
        tree = FIXTURE / name
        tree.mkdir(mode=0o755)
        (tree / 'data').write_text('readable payload\n')
        (tree / 'data').chmod(0o644)
    package = FIXTURE / 'positive/.pnpm/package'
    package.mkdir(parents=True)
    (package / 'data').write_text('linked readable payload\n')
    (FIXTURE / 'positive/package').symlink_to('.pnpm/package')
    (FIXTURE / 'private/link').symlink_to('data')
    shutil.copytree('/source/script/test/fixtures/pnpm-lock/tiny-dsh', FIXTURE / 'project')
    device = FIXTURE / 'device/node'
    os.mknod(device, stat.S_IFCHR | 0o644, os.makedev(1, 3))
    info = device.lstat()
    assert stat.S_ISCHR(info.st_mode) and info.st_rdev == os.makedev(1, 3)
    assert info.st_uid == 0 and info.st_gid == 0
    print(json.dumps({'device': 'character:1:3', 'verified': True}))


def ready(token):
    assert Path('/invocation/token').read_text().strip() == token
    for uid, gid in PAIRS:
        destination = home(uid, gid)
        if uid:
            command(drop(uid, gid) + ['mkdir', '-m', '700', destination])
        info = Path(destination).stat()
        assert info.st_uid == uid and info.st_gid == gid, (destination, info)
    assert command(['node', '--version']).strip().startswith('v22.')
    assert command(['/usr/local/bin/pnpm', '--version']).strip() == '11.11.0'
    command(['sh', '/source/images/pnpm/verify-pnpm.sh'])
    assert os.readlink('/usr/local/bin/pnx') == 'pnpm'
    assert os.readlink('/usr/local/bin/pnpx') == 'pnpm'
    print(json.dumps({'ready': token, 'arch': platform.machine(), 'launcher': launcher_hash()}))


def mutate(stage):
    if stage == 'private-negative':
        (FIXTURE / 'private/data').chmod(0o600)
    elif stage == 'private-repaired':
        (FIXTURE / 'private/data').chmod(0o644)
    elif stage == 'acl-negative':
        target = FIXTURE / 'acl/data'
        os.setxattr(target, 'system.posix_acl_access', ACL)
        value = os.getxattr(target, 'system.posix_acl_access')
        assert struct.unpack('<I', value[:4]) == (2,)
        decoded = [struct.unpack('<HHI', value[i:i + 8]) for i in range(4, len(value), 8)]
        assert decoded == ACL_ENTRIES
        assert stat.S_IMODE(target.stat().st_mode) == 0o644
        print('verified ACL bytes=' + value.hex())
    elif stage == 'acl-repaired':
        target = FIXTURE / 'acl/data'
        os.removexattr(target, 'system.posix_acl_access')
        try:
            os.getxattr(target, 'system.posix_acl_access')
        except OSError as error:
            assert error.errno == errno.ENODATA
        else:
            raise AssertionError('ACL still present')
        print('verified ACL removed ENODATA')
    elif stage == 'device-negative':
        info = (FIXTURE / 'device/node').lstat()
        assert stat.S_ISCHR(info.st_mode) and info.st_rdev == os.makedev(1, 3)
        print('verified contained character device 1:3')
    elif stage == 'device-repaired':
        (FIXTURE / 'device/node').unlink()
        assert not os.path.lexists(FIXTURE / 'device/node')
        print('verified contained device removed')
    elif stage != 'positive':
        raise ValueError('unknown stage')
    print(json.dumps({'mutation': stage, 'verified': True}))


def probe(token, stage, uid, gid, expected_hash):
    assert (os.getuid(), os.getgid()) == (uid, gid)
    assert os.environ['HOME'] == home(uid, gid)
    status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
    assert all(int(status[key].strip(), 16) == 0 for key in CAPS)
    assert status['NoNewPrivs'].strip() == '1'
    test = Path(os.environ['HOME']) / 'writable'
    test.write_text('caller-owned state')
    test.unlink()
    assert {interface.name for interface in Path('/sys/class/net').iterdir()} == {'lo'}, 'non-loopback network interface'
    assert command(['node', '--version']).strip().startswith('v22.')
    assert command(['/usr/local/bin/pnpm', '--version']).strip() == '11.11.0'
    command(['python3', '/source/images/pnpm/verify.py'])
    command([CHECKER, 'tool-access', 'pnpm', 'pnx', 'pnpx', '/usr/bin/node',
             '/opt/agent-vm/pnpm/bin/pnpm.mjs', '/opt/agent-vm/pnpm/bin/pnpx.mjs'])
    assert launcher_hash() == expected_hash
    assert command([CHECKER, 'pnpm-lock', 'check', 'dsh', '/fixtures/project']) == '0.1.5-rc.2\n'
    command([CHECKER, 'tool-access', '--tree', '/fixtures/positive'])
    for tree in ['private', 'acl', 'device']:
        result = subprocess.run([CHECKER, 'tool-access', '--tree', str(FIXTURE / tree)],
                                capture_output=True, text=True, timeout=30)
        diagnostic = {'private': 'not readable by all', 'acl': 'POSIX access ACL', 'device': 'special file in tree'}[tree]
        negative = stage == tree + '-negative'
        # The device is present from setup until its own repair, independently of other trees.
        if tree == 'device' and stage != 'device-repaired':
            negative = True
        assert result.returncode == (1 if negative else 0), (stage, tree, result.stderr)
        if negative:
            assert diagnostic in result.stderr, (diagnostic, result.stderr)
        print(f'{uid}:{gid} {stage} {tree}: ' + (result.stderr.strip() if negative else 'repaired/positive'))
    print(json.dumps({'probe': token, 'stage': stage, 'pair': f'{uid}:{gid}',
                      'caps': 'zero', 'offline': True, 'launcher': expected_hash}))


def main():
    action = sys.argv[1]
    if action == 'setup-device':
        setup_device()
    elif action == 'ready':
        ready(sys.argv[2])
    elif action == 'mutate':
        mutate(sys.argv[2])
    elif action == 'probe':
        probe(sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5]), sys.argv[6])
    else:
        raise ValueError('unknown action')


if __name__ == '__main__':
    main()
