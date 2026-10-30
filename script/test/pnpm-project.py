#!/usr/bin/env python3
"""Shared pnpm project capability table, bootstrap stage and transaction table.

`script/test/dsh-project.py` owns dsh's behaviour through this module; this suite
owns the shared questions that only exist because three consumers use it: the
closed consumer table, capabilities carrying their consumer, the bootstrap stage
authorization that does not require the project it creates, and the four-input
transaction's absent-destination and mode behaviour.

Syscall faults are injected through the module's own seams; there is no
environment variable that grants authority.
"""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / 'images/tools/pnpm-project.py'
ADAPTER = ROOT / 'images/tools/dsh/project.py'
HOST_MANAGER = ROOT / 'images/tools/pnpm-host-manager.py'
HOST_ADAPTER = ROOT / 'images/tools/dsh/host-manager.py'


def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


project = load_module(SOURCE, 'pnpm_project')
adapter = load_module(ADAPTER, 'dsh_project')


def recipe_contract_helper():
    cached = os.environ.get('RECIPE_CONTRACT_CONTEXT')
    if cached and (Path(cached) / 'out/recipe-contract').is_file():
        return Path(cached) / 'out/recipe-contract'
    result = subprocess.run(['bash', str(ROOT / 'script/build/recipe-contract.sh'), '--native'],
                            capture_output=True, text=True)
    if result.returncode:
        raise RuntimeError('cannot build the native recipe-contract helper: '
                           + (result.stderr.strip() or 'no diagnostic'))
    return Path(result.stdout.strip()) / 'out/recipe-contract'


class CapabilityTests(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp(prefix='pnpm-project-')).resolve()
        self.root.chmod(0o755)
        self.contract = self.root / 'recipe-contract'
        self.contract.mkdir()
        shutil.copyfile(recipe_contract_helper(), self.contract / 'recipe-contract')
        (self.contract / 'recipe-contract').chmod(0o755)
        self.env = patch.dict(os.environ, {'AGENT_VM_FIXTURE_ROOT': str(self.root),
                                           'AGENT_VM_CONTRACT_DIR': str(self.contract)})
        self.env.start()
        (self.root / 'pnpm').mkdir()
        (self.root / 'pnpm/pin.json').write_text('{}')
        self.prefix = self.make_prefix('prefix', 'old')
        self.stage = self.make_prefix('stage', 'new')

    def tearDown(self):
        self.env.stop()
        for path in (self.root,) + tuple(self.root.parent.glob(self.root.name + '-*')):
            shutil.rmtree(path, ignore_errors=True)

    def make_prefix(self, name, body):
        prefix = self.root / name
        prefix.mkdir()
        for filename in project.INPUTS:
            (prefix / filename).write_text(body + filename)
        return prefix

    def empty_prefix(self, name):
        prefix = self.root / name
        prefix.mkdir()
        return prefix

    def image(self, prefix):
        return [(prefix / filename).read_bytes() for filename in project.INPUTS]

    # --- the closed consumer table ------------------------------------------

    def test_consumer_table_is_closed(self):
        self.assertEqual(sorted(project.CONSUMERS), ['bridge', 'dsh', 'pi'])
        profiles = list(project.CONSUMERS.values())
        self.assertEqual({item.production_prefix for item in profiles},
                         {Path('/opt/agent-vm/dsh'), Path('/opt/agent-vm/pi'),
                          Path('/opt/agent-vm/pi-packages')})
        self.assertEqual(len({item.scratch_token for item in profiles}), 3)
        self.assertEqual(len({item.host_env for item in profiles}), 3)
        self.assertTrue(all(item.scratch_parent == Path('/tmp') for item in profiles))
        self.assertIs(project.CONSUMERS['dsh'], project.DSH)
        self.assertEqual(project.PRODUCTION, Path('/opt/agent-vm/dsh'))
        for name in ('npm', 'copilot', '', None, 1, Path('dsh')):
            with self.assertRaises(ValueError):
                project.consumer_of(name)
        with self.assertRaises(Exception):
            project.DSH.name = 'pi'
        # A caller-built profile with dsh's name but another prefix is not the
        # reviewed dsh: the table is the only source of production paths.
        forged = project.Consumer('dsh', self.root, None, 'dsh-', 'AGENT_VM_DSH', Path('/tmp'))
        with self.assertRaises(ValueError):
            project.consumer_of(forged)
        self.assertIs(project.consumer_of('pi'), project.CONSUMERS['pi'])

    def test_capabilities_carry_their_consumer(self):
        for name in ('dsh', 'pi', 'bridge'):
            capability = project.authorize_mutation(self.prefix, consumer=name)
            self.assertIs(capability.consumer, project.CONSUMERS[name])
            self.assertTrue(capability.mutation)
            scratch = project.owned_scratch(capability)
            self.assertTrue(scratch.name.startswith(project.CONSUMERS[name].scratch_token))
            # A scratch belongs to exactly one consumer's token.
            other = 'pi' if name != 'pi' else 'bridge'
            with self.assertRaises(ValueError):
                project.remove_scratch(scratch, consumer=other)
            project.cleanup_scratch(scratch)
        # The default remains dsh so the legacy adapter is unchanged.
        self.assertIs(project.authorize_mutation(self.prefix).consumer, project.DSH)

    def test_production_prefixes_belong_to_one_consumer(self):
        for name, profile in project.CONSUMERS.items():
            # Under its own consumer the prefix is production, not a fixture
            # child; under any other consumer it is neither.
            with self.assertRaises(ValueError) as raised:
                project.authorize_mutation(profile.production_prefix, consumer=name)
            self.assertNotIn('strict child of authorized root', str(raised.exception))
            for other in (item for item in project.CONSUMERS if item != name):
                with self.assertRaises(ValueError) as rejected:
                    project.authorize_mutation(profile.production_prefix, consumer=other)
                self.assertIn('strict child of authorized root', str(rejected.exception))

    def test_guard_and_contract_follow_the_consumer(self):
        # The environment checker authorizes a production prefix only for the
        # consumer that owns it. Every other consumer sees a fixture path, and a
        # production prefix outside the fixture root is refused as one.
        self.assertEqual(project.authorize_contract([self.prefix], consumer='pi'), self.contract)
        for name, profile in project.CONSUMERS.items():
            with self.assertRaises(project.ContractFailure, msg=name):
                project.authorize_contract([profile.production_prefix], consumer=name)
        with self.assertRaises(ValueError) as raised:
            project.authorize_contract([project.PRODUCTION], consumer='pi')
        self.assertIn('strict child of authorized root', str(raised.exception))
        # A caller CA bundle is refused for every consumer before Node starts.
        with patch.dict(os.environ, {'SSL_CERT_FILE': str(self.root / 'ca')}):
            for name in project.CONSUMERS:
                with self.assertRaises(ValueError):
                    project.guard_environment([self.prefix], consumer=name)

    def test_host_acquisition_variables_are_per_consumer(self):
        executable = self.root / 'pnpm-fixture'
        executable.write_text('#!/bin/sh\nexit 0\n')
        executable.chmod(0o755)
        dsh = project.authorize_mutation(self.prefix, consumer='dsh')
        pi = project.authorize_mutation(self.prefix, consumer='pi')
        with patch.dict(os.environ, {'AGENT_VM_PI_PNPM_PACKAGE': str(executable),
                                     'AGENT_VM_DSH_PNPM_PACKAGE': str(executable)}):
            # Only the capability's own variable selects the host route; the other
            # consumer's variable is inert rather than a second manager seam.
            with patch.object(project, 'authenticated_host_argv', return_value=['host']) as acquired:
                self.assertEqual(project.manager_argv(dsh), ['host'])
                self.assertEqual(acquired.call_args.args[1], str(executable))
        with patch.dict(os.environ, {'AGENT_VM_DSH_PNPM_PACKAGE': str(executable)}):
            with self.assertRaises(ValueError) as raised:
                project.manager_argv(pi)
            self.assertIn('fixture requires an authorized fake', str(raised.exception))
        with patch.dict(os.environ, {'AGENT_VM_PI_PNPM_PACKAGE': str(executable),
                                     'AGENT_VM_FIXTURE_PNPM': str(executable)}):
            with self.assertRaises(ValueError) as raised:
                project.manager_argv(pi)
            self.assertIn('ambiguous fixture manager selection', str(raised.exception))

    def test_host_manager_entry_is_consumer_scoped(self):
        # No network: a PATH `curl` writes bytes that cannot match the reviewed
        # pin, so the acquisition must stop at authentication, before extraction.
        # This is the only test of the host-bootstrap adapter and its ordering.
        bin_dir = self.root / 'curl-bin'
        bin_dir.mkdir()
        (bin_dir / 'curl').write_text('#!/bin/sh\n'
                                      'while [ "$#" -gt 0 ]; do\n'
                                      '    case $1 in -o) shift; output=$1 ;; esac\n'
                                      '    shift\n'
                                      'done\n'
                                      "printf 'not the authenticated distribution\\n' >\"$output\"\n")
        (bin_dir / 'curl').chmod(0o755)
        env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + '/usr/bin:/bin',
                   HOME=str(self.root))
        prefix = self.make_prefix('host-prefix', 'host')
        run = subprocess.run([sys.executable, str(HOST_ADAPTER), str(prefix)],
                             capture_output=True, env=env, text=True)
        self.assertEqual(run.returncode, 1, run.stdout + run.stderr)
        self.assertIn('archive SHA-512 mismatch', run.stderr)
        destination = prefix.parent / 'manager'
        self.assertTrue(destination.is_dir())
        self.assertFalse((destination / 'package').exists())
        shutil.rmtree(destination)
        # The shared entry names the consumer explicitly; the adapter does not
        # accept the flag and fixes dsh.
        shared = subprocess.run([sys.executable, str(HOST_MANAGER), '--consumer', 'pi',
                                 str(self.make_prefix('host-pi', 'host'))],
                                capture_output=True, env=env, text=True)
        self.assertEqual(shared.returncode, 1, shared.stdout + shared.stderr)
        self.assertIn('archive SHA-512 mismatch', shared.stderr)
        for argv in ([sys.executable, str(HOST_MANAGER), str(prefix)],
                     [sys.executable, str(HOST_ADAPTER), '--consumer', 'pi', str(prefix)]):
            refused = subprocess.run(argv, capture_output=True, env=env, text=True)
            self.assertEqual(refused.returncode, 2, (argv, refused.stderr))

    def test_production_refuses_host_acquisition(self):
        # Production owns the launcher, so the developer variables are refused
        # rather than silently ignored. The capability is built in-process: the
        # assertion is about the branch, not about a host gaining authority.
        capability = project.MutationAuthorization(
            project.PRODUCTION, Path('/opt/agent-vm'), Path('/opt/agent-vm/pnpm/pin.json'),
            True, None, Path('/opt/agent-vm/install-status'), project.DSH)
        with patch.dict(os.environ, {'AGENT_VM_DSH_PNPM_PACKAGE': '/somewhere/pnpm'}):
            with self.assertRaises(ValueError) as raised:
                project.manager_argv(capability)
            self.assertIn('production does not use host manager acquisition', str(raised.exception))
        with patch.dict(os.environ, {}):
            self.assertEqual(project.manager_argv(capability), ['/usr/local/bin/pnpm'])

    # --- bootstrap stage authorization --------------------------------------

    def test_stage_authorization_is_private_and_disjoint(self):
        stage = self.root / 'work' / 'candidate'
        stage.parent.mkdir()
        capability = project.authorize_stage(stage, consumer='pi')
        self.assertIsInstance(capability, project.BootstrapAuthorization)
        self.assertFalse(capability.mutation)
        self.assertNotIsInstance(capability, project.MutationAuthorization)
        self.assertEqual(capability.stage, stage)
        self.assertEqual(capability.prefix, stage)
        self.assertIs(capability.consumer, project.CONSUMERS['pi'])
        self.assertEqual(json.loads(json.dumps(capability.json())),
                         {'stage': str(stage), 'root': str(self.root),
                          'basePin': str(self.root / 'pnpm/pin.json'), 'production': False,
                          'consumer': 'pi'})
        # An unavailable stage leaf is the bootstrap case; its parent must exist.
        with self.assertRaises(ValueError):
            project.authorize_stage(self.root / 'absent-parent' / 'candidate', consumer='pi')
        # A stage cannot install payload, start a manager or write a status record.
        for verb in (project.remove_partial_install, project.manager_argv):
            with self.assertRaises(ValueError):
                verb(capability)
        # Private work children stay inside the stage and clean up normally.
        stage.mkdir(mode=0o700)
        scratch = project.owned_scratch(capability)
        self.assertEqual(scratch.parent, stage)
        self.assertTrue(scratch.name.startswith('pi-'))
        with self.assertRaises(ValueError):
            project.owned_scratch(capability, self.root)
        project.cleanup_scratch(scratch)

    def test_stage_manager_route_is_closed(self):
        stage = self.root / 'manager-stage'
        stage.mkdir(mode=0o700)
        capability = project.authorize_stage(stage, consumer='pi')
        # The ordinary manager verb still refuses a bootstrap capability: a first
        # import has no reviewed inputs and so cannot hold mutation authority.
        with self.assertRaises(ValueError):
            project.manager_argv(capability)
        # A fixture needs an authorized fake or the authenticated base launcher.
        with self.assertRaises(ValueError):
            project.stage_manager_argv(capability)
        with self.assertRaises(ValueError):
            project.stage_manager_argv(project.authorize_mutation(self.prefix, consumer='pi'))
        fake = self.root / 'fake-pnpm'
        fake.write_text('#!/bin/sh\nexit 0\n')
        fake.chmod(0o755)
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_PNPM': str(fake)}):
            self.assertEqual(project.stage_manager_argv(capability), [str(fake)])
            # The stage route may not borrow the consumer's host acquisition
            # variables either; ambiguity fails instead of picking one.
            with patch.dict(os.environ, {'AGENT_VM_PI_PNPM_PACKAGE': str(self.root / 'stock')}):
                with self.assertRaises(ValueError):
                    project.stage_manager_argv(capability)
        with patch.dict(os.environ, {'AGENT_VM_FIXTURE_PNPM': '',
                                     'AGENT_VM_FIXTURE_BASE_PNPM': '1'}):
            # The stock route authenticates the whole installed distribution; this
            # host has none, so it fails instead of falling back to a PATH lookup.
            with self.assertRaises((ValueError, OSError)):
                project.stage_manager_argv(capability)

    def test_stage_authorization_refusals(self):
        inside = self.root / 'work'
        inside.mkdir()
        stage = inside / 'stage'
        outside = self.root.parent / (self.root.name + '-outside')
        outside.mkdir()
        self.addCleanup(shutil.rmtree, outside, True)
        (self.root / 'link').symlink_to(outside, target_is_directory=True)
        link_stage = self.root / 'linked-stage'
        link_stage.symlink_to(outside, target_is_directory=True)
        # 0700 is the private boundary; a traversable stage is not accepted.
        loose = self.root / 'loose'
        loose.mkdir(mode=0o755)
        os.chmod(loose, 0o755)
        file_stage = self.root / 'not-a-dir'
        file_stage.write_text('x')
        cases = {
            'stage root itself': self.root,
            'stage outside the authorized root': outside / 'stage',
            'base pin': self.root / 'pnpm/pin.json',
            'base pin parent': self.root / 'pnpm',
            'status destination': self.root / 'install-status',
            'contract directory': self.contract,
            'tool source checkout': project.checkout_root(),
            'reviewed pnpm sources': project.reviewed_pnpm_source(),
            'each production prefix': next(iter(project.CONSUMERS.values())).production_prefix,
            'symlinked stage': link_stage,
            'symlinked ancestor': self.root / 'link' / 'stage',
            'traversable stage': loose,
            'stage is a file': file_stage,
        }
        for label, path in cases.items():
            with self.assertRaises((ValueError, project.ContractFailure), msg=label):
                project.authorize_stage(path, consumer='dsh')
        self.assertTrue(outside.is_dir())
        # A caller-named source is protected too: staging over it would let a
        # bootstrap publish into the project it is bootstrapping.
        source = inside / 'project'
        source.mkdir()
        for target in (source, source / 'candidate'):
            with self.assertRaises(ValueError):
                project.authorize_stage(target, consumer='dsh', sources=[source])
        project.authorize_stage(stage, consumer='dsh', sources=[source])
        with self.assertRaises(ValueError):
            project.authorize_stage(stage, consumer='forged')

    def test_stage_cli_and_closed_consumer_flag(self):
        stage = self.root / 'cli-stage'
        stage.mkdir(mode=0o700)
        result = subprocess.run([sys.executable, str(SOURCE), '--consumer', 'bridge',
                                 'authorize-stage', str(stage)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)['consumer'], 'bridge')
        for argv in ([sys.executable, str(SOURCE), 'authorize-stage', str(stage)],
                     [sys.executable, str(SOURCE), '--consumer', 'npm', 'authorize-stage', str(stage)]):
            refused = subprocess.run(argv, capture_output=True)
            self.assertEqual(refused.returncode, 2, argv)
            self.assertEqual(refused.stdout, b'')
        # dsh's adapter keeps its old argv with the dsh consumer.
        legacy = subprocess.run([sys.executable, str(ADAPTER), 'authorize-stage', str(stage)],
                                capture_output=True)
        self.assertEqual(legacy.returncode, 0, legacy.stderr)
        self.assertEqual(json.loads(legacy.stdout)['consumer'], 'dsh')

    # --- the four-input transaction -----------------------------------------

    def test_publication_creates_absent_inputs(self):
        fresh = self.empty_prefix('fresh')
        project.publish(self.stage, fresh, consumer='pi')
        self.assertEqual(self.image(fresh), self.image(self.stage))
        self.assertFalse(list(self.root.glob('pi-*')))
        # A second publication over the now-present inputs is an ordinary replace.
        project.publish(self.prefix, fresh, consumer='bridge')
        self.assertEqual(self.image(fresh), self.image(self.prefix))

    def test_absent_destinations_roll_back_to_absence(self):
        for absent in (0, 3, (0, 1)):
            positions = (absent,) if isinstance(absent, int) else absent
            fresh = self.empty_prefix('fresh-' + '-'.join(map(str, positions)))
            for filename in project.INPUTS:
                if project.INPUTS.index(filename) not in positions:
                    (fresh / filename).write_text('present ' + filename)
                    (fresh / filename).chmod(0o640)
            before = [(fresh / name).read_bytes() if (fresh / name).exists() else None
                      for name in project.INPUTS]
            calls = []
            def replace(source, destination):
                calls.append(destination.name)
                if len(calls) == 3:
                    destination.write_text('partial')
                    raise OSError('injected publish failure')
                os.replace(source, destination)
            with self.assertRaises(project.PublicationFailure) as raised:
                project.publish(self.stage, fresh, replace=replace, consumer='dsh')
            self.assertEqual(raised.exception.status, 1)
            # Attempts are recorded before each write, so the rollback is exactly
            # the reverse of the attempted destinations. A destination that did
            # not exist is removed instead of being restored, so it produces no
            # copy step; the ones that did exist are restored in reverse order.
            restored = [name for name, original in zip(project.INPUTS[:3], before[:3]) if original]
            self.assertEqual(calls[3:], list(reversed(restored)))
            for name, original in zip(project.INPUTS, before):
                target = fresh / name
                if original is None:
                    self.assertFalse(target.exists(), name)
                else:
                    self.assertEqual(target.read_bytes(), original, name)
                    self.assertEqual(stat.S_IMODE(target.stat().st_mode), 0o640, name)
            self.assertFalse(list(self.root.glob('dsh-*')))

    def test_incomplete_rollback_keeps_the_whole_recovery(self):
        fresh = self.empty_prefix('fresh-incomplete')
        for name in project.INPUTS[:2]:
            (fresh / name).write_text('present ' + name)
            (fresh / name).chmod(0o640)
        calls = []
        def replace(source, destination):
            index = len(calls)
            calls.append(destination.name)
            # Third call: the forward write of the third (absent) destination
            # fails. Fourth: restoring the second present destination fails, so
            # the rollback is incomplete and its recovery material must survive.
            if index in (2, 3):
                destination.write_text('partial')
                raise OSError('injected publish/restore failure')
            os.replace(source, destination)
        with self.assertRaises(project.PublicationFailure) as raised:
            project.publish(self.stage, fresh, replace=replace, consumer='bridge')
        error = raised.exception
        self.assertEqual(error.status, 3)
        self.assertTrue(error.recovery.is_absolute())
        self.assertIn(str(error.recovery), str(error))
        inventory = json.loads((error.recovery / 'inventory.json').read_text())
        self.assertEqual([entry['existed'] for entry in inventory], [True, True, False, False])
        self.assertEqual([entry['mode'] for entry in inventory], [0o640, 0o640, None, None])
        self.assertTrue(all(Path(entry['staged']).is_file() for entry in inventory))
        self.assertTrue(Path(inventory[0]['backup']).is_file())
        self.assertFalse(Path(inventory[2]['backup']).exists())
        project.cleanup_scratch(error.recovery)

    def test_publication_rechecks_the_destination_only(self):
        # The publication destination no longer has to hold complete inputs, but
        # it must still be the authorized path: a destination that is redirected
        # between writes is refused before the next write, and the incomplete
        # rollback keeps its recovery material instead of guessing.
        fresh = self.empty_prefix('fresh-replaced')
        outside = self.root.parent / (self.root.name + '-moved')
        outside.mkdir()
        self.addCleanup(shutil.rmtree, outside, True)
        calls = []
        def replace(source, destination):
            calls.append(destination.name)
            os.replace(source, destination)
            if len(calls) == 1:
                fresh.rename(outside / 'fresh')
                fresh.symlink_to(outside / 'fresh', target_is_directory=True)
        with self.assertRaises(project.PublicationFailure) as raised:
            project.publish(self.stage, fresh, replace=replace, consumer='dsh')
        self.assertEqual(raised.exception.status, 3)
        project.cleanup_scratch(raised.exception.recovery)
        self.assertEqual([item.name for item in (outside / 'fresh').iterdir()], [project.INPUTS[0]])

    def test_cli_parity_for_publication(self):
        fresh = self.empty_prefix('fresh-cli')
        result = subprocess.run([sys.executable, str(SOURCE), '--consumer', 'dsh', 'publish',
                                 str(self.stage), str(fresh)], capture_output=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.image(fresh), self.image(self.stage))
        legacy = subprocess.run([sys.executable, str(ADAPTER), 'publish', str(self.prefix),
                                 str(fresh)], capture_output=True)
        self.assertEqual(legacy.returncode, 0, legacy.stderr)
        self.assertEqual(self.image(fresh), self.image(self.prefix))


if __name__ == '__main__':
    unittest.main(verbosity=2)
