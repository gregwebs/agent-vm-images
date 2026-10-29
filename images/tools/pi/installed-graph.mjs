#!/usr/bin/env node
// Build-only physical installed-graph derivation for the Pi pnpm tree.
//
// A lock is a claim about a graph, not about directories. pnpm's isolated layout
// makes the mapping lock-instance -> physical directory indirect: the public
// root is a symlink into `node_modules/.pnpm/<entry>/node_modules/<name>`, each
// dependency is a relative symlink between virtual-store entries, and entry names
// carry peer suffixes and hashes that are opaque by design. This module therefore
// never parses an identity out of a directory name and never guesses
// `.pnpm/name@version`: it walks the actual edges from the canonical owning
// directory with Node's own ancestor resolution and checks every step against the
// locked snapshot it is supposed to realize.
//
// Two invariants make the derived map trustworthy for the byte checker:
//   * the same snapshot key always maps to one canonical directory, and one
//     canonical directory always maps to one snapshot key, so a same-version
//     directory from another peer context cannot stand in for the locked one;
//   * every physical package instance under the prefix is either a visited
//     instance or an explicitly typed platform omission, so a shadow copy
//     elsewhere cannot satisfy a check that should have followed an edge.
//
// The graph itself is always the validated Go projection
// (`recipe-contract pnpm-lock graph pi PREFIX`); this module never reads YAML.
import {spawnSync} from 'node:child_process';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

const ROOT_SCOPE = '@earendil-works';
const FAMILY_VOCABULARY = ['chord', 'pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui'];
// The two reviewed reachable shapes, selected by authenticated membership rather
// than by a version threshold: the historical release that still ships `chord`
// and the modern release that ships codemode/mcp instead.
const REVIEWED_FAMILY_SHAPES = [
  ['chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui'],
  ['pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui'],
];
const VIRTUAL_STORE = path.join('node_modules', '.pnpm');
const GRAPH_TIMEOUT_MS = 120_000;
const MAX_GRAPH_BYTES = 64 * 1024 * 1024;
const MAX_MANIFEST_BYTES = 1024 * 1024;
const MAX_INVENTORY_ENTRIES = 200_000;
const MAX_INVENTORY_DEPTH = 40;
const MAX_FILE_BYTES = 512 * 1024 * 1024;
const READ_CHUNK = 1024 * 1024;
// The Debian images this repository builds are glibc; a musl or unknown target
// would make a platform omission unprovable, so it is not modelled.
const TARGET_PLATFORMS = {
  'linux/amd64': {os: 'linux', cpu: 'x64', libc: 'glibc'},
  'linux/arm64': {os: 'linux', cpu: 'arm64', libc: 'glibc'},
};

export class GraphError extends Error {
  constructor(code, detail) {
    super(`${code}: ${detail}`);
    this.name = 'GraphError';
    this.code = code;
  }
}

const fail = (code, detail) => { throw new GraphError(code, detail); };

const relative = (from, to) => path.relative(from, to).split(path.sep).join('/');
const lexicallyInside = (parent, child) => {
  const rel = path.relative(parent, child);
  return rel !== '' && !rel.startsWith('..') && !path.isAbsolute(rel);
};

export function targetPlatform(value = process.env.AGENT_VM_PI_TARGET_PLATFORM) {
  if (value === undefined || value === '') {
    fail('missing-target-platform',
      'AGENT_VM_PI_TARGET_PLATFORM must name linux/amd64 or linux/arm64 explicitly; the host is never guessed');
  }
  const target = TARGET_PLATFORMS[value];
  if (target === undefined) {
    fail('unsupported-target-platform', `AGENT_VM_PI_TARGET_PLATFORM=${value} is not one of ${Object.keys(TARGET_PLATFORMS).join(', ')}`);
  }
  return {...target, spelling: value};
}

// pnpm's own checkList semantics, restated literally: a single `any` matches
// everything; otherwise a non-matching negation denies, a declared positive list
// requires a match, and an all-negative list permits every non-denied target.
export function axisAllows(selectors, value) {
  if (!Array.isArray(selectors) || selectors.length === 0) return true;
  if (selectors.length === 1 && selectors[0] === 'any') return true;
  const positives = selectors.filter(token => !token.startsWith('!'));
  const negatives = selectors.filter(token => token.startsWith('!')).map(token => token.slice(1));
  if (negatives.includes(value)) return false;
  if (positives.length === 0) return true;
  return positives.includes(value);
}

export function platformAllows(platform, target) {
  const axes = platform ?? {};
  return axisAllows(axes.os, target.os) && axisAllows(axes.cpu, target.cpu) && axisAllows(axes.libc, target.libc);
}

export function readGraph({contractDir, prefix}) {
  const binary = path.join(contractDir, 'recipe-contract');
  let info;
  try {
    info = fs.statSync(binary);
  } catch {
    fail('missing-contract', `no recipe-contract helper at ${binary}`);
  }
  if (!info.isFile()) fail('missing-contract', `${binary} is not a regular file`);
  try {
    fs.accessSync(binary, fs.constants.X_OK);
  } catch {
    fail('missing-contract', `${binary} is not executable`);
  }
  const result = spawnSync(binary, ['pnpm-lock', 'graph', 'pi', prefix], {
    encoding: 'utf8', timeout: GRAPH_TIMEOUT_MS, maxBuffer: MAX_GRAPH_BYTES,
  });
  if (result.error !== undefined) fail('graph-run', `${binary}: ${result.error.message}`);
  if (result.status !== 0) {
    fail('graph-run', `${binary} pnpm-lock graph pi exited ${result.status}: ${(result.stderr ?? '').trim()}`);
  }
  let graph;
  try {
    graph = JSON.parse(result.stdout);
  } catch {
    fail('graph-json', 'the graph projection is not JSON');
  }
  if (graph === null || typeof graph !== 'object' || !Array.isArray(graph.packages)
      || !Array.isArray(graph.snapshots) || !Array.isArray(graph.importer)) {
    fail('graph-json', 'the graph projection is missing packages/snapshots/importer');
  }
  return indexGraph(graph);
}

function indexGraph(graph) {
  const packages = new Map();
  for (const entry of graph.packages) {
    const name = entry?.id?.name;
    const version = entry?.id?.version;
    if (typeof name !== 'string' || typeof version !== 'string') fail('graph-json', 'package record without an id');
    packages.set(`${name}@${version}`, entry);
  }
  const snapshots = new Map();
  for (const entry of graph.snapshots) {
    const key = entry?.id?.key;
    const name = entry?.id?.packageId?.name;
    const version = entry?.id?.packageId?.version;
    if (typeof key !== 'string' || typeof name !== 'string' || typeof version !== 'string') {
      fail('graph-json', 'snapshot record without an id');
    }
    snapshots.set(key, entry);
  }
  return {packages, snapshots, importer: graph.importer, selection: graph.selection};
}

function packageOf(graph, name, version) {
  const entry = graph.packages.get(`${name}@${version}`);
  if (entry === undefined) fail('graph-json', `snapshot ${name}@${version} has no archive record`);
  return entry;
}

function edgesOf(snapshot) {
  if (!Array.isArray(snapshot.edges)) fail('graph-json', `${snapshot.id.key} has no edge list`);
  for (const edge of snapshot.edges) {
    if (typeof edge?.name !== 'string' || typeof edge?.target !== 'string') {
      fail('graph-json', `${snapshot.id.key} has an edge without a name or target`);
    }
  }
  return snapshot.edges;
}

// The reachable family is derived from the importer closure and matched against
// the reviewed shapes. An `@earendil-works` package the vocabulary does not name
// is an unreviewed family change, not an extra to ignore.
export function familyShape(graph) {
  // The selected root archive is itself in the @earendil-works scope; it is the
  // subject of the check, not a family member.
  const rootName = graph.importer[0]?.name;
  const reachable = new Set();
  const pending = graph.importer.map(edge => edge.target);
  while (pending.length) {
    const key = pending.pop();
    if (reachable.has(key)) continue;
    reachable.add(key);
    const snapshot = graph.snapshots.get(key);
    if (snapshot === undefined) fail('graph-json', `edge target ${key} has no snapshot`);
    for (const edge of edgesOf(snapshot)) pending.push(edge.target);
  }
  const members = new Map();
  for (const key of reachable) {
    const snapshot = graph.snapshots.get(key);
    const name = snapshot.id.packageId.name;
    if (!name.startsWith(`${ROOT_SCOPE}/`) || name === rootName) continue;
    const base = name.slice(ROOT_SCOPE.length + 1);
    if (!FAMILY_VOCABULARY.includes(base)) {
      fail('unreviewed-family-member', `${name}@${snapshot.id.packageId.version} is reachable but not a reviewed family name`);
    }
    if (members.has(base)) {
      fail('duplicate-family-member',
        `${name} is reachable in ${members.get(base).join(', ')} and ${key}`);
    }
    members.set(base, [key]);
  }
  const observed = [...members.keys()].sort();
  const matched = REVIEWED_FAMILY_SHAPES.filter(shape => shape.length === observed.length
    && shape.every(name => members.has(name)));
  if (matched.length !== 1) {
    fail('family-shape', `reachable family ${JSON.stringify(observed)} is not one of the reviewed shapes ${JSON.stringify(REVIEWED_FAMILY_SHAPES)}`);
  }
  return {
    shape: matched[0].slice().sort(),
    keys: new Map([...members].map(([base, keys]) => [base, keys[0]])),
  };
}

function readManifest(directory, label) {
  const file = path.join(directory, 'package.json');
  let info;
  try {
    info = fs.lstatSync(file);
  } catch {
    fail('missing-manifest', `${label} has no package.json at ${file}`);
  }
  if (!info.isFile() || info.size > MAX_MANIFEST_BYTES) {
    fail('missing-manifest', `${label} package.json is not a regular file <=1 MiB`);
  }
  let value;
  try {
    value = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    fail('missing-manifest', `${label} package.json is not JSON`);
  }
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    fail('missing-manifest', `${label} package.json is not an object`);
  }
  if (typeof value.name !== 'string' || typeof value.version !== 'string') {
    fail('missing-manifest', `${label} package.json has no name/version`);
  }
  return value;
}

function nodeModulesPaths(start) {
  const candidates = [];
  let directory = start;
  for (;;) {
    if (path.basename(directory) !== 'node_modules') candidates.push(path.join(directory, 'node_modules'));
    const parent = path.dirname(directory);
    if (parent === directory) break;
    directory = parent;
  }
  return candidates;
}

// Resolve one dependency name exactly the way Node would from the owning package
// directory, then prove the result is a pnpm link inside this prefix.
function resolveDependency(context, ownerDir, name, label) {
  for (const modules of nodeModulesPaths(ownerDir)) {
    const candidate = path.join(modules, name);
    let info;
    try {
      info = fs.lstatSync(candidate);
    } catch {
      continue;
    }
    if (info.isSymbolicLink()) {
      let target;
      try {
        target = fs.realpathSync(candidate);
      } catch {
        fail('dangling-link', `${label} ${candidate} does not resolve`);
      }
      if (!lexicallyInside(context.virtualStore, target)) {
        fail('link-escape', `${label} ${candidate} resolves outside ${context.virtualStore}`);
      }
      if (!target.endsWith(path.join('node_modules', ...name.split('/')))) {
        fail('link-layout', `${label} ${candidate} resolves to ${target}, which is not a ${name} package`);
      }
      if (!fs.statSync(target).isDirectory()) fail('link-layout', `${label} ${target} is not a directory`);
      return {link: candidate, dir: target, linkText: fs.readlinkSync(candidate)};
    }
    if (info.isDirectory()) {
      // A real directory in a dependency position is what pnpm's hoisted linker
      // would produce; with nodeLinker=isolated it is a shadow or a stray copy.
      fail('hoisted-dependency', `${label} ${candidate} is a real directory, not a virtual-store link`);
    }
    fail('link-layout', `${label} ${candidate} is neither a symlink nor a directory`);
  }
  return null;
}

// The pinned manager writes these metadata files at the top of `node_modules`.
// They are not packages, so an orphan scan must not read them as installed
// payload -- but only a *regular file* of exactly this name is metadata: a
// directory or a link with the same name is still reported, and every other
// unexplained entry still fails. Installed by the focused harness' first real
// cold install (pnpm 11.11.0 writes .modules.yaml, .package-map.json and
// .pnpm-workspace-state-v1.json), so this is observed manager output, not a
// convenience allowlist.
const MANAGER_METADATA = new Set(['.modules.yaml', '.package-map.json',
  '.pnpm-workspace-state-v1.json']);

function physicalInstances(prefix) {
  const found = [];
  const modules = path.join(prefix, 'node_modules');
  const inspect = (directory, depth) => {
    if (depth > 4) return;
    let entries;
    try {
      entries = fs.readdirSync(directory, {withFileTypes: true});
    } catch {
      return;
    }
    for (const entry of entries) {
      const location = path.join(directory, entry.name);
      if (entry.name === '.bin') continue;
      if (MANAGER_METADATA.has(entry.name) && entry.isFile()) continue;
      if (entry.name === '.pnpm') {
        for (const store of fs.readdirSync(location, {withFileTypes: true})) {
          if (store.name === 'node_modules') {
            // nodeLinker=isolated with hoist:false must not leave a hoisted
            // package scope. `.bin` plumbing there is not a package copy.
            let hoisted = [];
            try {
              hoisted = fs.readdirSync(path.join(location, store.name)).filter(name => name !== '.bin');
            } catch {
              hoisted = [];
            }
            if (hoisted.length) found.push({location: path.join(location, store.name), kind: 'hoisted-scope', names: hoisted});
            continue;
          }
          inspect(path.join(location, store.name, 'node_modules'), depth + 1);
        }
        continue;
      }
      if (entry.name.startsWith('@') && entry.isDirectory()) {
        inspect(location, depth + 1);
        continue;
      }
      found.push({location, kind: entry.isSymbolicLink() ? 'link' : 'instance'});
    }
  };
  inspect(modules, 0);
  return found;
}

export function resolveInstances(prefixInput, graph, {platform = targetPlatform()} = {}) {
  const prefix = fs.realpathSync(prefixInput);
  if (!fs.statSync(prefix).isDirectory()) fail('prefix', `${prefix} is not a directory`);
  const virtualStore = path.join(prefix, VIRTUAL_STORE);
  if (!fs.existsSync(virtualStore)) fail('layout', `no ${VIRTUAL_STORE} under ${prefix}`);
  if (graph.importer.length !== 1) {
    fail('importer-shape', `the Pi project must have exactly one root dependency, found ${graph.importer.length}`);
  }
  const rootEdge = graph.importer[0];
  const rootSnapshot = graph.snapshots.get(rootEdge.target);
  if (rootSnapshot === undefined) fail('graph-json', `root edge targets unknown snapshot ${rootEdge.target}`);
  if (rootSnapshot.id.packageId.name !== rootEdge.name) {
    fail('importer-shape', `root edge ${rootEdge.name} targets ${rootSnapshot.id.packageId.name}`);
  }
  const rootLink = path.join(prefix, 'node_modules', ...rootEdge.name.split('/'));
  let rootLinkInfo;
  try {
    rootLinkInfo = fs.lstatSync(rootLink);
  } catch {
    fail('missing-root', `${rootLink} does not exist`);
  }
  if (!rootLinkInfo.isSymbolicLink()) fail('missing-root', `${rootLink} is not the pnpm root link`);
  const rootDir = fs.realpathSync(rootLink);
  if (!lexicallyInside(virtualStore, rootDir)) {
    fail('link-escape', `the root link resolves outside ${virtualStore}: ${rootDir}`);
  }

  // The reviewed family shape is a property of the graph alone, so it is settled
  // before any directory is judged: a four/six/six/seven sibling set must fail as
  // a shape change, not as an incidental layout error.
  const family = familyShape(graph);
  const context = {virtualStore};
  const instances = new Map();
  const dirToKey = new Map();
  const omitted = new Map();
  const register = (key, dir, manifest, link) => {
    if (instances.has(key) && instances.get(key).dir !== dir) {
      fail('inconsistent-instance', `${key} maps to ${instances.get(key).dir} and ${dir}`);
    }
    if (dirToKey.has(dir) && dirToKey.get(dir) !== key) {
      fail('wrong-context-substitution', `${dir} is claimed by ${dirToKey.get(dir)} and ${key}`);
    }
    dirToKey.set(dir, key);
    if (!instances.has(key)) {
      instances.set(key, {key, dir, manifest, link, edges: edgesOf(graph.snapshots.get(key)), children: new Map()});
    }
    return instances.get(key);
  };

  const rootManifest = readManifest(rootDir, 'the root package');
  const rootIdentity = rootSnapshot.id.packageId;
  if (rootManifest.name !== rootIdentity.name || rootManifest.version !== rootIdentity.version) {
    fail('manifest-mismatch',
      `the root directory declares ${rootManifest.name}@${rootManifest.version}, the lock says ${rootIdentity.name}@${rootIdentity.version}`);
  }
  const rootInstance = register(rootSnapshot.id.key, rootDir, rootManifest, rootLink);

  const queue = [rootInstance];
  const processed = new Set();
  while (queue.length) {
    const owner = queue.shift();
    if (processed.has(owner.key)) continue;
    processed.add(owner.key);
    for (const edge of owner.edges) {
      const target = graph.snapshots.get(edge.target);
      if (target === undefined) fail('graph-json', `${owner.key} depends on unknown snapshot ${edge.target}`);
      const archive = packageOf(graph, target.id.packageId.name, target.id.packageId.version);
      // An absent payload is excused only by positive evidence: the declaring
      // edge is optional, the lock marks the target itself optional, and the
      // target's locked selectors exclude this platform. Optionality is a fact
      // the lock records twice — on the package record and on the snapshot — and
      // pnpm's `import` writes only the snapshot one, so either spelling of the
      // same fact counts. The selectors are the part that makes the omission
      // provable, and they must positively exclude the target: an unknown or
      // empty selector set leaves the package required, so a compatible optional
      // payload that is missing is still a hard failure.
      const optionalTarget = archive.optional === true || target.optional === true;
      if (edge.optional === true && optionalTarget && !platformAllows(archive.platform, platform)) {
        omitted.set(edge.target, {reason: 'platform', owner: owner.key, name: edge.name});
        continue;
      }
      const label = `${owner.key} -> ${edge.name}`;
      const resolved = resolveDependency(context, owner.dir, edge.name, label);
      if (resolved === null) fail('missing-dependency', `${label}: no resolvable package`);
      const manifest = readManifest(resolved.dir, label);
      if (manifest.name !== target.id.packageId.name || manifest.version !== target.id.packageId.version) {
        fail('manifest-mismatch',
          `${label}: ${resolved.dir} declares ${manifest.name}@${manifest.version}, the lock says ${target.id.packageId.name}@${target.id.packageId.version}`);
      }
      const instance = register(edge.target, resolved.dir, manifest, resolved.link);
      owner.children.set(edge.name, {link: resolved.link, linkText: resolved.linkText,
        dir: resolved.dir, key: edge.target, manifest});
      if (!processed.has(instance.key)) queue.push(instance);
    }
  }

  for (const key of graph.snapshots.keys()) {
    if (instances.has(key) || omitted.has(key)) continue;
    fail('unreachable-instance', `the lock names ${key} but no installed directory realizes it`);
  }
  for (const physical of physicalInstances(prefix)) {
    if (physical.kind === 'hoisted-scope') {
      fail('hoisted-shadow', `${physical.location} carries hoisted packages ${JSON.stringify(physical.names)}; nodeLinker=isolated with hoist:false must not create one`);
    }
    if (physical.kind === 'link') {
      let target;
      try {
        target = fs.realpathSync(physical.location);
      } catch {
        fail('dangling-link', `${physical.location} does not resolve`);
      }
      if (!dirToKey.has(target)) fail('unreachable-payload', `${physical.location} is a link to unvisited ${target}`);
      continue;
    }
    if (!dirToKey.has(physical.location)) {
      fail('unreachable-payload', `${physical.location} is an installed package nothing in the lock reaches`);
    }
  }

  for (const [base, key] of family.keys) {
    if (!instances.has(key) && !omitted.has(key)) fail('missing-sibling', `${ROOT_SCOPE}/${base} is not installed`);
  }
  return {prefix, virtualStore, root: rootInstance, instances, dirToKey, omitted, family};
}

// A non-following inventory keyed by path. Only mtime/uid/gid are dropped: the
// comparison needs the kind, the exact bytes, the exact link text and the modes
// an arbitrary reader would see.
export function inventory(root) {
  const entries = new Map();
  const walk = (directory, prefix, depth) => {
    if (depth > MAX_INVENTORY_DEPTH) fail('inventory-depth', `${directory} is deeper than ${MAX_INVENTORY_DEPTH}`);
    for (const name of fs.readdirSync(directory).sort()) {
      const location = path.join(directory, name);
      const relativePath = prefix === '' ? name : `${prefix}/${name}`;
      if (entries.size >= MAX_INVENTORY_ENTRIES) fail('inventory-size', `more than ${MAX_INVENTORY_ENTRIES} entries`);
      const info = fs.lstatSync(location);
      const mode = info.mode & 0o777;
      if (info.isSymbolicLink()) {
        entries.set(relativePath, {kind: 'symlink', mode, linkTarget: fs.readlinkSync(location)});
      } else if (info.isDirectory()) {
        entries.set(relativePath, {kind: 'dir', mode});
        walk(location, relativePath, depth + 1);
      } else if (info.isFile()) {
        if (info.size > MAX_FILE_BYTES) fail('inventory-size', `${relativePath} exceeds ${MAX_FILE_BYTES} bytes`);
        entries.set(relativePath, {kind: 'file', mode, size: info.size, bytes: hashFile(location)});
      } else {
        fail('special-file', `${relativePath} is not a regular file, directory or symlink`);
      }
    }
  };
  walk(root, '', 0);
  return entries;
}

export function hashFile(file) {
  const digest = crypto.createHash('sha256');
  const fd = fs.openSync(file, 'r');
  try {
    const buffer = Buffer.alloc(READ_CHUNK);
    for (;;) {
      const read = fs.readSync(fd, buffer, 0, buffer.length, null);
      if (read === 0) break;
      digest.update(buffer.subarray(0, read));
    }
  } finally {
    fs.closeSync(fd);
  }
  return digest.digest('hex');
}

// The generated package-root bin directory is the only thing pnpm adds inside a
// package root. It is normalised only when every entry in it is exactly a link
// this rule derives from an authenticated provider manifest.
export function binTargetsOf(manifest, label) {
  const declared = manifest.bin;
  const targets = new Map();
  if (declared === undefined || declared === null) return targets;
  if (typeof declared === 'string') {
    addBin(targets, manifest.name.slice(manifest.name.lastIndexOf('/') + 1), declared, label);
    return targets;
  }
  if (typeof declared !== 'object' || Array.isArray(declared)) fail('installed-bin', `${label} bin is neither a string nor an object`);
  for (const [name, target] of Object.entries(declared)) addBin(targets, name, target, label);
  return targets;
}

// A declared command target has to be a plain relative path inside the package:
// an absolute or dotted path would let a manifest point the byte proof (and the
// generated link) at a file the archive does not own.
function addBin(targets, name, target, label) {
  if (typeof name !== 'string' || name === '' || name.includes('/') || name.startsWith('-')) {
    fail('installed-bin', `${label} declares an invalid command name ${JSON.stringify(name)}`);
  }
  if (typeof target !== 'string' || target === '' || path.isAbsolute(target) || target.includes('\\')
      || target.split('/').some(part => part === '..' || part === '')) {
    fail('installed-bin', `${label} bin.${name} is not a normalised relative path: ${JSON.stringify(target)}`);
  }
  // A leading `./` is a spelling, not a location: npm and pnpm both collapse it
  // (`yaml@2.9.0` publishes `"./bin.mjs"`), so the derived target is the same file.
  // Every other dot segment was refused above.
  targets.set(name, target.split('/').filter(part => part !== '.').join('/'));
}

export {FAMILY_VOCABULARY, REVIEWED_FAMILY_SHAPES, VIRTUAL_STORE, nodeModulesPaths, readManifest, relative};
