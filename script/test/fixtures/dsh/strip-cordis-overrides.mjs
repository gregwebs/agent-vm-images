// Test-only: remove the four reviewed Cordis overrides from a disposable copy so
// the ticket's negative leg can re-resolve the graph without them. No production
// path imports this file, and it never writes outside the directory it is given.
//
// The rewrite is JSON (which is YAML): the bounded parser is reached through
// `recipe-contract pnpm-yaml`, and the negative leg re-resolves the file anyway.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';

const contract = process.env.AGENT_VM_CONTRACT_DIR;
assert.ok(contract, 'AGENT_VM_CONTRACT_DIR must name the composed helper directory');
const helper = path.join(contract, 'recipe-contract');
const [prefix, ...extra] = process.argv.slice(2);
if (!prefix || extra.length) {
  console.error('usage: strip-cordis-overrides.mjs PREFIX');
  process.exitCode = 2;
} else {
  let removed;
  for (const name of ['pnpm-workspace.yaml', 'pnpm-lock.yaml']) {
    const file = path.join(prefix, name);
    const parsed = spawnSync(helper, ['pnpm-yaml', file], { encoding: 'utf8' });
    assert.ifError(parsed.error);
    assert.equal(parsed.status, 0, `${name}: ${parsed.stderr}`);
    const value = JSON.parse(parsed.stdout);
    if (Object.hasOwn(value, 'overrides')) {
      removed = value.overrides;
      delete value.overrides;
      fs.writeFileSync(file, JSON.stringify(value));
    }
  }
  assert.ok(removed, 'no Cordis overrides to remove');
  // The removed map is the reviewed map the negative leg must compare against.
  console.log(JSON.stringify({overrides: removed}));
}
