// Authenticate the complete retained upstream inventory, separately from authored metadata.
import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const vendor = fileURLToPath(new URL("../../images/recipe-contract/vendor/yaml/", import.meta.url));
const authored = ["README.md", "upstream.sha256"];

function verify(root) {
  const entries = new Map();
  const manifest = fs.readFileSync(path.join(root, "upstream.sha256"), "utf8");
  assert.ok(manifest.endsWith("\n"), "manifest: missing final newline");
  for (const line of manifest.trimEnd().split("\n")) {
    const match = /^([0-9a-f]{64})  (LICENSE|package\.json|dist\/(?:[A-Za-z0-9_.-]+\/)*[A-Za-z0-9_-]+\.js)$/.exec(line);
    assert.ok(match, `manifest: malformed entry ${line}`);
    assert.ok(!entries.has(match[2]), "manifest: duplicate entry");
    entries.set(match[2], match[1]);
  }
  assert.ok(entries.has("LICENSE") && entries.has("package.json") && entries.has("dist/index.js"));
  const actual = [];
  function walk(dir, prefix = "") {
    for (const name of fs.readdirSync(dir)) {
      const relative = prefix + name;
      const file = path.join(dir, name);
      const stat = fs.lstatSync(file);
      assert.ok(!stat.isSymbolicLink(), `symlink substitution: ${relative}`);
      if (stat.isDirectory()) {
        assert.ok([...entries.keys()].some(entry => entry.startsWith(relative + "/")), `extra directory: ${relative}`);
        walk(file, relative + "/");
      } else {
        assert.ok(stat.isFile(), `not regular: ${relative}`);
        if (authored.includes(relative)) continue;
        actual.push(relative);
        assert.equal(crypto.createHash("sha256").update(fs.readFileSync(file)).digest("hex"), entries.get(relative), `digest: ${relative}`);
      }
    }
  }
  walk(root);
  assert.deepEqual(actual.sort(), [...entries.keys()].sort(), "retained file set");
  const pkg = JSON.parse(fs.readFileSync(path.join(root, "package.json")));
  assert.equal(pkg.name, "yaml");
  assert.equal(pkg.version, "2.9.0");
  assert.equal(pkg.license, "ISC");
}

verify(vendor);
const work = fs.mkdtempSync(path.join(os.tmpdir(), "pnpm-vendor-"));
try {
  const mutations = [
    root => fs.unlinkSync(path.join(root, "dist/index.js")),
    root => fs.appendFileSync(path.join(root, "LICENSE"), "changed"),
    root => fs.writeFileSync(path.join(root, "dist/extra.js"), ""),
    root => { fs.unlinkSync(path.join(root, "dist/index.js")); fs.symlinkSync("../package.json", path.join(root, "dist/index.js")); },
    root => fs.appendFileSync(path.join(root, "upstream.sha256"), "malformed\n"),
    root => fs.appendFileSync(path.join(root, "upstream.sha256"), fs.readFileSync(path.join(root, "upstream.sha256"), "utf8").split("\n")[0] + "\n"),
    root => fs.writeFileSync(path.join(root, "untracked-extra"), ""),
  ];
  for (const [index, mutate] of mutations.entries()) {
    const root = path.join(work, String(index));
    fs.cpSync(vendor, root, { recursive: true });
    mutate(root);
    assert.throws(() => verify(root), undefined, `vendor mutant ${index} survived`);
  }
} finally {
  fs.rmSync(work, { recursive: true, force: true });
}
console.log("pnpm-vendor: inventory, SHA-256 and 7 mutations PASS");
