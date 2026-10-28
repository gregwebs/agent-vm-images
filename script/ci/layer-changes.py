#!/usr/bin/env python3
"""Turn one workflow event into the example-layer build rows it must run.

Two levels of selection keep a single workflow honest. The workflow's own
``paths`` envelope stops an unrelated event from starting it at all; this
selector then narrows a relevant event to the contexts whose inputs changed, so
a Rust-only change does not rebuild Chromium. Discovery is the filesystem, not a
name table, so a new ``examples/layers/<name>`` joins the matrix without a YAML
edit. The unusual inputs below are deliberate: the full diff rather than the
last commit (a PR's relevant change is usually not its tip), NUL-delimited names
(a path may contain anything but NUL), and a bounded single-object fetch so a
missing comparison commit fails loudly instead of silently selecting nothing.

Usage:
  python3 script/ci/layer-changes.py --event-name pull_request|push|workflow_dispatch \
      --event-path FILE --output FILE [--evidence-dir DIR]
  python3 script/ci/layer-changes.py --self-test
"""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from enum import Enum
from pathlib import Path


class SelectionError(Exception):
    """A selection that must stop the workflow, never an empty success."""


# --- the two fixed native architectures --------------------------------------
@dataclass(frozen=True)
class Architecture:
    name: str
    runner: str


# CI runners only. QEMU is not an acceptance path, so the requested platform and
# the runner are one decision rather than two.
ARCHITECTURES = (Architecture('amd64', 'ubuntu-24.04'), Architecture('arm64', 'ubuntu-24.04-arm'))

GENERIC_TIMEOUT_MINUTES = 130
RUNTIME_SUITE_TIMEOUT_MINUTES = 205
# Chrome is the one context with a runtime suite after its cold build, so it is
# the one row that needs the larger job budget.
RUNTIME_SUITE_LAYERS = frozenset({'chrome-devtools'})

# Both names are matched with `fullmatch` at every call site: `match` with a `$`
# anchor accepts one trailing newline, so `rust-dev\n` would name a directory
# that does not exist and an unwrapped SHA would reach git as two arguments.
NAME_RE = re.compile(r'[a-z0-9][a-z0-9-]*')
SHA_RE = re.compile(r'[0-9a-f]{40}')
LAYERS_RELATIVE = 'examples/layers'

GIT_TIMEOUT_SECONDS = 60
FETCH_TIMEOUT_SECONDS = 120

# Routing table (see ADR 0010): a change here selects every layer, because these
# files are inputs to every build; a change to a layer's own directory selects
# that layer. Anything else changes no layer, and editing a base recipe does not
# change an example's frozen released parent.
SHARED_INPUTS = frozenset({
    'script/ci/layer-changes.py',
    'script/test/layer-build.sh',
    'script/test/host-watchdog.py',
    'script/test/require-daemon-builder.sh',
    'script/ci/apt-install.sh',
    'script/test/contracts.sh',
    'script/test/example-layers.sh',
    '.github/workflows/contracts.yml',
    '.github/workflows/example-layers.yml',
    # Kept during the rename transition so a merge-base that predates it still
    # schedules the renamed workflow.
    '.github/workflows/chrome-example.yml',
    # Parent inventory and source policy: every layer's parent is decided there.
    'images/parent-pins.json',
})
SHARED_PREFIXES = ('script/test/image-sources/',)
CHROME_INPUTS = frozenset({
    'script/test/chrome-example.sh',
    'script/test/chrome-example-watchdog.sh',
    'script/build/chrome-mcp-provenance.sh',
})
CHROME_PREFIXES = ('script/test/fixtures/chrome-example/',)
CHROME_LAYER = 'chrome-devtools'


@dataclass(frozen=True, order=True)
class LayerName:
    """An example layer directory name, validated on construction.

    Python has no private constructor, so the check lives in ``__post_init__``
    rather than pretending the type cannot be built from a raw string.
    """

    value: str

    def __post_init__(self) -> None:
        if not NAME_RE.fullmatch(self.value):
            raise SelectionError(f'invalid example layer name: {self.value!r}')


class EventKind(str, Enum):
    """The three events the workflow routes; anything else is a broken call."""

    PULL_REQUEST = 'pull_request'
    PUSH = 'push'
    WORKFLOW_DISPATCH = 'workflow_dispatch'


@dataclass(frozen=True)
class Sha:
    """A full 40-hex commit SHA, validated on construction.

    ``_sha`` is the intended producer; the dataclass constructor stays public
    because Python has no private one, so ``__post_init__`` enforces the
    invariant.
    """

    value: str

    def __post_init__(self) -> None:
        if not SHA_RE.fullmatch(self.value):
            raise SelectionError(f'not a full 40-hex commit SHA: {self.value!r}')

    def __str__(self) -> str:
        # A validated SHA is an argv value, never an interpolated command line.
        return self.value


# A push that created its branch has no earlier endpoint to compare.
ZERO_SHA = Sha('0' * 40)


@dataclass(frozen=True)
class LayerJob:
    layer: LayerName
    architecture: Architecture

    def matrix_row(self) -> dict:
        timeout = (RUNTIME_SUITE_TIMEOUT_MINUTES if self.layer.value in RUNTIME_SUITE_LAYERS
                   else GENERIC_TIMEOUT_MINUTES)
        return {'layer': self.layer.value, 'arch': self.architecture.name,
                'platform': f'linux/{self.architecture.name}',
                'runner': self.architecture.runner, 'timeout': timeout}


@dataclass(frozen=True)
class Selection:
    layers: tuple[LayerName, ...]
    removed: tuple[LayerName, ...]
    reasons: tuple[str, ...]

    @property
    def jobs(self) -> tuple[LayerJob, ...]:
        return tuple(LayerJob(layer, architecture)
                     for layer in self.layers for architecture in ARCHITECTURES)


@dataclass(frozen=True)
class ChangeEvent:
    kind: EventKind
    # Both endpoints are absent exactly when the event selects every layer.
    base: Sha | None = None
    head: Sha | None = None
    all_layers: bool = False
    reason: str = ''


def discover_layers(root: Path) -> tuple[LayerName, ...]:
    """Every real ``examples/layers/<name>`` build context, in name order."""
    directory = root / LAYERS_RELATIVE
    if not directory.is_dir():
        raise SelectionError(f'missing example layer directory: {LAYERS_RELATIVE}')
    found = []
    for entry in sorted(directory.iterdir()):
        if entry.is_symlink():
            raise SelectionError(f'example layer {entry.name} is a symlink')
        if not entry.is_dir():
            continue
        recipe = entry / 'Dockerfile'
        if recipe.is_symlink():
            raise SelectionError(f'example layer {entry.name} Dockerfile is a symlink')
        if not recipe.is_file():
            raise SelectionError(f'example layer {entry.name} has no regular Dockerfile')
        found.append(LayerName(entry.name))
    return tuple(found)


def parse_event(name: str, payload: dict) -> ChangeEvent:
    if not isinstance(payload, dict):
        raise SelectionError('event payload is not a JSON object')
    try:
        kind = EventKind(name)
    except ValueError:
        raise SelectionError(f'unsupported event name: {name!r}')
    if kind is EventKind.WORKFLOW_DISPATCH:
        return ChangeEvent(kind, all_layers=True, reason='manual dispatch selects every layer')
    if kind is EventKind.PULL_REQUEST:
        request = payload.get('pull_request')
        if not isinstance(request, dict):
            raise SelectionError('pull_request event without a pull_request object')
        base = _sha(_nested(request.get('base')), 'pull_request.base.sha')
        head = _sha(_nested(request.get('head')), 'pull_request.head.sha')
        return ChangeEvent(kind, base=base, head=head)
    before = _sha(payload.get('before'), 'before')
    after = _sha(payload.get('after'), 'after')
    if before == ZERO_SHA:
        # A new branch: there is no earlier endpoint to compare, so the safe
        # answer is every layer rather than none.
        return ChangeEvent(kind, all_layers=True, reason='push without a before commit')
    return ChangeEvent(kind, base=before, head=after)


def _nested(value):
    # GitHub nests the PR endpoints as objects; a bare SHA is accepted too.
    return value.get('sha') if isinstance(value, dict) else value


def _sha(value, field: str) -> Sha:
    if not isinstance(value, str) or not SHA_RE.fullmatch(value):
        raise SelectionError(f'{field} is not a full 40-hex commit SHA: {value!r}')
    return Sha(value)


def changed_paths(root: Path, event: ChangeEvent) -> tuple[str, ...]:
    if event.all_layers:
        return ()
    if event.base is None or event.head is None:
        raise SelectionError(f'{event.kind.value} event without both diff endpoints')
    _require_commit(root, event.base, 'base')
    _require_commit(root, event.head, 'head')
    # A PR compares from the merge base, so a base branch that advanced after the
    # branch was cut does not appear as a change; a push compares its endpoints.
    separator = '...' if event.kind is EventKind.PULL_REQUEST else '..'
    raw = _git(root, 'diff', '--no-renames', '--name-only', '-z',
               f'{event.base.value}{separator}{event.head.value}', '--')
    # A rename is reported as its delete and its add, so both sides are matched.
    return tuple(os.fsdecode(chunk) for chunk in raw.split(b'\0') if chunk)


def select_layers(layers: tuple[LayerName, ...], paths: tuple[str, ...], *,
                  all_layers: bool) -> Selection:
    known = {layer.value for layer in layers}
    selected = set(known) if all_layers else set()
    removed: set[str] = set()
    reasons = set()
    for path in paths:
        shared = _shared_reason(path)
        if shared is not None:
            selected |= known
            reasons.add(shared)
            continue
        if _is_chrome_input(path):
            if CHROME_LAYER in known:
                selected.add(CHROME_LAYER)
            reasons.add('chrome-devtools runtime inputs changed')
            continue
        context = _layer_segment(path)
        if context is None:
            continue
        if context in known:
            selected.add(context)
            reasons.add(f'{context} inputs changed')
        else:
            removed.add(context)
            reasons.add(f'{context} context removed or not buildable')
    return Selection(layers=tuple(LayerName(name) for name in sorted(selected)),
                     removed=tuple(LayerName(name) for name in sorted(removed)),
                     reasons=tuple(sorted(reasons)))


def _shared_reason(path: str) -> str | None:
    if path in SHARED_INPUTS:
        return f'shared build input: {path}'
    if path.startswith(SHARED_PREFIXES):
        return f'shared parent inventory or source policy: {path}'
    return None


def _is_chrome_input(path: str) -> bool:
    return path in CHROME_INPUTS or path.startswith(CHROME_PREFIXES)


def _layer_segment(path: str) -> str | None:
    """The layer whose directory contains ``path``, matching whole segments."""
    parts = path.split('/')
    if len(parts) < 3 or parts[0] != 'examples' or parts[1] != 'layers':
        return None
    name = parts[2]
    return name if NAME_RE.fullmatch(name) else None


def matrix_for(selection: Selection) -> dict:
    return {'include': [job.matrix_row() for job in selection.jobs]}


# --- git plumbing ------------------------------------------------------------
def _git(root: Path, *args: str | Sha, timeout: float = GIT_TIMEOUT_SECONDS) -> bytes:
    # No shell: every argument, a validated Sha included, is one argv entry.
    argv = [str(arg) for arg in args]
    try:
        completed = subprocess.run(['git', '-C', str(root), *argv], stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        raise SelectionError(f'git {argv[0]} timed out after {timeout:g}s')
    except OSError as error:
        raise SelectionError(f'cannot run git: {error}')
    if completed.returncode != 0:
        detail = completed.stderr.decode('utf-8', 'replace').strip()
        raise SelectionError(f'git {argv[0]} failed: {detail}')
    return completed.stdout


def _require_commit(root: Path, sha: Sha, role: str) -> None:
    try:
        _git(root, 'cat-file', '-e', f'{sha.value}^{{commit}}')
        return
    except SelectionError:
        pass
    # The checkout may be shallow or newer than the event's base. One bounded
    # fetch of exactly this object is the whole fallback: no API, no guessing.
    try:
        _git(root, 'fetch', '--no-tags', 'origin', sha, timeout=FETCH_TIMEOUT_SECONDS)
    except SelectionError as error:
        raise SelectionError(f'{role} commit {sha.value} is absent and the bounded fetch failed: {error}')
    if not _commit_present(root, sha):
        raise SelectionError(f'{role} commit {sha.value} is still absent after a bounded fetch')


def _commit_present(root: Path, sha: Sha) -> bool:
    try:
        _git(root, 'cat-file', '-e', f'{sha.value}^{{commit}}')
    except SelectionError:
        return False
    return True


# --- outputs and evidence ----------------------------------------------------
def write_outputs(output: Path, matrix: dict, has_layers: bool) -> None:
    # One line each: $GITHUB_OUTPUT is a key=value file, and an empty selection
    # must be exactly {"include":[]} rather than a placeholder row.
    with output.open('a') as handle:
        handle.write(f'matrix={json.dumps(matrix, separators=(",", ":"))}\n')
        handle.write(f'has_layers={"true" if has_layers else "false"}\n')


def write_evidence(directory: Path, record: dict) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    (directory / 'selection.json').write_text(json.dumps(record, indent=2, sort_keys=True) + '\n')
    (directory / 'summary.txt').write_text(summarize(record))


def summarize(record: dict) -> str:
    lines = [f'event: {record.get("event")}',
             f'checkout: {record.get("checkout_sha") or "unknown"}',
             f'diff: {record.get("diff_base") or "-"}...{record.get("diff_head") or "-"}']
    if record.get('failure'):
        lines.append(f'FAILED: {record["failure"]}')
        return '\n'.join(lines) + '\n'
    for path in record.get('changed_paths', []):
        lines.append(f'changed: {path}')
    for reason in record.get('reasons', []):
        lines.append(f'reason: {reason}')
    for name in record.get('removed', []):
        lines.append(f'removed: {name}')
    rows = record.get('matrix', {}).get('include', [])
    lines.append(f'rows: {len(rows)}')
    for row in rows:
        lines.append(f'row: {row["layer"]} {row["platform"]} {row["runner"]} {row["timeout"]}m')
    return '\n'.join(lines) + '\n'


def repository_root() -> Path:
    return Path(__file__).resolve().parents[2]


def main(argv: list[str], root: Path | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--event-name', choices=('pull_request', 'push', 'workflow_dispatch'))
    parser.add_argument('--event-path')
    parser.add_argument('--output')
    parser.add_argument('--evidence-dir')
    parser.add_argument('--self-test', action='store_true')
    arguments = parser.parse_args(argv)
    if arguments.self_test:
        return run_self_test()
    if not (arguments.event_name and arguments.event_path and arguments.output):
        parser.error('--event-name, --event-path and --output are required')
    root = root or repository_root()
    evidence = Path(arguments.evidence_dir) if arguments.evidence_dir else None
    record: dict = {'event': arguments.event_name, 'checkout_sha': _head_sha(root)}
    try:
        payload = json.loads(Path(arguments.event_path).read_text())
        event = parse_event(arguments.event_name, payload)
        record.update(diff_base=event.base.value if event.base else '',
                      diff_head=event.head.value if event.head else '',
                      all_layers=event.all_layers)
        paths = changed_paths(root, event)
        selection = select_layers(discover_layers(root), paths, all_layers=event.all_layers)
    except (OSError, ValueError, SelectionError) as error:
        record['failure'] = str(error or error.__class__.__name__)
        record.setdefault('reasons', [])
        if evidence:
            write_evidence(evidence, record)
        sys.stderr.write(f'layer-changes: {record["failure"]}\n')
        return 1
    matrix = matrix_for(selection)
    reasons = list(selection.reasons)
    if event.reason:
        reasons.insert(0, event.reason)
    record.update(changed_paths=list(paths), matrix=matrix, reasons=reasons,
                  removed=[layer.value for layer in selection.removed],
                  selected=[layer.value for layer in selection.layers])
    if evidence:
        write_evidence(evidence, record)
    write_outputs(Path(arguments.output), matrix, bool(selection.jobs))
    print(summarize(record), end='')
    return 0


def _head_sha(root: Path) -> str:
    try:
        return _git(root, 'rev-parse', 'HEAD').decode().strip()
    except SelectionError:
        return ''


# --- self-test: a scratch repository, no daemon and no network ---------------
SELF_TEST_LAYERS = ('chrome-devtools', 'go-dev', 'new-layer', 'rust-dev',
                    'rust-dev-extra', 'wirenboard-cpp')


class Fixture:
    """A scratch repository with a scratch bare origin.

    The environment is cleared of inherited git configuration so a developer's
    global config cannot redirect these commands, and the only remote is a local
    path (``file`` transport): the self-test never reaches a network service.
    """

    def __init__(self, root: Path) -> None:
        self.root = root
        self.repo = root / 'repo'
        self.origin = root / 'origin.git'
        self.home = root / 'home'
        self.home.mkdir()
        os.environ.update({
            'GIT_CONFIG_GLOBAL': os.devnull, 'GIT_CONFIG_SYSTEM': os.devnull,
            'GIT_CONFIG_NOSYSTEM': '1', 'GIT_TERMINAL_PROMPT': '0',
            'GIT_AUTHOR_NAME': 'fixture', 'GIT_AUTHOR_EMAIL': 'fixture@example.invalid',
            'GIT_COMMITTER_NAME': 'fixture', 'GIT_COMMITTER_EMAIL': 'fixture@example.invalid',
            'GIT_AUTHOR_DATE': '2026-01-01T00:00:00+0000',
            'GIT_COMMITTER_DATE': '2026-01-01T00:00:00+0000',
            'HOME': str(self.home),
        })
        self.repo.mkdir()
        self.git('init', '--quiet', '--initial-branch=main')
        files = {'README.md': '# fixture\n', 'examples/layers/README.md': '# layers\n',
                 'examples/layers/go-dev/notes.txt': 'notes\n',
                 'images/Dockerfile': 'FROM scratch\n', 'script/test/contracts.sh': '#!/bin/sh\n',
                 'script/test/chrome-example.sh': '#!/bin/sh\n'}
        for layer in SELF_TEST_LAYERS:
            files[f'examples/layers/{layer}/Dockerfile'] = 'FROM scratch\n'
        self.commit('base', files)
        self.base = self.rev('HEAD')
        subprocess.run(['git', 'init', '--quiet', '--bare', str(self.origin)], check=True)
        self.git('remote', 'add', 'origin', str(self.origin))
        self.git('push', '--quiet', 'origin', 'main')

    def git(self, *args: str) -> str:
        return subprocess.run(['git', '-C', str(self.repo), *args], check=True,
                              stdout=subprocess.PIPE, text=True,
                              env=dict(os.environ)).stdout

    def rev(self, reference: str) -> str:
        return self.git('rev-parse', reference).strip()

    def commit(self, message: str, changes: dict[str, str | None]) -> None:
        for path, text in changes.items():
            target = self.repo / path
            if text is None:
                self.git('rm', '--quiet', '-r', '--', path)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)
        self.git('add', '-A')
        self.git('commit', '--quiet', '-m', message)

    def branch(self, name: str, changes: dict[str, str | None]) -> str:
        self.git('checkout', '--quiet', '-b', name, self.base)
        self.commit(name, changes)
        self.git('checkout', '--quiet', 'main')
        return self.rev(name)


class SelfTest:
    def __init__(self, fixture: Fixture) -> None:
        self.fixture = fixture
        self.checks = 0
        self.failures = 0

    def expect(self, label: str, actual, expected) -> None:
        self.checks += 1
        if actual != expected:
            self.failures += 1
            print(f'self-test: FAIL {label}: expected {expected!r}, got {actual!r}', file=sys.stderr)
            return
        print(f'self-test: ok   {label}')

    def event(self, payload: dict) -> Path:
        path = self.fixture.root / f'event-{self.checks}-{len(payload)}.json'
        path.write_text(json.dumps(payload))
        return path

    def select(self, payload: dict, kind: str = 'pull_request') -> Selection:
        event = parse_event(kind, payload)
        return select_layers(discover_layers(self.fixture.repo),
                             changed_paths(self.fixture.repo, event),
                             all_layers=event.all_layers)

    def pull_request(self, base: str, head: str) -> dict:
        return {'pull_request': {'base': {'sha': base}, 'head': {'sha': head}}}


def run_self_test() -> int:
    with tempfile.TemporaryDirectory(prefix='layer-changes-selftest-') as temporary:
        fixture = Fixture(Path(temporary))
        test = SelfTest(fixture)
        _self_test_validated_values(test)
        _self_test_own_paths(test, fixture)
        _self_test_shared_inputs(test, fixture)
        _self_test_context_lifecycle(test, fixture)
        _self_test_history(test, fixture)
        _self_test_failures(test, fixture)
    if test.failures:
        print(f'self-test: {test.failures} of {test.checks} controls failed', file=sys.stderr)
        return 1
    print(f'self-test: passed ({test.checks} controls)')
    return 0


def _layers(selection: Selection) -> tuple[str, ...]:
    return tuple(layer.value for layer in selection.layers)


def _self_test_validated_values(test: SelfTest) -> None:
    """A validated value is the whole string, never a prefix of one."""
    sha = 'a' * 40

    def refused(label: str, build) -> None:
        try:
            build()
        except SelectionError:
            test.expect(label, 'refused', 'refused')
        else:
            test.expect(label, 'accepted', 'refused')

    refused('a layer name with a trailing newline is refused', lambda: LayerName('rust-dev\n'))
    refused('a layer name with a trailing space is refused', lambda: LayerName('rust-dev '))
    refused('a SHA with a trailing newline is refused', lambda: Sha(sha + '\n'))
    refused('a SHA with a trailing space is refused', lambda: Sha(sha + ' '))
    refused('a 12-hex abbreviation is refused', lambda: Sha(sha[:12]))
    test.expect('a validated SHA prints as its bare hex', str(Sha(sha)), sha)
    test.expect('a validated layer name keeps its bare value', LayerName('rust-dev').value,
                'rust-dev')
    # The same rule at the path-segment site, which never reaches a constructor.
    test.expect('a path segment with a trailing newline is not a layer',
                _layer_segment('examples/layers/rust-dev\n/file'), None)


def _self_test_own_paths(test: SelfTest, fixture: Fixture) -> None:
    cases = [(f'own path selects only {layer}', layer,
              {f'examples/layers/{layer}/Dockerfile': 'FROM scratch\n# changed\n'})
             for layer in SELF_TEST_LAYERS]
    cases.append(('a docs-only change selects nothing', None,
                  {'README.md': '# changed\n', 'examples/layers/README.md': '# changed\n'}))
    cases.append(('a product recipe alone selects nothing', None, {'images/Dockerfile': 'FROM x\n'}))
    for label, expected, changes in cases:
        head = fixture.branch(label.replace(' ', '-'), changes)
        selection = test.select(test.pull_request(fixture.base, head))
        test.expect(label, _layers(selection), () if expected is None else (expected,))
        test.expect(f'{label}: rows', len(selection.jobs), 0 if expected is None else 2)
        if expected is not None:
            rows = [job.matrix_row() for job in selection.jobs]
            test.expect(f'{label}: native rows', [(row['arch'], row['runner']) for row in rows],
                        [('amd64', 'ubuntu-24.04'), ('arm64', 'ubuntu-24.04-arm')])
            test.expect(f'{label}: row timeout', {row['timeout'] for row in rows},
                        {205} if expected == 'chrome-devtools' else {130})
    # A lookalike name is a different context, not a prefix match for its neighbour.
    head = fixture.branch('lookalike', {'examples/layers/rust-dev-extra/extra.txt': 'x\n'})
    test.expect('a lookalike name does not select its neighbour',
                _layers(test.select(test.pull_request(fixture.base, head))), ('rust-dev-extra',))
    head = fixture.branch('union', {'examples/layers/go-dev/a.txt': 'x\n',
                                   'examples/layers/wirenboard-cpp/b.txt': 'x\n',
                                   'examples/layers/go-dev/c.txt': 'x\n'})
    selection = test.select(test.pull_request(fixture.base, head))
    test.expect('several changed layers are listed once each', _layers(selection),
                ('go-dev', 'wirenboard-cpp'))
    test.expect('several changed layers produce one row pair each', len(selection.jobs), 4)


def _self_test_shared_inputs(test: SelfTest, fixture: Fixture) -> None:
    for path in ('script/test/host-watchdog.py', 'script/test/contracts.sh',
                 'images/parent-pins.json', 'script/ci/layer-changes.py',
                 'script/test/image-sources/tests/image_sources.rs',
                 '.github/workflows/chrome-example.yml'):
        head = fixture.branch(f'shared-{path.replace("/", "-")}', {path: 'changed\n'})
        selection = test.select(test.pull_request(fixture.base, head))
        test.expect(f'shared input {path} selects every layer', len(selection.layers),
                    len(SELF_TEST_LAYERS))
        test.expect(f'shared input {path} rows', len(selection.jobs), 2 * len(SELF_TEST_LAYERS))
    head = fixture.branch('chrome-only', {'script/test/chrome-example.sh': 'changed\n',
                                         'script/test/host-watchdog.py': 'x\n'})
    test.expect('shared input wins over the Chrome-only routing',
                _layers(test.select(test.pull_request(fixture.base, head))),
                tuple(sorted(SELF_TEST_LAYERS)))
    head = fixture.branch('chrome-helper', {'script/test/chrome-example-watchdog.sh': 'x\n'})
    test.expect('a Chrome runtime helper selects Chrome only',
                _layers(test.select(test.pull_request(fixture.base, head))), ('chrome-devtools',))
    dispatched = test.select({}, 'workflow_dispatch')
    test.expect('dispatch selects every layer', len(dispatched.layers), len(SELF_TEST_LAYERS))
    zeroes = '0' * 40
    bootstrap = test.select({'before': zeroes, 'after': fixture.rev('main')}, 'push')
    test.expect('a push without a before commit selects every layer',
                len(bootstrap.layers), len(SELF_TEST_LAYERS))


def _self_test_context_lifecycle(test: SelfTest, fixture: Fixture) -> None:
    # Each case selects against the head tree, as CI builds the merge commit; the
    # working tree and the diff therefore describe the same context set.
    def on_branch(name: str, change) -> Selection:
        fixture.git('checkout', '--quiet', '-b', name, fixture.base)
        change()
        fixture.commit(name, {})
        selection = test.select(test.pull_request(fixture.base, fixture.rev('HEAD')))
        fixture.git('checkout', '--quiet', 'main')
        return selection

    def cross_layer_rename() -> None:
        fixture.git('mv', 'examples/layers/go-dev/notes.txt',
                    'examples/layers/rust-dev/go-dev-notes.txt')

    selection = on_branch('rename-file', cross_layer_rename)
    test.expect('a cross-layer rename selects both contexts', _layers(selection),
                ('go-dev', 'rust-dev'))

    def context_rename() -> None:
        fixture.git('mv', 'examples/layers/new-layer', 'examples/layers/renamed-layer')

    selection = on_branch('rename-context', context_rename)
    test.expect('a context rename selects the new name', _layers(selection), ('renamed-layer',))
    test.expect('a context rename records the old name as removed',
                tuple(layer.value for layer in selection.removed), ('new-layer',))

    def context_removal() -> None:
        fixture.git('rm', '--quiet', '-r', 'examples/layers/new-layer')

    selection = on_branch('remove-context', context_removal)
    test.expect('a removed context has no row', _layers(selection), ())
    test.expect('a removed context is recorded, not built',
                tuple(layer.value for layer in selection.removed), ('new-layer',))

    layout = fixture.root / 'layout'
    (layout / 'examples/layers/rust-dev').mkdir(parents=True)
    (layout / 'examples/layers/rust-dev/Dockerfile').write_text('FROM scratch\n')
    test.expect('a valid layout discovers its contexts',
                [layer.value for layer in discover_layers(layout)], ['rust-dev'])

    def discovery_refusal(label: str, expected: str, prepare) -> None:
        prepare()
        try:
            discover_layers(layout)
        except SelectionError as error:
            test.expect(label, expected in str(error), True)
        else:
            test.expect(label, 'accepted', 'refused')

    def surviving_without_recipe() -> None:
        (layout / 'examples/layers/go-dev').mkdir()

    discovery_refusal('a surviving context without a Dockerfile fails', 'go-dev',
                      surviving_without_recipe)

    def symlinked_context() -> None:
        (layout / 'examples/layers/go-dev').rmdir()
        (layout / 'elsewhere').mkdir()
        (layout / 'examples/layers/go-dev').symlink_to(layout / 'elsewhere')

    discovery_refusal('a symlinked context fails', 'go-dev', symlinked_context)

    def symlinked_recipe() -> None:
        (layout / 'examples/layers/go-dev').unlink()
        (layout / 'examples/layers/go-dev').mkdir()
        (layout / 'recipe').write_text('FROM scratch\n')
        (layout / 'examples/layers/go-dev/Dockerfile').symlink_to(layout / 'recipe')

    discovery_refusal('a symlinked recipe fails', 'go-dev', symlinked_recipe)

    def invalid_name() -> None:
        (layout / 'examples/layers/go-dev/Dockerfile').unlink()
        (layout / 'examples/layers/go-dev').rmdir()
        (layout / 'examples/layers/Bad Name').mkdir()
        (layout / 'examples/layers/Bad Name/Dockerfile').write_text('FROM scratch\n')

    discovery_refusal('an invalid layer name fails', 'invalid example layer name', invalid_name)


def _self_test_history(test: SelfTest, fixture: Fixture) -> None:
    # A PR whose relevant change is not its last commit, against a base that moved
    # independently: only the merge-base diff selects the right layer.
    fixture.git('checkout', '--quiet', '-b', 'pr-branch', fixture.base)
    fixture.commit('pr', {'examples/layers/rust-dev/Dockerfile': 'FROM scratch\n# rust\n'})
    fixture.commit('pr', {'README.md': '# docs last\n'})
    tip = fixture.rev('HEAD')
    fixture.git('checkout', '--quiet', 'main')
    fixture.commit('base moved', {'examples/layers/go-dev/Dockerfile': 'FROM scratch\n# go\n'})
    moved_base = fixture.rev('main')
    selection = test.select(test.pull_request(moved_base, tip))
    test.expect('a PR uses the merge-base diff, not its last commit', _layers(selection),
                ('rust-dev',))
    selection = test.select({'before': fixture.base, 'after': tip}, 'push')
    test.expect('a push compares both endpoints', _layers(selection), ('rust-dev',))

    # A comparison commit the checkout has never seen is fetched once from origin.
    clone = fixture.root / 'clone'
    subprocess.run(['git', 'clone', '--quiet', '--branch', 'main', str(fixture.origin), str(clone)],
                   check=True)
    (clone / 'examples/layers/wirenboard-cpp/new.txt').write_text('x\n')
    subprocess.run(['git', '-C', str(clone), 'add', '-A'], check=True)
    subprocess.run(['git', '-C', str(clone), 'commit', '--quiet', '-m', 'elsewhere'],
                   check=True)
    subprocess.run(['git', '-C', str(clone), 'push', '--quiet', 'origin', 'HEAD:elsewhere'],
                   check=True)
    unpublished = subprocess.run(['git', '-C', str(clone), 'rev-parse', 'HEAD'], check=True,
                                 stdout=subprocess.PIPE, text=True).stdout.strip()
    selection = test.select(test.pull_request(fixture.base, unpublished))
    test.expect('a missing commit is obtained from the scratch origin', _layers(selection),
                ('wirenboard-cpp',))

    # NUL-delimited names: a newline inside a path is one entry, not two.
    awkward = 'examples/layers/rust-dev/odd name\nwith newline'
    head = fixture.branch('awkward', {awkward: 'x\n'})
    event = parse_event('pull_request', test.pull_request(fixture.base, head))
    paths = changed_paths(fixture.repo, event)
    test.expect('an unusual filename is one changed path', len(paths), 1)
    test.expect('an unusual filename still selects its layer',
                _layers(select_layers(discover_layers(fixture.repo), paths, all_layers=False)),
                ('rust-dev',))


def _self_test_failures(test: SelfTest, fixture: Fixture) -> None:
    def fails_closed(label: str, payload: dict, kind: str = 'pull_request') -> None:
        output = fixture.root / f'output-{test.checks}'
        evidence = fixture.root / f'evidence-{test.checks}'
        event_path = test.event(payload)
        status = main(['--event-name', kind, '--event-path', str(event_path),
                       '--output', str(output), '--evidence-dir', str(evidence)], fixture.repo)
        test.expect(label, status, 1)
        test.expect(f'{label}: no matrix is emitted', output.exists(), False)
        test.expect(f'{label}: evidence survives the failure',
                    'FAILED' in (evidence / 'summary.txt').read_text(), True)

    fails_closed('an unavailable comparison commit fails closed',
                 test.pull_request(fixture.base, 'f' * 40))
    fails_closed('a malformed event fails closed', {'pull_request': {'base': {}}})
    fails_closed('an abbreviated SHA fails closed', test.pull_request(fixture.base[:12], fixture.base))

    output = fixture.root / 'output-empty'
    evidence = fixture.root / 'evidence-empty'
    event_path = test.event({'before': fixture.base,
                             'after': fixture.branch('noop', {'README.md': '# docs\n'})})
    status = main(['--event-name', 'push', '--event-path', str(event_path),
                   '--output', str(output), '--evidence-dir', str(evidence)], fixture.repo)
    test.expect('a docs-only change succeeds', status, 0)
    test.expect('an empty selection is exactly {"include":[]}',
                output.read_text(), 'matrix={"include":[]}\nhas_layers=false\n')
    test.expect('a docs-only run still writes evidence',
                'rows: 0' in (evidence / 'summary.txt').read_text(), True)


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
