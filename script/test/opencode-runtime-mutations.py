#!/usr/bin/env python3
"""Mutate a disposable real isolated installation; no prompts or credentials."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

SOURCE = Path('/opencode-source')
INSTALLED = Path('/opt/agent-vm/opencode-plugins')
CLAUDE = '/opt/agent/.local/bin/claude'

with tempfile.TemporaryDirectory(prefix='opencode-runtime-') as private:
    work = Path(private)
    work.chmod(0o755)
    home = work / 'home'
    home.mkdir()
    env = {key: value for key, value in os.environ.items()
           if not key.startswith(('ANTHROPIC_', 'CLAUDE_', 'OPENCODE_', 'XDG_')) and key != 'HOME'}
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / 'config'), XDG_DATA_HOME=str(home / 'data'),
               XDG_STATE_HOME=str(home / 'state'), XDG_CACHE_HOME=str(home / 'cache'))
    prefix = work / 'plugins'

    def reset():
        if prefix.exists():
            shutil.rmtree(prefix)
        shutil.copytree(INSTALLED, prefix, symlinks=True)
        return prefix

    def run(ok, label):
        result = subprocess.run(['timeout', '--kill-after=5', '25', 'node',
                                 str(SOURCE / 'check-claude-runtime.mjs'), str(prefix), CLAUDE],
                                env=env, capture_output=True, text=True, timeout=35)
        assert (result.returncode == 0) == ok, (label, result.returncode, result.stdout, result.stderr)
        print(f'{label}: status={result.returncode}')

    reset()
    run(True, 'copied virtual store without external store/state')
    sdk = next((prefix / 'node_modules/.pnpm').glob('@anthropic-ai+claude-agent-sdk@*'))
    query = next((prefix / 'node_modules/.pnpm').glob('@openchamber+opencode-claude@*/node_modules/@openchamber/opencode-claude/dist/query.js'))
    query_text = query.read_text()
    old = 'options.pathToClaudeCodeExecutable = pathToClaudeCodeExecutable;'
    assert query_text.count(old) == 1
    query.write_text(query_text.replace(old, '/* executable option deliberately removed */'))
    run(False, 'missing executable handoff mutation')
    reset()
    sdk = next((prefix / 'node_modules/.pnpm').glob('@anthropic-ai+claude-agent-sdk@*'))
    shadow = prefix / 'node_modules/.pnpm/shadow/node_modules/@anthropic-ai/claude-agent-sdk'
    shutil.copytree(sdk / 'node_modules/@anthropic-ai/claude-agent-sdk', shadow)
    run(False, 'same-version physical SDK shadow')
    reset()
    sdk = next((prefix / 'node_modules/.pnpm').glob('@anthropic-ai+claude-agent-sdk@*'))
    shutil.copytree(sdk, sdk.with_name(sdk.name + '_extra-peer@1'), symlinks=True)
    run(False, 'extra SDK peer context')
    reset()
    optional = prefix / 'node_modules/.pnpm/platform/node_modules/@anthropic-ai/claude-agent-sdk-linux-arm64'
    optional.mkdir(parents=True)
    (optional / 'package.json').write_text(json.dumps({'name': '@anthropic-ai/claude-agent-sdk-linux-arm64', 'version': '0.3.281'}))
    run(False, 'optional platform payload')
    for target, label in (('missing', 'dangling dependency'), ('zod', 'cyclic dependency'), ('.', 'directory ancestor cycle'), ('/etc/passwd', 'external dependency')):
        reset()
        link = prefix / 'node_modules/zod'
        link.symlink_to(target)
        run(False, label)
    reset()
    denied = prefix / 'node_modules/.pnpm'
    physical = list(denied.glob('*/node_modules/zod/package.json'))
    logical = prefix / 'node_modules/@anthropic-ai/claude-agent-sdk/package.json'
    assert physical
    for file in (physical[0], logical):
        control = subprocess.run(['/contract/recipe-contract', 'tool-access', '--quiet', '--content', str(file)], capture_output=True)
        assert control.returncode == 0, control.stderr
    denied.chmod(0o700)
    for file in (physical[0], logical):
        result = subprocess.run(['/contract/recipe-contract', 'tool-access', '--quiet', '--content', str(file)], capture_output=True)
        assert result.returncode != 0 and b'.pnpm' in result.stderr, result.stderr
    denied.chmod(0o755)
    reset()
    bin_dir = work / 'bin'
    bin_dir.mkdir()
    for name in ('npm', 'npx', 'pnpm', 'pnx', 'pnpx', 'bun'):
        command = bin_dir / name
        command.write_text('#!/bin/sh\necho package-manager-invoked >&2\nexit 91\n')
        command.chmod(0o755)
    project = work / 'project'
    nested = project / 'nested'
    nested.mkdir(parents=True)
    config = project / 'opencode.json'
    config.write_text('{"plugins":[]}\n')
    original = config.read_bytes()
    capture_env = env | {'OPENCODE_CONFIG': os.environ['OPENCODE_CONFIG'], 'PATH': str(bin_dir) + ':' + env['PATH']}
    command = ['python3', str(SOURCE / 'probe-opencode.py'), 'capture', '--binary', '/audit/fixtures/opencode-compose.sh',
               '--state-root', str(work), '--directory', str(nested), '--timeout', '60']
    result = subprocess.run(command, env=capture_env, capture_output=True, timeout=80)
    assert result.returncode == 0, result.stderr
    assert b'package-manager-invoked' not in result.stderr
    assert config.read_bytes() == original, 'project config rewritten'
    evidence = work / 'composed.json'
    evidence.write_bytes(result.stdout)
    result = subprocess.run(['python3', str(SOURCE / 'probe-opencode.py'), 'check', '--evidence', str(evidence),
                             '--plugin-entry', str(INSTALLED / 'index.js'), '--directory', str(nested)], capture_output=True)
    assert result.returncode == 0, result.stderr
    config.write_text('{"plugins":["-openchamber.claude-code"]}\n')
    result = subprocess.run(command, env=capture_env, capture_output=True, timeout=80)
    assert result.returncode != 0, 'project disable was ignored'
    config.write_bytes(original)
    empty = work / 'replacement.json'
    empty.write_text('{"plugins":[]}\n')
    result = subprocess.run(command, env=capture_env | {'OPENCODE_CONFIG': str(empty)}, capture_output=True, timeout=80)
    assert result.returncode != 0, 'caller config replacement was ignored'
    wrong = bin_dir / 'claude'
    wrong.write_text('#!/bin/sh\nprintf "2.1.286 (Claude Code)\\n"\n')
    wrong.chmod(0o755)
    result = subprocess.run(['node', str(SOURCE / 'check-claude-runtime.mjs'), str(prefix), CLAUDE],
                            env=env | {'PATH': str(bin_dir) + ':' + env['PATH']}, capture_output=True, timeout=25)
    assert result.returncode != 0, 'wrong Claude PATH selection passed'
    print('global/project composition, nested directory, no manager, opt-outs and wrong Claude PATH passed')
print('OpenCode runtime mutations passed')
