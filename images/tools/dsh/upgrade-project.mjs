// Developer staging adapter. Production graph policy stays in pnpm-lock.mjs.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import YAML from '../../recipe-contract/vendor/yaml/dist/index.js';
import {readProject, compareDshUpdate, deriveDshCordisOverrides, readDshCordisOverrideMap, writeInputPins} from '../../recipe-contract/pnpm-lock.mjs';

const [action, before, stage, argument, extra] = process.argv.slice(2);
const file = (prefix, name) => path.join(prefix, name);
try {
  const original = readProject({prefix: before, kind: 'dsh'});
  if (action === 'tentative') {
    assert.ok(argument && !extra && /^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$/.test(argument), 'expected target exact selection');
    fs.mkdirSync(stage);
    const manifest = {...original.manifest, dependencies: {'@deepseek-ai/dsh': argument}};
    fs.writeFileSync(file(stage, 'package.json'), JSON.stringify(manifest, null, 2) + '\n');
    const workspace = YAML.parseDocument(fs.readFileSync(file(before, 'pnpm-workspace.yaml'), 'utf8'));
    workspace.delete('overrides');
    fs.writeFileSync(file(stage, 'pnpm-workspace.yaml'), String(workspace));
  } else if (action === 'final') {
    const proof = JSON.parse(fs.readFileSync(argument, 'utf8'));
    if (extra) assert.deepEqual(readDshCordisOverrideMap(extra), proof.overrides, 'reviewed map differs from independent requirements');
    else assert.deepEqual(proof.overrides, original.workspace.overrides, 'Cordis map changed; use --propose-cordis-overrides FILE then --cordis-overrides FILE');
    fs.mkdirSync(stage);
    fs.copyFileSync(file(path.dirname(argument) + '/tentative', 'package.json'), file(stage, 'package.json'));
    const bytes = fs.readFileSync(file(before, 'pnpm-workspace.yaml'), 'utf8');
    if (!extra || Object.entries(proof.overrides).every(([key, value]) => original.workspace.overrides[key] === value)) {
      fs.writeFileSync(file(stage, 'pnpm-workspace.yaml'), bytes);
    } else {
      const workspace = YAML.parseDocument(bytes);
      workspace.set('overrides', proof.overrides);
      fs.writeFileSync(file(stage, 'pnpm-workspace.yaml'), String(workspace));
    }
    // Only the final resolution may start from the committed graph.
    fs.copyFileSync(file(before, 'pnpm-lock.yaml'), file(stage, 'pnpm-lock.yaml'));
  } else if (action === 'check-final') {
    writeInputPins({prefix: stage, kind: 'dsh'});
    const project = readProject({prefix: stage, kind: 'dsh'});
    const proof = JSON.parse(fs.readFileSync(argument, 'utf8'));
    // This disposable copy has derivation capability only, never install/pin.
    const check = path.join(path.dirname(stage), 'final-requirements');
    fs.mkdirSync(check);
    fs.copyFileSync(file(stage, 'package.json'), file(check, 'package.json'));
    const workspace = YAML.parse(fs.readFileSync(file(stage, 'pnpm-workspace.yaml'), 'utf8'));
    const lock = YAML.parse(fs.readFileSync(file(stage, 'pnpm-lock.yaml'), 'utf8'));
    delete workspace.overrides; delete lock.overrides;
    fs.writeFileSync(file(check, 'pnpm-workspace.yaml'), JSON.stringify(workspace));
    fs.writeFileSync(file(check, 'pnpm-lock.yaml'), JSON.stringify(lock));
    // Both reviewed maps break a family tie the same way the tentative proof did:
    // the explicitly approved map decides, the committed one may only re-pin a
    // candidate. Passing them in is what makes the review route reachable.
    const approved = extra ? readDshCordisOverrideMap(extra) : undefined;
    const finalProof = deriveDshCordisOverrides({prefix: check, committed: original.workspace.overrides, approved});
    assert.deepEqual(finalProof.overrides, proof.overrides, 'tentative/final selected family drift');
    assert.deepEqual(finalProof.requirements.filter(row => row.kind !== 'dependency'), proof.requirements.filter(row => row.kind !== 'dependency'), 'tentative/final peer requirement drift');
    if (fs.readFileSync(file(before, 'pnpm-workspace.yaml')).equals(fs.readFileSync(file(stage, 'pnpm-workspace.yaml')))) {
      compareDshUpdate({before: original, after: project, requestedVersion: project.selection.version, refresh: true});
    } else {
      assert.ok(extra, 'changed map requires explicit review');
      assert.deepEqual(readDshCordisOverrideMap(extra), finalProof.overrides);
    }
    console.log(JSON.stringify({selection: project.selection, requirements: finalProof.requirements}));
  } else throw new Error('unknown developer staging action');
} catch (error) {
  console.error(`dsh upgrade: ${error.message}`);
  process.exitCode = 1;
}
