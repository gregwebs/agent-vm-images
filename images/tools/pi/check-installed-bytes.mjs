#!/usr/bin/env node
// Build-only authenticated byte proof for the Pi sibling family, never shipped.
//
// SRI in the lock authenticates what pnpm downloaded; it says nothing about what
// is now on disk. This checker compares each reachable sibling's installed
// package root against its independently downloaded, SRI-authenticated archive,
// and normalises exactly one thing: the generated package-root
// `node_modules/.bin` entries and the dependency links, whose expectations are
// derived from the lock and from authenticated provider manifests. Installed
// links and installed manifests are only ever checked *against* an expectation.
//
// Operand layout is fixed so nothing has to be inferred from the environment:
// the installer keeps every authenticated extraction under one owned parent,
//     PARENT/<package-name>/<version>/          extraction root (EXTRACTED_PACKAGE)
//     PARENT/evidence/<package-name>/<version>/<sha512hex>.tgz
// and this checker rehashes each retained provider tarball against the locked SRI
// on every invocation. A receipt, a filename or a directory name is never
// authentication by itself.
import {spawnSync} from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';

import {
  GraphError, binTargetsOf, familyShape, hashFile, inventory, readGraph, readManifest, relative,
  resolveInstances, targetPlatform,
} from './installed-graph.mjs';

const SIBLING_ARGUMENT = /^[a-z0-9][a-z0-9-]*$/;
const ARCHIVE_HELPER = path.join(path.dirname(fileURLToPath(import.meta.url)), 'archive.py');
const ARCHIVE_TIMEOUT_MS = 180_000;
const BIN_DIRECTORY = 'node_modules/.bin';
const DEPENDENCY_DIRECTORY = 'node_modules';
// Only a locked, authenticated declared bin target is expected to be executable
// after installation: pnpm chmods those to 0755, while every other entry keeps
// the archive's own executable bit. This is a derived rule, not a relaxation.
const INSTALLED_BIN_MODE = 0o755;
const DIRECTORY_ACCESS = 0o555;

const fail = (code, detail) => { throw new GraphError(code, detail); };

function usage() {
  process.stderr.write('usage: check-installed-bytes.mjs PREFIX SIBLING EXTRACTED_PACKAGE\n'
    + '       check-installed-bytes.mjs inventory PREFIX\n');
  process.exit(2);
}

// The installer is the shell half of the same proof: it owns curl and the SRI
// check, so it must learn *which* authenticated archives to fetch from the lock
// rather than from the installed tree. `inventory` is that list and nothing
// else: the reviewed family, and the bin-providing dependencies whose evidence
// this checker will rehash. Both are derived from the lock alone.
function familyInventory(prefixOperand) {
  const contractDir = process.env.AGENT_VM_CONTRACT_DIR ?? '/tmp/recipe-contract';
  const platform = targetPlatform().spelling;
  const graph = readGraph({contractDir, prefix: prefixOperand});
  const family = familyShape(graph);
  const record = key => {
    const snapshot = graph.snapshots.get(key);
    const identity = snapshot.id.packageId;
    const archive = graph.packages.get(`${identity.name}@${identity.version}`);
    if (archive === undefined) fail('graph-json', `${key} has no archive record`);
    if (typeof archive.integrity !== 'string' || !archive.integrity.startsWith('sha512-')) {
      fail('lock-sri', `${identity.name}@${identity.version} has no sha512 SRI in the lock`);
    }
    if (typeof archive.tarball !== 'string' || !archive.tarball.startsWith('https://')) {
      fail('lock-tarball', `${identity.name}@${identity.version} has no https tarball`);
    }
    return {name: identity.name, version: identity.version,
            integrity: archive.integrity, tarball: archive.tarball};
  };
  const members = family.shape.map(base => record(family.keys.get(base)));
  const familyNames = new Set(members.map(member => member.name));
  const providers = new Map();
  for (const base of family.shape) {
    const snapshot = graph.snapshots.get(family.keys.get(base));
    for (const edge of snapshot.edges ?? []) {
      const target = graph.snapshots.get(edge.target);
      const identity = target.id.packageId;
      const archive = graph.packages.get(`${identity.name}@${identity.version}`);
      if (archive === undefined) fail('graph-json', `${edge.target} has no archive record`);
      // Family providers come from the family extractions, never from evidence.
      if (archive.hasBin !== true || familyNames.has(identity.name)) continue;
      providers.set(`${identity.name}@${identity.version}`, record(edge.target));
    }
  }
  return {platform, family: members, providers: [...providers.values()]};
}

function sha512Integrity(tarball) {
  const digest = crypto.createHash('sha512');
  digest.update(fs.readFileSync(tarball));
  return `sha512-${digest.digest('base64')}`;
}

function integrityHex(integrity) {
  const token = integrity.split(' ')[0];
  if (!token.startsWith('sha512-')) fail('lock-sri', `not a sha512 SRI: ${integrity}`);
  return Buffer.from(token.slice('sha512-'.length), 'base64').toString('hex');
}

function evidenceTarball(evidenceRoot, identity, integrity) {
  return path.join(evidenceRoot, ...identity.name.split('/'), identity.version, `${integrityHex(integrity)}.tgz`);
}

// One authenticated extraction per provider identity, reused across siblings.
class Providers {
  constructor({evidenceRoot, familyExtractions, scratch}) {
    this.evidenceRoot = evidenceRoot;
    this.familyExtractions = familyExtractions;
    this.scratch = scratch;
    this.cache = new Map();
  }

  get(identity, integrity) {
    const key = `${identity.name}@${identity.version}`;
    if (this.cache.has(key)) return this.cache.get(key);
    const family = this.familyExtractions.get(identity.name);
    if (family !== undefined) {
      if (family.version !== identity.version) {
        fail('evidence-identity',
          `${identity.name}@${identity.version} needs the ${family.version} extraction`);
      }
      const manifest = readManifest(family.root, `the ${key} archive`);
      if (manifest.name !== identity.name || manifest.version !== identity.version) {
        fail('evidence-identity', `${family.root} declares ${manifest.name}@${manifest.version}`);
      }
      const provider = {dir: family.root, manifest};
      this.cache.set(key, provider);
      return provider;
    }
    const tarball = evidenceTarball(this.evidenceRoot, identity, integrity);
    let info;
    try {
      info = fs.lstatSync(tarball);
    } catch {
      fail('missing-evidence', `${key} needs its authenticated tarball at ${tarball}`);
    }
    if (!info.isFile() || info.isSymbolicLink()) fail('missing-evidence', `${tarball} is not a regular file`);
    const actual = sha512Integrity(tarball);
    if (actual !== integrity) fail('evidence-substitution', `${tarball} hashes to ${actual}, the lock says ${integrity}`);
    const destination = path.join(this.scratch, key.replace(/[^A-Za-z0-9._-]/g, '_'));
    const result = spawnSync('python3', [ARCHIVE_HELPER, 'evidence', tarball, integrity, destination], {
      encoding: 'utf8', timeout: ARCHIVE_TIMEOUT_MS, maxBuffer: 8 * 1024 * 1024,
    });
    if (result.error !== undefined) fail('archive-helper', `${ARCHIVE_HELPER}: ${result.error.message}`);
    if (result.status !== 0) fail('archive-helper', `${tarball}: ${(result.stderr ?? '').trim()}`);
    let described;
    try {
      described = JSON.parse(result.stdout);
    } catch {
      fail('archive-helper', `${tarball}: evidence is not JSON`);
    }
    if (described.name !== identity.name || described.version !== identity.version) {
      fail('evidence-identity', `${tarball} declares ${described.name}@${described.version}, the lock says ${key}`);
    }
    const manifest = readManifest(destination, `the ${key} archive`);
    const provider = {dir: destination, manifest, described};
    this.cache.set(key, provider);
    return provider;
  }
}

function compareManifestBins(installed, authenticated, label) {
  const installedBins = binTargetsOf(installed, `${label} installed manifest`);
  const authenticatedBins = binTargetsOf(authenticated, `${label} authenticated manifest`);
  const names = new Set([...installedBins.keys(), ...authenticatedBins.keys()]);
  for (const name of names) {
    if (installedBins.get(name) !== authenticatedBins.get(name)) {
      fail('installed-bin-disagreement',
        `${label}: installed manifest declares ${name}=${JSON.stringify(installedBins.get(name))}, the authenticated archive says ${JSON.stringify(authenticatedBins.get(name))}`);
    }
  }
}

function checkFamilyMember({name, version, owner, extraction, providers, graph}) {
  const ownerArchive = inventory(extraction);
  const installed = inventory(owner.dir);
  const label = `${name}@${version}`;
  const expectedLinks = new Map();
  const expectedDirectories = new Set();

  const authenticatedOwner = readManifest(extraction, `the ${label} archive`);
  compareManifestBins(owner.manifest, authenticatedOwner, label);
  const ownerBins = binTargetsOf(authenticatedOwner, `${label} archive`);
  const ownBinTargets = new Set([...ownerBins.values()]);

  const binDirectory = path.join(owner.dir, ...BIN_DIRECTORY.split('/'));
  const declaredCommands = new Map();
  // The expectation is derived here and nowhere else: a command name and its
  // package-relative target come from an authenticated manifest, the link text is
  // the canonical relative path to the installed provider directory.
  const addCommand = (command, providerDir, binPath) => {
    const existing = declaredCommands.get(command);
    if (existing !== undefined && existing.providerDir !== providerDir) {
      fail('bin-collision', `${label} would get command ${command} from two providers`);
    }
    declaredCommands.set(command, {providerDir, binPath});
    const target = path.join(providerDir, ...binPath.split('/'));
    expectedLinks.set(`${BIN_DIRECTORY}/${command}`, relative(binDirectory, target));
  };

  // pnpm links the *children's* commands into an owning package root's `.bin`
  // (`linkBinsOfDependencies` in the pinned 11.11.0 restorer). A package does not
  // get a link to its own command there; its own target is still a declared bin
  // target for the installation-mode rule, and its parent links it.
  const dependencyNames = [];
  for (const edge of owner.edges) {
    const snapshot = graph.snapshots.get(edge.target);
    if (snapshot === undefined) fail('graph-json', `${owner.key} depends on unknown snapshot ${edge.target}`);
    const identity = snapshot.id.packageId;
    const archive = graph.packages.get(`${identity.name}@${identity.version}`);
    if (archive === undefined) fail('graph-json', `${edge.target} has no archive record`);
    if (typeof archive.integrity !== 'string' || !archive.integrity.startsWith('sha512-')) {
      fail('lock-sri', `${identity.name}@${identity.version} has no sha512 SRI in the lock`);
    }
    const child = owner.children.get(edge.name);
    if (child === undefined) continue;
    dependencyNames.push(edge.name);
    checkDependencyLink({owner, name: edge.name, child});
    if (archive.hasBin !== true) continue;
    const provider = providers.get(identity, archive.integrity);
    compareManifestBins(child.manifest, provider.manifest, `${identity.name}@${identity.version}`);
    for (const [command, binPath] of binTargetsOf(provider.manifest, `${identity.name} provider`)) {
      expectedDirectories.add(BIN_DIRECTORY);
      addCommand(command, child.dir, binPath);
      const declared = provider.described?.bin?.[command];
      if (declared === undefined) continue;
      checkBinTarget(path.join(child.dir, ...declared.path.split('/')), declared);
    }
  }
  checkVirtualEntry({label, owner, dependencyNames});

  compareInventories({label, ownerArchive, installed, expectedLinks, expectedDirectories, boundBinTargets: ownBinTargets});
}

// The dependency edge is the thing that makes a package reachable, so its link is
// re-proved on its own: exact text, canonical target, and the identity already
// established by directory resolution. A correct bin link must not be able to
// conceal a retargeted dependency edge.
function checkDependencyLink({owner, name, child}) {
  let info;
  try {
    info = fs.lstatSync(child.link);
  } catch {
    fail('missing-dependency-link', `${owner.key}: ${child.link} is absent`);
  }
  if (!info.isSymbolicLink()) fail('dependency-link', `${child.link} is not a symlink`);
  const expected = relative(path.dirname(child.link), child.dir);
  if (child.linkText !== expected) {
    fail('dependency-link', `${child.link} points at ${child.linkText}, the canonical target text is ${expected}`);
  }
  if (fs.realpathSync(child.link) !== child.dir) {
    fail('dependency-link', `${child.link} does not resolve to ${child.dir}`);
  }
}

// A virtual-store entry holds exactly its own package directory and one link per
// locked dependency. Anything else in that container is an installed package the
// lock does not describe.
function checkVirtualEntry({label, owner, dependencyNames}) {
  const segments = owner.name.split('/');
  const scoped = segments.length > 1;
  // For a scoped package the scoped directory is the container; for an unscoped
  // one it is the virtual entry's own node_modules.
  const container = path.dirname(owner.dir);
  const expected = new Set([scoped ? segments[1] : segments[0]]);
  for (const name of dependencyNames) {
    if (scoped && !name.startsWith(`${segments[0]}/`)) continue;
    expected.add(scoped ? name.slice(segments[0].length + 1) : name.split('/')[0]);
  }
  const actual = fs.readdirSync(container).sort();
  for (const entry of actual) {
    if (expected.has(entry)) continue;
    fail('unexpected-entry', `${label}: ${path.join(container, entry)} is not a locked package or dependency`);
  }
  for (const entry of expected) {
    if (!actual.includes(entry)) fail('missing-dependency-link', `${label}: ${path.join(container, entry)} is absent`);
  }
}

// A generated link may only name an executable the authenticated archive holds,
// so the bytes and the installation mode are re-proved here, not just trusted.
function checkBinTarget(installedTarget, declared) {
  let info;
  try {
    info = fs.lstatSync(installedTarget);
  } catch {
    fail('missing-bin-target', `${installedTarget} is absent`);
  }
  if (!info.isFile()) fail('missing-bin-target', `${installedTarget} is not a regular file`);
  if ((info.mode & 0o777) !== INSTALLED_BIN_MODE) {
    fail('bin-target-mode',
      `${installedTarget} is ${(info.mode & 0o777).toString(8)}, a locked bin target must be ${INSTALLED_BIN_MODE.toString(8)}`);
  }
  if (hashFile(installedTarget) !== declared.sha256) {
    fail('bin-target-bytes', `${installedTarget} does not match the authenticated archive bytes`);
  }
}

function compareInventories({label, ownerArchive, installed, expectedLinks, expectedDirectories, boundBinTargets}) {
  if (expectedDirectories.has(BIN_DIRECTORY)) expectedDirectories.add(DEPENDENCY_DIRECTORY);
  for (const generated of [...expectedDirectories, BIN_DIRECTORY]) {
    if (ownerArchive.has(generated)) {
      fail('archive-owns-generated-path', `${label} archive already contains ${generated}`);
    }
    for (const [entry, value] of ownerArchive) {
      if (entry.startsWith(`${generated}/`) && value.kind !== 'dir') {
        fail('archive-owns-generated-path', `${label} archive already contains ${entry}`);
      }
    }
  }
  for (const [entry, value] of ownerArchive) {
    const actual = installed.get(entry);
    if (actual === undefined) fail('missing-path', `${label} is missing ${entry}`);
    if (actual.kind !== value.kind) {
      fail('kind-mismatch', `${label} ${entry} is a ${actual.kind}, the archive has a ${value.kind}`);
    }
    if (value.kind === 'file') {
      if (actual.bytes !== value.bytes || actual.size !== value.size) {
        fail('byte-mismatch', `${label} ${entry} does not match the authenticated archive bytes`);
      }
      const expectedMode = boundBinTargets.has(entry) ? INSTALLED_BIN_MODE : value.mode;
      if (actual.mode !== expectedMode) {
        fail('mode-mismatch', `${label} ${entry} is ${actual.mode.toString(8)}, expected ${expectedMode.toString(8)}`);
      }
    } else if (value.kind === 'symlink') {
      if (actual.linkTarget !== value.linkTarget) {
        fail('link-mismatch', `${label} ${entry} points at ${actual.linkTarget}, the archive says ${value.linkTarget}`);
      }
    } else if ((actual.mode & DIRECTORY_ACCESS) !== DIRECTORY_ACCESS) {
      fail('mode-mismatch', `${label} ${entry} is ${actual.mode.toString(8)}; every directory must stay readable and searchable`);
    }
  }
  const normalised = new Set([...expectedDirectories, BIN_DIRECTORY, ...expectedLinks.keys()]);
  for (const entry of normalised) {
    const actual = installed.get(entry);
    if (actual === undefined) {
      if (entry === BIN_DIRECTORY) continue;
      fail('missing-generated', `${label} is missing the generated ${entry}`);
    }
    if (entry === BIN_DIRECTORY || expectedDirectories.has(entry)) {
      if (actual.kind !== 'dir') fail('kind-mismatch', `${label} ${entry} is a ${actual.kind}, expected a directory`);
      if ((actual.mode & DIRECTORY_ACCESS) !== DIRECTORY_ACCESS) {
        fail('mode-mismatch', `${label} ${entry} is ${actual.mode.toString(8)}; generated directories must stay readable and searchable`);
      }
      continue;
    }
    if (actual.kind !== 'symlink') fail('kind-mismatch', `${label} ${entry} is a ${actual.kind}, expected a symlink`);
    const expected = expectedLinks.get(entry);
    if (actual.linkTarget !== expected) {
      fail('link-mismatch', `${label} ${entry} points at ${actual.linkTarget}, the derived target is ${expected}`);
    }
  }
  for (const entry of installed.keys()) {
    if (ownerArchive.has(entry) || normalised.has(entry)) continue;
    fail('unexpected-path', `${label} has an unexplained installed entry ${entry}`);
  }
}

function main(argv) {
  if (argv.length === 2 && argv[0] === 'inventory') {
    process.stdout.write(`${JSON.stringify(familyInventory(argv[1]))}\n`);
    return;
  }
  if (argv.length !== 3) usage();
  const [prefixOperand, sibling, extractionOperand] = argv;
  if (!SIBLING_ARGUMENT.test(sibling)) fail('bad-sibling', `SIBLING must be a family package base name: ${JSON.stringify(sibling)}`);
  const contractDir = process.env.AGENT_VM_CONTRACT_DIR ?? '/tmp/recipe-contract';
  const platform = targetPlatform();
  const graph = readGraph({contractDir, prefix: prefixOperand});
  const resolution = resolveInstances(prefixOperand, graph, {platform});

  const family = resolution.family;
  if (!family.keys.has(sibling)) {
    fail('unexpected-sibling', `${sibling} is not reachable in the installed family ${JSON.stringify(family.shape)}`);
  }
  let extraction;
  try {
    extraction = fs.realpathSync(extractionOperand);
  } catch {
    fail('missing-extraction', `${extractionOperand} does not exist`);
  }
  // The extraction tree is PARENT/<package name>/<version>; the scope segments
  // belong to the package name, so the owner parent is derived from the name the
  // lock gives this sibling, never from a fixed depth.
  const siblingName = `${'@earendil-works'}/${sibling}`;
  const siblingVersion = graph.snapshots.get(family.keys.get(sibling)).id.packageId.version;
  const tail = [...siblingName.split('/'), siblingVersion];
  const segments = extraction.split(path.sep);
  if (segments.length <= tail.length
      || !tail.every((segment, index) => segments[segments.length - tail.length + index] === segment)) {
    fail('extraction-layout', `EXTRACTED_PACKAGE ${extraction} is not a ${siblingName}/${siblingVersion} extraction root`);
  }
  const parent = segments.slice(0, segments.length - tail.length).join(path.sep);
  const familyExtractions = new Map();
  for (const base of family.shape) {
    const key = family.keys.get(base);
    const snapshot = graph.snapshots.get(key);
    const identity = snapshot.id.packageId;
    const root = path.join(parent, ...identity.name.split('/'), identity.version);
    let info;
    try {
      info = fs.lstatSync(root);
    } catch {
      fail('missing-extraction', `${identity.name}@${identity.version} has no authenticated extraction at ${root}`);
    }
    if (!info.isDirectory() || info.isSymbolicLink()) fail('missing-extraction', `${root} is not a directory`);
    familyExtractions.set(identity.name, {root, version: identity.version});
  }
  const expectedExtraction = familyExtractions.get(siblingName)?.root;
  if (expectedExtraction === undefined || expectedExtraction !== extraction) {
    fail('extraction-layout', `EXTRACTED_PACKAGE ${extraction} is not the ${sibling} extraction ${expectedExtraction}`);
  }

  const scratch = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'pi-bytes-')));
  const providers = new Providers({evidenceRoot: path.join(parent, 'evidence'), familyExtractions, scratch});
  try {
    for (const base of family.shape) {
      const name = `${'@earendil-works'}/${base}`;
      const key = family.keys.get(base);
      const snapshot = graph.snapshots.get(key);
      const archive = graph.packages.get(`${name}@${snapshot.id.packageId.version}`);
      if (archive === undefined || typeof archive.integrity !== 'string'
          || !archive.integrity.startsWith('sha512-')) {
        fail('lock-sri', `${name}@${snapshot.id.packageId.version} has no sha512 archive SRI in the lock`);
      }
      const owner = resolution.instances.get(key);
      if (owner === undefined) fail('missing-sibling', `${name} is not installed`);
      const memberExtraction = familyExtractions.get(name).root;
      checkFamilyMember({
        name, version: snapshot.id.packageId.version,
        owner: {...owner, name},
        extraction: memberExtraction, providers, graph,
      });
    }
  } finally {
    fs.rmSync(scratch, {recursive: true, force: true});
  }
  process.stdout.write(`${family.shape.length}/${family.shape.length} Pi sibling archives and installed bytes verified\n`);
}

try {
  main(process.argv.slice(2));
} catch (error) {
  if (error instanceof GraphError) {
    process.stderr.write(`check-installed-bytes.mjs: ${error.message}\n`);
    process.exit(1);
  }
  throw error;
}
