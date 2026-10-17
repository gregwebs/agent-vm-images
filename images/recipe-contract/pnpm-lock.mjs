// Data-only boundary: pins prove consistency, never approval of a new graph.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import YAML from "./vendor/yaml/dist/index.js";

/** @typedef {string & {readonly exactVersion: unique symbol}} ExactVersion */
/** @typedef {{name:string, version:ExactVersion}} PackageId */
/** @typedef {{packageId:PackageId, peerContext:SnapshotId[], key:string}} SnapshotId */
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
function keys(value, allowed, required = []) {
  for (const key of Object.keys(value)) assert.ok(allowed.includes(key), `unexpected key: ${key}`);
  for (const key of required) assert.ok(Object.hasOwn(value, key), `missing key: ${key}`);
}
function kindRoot(kind) {
  assert.ok(Object.hasOwn(roots, kind), "unknown consumer kind");
  return roots[kind];
}
function boundedBytes(file) {
  const stat = fs.lstatSync(file);
  assert.ok(stat.isFile() && stat.size <= MAX_BYTES, `${file}: must be a regular file <=16 MiB`);
  const data = fs.readFileSync(file);
  assert.ok(data.length <= MAX_BYTES, "input grew beyond size bound");
  return data;
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
      peerContext.push(parseId(depth + 1));
      assert.equal(key[offset++], ")", `malformed peer suffix: ${key}`);
    }
    assert.equal(new Set(peerContext.map(peer => peer.packageId.name)).size, peerContext.length, `ambiguous peer context: ${key}`);
    return { packageId, peerContext, key: key.slice(start, offset) };
  }
  const result = parseId(0);
  assert.equal(offset, key.length, `malformed peer suffix: ${key}`);
  return result;
}
const archiveKey = id => `${id.name}@${id.version}`;
const sorted = values => values.sort((a, b) => a < b ? -1 : a > b ? 1 : 0);
const canonicalEdges = edges => edges.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : Number(a.optional) - Number(b.optional));

function validateWorkspace(workspace, kind) {
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
  if (kind === "dsh") {
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
  const root = kindRoot(kind);
  if (checkPins) verifyPins(prefix);
  for (const file of [".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs", "package-lock.json"])
    assert.ok(!fs.existsSync(path.join(prefix, file)), `unreviewed input: ${file}`);
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
  validateWorkspace(workspace, kind);
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
  validateProject(project, selection);
  return project;
}

/** @param {LockedProject} project @param {Selection} selection */
export function validateProject(project, selection) {
  assert.deepEqual(selection, project.selection, "selection differs from manifest");
  const snapshots = new Map(project.snapshots.map(snapshot => [snapshot.id.key, snapshot]));
  for (const snapshot of project.snapshots) {
    const pendingContexts = [...snapshot.id.peerContext];
    while (pendingContexts.length) {
      const context = pendingContexts.pop();
      assert.ok(snapshots.has(context.key), `${snapshot.id.key}: dangling peer context ${context.key}`);
      pendingContexts.push(...context.peerContext);
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
  if (selection.kind === "dsh") checkExactPeers(project);
}

/** @param {LockedProject} project */
export function checkExactPeers(project) {
  const archives = new Map(project.packages.map(pkg => [archiveKey(pkg.id), pkg]));
  let cordisExactPeers = 0;
  for (const snapshot of project.snapshots) {
    const pkg = archives.get(archiveKey(snapshot.id.packageId));
    for (const [peer, wanted] of Object.entries(pkg.peers)) {
      if (!exact.test(wanted)) continue; // Deliberately not a general semver solver.
      if (peer === "@deepseek-ai/cordis") cordisExactPeers++;
      const edge = snapshot.edges.find(edge => edge.name === peer);
      const context = snapshot.id.peerContext.find(id => id.packageId.name === peer);
      const got = edge ? snapshotId(edge.target).packageId : undefined;
      const diagnostic = `${snapshot.id.key}: peer ${peer} wanted ${wanted}, got ${got ? archiveKey(got) : "missing"}`;
      if (!got) {
        assert.ok(pkg.optionalPeers.includes(peer) && !context, diagnostic);
        continue;
      }
      assert.ok(got.name === peer && got.version === wanted, diagnostic);
      if (context) assert.equal(context.key, edge.target, `${diagnostic}; ambiguous context`);
    }
  }
  assert.ok(cordisExactPeers > 0, "dsh must cover declared exact Cordis peers");
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

export function graph(project) {
  return { selection: project.selection, packages: project.packages, snapshots: project.snapshots, importer: project.importer };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const [verb, kind, prefix, ...extra] = process.argv.slice(2);
  if (!["check", "graph", "pin"].includes(verb) || !Object.hasOwn(roots, kind) || !prefix || extra.length) {
    console.error("usage: pnpm-lock.mjs check|graph|pin dsh|pi|bridge PREFIX");
    process.exitCode = 2;
  } else {
    try {
      if (verb === "pin") writeInputPins({ prefix, kind });
      else {
        const project = readProject({ prefix, kind });
        console.log(verb === "check" ? project.selection.version : JSON.stringify(graph(project)));
      }
    } catch (error) {
      console.error(`pnpm-lock: ${error.message}`);
      process.exitCode = 1;
    }
  }
}
