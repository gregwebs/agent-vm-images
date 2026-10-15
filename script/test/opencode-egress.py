#!/usr/bin/env python3
"""Drive the real addon with synthetic flows, without a network or mitmproxy install."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
from urllib.parse import urlsplit
sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[2]
FIXTURES = ROOT / 'script/test/fixtures/installer-egress'

def module(name, file):
    spec = importlib.util.spec_from_file_location(name, file)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result

rules = module('rules', FIXTURES / 'opencode-plugins.py')
manifest = json.loads((ROOT / 'images/tools/opencode/plugins/package.json').read_bytes())
with tempfile.TemporaryDirectory() as work:
    work = Path(work)
    (work / 'config').write_text(json.dumps(rules.rules(manifest)))
    os.environ.update(EGRESS_CONFIG=str(work / 'config'), EGRESS_LOG=str(work / 'log'))
    sys.modules['mitmproxy'] = SimpleNamespace(http=SimpleNamespace(Response=SimpleNamespace(make=lambda *a: a)))
    addon = module('addon', FIXTURES / 'addon.py')
    def flow(url, method='GET'):
        p = urlsplit(url)
        return SimpleNamespace(request=SimpleNamespace(pretty_url=url, pretty_host=p.hostname,
            path=p.path + ('?' + p.query if p.query else ''), method=method), response=None)
    allowed = ['https://registry.npmjs.org/@openchamber%2Fopencode-claude',
               'https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk',
               'https://registry.npmjs.org/zod',
               'https://registry.npmjs.org/zod/-/zod-4.6.7.tgz']
    allowed += ['https://registry.npmjs.org/@openchamber/opencode-claude/-/opencode-claude-1.4.1.tgz',
                'https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk/-/claude-agent-sdk-0.3.281.tgz']
    allowed += ['https://registry.npmjs.org/@anthropic-ai%2Fclaude-agent-sdk-' + s for s in
                ('darwin-arm64', 'darwin-x64', 'linux-arm64', 'linux-arm64-musl', 'linux-x64', 'linux-x64-musl', 'win32-arm64', 'win32-x64')]
    # Provenance bundles: fetched while pnpm enforces the base trust policy.
    allowed += ['https://registry.npmjs.org/-/npm/v1/attestations/@openchamber/opencode-claude@1.4.1',
                'https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk@0.3.281',
                'https://registry.npmjs.org/-/npm/v1/attestations/zod@4.6.7']
    allowed += ['https://registry.npmjs.org/-/npm/v1/attestations/@anthropic-ai/claude-agent-sdk-' + s + '@0.3.281' for s in
                ('darwin-arm64', 'darwin-x64', 'linux-arm64', 'linux-arm64-musl', 'linux-x64', 'linux-x64-musl', 'win32-arm64', 'win32-x64')]
    for url in allowed:
        f = flow(url); addon.request(f); assert f.response is None, url
    denied = ['http://registry.npmjs.org/zod', 'https://registry.npmjs.org:444/zod',
              'https://registry.npmjs.org/zod?token=secret', 'https://evil.invalid/zod',
              'https://registry.npmjs.org/zod/latest', 'https://registry.npmjs.org/zod/-/zod-4.6.8.tgz',
              'https://registry.npmjs.org/pnpm', 'https://registry.npmjs.org/@anthropic-ai/claude-agent-sdk-linux-arm64/-/claude-agent-sdk-linux-arm64-0.3.281.tgz',
              'https://registry.npmjs.org/-/npm/v1/attestations/zod@4.6.8',
              'https://registry.npmjs.org/-/npm/v1/attestations/evil-package@1.0.0']
    for url in denied:
        f = flow(url); addon.request(f); assert f.response is not None, url
    f = flow(allowed[0], 'POST'); addon.request(f); assert f.response is not None
    f = flow(allowed[0]); addon.request(f)
    f.response = SimpleNamespace(status_code=302, headers={'location': 'https://evil.invalid/zod'})
    addon.response(f); assert not addon._learned and f.response[0] == 403
    assert 'secret' not in (work / 'log').read_text()
print('OpenCode plugin egress: exact metadata/tarballs, HTTPS/443/GET/no-query, denied redirects and redaction passed')
