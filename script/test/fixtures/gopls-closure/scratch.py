#!/usr/bin/env python3
"""Remove a test's scratch tree, including what a real Go build made read-only.

Go writes its module cache and its build cache with directories at mode 0555 and
files at 0444, so a plain ``shutil.rmtree`` cannot unlink them and
``ignore_errors=True`` then reports success while the tree stays on disk. Every
gopls suite here runs a real ``go``, so every one of them leaked its scratch tree
until this helper existed (the product's own ``gopls_closure._remove_scratch``
already made paths writable before removing; this is the test-side counterpart,
kept in one place instead of copied three times).

A retained tree is a leak, not a failing assertion, so nothing raises: the paths
that could not be removed are returned and named on stderr. ``ignore_errors`` is
deliberately not used -- an unremovable tree must be visible, not swallowed.
"""
from __future__ import annotations

import os
import shutil
import stat
import sys
from pathlib import Path


def remove_tree(path) -> list[str]:
    """Delete PATH after making every directory and file writable.

    Returns the failures (empty when the tree is gone); each one is also printed
    to stderr so a leaked tree is never invisible in a suite's output.
    """
    root = Path(path)
    failures: list[str] = []
    if not os.path.lexists(root):
        return failures

    def make_writable_and_remove(function, target, _exception) -> None:
        try:
            mode = (stat.S_IWUSR | stat.S_IRUSR | stat.S_IXUSR
                    if os.path.isdir(target) else stat.S_IWUSR | stat.S_IRUSR)
            os.chmod(target, mode)
            function(target)
        except OSError as error:
            failures.append(f'{target}: {error}')

    # Directories first: a 0555 directory also blocks creating nothing here, but
    # it does block the unlink of its children on every platform this runs on.
    for directory, _names, _files in os.walk(root):
        try:
            os.chmod(directory, 0o700)
        except OSError as error:
            failures.append(f'{directory}: {error}')
    try:
        shutil.rmtree(root, onerror=make_writable_and_remove)
    except OSError as error:
        failures.append(f'{root}: {error}')
    if os.path.lexists(root):
        failures.append(f'{root}: still present')
    if failures:
        print(f'scratch: could not remove the test scratch tree {root}: '
              f'{"; ".join(failures)}', file=sys.stderr)
    return failures
