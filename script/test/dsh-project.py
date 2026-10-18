#!/usr/bin/env python3
"""Filesystem boundary tests; syscall faults are test-only, never environment bypasses."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
SOURCE = Path(__file__).resolve().parents[2] / 'images/tools/dsh/project.py'
spec = importlib.util.spec_from_file_location('dsh_project', SOURCE)
project = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = project
spec.loader.exec_module(project)


class ProjectTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix='dsh-project-')).resolve()
        self.env = patch.dict(os.environ, {'AGENT_VM_FIXTURE_ROOT': str(self.root)})
        self.env.start()
        self.prefix = self.make_prefix('prefix', 'old')
        self.stage = self.make_prefix('stage', 'new')
        (self.root / 'pnpm').mkdir()
        (self.root / 'pnpm/pin.json').write_text('{}')

    def tearDown(self):
        self.env.stop()
        shutil.rmtree(self.root)

    def make_prefix(self, name, body):
        prefix = self.root / name
        prefix.mkdir()
        for filename in project.INPUTS:
            (prefix / filename).write_text(body + filename)
        return prefix

    def image(self):
        return [(self.prefix / filename).read_bytes() for filename in project.INPUTS]

    def test_authorization_is_read_only(self):
        before = sorted(self.root.rglob('*'))
        capability = project.authorize_mutation(self.prefix, command=self.root / 'bin/dsh')
        self.assertTrue(capability.mutation)
        self.assertFalse(capability.production)
        self.assertEqual(before, sorted(self.root.rglob('*')))
        with self.assertRaises(ValueError):
            project.authorize_mutation(self.root)
        with patch.dict(os.environ, {}, clear=True), self.assertRaises(ValueError):
            project.authorize_mutation(self.prefix)

    def test_symlink_waypoints_and_inputs(self):
        outside = self.root.parent / (self.root.name + '-outside')
        outside.mkdir()
        try:
            (self.root / 'escape').symlink_to(outside, target_is_directory=True)
            with self.assertRaises(ValueError):
                project.authorize_mutation(self.root / 'escape')
            (self.root / 'inside').symlink_to(self.prefix, target_is_directory=True)
            with self.assertRaises(ValueError):
                project.authorize_mutation(self.root / 'inside')
            for target in (outside / 'missing', self.root / 'cycle'):
                path = self.prefix / 'package.json'
                path.unlink(missing_ok=True)
                path.symlink_to(target)
                with self.assertRaises((ValueError, OSError)):
                    project.authorize_mutation(self.prefix)
            (self.root / 'cycle').symlink_to(self.root / 'cycle')
            with self.assertRaises((ValueError, OSError)):
                project.authorize_mutation(self.prefix)
        finally:
            outside.rmdir()

    def test_path_collisions(self):
        for option in ({'command':self.prefix / 'dsh'}, {'status_dir':self.prefix},
                       {'status_dir':self.prefix / 'state'}, {'command':self.root / 'pnpm/pin.json'}):
            with self.assertRaises(ValueError):
                project.authorize_mutation(self.prefix, **option)
        command = self.root / 'command'
        command.write_text('caller command')
        with self.assertRaises(ValueError):
            project.authorize_mutation(self.prefix, command=command)
        self.assertEqual(command.read_text(), 'caller command')
        command.unlink()
        command.symlink_to('/bin/sh')
        with self.assertRaises(ValueError):
            project.authorize_mutation(self.prefix, command=command)
        (self.prefix / 'node_modules').symlink_to(self.stage, target_is_directory=True)
        with self.assertRaises(ValueError):
            project.authorize_mutation(self.prefix)

    def test_scratch_registration(self):
        capability = project.authorize_mutation(self.prefix)
        scratch = project.owned_scratch(capability)
        (scratch / 'sentinel').write_text('owned')
        project.cleanup_scratch(scratch)
        self.assertFalse(scratch.exists())
        with self.assertRaises(ValueError):
            project.cleanup_scratch(self.root)
        with self.assertRaises(ValueError):
            project.owned_scratch(capability, self.prefix)
        scratch = project.owned_scratch(capability)
        scratch.rmdir()
        scratch.symlink_to(self.stage)
        with self.assertRaises(ValueError):
            project.cleanup_scratch(scratch)

    def test_scratch_link_to_the_prefix_itself_is_allowed(self):
        # pnpm records the installed project index as a relative link that
        # resolves to the prefix root, not to a directory inside it: cleanup must
        # accept that exact destination or a production install cannot finish.
        capability = project.authorize_mutation(self.prefix)
        scratch = project.owned_scratch(capability)
        (scratch / 'store').mkdir()
        (scratch / 'store' / 'project').symlink_to(self.prefix, target_is_directory=True)
        project.cleanup_scratch(scratch, (self.prefix,))
        self.assertFalse(scratch.exists())
        scratch = project.owned_scratch(capability)
        (scratch / 'store').mkdir()
        (scratch / 'store' / 'elsewhere').symlink_to('/', target_is_directory=True)
        with self.assertRaises(ValueError):
            project.cleanup_scratch(scratch, (self.prefix,))

    def test_fixture_manager_is_not_a_path_lookup(self):
        capability = project.authorize_mutation(self.prefix)
        executable = self.root / 'pnpm-fixture'
        executable.write_text('#!/bin/sh\nexit 0\n')
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_PNPM':str(executable)}):
            with self.assertRaises(ValueError):
                project.manager_argv(capability)
            executable.chmod(0o755)
            self.assertEqual(project.manager_argv(capability), [str(executable)])
            executable.unlink()
            executable.symlink_to('/bin/sh')
            with self.assertRaises(ValueError):
                project.manager_argv(capability)
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_PNPM':'/bin/sh'}):
            with self.assertRaises(ValueError):
                project.manager_argv(capability)

    def test_caller_home_sentinels_survive(self):
        home = self.root / 'caller-home'
        home.mkdir()
        for name in ('HOME', 'XDG_CONFIG_HOME', 'XDG_CACHE_HOME', 'XDG_DATA_HOME', 'XDG_STATE_HOME'):
            (home / name).write_text('caller bytes')
        (home / '.npm').mkdir()
        (home / '.npm/sentinel').write_text('caller npm bytes')
        before = {str(p.relative_to(home)):p.read_bytes() for p in home.rglob('*') if p.is_file()}
        with patch.dict(os.environ, {'HOME':str(home)}):
            project.publish(self.stage, self.prefix)
        self.assertEqual(before, {str(p.relative_to(home)):p.read_bytes() for p in home.rglob('*') if p.is_file()})

    def test_scratch_removal_is_confined(self):
        scratch = self.root / 'dsh-owned'
        scratch.mkdir()
        (scratch / 'payload').write_text('owned')
        project.remove_scratch(scratch)
        self.assertFalse(scratch.exists())
        # A real pnpm store records the installed project with a relative index
        # link inside its own scratch; unlinking it is safe and must be allowed.
        sibling = self.root / 'dsh-sibling'
        sibling.mkdir()
        (sibling / 'store-index').symlink_to(self.stage)
        project.remove_scratch(sibling)
        self.assertFalse(sibling.exists())
        outside = self.root.parent / (self.root.name + '-outside')
        outside.mkdir()
        self.addCleanup(shutil.rmtree, outside, True)
        # A real dsh profile links the shared installed payload, so the prefix
        # may be named as an additional allowed target.
        linked = self.root / 'dsh-payload-links'
        linked.mkdir()
        payload = outside / 'payload'
        payload.mkdir()
        (linked / 'profile-package').symlink_to(payload)
        with self.assertRaises(ValueError):
            project.remove_scratch(linked)
        project.remove_scratch(linked, prefix=outside)
        self.assertFalse(linked.exists())
        for name in ('not-a-scratch', 'dsh-escape'):
            candidate = self.root / name
            candidate.mkdir()
            if name == 'dsh-escape':
                (candidate / 'away').symlink_to(outside)
            with self.assertRaises(ValueError):
                project.remove_scratch(candidate)
            shutil.rmtree(candidate)
        # A dsh-named directory outside the authorized scratch parents is refused.
        nested = self.root / 'nested' / 'dsh-x'
        nested.mkdir(parents=True)
        with self.assertRaises(ValueError):
            project.remove_scratch(nested)
        self.assertTrue(nested.exists())
        self.assertTrue(self.root.exists())

    def test_publication(self):
        project.publish(self.stage, self.prefix)
        self.assertEqual(self.image(), [(self.stage / f).read_bytes() for f in project.INPUTS])
        self.assertFalse(list(self.root.glob('dsh-*')))
        with self.assertRaises(ValueError):
            project.publish(self.prefix, self.prefix)

    def test_backup_failure_cannot_publish(self):
        before = self.image()
        copyfile = shutil.copyfile
        def fail_backup(source, destination):
            if Path(destination).name == 'backup-2':
                raise OSError('injected backup failure')
            return copyfile(source, destination)
        with patch.object(project.shutil, 'copyfile', side_effect=fail_backup):
            with self.assertRaises(project.PublicationFailure) as raised:
                project.publish(self.stage, self.prefix)
        self.assertEqual(raised.exception.status, 1)
        self.assertEqual(self.image(), before)
        self.assertFalse(list(self.root.glob('dsh-*')))

    def test_partial_cleanup_failure_restores_preimages(self):
        before = self.image()
        cleanup = project.cleanup_scratch
        calls = 0
        def partial_cleanup(scratch):
            nonlocal calls
            calls += 1
            if calls == 1:
                (scratch / 'backup-0').unlink()
                raise OSError('injected partial cleanup failure')
            return cleanup(scratch)
        with patch.object(project, 'cleanup_scratch', side_effect=partial_cleanup):
            with self.assertRaises(project.PublicationFailure) as raised:
                project.publish(self.stage, self.prefix)
        self.assertEqual(raised.exception.status, 1)
        self.assertEqual(self.image(), before)
        self.assertFalse(list(self.root.glob('dsh-*')))

    def test_usage_status(self):
        result = subprocess.run([sys.executable, str(SOURCE), 'publish', str(self.stage)], capture_output=True)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, b'')

    def test_every_partial_write_is_rolled_back_inclusively(self):
        before = self.image()
        for failure in range(4):
            calls = []
            def replace(source, destination):
                calls.append(destination.name)
                if len(calls) == failure + 1:
                    destination.write_text('partial write before failure')
                    raise OSError('injected publish failure')
                os.replace(source, destination)
            with self.assertRaises(project.PublicationFailure) as raised:
                project.publish(self.stage, self.prefix, replace=replace)
            self.assertEqual(raised.exception.status, 1)
            self.assertEqual(self.image(), before)
            self.assertEqual(calls[failure + 1:], list(reversed(project.INPUTS[:failure + 1])))
            self.assertFalse(list(self.root.glob('dsh-*')))

    def test_each_rollback_failure_retains_all_recovery_and_continues(self):
        for failure in range(4):
            for rollback in range(failure + 1):
                calls = []
                def replace(source, destination):
                    index = len(calls)
                    calls.append(destination.name)
                    if index == failure or index == failure + 1 + rollback:
                        destination.write_text('partial')
                        raise OSError('injected publish/restore failure')
                    os.replace(source, destination)
                with self.assertRaises(project.PublicationFailure) as raised:
                    project.publish(self.stage, self.prefix, replace=replace)
                error = raised.exception
                self.assertEqual(error.status, 3)
                self.assertTrue(error.recovery.is_absolute())
                inventory = json.loads((error.recovery / 'inventory.json').read_text())
                self.assertEqual(len(inventory), 4)
                self.assertTrue(all(Path(entry['backup']).is_file() and Path(entry['staged']).is_file() for entry in inventory))
                self.assertEqual(calls[failure + 1:], list(reversed(project.INPUTS[:failure + 1])))
                self.assertIn(str(error.recovery), str(error))
                # Explicit test recovery, not normal failure cleanup.
                for entry in inventory:
                    shutil.copyfile(entry['backup'], entry['destination'])
                project.cleanup_scratch(error.recovery)

    def test_pre_node_guard_and_project_hooks(self):
        for key, value in [('NODE_OPTIONS','--require=/does/not/exist'), ('NODE_PATH','/bad'),
                           ('BUN_OPTIONS','bad'), ('NODE_EXTRA_CA_CERTS','/bad'),
                           ('SSL_CERT_FILE','/bad'), ('SSL_CERT_DIR','/bad'),
                           ('NODE_TLS_REJECT_UNAUTHORIZED','0'), ('npm_config_registry','https://bad'),
                           ('PNPM_HOME','/bad'), ('COREPACK_HOME','/bad')]:
            with patch.dict(os.environ, {key:value}):
                result = subprocess.run([sys.executable, str(SOURCE), 'guard', str(self.prefix)], capture_output=True)
                self.assertEqual(result.returncode, 1, (key, result.stderr))
                self.assertIn(key.encode() if key != 'NODE_TLS_REJECT_UNAUTHORIZED' else b'TLS', result.stderr)
        (self.prefix / '.npmrc').symlink_to(self.root / 'absent')
        with self.assertRaises(ValueError):
            project.guard_environment([self.prefix])

    def test_shared_checker_keeps_caller_ca_and_dsh_rejects(self):
        checker = SOURCE.parents[2] / 'recipe-contract/check-pnpm-environment.mjs'
        # The OpenCode strict-egress profile supplies its own CA bundle to the
        # shared checker, so CA rejection belongs to dsh alone: project.py refuses
        # the same environment before the manager (or its Node preloads) can start.
        for key in project.TLS_ENV:
            env = {**os.environ, key:str(self.root / 'unreviewed-ca')}
            shared = subprocess.run(['node', str(checker), str(self.prefix)], env=env, capture_output=True)
            self.assertEqual(shared.returncode, 0, (key, shared.stderr))
            guarded = subprocess.run([sys.executable, str(SOURCE), 'guard', str(self.prefix)],
                                     env=env, capture_output=True)
            self.assertEqual(guarded.returncode, 1, (key, guarded.stdout))
            self.assertIn(key.encode(), guarded.stderr)

    def test_contract_directory_is_authorized_by_the_filesystem_module(self):
        contract = self.root / 'recipe-contract'
        contract.mkdir()
        (contract / 'pnpm-lock.mjs').write_text('// module\n')
        with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(contract)}):
            self.assertEqual(project.authorize_contract([self.prefix, self.stage]), contract)
            with self.assertRaises(project.ContractFailure) as raised:
                project.authorize_contract([project.PRODUCTION, self.stage])
            self.assertIn('production contract directory is fixed', str(raised.exception))
        outside = self.root.parent / (self.root.name + '-contract')
        outside.mkdir()
        (outside / 'pnpm-lock.mjs').write_text('// module\n')
        try:
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(outside)}):
                with self.assertRaises(ValueError):
                    project.authorize_contract([self.prefix])
            link = self.root / 'linked-contract'
            link.symlink_to(outside, target_is_directory=True)
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(link)}):
                with self.assertRaises(ValueError):
                    project.authorize_contract([self.prefix])
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(outside)}), \
                    patch.dict(os.environ, {}, clear=False):
                # A redirected parser is a capability: an unauthorized fixture root
                # must not make an arbitrary directory the effective parser.
                os.environ.pop('AGENT_VM_FIXTURE_ROOT', None)
                with self.assertRaises(ValueError):
                    project.authorize_contract([self.prefix])
        finally:
            shutil.rmtree(outside)

    def test_contract_helper_is_authorized_before_it_executes(self):
        # A redirected contract directory is executable policy, so the environment
        # checker must never be spawned from a directory the caller was not
        # authorized to name (AGENT_VM_CONTRACT_DIR is caller-controlled).
        def scratch_contract(directory):
            directory.mkdir()
            (directory / 'pnpm-lock.mjs').write_text('// module\n')
            (directory / 'check-pnpm-environment.mjs').write_text(
                'import fs from "node:fs";\n'
                f'fs.writeFileSync({json.dumps(str(self.root / "checker-ran"))}, "ran");\n')
            return directory

        sentinel = self.root / 'checker-ran'
        outside = self.root.parent / (self.root.name + '-untrusted-contract')
        try:
            scratch_contract(outside)
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(outside)}):
                with self.assertRaises(ValueError):
                    project.guard_environment([self.prefix])
                self.assertFalse(sentinel.exists(), 'unauthorized checker executed')
                result = subprocess.run([sys.executable, str(SOURCE), 'guard', str(self.prefix)],
                                        capture_output=True)
                self.assertEqual(result.returncode, 1, result.stderr)
                self.assertFalse(sentinel.exists(), 'the guard verb executed an unauthorized checker')
            # The same directory is authorized once it is the invocation's own
            # contract copy, so the sentinel proves the authorized path still runs.
            inside = scratch_contract(self.root / 'recipe-contract')
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(inside)}):
                project.guard_environment([self.prefix])
            self.assertEqual(sentinel.read_text(), 'ran')
        finally:
            shutil.rmtree(outside, ignore_errors=True)

    def test_host_checkout_contract_shape(self):
        # N2: a developer upgrade of a checkout keeps the checkout's own tracked
        # contract directory; only its staged prefixes live in a scratch root.
        tracked = SOURCE.parents[2] / 'recipe-contract'
        host = {key: value for key, value in os.environ.items()
                if key not in ('AGENT_VM_CONTRACT_DIR', 'AGENT_VM_FIXTURE_ROOT', 'AGENT_VM_SCRATCH_ROOT')}
        with patch.dict(os.environ, host, clear=True):
            self.assertEqual(project.authorize_contract([SOURCE]), tracked)
            # A prefix outside the checkout needs the invocation scratch root.
            with self.assertRaises(ValueError):
                project.authorize_contract([self.prefix])
            with patch.dict(os.environ, {'AGENT_VM_SCRATCH_ROOT': str(self.root)}):
                self.assertEqual(project.authorize_contract([self.prefix, SOURCE]), tracked)
            # Both spellings at once would make the authorized root ambiguous.
            with patch.dict(os.environ, {'AGENT_VM_SCRATCH_ROOT': str(self.root),
                                         'AGENT_VM_FIXTURE_ROOT': str(self.root)}):
                with self.assertRaises(ValueError):
                    project.authorize_contract([self.prefix])
            # AGENT_VM_SCRATCH_ROOT already grants the same contract-redirect
            # capability as a fixture root (the environment is caller-controlled), so
            # a contract copy inside the authorized root is accepted.
            local = self.root / 'recipe-contract'
            local.mkdir()
            (local / 'pnpm-lock.mjs').write_text('// module\n')
            with patch.dict(os.environ, {'AGENT_VM_SCRATCH_ROOT': str(self.root),
                                         'AGENT_VM_CONTRACT_DIR': str(local)}):
                self.assertEqual(project.authorize_contract([self.prefix]), local)
            with patch.dict(os.environ, {'AGENT_VM_CONTRACT_DIR': str(tracked)}):
                self.assertEqual(project.authorize_contract([SOURCE]), tracked)

    def test_signal_during_publication_rolls_back_inclusively(self):
        before = self.image()
        for failure in range(4):
            calls = []
            def replace(source, destination):
                calls.append(destination.name)
                if len(calls) == failure + 1:
                    raise KeyboardInterrupt('injected interrupt')
                os.replace(source, destination)
            with self.assertRaises(SystemExit) as raised:
                project.publish(self.stage, self.prefix, replace=replace)
            self.assertEqual(raised.exception.code, 130)
            self.assertEqual(self.image(), before)
            self.assertEqual(calls[failure + 1:], list(reversed(project.INPUTS[:failure + 1])))
            self.assertFalse(list(self.root.glob('dsh-*')))
            self.assertFalse(list(self.prefix.glob('.package.json-*')))
        # An incomplete rollback after a signal is status 3 with the recovery path.
        calls = []
        def fail_restore(source, destination):
            calls.append(destination.name)
            if len(calls) == 1:
                raise KeyboardInterrupt('injected interrupt')
            raise OSError('injected restore failure')
        with self.assertRaises(project.PublicationFailure) as raised:
            project.publish(self.stage, self.prefix, replace=fail_restore)
        self.assertEqual(raised.exception.status, 3)
        self.assertTrue(raised.exception.recovery.is_absolute())
        self.assertIn(str(raised.exception.recovery), str(raised.exception))
        shutil.rmtree(raised.exception.recovery)

    def test_real_signal_between_replaces_never_leaves_a_mixed_prefix(self):
        # The CLI has no fault seam, so widen the publication window with large
        # inputs and interrupt the real process: the prefix must be fully old or
        # fully new, and an interrupted run must exit with the signal status.
        body = 'x' * (8 * 1024 * 1024)
        for filename in project.INPUTS:
            (self.prefix / filename).write_text('old' + filename + body)
            (self.stage / filename).write_text('new' + filename + body)
        old = self.image()
        new = [(self.stage / f).read_bytes() for f in project.INPUTS]
        interrupted = 0
        for attempt in range(8):
            process = subprocess.Popen([sys.executable, str(SOURCE), 'publish', str(self.stage), str(self.prefix)],
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            time.sleep(0.05 + 0.05 * attempt)
            process.terminate()
            stdout, stderr = process.communicate(timeout=60)
            current = self.image()
            self.assertIn(current, (old, new), 'a signal left a mixed prefix')
            self.assertFalse(list(self.prefix.glob('.package.json-*')))
            status = process.returncode
            signal_status = status in (143, -signal.SIGTERM)
            if current == new:
                # The publication had already finished; a late signal may still kill
                # the process, but nothing was left half-written.
                self.assertIn(status, (0, 143, -signal.SIGTERM), stderr)
            else:
                self.assertTrue(signal_status or status == 3, (status, stderr))
                self.assertEqual(stdout, b'')
                if status == 3:
                    # Incomplete recovery must name its absolute recovery root.
                    self.assertIn(b'recovery root: ' + str(self.root).encode(), stderr)
                interrupted += 1
            # Restore the old generation for the next attempt.
            for index, filename in enumerate(project.INPUTS):
                (self.prefix / filename).write_bytes(old[index])
                (self.stage / filename).write_bytes(new[index])
            for leftover in self.root.glob('dsh-*'):
                shutil.rmtree(leftover)
        self.assertGreater(interrupted, 0, 'no attempt was interrupted')

    def test_caller_config_is_ignored_but_project_config_is_rejected(self):
        home = self.root / 'caller-config'
        home.mkdir()
        config = home / '.config'
        (home / '.npmrc').write_text('registry=https://caller.invalid')
        (config / 'pnpm').mkdir(parents=True)
        (config / 'pnpm/rc').write_text('registry=https://caller.invalid')
        with patch.dict(os.environ, {'HOME':str(home), 'XDG_CONFIG_HOME':str(config)}):
            # Every manager call owns a private HOME/XDG, so caller configuration is
            # never read. Rejecting it would refuse ordinary developer hosts.
            project.guard_environment([self.prefix])
            self.assertTrue((home / '.npmrc').is_file())
            self.assertTrue((config / 'pnpm/rc').is_file())
            for key, value in [('NODE_OPTIONS','--require=/does/not/exist'), ('NODE_PATH','/bad'),
                               ('BUN_OPTIONS','bad'), ('NODE_EXTRA_CA_CERTS','/bad'),
                               ('SSL_CERT_FILE','/bad'), ('SSL_CERT_DIR','/bad'),
                               ('NODE_TLS_REJECT_UNAUTHORIZED','0'), ('npm_config_registry','https://bad'),
                               ('PNPM_HOME','/bad'), ('corepack_home','/bad')]:
                with patch.dict(os.environ, {key:value}), self.assertRaises(ValueError):
                    project.guard_environment([self.prefix])
            for filename in ('.npmrc', '.pnpmfile.cjs', '.pnpmfile.js', '.pnpmfile.mjs', 'pnpmfile.cjs'):
                (self.prefix / filename).write_text('registry=https://unreviewed.invalid')
                with self.assertRaises(ValueError):
                    project.guard_environment([self.prefix])
                (self.prefix / filename).unlink()

    def base_distribution(self):
        # A fixture copy of the installed base manager: launcher bytes, aliases,
        # pristine bundle and the pin that authenticates them.
        base = self.root / 'base'
        if base.exists():
            shutil.rmtree(base)
        (base / 'usr/local/bin').mkdir(parents=True)
        (base / 'usr/bin').mkdir()
        (base / 'opt/agent-vm/pnpm/dist').mkdir(parents=True)
        (base / 'opt/agent-vm/pnpm/bin').mkdir()
        # mkdtemp is 0700; a traversable fixture root is required by the audit.
        self.root.chmod(0o755)
        reviewed = SOURCE.parents[2] / 'pnpm'
        source = self.root / 'source'
        shutil.rmtree(source, ignore_errors=True)
        source.mkdir()
        for name in ('pin.json', 'launcher.sh', 'bootstrap.py'):
            shutil.copyfile(reviewed / name, source / name)
        launcher = (source / 'launcher.sh').read_bytes()
        (base / 'usr/local/bin/pnpm').write_bytes(launcher)
        (base / 'usr/local/bin/pnpm').chmod(0o755)
        for alias in ('pnx', 'pnpx'):
            (base / 'usr/local/bin' / alias).symlink_to('pnpm')
        (base / 'usr/bin/node').write_text('#!/bin/sh\nexit 0\n')
        (base / 'usr/bin/node').chmod(0o755)
        (base / 'opt/agent-vm/pnpm/pin.json').write_bytes((source / 'pin.json').read_bytes())
        bundle = b'// authenticated bundle\n'
        (base / 'opt/agent-vm/pnpm/dist/pnpm.mjs').write_bytes(bundle)
        (base / 'opt/agent-vm/pnpm/bin/pnpm.mjs').write_text('// entry\n')
        for directory, dirs, files in os.walk(base):
            Path(directory).chmod(0o755)
            for name in files:
                item = Path(directory) / name
                if not item.is_symlink():
                    item.chmod(0o644)
        # Every fixture file above is normalized to 0644; the two executables are not.
        (base / 'usr/local/bin/pnpm').chmod(0o755)
        (base / 'usr/bin/node').chmod(0o755)
        # Re-pin the fixture bundle in both the reviewed source and the installed
        # copy so the only failure under test is the mutation.
        for pin_path in (source / 'pin.json', base / 'opt/agent-vm/pnpm/pin.json'):
            pin = json.loads(pin_path.read_text())
            pin['bundleSha256'] = hashlib.sha256(bundle).hexdigest()
            pin_path.write_text(json.dumps(pin))
            pin_path.chmod(0o644)
        return base, source

    def verify(self, base, source):
        project.verify_base_distribution(commands=base / 'usr/local/bin', prefix=base / 'opt/agent-vm/pnpm',
                                         node=base / 'usr/bin/node', source=source, fixture_root=self.root)

    def test_fixture_base_launcher_requires_full_distribution(self):
        cases = {
            'pristine pnpm distribution changed': lambda base: (base / 'opt/agent-vm/pnpm/dist/pnpm.mjs').write_text('// raw shadow\n'),
            'pnpm launcher changed': lambda base: (base / 'usr/local/bin/pnpm').write_text('#!/bin/sh\nexec pnpm "$@"\n'),
            'pnpm alias must link relatively to pnpm': lambda base: (base / 'usr/local/bin/pnpx').unlink() or (base / 'usr/local/bin/pnpx').write_text('#!/bin/sh\n'),
            'launcher mode must be 0755': lambda base: (base / 'usr/local/bin/pnpm').chmod(0o7755),
            'group/other-writable distribution path': lambda base: (base / 'opt/agent-vm/pnpm/dist/pnpm.mjs').chmod(0o666),
            'unexpected symlinked distribution entry': lambda base: (base / 'opt/agent-vm/pnpm/dist/alias.mjs').symlink_to('pnpm.mjs'),
            'unreadable distribution path': lambda base: (base / 'usr/bin/node').chmod(0o600),
        }
        for diagnostic, mutate in cases.items():
            base, source = self.base_distribution()
            self.verify(base, source)
            mutate(base)
            with self.assertRaises(ValueError) as raised:
                self.verify(base, source)
            self.assertIn(diagnostic, str(raised.exception), diagnostic)
        # A same-version raw shadow: the installed pin claims the same version but
        # disagrees with the reviewed source, so its bundle is not authenticated.
        base, source = self.base_distribution()
        shadow = json.loads((base / 'opt/agent-vm/pnpm/pin.json').read_text())
        shadow['bundleSha256'] = hashlib.sha256(b'shadow').hexdigest()
        (base / 'opt/agent-vm/pnpm/pin.json').write_text(json.dumps(shadow))
        with self.assertRaises(ValueError) as raised:
            self.verify(base, source)
        self.assertIn('installed pin disagrees with reviewed source', str(raised.exception))

    def test_fixture_base_launcher_seam_is_fixed_and_path_free(self):
        capability = project.authorize_mutation(self.prefix)
        fake_path = self.root / 'path-shadow'
        fake_path.mkdir()
        shadow = fake_path / 'pnpm'
        shadow.write_text('#!/bin/sh\nexit 9\n')
        shadow.chmod(0o755)
        with patch.dict(os.environ, {'PATH':str(fake_path), 'AGENT_VM_FIXTURE_BASE_PNPM':'1'},
                        clear=False):
            with patch.object(project, 'verify_base_distribution') as verified:
                self.assertEqual(project.manager_argv(capability), ['/usr/local/bin/pnpm'])
                verified.assert_called_once_with()
        # Without a successful verification the fixed path is never returned.
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_BASE_PNPM':'1'}):
            with patch.object(project, 'verify_base_distribution', side_effect=ValueError('unverified')):
                with self.assertRaises(ValueError) as raised:
                    project.manager_argv(capability)
        self.assertIn('unverified', str(raised.exception))
        for ambiguous in ({'AGENT_VM_FIXTURE_BASE_PNPM':'1', 'AGENT_VM_FIXTURE_PNPM':'/bin/sh'},
                          {'AGENT_VM_FIXTURE_BASE_PNPM':'1', 'AGENT_VM_DSH_PNPM_PACKAGE':str(self.root)}):
            with patch.dict(os.environ, ambiguous), self.assertRaises(ValueError) as raised:
                project.manager_argv(capability)
            self.assertIn('ambiguous fixture manager selection', str(raised.exception))

    @unittest.skipUnless((project.PNPM_PREFIX / 'pin.json').is_file(), 'installed base distribution runs in the native base image')
    def test_installed_base_launcher_authenticates(self):
        project.verify_base_distribution()
        capability = project.authorize_mutation(self.prefix)
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_BASE_PNPM':'1'}):
            self.assertEqual(project.manager_argv(capability), ['/usr/local/bin/pnpm'])


    def test_runtime_cannot_mutate_or_use_foreign_home(self):
        home = self.root / 'home'
        home.mkdir()
        command = self.prefix / 'node_modules/dsh/bin.js'
        command.parent.mkdir(parents=True)
        command.write_text('#!/usr/bin/env node\n')
        command.chmod(0o755)
        link = self.root / 'dsh'
        link.symlink_to(command)
        with patch.dict(os.environ, {'HOME':str(home)}):
            capability = project.authorize_runtime(self.prefix, link, home)
            self.assertFalse(capability.mutation)
            self.assertIsInstance(capability, project.RuntimeAuthorization)
            with self.assertRaises(ValueError):
                project.manager_argv(capability)
            scratch = project.owned_scratch(capability, home)
            project.cleanup_scratch(scratch)
            for options in ({'mutation':True}, {'status_dir':self.root / 'status'}):
                with self.assertRaises(ValueError):
                    project.authorize_runtime(self.prefix, link, home, **options)
            with self.assertRaises(ValueError):
                project.authorize_runtime(self.prefix, link, self.stage)
        (self.prefix / 'node_modules/away').symlink_to(self.stage, target_is_directory=True)
        (self.stage / 'back').symlink_to(command.parent, target_is_directory=True)
        link.unlink()
        link.symlink_to(self.prefix / 'node_modules/away/back/bin.js')
        self.assertEqual(link.resolve(), command)
        with patch.dict(os.environ, {'HOME':str(home)}), self.assertRaises(ValueError):
            project.authorize_runtime(self.prefix, link, home)
        link.unlink()
        link.symlink_to('/bin/sh')
        with patch.dict(os.environ, {'HOME':str(home)}), self.assertRaises(ValueError):
            project.authorize_runtime(self.prefix, link, home)

    @unittest.skipUnless(os.getuid() == 0 and hasattr(os, 'setuid'), 'real UID boundary runs in native Linux base')
    def test_root_installed_payload_numeric_uid(self):
        self.root.chmod(0o755)
        self.prefix.chmod(0o755)
        for filename in project.INPUTS:
            (self.prefix / filename).chmod(0o644)
        (self.root / 'pnpm').chmod(0o755)
        (self.root / 'pnpm/pin.json').chmod(0o644)
        home = self.root / 'runtime-homes/u12345g23456'
        home.mkdir(parents=True)
        home.parent.chmod(0o755)
        os.chown(home, 12345, 23456)
        command = self.prefix / 'command'
        command.write_text('#!/bin/sh\nexit 0\n')
        command.chmod(0o755)
        link = self.root / 'dsh'
        link.symlink_to(command)
        before = self.image()
        pid = os.fork()
        if pid == 0:
            try:
                os.setgroups([])
                os.setgid(23456)
                os.setuid(12345)
                os.environ['HOME'] = str(home)
                capability = project.authorize_runtime(self.prefix, link, home)
                scratch = project.owned_scratch(capability, home)
                (scratch / 'owned').write_text('runtime only')
                project.cleanup_scratch(scratch)
                try:
                    project.authorize_mutation(self.prefix)
                except ValueError:
                    pass
                else:
                    raise AssertionError('runtime acquired mutation capability')
                try:
                    project.authorize_runtime(self.prefix, link, self.stage)
                except ValueError:
                    pass
                else:
                    raise AssertionError('runtime acquired foreign HOME')
                os._exit(0)
            except Exception as error:
                print(error, file=sys.stderr, flush=True)
                os._exit(1)
        self.assertEqual(os.waitpid(pid, 0)[1], 0)
        self.assertEqual(self.image(), before)
        self.assertEqual(self.prefix.stat().st_uid, 0)


if __name__ == '__main__':
    unittest.main(verbosity=2)
