// Build-only payload validation and access inventory; no package manager imports.
import fs from 'node:fs';
import assert from 'node:assert/strict';
import path from 'node:path';

function config(args) {
  assert.deepEqual(JSON.parse(fs.readFileSync(args[0])), { plugins: [args[1]] });
  assert.equal(fs.readFileSync(args[1] + '/index.js', 'utf8'), 'export { default } from "@openchamber/opencode-claude";\n');
}

function report(args) {
  const report = JSON.parse(fs.readFileSync(args[0], 'utf8'));
  assert.equal(report.schema, 1);
  assert.equal(report.plugin_version, args[1]);
  assert.equal(report.sdk_version, '0.3.281');
  assert.equal(report.sdk_instances, 1);
  for (const key of ['handoff', 'sdk_available', 'adapter', 'omitted_peers']) assert.equal(report[key], true);
  assert.equal(report.optional_platforms, false);
  // Zero is tolerated: the build RUN has no guest credentials. The OpenCode
  // probe's /api/model assertion supplies the non-empty requirement from the
  // plugin's fallback catalog.
  assert.ok(Number.isInteger(report.initialization_models) && report.initialization_models >= 0);
}

function files(args) {
  function walk(file, ancestors = new Set()) {
    const real = fs.realpathSync(file);
    if (fs.statSync(file).isFile()) { process.stdout.write(file + "\0"); return; }
    if (ancestors.has(real)) throw new Error("cyclic directory link");
    const next = new Set(ancestors).add(real);
    for (const name of fs.readdirSync(file)) walk(path.join(file, name), next);
  }
  walk(args[0]);
}

try {
  const modes = { config, report, files };
  const mode = process.argv[2];
  assert.ok(Object.hasOwn(modes, mode), 'expected config, report or files');
  const args = process.argv.slice(3);
  assert.equal(args.length, mode === 'files' ? 1 : 2);
  modes[mode](args);
} catch (error) {
  console.error(`OpenCode payload: ${error.message}`);
  process.exitCode = 1;
}
