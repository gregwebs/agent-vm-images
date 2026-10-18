#!/usr/bin/env python3
"""Dsh filesystem capabilities and handled-failure publication, not a lock parser."""
import argparse
from dataclasses import dataclass, field
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
from typing import Literal

INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
PRODUCTION = Path('/opt/agent-vm/dsh')
# The build mount keeps the shared recipe-contract helpers outside the payload.
PRODUCTION_CONTRACT = Path('/tmp/recipe-contract')
# The installed base manager is addressed by absolute path, never through PATH.
PNPM_COMMANDS = Path('/usr/local/bin')
PNPM_PREFIX = Path('/opt/agent-vm/pnpm')
PNPM_NODE = Path('/usr/bin/node')
RUNTIME_ENV = ('NODE_OPTIONS', 'NODE_PATH', 'BUN_OPTIONS')
TLS_ENV = ('NODE_EXTRA_CA_CERTS', 'SSL_CERT_FILE', 'SSL_CERT_DIR')
SIGNALS = (signal.SIGINT, signal.SIGTERM, signal.SIGHUP)


@dataclass(frozen=True)
class Authorization:
    prefix: Path
    root: Path
    basePin: Path
    production: bool
    command: Path | None = None
    status_dir: Path | None = None

    def json(self):
        return {'prefix': str(self.prefix), 'root': str(self.root),
                'basePin': str(self.basePin), 'production': self.production}


@dataclass(frozen=True)
class MutationAuthorization(Authorization):
    mutation: Literal[True] = field(default=True, init=False)


@dataclass(frozen=True)
class RuntimeAuthorization(Authorization):
    mutation: Literal[False] = field(default=False, init=False)


class PublicationFailure(Exception):
    def __init__(self, status: int, recovery: Path, cause: Exception):
        self.status = status
        self.recovery = recovery
        detail = f'; recovery root: {recovery}' if status == 3 or recovery.exists() else ''
        super().__init__(f'publication failed: {cause}; status {status}{detail}')


class SignalFailure(BaseException):
    """Publication interrupted by a signal; handled like any other failure."""

    def __init__(self, signum):
        super().__init__(f'interrupted by signal {signum}')
        self.signum = signum


class ContractFailure(ValueError):
    pass


def checkout_root():
    """Root of the source checkout this module belongs to.

    Derived from this file, never from the environment or the caller's prefix, so
    a copied repository owns its own contract directory and no caller value can
    move executable policy.
    """
    return Path(__file__).resolve().parents[2]


def contract_dir():
    """Directory of the shared recipe-contract helpers.

    The build passes the read-only mount as AGENT_VM_CONTRACT_DIR; a host
    checkout keeps the same directory beside this module. There is deliberately
    no search path: a bogus caller value fails instead of silently selecting a
    different parser or environment checker.
    """
    value = os.environ.get('AGENT_VM_CONTRACT_DIR')
    if value:
        return absolute(value)
    return checkout_root() / 'recipe-contract'


def authorize_contract(prefixes=()):
    """Authorize the directory whose helpers this invocation may execute.

    That directory selects executable policy, so it is authorized before any
    command is spawned from it. Two legitimate shapes exist: a production prefix
    uses the read-only build mount, a fixture names a directory inside its own
    invocation-owned root, and a host checkout names its own tracked
    `images/recipe-contract` (derived from this module's location). A host
    checkout keeps its scratch prefixes inside the authorized root of the same
    invocation, so a claimed destination outside every authorized root fails.
    """
    directory = contract_dir()
    projects = [absolute(prefix) for prefix in prefixes]
    root = None
    if PRODUCTION in projects:
        if directory != PRODUCTION_CONTRACT:
            raise ContractFailure('production contract directory is fixed')
        no_redirect(directory)
    elif directory == checkout_root() / 'recipe-contract':
        no_redirect(directory)
        for project in projects:
            no_redirect(project)
            if checkout_root() in project.parents:
                continue
            # A developer upgrade stages outside the checkout; that prefix is
            # authorized by the invocation-owned scratch root it was created in.
            root = root or authorized_root()
            no_redirect(project, root)
    else:
        root = authorized_root()
        no_redirect(directory)
        below(directory, root)
        for project in projects:
            no_redirect(project, root)
    if not directory.is_dir() or not (directory / 'pnpm-lock.mjs').is_file():
        raise ContractFailure('contract directory has no graph module: ' + str(directory))
    regular(directory / 'pnpm-lock.mjs')
    return directory


def guard_environment(prefixes=()):
    # Python is the pre-Node boundary: a Node script cannot reject its own preload.
    for key in RUNTIME_ENV:
        if os.environ.get(key):
            raise ValueError('unsupported manager runtime override: ' + key)
    # dsh selects the fixed Linux public-CA bundle, so caller CA/TLS injection is
    # refused here rather than in the shared checker: the OpenCode strict-egress
    # profile legitimately supplies NODE_EXTRA_CA_CERTS to its own manager.
    for key in TLS_ENV:
        if os.environ.get(key):
            raise ValueError('unsupported manager TLS override: ' + key)
    if os.environ.get('NODE_TLS_REJECT_UNAUTHORIZED') == '0':
        raise ValueError('TLS verification cannot be disabled')
    # The checker is executable policy from the contract directory, so the
    # directory is authorized before anything in it is spawned.
    directory = authorize_contract(prefixes)
    checker = directory / 'check-pnpm-environment.mjs'
    if not checker.is_file():
        raise ValueError('environment checker missing: ' + str(checker))
    result = subprocess.run(['node', str(checker), *(str(prefix) for prefix in prefixes)],
                            capture_output=True, text=True, timeout=30)
    if result.returncode != 0:
        raise ValueError(result.stderr.strip() or result.stdout.strip()
                         or 'package-manager environment rejected')


def absolute(value):
    path = Path(value)
    if not path.is_absolute() or '..' in path.parts or '.' in path.parts:
        raise ValueError('absolute lexical path required: ' + str(path))
    return path


def below(path, root):
    if path == root or root not in path.parents:
        raise ValueError('path must be a strict child of authorized root: ' + str(path))


def no_redirect(path, root=None, leaf_link=False):
    # Checking only resolve() loses transient symlink waypoints.
    for item in reversed((path, *path.parents)):
        if item.is_symlink() and not (leaf_link and item == path):
            raise ValueError('symlink waypoint: ' + str(item))
    if root is not None:
        below(path, root)
        below(path.resolve(strict=False), root)
    if leaf_link and path.is_symlink():
        path.resolve(strict=True)


def runtime_target(command, prefix):
    if not command.is_symlink():
        target = command.resolve(strict=True)
        below(target, prefix)
        return target
    raw = Path(os.readlink(command))
    current = Path('/') if raw.is_absolute() else command.parent
    pending = list(raw.parts[1:] if raw.is_absolute() else raw.parts)
    entered = current == prefix or prefix in current.parents
    hops = 0
    while pending:
        part = pending.pop(0)
        candidate = current.parent if part == '..' else current / part
        inside = candidate == prefix or prefix in candidate.parents
        if not inside and (entered or candidate not in prefix.parents):
            raise ValueError('runtime command symlink waypoint escapes payload: ' + str(candidate))
        if candidate.is_symlink():
            hops += 1
            if hops > 64:
                raise ValueError('runtime command symlink cycle')
            target = Path(os.readlink(candidate))
            if target.is_absolute():
                # An absolute payload link restarts inside the payload, never in
                # the host root. Keep .. segments for the next waypoint check.
                below(target, prefix)
                pending = list(target.relative_to(prefix).parts) + pending
                current = prefix
                entered = True
            else:
                pending = list(target.parts) + pending
        else:
            candidate.lstat()
            current = candidate
            entered = entered or inside
    below(current, prefix)
    return current


def regular(path):
    if not stat.S_ISREG(path.lstat().st_mode):
        raise ValueError('regular nonsymlink file required: ' + str(path))


def disjoint(paths):
    for index, first in enumerate(paths):
        for second in paths[index + 1:]:
            if first == second or first in second.parents or second in first.parents:
                raise ValueError(f'path collision: {first}, {second}')


def authorized_root(runtime=False):
    """The invocation-owned root that authorizes a non-production prefix.

    A fixture harness names it AGENT_VM_FIXTURE_ROOT because it also supplies a
    fake manager. A developer upgrade of a host checkout has no fixture in play,
    so it names the same capability AGENT_VM_SCRATCH_ROOT: claiming a fixture
    root it does not have would put the checkout's own contract directory outside
    the root that authorized the staged prefixes. Exactly one spelling may be
    used so the authorized root is never ambiguous.
    """
    fixture = os.environ.get('AGENT_VM_FIXTURE_ROOT')
    scratch = os.environ.get('AGENT_VM_SCRATCH_ROOT')
    if fixture and scratch:
        raise ValueError('ambiguous authorized root: fixture and scratch are both set')
    value = fixture or scratch
    if not value:
        raise ValueError('AGENT_VM_FIXTURE_ROOT or AGENT_VM_SCRATCH_ROOT required')
    root = absolute(value)
    no_redirect(root)
    if root == Path('/') or not root.is_dir():
        raise ValueError('invalid authorized root')
    # A fixture root is docker cp'd into the fixture container, which preserves
    # the creating host UID; the probe container has no capabilities to own it.
    # Its confinement therefore rests on the lexical/no-redirect rules and the
    # ownership of everything created below it. A developer scratch root is
    # created by this invocation on this host, so it is owned here and must say so.
    if not fixture:
        allowed = (0, os.getuid()) if runtime else (os.getuid(),)
        if root.stat().st_uid not in allowed:
            raise ValueError('authorized root must be invocation-owned')
    return root


def _authorize(prefix, command=None, status_dir=None, runtime=False):
    prefix = absolute(prefix)
    production = prefix == PRODUCTION
    if production:
        if not runtime and os.getuid() != 0:
            raise ValueError('production mutation requires root')
        root, pin = Path('/opt/agent-vm'), Path('/opt/agent-vm/pnpm/pin.json')
        if command is not None and absolute(command) != Path('/usr/local/bin/dsh'):
            raise ValueError('production command must be /usr/local/bin/dsh')
        if status_dir is not None and absolute(status_dir) != Path('/opt/agent-vm/install-status'):
            raise ValueError('production status directory is fixed')
        status_dir = None if runtime else Path('/opt/agent-vm/install-status')
    else:
        root = authorized_root(runtime)
        below(prefix, root)
        pin = root / 'pnpm/pin.json'
        status_dir = None if runtime else absolute(status_dir or root / 'install-status')
    no_redirect(prefix, None if production else root)
    if not prefix.is_dir():
        raise ValueError('prefix must exist')
    owner = prefix.stat().st_uid
    if owner not in ((0, os.getuid()) if runtime else (os.getuid(),)):
        raise ValueError('prefix ownership mismatch')
    for filename in INPUTS:
        item = prefix / filename
        no_redirect(item, None if production else root)
        regular(item)
    no_redirect(pin, None if production else root)
    regular(pin)
    nodes = prefix / 'node_modules'
    if os.path.lexists(nodes):
        no_redirect(nodes, None if production else root)
        if not nodes.is_dir():
            raise ValueError('node_modules must be a directory')
    command = absolute(command) if command is not None else None
    if command is not None:
        no_redirect(command, None if production else root, leaf_link=True)
        if not runtime and os.path.lexists(command):
            if not command.is_symlink():
                raise ValueError('existing command destination must be a payload link')
            runtime_target(command, prefix)
        if runtime:
            target = runtime_target(command, prefix)
            if not target.is_file() or not os.access(command, os.R_OK | os.X_OK):
                raise ValueError('runtime command must be readable and executable')
    if status_dir is not None:
        no_redirect(status_dir, None if production else root)
        if status_dir.exists() and not status_dir.is_dir():
            raise ValueError('status destination must be a directory')
    # A runtime command *is* payload (the linked bin), so only mutation
    # destinations must be disjoint from the prefix and each other.
    disjoint([prefix, pin, *([command] if command and not runtime else []),
              *([status_dir] if status_dir else [])])
    if runtime:
        # Read-only consumers must never acquire a status-writing capability.
        for item in (prefix, *(prefix / filename for filename in INPUTS)):
            required = 0o005 if item.is_dir() else 0o004
            if item.stat().st_mode & required != required:
                raise ValueError('runtime payload must be world-readable a+rX')
    capability = RuntimeAuthorization if runtime else MutationAuthorization
    return capability(prefix, root, pin, production, command, status_dir)


def authorize_mutation(prefix, command=None, status_dir=None) -> MutationAuthorization:
    return _authorize(prefix, command, status_dir)


def authorize_runtime(prefix, command, scratch_root, *, status_dir=None, mutation=False) -> RuntimeAuthorization:
    if status_dir is not None or mutation or command is None:
        raise ValueError('runtime authorization requires a command and cannot mutate payload/status')
    capability = _authorize(prefix, command, runtime=True)
    home = absolute(os.environ.get('HOME', ''))
    no_redirect(home)
    scratch_root = absolute(scratch_root)
    no_redirect(scratch_root)
    if scratch_root != home:
        below(scratch_root, home)
    for item in (home, scratch_root):
        if not item.is_dir() or item.stat().st_uid != os.getuid():
            raise ValueError('runtime scratch/HOME must be owned by calling UID')
    disjoint([scratch_root, capability.prefix, capability.basePin,
              capability.root / 'install-status'])
    return capability


def authorize(prefix, **options):
    return authorize_mutation(prefix, **options)


# Only this process's registered directories may be recursively removed.
_scratch: dict[Path, tuple[int, int]] = {}


def owned_scratch(capability, parent=None):
    if capability.mutation:
        parent = absolute(parent or (Path('/tmp') if capability.production else capability.root))
        no_redirect(parent)
        if not capability.production:
            if parent != capability.root:
                below(parent, capability.root)
        elif parent != Path('/tmp'):
            raise ValueError('production scratch parent is fixed')
    else:
        parent = absolute(parent or os.environ['HOME'])
        authorize_runtime(capability.prefix, capability.command, parent)
    if not parent.is_dir() or parent.stat().st_uid != os.getuid():
        # /tmp is shared, but mkdtemp below creates the owned private boundary.
        if not (capability.production and capability.mutation and parent == Path('/tmp')):
            raise ValueError('scratch parent must be invocation-owned')
    if parent != capability.root:
        disjoint([parent, capability.prefix, capability.basePin,
                  *([capability.status_dir] if capability.status_dir else []),
                  *([capability.command] if capability.command and capability.mutation else [])])
    scratch = Path(tempfile.mkdtemp(prefix='dsh-', dir=parent)).resolve()
    info = scratch.stat()
    _scratch[scratch] = (info.st_dev, info.st_ino)
    return scratch


def _refuse_escaping_links(directory, allowed=()):
    """Refuse scratch links whose target leaves the authorized destinations.

    rmtree unlinks symlinks instead of following them, so this bounds a future
    cleanup implementation rather than the deletion itself. Two legitimate
    targets exist besides the scratch itself: the scratch parent (a pnpm store
    records the installed project with a relative index link) and the installed
    prefix (a real `dsh` profile links its shared payload packages). Neither is
    followed here, and anything outside those destinations is still refused.
    """
    parent = directory.parent
    for current, dirs, files in os.walk(directory, followlinks=False):
        for name in (*dirs, *files):
            item = Path(current) / name
            if not item.is_symlink():
                continue
            target = item.resolve(strict=False)
            if target == directory or directory in target.parents:
                continue
            # A pin may name the destination itself (a pnpm project index link
            # points at the prefix root), so equality counts, not only nesting.
            if (target != parent and parent not in target.parents
                    and not any(target == root or root in target.parents for root in allowed)):
                raise ValueError('symlink inside scratch escapes its authorized destinations: ' + str(item))


def cleanup_scratch(scratch, allowed=()):
    scratch = absolute(scratch)
    identity = _scratch.get(scratch)
    if identity is None:
        raise ValueError('scratch not registered by this invocation')
    no_redirect(scratch)
    info = scratch.lstat()
    if not stat.S_ISDIR(info.st_mode) or (info.st_dev, info.st_ino) != identity or info.st_uid != os.getuid():
        raise ValueError('scratch identity changed')
    _refuse_escaping_links(scratch, allowed)
    shutil.rmtree(scratch)
    del _scratch[scratch]


def publish(staged_prefix, prefix, *, replace=os.replace):
    target = authorize_mutation(prefix)
    stage = absolute(staged_prefix)
    if target.production:
        no_redirect(stage)
        # Production stages are private invocation directories, not an ambient
        # fixture override or a second source-maintenance prefix.
        parent = stage.parent
        if (parent.parent != Path('/tmp') or not parent.name.startswith('dsh-')
                or parent.stat().st_uid != os.getuid()
                or stat.S_IMODE(parent.stat().st_mode) != 0o700
                or stage.stat().st_uid != os.getuid()):
            raise ValueError('production stage must be in private owned /tmp/dsh-* scratch')
        for filename in INPUTS:
            regular(stage / filename)
    else:
        stage = authorize_mutation(stage).prefix
    disjoint([target.prefix, stage])
    recovery = owned_scratch(target)
    inventory = []
    attempted = []
    saved = []
    cleanup_started = False

    def interrupt(signum, frame):
        raise SignalFailure(signum)

    # A signal mid-publication must reach the same inclusive rollback as a write
    # failure: the prefix is either the old or the new generation, never a mix.
    previous = {item: signal.signal(item, interrupt) for item in SIGNALS}
    try:
        for index, filename in enumerate(INPUTS):
            destination = target.prefix / filename
            source = stage / filename
            regular(source)
            backup = recovery / f'backup-{index}'
            shutil.copyfile(destination, backup)
            staged = recovery / f'staged-{index}'
            shutil.copyfile(source, staged)
            saved.append((backup.read_bytes(), staged.read_bytes()))
            inventory.append({'destination': str(destination), 'source': str(source),
                              'backup': str(backup), 'staged': str(staged), 'existed': True,
                              'sha256': hashlib.sha256(backup.read_bytes()).hexdigest()})
        (recovery / 'inventory.json').write_text(json.dumps(inventory, indent=2) + '\n')
        for index, entry in enumerate(inventory):
            destination = Path(entry['destination'])
            authorize_mutation(target.prefix)
            temporary = destination.parent / ('.' + destination.name + '-' + recovery.name)
            attempted.append((index, temporary))
            with temporary.open('xb') as output:
                output.write(Path(entry['staged']).read_bytes())
            replace(temporary, destination)
        cleanup_started = True
        cleanup_scratch(recovery)
    except BaseException as cause:
        # A second signal must not interrupt the rollback that the first required.
        for item in previous:
            signal.signal(item, signal.SIG_IGN)
        incomplete = False
        if cleanup_started:
            # A cleanup failure is still a handled failure. Keep the preimages
            # until cleanup succeeds so even a partial rmtree can be recovered.
            try:
                no_redirect(recovery, None if target.production else target.root)
                recovery.mkdir(mode=0o700, exist_ok=True)
                info = recovery.stat()
                _scratch[recovery] = (info.st_dev, info.st_ino)
                for index, (original, proposed) in enumerate(saved):
                    for label, data in (('backup', original), ('staged', proposed)):
                        item = recovery / f'{label}-{index}'
                        item.unlink(missing_ok=True)
                        with item.open('xb') as output:
                            output.write(data)
                (recovery / 'inventory.json').write_text(json.dumps(inventory, indent=2) + '\n')
            except Exception:
                incomplete = True
        for index, temporary in reversed(attempted):
            entry = inventory[index]
            try:
                destination = Path(entry['destination'])
                no_redirect(destination, None if target.production else target.root)
                temporary.unlink(missing_ok=True)
                with temporary.open('xb') as output:
                    output.write(saved[index][0])
                replace(temporary, destination)
            except Exception:
                incomplete = True
        if incomplete:
            raise PublicationFailure(3, recovery, cause) from cause
        for _, temporary in attempted:
            temporary.unlink(missing_ok=True)
        try:
            cleanup_scratch(recovery)
        except OSError as cleanup_error:
            raise PublicationFailure(1, recovery, cleanup_error) from cause
        if isinstance(cause, (SignalFailure, KeyboardInterrupt)):
            # The old generation is fully restored, so report the signal's own
            # status (130/143/129) instead of a plain publication failure.
            signum = cause.signum if isinstance(cause, SignalFailure) else signal.SIGINT
            raise SystemExit(128 + signum)
        raise PublicationFailure(1, recovery, cause) from cause
    finally:
        for item, handler in previous.items():
            signal.signal(item, handler)


def _bootstrap(source):
    spec = importlib.util.spec_from_file_location('dsh_pnpm_bootstrap', source / 'bootstrap.py')
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def reviewed_pnpm_source():
    # Containers mount the reviewed base sources read-only; a host checkout keeps
    # them beside this module. The mounted path is the documented build mount,
    # never an arbitrary caller override of the reviewed pin.
    mounted = Path(os.environ.get('PNPM_SOURCE_DIR', '/tmp/pnpm-source'))
    if (mounted / 'pin.json').is_file():
        return mounted
    return Path(__file__).resolve().parents[2] / 'pnpm'


def dereferenced_paths(path):
    # Every path the kernel dereferences while resolving `path`, one hop at a
    # time: realpath() would hide a writable directory that is only a waypoint.
    paths = []
    resolved = Path(path.anchor or '/')
    pending = list(path.parts)
    hops = 0
    while pending:
        candidate = resolved / pending.pop(0)
        paths.append(candidate)
        if not candidate.is_symlink():
            resolved = candidate
            continue
        hops += 1
        if hops > 40:
            raise ValueError('symlink chain too deep: ' + str(path))
        target = Path(os.readlink(candidate))
        if not target.is_absolute():
            target = resolved / target
        resolved = Path(target.anchor or '/')
        pending = list(target.parts) + pending
    return paths


def distribution_entries(prefix):
    # The authenticated extraction contains no links, so a link is an
    # unauthenticated redirect rather than part of the reviewed distribution.
    entries = []
    pending = [prefix]
    while pending:
        with os.scandir(pending.pop()) as scan:
            for entry in scan:
                if entry.is_symlink():
                    raise ValueError('unexpected symlinked distribution entry: ' + entry.path)
                item = Path(entry.path)
                entries.append(item)
                if entry.is_dir(follow_symlinks=False):
                    pending.append(item)
    return entries


def verify_base_distribution(commands=PNPM_COMMANDS, prefix=PNPM_PREFIX, node=PNPM_NODE,
                            source=None, fixture_root=None):
    """Authenticate the installed base manager (images/pnpm/verify.py semantics).

    A fixture install must not accept a same-version shadow or a modified
    launcher/bundle, so presence of the fixed path is never enough.
    """
    source = Path(source) if source is not None else reviewed_pnpm_source()
    bootstrap = _bootstrap(source)
    pin = bootstrap.read_pin(source / 'pin.json')
    if bootstrap.read_pin(prefix / 'pin.json') != pin:
        raise ValueError('installed pin disagrees with reviewed source')
    if hashlib.sha256((prefix / 'dist/pnpm.mjs').read_bytes()).hexdigest() != pin['bundleSha256']:
        raise ValueError('pristine pnpm distribution changed')
    launcher = commands / 'pnpm'
    if launcher.is_symlink() or not launcher.is_file():
        raise ValueError('pnpm launcher must be a regular file')
    if launcher.read_bytes() != (source / 'launcher.sh').read_bytes():
        raise ValueError('pnpm launcher changed')
    for alias in ('pnx', 'pnpx'):
        link = commands / alias
        if not link.is_symlink() or os.readlink(link) != 'pnpm':
            raise ValueError('pnpm alias must link relatively to pnpm: ' + alias)
    checked = set()

    def audit(part):
        if fixture_root is not None and part in fixture_root.parents:
            return
        if part in checked:
            return
        checked.add(part)
        info = part.stat()
        if fixture_root is None and (info.st_uid != 0 or info.st_gid != 0):
            raise ValueError('non-image-owned distribution path: ' + str(part))
        if info.st_mode & 0o022:
            raise ValueError('group/other-writable distribution path: ' + str(part))
        required = 0o005 if stat.S_ISDIR(info.st_mode) or part in (launcher, node) else 0o004
        if info.st_mode & required != required:
            raise ValueError('unreadable distribution path: ' + str(part))

    for path in [prefix, launcher, commands, node, *distribution_entries(prefix)]:
        for part in dereferenced_paths(path):
            audit(part)
    if stat.S_IMODE(launcher.stat().st_mode) != 0o755:
        raise ValueError('pnpm launcher mode must be 0755')


def command_identity(name, expected):
    """Prove the first lexical PATH candidate for `name` is the installed file.

    A same-version copy earlier on PATH is a different program, so identity is
    a filesystem fact, never a version string. The first candidate that exists
    lexically is THE command: a dangling or unusable candidate fails instead of
    falling through to a later executable.
    """
    found = None
    for directory in os.environ.get('PATH', '').split(os.pathsep):
        candidate = Path(directory or '.') / name
        if os.path.lexists(candidate):
            found = candidate
            break
    if found is None:
        raise ValueError(name + ': not found on PATH')
    expected = absolute(expected)
    for item in (found, expected):
        if not item.is_file() or not os.access(item, os.R_OK | os.X_OK):
            raise ValueError(f'{name}: not an executable regular file: {item}')
    if not os.path.samefile(found, expected):
        raise ValueError(f'{name}: first PATH candidate {found} is not the installed command {expected}')


def remove_scratch(path, prefix=None):
    """Remove a scratch directory created by an earlier invocation.

    The in-process registry cannot survive a process boundary, so confinement
    falls back to these rules: direct child of the fixed /tmp or fixture-root
    scratch parent, owned by this UID, named like an invocation scratch, no
    symlink waypoints and no internal redirect escaping the directory. It is
    deliberately independent of the prefix state: cleanup after a failed
    publication must work even when the prefix itself was tampered with.
    """
    path = absolute(path)
    parents = [Path('/tmp')]
    fixture = os.environ.get('AGENT_VM_FIXTURE_ROOT') or os.environ.get('AGENT_VM_SCRATCH_ROOT')
    if fixture:
        parents.append(absolute(fixture))
    if path.parent not in parents:
        raise ValueError('scratch must be a direct child of an authorized scratch parent: ' + str(path))
    if not path.name.startswith('dsh-'):
        raise ValueError('not an invocation scratch: ' + str(path))
    no_redirect(path, path.parent)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
        raise ValueError('scratch must be an invocation-owned directory')
    allowed = ()
    if prefix is not None:
        prefix = absolute(prefix)
        no_redirect(prefix)
        if not prefix.is_dir():
            raise ValueError('installed prefix to preserve must be a directory: ' + str(prefix))
        allowed = (prefix,)
    _refuse_escaping_links(path, allowed)
    shutil.rmtree(path)


def remove_partial_install(capability):
    """Drop the authorized prefix's own node_modules after a hard install failure.

    A half-installed agent is broken rather than absent, so it must not survive.
    The four inputs, the base pin, the status record and caller HOME/XDG/.npm are
    never touched here.
    """
    if not isinstance(capability, MutationAuthorization):
        raise ValueError('runtime capability cannot remove installed payload')
    nodes = capability.prefix / 'node_modules'
    if not os.path.lexists(nodes):
        return
    no_redirect(nodes, None if capability.production else capability.root)
    info = nodes.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
        raise ValueError('node_modules must be an invocation-owned directory')
    shutil.rmtree(nodes)


def manager_argv(capability):
    if not isinstance(capability, MutationAuthorization):
        raise ValueError('runtime capability cannot start a package manager')
    if capability.production:
        return [str(PNPM_COMMANDS / 'pnpm')]
    if os.environ.get('AGENT_VM_DSH_PNPM_PACKAGE'):
        if os.environ.get('AGENT_VM_FIXTURE_PNPM') or os.environ.get('AGENT_VM_FIXTURE_BASE_PNPM'):
            raise ValueError('ambiguous fixture manager selection')
        return authenticated_host_argv(capability.prefix, os.environ['AGENT_VM_DSH_PNPM_PACKAGE'],
                                       os.environ.get('AGENT_VM_DSH_PNPM_ARCHIVE', ''), os.environ.get('AGENT_VM_DSH_NODE', ''))
    fake = os.environ.get('AGENT_VM_FIXTURE_PNPM')
    stock = os.environ.get('AGENT_VM_FIXTURE_BASE_PNPM')
    if fake and stock:
        raise ValueError('ambiguous fixture manager selection')
    if fake:
        executable = absolute(fake)
        no_redirect(executable, capability.root)
        regular(executable)
        if not os.access(executable, os.X_OK):
            raise ValueError('fixture manager must be executable')
        return [str(executable)]
    if stock:
        # The focused native harness installs under a fixture prefix with the real
        # installed base launcher. Returning the fixed absolute path (never a PATH
        # lookup) is allowed only after the whole distribution authenticates.
        verify_base_distribution()
        return [str(PNPM_COMMANDS / 'pnpm')]
    raise ValueError('fixture requires an authorized fake or the authenticated base launcher')


def authenticated_host_argv(prefix, package, archive, node):
    guard_environment([prefix])
    capability = authorize_mutation(prefix)
    if capability.production:
        raise ValueError('host stock-distribution route is developer-only')
    package, archive, node = map(absolute, (package, archive, node))
    for item in (package, archive):
        no_redirect(item, capability.root)
    regular(archive)
    if not package.is_dir():
        raise ValueError('stock distribution must be a directory')
    # Trust the reviewed source pin, not a caller-supplied same-version pin.
    source = Path(__file__).resolve().parents[2] / 'pnpm'
    bootstrap = _bootstrap(source)
    pin = bootstrap.read_pin(source / 'pin.json')
    if pin['version'] != '11.11.0':
        raise ValueError('unsupported base manager selection')
    if capability.basePin.read_bytes() != (source / 'pin.json').read_bytes():
        raise ValueError('fixture base pin differs from reviewed pin')
    bootstrap.authenticate(archive, pin)
    scratch = owned_scratch(capability)
    try:
        expected = bootstrap.extract(archive, scratch, pin)
        def inventory(root):
            result = {}
            for directory, dirs, files in os.walk(root, followlinks=False):
                for filename in (*dirs, *files):
                    item = Path(directory) / filename
                    no_redirect(item, root)
                    info = item.lstat()
                    if not (stat.S_ISREG(info.st_mode) or stat.S_ISDIR(info.st_mode)):
                        raise ValueError('unsafe distribution member')
                    result[str(item.relative_to(root))] = ('directory' if item.is_dir()
                                                          else hashlib.sha256(item.read_bytes()).hexdigest())
            return result
        if inventory(package) != inventory(expected):
            raise ValueError('stock distribution inventory differs from authenticated archive')
    finally:
        cleanup_scratch(scratch)
    if not node.is_file() or not os.access(node, os.X_OK):
        raise ValueError('absolute executable Node required')
    return [str(node), str(package / 'bin/pnpm.mjs')]


def main():
    parser = argparse.ArgumentParser()
    sub = parser.add_subparsers(dest='verb', required=True)
    auth = sub.add_parser('authorize')
    auth.add_argument('prefix')
    auth.add_argument('--command')
    auth.add_argument('--status-dir')
    runtime = sub.add_parser('authorize-runtime')
    runtime.add_argument('prefix')
    runtime.add_argument('--command', required=True)
    runtime.add_argument('--scratch-root', required=True)
    pub = sub.add_parser('publish')
    pub.add_argument('staged_prefix')
    pub.add_argument('prefix')
    guard = sub.add_parser('guard')
    guard.add_argument('prefixes', nargs='*')
    manager = sub.add_parser('manager')
    manager.add_argument('prefix')
    scratch = sub.add_parser('scratch')
    scratch.add_argument('prefix')
    cleanup = sub.add_parser('cleanup')
    cleanup.add_argument('path')
    cleanup.add_argument('--prefix')
    partial = sub.add_parser('remove-install')
    partial.add_argument('prefix')
    identity = sub.add_parser('command-identity')
    identity.add_argument('name')
    identity.add_argument('expected')
    contract = sub.add_parser('contract-dir')
    contract.add_argument('prefixes', nargs='*')
    args = vars(parser.parse_args())
    verb = args.pop('verb')
    if verb == 'authorize':
        print(json.dumps(authorize_mutation(**args).json()))
    elif verb == 'authorize-runtime':
        print(json.dumps(authorize_runtime(**args).json()))
    elif verb == 'publish':
        publish(**args)
    elif verb == 'guard':
        guard_environment(args['prefixes'])
    elif verb == 'manager':
        print('\n'.join(manager_argv(authorize_mutation(args['prefix']))))
    elif verb == 'scratch':
        capability = authorize_mutation(args['prefix'])
        scratch = owned_scratch(capability)
        # Cross-process use: cleanup re-authorizes the path instead of consulting
        # this process's registry, so drop the registration before printing.
        del _scratch[scratch]
        print(scratch)
    elif verb == 'cleanup':
        remove_scratch(args['path'], args['prefix'])
    elif verb == 'remove-install':
        remove_partial_install(authorize_mutation(args['prefix']))
    elif verb == 'command-identity':
        command_identity(args['name'], args['expected'])
    else:
        print(authorize_contract(args['prefixes']))


if __name__ == '__main__':
    try:
        main()
    except PublicationFailure as error:
        print('dsh-project: ' + str(error), file=sys.stderr)
        sys.exit(error.status)
    except (ValueError, OSError) as error:
        print('dsh-project: ' + str(error), file=sys.stderr)
        sys.exit(1)
