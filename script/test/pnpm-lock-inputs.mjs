// Independent fixture/CLI oracles; no package manager or production pin writer for negatives.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
// Independent fixture/CLI oracle. The graph reader is the shipped helper, spawned
// as a CLI, so no in-process module is shared with the parser under test.
const fixtures = fileURLToPath(new URL("fixtures/pnpm-lock/", import.meta.url));
const bin = (() => {
  const adapter = fileURLToPath(new URL("../../script/build/recipe-contract.sh", import.meta.url));
  const result = spawnSync("bash", [adapter, "--native"], { encoding: "utf8", timeout: 600000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  return path.join(result.stdout.trim(), "out", "recipe-contract");
})();
const pinnedInputs = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];
const files = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];
const pinFile = "pnpm-inputs.sha256";
const root = "@deepseek-ai/dsh";
const cordis = "@deepseek-ai/cordis";
const loader = "@deepseek-ai/cordis-plugin-loader";
const loaderKey = `${loader}@1.0.3(${cordis}@4.0.2)`;
const work = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "pnpm-lock-inputs-")));
let counter = 0;
const json = file => JSON.parse(fs.readFileSync(file, "utf8"));
function copy(fixture = "tiny-dsh") {
  const prefix = path.join(work, String(counter++));
  fs.cpSync(path.join(fixtures, fixture), prefix, { recursive: true });
  return prefix;
}
function pins(prefix) {
  fs.writeFileSync(path.join(prefix, pinFile), files.map(file => `${crypto.createHash("sha256").update(fs.readFileSync(path.join(prefix, file))).digest("hex")}  ${file}\n`).join(""));
}
function edit(prefix, file, change) {
  const value = json(path.join(prefix, file));
  change(value);
  fs.writeFileSync(path.join(prefix, file), JSON.stringify(value));
}
function inventory(prefix) {
  return fs.readdirSync(prefix).sort().map(file => [file, fs.readFileSync(path.join(prefix, file)).toString("base64")]);
}
// The graph reader is a CLI now, so every oracle assertion is out-of-process.
function graphOf(prefix) { return JSON.parse(run(["graph", "dsh", prefix], 0)); }
function peersOf(prefix) { return JSON.parse(run(["peers", "dsh", prefix], 0)); }
function deriveOrThrow(prefix, options = {}) {
  const args = ["dsh-cordis-overrides", prefix];
  if (options.before) args.push("--before", options.before);
  if (options.approved) args.push("--cordis-overrides", options.approved);
  if (options.propose) args.push("--propose", options.propose);
  const result = spawnSync(bin, args, { encoding: "utf8", timeout: 20000 });
  if (result.status !== 0) {
    const error = new Error(result.stderr.trim());
    error.status = result.status;
    throw error;
  }
  return JSON.parse(result.stdout);
}
function approvedFile(map) {
  const file = path.join(work, `approved-${counter++}.json`);
  fs.writeFileSync(file, JSON.stringify(map));
  return file;
}
// A committed project whose override map is the caller's: the no-op re-pin of an
// unchanged upgrade is decided by the map the graph was committed with.
function committedPrefix(map) {
  const prefix = copy("update-before");
  edit(prefix, "pnpm-workspace.yaml", w => { w.overrides = map; });
  edit(prefix, "pnpm-lock.yaml", l => { l.overrides = map; });
  pins(prefix);
  return prefix;
}

function run(args, status) {
  const result = spawnSync(bin, ["pnpm-lock", ...args], { encoding: "utf8", timeout: 20000, maxBuffer: 32 * 1024 * 1024 });
  assert.ifError(result.error);
  assert.equal(result.status, status, `${args}: ${result.stderr}`);
  if (status) {
    assert.equal(result.stdout, "", "failure leaked success payload");
    assert.ok(result.stderr.length > 0, "failure missing diagnostic");
  } else assert.equal(result.stderr, "", "success leaked diagnostics");
  return result.stdout;
}
function invalid(change, diagnostic, fixture = "tiny-dsh") {
  const prefix = copy(fixture);
  change(prefix);
  // Negative checks are repeated after independently re-pinning unsafe input.
  for (const repin of [false, true]) {
    if (repin) pins(prefix);
    for (const verb of ["check", "graph", "pin"]) {
      const before = inventory(prefix);
      const result = spawnSync(bin, ["pnpm-lock", verb, fixture === "real-bridge" ? "bridge" : "dsh", prefix], { encoding: "utf8", timeout: 20000 });
      assert.ifError(result.error);
      assert.equal(result.status, 1, `${verb}: negative survived: ${result.stderr}`);
      assert.equal(result.stdout, "");
      assert.ok(result.stderr.length);
      if (repin || verb === "pin") assert.match(result.stderr, diagnostic);
      assert.deepEqual(inventory(prefix), before, "failed validation/pin changed project");
    }
  }
}

try {
  for (const fixture of ["tiny-dsh", "tiny-dsh-previous"]) {
    const prefix = copy(fixture);
    const expected = json(path.join(fixtures, `${fixture}-expected.json`));
    assert.equal(run(["check", "dsh", prefix], 0), expected.selection.version + "\n");
    assert.equal(run(["graph", "dsh", prefix], 0), JSON.stringify(expected) + "\n");
    const before = inventory(prefix).filter(([file]) => file !== pinFile);
    fs.writeFileSync(path.join(prefix, pinFile), "broken previous pins\n");
    assert.equal(run(["pin", "dsh", prefix], 0), "");
    assert.deepEqual(inventory(prefix).filter(([file]) => file !== pinFile), before);
    const actualPins = fs.readFileSync(path.join(prefix, pinFile), "utf8");
    pins(prefix);
    assert.equal(actualPins, fs.readFileSync(path.join(prefix, pinFile), "utf8"));
    // The pinned project re-reads clean through the public CLI: `check` for the
    // structure and exact peers, `peers` for the numbers.
    run(["check", "dsh", prefix], 0);
    assert.deepEqual(JSON.parse(run(["peers", "dsh", prefix], 0)).unsatisfied, []);
  }
  const real = copy("real-bridge");
  {
    // A symlinked entrypoint must still dispatch: an argv[0] reached through a
    // symlink may not silently skip the command and exit 0 with no output.
    const prefix = copy("tiny-dsh");
    const linked = path.join(work, "recipe-contract-link");
    fs.symlinkSync(bin, linked);
    const expected = json(path.join(fixtures, "tiny-dsh-expected.json"));
    const ok = spawnSync(linked, ["pnpm-lock", "check", "dsh", prefix], { encoding: "utf8" });
    assert.ifError(ok.error);
    assert.equal(ok.status, 0, ok.stderr);
    assert.equal(ok.stdout, expected.selection.version + "\n");
    fs.appendFileSync(path.join(prefix, "pnpm-workspace.yaml"), "\n");
    const bad = spawnSync(linked, ["pnpm-lock", "check", "dsh", prefix], { encoding: "utf8" });
    assert.ifError(bad.error);
    assert.equal(bad.status, 1, "symlinked invocation silently ignored a failing check");
    assert.equal(bad.stdout, "");
    assert.match(bad.stderr, /bytes differ from reviewed pin/);
  }
  const evidence = json(path.join(fixtures, "real-bridge-evidence.json"));
  assert.equal(run(["check", "bridge", real], 0), "0.1.0\n");
  const realGraph = JSON.parse(run(["graph", "bridge", real], 0));
  assert.deepEqual(realGraph.selection, evidence.selection);
  assert.deepEqual(realGraph.importer, evidence.importer);
  for (const vector of [evidence.archive, evidence.scopedArchive]) {
    const pkg = realGraph.packages.find(pkg => pkg.id.name === vector.name && pkg.id.version === vector.version);
    assert.equal(pkg.integrity, vector.integrity);
  }
  assert.deepEqual(realGraph.snapshots.find(snapshot => snapshot.id.key === evidence.snapshot).edges, evidence.edges);
  assert.ok(realGraph.snapshots.find(snapshot => snapshot.id.key === evidence.optionalSnapshot).edges.some(edge => JSON.stringify(edge) === JSON.stringify(evidence.optionalEdge)));
  assert.equal(run(["pin", "bridge", real], 0), "");

  for (const verb of ["check", "graph", "pin"]) {
    for (const args of [[verb], [verb, "dsh"], [verb, "unknown", real], [verb, "dsh", real, "extra"]]) run(args, 2);
    run([verb, "dsh", path.join(work, "absent")], 1);
    const prefix = copy();
    fs.unlinkSync(path.join(prefix, "package.json"));
    const before = inventory(prefix);
    run([verb, "dsh", prefix], 1);
    assert.deepEqual(inventory(prefix), before);
  }
  for (const args of [[], ["wat", "dsh", real], ["--help"], ["check", "copilot", real]]) run(args, 2);

  for (const [file, change, message] of [
    ["package.json", m => m.dependencies[root] = "latest", /semver/],
    ["package.json", m => m.packageManager = "pnpm@12.9.1", /11.11.0/],
    ["package.json", m => m.scripts = { install: "bad" }, /unexpected key/],
    ["package.json", m => m.devDependencies = {}, /unexpected key/],
    ["package.json", m => m.workspaces = ["*"], /unexpected key/],
    ["package.json", m => m.dependencies.other = "1.0.0", /unexpected key/],
    ["pnpm-lock.yaml", l => l.lockfileVersion = "6.0", /v9/],
    ["pnpm-lock.yaml", l => l.importers.other = {}, /unexpected key/],
    ["pnpm-lock.yaml", l => l.importers["."].dependencies[root].specifier = "0.1.5", /specifier/],
    ["pnpm-lock.yaml", l => l.importers["."].dependencies[root].version = "0.1.5", /resolution/],
    ["pnpm-lock.yaml", l => delete l.snapshots[`${cordis}@4.0.2`], /dangling/],
    ["pnpm-lock.yaml", l => l.snapshots["orphan@1.0.0"] = {}, /archive/],
    ["pnpm-lock.yaml", l => l.packages["orphan@1.0.0"] = l.packages[`${cordis}@4.0.2`], /orphan/],
    ["pnpm-lock.yaml", l => l.packages[`${cordis}@4.0.2`].resolution.integrity = "sha512-AAAA", /sha512/],
    ["pnpm-lock.yaml", l => l.packages[`${cordis}@4.0.2`].resolution.tarball = "https://evil.invalid/a.tgz", /npmjs/],
    ["pnpm-lock.yaml", l => l.snapshots[loaderKey].dependencies[cordis] = "file:../bad", /semver/],
    ["pnpm-lock.yaml", l => l.snapshots[loaderKey].dependencies[cordis] = "npm:other@4.0.2", /semver/],
    ["pnpm-lock.yaml", l => l.snapshots[loaderKey].optionalDependencies = { [cordis]: "4.0.2" }, /ambiguous/],
  ]) invalid(prefix => edit(prefix, file, change), message);

  const workspace = json(path.join(fixtures, "tiny-dsh/pnpm-workspace.yaml"));
  for (const field of Object.keys(workspace)) {
    invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => delete w[field]), /missing|mapping/);
    invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => w[field] = "unsafe"), /policy|mapping/);
  }
  for (const [key, value] of Object.entries({ trustLockfile: true, ignoreScripts: false, minimumReleaseAge: 0, minimumReleaseAgeExclude: ["*"], dangerous: true }))
    invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => w[key] = value), /unexpected key/);
  invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => w.allowBuilds.anything = true), /policy/);
  invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => w.overrides.anything = "1.0.0"), /unexpected key/);
  invalid(prefix => edit(prefix, "pnpm-workspace.yaml", w => w.overrides[Object.keys(w.overrides)[0]] = "^4"), /semver/);
  for (const file of [".npmrc", ".pnpmfile.cjs", "package-lock.json"]) invalid(prefix => fs.writeFileSync(path.join(prefix, file), "{}"), /unreviewed/);
  for (const file of [".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs"]) invalid(prefix => fs.writeFileSync(path.join(prefix, file), "{}"), /unreviewed/);
  for (const file of ['.npmrc','package-lock.json']) {
    const prefix = copy();
    fs.symlinkSync(path.join(work,'absent'),path.join(prefix,file));
    for (const verb of ['check','graph','pin']) run([verb,'dsh',prefix],1);
    assert.ok(fs.lstatSync(path.join(prefix,file)).isSymbolicLink());
  }
  // A pinned input reached through a symlink is substituted bytes, not reviewed bytes.
  for (const file of files) {
    const prefix = copy();
    fs.renameSync(path.join(prefix, file), path.join(prefix, file + ".real"));
    fs.symlinkSync(file + ".real", path.join(prefix, file));
    for (const verb of ["check", "graph", "pin"]) {
      const before = inventory(prefix);
      run([verb, "dsh", prefix], 1);
      assert.deepEqual(inventory(prefix), before, `symlinked ${file}: ${verb} changed the project`);
    }
  }

  for (const source of [
    "x: 1\nx: 2\n", "x: &a {}\ny: *a\n", "x: !!str hello\n", "<<: {}\n", "1: hello\n",
    "x: 1\n---\ny: 2\n", "[truncated", "x: [" + "[".repeat(101) + "0" + "]".repeat(102),
    "x: " + "a".repeat(16 * 1024 * 1024),
    '{"__proto__":{"polluted":true}}',
  ]) invalid(prefix => fs.writeFileSync(path.join(prefix, "pnpm-workspace.yaml"), source), /pnpm-lock:/);
  const validWorkspace = JSON.stringify(workspace);
  for (const [source, diagnostic] of [
    [validWorkspace.replace('"hoist":true', '"hoist":true,"hoist":true'), /unique/],
    [validWorkspace.replace('"packages":["."]', '"packages": &a ["."]'), /anchors/],
    [validWorkspace.replace('"autoInstallPeers":true', '"autoInstallPeers": &a true').replace('"hoist":true', '"hoist": *a'), /anchors|aliases/],
    [validWorkspace.replace('"hoist":true', '"hoist": !!bool true'), /tags/],
    [validWorkspace.replace('"hoist":true', '"<<": {"hoist":true},"hoist":true'), /merge key/],
    [validWorkspace.replace('"hoist":true', '1: true,"hoist":true'), /keys must be strings/],
    [validWorkspace + " ".repeat(16 * 1024 * 1024), /16 MiB/],
    [validWorkspace.replace('"allowBuilds":{}', '"allowBuilds":' + '['.repeat(101) + '0' + ']'.repeat(101)), /depth100/],
    [validWorkspace.slice(0, -1), /flow map|end|expected/i],
  ]) invalid(prefix => fs.writeFileSync(path.join(prefix, "pnpm-workspace.yaml"), source), diagnostic);
  invalid(prefix => {
    const file = path.join(prefix, "package.json");
    fs.writeFileSync(file, fs.readFileSync(file, "utf8").replace('"name":', '"name":"earlier-name","name":'));
  }, /unique/);
  invalid(prefix => fs.writeFileSync(path.join(prefix, "pnpm-workspace.yaml"), Buffer.from([0xff, 0xfe, 0xff])), /encoded data|encoding/i);
  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => l.packages[`${loader}@1.0.3`].peerDependencies[cordis] = "file:../bad"), /non-registry peer/);
  assert.equal({}.polluted, undefined);

  // The maintenance writer must not follow a destination symlink or truncate
  // a hardlinked external file. Its invalid project paths remain byte-preserving.
  const symlinkPrefix = copy();
  const outsidePin = path.join(work, "outside-pin");
  fs.writeFileSync(outsidePin, "external bytes");
  fs.unlinkSync(path.join(symlinkPrefix, pinFile));
  fs.symlinkSync(outsidePin, path.join(symlinkPrefix, pinFile));
  const beforeSymlink = inventory(symlinkPrefix);
  run(["pin", "dsh", symlinkPrefix], 1);
  assert.deepEqual(inventory(symlinkPrefix), beforeSymlink);
  assert.equal(fs.readFileSync(outsidePin, "utf8"), "external bytes");
  const hardlinkPrefix = copy();
  fs.unlinkSync(path.join(hardlinkPrefix, pinFile));
  fs.linkSync(outsidePin, path.join(hardlinkPrefix, pinFile));
  run(["pin", "dsh", hardlinkPrefix], 0);
  assert.equal(fs.readFileSync(outsidePin, "utf8"), "external bytes");
  for (const suffix of ["(", ")", "()", "(bad)", "(a@1.0.0)(a@2.0.0)", "(a@file:bad)", "(a@1.0.0))"]) {
    invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
      l.snapshots[`${root}@0.1.5-rc.2${suffix}`] = l.snapshots[`${root}@0.1.5-rc.2`];
    }), /malformed|ambiguous|semver/);
  }
  for (const change of [
    body => body + body.split("\n")[0] + "\n",
    body => body.replace("package.json", "../package.json"),
    body => body.replace(/^[^\n]+\n/, ""),
    body => body + "0".repeat(64) + "  extra.yaml\n",
    body => body.replace(/^[0-9a-f]/, "G"),
    body => body.trimEnd() + "  \n",
  ]) {
    const prefix = copy();
    fs.writeFileSync(path.join(prefix, pinFile), change(fs.readFileSync(path.join(prefix, pinFile), "utf8")));
    for (const verb of ["check", "graph"]) run([verb, "dsh", prefix], 1);
  }
  // Exact peers: unrelated correct archive does not rescue wrong effective context.
  for (const vector of json(path.join(fixtures, "peer-vectors.json"))) {
    const prefix = copy();
    if (!vector.valid) {
      edit(prefix, "pnpm-lock.yaml", l => {
        l.packages[`${cordis}@4.0.4`] = l.packages[`${cordis}@4.0.2`];
        l.snapshots[`${cordis}@4.0.4`] = {};
        l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.2";
        l.snapshots[vector.snapshot] = { dependencies: { [cordis]: vector.resolved } };
        delete l.snapshots[loaderKey];
        l.snapshots[`${root}@0.1.5-rc.2`].dependencies[loader] = `1.0.3(${cordis}@4.0.4)`;
      });
      pins(prefix);
      const result = spawnSync(bin, ["pnpm-lock", "check", "dsh", prefix], { encoding: "utf8" });
      assert.equal(result.status, 1);
      assert.match(result.stderr, /peer @deepseek-ai\/cordis wanted 4.0.2, got .*4.0.4/);
    } else run(["check", "dsh", prefix], 0);
  }
  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.2";
    delete l.snapshots[loaderKey].dependencies[cordis];
  }), /wanted 4.0.2, got missing/);
  // The same graph through the non-throwing report: a negative leg can state both
  // numbers and only call it a peer rejection when a declaration is unmet.
  const unmet = copy();
  edit(unmet, "pnpm-lock.yaml", l => {
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.2";
    delete l.snapshots[loaderKey].dependencies[cordis];
  });
  pins(unmet);
  const unmetReport = peersOf(unmet);
  assert.equal(unmetReport.unsatisfied.length, 1, unmetReport);
  assert.match(unmetReport.unsatisfied[0], /peer @deepseek-ai\/cordis wanted 4\.0\.2, got missing/);
  assert.ok(unmetReport.exact > 0 && unmetReport.cordisExact > 0, unmetReport);
  // The negative leg's shape: a graph whose committed overrides are absent has no
  // published reader, so the report reader must still describe it.
  const overrideFreeReport = peersOf(copy('derivation'));
  assert.ok(overrideFreeReport.exact > 0 && overrideFreeReport.cordisExact > 0, overrideFreeReport);
  const intact = copy();
  const intactReport = peersOf(intact);
  assert.deepEqual(intactReport.unsatisfied, []);
  run(["check", "dsh", intact], 0);
  assert.deepEqual(intactReport, {exact:1,totalOptionalMissing:0,cordisExact:1,unsatisfied:[]});
  // Peer specifications are classified, never silently skipped: `=X`/`vX` and
  // surrounding whitespace are exact pins, the explicit range grammar is a range,
  // and any other syntax stops the check instead of escaping the exact-peer oracle.
  function peerSpec(spec, status) {
    // The sandbox archive carries the vector peer and a matching edge, while the
    // loader keeps the graph's required exact Cordis peer: this vector is about the
    // specification class alone.
    const prefix = copy();
    edit(prefix, "pnpm-lock.yaml", l => {
      l.packages['@deepseek-ai/dsh-sandbox-local@1.0.0'].peerDependencies = { [cordis]: spec };
      l.snapshots['@deepseek-ai/dsh-sandbox-local@1.0.0'].dependencies = { [cordis]: '4.0.2' };
    });
    pins(prefix);
    const result = spawnSync(bin, ["pnpm-lock", "check", "dsh", prefix], { encoding: "utf8" });
    assert.equal(result.status, status, `${JSON.stringify(spec)}: ${result.stderr}`);
    return result;
  }
  for (const spec of ["=4.0.2", "= 4.0.2", "=  4.0.2", "v4.0.2", " v4.0.2 ", " 4.0.2 ", "^4.0.2", "~4.0.2", ">=4.0.0 <5.0.0", "4.x", "4", "*", "x", "^4.0.2 || ^5.0.0", "4.0.0 - 4.1.0"]) peerSpec(spec, 0);
  // `= 4.0.3` is the same exact declaration as `=4.0.3`: a spaced comparator may
  // not fall through to the range grammar and skip the resolved 4.0.2.
  for (const spec of ["=4.0.3", "= 4.0.3", "=  4.0.3", "v4.0.3", " = 4.0.3 "]) assert.match(peerSpec(spec, 1).stderr, /wanted\s*=?\s*v?4\.0\.3\s*, got @deepseek-ai\/cordis@4\.0\.2/);
  // Real ranges stay ranges: the same declaration satisfied by 4.0.2 passes.
  for (const spec of ["^4.0.2", "~4.0.2", ">=4.0.0 <5.0.0", "4.x", "4.0.0 - 4.1.0"]) peerSpec(spec, 0);
  for (const spec of ["nonsense", "4.0.2 ||", ">=", "v4", "=", "~>4.0.2", "4.0.2, 4.0.3"]) {
    assert.match(peerSpec(spec, 1).stderr, /unknown peer specification/);
  }
  const optional = copy();
  edit(optional, "pnpm-lock.yaml", l => {
    l.packages[`${loader}@1.0.3`].peerDependenciesMeta = { [cordis]: { optional: true } };
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[loader] = "1.0.3";
    l.snapshots[`${loader}@1.0.3`] = {};
    delete l.snapshots[loaderKey]; delete l.snapshots[`${cordis}@4.0.2`]; delete l.packages[`${cordis}@4.0.2`];
  });
  pins(optional); run(["check", "dsh", optional], 0);
  assert.deepEqual(satisfiedSummary(peersOf(optional)), {exact:1,totalOptionalMissing:1,cordisExact:1,unsatisfied:0});

  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    delete l.packages[`${loader}@1.0.3`].peerDependencies;
  }), /exact Cordis peers/);
  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    l.packages[`${loader}@1.0.3`].peerDependenciesMeta = { [cordis]: { optional: true } };
    l.packages[`${cordis}@4.0.4`] = l.packages[`${cordis}@4.0.2`];
    l.snapshots[`${cordis}@4.0.4`] = {};
    l.snapshots[loaderKey].dependencies[cordis] = "4.0.4";
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.2";
  }), /wanted 4.0.2, got .*4.0.4/);
  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[loader] = `1.0.3(${cordis}@9.0.0)`;
    l.snapshots[`${loader}@1.0.3(${cordis}@9.0.0)`] = l.snapshots[loaderKey];
    delete l.snapshots[loaderKey];
  }), /dangling peer context/);

  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    l.packages[`${cordis}@4.0.4`] = l.packages[`${cordis}@4.0.2`];
    l.snapshots[`${cordis}@4.0.4`] = {};
    const wrongContext = `${loader}@1.0.3(${cordis}@4.0.4)`;
    l.snapshots[wrongContext] = l.snapshots[loaderKey];
    delete l.snapshots[loaderKey];
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[loader] = `1.0.3(${cordis}@4.0.4)`;
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.4";
  }), /ambiguous context/);

  // Every closed Pi/bridge policy field is exercised after independent pinning too.
  for (const kind of ["pi", "bridge"]) {
    const prefix = copy();
    const target = kind === "pi" ? "@earendil-works/pi-coding-agent" : "pi-claude-bridge";
    for (const file of ["package.json", "pnpm-lock.yaml"])
      fs.writeFileSync(path.join(prefix, file), fs.readFileSync(path.join(prefix, file), "utf8").replaceAll(root, target));
    edit(prefix, "pnpm-lock.yaml", l => { delete l.overrides; l.settings.autoInstallPeers = kind === "pi"; });
    const policy = {
      packages: ["."], autoInstallPeers: kind === "pi", strictPeerDependencies: false,
      enableGlobalVirtualStore: false, pmOnFail: "error", sideEffectsCache: false,
      allowBuilds: {}, extendNodePath: false,
      ...(kind === "pi" ? { nodeLinker: "isolated", hoist: false, preferSymlinkedExecutables: true, trustPolicyExclude: ["undici-types@6.21.0"] }
        : { nodeLinker: "hoisted", ignoredOptionalDependencies: ["@anthropic-ai/claude-agent-sdk-*"] }),
    };
    fs.writeFileSync(path.join(prefix, "pnpm-workspace.yaml"), JSON.stringify(policy));
    pins(prefix); run(["check", kind, prefix], 0);
    const original = fs.readFileSync(path.join(prefix, "pnpm-workspace.yaml"));
    for (const key of Object.keys(policy)) {
      edit(prefix, "pnpm-workspace.yaml", w => w[key] = "unsafe");
      pins(prefix);
      for (const verb of ["check", "graph", "pin"]) {
        const before = inventory(prefix);
        run([verb, kind, prefix], 1);
        assert.deepEqual(inventory(prefix), before);
      }
      fs.writeFileSync(path.join(prefix, "pnpm-workspace.yaml"), original);
    }
    pins(prefix); run(["check", kind, prefix], 0);
  }

  // Seeded deterministic scoped identity and harmless map-order permutations.
  let seed = 42;
  for (let index = 0; index < 16; index++) {
    seed = (1664525 * seed + 1013904223) >>> 0;
    const prefix = copy();
    const expected = json(path.join(fixtures, "tiny-dsh-expected.json"));
    const scoped = `@scope${seed}/package-${index}`;
    const peer = `@scope${seed}/peer-${index}`;
    const context = `${scoped}@1.2.3(${peer}@2.3.4)`;
    edit(prefix, "pnpm-lock.yaml", l => {
      l.packages[`${scoped}@1.2.3`] = { resolution: { integrity: expected.packages[0].integrity }, peerDependencies: { [peer]: "2.3.4" } };
      l.packages[`${peer}@2.3.4`] = { resolution: { integrity: expected.packages[0].integrity } };
      l.snapshots[context] = { dependencies: { [peer]: "2.3.4" } };
      l.snapshots[`${peer}@2.3.4`] = {};
      l.snapshots[`${root}@0.1.5-rc.2`].optionalDependencies = { [scoped]: `1.2.3(${peer}@2.3.4)` };
    });
    pins(prefix);
    const before = run(["graph", "dsh", prefix], 0);
    const scopedSnapshot = JSON.parse(before).snapshots.find(snapshot => snapshot.id.key === context);
    assert.deepEqual(scopedSnapshot.id.peerContext, [{ packageId: { name: peer, version: "2.3.4" }, peerContext: [], key: `${peer}@2.3.4` }]);
    assert.deepEqual(scopedSnapshot.edges, [{ name: peer, target: `${peer}@2.3.4`, optional: false }]);
    edit(prefix, "pnpm-lock.yaml", l => {
      for (const field of ["packages", "snapshots"]) l[field] = Object.fromEntries(Object.entries(l[field]).reverse());
    });
    pins(prefix);
    assert.equal(run(["graph", "dsh", prefix], 0), before);
  }
} finally {
  fs.rmSync(work, { recursive: true, force: true });
}
console.log("pnpm-lock-inputs: public CLI, policy, AST, pins, real dialect and peer contexts PASS");

// These expectations are hand-authored before implementation, not graph snapshots.
const updateVectors = json(path.join(fixtures, 'dsh-update-vectors.json'));
const derivationVectors = json(path.join(fixtures, 'dsh-derivation-vectors.json'));
// `locked` asserts the project is a locked dsh project through the public check and
// returns its published graph, so a negative case reports the CLI diagnostic.
function locked(prefix) {
  const result = spawnSync(bin, ['pnpm-lock', 'check', 'dsh', prefix], { encoding: 'utf8', timeout: 20000 });
  if (result.status !== 0) throw new Error(result.stderr.trim());
  return graphOf(prefix);
}
function satisfiedSummary(report) {
  assert.deepEqual(report.unsatisfied, []);
  return {...report, unsatisfied: report.unsatisfied.length};
}
assert.throws(() => satisfiedSummary({exact:1,totalOptionalMissing:0,cordisExact:1,unsatisfied:["unmet peer"]}));
function compare(before, after, requestedVersion = updateVectors.afterVersion, refresh = false) {
  const args = ['dsh-lock-update', before, after, requestedVersion];
  if (refresh) args.push('--refresh-lock');
  const result = spawnSync(bin, [...args, '--json'], { encoding: 'utf8', timeout: 20000 });
  if (result.status !== 0) throw new Error(result.stderr.trim());
  return JSON.parse(result.stdout);
}
function changedPair() { return [copy('update-before'), copy('update-after')]; }
function mutateAfter(change, diagnostic, refresh = false) {
  const [before, after] = changedPair();
  edit(after, 'pnpm-lock.yaml', change); pins(after);
  assert.throws(() => compare(before, after, updateVectors.afterVersion, refresh), diagnostic);
}
try {
  const [before, after] = changedPair();
  const result = compare(before, after);
  assert.equal(result.mode, 'changed');
  assert.deepEqual(result.addedArchives, [`${root}@${updateVectors.afterVersion}`, ...updateVectors.afterOnly]);
  assert.deepEqual(result.removedArchives, [`${root}@${updateVectors.beforeVersion}`, ...updateVectors.beforeOnly]);
  assert.deepEqual(result.addedSnapshots, ['@deepseek-ai/dsh@0.1.5-rc.1', 'leaf@2.0.0']);
  assert.deepEqual(result.removedSnapshots, ['@deepseek-ai/dsh@0.1.5-rc.2', 'leaf@1.0.0']);
  assert.equal(locked(after).packages.find(p => p.id.name === cordis).integrity, updateVectors.retainedIntegrity);
  assert.deepEqual(satisfiedSummary(peersOf(before)), updateVectors.peerSummary);
  assert.equal(compare(before, before, '').mode, 'equal');
  assert.equal(compare(before, before, '', true).mode, 'refresh');
  // The retired forged-project row is unrepresentable here (D14): the CLI never
  // accepts a project object, so there is no in-process graph to hand in.
  assert.throws(() => compare(before,after,'latest'), /semver/);
  const reorderedPins = copy('update-before');
  fs.writeFileSync(path.join(reorderedPins,pinFile),fs.readFileSync(path.join(reorderedPins,pinFile),'utf8').trimEnd().split('\n').reverse().join('\n')+'\n');
  assert.throws(() => compare(before,reorderedPins,''), /identical bytes/);
  const whitespaceWorkspace = changedPair();
  fs.appendFileSync(path.join(whitespaceWorkspace[1],'pnpm-workspace.yaml'),'\n'); pins(whitespaceWorkspace[1]);
  for (const refresh of [false,true]) assert.throws(() => compare(...whitespaceWorkspace,updateVectors.afterVersion,refresh), /workspace bytes/);
  for (const file of [...files, pinFile]) {
    const equal = copy('update-before');
    fs.appendFileSync(path.join(equal, file), '\n');
    if (file !== pinFile) pins(equal);
    assert.throws(() => compare(before, equal, ''), /byte/);
  }
  const broken = copy('update-after');
  fs.appendFileSync(path.join(broken, 'package.json'), ' ');
  for (const refresh of [false, true]) assert.throws(() => compare(before, broken, updateVectors.afterVersion, refresh), /pin/);
  for (const field of updateVectors.retainedArchiveMutations) {
    mutateAfter(l => {
      const p = l.packages[`${cordis}@4.0.2`];
      if (field === 'integrity') p.resolution.integrity = 'sha512-AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==';
      else if (field === 'tarball') p.resolution.tarball = 'https://registry.npmjs.org/@deepseek-ai/cordis/-/cordis-4.0.2.tgz';
      else p[field] = ({engines:{node:'>=20'}, optional:true, peerDependencies:{other:'^1.0.0'}, peerDependenciesMeta:{other:{optional:true}}, os:['linux'], cpu:['arm64'], libc:['glibc'], deprecated:'old', hasBin:true})[field];
      if (field === 'peerDependenciesMeta') p.peerDependencies = {other:'^1.0.0'};
    }, /retained archive/);
  }
  for (const field of updateVectors.retainedSnapshotMutations) {
    mutateAfter(l => { l.snapshots['shared@1.0.0'][field] = ({dependencies:{[cordis]:'4.0.2'}, optionalDependencies:{[cordis]:'4.0.2'},transitivePeerDependencies:['other'],optional:true})[field]; }, /retained snapshot/);
  }
  mutateAfter(l => { l.snapshots['shared@1.0.0'].dependencies = {[cordis]:'4.0.2'}; }, /retained snapshot/);
  const refreshed = changedPair();
  edit(refreshed[1], 'pnpm-lock.yaml', l => l.snapshots['shared@1.0.0'].dependencies = {[cordis]:'4.0.2'}); pins(refreshed[1]);
  assert.equal(compare(...refreshed, updateVectors.afterVersion, true).mode, 'refresh');
  // Refresh relaxes transitive snapshot selection only. A retained archive is npm's
  // immutable name@version, so a changed SRI, tarball or metadata is substitution.
  for (const field of ['integrity', 'tarball', 'engines']) {
    mutateAfter(l => {
      const p = l.packages[`${cordis}@4.0.2`];
      if (field === 'integrity') p.resolution.integrity = 'sha512-AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==';
      else if (field === 'tarball') p.resolution.tarball = 'https://registry.npmjs.org/@deepseek-ai/cordis/-/cordis-4.0.2.tgz';
      else p.engines = {node:'>=20'};
    }, /retained archive/, true);
  }
  for (const refresh of [false, true]) {
    mutateAfter(l => l.packages['orphan@1.0.0'] = {resolution:{integrity:updateVectors.retainedIntegrity}}, /orphan/, refresh);
    mutateAfter(l => { l.packages['orphan@1.0.0'] = {resolution:{integrity:updateVectors.retainedIntegrity}}; l.snapshots['orphan@1.0.0'] = {}; }, /orphan/, refresh);
    mutateAfter(l => l.snapshots['shared@1.0.0'].optionalDependencies = {absent:'1.0.0'}, /dangling/, refresh);
    mutateAfter(l => l.importers.other = {}, /unexpected/, refresh);
    mutateAfter(l => l.settings.excludeLinksFromLockfile = true, /false/, refresh);
    mutateAfter(l => { l.packages['pnpm@11.11.0'] = {resolution:{integrity:updateVectors.retainedIntegrity}}; l.snapshots['pnpm@11.11.0'] = {}; l.snapshots['shared@1.0.0'].dependencies = {pnpm:'11.11.0'}; }, /pnpm/, refresh);
    mutateAfter(l => { delete l.packages['@deepseek-ai/dsh-sandbox-local@1.0.0']; delete l.snapshots['@deepseek-ai/dsh-sandbox-local@1.0.0']; delete l.snapshots['@deepseek-ai/dsh@0.1.5-rc.1'].dependencies['@deepseek-ai/dsh-sandbox-local']; }, /sandbox/, refresh);
  }
  for (const [file, change, diagnostic] of [
    ['package.json', m => m.description = 'new metadata', /manifest/],
    ['package.json', m => m.devDependencies = {}, /unexpected/],
    ['pnpm-workspace.yaml', w => w.overrides[Object.keys(w.overrides)[0]] = '4.0.3', /overrides/],
  ]) {
    const pair = changedPair(); edit(pair[1], file, change); pins(pair[1]);
    assert.throws(() => compare(...pair), diagnostic);
  }
  const semantic = copy('update-after');
  edit(semantic, 'pnpm-lock.yaml', l => { l.packages = Object.fromEntries(Object.entries(l.packages).reverse()); l.snapshots = Object.fromEntries(Object.entries(l.snapshots).reverse()); }); pins(semantic);
  assert.equal(compare(before, semantic).mode, 'changed');
  const cyclic = changedPair();
  for (const prefix of cyclic) { edit(prefix, 'pnpm-lock.yaml', l => l.snapshots['shared@1.0.0'].dependencies = {shared:'1.0.0'}); pins(prefix); }
  assert.equal(compare(...cyclic).mode, 'changed');

  function contextCase(context, providerKeys, edgeTarget, expected) {
    const prefix = copy('update-before');
    edit(prefix, 'pnpm-lock.yaml', l => {
      const bare = `${cordis}@4.0.2`;
      delete l.snapshots[bare];
      l.packages['context-provider@1.0.0'] = {resolution:{integrity:updateVectors.retainedIntegrity}};
      l.snapshots['context-provider@1.0.0'] = {};
      for (const key of providerKeys) l.snapshots[key] = {dependencies:{'context-provider':'1.0.0'}};
      const old = l.snapshots[loaderKey]; delete l.snapshots[loaderKey];
      old.dependencies[cordis] = edgeTarget.slice(cordis.length + 1);
      l.snapshots[`${loader}@1.0.3(${context})`] = old;
      const r = l.snapshots[`${root}@0.1.5-rc.2`];
      r.dependencies[loader] = `1.0.3(${context})`;
      for (let i=0; i<providerKeys.length; i++) {
        if (i === 0) r.dependencies[cordis] = providerKeys[i].slice(cordis.length + 1);
        else l.snapshots[providerKeys[0]].dependencies[cordis] = providerKeys[i].slice(cordis.length + 1);
      }
    }); pins(prefix);
    if (expected) assert.throws(() => locked(prefix), expected);
    else assert.deepEqual(satisfiedSummary(peersOf(prefix)), updateVectors.peerSummary);
  }
  const bare = `${cordis}@4.0.2`, suffixed = `${bare}(context-provider@1.0.0)`;
  contextCase(bare, [suffixed], suffixed);
  contextCase(suffixed, [suffixed], suffixed);
  contextCase(bare, [suffixed, `${bare}(${updateVectors.opaque})`], suffixed, /ambiguous peer context/);
  contextCase(`${cordis}@9.0.0`, [suffixed], suffixed, /dangling peer context/);
  contextCase(suffixed, [suffixed, `${bare}(${updateVectors.opaque})`], `${bare}(${updateVectors.opaque})`, /ambiguous context/);
  const opaque = copy('update-before');
  edit(opaque, 'pnpm-lock.yaml', l => {
    l.snapshots[`${root}@0.1.5-rc.2(${updateVectors.opaque})`] = l.snapshots[`${root}@0.1.5-rc.2`]; delete l.snapshots[`${root}@0.1.5-rc.2`];
    l.importers['.'].dependencies[root].version += `(${updateVectors.opaque})`;
  }); pins(opaque);
  assert.equal(locked(opaque).importer[0].target, `${root}@0.1.5-rc.2(${updateVectors.opaque})`);
  for (const bad of updateVectors.malformed) {
    const prefix = copy('update-before');
    edit(prefix, 'pnpm-lock.yaml', l => { l.snapshots[`${root}@0.1.5-rc.2(${bad})`] = l.snapshots[`${root}@0.1.5-rc.2`]; delete l.snapshots[`${root}@0.1.5-rc.2`]; }); pins(prefix);
    assert.throws(() => locked(prefix), /malformed/);
  }

  const contextChanged = changedPair();
  edit(contextChanged[1],'pnpm-lock.yaml', l => {
    const hashed = `${loader}@1.0.3(${updateVectors.opaque})`;
    l.snapshots[hashed] = l.snapshots[loaderKey]; delete l.snapshots[loaderKey];
    l.snapshots[`${root}@0.1.5-rc.1`].dependencies[loader] = `1.0.3(${updateVectors.opaque})`;
  }); pins(contextChanged[1]);
  const contextDelta = compare(...contextChanged);
  assert.ok(contextDelta.removedSnapshots.includes(loaderKey));
  assert.ok(contextDelta.addedSnapshots.includes(`${loader}@1.0.3(${updateVectors.opaque})`));
  assert.equal(compare(before,opaque,'',true).mode,'refresh');
  const collision = changedPair();
  for (const prefix of collision) {
    edit(prefix,'pnpm-lock.yaml', l => {
      l.packages['@deepseek-ai/dsh-extra@1.0.0'] = {resolution:{integrity:updateVectors.retainedIntegrity}};
      l.snapshots['@deepseek-ai/dsh-extra@1.0.0'] = {};
      const selection = Object.keys(l.snapshots).find(key => key.startsWith(`${root}@`));
      l.snapshots[selection].dependencies['@deepseek-ai/dsh-extra'] = '1.0.0';
    }); pins(prefix);
  }
  edit(collision[1],'pnpm-lock.yaml',l => l.snapshots['@deepseek-ai/dsh-extra@1.0.0'].dependencies = {[cordis]:'4.0.2'}); pins(collision[1]);
  assert.throws(() => compare(...collision), /retained snapshot/);
  // The retired adapter is the `dsh-lock-update` subcommand: same argv contract,
  // plus `--json`. `production contract directory is fixed` is asserted by
  // script/test/dsh-project.py, which owns contract-directory authorization.
  for (const [args,expected] of [
    [[before,after,updateVectors.afterVersion],0],
    [[before,before,''],0],
    [[before,after,updateVectors.afterVersion,'--refresh-lock'],0],
    [[before,after],2], [[before,after,updateVectors.afterVersion,'--unknown'],2],
    [[before,after,updateVectors.afterVersion,'--refresh-lock','--refresh-lock'],2],
  ]) {
    const cliResult = spawnSync(bin,['dsh-lock-update',...args],{encoding:'utf8'});
    assert.equal(cliResult.status,expected,cliResult.stderr);
    if (expected) assert.equal(cliResult.stdout,'');
    else assert.match(cliResult.stdout,/1 exact peers satisfied/);
  }
  const jsonSummary = spawnSync(bin,['dsh-lock-update',before,after,updateVectors.afterVersion,'--json'],{encoding:'utf8'});
  assert.equal(jsonSummary.status,0,jsonSummary.stderr);
  const comparison = JSON.parse(jsonSummary.stdout);
  assert.equal(comparison.mode,'changed');
  assert.deepEqual(comparison.addedArchives, result.addedArchives);
  assert.deepEqual(comparison.retainedSnapshots, result.retainedSnapshots);
  assert.equal(jsonSummary.stdout.includes('exact peers satisfied'),false);

  // B2 raw-manifest regression: a lone surrogate in a free-text manifest field is a
  // distinct value, so two projects that differ only there must not read as "only
  // the selection changed". JSON is written with the escape text, so the file stays
  // valid UTF-8 while the parsed value keeps the unpaired unit.
  function withDescription(prefix, description) {
    const file = path.join(prefix, 'package.json');
    const value = JSON.parse(fs.readFileSync(file, 'utf8'));
    value.description = description;
    fs.writeFileSync(file, JSON.stringify(value));
    pins(prefix);
  }
  function refreshPair(left, right) {
    return spawnSync(bin, ['dsh-lock-update', left, right, '', '--refresh-lock', '--json'], { encoding: 'utf8' });
  }
  const surrogatePair = [copy('update-before'), copy('update-before')];
  withDescription(surrogatePair[0], '\ud800');
  withDescription(surrogatePair[1], '\ud801');
  for (const prefix of surrogatePair) run(['check', 'dsh', prefix], 0);
  const refusedRefresh = refreshPair(...surrogatePair);
  assert.equal(refusedRefresh.status, 1, refusedRefresh.stdout);
  assert.equal(refusedRefresh.stdout, '');
  assert.match(refusedRefresh.stderr, /only dsh manifest selection may change/);
  // Control: the same lone unit on both sides is no difference at all.
  const sameSurrogate = [copy('update-before'), copy('update-before')];
  for (const prefix of sameSurrogate) withDescription(prefix, '\ud800');
  assert.equal(refreshPair(...sameSurrogate).status, 0);
  // Control: a literal astral character and its surrogate-pair escape are the same
  // value, so the comparison must not report a difference.
  const escapedPair = [copy('update-before'), copy('update-before')];
  withDescription(escapedPair[0], '\u{1F600}');
  const escapedFile = path.join(escapedPair[1], 'package.json');
  const escapedValue = JSON.parse(fs.readFileSync(escapedFile, 'utf8'));
  escapedValue.description = '\u{1F600}';
  fs.writeFileSync(escapedFile, JSON.stringify(escapedValue).replace('\u{1F600}', '\\ud83d\\ude00'));
  pins(escapedPair[1]);
  assert.equal(refreshPair(...escapedPair).status, 0);
  // Control: U+FFFD is a different value from an unpaired surrogate.
  const replacement = copy('update-before');
  withDescription(replacement, '\ufffd');
  assert.equal(refreshPair(surrogatePair[0], replacement).status, 1);

  const tentative = copy('derivation');
  const derived = deriveOrThrow(tentative);
  assert.deepEqual(derived.overrides, derivationVectors.overrides);
  const timerPeer = '@deepseek-ai/cordis-plugin-timer';
  const hmr = '@deepseek-ai/cordis-plugin-hmr';
  assert.deepEqual(derived.requirements.filter(row => row.peer === timerPeer && row.kind !== 'dependency').map(row => ({version:row.version,kind:row.kind})), [{version:derivationVectors.declarations[timerPeer],kind:'range-peer'}]);
  for (const [peer, kind] of Object.entries(derivationVectors.kinds)) assert.ok(derived.requirements.some(r => r.peer === peer && r.kind === kind));
  assert.throws(() => locked(tentative), /mapping|overrides/);
  const tentativeBytes = inventory(tentative);
  run(['pin','dsh',tentative],1);
  assert.deepEqual(inventory(tentative),tentativeBytes);
  for (const [change, diagnostic] of [
    // Two exact declarations that disagree leave no candidate: every selection
    // fails the other declaration, and the message names both.
    [l => l.packages['declarations@1.0.0'].peerDependencies[cordis] = '4.0.3', /no family selection satisfies the declared requirements: @deepseek-ai\/cordis; rejected: 4\.0\.2 \(.*wants 4\.0\.3\)/],
    [l => { delete l.packages['@deepseek-ai/cordis-plugin-timer@1.1.4']; delete l.snapshots['@deepseek-ai/cordis-plugin-timer@1.1.4']; delete l.snapshots[`${root}@0.1.5-rc.2`].dependencies['@deepseek-ai/cordis-plugin-timer']; }, /missing family/],
    [l => l.packages['declarations@1.0.0'].peerDependencies['@deepseek-ai/cordis-plugin-timer'] = 'nonsense', /specification/],
  ]) {
    const prefix = copy('derivation'); edit(prefix,'pnpm-lock.yaml',change);
    assert.throws(() => deriveOrThrow(prefix), diagnostic);
  }
  // Two tentative selections of one family: the dependency edges are reachability
  // evidence only, so the pin comes from the declarations the graph really
  // carries (here the timer's own incompatible Cordis range prunes 1.1.7).
  const twoSelections = copy('derivation-two-selections');
  const twoDerived = deriveOrThrow(twoSelections);
  assert.deepEqual(twoDerived.overrides, derivationVectors.twoSelections.overrides);
  const twoGraph = json(path.join(twoSelections, 'pnpm-lock.yaml'));
  assert.deepEqual(twoGraph.packages[`${timerPeer}@1.1.4`] && twoGraph.packages[`${timerPeer}@1.1.7`] ? ['1.1.4','1.1.7'] : [], derivationVectors.twoSelections.timerSelections);
  assert.equal(twoDerived.overrides[`@deepseek-ai/dsh>${timerPeer}`], derivationVectors.twoSelections.derivedTimer);
  assert.ok(twoDerived.requirements.some(row => row.peer === timerPeer && row.kind === 'dependency' && row.version === '1.1.7'));
  assert.ok(twoDerived.requirements.some(row => row.peer === timerPeer && row.kind === 'range-peer'));
  // Neither dependency edge is reported as an exact declaration: only the two peer
  // rows describe what the family is allowed to be.
  assert.deepEqual(twoDerived.requirements.filter(row => row.peer === timerPeer && row.kind === 'exact-peer'), [],
    'a resolved dependency target must never be reported as an exact declaration');
  for (const [change, diagnostic] of [
    // A second exact requirement that disagrees with the first cannot be derived.
    [l => l.packages['@deepseek-ai/dsh-base@0.1.5-rc.3'].peerDependencies[cordis] = '4.0.3', /no family selection satisfies the declared requirements: @deepseek-ai\/cordis/],
    // The exact requirement must be present in the tentative graph, and two
    // disagreeing dependency targets cannot substitute for it.
    [l => {
      l.packages[`${cordis}@4.0.3`] = l.packages[`${cordis}@4.0.2`]; delete l.packages[`${cordis}@4.0.2`];
      l.snapshots[`${cordis}@4.0.3`] = {}; delete l.snapshots[`${cordis}@4.0.2`];
      for (const snapshot of Object.values(l.snapshots)) {
        if (snapshot.dependencies?.[cordis] === '4.0.2') snapshot.dependencies[cordis] = '4.0.3';
      }
      l.packages[`${cordis}@4.0.5`] = l.packages[`${cordis}@4.0.3`];
      l.snapshots[`${cordis}@4.0.5`] = {};
      l.snapshots[`${hmr}@1.0.17`].dependencies[cordis] = '4.0.5';
    }, /no family selection satisfies the declared requirements: @deepseek-ai\/cordis; rejected: 4\.0\.3 \(.*wants 4\.0\.2/],
    // A range that every tentative selection contradicts is unsatisfiable.
    [l => l.packages[`${hmr}@1.0.17`].peerDependencies[cordis] = '^5.0.0', /no family selection satisfies the declared requirements: @deepseek-ai\/cordis/],
    // Two selections that both satisfy every declaration are ambiguous, and the
    // message asks for the reviewed map instead of picking one.
    [l => l.packages[`${timerPeer}@1.1.7`].peerDependencies[cordis] = '^4.0.2', /ambiguous family selection: @deepseek-ai\/cordis-plugin-timer; candidates: 1\.1\.4, 1\.1\.7; .*pass --cordis-overrides after review/],
    // A dependency edge with no selecting archive is a malformed graph.
    [l => { delete l.snapshots[`${timerPeer}@1.1.7`].peerDependencies; delete l.packages[`${timerPeer}@1.1.7`]; }, /missing archive|archive/],
  ]) {
    const prefix = copy('derivation-two-selections'); edit(prefix,'pnpm-lock.yaml',change);
    assert.throws(() => deriveOrThrow(prefix), diagnostic);
  }
  // The candidate rule, one vector per branch of the adopted rule.
  const candidatePrefix = () => {
    const prefix = copy('derivation-two-selections');
    // 1.1.7 stays compatible with the derived Cordis pin, so both timer selections
    // satisfy every range declaration: only an exact peer can decide, or the
    // reviewed value when the graph itself cannot.
    edit(prefix,'pnpm-lock.yaml',l => l.packages[`${timerPeer}@1.1.7`].peerDependencies[cordis] = '^4.0.2');
    return prefix;
  };
  // (a) unique selection: an exact peer declaration filters the second selection.
  const uniqueSelection = candidatePrefix();
  edit(uniqueSelection,'pnpm-lock.yaml',l => l.packages[`${hmr}@1.0.17`].peerDependencies[timerPeer] = '1.1.4');
  assert.equal(deriveOrThrow(uniqueSelection).overrides[`@deepseek-ai/dsh>${timerPeer}`], '1.1.4');
  // (b) ambiguous with the committed value among the survivors: the no-op re-pin.
  const ambiguous = candidatePrefix();
  assert.equal(deriveOrThrow(ambiguous, { before: committedPrefix(derivationVectors.overrides) })
    .overrides[`@deepseek-ai/dsh>${timerPeer}`], '1.1.4');
  // The committed value may not invent a candidate the graph does not select.
  assert.throws(() => deriveOrThrow(ambiguous, {
    before: committedPrefix({...derivationVectors.overrides, [`@deepseek-ai/dsh>${timerPeer}`]: '1.1.5'}) }),
    /ambiguous family selection/);
  // (c) ambiguous without the committed value: a review stop, never a pick.
  assert.throws(() => deriveOrThrow(ambiguous),
    /ambiguous family selection: @deepseek-ai\/cordis-plugin-timer; candidates: 1\.1\.4, 1\.1\.7; .*pass --cordis-overrides after review/);
  // (c1) the route that stop advertises must work: an explicitly approved map is
  // passed into derivation, so it decides the graph the lockfile cannot and
  // outranks the committed no-op re-pin.
  const timerSelector = `@deepseek-ai/dsh>${timerPeer}`;
  const approvedNewer = {...derivationVectors.overrides,[timerSelector]:'1.1.7'};
  assert.equal(deriveOrThrow(ambiguous, { approved: approvedFile(approvedNewer) }).overrides[timerSelector],'1.1.7');
  assert.equal(deriveOrThrow(ambiguous, { before: committedPrefix(derivationVectors.overrides),
    approved: approvedFile(approvedNewer) }).overrides[timerSelector],'1.1.7');
  // (c2) an approved value must still be one of the surviving candidates; a
  // reviewed value that names nothing the graph selected fails and names them.
  assert.throws(() => deriveOrThrow(ambiguous, {
    before: committedPrefix(derivationVectors.overrides),
    approved: approvedFile({...derivationVectors.overrides, [timerSelector]: '1.1.5'}) }),
  /ambiguous family selection: @deepseek-ai\/cordis-plugin-timer; candidates: 1\.1\.4, 1\.1\.7; reviewed value 1\.1\.5 is not one of the candidates/);
  // The committed-value branch names itself so a reviewed map can never be
  // mistaken for it: the no-op re-pin keeps the message of a non-candidate value.
  // (d) none satisfying: the diagnostic names the declaration no candidate meets.
  const noneSatisfying = candidatePrefix();
  edit(noneSatisfying,'pnpm-lock.yaml',l => l.packages[`${hmr}@1.0.17`].peerDependencies[timerPeer] = '>=1.1.5 <1.1.7');
  assert.throws(() => deriveOrThrow(noneSatisfying, { before: committedPrefix(derivationVectors.overrides) }),
    /no family selection satisfies the declared requirements: @deepseek-ai\/cordis-plugin-timer; rejected: 1\.1\.4 \(.*wants >=1\.1\.5 <1\.1\.7\)/);
  const higherTimer = copy('derivation');
  edit(higherTimer,'pnpm-lock.yaml',l => {
    const family = '@deepseek-ai/cordis-plugin-timer';
    l.packages[`${family}@1.2.0`] = l.packages[`${family}@1.1.4`]; delete l.packages[`${family}@1.1.4`];
    l.snapshots[`${family}@1.2.0`] = {}; delete l.snapshots[`${family}@1.1.4`];
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[family] = '1.2.0';
  });
  assert.equal(deriveOrThrow(higherTimer).overrides['@deepseek-ai/dsh>@deepseek-ai/cordis-plugin-timer'],'1.2.0');
  const noDeclaration = copy('derivation');
  edit(noDeclaration,'pnpm-lock.yaml',l => delete l.packages['declarations@1.0.0'].peerDependencies['@deepseek-ai/cordis-plugin-timer']);
  assert.throws(() => deriveOrThrow(noDeclaration),/missing family peer requirements/);
  const hidden = copy('derivation');
  edit(hidden,'pnpm-workspace.yaml', w => w.overrides = derivationVectors.overrides);
  edit(hidden,'pnpm-lock.yaml', l => l.overrides = derivationVectors.overrides);
  assert.throws(() => deriveOrThrow(hidden), /overrides/);
  const proposal = path.join(work, 'proposal.json');
  const invoke = args => spawnSync(bin,['dsh-cordis-overrides',tentative,...args],{encoding:'utf8'});
  assert.equal(invoke(['--propose',proposal]).status,0);
  assert.equal(fs.readFileSync(proposal,'utf8'),JSON.stringify(derivationVectors.overrides,null,2)+'\n');
  assert.equal(invoke(['--cordis-overrides',proposal]).status,0);
  assert.equal(invoke(['--propose',proposal]).status,1);
  for (const map of [{...derivationVectors.overrides,extra:'1.0.0'},{...derivationVectors.overrides,[`@deepseek-ai/dsh>${cordis}`]:'4.0.3'}]) {
    fs.writeFileSync(proposal,JSON.stringify(map)); assert.equal(invoke(['--cordis-overrides',proposal]).status,1);
  }
  fs.writeFileSync(proposal,'{"duplicate":"1.0.0","duplicate":"1.0.0"}');
  assert.equal(invoke(['--cordis-overrides',proposal]).status,1);
  const unreviewed = path.join(work,'unreviewed.json');
  fs.writeFileSync(unreviewed, JSON.stringify({...derivationVectors.overrides,[timerSelector]:'1.1.5'}));
  const reviewed = path.join(work,'reviewed.json');
  fs.writeFileSync(reviewed, JSON.stringify(approvedNewer,null,2)+'\n');
  // The advertised review route on the ambiguous graph: rc 0 and the reviewed
  // candidate, not the stop it comes from.
  const decided = spawnSync(bin,['dsh-cordis-overrides',ambiguous,'--cordis-overrides',reviewed],{encoding:'utf8'});
  assert.equal(decided.status,0,decided.stderr);
  assert.equal(JSON.parse(decided.stdout).overrides[timerSelector],'1.1.7');
  assert.equal(decided.stdout.includes('ambiguous family selection'),false);
  const refused = spawnSync(bin,['dsh-cordis-overrides',ambiguous,'--cordis-overrides',unreviewed],{encoding:'utf8'});
  assert.equal(refused.status,1);
  assert.match(refused.stderr,/candidates: 1\.1\.4, 1\.1\.7; reviewed value 1\.1\.5 is not one of the candidates/);
} finally { fs.rmSync(work, {recursive:true, force:true}); }
console.log('pnpm-lock-inputs: independent dsh comparison, identity and derivation vectors PASS');

const stripFixture = fileURLToPath(new URL('fixtures/dsh/strip-cordis-overrides.mjs', import.meta.url));
const noContract = {...process.env}; delete noContract.AGENT_VM_CONTRACT_DIR;
const missingContract = spawnSync(process.execPath, [stripFixture,fixtures], {encoding:'utf8',env:noContract});
assert.equal(missingContract.status,1);
assert.match(missingContract.stderr,/AGENT_VM_CONTRACT_DIR must name/);
const missingHelper = spawnSync(process.execPath, [stripFixture,fixtures], {encoding:'utf8',env:{...process.env,AGENT_VM_CONTRACT_DIR:path.join(os.tmpdir(),'absent-recipe-contract-test')}});
assert.equal(missingHelper.status,1);
assert.match(missingHelper.stderr,/ENOENT/);
