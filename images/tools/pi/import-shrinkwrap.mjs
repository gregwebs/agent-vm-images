#!/usr/bin/env node
// Build-only conversion of an authenticated Pi archive into pnpm's import input.
//
// Pi <=1.0.3 publishes an `npm-shrinkwrap.json`; pnpm does not consume a
// dependency shrinkwrap, so a changed selection would silently resolve ranges
// instead of reproducing the bytes the archive shipped with. This module turns
// the archive's own shrinkwrap into one npm-shaped project lock and then proves
// that what pnpm imported is the same graph. It never installs, never contacts a
// registry, never guesses a hash and never parses YAML: JSON syntax (including
// duplicate-key rejection) belongs to `recipe-contract pnpm-json`, and the
// imported graph comes from `recipe-contract pnpm-lock(-candidate) graph pi`.
//
// Two boundaries are deliberately narrow:
//   * A resolver this module cannot authenticate (git/file/link/workspace/
//     non-npmjs registry, or a specifier that leaves the package) is rejected
//     instead of passed through, because pnpm import would resolve it live.
//   * Integrity is preserved byte-for-byte where the archive has it. The only
//     additions are the historically missing sibling SRIs, and only for exactly
//     the legacy family at the selected version, each bound to a verified
//     archive receipt supplied by the caller.
//
// A conversion delta is *reported*, never allowlisted: this module can classify
// the three reviewed representation normalizations (occurrence dedup, explicit
// optional peer edge, optional peer declaration completion) but it never treats
// one as permission, and nothing here is keyed on a version threshold.
import fs from 'node:fs';

const REGISTRY = 'https://registry.npmjs.org/';
const LEGACY_SIBLINGS = ['chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui'];
const SIBLING_SCOPE = '@earendil-works';
const LOCKFILE_VERSION = 3;
const STAGED_PROJECT_NAME = 'pi-selected-import';
const MAX_DEPTH = 32;
const MAX_PATH = 1024;
const MAX_REASONABLE_LENGTH = 2048;
// The one manifest key order the staged root record carries. It is the record
// `pnpm import` reads for the selected root package; anything else in the
// archive manifest (scripts, devDependencies, exports) is not import input.
const ROOT_RECORD_KEYS = ['dependencies', 'optionalDependencies', 'peerDependencies',
  'peerDependenciesMeta', 'bin', 'engines', 'os', 'cpu', 'libc', 'license'];

const NAME = /^(?:@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*$/;
const EXACT_VERSION = /^(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/;
// A dependency specifier is only resolvable offline when it is a plain version or
// a range over a plain version; every alias/local/git/URL resolver is live. Peer
// declarations are text, not resolutions, so they are only bounded here: the
// shrinkwrap legitimately carries ranges an installer never resolves itself.
const UNSAFE_SPECIFIER = /^(?:[a-z+]+:|\.{1,2}\/|\/|[A-Za-z]:|\\|\0)/i;
// `link:` and `file:` are represented as flags rather than specifiers by npm,
// and a bundled package bypasses registry integrity entirely.
const UNSAFE_RECORD_FLAGS = ['link', 'inBundle', 'extraneous'];

export class ConversionError extends Error {
  constructor(code, detail) {
    super(`${code}: ${detail}`);
    this.name = 'ConversionError';
    this.code = code;
  }
}

const fail = (code, detail) => { throw new ConversionError(code, detail); };

function isPlainObject(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value);
}

function expectObject(value, label) {
  if (!isPlainObject(value)) fail('not-an-object', `${label} must be a JSON object`);
  return value;
}

function expectName(value, label) {
  if (typeof value !== 'string' || !NAME.test(value) || value.length > 214) {
    fail('bad-package-name', `${label} is not a canonical package name: ${JSON.stringify(value)}`);
  }
  return value;
}

function expectVersion(value, label) {
  if (typeof value !== 'string' || !EXACT_VERSION.test(value)) {
    fail('bad-version', `${label} is not an exact semantic version: ${JSON.stringify(value)}`);
  }
  return value;
}

// npm integrity is a whitespace-separated SRI set; every token must be a
// complete sha512 digest. The raw string is preserved because a rewritten but
// equivalent encoding is still a change the reviewer must see.
function expectIntegrity(value, label) {
  if (typeof value !== 'string' || value.length > MAX_REASONABLE_LENGTH) {
    fail('bad-integrity', `${label} has no SRI string`);
  }
  const tokens = value.trim().split(/\s+/);
  if (tokens.length === 0) fail('bad-integrity', `${label} has an empty SRI`);
  for (const token of tokens) {
    const match = /^sha512-([A-Za-z0-9+/]+={0,2})$/.exec(token);
    if (!match) fail('bad-integrity', `${label} is not a sha512 SRI: ${JSON.stringify(token)}`);
    if (Buffer.from(match[1], 'base64').length !== 64) {
      fail('bad-integrity', `${label} sha512 digest is not 64 bytes: ${JSON.stringify(token)}`);
    }
  }
  return value;
}

// The only resolvers this converter can authenticate are exact npmjs tarballs.
// Anything else would be resolved live by pnpm import, outside the SRI proof.
function expectRegistryTarball(value, name, version, label) {
  if (typeof value !== 'string' || value.length > MAX_REASONABLE_LENGTH) {
    fail('unsafe-resolver', `${label} has no tarball URL`);
  }
  const basename = name.slice(name.lastIndexOf('/') + 1);
  const expected = `${REGISTRY}${name}/-/${basename}-${version}.tgz`;
  if (value !== expected) {
    fail('unsafe-resolver', `${label} is not the exact npmjs tarball ${expected}: ${JSON.stringify(value)}`);
  }
  return value;
}

function expectSpecifier(value, label, {declaration = false} = {}) {
  if (typeof value !== 'string' || value.length === 0 || value.length > 256) {
    fail('unsafe-specifier', `${label} is not a dependency specifier`);
  }
  if (declaration) return value;
  if (UNSAFE_SPECIFIER.test(value)) {
    fail('unsafe-specifier', `${label} is not a registry range: ${JSON.stringify(value)}`);
  }
  return value;
}

function dependencyMap(record, key, label, options = {}) {
  if (!(key in record)) return {};
  const map = expectObject(record[key], `${label}.${key}`);
  for (const [name, specifier] of Object.entries(map)) {
    expectName(name, `${label}.${key} name`);
    expectSpecifier(specifier, `${label}.${key}.${name}`, options);
  }
  return map;
}

// Every package location in an npm v3 lock is a `node_modules` path relative to
// the project root. A location that is absolute, empty, dotted or backslashed is
// either a traversal attempt or an npm mode this converter does not model.
function expectLocation(location, label) {
  if (location === '') return '';
  if (location.length > MAX_PATH) fail('unsafe-path', `${label} is longer than ${MAX_PATH}`);
  if (location.startsWith('/') || location.includes('\\') || location.includes('\0')) {
    fail('unsafe-path', `${label} is not a relative posix path: ${JSON.stringify(location)}`);
  }
  const segments = location.split('/');
  if (segments.length > MAX_DEPTH * 2) fail('unsafe-path', `${label} is deeper than ${MAX_DEPTH}`);
  if (segments.some(segment => segment === '' || segment === '.' || segment === '..')) {
    fail('unsafe-path', `${label} has an empty, dot or dot-dot segment: ${JSON.stringify(location)}`);
  }
  const base = location.split('node_modules/');
  if (base.length < 2) fail('unsafe-path', `${label} is not a node_modules path: ${JSON.stringify(location)}`);
  const name = base[base.length - 1];
  expectName(name, `${label} package name`);
  return location;
}

const locationName = location => location.split('node_modules/').slice(-1)[0];
const identityOf = (location, record) => `${locationName(location)}@${record.version}`;

function familySibling(name, version) {
  if (!name.startsWith(`${SIBLING_SCOPE}/`)) return null;
  const base = name.slice(SIBLING_SCOPE.length + 1);
  return LEGACY_SIBLINGS.includes(base) ? {base, version} : null;
}

// One authenticated archive receipt: name, exact version, the npmjs tarball URL
// the SRI covers, and the SRI itself. Registry receipts spell the URL `url` while
// npm lock records spell it `resolved`; both name the same authenticated tarball.
// `receipt` is optional provenance recorded by the caller (download/verification
// evidence); it is not trusted here.
function expectArchive(value, label) {
  const archive = expectObject(value, label);
  const name = expectName(archive.name, `${label}.name`);
  const version = expectVersion(archive.version, `${label}.version`);
  const integrity = expectIntegrity(archive.integrity, `${label}.integrity`);
  return {name, version, integrity, resolved: archive.resolved ?? archive.url};
}

// The caller supplies at most the five legacy siblings, either as a list of
// receipts or as a name-keyed object. Anything else is a rejected supplement.
function expectSiblingResolutions(value, rootVersion) {
  const map = new Map();
  if (value === undefined || value === null) return map;
  const list = Array.isArray(value)
    ? value
    : Object.entries(expectObject(value, 'siblingResolutions')).map(([name, entry]) => ({...expectObject(entry, `siblingResolutions.${name}`), name}));
  for (const entry of list) {
    const archive = expectArchive(entry, 'siblingResolutions entry');
    const family = familySibling(archive.name, archive.version);
    if (family === null) {
      fail('supplement-not-legacy-five', `${archive.name} is not one of the five legacy siblings`);
    }
    if (archive.version !== rootVersion) {
      fail('supplement-version', `${archive.name}@${archive.version} is not at the selected version ${rootVersion}`);
    }
    if (map.has(archive.name)) fail('supplement-duplicate', `${archive.name} is resolved twice`);
    archive.resolved = expectRegistryTarball(archive.resolved, archive.name, archive.version,
      `siblingResolutions ${archive.name}.resolved`);
    map.set(archive.name, archive);
  }
  return map;
}

function expectRootResolution(value, manifestName, manifestVersion) {
  const root = expectArchive(value, 'rootArchiveResolution');
  if (root.name !== manifestName) {
    fail('root-identity-mismatch', `root resolution names ${root.name}, archive manifest names ${manifestName}`);
  }
  if (root.version !== manifestVersion) {
    fail('root-version-mismatch', `root resolution is ${root.version}, archive manifest is ${manifestVersion}`);
  }
  expectRegistryTarball(root.resolved, root.name, root.version, 'rootArchiveResolution.resolved');
  return root;
}

function expectShrinkwrap(value, manifestName, manifestVersion) {
  const shrinkwrap = expectObject(value, 'shrinkwrap');
  if (shrinkwrap.lockfileVersion !== LOCKFILE_VERSION) {
    fail('unsupported-lockfile-version', `only npm lockfileVersion ${LOCKFILE_VERSION} is modelled: ${JSON.stringify(shrinkwrap.lockfileVersion)}`);
  }
  const packages = expectObject(shrinkwrap.packages, 'shrinkwrap.packages');
  const root = expectObject(packages[''], 'shrinkwrap root record');
  if (root.name !== manifestName) {
    fail('root-identity-mismatch', `shrinkwrap root names ${JSON.stringify(root.name)}, archive manifest names ${manifestName}`);
  }
  if (root.version !== manifestVersion) {
    fail('root-version-mismatch', `shrinkwrap root is ${JSON.stringify(root.version)}, archive manifest is ${manifestVersion}`);
  }
  return {root, packages};
}

// The staged project lock is npm-lock-shaped data for `pnpm import` only. It is
// never committed, never COPYed and never handed to npm. The returned object also
// carries `supplements`, the build-provenance list of SRIs this conversion had to
// complete; it is not lock schema and must be dropped before the lock is used.
export function synthesizeProjectLock({archiveManifest, shrinkwrap, rootArchiveResolution, siblingResolutions}) {
  const manifest = expectObject(archiveManifest, 'archiveManifest');
  const manifestName = expectName(manifest.name, 'archiveManifest.name');
  const manifestVersion = expectVersion(manifest.version, 'archiveManifest.version');
  const root = expectRootResolution(rootArchiveResolution, manifestName, manifestVersion);
  const sources = expectSiblingResolutions(siblingResolutions, manifestVersion);
  const {root: shrinkwrapRoot, packages} = expectShrinkwrap(shrinkwrap, manifestName, manifestVersion);
  dependencyMap(shrinkwrapRoot, 'dependencies', 'shrinkwrap root');
  dependencyMap(shrinkwrapRoot, 'optionalDependencies', 'shrinkwrap root');

  // The archive's own dependency graph is what pnpm must reproduce; the root
  // manifest and the shrinkwrap root record have to describe the same graph or
  // the conversion is being run on the wrong pair of files.
  for (const key of ['dependencies', 'optionalDependencies']) {
    const manifestDeps = dependencyMap(manifest, key, 'archiveManifest');
    const shrinkDeps = dependencyMap(shrinkwrapRoot, key, 'shrinkwrap root');
    const names = new Set([...Object.keys(manifestDeps), ...Object.keys(shrinkDeps)]);
    for (const name of names) {
      if (manifestDeps[name] !== shrinkDeps[name]) {
        fail('root-dependency-disagreement', `${key}.${name}: manifest ${JSON.stringify(manifestDeps[name])} vs shrinkwrap ${JSON.stringify(shrinkDeps[name])}`);
      }
    }
  }

  const rootArchive = `node_modules/${manifestName}`;
  const staged = new Map();
  const supplements = [];
  for (const [rawLocation, rawRecord] of Object.entries(packages)) {
    if (rawLocation === '') continue;
    const location = expectLocation(rawLocation, 'shrinkwrap location');
    if (location === rootArchive || location.startsWith(`${rootArchive}/`)) {
      fail('prefixed-archive', `shrinkwrap already places ${location} under the selected archive`);
    }
    const record = expectObject(rawRecord, `shrinkwrap ${location}`);
    for (const flag of UNSAFE_RECORD_FLAGS) {
      if (record[flag]) fail('unsafe-record-mode', `${location} carries ${flag}=true`);
    }
    if (record.dev) fail('dev-only-record', `${location} is a dev-only record`);
    expectVersion(record.version, `shrinkwrap ${location}.version`);
    if ('integrity' in record) {
      expectIntegrity(record.integrity, `shrinkwrap ${location}.integrity`);
    }
    if ('resolved' in record) {
      expectRegistryTarball(record.resolved, locationName(location), record.version, `shrinkwrap ${location}.resolved`);
    }
    dependencyMap(record, 'dependencies', `shrinkwrap ${location}`);
    dependencyMap(record, 'optionalDependencies', `shrinkwrap ${location}`);
    for (const key of ['peerDependencies']) dependencyMap(record, key, `shrinkwrap ${location}`, {declaration: true});

    const name = locationName(location);
    const stagedLocation = `${rootArchive}/${location}`;
    if (staged.has(stagedLocation)) fail('duplicate-location', `${location} appears twice`);
    const carried = {...record};
    if (!('integrity' in record)) {
      const family = familySibling(name, record.version);
      const supplement = sources.get(name);
      if (family === null) fail('missing-integrity', `${location} carries no SRI and is not a legacy sibling`);
      if (supplement === undefined) {
        fail('missing-integrity', `${location} carries no SRI and has no authenticated sibling resolution`);
      }
      if (!('resolved' in record) || record.resolved !== supplement.resolved) {
        fail('supplement-resolver-mismatch', `${location} does not resolve from the authenticated sibling receipt`);
      }
      carried.integrity = supplement.integrity;
      supplements.push({name, version: record.version, integrity: supplement.integrity, resolved: supplement.resolved});
    } else {
      const supplement = sources.get(name);
      if (supplement !== undefined && supplement.integrity !== record.integrity) {
        fail('sri-discrepancy', `${location} carries an SRI that disagrees with the authenticated sibling receipt`);
      }
    }
    staged.set(stagedLocation, carried);
  }

  for (const name of sources.keys()) {
    const used = supplements.some(entry => entry.name === name);
    if (!used) fail('unused-supplement', `${name} was resolved but the shrinkwrap does not omit its SRI`);
  }

  const rootRecord = {version: manifestVersion, resolved: root.resolved, integrity: root.integrity};
  for (const key of ROOT_RECORD_KEYS) {
    if (key in manifest) rootRecord[key] = manifest[key];
  }

  const stagedPackages = {'': {dependencies: {[manifestName]: manifestVersion}}};
  stagedPackages[rootArchive] = rootRecord;
  for (const [location, record] of staged) stagedPackages[location] = record;

  return {
    name: STAGED_PROJECT_NAME,
    lockfileVersion: LOCKFILE_VERSION,
    requires: true,
    packages: stagedPackages,
    // Provenance for the build record: exactly which SRIs the conversion had to
    // complete, in location order, so evidence can name them.
    supplements,
  };
}

// ---------------------------------------------------------------------------
// Delta validation: the synthesized expectation versus what pnpm imported.
// ---------------------------------------------------------------------------

function indexImported(importedProject) {
  const project = expectObject(importedProject, 'importedProject');
  const packages = new Map();
  for (const entry of project.packages ?? []) {
    const id = expectObject(entry.id, 'imported project package id');
    const name = expectName(id.name, 'imported project package name');
    const version = expectVersion(id.version, 'imported project package version');
    packages.set(`${name}@${version}`, {...entry, name, version});
  }
  const snapshots = new Map();
  for (const entry of project.snapshots ?? []) {
    const id = expectObject(entry.id, 'imported project snapshot id');
    const packageId = expectObject(id.packageId, 'imported project snapshot packageId');
    const name = expectName(packageId.name, 'imported project snapshot name');
    const version = expectVersion(packageId.version, 'imported project snapshot version');
    const key = id.key;
    if (typeof key !== 'string' || key.length === 0) fail('bad-imported-graph', 'snapshot id has no key');
    snapshots.set(key, {...entry, name, version, key});
  }
  const importer = Array.isArray(project.importer) ? project.importer : fail('bad-imported-graph', 'importer is not a list');
  const selection = expectObject(project.selection, 'imported project selection');
  return {packages, snapshots, importer, selection};
}

function snapshotByKeyOrFail(snapshots, key, label) {
  const snapshot = snapshots.get(key);
  if (snapshot === undefined) fail('bad-imported-graph', `${label} names an unknown snapshot ${JSON.stringify(key)}`);
  return snapshot;
}

function edgeMap(snapshot) {
  const map = new Map();
  for (const edge of snapshot?.edges ?? []) {
    if (map.has(edge.name)) fail('duplicate-edge', `${snapshot.key} declares ${edge.name} twice`);
    map.set(edge.name, {target: edge.target, optional: edge.optional === true});
  }
  return map;
}

// Node's own lookup: every ancestor's node_modules, skipping directories that are
// themselves named node_modules. This is the npm side of the correspondence; a
// guessed `.pnpm/name@version` directory is never used anywhere in this module.
function resolveFrom(locations, location, name) {
  const segments = location === '' ? [] : location.split('/');
  for (let index = segments.length; index >= 0; index -= 1) {
    if (index > 0 && segments[index - 1] === 'node_modules') continue;
    const prefix = segments.slice(0, index).join('/');
    const candidate = `${prefix === '' ? '' : `${prefix}/`}node_modules/${name}`;
    if (locations.has(candidate)) return candidate;
  }
  return null;
}

function dependencyEdges(record) {
  const edges = new Map();
  for (const [name, specifier] of Object.entries(record.dependencies ?? {})) edges.set(name, {specifier, optional: false});
  for (const [name, specifier] of Object.entries(record.optionalDependencies ?? {})) edges.set(name, {specifier, optional: true});
  return edges;
}

function peerDeclarations(record) {
  const declared = new Map();
  const ranges = isPlainObject(record.peerDependencies) ? record.peerDependencies : {};
  const meta = isPlainObject(record.peerDependenciesMeta) ? record.peerDependenciesMeta : {};
  for (const [name, range] of Object.entries(ranges)) declared.set(name, {range, optional: meta[name]?.optional === true});
  for (const [name, entry] of Object.entries(meta)) {
    if (!declared.has(name)) declared.set(name, {range: null, optional: entry?.optional === true});
  }
  return declared;
}

function importedPeerDeclarations(pkg) {
  const declared = new Map();
  for (const [name, range] of Object.entries(pkg?.peers ?? {})) {
    declared.set(name, {range, optional: (pkg?.optionalPeers ?? []).includes(name)});
  }
  for (const name of pkg?.optionalPeers ?? []) {
    if (!declared.has(name)) declared.set(name, {range: null, optional: true});
  }
  return declared;
}

export function validateShrinkwrapDelta({shrinkwrap, importedProject, allowedMissingSiblingSris}) {
  const rejected = [];
  const reported = [];
  const reject = (code, detail) => rejected.push({code, detail});
  const report = (code, detail) => reported.push({code, detail, requiresReview: true});

  const expected = expectObject(shrinkwrap, 'shrinkwrap');
  if (expected.lockfileVersion !== LOCKFILE_VERSION) {
    fail('unsupported-lockfile-version', `expected lock is not lockfileVersion ${LOCKFILE_VERSION}`);
  }
  const expectedPackages = expectObject(expected.packages, 'shrinkwrap.packages');
  const rootRecord = expectObject(expectedPackages[''], 'expected root record');
  const rootDependencies = expectObject(rootRecord.dependencies, 'expected root dependencies');
  const rootNames = Object.keys(rootDependencies);
  if (rootNames.length !== 1) {
    fail('importer-shape', `expected root must depend only on the selected Pi, found ${JSON.stringify(rootNames)}`);
  }
  const rootName = expectName(rootNames[0], 'expected root dependency');
  const rootVersion = expectVersion(rootDependencies[rootName], 'expected root dependency version');
  const rootArchive = `node_modules/${rootName}`;
  const rootArchiveRecord = expectObject(expectedPackages[rootArchive], 'expected root archive record');
  expectIntegrity(rootArchiveRecord.integrity, `expected ${rootArchive}.integrity`);

  const {packages, snapshots, importer} = indexImported(importedProject);
  if (importer.length !== 1) reject('importer-shape', `expected exactly one root dependency edge, imported ${importer.length}`);
  const rootEdge = importer[0] ?? {};
  if (rootEdge.name !== rootName) {
    reject('importer-shape', `imported root edge is ${JSON.stringify(rootEdge.name)}, expected ${rootName}`);
  }
  const rootSnapshot = rootEdge.target === undefined ? null : snapshots.get(rootEdge.target);
  if (rootSnapshot === null) {
    reject('importer-shape', 'imported root edge names no snapshot');
    return {accepted: false, rejected, reported, counts: {}};
  }
  if (rootSnapshot.name !== rootName || rootSnapshot.version !== rootVersion) {
    reject('root-identity-mismatch', `imported root is ${rootSnapshot.name}@${rootSnapshot.version}, expected ${rootName}@${rootVersion}`);
  }

  // Supplement binding: the caller may complete only the legacy five at the
  // selected version, and only with the SRI the receipt proved.
  const supplements = new Map();
  for (const entry of allowedMissingSiblingSris ?? []) {
    const name = expectName(isPlainObject(entry) ? entry.name : entry, 'allowedMissingSiblingSris entry');
    const version = isPlainObject(entry) ? entry.version ?? rootVersion : rootVersion;
    if (familySibling(name, version) === null) {
      reject('supplement-not-legacy-five', `${name}@${version} is not a legacy five sibling`);
      continue;
    }
    if (version !== rootVersion) {
      reject('supplement-version', `${name}@${version} is not at the selected version ${rootVersion}`);
      continue;
    }
    supplements.set(name, isPlainObject(entry) ? entry.integrity ?? null : null);
  }
  const expectedLocations = new Map();
  for (const [location, record] of Object.entries(expectedPackages)) {
    if (location === '') continue;
    expectedLocations.set(location, expectObject(record, `expected ${location}`));
  }
  for (const [name, integrity] of supplements) {
    const location = `${rootArchive}/node_modules/${name}`;
    const record = expectedLocations.get(location);
    if (record === undefined) {
      reject('supplement-unused', `${location} is not in the expected graph`);
      continue;
    }
    if (integrity !== null && record.integrity !== integrity) {
      reject('supplement-integrity-mismatch', `${location} does not carry the authenticated receipt SRI`);
    }
  }
  for (const [location, record] of expectedLocations) {
    if (!('integrity' in record)) reject('missing-integrity', `${location} carries no SRI in the expected graph`);
  }

  // Every expected occurrence must be an archive pnpm imported, and every
  // imported archive must be an expected occurrence.
  const expectedIdentities = new Map();
  for (const [location, record] of expectedLocations) {
    const identity = identityOf(location, record);
    if (!expectedIdentities.has(identity)) expectedIdentities.set(identity, []);
    expectedIdentities.get(identity).push(location);
  }
  const importedIdentities = new Set([...packages.keys()]);
  const addedIdentities = [...importedIdentities].filter(identity => !expectedIdentities.has(identity)).sort();
  const removedIdentities = [...expectedIdentities.keys()].filter(identity => !importedIdentities.has(identity)).sort();
  if (removedIdentities.length) reject('identity-removed', removedIdentities.join(', '));
  for (const identity of addedIdentities) {
    // An archive pnpm kept without any snapshot is a record the expectation
    // never described, regardless of reachability.
    reject('identity-added', identity);
  }

  // Root pair, then guarded edge traversal. A changed resolved target stops the
  // walk at that edge: the identity is reported, and its descendants are never
  // re-paired by name.
  const pairs = [];
  const queue = [[rootArchive, rootSnapshot.key]];
  const seen = new Set();
  const mappedExpected = new Set();
  const mappedImported = new Set();
  while (queue.length) {
    const [location, key] = queue.shift();
    const pairId = `${location}\u0000${key}`;
    if (seen.has(pairId)) continue;
    seen.add(pairId);
    mappedExpected.add(location);
    mappedImported.add(key);
    pairs.push([location, key]);
    const record = expectedLocations.get(location) ?? (location === rootArchive ? rootArchiveRecord : undefined);
    const snapshot = snapshots.get(key);
    if (record === undefined || snapshot === undefined) continue;
    const expectedEdges = dependencyEdges(record);
    const importedEdges = edgeMap(snapshot);
    for (const name of [...new Set([...expectedEdges.keys(), ...importedEdges.keys()])].sort()) {
      const want = expectedEdges.get(name);
      const got = importedEdges.get(name);
      if (want === undefined && got === undefined) continue;
      const declared = peerDeclarations(record).has(name) || importedPeerDeclarations(packages.get(identityOf(location, record))).has(name);
      if (want === undefined) {
        if (!declared) {
          reject('edge-added', `${location}: ${name} -> ${got.target}`);
          continue;
        }
        const importedTarget = snapshots.get(got.target);
        const npmTarget = resolveFrom(expectedLocations, location, name);
        const npmIdentity = npmTarget === null ? null : identityOf(npmTarget, expectedLocations.get(npmTarget));
        const importedIdentity = importedTarget === undefined ? null : `${importedTarget.name}@${importedTarget.version}`;
        if (npmIdentity === importedIdentity && npmIdentity !== null) {
          report('explicit-peer-expansion', {owner: location, name, imported: importedIdentity});
          queue.push([npmTarget, got.target]);
        } else {
          reject('effective-peer-changed', `${location}: peer ${name} resolves to ${JSON.stringify(importedIdentity)}, expected ${JSON.stringify(npmIdentity)}`);
        }
        continue;
      }
      if (got === undefined) {
        reject('edge-dropped', `${location}: ${name}`);
        continue;
      }
      if (want.optional !== got.optional) {
        reject('edge-optionality-changed', `${location}: ${name} is ${got.optional ? 'optional' : 'mandatory'} in the imported graph`);
      }
      const npmTarget = resolveFrom(expectedLocations, location, name);
      const importedTarget = snapshots.get(got.target);
      const npmIdentity = npmTarget === null ? null : identityOf(npmTarget, expectedLocations.get(npmTarget));
      const importedIdentity = importedTarget === undefined ? null : `${importedTarget.name}@${importedTarget.version}`;
      if (npmIdentity === null || importedIdentity === null || npmIdentity !== importedIdentity) {
        reject('edge-target-changed', `${location}: ${name} -> ${JSON.stringify(importedIdentity)}, expected ${JSON.stringify(npmIdentity)}`);
        continue;
      }
      queue.push([npmTarget, got.target]);
    }
  }

  const counts = {
    expectedOccurrences: expectedLocations.size,
    expectedIdentities: expectedIdentities.size,
    importedPackages: packages.size,
    importedSnapshots: snapshots.size,
    identityIntersection: [...expectedIdentities.keys()].filter(identity => importedIdentities.has(identity)).length,
    identitiesAdded: addedIdentities.length,
    identitiesRemoved: removedIdentities.length,
    mappedPairs: pairs.length,
    mappedExpectedOccurrences: mappedExpected.size,
    mappedImportedSnapshots: mappedImported.size,
    sriPairs: 0,
    sriRawEqualPairs: 0,
    sriRawUnequalPairs: 0,
    optionalClassificationPairs: 0,
    optionalClassificationMismatches: 0,
    binPresencePairs: 0,
    binPresenceMismatches: 0,
    peerComparisons: 0,
    peerDeclarationChanges: 0,
    effectivePeerChanges: rejected.filter(entry => entry.code === 'effective-peer-changed').length,
    explicitPeerExpansions: reported.filter(entry => entry.code === 'explicit-peer-expansion').length,
    occurrenceMultiplicityCollapsed: 0,
  };

  // Per-pair archive-level facts: SRI, snapshot optionality, bin presence and
  // declared peers. These are compared for every mapped pair, so a same-version
  // substitution cannot hide behind an equal identity.
  const pairsByImported = new Map();
  for (const [location, key] of pairs) {
    if (!pairsByImported.has(key)) pairsByImported.set(key, []);
    pairsByImported.get(key).push(location);
  }
  for (const [importedKey, locations] of pairsByImported) {
    if (locations.length > 1) {
      for (const location of locations) if (!expectedLocations.has(location)) fail('bad-mapping', `paired location ${location} is not an expected occurrence`);
      const identities = new Set(locations.map(location => identityOf(location, expectedLocations.get(location))));
      if (identities.size === 1) {
        const [identity] = identities;
        report('occurrence-multiplicity-collapse', {identity, expectedOccurrences: locations.length, importedSnapshots: 1});
        counts.occurrenceMultiplicityCollapsed += locations.length - 1;
      } else {
        reject('wrong-context-substitution', `${importedKey} is paired with ${locations.join(', ')}`);
      }
    }
  }
  for (const [location, key] of pairs) {
    const record = expectedLocations.get(location) ?? (location === rootArchive ? rootArchiveRecord : undefined);
    const snapshot = snapshots.get(key);
    if (record === undefined || snapshot === undefined) continue;
    const importedPackage = packages.get(`${snapshot.name}@${snapshot.version}`);
    if (importedPackage === undefined) {
      reject('identity-removed', `imported snapshot ${key} has no archive record`);
      continue;
    }
    counts.sriPairs += 1;
    if (record.integrity === importedPackage.integrity) {
      counts.sriRawEqualPairs += 1;
    } else {
      counts.sriRawUnequalPairs += 1;
      reject('sri-changed', `${snapshot.name}@${snapshot.version}: imported ${JSON.stringify(importedPackage.integrity)}, expected ${JSON.stringify(record.integrity)}`);
    }
    counts.optionalClassificationPairs += 1;
    const expectedOptional = record.optional === true;
    const importedOptional = snapshot.optional === true;
    if (expectedOptional !== importedOptional) {
      counts.optionalClassificationMismatches += 1;
      reject('optional-classification-changed', `${snapshot.name}@${snapshot.version}: imported optional=${importedOptional}, expected optional=${expectedOptional}`);
    }
    counts.binPresencePairs += 1;
    if ((record.bin !== undefined && record.bin !== null) !== (importedPackage.hasBin === true)) {
      counts.binPresenceMismatches += 1;
      reject('bin-presence-changed', `${snapshot.name}@${snapshot.version}: imported hasBin=${importedPackage.hasBin === true}`);
    }
    const expectedPeers = peerDeclarations(record);
    const importedPeers = importedPeerDeclarations(importedPackage);
    for (const name of [...new Set([...expectedPeers.keys(), ...importedPeers.keys()])].sort()) {
      counts.peerComparisons += 1;
      const want = expectedPeers.get(name) ?? {range: null, optional: false};
      const got = importedPeers.get(name) ?? {range: null, optional: false};
      if (want.range === got.range && want.optional === got.optional) continue;
      counts.peerDeclarationChanges += 1;
      // The one reviewed normalization is a wildcard declaration pnpm adds where
      // the archive carried only optional peer metadata. Any real range change,
      // any lost declaration and any optionality change is a hard rejection: a
      // representation delta is never a licence to rewrite peer semantics.
      if (want.optional !== got.optional) {
        reject('peer-optionality-changed', `${snapshot.name}@${snapshot.version}: peer ${name} optional=${got.optional}`);
      } else if (got.range === null) {
        reject('peer-declaration-dropped', `${snapshot.name}@${snapshot.version}: peer ${name} declared ${JSON.stringify(want.range)}, imported nothing`);
      } else if (want.range === null && got.range === '*') {
        report('peer-declaration-completion', {owner: `${snapshot.name}@${snapshot.version}`, peer: name, expected: want.range, imported: got.range});
      } else {
        reject('peer-declaration-changed', `${snapshot.name}@${snapshot.version}: peer ${name} declared ${JSON.stringify(got.range)}, expected ${JSON.stringify(want.range)}`);
      }
    }
  }

  // Reachability: an imported snapshot nothing reaches is a payload the
  // expectation never described, and an expected occurrence left unmapped means
  // the traversal could not prove correspondence.
  const reachable = new Set();
  const pending = [rootSnapshot.key];
  while (pending.length) {
    const key = pending.pop();
    if (reachable.has(key)) continue;
    reachable.add(key);
    for (const edge of edgeMap(snapshots.get(key) ?? {key}).values()) {
      if (edge.target !== undefined) pending.push(edge.target);
    }
  }
  for (const key of [...snapshots.keys()].sort()) {
    if (!reachable.has(key)) reject('unreachable-snapshot', key);
  }
  for (const location of [...expectedLocations.keys()].sort()) {
    if (!mappedExpected.has(location)) reject('unmapped-occurrence', location);
  }

  return {
    accepted: rejected.length === 0,
    rejected,
    reported,
    counts,
    supplements: [...supplements.keys()].sort(),
  };
}

// ---------------------------------------------------------------------------
// CLI. Inputs are canonical JSON already produced by `recipe-contract pnpm-json`
// (or by this module's own synthesis); this CLI never reads YAML itself.
// ---------------------------------------------------------------------------
function readJson(file) {
  if (file === '-') return JSON.parse(fs.readFileSync(0, 'utf8'));
  return JSON.parse(fs.readFileSync(file, 'utf8'));
}

function usage() {
  process.stderr.write(`usage: import-shrinkwrap.mjs synthesize --manifest FILE --shrinkwrap FILE \\
  --root FILE --siblings FILE | validate --expect FILE --graph FILE [--allowed-missing FILE]\n`);
  process.exit(2);
}

function parseArgs(argv) {
  const options = {};
  for (let index = 0; index < argv.length; index += 1) {
    const flag = argv[index];
    if (!flag.startsWith('--')) usage();
    const value = argv[index + 1];
    if (value === undefined) usage();
    options[flag.slice(2)] = value;
    index += 1;
  }
  return options;
}

function main(argv) {
  const verb = argv[0];
  const options = parseArgs(argv.slice(1));
  if (verb === 'synthesize') {
    if (!options.manifest || !options.shrinkwrap || !options.root || !options.siblings) usage();
    const result = synthesizeProjectLock({
      archiveManifest: readJson(options.manifest),
      shrinkwrap: readJson(options.shrinkwrap),
      rootArchiveResolution: readJson(options.root),
      siblingResolutions: readJson(options.siblings),
    });
    // `supplements` is build provenance, not npm lock schema: stdout stays a lock
    // `pnpm import` can consume, and the completed-SRI receipts go to stderr for
    // the build record.
    const {supplements, ...lock} = result;
    process.stdout.write(`${JSON.stringify(lock, null, 2)}\n`);
    process.stderr.write(`completed SRIs: ${JSON.stringify(supplements)}\n`);
    return;
  }
  if (verb === 'validate') {
    if (!options.expect || !options.graph) usage();
    const allowed = options['allowed-missing'] ? readJson(options['allowed-missing']) : [];
    const result = validateShrinkwrapDelta({
      shrinkwrap: readJson(options.expect),
      importedProject: readJson(options.graph),
      allowedMissingSiblingSris: allowed,
    });
    process.stdout.write(`${JSON.stringify(result, null, 2)}\n`);
    if (!result.accepted) process.exit(1);
    return;
  }
  usage();
}

if (process.argv[1] !== undefined && import.meta.url === `file://${process.argv[1]}`) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    if (error instanceof ConversionError) {
      process.stderr.write(`import-shrinkwrap.mjs: ${error.message}\n`);
      process.exit(1);
    }
    throw error;
  }
}
