#!/usr/bin/env node
// Fixture dsh CLI for the build-gate tests. It is the authorized command target
// and the process seam only: it models the report shape and hands `web` to the
// native web fixture. Nothing here claims to be the real harness.
'use strict';
const {spawn} = require('node:child_process');

const behaviour = process.env.DSH_FIXTURE_BEHAVIOUR || 'normal';
const version = process.env.DSH_FIXTURE_VERSION || '0.0.0-fixture';
const args = process.argv.slice(2);

if (behaviour === 'empty') process.exit(0);
if (behaviour === 'print-then-fail') { process.stdout.write(version + '\n'); process.exit(1); }
if (behaviour === 'extra-line') { process.stdout.write(version + '\nwarning: fixture\n'); process.exit(0); }
if (behaviour === 'hang') { setInterval(() => {}, 1000); return; }
if (args[0] === '--version') { process.stdout.write(version + '\n'); process.exit(0); }
if (args[0] === 'web') {
  const script = process.env.DSH_WEB_FIXTURE_SCRIPT;
  if (!script) { process.stderr.write('fixture dsh: no web fixture configured\n'); process.exit(2); }
  const child = spawn(process.env.DSH_WEB_FIXTURE_PYTHON || 'python3', [script, ...args.slice(1)], {stdio: 'inherit'});
  child.on('exit', (code, signal) => {
    if (signal) process.kill(process.pid, signal);
    else process.exit(code === null ? 1 : code);
  });
  return;
}
process.stderr.write('fixture dsh: unsupported command ' + String(args[0]) + '\n');
process.exit(2);
