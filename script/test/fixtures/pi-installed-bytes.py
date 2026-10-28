#!/usr/bin/env python3
"""Build a real-layout Pi byte-proof fixture for `script/test/pi-installed-bytes.mjs`.

The fixture is deliberately *not* the production Pi graph: it is the smallest
tree that still has every property the byte checker must survive — an isolated
pnpm virtual store with opaque entry names, a transitive sibling, scoped and
unscoped dependencies, a declared bin provider whose archive mode is not 0755, a
platform-excluded optional package, and two peer contexts of one identity.

`meta` (printed as JSON) names every path and every derived expectation, so a
mutant in the node suite is one filesystem edit plus one checker run, and a
repaired positive is the same edit reverted, never a reinstall.
"""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import sys
import tarfile

ROOT = '@earendil-works/pi-coding-agent'
SCOPE = '@earendil-works'
FAMILY = ('chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui')
MODERN = ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui')

# name -> (version, bin mapping, {relative path: (content, executable in archive)})
PACKAGES = {
    ROOT: ('1.4.0', {'pi': 'dist/bundle/cli.js'}, {
        'dist/bundle/cli.js': ('#!/usr/bin/env node\nrequire("./main")\n', False),
        'dist/index.js': ('export const pi = 1;\n', False),
    }),
    f'{SCOPE}/chord': ('1.4.0', None, {'dist/index.js': ('export const chord = 1;\n', False)}),
    f'{SCOPE}/pi-agent-core': ('1.4.0', None, {'dist/index.js': ('export const core = 1;\n', False)}),
    f'{SCOPE}/pi-ai': ('1.4.0', {'pi-ai': 'dist/cli.js'}, {
        # The authenticated archive ships this at 0644; installation derives 0755
        # because it is a locked, declared bin target.
        'dist/cli.js': ('#!/usr/bin/env node\nrequire("./provider")\n', False),
        'dist/provider.js': ('module.exports = 1;\n', False),
    }),
    f'{SCOPE}/pi-codemode': ('1.4.0', None, {'dist/index.js': ('export const codemode = 1;\n', False)}),
    f'{SCOPE}/pi-mcp': ('1.4.0', None, {'dist/index.js': ('export const mcp = 1;\n', False)}),
    f'{SCOPE}/pi-telemetry': ('1.4.0', None, {'dist/index.js': ('export const telemetry = 1;\n', False)}),
    f'{SCOPE}/pi-tui': ('1.4.0', None, {'dist/index.js': ('export const tui = 1;\n', False)}),
    '@anthropic-ai/sdk': ('0.129.0', {'anthropic-ai-sdk': 'bin/cli.js'}, {
        'bin/cli.js': ('#!/usr/bin/env node\nconsole.log("sdk")\n', True),
    }),
    'marked': ('18.0.11', {'marked': 'bin/marked.js'}, {
        'bin/marked.js': ('#!/usr/bin/env node\nconsole.log("marked")\n', True),
    }),
    'yaml': ('2.9.0', {'yaml': 'bin.mjs'}, {'bin.mjs': ('#!/usr/bin/env node\n', True)}),
    'esbuild': ('0.28.2', {'esbuild': 'bin/esbuild'}, {'bin/esbuild': ('#!/bin/sh\necho esbuild\n', True)}),
    'diff': ('8.0.4', None, {'index.js': ('module.exports = 1;\n', False)}),
    'quickjs-wasi': ('3.6.2', None, {'index.js': ('module.exports = 1;\n', False)}),
    '@esbuild/linux-arm64': ('0.28.2', None, {'bin/esbuild': ('ELF\n', True)}),
}

# A platform selector that no modelled target satisfies, so the omission rule is
# exercised instead of a missing payload.
PLATFORM = {'@esbuild/linux-arm64': {'os': ['linux'], 'cpu': ['mips'], 'libc': []}}

# shape -> dependency edges by package, plus the optional platform package.
# An edge is a name, or (name, peer-context index) for the two-context identity.
SHAPES = {
    'legacy': {
        'version': '0.87.0',
        'members': FAMILY,
        'edges': {
            ROOT: ['chord', 'pi-agent-core', 'pi-ai', 'pi-tui'],
            f'{SCOPE}/chord': ['esbuild', '@esbuild/linux-arm64'],
            f'{SCOPE}/pi-agent-core': ['pi-ai', 'pi-telemetry', 'yaml', 'diff'],
            f'{SCOPE}/pi-ai': ['@anthropic-ai/sdk'],
            f'{SCOPE}/pi-tui': ['marked'],
            f'{SCOPE}/pi-telemetry': [],
        },
        'optional': {'@esbuild/linux-arm64'},
        'contexts': {},
    },
    'modern': {
        'version': '1.4.0',
        'members': MODERN,
        'edges': {
            ROOT: ['pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui'],
            f'{SCOPE}/pi-agent-core': ['pi-ai', 'pi-telemetry', ('diff', 0)],
            f'{SCOPE}/pi-ai': ['@anthropic-ai/sdk'],
            f'{SCOPE}/pi-codemode': ['quickjs-wasi'],
            f'{SCOPE}/pi-mcp': [('diff', 1)],
            f'{SCOPE}/pi-tui': ['marked'],
            f'{SCOPE}/pi-telemetry': [],
        },
        'optional': set(),
        # Two peer contexts of one identity, so a same-version directory from the
        # wrong context can be substituted in a mutant.
        'contexts': {'diff': ['diff@8.0.4', 'diff@8.0.4(peerctx)']},
    },
}


def package_json(name, version, edges):
    manifest = {'name': name, 'version': version, 'type': 'module', 'license': 'MIT'}
    if PACKAGES[name][1]:
        manifest['bin'] = PACKAGES[name][1]
    if edges:
        manifest['dependencies'] = dict(sorted(edges.items()))
    return json.dumps(manifest, indent=2) + '\n'


def write_package(directory, name, version, declared_edges, executable_targets=()):
    directory.mkdir(parents=True, exist_ok=True)
    (directory / 'package.json').write_text(package_json(name, version, declared_edges))
    os.chmod(directory / 'package.json', 0o644)
    (directory / 'LICENSE').write_text('MIT\n')
    os.chmod(directory / 'LICENSE', 0o644)
    for relative, (content, executable) in sorted(PACKAGES[name][2].items()):
        target = directory / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
        os.chmod(target, 0o755 if (executable or relative in executable_targets) else 0o644)


def tarball(name, version):
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode='w:gz', format=tarfile.GNU_FORMAT) as archive:
        def add(relative, content, mode):
            payload = content.encode()
            info = tarfile.TarInfo(f'package/{relative}')
            info.size = len(payload)
            info.mode = mode
            info.mtime = 0
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            archive.addfile(info, io.BytesIO(payload))
        add('package.json', package_json(name, version, None), 0o644)
        add('LICENSE', 'MIT\n', 0o644)
        for relative, (content, executable) in sorted(PACKAGES[name][2].items()):
            add(relative, content, 0o755 if executable else 0o644)
    return buffer.getvalue()


def sri(data):
    return 'sha512-' + base64.b64encode(hashlib.sha512(data).digest()).decode()


def fake_sri(seed):
    return sri(hashlib.sha256(seed.encode()).digest() * 2)


def entry_name(name, version, suffix):
    return f'{name.replace("/", "+")}@{version}' + (f'({suffix})' if suffix else '')


def build(work, shape_name):
    shape = SHAPES[shape_name]
    version_of = {name: PACKAGES[name][0] for name in PACKAGES}
    version_of[ROOT] = shape['version']
    members = [f'{SCOPE}/{base}' for base in shape['members']]
    # The family ships at the selected Pi version, not at a per-package version.
    for name in members:
        version_of[name] = shape['version']

    work = Path(work)
    prefix, parent, contract = work / 'prefix', work / 'parent', work / 'contract'
    evidence = parent / 'evidence'
    for directory in (work, prefix, parent, contract):
        directory.mkdir(parents=True, exist_ok=True)

    def dep_name(spec):
        raw = spec[0] if isinstance(spec, tuple) else spec
        return f'{SCOPE}/{raw}' if raw in FAMILY or raw in MODERN else raw

    # One snapshot per (name, context); keys carry opaque peer contexts.
    snapshots = {}
    for name in sorted({*members, ROOT, *[dep_name(spec) for specs in shape['edges'].values() for spec in specs]}):
        keys = shape['contexts'].get(name, [f'{name}@{version_of[name]}'])
        snapshots[name] = [{'key': key, 'name': name, 'version': version_of[name], 'context': (
            key.split('(', 1)[1][:-1] if '(' in key else None), 'index': index}
            for index, key in enumerate(keys)]

    def context_key(name, index):
        entries = snapshots[name]
        return entries[min(index, len(entries) - 1)]['key']

    def edge_specs(owner):
        return shape['edges'].get(owner, [])

    provider_integrities = {}
    for name, entries in snapshots.items():
        if name == ROOT or name in members:
            continue
        data = tarball(name, version_of[name])
        digest = sri(data)
        provider_integrities[name] = digest
        target = evidence / name / version_of[name] / (base64.b64decode(digest.split('-', 1)[1]).hex() + '.tgz')
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)

    graph = {'selection': {'kind': 'pi', 'version': shape['version']},
             'packages': [], 'snapshots': [], 'importer': []}
    for name, entries in sorted(snapshots.items()):
        graph['packages'].append({
            'id': {'name': name, 'version': version_of[name]},
            'integrity': provider_integrities.get(name) or fake_sri(f'{name}@{version_of[name]}'),
            'tarball': f'https://registry.npmjs.org/{name}/-/{name.rsplit("/", 1)[-1]}-{version_of[name]}.tgz',
            'peers': {}, 'optionalPeers': [],
            'hasBin': PACKAGES[name][1] is not None,
            'platform': PLATFORM.get(name, {'os': [], 'cpu': [], 'libc': []}),
            'optional': name in shape['optional'],
        })
        for entry in entries:
            edges = [{'name': dep_name(spec),
                      'target': context_key(dep_name(spec), spec[1] if isinstance(spec, tuple) else -1),
                      'optional': dep_name(spec) in shape['optional']} for spec in edge_specs(name)]
            entry['edges'] = edges
            context = entry['context']
            graph['snapshots'].append({
                'id': {'packageId': {'name': name, 'version': version_of[name]},
                       'peerContext': [] if context is None else [{
                           'packageId': {'name': name, 'version': version_of[name]},
                           'peerContext': [], 'key': context}],
                       'key': entry['key']},
                'edges': edges,
                'optional': name in shape['optional'],
            })
    root_key = context_key(ROOT, -1)
    graph['importer'] = [{'name': ROOT, 'target': root_key, 'optional': False}]
    (work / 'graph.json').write_text(json.dumps(graph, indent=2) + '\n')

    modules = prefix / 'node_modules'
    store = modules / '.pnpm'
    modules.mkdir(parents=True, exist_ok=True)
    (prefix / 'package.json').write_text(json.dumps(
        {'name': 'pi-byte-fixture', 'private': True, 'dependencies': {ROOT: shape['version']}}, indent=2) + '\n')

    entry_directory = {}
    for name, entries in snapshots.items():
        for entry in entries:
            suffix = entry['context'] or (f'ctx{entry["index"]}' if len(entries) > 1 else None)
            entry_directory[entry['key']] = store / entry_name(name, version_of[name], suffix)

    all_entries = [entry for entries in snapshots.values() for entry in entries]
    omitted = {entry['key'] for entry in all_entries if entry['name'] in shape['optional']}
    for entry in all_entries:
        if entry['key'] in omitted:
            continue
        bin_targets = tuple((PACKAGES[entry['name']][1] or {}).values())
        write_package(entry_directory[entry['key']] / 'node_modules' / entry['name'],
                      entry['name'], entry['version'], None, bin_targets)

    links = []
    for entry in all_entries:
        if entry['key'] in omitted:
            continue
        directory = entry_directory[entry['key']] / 'node_modules'
        commands = []
        for edge in entry['edges']:
            if edge['target'] in omitted:
                continue
            dependency = next(item['id']['packageId']['name'] for item in graph['snapshots']
                              if item['id']['key'] == edge['target'])
            target_dir = entry_directory[edge['target']] / 'node_modules' / dependency
            link = directory / dependency
            link.parent.mkdir(parents=True, exist_ok=True)
            text = os.path.relpath(target_dir, link.parent)
            os.symlink(text, link)
            links.append({'path': str(link.relative_to(prefix)), 'text': text,
                          'targetKey': edge['target'], 'name': dependency})
            for command, target in (PACKAGES[dependency][1] or {}).items():
                commands.append((command, edge['target'], dependency, target))
        if commands:
            # pnpm links a package's dependency commands inside that package's own
            # root: <entry>/node_modules/<name>/node_modules/.bin.
            bin_dir = directory / entry['name'] / 'node_modules' / '.bin'
            bin_dir.mkdir(parents=True, exist_ok=True)
            for command, key, dependency, target in sorted(commands):
                target_path = entry_directory[key] / 'node_modules' / dependency / target
                text = os.path.relpath(target_path, bin_dir)
                os.symlink(text, bin_dir / command)
                links.append({'path': str(bin_dir.relative_to(prefix) / command), 'text': text,
                              'bin': command, 'owner': entry['name']})

    root_module = modules / SCOPE / 'pi-coding-agent'
    root_module.parent.mkdir(parents=True, exist_ok=True)
    os.symlink(os.path.relpath(entry_directory[root_key] / 'node_modules' / ROOT, root_module.parent), root_module)
    root_bin = modules / '.bin'
    root_bin.mkdir(parents=True, exist_ok=True)
    os.symlink('../@earendil-works/pi-coding-agent/dist/bundle/cli.js', root_bin / 'pi')

    for name in [*members, ROOT]:
        extraction = parent / name / version_of[name]
        extraction.mkdir(parents=True, exist_ok=True)
        write_package(extraction, name, version_of[name], None)

    contract_binary = contract / 'recipe-contract'
    contract_binary.write_text('#!/bin/sh\n'
                               'test "$1" = pnpm-lock && test "$2" = graph && test "$3" = pi || exit 9\n'
                               f'exec cat {work / "graph.json"}\n')
    os.chmod(contract_binary, 0o755)

    derived_bins = {}
    for base in shape['members']:
        name = f'{SCOPE}/{base}'
        entry = next(item for item in graph['snapshots'] if item['id']['packageId']['name'] == name)
        commands = set()
        for edge in entry['edges']:
            dependency = next(item['id']['packageId']['name'] for item in graph['snapshots']
                              if item['id']['key'] == edge['target'])
            commands.update((PACKAGES[dependency][1] or {}))
        derived_bins[base] = sorted(commands)

    print(json.dumps({
        'shape': shape_name,
        'version': shape['version'],
        'prefix': str(prefix),
        'parent': str(parent),
        'contract': str(contract),
        'graph': str(work / 'graph.json'),
        'family': list(shape['members']),
        'sibling': shape['members'][0],
        'derivedBins': derived_bins,
        'links': links,
        'entryDirectory': {key: str(path) for key, path in entry_directory.items()},
        'omittedKeys': sorted(omitted),
        'versions': {name: version_of[name] for name in snapshots},
    }, indent=2))


# Archive defects the extractor must refuse before it writes anything. Each case
# is a whole member list, so the preflight is exercised exactly as a hostile
# tarball would exercise it.
HOSTILE = {
    'traversal': [('package/../../escape', b'x', 0o644, tarfile.REGTYPE, '')],
    'absolute': [('/etc/passwd', b'x', 0o644, tarfile.REGTYPE, '')],
    'backslash': [('package\\evil', b'x', 0o644, tarfile.REGTYPE, '')],
    'empty-segment': [('package//evil', b'x', 0o644, tarfile.REGTYPE, '')],
    'duplicate': [('package/a', b'1', 0o644, tarfile.REGTYPE, ''), ('package/a', b'2', 0o644, tarfile.REGTYPE, '')],
    'special': [('package/fifo', b'', 0o644, tarfile.FIFOTYPE, '')],
    'hardlink': [('package/a', b'1', 0o644, tarfile.REGTYPE, ''),
                 ('package/b', b'', 0o644, tarfile.LNKTYPE, 'package/a')],
    'link-escape': [('package/link', b'', 0o755, tarfile.SYMTYPE, '../../outside')],
    'link-absolute': [('package/link', b'', 0o755, tarfile.SYMTYPE, '/etc/passwd')],
    'ancestor-file': [('package/a', b'file', 0o644, tarfile.REGTYPE, ''),
                      ('package/a/b', b'child', 0o644, tarfile.REGTYPE, '')],
    'ancestor-link': [('package/a', b'', 0o755, tarfile.SYMTYPE, 'b'),
                      ('package/a/c', b'child', 0o644, tarfile.REGTYPE, '')],
    'privilege': [('package/x', b'x', 0o4755, tarfile.REGTYPE, '')],
    'unexpected-root': [('other/a', b'x', 0o644, tarfile.REGTYPE, '')],
    'deep': [('package/' + '/'.join(['d'] * 40) + '/f', b'x', 0o644, tarfile.REGTYPE, '')],
    'long-name': [('package/' + 'n' * 1100, b'x', 0o644, tarfile.REGTYPE, '')],
}


def hostile(destination, case):
    members = HOSTILE[case]
    with tarfile.open(destination, 'w:gz', format=tarfile.GNU_FORMAT) as archive:
        for name, payload, mode, kind, linkname in members:
            info = tarfile.TarInfo(name)
            info.mode = mode
            info.mtime = 0
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            info.type = kind
            info.linkname = linkname
            if kind == tarfile.REGTYPE:
                info.size = len(payload)
                archive.addfile(info, io.BytesIO(payload))
            else:
                archive.addfile(info)


def benign(destination):
    with tarfile.open(destination, 'w:gz', format=tarfile.GNU_FORMAT) as archive:
        def add(name, payload, mode, kind=tarfile.REGTYPE, linkname=''):
            info = tarfile.TarInfo(name)
            info.mode = mode
            info.mtime = 0
            info.uid = info.gid = 0
            info.uname = info.gname = ''
            info.type = kind
            info.linkname = linkname
            if kind == tarfile.REGTYPE:
                info.size = len(payload)
                archive.addfile(info, io.BytesIO(payload))
            else:
                archive.addfile(info)
        add('package', b'', 0o755, tarfile.DIRTYPE)
        add('package/package.json', b'{"name":"benign","version":"1.0.0"}\n', 0o644)
        add('package/bin/tool', b'#!/bin/sh\n', 0o755)
        add('package/lib', b'', 0o755, tarfile.DIRTYPE)
        add('package/lib/alias', b'', 0o644, tarfile.SYMTYPE, 'index.js')
        add('package/lib/index.js', b'module.exports = 1;\n', 0o644)
        add('package/README.md', b'readme\n', 0o644)


def main(argv):
    if len(argv) == 3 and argv[0] == 'hostile':
        hostile(Path(argv[2]), argv[1])
        return
    if len(argv) == 2 and argv[0] == 'benign':
        benign(Path(argv[1]))
        return
    if len(argv) == 3 and argv[0] == 'build':
        build(argv[1], argv[2])
        return
    sys.stderr.write('usage: pi-installed-bytes.py {build WORK legacy|modern |'
                     ' hostile CASE TARBALL | benign TARBALL}\n')
    raise SystemExit(2)


if __name__ == '__main__':
    main(sys.argv[1:])
