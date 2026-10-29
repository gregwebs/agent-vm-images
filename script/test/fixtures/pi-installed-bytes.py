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

`legacy-probe` is the same historical family plus the three payloads the runtime
proof needs in order to finish offline: a `@google/genai` client export, a
`protobufjs` `Root` and an `esbuild` wrapper whose transform and native command can
run. Those three are stubs — the real ones are an integration leg (the focused
harness) — and they exist only so `check-runtime.mjs` reaches its transform,
scoped-chord and native-command proofs in a hermetic test.
"""
import base64
import gzip
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
    # The published yaml@2.9.0 spelling: a leading `./` is a spelling, not a
    # location, and pnpm normalizes it exactly like npm does.
    'yaml': ('2.9.0', {'yaml': './bin.mjs'}, {'bin.mjs': ('#!/usr/bin/env node\n', True)}),
    'esbuild': ('0.28.2', {'esbuild': 'bin/esbuild'}, {
        'bin/esbuild': ('#!/bin/sh\necho esbuild\n', True),
        # The transform probe imports the wrapper's JS entry, not its command.
        'lib/main.js': ('export function transformSync(source) {\n'
                        "  return {code: source.replace(/: *[A-Za-z_$]+/g, '')};\n"
                        '}\n', False),
    }),
    'diff': ('8.0.4', None, {'index.js': ('module.exports = 1;\n', False)}),
    'quickjs-wasi': ('3.6.2', None, {'index.js': ('module.exports = 1;\n', False)}),
    # The native platform package publishes no `bin` at all, exactly like the real
    # 0.28.2 packages: the wrapper finds `bin/esbuild` by convention. It answers
    # `--version` with its own version, so the file the probe runs is bound to the
    # locked identity rather than to its name.
    '@esbuild/linux-arm64': ('0.28.2', None, {'bin/esbuild': ('#!/bin/sh\necho 0.28.2\n', True)}),
    # Stubs whose only job is to let the runtime proof reach its transform and
    # native-command probes; the real @google/genai and protobufjs payloads are
    # integration legs (the focused harness), never this fixture.
    '@google/genai': ('1.0.0', None, {
        'dist/index.js': ('export class GoogleGenAI {}\nexport const version = "1.0.0";\n', False),
    }),
    'protobufjs': ('7.0.0', None, {'index.js': (
        'export function Root() {}\n'
        'Root.fromJSON = () => ({\n'
        '  lookupType: () => ({\n'
        '    encode: value => ({finish: () => Buffer.from(JSON.stringify(value), "utf8")}),\n'
        '    decode: bytes => JSON.parse(Buffer.from(bytes).toString("utf8")),\n'
        '  }),\n'
        '});\n', False)}),
}

# Packages that publish a `main` (PnP-less ESM resolution by file, no exports map).
MAINS = {
    '@google/genai': 'dist/index.js',
    'protobufjs': 'index.js',
    'esbuild': 'lib/main.js',
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
    # The historical shape plus the two probe payloads and an installed native
    # package, so the runtime proof can run to completion: the positive control
    # for the transform, the native command and the scoped chord instance. Its
    # family is unchanged, so it is still a reviewed shape.
    'legacy-probe': {
        'version': '0.87.0',
        'members': FAMILY,
        'edges': {
            ROOT: ['chord', 'pi-agent-core', 'pi-ai', 'pi-tui'],
            f'{SCOPE}/chord': ['esbuild', '@esbuild/linux-arm64'],
            f'{SCOPE}/pi-agent-core': ['pi-ai', 'pi-telemetry', 'yaml', 'diff',
                                       '@google/genai', 'protobufjs'],
            f'{SCOPE}/pi-ai': ['@anthropic-ai/sdk'],
            f'{SCOPE}/pi-tui': ['marked'],
            f'{SCOPE}/pi-telemetry': [],
        },
        'optional': set(),
        'contexts': {},
        # No platform selector: this shape installs the native package for the
        # modelled linux/arm64 target instead of excusing its absence.
        'platform': {},
    },
}


def package_json(name, version, edges):
    manifest = {'name': name, 'version': version, 'type': 'module', 'license': 'MIT'}
    if name in MAINS:
        manifest['main'] = MAINS[name]
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
            'platform': shape.get('platform', PLATFORM).get(name, {'os': [], 'cpu': [], 'libc': []}),
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
    # Each target here is inside the package read on its own: the escape and the
    # cycle exist only once another member's link is followed to the root.
    'link-composed': [('package/x', b'', 0o755, tarfile.SYMTYPE, '.'),
                      ('package/b', b'', 0o755, tarfile.SYMTYPE, 'x/..')],
    'link-cycle': [('package/a', b'', 0o755, tarfile.SYMTYPE, 'b'),
                   ('package/b', b'', 0o755, tarfile.SYMTYPE, 'a')],
    'link-self': [('package/self', b'', 0o755, tarfile.SYMTYPE, 'self')],
    'ancestor-file': [('package/a', b'file', 0o644, tarfile.REGTYPE, ''),
                      ('package/a/b', b'child', 0o644, tarfile.REGTYPE, '')],
    'ancestor-link': [('package/a', b'', 0o755, tarfile.SYMTYPE, 'b'),
                      ('package/a/c', b'child', 0o644, tarfile.REGTYPE, '')],
    # Case folding: APFS stores `d/X` and `d/x` as one entry, so the link `d/X` to
    # the package root is what `e/Y`'s target `../d/x/..` walks through -- one level
    # *above* the extraction root -- and `e/y/PWNED` is written outside it. Every
    # name and target here is contained when read byte for byte, and `e/Y` and
    # `e/y` are the collision the preflight can see.
    'case-collision': [
        ('package/d/X', b'', 0o755, tarfile.SYMTYPE, '..'),
        ('package/e/Y', b'', 0o755, tarfile.SYMTYPE, '../d/x/..'),
        ('package/e/y/PWNED', b'pwned\n', 0o644, tarfile.REGTYPE, ''),
    ],
    # The same shape in Unicode: `d/\u00e9` (NFC) is the escaping link, the target
    # spells it decomposed (`d/e` + U+0301), and the written-through link is
    # `e/\u00fd` whose decomposed spelling is member `e/y` + U+0301.
    'normalisation-collision': [
        ('package/d/\u00e9', b'', 0o755, tarfile.SYMTYPE, '..'),
        ('package/e/\u00fd', b'', 0o755, tarfile.SYMTYPE, '../d/e\u0301/..'),
        ('package/e/y\u0301/PWNED', b'pwned\n', 0o644, tarfile.REGTYPE, ''),
    ],
    # A target that names another member's link in the other normalisation form.
    # No member name collides here, so a byte-exact resolution reads `d/e` + U+0301
    # as a plain missing directory and calls the target contained, while the
    # filesystem follows `d/\u00e9` and its `..` out of the package.
    'link-folded-target': [
        ('package/d/\u00e9', b'', 0o755, tarfile.SYMTYPE, '..'),
        ('package/e/other', b'', 0o755, tarfile.SYMTYPE, '../d/e\u0301/..'),
    ],
    'privilege': [('package/x', b'x', 0o4755, tarfile.REGTYPE, '')],
    'unexpected-root': [('other/a', b'x', 0o644, tarfile.REGTYPE, '')],
    'deep': [('package/' + '/'.join(['d'] * 40) + '/f', b'x', 0o644, tarfile.REGTYPE, '')],
    'long-name': [('package/' + 'n' * 1100, b'x', 0o644, tarfile.REGTYPE, '')],
}


def add_members(archive, members):
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


def hostile(destination, case):
    with tarfile.open(destination, 'w:gz', format=tarfile.GNU_FORMAT) as archive:
        add_members(archive, HOSTILE[case])


def manifest(**fields):
    return json.dumps(fields, sort_keys=True).encode() + b'\n'


# Archives whose manifest declares a command the extractor must refuse to hash.
# Only `evidence` reads a bin target, so these cannot live in HOSTILE: a link the
# archive reaches its own command through is contained and still must not decide
# which bytes the checker hashes.
HOSTILE_EVIDENCE = {
    'bin-link-ancestor': [
        ('package/package.json', manifest(name='hostile', version='1.0.0',
                                          bin={'review': 'lib/tool'}), 0o644,
         tarfile.REGTYPE, ''),
        ('package/lib', b'', 0o755, tarfile.SYMTYPE, 'real'),
        ('package/real/tool', b'#!/bin/sh\n', 0o755, tarfile.REGTYPE, ''),
    ],
    'bin-link-final': [
        ('package/package.json', manifest(name='hostile', version='1.0.0',
                                          bin={'review': 'tool'}), 0o644,
         tarfile.REGTYPE, ''),
        ('package/real', b'#!/bin/sh\n', 0o755, tarfile.REGTYPE, ''),
        ('package/tool', b'', 0o755, tarfile.SYMTYPE, 'real'),
    ],
    'bin-missing': [
        ('package/package.json', manifest(name='hostile', version='1.0.0',
                                          bin={'review': 'tool'}), 0o644,
         tarfile.REGTYPE, ''),
    ],
}


def hostile_evidence(destination, case):
    with tarfile.open(destination, 'w:gz', format=tarfile.GNU_FORMAT) as archive:
        add_members(archive, HOSTILE_EVIDENCE[case])


# Archives about headers rather than members: the `package/` root entry and the
# extended headers are read by tarfile before any member exists, so they are built
# from raw header blocks instead of TarInfo, which cannot express a root file, a
# directory with declared data or a large PAX record.
def tar_header(name, typeflag=tarfile.REGTYPE, size=0, mode=0o644):
    field = name.encode()
    buf = bytearray(512)
    buf[0:len(field)] = field
    for start, length, value in ((100, 8, mode), (108, 8, 0), (116, 8, 0), (124, 12, size),
                                 (136, 12, 0)):
        buf[start:start + length] = f'{value:0{length - 1}o}'.encode() + b'\0'
    buf[148:156] = b' ' * 8
    buf[156:157] = typeflag
    buf[257:265] = b'ustar\x0000'
    buf[148:156] = f'{sum(buf):06o}'.encode() + b'\0 '
    return bytes(buf)


def tar_pad(payload):
    return payload + b'\0' * (-len(payload) % 512)


def pax_record(key, value):
    body = b' ' + key + b'=' + value + b'\n'
    length = len(body) + 1
    while len(str(length)) + len(body) != length:
        length = len(str(length)) + len(body)
    return str(length).encode() + body


MANIFEST_PAYLOAD = manifest(name='hostile', version='1.0.0')


def raw_archive(destination, blocks):
    # Streamed, not joined: `header-spread` is a gigabyte of repetitive header text
    # whose whole point is that it compresses to a megabyte, and materialising it in
    # this process would cost the fixture more memory than the case measures.
    with gzip.GzipFile(destination, 'wb', mtime=0) as handle:
        for block in blocks:
            handle.write(block)
        handle.write(b'\0' * 1024)


HOSTILE_BUDGET = {
    'root-duplicate': [
        tar_header('package', tarfile.DIRTYPE), tar_header('package', tarfile.DIRTYPE),
        tar_header('package/package.json', size=len(MANIFEST_PAYLOAD)),
        tar_pad(MANIFEST_PAYLOAD),
    ],
    'root-file': [tar_header('package', size=8 * 1024 * 1024), b'\0' * (8 * 1024 * 1024)],
    'root-size': [tar_header('package', tarfile.DIRTYPE, size=8 * 1024 * 1024),
                  b'\0' * (8 * 1024 * 1024)],
    # Exactly as many non-root members as the scaled budget allows, so only the
    # root header can tip the count.
    'root-count': [
        tar_header('package', tarfile.DIRTYPE),
        tar_header('package/package.json', size=len(MANIFEST_PAYLOAD)),
        tar_pad(MANIFEST_PAYLOAD),
        tar_header('package/a'),
    ],
}


def pax_blocks(payload):
    padded = tar_pad(payload)
    return [tar_header('PaxHeaders/hostile', tarfile.XHDTYPE, size=len(padded)), padded]


# PAX records are read between members, so neither the per-read cap nor a member's
# declared size ever sees them. `header-comment` is one record whose value fills
# 64 MiB -- the measured 600 MiB variant extracted with rc=0 and a 3.4 GB peak RSS --
# and `header-chain` is a long run of tiny `x` headers, each one a recursion into
# `tarfile.next()` that ended in a bare RecursionError traceback.
HEADER_COMMENT_BYTES = 64 * 1024 * 1024
HEADER_CHAIN_LENGTH = 5000
# Just under the between-members window, spent on 1,000 members: the per-window
# budget never fires, and 1.1 MB on the wire arrived as a 1.16 GB resident archive.
HEADER_SPREAD_MEMBERS = 1000
HEADER_SPREAD_BYTES = 1_040_000


def header_spread_blocks():
    for index in range(HEADER_SPREAD_MEMBERS):
        yield from pax_blocks(pax_record(b'comment', b'x' * HEADER_SPREAD_BYTES))
        yield tar_header(f'package/f{index:04d}')


def header_case_blocks(case):
    if case == 'header-spread':
        return header_spread_blocks()
    if case == 'header-comment':
        blocks = pax_blocks(pax_record(b'SCHILY.xattr.user.comment',
                                       b'x' * HEADER_COMMENT_BYTES))
        return blocks + [tar_header('package/package.json', size=len(MANIFEST_PAYLOAD)),
                         tar_pad(MANIFEST_PAYLOAD)]
    if case == 'header-chain':
        blocks = []
        for _ in range(HEADER_CHAIN_LENGTH):
            blocks += pax_blocks(pax_record(b'comment', b'chained'))
        return blocks + [tar_header('package/package.json', size=len(MANIFEST_PAYLOAD)),
                         tar_pad(MANIFEST_PAYLOAD)]
    return None


def hostile_budget(destination, case):
    if case == 'pax-bomb':
        # One valid PAX record whose value fills the declared 32 MiB buffer: the
        # declared size reaches the reader before the member it annotates exists.
        blocks = pax_blocks(pax_record(b'SCHILY.xattr.user.comment', b'x' * (32 * 1024 * 1024)))
        blocks += [tar_header('package/package.json', size=len(MANIFEST_PAYLOAD)),
                   tar_pad(MANIFEST_PAYLOAD)]
    else:
        blocks = header_case_blocks(case) or HOSTILE_BUDGET[case]
    raw_archive(destination, blocks)


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
    if len(argv) == 3 and argv[0] == 'hostile-evidence':
        hostile_evidence(Path(argv[2]), argv[1])
        return
    if len(argv) == 3 and argv[0] == 'hostile-budget':
        hostile_budget(Path(argv[2]), argv[1])
        return
    if len(argv) == 2 and argv[0] == 'benign':
        benign(Path(argv[1]))
        return
    if len(argv) == 3 and argv[0] == 'build':
        build(argv[1], argv[2])
        return
    sys.stderr.write('usage: pi-installed-bytes.py {build WORK legacy|modern |'
                     ' hostile CASE TARBALL | hostile-evidence CASE TARBALL |'
                     ' hostile-budget CASE TARBALL | benign TARBALL}\n')
    raise SystemExit(2)


if __name__ == '__main__':
    main(sys.argv[1:])
