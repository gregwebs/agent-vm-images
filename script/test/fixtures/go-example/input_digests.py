#!/usr/bin/env python3
"""Print the digest of every input the go-dev gopls steps build from.

The native harness records these in its log so acceptance evidence binds the
working-copy bytes, not just the source commit and a count of dirty tracked
files. Kept as a sibling script rather than an inline heredoc: shell sources in
this repository may not embed Python (script/test/contracts.sh enforces it).

Usage: input_digests.py ROOT EXAMPLE_RELATIVE
Prints one `input <sha256>  <path>` line per input, then
`inputs-set <sha256>` over the newline-joined lines, in sorted path order.
"""
from __future__ import annotations

import hashlib
import pathlib
import sys

# The floor and the required set are enforced *here*, on the path the evidence run
# takes, not only in go-example.sh's self-test: a walk that stopped seeing the helper
# module, or a layer that lost an adapter, must fail the derivation itself. The
# number is below today's total (52) so an honest edit is a diff, and the named
# paths are the ones whose loss would silently shrink what the run binds.
MINIMUM_INPUTS = 40
REQUIRED_INPUTS = (
    'Dockerfile',
    'install-gopls.sh',
    'verify-toolchain.sh',
    'gopls_closure.py',
    'data/gopls-closure/go.mod',
    'data/gopls-closure/go.sum',
    'data/gopls-closure/gopls.go.mod',
    'data/gopls-closure/pin.json',
    'data/gopls-closure/THIRD_PARTY_LICENSES',
    'helper/go.mod',
    'helper/go.sum',
    'helper/vendor/modules.txt',
    # The installed-state verifier the recipe and the native bundle run (#250): its
    # sources are digested by the helper walk, and these three name the packages
    # whose loss would leave the verb unable to load, run, or judge.
    'helper/installed/installed.go',
    'helper/gotool/gotool.go',
    'helper/buildinfo/buildinfo.go',
)


def main() -> int:
    if len(sys.argv) != 3:
        print('usage: input_digests.py ROOT EXAMPLE_RELATIVE', file=sys.stderr)
        return 2
    root = pathlib.Path(sys.argv[1])
    layer = root / sys.argv[2]
    # Exactly the inputs the Dockerfile binds for the gopls steps, in the two sets
    # the recipe uses: the final stage's mounts (recipe, three adapters, the Python
    # reader, the five committed closure files) and the builder stage's bind
    # (`helper`, the whole module including its vendor tree, which is what the
    # stage compiles). The helper tree is walked rather than listed so a new source
    # file cannot be built without being digested; the count is floored by the
    # self-test control, which is what makes a short walk fail rather than pass.
    paths = [layer / name for name in ('Dockerfile', 'install-gopls.sh',
                                       'verify-toolchain.sh', 'gopls_closure.py')]
    paths += sorted(path for path in (layer / 'data' / 'gopls-closure').iterdir()
                    if path.is_file())
    helper = layer / 'helper'
    paths += sorted(path for path in helper.rglob('*') if path.is_file())
    missing = [name for name in REQUIRED_INPUTS if not (layer / name).is_file()]
    if missing:
        print(f'input_digests: missing required input(s): {", ".join(missing)}',
              file=sys.stderr)
        return 1
    if len(paths) < MINIMUM_INPUTS:
        print(f'input_digests: found {len(paths)} inputs, want at least '
              f'{MINIMUM_INPUTS}: the walk is not looking at the whole layer',
              file=sys.stderr)
        return 1
    lines = [f'{hashlib.sha256(path.read_bytes()).hexdigest()}  '
             f'{path.relative_to(root)}' for path in paths]
    for line in lines:
        print('input', line)
    print('inputs-set',
          hashlib.sha256(('\n'.join(lines) + '\n').encode()).hexdigest())
    return 0


if __name__ == '__main__':
    sys.exit(main())
