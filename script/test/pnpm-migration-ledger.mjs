// Rebuilds docs/standard-pnpm-migration.json from two reviewable inputs: the
// baseline npm lock read out of git at BASELINE and the committed dsh pnpm
// project. Every selection, delta and peer row is derived here, so a per-row
// reason cannot drift from the row it explains. RECORDED holds only the
// measurements no input file contains (manager provenance, command
// transcripts, inspected script payloads); it is cited from the prose doc.
//
//   node script/test/pnpm-migration-ledger.mjs           # rewrite the ledger
//   node script/test/pnpm-migration-ledger.mjs --check   # drift gate, no write
//
// --check needs the baseline commit object, so callers must fetch it (CI uses
// fetch-depth: 0). A missing object is a hard failure, never a skip.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
// The graph reader is the shipped helper, spawned as a CLI: the ledger is an
// independent oracle and no longer shares an in-process module with the parser.
const BINARY = (() => {
  const result = spawnSync("bash", [fileURLToPath(new URL("../../script/build/recipe-contract.sh", import.meta.url)), "--native"], { encoding: "utf8", timeout: 300000 });
  if (result.error || result.status !== 0) throw new Error(`cannot build the native recipe-contract helper: ${result.error ?? result.stderr}`);
  return path.join(result.stdout.trim(), "out", "recipe-contract");
})();
const pinnedInputs = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];

function helper(args, status = 0) {
  const result = spawnSync(BINARY, args, { encoding: "utf8", timeout: 60000, maxBuffer: 32 * 1024 * 1024 });
  if (result.error) throw result.error;
  assert.equal(result.status, status, `${args.join(" ")}: ${result.stderr}`);
  if (status !== 0) assert.equal(result.stdout, "", "failure leaked success payload");
  return result.stdout;
}

function readProject({ prefix, kind }) {
  const graph = JSON.parse(helper(["pnpm-lock", "graph", kind, prefix]));
  const report = JSON.parse(helper(["pnpm-lock", "peers", "dsh", path.join(prefix)]));
  assert.deepEqual(report.unsatisfied, [], "the committed graph has unsatisfied exact peers");
  return {
    ...graph,
    manifest: JSON.parse(fs.readFileSync(path.join(prefix, "package.json"), "utf8")),
    workspace: JSON.parse(helper(["pnpm-yaml", path.join(prefix, "pnpm-workspace.yaml")])),
    lock: JSON.parse(helper(["pnpm-yaml", path.join(prefix, "pnpm-lock.yaml")])),
    prefix,
    peerSummary: { exact: report.exact, totalOptionalMissing: report.totalOptionalMissing, cordisExact: report.cordisExact, unsatisfied: 0 },
  };
}

const ROOT = fileURLToPath(new URL("../..", import.meta.url));
const BASELINE = "6c51509";
const BASELINE_PATH = "images/tools/dsh/package-lock.json";
const PROJECT = "images/tools/dsh";
const LEDGER = "docs/standard-pnpm-migration.json";
const PIN_FILE = "pnpm-inputs.sha256";
// The one archive whose single snapshot is reached by two canonical owners; the
// ledger records that reachability instead of asserting a hoist identity.
const SANDBOX_ARCHIVE = "@deepseek-ai/dsh-sandbox-local";
// Metadata a resolver may record differently for the same tarball bytes.
const METADATA_FIELDS = ["libc", "engines", "peerDependencies", "peerDependenciesMeta"];
const REASON = {
  addedPeer: "peer provider materialized",
  removedRemovedSource: "source identity removed",
  removedOptional: "mandatory-to-optional reclassification",
  retargetPeerPinned: "singleton selection change",
  retargetRange: "range re-selection",
};

const sha256 = file => crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
const sha256Of = value => crypto.createHash("sha256").update(value).digest("hex");
const read = file => fs.readFileSync(path.join(ROOT, file), "utf8");
const lexicographic = (a, b) => (a < b ? -1 : a > b ? 1 : 0);
const identity = archive => `${archive.id.name}@${archive.id.version}`;

function git(args) {
  const result = spawnSync("git", args, { cwd: ROOT, encoding: "utf8", maxBuffer: 64 * 1024 * 1024 });
  assert.equal(result.status, 0, `git ${args.join(" ")} failed: ${result.stderr.trim()}`);
  return result.stdout;
}

/** @returns {{sha:string, sourceSha:string, bytes:string, npmLocations:number, npmIdentities:number, entries:Map<string,{entry:object, locations:string[]}>}} */
function readBaseline() {
  const sourceSha = git(["rev-parse", `${BASELINE}^{commit}`]).trim();
  const bytes = git(["show", `${sourceSha}:${BASELINE_PATH}`]);
  const lock = JSON.parse(bytes);
  assert.equal(lock.lockfileVersion, 3, "baseline must be a v3 npm lock");
  const entries = new Map();
  for (const key of Object.keys(lock.packages)) {
    if (key === "") continue;
    const entry = lock.packages[key];
    const name = key.slice(key.lastIndexOf("node_modules/") + "node_modules/".length);
    const id = `${name}@${entry.version}`;
    const row = entries.get(id) ?? { entry, locations: [] };
    row.locations.push(key);
    entries.set(id, row);
  }
  for (const row of entries.values()) row.locations.sort(lexicographic);
  return {
    sha: sha256Of(bytes), sourceSha, bytes, entries,
    npmLocations: Object.keys(lock.packages).length - 1,
    npmIdentities: entries.size,
    rootDependencies: lock.packages[""]?.dependencies ?? {},
  };
}

/** npm's tree is a location map, so an edge target is the nearest nested or
 * hoisted location the old resolver would have loaded. */
function resolveNpmTarget(packages, location, dependency) {
  for (let base = location; ; ) {
    const candidate = base ? `${base}/node_modules/${dependency}` : `node_modules/${dependency}`;
    if (packages[candidate]) return candidate;
    if (!base) return undefined;
    const outer = base.indexOf("/node_modules/");
    base = outer === -1 ? "" : base.slice(0, outer);
  }
}

const stripContext = key => {
  let out = "";
  let depth = 0;
  for (const character of key) {
    if (character === "(") depth++;
    else if (character === ")") depth--;
    else if (depth === 0) out += character;
  }
  return out;
};

function readGraph() {
  const project = readProject({ prefix: path.join(ROOT, PROJECT), kind: "dsh" });
  const archives = new Map(project.packages.map(archive => [identity(archive), archive]));
  const snapshotsByIdentity = new Map();
  for (const snapshot of project.snapshots) {
    const id = stripContext(snapshot.id.key);
    snapshotsByIdentity.set(id, [...(snapshotsByIdentity.get(id) ?? []), snapshot.id.key]);
  }
  return { project, archives, snapshotsByIdentity };
}

/** Both graphs reduce to identity -> edge name -> Set(target identity): the npm
 * side resolves each declaration through the old location tree, the pnpm side
 * reads the snapshot edges and drops the peer-context suffix. */
function edgeIndex() {
  const mandatory = new Map();
  const optional = new Map();
  const add = (map, id, name, target) => {
    const row = map.get(id) ?? new Map();
    row.set(name, new Set([...(row.get(name) ?? []), target]));
    map.set(id, row);
  };
  return { mandatory, optional, add };
}

const npmIdentity = (packages, location) =>
  `${location.slice(location.lastIndexOf("node_modules/") + "node_modules/".length)}@${packages[location].version}`;

function npmEdgeIndex(entries, packages) {
  const index = edgeIndex();
  for (const [id, { entry, locations }] of entries) {
    for (const [field, map] of [["dependencies", index.mandatory], ["optionalDependencies", index.optional]]) {
      for (const dependency of Object.keys(entry[field] ?? {})) {
        for (const location of locations) {
          const target = resolveNpmTarget(packages, location, dependency);
          if (target) index.add(map, id, dependency, npmIdentity(packages, target));
        }
      }
    }
  }
  return index;
}

function pnpmEdgeIndex(graph) {
  const index = edgeIndex();
  for (const snapshot of graph.project.snapshots) {
    const id = stripContext(snapshot.id.key);
    for (const edge of snapshot.edges) {
      index.add(edge.optional ? index.optional : index.mandatory, id, edge.name, stripContext(edge.target));
    }
  }
  return index;
}

/** @returns {{rows:object[], added:object[], removed:object[], retargeted:object[]}} */
function diffEdges(before, after) {
  const rows = [];
  const added = [];
  const removed = [];
  const retargeted = [];
  const ids = new Set([...before.keys(), ...after.keys()]);
  for (const id of [...ids].sort(lexicographic)) {
    const beforeRow = before.get(id) ?? new Map();
    const afterRow = after.get(id) ?? new Map();
    const names = new Set([...beforeRow.keys(), ...afterRow.keys()]);
    for (const name of [...names].sort(lexicographic)) {
      const from = [...(beforeRow.get(name) ?? [])].sort(lexicographic);
      const to = [...(afterRow.get(name) ?? [])].sort(lexicographic);
      if (JSON.stringify(from) === JSON.stringify(to)) continue;
      const row = { edge: `${id} -> ${name}`, before: from.length ? from : null, after: to.length ? to : null };
      rows.push(row);
      if (!from.length) added.push(row);
      else if (!to.length) removed.push(row);
      else retargeted.push(row);
    }
  }
  return { rows, added, removed, retargeted };
}

function overrideSelectors(project) {
  return Object.entries(project.workspace.overrides ?? {}).map(([selector, version]) => ({
    selector, name: selector.slice(selector.lastIndexOf(">") + 1), version,
  }));
}

/** Removal reasons come from the removed row: its override selector, or the
 * baseline root's manager declaration. An unclassified removal is a review stop. */
function removalReason({ baseline, project }) {
  const selectors = overrideSelectors(project);
  return (name, versions) => {
    const override = selectors.find(candidate => candidate.name === name);
    if (override) {
      return `substituted by reviewed override "${override.selector}" at ${override.version} (npm recorded ${name} at ${versions.join(", ")})`;
    }
    assert.ok(Object.hasOwn(baseline.rootDependencies, name), `unclassified removal ${name}`);
    return `base-owned manager selection: the baseline root declared ${name}, and the base image owns the only manager`;
  };
}

function metadataDelta(entry, raw) {
  const delta = [];
  for (const field of METADATA_FIELDS) {
    const before = entry[field] ?? null;
    const after = raw[field] ?? null;
    if (JSON.stringify(before) !== JSON.stringify(after)) delta.push({ field, before, after });
  }
  return delta;
}

function deltaSection(context) {
  const { baseline, graph, packages, project } = context;
  const raw = project.lock.packages;
  const retained = [];
  const removed = [];
  const deduplicated = [];
  const metadataDeltas = [];
  for (const id of [...baseline.entries.keys()].sort(lexicographic)) {
    const { entry, locations } = baseline.entries.get(id);
    const archive = graph.archives.get(id);
    if (!archive) continue;
    const delta = metadataDelta(entry, raw[id]);
    if (delta.length) metadataDeltas.push({ id, delta });
    const tarball = `https://registry.npmjs.org/${archive.id.name}/-/${archive.id.name.split("/").at(-1)}-${archive.id.version}.tgz`;
    retained.push({
      id,
      oldLocations: locations,
      oldSri: entry.integrity,
      newSri: archive.integrity,
      sriEqual: entry.integrity === archive.integrity,
      resolverDelta: entry.resolved === tarball ? [] : [{ field: "tarball", before: entry.resolved, after: tarball }],
      metadataDelta: delta,
    });
    const snapshots = graph.snapshotsByIdentity.get(id) ?? [];
    if (locations.length > snapshots.length) deduplicated.push({ id, oldLocations: locations, newSnapshots: snapshots });
  }
  const versionsByName = new Map();
  for (const [id, { locations }] of baseline.entries) {
    const name = id.slice(0, id.lastIndexOf("@"));
    versionsByName.set(name, [...(versionsByName.get(name) ?? []), { id, version: id.slice(id.lastIndexOf("@") + 1), locations }]);
  }
  // Highest location first: the old tree's hoisted selection is the interesting one.
  for (const rows of versionsByName.values()) rows.sort((a, b) => a.locations[0].length - b.locations[0].length || lexicographic(a.id, b.id));
  const reasonFor = removalReason(context);
  for (const id of [...baseline.entries.keys()].sort(lexicographic)) {
    if (graph.archives.has(id)) continue;
    const { entry, locations } = baseline.entries.get(id);
    const name = id.slice(0, id.lastIndexOf("@"));
    removed.push({
      id, sri: entry.integrity, locations,
      reason: reasonFor(name, versionsByName.get(name).map(row => row.version)),
    });
  }
  const cordisSubstitutions = overrideSelectors(project).map(({ name, selector, version }) => {
    const dropped = (versionsByName.get(name) ?? []).filter(row => row.version !== version);
    assert.ok(dropped.length <= 1, `${name}: more than one dropped npm version`);
    return { name, selector, retained: version, dropped: dropped[0]?.version ?? null };
  });
  const mandatoryBefore = context.beforeEdges.mandatory;
  const mandatoryAfter = context.afterEdges.mandatory;
  const mandatory = diffEdges(mandatoryBefore, mandatoryAfter);
  const optional = diffEdges(context.beforeEdges.optional, context.afterEdges.optional);
  for (const row of optional.added) {
    const archive = graph.archives.get(row.edge.slice(0, row.edge.lastIndexOf(" -> ")));
    assert.ok(archive?.optionalPeers.includes(row.edge.slice(row.edge.lastIndexOf(" -> ") + 4)), `optional edge is not a declared optional peer: ${row.edge}`);
    row.reason = `${REASON.addedPeer}: ${row.edge.slice(row.edge.lastIndexOf(" -> ") + 4)} is an optional peer of ${archive.id.name}, which the old tree recorded only as a declaration`;
  }
  assert.deepEqual(optional.removed, [], "an optional edge was lost");
  const removedIds = new Set(removed.map(row => row.id));
  const optionalAfter = context.afterEdges.optional;
  for (const row of mandatory.removed) {
    const [source, name] = row.edge.split(" -> ");
    if (removedIds.has(source)) {
      row.reason = `${REASON.removedRemovedSource}: ${removed.find(candidate => candidate.id === source).reason}`;
    } else {
      assert.ok(optionalAfter.get(source)?.has(name), `unclassified edge removal: ${row.edge}`);
      row.reason = `${REASON.removedOptional}: ${name} is an optional peer of ${source}, which npm recorded in its mandatory projection`;
    }
  }
  for (const row of mandatory.added) {
    const [source, name] = row.edge.split(" -> ");
    assert.ok(graph.archives.get(source)?.peers[name] !== undefined, `edge addition is not a declared peer: ${row.edge}`);
    row.reason = `${REASON.addedPeer}: ${name} is a declared peer on ${source}, not an npm dependency edge`;
  }
  const exactPeersOf = (name, wanted) => [...graph.archives.values()]
    .filter(archive => archive.peers[name] === wanted)
    .map(identity).sort(lexicographic);
  const specOf = (id, name) => baseline.entries.get(id)?.entry.dependencies?.[name];
  const rootIdentity = stripContext(project.importer[0].target);
  const retargetedRows = [];
  const singletonByPackage = new Map();
  const overrides = Object.keys(project.workspace.overrides ?? {}).length;
  // Every range declaration for the package in the old tree, root selection
  // first: these are what pnpm re-selected, not a rewritten declaration.
  const rangeDeclarationsFor = name => [...baseline.entries]
    .filter(([, { entry }]) => entry.dependencies?.[name] !== undefined)
    .map(([id, { entry }]) => ({ selector: id, spec: entry.dependencies[name], root: id === rootIdentity }))
    .sort((a, b) => Number(b.root) - Number(a.root) || lexicographic(a.selector, b.selector));
  for (const row of mandatory.retargeted) {
    const [source, name] = row.edge.split(" -> ");
    const spec = specOf(source, name);
    assert.ok(spec && !/^(0|[1-9]\d*)\.\d+\.\d+/.test(spec), `retarget from an exact declaration: ${row.edge}`);
    const previous = row.before[0].slice(row.before[0].lastIndexOf("@") + 1);
    const selected = row.after[0].slice(row.after[0].lastIndexOf("@") + 1);
    const pinned = exactPeersOf(name, previous);
    if (!pinned.length) {
      row.reason = `${REASON.retargetRange}: range declaration ${spec} now selects ${selected}; no exact peer in this graph pins ${previous}`;
      retargetedRows.push(row);
      continue;
    }
    row.reason = `${REASON.retargetPeerPinned}: range declaration ${spec} now selects ${selected}, while ${pinned.join(", ")} still declares the exact peer ${previous}; strictPeerDependencies is false, so both versions stay in the graph in different peer contexts. Not a dedup`;
    const existing = singletonByPackage.get(name);
    if (existing) {
      assert.equal(`${existing.previous}->${existing.selected}`, `${previous}->${selected}`, `${name}: inconsistent singleton row`);
      existing.edges.push(row.edge);
    } else {
      const declarations = rangeDeclarationsFor(name);
      const rootDeclaration = declarations.find(declaration => declaration.root) ?? declarations[0];
      singletonByPackage.set(name, {
        package: name,
        edges: [row.edge],
        previous,
        selected,
        rootDeclaration,
        exactPeerDeclarations: pinned.map(selector => ({ selector, wanted: previous })),
        previousSnapshots: graph.snapshotsByIdentity.get(row.before[0]) ?? [],
        selectedSnapshots: graph.snapshotsByIdentity.get(row.after[0]) ?? [],
        reason: `${rootDeclaration.selector} declares the range ${rootDeclaration.spec} and pnpm selected the newest satisfying ${selected}; ${pinned.join(", ")} declares the exact peer ${previous} and keeps its own ${previous} context. strictPeerDependencies: false permits both, the review keeps its ${overrides} overrides and gains no fifth`,
      });
    }
    retargetedRows.push(row);
  }
  const groups = new Map();
  const count = reason => groups.set(reason, (groups.get(reason) ?? 0) + 1);
  for (const row of mandatory.added) count(row.reason.split(":")[0]);
  for (const row of mandatory.removed) count(row.reason.split(":")[0]);
  for (const row of mandatory.retargeted) count(row.reason.split(":")[0]);
  // An identity pnpm resolved that the npm tree never recorded is a new selection,
  // not a migration detail: it must be named with a reason in RECORDED, so a
  // regeneration cannot report "zero added" while the graph gained an archive.
  const recordedAdditions = RECORDED.additions ?? {};
  const added = [...graph.archives.values()]
    .filter(archive => !baseline.entries.has(identity(archive)))
    .map(archive => ({ id: identity(archive), sri: archive.integrity }))
    .sort((a, b) => lexicographic(a.id, b.id));
  for (const row of added) {
    assert.ok(Object.hasOwn(recordedAdditions, row.id), `unrecorded graph addition ${row.id}`);
    row.reason = recordedAdditions[row.id];
  }
  assert.deepEqual(added.map(row => row.id), Object.keys(recordedAdditions).sort(lexicographic),
    "a recorded addition is not in the graph");
  const summary = [...groups].map(([reason, rows]) => `${rows} ${reason}`).join("; ");
  const maxLocations = Math.max(...deduplicated.flatMap(row => row.oldLocations.length), 1);
  return {
    retained,
    added,
    removed,
    deduplicated,
    metadataDeltas,
    metadataDeltaReason: `same tarball (sha512 equal) recorded with a different metadata projection: pnpm records ${METADATA_FIELDS.join(", ")} that npm omits or writes differently`,
    deduplicatedReason: `npm hoisted one identity into up to ${maxLocations} locations; pnpm keeps one snapshot per peer context, so an npm location count is never a pnpm record count`,
    cordisSubstitutions,
    // A singleton selection change, not a dedup: honesty about the fifth Cordis
    // companion is worth more than a tidier story, and the runtime readiness leg
    // (not this row) proves the config/web path still resolves.
    singletonSelections: {
      reason: `no override was added: the review keeps its ${overrides} overrides, and these rows record range selections that differ from an exact peer declaration, permitted only because strictPeerDependencies is false`,
      rows: [...singletonByPackage.values()],
    },
    dependencyEdgeDeltas: {
      rows: mandatory.rows.length,
      autoInstalledPeers: mandatory.added.length,
      removed: mandatory.removed.length,
      retargeted: mandatory.retargeted.length,
      removedRows: mandatory.removed,
      retargetedRows,
      reason: `closed dsh-root closure; every row states its own derived reason: ${summary}`,
    },
    optional: optional.added,
    snapshotEdges: mandatory.rows,
  };
}

function graphSection(graph) {
  const { project } = graph;
  // Counted, not inferred from the key text: one opaque hash can appear in many
  // peer-context positions, so a bare "N opaque contexts" number reads as a hash
  // count. These are snapshots, hashes and snapshots-with-any-opaque-position.
  const hashes = new Set();
  let containing = 0;
  for (const snapshot of project.snapshots) {
    const opaque = snapshot.id.peerContext.filter(context => context.opaque);
    if (!opaque.length) continue;
    containing++;
    for (const context of opaque) hashes.add(context.key);
  }
  const identities = new Set(project.packages.map(identity));
  return {
    graph: {
      archives: project.packages.length,
      snapshots: project.snapshots.length,
      identities: identities.size,
      pnpmArchives: project.packages.filter(archive => archive.id.name === "pnpm").length,
      pnpmSnapshots: project.snapshots.filter(snapshot => stripContext(snapshot.id.key).startsWith("pnpm@")).length,
      opaquePeerContexts: {
        // The key text is a rendered identity, so a hash can appear in it in more
        // positions than the parsed peer context records; both numbers are stated
        // instead of one ambiguous "contexts" count.
        label: "snapshots whose peer context is exactly one opaque hash",
        snapshots: project.snapshots.filter(snapshot =>
          snapshot.id.peerContext.length === 1 && snapshot.id.peerContext[0].opaque).length,
        distinctOpaqueHashes: hashes.size,
        snapshotsContainingAnOpaqueHash: containing,
        snapshotKeysContainingAnOpaqueHash:
          project.snapshots.filter(snapshot => [...hashes].some(hash => snapshot.id.key.includes(hash))).length,
      },
    },
    peerSummary: project.peerSummary,
  };
}

function sandboxSection(graph) {
  const sandboxes = graph.project.snapshots.filter(snapshot => stripContext(snapshot.id.key).startsWith(`${SANDBOX_ARCHIVE}@`));
  const targets = new Set(sandboxes.map(snapshot => snapshot.id.key));
  const owners = [];
  for (const snapshot of graph.project.snapshots) {
    for (const edge of snapshot.edges) {
      if (targets.has(edge.target)) owners.push({ owner: stripContext(snapshot.id.key), target: edge.target });
    }
  }
  owners.sort((a, b) => lexicographic(a.owner, b.owner));
  return {
    sandbox: sandboxes.map(snapshot => ({ identity: stripContext(snapshot.id.key), snapshot: snapshot.id.key })),
    sandboxOwners: owners,
  };
}

/** Each exact peer row names the provider its own snapshot context resolved. */
function peerRows(graph) {
  const rows = [];
  for (const snapshot of graph.project.snapshots) {
    const archive = graph.archives.get(stripContext(snapshot.id.key));
    for (const [peer, wanted] of Object.entries(archive.peers)) {
      if (!/^(0|[1-9]\d*)\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$/.test(wanted)) continue;
      const edge = snapshot.edges.find(candidate => candidate.name === peer);
      rows.push({
        snapshot: snapshot.id.key,
        peer,
        wanted,
        effective: edge ? edge.target : null,
        optional: archive.optionalPeers.includes(peer),
        satisfied: Boolean(edge) && stripContext(edge.target) === `${peer}@${wanted}`,
      });
    }
  }
  rows.sort((a, b) => lexicographic(a.snapshot, b.snapshot) || lexicographic(a.peer, b.peer));
  return rows;
}

/** The five defaults are owned by the preparation script; derive the policy
 * from its exports and refuse to disagree with the resolved argv. */
function policySection() {
  const prepare = read(`${PROJECT}/prepare-lock.sh`);
  const install = read(`${PROJECT}/install-dsh.sh`);
  const value = (source, pattern, file) => {
    const match = new RegExp(pattern).exec(source);
    assert.ok(match, `${file}: policy value not found (${pattern})`);
    return match[1];
  };
  const policy = {
    minimumReleaseAge: Number(value(prepare, "PNPM_CONFIG_MINIMUM_RELEASE_AGE=(\\d+)", "prepare-lock.sh")),
    minimumReleaseAgeStrict: value(prepare, "PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT=(\\w+)", "prepare-lock.sh") === "true",
    minimumReleaseAgeIgnoreMissingTime: value(prepare, "PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME=(\\w+)", "prepare-lock.sh") === "true",
    trustPolicy: value(prepare, "PNPM_CONFIG_TRUST_POLICY=([a-z-]+)", "prepare-lock.sh"),
    ignoreScripts: value(prepare, "PNPM_CONFIG_IGNORE_SCRIPTS=(\\w+)", "prepare-lock.sh") === "true",
  };
  for (const source of [prepare, install]) {
    for (const [flag, expected] of [["minimum-release-age", policy.minimumReleaseAge],
      ["minimum-release-age-strict", policy.minimumReleaseAgeStrict], ["trust-policy", policy.trustPolicy]]) {
      assert.ok(source.includes(`--config.${flag}=${expected}`), `--config.${flag}=${expected} missing from a dsh script`);
    }
    assert.ok(source.includes("--ignore-scripts"), "resolving argv must carry --ignore-scripts");
  }
  return policy;
}

const RECORDED = {
    "measuredOn": "implementation host, macOS arm64; not proposed-SHA image acceptance",
    // An archive the pnpm graph resolved that the npm baseline never recorded, with
    // the review reason. Empty is the measured fact for this migration (565
    // archives, all retained); a regeneration that gains one must add it here.
    "additions": {},
    "changedCandidate": "0.1.5-rc.1",
    "manager": {
      "version": "11.11.0",
      "url": "https://registry.npmjs.org/pnpm/-/pnpm-11.11.0.tgz",
      "archiveSri": "sha512-RGP2X9gO2A1pvB1L8WPulPYFxzgPwxi7Wy6+FfjNEtScUaTVnpUbQB52TTtsp1HL9RvFDtcAGmvLSTXmhMNIgg==",
      "archiveSha512": "4463f65fd80ed80d69bc1d4bf163ee94f605c7380fc318bb5b2ebe15f8cd12d49c51a4d59e951b401e764d3b6ca751cbf51bc50ed7001a6bcb4935e684c34882",
      "bundleSha256": "e8abc02d5fbffb8f9d8817d44d6eee84c379ea8d5006dd5aa8694bb5e4428d8a",
      "installedBundleSha256": "e8abc02d5fbffb8f9d8817d44d6eee84c379ea8d5006dd5aa8694bb5e4428d8a",
      "upstreamTag": "v11.11.0",
      "upstreamCommit": "19696e1f4957caa2813ce3beb026d9629e24e6e1",
      "node": "v23.11.0",
      "platform": "Darwin arm64",
      "route": "authenticated stock distribution bin/pnpm.mjs invoked with an absolute Node path; the fixed Linux launcher was not run on macOS and no PATH manager was used",
      "hostPnpm12Used": false,
      "environment": {
        "PNPM_CONFIG_MINIMUM_RELEASE_AGE": "1440",
        "PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT": "true",
        "PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME": "false",
        "PNPM_CONFIG_TRUST_POLICY": "no-downgrade",
        "PNPM_CONFIG_IGNORE_SCRIPTS": "true"
      }
    },
    "commands": [
      {
        "argv": [
          "pnpm",
          "--version",
          "--pm-on-fail=error",
          "--config.update-notifier=false"
        ],
        "status": 0,
        "output": "11.11.0",
        "seconds": 0.36
      },
      {
        "argv": [
          "pnpm",
          "import",
          "--reporter=ndjson"
        ],
        "status": 0,
        "seconds": 3.51,
        "note": "pnpm 11.11.0 import accepts no CLI options; the five policy settings only arrive through the exported env defaults, and npmjs is the default registry"
      },
      {
        "argv": [
          "rm",
          "package-lock.json"
        ],
        "status": null,
        "note": "explicitly owned temporary seed copy only"
      },
      {
        "argv": [
          "pnpm",
          "install",
          "--lockfile-only",
          "--no-frozen-lockfile",
          "<five config flags>",
          "--ignore-scripts",
          "--ignore-pnpmfile",
          "--pm-on-fail=error",
          "--config.update-notifier=false",
          "--registry=https://registry.npmjs.org/",
          "--package-import-method=copy",
          "--store-dir",
          "<scratch>/store"
        ],
        "status": 0,
        "seconds": 0.37,
        "note": "lockfile-verification status cached because the import call under the same five env defaults already verified 565 entries"
      },
      {
        "argv": [
          "pnpm",
          "install",
          "--lockfile-only",
          "--no-frozen-lockfile",
          "<same flags>",
          "--store-dir",
          "<fresh store, cleared cache>"
        ],
        "status": 0,
        "seconds": 2.9,
        "note": "fresh-cache rerun exercised the real policy verification: pnpm:lockfile-verification started/done, entries 565, elapsedMs 2897; emitted lock bytes unchanged (sha256 8bfe8e92...)"
      },
      {
        "argv": [
          "node",
          "images/recipe-contract/pnpm-lock.mjs",
          "pin",
          "dsh",
          "images/tools/dsh"
        ],
        "status": 0
      },
      {
        "argv": [
          "node",
          "images/recipe-contract/pnpm-lock.mjs",
          "check",
          "dsh",
          "images/tools/dsh"
        ],
        "status": 0,
        "output": "0.1.5-rc.2"
      },
      {
        "argv": [
          "node",
          "images/recipe-contract/pnpm-lock.mjs",
          "graph",
          "dsh",
          "images/tools/dsh"
        ],
        "status": 0
      }
    ],
    "integration": {
      "equalPathOnRealInputs": {
        "command": "prepare-lock.sh PREFIX \"\" \"\" (fixture-authorized copy of the four published inputs)",
        "status": 0,
        "managerCalls": 0,
        "output": "dsh lock update: equal 0.1.5-rc.2 -> 0.1.5-rc.2; 243 exact peers satisfied; prepare-lock.sh: dsh 0.1.5-rc.2 unchanged; reviewed inputs reused"
      },
      "changedPathOnRealInputs": {
        "command": "prepare-lock.sh PREFIX 0.1.5-rc.1 \"\" with the authenticated rc.1 graph staged by the fixture manager",
        "status": 0,
        "managerCalls": 2,
        "managerArgv": [
          "--version --pm-on-fail=error --config.update-notifier=false",
          "install --lockfile-only --no-frozen-lockfile --config.minimum-release-age=1440 --config.minimum-release-age-strict=true --config.minimum-release-age-ignore-missing-time=false --config.trust-policy=no-downgrade --ignore-scripts --ignore-pnpmfile --pm-on-fail=error --config.update-notifier=false --registry=https://registry.npmjs.org/ --package-import-method=copy --store-dir <scratch>/store --reporter=ndjson"
        ],
        "publishedLockSha256": "441150cf8eca16d223c60062931314a1055e9de1cb8fc14279a9622db1fe3c51",
        "authenticatedRc1LockSha256": "441150cf8eca16d223c60062931314a1055e9de1cb8fc14279a9622db1fe3c51",
        "comparison": "changed 0.1.5-rc.2 -> 0.1.5-rc.1; 243 exact peers satisfied",
        "rc1Graph": {
          "archives": 565,
          "snapshots": 565
        }
      },
      "equalCopyComparison": "check-lock-update.js <prefix> <copy-of-prefix> \"\" -> equal 0.1.5-rc.2 -> 0.1.5-rc.2; 243 exact peers satisfied"
    },
    "scripts": [
      {
        "identity": "@deepseek-ai/dsh-subprocess-local@0.1.5-rc.3",
        "sha512": "sha512-old72v9tvx3DoQw6+HAsX4ZN8ObKjK0QfJvmcuDgwNY9i7m9XmyEK2FgdRT0dQGUdap5zOHh5dPj7dUI1oIywA==",
        "scripts": {
          "postinstall": "node scripts/ensure-spawn-helper.mjs"
        },
        "scriptFileHashes": {
          "package/scripts/ensure-spawn-helper.mjs": "ca5509febf1e6ec1356df121835ebe5ed2f9cace4bdc2ba6d83d41c7e45e0f1b"
        },
        "payload": [],
        "reason": "postinstall node scripts/ensure-spawn-helper.mjs only restores the executable bit of an existing node-pty platform spawn-helper; the exact node-pty payload ships spawn-helper only for darwin-arm64/darwin-x64, so Linux has nothing to act on",
        "runtimeDiscriminator": "real node-pty PTY spawn/read/exit from the canonical dsh-subprocess-local owner"
      },
      {
        "identity": "@google/genai@1.52.0",
        "sha512": "sha512-gwSvbpiN/17O9TbsqSsE/OzZcpv5Fo4RQjdngGgogtuB9RsyJ8ZHhX5KjHj1bp5N9snN2eK8LDGXSaWW2hof8Q==",
        "scripts": {
          "preinstall": "echo 'preinstall: no-op'",
          "prepare": "node scripts/prepare.js",
          "prepack": "echo 'prepack: no-op'",
          "postpack": "echo 'postpack: no-op'",
          "prebuild": "echo 'prebuild: no-op'",
          "build": "patch-package && rollup -c && npm-run-all --parallel api-extractor:dev:* && node scripts/ignore_missing_mcp_dep.js",
          "prebuild-prod": "echo 'prebuild-prod: no-op'",
          "build-prod": "patch-package && rollup -c && npm-run-all --parallel api-extractor:prod:* && node scripts/ignore_missing_mcp_dep.js",
          "api-extractor:dev:main": "api-extractor run --local --verbose",
          "api-extractor:dev:node": "api-extractor run -c api-extractor.node.json --local --verbose",
          "api-extractor:dev:web": "api-extractor run -c api-extractor.web.json --local --verbose",
          "api-extractor:dev:tokenizer-node": "api-extractor run -c api-extractor.tokenizer-node.json --local --verbose",
          "api-extractor:prod:main": "api-extractor run --verbose",
          "api-extractor:prod:node": "api-extractor run -c api-extractor.node.json --verbose",
          "api-extractor:prod:web": "api-extractor run -c api-extractor.web.json --verbose",
          "api-extractor:prod:tokenizer-node": "api-extractor run -c api-extractor.tokenizer-node.json --verbose",
          "api-extractor:dev:vertex_internal": "api-extractor run -c api-extractor.vertex_internal.json --local --verbose",
          "api-extractor:prod:vertex_internal": "api-extractor run -c api-extractor.vertex_internal.json --verbose",
          "unit-test": "tsc && cp src/cross/sentencepiece/sentencepiece_model.pb.js dist/src/cross/sentencepiece/ && jasmine dist/test/unit/**/*_test.js dist/test/unit/**/**/*_test.js dist/test/unit/*_test.js",
          "system-test": "tsc && jasmine dist/test/system/**/*_test.js",
          "test-server-tests": "tsc && GOOGLE_CLOUD_PROJECT=googcloudproj GOOGLE_CLOUD_LOCATION=googcloudloc jasmine dist/test/system/node/*_test.js -- --test-server",
          "test-server-tests:record": "tsc && jasmine --fail-fast dist/test/system/node/*_test.js -- --test-server --record",
          "docs": "typedoc && node --loader ts-node/esm scripts/add_docsite_license_headers.ts",
          "pages-main": "node --loader ts-node/esm scripts/generate_pages.ts main",
          "pages-release": "node --loader ts-node/esm scripts/generate_pages.ts release",
          "format": "prettier '**/*.ts' '**/*.mjs' '**/*.json' --write",
          "lint": "eslint '**/*.ts'",
          "lint-fix": "eslint --fix '**/*.ts'",
          "coverage-report": "./test/generate_report.sh",
          "generate-proto": "pbjs -t static-module -w es6 -o src/cross/sentencepiece/sentencepiece_model.pb.js src/cross/sentencepiece/sentencepiece_model.proto && pbts -o src/cross/sentencepiece/sentencepiece_model.pb.d.ts src/cross/sentencepiece/sentencepiece_model.pb.js && sed -i.bak 's/import \\* as \\$protobuf from \"protobufjs\\/minimal\"/import \\$protobuf from \"protobufjs\\/minimal.js\"/' src/cross/sentencepiece/sentencepiece_model.pb.js && rm src/cross/sentencepiece/sentencepiece_model.pb.js.bak"
        },
        "scriptFileHashes": {},
        "payload": [],
        "reason": "preinstall is literally echo preinstall: no-op and the installable hooks (prepack/postpack/prebuild/prebuild-prod) are echo no-ops; prepare/build are not run for registry installs and the shipped dist JS needs no generated artifact",
        "runtimeDiscriminator": "import shipped dist from the pi-ai owner context and assert the GoogleGenAI export"
      },
      {
        "identity": "koffi@3.3.1",
        "sha512": "sha512-FZYfhBfYQr/cmHhZpaQ9rhbKpkjHhxaytn2A0mbPhiD5yV8QzF3SiQwR1w/34rCJ1fw+6qBQSj/ZoXRdC+Xc2Q==",
        "scripts": {
          "install": "node ./cnoke.cjs -P . -D src/koffi --prebuild --release"
        },
        "scriptFileHashes": {},
        "payload": [],
        "reason": "install node ./cnoke.cjs --prebuild --release is a source-build path; the exact @koromix/koffi-linux-{x64,arm64} optional packages ship glibc and musl koffi.node prebuilds for every supported Linux arch, so no compilation is required",
        "runtimeDiscriminator": "load libc.so.6 and call getpid() from the dsh-subprocess-local owner context"
      },
      {
        "identity": "node-pty@1.2.0-beta.15",
        "sha512": "sha512-vORSzHXi4Ofl7HemVWpuudLqCPdaQb4LfpRCUpE5HPxhp4JYscl8zZwxh11p26v2wvW24WMwnMfLjhRLixrfxA==",
        "scripts": {
          "build": "tsc -b ./src/tsconfig.json",
          "watch": "tsc -b -w ./src/tsconfig.json",
          "lint": "eslint src/",
          "install": "node scripts/prebuild.js || node-gyp rebuild",
          "postinstall": "node scripts/post-install.js",
          "compileCommands": "node scripts/gen-compile-commands.js",
          "test": "cross-env NODE_ENV=test mocha -R spec --exit lib/*.test.js",
          "posttest": "npm run lint",
          "prepare": "npm run build",
          "prepublishOnly": "npm run build"
        },
        "scriptFileHashes": {
          "package/scripts/post-install.js": "98b3f6379debdab20eea21f216e0d09fa38c7757ddc3411f1a743eae7f5be14a",
          "package/scripts/prebuild.js": "7e604b10f7769d7dc95947d3481c00513b9e3bb6c561d5264506350a16a0381a"
        },
        "payload": [
          {
            "path": "package/prebuilds/darwin-arm64/pty.node",
            "bytes": 86904,
            "sha256": "0aae85518beb134a666f8e415f78bf528b2e9d40b47498af97433dc459661366"
          },
          {
            "path": "package/prebuilds/darwin-arm64/spawn-helper",
            "bytes": 50480,
            "sha256": "08bc83a084651d095645010d34783f94b9af443614747e953e66d6578027e999"
          },
          {
            "path": "package/prebuilds/darwin-x64/pty.node",
            "bytes": 58576,
            "sha256": "811f4a357e260fc0e520c250a3de5990483761d82afd635effbdf4b793b8a0e9"
          },
          {
            "path": "package/prebuilds/darwin-x64/spawn-helper",
            "bytes": 9248,
            "sha256": "a3bed36ae3ed83b2ac2fce475bd578c51f5b536041f0b2dfe116987f565e9758"
          },
          {
            "path": "package/prebuilds/linux-arm64/pty.node",
            "bytes": 69112,
            "sha256": "5a3b8930d62535edabec2eac79fe8eca70a178d93fbb76b1ae875dd60ee8f825"
          },
          {
            "path": "package/prebuilds/linux-x64/pty.node",
            "bytes": 75976,
            "sha256": "123792d8f22f36d519b21dfb6f4b716f9b18b9e579085ea2840d0773d88d35fd"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty.node",
            "bytes": 279040,
            "sha256": "6da15dc84b864bb4630d95faef97da5f323a078b53d73f395b5efd6f7952c3e0"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty.pdb",
            "bytes": 5918720,
            "sha256": "e1b9db81b9b10f8b8b7eb3ad5c9375d4adb328933d8dbd830eaeea4ce447fa4b"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty/OpenConsole.exe",
            "bytes": 1110560,
            "sha256": "7fa560be0c9b6c81db5d1b855a4a6e0f3ee5a2ea75d36eebb3ae44443b8d4866"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty/conpty.dll",
            "bytes": 106528,
            "sha256": "b3a9f975c51d5b9b96d290bf784e990e3f85203b063c21614457353f566266ff"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty_console_list.node",
            "bytes": 122368,
            "sha256": "327cf47db8b524a75813c98871037bc60f0a08660e75635897aed957a059b690"
          },
          {
            "path": "package/prebuilds/win32-arm64/conpty_console_list.pdb",
            "bytes": 4231168,
            "sha256": "21a3b6dc0600e4db43e1f1e3149daab199e333c064a65714fd61d5205a52ebfa"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty.node",
            "bytes": 291328,
            "sha256": "ec2fd8545c30e051ee3fa17d26127f7e615919814879c7f746cf4c80845226e0"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty.pdb",
            "bytes": 6311936,
            "sha256": "9275fecdca50f646579134e44604ba0ce81b184cea8edfc7c9fa53e59b475013"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty/OpenConsole.exe",
            "bytes": 1062472,
            "sha256": "7f68c840226505004215c0b82d4e502c24b5bc3f4b93c4baaaa19bd679c0def8"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty/conpty.dll",
            "bytes": 110152,
            "sha256": "3319b484b80bb53d1f4d0a9eb0ea60fd0f61da69db7280ca43b84215f19245ff"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty_console_list.node",
            "bytes": 134656,
            "sha256": "d46800643f90fc56b9fd67398f04a04a871521de0e825211c8382d187859d3ed"
          },
          {
            "path": "package/prebuilds/win32-x64/conpty_console_list.pdb",
            "bytes": 4321280,
            "sha256": "eedbd0fb50293fff4532465ac6e7d765c0d543923ae11e6b1a05b5ae55fe3476"
          }
        ],
        "reason": "install node scripts/prebuild.js || node-gyp rebuild only checks that prebuilds/<platform>-<arch> exists (or removes it for an explicit source build) and post-install only prunes build/Release plus Windows ConPTY copies; the tarball ships prebuilds/linux-arm64/pty.node and prebuilds/linux-x64/pty.node",
        "runtimeDiscriminator": "minimal PTY spawn/read/exit through the installed Linux pty.node"
      },
      {
        "identity": "protobufjs@7.6.6",
        "sha512": "sha512-dYDWdjSl5RNb7SgPxGQcRU+GtvP7s2fpkrY0r432PcOIaZ0/rBcxEZnQN67iJhFuQiVw754JDoPruPCNdGsbjg==",
        "scripts": {
          "bench": "node bench",
          "build": "npm run build:bundle && npm run build:types",
          "build:bundle": "gulp --gulpfile scripts/gulpfile.js",
          "build:types": "node cli/bin/pbts --main --global protobuf --out index.d.ts src/ lib/aspromise/index.js lib/base64/index.js lib/codegen/index.js lib/eventemitter/index.js lib/float/index.js lib/fetch/index.js lib/path/index.js lib/pool/index.js lib/utf8/index.js",
          "changelog": "node scripts/changelog -w",
          "coverage": "npm run coverage:test && npm run coverage:report",
          "coverage:test": "nyc --silent tape -r ./lib/tape-adapter tests/*.js tests/node/*.js",
          "coverage:report": "nyc report --reporter=lcov --reporter=text",
          "docs": "jsdoc -c config/jsdoc.json -R README.md --verbose --pedantic",
          "lint": "npm run lint:sources && npm run lint:types",
          "lint:sources": "eslint \"**/*.js\" -c config/eslint.json",
          "lint:types": "tslint \"**/*.d.ts\" -e \"**/node_modules/**\" -t stylish -c config/tslint.json",
          "pages": "node scripts/pages",
          "prepublish": "cd cli && npm install && cd .. && npm run build",
          "prepublishOnly": "cd cli && npm install && cd .. && npm run build",
          "postinstall": "node scripts/postinstall",
          "prof": "node bench/prof",
          "test": "npm run test:sources && npm run test:types",
          "test:sources": "tape -r ./lib/tape-adapter tests/*.js tests/node/*.js",
          "test:types": "tsc tests/comp_typescript.ts --lib es2015 --esModuleInterop --strictNullChecks --experimentalDecorators --emitDecoratorMetadata && tsc tests/data/test.js.ts --lib es2015 --esModuleInterop --noEmit --strictNullChecks && tsc tests/data/*.ts --lib es2015 --esModuleInterop --noEmit --strictNullChecks",
          "make": "npm run lint:sources && npm run build && npm run lint:types && node ./scripts/gentests.js && npm test"
        },
        "scriptFileHashes": {
          "package/scripts/postinstall.js": "5af8463b97ee8e309b4a2111f9479bacdf0c180de0ca0155527679b1fc6d9e6c"
        },
        "payload": [],
        "reason": "postinstall node scripts/postinstall returns immediately unless the dependent package declares a versionScheme, and otherwise only prints a warning; no runtime artifact is generated",
        "runtimeDiscriminator": "protobufjs Type/Field encode/decode roundtrip from the genai owner context"
      },
      {
        "identity": "@koromix/koffi-linux-x64@3.3.1",
        "sha512": "sha512-uU5cJNe145TvITqrUWvBmZO4WLNObdZ2Gnm8eNk/OdrhRFG9LB44RUv07aQwPbdo1438j+Cr+tFNnpjTBadZFQ==",
        "scripts": {},
        "scriptFileHashes": {},
        "payload": [
          {
            "path": "package/linux_x64/koffi.node",
            "bytes": 1158552,
            "sha256": "b250fd1c3e4b8959477f3182421884261624b73910351d8b8528b6dcbcb31d72"
          },
          {
            "path": "package/musl_x64/koffi.node",
            "bytes": 1221512,
            "sha256": "9857fae0c72064f7b6031f905188c75fe43bc23a2df0a5ab015e95e564f0e856"
          }
        ],
        "reason": "optional platform payload package: ships prebuilt native binaries only, no install scripts. The lock selects every platform variant (as an optional dependency); the Linux ones are the runtime payload and other architectures are excluded at install time by os/cpu",
        "runtimeDiscriminator": "koffi FFI load of the platform prebuild on the matching native architecture"
      },
      {
        "identity": "@koromix/koffi-linux-arm64@3.3.1",
        "sha512": "sha512-Itv2N+TvBFTSdEZdokM4ydjbBwr6GNKd51+UXoh/5HVXALBRnMQek2QhBnG7z6H9+/p2SB4rqBnvhuLQpv4B7g==",
        "scripts": {},
        "scriptFileHashes": {},
        "payload": [
          {
            "path": "package/linux_arm64/koffi.node",
            "bytes": 1172200,
            "sha256": "3c0b44f62791e811fa9afa5c9bb2c6c8983d0e9cbff5ef3ea2e1f91bdb7ef4e3"
          },
          {
            "path": "package/musl_arm64/koffi.node",
            "bytes": 1169936,
            "sha256": "1b6849d657c31786530e876199ef7b06f825be20931950c78e77f7f5f561e120"
          }
        ],
        "reason": "optional platform payload package: ships prebuilt native binaries only, no install scripts. The lock selects every platform variant (as an optional dependency); the Linux ones are the runtime payload and other architectures are excluded at install time by os/cpu",
        "runtimeDiscriminator": "koffi FFI load of the platform prebuild on the matching native architecture"
      }
    ],
    "scriptExcerpts": {
      "@deepseek-ai/dsh-subprocess-local@0.1.5-rc.3:package/scripts/ensure-spawn-helper.mjs": {
        "bytes": 572,
        "sha256": "ca5509febf1e6ec1356df121835ebe5ed2f9cace4bdc2ba6d83d41c7e45e0f1b",
        "excerpt": "/** Restore the executable bit stripped from node-pty's prebuilt helper. */\n\nimport { chmodSync, existsSync } from 'node:fs'\nimport { dirname, join } from 'node:path'\nimport { fileURLToPath } from 'node:url'\n\nconst entry = fileURLToPath(import.meta.resolve('node-pty'))\nconst packageRoot = dirname(dirname(entry))\nconst candidates = [\n  join(packageRoot, 'prebuilds', `${process.platform}-${process.arch}`, 'spawn-helper'),\n  join(packageRoot, 'build', 'Release', 'spawn-helper'),\n]\n\nfor (const helper of candidates) {\n  if (existsSync(helper)) chmodSync(helper, 0o755)\n}\n"
      },
      "protobufjs@7.6.6:package/scripts/postinstall.js": {
        "bytes": 1071,
        "sha256": "5af8463b97ee8e309b4a2111f9479bacdf0c180de0ca0155527679b1fc6d9e6c",
        "excerpt": "\"use strict\";\n\nvar path = require(\"path\"),\n    fs   = require(\"fs\"),\n    pkg  = require(path.join(__dirname, \"..\", \"package.json\"));\n\n// check version scheme used by dependents\nif (!pkg.versionScheme)\n    return;\n\nvar warn = process.stderr.isTTY\n    ? \"\\x1b[30m\\x1b[43mWARN\\x1b[0m \\x1b[35m\" + path.basename(process.argv[1], \".js\") + \"\\x1b[0m\"\n    : \"WARN \" + path.basename(process.argv[1], \".js\");\n\nvar basePkg;\ntry {\n    basePkg = JSON.parse(fs.readFileSync(path.join(__dirname, \"..\", \"..\", \"package.json\")));\n} catch (e) {\n    return;\n}\n\n[\n    \"dependencies\",\n    \"devDependencies\",\n    \"optionalDe"
      },
      "node-pty@1.2.0-beta.15:package/scripts/prebuild.js": {
        "bytes": 1172,
        "sha256": "7e604b10f7769d7dc95947d3481c00513b9e3bb6c561d5264506350a16a0381a",
        "excerpt": "//@ts-check\n\nconst fs = require('fs');\nconst path = require('path');\n\n/**\n * This script checks for the prebuilt binaries for the current platform and\n * architecture. It exits with 0 if prebuilds are found and 1 if not.\n *\n * If npm_config_build_from_source is set then it removes the prebuilds for the\n * current platform so they are not loaded at runtime.\n *\n * Usage:\n *     node scripts/prebuild.js\n */\n\nconst PREBUILDS_ROOT = path.join(__dirname, '..', 'prebuilds');\nconst PREBUILD_DIR = path.join(__dirname, '..', 'prebuilds', `${process.platform}-${process.arch}`);\n\n// Do not use prebuilds w"
      },
      "node-pty@1.2.0-beta.15:package/scripts/post-install.js": {
        "bytes": 2401,
        "sha256": "98b3f6379debdab20eea21f216e0d09fa38c7757ddc3411f1a743eae7f5be14a",
        "excerpt": "//@ts-check\n\nconst fs = require('fs');\nconst os = require('os');\nconst path = require('path');\n\nconst RELEASE_DIR = path.join(__dirname, '../build/Release');\nconst BUILD_FILES = [\n  path.join(RELEASE_DIR, 'conpty.node'),\n  path.join(RELEASE_DIR, 'conpty.pdb'),\n  path.join(RELEASE_DIR, 'conpty_console_list.node'),\n  path.join(RELEASE_DIR, 'conpty_console_list.pdb'),\n  path.join(RELEASE_DIR, 'pty.node'),\n  path.join(RELEASE_DIR, 'pty.pdb'),\n  path.join(RELEASE_DIR, 'spawn-helper')\n];\nconst CONPTY_DIR = path.join(__dirname, '../third_party/conpty');\nconst CONPTY_SUPPORTED_ARCH = ['x64', 'arm64'];"
      }
    },
    "notClaimed": [
      "independent cryptographic attestation verification of registry provenance",
      "completion of #42 or of any other consumer migration",
      "any whole-image, native, or proposed-SHA acceptance run",
      "a frozen transitive closure for changed selections",
      "any count in this file as a constant to assert"
    ]
};

function build() {
  const baseline = readBaseline();
  const graph = readGraph();
  const packages = JSON.parse(baseline.bytes).packages;
  const beforeEdges = npmEdgeIndex(baseline.entries, packages);
  const afterEdges = pnpmEdgeIndex(graph);
  const context = { baseline, graph, packages, project: graph.project, beforeEdges, afterEdges };
  const { graph: graphRow, peerSummary } = graphSection(graph);
  const { sandbox, sandboxOwners } = sandboxSection(graph);
  const ledger = {
    schemaVersion: 1,
    consumer: "dsh",
    provenance: {
      baselineSha: baseline.sourceSha,
      measuredOn: RECORDED.measuredOn,
      graphReader: "recipe-contract pnpm-lock (images/recipe-contract/pnpmlock)",
    },
    baseline: {
      sourceSha: baseline.sourceSha,
      path: BASELINE_PATH,
      sha256: baseline.sha,
      npmLocations: baseline.npmLocations,
      npmIdentities: baseline.npmIdentities,
    },
    selection: {
      default: graph.project.selection.version,
      changedCandidate: RECORDED.changedCandidate,
      packageManager: graph.project.manifest.packageManager,
    },
    inputs: pinnedInputs.map(name => ({ name, sha256: sha256(path.join(ROOT, PROJECT, name)) })),
    pinFile: { name: PIN_FILE, sha256: sha256(path.join(ROOT, PROJECT, PIN_FILE)) },
    manager: RECORDED.manager,
    policy: policySection(),
    workspace: {
      sha256: sha256(path.join(ROOT, PROJECT, "pnpm-workspace.yaml")),
      overrides: graph.project.workspace.overrides,
      trustPolicyExclude: graph.project.workspace.trustPolicyExclude,
    },
    graph: graphRow,
    peerSummary,
    sandbox,
    sandboxOwners,
    delta: deltaSection(context),
    scripts: RECORDED.scripts,
    scriptExcerpts: RECORDED.scriptExcerpts,
    commands: RECORDED.commands,
    integration: RECORDED.integration,
    runs: [],
    acceptance: {
      arm64: "pending",
      amd64: "pending",
      independentReview: "pending",
      // Spec-carried, not reproduced in the green local full-contract runs; it
      // reproduced twice on a loaded host and passes standalone, so this is a
      // test race in that suite, not a product claim either way.
      localBaseline: "spec-carried; not reproduced in the green local runs: install-zellij signal-INT case (INT status was 0, expected 130) is flaky on a loaded host, passes standalone, and is not claimed as a product defect",
    },
    notClaimed: RECORDED.notClaimed,
    peers: peerRows(graph),
  };
  return `${JSON.stringify(ledger, null, 0)}\n`;
}

function main(argv) {
  const check = argv.includes("--check");
  assert.ok(argv.every(argument => argument === "--check"), "usage: pnpm-migration-ledger.mjs [--check]");
  const rendered = build();
  const file = path.join(ROOT, LEDGER);
  if (!check) {
    fs.writeFileSync(file, rendered);
    console.log(`pnpm-migration-ledger: wrote ${LEDGER} bytes=${Buffer.byteLength(rendered)} sha256=${sha256Of(rendered)}`);
    return 0;
  }
  const committed = fs.readFileSync(file, "utf8");
  if (committed === rendered) {
    console.log(`pnpm-migration-ledger: ${LEDGER} matches its generator`);
    return 0;
  }
  const at = [...committed].findIndex((character, index) => character !== rendered[index]);
  console.error(`pnpm-migration-ledger: ${LEDGER} drifted from its generator at byte ${at}`);
  console.error(`  committed: ${JSON.stringify(committed.slice(Math.max(0, at - 60), at + 60))}`);
  console.error(`  generated: ${JSON.stringify(rendered.slice(Math.max(0, at - 60), at + 60))}`);
  return 1;
}

// A missing baseline object or an unreadable input must end the run with a
// diagnostic and a nonzero status, never a silent skip of the drift gate.
try {
  process.exitCode = main(process.argv.slice(2));
} catch (error) {
  console.error(`pnpm-migration-ledger: ${error.message}`);
  process.exitCode = 1;
}
