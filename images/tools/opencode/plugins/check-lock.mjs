// A closed, reviewed graph: an upgrade that changes this shape needs review.
//
// The three checked-in pnpm install inputs are verified as reviewed bytes rather
// than re-interpreted. No YAML parser runs in the build, so a configuration file
// cannot smuggle merge keys, anchors, explicit tags or duplicate keys past
// review: any byte that differs from the pin is a hard failure. The manifest is
// pinned too, which is what binds the reported selection to the lockfile — an
// unpinned manifest could name one plugin version while the pinned lock resolved
// another. Its policy is additionally asserted structurally, so a re-pin that
// changed the shape gets a specific message rather than only a digest mismatch.
// pnpm itself enforces what it installs (lockfile/manifest freshness,
// lockfile/config agreement and tarball integrity all fail closed), so this
// checker owns the manifest policy and the reviewed-bytes guarantee, not a
// second opinion on the resolved graph. Re-pin deliberately with
// pin-pnpm-inputs.mjs.
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";
import assert from "node:assert/strict";

const sdk = "@anthropic-ai/claude-agent-sdk";
const plugin = "@openchamber/opencode-claude";
const sdkVersion = "0.3.281";
const exact = /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;

export const pinFile = "pnpm-inputs.sha256";
export const pinnedInputs = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml"];

export function digest(file) {
  return crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex");
}

export function validateManifest(manifest) {
  assert.equal(manifest.name, "agent-vm-guest-opencode-plugins");
  assert.equal(manifest.version, "0.0.0");
  assert.equal(manifest.private, true);
  assert.equal(manifest.type, "module");
  assert.equal(manifest.packageManager, "pnpm@11.11.0");
  assert.deepEqual(Object.keys(manifest).sort(), ["name", "version", "private", "type", "packageManager", "dependencies"].sort());
  const version = manifest.dependencies[plugin];
  assert.equal(typeof version, "string");
  assert.ok(exact.test(version) && !version.includes("\n"), "plugin selection must be canonical exact semver");
  assert.deepEqual(manifest.dependencies, { [sdk]: sdkVersion, [plugin]: version });
  return version;
}

export function readPins(prefix) {
  const file = path.join(prefix, pinFile);
  assert.ok(fs.existsSync(file), `${pinFile}: missing; the pnpm inputs are unpinned`);
  const pins = new Map();
  for (const line of fs.readFileSync(file, "utf8").split("\n")) {
    if (line === "") continue;
    const match = /^([0-9a-f]{64}) {2}(\S+)$/.exec(line);
    assert.ok(match, `${pinFile}: malformed line ${JSON.stringify(line)}`);
    assert.ok(!pins.has(match[2]), `${pinFile}: ${match[2]} is pinned twice`);
    pins.set(match[2], match[1]);
  }
  assert.deepEqual([...pins.keys()].sort(), [...pinnedInputs].sort(), `${pinFile}: must pin exactly ${pinnedInputs.join(", ")}`);
  return pins;
}

export function verifyInputs(prefix) {
  const version = validateManifest(JSON.parse(fs.readFileSync(path.join(prefix, "package.json"))));
  for (const forbidden of [".npmrc", ".pnpmfile.cjs", ".pnpmfile.js", ".pnpmfile.mjs", "pnpmfile.cjs", "package-lock.json"]) {
    assert.ok(!fs.existsSync(path.join(prefix, forbidden)), `unreviewed input: ${forbidden}`);
  }
  const pins = readPins(prefix);
  for (const name of pinnedInputs) {
    assert.equal(
      digest(path.join(prefix, name)),
      pins.get(name),
      `${name}: bytes differ from the reviewed pin; re-pin deliberately with pin-pnpm-inputs.mjs`,
    );
  }
  return version;
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    if (process.argv.length !== 3) throw new Error("usage: check-lock.mjs PREFIX");
    console.log(verifyInputs(process.argv[2]));
  } catch (error) {
    console.error(`check-lock: ${error.message}`);
    process.exitCode = 1;
  }
}
