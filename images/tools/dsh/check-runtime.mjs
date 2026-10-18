#!/usr/bin/env node
// Build-only installed-tree inspection, never shipped.
//
// `--layout` answers one packaging question: does the installed tree carry the
// reviewed dsh payload and no second package manager? The installer consumes
// its `.resolvedBin`, and shell must never interpret package manifests itself.
//
// `--runtime` additionally exercises the real dependencies. A version report is
// not health: plain pnpm import can select a Cordis generation whose peers do
// not satisfy the effective declarations, native prebuilds can be missing, and
// the isolated layout can hide the private-hoisted sandbox. Each probe therefore
// loads the actual installed package from its canonical owning directory and
// behaves like the process dsh itself would start.
//
// Resolution invariant: pnpm installs the public root entry as a symlink, and
// `createRequire` on that *symlink* cannot see siblings. Every package is
// canonicalized with realpath before the next require is created, then looked
// up with an explicit `node_modules` walk from that real owner directory. That
// walk also reaches the private hoist (`node_modules/.pnpm/node_modules`) that
// carries the sandbox, and works for exports-hidden ESM packages like pi-ai
// where `createRequire.resolve` throws ERR_PACKAGE_PATH_NOT_EXPORTED. There is
// no bare-specifier import and no NODE_PATH fallback: a lookup can never leave
// the installed prefix.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import {createRequire} from 'node:module';
import os from 'node:os';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const MAX_DEPTH = 12;
const MAX_MANIFEST = 1024 * 1024;
const PTY_DEADLINE_MS = 10_000;
const SUBPROCESS_DEADLINE_MS = 10_000;
const RANGE_DEADLINE_MS = 5_000;
const PTY_MARKER = 'DSH_PTY_OK';
const SUBPROCESS_MARKER = 'DSH_SUBPROCESS_OK\n';
const SUBPROCESS_COMMAND = 'printf "DSH_SUBPROCESS_OK\\n"; exit 0';
const PROTOBUF_VALUE = 'dsh-probe';
const SUBPROCESS_GRACE_MS = 500;
const ROOT_PACKAGE = '@deepseek-ai/dsh';
const EXPECTED_BIN = {dsh: 'lib/bin.js'};
const CORDIS = '@deepseek-ai/cordis';
const SANDBOX = '@deepseek-ai/dsh-sandbox-local';
const SUBPROCESS_LOCAL = '@deepseek-ai/dsh-subprocess-local';

function readManifest(file) {
  const info = fs.lstatSync(file);
  assert.ok(info.isFile() && info.size <= MAX_MANIFEST, `${file}: expected a regular manifest <=1 MiB`);
  const value = JSON.parse(fs.readFileSync(file, 'utf8'));
  assert.ok(value !== null && typeof value === 'object' && !Array.isArray(value), `${file}: expected a JSON object`);
  return value;
}

function head(file) {
  const fd = fs.openSync(file, 'r');
  try {
    const buffer = Buffer.alloc(512);
    return buffer.subarray(0, fs.readSync(fd, buffer, 0, buffer.length, 0)).toString('utf8');
  } finally { fs.closeSync(fd); }
}

// Only directory names and .bin entries are inspected; tarball payloads are not
// parsed, so this walk stays bounded even for a 300 MB installed tree.
function forbidden(nodes) {
  const found = [];
  const pending = [[nodes, 0]];
  while (pending.length) {
    const [directory, depth] = pending.pop();
    assert.ok(depth <= MAX_DEPTH, `unexpected node_modules depth below ${nodes}: ${directory}`);
    for (const entry of fs.readdirSync(directory, {withFileTypes: true})) {
      const location = path.join(directory, entry.name);
      if (entry.name === 'pnpm') found.push({kind: 'package', path: location});
      if (entry.name === '.bin' && entry.isDirectory() && !entry.isSymbolicLink()) {
        for (const bin of fs.readdirSync(location)) {
          if (['pnpm', 'pnx', 'pnpx'].includes(bin)) found.push({kind: 'bin', path: path.join(location, bin)});
        }
      }
      if (entry.isDirectory() && !entry.isSymbolicLink()) pending.push([location, depth + 1]);
    }
  }
  return found;
}

function insidePrefix(prefixReal, target, what) {
  assert.ok(target === prefixReal || target.startsWith(prefixReal + path.sep),
    `${what}: ${target} escapes the installed prefix ${prefixReal}`);
  return target;
}

// One installed package instance, addressed by the realpath of its manifest.
function packageAt(manifestFile, prefixReal) {
  const real = insidePrefix(prefixReal, fs.realpathSync(manifestFile), manifestFile);
  assert.ok(fs.lstatSync(real).isFile(), `${real}: expected a regular manifest`);
  return {dir: path.dirname(real), file: real, manifest: readManifest(real)};
}

function describes(manifest, name) {
  return Boolean((manifest.dependencies && Object.hasOwn(manifest.dependencies, name))
    || (manifest.peerDependencies && Object.hasOwn(manifest.peerDependencies, name))
    || (manifest.optionalDependencies && Object.hasOwn(manifest.optionalDependencies, name)));
}

// An explicit Node-style ancestor walk: the directory name of a pnpm virtual
// store entry is opaque (peer-context keys, underscore-triples), so identities
// always come from the manifest and never from a path component.
function walk(parent, name, prefixReal) {
  let directory = parent.dir;
  for (;;) {
    const candidate = path.join(directory, 'node_modules', ...name.split('/'), 'package.json');
    if (fs.existsSync(candidate)) return packageAt(candidate, prefixReal);
    const up = path.dirname(directory);
    if (up === directory) break;
    directory = up;
  }
  throw new Error(`${name}: not installed below ${parent.dir}`);
}

function step(parent, name, prefixReal) {
  assert.ok(describes(parent.manifest, name), `${parent.manifest.name}: ${name} is not a declared dependency or peer`);
  const found = walk(parent, name, prefixReal);
  assert.equal(found.manifest.name, name, `${found.file}: package name is not ${name}`);
  return found;
}

// The private hoist (`node_modules/.pnpm/node_modules`) is how a package that
// declares no dependency at all still resolves its phantom import. pnpm points
// that entry at the same peer-context instance the declaring packages link.
function privateHoist(prefixReal, name) {
  const manifest = path.join(prefixReal, 'node_modules', '.pnpm', 'node_modules', ...name.split('/'), 'package.json');
  assert.ok(fs.existsSync(manifest), `${name}: absent from the private hoist`);
  const found = packageAt(manifest, prefixReal);
  assert.equal(found.manifest.name, name, `${found.file}: package name is not ${name}`);
  return found;
}

function pickCondition(target, conditions) {
  if (typeof target === 'string') return target;
  if (Array.isArray(target)) {
    for (const item of target) {
      try { return pickCondition(item, conditions); } catch { /* try the next alternative */ }
    }
  } else if (target && typeof target === 'object') {
    for (const key of Object.keys(target)) {
      if (conditions.includes(key)) return pickCondition(target[key], conditions);
    }
  }
  throw new Error(`no ${conditions.join('/')} export target`);
}

function isModule(pkg) {
  return pkg.manifest.type === 'module';
}

function entryFile(pkg, prefixReal, esm) {
  const field = pkg.manifest.exports;
  let relative;
  if (typeof field === 'string') relative = field;
  else if (field && typeof field === 'object') {
    assert.ok(Object.hasOwn(field, '.'), `${pkg.manifest.name}: exports do not expose the package root`);
    relative = pickCondition(field['.'], esm ? ['node', 'import', 'default'] : ['node', 'require', 'default']);
  }
  if (relative === undefined) relative = pkg.manifest.main || 'index.js';
  const real = insidePrefix(prefixReal, fs.realpathSync(path.join(pkg.dir, relative)), `${pkg.manifest.name} entry`);
  const info = fs.lstatSync(real);
  assert.ok(info.isFile() && info.size > 0, `${real}: expected a nonempty regular entry`);
  return real;
}

// ESM and CJS are genuinely different entry shapes: pi-ai and genai are
// ESM-only, node-pty and protobufjs are CommonJS. Loading the wrong one fails
// with a confusing module error instead of a probe diagnostic.
async function loadModule(pkg, prefixReal) {
  const esm = isModule(pkg);
  const file = entryFile(pkg, prefixReal, esm);
  if (esm) return import(pathToFileURL(file).href);
  return createRequire(pkg.file)(file);
}

function loadedNatives() {
  return Object.keys(createRequire(import.meta.url).cache)
    .filter(entry => entry.endsWith('.node'))
    .sort();
}

function layout(prefix) {
  assert.ok(path.isAbsolute(prefix), 'absolute prefix required');
  const prefixReal = fs.realpathSync(prefix);
  const selected = readManifest(path.join(prefix, 'package.json')).dependencies?.[ROOT_PACKAGE];
  assert.equal(typeof selected, 'string', 'prefix manifest must select a dsh version');
  const packageDir = path.join(prefix, 'node_modules', ...ROOT_PACKAGE.split('/'));
  const owner = packageAt(path.join(packageDir, 'package.json'), prefixReal);
  assert.equal(owner.manifest.version, selected, 'installed dsh version differs from the reviewed selection');
  assert.deepEqual(owner.manifest.bin, EXPECTED_BIN, 'installed bin layout is not the reviewed direct mapping');
  const target = path.join(owner.dir, ...EXPECTED_BIN.dsh.split('/'));
  const targetReal = insidePrefix(prefixReal, fs.realpathSync(target), 'installed dsh bin');
  const info = fs.lstatSync(target);
  assert.ok(info.isFile() && info.size > 0, 'installed dsh bin must be a nonempty regular file');
  const first = head(target).split('\n', 1)[0];
  assert.ok(first.startsWith('#!') && /(?:^|\s)node(?:\s|$)/.test(first.slice(2)),
    'installed dsh bin has no Node shebang');
  const nodes = path.join(prefix, 'node_modules');
  const nodesInfo = fs.lstatSync(nodes);
  assert.ok(nodesInfo.isDirectory() && !nodesInfo.isSymbolicLink(), 'node_modules must be a real directory');
  const virtualStore = fs.lstatSync(path.join(nodes, '.pnpm'));
  assert.ok(virtualStore.isDirectory() && !virtualStore.isSymbolicLink(),
    'isolated virtual store must remain installed payload');
  const metadata = fs.lstatSync(path.join(nodes, '.modules.yaml'));
  assert.ok(metadata.isFile() && !metadata.isSymbolicLink(), 'pnpm install metadata must be retained');
  const privateManagers = forbidden(nodes);
  assert.deepEqual(privateManagers, [], `second package manager installed: ${JSON.stringify(privateManagers)}`);
  return {prefix, prefixReal, version: selected, bin: EXPECTED_BIN.dsh, resolvedBin: targetReal,
    privateManager: false, owner};
}

function probeEnvironment(scratch) {
  // Credential-free child environment: the probe must not inherit a harness
  // token, proxy or caller state, and a child that needs PATH gets it explicitly.
  return {PATH: process.env.PATH ?? '/usr/bin:/bin', HOME: scratch, TMPDIR: scratch,
    TMP: scratch, TEMP: scratch, LANG: process.env.LANG ?? 'C'};
}

async function sandboxProbe(root, prefixReal) {
  const owners = ['@deepseek-ai/dsh-base', '@deepseek-ai/dsh-sdk-minimal'].map(name => step(root, name, prefixReal));
  const hoisted = privateHoist(prefixReal, SANDBOX);
  const resolved = [];
  for (const owner of owners) {
    // Both declared edges and the undeclared private-hoist route must land on
    // the same peer-context instance; a second copy is the failure this catches.
    const sandbox = step(owner, SANDBOX, prefixReal);
    resolved.push({owner: `${owner.manifest.name}@${owner.manifest.version}`,
      identity: `${sandbox.manifest.name}@${sandbox.manifest.version}`, dir: sandbox.dir});
  }
  resolved.push({owner: 'private hoist', identity: `${hoisted.manifest.name}@${hoisted.manifest.version}`,
    dir: hoisted.dir});
  assert.ok(resolved.every(item => item.dir === resolved[0].dir && item.identity === resolved[0].identity),
    `sandbox resolves to different instances: ${JSON.stringify(resolved)}`);
  return {identity: resolved[0].identity, resolved: resolved[0].dir,
    owners: resolved.map(item => item.owner)};
}

async function ptyProbe(subprocessOwner, prefixReal, scratch) {
  const pkg = step(subprocessOwner, 'node-pty', prefixReal);
  const pty = await loadModule(pkg, prefixReal);
  assert.equal(typeof pty.spawn, 'function', 'node-pty exposes no spawn');
  const child = pty.spawn('/bin/sh', ['-c', `printf ${PTY_MARKER}`],
    {cwd: scratch, env: probeEnvironment(scratch), name: 'dumb', cols: 80, rows: 24});
  let output = '';
  const exited = new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`PTY child did not exit within ${PTY_DEADLINE_MS} ms`)), PTY_DEADLINE_MS);
    child.onData(item => { output += item; });
    child.onExit(event => { clearTimeout(timer); resolve(event); });
  });
  let outcome;
  try {
    outcome = await exited;
  } catch (error) {
    try { child.kill(); } catch { /* the child is already gone */ }
    throw error;
  }
  assert.equal(outcome.exitCode, 0, `PTY child exited ${outcome.exitCode}`);
  assert.equal(output.replace(/\r\n/g, '\n'), PTY_MARKER, `PTY output was ${JSON.stringify(output)}`);
  return {identity: `${pkg.manifest.name}@${pkg.manifest.version}`, entry: pkg.dir, exitCode: 0};
}

async function koffiProbe(subprocessOwner, prefixReal) {
  const pkg = step(subprocessOwner, 'koffi', prefixReal);
  const namespace = await loadModule(pkg, prefixReal);
  const koffi = typeof namespace.load === 'function' ? namespace : namespace.default;
  assert.ok(koffi && typeof koffi.load === 'function', 'koffi exposes no load');
  // A harmless libc call proves the shipped prebuild loads in this ABI. No
  // compilation or install script runs as a fallback.
  const libc = koffi.load(process.platform === 'linux' ? 'libc.so.6' : 'libc.dylib');
  const getpid = libc.func('int getpid(void)');
  assert.equal(getpid(), process.pid, 'libc getpid disagrees with this process');
  if (typeof libc.unload === 'function') libc.unload();
  return {identity: `${pkg.manifest.name}@${pkg.manifest.version}`, pid: process.pid};
}

async function genaiProbe(root, prefixReal) {
  const base = step(root, '@deepseek-ai/dsh-base', prefixReal);
  const llm = step(base, '@deepseek-ai/dsh-llm-pi-ai', prefixReal);
  const piAi = step(llm, '@earendil-works/pi-ai', prefixReal);
  const genai = step(piAi, '@google/genai', prefixReal);
  const namespace = await loadModule(genai, prefixReal);
  const api = typeof namespace.GoogleGenAI === 'function' ? namespace : namespace.default;
  assert.equal(typeof api.GoogleGenAI, 'function', '@google/genai exposes no GoogleGenAI constructor');
  return {identity: `${genai.manifest.name}@${genai.manifest.version}`, package: genai, owners: [
    `${base.manifest.name}@${base.manifest.version}`, `${llm.manifest.name}@${llm.manifest.version}`,
    `${piAi.manifest.name}@${piAi.manifest.version}`]};
}

async function protobufProbe(genai, prefixReal) {
  const pkg = step(genai.package, 'protobufjs', prefixReal);
  const protobuf = await loadModule(pkg, prefixReal);
  assert.equal(typeof protobuf.Type, 'function', 'protobufjs exposes no Type');
  assert.equal(typeof protobuf.Field, 'function', 'protobufjs exposes no Field');
  const type = new protobuf.Type('Probe').add(new protobuf.Field('message', 1, 'string'));
  const decoded = type.decode(type.encode({message: PROTOBUF_VALUE}).finish());
  assert.equal(decoded.message, PROTOBUF_VALUE, `protobufjs roundtrip returned ${JSON.stringify(decoded)}`);
  return {identity: `${pkg.manifest.name}@${pkg.manifest.version}`, value: PROTOBUF_VALUE};
}

async function subprocessProbe(local, prefixReal, scratch) {
  const cordis = step(local, CORDIS, prefixReal);
  const context = await loadModule(cordis, prefixReal);
  const runtimeNamespace = await loadModule(local, prefixReal);
  const Runtime = runtimeNamespace.default;
  assert.equal(typeof Runtime, 'function', `${SUBPROCESS_LOCAL} exports no default runtime class`);
  // Same effective Cordis: a second copy would register a service the real one
  // cannot see, which is exactly the failure this probe must catch.
  assert.ok(Runtime.prototype instanceof context.Service,
    `${SUBPROCESS_LOCAL} resolves a different effective ${CORDIS}`);
  const ctx = new context.Context();
  const fiber = await ctx.plugin(Runtime);
  let handle;
  try {
    assert.ok(ctx.subprocess, 'the real provider did not register ctx.subprocess');
    const controller = new AbortController();
    const deadline = setTimeout(() => controller.abort(), SUBPROCESS_DEADLINE_MS);
    try {
      handle = ctx.subprocess.spawn({
        argv: ['/bin/sh', '-c', SUBPROCESS_COMMAND], cwd: scratch,
        stdio: {stdin: 'ignore', stdout: {maxBytes: 4096}, stderr: {maxBytes: 4096}},
        graceMs: SUBPROCESS_GRACE_MS, signal: controller.signal,
        env: probeEnvironment(scratch)});
      const outcome = await handle.done;
      assert.deepEqual({exitCode: outcome.exitCode, signal: outcome.signal}, {exitCode: 0, signal: null},
        `subprocess exited ${JSON.stringify(outcome)}`);
      const stdout = handle.collected.stdout.readFrom(0);
      const stderr = handle.collected.stderr.readFrom(0);
      assert.equal(stdout.lossy, false, 'subprocess stdout lost its head');
      assert.equal(stdout.text, SUBPROCESS_MARKER, `subprocess stdout was ${JSON.stringify(stdout.text)}`);
      assert.equal(stderr.text, '', `subprocess stderr was ${JSON.stringify(stderr.text)}`);
      const empty = await handle.waitForExit(AbortSignal.timeout(RANGE_DEADLINE_MS));
      assert.equal(empty, true, 'the owned process range was not empty before the deadline');
    } finally {
      clearTimeout(deadline);
    }
  } finally {
    await fiber.dispose();
  }
  return {identity: `${local.manifest.name}@${local.manifest.version}`,
    cordis: `${cordis.manifest.name}@${cordis.manifest.version}`, marker: SUBPROCESS_MARKER.trim(), exitCode: 0};
}

async function guarded(name, fn) {
  try {
    return await fn();
  } catch (error) {
    throw new Error(`${name}: ${error.message}`);
  }
}

async function runtime(prefix) {
  const installed = await guarded('layout', () => layout(prefix));
  const {prefixReal, owner} = installed;
  const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'dsh-runtime-'));
  const report = {prefix: installed.prefix, version: installed.version, bin: installed.bin,
    resolvedBin: installed.resolvedBin, privateManager: installed.privateManager};
  try {
    report.sandbox = await guarded('sandbox', () => sandboxProbe(owner, prefixReal));
    // node-pty/koffi are declared by the subprocess provider, so they must be
    // resolved from that package's own context, never from dsh or this script.
    const subprocessLocal = step(step(owner, '@deepseek-ai/dsh-sdk-minimal', prefixReal),
      SUBPROCESS_LOCAL, prefixReal);
    report.probes = {};
    report.probes.nodePty = await guarded('node-pty', () => ptyProbe(subprocessLocal, prefixReal, scratch));
    report.probes.koffi = await guarded('koffi', () => koffiProbe(subprocessLocal, prefixReal));
    const genai = await guarded('genai', () => genaiProbe(owner, prefixReal));
    report.probes.genai = {identity: genai.identity, owners: genai.owners};
    report.probes.protobuf = await guarded('protobuf', () => protobufProbe(genai, prefixReal));
    report.probes.subprocess = await guarded('subprocess', () => subprocessProbe(subprocessLocal, prefixReal, scratch));
    // Every installed native module the probes actually loaded must live inside
    // the prefix; a host/global or store-linked binary would break that.
    report.native = loadedNatives().map(file => insidePrefix(prefixReal, fs.realpathSync(file), 'loaded native module'));
    return report;
  } finally {
    fs.rmSync(scratch, {recursive: true, force: true});
  }
}

const args = process.argv.slice(2);
const mode = args.find(value => value.startsWith('--'));
const prefix = args.find(value => !value.startsWith('--'));
const modes = {['--layout']: () => layout(prefix), ['--runtime']: () => runtime(prefix)};
if (!prefix || args.length !== 2 || !Object.hasOwn(modes, mode)) {
  console.error('usage: check-runtime.mjs PREFIX --layout|--runtime');
  process.exitCode = 2;
} else {
  try {
    const result = await modes[mode]();
    if (mode === '--layout') {
      const {version, bin, resolvedBin} = result;
      console.log(JSON.stringify({prefix, version, bin, resolvedBin, privateManager: false}));
    } else {
      console.log(JSON.stringify(result));
    }
  } catch (error) {
    console.error(mode === '--layout' ? `dsh-runtime: layout: ${error.message}` : `dsh-runtime: ${error.message}`);
    process.exitCode = 1;
  }
}
