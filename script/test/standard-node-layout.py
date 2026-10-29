#!/usr/bin/env python3
"""Independent oracle for the filesystem layout an image actually ships.

Why a second implementation instead of reusing the image's own checks: the
production checker runs *inside* the build (Node, real install, real modules)
and the graph validator parses the lock the build just consumed. Neither can
show that the finished image carries the reviewed payload: a Dockerfile could
copy a different tree, a store could be linked from outside, or the installed
manifests could disagree with the lock. This oracle therefore reads only a
virtual copy of the extracted image and answers one question: is the shipped
file/link/manifest graph a self-contained, peer-correct install of the named
consumer?

Three consumers share the physical machinery below and keep their own
independently authored expectations:

* ``dsh``    -- isolated private store, hoisted sandbox route, native payloads.
* ``pi``     -- isolated store, the Pi family shape (six members for the current
                default, the historical five for the 0.87.0 vector), the
                package-root generated ``.bin`` inventory, and a separate
                warning-wrapper command that must *not* be the package entry.
* ``bridge`` -- a hoisted (link-free) tree with the SDK JavaScript present, no
                SDK platform payload, and no second Pi/typebox instance.

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

import hashlib
import json
import os
import re
import stat
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

USAGE = ('usage: standard-node-layout.py --consumer dsh|pi|bridge --root ROOT '
         '--platform linux/amd64|linux/arm64 [--expect-version EXACT] '
         '[--archive-inventories EXPECTED_JSON]\n'
         '       standard-node-layout.py --self-test')

# Production paths are fixed: this oracle certifies the image, not a developer
# staging directory, so there is no prefix/command override to get wrong.
PRODUCTION_PREFIX = ('opt', 'agent-vm', 'dsh')
PRODUCTION_COMMAND = ('usr', 'local', 'bin', 'dsh')
PRODUCTION_PREFIXES = {
    'dsh': PRODUCTION_PREFIX,
    'pi': ('opt', 'agent-vm', 'pi'),
    'bridge': ('opt', 'agent-vm', 'pi-packages'),
}
# Pi's reviewed command is the warning *wrapper* (`images/tools/pi/pi.sh`), which
# is a regular file and must never become the package entry. The bridge owns no
# command of its own: Pi loads it as an extension. `None` therefore means "this
# consumer must not install a command", not "the check is optional".
PRODUCTION_COMMANDS = {'dsh': PRODUCTION_COMMAND, 'pi': ('usr', 'local', 'bin', 'pi'),
                       'bridge': None}

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
PI_SOURCE_MANIFEST = Path(__file__).resolve().parents[2] / 'images/tools/pi/package.json'
BRIDGE_SOURCE_MANIFEST = Path(__file__).resolve().parents[2] / 'images/tools/pi/bridge/package.json'
SOURCE_MANIFESTS = {'dsh': SOURCE_MANIFEST, 'pi': PI_SOURCE_MANIFEST, 'bridge': BRIDGE_SOURCE_MANIFEST}
SOURCE_DEPENDENCIES = {'dsh': ROOT_PACKAGE, 'pi': '@earendil-works/pi-coding-agent',
                       'bridge': 'pi-claude-bridge'}


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

# --- Pi -------------------------------------------------------------------
# The reviewed family shape per *exact selected version*. This table is an
# independent expectation, not a derivation from the installed tree: a wrong
# shape has to be reported, so it cannot be inferred from the thing under test.
# `telemetry` is a transitive family member in both shapes (the root manifest
# declares it through `pi-ai`), which is why the direct-edge set is spelled out
# separately from the membership set. `bins` is the reviewed package-root
# generated `.bin` inventory; the link *targets* are still derived from each
# authenticated/installed dependency's own `bin` declaration and locked edge.
PI_FAMILY = {
    '1.4.0': {
        'members': ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui'),
        'direct': ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-tui'),
        'bins': {'pi-agent-core': ('pi-ai',), 'pi-ai': ('anthropic-ai-sdk',),
                 'pi-codemode': (), 'pi-mcp': (), 'pi-telemetry': (), 'pi-tui': ('marked',)},
    },
    '0.87.0': {
        'members': ('chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry', 'pi-tui'),
        'direct': ('chord', 'pi-agent-core', 'pi-ai', 'pi-tui'),
        'bins': {'chord': ('esbuild',), 'pi-agent-core': ('pi-ai', 'yaml'),
                 'pi-ai': ('anthropic-ai-sdk',), 'pi-telemetry': (), 'pi-tui': ('marked',)},
    },
}
PI_FAMILY_PREFIX = '@earendil-works/'
PI_ROOT_PACKAGE = '@earendil-works/pi-coding-agent'
PI_ROOT_BIN = {'pi': 'dist/bundle/cli.js'}
# ``node_modules/.bin/pi`` is the reviewed seam; ``/usr/local/bin/pi`` is the
# static warning wrapper and must stay a separate regular file.
PI_ROOT_BIN_LINK = ('node_modules', '.bin', 'pi')

# --- bridge ---------------------------------------------------------------
BRIDGE_ROOT_PACKAGE = 'pi-claude-bridge'
BRIDGE_SDK = '@anthropic-ai/claude-agent-sdk'
BRIDGE_SDK_SOURCE = ('src', 'index.ts')
# The reviewed workspace sets `autoInstallPeers: false`, so exactly these peer
# declarations stay unmet and no second copy may be installed instead. Peer
# *text* is data; only resolved identities are policed here.
BRIDGE_UNMET_PEERS = (PI_ROOT_PACKAGE, '@earendil-works/pi-ai', '@earendil-works/pi-tui',
                      'typebox', '@anthropic-ai/sdk')
# The one reviewed omission: the SDK's per-platform native packages, matched by
# the workspace's exact `ignoredOptionalDependencies` pattern. Every other
# optional payload that the lock resolves for this platform must be installed.
BRIDGE_IGNORED_OPTIONAL = re.compile(r'^@anthropic-ai/claude-agent-sdk-')
# No second Pi/typebox instance in the bridge tree, in any spelling.
BRIDGE_FORBIDDEN_NAMES = ('typebox', '@sinclair/typebox', 'pi-agent-core', 'pi-ai', 'pi-tui',
                          'pi-coding-agent')
BRIDGE_FORBIDDEN_PREFIXES = ('@earendil-works/', '@sinclair/')



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


def declared_bin(name, value, path):
    """A package's `bin`, in both spellings npm accepts.

    A string maps the package's own basename to that path; an object maps each
    command name. A leading `./` is a spelling, not a location -- the published
    `yaml@2.9.0` declares `"bin": "./bin.mjs"` -- so it is collapsed here the
    way npm and pnpm do, while any other dot or empty segment is refused rather
    than silently rewritten.
    """
    if value is None:
        return None
    if isinstance(value, str):
        mapping = {name.rsplit('/', 1)[-1]: value}
    elif isinstance(value, dict):
        mapping = value
    else:
        raise Mismatch(f'{virtual(path)}: manifest field bin must be a string or an object')
    result = {}
    for command, target in mapping.items():
        if not isinstance(target, str):
            raise Mismatch(f'{virtual(path)}: bin {command} has no string target')
        parts = target.split('/')
        if target.startswith('/') or any(part in ('', '..') for part in parts):
            raise Mismatch(f'{virtual(path)}: bin {command} target {target!r} is not a normalized '
                           'relative path')
        result[command] = '/'.join(part for part in parts if part != '.')
    return result


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
        bin=declared_bin(name, raw.get('bin'), path),
    )


class Analysis:
    """One audit of an extracted image root.

    This class carries the *physical* machinery every consumer shares (virtual
    root, link expansion, inventory, Node-style ancestor resolution, edge and
    manager checks) together with the dsh layout decisions it grew up with.
    ``PiAudit`` and ``BridgeAudit`` inherit the machinery and replace every dsh
    layout decision; no dsh method body is reached from their ``run()``.
    """

    def __init__(self, root: Path, platform: Platform, expect_version: str, consumer: str = 'dsh'):
        self.consumer = consumer
        self.filesystem = VirtualRoot(root)
        self.prefix = PRODUCTION_PREFIXES[consumer]
        self.command = PRODUCTION_COMMANDS[consumer]
        self.tree = Tree(self.filesystem).scan(self.prefix)
        self.platform = platform
        self.expect_version = expect_version
        self.instances = {}
        self.excluded = set()
        # Reviewed exceptions, set by the consumer that owns them: a peer that
        # is declared but deliberately not installed, and the predicate for an
        # optional dependency this platform/policy legitimately omits.
        self.unmet_peers = frozenset()
        self.optional_absent_allowed = lambda owner, name: True

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
                raise Mismatch(f'{virtual(path)}: link target {virtual(resolved)} leaves the '
                               f'{self.consumer} prefix')
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
                    if not self.optional_absent_allowed(owner, name):
                        raise Mismatch(f'{virtual(owner)}: optional dependency {name} is absent and '
                                       'no reviewed rule allows omitting it')
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
                    if name in self.unmet_peers:
                        # Reviewed: the workspace sets `autoInstallPeers: false`,
                        # so this declaration stays unmet on purpose.
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
            raise Mismatch(f'second package manager in the {self.consumer} tree: '
                           + '; '.join(sorted(set(found))))

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
# Pi and the bridge. Both reuse the physical machinery above and own every
# layout decision themselves. The expectations below (family shape, generated
# bin names, wrapper separation, bridge omission) are authored from the
# independent review of the selected archives and the reviewed workspaces --
# never read back from an installed tree or from the production checker.
# --------------------------------------------------------------------------


def manifest_bins(manifest: Manifest, path):
    """The package's declared bin mapping, validated as data."""
    if manifest.bin is None:
        return {}
    result = {}
    for name, target in manifest.bin.items():
        if not isinstance(name, str) or not name or '/' in name or name in ('.', '..'):
            raise Mismatch(f'{virtual(path)}: unusable bin name {name!r}')
        if not isinstance(target, str) or not target:
            raise Mismatch(f'{virtual(path)}: bin {name} has no string target')
        parts = target.split('/')
        if target.startswith('/') or any(part in ('', '.', '..') for part in parts):
            raise Mismatch(f'{virtual(path)}: bin {name} target {target!r} is not a normalized '
                           'relative path')
        result[name] = tuple(parts)
    return result


class MigratedAudit(Analysis):
    """Shared Pi/bridge audit: one isolated-or-hoisted project, one exact root.

    The dsh chain checks and the dsh command link are structurally different
    (dsh installs its own command; Pi installs a package bin and keeps a warning
    wrapper), so each consumer below supplies its own ``run`` and root checks.
    """

    root_package = ''
    hoisted = False

    def __init__(self, root, platform, expect_version, consumer, inventories=None):
        super().__init__(root, platform, expect_version, consumer)
        self.inventories = inventories

    def check_project(self):
        self.project = read_manifest(self.tree, self.prefix + ('package.json',),
                                    require_version=False)
        if set(self.project.dependencies) != {self.root_package}:
            raise Mismatch(f'installed {self.consumer} project must declare only the '
                           f'{self.root_package} dependency')
        selected = self.project.dependencies[self.root_package]
        if not EXACT_VERSION.match(selected):
            raise Mismatch(f'installed {self.consumer} project must pin {self.root_package} to an '
                           'exact version')
        raw = json.loads(self.filesystem.physical(self.prefix + ('package.json',)).read_bytes())
        if raw.get('devDependencies') or raw.get('workspaces'):
            raise Mismatch(f'installed {self.consumer} project must not carry dev dependencies or '
                           'workspace roots')
        node_modules = self.prefix + ('node_modules',)
        if node_modules in self.tree.links or node_modules not in self.tree.dirs:
            raise Mismatch(f'{virtual(node_modules)}: installed node_modules is missing or a link')
        modules_yaml = node_modules + ('.modules.yaml',)
        if modules_yaml in self.tree.links or modules_yaml not in self.tree.files:
            raise Mismatch(f'{virtual(modules_yaml)}: install metadata is missing from the payload')

    def check_root(self):
        found = self.resolve(self.prefix, self.root_package)
        if found is None:
            raise Mismatch(f'{self.root_package}: root dependency is not installed')
        manifest = self.instances[found]
        if manifest.version != self.expect_version:
            raise Mismatch(f'installed {self.root_package}@{manifest.version} is not the expected '
                           f'{self.expect_version}')
        return found

    def check_wrapper(self, prefix_message):
        """``None`` means the consumer owns no command; otherwise a regular file."""
        if self.command is None:
            raise Mismatch(prefix_message)
        try:
            info = self.filesystem.lstat(self.command)
        except FileNotFoundError:
            raise Mismatch(prefix_message) from None
        if stat.S_ISLNK(info.st_mode):
            raise Mismatch(f'{virtual(self.command)}: must be a regular file, not a link to the '
                           'installed package entry')
        if not stat.S_ISREG(info.st_mode):
            raise Mismatch(f'{virtual(self.command)}: must be a regular file')
        if not info.st_mode & stat.S_IXUSR:
            raise Mismatch(f'{virtual(self.command)}: is not executable')
        if info.st_size == 0 or info.st_size > MAX_ENTRY_BYTES:
            raise Mismatch(f'{virtual(self.command)}: has an implausible size')
        first = self.filesystem.physical(self.command).read_bytes().split(b'\n', 1)[0]
        if not first.startswith(b'#!'):
            raise Mismatch(f'{virtual(self.command)}: has no interpreter line')
        return virtual(self.command)

    def bin_entries(self, package_dir):
        """Generated ``.bin`` links directly inside one package root."""
        directory = package_dir + ('node_modules', '.bin')
        return {path[-1]: path for path in self.tree.links
                if path[:-1] == directory}

    def check_bin_inventory(self, package_dir, expected_names):
        """One package root's generated bins, derived from its locked edges.

        The reviewed table supplies the *names*; the target is derived from the
        dependency's own ``bin`` declaration resolved through the owner's real
        edge, so a same-name/version package in another context cannot satisfy
        it. Every entry must be a generated link and nothing else may exist.
        """
        present = self.bin_entries(package_dir)
        expected = set(expected_names)
        extra = sorted(set(present) - expected)
        if extra:
            raise Mismatch(f'{virtual(package_dir)}: unexpected generated bin {extra[0]}')
        resolved = {}
        for name in expected_names:
            if name not in present:
                raise Mismatch(f'{virtual(package_dir)}: missing generated bin {name}')
            owner = package_dir
            candidates = []
            for dependency, _spec in sorted(list(self.instances[owner].dependencies.items())
                                            + list(self.instances[owner].optional.items())):
                target = self.resolve(owner, dependency)
                if target is None or target in self.excluded:
                    continue
                bins = manifest_bins(self.instances[target], target)
                if name in bins:
                    candidates.append((target, bins[name]))
            if len(candidates) != 1:
                raise Mismatch(f'{virtual(package_dir)}: {len(candidates)} locked dependencies '
                               f'declare bin {name}; exactly one is required')
            target, relative = candidates[0]
            expected_target = target + relative
            if expected_target not in self.tree.files:
                raise Mismatch(f'{virtual(expected_target)}: declared bin file of {name} is missing')
            link = present[name]
            literal = self.filesystem.readlink(link)
            canonical = self.filesystem.follow((), link)
            if canonical != expected_target:
                raise Mismatch(f'{virtual(link)}: generated bin {name} resolves to '
                               f'{virtual(canonical)}, not the locked target '
                               f'{virtual(expected_target)}')
            resolved[name] = {'link': virtual(link), 'target': virtual(canonical),
                              'readlink': literal}
        return resolved

    def reachable(self, seeds):
        """Instances reachable from the seeds through declared dependency edges."""
        pending = list(seeds)
        visited = set()
        while pending:
            current = pending.pop()
            if current in visited or current not in self.instances:
                continue
            visited.add(current)
            manifest = self.instances[current]
            for name in sorted(set(manifest.dependencies) | set(manifest.optional)
                               | set(manifest.peers)):
                found = self.resolve(current, name)
                if found is not None:
                    pending.append(found)
        return visited

    def reject_orphans(self, seeds):
        visited = self.reachable(seeds)
        orphans = sorted(virtual(path) for path in self.instances
                         if path not in visited and path not in self.excluded)
        if orphans:
            raise Mismatch(f'installed {self.consumer} packages are not reachable from the reviewed '
                           'root edges: ' + ', '.join(orphans))

    # -- independently authenticated archive inventories --------------------

    def check_archive_inventories(self):
        """Compare installed package roots with pre-run authenticated archives.

        The expected file is authored *before* the run from independently
        downloaded and SRI-checked tarballs (see ``--archive-inventories`` and
        the self-test for the exact rule). Reading it back from the installed
        tree, or from the production byte checker, would make the comparison
        circular, so this method only ever consumes the supplied data.
        """
        if self.inventories is None:
            return None
        compared = {}
        for identity, package in sorted(self.inventories.items()):
            name, _, version = identity.rpartition('@')
            if not name:
                raise Mismatch(f'archive inventory {identity!r}: not name@version')
            instances = [path for path, manifest in self.instances.items()
                         if manifest.name == name and manifest.version == version]
            if len(instances) != 1:
                raise Mismatch(f'archive inventory {identity}: {len(instances)} installed '
                               'instances carry that identity; the archive can only be compared '
                               'with exactly one')
            compared[identity] = self.compare_package(instances[0], dict(package, identity=identity))
        return compared

    def compare_package(self, package_dir, package):
        entries = package['entries']
        allowed_generated = {package_dir + ('node_modules',),
                            package_dir + ('node_modules', '.bin')}
        for name in package.get('generatedBins', ()):
            allowed_generated.add(package_dir + ('node_modules', '.bin', name))
        problems = []
        for relative in sorted(entries):
            parts = tuple(relative.split('/'))
            path = package_dir + parts
            entry = entries[relative]
            if entry['kind'] == 'dir':
                if path not in self.tree.dirs:
                    problems.append(f'{virtual(path)}: expected archive directory is absent')
                continue
            if entry['kind'] == 'symlink':
                if path in self.tree.links:
                    if self.filesystem.readlink(path) != entry['target']:
                        problems.append(f'{virtual(path)}: link target '
                                        f'{self.filesystem.readlink(path)!r} is not the archive '
                                        f'{entry["target"]!r}')
                else:
                    problems.append(f'{virtual(path)}: expected archive link is absent')
                continue
            if path in self.tree.links or path not in self.tree.files:
                problems.append(f'{virtual(path)}: expected archive file is absent')
                continue
            mode, size = self.tree.files[path]
            if size != entry['size']:
                problems.append(f'{virtual(path)}: {size} bytes, archive has {entry["size"]}')
                continue
            digest = hashlib.sha256(self.filesystem.physical(path).read_bytes()).hexdigest()
            if digest != entry['sha256']:
                problems.append(f'{virtual(path)}: bytes differ from the authenticated archive')
            elif bool(mode & 0o111) != entry['executable']:
                problems.append(f'{virtual(path)}: executable bit differs from the archive')
        installed = set()
        for path in list(self.tree.files) + list(self.tree.links) + list(self.tree.dirs):
            if path[:len(package_dir)] == package_dir and len(path) > len(package_dir):
                installed.add(path)
        for path in sorted(installed):
            relative = '/'.join(path[len(package_dir):])
            if relative in entries or path in allowed_generated:
                continue
            problems.append(f'{virtual(path)}: extra path not present in the authenticated archive')
        if problems:
            raise Mismatch(f'{self.consumer} package {package["identity"]}: ' + '; '.join(problems[:8]))
        return len(entries)


class PiAudit(MigratedAudit):
    """Pi's isolated install: family shape, generated bins, wrapper split."""

    root_package = PI_ROOT_PACKAGE
    hoisted = False

    def __init__(self, root, platform, expect_version, inventories=None):
        super().__init__(root, platform, expect_version, 'pi', inventories)
        self.shape = PI_FAMILY.get(expect_version)
        if self.shape is None:
            raise Mismatch(f'no reviewed Pi family shape covers {expect_version}; add one from the '
                           'authenticated archives before certifying that selection')

    def check_store(self):
        store = self.prefix + ('node_modules', '.pnpm')
        if store in self.tree.links or store not in self.tree.dirs:
            raise Mismatch(f'{virtual(store)}: an isolated install carries the private store')

    def check_root(self):
        found = super().check_root()
        manifest = self.instances[found]
        if manifest.bin != PI_ROOT_BIN:
            raise Mismatch(f'{PI_ROOT_PACKAGE}: installed bin mapping {manifest.bin!r} is not the '
                           f'reviewed {PI_ROOT_BIN!r}')
        entry = found + tuple(PI_ROOT_BIN['pi'].split('/'))
        if entry in self.tree.links or entry not in self.tree.files:
            raise Mismatch(f'{virtual(entry)}: installed Pi entry must be a regular file')
        mode, size = self.tree.files[entry]
        if size == 0 or size > MAX_ENTRY_BYTES:
            raise Mismatch(f'{virtual(entry)}: installed Pi entry has an implausible size')
        if not mode & stat.S_IXUSR:
            raise Mismatch(f'{virtual(entry)}: installed Pi entry is not executable')
        first = self.filesystem.physical(entry).read_bytes().split(b'\n', 1)[0]
        if not first.startswith(b'#!') or b'node' not in first[2:]:
            raise Mismatch(f'{virtual(entry)}: installed Pi entry has no Node shebang')
        # The reviewed seam is the package bin link; the wrapper is checked apart.
        link = self.prefix + PI_ROOT_BIN_LINK
        if link not in self.tree.links:
            raise Mismatch(f'{virtual(link)}: the generated Pi bin link is missing')
        if self.filesystem.follow((), link) != entry:
            raise Mismatch(f'{virtual(link)}: resolves to '
                           f'{virtual(self.filesystem.follow((), link))}, not the installed Pi '
                           f'entry {virtual(entry)}')
        return found, entry

    def check_family(self, root):
        """The reviewed family shape, found through real edges only."""
        members = {}
        for short in self.shape['members']:
            name = PI_FAMILY_PREFIX + short
            candidates = sorted(path for path, manifest in self.instances.items()
                                if manifest.name == name)
            if len(candidates) != 1:
                raise Mismatch(f'{name}: {len(candidates)} installed instances; the reviewed family '
                               'shape has exactly one')
            path = candidates[0]
            if path in self.excluded:
                raise Mismatch(f'{virtual(path)}: family member is not eligible for {self.platform.key}')
            if self.instances[path].version != self.expect_version:
                raise Mismatch(f'{name}: installed {self.instances[path].version} is not the selected '
                               f'{self.expect_version}')
            members[short] = path
        declared = {name[len(PI_FAMILY_PREFIX):] for name in self.instances[root].dependencies
                    if name.startswith(PI_FAMILY_PREFIX)}
        if declared != set(self.shape['direct']):
            raise Mismatch(f'{PI_ROOT_PACKAGE}: direct family edges {sorted(declared)} are not the '
                           f'reviewed {sorted(self.shape["direct"])}')
        reachable = self.reachable([root])
        missing = sorted(short for short, path in members.items() if path not in reachable)
        if missing:
            raise Mismatch(f'Pi family members {missing} are installed but not reachable from the '
                           'root dependency edges')
        unexpected = sorted({manifest.name for manifest in self.instances.values()
                             if manifest.name.startswith(PI_FAMILY_PREFIX)}
                            - {PI_FAMILY_PREFIX + short for short in self.shape['members']}
                            - {PI_ROOT_PACKAGE})
        if unexpected:
            raise Mismatch('installed Pi family carries unreviewed members: ' + ', '.join(unexpected))
        return members

    def run(self):
        self.check_links()
        self.collect_instances()
        self.check_project()
        self.check_store()
        self.check_manager()
        root, entry = self.check_root()
        members = self.check_family(root)
        exact_peers, optional_missing = self.check_edges()
        bins = {short: self.check_bin_inventory(members[short], self.shape['bins'][short])
                for short in self.shape['members']}
        wrapper = self.check_wrapper(f'{virtual(self.command)}: the Pi wrapper is missing')
        self.reject_orphans([root])
        compared = self.check_archive_inventories()
        return {
            'consumer': 'pi',
            'version': self.instances[root].version,
            'platform': self.platform.key,
            'archives': sorted({f'{manifest.name}@{manifest.version}' for path, manifest
                                in self.instances.items() if path not in self.excluded}),
            'snapshots': len(self.instances) - len(self.excluded),
            'exactPeers': exact_peers,
            'optionalMissing': optional_missing,
            'familyShape': sorted(self.shape['members']),
            'family': {name: virtual(path) for name, path in sorted(members.items())},
            'bin': {'entry': PI_ROOT_BIN['pi'], 'target': virtual(entry),
                    'link': virtual(self.prefix + PI_ROOT_BIN_LINK), 'wrapper': wrapper},
            'generatedBins': bins,
            'archiveInventories': compared,
            'noPrivatePnpm': True,
        }


class BridgeAudit(MigratedAudit):
    """The bridge's hoisted install: SDK JavaScript, no platform payload, no alias."""

    root_package = BRIDGE_ROOT_PACKAGE
    hoisted = True

    def __init__(self, root, platform, expect_version, inventories=None):
        super().__init__(root, platform, expect_version, 'bridge', inventories)
        self.unmet_peers = frozenset(BRIDGE_UNMET_PEERS)
        self.optional_absent_allowed = lambda owner, name: bool(BRIDGE_IGNORED_OPTIONAL.match(name))

    def check_hoisted(self):
        """A hoisted tree has no package links; only generated ``.bin`` links."""
        for path in sorted(self.tree.links):
            if '.bin' in path:
                continue
            raise Mismatch(f'{virtual(path)}: a hoisted bridge install carries no package links')
        # pnpm 11.11.0 keeps a metadata-only `node_modules/.pnpm/lock.yaml` in a
        # hoisted install (observed by the focused harness' first real cold run),
        # so a store *directory* is not the discriminator: store *payload* is.
        store = self.prefix + ('node_modules', '.pnpm')
        for path in sorted(self.tree.dirs):
            if len(path) > len(store) and path[:len(store)] == store:
                raise Mismatch(f'{virtual(path)}: a hoisted bridge install carries no private '
                               'store payload')
        node_modules = self.prefix + ('node_modules',)
        instance = node_modules + (BRIDGE_ROOT_PACKAGE,)
        try:
            info = self.filesystem.lstat(instance)
        except FileNotFoundError:
            raise Mismatch(f'{virtual(instance)}: the bridge package directory is missing') from None
        if not stat.S_ISDIR(info.st_mode):
            raise Mismatch(f'{virtual(instance)}: the bridge package is not a real directory')
        return instance

    def check_no_second_pi(self):
        found = []
        for path, manifest in sorted(self.instances.items()):
            name = manifest.name
            if name.startswith(BRIDGE_FORBIDDEN_PREFIXES) or name in BRIDGE_FORBIDDEN_NAMES:
                found.append(f'{name} at {virtual(path)}')
            for peer in BRIDGE_UNMET_PEERS:
                if name == peer:
                    found.append(f'{name} at {virtual(path)}')
        if found:
            raise Mismatch('the bridge tree installs a second Pi/typebox package: '
                           + '; '.join(sorted(set(found))))

    def check_sdk(self, bridge):
        sdk = self.require(bridge, BRIDGE_SDK)
        if sdk in self.excluded:
            raise Mismatch(f'{virtual(sdk)}: the SDK JavaScript package is not eligible for '
                           f'{self.platform.key}')
        if self.instances[sdk].name != BRIDGE_SDK:
            raise Mismatch(f'{virtual(sdk)}: installed {self.instances[sdk].name} is not {BRIDGE_SDK}')
        # The platform payloads are omitted by the reviewed workspace; the
        # JavaScript that Pi's jiti loads is not. At least one shipped module
        # must exist, because a payload-less SDK is the failure the omission
        # was narrowed to prevent.
        modules = sorted(path for path in list(self.tree.files) + list(self.tree.links)
                         if path[:len(sdk)] == sdk and path[-1].endswith(('.js', '.mjs', '.cjs')))
        if not modules:
            raise Mismatch(f'{virtual(sdk)}: the SDK ships no JavaScript module')
        return sdk, modules[0]

    def check_native_omission(self):
        installed = sorted(f'{manifest.name}@{manifest.version}'
                           for manifest in self.instances.values()
                           if BRIDGE_IGNORED_OPTIONAL.match(manifest.name))
        if installed:
            raise Mismatch('the bridge installs an SDK platform payload the reviewed workspace omits: '
                           + ', '.join(installed))
        for path in list(self.tree.files) + list(self.tree.links):
            if path[:len(self.prefix)] != self.prefix:
                continue
            if any(part.startswith('claude-agent-sdk-') for part in path):
                raise Mismatch(f'{virtual(path)}: SDK platform payload present in the bridge tree')

    def run(self):
        self.check_links()
        self.collect_instances()
        self.check_project()
        self.check_hoisted()
        self.check_manager()
        bridge = self.check_root()
        source = bridge + BRIDGE_SDK_SOURCE
        if source in self.tree.links or source not in self.tree.files:
            raise Mismatch(f'{virtual(source)}: the bridge TypeScript entry is missing')
        exact_peers, optional_missing = self.check_edges()
        self.check_no_second_pi()
        self.check_native_omission()
        sdk, module = self.check_sdk(bridge)
        self.reject_orphans([bridge])
        compared = self.check_archive_inventories()
        return {
            'consumer': 'bridge',
            'version': self.instances[bridge].version,
            'platform': self.platform.key,
            'archives': sorted({f'{manifest.name}@{manifest.version}' for path, manifest
                                in self.instances.items() if path not in self.excluded}),
            'snapshots': len(self.instances) - len(self.excluded),
            'exactPeers': exact_peers,
            'optionalMissing': optional_missing,
            'hoisted': True,
            'sdk': {'path': virtual(sdk), 'module': virtual(module)},
            'source': virtual(source),
            'archiveInventories': compared,
            'noPrivatePnpm': True,
        }


def analyze_migrated(root, platform, expect_version, consumer, inventories):
    audit = {'pi': PiAudit, 'bridge': BridgeAudit}[consumer]
    return audit(Path(root), platform, expect_version, inventories).run()


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


# --------------------------------------------------------------------------
# Synthetic virtual roots for the migrated consumers. They are written into a
# temp directory and audited through the same public entry point as a real
# extraction. Every expectation the audit compares against (family shape, bin
# names, wrapper separation, hoisting, omission) is the reviewed table above;
# these fixtures only build a *tree* that either honours it or mutates it.
# --------------------------------------------------------------------------

PI_PREFIX = PRODUCTION_PREFIXES['pi']
BRIDGE_PREFIX = PRODUCTION_PREFIXES['bridge']


def migrated_entry(builder, directory, name, version, **fields):
    package = directory + tuple(name.split('/'))
    builder.mkdir(package)
    manifest = {'name': name, 'version': version}
    manifest.update(fields)
    builder.write(package + ('package.json',), json.dumps(manifest, sort_keys=True))
    builder.write(package + ('index.js',), 'module.exports = {};\n')
    return package


def migrated_link(builder, link_path, target_path, absolute=False):
    Builder.link(builder, link_path, virtual(target_path) if absolute
                 else relative_link(link_path, target_path))


def store_dir_for(prefix, entry):
    return prefix + ('node_modules', '.pnpm', entry, 'node_modules')


# Store entry ids stay opaque, exactly like pnpm's names. Keys are the unscoped
# family member names used by the reviewed table, plus the bin-providing locked
# dependencies of each shape.
PI_ENTRIES = {'root': 'r', 'pi-agent-core': 'c', 'pi-ai': 'a', 'pi-codemode': 'd',
              'pi-mcp': 'm', 'pi-telemetry': 't', 'pi-tui': 'u', 'chord': 'h',
              'sdk': 's', 'yaml': 'y', 'marked': 'k', 'esbuild': 'e', 'typebox': 'b',
              'extra': 'x', 'ctxtwo': 'Z', 'second': 'Q'}
# Every family member is published under the same scope the archives use
# (`@earendil-works/chord` included).
PI_FAMILY_NAMES = ('pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp', 'pi-telemetry', 'pi-tui',
                   'chord')
PI_BIN_PACKAGES = {'sdk': BRIDGE_SDK, 'yaml': 'yaml', 'marked': 'marked', 'esbuild': 'esbuild',
                   'typebox': 'typebox'}
# The exact version each non-family dependency resolves to in the two reviewed
# graphs (from the authenticated archive manifests).
PI_BIN_SPECS = {'sdk': '0.129.0', 'yaml': '2.9.0', 'marked': '18.0.11', 'esbuild': '0.28.2',
                'typebox': '1.3.27'}
# Family packages that publish their own bin entry. `pi-ai`'s authenticated
# `dist/cli.js` is mode 0644 in the archive; pnpm's fix-bin makes the installed
# *link target* executable, which is why the oracle checks existence and the
# canonical target here rather than this mode.
PI_FAMILY_BINS = {'pi-ai': {'pi-ai': 'dist/cli.js'}}
# Which locked dependency provides each reviewed generated bin name.
PI_BIN_OWNER = {('pi-agent-core', 'pi-ai'): 'pi-ai', ('pi-agent-core', 'yaml'): 'yaml',
                ('pi-ai', 'anthropic-ai-sdk'): 'sdk', ('pi-tui', 'marked'): 'marked',
                ('chord', 'esbuild'): 'esbuild'}
# The dependency edges this fixture models. The archives also carry packages with
# no generated bin and no family membership (aws-sdk, genai, ...); they are
# deliberately out of the fixture because they cannot change a bins/shape
# decision, and every edge a reviewed table depends on is present.
PI_FIXTURE_DEPS = {
    '1.4.0': {'pi-agent-core': ['pi-ai', 'typebox'], 'pi-ai': ['sdk', 'pi-telemetry'],
              'pi-codemode': [], 'pi-mcp': [], 'pi-telemetry': [], 'pi-tui': ['marked']},
    '0.87.0': {'chord': ['esbuild'],
               'pi-agent-core': ['chord', 'pi-ai', 'pi-telemetry', 'typebox', 'yaml'],
               'pi-ai': ['sdk', 'pi-telemetry'], 'pi-telemetry': [], 'pi-tui': ['marked']},
}


class PiFixture:
    """A complete isolated Pi payload for one reviewed family shape."""

    def __init__(self, builder, version='1.4.0', absolute=False):
        self.builder = builder
        self.version = version
        self.absolute = absolute
        self.table = PI_FAMILY[version]

    def dir(self, entry, name=None):
        base = store_dir_for(PI_PREFIX, PI_ENTRIES[entry])
        return base + tuple(name.split('/')) if name else base

    def package_dir(self, key):
        if key in PI_FAMILY_NAMES:
            return self.dir(key) + tuple((PI_FAMILY_PREFIX + key).split('/'))
        return self.dir(key) + tuple(PI_BIN_PACKAGES[key].split('/'))

    def run(self):
        builder = self.builder
        version = self.version
        builder.write(PI_PREFIX + ('package.json',), json.dumps({
            'name': 'agent-vm-guest-pi', 'private': True, 'packageManager': 'pnpm@11.11.0',
            'dependencies': {PI_ROOT_PACKAGE: version}}, sort_keys=True))
        builder.mkdir(PI_PREFIX + ('node_modules', '.pnpm'))
        builder.write(PI_PREFIX + ('node_modules', '.modules.yaml'), 'nodeLinker: isolated\n')
        deps = PI_FIXTURE_DEPS[version]
        members = self.table['members']
        direct = self.table['direct']
        # Root-side dependencies that are not family members, per reviewed graph:
        # 1.4.0 publishes `yaml` (its root package's own .bin), 0.87.0 reaches
        # yaml through pi-agent-core instead.
        root_extra = ['yaml'] if version == '1.4.0' else []
        root_dependencies = {PI_FAMILY_PREFIX + short: version for short in direct}
        root_dependencies.update({PI_BIN_PACKAGES[dep]: PI_BIN_SPECS[dep] for dep in root_extra})
        root = migrated_entry(builder, self.dir('root'), PI_ROOT_PACKAGE, version, bin=PI_ROOT_BIN,
                              dependencies=root_dependencies)
        builder.write(root + ('dist', 'bundle', 'cli.js'),
                      '#!/usr/bin/env node\nprocess.exit(0);\n', mode=0o755)
        needed = set(root_extra)
        for short in members:
            edges = {}
            for dep in deps[short]:
                name = PI_FAMILY_PREFIX + dep if dep in PI_FAMILY_NAMES else PI_BIN_PACKAGES[dep]
                edges[name] = version if dep in PI_FAMILY_NAMES else PI_BIN_SPECS[dep]
                if dep not in PI_FAMILY_NAMES:
                    needed.add(dep)
            fields = {}
            if edges:
                fields['dependencies'] = edges
            if short in PI_FAMILY_BINS:
                fields['bin'] = PI_FAMILY_BINS[short]
            migrated_entry(builder, self.dir(short), PI_FAMILY_PREFIX + short, version, **fields)
            for target in PI_FAMILY_BINS.get(short, {}).values():
                builder.write(self.package_dir(short) + tuple(target.split('/')),
                              '#!/usr/bin/env node\nprocess.exit(0);\n')
        if 'sdk' in needed:
            migrated_entry(builder, self.dir('sdk'), BRIDGE_SDK, PI_BIN_SPECS['sdk'],
                           bin={'anthropic-ai-sdk': 'bin/cli'})
            builder.write(self.package_dir('sdk') + ('bin', 'cli'), '#!/usr/bin/env node\n',
                          mode=0o755)
        if 'yaml' in needed:
            # The published spelling, verbatim: a *string* bin whose target is
            # `./bin.mjs`. Both facts (string form, leading `./`) are npm
            # spellings of the same location and must be normalized, not refused.
            migrated_entry(builder, self.dir('yaml'), 'yaml', PI_BIN_SPECS['yaml'],
                           bin='./bin.mjs')
            builder.write(self.package_dir('yaml') + ('bin.mjs',), '#!/usr/bin/env node\n')
        if 'marked' in needed:
            migrated_entry(builder, self.dir('marked'), 'marked', PI_BIN_SPECS['marked'],
                           bin={'marked': 'bin/marked.js'})
            builder.write(self.package_dir('marked') + ('bin', 'marked.js'),
                          '#!/usr/bin/env node\n', mode=0o755)
        if 'typebox' in needed:
            migrated_entry(builder, self.dir('typebox'), 'typebox', PI_BIN_SPECS['typebox'])
        if 'esbuild' in needed:
            migrated_entry(builder, self.dir('esbuild'), 'esbuild', PI_BIN_SPECS['esbuild'],
                           bin={'esbuild': 'bin/esbuild'})
            builder.write(self.package_dir('esbuild') + ('bin', 'esbuild'),
                          '#!/usr/bin/env node\n', mode=0o755)
        for short in direct:
            self.link_dep('root', PI_FAMILY_PREFIX + short, short)
        for dep in root_extra:
            self.link_dep('root', PI_BIN_PACKAGES[dep], dep)
        for short in members:
            for dep in deps[short]:
                name = PI_FAMILY_PREFIX + dep if dep in PI_FAMILY_NAMES else PI_BIN_PACKAGES[dep]
                self.link_dep(short, name, dep)
        for short, entries in self.table['bins'].items():
            for name in entries:
                self.bin(short, name, PI_BIN_OWNER[(short, name)])
        migrated_link(builder, PI_PREFIX + ('node_modules',) + tuple(PI_ROOT_PACKAGE.split('/')),
                      root, self.absolute)
        builder.mkdir(PI_PREFIX + ('node_modules', '.bin'))
        migrated_link(builder, PI_PREFIX + PI_ROOT_BIN_LINK, root + ('dist', 'bundle', 'cli.js'),
                      self.absolute)
        builder.mkdir(PRODUCTION_COMMANDS['pi'][:-1])
        builder.write(PRODUCTION_COMMANDS['pi'],
                      '#!/bin/sh\necho warning >&2\nexec pi-real "$@"\n', mode=0o755)

    def link_dep(self, owner, name, entry):
        migrated_link(self.builder, self.dir(owner) + tuple(name.split('/')),
                      self.package_dir(entry), self.absolute)

    def bin(self, short, name, entry):
        manifest = json.loads(self.builder.path(self.package_dir(entry)
                                                + ('package.json',)).read_text())
        declared = manifest.get('bin')
        relative = declared if isinstance(declared, str) else (declared or {})[name]
        relative = [part for part in relative.split('/') if part != '.']
        migrated_link(self.builder, self.package_dir(short) + ('node_modules', '.bin', name),
                      self.package_dir(entry) + tuple(relative), self.absolute)


def build_pi_healthy(builder, version='1.4.0', absolute=False):
    PiFixture(builder, version, absolute).run()


def bridge_store(entry=None):
    base = BRIDGE_PREFIX + ('node_modules',)
    return base + tuple(entry.split('/')) if entry else base


BRIDGE_TREE = {
    'pi-claude-bridge': {'version': '0.8.0',
                         'dependencies': {BRIDGE_SDK: '0.3.281',
                                          '@modelcontextprotocol/sdk': '1.30.1',
                                          'cc-session-io': '0.4.0', 'change-case': '5.4.4'},
                         'peerDependencies': {PI_ROOT_PACKAGE: '>=0.85.0',
                                              '@earendil-works/pi-ai': '>=0.85.0',
                                              '@earendil-works/pi-tui': '>=0.85.0',
                                              'typebox': '*'}},
    BRIDGE_SDK: {'version': '0.3.281',
                 'dependencies': {}, 'peerDependencies': {'@anthropic-ai/sdk': '>=0.93.0',
                                                          '@modelcontextprotocol/sdk': '^1.29.0',
                                                          'zod': '^4.0.0'},
                 'optionalDependencies': {'@anthropic-ai/claude-agent-sdk-linux-x64': '0.3.281'}},
    '@modelcontextprotocol/sdk': {'version': '1.30.1', 'dependencies': {'zod': '4.6.5'}},
    'zod': {'version': '4.6.5'},
    'cc-session-io': {'version': '0.4.0'},
    'change-case': {'version': '5.4.4'},
}


def build_bridge_healthy(builder):
    builder.write(BRIDGE_PREFIX + ('package.json',), json.dumps({
        'name': 'agent-vm-guest-pi-packages', 'private': True, 'packageManager': 'pnpm@11.11.0',
        'dependencies': {BRIDGE_ROOT_PACKAGE: '0.8.0'}}, sort_keys=True))
    builder.mkdir(bridge_store())
    builder.write(bridge_store() + ('.modules.yaml',), 'nodeLinker: hoisted\n')
    for name, fields in BRIDGE_TREE.items():
        version = fields['version']
        extra = {key: value for key, value in fields.items() if key != 'version'}
        migrated_entry(builder, bridge_store(), name, version, **extra)
    builder.write(bridge_store('pi-claude-bridge') + ('src', 'index.ts'),
                  'export default function register() {\n  return 12;\n}\n')
    builder.write(bridge_store(BRIDGE_SDK) + ('sdk.mjs',), 'export const models = [];\n')


class MigratedFixtures:
    def __init__(self, tempfile):
        self.vector = 0

    def audit(self, directory, consumer, expect, platform='linux/arm64', inventories=None):
        return analyze_migrated(directory, PLATFORMS[platform], expect, consumer, inventories)

    def expect_pass(self, label, build, consumer, expect, check=None):
        with tempfile.TemporaryDirectory(prefix='migrated-pass-') as directory:
            build(Builder(directory))
            report = self.audit(directory, consumer, expect)
            assert report['consumer'] == consumer, report
            if check is not None:
                check(report)
        self.vector += 1
        print(f'layout self-test: {label} PASS')

    def expect_fail(self, label, build, consumer, expect, reason):
        with tempfile.TemporaryDirectory(prefix='migrated-fail-') as directory:
            build(Builder(directory))
            try:
                self.audit(directory, consumer, expect)
            except Mismatch as error:
                assert reason in str(error), (label, reason, str(error))
            else:
                raise AssertionError(f'{label}: mismatch was not detected')
        self.vector += 1
        print(f'layout self-test: {label} PASS')


def migrated_self_test():
    """Pi and bridge vectors: shape, context, bins, wrapper, hoisting, bytes.

    How the independent vectors are authored: the family/bins/wrapper/omission
    tables in this file were transcribed from the authenticated archive review
    (never read back from an installed tree), and the archive-inventory vectors
    below build the *expected* inventory from the archive fixture first and only
    then mutate the installed tree. That mirrors the harness, where the
    inventory comes from independently downloaded and SRI-checked tarballs
    before the install runs.
    """
    fixtures = MigratedFixtures(tempfile)

    def pi_check(report):
        assert report['version'] == '1.4.0', report
        assert report['familyShape'] == ['pi-agent-core', 'pi-ai', 'pi-codemode', 'pi-mcp',
                                        'pi-telemetry', 'pi-tui'], report
        assert report['bin']['entry'] == 'dist/bundle/cli.js', report
        assert report['bin']['link'] == '/opt/agent-vm/pi/node_modules/.bin/pi', report
        assert report['bin']['wrapper'] == '/usr/local/bin/pi', report
        assert report['bin']['target'].endswith('/dist/bundle/cli.js'), report
        assert sorted(report['generatedBins']) == ['pi-agent-core', 'pi-ai', 'pi-codemode',
                                                   'pi-mcp', 'pi-telemetry', 'pi-tui'], report
        assert sorted(report['generatedBins']['pi-agent-core']) == ['pi-ai'], report
        assert sorted(report['generatedBins']['pi-tui']) == ['marked'], report
        assert report['noPrivatePnpm'] is True, report

    fixtures.expect_pass('healthy isolated Pi (modern six, wrapper separate)',
                         lambda b: build_pi_healthy(b), 'pi', '1.4.0', pi_check)
    fixtures.expect_pass('healthy isolated Pi with absolute links',
                         lambda b: build_pi_healthy(b, absolute=True), 'pi', '1.4.0')

    def legacy_check(report):
        assert report['familyShape'] == ['chord', 'pi-agent-core', 'pi-ai', 'pi-telemetry',
                                        'pi-tui'], report
        assert sorted(report['generatedBins']['chord']) == ['esbuild'], report
        assert sorted(report['generatedBins']['pi-agent-core']) == ['pi-ai', 'yaml'], report

    fixtures.expect_pass('healthy isolated Pi (historical five, chord/esbuild)',
                         lambda b: build_pi_healthy(b, version='0.87.0'), 'pi', '0.87.0', legacy_check)

    def escaped_string_bin(builder):
        # A string bin is still a path: one that leaves the package is refused
        # rather than normalized into a neighbour's file.
        build_pi_healthy(builder, version='0.87.0')
        builder.rewrite_manifest(store_dir_for(PI_PREFIX, PI_ENTRIES['yaml']) + ('yaml',),
                                 lambda manifest: manifest.update({'bin': '../escape.mjs'}))

    fixtures.expect_fail('string bin target escapes its package', escaped_string_bin, 'pi', '0.87.0',
                         'bin yaml target', )

    def missing_member(builder):
        build_pi_healthy(builder)
        mcp = '@earendil-works/pi-mcp'
        builder.remove(store_dir_for(PI_PREFIX, PI_ENTRIES['pi-mcp']) + tuple(mcp.split('/')))
        builder.remove(store_dir_for(PI_PREFIX, PI_ENTRIES['root']) + tuple(mcp.split('/')))

    fixtures.expect_fail('missing family member', missing_member, 'pi', '1.4.0',
                         'installed instances; the reviewed family shape has exactly one')

    def second_context_of_a_member(builder):
        build_pi_healthy(builder)
        other = store_dir_for(PI_PREFIX, PI_ENTRIES['second'])
        migrated_entry(builder, other, '@earendil-works/pi-tui', '1.4.0')
        migrated_link(builder, store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                      + tuple('@earendil-works/pi-tui'.split('/')),
                      other + tuple('@earendil-works/pi-tui'.split('/')))

    fixtures.expect_fail('same-version member substituted in another context',
                         second_context_of_a_member, 'pi', '1.4.0',
                         '@earendil-works/pi-tui: 2 installed instances')

    def wrong_member_version(builder):
        build_pi_healthy(builder)
        builder.rewrite_manifest(store_dir_for(PI_PREFIX, PI_ENTRIES['pi-tui'])
                                 + tuple('@earendil-works/pi-tui'.split('/')),
                                 lambda m: m.update({'version': '1.3.9'}))

    fixtures.expect_fail('family member at the wrong version', wrong_member_version, 'pi', '1.4.0',
                         'is not the selected 1.4.0')

    def unreviewed_member(builder):
        build_pi_healthy(builder)
        migrated_entry(builder, store_dir_for(PI_PREFIX, PI_ENTRIES['extra']),
                       '@earendil-works/pi-extra', '1.4.0')
        migrated_link(builder, store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                      + tuple('@earendil-works/pi-extra'.split('/')),
                      store_dir_for(PI_PREFIX, PI_ENTRIES['extra'])
                      + tuple('@earendil-works/pi-extra'.split('/')))
        builder.rewrite_manifest(store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                                 + tuple(PI_ROOT_PACKAGE.split('/')),
                                 lambda m: m['dependencies'].update(
                                     {'@earendil-works/pi-extra': '1.4.0'}))

    fixtures.expect_fail('unreviewed family member', unreviewed_member, 'pi', '1.4.0',
                         'direct family edges')

    def family_member_not_reachable(builder):
        build_pi_healthy(builder)
        builder.remove(store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                       + tuple('@earendil-works/pi-mcp'.split('/')))
        builder.rewrite_manifest(store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                                 + tuple(PI_ROOT_PACKAGE.split('/')),
                                 lambda m: m['dependencies'].pop('@earendil-works/pi-mcp'))

    fixtures.expect_fail('family member installed but not linked from its edges',
                         family_member_not_reachable, 'pi', '1.4.0', 'direct family edges')

    def wrapper_is_link(builder):
        build_pi_healthy(builder)
        builder.remove(PRODUCTION_COMMANDS['pi'])
        builder.link(PRODUCTION_COMMANDS['pi'], '/opt/agent-vm/pi/node_modules/.bin/pi')

    fixtures.expect_fail('wrapper replaced by a link to the package entry', wrapper_is_link, 'pi',
                         '1.4.0', 'must be a regular file')

    def wrapper_absent(builder):
        build_pi_healthy(builder)
        builder.remove(PRODUCTION_COMMANDS['pi'])

    fixtures.expect_fail('missing warning wrapper', wrapper_absent, 'pi', '1.4.0',
                         'the Pi wrapper is missing')

    def root_bin_absent(builder):
        build_pi_healthy(builder)
        builder.remove(PI_PREFIX + PI_ROOT_BIN_LINK)

    fixtures.expect_fail('missing generated Pi bin link', root_bin_absent, 'pi', '1.4.0',
                         'the generated Pi bin link is missing')

    def root_bin_retargeted(builder):
        build_pi_healthy(builder)
        builder.remove(PI_PREFIX + PI_ROOT_BIN_LINK)
        builder.link(PI_PREFIX + PI_ROOT_BIN_LINK, '/usr/local/bin/pi')

    fixtures.expect_fail('Pi bin link retargeted to the wrapper', root_bin_retargeted, 'pi', '1.4.0',
                         'leaves the pi prefix')

    def wrong_root_bin_map(builder):
        build_pi_healthy(builder)
        builder.rewrite_manifest(store_dir_for(PI_PREFIX, PI_ENTRIES['root'])
                                 + tuple(PI_ROOT_PACKAGE.split('/')),
                                 lambda m: m.update({'bin': {'pi': 'dist/other.js'}}))

    fixtures.expect_fail('wrong Pi bin mapping', wrong_root_bin_map, 'pi', '1.4.0',
                         'is not the reviewed')

    def missing_generated_bin(builder):
        build_pi_healthy(builder)
        builder.remove(store_dir_for(PI_PREFIX, PI_ENTRIES['pi-ai'])
                       + tuple('@earendil-works/pi-ai'.split('/'))
                       + ('node_modules', '.bin', 'anthropic-ai-sdk'))

    fixtures.expect_fail('missing generated package-root bin', missing_generated_bin, 'pi', '1.4.0',
                         'missing generated bin anthropic-ai-sdk')

    def extra_generated_bin(builder):
        build_pi_healthy(builder)
        link = (store_dir_for(PI_PREFIX, PI_ENTRIES['pi-tui'])
                + tuple('@earendil-works/pi-tui'.split('/')) + ('node_modules', '.bin', 'rogue'))
        target = (store_dir_for(PI_PREFIX, PI_ENTRIES['marked'])
                  + tuple('marked/bin/marked.js'.split('/')))
        builder.link(link, relative_link(link, target))

    fixtures.expect_fail('extra generated package-root bin', extra_generated_bin, 'pi', '1.4.0',
                         'unexpected generated bin rogue')

    def wrong_context_bin(builder):
        build_pi_healthy(builder)
        other = store_dir_for(PI_PREFIX, PI_ENTRIES['ctxtwo'])
        migrated_entry(builder, other, BRIDGE_SDK, '0.129.0', bin={'anthropic-ai-sdk': 'cli.js'})
        builder.write(other + tuple(BRIDGE_SDK.split('/')) + ('cli.js',), '#!/usr/bin/env node\n',
                      mode=0o755)
        link = (store_dir_for(PI_PREFIX, PI_ENTRIES['pi-ai'])
                + tuple('@earendil-works/pi-ai'.split('/')) + ('node_modules', '.bin',
                                                               'anthropic-ai-sdk'))
        builder.remove(link)
        builder.link(link, relative_link(link, other + tuple(BRIDGE_SDK.split('/')) + ('cli.js',)))

    fixtures.expect_fail('generated bin pointing at a same-version other context',
                         wrong_context_bin, 'pi', '1.4.0', 'not the locked target')

    def isolated_without_store(builder):
        build_pi_healthy(builder)
        builder.remove(PI_PREFIX + ('node_modules', '.pnpm'))

    fixtures.expect_fail('isolated Pi without the private store', isolated_without_store, 'pi',
                         '1.4.0', 'missing from the extracted image')

    def second_dependency(builder):
        build_pi_healthy(builder)
        builder.rewrite_manifest(PI_PREFIX, lambda m: m['dependencies'].update({'extra': '1.0.0'}))

    fixtures.expect_fail('installed Pi project with a second dependency', second_dependency, 'pi',
                         '1.4.0', 'must declare only')

    fixtures.expect_fail('installed pnpm package in the Pi tree',
                         lambda b: (build_pi_healthy(b),
                                    migrated_entry(b, store_dir_for(PI_PREFIX, PI_ENTRIES['extra']),
                                                   'pnpm', '11.11.0')),
                         'pi', '1.4.0', 'second package manager')

    def stray(builder):
        build_pi_healthy(builder)
        migrated_entry(builder, store_dir_for(PI_PREFIX, PI_ENTRIES['extra']), 'stray', '1.0.0')

    fixtures.expect_fail('unreachable Pi payload', stray, 'pi', '1.4.0',
                         'not reachable from the reviewed root edges')

    def unknown_shape(builder):
        build_pi_healthy(builder)

    fixtures.expect_fail('selection with no reviewed family shape', unknown_shape, 'pi', '9.9.9',
                         'no reviewed Pi family shape covers 9.9.9')

    # -- archive inventories --------------------------------------------
    def inventory_for(directory):
        """Author the expected inventory from the *archive fixtures*, as the
        harness does from authenticated tarballs, then delete nothing from
        disk: only the data crosses into the comparison."""
        packages = {}
        identities = {'@earendil-works/pi-ai@1.4.0': ('pi-ai', '@earendil-works/pi-ai'),
                      '@earendil-works/pi-tui@1.4.0': ('pi-tui', '@earendil-works/pi-tui')}
        for identity, (entry, name) in identities.items():
            root = Path(directory).joinpath(*(store_dir_for(PI_PREFIX, PI_ENTRIES[entry])
                                              + tuple(name.split('/'))))
            entries = {}
            for path in sorted(root.rglob('*')):
                relative = path.relative_to(root).as_posix()
                if relative.split('/')[0] == 'node_modules':
                    # A tarball has no installed node_modules subtree; the
                    # generated links/directories are covered by generatedBins.
                    continue
                if path.is_symlink():
                    entries[relative] = {'kind': 'symlink', 'target': os.readlink(path)}
                elif path.is_dir():
                    entries[relative] = {'kind': 'dir'}
                else:
                    info = path.stat()
                    entries[relative] = {'kind': 'file', 'size': info.st_size,
                                         'sha256': hashlib.sha256(path.read_bytes()).hexdigest(),
                                         'executable': bool(info.st_mode & 0o111)}
            generated = sorted(path.name for path in root.joinpath('node_modules', '.bin').iterdir())
            packages[identity] = {'entries': entries, 'generatedBins': generated}
        return packages

    def build_with_inventory(builder):
        build_pi_healthy(builder)

    with tempfile.TemporaryDirectory(prefix='migrated-inventory-') as directory:
        build_pi_healthy(Builder(directory))
        inventories = inventory_for(directory)
    with tempfile.TemporaryDirectory(prefix='migrated-inventory-pass-') as directory:
        build_pi_healthy(Builder(directory))
        report = fixtures.audit(directory, 'pi', '1.4.0', inventories=inventories)
        assert report['archiveInventories'] == {'@earendil-works/pi-ai@1.4.0': len(
            inventories['@earendil-works/pi-ai@1.4.0']['entries']),
            '@earendil-works/pi-tui@1.4.0': len(
                inventories['@earendil-works/pi-tui@1.4.0']['entries'])}, report
    fixtures.vector += 1
    print('layout self-test: authenticated archive inventories match installed bytes PASS')

    def with_inventory_mutation_unused():
        pass

    # Each mutant is audited against the pristine inventory authored above.
    def inventory_case(label, mutation, reason):
        with tempfile.TemporaryDirectory(prefix='migrated-inventory-mutant-') as directory:
            builder = Builder(directory)
            build_pi_healthy(builder)
            mutation(builder)
            try:
                fixtures.audit(directory, 'pi', '1.4.0', inventories=inventories)
            except Mismatch as error:
                assert reason in str(error), (label, reason, str(error))
            else:
                raise AssertionError(f'{label}: inventory mismatch was not detected')
        fixtures.vector += 1
        print(f'layout self-test: {label} PASS')

    def byte_edit(builder):
        # Same length, different bytes: only a real content comparison can see it.
        path = store_dir_for(PI_PREFIX, PI_ENTRIES['pi-ai']) + tuple(
            '@earendil-works/pi-ai'.split('/')) + ('index.js',)
        builder.write(path, 'module.exports = {x};')

    inventory_case('installed byte edit versus the archive', byte_edit,
                   'bytes differ from the authenticated archive')

    def missing_file(builder):
        builder.remove(store_dir_for(PI_PREFIX, PI_ENTRIES['pi-tui'])
                       + tuple('@earendil-works/pi-tui'.split('/')) + ('index.js',))

    inventory_case('missing installed file versus the archive', missing_file,
                   'expected archive file is absent')

    def extra_file(builder):
        builder.write(store_dir_for(PI_PREFIX, PI_ENTRIES['pi-ai'])
                      + tuple('@earendil-works/pi-ai'.split('/')) + ('dist', 'shadow.js'),
                      'module.exports = {};\n')

    inventory_case('extra installed file versus the archive', extra_file,
                   'extra path not present in the authenticated archive')

    def mode_change(builder):
        package = store_dir_for(PI_PREFIX, PI_ENTRIES['pi-tui']) + tuple('@earendil-works/pi-tui'.split('/'))
        builder.path(package + ('index.js',)).chmod(0o755)

    inventory_case('mode change versus the archive', mode_change,
                   'executable bit differs from the archive')

    # -- bridge --------------------------------------------------------
    def bridge_check(report):
        assert report['consumer'] == 'bridge' and report['hoisted'] is True, report
        assert report['version'] == '0.8.0', report
        assert report['source'] == '/opt/agent-vm/pi-packages/node_modules/pi-claude-bridge/src/index.ts', report
        assert report['sdk']['path'] == '/opt/agent-vm/pi-packages/node_modules/@anthropic-ai/claude-agent-sdk', report
        assert report['optionalMissing'] >= 1, report

    fixtures.expect_pass('healthy hoisted bridge with the SDK JavaScript',
                         lambda b: build_bridge_healthy(b), 'bridge', '0.8.0', bridge_check)

    def bridge_linked(builder):
        build_bridge_healthy(builder)
        real = bridge_store() + ('.store', 'pi-claude-bridge')
        migrated_entry(builder, bridge_store() + ('.store',), 'pi-claude-bridge', '0.8.0',
                       **{key: value for key, value in BRIDGE_TREE['pi-claude-bridge'].items()
                          if key != 'version'})
        builder.write(real + ('src', 'index.ts'), 'export default function register() {}\n')
        builder.remove(bridge_store('pi-claude-bridge'))
        builder.link(bridge_store('pi-claude-bridge'), '/opt/agent-vm/pi-packages/node_modules/.store/pi-claude-bridge')

    fixtures.expect_fail('bridge installed through a link', bridge_linked, 'bridge', '0.8.0',
                         'a hoisted bridge install carries no package links')

    def bridge_private_store(builder):
        build_bridge_healthy(builder)
        builder.mkdir(bridge_store() + ('.pnpm', 'x', 'node_modules'))

    fixtures.expect_fail('bridge with private-store payload', bridge_private_store, 'bridge',
                         '0.8.0', 'carries no private store payload')

    def bridge_metadata_store(builder):
        """A metadata-only store is real pnpm 11.11.0 hoisted output.

        The pinned manager writes `node_modules/.pnpm/lock.yaml` even with
        `nodeLinker: hoisted` (observed by the focused harness' first real cold
        install), so a store *file* must not be mistaken for store *payload*.
        """
        build_bridge_healthy(builder)
        builder.write(bridge_store() + ('.pnpm', 'lock.yaml'), 'lockfileVersion: 9.0\n')

    fixtures.expect_pass('bridge with a metadata-only store', bridge_metadata_store, 'bridge',
                         '0.8.0')

    def bridge_second_pi(builder):
        build_bridge_healthy(builder)
        migrated_entry(builder, bridge_store(), '@earendil-works/pi-ai', '1.4.0')

    fixtures.expect_fail('second Pi package in the bridge tree', bridge_second_pi, 'bridge',
                         '0.8.0', 'installs a second Pi/typebox package')

    def bridge_typebox(builder):
        build_bridge_healthy(builder)
        migrated_entry(builder, bridge_store(), 'typebox', '1.0.4')

    fixtures.expect_fail('typebox installed in the bridge tree', bridge_typebox, 'bridge', '0.8.0',
                         'installs a second Pi/typebox package')

    def bridge_native_payload(builder):
        build_bridge_healthy(builder)
        migrated_entry(builder, bridge_store(), '@anthropic-ai/claude-agent-sdk-linux-x64', '0.3.281')

    fixtures.expect_fail('SDK platform payload in the bridge tree', bridge_native_payload, 'bridge',
                         '0.8.0', 'SDK platform payload the reviewed workspace omits')

    def bridge_native_file(builder):
        build_bridge_healthy(builder)
        builder.write(bridge_store(BRIDGE_SDK) + ('vendor', 'claude-agent-sdk-linux-arm64', 'cli.js'),
                      'module.exports = {};\n')

    fixtures.expect_fail('SDK platform payload file in the bridge tree', bridge_native_file,
                         'bridge', '0.8.0', 'SDK platform payload present in the bridge tree')

    def bridge_sdk_absent(builder):
        build_bridge_healthy(builder)
        builder.remove(bridge_store(BRIDGE_SDK))

    fixtures.expect_fail('bridge without the SDK JavaScript package', bridge_sdk_absent, 'bridge',
                         '0.8.0', 'declared dependency @anthropic-ai/claude-agent-sdk is not installed')

    def bridge_sdk_js_absent(builder):
        build_bridge_healthy(builder)
        builder.remove(bridge_store(BRIDGE_SDK) + ('sdk.mjs',))
        builder.remove(bridge_store(BRIDGE_SDK) + ('index.js',))

    fixtures.expect_fail('SDK package with no JavaScript module', bridge_sdk_js_absent, 'bridge',
                         '0.8.0', 'the SDK ships no JavaScript module')

    def bridge_source_absent(builder):
        build_bridge_healthy(builder)
        builder.remove(bridge_store('pi-claude-bridge') + ('src', 'index.ts'))

    fixtures.expect_fail('bridge TypeScript entry missing', bridge_source_absent, 'bridge', '0.8.0',
                         'the bridge TypeScript entry is missing')

    def bridge_unreviewed_omission(builder):
        build_bridge_healthy(builder)
        builder.rewrite_manifest(bridge_store('change-case'),
                                 lambda m: m.update({'optionalDependencies': {'fsevents': '2.3.3'}}))

    fixtures.expect_fail('optional dependency omitted without a reviewed rule',
                         bridge_unreviewed_omission, 'bridge', '0.8.0',
                         'optional dependency fsevents is absent')

    def bridge_stray(builder):
        build_bridge_healthy(builder)
        migrated_entry(builder, bridge_store(), 'stray', '1.0.0')

    fixtures.expect_fail('unreachable bridge payload', bridge_stray, 'bridge', '0.8.0',
                         'not reachable from the reviewed root edges')

    fixtures.expect_pass('bridge peer text does not install a second Pi',
                         lambda b: (build_bridge_healthy(b), b.rewrite_manifest(
                             bridge_store('pi-claude-bridge'),
                             lambda m: m.update({'peerDependenciesMeta': {
                                 '@earendil-works/pi-ai': {'optional': True}}}))),
                         'bridge', '0.8.0', bridge_check)

    # CLI contract for the migrated consumers: one JSON line on success, a single
    # reason and no stdout on mismatch, usage 2 for an unusable inventory file.
    import subprocess
    import sys as _sys
    script = str(Path(__file__).resolve().parent / 'standard-node-layout.py')

    def cli(*args):
        return subprocess.run([_sys.executable, '-B', script, *args], capture_output=True,
                              text=True, timeout=300)

    with tempfile.TemporaryDirectory(prefix='migrated-cli-') as directory:
        build_pi_healthy(Builder(directory))
        ok = cli('--consumer', 'pi', '--root', directory, '--platform', 'linux/arm64',
                 '--expect-version', '1.4.0')
        assert ok.returncode == 0, (ok.returncode, ok.stderr)
        lines = [line for line in ok.stdout.splitlines() if line.strip()]
        assert len(lines) == 1 and json.loads(lines[0])['consumer'] == 'pi', ok.stdout
        bad = cli('--consumer', 'pi', '--root', directory, '--platform', 'linux/arm64',
                  '--expect-version', '0.87.0')
        assert bad.returncode == 1 and bad.stdout == '', (bad.returncode, bad.stdout)
        assert 'standard-node-layout: pi:' in bad.stderr, bad.stderr
        inventory = Path(directory).parent / (Path(directory).name + '-inventory.json')
        inventory.write_text(json.dumps({'consumer': 'pi', 'version': '0.87.0', 'packages': {
            'yaml@2.9.0': {'entries': {'package.json': {'kind': 'dir'}}}}}))
        mismatched = cli('--consumer', 'pi', '--root', directory, '--platform', 'linux/arm64',
                         '--expect-version', '1.4.0', '--archive-inventories', str(inventory))
        assert mismatched.returncode == 2, (mismatched.returncode, mismatched.stderr)
        assert 'is for 0.87.0, not 1.4.0' in mismatched.stderr, mismatched.stderr
        inventory.write_text('{"packages": {"x@1": {"entries": {"a": {"kind": "file"}}}}}')
        invalid = cli('--consumer', 'pi', '--root', directory, '--platform', 'linux/arm64',
                      '--archive-inventories', str(inventory))
        assert invalid.returncode == 2, (invalid.returncode, invalid.stderr)
        assert 'needs size/sha256/executable' in invalid.stderr, invalid.stderr
        inventory.unlink()
        for consumer in ('pi', 'bridge'):
            result = cli('--consumer', consumer, '--root', directory, '--platform', 'linux/arm64',
                         '--archive-inventories', '/nonexistent/inventory.json')
            assert result.returncode == 2, (consumer, result.returncode, result.stderr)
        dsh_inventory = cli('--consumer', 'dsh', '--root', directory, '--platform', 'linux/arm64',
                            '--archive-inventories', '/nonexistent/inventory.json')
        assert dsh_inventory.returncode == 2, dsh_inventory.stderr
    fixtures.vector += 1
    print('layout self-test: migrated CLI exit contract PASS')

    print(f'standard-node-layout: migrated self-test PASS ({fixtures.vector} vectors)')
    return fixtures.vector


def self_test():
    import subprocess

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
                 ['--consumer', 'copilot', '--root', '/tmp', '--platform', 'linux/arm64'],
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

    migrated = migrated_self_test()
    print(f'standard-node-layout: self-test PASS ({vector} dsh + {migrated} migrated vectors, '
          'virtual roots only)')
    return 0


MAX_INVENTORY_BYTES = 32 << 20
INVENTORY_KINDS = ('file', 'dir', 'symlink')
HEX_DIGEST = re.compile(r'^[0-9a-f]{64}$')


def load_inventories(path, consumer, expect_version):
    """Read the independently authored archive inventories.

    The file is *evidence supplied from outside this run*: the harness downloads
    each selected tarball, checks its SRI, extracts it, and records the per-path
    kind/size/SHA256/symlink/executable inventory. Nothing here may be produced
    by the production checker or read back from an installed tree, otherwise the
    comparison would only prove that two copies of one assumption agree.

    Shape (unknown keys are hard errors so a typo cannot silently narrow it):

        {"consumer": "pi",            # optional, must equal --consumer
         "version": "1.4.0",          # optional, must equal the selection
         "packages": {"name@version": {
            "entries": {"dist/cli.js": {"kind": "file", "size": 12,
                                        "sha256": "<64 hex>", "executable": true},
                        "node_modules": {"kind": "dir"},
                        "bin/x": {"kind": "symlink", "target": "../cli.js"}},
            "generatedBins": ["x"]}}}
    """
    source = Path(path)
    try:
        if source.stat().st_size > MAX_INVENTORY_BYTES:
            raise Usage(f'--archive-inventories is larger than {MAX_INVENTORY_BYTES} bytes')
        data = json.loads(source.read_text())
    except OSError as error:
        raise Usage(f'cannot read --archive-inventories: {error}') from None
    except ValueError as error:
        raise Usage(f'--archive-inventories is not valid JSON: {error}') from None
    if not isinstance(data, dict) or set(data) - {'consumer', 'version', 'packages'}:
        raise Usage('--archive-inventories must be an object with consumer/version/packages')
    if data.get('consumer') not in (None, consumer):
        raise Usage(f'--archive-inventories is for {data.get("consumer")}, not {consumer}')
    if data.get('version') not in (None, expect_version):
        raise Usage(f'--archive-inventories is for {data.get("version")}, not {expect_version}')
    packages = data.get('packages')
    if not isinstance(packages, dict) or not packages:
        raise Usage('--archive-inventories.packages must be a nonempty object')
    for identity, package in packages.items():
        if '@' not in identity or identity.startswith('@') and identity.count('@') < 2:
            raise Usage(f'archive inventory key {identity!r} is not name@version')
        if not isinstance(package, dict) or set(package) - {'entries', 'generatedBins'}:
            raise Usage(f'archive inventory {identity}: unexpected keys')
        entries = package.get('entries')
        if not isinstance(entries, dict) or not entries:
            raise Usage(f'archive inventory {identity}: entries must be a nonempty object')
        for relative, entry in entries.items():
            parts = relative.split('/')
            if relative.startswith('/') or any(part in ('', '.', '..') for part in parts):
                raise Usage(f'archive inventory {identity}: {relative!r} is not a relative path')
            if not isinstance(entry, dict) or entry.get('kind') not in INVENTORY_KINDS:
                raise Usage(f'archive inventory {identity}: {relative} has no usable kind')
            if entry['kind'] == 'file':
                if set(entry) != {'kind', 'size', 'sha256', 'executable'}:
                    raise Usage(f'archive inventory {identity}: {relative} needs size/sha256/'
                                'executable')
                if (not isinstance(entry['size'], int) or entry['size'] < 0
                        or not isinstance(entry['sha256'], str)
                        or not HEX_DIGEST.match(entry['sha256'])):
                    raise Usage(f'archive inventory {identity}: {relative} has an invalid size/hash')
                if not isinstance(entry['executable'], bool):
                    raise Usage(f'archive inventory {identity}: {relative} needs an executable flag')
            elif entry['kind'] == 'symlink':
                if set(entry) != {'kind', 'target'} or not isinstance(entry['target'], str) \
                        or not entry['target']:
                    raise Usage(f'archive inventory {identity}: {relative} needs a link target')
            elif set(entry) != {'kind'}:
                raise Usage(f'archive inventory {identity}: {relative} is not a plain directory')
        generated = package.get('generatedBins', [])
        if not isinstance(generated, list) or not all(isinstance(name, str) and name
                                                      for name in generated):
            raise Usage(f'archive inventory {identity}: generatedBins must be a list of names')
    return packages


def parse_arguments(args):
    if args == ['--self-test']:
        return {'self_test': True}
    if any(arg == '--self-test' for arg in args):
        raise Usage(USAGE)
    values = {}
    rest = list(args)
    while rest:
        flag = rest.pop(0)
        if flag not in ('--consumer', '--root', '--platform', '--expect-version',
                        '--archive-inventories'):
            raise Usage(f'unknown option: {flag}\n{USAGE}')
        if not rest or rest[0].startswith('--'):
            raise Usage(f'missing value for {flag}\n{USAGE}')
        value = rest.pop(0)
        if flag in values:
            raise Usage(f'duplicate option: {flag}\n{USAGE}')
        values[flag] = value
    if '--consumer' not in values:
        raise Usage(f'missing --consumer\n{USAGE}')
    consumer = values['--consumer']
    if consumer not in PRODUCTION_PREFIXES:
        raise Usage(f'unsupported consumer: {consumer}\n{USAGE}')
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
            expect = json.loads(SOURCE_MANIFESTS[consumer].read_text())[
                'dependencies'][SOURCE_DEPENDENCIES[consumer]]
        except (OSError, ValueError, KeyError, TypeError) as error:
            raise Usage(f'cannot read the source selection: {error}') from None
        if not isinstance(expect, str) or not EXACT_VERSION.match(expect):
            raise Usage(f'source manifest does not pin an exact {consumer} version')
    elif not EXACT_VERSION.match(expect):
        raise Usage(f'--expect-version must be an exact version: {expect}')
    inventories = None
    if '--archive-inventories' in values:
        inventories = load_inventories(values['--archive-inventories'], consumer, expect)
    return {'consumer': consumer, 'root': root, 'platform': PLATFORMS[values['--platform']],
            'expect_version': expect, 'inventories': inventories}


def main(args):
    try:
        options = parse_arguments(args)
    except Usage as error:
        print(f'standard-node-layout: {error}', file=sys.stderr)
        return 2
    if options.get('self_test'):
        return self_test()
    try:
        if options['consumer'] == 'dsh':
            if options['inventories'] is not None:
                raise Usage('--archive-inventories is only defined for the migrated consumers')
            report = analyze(options['root'], options['platform'], options['expect_version'])
        else:
            report = analyze_migrated(options['root'], options['platform'], options['expect_version'],
                                      options['consumer'], options['inventories'])
    except Usage as error:
        print(f'standard-node-layout: {error}', file=sys.stderr)
        return 2
    except (Mismatch, OSError) as error:
        print(f'standard-node-layout: {options["consumer"]}: {error}', file=sys.stderr)
        return 1
    print(json.dumps(report, sort_keys=True, separators=(',', ':')))
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
