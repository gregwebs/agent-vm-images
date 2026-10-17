// Independent fixture/CLI oracles; no package manager or production pin writer for negatives.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { readProject, validateProject, checkExactPeers } from "../../images/recipe-contract/pnpm-lock.mjs";

const fixtures = fileURLToPath(new URL("fixtures/pnpm-lock/", import.meta.url));
const cli = fileURLToPath(new URL("../../images/recipe-contract/pnpm-lock.mjs", import.meta.url));
const files = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];
const pinFile = "pnpm-inputs.sha256";
const root = "@deepseek-ai/dsh";
const cordis = "@deepseek-ai/cordis";
const loader = "@deepseek-ai/cordis-plugin-loader";
const loaderKey = `${loader}@1.0.3(${cordis}@4.0.2)`;
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pnpm-lock-inputs-"));
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
function run(args, status) {
  const result = spawnSync(process.execPath, [cli, ...args], { encoding: "utf8", timeout: 20000, maxBuffer: 32 * 1024 * 1024 });
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
      const result = spawnSync(process.execPath, [cli, verb, fixture === "real-bridge" ? "bridge" : "dsh", prefix], { encoding: "utf8", timeout: 20000 });
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
    const project = readProject({ prefix, kind: "dsh" });
    validateProject(project, project.selection);
    checkExactPeers(project);
  }
  const real = copy("real-bridge");
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
      const result = spawnSync(process.execPath, [cli, "check", "dsh", prefix], { encoding: "utf8" });
      assert.equal(result.status, 1);
      assert.match(result.stderr, /peer @deepseek-ai\/cordis wanted 4.0.2, got .*4.0.4/);
    } else run(["check", "dsh", prefix], 0);
  }
  invalid(prefix => edit(prefix, "pnpm-lock.yaml", l => {
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[cordis] = "4.0.2";
    delete l.snapshots[loaderKey].dependencies[cordis];
  }), /wanted 4.0.2, got missing/);
  const optional = copy();
  edit(optional, "pnpm-lock.yaml", l => {
    l.packages[`${loader}@1.0.3`].peerDependenciesMeta = { [cordis]: { optional: true } };
    l.snapshots[`${root}@0.1.5-rc.2`].dependencies[loader] = "1.0.3";
    l.snapshots[`${loader}@1.0.3`] = {};
    delete l.snapshots[loaderKey]; delete l.snapshots[`${cordis}@4.0.2`]; delete l.packages[`${cordis}@4.0.2`];
  });
  pins(optional); run(["check", "dsh", optional], 0);

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
