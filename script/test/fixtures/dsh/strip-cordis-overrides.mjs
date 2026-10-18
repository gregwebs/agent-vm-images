// Test-only: remove the four reviewed Cordis overrides from a disposable copy so
// the ticket's negative leg can re-resolve the graph without them. No production
// path imports this file, and it never writes outside the directory it is given.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import YAML from '../../../../images/recipe-contract/vendor/yaml/dist/index.js';

const [prefix, ...extra] = process.argv.slice(2);
if (!prefix || extra.length) {
  console.error('usage: strip-cordis-overrides.mjs PREFIX');
  process.exitCode = 2;
} else {
  let removed;
  for (const name of ['pnpm-workspace.yaml', 'pnpm-lock.yaml']) {
    const file = path.join(prefix, name);
    const document = YAML.parseDocument(fs.readFileSync(file, 'utf8'), {uniqueKeys: true});
    assert.equal(document.errors.length, 0, `${name}: ${document.errors.map(error => error.message).join('; ')}`);
    if (document.has('overrides')) {
      removed = document.get('overrides');
      document.delete('overrides');
      fs.writeFileSync(file, String(document));
    }
  }
  assert.ok(removed, 'no Cordis overrides to remove');
  // The removed map is the reviewed map the negative leg must compare against.
  console.log(JSON.stringify({overrides: removed}));
}
