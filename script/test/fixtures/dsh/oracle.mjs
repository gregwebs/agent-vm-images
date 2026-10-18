// Independent installed-manifest oracle: no production checker/graph imports.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {pathToFileURL} from 'node:url';

const [prefix, expected] = process.argv.slice(2);
const root = fs.realpathSync(prefix);
function owned(file) {
  const real = fs.realpathSync(file);
  assert.ok(real.startsWith(root + path.sep), `external resolution: ${file}`);
  return real;
}
function packageAt(directory) {
  const real = owned(directory);
  return {directory: real, manifest: JSON.parse(fs.readFileSync(path.join(real, 'package.json')))};
}
function dependency(owner, name) {
  let directory = owner.directory;
  while (directory.startsWith(root)) {
    const candidate = path.join(directory, 'node_modules', name);
    if (fs.existsSync(candidate)) {
      const result = packageAt(candidate);
      assert.equal(result.manifest.name, name);
      const wanted = owner.manifest.peerDependencies?.[name];
      if (wanted && /^\d+\.\d+\.\d+(?:-[\w.-]+)?$/.test(wanted)) {
        assert.equal(result.manifest.version, wanted, `effective peer ${owner.manifest.name} -> ${name}`);
      }
      return result;
    }
    const parent = path.dirname(directory);
    if (parent === directory) break;
    directory = parent;
  }
  throw new Error(`missing ${name} from ${owner.manifest.name}`);
}
async function load(pkg) {
  const exports = pkg.manifest.exports;
  let entry = exports?.['.'] ?? exports ?? pkg.manifest.main ?? 'index.js';
  while (entry && typeof entry === 'object') entry = entry.node ?? entry.import ?? entry.default ?? entry.require;
  assert.equal(typeof entry, 'string', `no runtime export ${pkg.manifest.name}`);
  return import(pathToFileURL(owned(path.join(pkg.directory, entry))));
}

const timer = setTimeout(() => { console.error('independent dsh oracle deadline'); process.exit(1); }, 25000);
const scratch = fs.mkdtempSync(path.join(os.tmpdir(), 'independent-dsh-'));
try {
  const dsh = packageAt(path.join(root, 'node_modules/@deepseek-ai/dsh'));
  assert.equal(dsh.manifest.version, expected);
  assert.deepEqual(dsh.manifest.bin, {dsh: 'lib/bin.js'});
  const base = dependency(dsh, '@deepseek-ai/dsh-base');
  const sdk = dependency(base, '@deepseek-ai/dsh-sdk-minimal');
  const sandbox = dependency(base, '@deepseek-ai/dsh-sandbox-local');
  assert.equal(dependency(sdk, '@deepseek-ai/dsh-sandbox-local').directory, sandbox.directory);
  const local = dependency(sdk, '@deepseek-ai/dsh-subprocess-local');
  const cordis = dependency(local, '@deepseek-ai/cordis');
  const {Context, Service} = await load(cordis);
  const {default: Provider} = await load(local);
  assert.ok(Provider.prototype instanceof Service, 'provider uses another Cordis instance');
  const context = new Context();
  const plugin = await context.plugin(Provider);
  let disposed = false;
  let stdout;
  try {
    const child = context.subprocess.spawn({
      argv: ['/bin/sh', '-c', 'printf "independent-dsh\\n"; exit 7'], cwd: scratch,
      stdio: {stdin: 'ignore', stdout: {maxBytes: 1024}, stderr: {maxBytes: 1024}},
      graceMs: 200, signal: AbortSignal.timeout(10000),
      env: {PATH: '/usr/bin:/bin', HOME: scratch, TMPDIR: scratch},
    });
    const outcome = await child.done;
    assert.equal(outcome.exitCode, 7);
    assert.equal(outcome.signal, null);
    stdout = child.collected.stdout.readFrom(0);
    assert.deepEqual({text: stdout.text, lossy: stdout.lossy}, {text: 'independent-dsh\n', lossy: false});
    assert.equal(child.collected.stderr.readFrom(0).text, '');
    assert.equal(await child.waitForExit(AbortSignal.timeout(3000)), true);
  } finally {
    await plugin.dispose();
    disposed = true;
  }
  console.log(JSON.stringify({version: expected, sandbox: sandbox.manifest.version, cordis: cordis.manifest.version,
    subprocess: {stdout: stdout.text, exitCode: 7, disposed}}));
} finally {
  clearTimeout(timer);
  fs.rmSync(scratch, {recursive: true, force: true});
}
