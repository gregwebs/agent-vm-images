#!/usr/bin/env python3
"""Hermetic owning-verifier controls with a synthetic plugin API/server."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
SOURCE = ROOT / 'images/tools/opencode'
os.umask(0o022)

sys.path.insert(0, str(ROOT / 'script/test'))
from contract_context import native_contract_directory  # noqa: E402  (path first)

CONTRACT = native_contract_directory(ROOT)
with tempfile.TemporaryDirectory(prefix='opencode-verifier-', dir='/tmp') as private:
    work = Path(private)
    work.chmod(0o755)
    prefix = work / 'plugins'
    prefix.mkdir()
    for name in ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256', 'index.js'):
        shutil.copyfile(SOURCE / 'plugins' / name, prefix / name)
    modules = prefix / 'node_modules'
    store = modules / '.pnpm'
    plugin = store / 'plugin/node_modules/@openchamber/opencode-claude'
    sdk = store / 'sdk/node_modules/@anthropic-ai/claude-agent-sdk'
    zod = store / 'zod/node_modules/zod'
    for directory, name, version in ((plugin, '@openchamber/opencode-claude', '1.4.1'),
                                     (sdk, '@anthropic-ai/claude-agent-sdk', '0.3.281'), (zod, 'zod', '4.6.7')):
        directory.mkdir(parents=True)
        (directory / 'package.json').write_text(json.dumps({'name': name, 'version': version, 'type': 'module', 'exports': './index.js'}))
        (directory / 'index.js').write_text('export default {};\n')
    for link, target in ((modules / '@openchamber/opencode-claude', plugin),
                         (modules / '@anthropic-ai/claude-agent-sdk', sdk),
                         (plugin.parent.parent / '@anthropic-ai/claude-agent-sdk', sdk),
                         (plugin.parent.parent / 'zod', zod), (sdk.parent.parent / 'zod', zod)):
        link.parent.mkdir(parents=True, exist_ok=True)
        link.symlink_to(os.path.relpath(target, link.parent))
    (plugin / 'dist').mkdir()
    shutil.copyfile(ROOT / 'script/test/fixtures/opencode-query.js', plugin / 'dist/query.js')
    (plugin / 'dist/executable-path.js').write_text('export async function resolveClaudeCodeExecutable() { return process.env.TEST_CLAUDE_PATH; }\n')
    native = work / 'native'
    (native / '.local/bin').mkdir(parents=True)
    (native / '.opencode/bin').mkdir(parents=True)
    claude = native / '.local/bin/claude'
    claude.write_text('#!/bin/sh\nprintf "fixture\\n"\n')
    claude.chmod(0o755)
    shutil.copyfile(ROOT / 'script/test/fixtures/opencode-server.py', native / '.opencode/bin/opencode')
    (native / '.opencode/bin/opencode').chmod(0o755)
    config = work / 'opencode.json'
    config.write_text(json.dumps({'plugins': [str(prefix)]}))
    status = work / 'status'
    status.mkdir()
    env = dict(os.environ, AGENT_VM_CONTRACT_DIR=str(CONTRACT),
               AGENT_VM_OPENCODE_SOURCE_DIR=str(SOURCE), AGENT_VM_OPENCODE_PLUGINS_PREFIX=str(prefix),
               AGENT_VM_OPENCODE_CONFIG_PATH=str(config), OPENCODE_CONFIG=str(config),
               AGENT_VM_CLAUDE_PREFIX=str(native), AGENT_VM_OPENCODE_PREFIX=str(native),
               AGENT_VM_INSTALL_STATUS_DIR=str(status), TEST_PLUGIN_ENTRY=str(prefix / 'index.js'), TEST_CLAUDE_PATH=str(claude))
    def run(ok, label, extra=None):
        (status / 'opencode-claude').write_text('installed\n')
        result = subprocess.run(['sh', str(SOURCE / 'verify-opencode-plugins.sh')], env=env | (extra or {}), capture_output=True, timeout=120)
        assert (result.returncode == 0) == ok, (label, result.returncode, result.stdout, result.stderr)
        print(f'{label}: status={result.returncode}')
    run(True, 'healthy plumbing (synthetic plugin; not upstream runtime proof)')
    run(True, 'zero supported models tolerated (build has no guest credentials)', {'TEST_EMPTY_MODELS': '1'})
    # Zero supported models is now tolerated (M1), so the handoff assertions must
    # still bite on their own: a wrong executable, the wrong cwd, or a leaked
    # credential must each fail the owning verifier.
    query = plugin / 'dist/query.js'
    original_query = query.read_text()
    query.write_text(original_query.replace(
        'pathToClaudeCodeExecutable: await resolveClaudeCodeExecutable()',
        'pathToClaudeCodeExecutable: "/bin/sh"'))
    run(False, 'wrong Claude executable handoff')
    query.write_text(original_query.replace(
        'for (const key of [\'ANTHROPIC_API_KEY\', \'ANTHROPIC_AUTH_TOKEN\', \'CLAUDE_CODE_OAUTH_TOKEN\']) delete env[key];',
        'for (const key of [\'ANTHROPIC_API_KEY\', \'ANTHROPIC_AUTH_TOKEN\', \'CLAUDE_CODE_OAUTH_TOKEN\']) delete env[key]; env.ANTHROPIC_API_KEY = "leak";'))
    run(False, 'credential leaked into handoff env')
    query.write_text(original_query.replace('cwd: params.cwd', 'cwd: "/tmp"'))
    run(False, 'wrong handoff cwd')
    query.write_text(original_query)
    run(False, 'missing image ENV despite stale record', {'OPENCODE_CONFIG': ''})
    run(False, 'wrong selected version', {'AGENT_VM_VERSION_OPENCODE_CLAUDE': '1.4.2'})
    run(False, 'wrong activation location', {'TEST_PROBE_MODE': 'location'})
    config_bytes = config.read_bytes()
    config.unlink()
    config.symlink_to(work / 'missing-config')
    run(False, 'dangling config despite stale record')
    config.unlink()
    config.write_bytes(config_bytes)
    adapter = (prefix / 'index.js').read_text()
    (prefix / 'index.js').write_text('export default {};\n')
    run(False, 'broken adapter despite stale record')
    (prefix / 'index.js').write_text(adapter)
    (store / 'ancestor-cycle').symlink_to(modules)
    run(False, 'directory cycle cannot hide behind canonical deduplication')
    (store / 'ancestor-cycle').unlink()
    (sdk / 'package.json').write_text(json.dumps({'name': '@anthropic-ai/claude-agent-sdk', 'version': '0.0.0', 'type': 'module', 'exports': './index.js'}))
    run(False, 'wrong actual SDK despite stale record')
print('OpenCode owning verifier controls passed')
