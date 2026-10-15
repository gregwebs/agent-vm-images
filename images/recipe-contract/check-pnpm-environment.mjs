// Maintained builds accept launcher defaults, not caller package-manager policy.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
const defaults = {
  PNPM_CONFIG_MINIMUM_RELEASE_AGE: '1440',
  PNPM_CONFIG_MINIMUM_RELEASE_AGE_STRICT: 'true',
  PNPM_CONFIG_MINIMUM_RELEASE_AGE_IGNORE_MISSING_TIME: 'false',
  PNPM_CONFIG_TRUST_POLICY: 'no-downgrade',
  PNPM_CONFIG_IGNORE_SCRIPTS: 'true',
};
try {
  for (const [key, value] of Object.entries(process.env)) {
    if (/^(npm_config_|pnpm_config_|corepack_|pnpm_home$)/i.test(key)) {
      assert.ok(Object.hasOwn(defaults, key) && (!value || value === defaults[key]), `unsupported package-manager environment: ${key}`);
    }
  }
  for (const key of ['NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS']) {
    assert.ok(!process.env[key], `unsupported manager runtime override: ${key}`);
  }
  assert.notEqual(process.env.NODE_TLS_REJECT_UNAUTHORIZED, '0', 'TLS verification cannot be disabled');
  for (const prefix of process.argv.slice(2)) {
    for (const file of ['.npmrc', '.pnpmfile.cjs', '.pnpmfile.js', '.pnpmfile.mjs', 'pnpmfile.cjs']) {
      let present = false;
      try {
        fs.lstatSync(path.join(prefix, file));
        present = true;
      } catch (error) {
        if (error.code !== 'ENOENT') throw error;
      }
      assert.ok(!present, `unreviewed package-manager input: ${file}`);
    }
  }
} catch (error) {
  console.error(`pnpm environment: ${error.message}`);
  process.exitCode = 1;
}
