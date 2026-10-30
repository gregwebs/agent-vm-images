#!/usr/bin/env python3
"""Drive `archive.py`'s own API where the CLI cannot reach it.

Two things are proved here and nowhere else.

*Scaled budgets.* The production budgets are sized for a real npm tarball —
200_000 headers, 1 GiB of payload, a little over 1 GiB decompressed, 1 MiB of
extended headers — and a tarball that cheaply exceeds them cannot be built in a
test. Scaling the module's own constants keeps the refusal hermetic while the
production code path stays the only code under test: nothing here reimplements a
check, and `peak` is the traced high-water mark of the whole run, so a refusal
that happens after `tarfile` allocated a declared buffer is visible as a large
peak next to a refusal code. `production` applies no scaling, which is how a
*cheap* hostile archive (a 64 MiB PAX value) is measured against the real limits.

*The write walk.* `walk` plants a link in the extraction root and asks the writer
for a member whose parent component is that link. The refusal must come from the
`dir_fd`/`O_NOFOLLOW` descent itself, so it holds on a case-sensitive filesystem
and for a name spelling the preflight never saw.

`importlib` loads the module by path under another name so that the scaled
constants belong to this process only.
"""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import tracemalloc

ARCHIVE = Path(__file__).resolve().parents[3] / 'images' / 'tools' / 'pi' / 'archive.py'
MIB = 1024 * 1024

BUDGETS = {
    'root-count': {'MAX_MEMBERS': 2},
    'root-size': {'MAX_TOTAL_BYTES': MIB},
    'pax-bomb': {'MAX_DECOMPRESSED_BYTES': MIB, 'MAX_READ_BYTES': MIB},
    'production': {},
}


def load(budgets):
    spec = importlib.util.spec_from_file_location('pi_archive_budget', ARCHIVE)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    for name, value in budgets.items():
        if not hasattr(module, name):
            raise SystemExit(f'pi-archive-budget: archive.py has no budget called {name}')
        setattr(module, name, value)
    return module


def walk(module):
    """Every way a write could descend through a link, refused by the walk itself.

    The tree is built here, not from an archive: a member whose parent component is
    a link never reaches extraction, because the preflight refuses a link and a
    member below it as an ancestor overwrite, and the case/normalisation collision
    that used to hide such a member is refused too. What is left to prove is that
    the *writer* would refuse it as well, which is what makes the collision check
    backstop rather than the only defence.
    """
    root = Path(tempfile.mkdtemp())
    outside = root.parent / (root.name + '-outside')
    root = root / 'tree'
    root.mkdir()
    outside.mkdir()
    os.symlink('..', root / 'escape')
    os.symlink(os.fspath(outside), root / 'outside')
    tree = module._ExtractionTree(root)
    results = {}
    try:
        for label, attempt in (
                ('directory-through-link', lambda: tree.directory('escape/sub')),
                ('file-through-link', lambda: tree.file('escape/file', 0o644)),
                ('outside-link', lambda: tree.file('outside/file', 0o644))):
            try:
                fd = attempt()
            except module.ArchiveError as error:
                results[label] = error.code
                continue
            results[label] = 'accepted'
            if isinstance(fd, int):
                os.close(fd)
    finally:
        tree.close()
    results['escaped'] = sorted(path.name for path in outside.iterdir())
    results['rootEntries'] = sorted(path.name for path in root.iterdir())
    sys.stdout.write(json.dumps(results) + '\n')
    return 0


def main(argv):
    if len(argv) == 1 and argv[0] == 'walk':
        return walk(load({}))
    if len(argv) != 3 or argv[0] != 'run':
        sys.stderr.write('usage: pi-archive-budget.py run CASE TARBALL | walk\n')
        return 2
    case, tarball = argv[1], Path(argv[2])
    module = load(BUDGETS[case])
    destination = Path(tempfile.mkdtemp()) / 'out'
    tracemalloc.start()
    code = 'accepted'
    try:
        module.extract(tarball, destination)
    except module.ArchiveError as error:
        code = error.code
    peak = tracemalloc.get_traced_memory()[1]
    tracemalloc.stop()
    sys.stdout.write(json.dumps({'code': code, 'peak': peak}) + '\n')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
