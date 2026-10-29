#!/usr/bin/env node
// The production root-command proof, exercised against a real isolated layout.
//
// `images/tools/pi/check-runtime.mjs` is the only code that decides whether the
// installed tree is the selection the lock describes, and the installer and the
// verifier both call it before they execute installed Pi code or publish a record.
// Its root-command proof is the half a byte comparison cannot supply: a tree whose
// `node_modules/.bin/pi` is a shim, a copy or a link to another package's file can
// still be byte-identical to the authenticated archives everywhere else.
//
// The fixture prefix, its graph and its stub contract come from
// `fixtures/pi-installed-bytes.py`, so the tree is the same real isolated layout the
// byte checker is proved against. Its later N6 probes cannot run here (the fixture
// ships no genai/protobufjs/esbuild payload): the positive case therefore proves the
// root-command proof *passed* by requiring the run to fail later, at a probe, with a
// diagnostic that is not `root-command`.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {fileURLToPath} from 'node:url';

const checker = fileURLToPath(new URL('../../images/tools/pi/check-runtime.mjs', import.meta.url));
const builder = fileURLToPath(new URL('fixtures/pi-installed-bytes.py', import.meta.url));
const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pi-runtime-check-'));

function build(shape) {
  const root = path.join(work, shape);
  const result = spawnSync('python3', [builder, 'build', root, shape], {encoding: 'utf8', timeout: 120000});
  assert.equal(result.status, 0, result.stderr);
  return JSON.parse(result.stdout);
}

function run(meta) {
  return spawnSync('node', [checker, meta.prefix], {
    encoding: 'utf8',
    timeout: 120000,
    env: {
      ...process.env,
      AGENT_VM_CONTRACT_DIR: meta.contract,
      AGENT_VM_PI_TARGET_PLATFORM: 'linux/arm64',
    },
  });
}

function expectRootCommand(meta, detail, mutate, repair) {
  mutate();
  const failed = run(meta);
  assert.equal(failed.status, 1, `expected a refusal, got ${failed.status}`);
  assert.ok(failed.stderr.startsWith('check-runtime.mjs: root-command: '),
    `expected a root-command refusal, got ${failed.stderr.trim()}`);
  assert.ok(failed.stderr.includes(detail),
    `expected the diagnostic to name ${detail}, got ${failed.stderr.trim()}`);
  repair();
  const repaired = run(meta);
  assert.equal(repaired.status, 1, 'the fixture ships no N6 payload, so the positive run still fails');
  assert.ok(!repaired.stderr.includes('root-command'),
    `the repaired tree must pass the root-command proof, got ${repaired.stderr.trim()}`);
}

const legacy = build('legacy');
const modern = build('modern');
// The shape whose probes can all be satisfied offline, so the rest of the proof
// runs: the scoped chord instance, the transform context and the native command.
const complete = build('legacy-probe');
const link = path.join(legacy.prefix, 'node_modules', '.bin', 'pi');
const text = fs.readlinkSync(link);

// The positive control first: the fixtures' own layout passes the root-command
// proof, and the run stops at the probe the fixture cannot satisfy.
for (const meta of [legacy, modern]) {
  const result = run(meta);
  assert.equal(result.status, 1);
  assert.ok(!result.stderr.includes('root-command'),
    `${meta.shape}: the fixture layout must pass the root-command proof, got ${result.stderr.trim()}`);
  assert.ok(result.stderr.includes('missing-probe-package'),
    `${meta.shape}: expected the first probe diagnostic, got ${result.stderr.trim()}`);
}

// A shim: the wrapper's default entry must be a link, because a wrapper there would
// make `pi --version` prove something other than the installed CLI.
expectRootCommand(legacy, 'is not a direct symlink', () => {
  fs.rmSync(link);
  fs.writeFileSync(link, '#!/bin/sh\nexit 0\n');
  fs.chmodSync(link, 0o755);
}, () => {
  fs.rmSync(link);
  fs.symlinkSync(text, link);
});

// A retargeted link: still a symlink inside the payload, and to a file that exists,
// so the resolved-target comparison is the only thing that can refuse it. Both
// halves are the point of the case: the earlier retarget named a virtual-store
// path the legacy fixture does not have, so the run failed in the resolver with
// "does not resolve" -- which satisfied a weaker `not ` assertion -- and deleting
// the target comparison entirely left this case passing.
expectRootCommand(legacy, path.join('dist', 'index.js') + ', not ', () => {
  fs.rmSync(link);
  fs.symlinkSync('../@earendil-works/pi-coding-agent/dist/index.js', link);
}, () => {
  fs.rmSync(link);
  fs.symlinkSync(text, link);
});

// An absolute link: resolvable, and refused because the text escapes the payload's
// own relative shape.
expectRootCommand(legacy, 'absolute link', () => {
  fs.rmSync(link);
  fs.symlinkSync(path.join(legacy.prefix, 'node_modules', '@earendil-works',
    'pi-coding-agent', 'dist', 'bundle', 'cli.js'), link);
}, () => {
  fs.rmSync(link);
  fs.symlinkSync(text, link);
});

// A link to a missing target: the root bin must exist, not merely be named. The
// diagnostic is the resolver's, so it is asserted by its own words rather than by
// the `root-command` prefix every refusal here carries.
expectRootCommand(legacy, 'does not resolve', () => {
  fs.rmSync(link);
  fs.symlinkSync('../@earendil-works/pi-coding-agent/dist/bundle/absent.js', link);
}, () => {
  fs.rmSync(link);
  fs.symlinkSync(text, link);
});

// The complete shape: every probe is satisfiable offline, so the run finishes and
// reports what it proved. This is the positive control the mutants below edit.
const completeRun = run(complete);
assert.equal(completeRun.status, 0, `the complete fixture must pass every probe, got ${completeRun.stderr.trim()}`);
const report = JSON.parse(completeRun.stdout.replace(/^pi runtime: /, ''));
assert.deepEqual(report.shape, ['chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui']);
assert.equal(report.command, 'dist/bundle/cli.js');
// The transform is proved through the SCOPED family instance's own `esbuild` edge:
// an unscoped `chord` resolves to nothing here, so `CHORD = 'chord'` would refuse
// the positive instead of probing whichever package happened to be called chord.
assert.equal(report.genai, '1.0.0 (1 client exports)');
assert.equal(report.transform, 'let x = 1');
// The native package declares no `bin`, so the command is the wrapper's own
// convention, and its `--version` is what binds that file to the locked identity.
assert.equal(report.nativePackage, '@esbuild/linux-arm64');
assert.equal(report.native, '0.28.2');
assert.deepEqual(report.esbuildOptionals, [{name: '@esbuild/linux-arm64', allowed: true}]);

const native = path.join(complete.prefix, 'node_modules', '.pnpm', '@esbuild+linux-arm64@0.28.2',
  'node_modules', '@esbuild', 'linux-arm64');
const nativeManifest = path.join(native, 'package.json');
const nativeManifestText = fs.readFileSync(nativeManifest, 'utf8');
const nativeBinary = path.join(native, 'bin', 'esbuild');
const nativeBinaryText = fs.readFileSync(nativeBinary, 'utf8');

function expectProbeRefusal(code, detail, mutate, repair) {
  mutate();
  const failed = run(complete);
  assert.equal(failed.status, 1, `expected a refusal, got ${failed.status} (${failed.stdout.trim()})`);
  assert.ok(failed.stderr.includes(code), `expected ${code}, got ${failed.stderr.trim()}`);
  assert.ok(failed.stderr.includes(detail), `expected the diagnostic to name ${detail}, got ${failed.stderr.trim()}`);
  assert.ok(!failed.stdout.startsWith('pi runtime: '), 'a refused tree must not publish a report');
  repair();
  const repaired = run(complete);
  assert.equal(repaired.status, 0, `the repaired tree must pass again: ${repaired.stderr.trim()}`);
}

// A different file at the convention path: the name is not the proof, the version
// it prints is.
expectProbeRefusal('native-version', 'reports "0.28.1"', () => {
  fs.writeFileSync(nativeBinary, '#!/bin/sh\necho 0.28.1\n');
  fs.chmodSync(nativeBinary, 0o755);
}, () => {
  fs.writeFileSync(nativeBinary, nativeBinaryText);
  fs.chmodSync(nativeBinary, 0o755);
});

// A command that cannot run is refused before it can be trusted.
expectProbeRefusal('probe-failed', 'is not executable', () => {
  fs.chmodSync(nativeBinary, 0o644);
}, () => {
  fs.chmodSync(nativeBinary, 0o755);
});

// A native package that DOES declare a command may not point it out of itself.
expectProbeRefusal('native-command', 'escapes the package', () => {
  fs.writeFileSync(nativeManifest,
    nativeManifestText.replace('"type": "module"', '"type": "module",\n  "bin": "../../../../../escape"'));
}, () => {
  fs.writeFileSync(nativeManifest, nativeManifestText);
});

fs.rmSync(work, {recursive: true, force: true});
process.stdout.write('pi-runtime-check: the production proof refuses every forged root entry and native command and passes the real layout\n');
