// Tentative graphs have no install/pin capability; this adapter only emits proof
// and an explicitly requested review map. It never resolves or contacts a registry.
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import {deriveDshCordisOverrides, readDshCordisOverrideMap, readProject, pinnedInputs} from '../../recipe-contract/pnpm-lock.mjs';

const [prefix, ...options] = process.argv.slice(2);
const allowed = ['--propose','--cordis-overrides','--before'];
const flags = new Map();
let usage = !prefix;
for (let index = 0; index < options.length; index += 2) {
  const flag = options[index], value = options[index + 1];
  if (!allowed.includes(flag) || flags.has(flag) || !value || value.startsWith('--')) usage = true;
  flags.set(flag,value);
}
if (flags.has('--propose') && flags.has('--cordis-overrides')) usage = true;
if (usage) {
  console.error('usage: derive-cordis-overrides.mjs TENTATIVE_PREFIX [--before COMMITTED_PREFIX] [--propose FILE | --cordis-overrides FILE]');
  process.exitCode = 2;
} else {
  try {
    const before = flags.has('--before') ? readProject({prefix:flags.get('--before'),kind:'dsh'}) : undefined;
    // The reviewed maps are the only tie-break for a graph that supports two
    // selections of one family; without one derivation stops instead of picking.
    // The map supplied on this command line is passed *into* derivation, so the
    // route the ambiguity stop advertises really can decide the graph.
    const approved = flags.has('--cordis-overrides') ? readDshCordisOverrideMap(flags.get('--cordis-overrides')) : undefined;
    const result = deriveDshCordisOverrides({prefix,committed:before?.workspace.overrides,approved});
    if (before) result.delta = Object.entries(result.overrides).filter(([selector,after]) => before.workspace.overrides[selector] !== after).map(([selector,after]) => ({selector,before:before.workspace.overrides[selector],after}));
    if (flags.has('--cordis-overrides')) assert.deepEqual(approved,result.overrides,'reviewed map differs from independent tentative requirements');
    if (flags.has('--propose')) {
      const requested = path.resolve(flags.get('--propose'));
      const parent = fs.realpathSync(path.dirname(requested));
      const destination = path.join(parent,path.basename(requested));
      const info = fs.statSync(parent);
      // Proposals may use an ordinary user-designated path, including sticky
      // system /tmp; they do not grant publication authority over source inputs.
      assert.ok(info.isDirectory() && (info.uid === process.getuid() || (info.uid === 0 && (info.mode & 0o1000))), 'proposal parent must be owned or root-owned sticky directory');
      fs.accessSync(parent,fs.constants.W_OK | fs.constants.X_OK);
      for (const project of [prefix, flags.get('--before')].filter(Boolean)) {
        for (const file of [...pinnedInputs,'pnpm-inputs.sha256']) assert.notEqual(destination,path.resolve(project,file),'proposal collides with input');
      }
      // link publishes a completed regular file without replacing even a dangling
      // symlink or an existing caller file. rename would silently overwrite it.
      const temporary = path.join(parent,`.dsh-cordis-${crypto.randomUUID()}`);
      try {
        fs.writeFileSync(temporary,JSON.stringify(result.overrides,null,2)+'\n',{flag:'wx',mode:0o600});
        fs.linkSync(temporary,destination);
      } finally {
        if (fs.existsSync(temporary)) fs.unlinkSync(temporary);
      }
    }
    console.log(JSON.stringify(result));
  } catch (error) {
    console.error(`dsh Cordis derivation: ${error.message}`);
    process.exitCode = 1;
  }
}
