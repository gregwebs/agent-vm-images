#!/usr/bin/env python3
"""The differential over the gopls *staging* writer: `avm-pin gopls stage` against the
Python stager it replaces (#252, slice 4a of #188).

The reader has had one of these since #249 (`gopls-closure-parity.py`); staging needs
its own because the two writers make a different decision. This suite drives both over
the synthetic fixture closure and compares, case by case:

  * the status (0 accepted, 1 refused, 2 usage/CI),
  * stdout and stderr byte for byte,
  * and everything the run wrote under `--out`: relative path, bytes and mode for each
    file, and mode for each directory, so an extra empty directory or a directory mode
    difference is visible too. That covers the five closure files, the rewritten
    Dockerfile and the retained summary; a refusal that retains a proposal is compared
    the same way, so the exact bytes a human is asked to review are the bytes both
    writers produced. Ownership, timestamps and extended attributes are not observed.

A comparison over nothing is a failure (ruling R-a): the case table is declared here,
every case runs, and the totals carry a floor per side and for the declared acceptances
and refusals. A case that cannot run is an error, never a skip.

One normalization is declared, for exactly one case: the case that withholds a module
object makes the `go` child fail, and the *child's* stderr below the writer's own line is
the compiler's -- its line order and how often it repeats an import chain vary between
runs on the same host. That case compares the writer's refusal line byte for byte on both
sides and leaves the child's diagnostic out; every other case compares the whole stream.

What the fixture proves, and what it does not. It resolves through a loopback proxy with
the sum database off, because the synthetic modules are not in the real transparency
log: the record both writers emit still names the reviewed production origins, so the
strict loader consumes exactly what production emits, and the *verdicts* are real.

Requests are measured three ways, and nothing more is claimed. (1) Every accepted case
compares the **set** of module objects the two sides asked the proxy for (`.info`,
`.mod`, `.zip`), not a list: `go mod tidy` may ask twice and the walk order is not a
contract. (2) The forbidden-path rule (`/sumdb/`, `@latest`, a toolchain module) is
decided on **both sides of every case**, accepted or refused, and the decisions are
counted, so deleting the rule is red. (3) It has two witnesses: a synthetic control over
each forbidden shape, and one live case whose `--go` names a compiler this host does not
have, so the pinned `go` child really does request `golang.org/toolchain` through the
recorder and the rule must report it. What no case claims is live checksum-database
authentication: the fixture's verifier key is off, and the record's named origins are the
production ones either way.

`--same-release` is the other half: it stages the reviewed v0.23.0 through both writers
with the production proxy and compares the six committed paths byte for byte. It needs
the network, so it is **opt-in and no gate runs it** — that leg is a maintainer action,
run for the slice's evidence and re-run by hand when the writer changes; the contracts
gate runs this suite without it.

The candidate re-read (`closure.Load` after the record is written) is defence in depth,
not a separately witnessed rule: no case here distinguishes deleting it while the
derivation and the loader agree, and `internal/goplsstage` names the unit test that
would. The rest of this file is the migration oracle for staging, and it dies with the
Python stager in #254.

Paths are absolute (`Path.resolve()`), so a checkout reached through a symlinked prefix
gives the same answer (ruling R-e). The Python side runs in-process with the fixture
environment injected, exactly as the pins suite drives it; the Go side is a child
process built twice from this checkout -- shipped, and with `-tags example_fixture`,
because the fixture origin is a compile-time seam and not a flag the shipped binary has
(ADR 0017, decision D1).
"""
from __future__ import annotations

import argparse
import contextlib
import dataclasses
import io
import os
import pathlib
import shutil
import subprocess
import sys
import traceback

sys.dont_write_bytecode = True

ROOT = pathlib.Path(__file__).resolve().parents[2]
FIXTURES = pathlib.Path(__file__).resolve().parent / 'fixtures' / 'gopls-closure'
DATA = ROOT / 'examples/layers/go-dev/data/gopls-closure'
DOCKERFILE = ROOT / 'examples/layers/go-dev/Dockerfile'
PYTHON_STAGER = ROOT / 'script/dev/gopls-closure-update.py'
REGISTER_MODULE = ROOT / 'examples/layers/rust-dev/helper'
DATA_FILES = ('go.mod', 'go.sum', 'gopls.go.mod', 'pin.json', 'THIRD_PARTY_LICENSES')

# Floors. Deliberately below today's counts, and above zero by enough that a broken
# table cannot pass.
MINIMUM_CASES = 10
MINIMUM_ACCEPTS = 3
MINIMUM_REFUSALS = 6
MINIMUM_COMPARISONS = 25

sys.path.insert(0, str(FIXTURES))
import fixture as fixture_module  # noqa: E402
import proxy as proxy_module  # noqa: E402,F401 - the fixture builds and owns the listener

Failures: list[str] = []


def check(name: str, condition: bool, detail: str = '') -> None:
    if condition:
        return
    Failures.append(f'{name}: {detail}' if detail else name)


def code_environment(fixture, *, ci: bool = False, toolchain: 'str | None' = None):
    """The fixture's injected environment, optionally the CI refusal.

    toolchain is the same value production derives from `--go` (`f'go{go}'`); a case
    that names another compiler must set it or the Python entry point refuses the
    mismatch before a child runs, which is a different rule than the one under test.
    """
    replacement = {'in_github_actions': ci}
    if toolchain is not None:
        replacement['toolchain'] = toolchain
    return dataclasses.replace(fixture.environment(), **replacement)


def python_stage(fixture, arguments: list[str], *, ci: bool = False,
                 toolchain: 'str | None' = None):
    """Run the real Python stager in-process and capture its contract output."""
    updater = fixture_module.updater
    stdout, stderr = io.StringIO(), io.StringIO()
    with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
        try:
            status = updater.main(arguments, code_environment(fixture, ci=ci,
                                                              toolchain=toolchain))
        except SystemExit as exit_status:  # argparse's own usage path
            status = int(exit_status.code or 0)
        except BaseException:  # noqa: BLE001 - an oracle crash is a difference to see
            status = 70
            traceback.print_exc(file=stderr)
    return status, stdout.getvalue(), stderr.getvalue()


def go_environment(go_root: pathlib.Path, *, ci: bool = False) -> dict[str, str]:
    env = {
        'PATH': f'{go_root / "bin"}:/usr/bin:/bin',
        'HOME': os.environ.get('HOME', '/tmp'),
        'LANG': 'C',
        'TMPDIR': os.environ.get('TMPDIR', '/tmp'),
    }
    if ci:
        env['GITHUB_ACTIONS'] = 'true'
    return env


def go_stage(binary: pathlib.Path, fixture, go_root: pathlib.Path, arguments: list[str], *,
             ci: bool = False, fixture_build: bool = True):
    """Run the Go staging verb as a child of this suite."""
    argv = [str(binary), 'gopls', 'stage', *arguments]
    if fixture_build:
        argv.extend(['--fixture-proxy', fixture.proxy.url(), '--fixture-sumdb', 'off'])
    result = subprocess.run(argv, cwd=str(ROOT), env=go_environment(go_root, ci=ci),
                            capture_output=True, text=True, timeout=1800)
    return result.returncode, result.stdout, result.stderr


def build_binary(workspace: pathlib.Path, name: str, *, fixture: bool) -> pathlib.Path:
    """Build one `avm-pin` variant from this checkout, in vendor mode and offline."""
    target = workspace / 'bin' / name
    target.parent.mkdir(parents=True, exist_ok=True)
    argv = ['go', 'build', '-trimpath', '-buildvcs=false', '-o', str(target)]
    if fixture:
        argv.extend(['-tags', 'example_fixture'])
    argv.append('./cmd/avm-pin')
    env = {**os.environ, 'GOFLAGS': '-mod=vendor', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'auto',
           'CGO_ENABLED': '0'}
    result = subprocess.run(argv, cwd=str(ROOT), env=env, capture_output=True, text=True)
    if result.returncode != 0:
        raise SystemExit(f'gopls-stage-parity: cannot build {name}: '
                         f'{result.stderr.strip() or result.stdout.strip()}')
    return target


def guarded_register() -> None:
    """Owner ruling G2: the import register runs before any fixture child is built."""
    result = subprocess.run(
        ['go', 'test', '-count=1', '-run',
         'TestImportsAreReviewed|TestImportBoundaryControls', '.'], cwd=str(REGISTER_MODULE),
        capture_output=True, text=True,
        env={**os.environ, 'GOFLAGS': '-mod=vendor', 'GOPROXY': 'off', 'GOTOOLCHAIN': 'auto'})
    if result.returncode != 0:
        raise SystemExit('gopls-stage-parity: the guarded-import register refused this tree; '
                         f'no fixture child was built:\n{result.stdout}{result.stderr}')


def staging_forbidden(paths: list[str]) -> list[str]:
    """Paths neither writer may ask for while staging.

    A version list is not on this list: resolution is exactly where a version may be
    chosen, and `go get` asks for one. What may never happen is a checksum-database
    lookup (the record's sum-database column is the committed determination, and the
    fixture's key is off), a `@latest` query (a moving target), or a toolchain module
    download (`GOTOOLCHAIN` names the pin, so a different compiler cannot be selected).
    """
    forbidden = []
    for path in paths:
        if '/sumdb/' in path or path.endswith('/@latest'):
            forbidden.append(path)
        elif path.startswith('/golang.org/toolchain'):
            forbidden.append(path)
    return forbidden


def module_requests(paths: list[str]) -> set[str]:
    """The module objects two runs asked the proxy for: `.info`, `.mod` and `.zip` paths.

    Version-list probes are excluded: a resolution may ask for one and this suite is
    comparing what each writer selected, not how its compiler walked there.
    """
    objects = set()
    for path in paths:
        if '/@v/' not in path or path.endswith('/@v/list') or path.endswith('/@latest'):
            continue
        objects.add(path)
    return objects


def tree(root: pathlib.Path) -> dict[str, tuple[bytes, int]]:
    """Everything a run wrote under root: relative path -> (bytes, mode).

    Directories are entries too, keyed with a trailing slash and empty bytes, so an
    extra empty directory or a directory mode difference is visible. Only the
    regular-file, symlink and directory kinds are observed; ownership, timestamps and
    extended attributes are not compared anywhere in this suite.
    """
    if not root.exists():
        return {}
    found = {}
    for path in sorted(root.rglob('*')):
        relative = path.relative_to(root).as_posix()
        mode = path.lstat().st_mode & 0o777
        if path.is_symlink():
            found[relative] = (b'symlink:' + os.readlink(path).encode(), mode)
        elif path.is_file():
            found[relative] = (path.read_bytes(), mode)
        elif path.is_dir():
            found[relative + '/'] = (b'', mode)
    return found


def compare_trees(label: str, python_root: pathlib.Path, go_root: pathlib.Path,
                  require_written: bool) -> int:
    """Compare two staged trees file by file, returning how many files were compared."""
    left, right = tree(python_root), tree(go_root)
    if require_written:
        check(f'{label}: the Python side wrote something', bool(left),
              f'nothing under {python_root}')
        check(f'{label}: the Go side wrote something', bool(right), f'nothing under {go_root}')
    compared = 0
    for name in sorted(set(left) | set(right)):
        if name not in right:
            check(f'{label}: {name} is only in the Python tree', False, str(python_root))
            continue
        if name not in left:
            check(f'{label}: {name} is only in the Go tree', False, str(go_root))
            continue
        compared += 1
        python_raw, python_mode = left[name]
        go_raw, go_mode = right[name]
        check(f'{label}: {name} bytes differ', python_raw == go_raw,
              f'python {len(python_raw)}B, go {len(go_raw)}B')
        check(f'{label}: {name} mode differs', python_mode == go_mode,
              f'python {python_mode:o}, go {go_mode:o}')
    return compared


class Case:
    """One staging comparison: the same release, the same fixture, both writers."""

    def __init__(self, name: str, *, release: str, current: 'pathlib.Path | None' = None,
                 review: 'tuple[str, str] | None' = None,
                 withhold: 'tuple[str, str, str] | None' = None, out_exists: bool = False,
                 ci: bool = False, expect: str = 'accept', stderr: str = 'exact',
                 stderr_prefix: str = 'gopls-closure-update: go get -tool failed (status 1):',
                 request_set: bool = True, compiler: 'str | None' = None,
                 expect_forbidden: bool = False):
        self.name = name
        self.release = release
        self.current = current
        self.review = review
        self.withhold = withhold
        self.out_exists = out_exists
        self.ci = ci
        self.expect = expect
        # `exact` compares the whole contract stream. `child` compares only the line the
        # writer itself decides -- "PREFIX: <what> failed (status N):" -- because the
        # rest is the `go` child's own diagnostic, whose line order and repetition are
        # the compiler's, not ours. It is a declared normalization for exactly the cases
        # that provoke a failing child, and the fragment is asserted on both sides.
        self.stderr = stderr
        # Whether the two sides' requested module-object sets must be equal. It is a set,
        # not a list: `go mod tidy` may ask for one object twice, and the order in which
        # a resolution walks the graph is not a contract. Every accepted case compares it.
        self.request_set = request_set
        # A case may name a compiler other than the fixture's. That is how a request on
        # the forbidden list is produced for real: `GOTOOLCHAIN=go1.28.0` makes the
        # pinned `go` child ask the proxy for the toolchain module, which must be
        # reported by staging_forbidden rather than passing unseen.
        self.compiler = compiler or fixture_module.PINNED_GO
        self.expect_forbidden = expect_forbidden
        self.stderr_prefix = stderr_prefix
        # Every side of every case must decide the request rule; main() holds the total.
        self.request_decisions = 0

    def arguments(self, current: str, out: pathlib.Path) -> list[str]:
        arguments = ['--gopls', self.release, '--go', self.compiler, '--current',
                     current, '--out', str(out)]
        if self.review is not None:
            arguments.extend(['--review-determination', self.review[0], '--reviewer',
                              self.review[1]])
        return arguments


def run_case(case: Case, fixture, fixture_binary: pathlib.Path, go_root: pathlib.Path,
             workspace: pathlib.Path) -> tuple[int, int]:
    """Run one case on both sides and compare everything it produced."""
    root = workspace / 'cases' / case.name.replace(' ', '-').replace('/', '-')
    root.mkdir(parents=True, exist_ok=True)
    out = root / 'out'
    current = str(case.current) if case.current is not None else str(root / 'no-current')
    if case.out_exists:
        out.mkdir(exist_ok=True)
    arguments = case.arguments(current, out)
    fixture.proxy.clear_requests()
    if case.withhold is not None:
        fixture.proxy.withhold(*case.withhold)
    try:
        # The stager's own grammar is `stage [flags]`; the Go verb spells the subcommand
        # under `gopls stage`.
        python_status, python_stdout, python_stderr = python_stage(
            fixture, ['stage', *arguments], ci=case.ci,
            toolchain='go' + case.compiler)
        python_paths = fixture.proxy.requested_paths()
        python_requests = len(python_paths)
        python_tree = root / 'python-tree'
        shutil.rmtree(python_tree, ignore_errors=True)
        if out.exists():
            shutil.copytree(out, python_tree)
            shutil.rmtree(out)
        if case.out_exists:
            out.mkdir()
        fixture.proxy.clear_requests()
        go_status, go_stdout, go_stderr = go_stage(
            fixture_binary, fixture, go_root, [*arguments, '--dockerfile', str(DOCKERFILE)],
            ci=case.ci)
        go_paths = fixture.proxy.requested_paths()
        go_requests = len(go_paths)
    finally:
        if case.withhold is not None:
            fixture.proxy.faults.pop(
                f'/{case.withhold[0]}/@v/{case.withhold[1]}.{case.withhold[2]}', None)

    check(f'{case.name}: status', python_status == go_status,
          f'python {python_status}, go {go_status}\npython stderr:\n{python_stderr[-800:]}'
          f'\ngo stderr:\n{go_stderr[-800:]}')
    check(f'{case.name}: stdout', python_stdout == go_stdout,
          'python:\n' + python_stdout[-1200:] + '\ngo:\n' + go_stdout[-1200:])
    if case.stderr == 'exact':
        check(f'{case.name}: stderr', python_stderr == go_stderr,
              'python:\n' + python_stderr[-1200:] + '\ngo:\n' + go_stderr[-1200:])
    else:
        # The writer's own refusal line, byte for byte, on both sides; the child's
        # diagnostic below it is not compared (see Case.stderr).
        prefix = case.stderr_prefix
        for side, stream in (('python', python_stderr), ('go', go_stderr)):
            check(f'{case.name}: the {side} refusal line', stream.startswith(prefix),
                  stream[-600:])
        check(f'{case.name}: both sides refused the same way',
              python_stderr.splitlines()[:1] == go_stderr.splitlines()[:1],
              python_stderr.splitlines()[:1] + go_stderr.splitlines()[:1])
    if case.expect == 'accept':
        check(f'{case.name}: both sides accepted', python_status == 0 and go_status == 0,
              f'python {python_status}, go {go_status}\n'
              f'python stderr:\n{python_stderr[-900:]}\ngo stderr:\n{go_stderr[-900:]}')
        check(f'{case.name}: the Python side made requests', python_requests > 0,
              'the recorder saw nothing: an empty request log is not evidence')
        check(f'{case.name}: the Go side made requests', go_requests > 0,
              'the recorder saw nothing: an empty request log is not evidence')
        if case.request_set:
            python_objects = module_requests(python_paths)
            go_objects = module_requests(go_paths)
            check(f'{case.name}: both sides requested module objects',
                  bool(python_objects) and bool(go_objects),
                  f'python {len(python_objects)}, go {len(go_objects)}')
            check(f'{case.name}: the requested module objects are the same set',
                  python_objects == go_objects,
                  'python only: ' + str(sorted(python_objects - go_objects)) +
                  ' go only: ' + str(sorted(go_objects - python_objects)))
    if case.expect == 'refuse':
        check(f'{case.name}: both sides refused non-zero', python_status != 0 and go_status != 0,
              f'python {python_status}, go {go_status}')
    # The forbidden rule is decided for every run, whichever way the verdict went: a
    # refusing case is exactly where a fallback request would hide. The decision is
    # counted, so deleting this block is red rather than silent.
    for side, paths in (('python', python_paths), ('go', go_paths)):
        forbidden = staging_forbidden(paths)
        case.request_decisions += 1
        if case.expect_forbidden:
            check(f'{case.name}: the {side} side asked for the toolchain module',
                  any(path.startswith('/golang.org/toolchain') for path in paths),
                  str(paths[:4]))
            check(f'{case.name}: the {side} toolchain request is on the forbidden list',
                  bool(forbidden), str(paths[:4]))
        else:
            for path in forbidden:
                check(f'{case.name}: the {side} side made a forbidden request', False, path)
    if case.expect == 'ci':
        check(f'{case.name}: CI is refused with status 2 on both sides',
              python_status == 2 and go_status == 2,
              f'python {python_status}, go {go_status}')
        check(f'{case.name}: neither side made a request',
              python_requests == 0 and go_requests == 0,
              f'python {python_requests}, go {go_requests}')
    check(f'{case.name}: the request rule was decided on both sides',
          case.request_decisions == 2, f'{case.request_decisions} decisions')
    compared = compare_trees(case.name, python_tree, out,
                             require_written=case.expect == 'accept')
    if case.out_exists:
        check(f'{case.name}: nothing was written into the existing --out',
              not tree(out) and not tree(python_tree), 'a run wrote into an existing --out')
    return compared, python_status


def request_rule_control() -> int:
    """The request rule's own witness: each forbidden shape is reported, a list is not.

    Without this, "no forbidden request" could be a checker that reports nothing on any
    input. The live arm of the rule is the toolchain case in the table.
    """
    expectations = [
        ('/sumdb/lookup/example.test/dep@v1.0.0', True),
        ('/golang.org/toolchain/@v/v0.0.1-go1.28.0.darwin-arm64.info', True),
        ('/example.test/dep/@latest', True),
        ('/example.test/dep/@v/list', False),
        ('/example.test/dep/@v/v1.0.0.zip', False),
    ]
    for path, forbidden in expectations:
        check(f'the request rule reports {path!r} correctly',
              bool(staging_forbidden([path])) == forbidden, path)
    return len(expectations)


def corpus(fixture, workspace: pathlib.Path) -> list[Case]:
    """The declared table. Every case runs; none is conditional."""
    # The first adoption is also the record later cases derive from, staged once
    # through the real Python stager as the pins suite does.
    adopted = fixture.stage('v0.0.1', name='adopted')
    partial = workspace / 'partial'
    shutil.copytree(adopted, partial)
    (partial / 'pin.json').unlink()
    return [
        Case('first adoption with a dated review', release='v0.0.1',
             review=(fixture_module.REVIEWED_ON, fixture_module.REVIEWER)),
        Case('a rerun of the committed release carries the review', release='v0.0.1',
             current=adopted),
        Case('a graph change without a review retains a proposal', release='v0.0.3',
             current=adopted, expect='refuse'),
        Case('a graph change with a dated review', release='v0.0.3', current=adopted,
             review=('2026-02-03', 'second-reviewer')),
        Case('the adoption restriction refuses an unknown leaf', release='v0.0.2',
             current=adopted, review=('2026-02-04', 'second-reviewer'), expect='refuse'),
        Case('a module without a root notice is an unreviewed layout', release='v0.0.5',
             current=adopted, review=('2026-02-05', 'second-reviewer'), expect='refuse'),
        Case('a module whose only notice is nested is an unreviewed layout',
             release='v0.0.6', current=adopted, review=('2026-02-05', 'second-reviewer'),
             expect='refuse'),
        Case('the signature inventory refuses a version it has not seen', release='v0.0.7',
             current=adopted, review=('2026-02-06', 'second-reviewer'), expect='refuse'),
        Case('a withheld module object fails resolution', release='v0.0.3', current=adopted,
             review=('2026-02-03', 'second-reviewer'),
             withhold=(fixture_module.LEAF, 'v1.0.0', 'zip'), expect='refuse', stderr='child'),
        Case('partial pinned state is refused', release='v0.0.1', current=partial,
             review=(fixture_module.REVIEWED_ON, fixture_module.REVIEWER), expect='refuse'),
        Case('an existing --out is refused', release='v0.0.1', out_exists=True,
             review=(fixture_module.REVIEWED_ON, fixture_module.REVIEWER), expect='refuse'),
        Case('CI refuses before any child', release='v0.0.1', ci=True, expect='ci'),
        # The one case whose recorder must see a path on the forbidden list: `--go`
        # names a compiler this host does not have, so the pinned `go` child asks the
        # proxy for the toolchain module before the writer can refuse. It is the live
        # arm of the request rule; the synthetic control below holds the checker itself.
        Case('a compiler the host does not have asks for its toolchain', release='v0.0.1',
             compiler='1.28.0', expect='refuse', stderr='child',
             stderr_prefix='gopls-closure-update: go env GOVERSION failed (status 1):',
             request_set=False, expect_forbidden=True),
    ]


def deterministic_reproduction(fixture, fixture_binary: pathlib.Path, go_root: pathlib.Path,
                               workspace: pathlib.Path) -> int:
    """The same release staged twice writes the same six files and the same summary."""
    compared = 0
    trees = []
    for run in ('first', 'second'):
        out = workspace / 'deterministic' / run
        arguments = ['--gopls', 'v0.0.1', '--go', fixture_module.PINNED_GO, '--current',
                     str(workspace / 'no-current'), '--out', str(out), '--dockerfile',
                     str(DOCKERFILE), '--review-determination', fixture_module.REVIEWED_ON,
                     '--reviewer', fixture_module.REVIEWER]
        status, _stdout, stderr = go_stage(fixture_binary, fixture, go_root, arguments)
        check(f'deterministic {run} run accepted', status == 0, stderr[-600:])
        trees.append(tree(out))
    first, second = trees
    check('a deterministic reproduction wrote something', bool(first), 'nothing was written')
    for name in sorted(set(first) | set(second)):
        if name not in first or name not in second:
            check(f'deterministic: {name} appears in only one run', False, name)
            continue
        compared += 1
        check(f'deterministic: {name} bytes differ between runs', first[name][0] == second[name][0])
        check(f'deterministic: {name} mode differs between runs', first[name][1] == second[name][1])
    for name in DATA_FILES:
        check(f'deterministic: the staged set carries {name}',
              f'gopls-closure/{name}' in first)
    check('deterministic: the staged set carries the Dockerfile', 'Dockerfile' in first)
    return compared


def same_release(shipped: pathlib.Path, go_root: pathlib.Path) -> int:
    """The reviewed release staged for real by both writers, against the committed six.

    The committed paths are the bytes a same-release stage must reproduce, and the Python
    stager is the oracle for the same run. Needs the reviewed proxy, so it is opt-in.
    """
    import json

    pinned = json.loads((DATA / 'pin.json').read_text())
    release, compiler = pinned['gopls'], pinned['go']
    root = pathlib.Path(tempfile_directory()) / f'gopls-stage-same-release.{os.getpid()}'
    shutil.rmtree(root, ignore_errors=True)
    root.mkdir(parents=True)
    compared = 0
    staged = {}
    for side in ('python', 'go'):
        out = root / side
        shared = ['--gopls', release, '--go', compiler, '--current', str(DATA),
                  '--out', str(out)]
        if side == 'python':
            result = subprocess.run([sys.executable, str(PYTHON_STAGER), 'stage', *shared],
                                    cwd=str(ROOT), env=production_environment(go_root),
                                    capture_output=True, text=True, timeout=3600)
            status, stdout, stderr = result.returncode, result.stdout, result.stderr
        else:
            status, stdout, stderr = go_stage(
                shipped, None, go_root, [*shared, '--dockerfile', str(DOCKERFILE)],
                fixture_build=False)
        check(f'same release ({side}): accepted', status == 0, stderr[-900:])
        if status != 0:
            return compared
        staged[side] = out
        for name in DATA_FILES:
            compared += 1
            check(f'same release ({side}): {name} differs from the committed bytes',
                  (out / 'gopls-closure' / name).read_bytes() == (DATA / name).read_bytes())
        compared += 1
        check(f'same release ({side}): the Dockerfile differs from the committed file',
              (out / 'Dockerfile').read_bytes() == DOCKERFILE.read_bytes())
        check(f'same release ({side}): the run carried the committed review',
              'carried=True' in stdout, stdout[-600:])
    compared += compare_trees('same release', staged['python'], staged['go'], True)
    return compared


def production_environment(go_root: pathlib.Path) -> dict[str, str]:
    """The maintainer environment for the real leg: the pinned go first, and the host's
    transport variables, which carry a route and a CA rather than a content decision."""
    env = {'PATH': f'{go_root / "bin"}:/usr/bin:/bin:/usr/local/bin',
           'HOME': os.environ.get('HOME', '/tmp'), 'LANG': 'C',
           'TMPDIR': os.environ.get('TMPDIR', '/tmp'),
           'PYTHONDONTWRITEBYTECODE': '1'}
    for key in ('HTTP_PROXY', 'HTTPS_PROXY', 'NO_PROXY', 'http_proxy', 'https_proxy',
                'no_proxy', 'SSL_CERT_FILE', 'SSL_CERT_DIR'):
        if key in os.environ:
            env[key] = os.environ[key]
    return env


def tempfile_directory() -> str:
    return os.environ.get('TMPDIR', '/tmp')


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--go-root', required=True,
                        help='the selected Go root (absolute) the fixture children use')
    parser.add_argument('--same-release', action='store_true',
                        help='also stage the reviewed v0.23.0 with the production proxy '
                             '(needs the network; never in the contracts gate)')
    parser.add_argument('--work', help='a scratch directory to reuse (default: a new one)')
    arguments = parser.parse_args()
    go_root = pathlib.Path(arguments.go_root)
    if not go_root.is_absolute() or not (go_root / 'bin' / 'go').is_file():
        print(f'gopls-stage-parity: --go-root must be an absolute Go root: {go_root}',
              file=sys.stderr)
        return 2
    workspace = (pathlib.Path(arguments.work)
                 if arguments.work else pathlib.Path(tempfile_directory())
                 / f'gopls-stage-parity.{os.getpid()}')
    shutil.rmtree(workspace, ignore_errors=True)
    workspace.mkdir(parents=True)
    try:
        guarded_register()
        fixture_binary = build_binary(workspace, 'avm-pin-fixture', fixture=True)
        fixture = fixture_module.build(workspace / 'fixture', go_root)
        try:
            cases = corpus(fixture, workspace)
            check('the corpus is declared', len(cases) >= MINIMUM_CASES,
                  f'{len(cases)} cases, want at least {MINIMUM_CASES}')
            comparisons = 0
            accepts = refusals = 0
            for case in cases:
                compared, python_status = run_case(case, fixture, fixture_binary, go_root,
                                                   workspace)
                comparisons += compared
                if python_status == 0:
                    accepts += 1
                else:
                    refusals += 1
            comparisons += request_rule_control()
            decisions = sum(case.request_decisions for case in cases)
            check('the request rule was decided on both sides of every case',
                  decisions == 2 * len(cases), f'{decisions} decisions for {len(cases)} cases')
            comparisons += deterministic_reproduction(fixture, fixture_binary, go_root,
                                                      workspace)
            check('the table has acceptances', accepts >= MINIMUM_ACCEPTS,
                  f'{accepts} accepted cases, want at least {MINIMUM_ACCEPTS}')
            check('the table has refusals', refusals >= MINIMUM_REFUSALS,
                  f'{refusals} refused cases, want at least {MINIMUM_REFUSALS}')
            check('the comparison is not empty', comparisons >= MINIMUM_COMPARISONS,
                  f'{comparisons} comparisons, want at least {MINIMUM_COMPARISONS}')
        finally:
            fixture.proxy.close()
        if arguments.same_release:
            shipped = build_binary(workspace, 'avm-pin', fixture=False)
            comparisons += same_release(shipped, go_root)
    finally:
        if not Failures:
            shutil.rmtree(workspace, ignore_errors=True)
    if Failures:
        for failure in Failures:
            print(f'gopls-stage-parity: {failure}', file=sys.stderr)
        return 1
    print(f'gopls-stage-parity: {len(cases)} cases, {accepts} accepted, {refusals} refused, '
          f'{comparisons} file/field comparisons, all closed')
    return 0


if __name__ == '__main__':
    sys.exit(main())
