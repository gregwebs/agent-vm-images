#!/usr/bin/env python3
"""Build a fixture dsh prefix for the build-gate tests.

The tree mirrors the physical pnpm isolated layout (public root symlink, opaque
virtual-store entry, private hoist below `.pnpm/node_modules`) because the
verifier's resolution rules are what is under test. The packages themselves are
tiny stubs: this fixture controls the gate's control flow and proves the
resolution/canonicalization rules, never that the real harness works.
"""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import sys

FIXTURES = Path(__file__).resolve().parent
ROOT = '@deepseek-ai/dsh'
BASE = '@deepseek-ai/dsh-base'
SDK = '@deepseek-ai/dsh-sdk-minimal'
SUBPROCESS_LOCAL = '@deepseek-ai/dsh-subprocess-local'
LLM_PI_AI = '@deepseek-ai/dsh-llm-pi-ai'
PI_AI = '@earendil-works/pi-ai'
GENAI = '@google/genai'
SANDBOX = '@deepseek-ai/dsh-sandbox-local'
CORDIS = '@deepseek-ai/cordis'
UPSTREAM_VERSION = '0.1.5-rc.3'

PTY_SOURCE = """'use strict';
const {spawn} = require('node:child_process');
function spawnPty(file, args, options) {
  const child = spawn(file, args, {cwd: options.cwd, env: options.env, stdio: ['ignore', 'pipe', 'pipe']});
  let onData = () => {};
  let onExit = () => {};
  child.stdout.on('data', chunk => onData(chunk.toString()));
  child.stderr.on('data', chunk => onData(chunk.toString()));
  child.on('exit', (code, signal) => onExit({exitCode: code, signal}));
  return {onData(handler) { onData = handler; }, onExit(handler) { onExit = handler; },
    kill() { try { child.kill('SIGKILL'); } catch {} }};
}
module.exports = {spawn: spawnPty};
"""

KOFFI_SOURCE = """export function load(name) {
  return {func() { return () => process.pid; }, unload() {}};
}
export default {load};
"""

CORDIS_SOURCE = """export class Service {
  constructor(ctx, name) { this.ctx = ctx; this.name = name; ctx.services[name] = this; }
}
export class Context {
  constructor() { this.services = {}; this.logger = {warn() {}}; }
  effect(run) { return run(); }
  get subprocess() { return this.services.subprocess; }
  async plugin(Plugin) {
    const instance = new Plugin(this);
    this.services[instance.name] = instance;
    return {dispose: async () => { delete this.services[instance.name]; }};
  }
}
"""

SUBPROCESS_SOURCE = """import {Service} from '@deepseek-ai/cordis';
export class SubprocessRuntime extends Service {
  constructor(ctx) { super(ctx, 'subprocess'); }
}
export default SubprocessRuntime;
"""

SUBPROCESS_LOCAL_SOURCE = """import {spawn} from 'node:child_process';
import {SubprocessRuntime} from '@deepseek-ai/dsh-subprocess';
export class LocalSubprocessRuntime extends SubprocessRuntime {
  spawn(spec) {
    const child = spawn(spec.argv[0], spec.argv.slice(1),
      {cwd: spec.cwd, env: {...process.env, ...(spec.env || {})}, stdio: ['ignore', 'pipe', 'pipe']});
    let out = '';
    let err = '';
    child.stdout.on('data', chunk => { out += chunk.toString(); });
    child.stderr.on('data', chunk => { err += chunk.toString(); });
    const done = new Promise((resolve, reject) => {
      child.on('error', reject);
      child.on('exit', (code, signal) => resolve({exitCode: code, signal}));
    });
    return {
      collected: {
        stdout: {readFrom: () => ({text: out, nextOffset: out.length, lossy: false})},
        stderr: {readFrom: () => ({text: err, nextOffset: err.length, lossy: false})},
      },
      done,
      terminate() { try { child.kill('SIGKILL'); } catch {} },
      async waitForExit() { await done; return true; },
    };
  }
}
export default LocalSubprocessRuntime;
"""

FOREIGN_LOCAL_SOURCE = SUBPROCESS_LOCAL_SOURCE.replace(
    "import {SubprocessRuntime} from '@deepseek-ai/dsh-subprocess';",
    'class SubprocessRuntime { constructor(ctx) { this.ctx = ctx; this.name = "subprocess"; ctx.services.subprocess = this; } }')

GENAI_SOURCE = """export class GoogleGenAI {
  constructor(options) { this.options = options; }
}
"""

PROTOBUF_SOURCE = """'use strict';
class Field {
  constructor(name, id, type) { this.name = name; this.id = id; this.type = type; }
}
class Type {
  constructor(name) { this.name = name; this.fields = []; }
  add(field) { this.fields.push(field); return this; }
  encode(value) { const json = JSON.stringify(value); return {finish: () => Buffer.from(json)}; }
  decode(buffer) { return JSON.parse(buffer.toString()); }
}
module.exports = {Type, Field};
"""


def package(name, version, files, **fields):
    spec = {'name': name, 'version': version, 'files': files}
    spec.update(fields)
    return spec


def specs(version, mode):
    packages = {
        CORDIS: package(CORDIS, '4.0.2', {'lib/index.js': CORDIS_SOURCE}, type='module', main='lib/index.js'),
        '@deepseek-ai/dsh-subprocess': package(
            '@deepseek-ai/dsh-subprocess', UPSTREAM_VERSION, {'lib/index.js': SUBPROCESS_SOURCE},
            type='module', exports={'.': {'default': './lib/index.js'}},
            dependencies={CORDIS: '4.0.2'}),
        SUBPROCESS_LOCAL: package(
            SUBPROCESS_LOCAL, UPSTREAM_VERSION, {'lib/index.js': SUBPROCESS_LOCAL_SOURCE},
            type='module', exports={'.': {'types': './lib/types/index.d.ts', 'default': './lib/index.js'}},
            dependencies={'node-pty': '1.2.0-beta.15', 'koffi': '3.3.1',
                          '@deepseek-ai/dsh-subprocess': UPSTREAM_VERSION},
            peerDependencies={CORDIS: '4.0.2'}),
        'node-pty': package('node-pty', '1.2.0-beta.15', {'lib/index.js': PTY_SOURCE}, main='./lib/index.js'),
        'koffi': package('koffi', '3.3.1', {'index.js': KOFFI_SOURCE}, type='module',
                         exports={'.': {'import': './index.js', 'require': './index.cjs'}}),
        SANDBOX: package(SANDBOX, UPSTREAM_VERSION, {'lib/index.js': "export const profile = 'fixture';\n"},
                         type='module', main='lib/index.js'),
        PI_AI: package(
            PI_AI, '0.85.1', {'dist/index.js': "export const provider = 'fixture-pi-ai';\n"},
            type='module', exports={'.': {'types': './dist/index.d.ts', 'import': './dist/index.js'}},
            dependencies={GENAI: '^1.52.0'}),
        LLM_PI_AI: package(
            LLM_PI_AI, UPSTREAM_VERSION, {'lib/index.js': 'export default {};\n'},
            type='module', exports={'.': {'default': './lib/index.js'}},
            dependencies={PI_AI: '^0.85.1'}),
        GENAI: package(
            GENAI, '1.52.0',
            {'dist/node/index.mjs': GENAI_SOURCE, 'dist/index.mjs': 'export const browser = true;\n',
             'dist/web/index.mjs': 'export const web = true;\n'},
            type='module',
            exports={'.': {'browser': {'import': './dist/web/index.mjs'},
                           'node': {'types': './dist/node/node.d.ts', 'import': './dist/node/index.mjs'},
                           'types': './dist/genai.d.ts', 'import': './dist/index.mjs',
                           'default': './dist/index.mjs'}},
            dependencies={'protobufjs': '^7.6.6'}),
        'protobufjs': package('protobufjs', '7.6.6', {'index.js': PROTOBUF_SOURCE}, main='index.js'),
        BASE: package(
            BASE, UPSTREAM_VERSION, {'lib/index.js': 'export const base = true;\n'},
            type='module', exports={'.': {'default': './lib/index.js'}},
            dependencies={SANDBOX: UPSTREAM_VERSION, LLM_PI_AI: UPSTREAM_VERSION,
                          SUBPROCESS_LOCAL: UPSTREAM_VERSION},
            peerDependencies={CORDIS: '4.0.2'}),
        SDK: package(
            SDK, UPSTREAM_VERSION, {'lib/index.js': 'export const sdk = true;\n'},
            type='module', exports={'.': {'default': './lib/index.js'}},
            dependencies={SANDBOX: UPSTREAM_VERSION, SUBPROCESS_LOCAL: UPSTREAM_VERSION},
            peerDependencies={CORDIS: '4.0.2'}),
    }
    if mode == 'missing-sandbox':
        del packages[SANDBOX]
    if mode == 'missing-node-pty':
        del packages['node-pty']
    if mode == 'missing-koffi':
        del packages['koffi']
    if mode == 'missing-pi-ai':
        del packages[PI_AI]
    if mode == 'genai-missing':
        del packages[GENAI]
    if mode == 'genai-wrong-export':
        packages[GENAI]['files']['dist/node/index.mjs'] = 'export class NotGenai {}\n'
    if mode == 'pty-no-marker':
        packages['node-pty']['files']['lib/index.js'] = PTY_SOURCE.replace(
            "child.stdout.on('data', chunk => onData(chunk.toString()));", '')
    if mode == 'pty-exit-nonzero':
        packages['node-pty']['files']['lib/index.js'] = PTY_SOURCE.replace(
            'onExit({exitCode: code, signal})', 'onExit({exitCode: 3, signal})')
    if mode == 'koffi-wrong-pid':
        packages['koffi']['files']['index.js'] = KOFFI_SOURCE.replace(
            'return () => process.pid;', 'return () => process.pid + 1;')
    if mode == 'protobuf-bad-roundtrip':
        packages['protobufjs']['files']['index.js'] = PROTOBUF_SOURCE.replace(
            "return JSON.parse(buffer.toString());", "return {message: 'fixture'};")
    if mode == 'subprocess-empty-output':
        packages[SUBPROCESS_LOCAL]['files']['lib/index.js'] = SUBPROCESS_LOCAL_SOURCE.replace(
            "child.stdout.on('data', chunk => { out += chunk.toString(); });", '')
    if mode == 'subprocess-nonzero':
        packages[SUBPROCESS_LOCAL]['files']['lib/index.js'] = SUBPROCESS_LOCAL_SOURCE.replace(
            'resolve({exitCode: code, signal})', 'resolve({exitCode: 1, signal: null})')
    if mode == 'subprocess-wrong-cordis':
        packages[SUBPROCESS_LOCAL]['files']['lib/index.js'] = FOREIGN_LOCAL_SOURCE
    return packages


def write_package(directory, spec):
    directory.mkdir(parents=True)
    manifest = {'name': spec['name'], 'version': spec['version']}
    for key in ('type', 'main', 'exports', 'bin', 'dependencies', 'peerDependencies'):
        if key in spec:
            manifest[key] = spec[key]
    (directory / 'package.json').write_text(json.dumps(manifest, indent=2) + '\n')
    for relative, content in spec['files'].items():
        path = directory / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)


def build(prefix, version, mode, pnpm_version):
    if prefix.exists():
        shutil.rmtree(prefix)
    prefix.mkdir(parents=True)
    packages = specs(version, mode)
    nodes = prefix / 'node_modules'
    hoist = nodes / '.pnpm' / 'node_modules'
    hoist.mkdir(parents=True)
    for name, spec in packages.items():
        write_package(hoist / name, spec)
    identifier = '@deepseek-ai+dsh@' + version
    root = nodes / '.pnpm' / identifier / 'node_modules' / ROOT
    root_package = package(ROOT, version, {'lib/bin.js': (FIXTURES / 'bin.js').read_text()},
                           bin={'dsh': 'lib/bin.js'} if mode != 'bin-wrong' else {'dsh': 'lib/cli.js'},
                           dependencies={BASE: UPSTREAM_VERSION, SDK: UPSTREAM_VERSION})
    if mode == 'bin-wrong':
        root_package['files']['lib/cli.js'] = '#!/usr/bin/env node\n'
    write_package(root, root_package)
    (root / 'lib/bin.js').chmod(0o755)
    if mode == 'bin-wrong':
        (root / 'lib/cli.js').chmod(0o755)
    (nodes / '@deepseek-ai').mkdir()
    (nodes / '@deepseek-ai' / 'dsh').symlink_to(
        Path('..') / '.pnpm' / identifier / 'node_modules' / ROOT)
    if mode == 'private-pnpm':
        write_package(nodes / 'pnpm', package('pnpm', pnpm_version, {'index.js': ''}))
    if mode == 'escaping-sandbox':
        outside = prefix.parent / 'sandbox-escape'
        outside.mkdir(exist_ok=True)
        target = outside / 'dsh-sandbox-local'
        shutil.rmtree(hoist / SANDBOX)
        write_package(target, packages.get(SANDBOX, package(SANDBOX, UPSTREAM_VERSION, {})))
        (hoist / SANDBOX).symlink_to(target)
    (nodes / '.modules.yaml').write_text("hoistPattern:\n  - '*'\n")
    manifest = {'name': 'agent-vm-dsh-layer', 'private': True,
                'packageManager': f'pnpm@{pnpm_version}',
                'dependencies': {ROOT: version}}
    (prefix / 'package.json').write_text(json.dumps(manifest, indent=2) + '\n')
    (prefix / 'pnpm-workspace.yaml').write_text('packages: [.]\n')
    (prefix / 'pnpm-lock.yaml').write_text("lockfileVersion: '9.0'\n")
    inputs = [prefix / name for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml')]
    (prefix / 'pnpm-inputs.sha256').write_text(''.join(
        f'{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n' for path in inputs))
    for path in [prefix, *prefix.rglob('*')]:
        if path.is_symlink():
            continue
        executable = path.name in ('bin.js', 'cli.js')
        path.chmod(0o755 if path.is_dir() or executable else 0o644)


def main(argv):
    parser = argparse.ArgumentParser()
    parser.add_argument('prefix')
    parser.add_argument('--version', default='0.0.0-fixture')
    parser.add_argument('--pnpm', default='11.7.0')
    parser.add_argument('--mode', default='healthy')
    options = parser.parse_args(argv)
    build(Path(options.prefix), options.version, options.mode, options.pnpm)
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
