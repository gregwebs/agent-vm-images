#!/usr/bin/env python3
"""Native, local-only transport controls. A fixture is not standard acceptance."""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / 'script/release'))
import content as c
import operations as op
import peaks
import release_trace


class Controls:
    def __init__(self, scratch: Path, arch: str, msb: Path | None):
        self.scratch, self.arch, self.msb = scratch, arch, msb
        self.prefix = 'avm264-' + scratch.name.lower().replace('_', 'x')
        self.network_created = False
        self.network = self.prefix + '-net'
        self.containers: list[str] = []
        self.images: list[str] = []
        self.sequence = 0
        endpoint = op.docker_endpoint(json.loads(op.run(['docker', 'context', 'inspect'])))
        if not isinstance(endpoint, str) or not endpoint.startswith('unix://'):
            raise ValueError('fixture controls require actual Unix Docker endpoint')
        self.env = dict(os.environ)
        for key in ('GITHUB_TOKEN', 'GH_TOKEN', 'PUBLISH_TOKEN', 'GHCR_PACKAGE_INVENTORY_TOKEN', 'REGISTRY_AUTH_FILE', 'DOCKER_CONTEXT'):
            self.env.pop(key, None)
        auth = scratch / 'auth'
        auth.mkdir()
        plugins = json.loads(op.run(['docker', 'info', '--format', '{{json .ClientInfo.Plugins}}']))
        plugin_dirs = [str(op.buildx_plugin(plugins).parent)]
        (auth / 'config.json').write_text(json.dumps({'auths': {}, 'cliPluginsExtraDirs': plugin_dirs}) + '\n')
        self.env.update(DOCKER_HOST=endpoint, DOCKER_CONFIG=str(auth), REGISTRY_AUTH_FILE=str(auth / 'config.json'))
        self.pins = c.read_json(ROOT / 'script/release/tool-pins.json')
        self.tool = None
        self.registries: dict[str, str] = {}
        self.logs: list[str] = []
        self.state_dirs: list[Path] = []
        self.sampler = peaks.Sampler(scratch / 'peak-samples.json', [scratch])
        self.sampling = False

    def command(self, argv: list[str], *, expected_failure: bool = False, env: dict[str, str] | None = None) -> bytes:
        self.sequence += 1
        name = f'command-{self.sequence:03d}.log'
        path = self.scratch / name
        self.logs.append(name)
        return release_trace.capture(argv, path=path, env=env if env is not None else self.env,
                                     timeout=1800, rejection=expected_failure, rejection_stderr=True)

    def start_registry(self, *, private=False) -> tuple[str, str]:
        name = self.prefix + ('-private' if private else '-public')
        argv = ['docker', 'create', '--name', name, '--network', self.network, '-p', '127.0.0.1::5000']
        if private:
            if not shutil.which('htpasswd'):
                raise ValueError('fixture htpasswd prerequisite missing (apache2-utils on CI)')
            password = self.command(['htpasswd', '-Bbn', 'fixture', 'fixture-password'])
            folder = self.scratch / 'private-auth'
            folder.mkdir()
            file = folder / 'htpasswd'
            file.write_bytes(password)
            argv += ['-e', 'REGISTRY_AUTH=htpasswd', '-e', 'REGISTRY_AUTH_HTPASSWD_REALM=fixture',
                     '-e', 'REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd']
        argv += [self.pins['fixture_registry']]
        self.command(argv)
        self.containers.append(name)
        if private:
            self.command(['docker', 'cp', str(self.scratch / 'private-auth'), name + ':/auth'])
        self.command(['docker', 'start', name])
        port = self.command(['docker', 'port', name, '5000/tcp']).decode().strip().rsplit(':', 1)[-1]
        endpoint = '127.0.0.1:' + port
        self.registries[endpoint] = name + ':5000'
        for _ in range(50):
            argv = ['curl', '-q', '--netrc-file', '/dev/null', '--silent', '--show-error', '--connect-timeout', '2', '--max-time', '3',
                    '--output', '/dev/null', '--write-out', '%{http_code}', 'http://' + endpoint + '/v2/']
            result = subprocess.run(argv, capture_output=True, timeout=5, env=op.public_http.public_env())
            if result.returncode == 0 and result.stdout == (b'401' if private else b'200'):
                return name, endpoint
            time.sleep(0.1)
        raise ValueError('loopback registry readiness failed')

    def skopeo(self, argv: list[str], *, expected_failure=False) -> bytes:
        if shutil.which('skopeo'):
            return self.command(['skopeo'] + argv, expected_failure=expected_failure)
        # Test-only explicit pinned toolbox adapter. Extra copies are tiny and
        # retained as evidence; production never silently substitutes this path.
        if self.tool is None:
            self.tool = self.prefix + '-skopeo'
            self.command(['docker', 'run', '-d', '--name', self.tool, '--network', self.network,
                          '--entrypoint', '/bin/sh', self.pins['fixture_skopeo'], '-c',
                          'mkdir -p /work; sleep 7200'])
            self.containers.append(self.tool)
        translated, downloads = [], []
        for value in argv:
            if value.startswith('oci:'):
                path, tag = value[4:].rsplit(':', 1)
                local = Path(path)
                remote = '/work/oci-' + str(self.sequence) + '-' + str(len(translated))
                if local.exists():
                    self.command(['docker', 'cp', str(local), self.tool + ':' + remote])
                else:
                    downloads.append((remote, local))
                value = 'oci:' + remote + ':' + tag
            elif value.startswith('docker://'):
                for endpoint, container in self.registries.items():
                    value = value.replace('docker://' + endpoint + '/', 'docker://' + container + '/')
            elif Path(value).is_file() and value.endswith('config.json'):
                remote = '/work/auth-' + str(self.sequence) + '.json'
                self.command(['docker', 'cp', value, self.tool + ':' + remote])
                value = remote
            translated.append(value)
        output = self.command(['docker', 'exec', self.tool, 'skopeo'] + translated, expected_failure=expected_failure)
        if not expected_failure:
            for remote, local in downloads:
                self.command(['docker', 'cp', self.tool + ':' + remote, str(local)])
        return output

    def msb_env(self, route: str) -> dict:
        if self.msb is None:
            raise ValueError('msb control requested without runtime')
        # Keep Unix socket paths short on macOS; no shared cache/catalog copy.
        from capacity import check_space
        check_space(Path('/tmp'), 64 * 1024 ** 2)
        directory = Path(tempfile.mkdtemp(prefix='a264-', dir='/tmp'))
        self.sampler.paths.append(directory)
        self.state_dirs.append(directory)
        (directory / 'config.json').write_text('{}\n')
        for name in ('home', 'state', 'xdg'):
            (directory / name).mkdir()
        env = dict(self.env, HOME=str(directory / 'home'), XDG_CONFIG_HOME=str(directory / 'xdg'),
                   XDG_CACHE_HOME=str(directory / 'xdg/cache'), MSB_HOME=str(directory / 'state'),
                   MSB_CONFIG_PATH=str(directory / 'config.json'), MSB_PATH=str(self.msb))
        if not env.get('MSB_LIBKRUNFW_PATH'):
            raise ValueError('explicit matching MSB_LIBKRUNFW_PATH required')
        from capacity import check_space
        check_space(directory, 64 * 1024 ** 2)
        (self.scratch / (route + '-state.txt')).write_text(str(directory) + '\n')
        return env

    def inspect_msb(self, reference: str, graph: c.ImageGraph, env: dict) -> None:
        value = json.loads(self.command([str(self.msb), 'image', 'inspect', '--format', 'json', reference], env=env))
        c.check_msb_graph(value, graph)

    def negative_load(self, archive: Path, name: str) -> None:
        # Wrapper rejection is mandatory even on hosted CI without msb.
        try:
            c.verify_archive(archive, self.graph)
        except (ValueError, OSError, EOFError, c.tarfile.TarError) as error:
            (self.scratch / (name + '-wrapper.log')).write_text(str(error) + '\n')
            self.logs.append(name + '-wrapper.log')
        else:
            raise ValueError('archive wrapper accepted ' + name)
        if self.msb is None:
            return
        env = self.msb_env(name)
        ref = self.prefix + ':' + name
        self.command([str(self.msb), 'image', 'load', '--input', str(archive), '--tag', ref], expected_failure=True, env=env)
        self.command([str(self.msb), 'image', 'inspect', '--format', 'json', ref], expected_failure=True, env=env)

    def cleanup(self) -> None:
        commands = [['docker', 'rm', '-f', '-v', name] for name in reversed(self.containers)]
        commands += [['docker', 'image', 'rm', tag] for tag in self.images]
        if self.network_created:
            commands.append(['docker', 'network', 'rm', self.network])
        failures = []
        if getattr(self, 'sampling', False):
            try:
                self.sampler.stop()
            except ValueError as error:
                failures.append(str(error))
            self.sampling = False
        for argv in commands:
            try:
                self.command(argv)
            except (ValueError, OSError, subprocess.SubprocessError) as error:
                failures.append(str(error))
        for directory in self.state_dirs:
            try:
                shutil.rmtree(directory)
            except OSError as error:
                failures.append('msb state cleanup failed: ' + str(error))
        if failures:
            (self.scratch / 'result.json').unlink(missing_ok=True)
            raise ValueError('owned cleanup failed: ' + '; '.join(failures))

    def execute(self, caller_layout: Path | None) -> None:
        op.native(self.arch)
        self.sampler.start()
        self.sampling = True
        self.command(['docker', 'network', 'create', self.network])
        self.network_created = True
        registry, endpoint = self.start_registry()
        # Preserve only endpoint, not operator builder/auth state, after isolation.
        builder = self.command(['docker', 'context', 'show']).decode().strip()
        inspected = self.command(['docker', 'buildx', 'inspect', builder]).decode()
        import re
        if not re.search(r'^Driver:\s+docker$', inspected, re.M):
            raise ValueError('daemon-backed OCI-capable driver required')
        tag = self.prefix + ':fixture'
        if caller_layout is None:
            layout = self.scratch / 'layout'
            self.command(['docker', 'buildx', 'build', '--builder', builder, '--platform', 'linux/' + self.arch,
                          '--provenance=false', '--sbom=false', '-t', tag, '--output',
                          f'type=oci,dest={layout},tar=false,compression=gzip',
                          str(ROOT / 'script/test/fixtures/release-image')])
            graph = c.inventory_layout(layout, self.arch)
            c.normalize_root(layout, graph)
            self.images.append(tag)
        else:
            layout = caller_layout.resolve()
            graph = c.inventory_layout(layout, self.arch, require_platform=True)
        self.graph = graph
        archive_dir = self.scratch / 'archive'
        self.command(['bash', str(ROOT / 'script/release/archive-oci.sh'), '--layout', str(layout),
                      '--arch', self.arch, '--out', str(archive_dir)])
        archive = archive_dir / 'archive.oci.tar'
        c.atomic_json(self.scratch / 'graph.json', graph.json())
        # Caller metadata may name an operator image. Never Docker-load it.
        if caller_layout is None:
            self.command(['docker', 'image', 'load', '--input', str(archive)])
            self.command(['docker', 'run', '--rm', '--network', 'none', '--cap-drop', 'ALL', '--user', '12345:23456', tag])
            if len(graph.layers) == len(set(graph.layers)):
                raise ValueError('R1 fixture did not exercise repeated empty layers')
        self.skopeo(['copy', '--authfile', str(self.scratch / 'auth/config.json'), '--preserve-digests',
                     '--dest-tls-verify=false', 'oci:' + str(layout) + ':standard', 'docker://' + endpoint + '/standard:probe'])
        raw = self.skopeo(['inspect', '--raw', '--no-creds', '--tls-verify=false',
                           'docker://' + endpoint + '/standard@' + graph.manifest.digest.value])
        if raw != graph.manifest.blob(layout).read_bytes():
            raise ValueError('registry changed raw child manifest')
        downloaded = self.scratch / 'registry-copy'
        self.skopeo(['copy', '--src-no-creds', '--src-tls-verify=false', '--preserve-digests',
                     'docker://' + endpoint + '/standard@' + graph.manifest.digest.value,
                     'oci:' + str(downloaded) + ':standard'])
        c.compare_platforms(graph, c.inventory_layout(downloaded, self.arch))
        # Skopeo omits root platform. This is deliberately accepted for registry
        # wrappers (R2), then only owned wrapping metadata is normalized for tar.
        index = c.read_json(downloaded / 'index.json')
        if 'platform' in index['manifests'][0]:
            del index['manifests'][0]['platform']
            c.atomic_json(downloaded / 'index.json', index)
        c.compare_platforms(graph, c.inventory_layout(downloaded, self.arch))
        c.normalize_root(downloaded, graph)
        self.command(['bash', str(ROOT / 'script/release/archive-oci.sh'), '--layout', str(downloaded),
                      '--arch', self.arch, '--out', str(self.scratch / 'roundtrip')])
        if self.msb:
            env = self.msb_env('archive')
            ref = self.prefix + ':archive'
            self.command([str(self.msb), 'image', 'load', '--input', str(archive), '--tag', ref], env=env)
            self.inspect_msb(ref, graph, env)
            env = self.msb_env('registry')
            ref = endpoint + '/standard@' + graph.manifest.digest.value
            self.command([str(self.msb), 'image', 'pull', '--insecure', ref], env=env)
            self.inspect_msb(ref, graph, env)
        bad = self.scratch / 'corrupt-layout'
        shutil.copytree(layout, bad)
        layer = graph.layers[0].blob(bad)
        layer.chmod(0o600)
        with layer.open('r+b') as stream:
            first = stream.read(1)
            stream.seek(0)
            stream.write(bytes([first[0] ^ 1]))
        try:
            c.inventory_layout(bad, self.arch)
        except ValueError:
            pass
        else:
            raise ValueError('corrupt compressed blob accepted')
        bad_tar = self.scratch / 'corrupt.tar'
        self.command(['tar', '-C', str(bad), '-cf', str(bad_tar), 'oci-layout', 'index.json', 'blobs'],
                     env=dict(self.env, COPYFILE_DISABLE='1'))
        self.negative_load(bad_tar, 'corrupt')
        missing = self.scratch / 'missing-blob-layout'
        shutil.copytree(layout, missing)
        graph.layers[0].blob(missing).unlink()
        missing_tar = self.scratch / 'missing-blob.tar'
        self.command(['tar', '-C', str(missing), '-cf', str(missing_tar), 'oci-layout', 'index.json', 'blobs'],
                     env=dict(self.env, COPYFILE_DISABLE='1'))
        self.negative_load(missing_tar, 'missing-blob')
        truncated = self.scratch / 'truncated.tar'
        with archive.open('rb') as stream:
            truncated.write_bytes(stream.read(100))
        self.negative_load(truncated, 'truncated')
        opposite = 'amd64' if self.arch == 'arm64' else 'arm64'
        wrong = self.scratch / 'wrong-arch'
        self.skopeo(['--override-os', 'linux', '--override-arch', opposite, 'copy', '--src-no-creds', '--preserve-digests',
                     'docker://docker.io/library/alpine@' + self.pins['fixture_base'].split('@')[1], 'oci:' + str(wrong) + ':standard'])
        wrong_graph = c.inventory_layout(wrong, opposite)
        c.normalize_root(wrong, wrong_graph)
        self.command(['bash', str(ROOT / 'script/release/archive-oci.sh'), '--layout', str(wrong),
                      '--arch', opposite, '--out', str(self.scratch / 'wrong-archive')])
        self.negative_load(self.scratch / 'wrong-archive/archive.oci.tar', 'wrong-architecture')
        # A protected interrupted publication must never look like an absent ref.
        _, private_endpoint = self.start_registry(private=True)
        auth = self.scratch / 'private-config.json'
        auth.write_text(json.dumps({'auths': {private_endpoint: {'auth': base64.b64encode(b'fixture:fixture-password').decode()},
            self.registries[private_endpoint]: {'auth': base64.b64encode(b'fixture:fixture-password').decode()}}}))
        self.skopeo(['copy', '--authfile', str(auth), '--preserve-digests', '--dest-tls-verify=false',
                     'oci:' + str(layout) + ':standard', 'docker://' + private_endpoint + '/standard:partial'])
        for path in ('partial', 'absent'):
            self.skopeo(['inspect', '--raw', '--no-creds', '--tls-verify=false',
                         'docker://' + private_endpoint + '/standard:' + path], expected_failure=True)
        raw = self.skopeo(['inspect', '--raw', '--authfile', str(auth), '--tls-verify=false',
                           'docker://' + private_endpoint + '/standard:partial'])
        if raw != graph.manifest.blob(layout).read_bytes():
            raise ValueError('authorized partial ref changed')
        self.skopeo(['inspect', '--raw', '--authfile', str(auth), '--tls-verify=false',
                               'docker://' + private_endpoint + '/standard:absent'], expected_failure=True)
        # HTTP status/body distinguishes authorized missing from denied.
        for password, ref, expected in (('wrong', 'partial', 401), ('fixture-password', 'partial', 200),
                                         ('fixture-password', 'absent', 404)):
            body = self.scratch / ('auth-probe-' + password + '-' + ref + '.json')
            result = self.command(['curl', '-q', '--netrc-file', '/dev/null', '--silent', '--show-error', '--user', 'fixture:' + password,
                '--output', str(body), '--write-out', '%{http_code}', '--header', 'Accept: ' + c.MANIFEST,
                'http://' + private_endpoint + '/v2/standard/manifests/' + ref], env=op.public_http.public_env())
            if int(result) != expected:
                raise ValueError('private registry authority/missing classification failed')
            if expected == 404 and c.read_json(body)['errors'][0]['code'] != 'MANIFEST_UNKNOWN':
                raise ValueError('authorized missing manifest not classified')
        # Corrupt only our stopped fixture registry's stored compressed blob.
        self.command(['docker', 'stop', registry])
        stored = '/var/lib/registry/docker/registry/v2/blobs/sha256/' + graph.layers[0].digest.value[7:9] + '/' + graph.layers[0].digest.value[7:] + '/data'
        corrupt = self.scratch / 'registry-corrupt-blob'
        self.command(['docker', 'cp', registry + ':' + stored, str(corrupt)])
        with corrupt.open('r+b') as stream:
            first = stream.read(1); stream.seek(0); stream.write(bytes([first[0] ^ 1]))
        self.command(['docker', 'cp', str(corrupt), registry + ':' + stored])
        self.command(['docker', 'start', registry])
        # A new toolbox has no previously fetched layer to mask corruption.
        if self.tool is not None:
            self.command(['docker', 'rm', '-f', '-v', self.tool])
            self.containers.remove(self.tool)
            self.tool = None
        cold = self.scratch / 'corrupt-registry-cold'
        rejection = self.skopeo(['copy', '--src-no-creds', '--src-tls-verify=false', '--preserve-digests',
                     'docker://' + endpoint + '/standard@' + graph.manifest.digest.value,
                     'oci:' + str(cold) + ':standard'], expected_failure=True)
        if (b'digest did not match' not in rejection.lower() and b'digest mismatch' not in rejection.lower()) or graph.layers[0].digest.value.encode() not in rejection:
            raise ValueError('cold registry rejection was not the expected corrupted blob digest mismatch')
        if self.msb:
            env = self.msb_env('corrupt-registry')
            ref = endpoint + '/standard@' + graph.manifest.digest.value
            self.command([str(self.msb), 'image', 'pull', '--insecure', ref], expected_failure=True, env=env)
            self.command([str(self.msb), 'image', 'inspect', '--format', 'json', ref], expected_failure=True, env=env)
        # Draft controls use the actual curl client against sanitized loopback
        # responses, never production GitHub tokens (separate test module).
        self.command(['python3', str(ROOT / 'script/test/release-http.py')])
        c.atomic_json(self.scratch / 'result.json', {'architecture': self.arch, 'graph': graph.json(),
            'fixture_only': True, 'msb_import_controls': self.msb is not None,
            'msb_boot': 'NOT RUN', 'logs': [op.asset(self.scratch / name).json() for name in self.logs]})


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--arch', required=True)
    parser.add_argument('--layout', type=Path)
    parser.add_argument('--msb-bin', type=Path)
    parser.add_argument('--scratch-root', type=Path, required=True)
    args = parser.parse_args()
    args.scratch_root.mkdir(parents=True, exist_ok=True)
    scratch = Path(tempfile.mkdtemp(prefix='transport-', dir=args.scratch_root)).resolve()
    controls = None
    result = 0
    try:
        controls = Controls(scratch, c.parse_arch(args.arch), args.msb_bin.resolve() if args.msb_bin else None)
        controls.execute(args.layout)
        controls.cleanup()
        controls = None
        print('fixture transports passed; retained evidence: ' + str(scratch))
    except (ValueError, OSError, KeyError, TypeError, subprocess.SubprocessError) as error:
        print(f'transport: {error}; retained {scratch}', file=sys.stderr)
        result = 1
    finally:
        if controls is not None:
            try:
                controls.cleanup()
            except (ValueError, OSError, subprocess.SubprocessError) as error:
                print(f'cleanup: {error}; retained {scratch}', file=sys.stderr)
                result = 1
    return result


if __name__ == '__main__':
    raise SystemExit(main())
