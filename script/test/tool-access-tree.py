#!/usr/bin/env python3
"""Tree permission/link oracles for `recipe-contract tool-access --tree`.

The filesystem-root establishment control lives in the Go module
(toolaccess.TestTreeRootEstablishment) rather than as a mocked import here.
"""
import errno
import os
from pathlib import Path
import socket
import stat
import struct
import subprocess
import sys
import tempfile

sys.dont_write_bytecode = True
REPO_ROOT = Path(__file__).resolve().parents[2]


from contract_context import native_contract_directory

CHECKER = native_contract_directory(REPO_ROOT) / 'recipe-contract'


def main():
    with tempfile.TemporaryDirectory(prefix='tool-access-tree-', dir='/tmp') as temporary:
        work = Path(temporary).resolve()
        work.chmod(0o755)
        tree = work / 'owned'
        tree.mkdir()
        payload = tree / '.pnpm/pkg/node_modules/pkg'
        payload.mkdir(parents=True)
        data = payload / 'space\ncontrol file'
        data.write_text('content, not an executable\n')
        data.chmod(0o644)
        (tree / 'pkg').symlink_to('.pnpm/pkg/node_modules/pkg')

        def run(target=tree, status=0, diagnostic='', flags=('--tree',), cwd=None, env=None):
            result = subprocess.run([str(CHECKER), 'tool-access', *flags, str(target)],
                                    cwd=cwd, env=env, capture_output=True, text=True, timeout=10)
            assert result.returncode == status, (result.returncode, result.stdout, result.stderr)
            if diagnostic:
                assert diagnostic in result.stderr, result.stderr
            return result

        run()
        for target, mode, diagnostic in [
            (tree, 0o700, 'not traversable by all'),
            (data, 0o600, 'not readable by all'),
            (data, 0o604, 'not readable by all'),
            (payload, 0o745, 'not traversable by all'),
            (payload, 0o711, 'not readable/traversable by all'),
        ]:
            original = stat.S_IMODE(target.stat().st_mode)
            target.chmod(mode)
            run(status=1, diagnostic=diagnostic)
            target.chmod(original)
            run()

        # Outside ancestors need traversal, not listing; preserve legacy modes.
        outer = work / 'traverse-only'
        outer.mkdir()
        inner = outer / 'owned'
        inner.mkdir()
        (inner / 'data').write_text('readable')
        outer.chmod(0o711)
        run(inner)
        outer.chmod(0o755)

        link = tree / 'edge'
        outside = work / 'outside'
        outside.mkdir()
        (outside / 'back').symlink_to(data)
        for destination, diagnostic in [
            ('missing', 'does not exist'),
            (str(outside), 'escapes tree root'),
            (str(outside / 'back'), 'escapes tree root'),
            ('../outside/back', 'escapes tree root'),
        ]:
            link.symlink_to(destination)
            run(status=1, diagnostic=diagnostic)
            link.unlink()
            run()

        private = tree / 'private'
        private.mkdir()
        private.chmod(0o700)
        link.symlink_to('private/../.pnpm/pkg/node_modules/pkg/space\ncontrol file')
        run(status=1, diagnostic='not traversable by all')
        private.chmod(0o755)
        run()
        # Two paths to the same inode cannot bypass lexical ancestry checks.
        (tree / 'alias').symlink_to(data)
        private.chmod(0o700)
        run(status=1, diagnostic='not traversable by all')
        private.chmod(0o755)
        run()
        link.unlink()

        link.symlink_to('chain')
        chain = tree / 'chain'
        chain.symlink_to('edge')
        run(status=1, diagnostic='symlink chain exceeds')
        link.unlink()
        chain.unlink()
        run()
        # A directory dependency cycle is allowed; symlink-chain ELOOP is not.
        (payload / 'ancestor').symlink_to('../../..')
        (payload / 'root').symlink_to(tree)
        run()

        fifo = tree / 'fifo'
        os.mkfifo(fifo, 0o644)
        run(status=1, diagnostic='special file in tree')
        fifo.unlink()
        run()
        sock = socket.socket(socket.AF_UNIX)
        sock.bind(str(tree / 'socket'))
        run(status=1, diagnostic='special file in tree')
        sock.close()
        (tree / 'socket').unlink()
        run()
        device = tree / 'device'
        try:
            os.mknod(device, stat.S_IFCHR | 0o644, os.makedev(1, 3))
        except (PermissionError, AttributeError):
            print('tool-access-tree: host device fixture unavailable; required native smoke owns this leg')
        else:
            assert stat.S_ISCHR(device.lstat().st_mode)
            run(status=1, diagnostic='special file in tree')
            device.unlink()
            run()

        acl = struct.pack('<I', 2) + b''.join(struct.pack('<HHI', tag, perm, uid) for tag, perm, uid in [
            (1, 6, 0xffffffff), (2, 4, 12345), (4, 4, 0xffffffff), (16, 4, 0xffffffff), (32, 4, 0xffffffff)])
        try:
            os.setxattr(data, 'system.posix_acl_access', acl)
        except (OSError, AttributeError):
            print('tool-access-tree: host Linux access ACL unavailable; required native smoke owns this leg')
        else:
            assert os.getxattr(data, 'system.posix_acl_access') == acl
            run(status=1, diagnostic='POSIX access ACL')
            os.removexattr(data, 'system.posix_acl_access')
            try:
                os.getxattr(data, 'system.posix_acl_access')
                raise AssertionError('ACL removal failed')
            except OSError as error:
                assert error.errno == errno.ENODATA
            run()

        # A subordinate link whose *absolute* target is spelled through a symlink
        # outside the root is rejected even when the final target is physically
        # inside: that intermediate hop leaves the authorized physical root, so
        # containment fails closed. Spelling the same target physically is allowed
        # (outside ancestors may address an in-root absolute path).
        physical = work / 'physical'
        (physical / 'owned').mkdir(parents=True)
        (physical / 'owned' / 'data').write_text('readable')
        spelling = work / 'spelling'
        spelling.symlink_to(physical)
        interior = physical / 'owned' / 'link'
        interior.symlink_to(spelling / 'owned' / 'data')
        run(physical / 'owned', status=1, diagnostic='escapes tree root')
        interior.unlink()
        interior.symlink_to(physical / 'owned' / 'data')
        run(physical / 'owned')

        run('.', cwd=tree)
        dot_child = work / 'dot-root/child'
        dot_child.mkdir(parents=True)
        run('..', cwd=dot_child)
        dot_child.chmod(0o700)
        run('..', cwd=dot_child, status=1, diagnostic='not traversable by all')
        dot_child.chmod(0o755)
        run('..', cwd=dot_child)
        run(str(tree) + '/')
        # Bare names resolve beneath cwd, regardless of a command on PATH.
        run('owned', cwd=work, env=dict(os.environ, PATH='/nonexistent'))
        root_link = work / 'root-link'
        root_link.symlink_to(tree)
        run(root_link)
        link.symlink_to(outside)
        run(root_link, status=1, diagnostic='escapes tree root')
        link.unlink()
        run(root_link)
        private_root = work / 'private-root'
        private_root.mkdir()
        nested_link = private_root / 'link'
        nested_link.symlink_to(tree)
        private_root.chmod(0o700)
        run(nested_link, status=1, diagnostic='not traversable by all')
        private_root.chmod(0o755)
        run(nested_link)
        run(data, status=1, diagnostic='tree root is not a directory')
        run(tree, status=2, diagnostic='usage:', flags=('--tree', '--content'))
        run(tree, status=2, diagnostic='unknown option', flags=('--bad',))
        result = subprocess.run([str(CHECKER), 'tool-access', '--tree'], capture_output=True)
        assert result.returncode == 2
        assert run(flags=('--quiet', '--tree')).stdout == ''
        assert run(flags=('--help',)).returncode == 0
    print('tool-access-tree: root forms, permissions, edges and repaired positives PASS')


if __name__ == '__main__':
    main()
