#!/usr/bin/env python3
"""Explicit anonymous consumption of a named release. No build/fallback operation."""
from __future__ import annotations

import sys
sys.dont_write_bytecode = True

import argparse
import json
import os
import platform
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path
from dataclasses import dataclass

import attestations
import capacity
import peaks
import content as c
import operations as op
import public_http
import release_trace
from typing import Callable
from evidence import MANDATORY_CHECKS


@dataclass(frozen=True)
class VerifyRequest:
    version: str
    arch: c.Arch
    assets_dir: Path
    msb_bin: Path
    boot: bool


class Verification:
    def __init__(self, root: Path, arch: c.Arch, msb: Path, firmware: Path):
        self.root, self.arch, self.msb, self.firmware = root, arch, msb, firmware
        self.logs = []
        self.checks = {}
        self.sequence = 0
        self.sampler = peaks.Sampler(root / (arch + "-peak-samples.json"), [root])
        self.env = dict(os.environ)
        for key in list(self.env):
            if key.startswith(('MSB_', 'DOCKER_', 'XDG_')) or key in (
                    'GITHUB_TOKEN', 'GH_TOKEN', 'PUBLISH_TOKEN', 'GHCR_PACKAGE_INVENTORY_TOKEN', 'REGISTRY_AUTH_FILE'):
                self.env.pop(key)
        endpoint = op.docker_endpoint(json.loads(op.run(['docker', 'context', 'inspect'])))
        if not isinstance(endpoint, str) or not endpoint.startswith('unix://'):
            raise ValueError('explicit local Unix Docker endpoint required')
        plugins = json.loads(op.run(['docker', 'info', '--format', '{{json .ClientInfo.Plugins}}']))
        config = root / 'docker-auth'
        config.mkdir()
        (config / 'config.json').write_text(json.dumps({'auths': {}, 'cliPluginsExtraDirs':
            [str(op.buildx_plugin(plugins).parent)]}))
        home = root / 'home'
        home.mkdir()
        self.env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / '.config'), XDG_CACHE_HOME=str(home / '.cache'),
                        DOCKER_HOST=endpoint, DOCKER_CONFIG=str(config), REGISTRY_AUTH_FILE=str(config / 'config.json'))

    def command(self, argv: list[str], *, env: dict[str, str] | None = None, rejection: bool = False, timeout: int = 1800) -> bytes:
        self.sequence += 1
        path = self.root / f'{self.arch}-command-{self.sequence:03d}.log'
        self.logs.append(path)
        return release_trace.capture(argv, path=path, env=env if env is not None else self.env,
                                     timeout=timeout, rejection=rejection)

    def captured(self, argv: list[str], status: int, stdout: bytes, stderr: bytes) -> None:
        self.sequence += 1
        path = self.root / f'{self.arch}-captured-{self.sequence:03d}.log'
        path.write_bytes((json.dumps({'argv': argv, 'status': status}) + '\n').encode() + stdout + stderr)
        self.logs.append(path)

    def check(self, name: str, function: Callable[[], object]) -> None:
        start, previous = time.monotonic(), len(self.logs)
        token = release_trace.sink.set(self.captured)
        try:
            function()
        finally:
            release_trace.sink.reset(token)
        if len(self.logs) == previous:
            raise ValueError('check produced no command or validated-content evidence: ' + name)
        self.checks[name] = {'status': 0, 'duration_seconds': time.monotonic() - start,
                             'logs': [x.name for x in self.logs[previous:]]}

    def missing_asset(self, *, version: str, name: str) -> None:
        status = self.command(['curl', '-q', '--netrc-file', '/dev/null', '--silent', '--show-error', '--location',
                '--proto', '=https', '--proto-redir', '=https', '--connect-timeout', '15', '--max-time', '60',
                '--max-filesize', str(c.JSON_CAP), '--output', '/dev/null', '--write-out', '%{http_code}',
                c.SOURCE + '/releases/download/v' + version + '/' + name + '.missing-control'],
                env=public_http.public_env())
        if status != b'404':
            raise ValueError('missing-asset requires a successfully received HTTP 404')

    def msb_environment(self, route: str, *, allocation: int = 2 * c.UNCOMPRESSED_CAP + c.COMPRESSED_CAP) -> dict[str, str]:
        capacity.check_space(Path('/tmp'), allocation)
        state = Path(tempfile.mkdtemp(prefix='av264-', dir='/tmp'))
        self.sampler.paths.append(state)
        for directory in ('home', 'state', 'xdg', 'bin'):
            (state / directory).mkdir()
        (state / 'config.json').write_text('{}\n')
        # Boot subprocess PATH deliberately has no Docker or launcher. Only
        # required ordinary host utilities are exposed; no runtime auto-update.
        for tool in ('sh', 'ps', 'kill', 'uname', 'codesign', 'sw_vers', 'diskutil', 'hdiutil', 'lsof'):
            executable = shutil.which(tool)
            if executable:
                (state / 'bin' / tool).symlink_to(Path(executable).resolve())
        env = dict(self.env, HOME=str(state / 'home'), XDG_CONFIG_HOME=str(state / 'xdg'),
                   XDG_CACHE_HOME=str(state / 'xdg/cache'), MSB_HOME=str(state / 'state'),
                   MSB_CONFIG_PATH=str(state / 'config.json'), MSB_PATH=str(self.msb),
                   MSB_LIBKRUNFW_PATH=str(self.firmware), PATH=str(state / 'bin'))
        for key in ('DOCKER_HOST', 'DOCKER_CONFIG', 'REGISTRY_AUTH_FILE'):
            env.pop(key, None)
        if os.environ.get('MSB_AGENTD_PATH'):
            env['MSB_AGENTD_PATH'] = str(Path(os.environ['MSB_AGENTD_PATH']).resolve(strict=True))
        path = self.root / (self.arch + '-' + route + '-state.log')
        path.write_text(json.dumps({'MSB_HOME': str(state / 'state'), 'MSB_CONFIG_PATH': str(state / 'config.json'),
                                    'initial_cache_entries': [], 'docker_on_path': False}) + '\n')
        self.logs.append(path)
        return env

    def inspect(self, ref: str, graph: c.ImageGraph, env: dict) -> None:
        value = json.loads(self.command([str(self.msb), 'image', 'inspect', '--format', 'json', ref], env=env))
        c.check_msb_graph(value, graph)

    def boot(self, ref: str, env: dict, *, pairs: list[str], probe: str, fixture=False) -> None:
        for number, pair in enumerate(pairs):
            home = self.root / f'guest-home-{self.sequence}-{number}'
            home.mkdir(mode=0o777)
            home.chmod(0o777)
            capacity.check_space(Path(env['MSB_HOME']), 2 * 1024 ** 3 if fixture else 2 * c.UNCOMPRESSED_CAP + c.COMPRESSED_CAP)
            name = 'av264-' + self.arch + '-' + str(self.sequence) + '-' + str(number)
            created = False
            try:
                self.command([str(self.msb), 'create', '--name', name, '--pull', 'never', '--user', pair,
                    '--env', 'HOME=/home/probe', '--mount-dir', str(home) + ':/home/probe', '--no-net', '-m', '2G', ref], env=env)
                created = True
                for attempt in range(30):
                    ping = subprocess.run([str(self.msb), 'ping', name], env=env, capture_output=True, timeout=15)
                    if ping.returncode == 0:
                        self.command([str(self.msb), 'ping', name], env=env)
                        break
                    time.sleep(1)
                else:
                    raise ValueError('native msb ping timed out')
                argv = [str(self.msb), 'exec', '--no-tty', '--timeout', '3m', name, '--',
                        'sh' if fixture else 'bash', '-c', probe]
                if not fixture:
                    selected = op.selections()
                    argv += ['released-agent-probe', *pair.split(':'), *[selected[x] for x in
                        ('codex', 'opencode', 'claude', 'copilot', 'dsh', 'pnpm', 'pi', 'pi-claude-bridge')]]
                self.command(argv, env=env, timeout=200)
            finally:
                if created:
                    self.command([str(self.msb), 'stop', '--timeout', '15', name], env=env)
                    self.command([str(self.msb), 'remove', name], env=env)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--version', required=True)
    parser.add_argument('--arch', required=True)
    parser.add_argument('--assets-dir', type=Path, required=True)
    parser.add_argument('--msb-bin', type=Path, default=Path(shutil.which('msb') or '/missing-msb'))
    parser.add_argument('--boot', action='store_true')
    namespace = parser.parse_args()
    root = None
    sampler = None
    result = 0
    try:
        args = VerifyRequest(c.parse_version(namespace.version).value, c.parse_arch(namespace.arch),
                             namespace.assets_dir, namespace.msb_bin, namespace.boot)
        arch = args.arch
        c.parse_version(args.version)
        op.native(arch)
        release = op.authenticate_metadata(args.assets_dir, args.version)
        if op.source_sha() != release.source_sha or op.selections() != dict(release.platform(arch).selections):
            raise ValueError('verifier must run from the exact signed committed source SHA')
        msb = args.msb_bin.resolve(strict=True)
        firmware = Path(os.environ['MSB_LIBKRUNFW_PATH']).resolve(strict=True)
        libkrun_version = os.environ['MSB_LIBKRUN_VERSION']
        if not libkrun_version:
            raise ValueError('reviewed standalone libkrun runtime version must be recorded')
        root = Path(tempfile.mkdtemp(prefix='verify-' + arch + '-', dir=args.assets_dir)).resolve()
        verify = Verification(root, arch, msb, firmware)
        sampler = verify.sampler
        sampler.start()
        inventory = release.platform(arch)
        capacity.check_space(root, inventory.archive.size * 3 + c.COMPRESSED_CAP * 3 + c.UNCOMPRESSED_CAP * 4)
        tools = {}
        for name, executable, argv in (
            ('docker', Path(shutil.which('docker')).resolve(), ['docker', 'version']),
            ('buildx', op.buildx_plugin(json.loads(op.run(['docker', 'info', '--format', '{{json .ClientInfo.Plugins}}']))), ['docker', 'buildx', 'version']),
            ('skopeo', Path(shutil.which('skopeo') or '/missing-skopeo').resolve(), ['skopeo', '--version']),
            ('msb', msb, [str(msb), '--version']),
            ('gh', Path(shutil.which('gh') or '/missing-gh').resolve(), ['gh', '--version'])):
            tools[name] = {'version': verify.command(argv).decode().strip(), 'sha256': c.hash_file(executable)[0].value}
        tools['firmware'] = {'version': 'matching reviewed standalone firmware', 'sha256': c.hash_file(firmware)[0].value}
        linkage = verify.command(['otool', '-L', str(msb)] if platform.system() == 'Darwin'
                                  else ['ldd', str(msb)]).decode()
        if os.environ.get('MSB_LIBKRUN_EMBEDDED') == '1':
            if 'libkrun.' in linkage:
                raise ValueError('embedded runtime declaration conflicts with dynamic linkage')
            tools['libkrun'] = {'version': libkrun_version, 'sha256': c.hash_file(msb)[0].value,
                                'linkage': 'embedded in recorded msb executable; reviewed distribution declaration'}
        else:
            runtime = Path(os.environ['MSB_LIBKRUN_PATH']).resolve(strict=True)
            if str(runtime) not in linkage:
                raise ValueError('reviewed libkrun path not resolved in executable linkage')
            tools['libkrun'] = {'version': libkrun_version, 'sha256': c.hash_file(runtime)[0].value,
                                'path': str(runtime), 'linkage': 'dynamic resolved dependency'}
        if os.environ.get('MSB_AGENTD_PATH'):
            tools['agentd'] = {'version': 'reviewed explicit agentd', 'sha256': c.hash_file(Path(os.environ['MSB_AGENTD_PATH']).resolve())[0].value}
        verify.check('runtime-doctor', lambda: verify.command([str(msb), 'doctor'], env=verify.msb_environment('doctor', allocation=64 * 1024 ** 2)))
        # Genuine pinned fixture acquisition is skopeo transport, not a build.
        fixture = root / 'fixture-layout'
        pins = c.read_json(op.ROOT / 'script/release/tool-pins.json')
        fixture_ref = 'docker.io/library/alpine@' + pins['fixture_base'].split('@')[1]
        verify.command(['skopeo', '--override-os', 'linux', '--override-arch', arch, 'copy', '--src-no-creds',
            '--authfile', str(root / 'docker-auth/config.json'), '--preserve-digests', 'docker://' + fixture_ref,
            'oci:' + str(fixture) + ':standard'])
        fixture_graph = c.inventory_layout(fixture, arch)
        c.normalize_root(fixture, fixture_graph)
        fixture_archive = root / 'fixture.tar'
        verify.command(['tar', '-C', str(fixture), '-cf', str(fixture_archive), 'oci-layout', 'index.json', 'blobs'],
                       env=dict(verify.env, COPYFILE_DISABLE='1'))
        sys.path.insert(0, str(op.ROOT / 'script/test'))
        import importlib.util
        spec = importlib.util.spec_from_file_location('fixture_transports', op.ROOT / 'script/test/release-transports.py')
        module = importlib.util.module_from_spec(spec); spec.loader.exec_module(module)
        controls_root = root / 'fixture-controls'; controls_root.mkdir()
        controls = module.Controls(controls_root, arch, msb)
        controls.env['MSB_LIBKRUNFW_PATH'] = str(firmware)
        try:
            controls.execute(fixture)
        finally:
            controls.cleanup()
        for path in controls_root.glob('*.log'):
            target = root / (arch + '-fixture-' + path.name)
            op.stage_file(path, target)
            verify.logs.append(target)
        for name in ('fixture-corrupt-blob', 'fixture-corrupt-archive', 'fixture-wrong-architecture'):
            verify.checks[name] = {'status': 0, 'duration_seconds': 0,
                                  'logs': [x.name for x in verify.logs if '-fixture-' in x.name]}
        if args.boot:
            fixture_env = verify.msb_environment('fixture-boot', allocation=2 * 1024 ** 3)
            verify.command([str(msb), 'image', 'load', '--input', str(fixture_archive), '--tag', 'av264:boot-fixture'], env=fixture_env)
            verify.boot('av264:boot-fixture', fixture_env, pairs=['12345:23456'],
                probe='set -eu; test "$(id -u)" = 12345; test "$(id -g)" = 23456; echo native > "$HOME/probe"', fixture=True)
        verify.check('metadata-signature', lambda: op.authenticate_metadata(args.assets_dir, args.version))
        invocation = inventory.invocation_url
        def signatures():
            bundle = args.assets_dir / f'platform-{arch}-files.sigstore.json'
            attestations.verify(args.assets_dir / f'platform-{arch}.json', bundle, source_sha=release.source_sha, invocation=invocation)
            if c.read_json(args.assets_dir / f'platform-{arch}.json') != inventory.json():
                raise ValueError('published inventory differs from release metadata')
            for payload in (inventory.parts or (inventory.archive,)) + (inventory.sbom,):
                attestations.verify(args.assets_dir / payload.name, bundle, source_sha=release.source_sha, invocation=invocation)
            subject = 'oci://' + op.IMAGE + '@' + inventory.graph.manifest.digest.value
            attestations.verify(subject, args.assets_dir / f'platform-{arch}-image.sigstore.json', source_sha=release.source_sha,
                                digest=inventory.graph.manifest.digest, invocation=invocation)
            attestations.verify(subject, args.assets_dir / f'platform-{arch}-sbom.sigstore.json', source_sha=release.source_sha,
                                digest=inventory.graph.manifest.digest, invocation=invocation,
                        spdx_file=args.assets_dir / inventory.sbom.name)
            attestations.verify('oci://' + op.IMAGE + '@' + release.index_digest.value,
                                args.assets_dir / 'release-index.sigstore.json', source_sha=release.source_sha,
                                digest=release.index_digest, invocation=invocation)
        verify.check('platform-signatures', signatures)
        public = root / 'public-assets'
        verify.check('public-assets', lambda: op.download_release(op.DownloadRequest(version=args.version, arch=arch, out=public)))
        if (public / 'release.json').read_bytes() != (args.assets_dir / 'release.json').read_bytes():
            raise ValueError('public release changed after local metadata review')
        anon = root / 'anonymous'
        anon.mkdir()
        verify.check('anonymous-index', lambda: op.anonymous_registry(release, anon))
        verify.check('anonymous-platform', lambda: c.compare_platforms(
            c.inventory_layout(anon / ('registry-' + arch), arch), inventory.graph))
        archive = root / 'verified.oci.tar'
        verify.check('registry-archive-graph', lambda: c.assemble_archive(inventory, public, archive))
        # Capacity for actual Docker guest storage must be measured on daemon,
        # never inferred from host scratch. No installer/build is called.
        import storage
        stats = storage.measure(root, root / 'daemon-storage.json')
        for record in stats['daemon_filesystems']:
            if record['free_bytes'] < inventory.graph.uncompressed_bytes + c.COMPRESSED_CAP + capacity.RESERVE:
                raise ValueError('insufficient measured daemon capacity for artifact pull')
        ref = op.IMAGE + '@' + inventory.graph.manifest.digest.value
        def docker():
            verify.command(['docker', 'pull', ref])
            verify.command(['bash', str(op.ROOT / 'script/test/standard-image.sh'), '--artifact', ref,
                            '--platform', 'linux/' + arch], timeout=5400)
        verify.check('docker-artifact', docker)
        pairs = list(dict.fromkeys([f'{os.getuid()}:{os.getgid()}', '12345:23456', '54321:34567']))
        routes = {}
        for route in ('registry', 'archive'):
            env = verify.msb_environment(route)
            local_ref = ref if route == 'registry' else 'av264-standard:archive'
            def ingest(route=route, env=env, local_ref=local_ref):
                if route == 'registry':
                    verify.command([str(msb), 'image', 'pull', ref], env=env)
                else:
                    verify.command([str(msb), 'image', 'load', '--input', str(archive), '--tag', local_ref], env=env)
                verify.inspect(local_ref, inventory.graph, env)
            verify.check('msb-' + route + '-inspect', ingest)
            routes[route] = (env, local_ref)
        bad = root / 'truncated-standard.tar'
        with archive.open('rb') as stream:
            bad.write_bytes(stream.read(100))
        def reject_bad():
            env = verify.msb_environment('corrupt-standard')
            verify.command([str(msb), 'image', 'load', '--input', str(bad), '--tag', 'av264:corrupt'], env=env, rejection=True)
            verify.command([str(msb), 'image', 'inspect', '--format', 'json', 'av264:corrupt'], env=env, rejection=True)
        verify.check('corrupt-archive', reject_bad)
        verify.check('missing-asset', lambda: verify.missing_asset(version=args.version, name=inventory.archive.name))
        opposite = 'amd64' if arch == 'arm64' else 'arm64'
        other = root / 'opposite-assets'
        def reject_other():
            op.download_release(op.DownloadRequest(version=args.version, arch=opposite, out=other))
            other_tar = root / 'opposite.tar'
            c.assemble_archive(release.platform(opposite), other, other_tar)
            env = verify.msb_environment('wrong-architecture')
            verify.command([str(msb), 'image', 'load', '--input', str(other_tar), '--tag', 'av264:wrong'], env=env, rejection=True)
            verify.command([str(msb), 'image', 'inspect', '--format', 'json', 'av264:wrong'], env=env, rejection=True)
        verify.check('wrong-architecture', reject_other)
        if args.boot:
            probe = (op.ROOT / 'script/test/fixtures/released-agent-probe.sh').read_text()
            for route, (env, local_ref) in routes.items():
                verify.check('msb-' + route + '-boot', lambda env=env, local_ref=local_ref:
                             verify.boot(local_ref, env, pairs=pairs, probe=probe))
            if set(verify.checks) != MANDATORY_CHECKS:
                raise ValueError('native verification mandatory check set incomplete')
        sampler.stop()
        sampler = None
        verify.logs.append(verify.sampler.output)
        record = {'schema_version': 1, 'architecture': arch, 'release_subject': c.hash_file(public / 'release.json')[0].value,
                  'platform': inventory.json(), 'index_digest': release.index_digest.value,
                  'host': platform.platform(), 'tools': tools, 'uid_gid_pairs': pairs, 'cold_cache': True,
                  'checks': verify.checks, 'logs': [op.asset(x).json() for x in verify.logs]}
        filename = ('verification-' if args.boot else 'inspection-') + arch + '.json'
        c.atomic_json(root / filename, record)
        print('completed ' + filename + '; retained evidence: ' + str(root))
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(f'verify-release: {error}; retained evidence: {root}', file=sys.stderr)
        result = 1
    finally:
        if sampler is not None:
            try:
                sampler.stop()
            except ValueError as error:
                print(f'verification peak sampling: {error}', file=sys.stderr)
                result = 1
    return result


if __name__ == '__main__':
    raise SystemExit(main())
