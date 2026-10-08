#!/usr/bin/env python3
"""Offline amd64 retention tests, including sticky failure and upload attempts."""
import copy
import importlib.util
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'script/release'))


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


fixtures = load('boot_content_fixtures', ROOT / 'script/test/release-content.py')
import content
import evidence
checker = load('boot_checker', ROOT / 'script/release/check-ci-boot-evidence.py')


class RetentionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.assets = self.root / 'assets'
        self.original = self.assets / 'verify-amd64-one'
        self.original.mkdir(parents=True)
        self.runtime = self.root / 'runtime'
        self.runtime.mkdir()
        (self.runtime / 'kvm.log').write_text('runtime diagnostic')
        (self.runtime / 'z-after.log').write_text('later diagnostic')
        self.stage = self.root / 'evidence'
        # Reuse real release parsers and OCI fixture conventions, not a fake schema.
        fixture_case = fixtures.ContentTests()
        fixture_case.root = self.root
        platforms = [content.PlatformInventory.parse(fixture_case.inventory_for(arch))
                     for arch in ('amd64', 'arm64')]
        self.release = content.ReleaseMetadata(content.parse_version('0.1.0'), 'a' * 40, '1', '1',
                                              content.parse_sha256('sha256:' + 'e' * 64), tuple(platforms))
        content.atomic_json(self.assets / 'release.json', self.release.json())
        content.atomic_json(self.assets / 'platform-amd64.json', platforms[0].json())
        (self.assets / 'SHA256SUMS').write_text('fixture metadata checksums')
        (self.assets / 'release-metadata.sigstore.json').write_text('fixture bundle boundary')
        logs = []
        for name, data in [('first.log', b'first command'), ('second.log', b'second command log')]:
            path = self.original / name
            path.write_bytes(data)
            digest, size = content.hash_file(path)
            logs.append(content.ArchivePart(name, digest, size).json())
        self.record = {'schema_version': 1, 'architecture': 'amd64',
                       'release_subject': content.hash_file(self.assets / 'release.json')[0].value,
                       'platform': platforms[0].json(), 'index_digest': self.release.index_digest.value,
                       'host': 'offline fixture', 'tools': {name: {'version': '1', 'sha256': 'sha256:' + 'f' * 64}
                           for name in ('docker', 'buildx', 'skopeo', 'msb', 'firmware', 'libkrun')},
                       'uid_gid_pairs': ['1000:1000', '12345:23456', '54321:34567'], 'cold_cache': True,
                       'checks': {name: {'status': 0, 'duration_seconds': 0.1,
                                        'logs': ['first.log', 'second.log']} for name in evidence.MANDATORY_CHECKS},
                       'logs': logs}
        self.write_record()

    def write_record(self):
        content.atomic_json(self.original / 'verification-amd64.json', self.record)

    def stage_files(self, env=None):
        return subprocess.run(['bash', str(ROOT / 'script/release/stage-ci-boot-evidence.sh'),
                               str(self.assets), str(self.runtime), '', str(self.stage)],
                              env=env, capture_output=True)

    def validate(self):
        checker.validate_amd64(self.release, self.stage / 'release/release.json',
                              self.assets, self.stage / 'verification')

    def validated_tail(self, verifier_ok=True, stage_status=0):
        validation_ok = False
        if verifier_ok:
            try:
                self.validate()
                validation_ok = True
            except (ValueError, OSError, TypeError, KeyError):
                pass
        # This sentinel models Actions always() after validation, not error masking.
        sentinel = self.root / 'upload-attempted'
        sentinel.touch()
        verdict = verifier_ok and stage_status == 0 and validation_ok
        self.assertTrue(sentinel.is_file())
        return verdict

    def tail(self, verifier_ok=True, env=None):
        staged = self.stage_files(env)
        return self.validated_tail(verifier_ok, staged.returncode), staged

    def test_complete_amd64_only(self):
        verdict, result = self.tail()
        self.assertTrue(verdict, result.stderr)
        self.assertFalse((self.stage / 'verification/verification-arm64.json').exists())

    def test_original_missing_or_multiple(self):
        for multiple in (False, True):
            with self.subTest(multiple=multiple):
                if multiple:
                    self.write_record()
                    other = self.assets / 'verify-amd64-two'
                    other.mkdir()
                    shutil.copyfile(self.original / 'verification-amd64.json', other / 'verification-amd64.json')
                else:
                    (self.original / 'verification-amd64.json').unlink()
                verdict, _ = self.tail()
                self.assertFalse(verdict)

    def test_original_removed_after_staging(self):
        self.assertEqual(self.stage_files().returncode, 0)
        (self.original / 'verification-amd64.json').unlink()
        self.assertFalse(self.validated_tail())

    def test_missing_staged_record(self):
        self.assertEqual(self.stage_files().returncode, 0)
        (self.stage / 'verification/verification-amd64.json').unlink()
        self.assertFalse(self.validated_tail())

    def test_missing_log_before_or_after_copy(self):
        for before in (False, True):
            with self.subTest(before=before):
                if before:
                    (self.original / 'second.log').unlink()
                self.assertEqual(self.stage_files().returncode, 0)
                (self.stage / 'verification/second.log').unlink(missing_ok=True)
                self.assertFalse(self.validated_tail())

    def test_corrupt_record_and_log_hash_and_size(self):
        for name in ('verification-amd64.json', 'second.log'):
            for same_size in (False, True):
                with self.subTest(name=name, same_size=same_size):
                    self.assertEqual(self.stage_files().returncode, 0)
                    path = self.stage / 'verification' / name
                    data = path.read_bytes()
                    path.write_bytes(b'x' * len(data) if same_size else data + b'changed length')
                    self.assertFalse(self.validated_tail())

    def test_invalid_record_fields(self):
        good = copy.deepcopy(self.record)
        mutations = [lambda r: r.update(release_subject='sha256:' + '0' * 64),
                     lambda r: r.update(platform={}), lambda r: r.update(index_digest='sha256:' + '0' * 64),
                     lambda r: r['logs'].append(r['logs'][0]),
                     lambda r: r['logs'][0].update(name='../unsafe.log'),
                     lambda r: r['checks']['runtime-doctor'].update(status=1),
                     lambda r: r['checks']['runtime-doctor'].update(status=False),
                     lambda r: r['checks']['runtime-doctor'].update(logs=['unlisted.log']),
                     lambda r: r['checks']['runtime-doctor'].update(duration_seconds=-1),
                     lambda r: r['checks']['runtime-doctor'].update(duration_seconds=float('nan')),
                     lambda r: r.update(logs=[]), lambda r: r.update(cold_cache=False),
                     lambda r: r.update(uid_gid_pairs=['1000:1000']), lambda r: r.update(tools={}),
                     lambda r: r.update(host=''), lambda r: r.update(extra='unexpected')]
        for number, mutate in enumerate(mutations):
            with self.subTest(mutation=number):
                self.record = copy.deepcopy(good)
                mutate(self.record)
                self.write_record()
                verdict, _ = self.tail()
                self.assertFalse(verdict)

    def copy_environment(self, target, corrupt=False):
        binary = self.root / 'bin'
        binary.mkdir(exist_ok=True)
        script = binary / 'cp'
        # Stub just the copy boundary; all other copies use the real system cp.
        script.write_text('#!' + sys.executable + '\n' +
            'import os,sys,subprocess\nfrom pathlib import Path\n' +
            f'with open({str(self.root / "attempts")!r}, "a") as f: f.write(sys.argv[-2]+"\\n")\n' +
            f'if Path(sys.argv[-2]).name == {target!r}:\n' +
            ('    dest=Path(sys.argv[-1])/Path(sys.argv[-2]).name\n'
             '    dest.write_bytes(b"corrupt but copy succeeded")\n    sys.exit(0)\n' if corrupt else '    sys.exit(9)\n') +
            f'sys.exit(subprocess.call([{shutil.which("cp")!r}]+sys.argv[1:]))\n')
        script.chmod(0o755)
        return dict(os.environ, PATH=str(binary) + os.pathsep + os.environ['PATH'])

    def test_copy_failures_accumulate_and_attempt_later_files(self):
        for target in ('verification-amd64.json', 'second.log', 'kvm.log'):
            with self.subTest(target=target):
                verdict, result = self.tail(env=self.copy_environment(target))
                self.assertNotEqual(result.returncode, 0)
                self.assertFalse(verdict)
                attempts = (self.root / 'attempts').read_text()
                self.assertIn('release.json', attempts)
                self.assertIn('kvm.log', attempts)
                self.assertIn('z-after.log', attempts)
                self.assertIn('verification-amd64.json', attempts)
                (self.root / 'attempts').unlink()

    def test_silent_corrupt_copy_is_rejected(self):
        for target in ('verification-amd64.json', 'second.log'):
            with self.subTest(target=target):
                verdict, result = self.tail(env=self.copy_environment(target, corrupt=True))
                self.assertEqual(result.returncode, 0)
                self.assertFalse(verdict)

    def test_prior_failure_stays_failed_and_retains_diagnostics(self):
        (self.original / 'verification-amd64.json').unlink()
        verdict, result = self.tail(verifier_ok=False)
        self.assertEqual(result.returncode, 0)
        self.assertFalse(verdict)
        self.assertTrue((self.stage / 'verification/first.log').is_file())
        self.assertTrue((self.stage / 'runtime/kvm.log').is_file())

    def test_cli_authentication_boundary(self):
        self.assertEqual(self.stage_files().returncode, 0)
        source = self.root / 'signed-source/script/release'
        source.mkdir(parents=True)
        for name in ('content.py', 'evidence.py', 'release_trace.py'):
            shutil.copyfile(ROOT / 'script/release' / name, source / name)
        # Offline stub authenticates a known fixture hash and version; it cannot
        # leak into production because only this temporary source root imports it.
        (source / 'operations.py').write_text(
            'import content\nfrom pathlib import Path\n'
            'def authenticate_metadata(out, version):\n'
            '    if (Path(__file__).parent / "reject").exists(): raise ValueError("authentication failed")\n'
            f'    if content.hash_file(out / "release.json")[0].value != {self.record["release_subject"]!r}:\n'
            '        raise ValueError("staged metadata mismatch")\n'
            '    release=content.parse_release(out / "release.json")\n'
            '    if release.version.value != version: raise ValueError("version mismatch")\n'
            '    return release\n')
        command = [sys.executable, str(ROOT / 'script/release/check-ci-boot-evidence.py'),
                   str(source.parents[1]), str(self.assets), str(self.stage), '0.1.0', 'a' * 40]
        self.assertEqual(subprocess.run(command, capture_output=True).returncode, 0)
        for mode in ('source', 'version', 'auth', 'metadata'):
            with self.subTest(mode=mode):
                args = command.copy()
                if mode == 'source': args[-1] = '0' * 40
                if mode == 'version': args[-2] = '9.9.9'
                if mode == 'auth': (source / 'reject').touch()
                if mode == 'metadata': (self.stage / 'release/release.json').write_text('{}')
                result = subprocess.run(args, capture_output=True)
                (self.root / 'upload-attempted').touch()
                self.assertNotEqual(result.returncode, 0)
                self.assertTrue((self.root / 'upload-attempted').is_file())
                (source / 'reject').unlink(missing_ok=True)
        self.assertFalse(list((self.root / 'signed-source').rglob('__pycache__')))


if __name__ == '__main__':
    unittest.main()
