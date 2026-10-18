// Test syntax adapter only: manager fakes must accept both JSON and emitted YAML.
import fs from 'node:fs';
import YAML from '../../../../images/recipe-contract/vendor/yaml/dist/index.js';
console.log(JSON.stringify(YAML.parse(fs.readFileSync('pnpm-workspace.yaml', 'utf8'))));
