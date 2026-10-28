#!/usr/bin/env node
// Hermetic vectors for the authenticated selected-shrinkwrap converter.
//
// The inputs are the Phase 0 gate's own authenticated evidence: the 0.87.0
// archive manifest, its published npm-shrinkwrap.json, the six archive receipts
// the gate authenticated, the npm-shaped lock that gate synthesized, and the pnpm
// 11.11.0 import result for that lock. Nothing here uses the network, a package
// manager or a committed 0.87.0 pnpm workspace.
//
// The expected numbers are the gate's reviewed counts, not this module's output:
// a converter that drifts in identity, occurrence, SRI, edge, optional or peer
// terms fails here before any consumer sees it.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

import {
  ConversionError, synthesizeProjectLock, validateShrinkwrapDelta,
} from '../../images/tools/pi/import-shrinkwrap.mjs';

const fixtures = fileURLToPath(new URL('fixtures/pi-shrinkwrap-0.87.0/', import.meta.url));
const module_ = fileURLToPath(new URL('../../images/tools/pi/import-shrinkwrap.mjs', import.meta.url));
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pi-shrinkwrap-'));
let counter = 0;

const read = name => JSON.parse(fs.readFileSync(path.join(fixtures, name), 'utf8'));
const clone = value => JSON.parse(JSON.stringify(value));
const receipts = read('resolutions.json');
const rootReceipt = receipts.find(receipt => receipt.name === '@earendil-works/pi-coding-agent');
const siblingReceipts = receipts.filter(receipt => receipt !== rootReceipt);
const manifest = read('archive-manifest.json');
const shrinkwrap = read('authenticated-shrinkwrap.json');
const expected = read('synthesized-package-lock.json');
const importedRaw = read('result/parse-yaml.stdout');
const curatedMutants = read('mutants.json');
const expectedDelta = read('expected-delta.json');

// The converter's staged project name is its own; it is not fidelity data, so it
// is the one field both sides drop before the deep comparison.
function withoutProjectName(lock) {
  const copy = clone(lock);
  delete copy.name;
  delete copy.supplements;
  if (copy.packages?.['']) delete copy.packages[''].name;
  return copy;
}

function synthesize(overrides = {}) {
  return synthesizeProjectLock({
    archiveManifest: overrides.manifest ?? manifest,
    shrinkwrap: overrides.shrinkwrap ?? shrinkwrap,
    rootArchiveResolution: overrides.root ?? rootReceipt,
    siblingResolutions: overrides.siblings ?? siblingReceipts,
  });
}

function rejects(overrides, code, label) {
  try {
    synthesize(overrides);
  } catch (error) {
    assert.ok(error instanceof ConversionError, `${label}: expected a ConversionError, got ${error}`);
    assert.equal(error.code, code, `${label}: ${error.message}`);
    return;
  }
  assert.fail(`${label}: expected ${code}`);
}

function mutateShrinkwrap(change) {
  const copy = clone(shrinkwrap);
  change(copy);
  return copy;
}

// --- synthesis fidelity ----------------------------------------------------

const staged = synthesize();
assert.deepEqual(withoutProjectName(staged), withoutProjectName(expected),
  'the converter must reproduce the Phase 0 synthesized npm-shaped lock exactly');
assert.deepEqual(staged.supplements.map(entry => entry.name).sort(),
  ['@earendil-works/chord', '@earendil-works/pi-agent-core', '@earendil-works/pi-ai',
    '@earendil-works/pi-telemetry', '@earendil-works/pi-tui'],
  'only the five legacy siblings may gain an SRI');
for (const supplement of staged.supplements) {
  const receipt = siblingReceipts.find(entry => entry.name === supplement.name);
  assert.equal(supplement.integrity, receipt.integrity, `${supplement.name} SRI must be the verified receipt`);
  assert.equal(supplement.version, '0.87.0', `${supplement.name} must be at the selected version`);
}
// Existing archive SRI is preserved byte for byte, including the root.
const rootRecord = staged.packages['node_modules/@earendil-works/pi-coding-agent'];
assert.equal(rootRecord.integrity, rootReceipt.integrity);
assert.equal(rootRecord.resolved, rootReceipt.url);
for (const [location, record] of Object.entries(shrinkwrap.packages)) {
  if (location === '') continue;
  const stagedRecord = staged.packages[`node_modules/@earendil-works/pi-coding-agent/${location}`];
  if (record.integrity !== undefined) {
    assert.equal(stagedRecord.integrity, record.integrity, `${location} SRI must be untouched`);
  }
  assert.equal(stagedRecord.resolved, record.resolved, `${location} tarball must be untouched`);
}
assert.equal(Object.keys(staged.packages).length, 145, 'importer + root archive + 143 occurrences');

// --- synthesis rejections --------------------------------------------------

rejects({siblings: []}, 'missing-integrity', 'a shrinkwrapped selection without supplements');
rejects({root: {...rootReceipt, name: '@earendil-works/chord'}}, 'root-identity-mismatch', 'root name');
rejects({root: {...rootReceipt, version: '1.4.0'}}, 'root-version-mismatch', 'root version');
rejects({root: {...rootReceipt, resolved: 'https://example.invalid/pi.tgz'}}, 'unsafe-resolver', 'other registry');
rejects({root: {...rootReceipt, resolved: rootReceipt.url.replace('https://', 'http://')}}, 'unsafe-resolver', 'plain http');
rejects({root: {...rootReceipt, integrity: 'sha1-AAAA'}}, 'bad-integrity', 'weak digest');
rejects({siblings: [...siblingReceipts, siblingReceipts[0]]}, 'supplement-duplicate', 'duplicate supplement');
rejects({siblings: [...siblingReceipts.slice(0, 4)]}, 'missing-integrity', 'four supplements');
rejects({siblings: [...siblingReceipts, {
  name: '@earendil-works/pi-codemode', version: '0.87.0', url: 'https://registry.npmjs.org/@earendil-works/pi-codemode/-/pi-codemode-0.87.0.tgz', integrity: rootReceipt.integrity,
}]}, 'supplement-not-legacy-five', 'sixth sibling');
rejects({siblings: siblingReceipts.map(receipt => ({...receipt, version: '0.86.0'}))},
  'supplement-version', 'supplement at another version');
rejects({siblings: siblingReceipts.slice(1)}, 'missing-integrity', 'a sibling without its receipt');

const extraRecord = clone(shrinkwrap);
extraRecord.packages['node_modules/is-number'] = {version: '7.0.0'};
rejects({shrinkwrap: extraRecord}, 'missing-integrity', 'a new hashless record');

const traversal = clone(shrinkwrap);
traversal.packages['node_modules/../../escape'] = {version: '1.0.0', integrity: rootReceipt.integrity};
rejects({shrinkwrap: traversal}, 'unsafe-path', 'path traversal');

const absolute = clone(shrinkwrap);
absolute.packages['/node_modules/escape'] = {version: '1.0.0', integrity: rootReceipt.integrity};
rejects({shrinkwrap: absolute}, 'unsafe-path', 'absolute path');

const doubled = clone(shrinkwrap);
doubled.packages['node_modules//escape'] = {version: '1.0.0', integrity: rootReceipt.integrity};
rejects({shrinkwrap: doubled}, 'unsafe-path', 'empty segment');

const linked = clone(shrinkwrap);
linked.packages['node_modules/@anthropic-ai/sdk'].link = true;
rejects({shrinkwrap: linked}, 'unsafe-record-mode', 'link record');

const devOnly = clone(shrinkwrap);
devOnly.packages['node_modules/@anthropic-ai/sdk'].dev = true;
rejects({shrinkwrap: devOnly}, 'dev-only-record', 'dev-only record');

const gitResolver = clone(shrinkwrap);
gitResolver.packages['node_modules/@anthropic-ai/sdk'].resolved = 'git+https://example.invalid/sdk.git';
rejects({shrinkwrap: gitResolver}, 'unsafe-resolver', 'git resolver');

const wrongTarball = clone(shrinkwrap);
wrongTarball.packages['node_modules/@anthropic-ai/sdk'].resolved =
  'https://registry.npmjs.org/@anthropic-ai/sdk/-/sdk-0.124.1.tgz';
rejects({shrinkwrap: wrongTarball}, 'unsafe-resolver', 'tarball for another version');

const alias = clone(shrinkwrap);
alias.packages['node_modules/@anthropic-ai/sdk'].dependencies = {zod: 'npm:zod@3.0.0'};
rejects({shrinkwrap: alias}, 'unsafe-specifier', 'alias dependency');

const localPath = clone(shrinkwrap);
localPath.packages['node_modules/@anthropic-ai/sdk'].dependencies = {zod: 'file:../zod'};
rejects({shrinkwrap: localPath}, 'unsafe-specifier', 'file dependency');

const oldLock = clone(shrinkwrap);
oldLock.lockfileVersion = 2;
rejects({shrinkwrap: oldLock}, 'unsupported-lockfile-version', 'lockfileVersion 2');

const disagreed = clone(shrinkwrap);
disagreed.packages[''].dependencies = {...disagreed.packages[''].dependencies, extra: '1.0.0'};
rejects({shrinkwrap: disagreed}, 'root-dependency-disagreement', 'root manifest disagreement');

const prefixed = clone(shrinkwrap);
prefixed.packages['node_modules/@earendil-works/pi-coding-agent/node_modules/is-number'] =
  {version: '7.0.0', integrity: rootReceipt.integrity};
rejects({shrinkwrap: prefixed}, 'prefixed-archive', 'already-prefixed record');

const discrepant = clone(shrinkwrap);
discrepant.packages['node_modules/@earendil-works/pi-ai'].integrity =
  'sha512-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==';
rejects({shrinkwrap: discrepant}, 'sri-discrepancy', 'SRI that disagrees with the receipt');

const mismatchedResolver = clone(shrinkwrap);
mismatchedResolver.packages['node_modules/@earendil-works/chord'].resolved =
  'https://registry.npmjs.org/@earendil-works/chord/-/chord-0.87.1.tgz';
rejects({shrinkwrap: mismatchedResolver}, 'unsafe-resolver', 'supplement resolver for another version');

rejects({manifest: {...manifest, version: '0.87.1'}}, 'root-version-mismatch', 'manifest version');
assert.throws(() => synthesizeProjectLock({}), error => error instanceof ConversionError && error.code === 'not-an-object',
  'missing inputs are refused');

// --- delta validation against the real import ------------------------------

// pnpm's own v9 lock JSON is not the typed graph: this adapter is the test's, and
// it only renames the fields the Go projection carries. Everything it can get
// wrong (a wrong edge target, a wrong identity) shows up as a delta below.
function identityOfKey(key) {
  return key.split('(', 1)[0];
}

function parseKey(key) {
  const base = identityOfKey(key);
  const at = base.lastIndexOf('@');
  return {name: base.slice(0, at), version: base.slice(at + 1)};
}

function contextsOf(key) {
  const out = [];
  let depth = 0;
  let start = null;
  for (let index = 0; index < key.length; index += 1) {
    if (key[index] === '(') {
      if (depth === 0) start = index + 1;
      depth += 1;
    } else if (key[index] === ')') {
      depth -= 1;
      if (depth === 0) out.push(key.slice(start, index));
    }
  }
  return out;
}

function toGraph(raw) {
  const packages = Object.entries(raw.packages).map(([key, record]) => ({
    id: parseKey(key),
    integrity: record.resolution?.integrity,
    tarball: record.resolution?.tarball,
    peers: record.peerDependencies ?? {},
    optionalPeers: Object.entries(record.peerDependenciesMeta ?? {})
      .filter(([, meta]) => meta?.optional === true).map(([name]) => name),
    hasBin: record.hasBin === true,
    platform: {os: [], cpu: [], libc: []},
    optional: record.optional === true,
  }));
  const snapshots = Object.entries(raw.snapshots).map(([key, record]) => ({
    id: {
      packageId: parseKey(key),
      peerContext: contextsOf(key).map(context => ({packageId: parseKey(context), peerContext: [], key: context})),
      key,
    },
    edges: Object.entries({...record.dependencies, ...record.optionalDependencies}).map(([name, reference]) => {
      const text = typeof reference === 'object' ? reference.version : reference;
      return {name, target: `${name}@${text}` in raw.snapshots ? `${name}@${text}` : text,
        optional: name in (record.optionalDependencies ?? {})};
    }),
    optional: record.optional === true,
  }));
  const importer = Object.entries(raw.importers['.'].dependencies).map(([name, reference]) => {
    const text = typeof reference === 'object' ? reference.version : reference;
    return {name, target: `${name}@${text}`, optional: false};
  });
  return {selection: {kind: 'pi', version: '0.87.0'}, packages, snapshots, importer};
}

const allowedSiblings = siblingReceipts.map(receipt => ({
  name: receipt.name, version: receipt.version, integrity: receipt.integrity,
}));

function delta(imported, overrides = {}) {
  return validateShrinkwrapDelta({
    shrinkwrap: overrides.shrinkwrap ?? staged,
    importedProject: imported,
    allowedMissingSiblingSris: overrides.allowed ?? allowedSiblings,
  });
}

const clean = delta(toGraph(importedRaw));
assert.equal(clean.accepted, true, JSON.stringify(clean.rejected.slice(0, 3)));
assert.deepEqual(clean.rejected, []);
const counts = expectedDelta.counts;
assert.equal(clean.counts.mappedPairs, counts.rootedIdentityGuarded.pairs);
assert.equal(clean.counts.sriPairs, counts.rootedIdentityGuarded.sriEqualPairs);
assert.equal(clean.counts.sriRawEqualPairs, counts.rootedIdentityGuarded.sriRawEqualPairs);
assert.equal(clean.counts.sriRawUnequalPairs, counts.rootedIdentityGuarded.sriRawUnequalPairs);
assert.equal(clean.counts.identityIntersection, counts.identityIntersection);
assert.equal(clean.counts.identitiesAdded, counts.newOnlyIdentities);
assert.equal(clean.counts.identitiesRemoved, counts.oldOnlyIdentities);
assert.equal(clean.counts.occurrenceMultiplicityCollapsed, counts.identityMultisetRemovedOccurrences);
assert.equal(clean.counts.optionalClassificationMismatches, counts.rootedIdentityGuarded.optionalClassificationDifferentPairs);
assert.equal(clean.counts.binPresenceMismatches, counts.rootedIdentityGuarded.binPresenceDifferentPairs);
assert.equal(clean.counts.peerComparisons, counts.rootedIdentityGuarded.declaredPeerComparisons);
assert.equal(clean.counts.peerDeclarationChanges, counts.rootedIdentityGuarded.peerDeclarationChanges);
assert.equal(clean.counts.effectivePeerChanges, counts.rootedIdentityGuarded.effectivePeerChanges);
assert.equal(clean.counts.explicitPeerExpansions, counts.rootedIdentityGuarded.explicitPeerExpansionEdges);
// The three reviewed representation deltas are reported, never allowlisted.
const reported = new Map(clean.reported.map(entry => [entry.code, entry]));
assert.deepEqual([...reported.keys()].sort(),
  ['explicit-peer-expansion', 'occurrence-multiplicity-collapse', 'peer-declaration-completion']);
assert.equal(reported.get('occurrence-multiplicity-collapse').detail.identity, 'agent-base@9.0.0');
assert.equal(reported.get('explicit-peer-expansion').detail.imported, 'ws@8.21.0');
assert.equal(reported.get('peer-declaration-completion').detail.peer, 'supports-color');
for (const entry of clean.reported) assert.equal(entry.requiresReview, true, `${entry.code} must stay review-only`);

function mutateImport(edits) {
  const raw = clone(importedRaw);
  for (const edit of edits) {
    let cursor = raw;
    for (const key of edit.path.slice(0, -1)) {
      if (cursor[key] === undefined) cursor[key] = {};
      cursor = cursor[key];
    }
    cursor[edit.path[edit.path.length - 1]] = edit.value;
  }
  return toGraph(raw);
}

const mutantCodes = {
  sri: 'sri-changed',
  optional: 'optional-classification-changed',
  edge: 'edge-target-changed',
  peer: 'effective-peer-changed',
  orphan: 'identity-added',
};
for (const mutant of curatedMutants) {
  const result = delta(mutateImport(mutant.edits));
  assert.equal(result.accepted, false, `${mutant.mutant} must be rejected`);
  const codes = new Set(result.rejected.map(entry => entry.code));
  assert.ok(codes.has(mutantCodes[mutant.mutant]),
    `${mutant.mutant} must report ${mutantCodes[mutant.mutant]}, saw ${[...codes].join(',')}`);
}

// A semantic peer-range rewrite is not a representation delta.
const peerRange = mutateImport([{
  path: ['packages', 'debug@4.4.3', 'peerDependencies', 'supports-color'], value: '^9.0.0',
}]);
assert.ok(delta(peerRange).rejected.some(entry => entry.code === 'peer-declaration-changed'));

// The supplement set is bound: an extra or mis-versioned receipt is refused.
const extraAllowed = delta(toGraph(importedRaw), {allowed: [...allowedSiblings, {
  name: '@earendil-works/pi-codemode', version: '0.87.0',
}]});
assert.ok(extraAllowed.rejected.some(entry => entry.code === 'supplement-not-legacy-five'));
const wrongVersion = delta(toGraph(importedRaw), {
  allowed: allowedSiblings.map(entry => ({...entry, version: '0.86.0'})),
});
assert.ok(wrongVersion.rejected.some(entry => entry.code === 'supplement-version'));

// An importer that claims two roots is not the staged project.
const twoRoots = toGraph(importedRaw);
twoRoots.importer.push({name: '@earendil-works/chord', target: 'not-a-snapshot', optional: false});
assert.ok(delta(twoRoots).rejected.some(entry => entry.code === 'importer-shape'));

// An expectation without the root archive record cannot be compared, so it is
// refused rather than silently skipping the root SRI.
const noRoot = clone(staged);
delete noRoot.packages['node_modules/@earendil-works/pi-coding-agent'];
assert.throws(() => delta(toGraph(importedRaw), {shrinkwrap: noRoot}), ConversionError);

// --- CLI -------------------------------------------------------------------

function write(name, value) {
  const file = path.join(work, `${counter++}-${name}`);
  fs.writeFileSync(file, JSON.stringify(value));
  return file;
}

function run(args) {
  return spawnSync(process.execPath, [module_, ...args], {encoding: 'utf8', timeout: 120000});
}

const synthesizedFile = path.join(work, 'synthesized.json');
const cli = run(['synthesize', '--manifest', write('manifest.json', manifest),
  '--shrinkwrap', write('shrinkwrap.json', shrinkwrap),
  '--root', write('root.json', rootReceipt),
  '--siblings', write('siblings.json', siblingReceipts)]);
assert.equal(cli.status, 0, cli.stderr);
fs.writeFileSync(synthesizedFile, cli.stdout);
assert.deepEqual(withoutProjectName(JSON.parse(cli.stdout)), withoutProjectName(expected));
// stdout is a lock `pnpm import` can consume; the completed-SRI receipts are
// build provenance and must not appear in it.
assert.equal(JSON.parse(cli.stdout).supplements, undefined);
assert.equal(JSON.parse(cli.stderr.replace('completed SRIs: ', '')).length, 5, cli.stderr);

const graphFile = write('graph.json', toGraph(importedRaw));
const accepted = run(['validate', '--expect', synthesizedFile, '--graph', graphFile,
  '--allowed-missing', write('allowed.json', allowedSiblings)]);
assert.equal(accepted.status, 0, accepted.stderr);
assert.equal(JSON.parse(accepted.stdout).accepted, true);

const badGraph = write('bad-graph.json', mutateImport(curatedMutants[0].edits));
const rejected = run(['validate', '--expect', synthesizedFile, '--graph', badGraph,
  '--allowed-missing', write('allowed2.json', allowedSiblings)]);
assert.equal(rejected.status, 1, 'a rejected delta must exit 1');
assert.equal(JSON.parse(rejected.stdout).accepted, false);
assert.equal(JSON.parse(rejected.stdout).rejected[0].code, 'sri-changed');

const usage = run(['synthesize']);
assert.equal(usage.status, 2);
const unknown = run(['wat']);
assert.equal(unknown.status, 2);

fs.rmSync(work, {recursive: true, force: true});
process.stdout.write(`pi-shrinkwrap: ${curatedMutants.length} curated mutants and ${'a'} reviewed delta verified\n`);
