#!/usr/bin/env node
// Hermetic mutant table for the Pi installed-byte checker.
//
// Every case builds a real isolated-layout fixture (`fixtures/pi-installed-bytes.py`),
// applies exactly one filesystem or graph mutation, asserts the exact diagnostic,
// then reverts the mutation and re-runs to prove the repaired tree passes *without*
// reinstalling or re-normalising modes. The reviewed bin tables below are the
// plan's independent expectation; the checker derives its own from authenticated
// manifests.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

const checker = fileURLToPath(new URL('../../images/tools/pi/check-installed-bytes.mjs', import.meta.url));
const builder = fileURLToPath(new URL('fixtures/pi-installed-bytes.py', import.meta.url));
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pi-bytes-test-'));
const SIBLING_SCOPE = '@earendil-works';

// The reviewed expectation from the issue. It is never the checker's input; the
// positive runs must derive the same sets from the fixture's authenticated
// manifests, unrelated to this table.
const REVIEWED_BINS = {
  legacy: {
    chord: ['esbuild'],
    'pi-agent-core': ['pi-ai', 'yaml'],
    'pi-ai': ['anthropic-ai-sdk'],
    'pi-telemetry': [],
    'pi-tui': ['marked'],
  },
  modern: {
    'pi-agent-core': ['pi-ai'],
    'pi-ai': ['anthropic-ai-sdk'],
    'pi-codemode': [],
    'pi-mcp': [],
    'pi-telemetry': [],
    'pi-tui': ['marked'],
  },
};

function build(shape) {
  const root = path.join(work, shape);
  const result = spawnSync('python3', [builder, 'build', root, shape], {encoding: 'utf8', timeout: 120000});
  assert.equal(result.status, 0, result.stderr);
  const meta = JSON.parse(result.stdout);
  meta.work = root;
  return meta;
}

function run(meta, {sibling = meta.sibling, extraction, env = {}} = {}) {
  const environment = {
    ...process.env,
    AGENT_VM_CONTRACT_DIR: meta.contract,
    AGENT_VM_PI_TARGET_PLATFORM: 'linux/arm64',
    ...env,
  };
  for (const [key, value] of Object.entries(env)) {
    if (value === undefined) delete environment[key];
  }
  const version = versionOf(meta, sibling);
  const operand = extraction ?? (version === undefined
    ? path.join(meta.parent, sibling)
    : path.join(meta.parent, ...`${SIBLING_SCOPE}/${sibling}`.split('/'), version));
  return spawnSync(process.execPath, [checker, meta.prefix, sibling, operand], {
    encoding: 'utf8', timeout: 180000, env: environment,
  });
}

function graphOf(meta) {
  return JSON.parse(fs.readFileSync(meta.graph, 'utf8'));
}

function versionOf(meta, base) {
  return meta.versions[`${SIBLING_SCOPE}/${base}`];
}

function ownerDir(meta, name) {
  const graph = graphOf(meta);
  const snapshot = graph.snapshots.find(entry => entry.id.packageId.name === name);
  const entry = meta.entryDirectory[snapshot.id.key];
  return path.join(entry, 'node_modules', ...name.split('/'));
}

function expectPass(meta, label) {
  const result = run(meta);
  assert.equal(result.status, 0, `${label}: ${result.stderr}`);
  assert.equal(result.stdout, `${meta.family.length}/${meta.family.length} Pi sibling archives and installed bytes verified\n`,
    `${label}: the marker must only appear after every sibling passed`);
}

function expectCode(meta, code, label, options = {}) {
  const result = run(meta, options);
  assert.equal(result.status, 1, `${label}: expected a hard failure, got status ${result.status}: ${result.stdout}`);
  assert.ok(result.stderr.startsWith(`check-installed-bytes.mjs: ${code}: `),
    `${label}: expected ${code}, got ${result.stderr.trim()}`);
  return result;
}

// --- mutation primitives, each returning its own repair ---------------------

function fileSnapshot(file) {
  const buffer = fs.readFileSync(file);
  const mode = fs.statSync(file).mode & 0o777;
  return () => {
    fs.writeFileSync(file, buffer);
    fs.chmodSync(file, mode);
  };
}

function editFile(file, content) {
  const restore = fileSnapshot(file);
  fs.writeFileSync(file, content);
  return restore;
}

function appendFile(file) {
  const restore = fileSnapshot(file);
  fs.appendFileSync(file, '// tampered\n');
  return restore;
}

function chmodPath(target, mode) {
  const previous = fs.statSync(target).mode & 0o777;
  fs.chmodSync(target, mode);
  return () => fs.chmodSync(target, previous);
}

function removePath(target) {
  const info = fs.lstatSync(target);
  const parked = path.join(fs.mkdtempSync(path.join(work, 'stash-')), 'item');
  if (info.isSymbolicLink()) {
    const text = fs.readlinkSync(target);
    fs.rmSync(target);
    return () => fs.symlinkSync(text, target);
  }
  if (info.isDirectory()) {
    fs.cpSync(target, parked, {recursive: true, verbatimSymlinks: true});
    fs.rmSync(target, {recursive: true, force: true});
    return () => fs.cpSync(parked, target, {recursive: true, verbatimSymlinks: true});
  }
  fs.copyFileSync(target, parked);
  fs.rmSync(target);
  return () => {
    fs.copyFileSync(parked, target);
    fs.chmodSync(target, info.mode & 0o777);
  };
}

// Repair removes the highest directory this mutation created, not just the leaf,
// so a repaired tree is byte-identical to the fixture's.
function addPath(target, make) {
  let created = target;
  for (let cursor = path.dirname(target); cursor !== path.dirname(cursor); cursor = path.dirname(cursor)) {
    if (fs.existsSync(cursor)) break;
    created = cursor;
  }
  make(target);
  return () => fs.rmSync(created, {recursive: true, force: true});
}

function replaceLink(link, text) {
  const previous = fs.readlinkSync(link);
  fs.rmSync(link);
  fs.symlinkSync(text, link);
  return () => {
    fs.rmSync(link, {force: true});
    fs.symlinkSync(previous, link);
  };
}

function editGraph(meta, change) {
  const graph = graphOf(meta);
  change(graph);
  const restore = fileSnapshot(meta.graph);
  fs.writeFileSync(meta.graph, JSON.stringify(graph, null, 2) + '\n');
  return restore;
}

// A variant that must still pass: the fixture is mutated, the positive is
// re-proved, and the graph is restored. Used where two lock spellings of one
// fact must be accepted, not just rejected alike.
function withPositive(meta, label, mutate) {
  const repair = mutate();
  try {
    expectPass(meta, label);
  } finally {
    repair();
  }
}

function withMutation(meta, label, code, mutate, options = {}) {
  const repair = mutate();
  try {
    expectCode(meta, code, label, options);
  } finally {
    repair();
  }
  expectPass(meta, `${label} (repaired)`);
}

// --- fixtures and positives -------------------------------------------------

const legacy = build('legacy');
const modern = build('modern');

for (const meta of [legacy, modern]) {
  expectPass(meta, `${meta.shape} positive`);
  assert.deepEqual(meta.derivedBins, REVIEWED_BINS[meta.shape],
    `${meta.shape}: the reviewed bin table must match the fixture graph`);
  // The installed `.bin` sets are the same claim on disk, so a byte proof that
  // normalises a different set would already have failed the positive.
  for (const base of meta.family) {
    const name = `${SIBLING_SCOPE}/${base}`;
    const binDir = path.join(ownerDir(meta, name), 'node_modules', '.bin');
    const actual = fs.existsSync(binDir) ? fs.readdirSync(binDir).sort() : [];
    assert.deepEqual(actual, REVIEWED_BINS[meta.shape][base], `${meta.shape}: ${base} generated bins`);
  }
  const links = run(meta);
  assert.equal(links.status, 0);
}

// The omission is typed, not an absence: the legacy graph declares an optional
// package whose locked selectors exclude this target.
assert.equal(legacy.omittedKeys.length, 1, 'the legacy fixture must carry one platform omission');

// --- sibling tree mutants ---------------------------------------------------

withMutation(modern, 'sibling regular bytes', 'byte-mismatch', () =>
  appendFile(path.join(modern.parent, SIBLING_SCOPE, 'pi-ai', versionOf(modern, 'pi-ai'), 'dist', 'provider.js')));

withMutation(modern, 'sibling missing file', 'missing-path', () =>
  removePath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'LICENSE')));

withMutation(modern, 'archive entry missing from the tree', 'missing-path', () =>
  removePath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-telemetry`), 'dist')));

withMutation(modern, 'sibling extra file', 'unexpected-path', () =>
  addPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'dist', 'extra.js'), target => {
    fs.mkdirSync(path.dirname(target), {recursive: true});
    fs.writeFileSync(target, 'extra\n');
  }));

// A semantically valid manifest edit: identity and bin stay the same, so only the
// byte comparison can catch it.
withMutation(modern, 'installed package.json bytes', 'byte-mismatch', () => {
  const file = path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-ai`), 'package.json');
  const restore = fileSnapshot(file);
  const manifest = JSON.parse(fs.readFileSync(file, 'utf8'));
  manifest.description = 'added after installation';
  fs.writeFileSync(file, JSON.stringify(manifest, null, 2) + '\n');
  return restore;
});

withMutation(modern, 'unrelated empty directory', 'unexpected-path', () =>
  addPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-telemetry`), 'empty'), target => fs.mkdirSync(target)));

withMutation(modern, 'nested dist node_modules shadow', 'unexpected-path', () =>
  addPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'dist', 'node_modules', 'shadow'), target => {
    fs.mkdirSync(target, {recursive: true});
    fs.writeFileSync(path.join(target, 'package.json'), '{"name":"shadow","version":"1.0.0"}\n');
  }));

// --- reviewed family shape --------------------------------------------------

withMutation(legacy, 'legacy four siblings', 'family-shape', () => editGraph(legacy, graph => {
  const root = graph.snapshots.find(entry => entry.id.packageId.name === '@earendil-works/pi-coding-agent');
  root.edges = root.edges.filter(edge => edge.name !== `${SIBLING_SCOPE}/chord`);
  graph.snapshots = graph.snapshots.filter(entry => entry.id.packageId.name !== `${SIBLING_SCOPE}/chord`);
  graph.packages = graph.packages.filter(entry => entry.id.name !== `${SIBLING_SCOPE}/chord`);
}));

withMutation(legacy, 'legacy six siblings', 'family-shape', () => editGraph(legacy, graph => {
  const root = graph.snapshots.find(entry => entry.id.packageId.name === '@earendil-works/pi-coding-agent');
  root.edges.push({name: `${SIBLING_SCOPE}/pi-codemode`, target: `${SIBLING_SCOPE}/pi-codemode@${legacy.version}`, optional: false});
  graph.packages.push({...graph.packages.find(entry => entry.id.name === `${SIBLING_SCOPE}/pi-tui`),
    id: {name: `${SIBLING_SCOPE}/pi-codemode`, version: legacy.version}});
  graph.snapshots.push({id: {packageId: {name: `${SIBLING_SCOPE}/pi-codemode`, version: legacy.version}, peerContext: [], key: `${SIBLING_SCOPE}/pi-codemode@${legacy.version}`}, edges: [], optional: false});
}));

withMutation(modern, 'modern five siblings', 'family-shape', () => editGraph(modern, graph => {
  const root = graph.snapshots.find(entry => entry.id.packageId.name === '@earendil-works/pi-coding-agent');
  root.edges = root.edges.filter(edge => edge.name !== `${SIBLING_SCOPE}/pi-mcp`);
  graph.snapshots = graph.snapshots.filter(entry => entry.id.packageId.name !== `${SIBLING_SCOPE}/pi-mcp`);
  graph.packages = graph.packages.filter(entry => entry.id.name !== `${SIBLING_SCOPE}/pi-mcp`);
}));

withMutation(modern, 'modern seven siblings', 'family-shape', () => editGraph(modern, graph => {
  const root = graph.snapshots.find(entry => entry.id.packageId.name === '@earendil-works/pi-coding-agent');
  root.edges.push({name: `${SIBLING_SCOPE}/chord`, target: `${SIBLING_SCOPE}/chord@${modern.version}`, optional: false});
  graph.packages.push({...graph.packages.find(entry => entry.id.name === `${SIBLING_SCOPE}/pi-tui`),
    id: {name: `${SIBLING_SCOPE}/chord`, version: modern.version}});
  graph.snapshots.push({id: {packageId: {name: `${SIBLING_SCOPE}/chord`, version: modern.version}, peerContext: [], key: `${SIBLING_SCOPE}/chord@${modern.version}`}, edges: [], optional: false});
}));

// --- identity, context and edges -------------------------------------------

withMutation(modern, 'mismatched sibling version', 'manifest-mismatch', () => editGraph(modern, graph => {
  const moved = '1.4.1';
  const name = `${SIBLING_SCOPE}/pi-tui`;
  for (const entry of graph.packages) if (entry.id.name === name) entry.id.version = moved;
  for (const entry of graph.snapshots) {
    if (entry.id.packageId.name !== name) continue;
    entry.id.packageId.version = moved;
    entry.id.key = `${name}@${moved}`;
  }
  const root = graph.snapshots.find(entry => entry.id.packageId.name === '@earendil-works/pi-coding-agent');
  for (const edge of root.edges) if (edge.name === name) edge.target = `${name}@${moved}`;
}));

withMutation(modern, 'hashless sibling', 'lock-sri', () => editGraph(modern, graph => {
  for (const entry of graph.packages) if (entry.id.name === `${SIBLING_SCOPE}/pi-tui`) delete entry.integrity;
}));

// A locked record with no installed directory is a payload the proof cannot
// account for, and an installed directory no lock edge reaches is caught below.
withMutation(modern, 'orphan locked snapshot', 'unreachable-instance', () => editGraph(modern, graph => {
  const key = 'diff@8.0.4(thirdctx)';
  graph.packages.push({...graph.packages.find(entry => entry.id.name === 'diff'), id: {name: 'diff', version: '8.0.4'}});
  graph.snapshots.push({id: {packageId: {name: 'diff', version: '8.0.4'}, peerContext: [], key}, edges: [], optional: false});
}));

withMutation(modern, 'dependency link retarget to another context', 'wrong-context-substitution', () => {
  const graph = graphOf(modern);
  const mcp = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-mcp`);
  const core = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-agent-core`);
  const mcpDiff = mcp.edges.find(edge => edge.name === 'diff').target;
  const coreDiff = core.edges.find(edge => edge.name === 'diff').target;
  assert.notEqual(mcpDiff, coreDiff, 'the fixture must carry two diff contexts');
  const link = path.join(modern.entryDirectory[mcp.id.key], 'node_modules', 'diff');
  const target = path.join(modern.entryDirectory[coreDiff], 'node_modules', 'diff');
  return replaceLink(link, path.relative(path.dirname(link), target));
});

withMutation(modern, 'dependency link escape', 'link-escape', () => {
  const graph = graphOf(modern);
  const core = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-agent-core`);
  const link = path.join(modern.entryDirectory[core.id.key], 'node_modules', 'diff');
  // An extracton root outside the virtual store: it resolves, so the refusal is
  // containment, not a broken link.
  return replaceLink(link, path.join(modern.parent, SIBLING_SCOPE, 'pi-ai', versionOf(modern, 'pi-ai')));
});

withMutation(modern, 'dependency link retarget to another identity', 'link-layout', () => {
  const graph = graphOf(modern);
  const core = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-agent-core`);
  const entries = modern.entryDirectory;
  const link = path.join(entries[core.id.key], 'node_modules', `${SIBLING_SCOPE}/pi-ai`);
  const telemetry = path.join(entries[`${SIBLING_SCOPE}/pi-telemetry@${modern.version}`], 'node_modules', `${SIBLING_SCOPE}/pi-telemetry`);
  return replaceLink(link, path.relative(path.dirname(link), telemetry));
});

withMutation(modern, 'dangling dependency link', 'dangling-link', () => {
  const graph = graphOf(modern);
  const core = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-agent-core`);
  const link = path.join(modern.entryDirectory[core.id.key], 'node_modules', 'diff');
  return replaceLink(link, '../.pnpm/absent-entry/node_modules/diff');
});

withMutation(modern, 'cyclic dependency link', 'dangling-link', () => {
  const graph = graphOf(modern);
  const core = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-agent-core`);
  const link = path.join(modern.entryDirectory[core.id.key], 'node_modules', 'diff');
  return replaceLink(link, 'diff');
});

withMutation(modern, 'unexplained entry in a virtual-store scope', 'unexpected-entry', () => {
  const graph = graphOf(modern);
  const mcp = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-mcp`);
  const scope = path.join(modern.entryDirectory[mcp.id.key], 'node_modules', SIBLING_SCOPE);
  const stray = path.join(scope, 'pi-ai');
  fs.symlinkSync(path.join(modern.entryDirectory[`${SIBLING_SCOPE}/pi-ai@${modern.version}`], 'node_modules', SIBLING_SCOPE, 'pi-ai'), stray);
  return () => fs.rmSync(stray);
});

withMutation(modern, 'hoisted package scope', 'hoisted-shadow', () => addPath(
  path.join(modern.prefix, 'node_modules', '.pnpm', 'node_modules', 'yaml'), target => {
    fs.mkdirSync(target, {recursive: true});
    fs.writeFileSync(path.join(target, 'package.json'), '{"name":"yaml","version":"2.9.0"}\n');
  }));

withMutation(modern, 'unreachable installed payload', 'unreachable-payload', () => addPath(
  path.join(modern.prefix, 'node_modules', '.pnpm', 'stray@1.0.0', 'node_modules', 'stray'), target => {
    fs.mkdirSync(target, {recursive: true});
    fs.writeFileSync(path.join(target, 'package.json'), '{"name":"stray","version":"1.0.0"}\n');
  }));

withMutation(legacy, 'compatible optional payload missing', 'missing-dependency', () => editGraph(legacy, graph => {
  for (const entry of graph.packages) {
    if (entry.id.name === '@esbuild/linux-arm64') entry.platform = {os: [], cpu: [], libc: []};
  }
}));

// pnpm's `import` records optionality on the snapshot and not on the package
// record, so the same omission must still be authorized when only the snapshot
// carries the flag -- that is the real shape of an imported 0.87.0 graph.
withPositive(legacy, 'import-style optionality (snapshot flag only)', () => editGraph(legacy, graph => {
  for (const entry of graph.packages) {
    if (entry.id.name === '@esbuild/linux-arm64') entry.optional = false;
  }
}));

// Conversely, locked platform selectors never excuse a *required* payload: with
// no optional target, a platform-excluded dependency is a hard failure.
withMutation(legacy, 'platform-excluded required target', 'missing-dependency', () => editGraph(legacy, graph => {
  for (const entry of graph.packages) {
    if (entry.id.name === '@esbuild/linux-arm64') entry.optional = false;
  }
  for (const entry of graph.snapshots) {
    if (entry.id.packageId.name === '@esbuild/linux-arm64') entry.optional = false;
  }
}));

// --- generated bin directory ------------------------------------------------

function binDir(meta, name) {
  return path.join(ownerDir(meta, name), 'node_modules', '.bin');
}

withMutation(modern, 'bin shell shim', 'kind-mismatch', () => {
  const link = path.join(binDir(modern, `${SIBLING_SCOPE}/pi-ai`), 'anthropic-ai-sdk');
  const text = fs.readlinkSync(link);
  fs.rmSync(link);
  fs.writeFileSync(link, `#!/bin/sh\nexec node "$(dirname "$0")/${text}"\n`);
  fs.chmodSync(link, 0o755);
  return () => {
    fs.rmSync(link);
    fs.symlinkSync(text, link);
  };
});

withMutation(modern, 'bin non-canonical but equivalent text', 'link-mismatch', () => {
  const link = path.join(binDir(modern, `${SIBLING_SCOPE}/pi-agent-core`), 'pi-ai');
  const target = path.resolve(path.dirname(link), fs.readlinkSync(link));
  return replaceLink(link, target);
});

withMutation(modern, 'bin retarget', 'link-mismatch', () => {
  const link = path.join(binDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'marked');
  return replaceLink(link, '../index.js');
});

withMutation(modern, 'extra bin entry', 'unexpected-path', () =>
  addPath(path.join(binDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'extra'), target => {
    fs.symlinkSync('../index.js', target);
  }));

withMutation(modern, 'missing bin entry', 'missing-generated', () =>
  removePath(path.join(binDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'marked')));

withMutation(modern, 'missing bin directory', 'missing-generated', () =>
  removePath(binDir(modern, `${SIBLING_SCOPE}/pi-tui`)));

withMutation(legacy, 'archive owns a generated bin path', 'archive-owns-generated-path', () =>
  addPath(path.join(legacy.parent, SIBLING_SCOPE, 'chord', legacy.version, 'node_modules', '.bin', 'esbuild'), target => {
    fs.mkdirSync(path.dirname(target), {recursive: true});
    fs.writeFileSync(target, '#!/bin/sh\n');
  }));

// --- modes: the checker runs without a repair pass ---------------------------

withMutation(modern, 'own bin target 0644', 'mode-mismatch', () =>
  chmodPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-ai`), 'dist', 'cli.js'), 0o644));

withMutation(modern, 'own bin target 0700', 'mode-mismatch', () =>
  chmodPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-ai`), 'dist', 'cli.js'), 0o700));

withMutation(modern, 'provider bin target 0644', 'bin-target-mode', () => {
  const graph = graphOf(modern);
  const tui = graph.snapshots.find(entry => entry.id.packageId.name === `${SIBLING_SCOPE}/pi-tui`);
  const marked = graph.snapshots.find(entry => entry.id.packageId.name === 'marked');
  assert.ok(tui.edges.some(edge => edge.name === 'marked'));
  return chmodPath(path.join(modern.entryDirectory[marked.id.key], 'node_modules', 'marked', 'bin', 'marked.js'), 0o644);
});

withMutation(modern, 'unrelated file made executable', 'mode-mismatch', () =>
  chmodPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'dist', 'index.js'), 0o755));

withMutation(modern, 'denied ancestor directory', 'mode-mismatch', () =>
  chmodPath(path.join(ownerDir(modern, `${SIBLING_SCOPE}/pi-tui`), 'dist'), 0o700));

// --- provider evidence ------------------------------------------------------

function evidenceOf(meta, name) {
  const version = meta.versions[name] ?? '0.129.0';
  const directory = path.join(meta.parent, 'evidence', ...name.split('/'), version);
  return path.join(directory, fs.readdirSync(directory)[0]);
}

withMutation(modern, 'missing provider evidence', 'missing-evidence', () =>
  removePath(evidenceOf(modern, 'marked')));

withMutation(modern, 'substituted provider evidence', 'evidence-substitution', () => {
  const tarball = evidenceOf(modern, 'marked');
  const restore = fileSnapshot(tarball);
  fs.writeFileSync(tarball, Buffer.concat([fs.readFileSync(tarball), Buffer.from('tampered')]));
  return restore;
});

// The coordinated case: an installed provider manifest and its generated link are
// retargeted together and stay internally consistent. They still fail, because
// the expectation comes from the authenticated archive.
withMutation(modern, 'coordinated provider manifest and link retarget', 'installed-bin-disagreement', () => {
  const graph = graphOf(modern);
  const sdk = graph.snapshots.find(entry => entry.id.packageId.name === '@anthropic-ai/sdk');
  const sdkDir = path.join(modern.entryDirectory[sdk.id.key], 'node_modules', '@anthropic-ai', 'sdk');
  const restoreManifest = fileSnapshot(path.join(sdkDir, 'package.json'));
  const manifest = JSON.parse(fs.readFileSync(path.join(sdkDir, 'package.json'), 'utf8'));
  manifest.bin = {'anthropic-ai-sdk': 'bin/other.js'};
  fs.writeFileSync(path.join(sdkDir, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
  fs.writeFileSync(path.join(sdkDir, 'bin', 'other.js'), '#!/usr/bin/env node\n');
  fs.chmodSync(path.join(sdkDir, 'bin', 'other.js'), 0o755);
  const link = path.join(binDir(modern, `${SIBLING_SCOPE}/pi-ai`), 'anthropic-ai-sdk');
  const restoreLink = replaceLink(link, '../@anthropic-ai/sdk/bin/other.js');
  return () => {
    restoreLink();
    fs.rmSync(path.join(sdkDir, 'bin', 'other.js'), {force: true});
    restoreManifest();
  };
});

// --- operands and the container --------------------------------------------

{
  const result = run(modern, {sibling: 'PI-ai'});
  assert.equal(result.status, 1, 'a malformed sibling operand is a hard rejection');
  assert.ok(result.stderr.startsWith('check-installed-bytes.mjs: bad-sibling: '), result.stderr);
}
{
  const result = spawnSync(process.execPath, [checker, modern.prefix], {encoding: 'utf8'});
  assert.equal(result.status, 2, 'missing operands are a usage error');
  assert.ok(result.stderr.startsWith('usage: check-installed-bytes.mjs '));
}
{
  const result = run(modern, {sibling: 'pi-extra'});
  assert.equal(result.status, 1);
  assert.ok(result.stderr.startsWith('check-installed-bytes.mjs: unexpected-sibling: '), result.stderr);
}
withMutation(modern, 'wrong extraction operand', 'extraction-layout', () => () => {}, {
  sibling: 'pi-ai',
  extraction: path.join(modern.parent, SIBLING_SCOPE, 'pi-tui', versionOf(modern, 'pi-tui')),
});

// The extraction operand check above mutates nothing, so its "repair" is a no-op
// and the positive rerun is the same tree.
{
  const result = run(modern, {
    sibling: 'pi-ai',
    extraction: path.join(modern.parent, SIBLING_SCOPE, 'pi-ai', versionOf(modern, 'pi-ai')),
  });
  assert.equal(result.status, 0, `the canonical extraction operand passes: ${result.stderr}`);
}

{
  const moved = path.join(modern.parent, SIBLING_SCOPE, 'pi-mcp');
  const parked = `${moved}-parked`;
  fs.renameSync(moved, parked);
  try {
    expectCode(modern, 'missing-extraction', 'missing family extraction');
  } finally {
    fs.renameSync(parked, moved);
  }
  expectPass(modern, 'missing family extraction (repaired)');
}

{
  const contract = path.join(modern.contract, 'recipe-contract');
  const restore = chmodPath(contract, 0o644);
  try {
    expectCode(modern, 'missing-contract', 'non-executable contract helper');
  } finally {
    restore();
  }
  expectPass(modern, 'non-executable contract helper (repaired)');
}

{
  const contract = path.join(modern.contract, 'recipe-contract');
  const restore = editFile(contract, '#!/bin/sh\nexit 3\n');
  try {
    expectCode(modern, 'graph-run', 'failing graph helper');
  } finally {
    restore();
  }
  expectPass(modern, 'failing graph helper (repaired)');
}

for (const [value, code] of [[undefined, 'missing-target-platform'], ['linux/s390x', 'unsupported-target-platform']]) {
  const result = run(modern, {env: {AGENT_VM_PI_TARGET_PLATFORM: value}});
  assert.equal(result.status, 1, `${value}: expected a hard failure`);
  assert.ok(result.stderr.startsWith(`check-installed-bytes.mjs: ${code}: `),
    `${value}: expected ${code}, got ${result.stderr.trim()}`);
}

// The platform tuple is part of the omission proof: the same tree is green on both
// native targets the images ship, and no host value is consulted.
for (const platform of ['linux/amd64', 'linux/arm64']) {
  const result = run(legacy, {env: {AGENT_VM_PI_TARGET_PLATFORM: platform}});
  assert.equal(result.status, 0, `${platform}: ${result.stderr}`);
}

// The evidence path is the lock's identity and SRI, and the bytes there hash to
// that SRI: the checker's path derivation is the same one the installer must use.
{
  const graph = graphOf(modern);
  const sdk = graph.packages.find(entry => entry.id.name === '@anthropic-ai/sdk');
  const hex = Buffer.from(sdk.integrity.split('-')[1], 'base64').toString('hex');
  const tarball = path.join(modern.parent, 'evidence', '@anthropic-ai', 'sdk', sdk.id.version, `${hex}.tgz`);
  assert.ok(fs.existsSync(tarball), `the lock-indexed evidence path must exist: ${tarball}`);
  assert.equal(`sha512-${crypto.createHash('sha512').update(fs.readFileSync(tarball)).digest('base64')}`, sdk.integrity);
}

// --- archive preflight ------------------------------------------------------
//
// The extractor owns the boundary between authentic bytes and a safe tree, so its
// refusals are their own table: each hostile archive must stop before the first
// file is written, and a benign archive with a directory, an executable file and a
// contained symlink must extract with those kinds preserved.
const archiveHelper = fileURLToPath(new URL('../../images/tools/pi/archive.py', import.meta.url));

function archiveCase(args) {
  return spawnSync('python3', [archiveHelper, ...args], {encoding: 'utf8', timeout: 120000});
}

const hostileCases = {
  traversal: 'unsafe-path',
  absolute: 'unsafe-path',
  backslash: 'unsafe-path',
  'empty-segment': 'unsafe-path',
  duplicate: 'duplicate-member',
  special: 'special-member',
  hardlink: 'special-member',
  'link-escape': 'link-escape',
  'link-absolute': 'link-escape',
  'link-composed': 'link-escape',
  'link-cycle': 'link-cycle',
  'link-self': 'link-cycle',
  'ancestor-file': 'ancestor-overwrite',
  'ancestor-link': 'ancestor-overwrite',
  // Case and Unicode normalisation decide how a filesystem names entries, not how
  // the archive spells them: on APFS `d/X` and `d/x` are one entry, so `e/Y`'s
  // target `../d/x/..` walks through a link to the package root and one level above
  // it. Both vectors wrote `PWNED` outside the extraction root with status 0 before
  // these refusals existed. `link-folded-target` is the same escape with no
  // member-name collision at all, so only a folded resolution can see it.
  'case-collision': 'name-collision',
  'normalisation-collision': 'name-collision',
  'link-folded-target': 'link-escape',
  privilege: 'privilege-mode',
  'unexpected-root': 'unexpected-root',
  deep: 'unsafe-path',
  'long-name': 'unsafe-path',
};
for (const [name, code] of Object.entries(hostileCases)) {
  const tarball = path.join(work, `hostile-${name}.tgz`);
  const built = spawnSync('python3', [builder, 'hostile', name, tarball], {encoding: 'utf8', timeout: 60000});
  assert.equal(built.status, 0, built.stderr);
  const destination = path.join(work, `hostile-${name}-out`);
  const result = archiveCase(['extract', tarball, destination]);
  assert.equal(result.status, 1, `${name}: expected a refusal, got status ${result.status}`);
  assert.ok(result.stderr.startsWith(`archive.py: ${code}: `),
    `${name}: expected ${code}, got ${result.stderr.trim()}`);
  assert.ok(!fs.existsSync(destination) || fs.readdirSync(destination).length === 0,
    `${name}: a refused archive must not leave extracted bytes`);
}

// A bin command the archive reaches through a link, or not at all, must be a
// refusal: hashing through a link let the archive choose the bytes the checker
// compared with the installed tree.
const hostileEvidenceCases = {
  'bin-link-ancestor': 'bin-target',
  'bin-link-final': 'bin-target',
  'bin-missing': 'bin-target',
};
for (const [name, code] of Object.entries(hostileEvidenceCases)) {
  const tarball = path.join(work, `evidence-${name}.tgz`);
  const built = spawnSync('python3', [builder, 'hostile-evidence', name, tarball], {encoding: 'utf8', timeout: 60000});
  assert.equal(built.status, 0, built.stderr);
  const integrity = `sha512-${crypto.createHash('sha512').update(fs.readFileSync(tarball)).digest('base64')}`;
  const destination = path.join(work, `evidence-${name}-out`);
  const result = archiveCase(['evidence', tarball, integrity, destination]);
  assert.equal(result.status, 1, `${name}: expected a refusal, got status ${result.status}`);
  assert.ok(result.stderr.startsWith(`archive.py: ${code}: `),
    `${name}: expected ${code}, got ${result.stderr.trim()}`);
}

// Headers are read before members: the `package/` root entry and an extended
// header reach tarfile with nothing yet visible, so they carry their own budgets.
// Root entries used to be skipped before every check, which let repeated roots
// stand in for the member count and let a root file or a directory with declared
// data keep 8 MiB out of the totals.
const budgetHelper = fileURLToPath(new URL('fixtures/pi-archive-budget.py', import.meta.url));

function budgetCase(name, tarball) {
  return spawnSync('python3', [budgetHelper, 'run', name, tarball], {encoding: 'utf8', timeout: 120000});
}

const rootCases = {
  'root-duplicate': 'duplicate-member',
  'root-file': 'unexpected-root',
  'root-size': 'unexpected-root',
};
for (const [name, code] of Object.entries(rootCases)) {
  const tarball = path.join(work, `${name}.tgz`);
  const built = spawnSync('python3', [builder, 'hostile-budget', name, tarball], {encoding: 'utf8', timeout: 120000});
  assert.equal(built.status, 0, built.stderr);
  const result = archiveCase(['extract', tarball, path.join(work, `${name}-out`)]);
  assert.equal(result.status, 1, `${name}: expected a refusal, got status ${result.status}`);
  assert.ok(result.stderr.startsWith(`archive.py: ${code}: `),
    `${name}: expected ${code}, got ${result.stderr.trim()}`);
}

// The production budgets are sized for a real npm tarball, so the scaled runs
// are what can prove a header counts: each of these is accepted under the
// module's own limits and refused once the budget is small enough to see it.
for (const [name, code] of Object.entries({'root-count': 'member-count', 'root-size': 'total-size'})) {
  const tarball = path.join(work, `${name}-scaled.tgz`);
  const built = spawnSync('python3', [builder, 'hostile-budget', name, tarball], {encoding: 'utf8', timeout: 120000});
  assert.equal(built.status, 0, built.stderr);
  const result = budgetCase(name, tarball);
  assert.equal(result.status, 0, `${name}: ${result.stderr}`);
  const observed = JSON.parse(result.stdout);
  assert.equal(observed.code, code, `${name}: expected ${code}, got ${JSON.stringify(observed)}`);
  assert.ok(observed.peak < 4 * 1024 * 1024, `${name}: allocated ${observed.peak} bytes`);
}

// A valid 32 MiB PAX record is a valid archive under the production budget, so
// the bound is proved by the scaled run plus the allocation it must precede: a
// refusal that arrived after tarfile had the buffer would show its size as peak.
{
  const tarball = path.join(work, 'pax-bomb.tgz');
  const built = spawnSync('python3', [builder, 'hostile-budget', 'pax-bomb', tarball], {encoding: 'utf8', timeout: 120000});
  assert.equal(built.status, 0, built.stderr);
  const result = budgetCase('pax-bomb', tarball);
  assert.equal(result.status, 0, result.stderr);
  const observed = JSON.parse(result.stdout);
  assert.ok(['read-size', 'decompressed-size'].includes(observed.code),
    `pax-bomb: expected a decompressed-stream refusal, got ${JSON.stringify(observed)}`);
  assert.ok(observed.peak < 4 * 1024 * 1024,
    `pax-bomb: the 32 MiB declared buffer reached tarfile before the refusal (${observed.peak} bytes)`);
}

// Extended headers are the other half of the same boundary: they are read between
// members, in `_safe_read` chunks of their own, so neither the per-read cap nor a
// member's declared size ever sees them. These run at the *production* budgets (no
// scaling), which is what makes them cheap enough to build: a 64 MiB PAX value gzips
// to 65 KiB, and 5000 chained `x` headers are small.
//
// `header-spread` is the archive-wide total: 1,000 zero-byte members each carrying a
// 1,040,000-byte `comment`, which never fills a window. The archive is 1.1 MB on the
// wire and was accepted with a 1,044,612,040-byte traced peak and 1.16 GB resident,
// because `tarfile` keeps every member's `pax_headers`. Its peak is the 8 MiB budget
// plus `tarfile`'s parse of the one record that would have crossed it.
{
  const headerCases = {
    'header-comment': {code: 'header-bytes', peak: 4 * 1024 * 1024},
    'header-chain': {code: 'header-chain', peak: 4 * 1024 * 1024},
    'header-spread': {code: 'header-bytes', peak: 12 * 1024 * 1024},
  };
  for (const [name, {code, peak}] of Object.entries(headerCases)) {
    const tarball = path.join(work, `${name}.tgz`);
    const built = spawnSync('python3', [builder, 'hostile-budget', name, tarball],
      {encoding: 'utf8', timeout: 120000});
    assert.equal(built.status, 0, built.stderr);
    const result = budgetCase('production', tarball);
    assert.equal(result.status, 0, result.stderr);
    const observed = JSON.parse(result.stdout);
    assert.equal(observed.code, code, `${name}: expected ${code}, got ${JSON.stringify(observed)}`);
    // The 64 MiB record arrived as 1 MiB chunks and was fully allocated before the
    // between-members budget existed: 366 MiB peak for this archive, 3.4 GB for the
    // 600 MiB one the review measured.
    assert.ok(observed.peak < peak,
      `${name}: allocated ${observed.peak} bytes before the refusal`);
  }
}

// The write walk is a refusal of its own: every parent component is opened with
// `O_NOFOLLOW` relative to the directory above it, so even a member whose parent
// component is a link cannot be written through -- on a case-sensitive filesystem,
// and for a spelling the preflight never saw.
{
  const result = spawnSync('python3', [budgetHelper, 'walk'], {encoding: 'utf8', timeout: 120000});
  assert.equal(result.status, 0, result.stderr);
  const observed = JSON.parse(result.stdout);
  for (const label of ['directory-through-link', 'file-through-link', 'outside-link']) {
    assert.equal(observed[label], 'link-traversal', `${label}: ${JSON.stringify(observed)}`);
  }
  assert.deepEqual(observed.escaped, [], 'a write reached a directory outside the root');
}

{
  const tarball = path.join(work, 'benign.tgz');
  const built = spawnSync('python3', [builder, 'benign', tarball], {encoding: 'utf8', timeout: 60000});
  assert.equal(built.status, 0, built.stderr);
  const destination = path.join(work, 'benign-out');
  const result = archiveCase(['extract', tarball, destination]);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(fs.statSync(path.join(destination, 'bin', 'tool')).mode & 0o777, 0o755,
    'the archive executable bit must survive extraction');
  assert.equal(fs.statSync(path.join(destination, 'lib', 'index.js')).mode & 0o777, 0o644);
  assert.ok(fs.lstatSync(path.join(destination, 'lib', 'alias')).isSymbolicLink());
  assert.equal(fs.readlinkSync(path.join(destination, 'lib', 'alias')), 'index.js');
  assert.ok(!fs.existsSync(path.join(destination, 'package')), 'the package/ root must be stripped');
}

fs.rmSync(work, {recursive: true, force: true});
process.stdout.write(`pi-installed-bytes: fixture, reviewed tables, ${Object.keys(hostileCases).length} archive refusals, ${Object.keys(hostileEvidenceCases).length} bin-target refusals, ${Object.keys(rootCases).length} root-header refusals, scaled header budgets, the production per-window and archive-wide header budgets, the write walk and every mutant diagnostic verified\n`);
