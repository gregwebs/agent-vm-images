#!/usr/bin/env python3
"""Shared pnpm project filesystem capabilities and handled-failure publication.

Three consumers (dsh, Pi and its Claude bridge) share one install/authorization
boundary; a lock parser is deliberately not part of it. Every consumer difference
lives in the closed table below, so no caller can select a production path, a
scratch parent or a manager route by argument or environment.
"""
import argparse
from dataclasses import dataclass, field
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
from typing import Literal

sys.dont_write_bytecode = True

INPUTS = ('package.json', 'pnpm-lock.yaml', 'pnpm-workspace.yaml', 'pnpm-inputs.sha256')
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
class Consumer:
    """One reviewed consumer's fixed production facts.

    Production paths are data here rather than a caller argument: a mutable
    global or an argument-selected prefix would let any of the three callers
    adopt another consumer's authority. `host_env` names the developer host
    acquisition variables (`{host_env}_PNPM_PACKAGE|_PNPM_ARCHIVE|_NODE`); the
    production image owns the manager and refuses all three.
    """
    name: str
    production_prefix: Path
    production_command: Path | None
    scratch_token: str
    host_env: str
    scratch_parent: Path


CONSUMERS = {item.name: item for item in (
    # dsh links a fixed command. Pi's runtime command is the payload's own
    # generated bin link -- the verifier authorizes exactly that, and the warning
    # wrapper at /usr/local/bin/pi is a separate regular file checked by content,
    # never a runtime target (a command outside the prefix cannot be one). The
    # bridge has no command at all.
    Consumer('dsh', Path('/opt/agent-vm/dsh'), Path('/usr/local/bin/dsh'),
             'dsh-', 'AGENT_VM_DSH', Path('/tmp')),
    Consumer('pi', Path('/opt/agent-vm/pi'), Path('/opt/agent-vm/pi/node_modules/.bin/pi'),
             'pi-', 'AGENT_VM_PI', Path('/tmp')),
    Consumer('bridge', Path('/opt/agent-vm/pi-packages'), None,
             'bridge-', 'AGENT_VM_BRIDGE', Path('/tmp')),
)}
DSH = CONSUMERS['dsh']
PRODUCTION = DSH.production_prefix


def consumer_of(value):
    """Resolve a consumer name to its reviewed profile; there is no default.

    A profile object is accepted only when it *is* the table's entry, so a caller
    cannot build a same-named profile with another production path and present it
    to an authorization function.
    """
    if isinstance(value, Consumer):
        known = CONSUMERS.get(value.name)
        if known is None or known != value:
            raise ValueError('consumer profile is not the reviewed table entry: ' + value.name)
        return known
    try:
        return CONSUMERS[value]
    except (KeyError, TypeError):
        raise ValueError('unknown consumer: ' + str(value))


@dataclass(frozen=True)
class Authorization:
    prefix: Path
    root: Path
    basePin: Path
    production: bool
    command: Path | None = None
    status_dir: Path | None = None
    consumer: Consumer = DSH

    def json(self):
        return {'prefix': str(self.prefix), 'root': str(self.root),
                'basePin': str(self.basePin), 'production': self.production,
                'consumer': self.consumer.name}


@dataclass(frozen=True)
class MutationAuthorization(Authorization):
    mutation: Literal[True] = field(default=True, init=False)


@dataclass(frozen=True)
class RuntimeAuthorization(Authorization):
    mutation: Literal[False] = field(default=False, init=False)


@dataclass(frozen=True)
class BootstrapAuthorization(Authorization):
    """Authority to create a project's own first inputs in a private stage.

    Deliberately not a MutationAuthorization: a stage cannot install payload,
    publish to the live prefix, write status records or acquire a manager. The
    stage (`.stage`) is the only destination it names.
    """
    mutation: Literal[False] = field(default=False, init=False)

    @property
    def stage(self) -> Path:
        return self.prefix

    def json(self):
        return {'stage': str(self.stage), 'root': str(self.root),
                'basePin': str(self.basePin), 'production': self.production,
                'consumer': self.consumer.name}


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
    """Images source root of the checkout this module belongs to.

    Derived from this file, never from the environment or the caller's prefix, so
    a copied repository owns its own contract directory and no caller value can
    move executable policy. This file sits in `images/tools/`, beside the
    per-consumer source directories, so its parent's parent is the source root.
    """
    return Path(__file__).resolve().parents[1]


def contract_dir():
    """Directory of the composed recipe-contract helper.

    The build passes the read-only mount as AGENT_VM_CONTRACT_DIR; a host
    checkout builds the same composed directory and passes it the same way. The
    fallback names the tracked module beside this file, which is canonical source
    but carries no binary, so a caller that forgot the build gets a named failure
    instead of a different parser. There is deliberately no search path: a bogus
    caller value fails rather than silently selecting executable policy.
    """
    value = os.environ.get('AGENT_VM_CONTRACT_DIR')
    if value:
        return absolute(value)
    return checkout_root() / 'recipe-contract'


def host_contract_dir():
    """Composed helper directory of this host checkout, or None.

    script/build/recipe-contract.sh --native publishes `<repo>/target/
    recipe-contract/<os>-<arch>/out`; a host can only run its own platform's
    build, and a machine outside those spellings has no host shape at all.
    """
    machine = platform.machine().lower()
    arch = {'arm64': 'arm64', 'aarch64': 'arm64',
            'x86_64': 'amd64', 'amd64': 'amd64'}.get(machine)
    if arch is None:
        return None
    return (checkout_root().parent / 'target' / 'recipe-contract'
            / f'{platform.system().lower()}-{arch}' / 'out')


def authorize_contract(prefixes=(), *, consumer=DSH):
    """Authorize the directory whose helpers this invocation may execute.

    That directory selects executable policy, so it is authorized before any
    command is spawned from it. Two legitimate shapes exist: a production prefix
    uses the read-only build mount, a fixture names a directory inside its own
    invocation-owned root, and a host checkout names its own tracked
    `images/recipe-contract` (derived from this module's location). A host
    checkout keeps its scratch prefixes inside the authorized root of the same
    invocation, so a claimed destination outside every authorized root fails.
    """
    profile = consumer_of(consumer)
    directory = contract_dir()
    projects = [absolute(prefix) for prefix in prefixes]
    root = None
    host = host_contract_dir()
    if profile.production_prefix in projects:
        if directory != PRODUCTION_CONTRACT:
            raise ContractFailure('production contract directory is fixed')
        no_redirect(directory)
    elif directory == checkout_root() / 'recipe-contract' or (host is not None and directory == host):
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
    binary = directory / 'recipe-contract'
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise ContractFailure(
            'no executable recipe-contract in ' + str(directory)
            + ' (set AGENT_VM_CONTRACT_DIR to a composed helper directory;'
            ' a source checkout carries no binary)')
    regular(binary)
    return directory


def guard_environment(prefixes=(), *, consumer=DSH):
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
    directory = authorize_contract(prefixes, consumer=consumer)
    result = subprocess.run([str(directory / 'recipe-contract'), 'pnpm-environment',
                             *(str(prefix) for prefix in prefixes)],
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


def _authorize_destination(prefix, command=None, status_dir=None, runtime=False, consumer=DSH):
    """Authorize one project destination without requiring its inputs.

    Publication is the operation that creates or replaces those inputs, so it
    authorizes the destination itself: the path shape, ownership, base pin,
    command and status destinations. Ordinary mutation and runtime consumers add
    the four-input readiness check below instead of weakening this one.
    """
    profile = consumer_of(consumer)
    prefix = absolute(prefix)
    production = prefix == profile.production_prefix
    if production:
        if not runtime and os.getuid() != 0:
            raise ValueError('production mutation requires root')
        root, pin = Path('/opt/agent-vm'), Path('/opt/agent-vm/pnpm/pin.json')
        if command is not None and absolute(command) != profile.production_command:
            raise ValueError('production command is fixed')
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
    return capability(prefix, root, pin, production, command, status_dir, profile)


def _require_inputs(prefix, root, production):
    """The reviewed four inputs must already be complete project input."""
    for filename in INPUTS:
        item = Path(prefix) / filename
        no_redirect(item, None if production else root)
        regular(item)


def _authorize(prefix, command=None, status_dir=None, runtime=False, consumer=DSH):
    capability = _authorize_destination(prefix, command, status_dir, runtime, consumer)
    _require_inputs(capability.prefix, capability.root, capability.production)
    return capability


def authorize_mutation(prefix, command=None, status_dir=None, *, consumer=DSH) -> MutationAuthorization:
    return _authorize(prefix, command, status_dir, consumer=consumer)


def authorize_runtime(prefix, command, scratch_root, *, consumer=DSH,
                      status_dir=None, mutation=False) -> RuntimeAuthorization:
    if status_dir is not None or mutation or command is None:
        raise ValueError('runtime authorization requires a command and cannot mutate payload/status')
    capability = _authorize(prefix, command, runtime=True, consumer=consumer)
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


def authorize_stage(stage, *, consumer=DSH, sources=()):
    """Authorize a consumer's private bootstrap stage before anything is created.

    A first import has no four inputs and may not have a fixture base pin yet, so
    ordinary mutation authorization cannot describe it: that authority requires
    the project this episode creates. A stage is therefore authorized by shape
    only -- an invocation-owned private child of the consumer's fixed scratch
    parent (production) or of this invocation's authorized root (fixture) -- and
    must stay disjoint from every protected path: the three production prefixes,
    the contract directory, the reviewed pnpm sources, this checkout, the base
    pin, the status destination and any source the caller names.
    """
    profile = consumer_of(consumer)
    stage = absolute(stage)
    production = stage.parent == profile.scratch_parent
    if production:
        root = Path('/opt/agent-vm')
        if not stage.name.startswith(profile.scratch_token):
            raise ValueError('production stage name must start with ' + profile.scratch_token)
    else:
        root = authorized_root()
        below(stage, root)
    no_redirect(stage)
    pin = root / 'pnpm/pin.json'
    parent = stage.parent
    if not parent.is_dir() or parent.stat().st_uid != os.getuid():
        raise ValueError('stage parent must be an invocation-owned directory')
    if os.path.lexists(stage):
        info = stage.lstat()
        if not stat.S_ISDIR(info.st_mode):
            raise ValueError('stage must be a directory')
        if info.st_uid != os.getuid() or stat.S_IMODE(info.st_mode) & 0o077:
            raise ValueError('stage must be an invocation-owned private 0700 directory')
    protected = [*[item.production_prefix for item in CONSUMERS.values()],
                 contract_dir(), reviewed_pnpm_source(), checkout_root(), pin,
                 root / 'install-status',
                 *[absolute(source) for source in sources]]
    # The protected paths legitimately nest inside each other (the checkout owns
    # the pnpm sources), so only the stage is compared with each of them.
    for path in protected:
        disjoint([stage, path])
    return BootstrapAuthorization(stage, root, pin, production, None, None, profile)


# Only this process's registered directories may be recursively removed.
_scratch: dict[Path, tuple[int, int]] = {}


def scratch_parents(profile):
    """Every directory an invocation may create its own scratch child in.

    The canonical parent (or the authorized fixture root) plus, when the caller
    names one, this consumer's `AGENT_VM_<CONSUMER>_WORK_DIR`. The work directory
    is an *authorized parent*, never a scratch: the invocation still allocates a
    fresh 0700 `mkdtemp` child with the consumer's token inside it and never
    deletes the caller's directory. A shared fixed scratch is exactly what that
    shape prevents (two concurrent runs would otherwise read each other's state),
    so widening the parent does not widen what can be removed.
    """
    parents = [profile.scratch_parent]
    work = os.environ.get(profile.host_env + '_WORK_DIR')
    if work:
        parent = absolute(work)
        no_redirect(parent)
        if not parent.is_dir() or parent.stat().st_uid != os.getuid():
            raise ValueError('work directory must be an invocation-owned directory')
        parents.append(parent)
    return parents


def owned_scratch(capability, parent=None):
    """Create this invocation's private scratch below the capability's parent.

    Mutation and bootstrap capabilities get the consumer's scratch token so a
    cross-process cleanup can tell which consumer owns the directory, and a
    bootstrap capability stages only inside its own authorized stage.
    """
    if capability.mutation:
        if capability.production:
            fixed = capability.consumer.scratch_parent
            parent = absolute(parent or fixed)
            no_redirect(parent)
            if parent not in scratch_parents(capability.consumer):
                raise ValueError('production scratch parent is not authorized')
        else:
            parent = absolute(parent or capability.root)
            no_redirect(parent)
            fixed = capability.root
            if parent != fixed:
                below(parent, capability.root)
    elif isinstance(capability, BootstrapAuthorization):
        # A bootstrap stage is itself the private scratch; work children never
        # escape it, so a stage cannot stage outside its authorized directory.
        parent = absolute(parent or capability.stage)
        no_redirect(parent)
        if parent != capability.stage:
            below(parent, capability.stage)
    else:
        parent = absolute(parent or os.environ['HOME'])
        authorize_runtime(capability.prefix, capability.command, parent,
                          consumer=capability.consumer)
    if not parent.is_dir() or parent.stat().st_uid != os.getuid():
        # /tmp is shared, but mkdtemp below creates the owned private boundary.
        if not (capability.production and capability.mutation
                and parent == capability.consumer.scratch_parent):
            raise ValueError('scratch parent must be invocation-owned')
    if parent != capability.root:
        # A bootstrap stage *is* its own work parent, so it is not compared with
        # itself; only the protected destinations must stay out of the way.
        protected = [capability.basePin,
                     *([capability.status_dir] if capability.status_dir else []),
                     *([capability.command] if capability.command and capability.mutation else [])]
        if isinstance(capability, BootstrapAuthorization):
            for path in protected:
                disjoint([parent, path])
        else:
            disjoint([parent, capability.prefix, *protected])
    scratch = Path(tempfile.mkdtemp(prefix=capability.consumer.scratch_token, dir=parent)).resolve()
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


def publish(staged_prefix, prefix, *, replace=os.replace, consumer=DSH):
    """Publish the four reviewed inputs as one all-or-nothing transaction.

    A destination may be absent: a first import creates inputs rather than
    replacing them, and a destination that disappears between authorization and
    publication must still roll back to the same absence instead of leaving a
    file the caller never authored. Modes are part of the preimage, because a
    restored file with the wrong access bits is not the reviewed input.
    """
    target = _authorize_destination(prefix, consumer=consumer)
    stage = absolute(staged_prefix)
    if target.production:
        no_redirect(stage)
        # Production stages are private invocation directories, not an ambient
        # fixture override or a second source-maintenance prefix.
        parent = stage.parent
        if (parent.parent != target.consumer.scratch_parent
                or not parent.name.startswith(target.consumer.scratch_token)
                or parent.stat().st_uid != os.getuid()
                or stat.S_IMODE(parent.stat().st_mode) != 0o700
                or stage.stat().st_uid != os.getuid()):
            raise ValueError('production stage must be in private owned '
                             + str(target.consumer.scratch_parent) + '/'
                             + target.consumer.scratch_token + '* scratch')
        _require_inputs(stage, target.root, target.production)
    else:
        staged = _authorize_destination(stage, consumer=consumer)
        _require_inputs(staged.prefix, staged.root, staged.production)
        stage = staged.prefix
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
            existed = os.path.lexists(destination)
            original = None
            mode = None
            if existed:
                no_redirect(destination, None if target.production else target.root)
                regular(destination)
                shutil.copyfile(destination, backup)
                original = backup.read_bytes()
                mode = stat.S_IMODE(destination.lstat().st_mode)
            staged_file = recovery / f'staged-{index}'
            shutil.copyfile(source, staged_file)
            saved.append((original, staged_file.read_bytes()))
            inventory.append({'destination': str(destination), 'source': str(source),
                              'backup': str(backup), 'staged': str(staged_file),
                              'existed': existed, 'mode': mode,
                              'sha256': hashlib.sha256(original).hexdigest() if existed else None})
        (recovery / 'inventory.json').write_text(json.dumps(inventory, indent=2) + '\n')
        for index, entry in enumerate(inventory):
            destination = Path(entry['destination'])
            # Re-check the destination path itself before every write: the
            # inputs may be absent here, the destination may not have moved.
            _authorize_destination(target.prefix, consumer=target.consumer)
            temporary = destination.parent / ('.' + destination.name + '-' + recovery.name)
            # The attempt is recorded before the write so a partially written
            # destination is inside the reverse rollback.
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
                        if data is None:
                            continue
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
                original, _ = saved[index]
                if original is None:
                    # The destination did not exist before this publication, so a
                    # restored failure removes it instead of inventing a preimage.
                    destination.unlink(missing_ok=True)
                    continue
                with temporary.open('xb') as output:
                    output.write(original)
                replace(temporary, destination)
                if entry['mode'] is not None:
                    os.chmod(destination, entry['mode'])
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
    return Path(__file__).resolve().parents[1] / 'pnpm'


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


def remove_scratch(path, prefix=None, *, consumer=DSH):
    """Remove a scratch directory created by an earlier invocation.

    The in-process registry cannot survive a process boundary, so confinement
    falls back to these rules: direct child of the consumer's fixed scratch
    parent or the fixture root, owned by this UID, named with the consumer's
    invocation token, no symlink waypoints and no internal redirect escaping the
    directory. It is deliberately independent of the prefix state: cleanup after
    a failed publication must work even when the prefix itself was tampered with.
    """
    profile = consumer_of(consumer)
    path = absolute(path)
    parents = scratch_parents(profile)
    fixture = os.environ.get('AGENT_VM_FIXTURE_ROOT') or os.environ.get('AGENT_VM_SCRATCH_ROOT')
    if fixture:
        parents.append(absolute(fixture))
    if path.parent not in parents:
        raise ValueError('scratch must be a direct child of an authorized scratch parent: ' + str(path))
    if not path.name.startswith(profile.scratch_token):
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


# --- Pi gate layout and absence authority -----------------------------------
#
# The Pi curl producer removes the *entire* Pi prefix on a genuine transport
# failure, so no capability that requires a four-input project can describe the
# result. Absence therefore has its own authority, deliberately narrower than
# every ordinary one, and exactly one recognition site (the verifier's entry):
#
#   PiGateLayout            the fixed production tuple, or one all-fixture tuple
#   PresentPi               an installed command; carries health checks only
#   PiAbsenceAuthorization  a validated curl absence, plus a finite retirement
#                           inventory (nothing else is deletable)
#   PiTransportRemoval      the installer's own post-removal record, minted from
#                           a live MutationAuthorization and a classified code
#
# Neither absence type can install, publish inputs, start a manager, delete the
# status directory or reach caller state.
PI_GATE_DEFAULTS = {
    'pi': Path('/opt/agent-vm/pi'),
    'bridge': Path('/opt/agent-vm/pi-packages'),
    'extensions': Path('/opt/agent-vm/pi-extensions'),
    'wrapper': Path('/usr/local/bin/pi'),
    'seed': Path('/opt/agent-vm/seed.d/20-pi-claude-bridge'),
    'status_dir': Path('/opt/agent-vm/install-status'),
}
# The retired npm vocabulary is not reachable here: an absent Pi prefix can only
# be the independent curl producer's work, so the only admissible record is one
# classified curl code.
PI_CURL_CODES = frozenset({'5', '6', '7', '18', '28', '35', '52', '55', '56', '60'})
PI_ABSENT_RECORD = 'absent-transport '
BRIDGE_ABSENT_RECORD = 'absent-pi\n'


@dataclass(frozen=True)
class PiGateLayout:
    """Every slot the Pi feature owns, validated as one closed tuple."""
    pi: Path
    bridge: Path
    extensions: Path
    wrapper: Path
    seed: Path
    status_dir: Path
    contract: Path
    root: Path
    production: bool

    def json(self):
        return {name: str(getattr(self, name)) for name in
                ('pi', 'bridge', 'extensions', 'wrapper', 'seed', 'status_dir', 'contract')}

    @property
    def pi_bin(self):
        return self.pi / 'node_modules/.bin/pi'


@dataclass(frozen=True)
class PresentPi:
    """The command exists; no mutation or health authority is implied."""
    layout: PiGateLayout
    mutation: Literal[False] = field(default=False, init=False)


@dataclass(frozen=True)
class PiAbsenceAuthorization:
    """Authority to retire the feature for one recognised curl absence.

    The inventory is finite and fixed by the layout: two regular leaves (the
    warning wrapper and the seed hook), two owned directory roots (extensions
    and the bridge payload) and one atomic bridge status record. The Pi status
    record is preserved, because it is the evidence that authorises this work.
    """
    layout: PiGateLayout
    code: str
    mutation: Literal[False] = field(default=False, init=False)

    def retire(self):
        retire_absent_pi(self)


@dataclass(frozen=True)
class PiTransportRemoval:
    """The installer's own removal, derived from its live mutation capability."""
    capability: MutationAuthorization
    code: str
    mutation: Literal[False] = field(default=False, init=False)

    def remove(self):
        remove_pi_transport(self)


def _slot(value):
    """One gate slot: absolute and free of symlink waypoints."""
    path = absolute(value)
    no_redirect(path)
    return path


def authorize_pi_gate_layout(*, pi, bridge, extensions, wrapper, seed, status_dir,
                             contract=None, consumer='pi'):
    """Authorize the one closed tuple the absence decision may inspect.

    Production is the fixed image tuple. A fixture must place *every* slot under
    one authorized AGENT_VM_FIXTURE_ROOT, so a caller cannot combine a real
    destination with a fixture one and retire the wrong tree. The Pi leaf prefix
    may be missing here -- that is the state this layout exists to describe --
    while the status directory, its ancestors and the contract executable must be
    present and unredirected, or the answer is indeterminate rather than absent.
    """
    if consumer_of(consumer).name != 'pi':
        raise ValueError('the Pi gate layout belongs to the pi consumer only')
    if contract is not None:
        # The contract directory selects executable policy. It is named once (by
        # the build environment or by this explicit option) and then authorized
        # below exactly like every other caller's contract directory; an
        # unauthorized value fails there rather than silently taking effect.
        os.environ['AGENT_VM_CONTRACT_DIR'] = str(contract)
    slots = {}
    for name in ('pi', 'bridge', 'extensions', 'wrapper', 'seed', 'status_dir'):
        slots[name] = _slot({'pi': pi, 'bridge': bridge, 'extensions': extensions,
                             'wrapper': wrapper, 'seed': seed,
                             'status_dir': status_dir}[name])
    productions = [slots[name] == PI_GATE_DEFAULTS[name] for name in slots]
    if all(productions) != any(productions):
        raise ValueError('mixed production/fixture Pi gate tuple')
    production = all(productions)
    disjoint(list(slots.values()))
    if production:
        if os.getuid() != 0:
            raise ValueError('production Pi gate requires root')
        root = Path('/opt/agent-vm')
    else:
        root = authorized_root()
        for path in slots.values():
            below(path, root)
            below(path.resolve(strict=False), root)
    directory = authorize_contract([slots['pi']], consumer='pi')
    if contract is not None and Path(contract) != directory:
        raise ValueError('gate contract directory is not the authorized contract directory')
    status = slots['status_dir']
    if not status.is_dir() or status.is_symlink():
        # A missing (or relocated) status directory makes "no absence record"
        # indistinguishable from "no status destination", so it is indeterminate.
        raise ValueError('status directory must be an existing unredirected directory: ' + str(status))
    if production and status != Path('/opt/agent-vm/install-status'):
        raise ValueError('production status directory is fixed')
    for name in ('wrapper', 'seed'):
        if os.path.lexists(slots[name]):
            regular(slots[name])
    return PiGateLayout(slots['pi'], slots['bridge'], slots['extensions'], slots['wrapper'],
                        slots['seed'], status, directory, root, production)


def read_status_line(path, *, root=None):
    """One exact, bounded, owned, single-link record file; anything else is None.

    Deliberately not a stat-then-read: the same descriptor is opened with
    O_NOFOLLOW, its identity is compared with the lstat that named it, and the
    bytes are read from that descriptor, so a symlink or a swapped inode cannot
    substitute a different record.
    """
    path = absolute(path)
    try:
        info = path.lstat()
    except OSError:
        return None
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        return None
    if info.st_mode & 0o022 or info.st_uid != os.getuid():
        return None
    flags = os.O_RDONLY | getattr(os, 'O_NOFOLLOW', 0) | getattr(os, 'O_CLOEXEC', 0)
    try:
        descriptor = os.open(path, flags)
    except OSError:
        return None
    try:
        opened = os.fstat(descriptor)
        if (opened.st_dev, opened.st_ino) != (info.st_dev, info.st_ino):
            return None
        data = os.read(descriptor, 4096)
        if os.read(descriptor, 1):
            return None
    except OSError:
        return None
    finally:
        os.close(descriptor)
    return data


def pi_absent_code(layout, *, contract=None):
    """Validate `status-dir/pi` as one curl absence record and return its code."""
    data = read_status_line(layout.status_dir / 'pi', root=layout.root)
    if data is None:
        return None
    try:
        text = data.decode('ascii')
    except UnicodeDecodeError:
        return None
    if not text.startswith(PI_ABSENT_RECORD) or not text.endswith('\n') or text.count('\n') != 1:
        return None
    code = text[len(PI_ABSENT_RECORD):-1]
    if code not in PI_CURL_CODES:
        return None
    return code


def _path_state(contract, path):
    """The Go helper's typed PathState, as (0 absent, 1 present, 2 indeterminate)."""
    result = subprocess.run([str(Path(contract) / 'recipe-contract'), 'install-status',
                             'path-state', str(path)], capture_output=True, text=True)
    return result.returncode


def _recheck_absent(layout):
    state = _path_state(layout.contract, layout.pi)
    if state != 0:
        raise ValueError('the Pi prefix is not absent' if state == 1 else
                         'the Pi prefix state is indeterminate')


def pi_gate_entry(layout):
    """The one absence-recognition site: exactly one answer per invocation.

    A present command wins even when a stale absence record exists; a surviving
    prefix, a dangling or non-executable command, a broken ancestor or an
    indeterminate walk is a hard failure, never an absence. Absence additionally
    requires the record grammar and a second, immediately repeated look at the
    prefix, so the answer never describes a tree that changed under it.
    """
    state = _path_state(layout.contract, layout.pi_bin)
    if state == 1:
        return PresentPi(layout)
    if state != 0:
        raise ValueError('cannot classify ' + str(layout.pi_bin) + ' (permission/loop)')
    _recheck_absent(layout)
    code = pi_absent_code(layout)
    if code is None:
        raise ValueError('the Pi prefix is missing without a valid curl absence record')
    _recheck_absent(layout)
    if pi_absent_code(layout) != code:
        raise ValueError('the Pi absence record changed during recognition')
    return PiAbsenceAuthorization(layout, code)


def _owned_root(path, root):
    """A directory this feature owns: no symlink, correct owner, inside its root."""
    if not path.is_dir() or path.is_symlink():
        raise ValueError('expected an owned directory: ' + str(path))
    below(path, root)
    item = path.stat()
    allowed = (0, os.getuid()) if os.getuid() == 0 else (os.getuid(),)
    if item.st_uid not in allowed:
        raise ValueError('directory ownership mismatch: ' + str(path))
    return (item.st_dev, item.st_ino)


def retire_absent_pi(authorization):
    """Retire the feature for a recognised curl absence, and nothing else.

    Everything is preflighted before the first unlink, every deletion is
    non-following, and each target is rechecked against the identity recorded by
    the preflight. A symlinked or replaced root fails instead of being followed,
    so a sentinel outside the feature cannot be reached through it.
    """
    layout = authorization.layout
    _recheck_absent(layout)
    if pi_absent_code(layout) != authorization.code:
        raise ValueError('the Pi absence record changed before retirement')
    inventory = []
    for name in ('wrapper', 'seed'):
        path = getattr(layout, name)
        if not os.path.lexists(path):
            continue
        regular(path)
        info = path.lstat()
        inventory.append(('leaf', path, (info.st_dev, info.st_ino)))
    for name in ('extensions', 'bridge'):
        path = getattr(layout, name)
        if not os.path.lexists(path):
            continue
        inventory.append(('root', path, _owned_root(path, layout.root if not layout.production
                                                    else Path('/opt/agent-vm'))))
    record = layout.status_dir / 'pi-claude-bridge'
    if os.path.lexists(record) and record.is_symlink():
        raise ValueError('refusing to replace a symlinked bridge status record')
    for kind, path, identity in inventory:
        if kind == 'leaf':
            info = path.lstat()
            if (info.st_dev, info.st_ino) != identity:
                raise ValueError('retirement target identity changed: ' + str(path))
            path.unlink()
        else:
            info = path.lstat()
            if (info.st_dev, info.st_ino) != identity or not stat.S_ISDIR(info.st_mode):
                raise ValueError('retirement root identity changed: ' + str(path))
            shutil.rmtree(path)
    _write_record(record, BRIDGE_ABSENT_RECORD, layout.root)
    if os.path.lexists(layout.pi):
        raise ValueError('the Pi prefix reappeared during retirement')


def _write_record(destination, text, root):
    """Atomically publish one owned record, never through a symlink."""
    parent = destination.parent
    if not parent.is_dir() or parent.is_symlink():
        raise ValueError('record destination directory is missing: ' + str(parent))
    temporary = parent / ('.' + destination.name + '-agent-vm')
    if os.path.lexists(temporary):
        raise ValueError('refusing to reuse an existing temporary record: ' + str(temporary))
    try:
        with temporary.open('xb') as handle:
            handle.write(text.encode('ascii'))
        os.chmod(temporary, 0o644)
        if os.path.lexists(destination) and destination.is_symlink():
            raise ValueError('refusing to replace a symlinked record: ' + str(destination))
        os.replace(temporary, destination)
    except BaseException:
        try:
            temporary.unlink()
        except OSError:
            pass
        raise


def remove_pi_transport(removal):
    """The installer's own removal, then its curl absence record.

    Minted only from a live MutationAuthorization for the Pi prefix and a code
    the download contract classified; a pnpm failure, a checksum mismatch or a
    stale record can never reach this function.
    """
    capability = removal.capability
    if capability.consumer.name != 'pi':
        raise ValueError('Pi transport removal belongs to the pi consumer only')
    if removal.code not in PI_CURL_CODES:
        raise ValueError('not a classified download code: ' + str(removal.code))
    prefix = capability.prefix
    if os.path.lexists(prefix):
        no_redirect(prefix, None if capability.production else capability.root)
        info = prefix.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.getuid():
            raise ValueError('Pi prefix must be an invocation-owned directory')
        shutil.rmtree(prefix)
    if os.path.lexists(prefix):
        raise ValueError('the Pi prefix survived its own removal')
    _write_record(capability.status_dir / 'pi', PI_ABSENT_RECORD + removal.code + '\n',
                  capability.root)


def pi_transport_removal(prefix, code, *, consumer='pi'):
    """CLI/installer entry: authorize the live prefix, then record the removal."""
    capability = authorize_mutation(prefix, consumer=consumer)
    removal = PiTransportRemoval(capability, code)
    return removal


def _acquire_manager(profile, production, root, prefix):
    """Resolve the manager argv for one consumer's capability; no PATH lookup.

    Production owns the launcher outright, so a developer host acquisition
    variable is refused there instead of being silently ignored. The three
    variables are named per consumer: a Pi invocation cannot borrow dsh's
    authenticated stock distribution (or vice versa).
    """
    acquired = {suffix: os.environ.get(profile.host_env + suffix)
                for suffix in ('_PNPM_PACKAGE', '_PNPM_ARCHIVE', '_NODE')}
    if production:
        if any(acquired.values()):
            raise ValueError('production does not use host manager acquisition variables')
        return [str(PNPM_COMMANDS / 'pnpm')]
    if acquired['_PNPM_PACKAGE']:
        if os.environ.get('AGENT_VM_FIXTURE_PNPM') or os.environ.get('AGENT_VM_FIXTURE_BASE_PNPM'):
            raise ValueError('ambiguous fixture manager selection')
        return authenticated_host_argv(prefix, acquired['_PNPM_PACKAGE'],
                                       acquired['_PNPM_ARCHIVE'] or '',
                                       acquired['_NODE'] or '', consumer=profile)
    fake = os.environ.get('AGENT_VM_FIXTURE_PNPM')
    stock = os.environ.get('AGENT_VM_FIXTURE_BASE_PNPM')
    if fake and stock:
        raise ValueError('ambiguous fixture manager selection')
    if fake:
        executable = absolute(fake)
        no_redirect(executable, root)
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


def manager_argv(capability):
    """The manager argv an ordinary mutation capability may start."""
    if not isinstance(capability, MutationAuthorization):
        raise ValueError('runtime capability cannot start a package manager')
    return _acquire_manager(capability.consumer, capability.production,
                            capability.root, capability.prefix)


def stage_manager_argv(capability):
    """The manager argv a first-import bootstrap stage may start.

    A freshly adopted project has no reviewed inputs yet, so its driver cannot
    hold the MutationAuthorization `manager_argv` requires; it holds the stage
    instead and gets the same closed manager routes: the fixed base launcher in
    production, an authorized fake or the authenticated base launcher in a
    fixture. Nothing else is reachable, so the weaker destination authority does
    not widen who may be executed.
    """
    if not isinstance(capability, BootstrapAuthorization):
        raise ValueError('stage manager requires a bootstrap authorization')
    return _acquire_manager(capability.consumer, capability.production,
                            capability.root, capability.stage)


def authenticated_host_argv(prefix, package, archive, node, *, consumer=DSH):
    profile = consumer_of(consumer)
    guard_environment([prefix], consumer=profile)
    capability = authorize_mutation(prefix, consumer=profile)
    if capability.production:
        raise ValueError('host stock-distribution route is developer-only')
    package, archive, node = map(absolute, (package, archive, node))
    for item in (package, archive):
        no_redirect(item, capability.root)
    regular(archive)
    if not package.is_dir():
        raise ValueError('stock distribution must be a directory')
    # Trust the reviewed source pin, not a caller-supplied same-version pin.
    source = Path(__file__).resolve().parents[1] / 'pnpm'
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


def main(consumer=None):
    """CLI entry; the shared CLI requires --consumer, adapters supply a default."""
    parser = argparse.ArgumentParser(prog='pnpm-project.py')
    if consumer is None:
        parser.add_argument('--consumer', required=True, choices=sorted(CONSUMERS))
    sub = parser.add_subparsers(dest='verb', required=True)
    auth = sub.add_parser('authorize')
    auth.add_argument('prefix')
    auth.add_argument('--command')
    auth.add_argument('--status-dir')
    runtime = sub.add_parser('authorize-runtime')
    runtime.add_argument('prefix')
    runtime.add_argument('--command', required=True)
    runtime.add_argument('--scratch-root', required=True)
    bootstrap = sub.add_parser('authorize-stage')
    bootstrap.add_argument('stage')
    bootstrap.add_argument('--source', action='append', default=[])
    pub = sub.add_parser('publish')
    pub.add_argument('staged_prefix')
    pub.add_argument('prefix')
    guard = sub.add_parser('guard')
    guard.add_argument('prefixes', nargs='*')
    manager = sub.add_parser('manager')
    manager.add_argument('prefix')
    stage_manager = sub.add_parser('stage-manager')
    stage_manager.add_argument('stage')
    stage_manager.add_argument('--source', action='append', default=[])
    scratch = sub.add_parser('scratch')
    scratch.add_argument('prefix')
    scratch.add_argument('--parent')
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
    gate = sub.add_parser('pi-gate-entry')
    gate.add_argument('--pi-prefix', required=True)
    gate.add_argument('--bridge-prefix', required=True)
    gate.add_argument('--extension-root', required=True)
    gate.add_argument('--wrapper', required=True)
    gate.add_argument('--seed', required=True)
    gate.add_argument('--status-dir', required=True)
    gate.add_argument('--contract-dir')
    gate.add_argument('--retire', action='store_true')
    transport = sub.add_parser('pi-transport-removal')
    transport.add_argument('prefix')
    transport.add_argument('code')
    options = vars(parser.parse_args())
    profile = consumer_of(options.pop('consumer', consumer))
    verb = options.pop('verb')
    if verb == 'authorize':
        print(json.dumps(authorize_mutation(**options, consumer=profile).json()))
    elif verb == 'authorize-runtime':
        print(json.dumps(authorize_runtime(**options, consumer=profile).json()))
    elif verb == 'authorize-stage':
        print(json.dumps(authorize_stage(options['stage'], consumer=profile,
                                         sources=options['source']).json()))
    elif verb == 'publish':
        publish(**options, consumer=profile)
    elif verb == 'guard':
        guard_environment(options['prefixes'], consumer=profile)
    elif verb == 'manager':
        print('\n'.join(manager_argv(authorize_mutation(options['prefix'], consumer=profile))))
    elif verb == 'stage-manager':
        stage = authorize_stage(options['stage'], consumer=profile, sources=options['source'])
        print('\n'.join(stage_manager_argv(stage)))
    elif verb == 'scratch':
        capability = authorize_mutation(options['prefix'], consumer=profile)
        scratch = owned_scratch(capability, options['parent'])
        # Cross-process use: cleanup re-authorizes the path instead of consulting
        # this process's registry, so drop the registration before printing.
        del _scratch[scratch]
        print(scratch)
    elif verb == 'cleanup':
        remove_scratch(options['path'], options['prefix'], consumer=profile)
    elif verb == 'remove-install':
        remove_partial_install(authorize_mutation(options['prefix'], consumer=profile))
    elif verb == 'command-identity':
        command_identity(options['name'], options['expected'])
    elif verb == 'pi-gate-entry':
        layout = authorize_pi_gate_layout(
            pi=options['pi_prefix'], bridge=options['bridge_prefix'],
            extensions=options['extension_root'], wrapper=options['wrapper'],
            seed=options['seed'], status_dir=options['status_dir'],
            contract=options['contract_dir'], consumer=profile)
        entry = pi_gate_entry(layout)
        if isinstance(entry, PresentPi):
            print('present')
        elif options['retire']:
            entry.retire()
            print('absent-retired')
        else:
            print('absent')
    elif verb == 'pi-transport-removal':
        pi_transport_removal(options['prefix'], options['code'], consumer=profile).remove()
    else:
        print(authorize_contract(options['prefixes'], consumer=profile))


def entry(consumer=None):
    try:
        main(consumer)
    except PublicationFailure as error:
        print('pnpm-project: ' + str(error), file=sys.stderr)
        sys.exit(error.status)
    except (ValueError, OSError) as error:
        print('pnpm-project: ' + str(error), file=sys.stderr)
        sys.exit(1)


if __name__ == '__main__':
    entry()
