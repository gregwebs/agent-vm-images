#!/usr/bin/env node
// Build-only runtime proof for the shipped Pi tree, never shipped.
//
// A byte-equal tree can still be a broken agent: the N6 lifecycle-script packages
// are trusted on the strength of their *shipped* code, not of their install
// scripts, and the isolated layout decides which of several copies a module call
// actually reaches. Each probe therefore loads a package from the instance the
// lock's own edges resolved, not from a bare specifier: a probe that succeeded
// through a different copy would prove nothing about this image.
//
// The two shapes are proven by their authenticated contents, never by a version
// threshold. The historical selection ships `chord`, and its TypeScript transform
// is the real check that esbuild's native optional package is present and usable
// without the postinstall download fallback. The modern selection has no chord
// and no esbuild at all, and that absence is asserted explicitly rather than
// skipped, so a silently reintroduced transform dependency fails here.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {pathToFileURL} from 'node:url';

import {
  GraphError, platformAllows, readGraph, readManifest, resolveInstances, targetPlatform,
} from './installed-graph.mjs';

const PROBE_TIMEOUT_MS = 120_000;
const PROTOBUF_VALUE = 'pi-runtime-probe';
const TYPESCRIPT_SOURCE = 'let x: number = 1';
const MODERN_ABSENT = ['@earendil-works/chord', 'esbuild'];
const MODERN_SHIPPED = ['quickjs-wasi', 'cross-spawn'];

const fail = (code, detail) => { throw new GraphError(code, detail); };

function usage() {
  process.stderr.write('usage: check-runtime.mjs PREFIX\n');
  process.exit(2);
}

function instanceNamed(resolution, name) {
  for (const instance of resolution.instances.values()) {
    if (instance.manifest.name === name) return instance;
  }
  return undefined;
}

// A package's own entry, taken from its manifest as data. `exports` is not
// honour-able without a resolver, so a package with only conditional exports
// needs an explicit main; refusing is better than importing a guess.
function entryFile(instance) {
  const manifest = instance.manifest;
  const exported = manifest.exports;
  const candidates = [];
  if (typeof exported === 'string') candidates.push(exported);
  if (exported !== null && typeof exported === 'object' && !Array.isArray(exported)) {
    const root = exported['.'] ?? exported;
    if (typeof root === 'string') candidates.push(root);
    if (root !== null && typeof root === 'object') {
      for (const key of ['import', 'node', 'default', 'require']) {
        if (typeof root[key] === 'string') candidates.push(root[key]);
      }
    }
  }
  if (typeof manifest.main === 'string') candidates.push(manifest.main);
  for (const candidate of candidates) {
    const file = path.join(instance.dir, candidate);
    if (fs.existsSync(file)) return file;
  }
  fail('missing-entry', `${instance.manifest.name}@${instance.manifest.version} declares no importable entry`);
}

async function load(instance) {
  return import(pathToFileURL(entryFile(instance)).href);
}

function requirePackage(resolution, name) {
  const instance = instanceNamed(resolution, name);
  if (instance === undefined) fail('missing-probe-package', `${name} is not installed`);
  return instance;
}

async function probeGenai(resolution) {
  const instance = requirePackage(resolution, '@google/genai');
  const module = await load(instance);
  const exported = Object.keys(module).filter(key => /GenAI|Client|Google/.test(key));
  assert.ok(exported.length > 0,
    `@google/genai shipped no client export (the preinstall is a no-op echo, so the dist JS must carry it)`);
  return `${instance.manifest.version} (${exported.length} client exports)`;
}

async function probeProtobuf(resolution) {
  const instance = requirePackage(resolution, 'protobufjs');
  const module = await load(instance);
  const protobuf = module.default ?? module;
  assert.equal(typeof protobuf.Root, 'function', 'protobufjs shipped no Root constructor');
  const root = protobuf.Root.fromJSON({nested: {Probe: {fields: {value: {type: 'string', id: 1}}}}});
  const Type = root.lookupType('Probe');
  const encoded = Type.encode({value: PROTOBUF_VALUE}).finish();
  const decoded = Type.decode(encoded).value;
  assert.equal(decoded, PROTOBUF_VALUE, 'protobufjs encode/decode round trip changed the value');
  return `${instance.manifest.version} (${encoded.length} bytes)`;
}

// The transform must go through chord's resolution context, so a missing native
// package is a hard failure instead of a silent postinstall download.
async function probeLegacyTransform(resolution, platform) {
  const chord = requirePackage(resolution, 'chord');
  const esbuild = requirePackage(resolution, 'esbuild');
  for (const owner of [chord, esbuild]) {
    const declared = owner.children.get('esbuild');
    if (declared !== undefined && declared.dir !== esbuild.dir) {
      fail('probe-context', `${owner.key} resolves esbuild to a different instance`);
    }
  }
  const module = await load(esbuild);
  const transform = module.transformSync ?? module.default?.transformSync;
  assert.equal(typeof transform, 'function', 'esbuild shipped no transformSync');
  const output = transform(TYPESCRIPT_SOURCE, {loader: 'ts'}).code ?? transform(TYPESCRIPT_SOURCE, {loader: 'ts'});
  assert.ok(!/: number/.test(output), `the TypeScript annotation survived the transform: ${JSON.stringify(output)}`);
  assert.ok(/let x = 1|var x = 1|const x = 1/.test(output), `unexpected transform output: ${JSON.stringify(output)}`);

  return {output: output.trim(), native: platformNative(resolution, platform)};
}

function platformNative(resolution, platform) {
  const wanted = `@esbuild/${platform.os}-${platform.cpu === 'x64' ? 'x64' : platform.cpu}`;
  const instance = instanceNamed(resolution, wanted);
  if (instance === undefined) {
    const available = [...resolution.instances.values()]
      .map(entry => entry.manifest.name).filter(name => name.startsWith('@esbuild/'));
    fail('missing-native', `${wanted} is not installed; a missing native package is never replaced by a download (have ${JSON.stringify(available)})`);
  }
  return instance;
}

function checkNativeBinary(native) {
  const declared = readManifest(native.dir, 'esbuild native').bin;
  const relative = typeof declared === 'string' ? declared : declared?.esbuild;
  assert.equal(typeof relative, 'string', `${native.key} declares no esbuild command`);
  const binary = path.join(native.dir, ...relative.split('/'));
  const info = fs.lstatSync(binary);
  assert.ok(info.isFile(), `${binary} is not a regular file`);
  assert.equal(info.mode & 0o111, 0o111, `${binary} is not executable`);
  try {
    fs.accessSync(binary, fs.constants.X_OK);
  } catch {
    fail('native-access', `${binary} is not executable by this user`);
  }
  const ran = spawnSync(binary, ['--version'], {encoding: 'utf8', timeout: PROBE_TIMEOUT_MS});
  if (ran.status !== 0) fail('native-version', `${binary} --version exited ${ran.status}: ${(ran.stderr ?? '').trim()}`);
  const reported = ran.stdout.trim();
  if (reported !== native.manifest.version) {
    fail('native-version', `${binary} reports ${JSON.stringify(reported)}, the lock says ${native.manifest.version}`);
  }
  return reported;
}

// The wrapper's default entry is this link. A generated shim or a wrapper here
// would make `pi --version` prove something other than the installed CLI.
function checkRootCommand(resolution) {
  const root = resolution.root;
  const declared = root.manifest.bin;
  const relative = typeof declared === 'string' ? declared : declared?.pi;
  if (typeof relative !== 'string') fail('root-command', `${root.key} declares no pi command`);
  const link = path.join(resolution.prefix, 'node_modules', '.bin', 'pi');
  let info;
  try {
    info = fs.lstatSync(link);
  } catch {
    fail('root-command', `${link} is absent`);
  }
  if (!info.isSymbolicLink()) fail('root-command', `${link} is not a direct symlink`);
  // pnpm links a project's direct commands through the public package link, not
  // through the virtual-store directory, so that is the text it must have; the
  // resolved file still has to be the locked root instance's own command.
  const exported = path.join(resolution.prefix, 'node_modules', ...root.manifest.name.split('/'), ...relative.split('/'));
  const expected = path.relative(path.dirname(link), exported);
  if (fs.readlinkSync(link) !== expected) {
    fail('root-command', `${link} points at ${fs.readlinkSync(link)}, the canonical target is ${expected}`);
  }
  const command = path.join(root.dir, ...relative.split('/'));
  const target = fs.realpathSync(link);
  if (target !== fs.realpathSync(command)) fail('root-command', `${link} resolves to ${target}, not ${command}`);
  const targetInfo = fs.lstatSync(command);
  if (!targetInfo.isFile() || (targetInfo.mode & 0o111) !== 0o111) {
    fail('root-command', `${command} is not an executable regular file`);
  }
  return relative;
}

async function probeModernShipped(resolution) {
  const loaded = [];
  for (const name of MODERN_SHIPPED) {
    const instance = instanceNamed(resolution, name);
    if (instance === undefined) continue; // a selection that does not ship it
    await load(instance);
    loaded.push(`${name}@${instance.manifest.version}`);
  }
  return loaded;
}

async function main(argv) {
  if (argv.length !== 1) usage();
  const prefix = argv[0];
  const contractDir = process.env.AGENT_VM_CONTRACT_DIR ?? '/tmp/recipe-contract';
  const platform = targetPlatform();
  const graph = readGraph({contractDir, prefix});
  const resolution = resolveInstances(prefix, graph, {platform});
  const shape = resolution.family.shape;
  const legacy = shape.includes('chord');

  const command = checkRootCommand(resolution);
  const genai = await probeGenai(resolution);
  const protobuf = await probeProtobuf(resolution);

  const report = {prefix, shape, platform: platform.spelling, command, genai, protobuf};
  if (legacy) {
    const esbuild = instanceNamed(resolution, 'esbuild');
    if (esbuild === undefined) fail('missing-probe-package', 'the legacy shape ships chord and must ship esbuild');
    const transform = await probeLegacyTransform(resolution, platform);
    report.transform = transform.output;
    report.native = checkNativeBinary(transform.native);
    report.nativePackage = transform.native.manifest.name;
  } else {
    for (const name of MODERN_ABSENT) {
      if (instanceNamed(resolution, name) !== undefined) {
        fail('unexpected-transform-dependency', `${name} is installed in a selection that must not ship it`);
      }
      if (graph.packages.has(`${name}@${graph.snapshots.get(resolution.root.key).id.packageId.version}`)) {
        fail('unexpected-transform-dependency', `the lock carries ${name} in a selection that must not ship it`);
      }
    }
    report.dependencies = await probeModernShipped(resolution);
  }
  report.esbuildOptionals = [...graph.packages.values()]
    .filter(entry => entry.id.name.startsWith('@esbuild/'))
    .map(entry => ({name: entry.id.name, allowed: platformAllows(entry.platform, platform)}));
  process.stdout.write(`pi runtime: ${JSON.stringify(report)}\n`);
}

try {
  await main(process.argv.slice(2));
} catch (error) {
  if (error instanceof GraphError) {
    process.stderr.write(`check-runtime.mjs: ${error.message}\n`);
    process.exit(1);
  }
  // A failed probe is a product failure, not a stack trace for the reader.
  process.stderr.write(`check-runtime.mjs: probe-failed: ${error.message}\n`);
  process.exit(1);
}
