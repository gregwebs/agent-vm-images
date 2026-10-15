// The pnpm inputs are reviewed bytes: any changed byte must fail, whichever
// policy or resolved package it would have changed. The manifest is JSON and is
// checked structurally, so its matrix stays meaningful.
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import assert from "node:assert/strict";
import { pinFile, pinnedInputs, validateManifest, verifyInputs } from "../../images/tools/opencode/plugins/check-lock.mjs";
import { writePins } from "../../images/tools/opencode/plugins/pin-pnpm-inputs.mjs";

const source = new URL("../../images/tools/opencode/plugins/", import.meta.url);
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), "pnpm-inputs-"));
const prefix = path.join(scratch, "prefix");
fs.mkdirSync(prefix);
for (const name of [...pinnedInputs, pinFile]) {
  fs.copyFileSync(new URL(name, source), path.join(prefix, name));
}
const pinsFile = path.join(prefix, pinFile);
const reviewedPins = fs.readFileSync(pinsFile, "utf8");
assert.equal(verifyInputs(prefix), "1.4.1");

function swap(bytes, from, to) {
  const text = bytes.toString("utf8");
  assert.ok(text.includes(from), `fixture lost its ${JSON.stringify(from)} anchor`);
  return Buffer.from(text.replace(from, to), "utf8");
}

const mutations = {
  "package.json": [
    // The manifest is pinned as well as policy-checked, so a policy-neutral byte
    // change fires the digest, while a shape change fires the manifest assertion
    // first (see the parsed-manifest matrix below).
    ["reindent", bytes => swap(bytes, '  "name"', '    "name"')],
    ["trailing-newline", bytes => Buffer.from(bytes.toString("utf8").replace(/\n$/, ""), "utf8")],
    // This is the defect the manifest pin exists for: validated alone, a bumped
    // selection is self-consistent, and only the pinned bytes tie it to the lock.
    ["selection", bytes => swap(bytes, '"@openchamber/opencode-claude": "1.4.1"', '"@openchamber/opencode-claude": "1.4.2"')],
  ],
  "pnpm-workspace.yaml": [
    ["padded", bytes => Buffer.concat([bytes, Buffer.from("\n")])],
    ["build-script-allowed", bytes => swap(bytes, "allowBuilds: {}", "allowBuilds: {zod: true}")],
    ["peer-install", bytes => swap(bytes, "autoInstallPeers: false", "autoInstallPeers: true")],
    ["override-drift", bytes => swap(bytes, "zod: 4.6.7", "zod: 4.6.8")],
  ],
  "pnpm-lock.yaml": [
    ["comment", bytes => Buffer.concat([bytes, Buffer.from("# reviewed?\n")])],
    ["integrity", bytes => swap(bytes, "sha512-TyLdAdcryY", "sha512-TyLdAdcryZ")],
    ["extra-package", bytes => swap(bytes, "snapshots:", "snapshots:\n  extra@1.0.0: {}")],
    ["settings", bytes => swap(bytes, "autoInstallPeers: false", "autoInstallPeers: true")],
  ],
};
for (const [name, cases] of Object.entries(mutations)) {
  const file = path.join(prefix, name);
  const original = fs.readFileSync(file);
  for (const [label, mutate] of cases) {
    const mutated = mutate(original);
    assert.notDeepEqual(mutated, original, `${name} ${label}: mutation was a no-op`);
    fs.writeFileSync(file, mutated);
    assert.throws(() => verifyInputs(prefix), /bytes differ from the reviewed pin/, `${name} ${label}: accepted`);
    fs.writeFileSync(file, original);
  }
}
assert.equal(verifyInputs(prefix), "1.4.1");

// The pin file itself is an input: a missing, malformed or incomplete pin is a
// failure rather than an implicit re-pin.
const firstPin = reviewedPins.split("\n")[0] + "\n";
for (const [label, content] of [
  ["empty", ""],
  ["missing-lock", reviewedPins.split("\n").filter(line => !line.includes("pnpm-lock.yaml")).join("\n")],
  ["extra-input", reviewedPins + `${"0".repeat(64)}  another.yaml\n`],
  ["duplicate", reviewedPins + firstPin],
  ["wrong-digest", reviewedPins.replace(/^[0-9a-f]{8}/m, "deadbeef")],
  ["wrong-separator", reviewedPins.replace("  ", " ")],
  ["malformed-digest", reviewedPins.replace(/^[0-9a-f]{64}/m, "z".repeat(64))],
]) {
  fs.writeFileSync(pinsFile, content);
  assert.throws(() => verifyInputs(prefix), undefined, `${pinFile} ${label}: accepted`);
}
fs.unlinkSync(pinsFile);
assert.throws(() => verifyInputs(prefix), /missing; the pnpm inputs are unpinned/, "absent pin accepted");

// Re-pinning is a deliberate developer action, and it is the only way a changed
// byte becomes acceptable again.
fs.writeFileSync(pinsFile, reviewedPins);
const lock = path.join(prefix, "pnpm-lock.yaml");
const reviewedLock = fs.readFileSync(lock);
fs.writeFileSync(lock, swap(reviewedLock, "zod@4.6.7", "zod@4.6.8"));
assert.throws(() => verifyInputs(prefix), /bytes differ from the reviewed pin/);
writePins(prefix);
assert.equal(verifyInputs(prefix), "1.4.1");
fs.writeFileSync(lock, reviewedLock);
fs.writeFileSync(pinsFile, reviewedPins);
assert.equal(verifyInputs(prefix), "1.4.1");

// Manifest policy stays structural: JSON needs no parser and no pin.
const manifest = JSON.parse(fs.readFileSync(new URL("package.json", source)));
for (const [label, mutate] of [
  ["manager-version", m => { m.packageManager = "pnpm@12.9.1"; }],
  ["range-specifier", m => { m.dependencies["@openchamber/opencode-claude"] = "^1.4.1"; }],
  ["build-script", m => { m.scripts = { postinstall: "false" }; }],
  ["extra-dependency", m => { m.dependencies.zod = "4.6.7"; }],
  ["public", m => { m.private = false; }],
  ["second-sdk", m => { m.dependencies["@anthropic-ai/claude-agent-sdk"] = "0.3.300"; }],
]) {
  const mutated = structuredClone(manifest);
  mutate(mutated);
  assert.notDeepEqual(mutated, manifest, `${label}: mutation was a no-op`);
  assert.throws(() => validateManifest(mutated), undefined, `accepted manifest mutation: ${label}`);
}
assert.equal(validateManifest(manifest), "1.4.1");
fs.rmSync(scratch, { recursive: true, force: true });
console.log("pnpm inputs: reviewed-byte pins enforced, manifest policy structural, re-pin explicit");
