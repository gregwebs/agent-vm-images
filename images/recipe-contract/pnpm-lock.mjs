// Data-only boundary: pins prove consistency, never approval of a new graph.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import YAML from "./vendor/yaml/dist/index.js";

/** @typedef {string & {readonly exactVersion: unique symbol}} ExactVersion */
/** @typedef {{name:string, version:ExactVersion}} PackageId */
/** @typedef {{opaque:true,key:string}} OpaquePeerContext */
/** @typedef {{opaque?:false,packageId:PackageId, peerContext:(SnapshotId|OpaquePeerContext)[], key:string}} SnapshotId */
/** @typedef {Omit<LockedProject,'selection'> & {selection:{kind:'dsh',version:ExactVersion},tentative:true}} TentativeDshProject */
/** @typedef {{mode:'equal'|'changed'|'refresh',beforeVersion:ExactVersion,afterVersion:ExactVersion,addedArchives:string[],removedArchives:string[],retainedArchives:string[],addedSnapshots:string[],removedSnapshots:string[],retainedSnapshots:string[]}} DshUpdateComparison */
/** @typedef {{exact:number,totalOptionalMissing:number,cordisExact:number,unsatisfied:0}} ExactPeerSummary */
/** @typedef {{selector:string,version:string,package:string,packageVersion:ExactVersion,integrity:string,peer:string,kind:'exact-peer'|'range-peer'|'dependency'}} CordisRequirement */
/** @typedef {{overrides:Record<string,ExactVersion>,requirements:CordisRequirement[],delta:{selector:string,before:ExactVersion|null,after:ExactVersion}[]}} DshCordisDerivation */
/** @typedef {{kind:'dsh'|'pi'|'bridge', version:ExactVersion}} Selection */
/** @typedef {{id:PackageId, integrity:string, tarball:string, peers:Record<string,string>, optionalPeers:string[]}} Archive */
/** @typedef {{name:string, target:string, optional:boolean}} Edge */
/** @typedef {{id:SnapshotId, edges:Edge[]}} Snapshot */
/** @typedef {{manifest:Record<string,unknown>,workspace:Record<string,unknown>,lock:Record<string,unknown>,packages:Archive[],snapshots:Snapshot[],importer:Edge[],prefix:string,selection:Selection}} LockedProject */

const MAX_BYTES = 16 * 1024 * 1024;
const MAX_DEPTH = 100;
const exact = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;
const registryName = /^(?:@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*$/;
const roots = { dsh: "@deepseek-ai/dsh", pi: "@earendil-works/pi-coding-agent", bridge: "pi-claude-bridge" };
const overrideNames = ["cordis", "cordis-plugin-loader", "cordis-plugin-timer", "cordis-plugin-include"].map(name => `@deepseek-ai/dsh>@deepseek-ai/${name}`);
export const pinnedInputs = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];
const pinFile = "pnpm-inputs.sha256";

/** @param {unknown} value @returns {Record<string,unknown>} */
function record(value) {
  assert.ok(value !== null && typeof value === "object" && !Array.isArray(value), "expected mapping");
  return /** @type {Record<string,unknown>} */ (value);
}
/** @param {unknown} value @returns {string} */
function text(value) { assert.equal(typeof value, "string", "expected string"); return /** @type {string} */ (value); }
/** @param {unknown} value @returns {ExactVersion} */
function version(value) {
  const str = text(value);
  assert.ok(exact.test(str) && !str.includes("\n"), `not canonical exact semver: ${str}`);
  return /** @type {ExactVersion} */ (str);
}
function name(value) {
  const str = text(value);
  assert.ok(registryName.test(str) && !str.includes("\n") && str.length <= 214, `not registry package name: ${str}`);
  return str;
}

/** Version syntax a peer range may use: partials, wildcards, comparators and
 * `||` alternatives. Anything outside it is a review stop, never a silent skip. */
const partial = "(?:0|[1-9][0-9]*|[xX*])(?:\\.(?:0|[1-9][0-9]*|[xX*])){0,2}(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\\+[0-9A-Za-z-]+(?:\\.[0-9A-Za-z-]+)*)?";
// `>= 4.11` (operator, space, partial) is a normal npm range, so a comparator
// may separate its operator from its version by whitespace.
const comparatorTerm = `(?:\\^|~|>=|<=|>|<|=)?\\s*${partial}`;
const comparatorSet = new RegExp(`^${comparatorTerm}(?:\\s+${comparatorTerm})*$`);
const hyphenRange = new RegExp(`^${partial}\\s+-\\s+${partial}$`);

/** @param {string} wanted @returns {string|null} the exact version it pins, or null */
function exactDeclaration(wanted) {
  // `=X`, `= X`, `vX` and surrounding whitespace all pin one version. The
  // alternative spelling is stripped and re-trimmed before the exact test, so a
  // spaced comparator form cannot fall through to the range grammar and escape
  // the exact-peer oracle.
  const pin = wanted.trim().replace(/^=\s*/, "").trim().replace(/^v/, "");
  return exact.test(pin) ? pin : null;
}

/** @param {string} wanted @returns {boolean} */
function rangeDeclaration(wanted) {
  const spec = wanted.trim();
  if (spec === "") return false;
  return spec.split(/\s*\|\|\s*/).every(alternative => {
    const trimmed = alternative.trim();
    return trimmed !== '' && (hyphenRange.test(trimmed) || comparatorSet.test(trimmed));
  });
}

/** @param {string} wanted @returns {'exact'|'range'} */
function peerSpecClass(wanted) {
  if (exactDeclaration(wanted) !== null) return "exact";
  assert.ok(rangeDeclaration(wanted), `unknown peer specification: ${wanted}`);
  return "range";
}
function keys(value, allowed, required = []) {
  for (const key of Object.keys(value)) assert.ok(allowed.includes(key), `unexpected key: ${key}`);
  for (const key of required) assert.ok(Object.hasOwn(value, key), `missing key: ${key}`);
}
function kindRoot(kind) {
  assert.ok(Object.hasOwn(roots, kind), "unknown consumer kind");
  return roots[kind];
}
function boundedBytes(file) {
  assert.ok(fs.lstatSync(file).isFile(), `${file}: must be a regular file`);
  const fd = fs.openSync(file, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const stat = fs.fstatSync(fd);
    assert.ok(stat.isFile() && stat.size <= MAX_BYTES, `${file}: must be a regular file <=16 MiB`);
    const data = Buffer.alloc(stat.size + 1);
    let length = 0;
    while (length < data.length) {
      const read = fs.readSync(fd,data,length,data.length-length,null);
      if (!read) break;
      length += read;
    }
    assert.ok(length <= stat.size, "input grew beyond size bound");
    return data.subarray(0,length);
  } finally { fs.closeSync(fd); }
}
function hasEntry(file) {
  try { fs.lstatSync(file); return true; }
  catch (error) { if (error.code === 'ENOENT') return false; throw error; }
}
function boundedRead(file) {
  return new TextDecoder("utf-8", { fatal: true }).decode(boundedBytes(file));
}
function parse(file, json = false) {
  const source = boundedRead(file);
  // JSON grammar and YAML AST duplicate detection are both required for manifests.
  if (json) JSON.parse(source);
  const doc = YAML.parseDocument(source, { uniqueKeys: true, merge: false, strict: true });
  assert.equal(doc.errors.length, 0, doc.errors.map(error => error.message).join("; "));
  assert.equal(doc.warnings.length, 0, doc.warnings.map(error => error.message).join("; "));
  function walk(node, depth) {
    assert.ok(depth <= MAX_DEPTH, "input exceeds depth100");
    if (!node) return;
    assert.ok(!YAML.isAlias(node) && !node.anchor && !node.tag, "anchors, aliases and explicit tags forbidden");
    if (YAML.isMap(node)) {
      for (const pair of node.items) {
        assert.ok(YAML.isScalar(pair.key) && typeof pair.key.value === "string", "mapping keys must be strings");
        assert.notEqual(pair.key.value, "<<", "merge key forbidden");
        walk(pair.key, depth + 1);
        walk(pair.value, depth + 1);
      }
    } else if (YAML.isSeq(node)) {
      for (const item of node.items) walk(item, depth + 1);
    }
  }
  walk(doc.contents, 0);
  return record(doc.toJS({ maxAliasCount: 0 }));
}

/** @param {string} key @returns {SnapshotId} */
function snapshotId(key) {
  let offset = 0;
  function parseId(depth) {
    assert.ok(depth <= MAX_DEPTH, "peer context too deep");
    const start = offset;
    const head = /^(\@[^/@()]+\/[^@()]+|[^@()/]+)@([^()]+)/.exec(key.slice(offset));
    assert.ok(head, `malformed snapshot: ${key}`);
    const packageId = { name: name(head[1]), version: version(head[2]) };
    offset += head[0].length;
    const peerContext = [];
    while (key[offset] === "(") {
      offset++;
      const opaque = /^([0-9a-f]{32})\)/.exec(key.slice(offset));
      if (opaque) {
        peerContext.push({ opaque: true, key: opaque[1] });
        offset += opaque[1].length;
      } else peerContext.push(parseId(depth + 1));
      assert.equal(key[offset++], ")", `malformed peer suffix: ${key}`);
    }
    assert.equal(new Set(peerContext.map(peer => peer.opaque ? `opaque:${peer.key}` : `package:${peer.packageId.name}`)).size, peerContext.length, `ambiguous peer context: ${key}`);
    return { packageId, peerContext, key: key.slice(start, offset) };
  }
  const result = parseId(0);
  assert.equal(offset, key.length, `malformed peer suffix: ${key}`);
  return result;
}
const archiveKey = id => `${id.name}@${id.version}`;
const sorted = values => values.sort((a, b) => a < b ? -1 : a > b ? 1 : 0);
const canonicalEdges = edges => edges.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : Number(a.optional) - Number(b.optional));

function validateWorkspace(workspace, kind, tentative = false, report = false) {
  const common = {
    packages: ["."], autoInstallPeers: kind !== "bridge", strictPeerDependencies: false,
    enableGlobalVirtualStore: false, pmOnFail: "error", sideEffectsCache: false,
    allowBuilds: {}, extendNodePath: false,
  };
  const policy = kind === "dsh" ? {
    nodeLinker: "isolated", hoist: true, trustPolicyExclude: ["chokidar@4.0.3"],
  } : kind === "pi" ? {
    nodeLinker: "isolated", hoist: false, preferSymlinkedExecutables: true,
    trustPolicyExclude: ["undici-types@6.21.0"],
  } : { nodeLinker: "hoisted", ignoredOptionalDependencies: ["@anthropic-ai/claude-agent-sdk-*"] };
  keys(workspace, [...Object.keys(common), ...Object.keys(policy), ...(kind === "dsh" ? ["overrides"] : [])], [...Object.keys(common), ...Object.keys(policy)]);
  for (const [key, value] of Object.entries({ ...common, ...policy })) assert.deepEqual(workspace[key], value, `workspace policy: ${key}`);
  if (kind === "dsh" && tentative) {
    assert.equal(workspace.overrides, undefined, "tentative derivation must remove old overrides");
  } else if (kind === "dsh" && !(report && workspace.overrides === undefined)) {
    // A report may describe a graph with no overrides (the negative leg's own
    // re-resolution), so the map is validated whenever it is present.
    const overrides = record(workspace.overrides);
    keys(overrides, overrideNames, overrideNames);
    for (const value of Object.values(overrides)) version(value);
  }
}
function verifyPins(prefix) {
  const entries = new Map();
  for (const line of boundedRead(path.join(prefix, pinFile)).split("\n")) {
    if (line === "") continue;
    const match = /^([a-f0-9]{64}) {2}([a-z.-]+)$/.exec(line);
    assert.ok(match, "malformed input pin");
    assert.ok(!entries.has(match[2]), "duplicate input pin");
    entries.set(match[2], match[1]);
  }
  assert.deepEqual(sorted([...entries.keys()]), sorted([...pinnedInputs]), "pins must cover exactly three input files");
  for (const file of pinnedInputs) assert.equal(digest(prefix, file), entries.get(file), `${file}: bytes differ from reviewed pin`);
}
function digest(prefix, file) { return crypto.createHash("sha256").update(boundedBytes(path.join(prefix, file))).digest("hex"); }

/** @param {{prefix:string,kind:'dsh'|'pi'|'bridge',verifyPins?:boolean}} options @returns {LockedProject} */
export function readProject({ prefix, kind, verifyPins: checkPins = true }) {
  return readProjectInputs({prefix, kind, checkPins, tentative: false});
}

// This capability is private to derivation; ordinary validation can never skip peers.
function readProjectInputs({prefix, kind, checkPins, tentative, report = false}) {
  const root = kindRoot(kind);
  if (checkPins) verifyPins(prefix);
  for (const file of [".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs", "package-lock.json"])
    assert.ok(!hasEntry(path.join(prefix, file)), `unreviewed input: ${file}`);
  const manifest = parse(path.join(prefix, "package.json"), true);
  keys(manifest, ["name", "version", "private", "type", "description", "license", "packageManager", "dependencies"], ["name", "private", "packageManager", "dependencies"]);
  name(manifest.name);
  if (manifest.version !== undefined) version(manifest.version);
  if (manifest.type !== undefined) assert.equal(manifest.type, "module");
  for (const field of ["description", "license"]) if (manifest[field] !== undefined) text(manifest[field]);
  assert.equal(manifest.private, true);
  assert.equal(manifest.packageManager, "pnpm@11.11.0");
  const dependencies = record(manifest.dependencies);
  keys(dependencies, [root], [root]);
  const selection = { kind, version: version(dependencies[root]) };
  const workspace = parse(path.join(prefix, "pnpm-workspace.yaml"));
  validateWorkspace(workspace, kind, tentative, report);
  const lock = parse(path.join(prefix, "pnpm-lock.yaml"));
  keys(lock, ["lockfileVersion", "settings", "overrides", "ignoredOptionalDependencies", "importers", "packages", "snapshots"], ["lockfileVersion", "settings", "importers", "packages", "snapshots"]);
  assert.equal(String(lock.lockfileVersion), "9.0", "requires v9 lock");
  const settings = record(lock.settings);
  keys(settings, ["autoInstallPeers", "excludeLinksFromLockfile"], ["autoInstallPeers", "excludeLinksFromLockfile"]);
  assert.equal(settings.autoInstallPeers, workspace.autoInstallPeers);
  assert.equal(settings.excludeLinksFromLockfile, false);
  if (kind === "dsh") assert.deepEqual(lock.overrides, workspace.overrides, "lock overrides differ");
  else assert.equal(lock.overrides, undefined);
  if (lock.ignoredOptionalDependencies !== undefined) assert.deepEqual(lock.ignoredOptionalDependencies, workspace.ignoredOptionalDependencies);
  const importers = record(lock.importers);
  keys(importers, ["."], ["."]);
  const importer = record(importers["."]);
  keys(importer, ["dependencies"], ["dependencies"]);
  const imported = record(importer.dependencies);
  keys(imported, [root], [root]);
  const resolved = record(imported[root]);
  keys(resolved, ["specifier", "version"], ["specifier", "version"]);
  assert.equal(resolved.specifier, selection.version, "importer specifier mismatch");
  const rootId = snapshotId(`${root}@${text(resolved.version)}`);
  assert.equal(rootId.packageId.version, selection.version, "importer resolution mismatch");
  const archives = record(lock.packages);
  const packages = sorted(Object.keys(archives)).map(key => {
    const id = snapshotId(key);
    assert.equal(id.peerContext.length, 0, "archive cannot have peer context");
    const pkg = record(archives[key]);
    keys(pkg, ["resolution", "engines", "cpu", "os", "libc", "deprecated", "hasBin", "peerDependencies", "peerDependenciesMeta", "optional", "bundledDependencies"], ["resolution"]);
    const resolution = record(pkg.resolution);
    keys(resolution, ["integrity", "tarball"], ["integrity"]);
    const integrity = text(resolution.integrity);
    assert.ok(/^sha512-[A-Za-z0-9+/]{86}==$/.test(integrity) && Buffer.from(integrity.slice(7), "base64").length === 64 && Buffer.from(integrity.slice(7), "base64").toString("base64") === integrity.slice(7), `${key}: full sha512 required`);
    const tarball = `https://registry.npmjs.org/${id.packageId.name}/-/${id.packageId.name.split("/").at(-1)}-${id.packageId.version}.tgz`;
    if (resolution.tarball !== undefined) assert.equal(resolution.tarball, tarball, "only canonical npmjs tarball allowed");
    const peers = Object.create(null);
    const declaredPeers = record(pkg.peerDependencies ?? {});
    for (const peer of sorted(Object.keys(declaredPeers))) {
      const wanted = text(declaredPeers[peer]);
      assert.ok(wanted.length > 0 && !/[:/@\\\\\r\n]/.test(wanted), `${key}: non-registry peer specification`);
      peers[name(peer)] = wanted;
    }
    const optionalPeers = [];
    for (const [peer, meta] of Object.entries(record(pkg.peerDependenciesMeta ?? {}))) {
      name(peer); assert.ok(Object.hasOwn(peers, peer), "peer metadata without declaration");
      keys(record(meta), ["optional"], ["optional"]);
      assert.equal(meta.optional, true);
      optionalPeers.push(peer);
    }
    return { id: id.packageId, integrity, tarball, peers, optionalPeers: sorted(optionalPeers) };
  });
  const snapshots = sorted(Object.keys(record(lock.snapshots))).map(key => {
    const id = snapshotId(key);
    assert.ok(Object.hasOwn(archives, archiveKey(id.packageId)), `${key}: missing archive`);
    const entry = record(lock.snapshots[key]);
    keys(entry, ["dependencies", "optionalDependencies", "transitivePeerDependencies", "optional"]);
    if (entry.optional !== undefined) assert.equal(typeof entry.optional, "boolean");
    if (entry.transitivePeerDependencies !== undefined) {
      assert.ok(Array.isArray(entry.transitivePeerDependencies));
      entry.transitivePeerDependencies.forEach(name);
    }
    const edges = [];
    const names = new Set();
    for (const [field, optional] of [["dependencies", false], ["optionalDependencies", true]]) {
      for (const [dependency, target] of Object.entries(record(entry[field] ?? {}))) {
        name(dependency);
        assert.ok(!names.has(dependency), `${key}: ambiguous dependency ${dependency}`);
        names.add(dependency);
        const targetId = snapshotId(`${dependency}@${text(target)}`);
        edges.push({ name: dependency, target: targetId.key, optional });
      }
    }
    return { id, edges: canonicalEdges(edges) };
  });
  const project = { manifest, workspace, lock, packages, snapshots, importer: [{ name: root, target: rootId.key, optional: false }], prefix, selection };
  if (tentative) {
    assert.equal(kind, "dsh");
    validateGraph(project, selection);
    validateContextEdges(project);
    return { ...project, tentative: true };
  }
  if (report) {
    // Report-only reader: structure and context edges are still enforced, so a
    // peer report always describes a well-formed graph; only the exact-peer
    // assertion is left to the caller. Reached solely by this module's `peers`
    // verb, never by a check, pin, install or publication path.
    validateGraph(project, selection);
    validateContextEdges(project);
    return project;
  }
  validateProject(project, selection);
  return project;
}

/** @param {LockedProject} project @param {Selection} selection */
export function validateProject(project, selection) {
  assert.ok(!project.tentative, "tentative graph is not a locked project");
  validateGraph(project, selection);
  if (selection.kind === "dsh") checkExactPeers(project);
  validateContextEdges(project);
}

function resolveContext(context, snapshots, source) {
  if (context.opaque) return undefined;
  if (snapshots.has(context.key)) return snapshots.get(context.key);
  const matches = [...snapshots.values()].filter(snapshot => archiveKey(snapshot.id.packageId) === context.key);
  assert.ok(matches.length <= 1, `${source}: ambiguous peer context ${context.key}`);
  assert.equal(matches.length, 1, `${source}: dangling peer context ${context.key}`);
  return matches[0];
}

function validateContextEdges(project) {
  const snapshots = new Map(project.snapshots.map(snapshot => [snapshot.id.key,snapshot]));
  for (const owner of project.snapshots) for (const context of owner.id.peerContext) {
    if (context.opaque) continue;
    const provider = resolveContext(context,snapshots,owner.id.key);
    const edge = owner.edges.find(edge => edge.name === context.packageId.name);
    if (edge) assert.equal(provider.id.key,edge.target,`${owner.id.key}: ambiguous context ${context.key}`);
  }
}

function validateGraph(project, selection) {
  assert.deepEqual(selection, project.selection, "selection differs from manifest");
  const snapshots = new Map(project.snapshots.map(snapshot => [snapshot.id.key, snapshot]));
  for (const snapshot of project.snapshots) {
    const pendingContexts = snapshot.id.peerContext.map(context => ({context, owner:snapshot}));
    while (pendingContexts.length) {
      const {context, owner} = pendingContexts.pop();
      const provider = resolveContext(context, snapshots, owner.id.key);
      if (context.opaque) continue;
      pendingContexts.push(...context.peerContext.map(nested => ({context:nested, owner:provider})));
    }
  }
  const seen = new Set();
  const pending = [...project.importer];
  while (pending.length) {
    const edge = pending.pop();
    const snapshot = snapshots.get(edge.target);
    assert.ok(snapshot, `dangling snapshot edge: ${edge.target}`);
    if (seen.has(edge.target)) continue;
    seen.add(edge.target);
    pending.push(...snapshot.edges);
  }
  assert.equal(seen.size, snapshots.size, "orphan snapshot");
  const archiveIds = new Set(project.snapshots.map(snapshot => archiveKey(snapshot.id.packageId)));
  assert.equal(archiveIds.size, project.packages.length, "orphan archive");
  if (selection.kind === "dsh") {
    assert.ok(project.packages.every(pkg => pkg.id.name !== "pnpm") && project.snapshots.every(snapshot => snapshot.id.packageId.name !== "pnpm"), "dsh cannot own pnpm");
    assert.ok(project.snapshots.some(snapshot => snapshot.id.packageId.name === "@deepseek-ai/dsh-sandbox-local" && seen.has(snapshot.id.key)), "dsh sandbox must be reachable");
  }
}

/** Peer satisfaction without throwing, so a caller can report the numbers and
 * decide for itself. Each entry names the declaration it could not satisfy.
 * @param {LockedProject} project
 * @returns {{exact:number,totalOptionalMissing:number,cordisExact:number,unsatisfied:string[]}} */
export function exactPeerReport(project) {
  const archives = new Map(project.packages.map(pkg => [archiveKey(pkg.id), pkg]));
  let cordisExactPeers = 0;
  let exactPeers = 0;
  let optionalMissing = 0;
  const unsatisfied = [];
  const snapshots = new Map(project.snapshots.map(snapshot => [snapshot.id.key, snapshot]));
  for (const snapshot of project.snapshots) {
    const pkg = archives.get(archiveKey(snapshot.id.packageId));
    for (const [peer, wanted] of Object.entries(pkg.peers)) {
      const pinned = exactDeclaration(wanted);
      // A declared range is a compatibility constraint, not an exact pin; any
      // syntax outside the range grammar could hide an exact pin, so it stops here.
      if (peerSpecClass(wanted) === "range") continue;
      exactPeers++;
      if (peer === "@deepseek-ai/cordis") cordisExactPeers++;
      const edge = snapshot.edges.find(edge => edge.name === peer);
      const context = snapshot.id.peerContext.find(id => !id.opaque && id.packageId.name === peer);
      const got = edge ? snapshotId(edge.target).packageId : undefined;
      const diagnostic = `${snapshot.id.key}: peer ${peer} wanted ${wanted}, got ${got ? archiveKey(got) : "missing"}`;
      if (!got) {
        if (pkg.optionalPeers.includes(peer) && !context) optionalMissing++;
        else unsatisfied.push(diagnostic);
        continue;
      }
      if (!(got.name === peer && got.version === pinned)) unsatisfied.push(diagnostic);
      else if (context && resolveContext(context, snapshots, snapshot.id.key).id.key !== edge.target) {
        unsatisfied.push(`${diagnostic}; ambiguous context`);
      }
    }
  }
  return {exact: exactPeers, totalOptionalMissing: optionalMissing, cordisExact: cordisExactPeers, unsatisfied};
}

/** @param {LockedProject} project @returns {ExactPeerSummary} */
export function checkExactPeers(project) {
  const report = exactPeerReport(project);
  assert.equal(report.unsatisfied.length, 0, report.unsatisfied.join("; "));
  assert.ok(report.cordisExact > 0, "dsh must cover declared exact Cordis peers");
  return {exact: report.exact, totalOptionalMissing: report.totalOptionalMissing,
          cordisExact: report.cordisExact, unsatisfied: 0};
}

/** @param {{prefix:string,kind:'dsh'|'pi'|'bridge'}} options */
export function writeInputPins({ prefix, kind }) {
  readProject({ prefix, kind, verifyPins: false });
  const body = pinnedInputs.map(file => `${digest(prefix, file)}  ${file}\n`).join("");
  const destination = path.join(prefix, pinFile);
  let previous;
  try { previous = fs.lstatSync(destination); }
  catch (error) { if (error.code !== "ENOENT") throw error; }
  if (previous) assert.ok(previous.isFile(), "pin destination must be a regular file");
  // Replacing, rather than truncating, cannot write through an existing hardlink.
  const temporary = path.join(prefix, `.pnpm-pins-${crypto.randomUUID()}`);
  try {
    fs.writeFileSync(temporary, body, { flag: "wx", mode: 0o644 });
    fs.renameSync(temporary, destination);
  } finally {
    if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
  }
}


/**
 * @param {{before:LockedProject,after:LockedProject,requestedVersion:string,refresh?:boolean}} options
 * @returns {DshUpdateComparison}
 */
export function compareDshUpdate({before, after, requestedVersion, refresh = false}) {
  assert.equal(typeof refresh, 'boolean');
  for (const project of [before, after]) {
    assert.equal(project.selection.kind, 'dsh', 'comparison requires dsh projects');
    assert.deepEqual(project, readProject({prefix:project.prefix, kind:'dsh'}), 'project differs from validated inputs');
  }
  const requested = requestedVersion === '' ? before.selection.version : version(requestedVersion);
  assert.equal(after.selection.version, requested, 'after selection differs from request');
  const changed = before.selection.version !== requested;
  const manifest = project => {
    const value = structuredClone(project.manifest);
    if (changed) value.dependencies[roots.dsh] = '<selected>';
    return value;
  };
  assert.deepEqual(manifest(after), manifest(before), 'only dsh manifest selection may change');
  assert.ok(boundedBytes(path.join(before.prefix, 'pnpm-workspace.yaml')).equals(boundedBytes(path.join(after.prefix, 'pnpm-workspace.yaml'))), 'workspace bytes changed');
  const header = project => {
    const value = structuredClone(project.lock);
    delete value.packages; delete value.snapshots;
    if (changed || refresh) value.importers['.'].dependencies[roots.dsh] = {specifier:'<selected>', version:'<selected>'};
    return value;
  };
  assert.deepEqual(header(after), header(before), 'lock settings/overrides/importer changed');
  if (!changed && !refresh) {
    for (const file of [...pinnedInputs, pinFile]) assert.ok(boundedBytes(path.join(before.prefix, file)).equals(boundedBytes(path.join(after.prefix, file))), `${file}: equal selection requires identical bytes`);
  }
  function closure(project) {
    const snapshots = new Map(project.snapshots.map(snapshot => [snapshot.id.key, snapshot]));
    const visited = new Set();
    const pending = project.importer.map(edge => edge.target);
    while (pending.length) {
      const key = pending.pop();
      if (visited.has(key)) continue;
      const snapshot = snapshots.get(key);
      assert.ok(snapshot, `dangling closure: ${key}`);
      visited.add(key);
      pending.push(...snapshot.edges.map(edge => edge.target));
    }
    return {snapshots:visited, archives:new Set([...visited].map(key => archiveKey(snapshots.get(key).id.packageId)))};
  }
  const oldClosure = closure(before), newClosure = closure(after);
  const result = {mode: refresh ? 'refresh' : changed ? 'changed' : 'equal', beforeVersion:before.selection.version, afterVersion:after.selection.version};
  for (const [field, label] of [['packages','Archives'], ['snapshots','Snapshots']]) {
    const oldRecords = before.lock[field], newRecords = after.lock[field];
    const oldKeys = Object.keys(oldRecords), newKeys = Object.keys(newRecords);
    result[`added${label}`] = sorted(newKeys.filter(key => !Object.hasOwn(oldRecords,key)));
    result[`removed${label}`] = sorted(oldKeys.filter(key => !Object.hasOwn(newRecords,key)));
    result[`retained${label}`] = sorted(oldKeys.filter(key => Object.hasOwn(newRecords,key)));
    for (const key of result[`added${label}`]) assert.ok(newClosure[label.toLowerCase()].has(key), `addition outside changed root closure: ${key}`);
    for (const key of result[`removed${label}`]) assert.ok(oldClosure[label.toLowerCase()].has(key), `removal outside changed root closure: ${key}`);
    for (const key of result[`retained${label}`]) {
      // Refresh may re-select transitive snapshots, never substitute a retained
      // archive: npm versions are immutable, so a different SRI/tarball/metadata
      // for the same name@version is a different artifact, not a refresh.
      if (refresh && field === "snapshots") continue;
      const oldRecord = structuredClone(oldRecords[key]), newRecord = structuredClone(newRecords[key]);
      if (field === 'snapshots' && changed && key === before.importer[0].target && key === after.importer[0].target) {
        delete oldRecord.dependencies; delete oldRecord.optionalDependencies;
        delete newRecord.dependencies; delete newRecord.optionalDependencies;
      }
      assert.deepEqual(newRecord, oldRecord, `retained ${field === 'packages' ? 'archive' : 'snapshot'} changed: ${key}`);
    }
  }
  return result;
}

// Deliberately conservative: unprovable syntax is a review stop, not a permissive
// fallback or a reason to choose another version. Cordis families use stable pins.
function satisfiesFamilyRange(selected, wanted) {
  const pin = exactDeclaration(wanted);
  if (pin !== null) return selected === pin;
  assert.ok(!selected.includes('-') && !selected.includes('+'), `unprovable family range: ${wanted}`);
  const parts = selected.split('.').map(Number);
  assert.ok(parts.every(Number.isSafeInteger), `unprovable family range: ${wanted}`);
  const cmp = other => {
    for (let i = 0; i < 3; i++) if (parts[i] !== other[i]) return parts[i] < other[i] ? -1 : 1;
    return 0;
  };
  return wanted.split(/\s*\|\|\s*/).map(alternative => {
    const terms = alternative.trim().split(/\s+/);
    return terms.map(term => {
      const match = /^(\^|~|>=|<=|>|<|=)?((?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*))$/.exec(term);
      assert.ok(match, `unprovable family range: ${wanted}`);
      const lower = match[2].split('.').map(Number);
      assert.ok(lower.every(Number.isSafeInteger), `unprovable family range: ${wanted}`);
      const comparison = cmp(lower);
      switch (match[1] ?? '=') {
        case '=': return comparison === 0;
        case '>=': return comparison >= 0;
        case '<=': return comparison <= 0;
        case '>': return comparison > 0;
        case '<': return comparison < 0;
        case '~':
          assert.ok(Number.isSafeInteger(lower[1]+1), `unprovable family range: ${wanted}`);
          return comparison >= 0 && cmp([lower[0],lower[1]+1,0]) < 0;
        case '^': {
          const upper = lower[0] ? [lower[0]+1,0,0] : lower[1] ? [0,lower[1]+1,0] : [0,0,lower[2]+1];
          assert.ok(upper.every(Number.isSafeInteger), `unprovable family range: ${wanted}`);
          return comparison >= 0 && cmp(upper) < 0;
        }
      }
    }).every(Boolean);
  }).some(Boolean);
}

/** A committed override map, validated like the on-disk proposal format.
 * @param {unknown} value @returns {Record<string, ExactVersion>} */
function overrideMap(value) {
  if (value === undefined) return {};
  const map = record(value);
  keys(map, overrideNames, overrideNames);
  for (const wanted of Object.values(map)) version(wanted);
  return /** @type {Record<string, ExactVersion>} */ (map);
}

/**
 * @param {{prefix:string, committed?:Record<string,string>, approved?:Record<string,string>}} options
 *   `committed` is the override map of the project being upgraded (the no-op
 *   re-pin of an unchanged upgrade). `approved` is the map a reviewer explicitly
 *   supplied with `--cordis-overrides`; it outranks `committed` because it is the
 *   only thing that can decide a graph the lockfile cannot, and it must name one
 *   of the surviving candidates.
 * @returns {DshCordisDerivation}
 */
export function deriveDshCordisOverrides({prefix, committed, approved}) {
  /** @type {TentativeDshProject} */
  const project = readProjectInputs({prefix, kind:'dsh', checkPins:false, tentative:true});
  const overrides = {}, requirements = [];
  const archives = new Map(project.packages.map(pkg => [archiveKey(pkg.id),pkg]));
  // Every permitted family, with every declaration the tentative graph carries:
  // peer specifications (exact or range, with their file source) and the
  // dependency edges that make the family's selected instances reachable.
  const families = sorted([...overrideNames]).map(selector => {
    const peer = selector.split('>')[1];
    const selections = project.packages.filter(pkg => pkg.id.name === peer);
    const peerRows = project.packages.filter(pkg => Object.hasOwn(pkg.peers,peer)).map(pkg => ({
      selector,version:pkg.peers[peer],package:pkg.id.name,packageVersion:pkg.id.version,
      integrity:pkg.integrity,peer,kind:peerSpecClass(pkg.peers[peer]) === 'exact' ? 'exact-peer' : 'range-peer',
    }));
    const dependencyRows = [];
    for (const snapshot of project.snapshots) for (const edge of snapshot.edges) {
      if (edge.name !== peer) continue;
      const pkg = archives.get(archiveKey(snapshot.id.packageId));
      dependencyRows.push({selector,version:snapshotId(edge.target).packageId.version,package:pkg.id.name,packageVersion:pkg.id.version,integrity:pkg.integrity,peer,kind:'dependency'});
    }
    // A dependency edge records that this family's instance is reachable; it is
    // never an exact declaration. A lockfile carries no dependency specifier, so
    // the number of distinct dependency targets proves nothing about the range
    // that produced them and must not be read as an exact requirement.
    return {peer,selector,selections,declarations:[...peerRows, ...dependencyRows]};
  });
  const proof = family => JSON.stringify(family.declarations);
  const reviewed = overrideMap(committed);
  const decided = overrideMap(approved);
  for (const family of families) {
    assert.ok(family.selections.length > 0, `missing family: ${family.peer}; requirements: ${proof(family)}`);
    assert.ok(family.declarations.some(row => row.kind !== 'dependency'), `missing family peer requirements: ${family.peer}`);
  }
  // A declaration constrains the map only while its own package is still in the
  // graph: an override pins one version per family, so an instance of a family at
  // any other version is pruned together with its peer declarations. Each family's
  // domains are therefore solved together instead of one family at a time.
  const familyOf = new Map(families.map(family => [family.peer,family]));
  const constraints = new Map(families.map(family => [family.peer,[]]));
  for (const pkg of project.packages) for (const [peer,wanted] of Object.entries(pkg.peers)) {
    if (!constraints.has(peer)) continue;
    const owner = familyOf.get(pkg.id.name);
    constraints.get(peer).push({owner:owner ? owner.peer : null,version:pkg.id.version,wanted,source:`${pkg.id.name}@${pkg.id.version}`});
  }
  const problems = (assignment,family,value) => constraints.get(family.peer)
    .filter(row => row.owner === null || assignment[row.owner] === row.version)
    .map(row => {
      try { return satisfiesFamilyRange(value,row.wanted) ? null : `${row.source} wants ${row.wanted}`; }
      catch (error) { return error.message; }
    }).filter(problem => problem !== null);
  const domains = families.map(family => sorted([...new Set(family.selections.map(pkg => pkg.id.version))]));
  const solutions = [];
  const solve = (index,assignment) => {
    if (index === families.length) {
      const inconsistent = families.some(family => problems(assignment,family,assignment[family.peer]).length > 0);
      if (!inconsistent) solutions.push({...assignment});
      return;
    }
    for (const value of domains[index]) solve(index + 1,{...assignment,[families[index].peer]:value});
  };
  solve(0,{});
  if (solutions.length === 0) {
    // Nothing the tentative graph selected can satisfy every declaration. The
    // unmet declarations are named for the most constrained family, with the
    // pruned-instance declarations excluded so the report names what really binds.
    const reports = families.map(family => {
      const rejected = domains[families.indexOf(family)].map(value => {
        const unmet = problems({},family,value);
        return {value,text:`${value} (${unmet.join('; ') || 'no declared requirement'})`,met:unmet.length === 0};
      });
      return {family,rejected};
    });
    const report = reports.find(row => row.rejected.every(value => !value.met))
      ?? reports.sort((a,b) => a.rejected.filter(row => row.met).length - b.rejected.filter(row => row.met).length)[0];
    throw new Error(`no family selection satisfies the declared requirements: ${report.family.peer}; `
      + `rejected: ${report.rejected.map(row => row.text).join(', ')}; requirements: ${proof(report.family)}`);
  }
  const derived = new Map();
  for (const family of families) {
    const {peer,selector} = family;
    // A family's candidates are the values the tentative graph itself selected in
    // some assignment that satisfies every live declaration. Nothing outside the
    // graph may invent a candidate.
    const candidates = sorted([...new Set(solutions.map(solution => solution[peer]))]);
    if (candidates.length > 1) {
      // Two selections can satisfy every declaration (a root range selecting a
      // newer instance beside an exact peer's older one). A lockfile records no
      // dependency specifier, so the graph cannot choose between them: only a
      // reviewed value, while it is one of the survivors, can decide. An
      // explicitly approved map is that decision (it is passed in, so the review
      // route never re-enters the stop it comes from); the committed map is the
      // legitimate no-op re-pin of an unchanged upgrade. Without an approved map
      // the stop names the route that can decide it.
      const hasApproved = decided[selector] !== undefined;
      const keep = hasApproved ? decided[selector] : reviewed[selector];
      if (keep === undefined || !candidates.includes(keep)) {
        const why = keep === undefined ? ''
          : `${hasApproved ? 'reviewed' : 'committed'} value ${keep} is not one of the candidates; `;
        throw new Error(`ambiguous family selection: ${peer}; candidates: ${candidates.join(', ')}; `
          + `${why}requirements: ${proof(family)}`
          + (hasApproved ? '' : '; pass --cordis-overrides after review'));
      }
      candidates.length = 0;
      candidates.push(keep);
    }
    derived.set(peer,candidates[0]);
    overrides[selector] = candidates[0];
  }
  // Invariant: the chosen map is one of the assignments the graph supports.
  const chosen = Object.fromEntries(families.map(family => [family.peer,derived.get(family.peer)]));
  const conflicts = families.flatMap(family => problems(chosen,family,chosen[family.peer]).map(problem => `${family.peer}: ${problem}`));
  assert.equal(conflicts.length,0,`family selection conflict: ${conflicts.join('; ')}`);
  for (const family of families) requirements.push(...family.declarations);
  const unique = [...new Map(requirements.map(row => [JSON.stringify(row),row])).values()];
  unique.sort((a,b) => JSON.stringify(a) < JSON.stringify(b) ? -1 : JSON.stringify(a) > JSON.stringify(b) ? 1 : 0);
  return {overrides,requirements:unique,delta:Object.entries(overrides).map(([selector,after]) => ({selector,before:null,after}))};
}

/** Strict map syntax shares the bounded duplicate-rejecting parser, not JSON.parse alone. */
export function readDshCordisOverrideMap(file) {
  const map = parse(file,true);
  keys(map,overrideNames,overrideNames);
  for (const value of Object.values(map)) version(value);
  return Object.fromEntries(sorted(Object.keys(map)).map(key => [key,map[key]]));
}

export function graph(project) {
  return { selection: project.selection, packages: project.packages, snapshots: project.snapshots, importer: project.importer };
}

// Compare canonical paths: Node resolves the real file for import.meta.url, so a
// lexical comparison against a symlinked argv[1] silently skipped dispatch.
const entrypoint = (() => {
  if (!process.argv[1]) return false;
  try { return import.meta.url === pathToFileURL(fs.realpathSync(process.argv[1])).href; }
  catch { return false; }
})();
if (entrypoint) {
  const [verb, kind, prefix, ...extra] = process.argv.slice(2);
  if (!["check", "graph", "pin", "peers"].includes(verb) || !Object.hasOwn(roots, kind) || !prefix || extra.length) {
    console.error("usage: pnpm-lock.mjs check|graph|pin|peers dsh|pi|bridge PREFIX");
    process.exitCode = 2;
  } else {
    try {
      if (verb === "pin") writeInputPins({ prefix, kind });
      else if (verb === "peers") {
        // Report-only. It reuses derivation's peer-assertion-free reader so a
        // negative leg can state both numbers instead of guessing from a
        // diagnostic; nothing is pinned, installed or published, and every
        // publication path keeps the asserting reader.
        assert.equal(kind, "dsh", "peer reporting is dsh-only");
        console.log(JSON.stringify(exactPeerReport(readProjectInputs({ prefix, kind, checkPins: false, report: true }))));
      } else {
        const project = readProject({ prefix, kind });
        console.log(verb === "check" ? project.selection.version : JSON.stringify(graph(project)));
      }
    } catch (error) {
      console.error(`pnpm-lock: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
