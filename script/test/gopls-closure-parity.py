#!/usr/bin/env python3
"""The differential over the gopls closure reader: Go `check` against Python `check`.

`examples/layers/go-dev/helper` is the layer's own reader (#249); the Python
reader is the live oracle until #254 (owner ruling G5). This suite runs both over
the committed five files and over a mutant corpus that reaches every rule of
`load_closure`, and compares:

  * the status (0 accepted, 1 refused),
  * the exact stdout the consumer parses (`v0.23.0`), byte for byte,
  * the parsed field that stdout carries,
  * and, for a refusal, the rule fragment each side's own diagnostic must name.

A comparison over nothing is a failure (ruling R-a): the case table is declared
here, every case runs, and the totals carry a floor per side, per verdict and for
the declared divergences. A case that cannot run is an error, never a skip.

The declared divergences are the reproductions of #188's G3 defects: this reader
refuses what the Python oracle accepted (or reads what the oracle silently
dropped), and each is asserted with *both* verdicts by name. Changing either side
turns its case red, which is what makes the difference a reviewed statement rather
than a drift. They die with the oracle in #254.

Paths are absolute and resolved (`Path.resolve()`), so a checkout reached through a
symlinked prefix gives the same answer on both sides (ruling R-e). Everything is
read-only over the committed data: each case copies the five files into a fresh
directory and mutates the copy.
"""
from __future__ import annotations

import argparse
import base64
import dataclasses
import hashlib
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile

ROOT = pathlib.Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / 'examples/layers/go-dev'
DATA = EXAMPLE / 'data/gopls-closure'
PYTHON_READER = EXAMPLE / 'gopls_closure.py'
HELPER_BUILDER = ROOT / 'script/build/example-helper.sh'
PIN_FILE = 'pin.json'
ROOT_EVIDENCE_FILE = 'gopls.go.mod'
LICENSE_FILE = 'THIRD_PARTY_LICENSES'
GO_MOD_FILE = 'go.mod'
GO_SUM_FILE = 'go.sum'
ROOT_MODULE = 'golang.org/x/tools/gopls'
FILES = (GO_MOD_FILE, GO_SUM_FILE, ROOT_EVIDENCE_FILE, PIN_FILE, LICENSE_FILE)
PINNED_RELEASE = 'v0.23.0'
PINNED_GO = '1.27.3'

# Floors. A corpus that shrinks below these is not this suite any more: the numbers
# are deliberately below today's counts so an honest edit is a diff, and above zero
# by enough that a broken table cannot pass.
MINIMUM_CASES = 45
MINIMUM_AGREEMENTS = 30
MINIMUM_GO_REFUSALS = 25
MINIMUM_PYTHON_REFUSALS = 20
MINIMUM_ACCEPTS = 4
MINIMUM_DIVERGENCES = 6

ACCEPT = 0
REFUSE = 1


@dataclasses.dataclass(frozen=True)
class Case:
    """One comparison: a mutation, the verdicts, and the fragments that name them."""

    name: str
    mutate: 'Callable[[pathlib.Path], None]'
    go: int
    python: int
    # The rule fragment each side must name. Declared separately where the two
    # readers word the same refusal differently; both are then visible here.
    go_fragment: str = ''
    python_fragment: str = ''
    # Declared divergence: both verdicts are asserted and the reason is recorded in
    # DECLARED_DIVERGENCES. An undeclared difference fails the case.
    divergent: str = ''
    args: tuple[str, ...] = ()

    @property
    def fragments(self) -> tuple[str, str]:
        shared = self.go_fragment or self.python_fragment
        return (self.go_fragment or shared, self.python_fragment or shared)


# --- mutation helpers -------------------------------------------------------
def read(directory: pathlib.Path, name: str) -> bytes:
    return (directory / name).read_bytes()


def write(directory: pathlib.Path, name: str, content: bytes) -> None:
    (directory / name).write_bytes(content)


def rewrite(directory: pathlib.Path, name: str, mutate) -> None:
    before = read(directory, name)
    after = mutate(before)
    assert after != before, f'{name}: the mutation did not apply'
    write(directory, name, after)


def append(directory: pathlib.Path, name: str, text: bytes) -> None:
    rewrite(directory, name, lambda raw: raw + text)


def drop_line(directory: pathlib.Path, name: str, fragment: str) -> None:
    def mutate(raw: bytes) -> bytes:
        lines = raw.split(b'\n')
        kept = [line for line in lines if fragment.encode() not in line]
        assert len(kept) == len(lines) - 1, f'{name}: no single line carries {fragment!r}'
        return b'\n'.join(kept)
    rewrite(directory, name, mutate)


def edit_pin(directory: pathlib.Path, edit) -> None:
    document = json.loads(read(directory, PIN_FILE).decode())
    edit(document)
    write(directory, PIN_FILE, (json.dumps(document, indent=2) + '\n').encode())


def repin(directory: pathlib.Path) -> None:
    """Re-bind the four file digests and the two scope digests.

    `modules[].h1`/`go_mod_h1` and the reviewer/date are deliberately untouched:
    the cases that move them are testing exactly the disagreement an automatic
    repair would hide.
    """
    digests = {name: hashlib.sha256(read(directory, name)).hexdigest()
               for name in (GO_MOD_FILE, GO_SUM_FILE, ROOT_EVIDENCE_FILE, LICENSE_FILE)}

    def edit(document):
        document['files'] = digests
        document['authentication']['scope']['go.mod'] = digests[GO_MOD_FILE]
        document['authentication']['scope']['go.sum'] = digests[GO_SUM_FILE]

    edit_pin(directory, edit)


def h1_of_go_mod(raw: bytes) -> str:
    """The go.sum go.mod h1 of a root module's bytes, as Go computes it."""
    inner = hashlib.sha256(raw).hexdigest()
    digest = hashlib.sha256(f'{inner}  go.mod\n'.encode()).digest()
    return 'h1:' + base64.b64encode(digest).decode()


def repin_root_evidence(directory: pathlib.Path) -> None:
    """Re-bind the committed root evidence to go.sum and to the pin, then re-pin.

    The cases that mutate `gopls.go.mod` must do this or the evidence h1 rule fires
    first on both sides and the rule under test is never reached.
    """
    digest = h1_of_go_mod(read(directory, ROOT_EVIDENCE_FILE))

    def mutate(raw: bytes) -> bytes:
        lines = raw.decode().split('\n')
        replaced = 0
        for index, line in enumerate(lines):
            fields = line.split(' ')
            if len(fields) == 3 and fields[0] == ROOT_MODULE and fields[1].endswith('/go.mod'):
                lines[index] = ' '.join(fields[:2] + [digest])
                replaced += 1
        assert replaced == 1, 'go.sum: no root go.mod line to re-bind'
        return '\n'.join(lines).encode()

    rewrite(directory, GO_SUM_FILE, mutate)

    def edit(document):
        for module in document['modules']:
            if module['path'] == ROOT_MODULE:
                module['go_mod_h1'] = digest

    edit_pin(directory, edit)
    repin(directory)


def notice_frames(directory: pathlib.Path) -> tuple[bytes, list[bytes]]:
    """Split the length-framed evidence into its header and whole frames."""
    raw = read(directory, LICENSE_FILE)
    marker = b'=== '
    first = raw.index(marker)
    header, rest = raw[:first], raw[first:]
    frames = []
    while rest:
        following = rest.find(marker, len(marker))
        if following < 0:
            frames.append(rest)
            break
        frames.append(rest[:following])
        rest = rest[following:]
    assert len(frames) >= 30, f'found {len(frames)} notice frames, want at least 30'
    assert header + b''.join(frames) == raw, 'the frame split does not rejoin'
    return header, frames


def write_frames(directory: pathlib.Path, header: bytes, frames: list[bytes]) -> None:
    write(directory, LICENSE_FILE, header + b''.join(frames))


def frame(path: str, version: str, name: str, body: bytes) -> bytes:
    digest = hashlib.sha256(body).hexdigest()
    return (f'=== {path} {version} {name} bytes={len(body)} sha256={digest}\n'
            .encode() + body + b'\n')


def second_version(directory: pathlib.Path, path: str, version: str, other: str) -> None:
    """Add a second version of one module path, coherently.

    Its frames, its go.sum lines and its pin row all match, and the row is inserted
    *before* the real one, so the only thing that can decide the case is that one
    path appears at two versions.
    """
    header, frames = notice_frames(directory)
    selected = [candidate for candidate in frames
                if candidate.startswith(f'=== {path} {version} '.encode())]
    assert len(selected) == 1, f'no single frame for {path}@{version}'
    moved = selected[0].replace(f'=== {path} {version} '.encode(),
                                f'=== {path} {other} '.encode(), 1)
    write_frames(directory, header, frames + [moved])

    readings = {}

    def add_row(document):
        for index, module in enumerate(document['modules']):
            if module['path'] == path and module['version'] == version:
                readings['h1'] = module['h1']
                readings['go_mod_h1'] = module['go_mod_h1']
                duplicate = dict(module)
                duplicate['version'] = other
                # Immediately before the real row, so the path order the Python
                # reader also checks is untouched and the only rule in question is
                # the one-version-per-path rule.
                document['modules'].insert(index, duplicate)
                return
        raise AssertionError(f'the pin does not select {path}@{version}')

    edit_pin(directory, add_row)

    def add_sum(raw: bytes) -> bytes:
        lines = raw.decode().split('\n')
        inserted = False
        kept = []
        for line in lines:
            if not inserted and line.startswith(f'{path} {version} '):
                kept.append(f'{path} {other} {readings["h1"]}')
                kept.append(f'{path} {other}/go.mod {readings["go_mod_h1"]}')
                inserted = True
            kept.append(line)
        assert inserted, f'go.sum: no lines for {path} {version}'
        return '\n'.join(kept).encode()

    rewrite(directory, GO_SUM_FILE, add_sum)


def replace_digest_character(directory: pathlib.Path, prefix: str) -> None:
    def mutate(raw: bytes) -> bytes:
        for line in raw.decode().split('\n'):
            if not line.startswith(prefix):
                continue
            digest = line[len(prefix):]
            replacement = 'B' if digest.startswith('A') else 'A'
            return raw.replace(line.encode(), (prefix + replacement + digest[1:]).encode(), 1)
        raise AssertionError(f'no line named by {prefix!r}')

    rewrite(directory, GO_SUM_FILE, mutate)


# --- the corpus -------------------------------------------------------------
def corpus() -> list[Case]:
    cases: list[Case] = []
    add = cases.append

    # Accepted: the positive control, and the Dockerfile's two assertions.
    add(Case('committed data', lambda directory: None, ACCEPT, ACCEPT))
    add(Case('committed data with the recipe assertions', lambda directory: None, ACCEPT,
             ACCEPT, args=('--expect-gopls', PINNED_RELEASE, '--expect-go', PINNED_GO)))
    add(Case('committed data with the release assertion alone', lambda directory: None,
             ACCEPT, ACCEPT, args=('--expect-gopls', PINNED_RELEASE)))
    add(Case('committed data with the compiler assertion alone', lambda directory: None,
             ACCEPT, ACCEPT, args=('--expect-go', PINNED_GO)))

    # The input set: five regular files, nothing else.
    add(Case('a missing input', lambda directory: (directory / GO_SUM_FILE).unlink(),
             REFUSE, REFUSE, 'missing input: go.sum'))
    add(Case('an unreviewed extra input',
             lambda directory: write(directory, '.extra', b'unexpected'), REFUSE, REFUSE,
             'unreviewed input: .extra'))
    add(Case('a missing input beside an unreviewed one',
             lambda directory: ((directory / GO_SUM_FILE).unlink(),
                                write(directory, '.extra', b'unexpected')),
             REFUSE, REFUSE, 'missing input: go.sum'))
    add(Case('a symlinked input', symlink_input, REFUSE, REFUSE, 'symlink go.sum'))
    add(Case('a directory where an input belongs', directory_input, REFUSE, REFUSE,
             'not a regular file: pin.json'))

    # The pin document's grammar.
    add(Case('an unknown pin key',
             lambda directory: edit_pin(directory,
                                        lambda document: document.update(extra=True)),
             REFUSE, REFUSE, "pin.json: unknown key 'extra'"))
    add(Case('a missing pin key',
             lambda directory: edit_pin(directory,
                                        lambda document: document.pop('gopls')),
             REFUSE, REFUSE, "pin.json: missing 'gopls'"))
    add(Case('schema written as a float',
             lambda directory: rewrite(directory, PIN_FILE,
                                       lambda raw: raw.replace(b'"schema": 1',
                                                               b'"schema": 1.0', 1)),
             REFUSE, REFUSE, 'schema: expected an integer'))
    add(Case('schema written as a boolean',
             lambda directory: rewrite(directory, PIN_FILE,
                                       lambda raw: raw.replace(b'"schema": 1',
                                                               b'"schema": true', 1)),
             REFUSE, REFUSE, 'schema: expected an integer'))
    add(Case('a duplicate pin key',
             lambda directory: rewrite(directory, PIN_FILE,
                                       lambda raw: raw.replace(
                                           b'"gopls": "v0.23.0"',
                                           b'"gopls": "v0.23.0", "gopls": "v0.23.0"', 1)),
             REFUSE, REFUSE, "duplicate key 'gopls'"))
    add(Case('a leading byte-order mark',
             lambda directory: rewrite(directory, PIN_FILE,
                                       lambda raw: b'\xef\xbb\xbf' + raw),
             REFUSE, REFUSE, 'leading UTF-8 BOM is not accepted'))
    add(Case('a NaN constant',
             lambda directory: rewrite(directory, PIN_FILE,
                                       lambda raw: raw.replace(b'"schema": 1',
                                                               b'"schema": NaN', 1)),
             REFUSE, REFUSE, 'not valid JSON'))
    add(Case('the wrong wrapper module',
             lambda directory: edit_pin(directory, lambda document: document.update(
                 module='agent-vm.invalid/other')),
             REFUSE, REFUSE, 'module must be agent-vm.invalid/go-dev/gopls-closure'))
    add(Case('a release that is not an exact version',
             lambda directory: edit_pin(directory, lambda document: document.update(
                 gopls='v0.23')),
             REFUSE, REFUSE, 'gopls: expected an exact vX.Y.Z release version'))
    add(Case('an unsupported schema',
             lambda directory: edit_pin(directory, lambda document: document.update(
                 schema=2)),
             REFUSE, REFUSE, 'pin.json: unsupported schema'))

    # The four digests the pin carries.
    for name in (GO_MOD_FILE, GO_SUM_FILE, ROOT_EVIDENCE_FILE, LICENSE_FILE):
        add(Case(f'changed bytes in {name}',
                 lambda directory, name=name: append(directory, name, b'\n'),
                 REFUSE, REFUSE, f'{name}: bytes differ from the committed digest'))

    # The wrapper's canonical form.
    add(Case('a replace directive in the wrapper', wrapper_replace, REFUSE, REFUSE,
             'directive not allowed: replace'))
    add(Case('a toolchain directive in the wrapper', wrapper_toolchain, REFUSE, REFUSE,
             'directive not allowed: toolchain'))
    add(Case('an unfinished require block in the wrapper', wrapper_unfinished, REFUSE,
             REFUSE, 'go.mod is not in the reviewed canonical form'))
    add(Case('a wrapper module directive that is not the wrapper', wrapper_module, REFUSE,
             REFUSE, 'go.mod: module directive must be'))
    add(Case('a wrapper tool that is not gopls', wrapper_tool, REFUSE, REFUSE,
             'go.mod: tool directive must be exactly ' + ROOT_MODULE))
    add(Case('a require block out of sorted order', wrapper_unsorted, REFUSE, REFUSE,
             'go.mod is not in the reviewed canonical form'))

    # Both checksum kinds per selected module, and Go's line order.
    add(Case('the root zip h1 removed',
             lambda directory: (drop_line(directory, GO_SUM_FILE,
                                          f'{ROOT_MODULE} {PINNED_RELEASE} h1:'),
                                repin(directory)),
             REFUSE, REFUSE, f'go.sum lacks h1 for {ROOT_MODULE}@{PINNED_RELEASE}'))
    add(Case('the root go.mod h1 removed',
             lambda directory: (drop_line(directory, GO_SUM_FILE,
                                          f'{ROOT_MODULE} {PINNED_RELEASE}/go.mod h1:'),
                                repin(directory)),
             REFUSE, REFUSE, f'go.sum lacks go.mod hash for {ROOT_MODULE}@{PINNED_RELEASE}'))
    add(Case('a module go.mod h1 moved', lambda directory: (
        replace_digest_character(directory, 'github.com/fatih/camelcase v1.0.0/go.mod h1:'),
        repin(directory)), REFUSE, REFUSE, 'go.mod h1 differs from go.sum'))
    add(Case('a conflicting duplicate sum', lambda directory: (
        append(directory, GO_SUM_FILE,
               b'github.com/fatih/camelcase v1.0.0 '
               b'h1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\n'), repin(directory)),
             REFUSE, REFUSE, 'conflicting'))
    add(Case('two sums out of semver order', lambda directory: (
        swap_adjacent_sums(directory), repin(directory)),
             REFUSE, REFUSE, 'unsorted or duplicated entries'))
    add(Case('a malformed sum line', lambda directory: (
        append(directory, GO_SUM_FILE, b'mvdan.cc/gofumpt v0.10.0\n'), repin(directory)),
             REFUSE, REFUSE, 'malformed line'))

    # The independent root evidence h1.
    add(Case('root evidence bytes moved after its digest', root_evidence_desync, REFUSE,
             REFUSE, 'root go.mod evidence does not match go.sum'))

    # Adoption at matching root versions, and the compiler/language relation.
    add(Case('a selected version the root does not require', adoption_version, REFUSE,
             REFUSE, "differs from the root module's requirement"))
    add(Case('a selected module the root does not name', adoption_absent, REFUSE, REFUSE,
             'is not required by the root module'))
    add(Case('a wrapper go directive that disagrees', wrapper_go_mismatch, REFUSE, REFUSE,
             'does not equal the root module'))
    add(Case('a language newer than the compiler', language_too_new, REFUSE, REFUSE,
             'is newer than the pinned compiler'))

    # The module list and the notice evidence.
    add(Case('modules out of path order', modules_unsorted, REFUSE, REFUSE,
             'pin.json: modules must be sorted by path'))
    add(Case('the same module row twice', modules_duplicate_row, REFUSE, REFUSE,
             'pin.json: duplicate module'))
    add(Case('a notice body that breaks its frame digest', notice_body_drift, REFUSE,
             REFUSE, 'license evidence differs from the committed digest'))
    add(Case('an unreviewed nested notice layout', notice_nested_layout, REFUSE, REFUSE,
             'unsupported license layout'))
    add(Case('a notice header naming another release', notice_header, REFUSE, REFUSE,
             'header disagrees with the pinned release'))
    add(Case('a truncated final notice frame', notice_truncated, REFUSE, REFUSE,
             'section is not newline-terminated'))

    # The dated, scoped review.
    for name, mutate, fragment in [
        ('a scope naming another root', scope_root, 'scope root'),
        ('a scope that does not cover the committed sums', scope_sums,
         'digests differ from the committed files'),
        ('a date that is not a calendar day', review_date,
         'not a calendar date: 2026-02-30'),
        ('an unsupported determination', determination, 'unsupported determination'),
        ('a sumdb that is not the reviewed key', sumdb,
         'sumdb is not the reviewed verifier key'),
        ('empty evidence', empty_evidence, 'evidence must be recorded'),
        ('a signature claimed as verified', signature_verified,
         'does not verify VCS signatures'),
        ('a signature row that outlived its version', signature_version, 'but the closure selects'),
        ('an origin that is not this release', provenance_source,
         'not the canonical proxy .info origin'),
        ('an authenticated provenance claim', provenance_authenticated,
         'unauthenticated metadata'),
    ]:
        add(Case(name, lambda directory, mutate=mutate: mutate(directory), REFUSE, REFUSE,
                 fragment))

    # The caller's assertions.
    add(Case('a wrong gopls assertion', lambda directory: None, REFUSE, REFUSE,
             f'asserted gopls version v0.24.0 != pinned {PINNED_RELEASE}',
             args=('--expect-gopls', 'v0.24.0')))
    add(Case('a wrong Go assertion', lambda directory: None, REFUSE, REFUSE,
             f'pin generated with go{PINNED_GO}; the layer installs go1.27.4',
             args=('--expect-go', '1.27.4')))
    add(Case('a malformed gopls assertion', lambda directory: None, REFUSE, REFUSE,
             '--expect-gopls: expected an exact vX.Y.Z release version',
             args=('--expect-gopls', 'v0.23')))

    # The declared divergences: #188 G3, reproduced and fixed here.
    add(Case('DIVERGE: root evidence names another module', root_evidence_module,
             REFUSE, ACCEPT, divergent=DECLARED_DIVERGENCES['root module directive'],
             args=()))
    add(Case('DIVERGE: root evidence has an unfinished require block',
             root_evidence_unfinished, REFUSE, ACCEPT,
             divergent=DECLARED_DIVERGENCES['unfinished require block']))
    add(Case('DIVERGE: root evidence carries a tab-separated require entry',
             root_evidence_tab, ACCEPT, REFUSE,
             divergent=DECLARED_DIVERGENCES['tab-separated directive']))
    add(Case('DIVERGE: a duplicated framed notice', notice_duplicate_frame, REFUSE,
             ACCEPT, divergent=DECLARED_DIVERGENCES['duplicate framed notice']))
    add(Case('DIVERGE: a duplicate notice name in one module', notice_duplicate_name,
             REFUSE, ACCEPT, divergent=DECLARED_DIVERGENCES['duplicate notice name']))
    add(Case('DIVERGE: two versions of one module path', module_two_versions, REFUSE,
             ACCEPT, divergent=DECLARED_DIVERGENCES['two versions of one path']))
    add(Case('DIVERGE: an explicitly empty gopls assertion', lambda directory: None,
             REFUSE, ACCEPT, args=('--expect-gopls', ''),
             divergent=DECLARED_DIVERGENCES['empty assertion']))
    return cases


DECLARED_DIVERGENCES = {
    'root module directive': 'the Python evidence reader never reads the module '
                             'directive, so a wrong one passed; this reader refuses it (#188 G3)',
    'unfinished require block': 'the Python evidence reader never checks that a require '
                                'block was closed; this reader refuses it (#188 G3)',
    'tab-separated directive': 'the Python evidence reader split the directive on one '
                              'space, so a tab-separated require entry was invisible '
                              'and its module was then refused as unadopted; this reader '
                              'reads it as Go does (#188 G3)',
    'duplicate framed notice': 'the Python loader indexed framed notices by identity, so '
                               'a duplicate frame collapsed and the set still balanced; '
                               'this reader refuses the duplicate (#188 G3)',
    'duplicate notice name': 'the Python loader keyed a module\'s notice rows by name, so '
                             'a repeated row collapsed; this reader refuses it (#188 G3)',
    'two versions of one path': 'the Python loader keyed the selection by module path, so '
                                'a second version of one path vanished before the version '
                                'comparisons; this reader refuses it (#188 G3)',
    'empty assertion': 'argparse defaults --expect-gopls to None and the Python reader '
                       'tests truthiness, so an explicitly empty value means "unset"; '
                       'the Go flag is a pointer, so an empty assertion fails closed',
}


def scope_root(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['scope']['root'] = f'{ROOT_MODULE}@v0.22.0'

    edit_pin(directory, edit)
    repin(directory)


def scope_sums(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['scope']['go.sum'] = '0' * 64

    edit_pin(directory, edit)


def review_date(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['reviewed_on'] = '2026-02-30'

    edit_pin(directory, edit)
    repin(directory)


def determination(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['determination'] = 'trust-me-v1'

    edit_pin(directory, edit)
    repin(directory)


def sumdb(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['sumdb'] = 'sum.golang.org'

    edit_pin(directory, edit)
    repin(directory)


def empty_evidence(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['evidence'] = []

    edit_pin(directory, edit)
    repin(directory)


def signature_verified(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['known_vcs_signatures'][0]['verified'] = True

    edit_pin(directory, edit)
    repin(directory)


def signature_version(directory: pathlib.Path) -> None:
    def edit(document):
        document['authentication']['known_vcs_signatures'][0]['version'] = 'v1.7.0'

    edit_pin(directory, edit)
    repin(directory)


def provenance_source(directory: pathlib.Path) -> None:
    def edit(document):
        document['provenance']['source'] = (
            f'https://proxy.golang.org/{ROOT_MODULE}/@v/v0.22.0.info')

    edit_pin(directory, edit)
    repin(directory)


def provenance_authenticated(directory: pathlib.Path) -> None:
    def edit(document):
        document['provenance']['authenticated'] = True

    edit_pin(directory, edit)
    repin(directory)


def symlink_input(directory: pathlib.Path) -> None:
    path = directory / GO_SUM_FILE
    path.unlink()
    path.symlink_to(GO_MOD_FILE)


def directory_input(directory: pathlib.Path) -> None:
    path = directory / PIN_FILE
    path.unlink()
    path.mkdir()


def wrapper_replace(directory: pathlib.Path) -> None:
    append(directory, GO_MOD_FILE,
           b'\nreplace github.com/fatih/camelcase => github.com/fatih/camelcase v1.0.0\n')
    repin(directory)


def wrapper_toolchain(directory: pathlib.Path) -> None:
    append(directory, GO_MOD_FILE, b'\ntoolchain go1.99.0\n')
    repin(directory)


def wrapper_unfinished(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        text = raw.decode()
        index = text.rindex(')')
        return (text[:index] + text[index + 1:]).encode()

    rewrite(directory, GO_MOD_FILE, mutate)
    repin(directory)


def wrapper_module(directory: pathlib.Path) -> None:
    rewrite(directory, GO_MOD_FILE,
            lambda raw: raw.replace(b'module agent-vm.invalid/go-dev/gopls-closure',
                                    b'module agent-vm.invalid/other', 1))
    repin(directory)


def wrapper_tool(directory: pathlib.Path) -> None:
    rewrite(directory, GO_MOD_FILE,
            lambda raw: raw.replace(f'tool {ROOT_MODULE}'.encode(),
                                    f'tool {ROOT_MODULE}d'.encode(), 1))
    repin(directory)


def wrapper_unsorted(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        lines = raw.decode().split('\n')
        for index in range(len(lines) - 1):
            if lines[index].startswith('\t') and lines[index + 1].startswith('\t'):
                lines[index], lines[index + 1] = lines[index + 1], lines[index]
                return '\n'.join(lines).encode()
        raise AssertionError('no two adjacent requires to swap')

    rewrite(directory, GO_MOD_FILE, mutate)
    repin(directory)


def swap_adjacent_sums(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        lines = raw.decode().split('\n')
        for index in range(len(lines) - 1):
            first, second = lines[index], lines[index + 1]
            if first.startswith('github.com/fatih/camelcase ') and second.startswith(
                    'github.com/fatih/camelcase '):
                lines[index], lines[index + 1] = second, first
                return '\n'.join(lines).encode()
        raise AssertionError('no two adjacent sums for one module to swap')

    rewrite(directory, GO_SUM_FILE, mutate)


def root_evidence_desync(directory: pathlib.Path) -> None:
    append(directory, ROOT_EVIDENCE_FILE, b'\n// re-pinned evidence only\n')
    repin(directory)


def adoption_version(directory: pathlib.Path) -> None:
    rewrite(directory, GO_MOD_FILE,
            lambda raw: raw.replace(b'\tgithub.com/fsnotify/fsnotify v1.9.0',
                                    b'\tgithub.com/fsnotify/fsnotify v1.10.0', 1))
    repin(directory)


def adoption_absent(directory: pathlib.Path) -> None:
    rewrite(directory, GO_MOD_FILE,
            lambda raw: raw.replace(b'\tgithub.com/fatih/structtag v1.2.0',
                                    b'\tgithub.com/fatih/structtagx v1.2.0', 1))
    repin(directory)


def wrapper_go_mismatch(directory: pathlib.Path) -> None:
    rewrite(directory, GO_MOD_FILE,
            lambda raw: raw.replace(b'\ngo 1.26.0\n', b'\ngo 1.26.1\n', 1))
    repin(directory)


def language_too_new(directory: pathlib.Path) -> None:
    for name in (GO_MOD_FILE, ROOT_EVIDENCE_FILE):
        rewrite(directory, name, lambda raw: raw.replace(b'\ngo 1.26.0\n',
                                                         b'\ngo 1.99.0\n', 1))
    repin_root_evidence(directory)


def modules_unsorted(directory: pathlib.Path) -> None:
    def swap(document):
        modules = document['modules']
        modules[0], modules[1] = modules[1], modules[0]

    edit_pin(directory, swap)
    repin(directory)


def modules_duplicate_row(directory: pathlib.Path) -> None:
    def duplicate(document):
        document['modules'].append(document['modules'][0])

    edit_pin(directory, duplicate)
    repin(directory)


def notice_body_drift(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        header, frames = notice_frames(directory)
        end = frames[0].index(b'\n') + 1
        first = bytearray(frames[0])
        first[end] ^= 0x20
        return header + bytes(first) + b''.join(frames[1:])

    rewrite(directory, LICENSE_FILE, mutate)
    repin(directory)


def notice_nested_layout(directory: pathlib.Path) -> None:
    header, frames = notice_frames(directory)
    body = b'nested notice bytes\n'
    write_frames(directory, header, frames + [
        frame('github.com/fatih/camelcase', 'v1.0.0', 'sub/LICENSE', body)])
    digest = hashlib.sha256(body).hexdigest()

    def edit(document):
        for module in document['modules']:
            if (module['path'], module['version']) == ('github.com/fatih/camelcase',
                                                       'v1.0.0'):
                module['licenses'].append({'name': 'sub/LICENSE', 'sha256': digest})
                return
        raise AssertionError('the pin does not select the module the notice frames')

    edit_pin(directory, edit)
    repin(directory)


def notice_header(directory: pathlib.Path) -> None:
    rewrite(directory, LICENSE_FILE,
            lambda raw: raw.replace(f'for {ROOT_MODULE} v0.23.0'.encode(),
                                    f'for {ROOT_MODULE} v0.22.0'.encode(), 1))
    repin(directory)


def notice_truncated(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        return raw[:-1]

    rewrite(directory, LICENSE_FILE, mutate)
    repin(directory)


def root_evidence_module(directory: pathlib.Path) -> None:
    rewrite(directory, ROOT_EVIDENCE_FILE,
            lambda raw: raw.replace(f'module {ROOT_MODULE}'.encode(),
                                    f'module {ROOT_MODULE}x'.encode(), 1))
    repin_root_evidence(directory)


def root_evidence_unfinished(directory: pathlib.Path) -> None:
    def mutate(raw: bytes) -> bytes:
        text = raw.decode()
        index = text.rindex(')')
        return (text[:index] + text[index + 1:]).encode()

    rewrite(directory, ROOT_EVIDENCE_FILE, mutate)
    repin_root_evidence(directory)


def root_evidence_tab(directory: pathlib.Path) -> None:
    rewrite(directory, ROOT_EVIDENCE_FILE,
            lambda raw: raw.replace(b'\tgithub.com/fsnotify/fsnotify v1.9.0',
                                    b'\tgithub.com/fsnotify/fsnotify\tv1.9.0', 1))
    repin_root_evidence(directory)


def notice_duplicate_frame(directory: pathlib.Path) -> None:
    header, frames = notice_frames(directory)
    write_frames(directory, header, frames + [frames[0]])
    repin(directory)


def notice_duplicate_name(directory: pathlib.Path) -> None:
    def duplicate(document):
        for module in document['modules']:
            if module['path'] == 'github.com/fatih/camelcase':
                module['licenses'].append(module['licenses'][0])
                return
        raise AssertionError('the pin does not select the camelcase module')

    edit_pin(directory, duplicate)
    repin(directory)


def module_two_versions(directory: pathlib.Path) -> None:
    second_version(directory, 'github.com/fatih/camelcase', 'v1.0.0', 'v0.0.1')
    repin(directory)


# --- running the two readers -------------------------------------------------
def build_helper() -> pathlib.Path:
    """Build the layer's own reader for this host, with the stage's flags.

    The builder is the one place the host flags are written down, and it prints the
    absolute path of the binary it wrote. A build that fails or prints a path that
    does not exist is an error: this suite has no second way to get a reader.
    """
    os_name = 'linux' if sys.platform.startswith('linux') else 'darwin'
    result = subprocess.run(
        ['bash', str(HELPER_BUILDER), 'go-dev', '--root', str(ROOT), '--os', os_name],
        capture_output=True, text=True, check=False, cwd=ROOT)
    if result.returncode != 0:
        raise SystemExit(f'gopls-closure-parity: cannot build the host helper:\n'
                         f'{result.stdout}{result.stderr}')
    binary = pathlib.Path(result.stdout.strip().splitlines()[-1])
    if not binary.is_absolute():
        raise SystemExit(f'gopls-closure-parity: the builder printed a relative path: {binary}')
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise SystemExit(f'gopls-closure-parity: the builder printed a path with no '
                         f'executable: {binary}')
    return binary


@dataclasses.dataclass(frozen=True)
class Observation:
    status: int
    stdout: str
    stderr: str


def run(program: list[str], data: pathlib.Path, args: tuple[str, ...]) -> Observation:
    environment = dict(os.environ)
    environment['PYTHONDONTWRITEBYTECODE'] = '1'
    result = subprocess.run(program + ['check', str(data), *args], capture_output=True,
                            text=True, check=False, cwd=ROOT, env=environment)
    return Observation(result.returncode, result.stdout, result.stderr)


def failures_for(case: Case, go: Observation, python: Observation) -> list[str]:
    """Every way this case disagrees with what it declared."""
    problems: list[str] = []
    if go.status != case.go:
        problems.append(f'go status {go.status}, want {case.go} ({go.stderr.strip()})')
    if python.status != case.python:
        problems.append(f'python status {python.status}, want {case.python} '
                        f'({python.stderr.strip()})')
    if case.go == case.python:
        # An agreement: the two readers must produce the same bytes on the stream
        # the consumer parses, and neither may say anything on the other stream when
        # it accepts.
        if go.stdout != python.stdout:
            problems.append(f'stdout differs: go {go.stdout!r}, python {python.stdout!r}')
    for side, observation, verdict in (('go', go, case.go), ('python', python, case.python)):
        if verdict == ACCEPT:
            if observation.stdout != f'{PINNED_RELEASE}\n':
                problems.append(f'{side} accepted with stdout {observation.stdout!r}, want '
                                f'the selected release {PINNED_RELEASE!r}')
            if observation.stderr != '':
                problems.append(f'{side} accepted but wrote stderr {observation.stderr!r}')
        else:
            if observation.stdout != '':
                problems.append(f'{side} refused but wrote stdout {observation.stdout!r}')
    go_fragment, python_fragment = case.fragments
    if case.go == REFUSE and go_fragment and go_fragment not in go.stderr:
        problems.append(f'go refusal {go.stderr.strip()!r} does not name {go_fragment!r}')
    if case.python == REFUSE and python_fragment and python_fragment not in python.stderr:
        problems.append(f'python refusal {python.stderr.strip()!r} does not name '
                        f'{python_fragment!r}')
    if go.status != python.status and not case.divergent:
        problems.append('an undeclared divergence: the table declares the two verdicts equal')
    return problems


def check_comparator_control(helper: pathlib.Path, scratch: pathlib.Path) -> str:
    """The comparison must be able to fail.

    A comparator that reports "agree" for two readings it was told to expect
    differently is measuring nothing, so one case is run with a deliberately wrong
    expectation and must produce a failure listing.
    """
    directory = scratch / 'control'
    shutil.copytree(DATA, directory)
    go = run([str(helper)], directory, ())
    python = run([sys.executable, str(PYTHON_READER)], directory, ())
    wrong = Case('control', lambda path: None, REFUSE, ACCEPT)
    if not failures_for(wrong, go, python):
        raise SystemExit('gopls-closure-parity: the comparator accepted a case whose '
                         'expectations contradict both readers; it cannot fail, so it '
                         'cannot pass')
    return 'control: the comparator reports a disagreement against a wrong expectation'


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument('--keep-scratch', action='store_true',
                       help='keep the mutant trees for inspection')
    arguments = parser.parse_args(argv)
    for required in (PYTHON_READER, DATA / PIN_FILE):
        if not required.is_file():
            raise SystemExit(f'gopls-closure-parity: missing {required}')
    helper = build_helper()
    scratch = pathlib.Path(tempfile.mkdtemp(prefix='gopls-closure-parity.'))
    cases = corpus()
    # The table is a declared set: a duplicated name would run one comparison twice
    # and hide a missing rule.
    names = [case.name for case in cases]
    if len(set(names)) != len(names):
        raise SystemExit('gopls-closure-parity: duplicate case names')
    declared = {case.name for case in cases if case.divergent}
    reasons = set(DECLARED_DIVERGENCES.values())
    for case in cases:
        if case.divergent and case.divergent not in reasons:
            raise SystemExit(f'{case.name}: divergence reason is not in the register')
    try:
        control = check_comparator_control(helper, scratch)
        comparisons = 0
        agreements = 0
        divergences = 0
        go_refusals = python_refusals = 0
        accepts = 0
        problems: list[str] = []
        for index, case in enumerate(cases, start=1):
            directory = scratch / f'case-{index:03d}'
            shutil.copytree(DATA, directory)
            case.mutate(directory)
            go = run([str(helper)], directory, case.args)
            python = run([sys.executable, str(PYTHON_READER)], directory, case.args)
            comparisons += 1
            if go.status == ACCEPT:
                accepts += 1
            if go.status == REFUSE:
                go_refusals += 1
            if python.status == REFUSE:
                python_refusals += 1
            if case.go == case.python:
                agreements += 1
            else:
                divergences += 1
            found = failures_for(case, go, python)
            verdict = 'agree' if case.go == case.python else 'diverges'
            detail = (f'go={go.status} python={python.status}')
            if found:
                problems.append(f'{case.name} ({verdict} {detail})')
                problems.extend(f'  {problem}' for problem in found)
                print(f'case {index}/{len(cases)} FAIL {case.name} [{detail}]')
            else:
                print(f'case {index}/{len(cases)} ok   {case.name} [{verdict} {detail}]')
        print(control)
        print(f'cases {comparisons} agreements {agreements} declared divergences '
              f'{divergences} go-refusals {go_refusals} python-refusals {python_refusals} '
              f'accepts {accepts}')
        # Floors. A comparison over nothing is a failure, not a pass (ruling R-a).
        if comparisons < MINIMUM_CASES:
            problems.append(f'compared {comparisons} cases, want at least {MINIMUM_CASES}')
        if comparisons != len(cases):
            problems.append(f'compared {comparisons} of {len(cases)} cases')
        if agreements < MINIMUM_AGREEMENTS:
            problems.append(f'{agreements} agreements, want at least {MINIMUM_AGREEMENTS}')
        if go_refusals < MINIMUM_GO_REFUSALS:
            problems.append(f'{go_refusals} Go refusals, want at least {MINIMUM_GO_REFUSALS}')
        if python_refusals < MINIMUM_PYTHON_REFUSALS:
            problems.append(f'{python_refusals} Python refusals, want at least '
                            f'{MINIMUM_PYTHON_REFUSALS}')
        if accepts < MINIMUM_ACCEPTS:
            problems.append(f'{accepts} Go accepts, want at least {MINIMUM_ACCEPTS}: a corpus '
                            f'whose every case refuses proves only that both refuse')
        if divergences != len(declared) or len(declared) < MINIMUM_DIVERGENCES:
            problems.append(f'{divergences} divergences observed, {len(declared)} declared, '
                            f'want at least {MINIMUM_DIVERGENCES}: the #188 G3 witnesses must '
                            f'both still diverge')
        if problems:
            print('gopls-closure-parity: FAIL', file=sys.stderr)
            for problem in problems:
                print(problem, file=sys.stderr)
            print(f'scratch: {scratch}', file=sys.stderr)
            return 1
    finally:
        if not arguments.keep_scratch and sys.exc_info()[0] is None:
            shutil.rmtree(scratch, ignore_errors=True)
    print(f'gopls-closure-parity: {comparisons} comparisons, {divergences} declared '
          f'divergences, all agreements and floors held')
    return 0


if __name__ == '__main__':
    sys.exit(main())
