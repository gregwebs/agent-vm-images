#!/usr/bin/env python3
"""Independent oracle mutation controls, including contradictory raw wire."""
import base64
import copy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location('oracle', Path(__file__).with_name('opencode-audit.py'))
oracle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(oracle)
directory = '/project/nested'
entry = '/opt/agent-vm/opencode-plugins/index.js'

def encoded(data):
    return base64.b64encode(json.dumps(data).encode()).decode()

def located(data):
    return {'location': {'directory': directory}, 'data': data}

wire = {'schema': 1,
    'model_b64': encoded(located([{'id': 'claude', 'providerID': 'claude-code'}])),
    'plugin_b64': encoded(located([{'id': 'openchamber.claude-code', 'state': {'status': 'active'}, 'source': {'type': 'local', 'path': entry}}])),
    'provider_b64': encoded(located([{'id': 'claude-code', 'activation': 'enabled', 'settings': {'baseURL': 'http://127.0.0.1:1234/v1'}}])),
    'health_b64': encoded({'ok': True, 'provider': 'claude-code'})}
report = {'schema': 1, 'plugin_version': '1.4.1', 'sdk_version': '0.3.281', 'sdk_instances': 1,
          'handoff': True, 'sdk_available': True, 'adapter': True, 'omitted_peers': True, 'optional_platforms': False,
          'initialization_models': 1, 'sdk_entry': '/opt/agent-vm/opencode-plugins/node_modules/.pnpm/sdk/sdk.mjs',
          'sdk_directory': '/opt/agent-vm/opencode-plugins/node_modules/.pnpm/sdk', 'claude_path': '/opt/agent/claude'}

def check(w=wire, r=report, status=b'installed\n'):
    oracle.audit(w, r, '1.4.1', status, directory, entry)

check()
count = 0
# Positive control: zero supported models is informational, not a failure.
check(r={**report, 'initialization_models': 0})
for key, mutate in [
    ('model_b64', lambda d: d.update(data=[])),
    ('model_b64', lambda d: d['data'].append(copy.deepcopy(d['data'][0]))),
    ('model_b64', lambda d: d['data'][0].update(providerID='other')),
    ('plugin_b64', lambda d: d['data'][0]['source'].update(path='/wrong')),
    ('plugin_b64', lambda d: d['data'][0]['state'].update(status='failed')),
    ('provider_b64', lambda d: d['data'][0]['settings'].update(baseURL='http://127.0.0.1:0/v1')),
    ('provider_b64', lambda d: d['data'][0].update(activation='disabled')),
    ('health_b64', lambda d: d.update(ok=False)),
    ('health_b64', lambda d: d.update(provider='other')),
] + [(key, lambda d: d['location'].update(directory='/wrong')) for key in ('model_b64', 'plugin_b64', 'provider_b64')]:
    bad = copy.deepcopy(wire)
    data = json.loads(base64.b64decode(bad[key]))
    mutate(data)
    bad[key] = encoded(data)
    try:
        check(w=bad)
    except (AssertionError, ValueError, KeyError):
        count += 1
    else:
        raise AssertionError(f'oracle accepted mutation: {key}')
for key, value in [('plugin_version', '1.4.2'), ('sdk_instances', 2), ('handoff', False), ('adapter', False),
                   ('sdk_available', False), ('initialization_models', -1), ('omitted_peers', False), ('optional_platforms', True)]:
    bad = copy.deepcopy(report)
    bad[key] = value
    try:
        check(r=bad)
    except AssertionError:
        count += 1
    else:
        raise AssertionError(f'oracle accepted runtime mutation: {key}')
for status in (b'installed', b'pending\n', b'absent-transport 6\n', b'installed\ninstalled\n', b'installed\x00\n'):
    try:
        check(status=status)
    except AssertionError:
        count += 1
    else:
        raise AssertionError('oracle accepted stale/malformed status')
print(f'independent OpenCode oracle: {count} wire/runtime/status mutations rejected')

spec = importlib.util.spec_from_file_location('inventory', Path(__file__).with_name('opencode-access-files.py'))
inventory = importlib.util.module_from_spec(spec)
spec.loader.exec_module(inventory)
with tempfile.TemporaryDirectory() as private:
    root = (Path(private) / 'plugins').resolve()
    physical = root / 'node_modules/.pnpm/pkg/node_modules/pkg'
    physical.mkdir(parents=True)
    (physical / 'index.js').write_text('export default {};\n')
    logical = root / 'node_modules/pkg'
    logical.symlink_to('.pnpm/pkg/node_modules/pkg')
    files = set(inventory.content_files(root))
    assert physical / 'index.js' in files and logical / 'index.js' in files
    for target in ('missing', 'bad', '.', '/etc/passwd'):
        bad = root / 'node_modules/bad'
        bad.symlink_to(target)
        try:
            list(inventory.content_files(root))
        except (ValueError, OSError, RuntimeError):
            pass
        else:
            raise AssertionError(f'independent inventory accepted link {target}')
        bad.unlink()
print('independent access inventory: logical/physical coverage and dangling/cyclic/external rejection passed')
