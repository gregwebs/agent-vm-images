#!/usr/bin/env python3
"""Independent oracle for the dsh filesystem layout an image actually ships.

Why a second implementation instead of reusing the image's own checks: the
production checker runs *inside* the build (Node, real install, real modules)
and the graph validator parses the lock the build just consumed. Neither can
show that the finished image carries the reviewed payload: a Dockerfile could
copy a different tree, a store could be linked from outside, or the installed
manifests could disagree with the lock. This oracle therefore reads only a
virtual copy of the extracted image and answers one question: is the shipped
file/link/manifest graph a self-contained, peer-correct dsh install?

Invariants that come from that purpose:

* ``--root`` is a **virtual image filesystem root** (an extraction containing
  ``opt/agent-vm/dsh/...`` and ``usr/local/bin/dsh``), never the host ``/opt``.
  Every absolute symlink target restarts at the virtual root, so an image can be
  audited on a machine whose ``/opt`` looks nothing like the image; reading the
  host would turn a foreign file into false evidence.
* pnpm virtual-store directory names are opaque (peer-context keys, truncated
  hashes, underscores). Identities come from each ``package.json``; nothing is
  parsed out of a directory name.
* Symlinks are preserved and interpreted here. The walk never follows a
  directory symlink, so a cycle cannot make the walk diverge and a link is
  always reported as the link it is rather than silently dereferenced.
* ``--platform`` is mandatory and explicit. A Linux arm64 image is not evidence
  for amd64, and the oracle must not infer the architecture from the host or
  from whichever native optional package happens to be present.
* Effective dependencies and peers are resolved with a Node-style ancestor
  ``node_modules`` walk from each real owner directory, so a package that finds
  the right *global* version through the wrong peer context still fails.
"""

from __future__ import annotations

import json
import os
import re
import stat
import sys
from dataclasses import dataclass
from pathlib import Path

USAGE = ('usage: standard-node-layout.py --consumer dsh --root ROOT '
         '--platform linux/amd64|linux/arm64 [--expect-version EXACT]\n'
         '       standard-node-layout.py --self-test')

# Production paths are fixed: this oracle certifies the image, not a developer
# staging directory, so there is no prefix/command override to get wrong.
PRODUCTION_PREFIX = ('opt', 'agent-vm', 'dsh')
PRODUCTION_COMMAND = ('usr', 'local', 'bin', 'dsh')

ROOT_PACKAGE = '@deepseek-ai/dsh'
BASE = '@deepseek-ai/dsh-base'
SDK = '@deepseek-ai/dsh-sdk-minimal'
SANDBOX = '@deepseek-ai/dsh-sandbox-local'
SUBPROCESS_LOCAL = '@deepseek-ai/dsh-subprocess-local'
LLM = '@deepseek-ai/dsh-llm-pi-ai'
PI_AI = '@earendil-works/pi-ai'
GENAI = '@google/genai'
PROTOBUF = 'protobufjs'
EXPECTED_BIN = {'dsh': 'lib/bin.js'}

# Bounds: an extracted payload is ~600 store entries, so these only stop
# pathological trees from hanging the audit.
MAX_HOPS = 40
MAX_STEPS = 8192
MAX_ENTRIES = 200000
MAX_DEPTH = 32
MAX_MANIFEST_BYTES = 1 << 20
MAX_ENTRY_BYTES = 1 << 20

EXACT_VERSION = re.compile(r'^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?$')
MANAGER_NAMES = ('pnpm', 'pnx', 'pnpx')

SOURCE_MANIFEST = Path(__file__).resolve().parents[2] / 'images/tools/dsh/package.json'


class Mismatch(Exception):
    """The extracted image does not carry the reviewed layout (exit 1)."""


class Missing(Mismatch):
    """A virtual path does not exist; the resolver treats this as 'not found'."""


class Usage(Exception):
    """Invocation problem (exit 2)."""


@dataclass(frozen=True)
class Platform:
    key: str
    os: str
    cpu: str
    libc: str


PLATFORMS = {
    'linux/amd64': Platform('linux/amd64', 'linux', 'x64', 'glibc'),
    'linux/arm64': Platform('linux/arm64', 'linux', 'arm64', 'glibc'),
}

# A "process" in production means node-pty's native binding and koffi's FFI
# library really load. Both are published per platform: koffi as an optional
# platform package, node-pty as a prebuild inside its own package. An optional
# payload that is wholly absent breaks the runtime exactly like an unlinked one,
# so the platform-matching payload is required, never merely optional.
# The payload location is exact, not an architecture substring: these packages
# also ship foreign prebuilds (darwin-*, win32-*, musl_*) and unrelated `.node`
# files can appear anywhere in a package, so a path merely containing the
# architecture is not evidence that this platform's binding exists.
NATIVE_PAYLOAD = {
    'linux/arm64': {'koffi': '@koromix/koffi-linux-arm64',
                    'koffiPayload': ('linux_arm64', 'koffi.node'),
                    'ptyPayload': ('prebuilds', 'linux-arm64', 'pty.node')},
    'linux/amd64': {'koffi': '@koromix/koffi-linux-x64',
                    'koffiPayload': ('linux_x64', 'koffi.node'),
                    'ptyPayload': ('prebuilds', 'linux-x64', 'pty.node')},
}


def virtual(path):
    """Render a virtual path tuple the way an image path is written."""
    return '/' + '/'.join(path)


class VirtualRoot:
    """Interprets links inside one virtual image root.

    Every lookup is ``root + components`` after lexical ``.``/``..`` handling and
    link expansion, so a resolved path can never name a host file outside the
    extraction and the caller always sees the same canonical tuple.
    """

    def __init__(self, root: Path):
        self.root = root

    def physical(self, path):
        return self.root.joinpath(*path) if path else self.root

    def lstat(self, path):
        return os.lstat(self.physical(path))

    def readlink(self, path):
        return os.readlink(self.physical(path))

    def follow(self, base, parts):
        current = list(base)
        pending = list(parts)
        hops = 0
        steps = 0
        while pending:
            steps += 1
            if steps > MAX_STEPS:
                raise Mismatch(f'{virtual(tuple(base) + tuple(parts))}: link expansion is unbounded')
            name = pending.pop(0)
            if name in ('', '.'):
                continue
            if name == '..':
                if not current:
                    raise Mismatch(f'{virtual(tuple(base) + tuple(parts))}: link target escapes the '
                                   'image root')
                current.pop()
                continue
            candidate = tuple(current) + (name,)
            try:
                info = self.lstat(candidate)
            except (FileNotFoundError, NotADirectoryError):
                raise Missing(f'{virtual(candidate)}: missing from the extracted image') from None
            if stat.S_ISLNK(info.st_mode):
                hops += 1
                if hops > MAX_HOPS:
                    raise Mismatch(f'{virtual(candidate)}: symlink cycle')
                target = self.readlink(candidate)
                if not target:
                    raise Mismatch(f'{virtual(candidate)}: empty link target')
                current = [] if target.startswith('/') else current
                pending = [part for part in target.split('/') if part] + pending
                continue
            if pending and not stat.S_ISDIR(info.st_mode):
                raise Mismatch(f'{virtual(candidate)}: not a directory')
            current.append(name)
        return tuple(current)


class Tree:
    """Physical inventory of one subtree; symlinked directories are not entered."""

    def __init__(self, filesystem: VirtualRoot):
        self.filesystem = filesystem
        self.files = {}
        self.links = {}
        self.dirs = set()
        self.special = []

    def scan(self, start):
        pending = [tuple(start)]
        count = 0
        while pending:
            directory = pending.pop()
            try:
                entries = sorted(os.scandir(self.filesystem.physical(directory)), key=lambda item: item.name)
            except OSError as error:
                raise Mismatch(f'{virtual(directory)}: cannot be inspected ({error.strerror})') from None
            self.dirs.add(directory)
            for entry in entries:
                count += 1
                if count > MAX_ENTRIES:
                    raise Mismatch(f'{virtual(directory)}: too many entries')
                path = directory + (entry.name,)
                if len(path) - len(start) > MAX_DEPTH:
                    raise Mismatch(f'{virtual(path)}: path depth exceeds {MAX_DEPTH}')
                if entry.is_symlink():
                    self.links[path] = os.readlink(entry.path)
                elif entry.is_dir(follow_symlinks=False):
                    pending.append(path)
                elif entry.is_file(follow_symlinks=False):
                    info = entry.stat(follow_symlinks=False)
                    self.files[path] = (info.st_mode, info.st_size)
                else:
                    # A fifo/device/socket inside an installed payload is not
                    # something a package manager may ship; refusing it keeps the
                    # rest of the walk from blocking on a special file.
                    self.special.append(path)
        return self


@dataclass(frozen=True)
class Manifest:
    name: str
    version: str
    dependencies: dict
    optional: dict
    peers: dict
    peer_meta: dict
    os_field: list | None
    cpu: list | None
    libc: list | None
    bin: dict | None

    def declares(self, name):
        return name in self.dependencies or name in self.optional or name in self.peers

    def peer_is_optional(self, name):
        entry = self.peer_meta.get(name)
        return isinstance(entry, dict) and entry.get('optional') is True


def string_map(value, field):
    if value is None:
        return {}
    if not isinstance(value, dict) or not all(isinstance(key, str) and isinstance(item, str)
                                              for key, item in value.items()):
        raise Mismatch(f'manifest field {field} must map names to strings')
    return dict(value)


def string_list(value, field):
    if value is None:
        return None
    if not isinstance(value, list) or not all(isinstance(item, str) and item for item in value):
        raise Mismatch(f'manifest field {field} must be a nonempty list of strings')
    return list(value)


def read_manifest(tree: Tree, path, require_version=True) -> Manifest:
    if path in tree.links:
        raise Mismatch(f'{virtual(path)}: manifest must be a regular file, not a link')
    if path not in tree.files:
        raise Mismatch(f'{virtual(path)}: missing regular file')
    mode, size = tree.files[path]
    if not stat.S_ISREG(mode):
        raise Mismatch(f'{virtual(path)}: not a regular file')
    if size > MAX_MANIFEST_BYTES:
        raise Mismatch(f'{virtual(path)}: manifest is larger than {MAX_MANIFEST_BYTES} bytes')
    try:
        raw = json.loads(tree.filesystem.physical(path).read_bytes())
    except (OSError, ValueError) as error:
        raise Mismatch(f'{virtual(path)}: unreadable manifest ({error})') from None
    if not isinstance(raw, dict):
        raise Mismatch(f'{virtual(path)}: manifest must be a JSON object')
    name = raw.get('name')
    version = raw.get('version')
    if not isinstance(name, str) or not name:
        raise Mismatch(f'{virtual(path)}: manifest has no package name')
    if not isinstance(version, str) or not version:
        if require_version:
            raise Mismatch(f'{virtual(path)}: manifest has no version')
        version = ''
    peer_meta = raw.get('peerDependenciesMeta') or {}
    if not isinstance(peer_meta, dict):
        raise Mismatch(f'{virtual(path)}: peerDependenciesMeta must be an object')
    return Manifest(
        name=name,
        version=version,
        dependencies=string_map(raw.get('dependencies'), 'dependencies'),
        optional=string_map(raw.get('optionalDependencies'), 'optionalDependencies'),
        peers=string_map(raw.get('peerDependencies'), 'peerDependencies'),
        peer_meta=peer_meta,
        os_field=string_list(raw.get('os'), 'os'),
        cpu=string_list(raw.get('cpu'), 'cpu'),
        libc=string_list(raw.get('libc'), 'libc'),
        bin=raw.get('bin') if isinstance(raw.get('bin'), dict) else None,
    )


class Analysis:
    """One audit of an extracted image root."""

    def __init__(self, root: Path, platform: Platform, expect_version: str):
        self.filesystem = VirtualRoot(root)
        self.tree = Tree(self.filesystem).scan(PRODUCTION_PREFIX)
        self.prefix = PRODUCTION_PREFIX
        self.platform = platform
        self.expect_version = expect_version
        self.instances = {}
        self.excluded = set()

    # -- platform eligibility -------------------------------------------------

    def _field(self, values, actual):
        if values is None:
            return True
        positive = [value for value in values if not value.startswith('!')]
        negative = [value[1:] for value in values if value.startswith('!')]
        if actual in negative:
            return False
        return not positive or 'any' in positive or actual in positive

    def compatible(self, manifest: Manifest) -> bool:
        return (self._field(manifest.os_field, self.platform.os)
                and self._field(manifest.cpu, self.platform.cpu)
                and self._field(manifest.libc, self.platform.libc))

    # -- resolution -----------------------------------------------------------

    def resolve(self, owner, name):
        """Node-style ancestor walk; returns the canonical instance directory."""
        components = tuple(name.split('/'))
        if not components or len(components) > 2 or any(part in ('', '.', '..') for part in components):
            raise Mismatch(f'{name!r}: not a package name')
        directory = tuple(owner)
        while True:
            if directory and directory[-1] != 'node_modules':
                candidate = directory + ('node_modules',) + components
                try:
                    resolved = self.filesystem.follow((), candidate)
                except Missing:
                    resolved = None
                if (resolved is not None and resolved in self.tree.dirs
                        and resolved + ('package.json',) in self.tree.files):
                    return resolved
            if not directory:
                return None
            directory = directory[:-1]

    def require(self, owner, name):
        if owner not in self.instances:
            raise Mismatch(f'{virtual(owner)}: not an installed package')
        if not self.instances[owner].declares(name):
            raise Mismatch(f'{virtual(owner)}: {name} is not a declared dependency or peer')
        found = self.resolve(owner, name)
        if found is None:
            raise Mismatch(f'{virtual(owner)}: declared dependency {name} is not installed')
        return found

    # -- checks ---------------------------------------------------------------

    def check_links(self):
        for path in sorted(self.tree.links):
            resolved = self.filesystem.follow((), path)
            if resolved[:len(self.prefix)] != self.prefix:
                raise Mismatch(f'{virtual(path)}: link target {virtual(resolved)} leaves the dsh prefix')
        for path in self.tree.special:
            raise Mismatch(f'{virtual(path)}: unexpected special file')

    def collect_instances(self):
        for directory in sorted(self.tree.dirs):
            # A package instance sits directly in a node_modules directory
            # (scoped names add one level). Markers like
            # `pkg/helpers/esm/package.json` are manifest-shaped files inside a
            # package, not installed packages.
            if not (directory[-2:-1] == ('node_modules',)
                    or (len(directory) > 2 and directory[-3] == 'node_modules'
                        and directory[-2].startswith('@'))):
                continue
            manifest_path = directory + ('package.json',)
            if manifest_path in self.tree.links:
                raise Mismatch(f'{virtual(manifest_path)}: package manifest must not be a link')
            if manifest_path in self.tree.files:
                manifest = read_manifest(self.tree, manifest_path)
                self.instances[directory] = manifest
                if not self.compatible(manifest):
                    self.excluded.add(directory)

    def check_project(self):
        self.project = read_manifest(self.tree, self.prefix + ('package.json',),
                                    require_version=False)
        if set(self.project.dependencies) != {ROOT_PACKAGE}:
            raise Mismatch('installed project must declare only the dsh dependency')
        selected = self.project.dependencies[ROOT_PACKAGE]
        if not EXACT_VERSION.match(selected):
            raise Mismatch('installed project must pin dsh to an exact version')
        raw = json.loads(self.filesystem.physical(self.prefix + ('package.json',)).read_bytes())
        if raw.get('devDependencies') or raw.get('workspaces'):
            raise Mismatch('installed project must not carry dev dependencies or workspace roots')
        for path, what, kind in [(self.prefix + ('node_modules',), 'node_modules', 'dir'),
                                 (self.prefix + ('node_modules', '.pnpm'), 'virtual store', 'dir'),
                                 (self.prefix + ('node_modules', '.modules.yaml'), 'install metadata', 'file')]:
            present = path in self.tree.files if kind == 'file' else path in self.tree.dirs
            if path in self.tree.links:
                raise Mismatch(f'{virtual(path)}: {what} must not be a link')
            if not present:
                raise Mismatch(f'{virtual(path)}: {what} is missing from the installed payload')

    def check_dsh(self):
        found = self.resolve(self.prefix, ROOT_PACKAGE)
        if found is None:
            raise Mismatch(f'{ROOT_PACKAGE}: root dependency is not installed')
        manifest = self.instances[found]
        if manifest.version != self.expect_version:
            raise Mismatch(f'installed {ROOT_PACKAGE}@{manifest.version} is not the expected '
                           f'{self.expect_version}')
        if manifest.bin != EXPECTED_BIN:
            raise Mismatch(f'{ROOT_PACKAGE}: installed bin mapping {manifest.bin!r} is not '
                           f'the reviewed {EXPECTED_BIN!r}')
        entry = found + tuple(EXPECTED_BIN['dsh'].split('/'))
        if entry in self.tree.links:
            raise Mismatch(f'{virtual(entry)}: installed dsh entry must be a regular file')
        if entry not in self.tree.files:
            raise Mismatch(f'{virtual(entry)}: installed dsh entry is missing')
        mode, size = self.tree.files[entry]
        if size == 0 or size > MAX_ENTRY_BYTES:
            raise Mismatch(f'{virtual(entry)}: installed dsh entry has an implausible size')
        if not mode & stat.S_IXUSR:
            raise Mismatch(f'{virtual(entry)}: installed dsh entry is not executable')
        first = self.filesystem.physical(entry).read_bytes().split(b'\n', 1)[0]
        if not first.startswith(b'#!') or b'node' not in first[2:]:
            raise Mismatch(f'{virtual(entry)}: installed dsh entry has no Node shebang')
        return found, entry

    def check_command(self, entry):
        try:
            info = self.filesystem.lstat(PRODUCTION_COMMAND)
        except FileNotFoundError:
            raise Mismatch(f'{virtual(PRODUCTION_COMMAND)}: missing from the image') from None
        if not stat.S_ISLNK(info.st_mode):
            raise Mismatch(f'{virtual(PRODUCTION_COMMAND)}: must be a direct link to the installed entry')
        literal = self.filesystem.readlink(PRODUCTION_COMMAND)
        if '.bin' in literal.split('/'):
            raise Mismatch(f'{virtual(PRODUCTION_COMMAND)}: must not route through a generated .bin wrapper')
        resolved = self.filesystem.follow((), PRODUCTION_COMMAND)
        if resolved != entry:
            raise Mismatch(f'{virtual(PRODUCTION_COMMAND)}: links to {virtual(resolved)}, '
                           f'not the installed dsh entry {virtual(entry)}')

    def check_edges(self):
        exact_peers = 0
        optional_missing = 0
        for owner in sorted(self.instances):
            if owner in self.excluded:
                continue
            manifest = self.instances[owner]
            for name, spec in sorted(manifest.dependencies.items()):
                self.check_edge(owner, name, spec, optional=False)
            for name, spec in sorted(manifest.optional.items()):
                found = self.resolve(owner, name)
                if found is None:
                    optional_missing += 1
                    self.check_optional_absence(owner, name)
                else:
                    self.check_edge(owner, name, spec, optional=True)
            for name, spec in sorted(manifest.peers.items()):
                optional = manifest.peer_is_optional(name)
                found = self.resolve(owner, name)
                if found is None:
                    if optional:
                        optional_missing += 1
                        continue
                    raise Mismatch(f'{virtual(owner)}: mandatory peer {name} is not installed')
                self.check_edge(owner, name, spec, optional=optional)
                if EXACT_VERSION.match(spec):
                    exact_peers += 1
        return exact_peers, optional_missing

    def check_edge(self, owner, name, spec, optional):
        found = self.resolve(owner, name)
        if found is None:
            if optional:
                return
            raise Mismatch(f'{virtual(owner)}: declared dependency {name} is not installed')
        if found in self.excluded:
            # A package manager may keep a foreign-architecture payload on disk,
            # but an owner for *this* platform must not resolve it.
            raise Mismatch(f'{virtual(owner)}: {name} resolves to {virtual(found)}, which is not '
                           f'eligible for {self.platform.key}')
        installed = self.instances[found]
        if installed.name != name:
            raise Mismatch(f'{virtual(found)}: manifest name {installed.name} is not {name}')
        if EXACT_VERSION.match(spec) and installed.version != spec:
            raise Mismatch(f'{virtual(owner)}: {name} resolves to {installed.version}, not the '
                           f'declared {spec}')

    def check_optional_absence(self, owner, name):
        present = sorted(path for path, manifest in self.instances.items()
                         if manifest.name == name and path not in self.excluded)
        if present:
            raise Mismatch(f'{virtual(owner)}: optional dependency {name} is installed at '
                           f'{virtual(present[0])} but not linked from its owner')

    def check_chains(self, dsh):
        base = self.require(dsh, BASE)
        sdk = self.require(dsh, SDK)
        sandbox_routes = [(self.instances[base].name, self.require(base, SANDBOX)),
                          (self.instances[sdk].name, self.require(sdk, SANDBOX))]
        hoist = self.filesystem.follow(
            (), self.prefix + ('node_modules', '.pnpm', 'node_modules') + tuple(SANDBOX.split('/')))
        sandbox_routes.append(('private hoist', hoist))
        if len({route for _, route in sandbox_routes}) != 1:
            detail = ', '.join(f'{owner} -> {virtual(path)}' for owner, path in sandbox_routes)
            raise Mismatch(f'{SANDBOX}: resolves to different instances ({detail})')
        subprocess_owners = [self.require(sdk, SUBPROCESS_LOCAL)]
        if self.instances[base].declares(SUBPROCESS_LOCAL):
            subprocess_owners.append(self.require(base, SUBPROCESS_LOCAL))
        if len(set(subprocess_owners)) != 1:
            raise Mismatch(f'{SUBPROCESS_LOCAL}: resolves to different instances')
        local = subprocess_owners[0]
        native = [self.require(local, 'node-pty'), self.require(local, 'koffi')]
        self.check_native(local)
        genai = self.require(self.require(self.require(base, LLM), PI_AI), GENAI)
        self.require(genai, PROTOBUF)
        return {
            'sandbox': sorted(f'{owner} -> {virtual(path)}' for owner, path in sandbox_routes),
            'subprocessOwners': sorted({virtual(path) for path in subprocess_owners + native}),
        }

    def check_native(self, local):
        payload = NATIVE_PAYLOAD[self.platform.key]
        koffi = self.require(local, 'koffi')
        platform = self.resolve(koffi, payload['koffi'])
        if platform is None:
            raise Mismatch(f'{virtual(koffi)}: koffi requires the {self.platform.key} platform '
                           f'package {payload["koffi"]}, which is absent')
        if platform in self.excluded:
            raise Mismatch(f'{virtual(koffi)}: {payload["koffi"]} is not eligible for {self.platform.key}')
        declared = self.instances[koffi].optional.get(payload['koffi'])
        installed = self.instances[platform]
        if installed.name != payload['koffi']:
            raise Mismatch(f'{virtual(platform)}: package name {installed.name} is not {payload["koffi"]}')
        if declared is not None and EXACT_VERSION.match(declared) and installed.version != declared:
            raise Mismatch(f'{virtual(koffi)}: {payload["koffi"]} resolves to {installed.version}, '
                           f'not the declared {declared}')
        # koffi dlopens exactly one build directory of its platform package.
        build = platform + payload['koffiPayload']
        if build not in self.tree.files:
            raise Mismatch(f'{virtual(platform)}: no {self.platform.key} koffi build payload at '
                           f'{virtual(build)}')
        pty = self.require(local, 'node-pty')
        # node-pty loads prebuilds/<os>-<arch>/pty.node; a package without this
        # exact file has nothing to require at runtime.
        prebuild = pty + payload['ptyPayload']
        if prebuild not in self.tree.files:
            raise Mismatch(f'{virtual(pty)}: no {self.platform.key} prebuild payload at '
                           f'{virtual(prebuild)}')

    def check_manager(self):
        found = []
        for path, manifest in sorted(self.instances.items()):
            if manifest.name in MANAGER_NAMES:
                found.append(f'{manifest.name} package at {virtual(path)}')
        names = {manager + suffix for manager in MANAGER_NAMES for suffix in ('', '.mjs', '.cjs')}
        for path in sorted(self.tree.links) + sorted(self.tree.files):
            name = path[-1]
            if '.bin' in path and name in MANAGER_NAMES:
                found.append(f'generated bin at {virtual(path)}')
            if name in names:
                found.append(f'manager executable at {virtual(path)}')
        if found:
            raise Mismatch('second package manager in the dsh tree: ' + '; '.join(sorted(set(found))))

    def check_reachable(self):
        pending = []
        for name in sorted(self.project.dependencies):
            found = self.resolve(self.prefix, name)
            if found is not None:
                pending.append(found)
        # The private hoist exists precisely for phantom dependencies that no
        # manifest declares; those entries are intentional payload.
        hoist = self.prefix + ('node_modules', '.pnpm', 'node_modules')
        for path in sorted(self.tree.links):
            if path[:len(hoist)] == hoist and len(path) > len(hoist):
                pending.append(self.filesystem.follow((), path))
        visited = set()
        while pending:
            current = pending.pop()
            if current in visited or current not in self.instances:
                continue
            visited.add(current)
            manifest = self.instances[current]
            for name in sorted(set(manifest.dependencies) | set(manifest.optional) | set(manifest.peers)):
                found = self.resolve(current, name)
                if found is not None:
                    pending.append(found)
        orphans = sorted(virtual(path) for path in self.instances
                         if path not in visited and path not in self.excluded)
        if orphans:
            raise Mismatch('installed packages are not reachable from the project or the private '
                           'hoist: ' + ', '.join(orphans))

    # -- report ---------------------------------------------------------------

    def run(self):
        self.check_links()
        self.collect_instances()
        self.check_project()
        self.check_manager()
        exact_peers, optional_missing = self.check_edges()
        dsh, entry = self.check_dsh()
        chains = self.check_chains(dsh)
        self.check_command(entry)
        self.check_reachable()
        archives = sorted({f'{manifest.name}@{manifest.version}'
                           for path, manifest in self.instances.items() if path not in self.excluded})
        return {
            'consumer': 'dsh',
            'version': self.instances[dsh].version,
            'platform': self.platform.key,
            'archives': archives,
            'snapshots': len(self.instances) - len(self.excluded),
            'exactPeers': exact_peers,
            'optionalMissing': optional_missing,
            'sandbox': chains['sandbox'],
            'subprocessOwners': chains['subprocessOwners'],
            'bin': {'entry': EXPECTED_BIN['dsh'], 'target': virtual(entry),
                    'command': virtual(PRODUCTION_COMMAND)},
            'noPrivatePnpm': True,
        }


def analyze(root, platform, expect_version):
    return Analysis(Path(root), platform, expect_version).run()


# --------------------------------------------------------------------------
# Synthetic virtual roots for the self-test. They are written into a temp
# directory and audited through the same public entry point as a real
# extraction, so the self-test cannot pass by exercising a private shortcut.
# --------------------------------------------------------------------------

class Builder:
    def __init__(self, root):
        self.root = Path(root)

    def path(self, parts):
        return self.root.joinpath(*parts)

    def mkdir(self, parts):
        self.path(parts).mkdir(parents=True, exist_ok=True)

    def write(self, parts, text, mode=0o644):
        target = self.path(parts)
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)
        target.chmod(mode)

    def read(self, parts):
        return json.loads(self.path(parts).read_text())

    def link(self, parts, target):
        target_path = self.path(parts)
        target_path.parent.mkdir(parents=True, exist_ok=True)
        if target_path.is_symlink():
            target_path.unlink()
        os.symlink(target, target_path)

    def remove(self, parts):
        target = self.path(parts)
        if target.is_symlink() or target.is_file():
            target.unlink()
        elif target.is_dir():
            for child in sorted(target.rglob('*'), reverse=True):
                if child.is_symlink() or child.is_file():
                    child.unlink()
                else:
                    child.rmdir()
            target.rmdir()

    def entry(self, as_entry, name, version, **fields):
        """One physical store instance; the directory name stays opaque."""
        manifest = {'name': name, 'version': version}
        for key, value in fields.items():
            manifest['os' if key == 'os' else key] = value
        directory = store_dir(as_entry) + tuple(name.split('/'))
        self.mkdir(directory)
        self.write(directory + ('package.json',), json.dumps(manifest, sort_keys=True))
        self.write(directory + ('index.js',), 'module.exports = {};\n')
        return directory

    def rewrite_manifest(self, path, change):
        manifest = json.loads(self.path(path + ('package.json',)).read_text())
        change(manifest)
        self.write(path + ('package.json',), json.dumps(manifest, sort_keys=True))


def store_dir(entry):
    return PRODUCTION_PREFIX + ('node_modules', '.pnpm', entry, 'node_modules')


def relative_link(link_path, target_path):
    """A literal relative symlink target, exactly what pnpm writes."""
    shared = 0
    while shared < len(link_path) - 1 and shared < len(target_path) and link_path[shared] == target_path[shared]:
        shared += 1
    return '../' * (len(link_path) - 1 - shared) + '/'.join(target_path[shared:])


def link_in(builder, entry, name, target_entry, target_name, absolute=False):
    link_path = store_dir(entry) + tuple(name.split('/'))
    target = store_dir(target_entry) + tuple(target_name.split('/'))
    builder.link(link_path, virtual(target) if absolute else relative_link(link_path, target))


def link_root(builder, name, entry, absolute=False):
    link_path = PRODUCTION_PREFIX + ('node_modules',) + tuple(name.split('/'))
    target = store_dir(entry) + tuple(name.split('/'))
    builder.link(link_path, virtual(target) if absolute else relative_link(link_path, target))


def link_hoist(builder, name, entry, absolute=False):
    link_path = PRODUCTION_PREFIX + ('node_modules', '.pnpm', 'node_modules') + tuple(name.split('/'))
    target = store_dir(entry) + tuple(name.split('/'))
    builder.link(link_path, virtual(target) if absolute else relative_link(link_path, target))


ENTRIES = {
    'dsh': 'a', 'base': 'b', 'sdk': 'c', 'llm': 'd', 'pi-ai': 'e', 'genai': 'f',
    'protobuf': 'g', 'sandbox': 'h', 'subprocess': 'i', 'node-pty': 'j', 'koffi': 'k',
    'koffi-arm': 'l', 'koffi-x64': 'm', 'cordis': 'n', 'plugin': 'o', 'plugin-two': 'p',
    'cordis-old': 'q', 'provider': 'r', 'hoisted': 's', 'second-sandbox': 't',
}

PACKAGE_ENTRY = {
    ROOT_PACKAGE: ENTRIES['dsh'], BASE: ENTRIES['base'], SDK: ENTRIES['sdk'],
    LLM: ENTRIES['llm'], PI_AI: ENTRIES['pi-ai'], GENAI: ENTRIES['genai'],
    PROTOBUF: ENTRIES['protobuf'], SANDBOX: ENTRIES['sandbox'],
    SUBPROCESS_LOCAL: ENTRIES['subprocess'], 'node-pty': ENTRIES['node-pty'],
    'koffi': ENTRIES['koffi'], 'cordis': ENTRIES['cordis'],
    '@deepseek-ai/cordis-plugin-x': ENTRIES['plugin'],
    '@koromix/koffi-linux-arm64': ENTRIES['koffi-arm'],
    '@koromix/koffi-linux-x64': ENTRIES['koffi-x64'],
}


def build_healthy(builder, absolute=False, dsh_version='1.0.0', link_platform_optional=True,
                  arch='arm64'):
    """A complete dsh payload for one architecture with every required owner chain.

    pnpm installs the platform-matching koffi optional and skips the others; the
    fixture keeps the foreign package installed to exercise the eligibility rule.
    """
    builder.write(PRODUCTION_PREFIX + ('package.json',), json.dumps({
        'name': 'agent-vm-dsh-layer', 'private': True, 'packageManager': 'pnpm@11.11.0',
        'dependencies': {ROOT_PACKAGE: dsh_version}}, sort_keys=True))
    builder.mkdir(PRODUCTION_PREFIX + ('node_modules', '.pnpm', 'node_modules'))
    builder.write(PRODUCTION_PREFIX + ('node_modules', '.modules.yaml'), 'hoistPattern:\n  - "*"\n')

    builder.entry(ENTRIES['dsh'], ROOT_PACKAGE, dsh_version, bin=EXPECTED_BIN, dependencies={
        BASE: '1.0.0', SDK: '1.0.0', 'cordis': '4.0.2', '@deepseek-ai/cordis-plugin-x': '1.0.0'})
    dsh_dir = store_dir(ENTRIES['dsh']) + tuple(ROOT_PACKAGE.split('/'))
    builder.write(dsh_dir + ('lib', 'bin.js'), '#!/usr/bin/env node\nprocess.exit(0);\n', mode=0o755)
    builder.entry(ENTRIES['base'], BASE, '1.0.0', dependencies={LLM: '1.0.0', SANDBOX: '1.0.0'})
    builder.entry(ENTRIES['sdk'], SDK, '1.0.0', dependencies={SUBPROCESS_LOCAL: '1.0.0', SANDBOX: '1.0.0'})
    builder.entry(ENTRIES['llm'], LLM, '1.0.0', dependencies={PI_AI: '1.0.0'})
    builder.entry(ENTRIES['pi-ai'], PI_AI, '1.0.0', dependencies={GENAI: '1.0.0'})
    builder.entry(ENTRIES['genai'], GENAI, '1.0.0', dependencies={PROTOBUF: '1.0.0'})
    builder.entry(ENTRIES['protobuf'], PROTOBUF, '1.0.0')
    builder.entry(ENTRIES['sandbox'], SANDBOX, '1.0.0')
    builder.entry(ENTRIES['subprocess'], SUBPROCESS_LOCAL, '1.0.0',
                  dependencies={'node-pty': '1.0.0', 'koffi': '1.0.0'})
    builder.entry(ENTRIES['node-pty'], 'node-pty', '1.0.0')
    # node-pty publishes prebuildify layouts: prebuilds/<os>-<arch>/pty.node.
    builder.write(store_dir(ENTRIES['node-pty']) + ('node-pty', 'prebuilds', f'linux-{arch}', 'pty.node'), 'ELF')
    builder.entry(ENTRIES['koffi'], 'koffi', '1.0.0', optionalDependencies={
        '@koromix/koffi-linux-arm64': '1.0.0', '@koromix/koffi-linux-x64': '1.0.0'})
    arm_package = builder.entry(ENTRIES['koffi-arm'], '@koromix/koffi-linux-arm64', '1.0.0',
                                os=['linux'], cpu=['arm64'])
    x64_package = builder.entry(ENTRIES['koffi-x64'], '@koromix/koffi-linux-x64', '1.0.0',
                                os=['linux'], cpu=['x64'])
    # Each package ships glibc and musl bindings; the oracle must name the glibc
    # one exactly instead of accepting any path that mentions the architecture.
    for package, suffix, payload in [(arm_package, 'arm64', ('linux_arm64', 'koffi.node')),
                                     (x64_package, 'x64', ('linux_x64', 'koffi.node'))]:
        builder.write(package + payload, 'ELF')
        builder.write(package + (f'musl_{suffix}', 'koffi.node'), 'ELF')
        builder.write(package + ('docs', f'{suffix}-not-a-build.node'), 'not a binding')
        builder.write(package + (f'darwin_{suffix}', 'koffi.node'), 'not a binding')
    builder.entry(ENTRIES['cordis'], 'cordis', '4.0.2')
    builder.entry(ENTRIES['plugin'], '@deepseek-ai/cordis-plugin-x', '1.0.0',
                  peerDependencies={'cordis': '4.0.2'})

    for entry, names in [
            (ENTRIES['dsh'], [BASE, SDK, 'cordis', '@deepseek-ai/cordis-plugin-x']),
            (ENTRIES['base'], [LLM, SANDBOX]),
            (ENTRIES['sdk'], [SUBPROCESS_LOCAL, SANDBOX]),
            (ENTRIES['llm'], [PI_AI]),
            (ENTRIES['pi-ai'], [GENAI]),
            (ENTRIES['genai'], [PROTOBUF]),
            (ENTRIES['subprocess'], ['node-pty', 'koffi']),
            (ENTRIES['plugin'], ['cordis'])]:
        for name in names:
            link_in(builder, entry, name, PACKAGE_ENTRY[name], name, absolute)
    if link_platform_optional:
        name = '@koromix/koffi-linux-arm64' if arch == 'arm64' else '@koromix/koffi-linux-x64'
        entry = ENTRIES['koffi-arm'] if arch == 'arm64' else ENTRIES['koffi-x64']
        link_in(builder, ENTRIES['koffi'], name, entry, name, absolute)
    link_root(builder, ROOT_PACKAGE, ENTRIES['dsh'], absolute)
    link_hoist(builder, SANDBOX, ENTRIES['sandbox'], absolute)
    builder.link(PRODUCTION_COMMAND,
                 virtual(dsh_dir + ('lib', 'bin.js')) if absolute
                 else relative_link(PRODUCTION_COMMAND, dsh_dir + ('lib', 'bin.js')))


def self_test():
    import subprocess
    import tempfile

    vector = 0

    def audit(directory, platform='linux/arm64', expect='1.0.0'):
        return analyze(directory, PLATFORMS[platform], expect)

    def expect_pass(label, build, platform='linux/arm64', expect='1.0.0', check=None):
        nonlocal vector
        with tempfile.TemporaryDirectory(prefix='layout-pass-') as directory:
            build(Builder(directory))
            report = audit(directory, platform, expect)
            assert report['consumer'] == 'dsh' and report['noPrivatePnpm'] is True, report
            if check is not None:
                check(report)
        vector += 1
        print(f'layout self-test: {label} PASS')

    def expect_fail(label, build, reason, platform='linux/arm64', expect='1.0.0'):
        nonlocal vector
        with tempfile.TemporaryDirectory(prefix='layout-fail-') as directory:
            build(Builder(directory))
            try:
                audit(directory, platform, expect)
            except Mismatch as error:
                assert reason in str(error), (label, reason, str(error))
            else:
                raise AssertionError(f'{label}: mismatch was not detected')
        vector += 1
        print(f'layout self-test: {label} PASS')

    def healthy_check(report):
        # 15 store instances, 14 eligible: the foreign-architecture optional is
        # installed but must not be linked (and must not be required) here.
        assert report['snapshots'] == 14, report
        assert len(report['archives']) == 14, report
        assert report['exactPeers'] == 1, report
        assert report['optionalMissing'] == 1, report
        assert report['version'] == '1.0.0', report
        assert sorted(report['subprocessOwners']) == sorted({
            virtual(store_dir(ENTRIES['subprocess']) + tuple(SUBPROCESS_LOCAL.split('/'))),
            virtual(store_dir(ENTRIES['node-pty']) + ('node-pty',)),
            virtual(store_dir(ENTRIES['koffi']) + ('koffi',))}), report
        assert report['bin']['command'] == '/usr/local/bin/dsh', report
        assert report['bin']['target'].endswith('/@deepseek-ai/dsh/lib/bin.js'), report
        assert report['sandbox'] == sorted(report['sandbox']) and len(report['sandbox']) == 3, report

    expect_pass('healthy relative links', lambda b: build_healthy(b), check=healthy_check)
    expect_pass('healthy absolute links', lambda b: build_healthy(b, absolute=True), check=healthy_check)

    def unlinked_platform_optional(builder):
        build_healthy(builder, link_platform_optional=False)

    expect_fail('missing matching native optional', unlinked_platform_optional,
                'optional dependency @koromix/koffi-linux-arm64 is installed')
    # An arm64 payload is not evidence for amd64: its koffi platform optional is
    # installed but not linked for the requested platform.
    expect_fail('foreign payload is not amd64 evidence', lambda b: build_healthy(b),
                'is not eligible for linux/amd64', platform='linux/amd64')

    for arch, platform, package in [('arm64', 'linux/arm64', '@koromix/koffi-linux-arm64'),
                                    ('x64', 'linux/amd64', '@koromix/koffi-linux-x64')]:
        def healthy_arch(builder, arch=arch):
            build_healthy(builder, arch=arch)

        expect_pass(f'healthy {arch} native payloads', healthy_arch, platform=platform)

        def absent_platform_optional(builder, arch=arch, package=package):
            build_healthy(builder, arch=arch)
            # Wholly absent: the optional package and its link are both gone, as a
            # skipped platform install leaves them. An optional native payload the
            # runtime requires is not an acceptable absence.
            builder.remove(store_dir(ENTRIES['koffi']) + tuple(package.split('/')))
            builder.remove(store_dir(ENTRIES['koffi-x64' if arch == 'x64' else 'koffi-arm'])
                           + tuple(package.split('/')))

        expect_fail(f'wholly absent {arch} koffi platform optional', absent_platform_optional,
                    f'koffi requires the {platform} platform package {package}', platform=platform)

        def absent_prebuild(builder, arch=arch):
            build_healthy(builder, arch=arch)
            builder.remove(store_dir(ENTRIES['node-pty']) + ('node-pty', 'prebuilds', f'linux-{arch}', 'pty.node'))

        expect_fail(f'wholly absent {arch} node-pty prebuild', absent_prebuild,
                    f'no {platform} prebuild payload', platform=platform)

    # A path that merely contains the architecture is not this platform's payload.
    # Both packages also ship foreign and unrelated `.node` files, so only the exact
    # location may satisfy the native check.
    for label, path in [('a foreign darwin prebuild', ('prebuilds', 'darwin-arm64', 'pty.node')),
                        ('a foreign win32 prebuild', ('prebuilds', 'win32-arm64', 'pty.node')),
                        ('an unrelated .node file', ('docs', 'arm64-not-a-prebuild.node'))]:
        def pty_decoy(builder, path=path):
            build_healthy(builder, arch='arm64')
            pty = store_dir(ENTRIES['node-pty']) + ('node-pty',)
            builder.remove(pty + ('prebuilds', 'linux-arm64', 'pty.node'))
            builder.write(pty + path, 'not a linux binding')

        expect_fail(f'node-pty rejects {label}', pty_decoy,
                    'no linux/arm64 prebuild payload at')

    def koffi_decoy(builder):
        build_healthy(builder, arch='arm64')
        package = store_dir(ENTRIES['koffi-arm']) + tuple('@koromix/koffi-linux-arm64'.split('/'))
        builder.remove(package + ('linux_arm64', 'koffi.node'))
        builder.write(package + ('docs', 'arm64-not-a-build.node'), 'not a binding')
        builder.write(package + ('musl_arm64', 'koffi.node'), 'not the glibc binding')

    expect_fail('koffi rejects an unrelated .node for linux/arm64', koffi_decoy,
                'no linux/arm64 koffi build payload at')

    def host_trap(builder):
        build_healthy(builder, absolute=True)
        builder.remove(store_dir(ENTRIES['sandbox']) + tuple(SANDBOX.split('/')))
        builder.remove(PRODUCTION_PREFIX + ('node_modules', '.pnpm', 'node_modules') + tuple(SANDBOX.split('/')))
        for entry in (ENTRIES['base'], ENTRIES['sdk']):
            builder.remove(store_dir(entry) + tuple(SANDBOX.split('/')))
            builder.link(store_dir(entry) + tuple(SANDBOX.split('/')), '/etc/hosts')

    # /etc/hosts exists on the host; an implementation that reads it would call
    # the sandbox resolved. Under the virtual root it is simply absent.
    expect_fail('absolute link never reads the host', host_trap, '/etc: missing from the extracted image')

    def sandbox_absent(builder):
        # Only a genuinely absent package is a failure: the owner link alone is
        # masked by the private hoist, which is a real resolution route.
        build_healthy(builder)
        builder.remove(store_dir(ENTRIES['base']) + tuple(SANDBOX.split('/')))
        builder.remove(PRODUCTION_PREFIX + ('node_modules', '.pnpm', 'node_modules') + tuple(SANDBOX.split('/')))

    expect_fail('absent sandbox edge', sandbox_absent,
                'declared dependency @deepseek-ai/dsh-sandbox-local is not installed')

    def sandbox_escaping(builder):
        build_healthy(builder)
        builder.remove(store_dir(ENTRIES['sdk']) + tuple(SANDBOX.split('/')))
        builder.link(store_dir(ENTRIES['sdk']) + tuple(SANDBOX.split('/')), '../' * 20 + 'outside')

    expect_fail('escaping sandbox link', sandbox_escaping, 'escapes the image root')

    def sandbox_misplaced(builder):
        build_healthy(builder)
        builder.entry(ENTRIES['second-sandbox'], SANDBOX, '1.0.0')
        link_in(builder, ENTRIES['sdk'], SANDBOX, ENTRIES['second-sandbox'], SANDBOX)

    expect_fail('misplaced sandbox instance', sandbox_misplaced,
                '@deepseek-ai/dsh-sandbox-local: resolves to different instances')

    def wrong_effective_peer(builder):
        build_healthy(builder)
        builder.entry(ENTRIES['cordis-old'], 'cordis', '3.0.0')
        link_in(builder, ENTRIES['plugin'], 'cordis', ENTRIES['cordis-old'], 'cordis')

    expect_fail('wrong effective peer with the right global version', wrong_effective_peer,
                'resolves to 3.0.0, not the declared 4.0.2')

    def two_contexts(builder):
        build_healthy(builder)
        builder.entry(ENTRIES['plugin-two'], '@deepseek-ai/cordis-plugin-x', '1.0.0',
                      peerDependencies={'cordis': '4.0.2'})
        link_in(builder, ENTRIES['plugin-two'], 'cordis', ENTRIES['cordis'], 'cordis')
        link_in(builder, ENTRIES['sdk'], '@deepseek-ai/cordis-plugin-x',
                ENTRIES['plugin-two'], '@deepseek-ai/cordis-plugin-x')
        builder.rewrite_manifest(store_dir(ENTRIES['sdk']) + tuple(SDK.split('/')),
                                 lambda m: m.setdefault('dependencies', {}).update(
                                     {'@deepseek-ai/cordis-plugin-x': '1.0.0'}))

    def two_context_check(report):
        assert report['snapshots'] == 15 and report['exactPeers'] == 2, report

    expect_pass('same name/version in two contexts checked independently', two_contexts,
                check=two_context_check)

    def two_contexts_bad(builder):
        two_contexts(builder)
        builder.entry(ENTRIES['cordis-old'], 'cordis', '3.0.0')
        link_in(builder, ENTRIES['plugin-two'], 'cordis', ENTRIES['cordis-old'], 'cordis')

    expect_fail('one of two contexts has the wrong peer', two_contexts_bad,
                'resolves to 3.0.0, not the declared 4.0.2')

    def optional_marked(builder):
        build_healthy(builder)
        builder.rewrite_manifest(
            store_dir(ENTRIES['subprocess']) + tuple(SUBPROCESS_LOCAL.split('/')),
            lambda m: m.update({'peerDependencies': {'absent-provider': '2.0.0'},
                                'peerDependenciesMeta': {'absent-provider': {'optional': True}}}))

    def optional_check(report):
        assert report['optionalMissing'] == 2, report

    expect_pass('optional peer missing is marked', optional_marked, check=optional_check)

    def optional_present_wrong(builder):
        optional_marked(builder)
        builder.entry(ENTRIES['provider'], 'absent-provider', '1.0.0')
        link_in(builder, ENTRIES['subprocess'], 'absent-provider', ENTRIES['provider'], 'absent-provider')

    expect_fail('optional peer present with the wrong version', optional_present_wrong,
                'resolves to 1.0.0, not the declared 2.0.0')

    def bin_wrapper(builder):
        build_healthy(builder)
        builder.remove(PRODUCTION_COMMAND)
        builder.link(PRODUCTION_PREFIX + ('node_modules', '.bin', 'dsh'), '../@deepseek-ai/dsh/lib/bin.js')
        builder.link(PRODUCTION_COMMAND, '/opt/agent-vm/dsh/node_modules/.bin/dsh')

    expect_fail('generated .bin wrapper as command', bin_wrapper,
                'must not route through a generated .bin wrapper')

    def bin_script(builder):
        build_healthy(builder)
        builder.remove(PRODUCTION_COMMAND)
        builder.write(PRODUCTION_COMMAND, '#!/bin/sh\nexec dsh "$@"\n', mode=0o755)

    expect_fail('command is not a direct link', bin_script, 'must be a direct link to the installed entry')

    expect_fail('installed pnpm package',
                lambda b: (build_healthy(b), b.entry('private', 'pnpm', '11.11.0')),
                'second package manager')
    expect_fail('private .bin/pnpm',
                lambda b: (build_healthy(b), b.link(PRODUCTION_PREFIX + ('node_modules', '.bin', 'pnpm'),
                                                    '../.pnpm/a/node_modules/@deepseek-ai/dsh/lib/bin.js')),
                'second package manager')

    def special_file(builder):
        build_healthy(builder)
        os.mkfifo(builder.path(PRODUCTION_PREFIX + ('node_modules', '.pnpm', 'fifo')))

    expect_fail('special file in the payload', special_file, 'unexpected special file')

    def dangling(builder):
        build_healthy(builder)
        builder.link(PRODUCTION_PREFIX + ('node_modules', 'dangling'), './nowhere')

    expect_fail('dangling link', dangling, 'missing from the extracted image')

    def cyclic(builder):
        build_healthy(builder)
        builder.link(PRODUCTION_PREFIX + ('node_modules', 'loop-a'), 'loop-b')
        builder.link(PRODUCTION_PREFIX + ('node_modules', 'loop-b'), 'loop-a')

    expect_fail('cyclic links', cyclic, 'symlink cycle')

    def peer_count(report):
        assert report['exactPeers'] == 1, report

    # This link still walks up and back down inside the prefix, so it must pass;
    # the same shape escaping by one more level fails below.
    def confined_parent(builder):
        build_healthy(builder)
        link_path = store_dir(ENTRIES['plugin']) + ('cordis',)
        builder.remove(link_path)
        builder.link(link_path, relative_link(link_path, store_dir(ENTRIES['cordis']) + ('cordis',)))

    expect_pass('confined .. inside the prefix', confined_parent, check=peer_count)

    def escaping_parent(builder):
        build_healthy(builder)
        link_path = store_dir(ENTRIES['plugin']) + ('cordis',)
        builder.remove(link_path)
        builder.link(link_path, '../' * 20 + 'cordis')

    expect_fail('escaping ..', escaping_parent, 'escapes the image root')

    def ancestor_resolution(builder):
        build_healthy(builder)
        builder.rewrite_manifest(store_dir(ENTRIES['plugin']) + tuple('@deepseek-ai/cordis-plugin-x'.split('/')),
                                 lambda m: m.setdefault('dependencies', {}).update({'hoisted-dep': '1.0.0'}))
        builder.entry(ENTRIES['hoisted'], 'hoisted-dep', '1.0.0')
        link_hoist(builder, 'hoisted-dep', ENTRIES['hoisted'])

    expect_pass('ancestor/private-hoist resolution', ancestor_resolution, check=peer_count)

    def exports_hidden(builder):
        build_healthy(builder)
        path = store_dir(ENTRIES['pi-ai']) + tuple(PI_AI.split('/'))
        builder.rewrite_manifest(path, lambda m: m.update(
            {'type': 'module', 'exports': {'.': {'import': './dist/index.js'}}}))
        builder.write(path + ('dist', 'index.js'), 'export const x = 1;\n')

    expect_pass('exports-hidden package with a real entry', exports_hidden)

    def nested_marker(builder):
        # Real payloads ship manifest-shaped markers inside a package
        # (@babel/runtime/helpers/esm/package.json). They are not packages.
        build_healthy(builder)
        package = store_dir(ENTRIES['protobuf']) + ('protobufjs',)
        builder.write(package + ('helpers', 'esm', 'package.json'), '{"type":"module"}\n')

    expect_pass('manifest-shaped marker inside a package', nested_marker, check=peer_count)

    def dev_root(builder):
        build_healthy(builder)
        builder.rewrite_manifest(PRODUCTION_PREFIX, lambda m: m.update(
            {'devDependencies': {'typescript': '1.0.0'}}))

    expect_fail('dev dependencies in the installed project', dev_root, 'must not carry dev dependencies')

    expect_fail('unreachable package instance',
                lambda b: (build_healthy(b), b.entry('stray', 'stray-package', '1.0.0')),
                'not reachable from the project or the private hoist')

    def wrong_bin_map(builder):
        build_healthy(builder)
        builder.rewrite_manifest(store_dir(ENTRIES['dsh']) + tuple(ROOT_PACKAGE.split('/')),
                                 lambda m: m.update({'bin': {'dsh': 'lib/other.js'}}))

    expect_fail('wrong installed bin mapping', wrong_bin_map, 'is not the reviewed')

    expect_fail('expected version mismatch', lambda b: build_healthy(b, dsh_version='1.0.1'),
                'is not the expected 1.0.0')

    expect_fail('expected version cannot be inferred from the image',
                lambda b: build_healthy(b), 'is not the expected 2.0.0', expect='2.0.0')

    selection = json.loads(SOURCE_MANIFEST.read_text())['dependencies'][ROOT_PACKAGE]
    with tempfile.TemporaryDirectory(prefix='layout-default-') as directory:
        build_healthy(Builder(directory), dsh_version=selection)
        default_report = analyze(directory, PLATFORMS['linux/arm64'], selection)
        assert default_report['version'] == selection, default_report
    vector += 1
    print('layout self-test: default expectation is the committed source selection PASS')

    # Deterministic reporting: the same tree, built twice in a different link
    # style and order, must produce byte-identical summaries.
    with tempfile.TemporaryDirectory(prefix='layout-order-') as first, \
            tempfile.TemporaryDirectory(prefix='layout-order-') as second:
        build_healthy(Builder(first))
        build_healthy(Builder(second), absolute=True)
        left = json.dumps(audit(first), sort_keys=True, separators=(',', ':'))
        right = json.dumps(audit(second), sort_keys=True, separators=(',', ':'))
        assert left == right, (left, right)
        assert left == json.dumps(audit(first), sort_keys=True, separators=(',', ':')), left
    vector += 1
    print('layout self-test: deterministic summary PASS')

    # CLI contract: usage 2, mismatch 1 with the reason, success 0 and one line.
    script = str(Path(__file__).resolve())

    def cli(*args):
        return subprocess.run([sys.executable, '-B', script, *args], capture_output=True, text=True,
                              timeout=300)

    for args in ([], ['--consumer', 'dsh'], ['--consumer', 'dsh', '--root', '/tmp'],
                 ['--consumer', 'dsh', '--root', '/tmp', '--platform', 'linux/riscv64'],
                 ['--consumer', 'pi', '--root', '/tmp', '--platform', 'linux/arm64'],
                 ['--consumer', 'dsh', '--consumer', 'dsh', '--root', '/tmp', '--platform', 'linux/arm64'],
                 ['--consumer', 'dsh', '--root', '/tmp', '--platform', 'linux/arm64', '--unexpected', 'x'],
                 ['--consumer', 'dsh', '--root', 'relative/path', '--platform', 'linux/arm64'],
                 ['--self-test', '--root', '/tmp'],
                 ['--consumer', 'dsh', '--root', '/tmp', '--platform', 'linux/arm64',
                  '--expect-version', 'not-a-version']):
        result = cli(*args)
        assert result.returncode == 2, (args, result.returncode, result.stderr)
    with tempfile.TemporaryDirectory(prefix='layout-cli-') as directory:
        build_healthy(Builder(directory))
        ok = cli('--consumer', 'dsh', '--root', directory, '--platform', 'linux/arm64',
                 '--expect-version', '1.0.0')
        assert ok.returncode == 0, (ok.returncode, ok.stderr)
        lines = [line for line in ok.stdout.splitlines() if line.strip()]
        assert len(lines) == 1 and json.loads(lines[0])['consumer'] == 'dsh', ok.stdout
        bad = cli('--consumer', 'dsh', '--root', directory, '--platform', 'linux/arm64',
                  '--expect-version', '9.9.9')
        assert bad.returncode == 1 and bad.stdout == '', (bad.returncode, bad.stdout)
        assert 'is not the expected 9.9.9' in bad.stderr, bad.stderr
        wrong_platform = cli('--consumer', 'dsh', '--root', directory, '--platform', 'linux/amd64')
        assert wrong_platform.returncode == 1, wrong_platform.stderr
    vector += 1
    print('layout self-test: CLI exit contract PASS')

    print(f'standard-node-layout: self-test PASS ({vector} vectors, virtual roots only)')
    return 0


def parse_arguments(args):
    if args == ['--self-test']:
        return {'self_test': True}
    if any(arg == '--self-test' for arg in args):
        raise Usage(USAGE)
    values = {}
    rest = list(args)
    while rest:
        flag = rest.pop(0)
        if flag not in ('--consumer', '--root', '--platform', '--expect-version'):
            raise Usage(f'unknown option: {flag}\n{USAGE}')
        if not rest or rest[0].startswith('--'):
            raise Usage(f'missing value for {flag}\n{USAGE}')
        value = rest.pop(0)
        if flag in values:
            raise Usage(f'duplicate option: {flag}\n{USAGE}')
        values[flag] = value
    if '--consumer' not in values:
        raise Usage(f'missing --consumer\n{USAGE}')
    if values['--consumer'] != 'dsh':
        raise Usage(f'unsupported consumer: {values["--consumer"]}\n{USAGE}')
    for required in ('--root', '--platform'):
        if required not in values:
            raise Usage(f'missing {required}\n{USAGE}')
    if values['--platform'] not in PLATFORMS:
        raise Usage(f'unsupported platform: {values["--platform"]}\n{USAGE}')
    root = Path(values['--root'])
    if not root.is_absolute():
        raise Usage('--root must be an absolute virtual image root')
    if Path(os.path.abspath(root)) == Path('/'):
        raise Usage('--root must be an extracted image root, not the host root')
    if not root.is_dir():
        raise Usage(f'--root is not a directory: {root}')
    expect = values.get('--expect-version')
    if expect is None:
        try:
            expect = json.loads(SOURCE_MANIFEST.read_text())['dependencies'][ROOT_PACKAGE]
        except (OSError, ValueError, KeyError, TypeError) as error:
            raise Usage(f'cannot read the source selection: {error}') from None
        if not isinstance(expect, str) or not EXACT_VERSION.match(expect):
            raise Usage('source manifest does not pin an exact dsh version')
    elif not EXACT_VERSION.match(expect):
        raise Usage(f'--expect-version must be an exact version: {expect}')
    return {'root': root, 'platform': PLATFORMS[values['--platform']], 'expect_version': expect}


def main(args):
    try:
        options = parse_arguments(args)
    except Usage as error:
        print(f'standard-node-layout: {error}', file=sys.stderr)
        return 2
    if options.get('self_test'):
        return self_test()
    try:
        report = analyze(options['root'], options['platform'], options['expect_version'])
    except (Mismatch, OSError) as error:
        print(f'standard-node-layout: dsh: {error}', file=sys.stderr)
        return 1
    print(json.dumps(report, sort_keys=True, separators=(',', ':')))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
