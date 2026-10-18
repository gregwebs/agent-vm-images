#!/usr/bin/env node
'use strict';
// Syntax adapter only; pnpm-lock.mjs owns graph and effective-peer rules and
// project.py owns authorization of a caller-selected contract directory.
const fs = require('node:fs');
const path = require('node:path');
const {pathToFileURL} = require('node:url');
const {spawnSync} = require('node:child_process');

const [beforePrefix, afterPrefix, requestedVersion, ...options] = process.argv.slice(2);
if (process.argv.length < 5 || options.length > 1 || (options.length && options[0] !== '--refresh-lock')) {
  console.error('usage: check-lock-update.js OLD_PREFIX NEW_PREFIX DSH_VERSION [--refresh-lock]');
  process.exitCode = 2;
} else {
  (async () => {
    let directory = path.resolve(__dirname,'../../recipe-contract');
    if (process.env.AGENT_VM_CONTRACT_DIR) {
      // A redirected parser is a filesystem capability: the Python boundary
      // decides whether this caller may name it, and this adapter only reads back
      // the authorized absolute path.
      const checked = spawnSync('python3', ['-B', path.join(__dirname,'project.py'), 'contract-dir',
        beforePrefix, afterPrefix], {encoding:'utf8', timeout:20000, maxBuffer:1024 * 1024});
      if (checked.error || checked.status !== 0) throw new Error(checked.stderr.trim() || 'contract directory rejected');
      directory = checked.stdout.trim();
    }
    if (!fs.lstatSync(path.join(directory,'pnpm-lock.mjs')).isFile()) throw new Error('contract module must be a regular file');
    const {readProject,compareDshUpdate,checkExactPeers} = await import(pathToFileURL(path.join(directory,'pnpm-lock.mjs')).href);
    const before = readProject({prefix:beforePrefix,kind:'dsh'});
    const after = readProject({prefix:afterPrefix,kind:'dsh'});
    const result = compareDshUpdate({before,after,requestedVersion,refresh:options.length === 1});
    console.log(`dsh lock update: ${result.mode} ${result.beforeVersion} -> ${result.afterVersion}; ${checkExactPeers(after).exact} exact peers satisfied`);
  })().catch(error => {
    console.error(`dsh lock update: ${error.message}`);
    process.exitCode = 1;
  });
}
