// Replay the corpus against immutable retired sources, never the Go parser.
// Requires the main=3b21103 object; no network or npm installation is needed.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {execFileSync} from 'node:child_process';
import {fileURLToPath, pathToFileURL} from 'node:url';

const root = fileURLToPath(new URL('../../', import.meta.url));
try {
  execFileSync('git', ['cat-file', '-e', '3b21103^{commit}'], {cwd:root, stdio:'ignore'});
} catch {
  console.error('recipe-contract-oracle: missing historical commit 3b21103; fetch full history (git fetch --unshallow in a shallow clone, otherwise git fetch origin). CI checkouts must use fetch-depth: 0.');
  process.exit(1);
}
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'recipe-contract-oracle-'));
try {
  const archive = execFileSync('git', ['archive', '3b21103', 'images/recipe-contract/vendor/yaml'], {cwd:root, maxBuffer:16 << 20});
  execFileSync('tar', ['-xf', '-', '-C', scratch], {input:archive});
  const {default:YAML} = await import(pathToFileURL(path.join(scratch, 'images/recipe-contract/vendor/yaml/dist/index.js')));
  const rows = JSON.parse(fs.readFileSync(path.join(root, 'images/recipe-contract/internal/document/testdata/parse-vectors.json'), 'utf8'));
  function parse(row) {
    const source = new TextDecoder('utf-8', {fatal:true}).decode(Buffer.from(row.source));
    if (row.format === 'json') JSON.parse(source);
    const doc = YAML.parseDocument(source, {uniqueKeys:true, merge:false, strict:true});
    assert.equal(doc.errors.length, 0, doc.errors.map(e => e.message).join('; '));
    assert.equal(doc.warnings.length, 0, doc.warnings.map(e => e.message).join('; '));
    function walk(node, depth) {
      assert.ok(depth <= 100, 'input exceeds depth100');
      if (!node) return;
      assert.ok(!YAML.isAlias(node) && !node.anchor && !node.tag, 'anchors, aliases and explicit tags forbidden');
      if (YAML.isMap(node)) {
        for (const pair of node.items) {
          assert.ok(YAML.isScalar(pair.key) && typeof pair.key.value === 'string', 'mapping keys must be strings');
          assert.notEqual(pair.key.value, '<<', 'merge key forbidden');
          walk(pair.key, depth + 1); walk(pair.value, depth + 1);
        }
      } else if (YAML.isSeq(node)) for (const item of node.items) walk(item, depth + 1);
    }
    walk(doc.contents, 0);
    const value = doc.toJS({maxAliasCount:0});
    return JSON.stringify(value === undefined ? null : value);
  }
  for (const [index, row] of rows.entries()) {
    let outcome = 'accept', json;
    try { json = parse(row); } catch { outcome = 'reject'; }
    const expected = row.js || row.outcome;
    assert.equal(outcome, expected, `row ${index}: ${JSON.stringify(row.source)}`);
    if (expected === 'accept' && row.json !== undefined) assert.equal(json, row.json, `row ${index}: oracle value`);
  }
  console.log(`recipe-contract-oracle: ${rows.length} JS outcomes verified`);
} finally {
  fs.rmSync(scratch, {recursive:true, force:true});
}
